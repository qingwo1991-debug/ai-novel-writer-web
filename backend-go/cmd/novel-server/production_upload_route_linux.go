//go:build linux

package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/uploadstore"
)

// This file closes the migration's largest structural hole: the route that accepts
// a browser upload.
//
// # What was broken
//
// Three served families took a staged-upload id they had no way to receive:
//
//	kb:import-document / kb:import-folder / kb:import-reference-text
//	    every call answered KNOWLEDGE_BASE_UPLOAD_UNAVAILABLE because
//	    newProductionKnowledgeUploads(knowledge, nil) installed no upload owner
//	    (production_knowledge_uploads_linux.go:399-418);
//	character-avatar:choose
//	    answered AVATAR_UPLOAD_UNAVAILABLE for the same reason
//	    (production_character_avatar_linux.go:317-330);
//	dialog:select-knowledge-files / -folder and skills:pick-local
//	    answered PICKER_UPLOAD_UNAVAILABLE, so the author could never reach the
//	    upload slot those names advertise
//	    (production_browser_pickers_linux.go:1127-1130, :1161-1164, :1222-1225).
//
// And POST /api/uploads itself answered 501 GO_ROUTE_NOT_MIGRATED
// (internal/transport/server.go:123), because nothing mounted it on the production
// mux (production_open_linux.go:824-831) - so even a page that guessed the route
// had nowhere to send the bytes.
//
// # What is mounted
//
// The renderer's own request form, unchanged: two POST routes served by
// internal/uploadstore (see that package for the exact headers, the bounds and the
// eviction rule). This file owns only the production wiring:
//
//	POST /api/uploads         raw octet-stream body, X-Upload-Name (+ X-Upload-Folder
//	                          and X-Upload-Relative for a folder member)
//	                          -> 201 {uploadId, name, size}
//	POST /api/upload-folders  JSON object body
//	                          -> 201 {folderUploadId}
//
// Both are admitted by the gateway's own credential path - the cookie session, the
// 32-tab cap from transport.Server.ResolveTransferClient
// (internal/transport/server.go:61-69), then Origin/Sec-Fetch-Site/X-CSRF-Token from
// security.Sessions.Verify. No new credential rule is introduced, and no request
// value ever becomes a host path: the bytes are the body, and the id the answer
// carries is the only handle.
//
// # Where the bytes live
//
// One private directory, <dataRoot>/upload-staging, created 0700 by this file and
// owned by this process. It sits beside the library (dataRoot/workspaces) and is
// inside no served root: no fs:grant-read-file lease, no project root and no
// download route can reach it, and the staging store serves bytes only through the
// upload ids it minted. It is disposable staging, not author data, and Close
// removes what is left.
//
// # The seam
//
// productionUploadRoute implements productionPickerUploads
// (production_browser_pickers_linux.go:314-323) over that store, so the three lanes
// above read the bytes the route staged, keyed by the caller's own cookie session
// and tab. The parent swaps three constructor calls to install it; the exact lines
// are in this file's constructor comments, and every one of them keeps its
// nil-seam refusal path intact for the case where the route could not be opened.
//
// # What still refuses here, and why that is not a gap
//
// skills:install-local gets past the upload, the inspection and the single-use
// candidate, and then refuses at SKILLS_LOCAL_INSTALL_UNAVAILABLE on purpose:
// internal/promptskills exports no local publication port (`install` is unexported,
// install_linux.go:14, and InstallGitHub is the only exported installer,
// remote.go:47), so publishing a skill from here would mean writing that package's
// pinned root by hand. The refusal names the missing port instead.

const (
	// productionUploadStagingDirectory is the one directory this lane adds to the
	// author data root. It is disposable staging, never author data.
	productionUploadStagingDirectory = "upload-staging"
)

// productionUploadRoute is the mounted upload handler AND the seam the three
// upload-consuming lanes read. One value, because they must be the same store: an
// id minted by the route is only redeemable through the store that minted it.
type productionUploadRoute struct {
	store *uploadstore.Store
}

// productionUploadRoute is the upload owner the three lanes declare.
var _ productionPickerUploads = (*productionUploadRoute)(nil)

// newProductionUploadRoute opens the staging store over the production data root
// and returns the value that is both the mounted route and the seam.
//
// The parent calls this once, before the picker binder (production_open_linux.go:497),
// and keeps the result for the mux and the cleanup closure:
//
//	uploads, uploadErr := newProductionUploadRoute(open, server, address)
//	if uploadErr != nil {
//		log.Printf("novel-server: browser upload staging unavailable (%v); every upload-dependent name keeps refusing", uploadErr)
//	}
//
// and then mounts it beside the library route (production_open_linux.go:824):
//
//	if uploads != nil {
//		if mountErr := uploads.Mount(mux); mountErr != nil {
//			_ = open.Close()
//			return nil, nil, mountErr
//		}
//	}
//
// and closes it in the cleanup closure (production_open_linux.go:833):
//
//	if uploads != nil {
//		_ = uploads.Close()
//	}
//
// A failure here is NOT fatal to the lane: the store is disposable staging, so a
// read-only or replaced directory leaves the app usable with the honest refusal
// each of those names already knows how to answer.
func newProductionUploadRoute(open *productionOpen, server *transport.Server,
	address string) (*productionUploadRoute, error) {
	return newProductionUploadRouteWith(open, server, address, uploadstore.DefaultLimits())
}

// newProductionUploadRouteWith is the same constructor with explicit bounds. It is
// the test seam this lane's own rule needs: the eviction and lifetime cases are
// only observable with a small budget, and they must be observed through the same
// wiring production uses.
func newProductionUploadRouteWith(open *productionOpen, server *transport.Server,
	address string, limits uploadstore.Limits) (*productionUploadRoute, error) {
	if open == nil || server == nil || strings.TrimSpace(address) == "" {
		return nil, errors.New("PRODUCTION_UPLOAD_CONFIGURATION_INVALID")
	}
	open.mu.Lock()
	dataRoot, closed := open.layout.dataRoot, open.closed
	open.mu.Unlock()
	if closed || dataRoot == "" {
		return nil, errors.New("PRODUCTION_UPLOAD_LANE_CLOSED")
	}
	directory := filepath.Join(dataRoot, productionUploadStagingDirectory)
	if err := ensureProductionUploadStaging(directory); err != nil {
		return nil, err
	}
	// The origin allowlist is the one the operation gateway was built with
	// (production_open_linux.go:469): an upload is a mutation and is checked
	// against exactly the same origins, never a wider set.
	store, err := uploadstore.New(directory, server.SessionStore(),
		security.NewOrigins("http://"+address), server.ResolveTransferClient, limits)
	if err != nil {
		return nil, fmt.Errorf("production upload staging: %w", err)
	}
	return &productionUploadRoute{store: store}, nil
}

// ensureProductionUploadStaging creates the staging directory once and proves it is
// a private directory owned by this process. It never repairs a directory someone
// else made: a symlinked, shared or foreign root is a refusal.
func ensureProductionUploadStaging(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return errors.New("PRODUCTION_UPLOAD_STAGING_UNSAFE")
	}
	if err := requirePhysicalDirectory(filepath.Dir(directory)); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return errors.New("PRODUCTION_UPLOAD_STAGING_UNAVAILABLE")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return errors.New("PRODUCTION_UPLOAD_STAGING_UNSAFE")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("PRODUCTION_UPLOAD_STAGING_UNSAFE")
	}
	return nil
}

// Mount installs both upload routes on the production mux. ServeMux prefers the
// longest registered pattern, so these two registrations shadow the "/api/"
// catch-all and the transport's 501 branch is never reached for an upload again.
// The paths are the picker slot's own constants, so the route a name advertises and
// the route that answers can never drift apart.
func (u *productionUploadRoute) Mount(mux *http.ServeMux) error {
	if u == nil || u.store == nil {
		return errors.New("PRODUCTION_UPLOAD_ROUTE_UNAVAILABLE")
	}
	if mux == nil {
		return errors.New("PRODUCTION_UPLOAD_MUX_REQUIRED")
	}
	mux.Handle(productionPickerUploadRoute, u.store)
	mux.Handle(productionPickerUploadFolderRoute, u.store)
	return nil
}

// Close drops the store's rows and releases its pinned root. The staged bytes are
// disposable, so nothing survives a restart that no session could claim anyway.
func (u *productionUploadRoute) Close() error {
	if u == nil || u.store == nil {
		return nil
	}
	return u.store.Close()
}

// ReadUpload is the seam's single-file read: the staged bytes and their safe
// display name, for the caller's own cookie session and tab.
func (u *productionUploadRoute) ReadUpload(sessionID, clientID, uploadID string) (string, []byte, error) {
	if u == nil || u.store == nil {
		return "", nil, uploadstore.ErrUnavailable
	}
	return u.store.Read(sessionID, clientID, uploadID)
}

// ReadFolder is the seam's folder read: the staged folder's display label and its
// members by relative label. The labels are labels - nothing here resolves one
// against a filesystem.
func (u *productionUploadRoute) ReadFolder(sessionID, clientID,
	folderID string) (string, []productionPickerUploaded, error) {
	if u == nil || u.store == nil {
		return "", nil, uploadstore.ErrUnavailable
	}
	name, members, err := u.store.ReadFolder(sessionID, clientID, folderID)
	if err != nil {
		return "", nil, err
	}
	files := make([]productionPickerUploaded, 0, len(members))
	for _, member := range members {
		files = append(files, productionPickerUploaded{Relative: member.Relative, Data: member.Data})
	}
	return name, files, nil
}

// newProductionKnowledgeUploadsFrom installs the staging store on the three
// knowledge imports. The parent replaces production_open_linux.go:784
//
//	func() error { return newProductionKnowledgeUploads(knowledge, nil).Bind(server) },
//
// with
//
//	func() error { return newProductionKnowledgeUploadsFrom(knowledge, uploads).Bind(server) },
//
// A nil route is passed through as a nil seam rather than as a typed nil, so the
// binder keeps answering KNOWLEDGE_BASE_UPLOAD_UNAVAILABLE for every name that
// needs bytes - the honest refusal, not a panic.
func newProductionKnowledgeUploadsFrom(imports *productionKnowledgeImports,
	uploads *productionUploadRoute) *productionKnowledgeUploads {
	if uploads == nil {
		return newProductionKnowledgeUploads(imports, nil)
	}
	return newProductionKnowledgeUploads(imports, uploads)
}

// newProductionCharacterAvatarsFrom installs the staging store on
// character-avatar:choose, which is the one avatar name a browser needs it for:
// commit already takes the bytes as base64. The parent replaces
// production_open_linux.go:788
//
//	func() error { return newProductionCharacterAvatars(open.durable, nil).Bind(server) },
//
// with
//
//	func() error { return newProductionCharacterAvatarsFrom(open.durable, uploads).Bind(server) },
func newProductionCharacterAvatarsFrom(durable *productionDurableOpen,
	uploads *productionUploadRoute) *productionCharacterAvatars {
	if uploads == nil {
		return newProductionCharacterAvatars(durable, nil)
	}
	return newProductionCharacterAvatars(durable, uploads)
}

// newProductionBrowserPickersFrom installs the staging store on the picker family,
// which is what makes dialog:select-knowledge-files, dialog:select-knowledge-folder
// and skills:pick-local answer verified uploads instead of an unavailable slot. The
// parent replaces production_open_linux.go:497
//
//	if bindErr := newProductionBrowserPickers(open).Bind(server); bindErr != nil {
//
// with
//
//	if bindErr := newProductionBrowserPickersFrom(open, uploads).Bind(server); bindErr != nil {
//
// The canonical constructor is called first and only the seam field is set, so the
// grants adapter and the candidate table cannot diverge from the lane's own. The
// skill publisher stays nil: see this file's header.
func newProductionBrowserPickersFrom(open *productionOpen,
	uploads *productionUploadRoute) *productionBrowserPickers {
	pickers := newProductionBrowserPickers(open)
	if uploads != nil {
		pickers.uploads = uploads
	}
	return pickers
}

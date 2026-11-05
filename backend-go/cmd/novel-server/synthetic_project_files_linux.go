//go:build linux

package main

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the durable lane's browser file surface: the confined project
// file operations (fs:read-file, fs:write-file, fs:mkdir, fs:check-exists) plus
// the two transfer routes that replace the desktop dialog:* pickers.
//
// The desktop contract let the main process touch any path under the canonical
// project root and minted external-file grants for paths anywhere on the host.
// A browser can do neither, so every path here is resolved under the admitted
// project's own pinned root and the browser never names a host path. That is
// STRICTER than the original grant model on purpose: reusing it by name in a
// browser-exposed server would let any page ask the server to read or replace a
// file the server user can reach.
const (
	// syntheticProjectFileMaxBytes bounds one uploaded or downloaded file.
	syntheticProjectFileMaxBytes = projectfs.BrowserFileMaxBytes
	// syntheticUploadBodyMaxBytes leaves room for the multipart envelope around
	// one bounded file; the file itself is bounded again after parsing.
	syntheticUploadBodyMaxBytes = projectfs.BrowserFileMaxBytes + (1 << 20)
	// syntheticUploadMemoryBytes is how much of the multipart body is held in
	// memory before net/http spills to a private temporary file.
	syntheticUploadMemoryBytes = int64(8 << 20)
)

// projectFileOwner is the browser cookie/tab identity the durable lease is keyed
// by. It is server-owned: nothing in the request body can choose it.
func projectFileOwner(caller transport.Caller) projectsession.Owner {
	return projectsession.Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
}

// projectFileRoots snapshots the lane's lifecycle and library under the lane
// lock. Callers never hold that lock while a file operation runs.
func (s *syntheticDurableOpen) projectFileRoots() (*authorruntime.ReadLifecycle, *projectfs.Library) {
	if s == nil {
		return nil, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, nil
	}
	return s.lifecycle, s.library
}

// withProjectFileLease runs one file operation under the exact durable lease
// the caller froze. It re-observes the lease first (so a stale or forged
// context never opens a project tree), pins the project by id, and releases the
// pinned project afterwards. No database transaction is involved: the durable
// writer lock this lane already holds keeps other writers out, and the file
// itself is published atomically.
func (s *syntheticDurableOpen) withProjectFileLease(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, action func(*projectfs.Project) error) error {
	if ctx == nil || action == nil {
		return errors.New("SYNTHETIC_DURABLE_CLOSED")
	}
	lifecycle, library := s.projectFileRoots()
	if lifecycle == nil || library == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, "SYNTHETIC_DURABLE_CLOSED")
	}
	current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return err
	}
	if !present || current != expected {
		return security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
	}
	project, err := library.OpenProject(expected.ProjectPath)
	if err != nil {
		return err
	}
	if project.ProjectID() != expected.ProjectID {
		_ = project.Close()
		return security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
	}
	actionErr := action(project)
	closeErr := project.Close()
	if actionErr != nil {
		return actionErr
	}
	return closeErr
}

// verifyProjectFileLease re-observes the lease after a write committed. The
// bytes are already the project's content, so a lease that changed in between
// is an unknown outcome, never a clean failure: the next holder of the project
// may already be reading them.
func (s *syntheticDurableOpen) verifyProjectFileLease(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) error {
	lifecycle, _ := s.projectFileRoots()
	if lifecycle == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, "SYNTHETIC_DURABLE_CLOSED")
	}
	current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return err
	}
	if !present || current != expected {
		return projectfs.ErrBrowserCommitUnknown
	}
	return nil
}

// ReadProjectFile is the provider seam for fs:read-file.
func (s *syntheticDurableOpen) ReadProjectFile(ctx context.Context, caller transport.Caller, raw any, relative string, limit int64) ([]byte, error) {
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, err
	}
	return s.readProjectFileAs(ctx, projectFileOwner(caller), expected, relative, limit)
}

func (s *syntheticDurableOpen) readProjectFileAs(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, relative string, limit int64) ([]byte, error) {
	var content []byte
	if err := s.withProjectFileLease(ctx, owner, expected, func(project *projectfs.Project) error {
		data, readErr := project.ReadBrowserFile(ctx, relative, limit)
		content = data
		return readErr
	}); err != nil {
		return nil, err
	}
	return content, nil
}

// WriteProjectFile is the provider seam for fs:write-file.
func (s *syntheticDurableOpen) WriteProjectFile(ctx context.Context, caller transport.Caller, raw any, relative string, data []byte) error {
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return err
	}
	return s.writeProjectFileAs(ctx, projectFileOwner(caller), expected, relative, data)
}

func (s *syntheticDurableOpen) writeProjectFileAs(ctx context.Context, owner projectsession.Owner, expected projectsession.Context, relative string, data []byte) error {
	if err := s.withProjectFileLease(ctx, owner, expected, func(project *projectfs.Project) error {
		return project.WriteBrowserFile(ctx, relative, data)
	}); err != nil {
		return err
	}
	return s.verifyProjectFileLease(ctx, owner, expected)
}

// ProjectPathExists is the provider seam for fs:check-exists.
func (s *syntheticDurableOpen) ProjectPathExists(ctx context.Context, caller transport.Caller, raw any, relative string) (bool, error) {
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return false, err
	}
	present := false
	if err := s.withProjectFileLease(ctx, projectFileOwner(caller), expected, func(project *projectfs.Project) error {
		found, existsErr := project.BrowserPathExists(ctx, relative)
		present = found
		return existsErr
	}); err != nil {
		return false, err
	}
	return present, nil
}

// MakeProjectDirectory is the provider seam for fs:mkdir.
func (s *syntheticDurableOpen) MakeProjectDirectory(ctx context.Context, caller transport.Caller, raw any, relative string) error {
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return err
	}
	return s.withProjectFileLease(ctx, projectFileOwner(caller), expected, func(project *projectfs.Project) error {
		return project.MakeBrowserDirectory(ctx, relative)
	})
}

// syntheticProjectTransfers is the durable lane's browser transfer surface. It
// is the web re-expression of the desktop dialog:* family, not a second name
// for it: a page cannot open a native picker, so the browser's own file input
// uploads bytes to a confined project path (POST /api/uploads, multipart) and
// the browser's own download saves a confined project file
// (GET /api/downloads/<relative path>).
//
// Neither route accepts a grant id, a host path or a project path. The project
// is whatever the caller's cookie and tab currently hold a durable lease for,
// so the transfer surface can never address a project the caller has not
// admitted. No operation name is bound to these routes: the dialog:* names stay
// at GO_OPERATION_NOT_MIGRATED because their consumers (import inspection,
// knowledge import, archive restore, export publication) are not migrated.
type syntheticProjectTransfers struct {
	open    *syntheticDurableOpen
	server  *transport.Server
	origins security.Origins
}

func (t *syntheticProjectTransfers) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := t.serve(w, r); err != nil {
		_ = security.WriteError(w, err)
	}
}

func (t *syntheticProjectTransfers) serve(w http.ResponseWriter, r *http.Request) error {
	if t == nil || t.open == nil || t.server == nil || r == nil {
		return security.NewHTTPError(http.StatusServiceUnavailable, "PROJECT_FILE_UNAVAILABLE")
	}
	if r.URL.Path == "/api/uploads" && r.Method == http.MethodPost {
		return t.upload(w, r)
	}
	if strings.HasPrefix(r.URL.Path, "/api/downloads/") && r.Method == http.MethodGet {
		return t.download(w, r)
	}
	return security.NewHTTPError(http.StatusNotFound, "API_ROUTE_NOT_FOUND")
}

// transferOwner reuses the RPC gateway's cookie and 32-tab admission, exactly
// like the existing upload staging route. A mutation additionally carries the
// gateway's CSRF and Origin check; a download is a session-bound read like the
// browser gateway's own /api/downloads route.
func (t *syntheticProjectTransfers) transferOwner(w http.ResponseWriter, r *http.Request, mutable bool) (projectsession.Owner, error) {
	session, client, err := t.server.ResolveTransferClient(w, r)
	if err != nil {
		return projectsession.Owner{}, err
	}
	if session == nil || session.ID() == "" || client == "" {
		return projectsession.Owner{}, security.NewHTTPError(http.StatusBadRequest, "CLIENT_ID_REQUIRED")
	}
	if mutable {
		if err := t.server.SessionStore().Verify(r, session, t.origins); err != nil {
			return projectsession.Owner{}, err
		}
	}
	return projectsession.Owner{SessionID: session.ID(), ClientID: client}, nil
}

// currentProjectLease resolves the caller's live durable lease. A route never
// takes a project path from the request: a caller without an admitted project
// has nothing to transfer.
func (t *syntheticProjectTransfers) currentProjectLease(ctx context.Context, owner projectsession.Owner) (projectsession.Context, error) {
	if err := t.open.VerifyRecovery(); err != nil {
		return projectsession.Context{}, security.NewHTTPError(http.StatusServiceUnavailable, "PROJECT_CATALOG_RECOVERY_PENDING")
	}
	lifecycle, _ := t.open.projectFileRoots()
	if lifecycle == nil {
		return projectsession.Context{}, security.NewHTTPError(http.StatusServiceUnavailable, "SYNTHETIC_DURABLE_CLOSED")
	}
	current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return projectsession.Context{}, err
	}
	if !present {
		return projectsession.Context{}, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_REQUIRED")
	}
	return current, nil
}

// upload writes one multipart file into the caller's project at the confined
// relative path in the ?path= query parameter. The bytes are committed with the
// same atomic replacement fs:write-file uses, so an upload is a file the browser
// can immediately read back through the file surface.
func (t *syntheticProjectTransfers) upload(w http.ResponseWriter, r *http.Request) error {
	owner, err := t.transferOwner(w, r, true)
	if err != nil {
		return err
	}
	expected, err := t.currentProjectLease(r.Context(), owner)
	if err != nil {
		return err
	}
	relative := r.URL.Query().Get("path")
	if relative == "" {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_FILE_PATH_REQUIRED")
	}
	// Refuse the destination before the body is buffered: an absolute path or a
	// traversal must not make the server hold an upload it will only discard.
	if _, err := projectfs.BrowserRelativePath(relative); err != nil {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_FILE_PATH_REFUSED")
	}
	r.Body = http.MaxBytesReader(w, r.Body, syntheticUploadBodyMaxBytes)
	parseErr := r.ParseMultipartForm(syntheticUploadMemoryBytes)
	// Register the form cleanup before inspecting the error: a body that spilled
	// to a temporary file must be removed even when the parse is refused, or a
	// hostile client could fill the private temporary directory by repeating a
	// failing upload.
	if r.MultipartForm != nil {
		defer func() { _ = r.MultipartForm.RemoveAll() }()
	}
	if parseErr != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(parseErr, &tooLarge) {
			return security.NewHTTPError(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		}
		return security.NewHTTPError(http.StatusBadRequest, "UPLOAD_BODY_INVALID")
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		return security.NewHTTPError(http.StatusBadRequest, "UPLOAD_FILE_REQUIRED")
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, syntheticProjectFileMaxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > syntheticProjectFileMaxBytes {
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, "PROJECT_FILE_TOO_LARGE")
	}
	// The display name is a browser fact, never a path: filepath.Base is applied
	// to the part's own filename so the response cannot echo a directory.
	name := ""
	if header != nil {
		name = filepath.Base(header.Filename)
	}
	if err := t.open.writeProjectFileAs(r.Context(), owner, expected, relative, data); err != nil {
		return transferProjectFileError(err)
	}
	// No uploadId is minted. The bytes are already committed into a path the
	// caller can read, so a staging id would be a second, unowned authority.
	return security.WriteJSON(w, http.StatusCreated, map[string]any{
		"success": true, "path": relative, "name": name, "size": len(data),
	})
}

// download streams one confined project file to the browser's own save
// mechanism. The path in the URL is not authority: it is resolved under the
// caller's live lease, so a stale tab cannot download a manuscript it no longer
// holds.
func (t *syntheticProjectTransfers) download(w http.ResponseWriter, r *http.Request) error {
	owner, err := t.transferOwner(w, r, false)
	if err != nil {
		return err
	}
	relative, err := url.PathUnescape(strings.TrimPrefix(r.URL.Path, "/api/downloads/"))
	if err != nil {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_FILE_PATH_REFUSED")
	}
	if _, err := projectfs.BrowserRelativePath(relative); err != nil {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_FILE_PATH_REFUSED")
	}
	expected, err := t.currentProjectLease(r.Context(), owner)
	if err != nil {
		return err
	}
	data, err := t.open.readProjectFileAs(r.Context(), owner, expected, relative, syntheticProjectFileMaxBytes)
	if err != nil {
		return transferProjectFileError(err)
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(filepath.Base(relative)))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(data); err != nil {
		return err
	}
	return nil
}

// transferProjectFileError maps a confined file refusal to the transfer routes'
// HTTP envelope. A lane error that already carries an HTTP code keeps it.
func transferProjectFileError(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	switch {
	case errors.Is(err, projectfs.ErrBrowserPathRefused):
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_FILE_PATH_REFUSED")
	case errors.Is(err, projectfs.ErrTooLarge):
		return security.NewHTTPError(http.StatusRequestEntityTooLarge, "PROJECT_FILE_TOO_LARGE")
	case errors.Is(err, fs.ErrNotExist):
		return security.NewHTTPError(http.StatusNotFound, "PROJECT_FILE_NOT_FOUND")
	case errors.Is(err, projectfs.ErrBrowserCommitUnknown):
		return security.NewHTTPError(http.StatusConflict, "PROJECT_FILE_COMMIT_UNKNOWN")
	case errors.Is(err, projectfs.ErrUnsafeFile), errors.Is(err, projectfs.ErrUnsafeDirectory), errors.Is(err, projectfs.ErrChanged):
		return security.NewHTTPError(http.StatusForbidden, "PROJECT_FILE_UNSAFE")
	}
	return security.NewHTTPError(http.StatusInternalServerError, "PROJECT_FILE_FAILED")
}

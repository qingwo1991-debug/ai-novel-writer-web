//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves `dialog:select-novel-files` - the import picker - on the
// production lane.
//
// # What refused, and why it now does not
//
// The name is a renderer capability channel, not a native dialog
// (src/components/dialogs/ImportNovelDialog.tsx:214-219 sends
// {runId, purpose, locale, expectedProjectPath} plus the frozen project session
// and reads back an inspection token). Transport already owns its binder:
// BindImportSelection (internal/transport/import_selection.go:48-118) validates
// the two arguments, the frozen three-key project context and the purpose, then
// asks an ImportSelectionProvider for the captured sources and freezes them in an
// importsource.Store. The lane had no such
// provider, so the name stayed at GO_OPERATION_NOT_MIGRATED with a reason that
// said "POST /api/uploads is 501" (production_project_portability_linux.go:222-224).
// That reason is no longer true: the upload route is mounted
// (production_upload_route_linux.go:97-107, :216-221). This file supplies the two
// halves the binder asks for - one importsource.Store and one provider over the
// uploads this lane owns.
//
// # Where the bytes come from, and why no id can arrive in the request
//
// Every other upload consumer in this lane takes upload ids from its arguments
// (production_browser_pickers_linux.go:316-323,
// production_knowledge_uploads_linux.go:399-418). This binder cannot: its two
// arguments are exactly a four-key request and a three-key frozen context
// (import_selection.go:52-78), and both counts are pinned, so an added
// `uploadIds` field is refused as INVALID_ARGUMENTS before any provider runs.
// The desktop shape explains why - selection, bounded reading and inspection were
// ONE main-process operation there (electron/controllers/import-controller.ts:322-323),
// with the native dialog between them. A browser has no native dialog, so the
// equivalent is two steps the page performs in order:
//
//	POST /api/uploads      the author's manuscripts, staged by the upload route
//	                       (internal/uploadstore, one id per file)
//	dialog:select-novel-files
//	                       exactly what the desktop did: read, bound, parse, inspect
//
// The provider therefore resolves the caller's PENDING staging itself. It never
// trusts that resolution: staging directory entries only ever yield CANDIDATE ids,
// and every candidate is redeemed through the staging owner's own
// ReadUpload(sessionID, clientID, id) - the frozen ownership and expiry check
// (internal/uploadstore/uploads.go:556-575). A foreign tab's id, another cookie
// session's id, an evicted id and an id this process never minted are one refusal
// there (uploads.go:128-134), so none of them can become a source: they are simply
// not redeemable, and if nothing is redeemable the selection refuses by name
// instead of inventing an empty manuscript.
//
// Because the inbox is shared staging, not an import-specific drop box, the only
// uploads a selection admits are the ones SHAPED like manuscripts (the extensions
// importsource.Parse accepts, minus an installed package's SKILL.md). A page that
// stages an avatar or a knowledge folder in the same tab is therefore never
// silently imported; the answer names every source the author will see, and a
// second selection re-reads the same pending set, replacing the caller's receipt
// exactly as the desktop's controller does
// (internal/importsource/inspection.go:100-101).
//
// # What the picker admits
//
//   - the caller's own cookie session and browser tab: both halves of the staging
//     key, taken from the authenticated transport.Caller, never from an argument;
//   - while the caller holds the live lease the request names: projectId, leaseId
//     and projectPath are compared with projectsession's own current context
//     (productionFSGrantProject, production_fs_grants_linux.go:249-276), so a tab
//     that has moved on cannot import through a lease id it still remembers;
//   - either purpose the renderer declares: author-manuscript, which
//     db:import-run-prepare-inspection turns into the author run, and reference,
//     which the same name turns into the durable parsing run
//     (store.ImportRuns.BeginParsing -> CommitParsedSource -> FinalizeParsing). The
//     receipt is frozen under the requested purpose and only that purpose's binding
//     can spend it;
//   - within importsource's limits: source count and total bytes, and the parser's
//     own per-source bounds. One oversized pending set refuses the whole selection
//     with the code the renderer already understands rather than truncating.
//
// Every refusal happens before Inspect, so the receipt table stays empty and the
// project tree, the database and the skill root are byte-identical.
//
// # The two lines the parent adds
//
// production_open_linux.go, beside the picker binder (:507):
//
//	selection, selectionErr := newProductionImportSelection(open, uploads)
//	if selectionErr != nil {
//		log.Printf("novel-server: import selection unavailable (%v); dialog:select-novel-files keeps refusing", selectionErr)
//	} else if bindErr := selection.Bind(server); bindErr != nil {
//		_ = open.Close()
//		return nil, nil, bindErr
//	}

// The refusal vocabulary of this file. Each code is symbolic, each is logged, and
// none reaches the wire verbatim: transport's binder folds a provider error into
// IMPORT_SELECTION_FAILED (import_selection.go:124-142) and only re-exposes the
// codes it already knows (IMPORT_SOURCE_BYTES_EXCEEDED and its siblings), because
// the page cannot read a finer reason anyway.
const (
	// productionImportSelectionLaneCode: this lane has no store or no lane value.
	productionImportSelectionLaneCode = "IMPORT_SELECTION_LANE_UNAVAILABLE"
	// productionImportSelectionContextCode: no request context to work with.
	productionImportSelectionContextCode = "IMPORT_SELECTION_CONTEXT_INVALID"
	// productionImportSelectionProjectCode: the caller has no admitted lease, so
	// no project of theirs can own an import.
	productionImportSelectionProjectCode = "PROJECT_SESSION_REQUIRED"
	// productionImportSelectionStaleCode: the lease the request named is not the
	// lease the runtime holds for this caller.
	productionImportSelectionStaleCode = "PROJECT_SESSION_STALE"
	// productionImportSelectionClosedCode: the durable runtime is not open.
	productionImportSelectionClosedCode = "PROJECT_NOT_OPEN"
	// productionImportSelectionUploadsCode: no staging owner, or staging cannot be
	// read, so there is no inbox to inspect.
	productionImportSelectionUploadsCode = "IMPORT_SELECTION_UPLOADS_UNAVAILABLE"
	// productionImportSelectionSourcesCode: the caller has nothing pending that is
	// shaped like a manuscript. This is also the answer for a foreign or an
	// evicted upload id, because staging does not distinguish the two
	// (uploads.go:128-134).
	productionImportSelectionSourcesCode = "IMPORT_SELECTION_SOURCES_UNAVAILABLE"
	// productionImportSelectionBytesCode is the parser's own pass-through code for
	// a pending set larger than importsource's total bound, so the page hears a
	// reason it already knows.
	productionImportSelectionBytesCode = "IMPORT_SOURCE_BYTES_EXCEEDED"
)

// productionImportUploadID is the only shape a staged blob can have
// (internal/uploadstore/uploads.go:353-355), repeated because that regexp is
// unexported. A directory entry that does not match is not even a candidate: an
// in-flight upload's `.part` file and anything else staging holds are skipped
// without ever being redeemed.
var productionImportUploadID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

// productionImportManuscriptExtensions are the extensions importsource.Parse can
// read (internal/importsource/parser.go:160-166). The picker admits only these, so
// an avatar or an archive staged in the same tab is never offered as a manuscript.
var productionImportManuscriptExtensions = []string{".txt", ".text", ".md", ".epub"}

// productionImportUploads is the staging seam this file needs, and it is
// deliberately the picker's own single-file half
// (production_browser_pickers_linux.go:316-321): one owner, one ownership rule,
// one refusal vocabulary for "this id is not yours".
type productionImportUploads interface {
	ReadUpload(sessionID, clientID, uploadID string) (name string, data []byte, err error)
}

// productionImportUploadRoute ties the route value to the seam it must satisfy.
// The route is what the parent mounts and what this picker reads, so an id minted
// by the mounted route is exactly the id this picker redeems.
var _ productionImportUploads = (*productionUploadRoute)(nil)

// productionImportStaged is one candidate the inbox had on disk: an opaque id and
// the moment it was staged. It is not a path and carries no bytes.
type productionImportStaged struct {
	id   string
	info os.FileInfo
}

// productionImportSelection is the import picker: an importsource.Store (the
// binder's frozen receipts) plus the provider that fills it from the caller's own
// staging.
type productionImportSelection struct {
	open    *productionOpen
	uploads productionImportUploads
	inbox   string
	store   *importsource.Store
}

// newProductionImportSelection builds the pair production_open_linux.go hands to
// transport.BindImportSelection. The store's MAC key is per startup and never
// leaves this process: receipts live in memory only
// (internal/importsource/inspection.go:67-86), so nothing here is persisted and
// nothing has to be re-proved after a restart.
func newProductionImportSelection(open *productionOpen,
	uploads *productionUploadRoute) (*productionImportSelection, error) {
	if open == nil {
		return nil, errors.New("PRODUCTION_IMPORT_SELECTION_CONFIGURATION_INVALID")
	}
	open.mu.Lock()
	dataRoot, closed := open.layout.dataRoot, open.closed
	open.mu.Unlock()
	if closed || dataRoot == "" {
		return nil, errors.New("PRODUCTION_IMPORT_SELECTION_LANE_CLOSED")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, errors.New("PRODUCTION_IMPORT_SELECTION_UNAVAILABLE")
	}
	store, err := importsource.NewStore(key, importsource.StoreOptions{})
	if err != nil {
		return nil, errors.New("PRODUCTION_IMPORT_SELECTION_UNAVAILABLE")
	}
	selection := &productionImportSelection{open: open, store: store}
	if uploads != nil {
		selection.uploads = uploads
		// The staging root is the one the route created from the same data root
		// (production_upload_route_linux.go:152-155), never a request value.
		selection.inbox = filepath.Join(dataRoot, productionUploadStagingDirectory)
	}
	return selection, nil
}

// Bind installs the two halves transport asks for. Nothing else is bound: the
// name, its argument shape and its return envelope stay the binder's.
func (s *productionImportSelection) Bind(server *transport.Server) error {
	if s == nil || s.store == nil || server == nil {
		return errors.New("PRODUCTION_IMPORT_SELECTION_BINDER_INVALID")
	}
	return server.BindImportSelection(s.store, s)
}

// BeginImportSelection is the provider half. It proves the caller and the lease,
// then captures the caller's pending manuscripts as sources. The release function
// owns nothing: the store keeps the receipt, and the binder closes over it.
func (s *productionImportSelection) BeginImportSelection(ctx context.Context, caller transport.Caller,
	request transport.ImportSelectionRequest,
	expected map[string]any) (transport.ImportSelection, func(), error) {
	if s == nil || s.store == nil || s.open == nil {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionLaneCode)
	}
	if ctx == nil {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionContextCode)
	}
	// The TAB IDENTITY (cookie session plus browser tab) keys the staged sources
	// this provider redeems; the LEASE OWNER below (the authenticated account)
	// names the project lease the frozen context must still match. Aliasing them
	// would either leak one tab's staged manuscript into its sibling or refuse a
	// legitimate second tab.
	tab, ownerErr := productionTabOwner(caller)
	if ownerErr != nil {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionProjectCode)
	}
	leaseOwner, leaseOwnerErr := productionLeaseOwner(caller)
	if leaseOwnerErr != nil {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionProjectCode)
	}
	projectID, idOK := expected["projectId"].(string)
	leaseID, leaseOK := expected["leaseId"].(string)
	projectPath, pathOK := expected["projectPath"].(string)
	if !idOK || !leaseOK || !pathOK || projectID == "" || leaseID == "" || projectPath == "" {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionProjectCode)
	}
	// Both purposes reach a provider now: the binder validates the vocabulary and
	// this provider only re-asserts it, so a purpose added to the binder without a
	// capture rule here would still be refused instead of frozen under a wrong
	// binding.
	if request.Purpose != importsource.AuthorManuscript && request.Purpose != importsource.Reference {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionContextCode)
	}
	s.open.mu.Lock()
	durable, closed := s.open.durable, s.open.closed
	s.open.mu.Unlock()
	if closed || durable == nil {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionClosedCode)
	}
	// The runtime's own current lease for this caller, compared with the context
	// the request named. This is the same proof the portability and grant families
	// make (production_project_portability_linux.go:560-570), and it is why an
	// upload staged for another project cannot be imported into this one.
	current, present, leaseErr := durable.productionFSGrantProject(ctx, leaseOwner)
	if leaseErr != nil || !present {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionProjectCode)
	}
	if current.ProjectID != projectID || current.LeaseID != leaseID || current.ProjectPath != projectPath {
		return transport.ImportSelection{}, nil,
			productionImportSelectionRefusal(productionImportSelectionStaleCode)
	}
	sources, sourceErr := s.capture(tab.SessionID, tab.ClientID)
	if sourceErr != nil {
		return transport.ImportSelection{}, nil, sourceErr
	}
	// The returned identity is the runtime's, not the request's: the binder still
	// compares it with the frozen context (import_selection.go:99-101) and refuses
	// AUTHORITY_CHANGED on any difference.
	return transport.ImportSelection{ProjectID: current.ProjectID, LeaseID: current.LeaseID,
		Sources: sources}, func() {}, nil
}

// capture redeems this caller's pending staged manuscripts, in the order they were
// uploaded. Candidate ids come from the staging inbox; every byte comes from the
// staging owner's ownership-checked read, so a foreign or evicted id contributes
// nothing and is never distinguished from one this process never minted.
func (s *productionImportSelection) capture(sessionID, clientID string) ([]importsource.CapturedSource, error) {
	if s.uploads == nil || s.inbox == "" {
		return nil, productionImportSelectionRefusal(productionImportSelectionUploadsCode)
	}
	entries, err := os.ReadDir(s.inbox)
	if errors.Is(err, fs.ErrNotExist) {
		// A lane with staging installed but no inbox yet staged nothing.
		return nil, productionImportSelectionRefusal(productionImportSelectionSourcesCode)
	}
	if err != nil {
		return nil, productionImportSelectionRefusal(productionImportSelectionUploadsCode)
	}
	pending := make([]productionImportStaged, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !entry.Type().IsRegular() || !productionImportUploadID.MatchString(entry.Name()) {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		pending = append(pending, productionImportStaged{id: entry.Name(), info: info})
	}
	// One id per upload, renamed only once the blob is complete
	// (uploads.go:509-556), so the file's own time is the order the author staged
	// in; the id breaks a tie deterministically.
	sort.SliceStable(pending, func(i, j int) bool {
		first, second := pending[i].info.ModTime(), pending[j].info.ModTime()
		if !first.Equal(second) {
			return first.Before(second)
		}
		return pending[i].id < pending[j].id
	})
	limits := importsource.DefaultLimits()
	sources := make([]importsource.CapturedSource, 0, len(pending))
	var total int64
	for _, item := range pending {
		if len(sources) >= limits.MaxSourceFiles {
			break
		}
		name, data, readErr := s.uploads.ReadUpload(sessionID, clientID, item.id)
		if readErr != nil {
			// Not this caller's, evicted, or never staged by this process: staging
			// answers all three the same way, so this one is skipped without a word.
			continue
		}
		if !productionImportManuscriptName(name) || !productionImportDisplayName(name) {
			continue
		}
		if total+int64(len(data)) > limits.MaxTotalBytes {
			return nil, productionImportSelectionRefusal(productionImportSelectionBytesCode)
		}
		total += int64(len(data))
		sources = append(sources, importsource.CapturedSource{
			DisplayName: name,
			Reader:      bytes.NewReader(data),
			// Server-derived identity of a trusted staging object: an opaque upload
			// id, never a host path, and stable across a re-selection of the same
			// staged bytes.
			Identity: importsource.Identity{
				CanonicalLocation: "upload:" + item.id,
				FileIdentity:      item.id,
			},
		})
	}
	if len(sources) == 0 {
		return nil, productionImportSelectionRefusal(productionImportSelectionSourcesCode)
	}
	return sources, nil
}

// productionImportSelectionRefusal is the one place this file answers a refusal:
// the code is logged (symbolic, no path, no id, no request value) and returned as
// the binder's input. transport decides what the page reads.
func productionImportSelectionRefusal(code string) error {
	log.Printf("novel-server: import selection refused: %s", code)
	return errors.New(code)
}

// productionImportManuscriptName reports whether an uploaded display name is one
// importsource.Parse can read. An installed package's SKILL.md is deliberately not
// a manuscript: the skills lane stages it in the same inbox
// (production_browser_pickers_linux.go:1192-1249), and importing a package as
// prose would be an accident, not a selection.
func productionImportManuscriptName(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" || lower == "skill.md" {
		return false
	}
	for _, extension := range productionImportManuscriptExtensions {
		if strings.HasSuffix(lower, extension) {
			return true
		}
	}
	return false
}

// productionImportDisplayName is importsource's own display-name rule
// (internal/importsource/parser.go:72-76): one visible name, valid UTF-8, no
// separator or NUL, not a dot entry, bounded in UTF-16 units. It is repeated
// because the parser's copy is unexported, and applied here so one unusable name
// is skipped instead of failing the whole inspection with IMPORT_SOURCE_INVALID.
func productionImportDisplayName(name string) bool {
	if name == "" || !utf8.ValidString(name) || strings.ContainsAny(name, "/\\\x00") ||
		name == "." || name == ".." || strings.TrimSpace(name) == "" {
		return false
	}
	units := 0
	for _, char := range name {
		units++
		if char > 0xffff {
			units++
		}
	}
	return units <= 255
}

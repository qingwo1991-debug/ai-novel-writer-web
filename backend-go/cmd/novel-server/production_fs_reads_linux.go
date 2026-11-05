//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's READ-ONLY slice of the `fs:*` family in
// web/operations.json (ten names, web/operations.json:204-213). It binds exactly
// three of them and installs no other name, so every write and every name this
// lane cannot answer honestly keeps failing closed at 501
// GO_OPERATION_NOT_MIGRATED.
//
// # Why the family binder is not used
//
// transport.BindProjectFiles (internal/transport/project_files.go:75-195) is
// atomic across four names: it publishes fs:read-file, fs:mkdir, fs:check-exists
// and fs:write-file together, or nothing at all (:86-94). Two of those four are
// mutations - fs:write-file replaces an author file through a temporary file and
// one rename (internal/projectfs/browser_files.go:177-210, :215-254) and fs:mkdir
// creates directories (browser_files.go:316-358) - so binding that family here
// would hand this lane a write surface it does not own. The three names are
// therefore bound per name with server.Bind, the exported single-name binder
// (internal/transport/server.go:73-77), exactly the way
// production_importrun_reads_linux.go:140-159 installs its three reads and
// production_open_linux.go:580 installs prompt:load-global. server.Bind itself
// refuses a name that is not inventoried (OPERATION_NOT_EXPOSED) or is already
// bound (OPERATION_ALREADY_BOUND), so a name this file should not publish is a
// startup error instead of silent dead code.
//
// # Bound, and the confinement each one uses
//
//	fs:read-file     arity 3  [relativePath, claimedRoot, context]
//	fs:check-exists  arity 3  [relativePath, claimedRoot, context]
//	fs:read-json     arity 3  [absoluteMarkerPath, claimedRoot, context]
//
// The wire arity is the family binder's own table
// (internal/transport/project_files.go:37-51), with the frozen three-key session
// context LAST - the position the renderer appends it to
// (src/services/ipc-client.ts:83) and the one draftExpectedContext reads. The
// second-to-last argument is the claimed project root the renderer froze; it must
// equal the project inside the frozen context, so a page cannot aim one project's
// lease at another project's tree.
//
//  1. fs:read-file and fs:check-exists are confined to the PROJECT ROOT by a
//     path the lane never treats as a host path: the browser's path must be
//     relative, and projectfs.BrowserRelativePath (internal/projectfs/
//     browser_files.go:73-84) refuses absolute paths, drive letters, backslashes,
//     NUL bytes, "." / ".." components, hidden components and the storage-reserved
//     names (project.json, project.db and its sidecars) BEFORE anything is opened.
//     The validated relative path is then walked one component at a time through
//     already pinned os.Root handles (browser_files.go:96-132), and each hop is
//     re-verified by projectfs.openChild - Lstat, OpenRoot, then a before/opened/
//     after SameFile re-check that refuses a symlink or a swapped ancestor
//     (internal/projectfs/project.go:62-82). The final component must be a
//     single-link regular file opened with O_NOFOLLOW
//     (project.go:218-238). os.Root refuses an escape even if an ancestor is
//     swapped mid-walk, so a symlink cannot walk out of the project. That walk is
//     Project.ReadBrowserFile (browser_files.go:137-170) and
//     Project.BrowserPathExists (browser_files.go:271-310); this file calls the
//     browser contract directly and never joins a browser string onto a host path.
//
//  2. fs:read-json is confined to the STORAGE ROOT, to the single name the lane
//     derives itself: <projectPath>/.ai-novel/partial_arch.json. The family binder
//     for this name accepts the argument only when it is exactly
//     filepath.Join(root, projectfs.StorageDirectory, "partial_arch.json")
//     (internal/transport/editor_checkpoint.go:47); this file derives the same
//     absolute path from the frozen context (productionFSArchCheckpointPath) and
//     compares the argument against it, so the request's string is never opened.
//     The bytes then come from authorruntime.ReadLifecycle.ReadArchCheckpoint
//     (internal/authorruntime/editor_checkpoint.go:22-53), which reads the fixed
//     name project.OpenFileRead("partial_arch.json") inside its admitted,
//     re-verified project handle (:55-88) - the file name is a constant of that
//     method, not an argument of the request.
//
// # Refused, with the line that makes each refusal
//
// Writes:
//
//	fs:write-file  internal/transport/project_files.go:93,147-165 -> projectfs.Project.WriteBrowserFile (browser_files.go:177-210): temp file, Sync, rename, readback. A write.
//	fs:mkdir       internal/transport/project_files.go:91,166-170 -> projectfs.Project.MakeBrowserDirectory (browser_files.go:316-358): parent.Mkdir at :346. A write.
//	fs:write-json  internal/transport/project_files.go:69-74 records that it stays at GO_OPERATION_NOT_MIGRATED because its only observed caller writes the .ai-novel storage marker, which this confined surface keeps out of a browser's reach.
//
// Grant operations:
//
//	fs:grant-read-file   web/operations.json:211 ("flexible")
//	fs:grant-write-file  web/operations.json:212 ("flexible")
//	fs:grant-mkdir       web/operations.json:213 ("flexible")
//
//	There is no server-issued external grant in a browser: the desktop mints one
//	after a native picker (src/shared/ipc-channels.ts:488 "用户选择后签发的授权")
//	and the renderer then carries only that grantId, so the target is a host path
//	the server never chose - export-service.ts:328,352,372,416 writes an export
//	through fs:grant-write-file/fs:grant-mkdir to a user-selected output directory,
//	and knowledge-service.ts:143 reads an externally picked file through
//	fs:grant-read-file. Minting that authority on a server would let any page name
//	a path outside every project root. internal/transport/project_files.go:69-74
//	states the same refusal, and no binder in internal/transport publishes any
//	fs:grant-* name at all (the only Go occurrences are 501 assertions in
//	synthetic_project_files_http_linux_test.go:360-362).
//
// Absolute-path listing:
//
//	fs:list-dir  internal/transport/editor_nav_reads.go:60-92 takes an ABSOLUTE
//	host directory in the request and derives a relative one by filepath.Rel
//	(:80-83) - a lexical containment check on a request-named host path, the one
//	comparison internal/projectfs/browser_files.go:38-52 says a browser-facing
//	server must not depend on. Its projection also carries absolute host paths
//	back to the page (internal/projectfs/read_tree.go:21-27, :132
//	Path: filepath.Join(p.path, ...)), which the relative-only file contract above
//	does not. This lane already pins it fail-closed:
//	production_editor_reads_linux_test.go:420 asserts fs:list-dir stays 501.
//
// # The fence
//
// Every request re-observes the caller's own durable lease before any project
// tree is opened (productionFSLease): the frozen three-key context is parsed
// (internal/projectsession/session.go:32-47), the caller's own cookie/tab
// identity names the owner (production_durable_linux.go:152), the project path
// must be one the startup inventory issued a catalog key for
// (production_durable_linux.go:163), and
// authorruntime.ReadLifecycle.CurrentProjectDurable
// (internal/authorruntime/durable_linux.go:258) re-proves the live lease for that
// exact owner. A stale, foreign or absent lease is refused there, before
// projectfs.Library.OpenProject pins the tree, and the pinned project is closed on
// every path. Nothing here opens a database, resolves a host path from a request
// value, or makes an admission decision.
//
// # Refusal envelopes
//
// fs:read-file keeps the family's business envelope, because the desktop answered
// it that way and the renderer branches on `success`
// (electron/controllers/fs-controller.ts:101-119, internal/transport/
// project_files.go:201-209): `{"success":false,"content":"","error":CODE}` at HTTP
// 200. fs:check-exists answered a bare boolean on the desktop and threw on a
// refusal, so it answers a transport error instead (project_files.go:99-106,
// :249-267), and fs:read-json answers the transport envelope its own binder uses
// (internal/transport/editor_checkpoint.go:33-60) - `{"success":false,"data":null}`
// only for a checkpoint that does not exist. Every code is a symbolic constant, so
// no host path, SQLite message or os error text can reach the browser
// (productionFSCode is finite).

const (
	productionFSReadFileName    = "fs:read-file"
	productionFSCheckExistsName = "fs:check-exists"
	productionFSReadJSONName    = "fs:read-json"

	// productionFSStorageMarkerName is the ONE file under the canonical storage
	// root that fs:read-json may serve. It is the name the family binder pins
	// (internal/transport/editor_checkpoint.go:47) and the name
	// authorruntime.ReadArchCheckpoint opens inside its admitted project
	// (internal/authorruntime/editor_checkpoint.go:65).
	productionFSStorageMarkerName = "partial_arch.json"

	// The wire shape of the three bound names: the frozen context is always last,
	// the claimed root immediately before it, the browser path first.
	productionFSArity      = 3
	productionFSContextPos = 2
	productionFSRootPos    = 1
	productionFSPathPos    = 0

	// productionFSMaxPathBytes is the path bound the transport lane also applies
	// (internal/transport/editor_nav_reads.go:110-113) and the ceiling
	// projectfs.BrowserRelativePathMaxBytes enforces for a relative path.
	productionFSMaxPathBytes = 4096
)

// productionFSReads lends the admitted project tree to the three browser reads.
// The adapter is stateless: every call re-observes the lease and pins the project
// for its own request, so a lane opened before the runtime exists (or after a
// refused startup) answers the symbolic PRODUCTION_CLOSED refusal instead of
// holding a handle this process no longer owns.
type productionFSReads struct {
	open *productionOpen
}

// newProductionFSReads returns the read adapter for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing at
// construction time; the library and the durable runtime are snapshotted per
// request, because Close releases them.
func newProductionFSReads(open *productionOpen) *productionFSReads {
	return &productionFSReads{open: open}
}

// Bind installs the three reads, one server.Bind per name. A nil adapter is
// refused instead of being published as handlers that can only fail, and each
// name is checked against the registry by Bind itself
// (internal/transport/server.go:73-77), so an uninventoried or already bound name
// is a startup error rather than silent dead code.
func (r *productionFSReads) Bind(server *transport.Server) error {
	if r == nil || r.open == nil || server == nil {
		return errors.New("PRODUCTION_FS_BINDER_INVALID")
	}
	for _, operation := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionFSReadFileName, r.readFile},
		{productionFSCheckExistsName, r.checkExists},
		{productionFSReadJSONName, r.readJSON},
	} {
		if err := server.Bind(operation.name, operation.handler); err != nil {
			return err
		}
	}
	return nil
}

// roots snapshots the lane's library and durable runtime under the lane lock.
// Callers never hold that lock while a file operation runs, exactly like
// productionOpen.Close captures them before releasing the lock
// (production_open_linux.go:397-413).
func (r *productionFSReads) roots() (*productionDurableOpen, *projectfs.Library, error) {
	if r == nil || r.open == nil {
		return nil, nil, productionFSHTTPError(productionFSClosedCode)
	}
	open := r.open
	open.mu.Lock()
	defer open.mu.Unlock()
	if open.closed || open.durable == nil || open.library == nil {
		return nil, nil, productionFSHTTPError(productionFSClosedCode)
	}
	return open.durable, open.library, nil
}

// productionFSLifecycle snapshots the durable runtime's lifecycle for one
// project, under the runtime's own lock, and requires the path to be one the
// startup inventory issued a catalog key for - the same inventory check
// borrowAuthorDatabase performs (production_durable_linux.go:322-331) before it
// lends anything. A browser path never becomes a key.
func (d *productionDurableOpen) productionFSLifecycle(projectPath string) (*authorruntime.ReadLifecycle, error) {
	if d == nil {
		return nil, productionFSHTTPError(productionFSClosedCode)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.lifecycle == nil {
		return nil, productionFSHTTPError(productionFSClosedCode)
	}
	if _, known := d.keyFor(projectPath); !known {
		return nil, productionFSHTTPError("PROJECT_NOT_OPEN")
	}
	return d.lifecycle, nil
}

// productionFSLease re-observes the caller's live durable lease and returns the
// lifecycle that owns it plus the owner the lease is keyed by. It is the single
// fence all three names share: the frozen context is parsed, the caller's own
// cookie/tab identity names the owner, the project is a catalog-admitted one, and
// the runtime re-proves that the exact context this request froze is still the
// context this owner holds. A stale, foreign or absent lease never reaches a
// project tree.
func (r *productionFSReads) productionFSLease(ctx context.Context, caller transport.Caller, expected projectsession.Context) (*authorruntime.ReadLifecycle, projectsession.Owner, error) {
	if ctx == nil {
		return nil, projectsession.Owner{}, productionFSHTTPError(productionFSClosedCode)
	}
	owner, err := productionOwner(caller)
	if err != nil {
		return nil, projectsession.Owner{}, productionFSHTTPError("PROJECT_SESSION_REQUIRED")
	}
	durable, _, err := r.roots()
	if err != nil {
		return nil, projectsession.Owner{}, err
	}
	lifecycle, err := durable.productionFSLifecycle(expected.ProjectPath)
	if err != nil {
		return nil, projectsession.Owner{}, err
	}
	current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return nil, projectsession.Owner{}, err
	}
	if !present || current != expected {
		return nil, projectsession.Owner{}, productionFSHTTPError("PROJECT_SESSION_STALE")
	}
	return lifecycle, owner, nil
}

// withProjectFile runs one confined read under the caller's live lease. The
// lease is re-observed first, then the project is pinned by id through the
// library, and the pinned tree is closed on every path. No database transaction
// is involved: the durable writer lock this lane already holds keeps other
// writers out, and the browser contract refuses anything the tree does not
// contain.
func (r *productionFSReads) withProjectFile(ctx context.Context, caller transport.Caller, expected projectsession.Context, action func(*projectfs.Project) error) error {
	if action == nil {
		return productionFSHTTPError(productionFSClosedCode)
	}
	if _, _, err := r.productionFSLease(ctx, caller, expected); err != nil {
		return err
	}
	_, library, err := r.roots()
	if err != nil {
		return err
	}
	project, err := library.OpenProject(expected.ProjectPath)
	if err != nil {
		return err
	}
	// The lease proved WHICH project this caller holds; the manifest proves the
	// tree on disk is still that same project. A swapped entry is a stale lease,
	// not a file refusal.
	if project.ProjectID() != expected.ProjectID {
		_ = project.Close()
		return productionFSHTTPError("PROJECT_SESSION_STALE")
	}
	actionErr := action(project)
	closeErr := project.Close()
	if actionErr != nil {
		return actionErr
	}
	return closeErr
}

// readFile serves fs:read-file: one relative path under the admitted project root,
// read through the pinned browser tree. Its refusals keep the family's business
// envelope so the renderer keeps branching on `success`.
func (r *productionFSReads) readFile(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, code := productionFSParseFileRequest(args)
	if code != "" {
		return productionFSReadFileFailure(code), nil
	}
	data, err := r.readProjectFile(ctx, caller, request)
	if err != nil {
		return productionFSReadFileFailure(productionFSCode(err)), nil
	}
	return map[string]any{"success": true, "content": string(data)}, nil
}

// checkExists serves fs:check-exists: a bare boolean, exactly as the desktop
// answered it. A missing confined path is false rather than an error
// (internal/projectfs/browser_files.go:266-270), and a refusal is the transport
// envelope the family's own handler uses (project_files.go:99-106).
func (r *productionFSReads) checkExists(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	request, code := productionFSParseFileRequest(args)
	if code != "" {
		return nil, productionFSHTTPError(code)
	}
	present, err := r.projectPathExists(ctx, caller, request)
	if err != nil {
		return nil, productionFSHTTPError(productionFSCode(err))
	}
	return present, nil
}

// readJSON serves fs:read-json for the one storage marker this lane can name
// itself. The requested path is only compared with the derived one; the bytes come
// from the runtime's fixed-name read under the admitted project handle.
func (r *productionFSReads) readJSON(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != productionFSArity {
		return nil, productionFSHTTPError("INVALID_ARGUMENTS")
	}
	expected, err := projectsession.ParseContext(args[productionFSContextPos])
	if err != nil || !productionFSCanonicalPath(expected.ProjectPath) {
		return nil, productionFSHTTPError("PROJECT_SESSION_REQUIRED")
	}
	claimed, ok := args[productionFSRootPos].(string)
	if !ok || !productionFSCanonicalPath(claimed) {
		return nil, productionFSHTTPError("PROJECT_CONTEXT_REQUIRED")
	}
	if claimed != expected.ProjectPath {
		return nil, productionFSHTTPError("PROJECT_CONTEXT_CHANGED")
	}
	// The path this lane serves is derived here, from the frozen context. The
	// request's value is a claim to compare, never a path to open: any other file
	// under the storage root - the manifest, the database - is refused here.
	marker, ok := args[productionFSPathPos].(string)
	if !ok || marker != productionFSArchCheckpointPath(expected.ProjectPath) {
		return nil, productionFSHTTPError("PROJECT_CONTEXT_REQUIRED")
	}
	lifecycle, owner, err := r.productionFSLease(ctx, caller, expected)
	if err != nil {
		return nil, productionFSHTTPError(productionFSCode(err))
	}
	wire, found, err := lifecycle.ReadArchCheckpoint(ctx, owner, expected)
	if err != nil {
		return nil, productionFSHTTPError(productionFSCode(err))
	}
	if !found {
		// The original's nullable shape for a checkpoint that was never written
		// (internal/transport/editor_checkpoint.go:54-56). Answering an error here
		// would make a first-run project look broken.
		return map[string]any{"success": false, "data": nil}, nil
	}
	if !json.Valid(wire) {
		return nil, productionFSHTTPError("FS_READ_JSON_FAILED")
	}
	return map[string]any{"success": true, "data": json.RawMessage(append([]byte(nil), wire...))}, nil
}

// productionFSFileRequest is one validated fs:read-file / fs:check-exists
// request: the caller's frozen lease and the browser path, already confined to the
// project root by projectfs.BrowserRelativePath.
type productionFSFileRequest struct {
	expected projectsession.Context
	relative string
}

// productionFSParseFileRequest checks the wire shape the two path-addressed reads
// share and returns a symbolic refusal code, empty meaning the request may be
// served. The claimed root must equal the project inside the frozen context, and
// the browser path is validated before any project is borrowed.
func productionFSParseFileRequest(args []any) (productionFSFileRequest, string) {
	if len(args) != productionFSArity {
		return productionFSFileRequest{}, "INVALID_ARGUMENTS"
	}
	expected, err := projectsession.ParseContext(args[productionFSContextPos])
	if err != nil || !productionFSCanonicalPath(expected.ProjectPath) {
		return productionFSFileRequest{}, "PROJECT_SESSION_REQUIRED"
	}
	claimed, ok := args[productionFSRootPos].(string)
	if !ok || !productionFSCanonicalPath(claimed) {
		return productionFSFileRequest{}, "PROJECT_CONTEXT_REQUIRED"
	}
	if claimed != expected.ProjectPath {
		return productionFSFileRequest{}, "PROJECT_CONTEXT_CHANGED"
	}
	path, ok := args[productionFSPathPos].(string)
	if !ok || !utf8.ValidString(path) {
		return productionFSFileRequest{}, "PROJECT_FILE_PATH_REFUSED"
	}
	// The browser path is confined BEFORE the project is borrowed: an absolute
	// path, a traversal, a hidden component or a storage-reserved name never
	// reaches a filesystem call (internal/projectfs/browser_files.go:73-91).
	relative, err := projectfs.BrowserRelativePath(path)
	if err != nil {
		return productionFSFileRequest{}, "PROJECT_FILE_PATH_REFUSED"
	}
	return productionFSFileRequest{expected: expected, relative: relative}, ""
}

// readProjectFile reads one confined file through the pinned project tree. The
// ceiling is the same 64 MiB browser budget the family binder passes
// (internal/transport/project_files.go:17, :142).
func (r *productionFSReads) readProjectFile(ctx context.Context, caller transport.Caller, request productionFSFileRequest) ([]byte, error) {
	var content []byte
	if err := r.withProjectFile(ctx, caller, request.expected, func(project *projectfs.Project) error {
		data, readErr := project.ReadBrowserFile(ctx, request.relative, projectfs.BrowserFileMaxBytes)
		content = data
		return readErr
	}); err != nil {
		return nil, err
	}
	return content, nil
}

// projectPathExists asks the pinned tree whether one confined path is a readable
// regular file or a real directory. A symlink or special file is refused by the
// browser contract itself (internal/projectfs/browser_files.go:296-302).
func (r *productionFSReads) projectPathExists(ctx context.Context, caller transport.Caller, request productionFSFileRequest) (bool, error) {
	present := false
	if err := r.withProjectFile(ctx, caller, request.expected, func(project *projectfs.Project) error {
		found, existsErr := project.BrowserPathExists(ctx, request.relative)
		present = found
		return existsErr
	}); err != nil {
		return false, err
	}
	return present, nil
}

// productionFSArchCheckpointPath is the only storage path this file serves. It is
// derived from the frozen context, never from a request value, and it names the
// same marker the family binder pins
// (internal/transport/editor_checkpoint.go:47).
func productionFSArchCheckpointPath(projectPath string) string {
	return filepath.Join(projectPath, projectfs.StorageDirectory, productionFSStorageMarkerName)
}

// productionFSCanonicalPath is this file's copy of the path grammar the transport
// lane applies to a claimed project root
// (internal/transport/editor_nav_reads.go:110-113): absolute, cleaned, bounded,
// valid UTF-8, and with no NUL or backslash. A path that fails it is a malformed
// context, not a file target.
func productionFSCanonicalPath(path string) bool {
	return path != "" && len(path) <= productionFSMaxPathBytes && utf8.ValidString(path) &&
		!strings.ContainsAny(path, "\x00\\") && filepath.IsAbs(path) && filepath.Clean(path) == path
}

// productionFSClosedCode is the symbolic refusal of a lane that was never opened
// or is already closed.
const productionFSClosedCode = "PRODUCTION_CLOSED"

// productionFSReadFileFailure is the family's business refusal envelope for
// fs:read-file (internal/transport/project_files.go:201-209, the shape the desktop
// returned at electron/controllers/fs-controller.ts:101-119). The code is symbolic
// because this lane has no i18n table and because a code cannot leak a host path.
func productionFSReadFileFailure(code string) map[string]any {
	return map[string]any{"success": false, "content": "", "error": code}
}

// productionFSSymbolicCode bounds the vocabulary a repository, runtime or
// projectfs refusal may surface: an uppercase symbolic constant, never a wrapped
// host path or an os error sentence.
var productionFSSymbolicCode = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// productionFSCode maps a refusal to one symbolic code. The mapping is finite on
// purpose: a wrapped host path, a SQLite message or an os error text must never
// reach the browser.
func productionFSCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, projectfs.ErrBrowserPathRefused):
		return "PROJECT_FILE_PATH_REFUSED"
	case errors.Is(err, projectfs.ErrTooLarge):
		return "PROJECT_FILE_TOO_LARGE"
	case errors.Is(err, projectfs.ErrUnsafeFile), errors.Is(err, projectfs.ErrUnsafeDirectory):
		return "PROJECT_FILE_UNSAFE"
	case errors.Is(err, projectfs.ErrChanged):
		return "PROJECT_FILE_CHANGED"
	case errors.Is(err, projectfs.ErrClosed):
		return "PROJECT_HANDLE_CLOSED"
	case errors.Is(err, fs.ErrNotExist):
		return "PROJECT_FILE_NOT_FOUND"
	case errors.Is(err, context.Canceled):
		return "REQUEST_CANCELED"
	case errors.Is(err, context.DeadlineExceeded):
		return "REQUEST_DEADLINE_EXCEEDED"
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil && public.Code != "" {
		return public.Code
	}
	if productionFSSymbolicCode.MatchString(err.Error()) {
		return err.Error()
	}
	return "PROJECT_FILE_FAILED"
}

// productionFSHTTPError is the transport refusal envelope for the two names that
// answer with one. The status table follows the family's own
// (internal/transport/project_files.go:251-267): a malformed request is a 400, a
// changed or stale context a 409, a missing file a 404, an unsafe entry a 403 and
// an unavailable lane a 503.
func productionFSHTTPError(code string) error {
	status := http.StatusInternalServerError
	switch code {
	case "INVALID_ARGUMENTS", "PROJECT_SESSION_REQUIRED", "PROJECT_CONTEXT_REQUIRED", "PROJECT_FILE_PATH_REFUSED":
		status = http.StatusBadRequest
	case "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED", "PROJECT_NOT_OPEN", "PROJECT_FILE_CHANGED":
		status = http.StatusConflict
	case "PROJECT_FILE_NOT_FOUND":
		status = http.StatusNotFound
	case "PROJECT_FILE_TOO_LARGE":
		status = http.StatusRequestEntityTooLarge
	case "PROJECT_FILE_UNSAFE":
		status = http.StatusForbidden
	case "REQUEST_CANCELED", "REQUEST_DEADLINE_EXCEEDED", productionFSClosedCode:
		status = http.StatusServiceUnavailable
	}
	return security.NewHTTPError(status, code)
}

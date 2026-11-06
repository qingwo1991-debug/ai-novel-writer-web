//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

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
//     The bytes come from projectfs.Project.OpenFileRead("partial_arch.json") on
//     the pinned project (internal/projectfs/project.go:356-396): the name is a
//     constant of this file, it is resolved inside the pinned storage root, and
//     the final component goes through openRegular (O_NOFOLLOW, single link,
//     project.go:218-238). That is the same call the runtime's own marker read
//     makes (internal/authorruntime/editor_checkpoint.go:59-91), and this file
//     keeps the double read and the invalid-JSON-is-absent rule (:38-50) under the
//     same 1 MiB bound (:17); it cannot call
//     authorruntime.ReadLifecycle.ReadArchCheckpoint itself, because that method
//     asserts the READ-ONLY admitted handle (:31-34) while this lane's durable
//     lease admits *admittedAuthor (internal/authorruntime/durable_linux.go:194)
//     and is answered with AUTHOR_READ_INVALID (internal/authorruntime/
//     read_linux.go:24) - a proof this lane can never mint.
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
//	fs:grant-read-file   production_fs_grants_linux.go, internal/librarypath
//	fs:grant-write-file  the same binder
//	fs:grant-mkdir       the same binder
//
//	These are served, and the reason the older note here was wrong is worth
//	keeping: it argued that a server cannot mint the desktop's grant without
//	letting a page name a path outside every project root. That is true of a
//	grant over an arbitrary host path, and it is why this binder never accepts
//	one. The browser gets the same capability over a path the SERVER resolved:
//	the request names a symbolic root and a relative path, every component is
//	Lstat'd against a physical root, any symlink is an escape even when it points
//	back inside, and the token is bound to the caller, the lease epoch and an
//	expiry, with a MAC over its own fields. Write and mkdir moreover refuse author
//	data outright, so an export grant cannot be turned into an edit of the
//	project's database. The chooser that would call these is front-end work; the
//	backend capability is here.
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
// tree is opened (productionFSFence): the frozen three-key context is parsed
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
	// projectfs.Project.OpenFileRead resolves relative to the pinned storage root
	// (internal/projectfs/project.go:356-396).
	productionFSStorageMarkerName = "partial_arch.json"

	// productionFSMarkerMaxBytes is the ceiling the runtime's own marker read
	// applies (internal/authorruntime/editor_checkpoint.go:17). A larger marker is
	// refused rather than served truncated.
	productionFSMarkerMaxBytes = 1 << 20

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

// productionFSHolds proves, for one frozen context, the three properties a file
// read needs and nothing more: the durable runtime is still open, the project is
// one the startup inventory issued a catalog key for - the same inventory check
// borrowAuthorDatabase performs (production_durable_linux.go:322-331) before it
// lends anything, so a browser path never becomes a key - and this owner still
// holds exactly this context. It lends no handle: this file reads the marker
// through the pinned project tree, not through a runtime borrow.
func (d *productionDurableOpen) productionFSHolds(ctx context.Context, owner projectsession.Owner, expected projectsession.Context) error {
	if d == nil || ctx == nil {
		return productionFSHTTPError(productionFSClosedCode)
	}
	d.mu.Lock()
	lifecycle, closed := d.lifecycle, d.closed
	_, known := d.keyFor(expected.ProjectPath)
	d.mu.Unlock()
	if closed || lifecycle == nil {
		return productionFSHTTPError(productionFSClosedCode)
	}
	if !known {
		return productionFSHTTPError("PROJECT_NOT_OPEN")
	}
	// CurrentProjectDurable re-verifies the retained writer lock and the live
	// handle before it reports the context (internal/projectsession/
	// active.go:282-311), so a swapped project or a lost lock is refused here.
	current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return err
	}
	if !present || current != expected {
		return productionFSHTTPError("PROJECT_SESSION_STALE")
	}
	return nil
}

// productionFSFence re-observes the caller's live durable lease. It is the single
// fence all three names share: the caller's own cookie/tab identity names the
// owner, the runtime is required to be open, the project is a catalog-admitted
// one, and the exact context this request froze must still be the context this
// owner holds. A stale, foreign or absent lease never reaches a project tree.
func (r *productionFSReads) productionFSFence(ctx context.Context, caller transport.Caller, expected projectsession.Context) error {
	owner, err := productionOwner(caller)
	if err != nil {
		return productionFSHTTPError("PROJECT_SESSION_REQUIRED")
	}
	durable, _, err := r.roots()
	if err != nil {
		return err
	}
	return durable.productionFSHolds(ctx, owner, expected)
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
	if err := r.productionFSFence(ctx, caller, expected); err != nil {
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
// answered it. A path the tree does not contain is false rather than an error
// (internal/projectfs/browser_files.go:266-270, :292-294), and a refusal is the
// transport envelope the family's own handler uses (project_files.go:99-106).
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
	wire, found, err := r.readArchCheckpoint(ctx, caller, expected)
	if err != nil {
		return nil, productionFSHTTPError(productionFSCode(err))
	}
	if !found {
		// The original's nullable shape for a checkpoint that was never written
		// (internal/transport/editor_checkpoint.go:54-56). Answering an error here
		// would make a first-run project look broken.
		return map[string]any{"success": false, "data": nil}, nil
	}
	return map[string]any{"success": true, "data": wire}, nil
}

// readArchCheckpoint reads the fixed storage marker twice through the pinned
// project storage. The two reads must agree, so a replacement or an in-place edit
// between them is refused rather than half-served - the same discipline
// authorruntime.ReadArchCheckpoint applies
// (internal/authorruntime/editor_checkpoint.go:37-54), which this file cannot
// call here because that method asserts the READ-ONLY admitted handle (:31-34)
// and this lane's durable lease admits *admittedAuthor
// (internal/authorruntime/durable_linux.go:194).
func (r *productionFSReads) readArchCheckpoint(ctx context.Context, caller transport.Caller, expected projectsession.Context) (json.RawMessage, bool, error) {
	var first, second []byte
	var firstFound, secondFound bool
	if err := r.withProjectFile(ctx, caller, expected, func(project *projectfs.Project) error {
		var err error
		if first, firstFound, err = productionFSMarkerBytes(ctx, project); err != nil {
			return err
		}
		second, secondFound, err = productionFSMarkerBytes(ctx, project)
		return err
	}); err != nil {
		return nil, false, err
	}
	if firstFound != secondFound || !bytes.Equal(first, second) {
		return nil, false, productionFSHTTPError("PROJECT_FILE_CHANGED")
	}
	if !firstFound || !json.Valid(first) {
		// A marker the runtime itself would not hand out - because it is absent,
		// or because it does not hold JSON (internal/authorruntime/
		// editor_checkpoint.go:48-50) - is the nullable answer, not a failure.
		return nil, false, nil
	}
	return json.RawMessage(append([]byte(nil), first...)), true, nil
}

// productionFSMarkerBytes reads the one storage-reserved name this lane may
// serve, out of an already pinned project. The name is a constant of this file;
// projectfs.Project.OpenFileRead resolves it inside the pinned storage root
// (internal/projectfs/project.go:356-396) and refuses a database name, a
// non-regular or multi-link file (project.go:218-238), so a symlink planted at
// that name is refused rather than followed.
func productionFSMarkerBytes(ctx context.Context, project *projectfs.Project) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	file, err := project.OpenFileRead(productionFSStorageMarkerName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if info.Size() > productionFSMarkerMaxBytes {
		return nil, false, productionFSHTTPError("PROJECT_FILE_TOO_LARGE")
	}
	data, err := io.ReadAll(io.LimitReader(file, productionFSMarkerMaxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > productionFSMarkerMaxBytes {
		return nil, false, productionFSHTTPError("PROJECT_FILE_TOO_LARGE")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return data, true, nil
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
		if errors.Is(existsErr, fs.ErrNotExist) {
			// A missing PARENT surfaces as not-exist from the pinned walk
			// (internal/projectfs/project.go:62-66), not as a false answer. The
			// desktop asked fs:check-exists exactly to learn this - "does the file
			// I am about to create already exist?" - so fold it back into that
			// boolean. Nothing is opened or created by doing so, and the caller
			// still learns nothing about the filesystem beyond yes/no.
			return nil
		}
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

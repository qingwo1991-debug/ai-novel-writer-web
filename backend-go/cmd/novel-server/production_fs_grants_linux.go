//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's GRANT slice of the `fs:*` family in
// web/operations.json:204-213. It binds exactly three names:
//
//	fs:grant-read-file   arity 2  [grantId, relativePath?]
//	fs:grant-write-file  arity 3  [grantId, relativePath, content]
//	fs:grant-mkdir       arity 2  [grantId, relativePath]
//
// with the wire shapes and the answers of src/shared/ipc-channels.ts:489-502, and
// the renderer is unchanged: a read answers `{success, content, error}` at HTTP
// 200, a write answers `{success}` or `{success:false, commitState, error}`, and a
// mkdir answers `{success, error}`. Those are the desktop's business envelopes
// (electron/controllers/external-file-grant-controller.ts:114-180: every refusal
// there is caught and returned, not thrown), so the page's existing branching on
// `success` keeps working. `commitState` is `committed` on success and
// `not_committed` on every refusal except the one case the field exists for: a
// replacement that may have landed but could not be re-verified answers `unknown`
// and never `not_committed`.
//
// # Why the desktop model cannot be copied
//
// On the desktop the grantId is minted by the main process after a native picker
// and carries the absolute host path the author chose
// (electron/services/external-file-grant-service.ts:26-52, :113-140); the renderer
// then sends only that opaque id plus a relative path. A browser has no picker, so
// the two halves of that model are replaced, not ported:
//
//   - The AUTHORITY is an allowlist (internal/librarypath): the configured library
//     root, plus the project tree the caller's own durable lease currently names.
//     Nothing else is addressable. There is no request-supplied absolute path
//     anywhere on this surface, and the answer never echoes the caller's string.
//   - The IDENTITY is an opaque token minted by librarypath.Registry and bound to
//     the caller's cookie session and tab, with a TTL, a usage budget, a
//     single-writer-safe usage record and revocation. It is 256 bits of
//     crypto/rand, MAC'd per record with a per-process key, at least as hard to
//     guess as the review-context handles the same repository uses for an opaque
//     handle (internal/reviewrevprep/store.go).
//
// Both halves arrive from the same place the desktop minted its grants: the
// browser's own chooser. The binder that serves `dialog:select-*` and the bounded
// listing mints through productionFSGrants.Grants() - one process-wide registry,
// so every binder constructed by newProductionFSGrants shares it - and this file
// redeems. The registry exists before that binder does, so the three names below
// are served (and refuse honestly) today rather than answering 501.
//
// # A grant is not a capability to write author data
//
// Do not "simplify" this. A token authorises a PATH. It is not authority over the
// author's content, and it never stands in for the author database's
// single-writer lease: writing author data still goes through the operation that
// owns that data, under the lease that operation already holds. Concretely, a
// write or an mkdir whose target is a project tree - the admitted project root
// (KindProject) or any directory below the library root that holds a project
// manifest - is refused with FS_GRANT_AUTHOR_DATA by
// librarypath.Target.requireOutput before a single byte is written, and the caller
// is expected to use the owning operation. What a browser grant may write is an
// OUTPUT: a file below a configured root that is not a project tree, which is the
// export lane the desktop served through the user's chosen directory
// (src/services/export-service.ts:328,352,372,416). Reads are not restricted the
// same way, because importing a manuscript (src/services/knowledge-service.ts:143)
// writes nothing.
//
// # Never a path from the body
//
// `relativePath` is an extent inside the grant's own resolved target, never a
// target: librarypath.JoinRelative validates it with the same grammar as the
// allowlist (no absolute path, no `..`, no backslash, no percent-encoded
// separator, no hidden component, bounded length) and the combined path is then
// re-resolved through this request's allowlist, so the physical identity - and a
// symlink planted anywhere on the way - is re-proved at use time. A token minted
// for a root that this request's allowlist no longer holds is refused
// (LIBRARY_PATH_ROOT_UNKNOWN, answered as FS_GRANT_PATH_REFUSED), which is exactly
// "no root known to belong to the admitted project or library".
//
// # The allowlist per request
//
// library root  - always, while the lane is open; it is the configured root every
//                 project directory is a child of.
// project root  - only while this caller's cookie session and tab still hold a
//                 live durable lease (productionFSGrantProject), and only for a
//                 project the startup inventory issued a catalog key for.
//
// A closed lane, a missing lifecycle or a lease naming an unknown project is a
// refusal, never an empty allowlist that happens to answer "not found".
//
// # Refusal vocabulary
//
// Every code is symbolic and finite (productionFSGrantCode), so no host path, no
// os error sentence and no SQLite message can reach the browser.
//
// # Wiring
//
// production_open_linux.go's []func() error binder table (the entry that is
// `func() error { return newProductionFSReads(open).Bind(server) }`) gains one
// line beside it:
//
//	func() error { return newProductionFSGrants(open).Bind(server) },
//
// which takes productionServedOperations from 162 to 165. Two neighbours then say
// something that is no longer true and must move in the same change: the three
// names come out of production_fs_reads_linux_test.go's unbound-501 list, and the
// "there is no server-issued external grant in a browser" paragraph in
// production_fs_reads_linux.go now describes the desktop-only past - the browser
// answer is the allowlist plus a token, not a host path from a picker.
const (
	productionFSGrantReadName  = "fs:grant-read-file"
	productionFSGrantWriteName = "fs:grant-write-file"
	productionFSGrantMkdirName = "fs:grant-mkdir"

	// The wire arity of each name. fs:grant-read-file's second argument is
	// optional on the wire (knowledge-service.ts:143 sends the grantId alone), so
	// its arity is the maximum, not the requirement.
	productionFSGrantReadArity  = 2
	productionFSGrantWriteArity = 3
	productionFSGrantMkdirArity = 2

	// productionFSGrantMaxBytes is the one body and file ceiling this surface
	// holds in memory, the same 64 MiB budget projectfs.BrowserFileMaxBytes applies
	// to a browser file operation.
	productionFSGrantMaxBytes = projectfs.BrowserFileMaxBytes

	// productionFSGrantCommitUnknownCode is the one code that makes the answer
	// report commitState "unknown": the bytes may already be the file's content.
	productionFSGrantCommitUnknownCode = "FS_GRANT_COMMIT_UNKNOWN"
)

// productionFSGrants lends the allowlist and the token store to the three grant
// names. It holds no project handle and no database: every request re-observes the
// caller's lease, rebuilds the allowlist and pins nothing beyond the one directory
// a filesystem call needs.
type productionFSGrants struct {
	open   *productionOpen
	grants *librarypath.Registry
}

// productionFSGrantUnavailable is the refusal of a lane whose per-process signing
// key could not be drawn from crypto/rand.
var productionFSGrantUnavailable = errors.New("FS_GRANT_SECRET_UNAVAILABLE")

// The process-wide token registry. It is one instance per process on purpose: a
// token minted by the chooser binder has to be redeemable by this binder, and both
// are constructed from the same lane. It is created on first use, never at init,
// so a lane that fails to start leaves nothing behind.
var (
	productionFSGrantOnce        sync.Once
	productionFSGrantRegistry    *librarypath.Registry
	productionFSGrantRegistryErr error
)

// productionFSGrantsRegistry returns the process-wide registry, minting its
// per-process MAC key from crypto/rand on first use. An entropy failure is kept as
// the error every caller sees, so the three names refuse rather than run with an
// unkeyed store.
func productionFSGrantsRegistry() (*librarypath.Registry, error) {
	productionFSGrantOnce.Do(func() {
		var key [32]byte
		if _, err := rand.Read(key[:]); err != nil {
			productionFSGrantRegistryErr = productionFSGrantUnavailable
			return
		}
		productionFSGrantRegistry, productionFSGrantRegistryErr = librarypath.NewRegistry(key[:], librarypath.RegistryOptions{})
	})
	return productionFSGrantRegistry, productionFSGrantRegistryErr
}

// newProductionFSGrants returns the grant adapter for the lane built by
// productionHandlerWithContract. It resolves nothing and opens nothing at
// construction time.
func newProductionFSGrants(open *productionOpen) *productionFSGrants {
	registry, _ := productionFSGrantsRegistry()
	return &productionFSGrants{open: open, grants: registry}
}

// Grants exposes the same process-wide registry the three names redeem from, so
// the browser chooser that serves `dialog:select-*` can mint the token this file
// consumes. It returns nil when the registry could not be created; a chooser must
// treat that as "no capability available" and answer its own refusal.
func (g *productionFSGrants) Grants() *librarypath.Registry {
	if g == nil {
		return nil
	}
	return g.grants
}

// Bind installs the three grant names, one server.Bind per name (the exported
// single-name binder, internal/transport/server.go:73-77), so an uninventoried or
// already bound name is a startup error rather than silent dead code. A nil
// adapter or an unkeyed registry is refused instead of being published as handlers
// that can only fail.
func (g *productionFSGrants) Bind(server *transport.Server) error {
	if g == nil || g.open == nil || server == nil || g.grants == nil {
		return errors.New("PRODUCTION_FS_GRANT_BINDER_INVALID")
	}
	for _, operation := range []struct {
		name    string
		handler transport.Operation
	}{
		{productionFSGrantReadName, g.readFile},
		{productionFSGrantWriteName, g.writeFile},
		{productionFSGrantMkdirName, g.mkdir},
	} {
		if err := server.Bind(operation.name, operation.handler); err != nil {
			return err
		}
	}
	return nil
}

// lane snapshots the lane's durability and its library under the lane lock, the
// same way productionFSReads.roots does: the lock is never held while a file
// operation runs.
func (g *productionFSGrants) lane() (*productionDurableOpen, *projectfs.Library, error) {
	if g == nil || g.open == nil {
		return nil, nil, productionFSHTTPError(productionFSClosedCode)
	}
	open := g.open
	open.mu.Lock()
	defer open.mu.Unlock()
	if open.closed || open.durable == nil || open.library == nil {
		return nil, nil, productionFSHTTPError(productionFSClosedCode)
	}
	return open.durable, open.library, nil
}

// productionFSGrantProject reports the project tree this caller's own lease
// currently names, if any, without borrowing it. It proves three things and lends
// nothing: the runtime is still open, the owner really holds an admitted handle
// (CurrentProjectDurable re-verifies the retained writer lock,
// internal/projectsession/active.go:282-311), and the path it names is one the
// startup inventory issued a catalog key for - the same inventory check
// borrowAuthorDatabase performs (production_durable_linux.go:322-331), so a path
// can never become a key here.
func (d *productionDurableOpen) productionFSGrantProject(ctx context.Context, owner projectsession.Owner) (projectsession.Context, bool, error) {
	if d == nil || ctx == nil {
		return projectsession.Context{}, false, productionFSHTTPError(productionFSClosedCode)
	}
	d.mu.Lock()
	lifecycle, closed := d.lifecycle, d.closed
	d.mu.Unlock()
	if closed || lifecycle == nil {
		return projectsession.Context{}, false, productionFSHTTPError(productionFSClosedCode)
	}
	current, present, err := lifecycle.CurrentProjectDurable(ctx, owner)
	if err != nil {
		return projectsession.Context{}, false, err
	}
	if !present {
		return projectsession.Context{}, false, nil
	}
	d.mu.Lock()
	_, known := d.keyFor(current.ProjectPath)
	d.mu.Unlock()
	if !known {
		return projectsession.Context{}, false, productionFSHTTPError("PROJECT_NOT_OPEN")
	}
	return current, true, nil
}

// roots builds this request's allowlist and the caller binding the tokens are
// compared against. The binding is the TAB IDENTITY - the cookie session and the
// tab, exactly as projectsession.Owner spells them - and it is derived from the
// authenticated transport caller, never from an argument. The live project lease
// beside it is looked up under the LEASE OWNER instead (this lane keys it by
// account), so two tabs of one author both hold the project while neither can
// redeem the other's file grant.
func (g *productionFSGrants) roots(ctx context.Context, caller transport.Caller) (librarypath.Roots, librarypath.Binding, error) {
	tab, err := productionTabOwner(caller)
	if err != nil {
		return librarypath.Roots{}, librarypath.Binding{}, productionFSHTTPError("PROJECT_SESSION_REQUIRED")
	}
	binding := librarypath.Binding{SessionID: tab.SessionID, ClientID: tab.ClientID}
	leaseOwner, leaseErr := productionLeaseOwner(caller)
	if leaseErr != nil {
		return librarypath.Roots{}, librarypath.Binding{}, productionFSHTTPError("PROJECT_SESSION_REQUIRED")
	}
	durable, library, err := g.lane()
	if err != nil {
		return librarypath.Roots{}, librarypath.Binding{}, err
	}
	candidates := []librarypath.Root{{
		ID:   librarypath.LibraryRootID,
		Kind: librarypath.KindLibrary,
		Path: library.Path(),
	}}
	current, present, err := durable.productionFSGrantProject(ctx, leaseOwner)
	if err != nil {
		return librarypath.Roots{}, librarypath.Binding{}, err
	}
	if present {
		candidates = append(candidates, librarypath.Root{
			ID:   librarypath.ProjectRootID,
			Kind: librarypath.KindProject,
			Path: current.ProjectPath,
		})
	}
	roots, err := librarypath.NewRoots(candidates)
	if err != nil {
		// A configured root that is not a physical directory is a configuration
		// refusal, not a missing file: fail closed with a symbolic code.
		return librarypath.Roots{}, librarypath.Binding{}, productionFSHTTPError("PROJECT_CONTEXT_REQUIRED")
	}
	return roots, binding, nil
}

// resolve is the one path from a request to an operation, and the order of its
// three steps is the contract: admit the caller and the token without spending
// anything, validate the request's extent as a path inside the grant's own target,
// and only then spend one use - which re-checks every rule atomically. A refused
// path therefore costs the caller nothing and answers its own code instead of
// FS_GRANT_EXHAUSTED, and a last use two requests race over is refused rather than
// double spent. That is the desktop's order too (electron/services/
// external-file-grant-service.ts:265-303: the path is validated at :287-299, the
// use is decremented at :300-303 only after it, and its controller calls the same
// resolver twice - once consuming, once not - at
// electron/controllers/external-file-grant-controller.ts:66-82).
func (g *productionFSGrants) resolve(ctx context.Context, caller transport.Caller, token string, required librarypath.Op, relative string) (librarypath.Grant, librarypath.Target, error) {
	roots, binding, err := g.roots(ctx, caller)
	if err != nil {
		return librarypath.Grant{}, librarypath.Target{}, err
	}
	if g.grants == nil {
		return librarypath.Grant{}, librarypath.Target{}, productionFSGrantUnavailable
	}
	// 1. Who is calling, what the token may do, and whether it has budget left -
	//    asked, not spent. No filesystem call has happened yet.
	grant, err := g.grants.Peek(token, binding, required)
	if err != nil {
		return librarypath.Grant{}, librarypath.Target{}, err
	}
	// 2. The request's extent, validated as a path inside the grant's own target
	//    and re-resolved through this request's allowlist, so a traversal, an
	//    absolute path and a symlink are all refused before the budget is touched.
	combined, err := librarypath.JoinRelative(grant.Target.Relative, relative)
	if err != nil {
		return librarypath.Grant{}, librarypath.Target{}, err
	}
	target, err := roots.Resolve(grant.Target.RootID, combined)
	if err != nil {
		return librarypath.Grant{}, librarypath.Target{}, err
	}
	// 3. Now, and only now, one use. Spend re-runs every admission check under the
	//    lock that mutates the record, so the peek above cannot have handed out a
	//    use that is already gone.
	if _, err := g.grants.Spend(token, binding, required); err != nil {
		return librarypath.Grant{}, librarypath.Target{}, err
	}
	return grant, target, nil
}

// readFile serves fs:grant-read-file. The bytes come from the grant's resolved
// target, read through a pinned parent directory, bounded by the same 64 MiB
// budget as fs:read-file, and refused unless they are text.
//
// A refusal here is a refusal of the OPERATION, so it comes after the use was
// spent: the path was real, the caller asked for something the file cannot do
// (too large, not text), and the desktop answers the same way - its resolve
// consumes the use before the fs call that can still fail
// (electron/controllers/external-file-grant-controller.ts:115-124).
//
// A request that creates AND writes spends one use, not two: the wire here is one
// call, where the desktop's controller resolves "write" and then "create"
// separately and so decrements twice
// (electron/controllers/external-file-grant-controller.ts:127-152). The budget
// counts browser requests, not the internal checks one request happens to need.
func (g *productionFSGrants) readFile(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	token, relative, ok := productionFSGrantRequest(args, productionFSGrantReadArity)
	if !ok {
		return productionFSReadFileFailure("INVALID_ARGUMENTS"), nil
	}
	if err := ctx.Err(); err != nil {
		return productionFSReadFileFailure(productionFSGrantCode(err)), nil
	}
	_, target, err := g.resolve(ctx, caller, token, librarypath.OpRead, relative)
	if err != nil {
		return productionFSReadFileFailure(productionFSGrantCode(err)), nil
	}
	data, err := target.ReadBytes(productionFSGrantMaxBytes)
	if err != nil {
		return productionFSReadFileFailure(productionFSGrantCode(err)), nil
	}
	return map[string]any{"success": true, "content": string(data)}, nil
}

// writeFile serves fs:grant-write-file. A new file needs the grant's create
// permission as well as its write permission - the desktop's separate "create"
// operation, electron/controllers/external-file-grant-controller.ts:139-152 - so a
// write-only grant can replace an existing file but cannot bring a new one into
// being. The target is an output by construction: requireOutput refuses author
// data before anything is written.
func (g *productionFSGrants) writeFile(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != productionFSGrantWriteArity {
		return productionFSGrantWriteFailure("INVALID_ARGUMENTS"), nil
	}
	token, ok := args[0].(string)
	if !ok || token == "" {
		return productionFSGrantWriteFailure("INVALID_ARGUMENTS"), nil
	}
	relative, ok := args[1].(string)
	if !ok {
		return productionFSGrantWriteFailure("INVALID_ARGUMENTS"), nil
	}
	content, ok := args[2].(string)
	if !ok {
		return productionFSGrantWriteFailure("INVALID_ARGUMENTS"), nil
	}
	if int64(len(content)) > productionFSGrantMaxBytes {
		return productionFSGrantWriteFailure("FS_GRANT_FILE_TOO_LARGE"), nil
	}
	if err := ctx.Err(); err != nil {
		return productionFSGrantWriteFailure(productionFSGrantCode(err)), nil
	}
	grant, target, err := g.resolve(ctx, caller, token, librarypath.OpWrite, relative)
	if err != nil {
		return productionFSGrantWriteFailure(productionFSGrantCode(err)), nil
	}
	create := grant.Ops.Permits(librarypath.OpMkdir)
	if err := target.WriteOutput([]byte(content), create); err != nil {
		return productionFSGrantWriteFailure(productionFSGrantCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// mkdir serves fs:grant-mkdir. The ancestors of the relative path are created one
// component at a time through a pinned parent, and a target that is author data is
// refused before the first mkdir.
//
// A request with missing ancestors therefore makes several mkdir calls, and it is
// still ONE redemption: the grant's use counts requests, not path components - the
// desktop's mkdir spends one use for one resolveGrantPath call regardless of how
// deep the target is (electron/controllers/external-file-grant-controller.ts:
// 163-170). The refusal to create more than the path names is the entry cap and the
// component grammar, not the budget.
func (g *productionFSGrants) mkdir(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	token, relative, ok := productionFSGrantRequest(args, productionFSGrantMkdirArity)
	if !ok {
		return productionFSGrantFailure("INVALID_ARGUMENTS"), nil
	}
	if err := ctx.Err(); err != nil {
		return productionFSGrantFailure(productionFSGrantCode(err)), nil
	}
	_, target, err := g.resolve(ctx, caller, token, librarypath.OpMkdir, relative)
	if err != nil {
		return productionFSGrantFailure(productionFSGrantCode(err)), nil
	}
	if err := target.MakeDirectory(); err != nil {
		return productionFSGrantFailure(productionFSGrantCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// productionFSGrantRequest reads the [grantId, relativePath?] shape two of the
// three names share. The relative path is optional - knowledge-service.ts:143
// sends the grantId alone, meaning "the target this grant already names" - and a
// non-string in either position is a malformed request, not an empty path.
func productionFSGrantRequest(args []any, arity int) (string, string, bool) {
	if len(args) < 1 || len(args) > arity {
		return "", "", false
	}
	token, ok := args[0].(string)
	if !ok || token == "" {
		return "", "", false
	}
	if len(args) == 1 {
		return token, "", true
	}
	relative, ok := args[1].(string)
	if !ok {
		return "", "", false
	}
	return token, relative, true
}

// productionFSGrantFailure is the business refusal envelope of fs:grant-mkdir.
// The read side reuses productionFSReadFileFailure and the write side has its own
// commitState envelope, so all three keep exactly the shape their renderer type
// declares (src/shared/ipc-channels.ts:489-502).
func productionFSGrantFailure(code string) map[string]any {
	return map[string]any{"success": false, "error": code}
}

// productionFSGrantWriteFailure is the write envelope. `commitState` is
// `not_committed` for every refusal that did not reach the filesystem, and
// `unknown` for the one that may have landed; reporting a possibly-landed
// replacement as not_committed would invite a retry over the author's own bytes.
func productionFSGrantWriteFailure(code string) map[string]any {
	commitState := "not_committed"
	if code == productionFSGrantCommitUnknownCode {
		commitState = "unknown"
	}
	return map[string]any{"success": false, "commitState": commitState, "error": code}
}

// productionFSGrantCode maps a refusal to one symbolic code. The table is finite
// on purpose: a wrapped host path, an os error sentence or a SQLite message must
// never reach the browser. The fallback is productionFSCode, which already knows
// the transport envelope and the symbolic-failure rule.
func productionFSGrantCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, librarypath.ErrGrantUnknown):
		return "FS_GRANT_UNKNOWN"
	case errors.Is(err, librarypath.ErrGrantExpired):
		return "FS_GRANT_EXPIRED"
	case errors.Is(err, librarypath.ErrGrantRevoked):
		return "FS_GRANT_REVOKED"
	case errors.Is(err, librarypath.ErrGrantExhausted):
		return "FS_GRANT_EXHAUSTED"
	case errors.Is(err, librarypath.ErrCallerMismatch):
		return "FS_GRANT_CALLER_MISMATCH"
	case errors.Is(err, librarypath.ErrGrantOperationDenied):
		return "FS_GRANT_OPERATION_DENIED"
	case errors.Is(err, librarypath.ErrGrantLimit):
		return "FS_GRANT_LIMIT"
	case errors.Is(err, librarypath.ErrBindingInvalid):
		return "PROJECT_SESSION_REQUIRED"
	case errors.Is(err, librarypath.ErrAuthorData):
		return "FS_GRANT_AUTHOR_DATA"
	case errors.Is(err, librarypath.ErrSymlinkEscape):
		return "FS_GRANT_PATH_ESCAPE"
	case errors.Is(err, librarypath.ErrPathAbsolute),
		errors.Is(err, librarypath.ErrPathSeparator),
		errors.Is(err, librarypath.ErrPathEncoded),
		errors.Is(err, librarypath.ErrPathTraversal),
		errors.Is(err, librarypath.ErrPathHidden),
		errors.Is(err, librarypath.ErrPathInvalid),
		errors.Is(err, librarypath.ErrRootUnknown),
		errors.Is(err, librarypath.ErrTargetInvalid),
		errors.Is(err, librarypath.ErrRootInvalid):
		return "FS_GRANT_PATH_REFUSED"
	case errors.Is(err, librarypath.ErrPathNotDirectory):
		return "FS_GRANT_NOT_DIRECTORY"
	case errors.Is(err, librarypath.ErrTooLarge):
		return "FS_GRANT_FILE_TOO_LARGE"
	case errors.Is(err, librarypath.ErrNotText):
		return "FS_GRANT_FILE_NOT_TEXT"
	case errors.Is(err, librarypath.ErrUnsafeEntry):
		return "FS_GRANT_PATH_UNSAFE"
	case errors.Is(err, librarypath.ErrCommitUnknown):
		return productionFSGrantCommitUnknownCode
	case errors.Is(err, librarypath.ErrChanged), errors.Is(err, librarypath.ErrPathUnavailable):
		return "FS_GRANT_PATH_CHANGED"
	case errors.Is(err, fs.ErrNotExist):
		return "FS_GRANT_FILE_NOT_FOUND"
	case errors.Is(err, productionFSGrantUnavailable):
		return "FS_GRANT_UNAVAILABLE"
	}
	return productionFSCode(err)
}

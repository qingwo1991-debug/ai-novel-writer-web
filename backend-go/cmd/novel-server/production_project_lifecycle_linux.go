//go:build linux

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// Project lifecycle for the production lane: `project:delete` is served here.
// `project:create` is served by production_project_create_linux.go, which mints
// the pinned v7 database and registers the new project with the live catalog.
// The two halves are explained in that order below, because what a delete may
// touch is decided by what a create could have written.
//
// WHAT project:delete REMOVES
//
// The reference is electron/controllers/project-controller.ts:907-1000. Before
// it removes anything it proves the request is the caller's own project session
// (electron/services/project-access.ts:290-310: the request path is the session
// root and the manifest projectId is the session projectId), closes the author
// database, removes the WHOLE project directory
// (project-controller.ts:951 `removeDirectoryWithWindowsRetry(resolvedPath)`,
// itself in electron/utils/remove-directory.ts), decides the answer from the
// disk fact instead of from the call's own optimism
// (project-controller.ts:958 `const directoryDeleted = !fs.existsSync(resolvedPath)`),
// drops the recent-project entry (project-controller.ts:986) and invalidates the
// current session. It answers `{success, directoryDeleted, databaseRestored,
// error?, warning?}` (src/shared/ipc-channels.ts:432) and never throws: every
// refusal is a business answer, which is why this binder answers 200 too.
//
// This file removes exactly that tree: the canonical project root with its
// `.ai-novel/project.json` manifest, `.ai-novel/project.db`, `.ai-novel/prompts`
// and everything else the author put there, plus a `.vela` sibling if one is
// present (a canonical project may never carry one:
// internal/projectfs/project.go:280-290 admits on `.ai-novel` and refuses a
// leftover `.vela`). The whole root goes because the manifest IS the project
// identity: a surviving `.ai-novel` is still admitted by project:open, so a
// partial removal would leave a project that opens and cannot be deleted twice.
//
// # DELETE REFUSES RATHER THAN DELETES UNDER A WRITER
//
// SQLite keeps writing to an unlinked file, so a delete that ran beside a live
// writer could answer "deleted" while an admitted handle still held the rows,
// and a concurrent project:open/close would then race the removal. Deletion is
// therefore serialised behind the same admission everything else uses:
// internal/projectsession/active.go:86-100 refuses a second owner on the same
// project path, so the only live lease on this project - if any - is the
// caller's own. Consequently:
//
//   - the caller's lease must be live and exactly the frozen context
//     (active.go:282-311), else PROJECT_SESSION_STALE;
//   - the handle goes away through the runtime's own close
//     (authorruntime/durable_linux.go:249 -> projectsession.CloseProject,
//     active.go:315-370), which fences new work and waits for the in-flight work
//     to drain before the handle is dropped, so the removal never races a borrow;
//   - only then is a file renamed or removed.
//
// No borrowAuthorDatabase on purpose: a delete performs no SQL, and borrowing
// the admitted handle in order to unlink the file it has open is the exact
// pattern this fence exists to prevent. The borrow those functions need - the
// caller's live, exactly-frozen lease under the process-wide writer lease - is
// the same admission, taken through CurrentProjectDurable/CloseProjectDurable.
//
// WHY project:create IS NOT BOUND
//
// `project:create` keeps failing closed at GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116) because this tree has no honest way to
// produce a project the rest of this lane would then admit. Two pieces are
// genuinely missing, and neither may be mocked from inside a binder:
//
//  1. There is no source of the v7 schema to mint a database from.
//     admission.PinnedSchemaContract (internal/admission/pinned_contract.go:11-19,
//     `//go:embed recognized-schemas-v7.json`) carries FINGERPRINTS only, and
//     projectaccess.AdmitAuthor (internal/projectaccess/admit_author.go:56)
//     admits nothing but user_version 7 plus a recognised fingerprint. The DDL
//     that produces those bytes exists only as TypeScript
//     (electron/migrations/baseline-schema.ts, driven by runner.migrateSchema),
//     is materialised only by the CI-only contract export
//     (backend-go/contract/export-sqlite-contract.mjs, which asserts
//     GITHUB_ACTIONS === 'true') and is read back only by the CI-only fixture
//     seeder (backend-go/cmd/production-fixture/main.go, -baseline). The upgrade
//     lanes (internal/admission/upgrade*.go) only apply deltas to an already
//     recognised file, and internal/projectfs states its own boundary: "It does
//     not create projects, migrate data, or establish SQLite validity"
//     (internal/projectfs/manifest.go:1-2). Hand-writing the v7 DDL
//     here would mint a database whose fingerprint this same file then asks the
//     lane to trust - that is the fake, not the fix.
//  2. There is no way to register a new project with the live catalog.
//     project:open resolves a path only through the keys the startup harvest
//     wrote (production_durable_linux.go:161-166 keyFor ->
//     projectcatalog.Store.Resolve), and projectcatalog.Store.Register
//     (internal/projectcatalog/catalog_linux.go:428) is reachable only from
//     newProductionDurable (production_durable_linux.go:74-84), because
//     authorruntime.ReadLifecycle keeps its catalog private
//     (internal/authorruntime/lifecycle_linux.go:23-27). A project created
//     inside a binder would sit on disk and still be unopenable for the rest of
//     the process lifetime, so the operation would have to answer success for a
//     book the browser cannot open.
//
// Unblocking create therefore needs, in this order: (a) a Go-owned source of the
// v7 schema (an embedded `baseline-v7.db` template shipped beside the binary, or
// the DDL as Go next to admission), and (b) an exported registration seam on
// authorruntime.ReadLifecycle (or a template field in productionConfig) so a new
// project enters the same inventory project:open resolves. Both live outside
// this file, so neither is faked here; until they exist `project:create` stays
// unbound and its failure mode stays the one the browser already knows.
const (
	// productionProjectDeleteName is the only lifecycle name this file binds.
	productionProjectDeleteName = "project:delete"
	// The browser sends the frozen project session as the last argument
	// (src/services/ipc-client.ts appends it to every project-scoped channel),
	// so the wire shape is [projectPath, projectId, sessionLease, context].
	productionProjectDeleteArity           = 4
	productionProjectDeletePathPosition    = 0
	productionProjectDeleteIDPosition      = 1
	productionProjectDeleteLeasePosition   = 2
	productionProjectDeleteContextPosition = 3
	// productionProjectTombstonePrefix names the transient sibling a delete
	// renames the project to before removing it. It is a direct child of the data
	// root, not of the library, so a removal that is interrupted between the two
	// steps can never leave a directory that the next startup would admit as a
	// project (ListCanonicalProjects enumerates the library, and hidden books are
	// listed on purpose: internal/projectfs/list.go:78-90).
	productionProjectTombstonePrefix = ".deleting-"
	// productionProjectTombstoneBytes is the randomness in a tombstone name. The
	// name only has to be absent from the data root; it is not a credential.
	productionProjectTombstoneBytes = 16
)

// productionProjectDeleteResult is the wire answer of project:delete, field for
// field the shape src/shared/ipc-channels.ts:432 declares. An empty error or
// warning is omitted, so a success never carries a stale code.
//
// databaseRestored reports whether the caller's admitted handle is still usable
// after the call. It is true for every refusal taken before the handle is
// closed, and false once the handle is gone: unlike the Electron main process,
// this lane cannot silently reopen the database under the SAME lease, so it
// never claims a restore it did not perform (see productionProjectCloseForDelete).
type productionProjectDeleteResult struct {
	Success          bool   `json:"success"`
	DirectoryDeleted bool   `json:"directoryDeleted"`
	DatabaseRestored bool   `json:"databaseRestored"`
	Error            string `json:"error,omitempty"`
	Warning          string `json:"warning,omitempty"`
}

// productionProjectDeleteTarget is the request after the wire shape and the
// frozen context have been read once. Nothing after this point re-reads args.
type productionProjectDeleteTarget struct {
	projectPath string
	projectID   string
	expected    projectsession.Context
}

// productionProjectLifecycle binds the project lifecycle names of this lane.
// The adapter borrows the lane's own inventory, library and durable runtime
// rather than opening a second view of the data root, so a delete can only ever
// remove a project project:open would resolve.
type productionProjectLifecycle struct {
	open *productionOpen
	// mu serialises deletions in this process. The coordinator already refuses a
	// second close for one lease, so this lock is about the inventory update and
	// the tombstone name, not about the removal itself.
	mu sync.Mutex
}

func newProductionProjectLifecycle(open *productionOpen) *productionProjectLifecycle {
	return &productionProjectLifecycle{open: open}
}

// Bind installs project:delete. project:create is absent on purpose: the header
// comment above records the two missing pieces that keep it unbound.
func (l *productionProjectLifecycle) Bind(server *transport.Server) error {
	if l == nil || l.open == nil || server == nil {
		return errors.New("PROJECT_LIFECYCLE_BINDER_INVALID")
	}
	return server.Bind(productionProjectDeleteName, l.delete)
}

// delete answers project:delete for one admitted project of this lane's library.
// Every refusal is a business answer with a documented code; none of them throws,
// exactly like the reference.
func (l *productionProjectLifecycle) delete(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	target, code := productionProjectDeleteRequest(args)
	if code != "" {
		return productionProjectDeleteAnswer(false, false, true, code, ""), nil
	}
	if ctx == nil {
		return productionProjectDeleteAnswer(false, false, true, "PROJECT_SESSION_REQUIRED", ""), nil
	}
	owner, ownerErr := productionLeaseOwner(caller)
	if ownerErr != nil {
		return productionProjectDeleteAnswer(false, false, true, "PROJECT_SESSION_REQUIRED", ""), nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	open := l.open
	open.mu.Lock()
	closed, durable, library, layout := open.closed, open.durable, open.library, open.layout
	open.mu.Unlock()
	if closed || durable == nil || library == nil || durable.lifecycle == nil {
		return productionProjectDeleteAnswer(false, false, true, "PRODUCTION_CLOSED", ""), nil
	}
	// The browser path is never authority: it selects one entry of the admitted
	// startup inventory, and only that entry's path is ever touched below.
	admitted, inventoried := open.productionProjectAdmitted(target.projectPath, target.projectID)
	if !inventoried {
		// A project this lane inventoried but refuses to stage still exists on
		// disk. Naming its refusal beats answering as though it were never here.
		if refusal, refused := open.productionProjectRefused(target.projectPath, target.projectID); refused {
			return productionProjectDeleteAnswer(false, false, true, refusal, ""), nil
		}
		return productionProjectDeleteAnswer(false, false, true, "PROJECT_NOT_FOUND", ""), nil
	}
	if !durable.productionProjectKeyed(admitted.Path) {
		return productionProjectDeleteAnswer(false, false, true, "PROJECT_NOT_FOUND", ""), nil
	}
	// A directory this lane did not enumerate as a direct child of its own pinned
	// library root is never removed, whatever the request said.
	if !productionProjectDeletable(admitted.Path, layout.libraryRoot) {
		return productionProjectDeleteAnswer(false, false, true, "PROJECT_DELETE_TARGET_MISMATCH", ""), nil
	}
	// The caller must hold this project under exactly the lease it rendered with.
	// Anything else (rotated, released, another tab's, never issued) is stale, and
	// nothing has been touched yet.
	current, live, currentErr := durable.lifecycle.CurrentProjectDurable(ctx, owner)
	if currentErr != nil || !live || current != target.expected {
		return productionProjectDeleteAnswer(false, false, true, "PROJECT_SESSION_STALE", ""), nil
	}
	// The manifest on disk must still name the project the inventory admitted: a
	// project directory swapped underneath this lane is refused, not deleted.
	if code := productionProjectConfirmIdentity(library, admitted, target.expected); code != "" {
		return productionProjectDeleteAnswer(false, false, true, code, ""), nil
	}
	restored, closeCode := productionProjectCloseForDelete(ctx, durable, owner, target.expected)
	if closeCode != "" {
		return productionProjectDeleteAnswer(false, false, restored, closeCode, ""), nil
	}
	// The writer is gone. Renaming first makes the project disappear from its
	// canonical path in one step, so an interrupted removal can never leave a
	// half-deleted book that project:open would still admit.
	tombstone, named := productionProjectTombstonePath(layout.dataRoot, admitted.Path)
	if !named {
		return productionProjectDeleteAnswer(false, false, false, "PROJECT_DELETE_FAILED", "PROJECT_DELETE_RENAME_FAILED"), nil
	}
	if err := os.Rename(admitted.Path, tombstone); err != nil {
		return productionProjectDeleteAnswer(false, false, false, "PROJECT_DELETE_FAILED", "PROJECT_DELETE_RENAME_FAILED"), nil
	}
	// Decide from the disk fact, not from the call.
	if productionProjectPresent(admitted.Path) {
		return productionProjectDeleteAnswer(false, false, false, "PROJECT_DELETE_FAILED", "PROJECT_DELETE_RENAME_FAILED"), nil
	}
	// From here the project is deleted; what remains is releasing its bytes.
	warning := ""
	if err := os.RemoveAll(tombstone); err != nil {
		warning = "PROJECT_DELETE_CLEANUP_FAILED"
	}
	open.productionProjectForget(admitted.Path, admitted.ProjectID)
	durable.productionProjectForgetKey(admitted.Path)
	return productionProjectDeleteAnswer(true, true, false, "", warning), nil
}

// productionProjectDeleteRequest reads the four wire arguments once and checks
// that they agree with each other. It returns a symbolic code instead of an
// error because the reference answers these refusals as results, not throws.
func productionProjectDeleteRequest(args []any) (productionProjectDeleteTarget, string) {
	if len(args) != productionProjectDeleteArity {
		return productionProjectDeleteTarget{}, "INVALID_ARGUMENTS"
	}
	projectPath, pathOK := args[productionProjectDeletePathPosition].(string)
	projectID, idOK := args[productionProjectDeleteIDPosition].(string)
	lease, leaseOK := args[productionProjectDeleteLeasePosition].(string)
	expected, contextErr := projectsession.ParseContext(args[productionProjectDeleteContextPosition])
	if !pathOK || !idOK || !leaseOK || contextErr != nil {
		return productionProjectDeleteTarget{}, "PROJECT_SESSION_REQUIRED"
	}
	if projectPath == "" || projectID == "" || lease == "" {
		return productionProjectDeleteTarget{}, "PROJECT_SESSION_REQUIRED"
	}
	// The three scalars are the same frozen session the context carries; a page
	// that froze one and rendered another is refused instead of being trusted.
	if projectPath != expected.ProjectPath || projectID != expected.ProjectID || lease != expected.LeaseID {
		return productionProjectDeleteTarget{}, "PROJECT_CONTEXT_CHANGED"
	}
	return productionProjectDeleteTarget{projectPath: projectPath, projectID: projectID, expected: expected}, ""
}

// productionProjectConfirmIdentity repins the project through the same library
// admission that listed it and proves the manifest still names the inventoried
// project. It returns a symbolic code, or "" when the identity holds.
func productionProjectConfirmIdentity(library *projectfs.Library, admitted projectfs.ListedProject, expected projectsession.Context) string {
	project, err := library.OpenProject(admitted.Path)
	if err != nil {
		// Missing manifest, missing database, unsafe storage root or a pending
		// migration: not a project this lane may remove, whatever the inventory
		// remembered at startup.
		return "PROJECT_NOT_FOUND"
	}
	identity, pinned := project.ProjectID(), project.Path()
	if closeErr := project.Close(); closeErr != nil {
		return "PROJECT_DELETE_FAILED"
	}
	if identity != admitted.ProjectID || identity != expected.ProjectID || pinned != admitted.Path {
		return "PROJECT_DELETE_IDENTITY_MISMATCH"
	}
	return ""
}

// productionProjectCloseForDelete closes the caller's own handle and reports
// whether the database is still usable afterwards. The second return value is ""
// when the handle is gone and the removal may proceed.
//
// restored is false when the coordinator was poisoned by a failed handle close:
// this lane can prove the failure but cannot reopen the database under the same
// lease, and claiming otherwise would leave the browser writing into a session
// this server no longer honours.
func productionProjectCloseForDelete(ctx context.Context, durable *productionDurableOpen, owner projectsession.Owner, expected projectsession.Context) (bool, string) {
	if err := durable.lifecycle.CloseProjectDurable(ctx, owner, expected); err != nil {
		if errors.Is(err, novelruntime.ErrNotOwned) {
			// Not ours (any more): nothing was closed, nothing will be removed.
			return true, "PROJECT_SESSION_STALE"
		}
		// Otherwise ask the coordinator instead of guessing: a cancellation or a
		// refused writer-lease verification leaves the handle in place and able to
		// serve, a failed handle close does not.
		if current, live, probeErr := durable.lifecycle.CurrentProjectDurable(ctx, owner); probeErr == nil && live && current == expected {
			return true, "PROJECT_DELETE_FAILED"
		}
		return false, "PROJECT_DELETE_FAILED"
	}
	return false, ""
}

// productionProjectDeletable reports whether path is the canonical shape of one
// direct child of the pinned library root: absolute, already clean, not a
// separator or a dot entry, and with the library root as its parent. The
// component scan refuses a path that could only have come from a request.
func productionProjectDeletable(projectPath, libraryRoot string) bool {
	if projectPath == "" || libraryRoot == "" || !filepath.IsAbs(projectPath) || !filepath.IsAbs(libraryRoot) {
		return false
	}
	if filepath.Clean(projectPath) != projectPath || filepath.Clean(libraryRoot) != libraryRoot {
		return false
	}
	if strings.ContainsAny(projectPath, "\x00\\") {
		return false
	}
	name := filepath.Base(projectPath)
	if name == "." || name == ".." || name == string(filepath.Separator) || name == "" {
		return false
	}
	return filepath.Dir(projectPath) == libraryRoot && projectPath != libraryRoot
}

// productionProjectTombstonePath mints an absent sibling name for the removal.
// It refuses instead of overwriting: an existing path here is never reused.
func productionProjectTombstonePath(dataRoot, projectPath string) (string, bool) {
	var buffer [productionProjectTombstoneBytes]byte
	if _, err := rand.Read(buffer[:]); err != nil {
		return "", false
	}
	tombstone := filepath.Join(dataRoot, productionProjectTombstonePrefix+hex.EncodeToString(buffer[:]))
	if tombstone == projectPath {
		return "", false
	}
	_, statErr := os.Lstat(tombstone)
	if statErr == nil {
		return "", false
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", false
	}
	return tombstone, true
}

// productionProjectPresent reports whether the canonical path still exists. It
// is the disk fact the reference answers from (project-controller.ts:958), so a
// rename that reported success but left the directory is still a failure.
func productionProjectPresent(projectPath string) bool {
	_, err := os.Lstat(projectPath)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	// An unreadable parent is not proof of absence.
	return true
}

// productionProjectDeleteAnswer builds one business answer. Every field is set
// on both paths so a refusal can never look like a partial success.
func productionProjectDeleteAnswer(success, directoryDeleted, databaseRestored bool, code, warning string) productionProjectDeleteResult {
	return productionProjectDeleteResult{
		Success:          success,
		DirectoryDeleted: directoryDeleted,
		DatabaseRestored: databaseRestored,
		Error:            code,
		Warning:          warning,
	}
}

// productionProjectAdmitted returns the admitted inventory entry for one path
// and project id. Both must match: a path alone is not authority, and an id
// alone says nothing about which directory holds it.
func (p *productionOpen) productionProjectAdmitted(projectPath, projectID string) (projectfs.ListedProject, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, admitted := range p.projects {
		if admitted.listed.Path == projectPath && admitted.listed.ProjectID == projectID {
			return admitted.listed, true
		}
	}
	return projectfs.ListedProject{}, false
}

// productionProjectRefused returns the named refusal of one inventoried-but-
// unserved project (over the staging budget). Both the path and the project id
// must match, exactly as productionProjectAdmitted requires them, so a path
// alone never selects a refusal either.
func (p *productionOpen) productionProjectRefused(projectPath, projectID string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, item := range p.refused {
		if item.listed.Path == projectPath && item.listed.ProjectID == projectID {
			return item.code, true
		}
	}
	return "", false
}

// productionProjectForget drops one admitted entry after its directory is gone,
// which is the lane's `removeRecentProject`: the recent shelf and the library
// chooser are both projections of this slice, so both stop naming a deleted
// book in the same step.
func (p *productionOpen) productionProjectForget(projectPath, projectID string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for index, admitted := range p.projects {
		if admitted.listed.Path == projectPath && admitted.listed.ProjectID == projectID {
			p.projects = append(p.projects[:index], p.projects[index+1:]...)
			return true
		}
	}
	return false
}

// productionProjectKeyed reports whether project:open would still resolve this
// path, using the same predicate project:open uses (keyFor under d.mu).
func (d *productionDurableOpen) productionProjectKeyed(projectPath string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return false
	}
	_, keyed := d.keyFor(projectPath)
	return keyed
}

// productionProjectForgetKey forgets the catalog key of a project whose
// directory is gone, so a later project:open answers PROJECT_NOT_FOUND instead
// of resolving a key to a path that no longer exists. The key itself is not
// revoked (the catalog offers no such seam) and does not need to be: a key is
// only ever consulted through this map.
func (d *productionDurableOpen) productionProjectForgetKey(projectPath string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.keys, projectPath)
}

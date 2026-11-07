//go:build linux

package main

// The legacy import operation: `project:import-legacy-copy`, the last name the
// project family refused. The desktop's "import a legacy project" copies an
// old-format project directory into the current library; this file serves that
// copy for a browser, out of the two grants the pickers already mint, and proves
// it byte for byte.
//
// THE REFERENCE IS THE SPEC
//
//	electron/controllers/project-controller.ts:372-426
//	    ipcMain.handle('project:import-legacy-copy', (event, sourceGrantId,
//	    targetGrantId)). BOTH arguments are grant ids and never paths: the source
//	    is resolved with the `legacy-import` operation
//	    (externalFileGrants.resolveDirectoryPath, :399-401) and the destination
//	    with the restore-target picker's `create` permission
//	    (resolveExactPath, :402-406). Either resolution failing, or an argument
//	    that is not a string, answers {state:'blocked',
//	    code:'LEGACY_IMPORT_PATH_INVALID'} (:377-379, :407-409). No host path is
//	    ever read out of the request.
//	electron/services/legacy-project-copy-import.ts:306-458
//	    importLegacyProjectCopy({sourceRoot, targetRoot}). physicalDirectory
//	    (:47-54) refuses a symlinked or non-directory component on both ends; the
//	    two trees may not overlap (:320, LEGACY_IMPORT_PATH_OVERLAP); an existing
//	    destination is refused (:321, :441, LEGACY_IMPORT_TARGET_EXISTS); a source
//	    that already carries a current project - `.ai-novel` or
//	    `.ai-novel-migration` - is refused (:325,
//	    LEGACY_IMPORT_UNSUPPORTED_SOURCE). It COPIES, it never moves: the legacy
//	    project is kept and the two copies never sync afterwards
//	    (project-controller.ts:390-391), and the desktop's confirmation dialog
//	    says so. The copy is proved by hashing the source tree before (:343), the
//	    copied tree beside it (:86-91 sameTree), and the source again (:354,
//	    :356, :440, LEGACY_IMPORT_SOURCE_CHANGED). What it additionally does -
//	    importing the lancedb vector store, the m05 character-asset migration and
//	    the portable transfer authority install (:381-420) - is the conversion
//	    this lane has no port for, and it is exactly why the name was refused in
//	    production_project_portability_linux.go.
//
// The refusal this file answers, quoted from that file's header before the row was
// dropped, because its reason is what still bounds the answer:
//
//	project:import-legacy-copy  The legacy import vector: an old desktop copy is a
//	                     *directory* of project trees, a lancedb vector store and a
//	                     per-project journal. Two of the three have no Go port
//	                     (internal/projectfs has no vector store, and m05
//	                     character-asset migration lives in
//	                     electron/services/import-legacy*.ts) ... A binder here could
//	                     only refuse every real request, which the unbound name
//	                     already does for free.
//
// The last sentence is the part this file answers: the copy itself was always
// portable, and it is the conversion that was missing. So the copy is served and
// the conversion is not pretended.
//
// WHAT THIS FILE SERVES
//
// The copy, and only the copy: every name, every directory and every byte below
// the granted legacy directory, written into one new directory in the library.
// The promise is byte-for-byte, and it is proved rather than declared: one
// digest over the source tree (names, kinds, sizes, contents), one over the
// destination tree, and the copy is published only when the two are equal and a
// third digest proves the source did not move while it was read. Modes and
// timestamps are not part of that digest, exactly as the desktop's treeHash
// (:55-75) does not include them.
//
// THE CONFINEMENT, AND WHY IT IS THE PICKERS'
//
// A browser cannot hand over a host path, so the source is the token
// `dialog:select-legacy-project` mints (production_browser_pickers_linux.go:388,
// productionPickerSpecs: a directory, read only) and the destination is the token
// `dialog:select-project-restore-target` mints (:382: a directory that need not
// exist yet, because a copy's name is new). Both are redeemed here through the
// exact instances the three `fs:grant-*` names use: b.grants.roots(ctx, caller)
// builds this request's allowlist from the live lease and the configured library
// (internal/librarypath/roots.go), the process-wide registry binds each token to
// this cookie session and tab (internal/librarypath/grants.go), and each grant is
// RE-RESOLVED through this request's allowlist rather than trusted - a token whose
// symbolic root this request does not hold is refused by name
// (productionLegacyImportPathInvalidCode), which is what makes "a path outside the
// roots" unreachable through one. The two ends are not symmetric, and that is
// deliberate: the source may sit under either allowlisted root, because reading a
// tree writes nothing (the package's own rule, internal/librarypath/doc.go), while
// the destination must be one new child of the library root and never the project
// root the caller already holds. The picker's own answer is richer (kind,
// creatable, identity, a bounded listing: productionPickerKind, :661,
// productionPickerIdentity, :648); this file reads exactly one field of it, the
// grantId, and re-derives everything else itself.
//
// The filesystem half is the layer internal/librarypath itself uses: a pinned
// os.Root anchored at the resolved directory (librarypath/io.go:264-291
// openPinned, and productionPortabilityOpenBinary for the binary case). That is
// what lets this walk copy `.vela` and the other dot-prefixed names a legacy
// project is made of - librarypath's public Target methods refuse every
// dot-prefixed component on purpose (roots.go cleanRelative, ErrPathHidden) -
// while keeping the property that matters: every open, mkdir, create and remove
// is relative to that one directory, a symlink is refused by Lstat before it is
// ever opened, and an os.Root cannot be made to name anything outside itself even
// if a component is swapped mid-walk. Nothing below a request string reaches a
// syscall.
//
// WHAT THIS LANE DOES NOT DO, STATED ONCE
//
// The copy is not a conversion. It writes no `.ai-novel` manifest, opens no
// database, migrates no vectors and installs no transfer authority, so the copied
// directory is the legacy bytes and not yet a book this lane can open. That is
// the same subset decision production_project_portability_linux.go makes for its
// two archive names: the bytes are proved, and the port this lane does not have
// is named instead of half-run. A refusal after the destination exists removes the
// whole tree it created, so a refusal leaves the library as it found it.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The answer's two states. `blocked` is the desktop's own union
// (src/shared/ipc-channels.ts:329-334) and it carries a symbolic code; `ready`
// carries the identity of the copy.
const (
	productionLegacyImportReadyState   = "ready"
	productionLegacyImportBlockedState = "blocked"
)

// The wire arity: [sourceGrantId, targetGrantId] (src/shared/ipc-channels.ts:330,
// passed whole by src/stores/project-store.ts:881).
const productionLegacyImportArity = 2

const (
	// productionLegacyImportLegacyDirectory is the old format's storage directory.
	// Its presence is the one shape check this lane makes: without it the granted
	// directory is not a legacy project, and the name promises a legacy copy.
	productionLegacyImportLegacyDirectory = ".vela"
	// productionLegacyImportLegacyDatabase is the old database the desktop refuses
	// to import without (legacy-project-copy-import.ts:328).
	productionLegacyImportLegacyDatabase = "vela.db"
	// productionLegacyImportMigrationDirectory is the desktop's second marker of a
	// canonical project (legacy-project-copy-import.ts:325). Either one anywhere in
	// the source is a self-copy and is refused.
	productionLegacyImportMigrationDirectory = ".ai-novel-migration"
	// productionLegacyImportReadBatch is how many names one readdir call may
	// consume, the same bounded-read discipline librarypath/listing.go uses.
	productionLegacyImportReadBatch = 64
	// productionLegacyImportBufferBytes bounds one read of one file. Files are
	// streamed, so this is the whole memory a copy holds at once.
	productionLegacyImportBufferBytes = 1 << 20
	// productionLegacyImportMaxEntries bounds one tree's names (directories and
	// files together). A copy is a server-side write of unknown size, so it has a
	// ceiling instead of an assumption.
	productionLegacyImportMaxEntries = 8192
	// productionLegacyImportMaxTreeBytes bounds one tree's file bytes.
	productionLegacyImportMaxTreeBytes = int64(512 << 20)
	// productionLegacyImportMaxManifestBytes bounds the one optional document this
	// lane reads for its answer. The old manifest is four short fields.
	productionLegacyImportMaxManifestBytes = int64(64 << 10)
)

// The refusal vocabulary. Every code is symbolic and mirrors the desktop's own
// `LEGACY_IMPORT_*` vocabulary, so the page's `blocked.code` reads the same reason
// the desktop would have reported, and no code can carry a host path.
const (
	// productionLegacyImportPathInvalidCode refuses a request whose grants are not
	// both live, readable, bound to this caller, and resolvable through this
	// request's allowlist. It is the desktop's code for the same failure
	// (project-controller.ts:377-379, :407-409).
	productionLegacyImportPathInvalidCode = "LEGACY_IMPORT_PATH_INVALID"
	// productionLegacyImportOverlapCode refuses a destination inside the source,
	// or a source inside the destination (legacy-project-copy-import.ts:320).
	productionLegacyImportOverlapCode = "LEGACY_IMPORT_PATH_OVERLAP"
	// productionLegacyImportOwnProjectCode refuses a source that carries this
	// lane's own project files: copying it would duplicate the library's own
	// project rather than import a legacy one (:325).
	productionLegacyImportOwnProjectCode = "LEGACY_IMPORT_UNSUPPORTED_SOURCE"
	// productionLegacyImportSourceCode refuses a source that is not a legacy
	// project tree: no `.vela`, or no `.vela/vela.db` inside it.
	productionLegacyImportSourceCode = "LEGACY_IMPORT_SOURCE_INVALID"
	// productionLegacyImportManifestCode refuses a present but unreadable legacy
	// manifest (:330-339, LEGACY_IMPORT_INVALID_MANIFEST).
	productionLegacyImportManifestCode = "LEGACY_IMPORT_INVALID_MANIFEST"
	// productionLegacyImportTargetExistsCode refuses an existing destination
	// (:321, :441).
	productionLegacyImportTargetExistsCode = "LEGACY_IMPORT_TARGET_EXISTS"
	// productionLegacyImportTargetUnsafeCode refuses a destination that is not one
	// new IMMEDIATE child of the library root the allowlist names, and a library
	// root that is itself a project tree.
	productionLegacyImportTargetUnsafeCode = "LEGACY_IMPORT_TARGET_UNSAFE"
	// productionLegacyImportUnsafeCode refuses a symlink or a special file
	// anywhere in the source: neither is a byte this lane can prove it copied
	// (:47-54, :55-75, LEGACY_IMPORT_UNSAFE_PATH).
	productionLegacyImportUnsafeCode = "LEGACY_IMPORT_UNSAFE_PATH"
	// productionLegacyImportCopyChangedCode refuses a destination whose digest is
	// not the source's (:86-91, LEGACY_IMPORT_COPY_CHANGED).
	productionLegacyImportCopyChangedCode = "LEGACY_IMPORT_COPY_CHANGED"
	// productionLegacyImportSourceChangedCode refuses a source that moved while it
	// was being read (:354, :356, :440).
	productionLegacyImportSourceChangedCode = "LEGACY_IMPORT_SOURCE_CHANGED"
	// productionLegacyImportLimitCode refuses a tree above the bounds above.
	productionLegacyImportLimitCode = "LEGACY_IMPORT_LIMIT_EXCEEDED"
	// productionLegacyImportIOFailedCode is every other filesystem failure. It is
	// the desktop's own fallback code (:446).
	productionLegacyImportIOFailedCode = "LEGACY_IMPORT_IO_FAILED"
	// productionLegacyImportRollbackCode is logged, never answered: it says a
	// refusal could not remove the tree it had created.
	productionLegacyImportRollbackCode = "LEGACY_IMPORT_ROLLBACK_FAILED"
)

// productionLegacyImportRefusal is a refusal that already knows its symbolic
// code, so no internal helper has to return a sentence an operator would have to
// parse and no path can be smuggled into an answer.
type productionLegacyImportRefusal struct{ code string }

func (r *productionLegacyImportRefusal) Error() string {
	if r == nil {
		return productionLegacyImportIOFailedCode
	}
	return r.code
}

// productionLegacyImportFail is the one constructor of a coded refusal.
func productionLegacyImportFail(code string) error {
	return &productionLegacyImportRefusal{code: code}
}

// productionLegacyImportCode maps a helper's error to the code on the wire. An
// error this file did not mint is the generic I/O failure, because not knowing
// which rule refused is not a reason to invent one.
func productionLegacyImportCode(err error) string {
	var refusal *productionLegacyImportRefusal
	if errors.As(err, &refusal) && refusal.code != "" {
		return refusal.code
	}
	return productionLegacyImportIOFailedCode
}

// productionLegacyImportBudget is one walk's accounting: how many names and how
// many file bytes a tree may hold. A fresh budget is taken for every walk, so the
// destination is held to the same ceiling as the source.
type productionLegacyImportBudget struct {
	entries int
	bytes   int64
}

// charge records one entry and refuses a tree that crossed either bound.
func (b *productionLegacyImportBudget) charge(size int64) error {
	if b == nil || size < 0 {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	b.entries++
	if b.entries > productionLegacyImportMaxEntries || size > librarypath.MaxFileBytes {
		return productionLegacyImportFail(productionLegacyImportLimitCode)
	}
	b.bytes += size
	if b.bytes > productionLegacyImportMaxTreeBytes {
		return productionLegacyImportFail(productionLegacyImportLimitCode)
	}
	return nil
}

// productionLegacyImportEntry is one child of a directory this walk will copy: a
// directory or a single regular file, by its own name and size.
type productionLegacyImportEntry struct {
	name string
	dir  bool
	size int64
}

// importLegacyCopy serves `project:import-legacy-copy`. Every refusal is the
// desktop's `blocked` envelope at HTTP 200, because the shipped renderer reads
// `result.state` instead of a thrown error (src/stores/project-store.ts:883-888).
func (b *productionPortability) importLegacyCopy(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	deny := func(err error) (any, error) {
		code := productionLegacyImportCode(err)
		log.Printf("novel-server: %s refused: %s", productionPortabilityLegacyCopyName, code)
		return map[string]any{"state": productionLegacyImportBlockedState, "code": code}, nil
	}
	if b == nil || b.open == nil || b.grants == nil || ctx == nil {
		return deny(productionLegacyImportFail(productionPortabilityClosedCode))
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportIOFailedCode))
	}
	sourceID, targetID, argumentsErr := productionLegacyImportArguments(args)
	if argumentsErr != nil {
		return deny(argumentsErr)
	}
	// 1. This request's allowlist, built from the live lease and the configured
	//    library exactly as the pickers and the three `fs:grant-*` names build it.
	//    A caller with no authenticated session has no allowlist at all.
	roots, binding, rootsErr := b.grants.roots(ctx, caller)
	if rootsErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	registry := b.grants.Grants()
	if registry == nil {
		return deny(productionLegacyImportFail(productionLegacyImportIOFailedCode))
	}
	// 2. Both tokens, asked and not yet spent. Peek runs every admission check:
	//    unknown, expired, revoked, another tab's, or an operation the token does
	//    not carry, and none of them costs the caller a use.
	sourceGrant, sourcePeekErr := registry.Peek(sourceID, binding, librarypath.OpRead)
	if sourcePeekErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	targetGrant, targetPeekErr := registry.Peek(targetID, binding, librarypath.OpRead)
	if targetPeekErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	// 3. Both grants are RE-RESOLVED through this request's allowlist. A grant is
	//    a symbolic root plus a relative path, and a root this request does not
	//    hold is refused here rather than opened - that is the whole reason a
	//    token minted for another tree cannot become a filesystem call.
	source, sourceErr := roots.Resolve(sourceGrant.Target.RootID, sourceGrant.Target.Relative)
	if sourceErr != nil || !source.Exists || !source.IsDir {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	destination, destinationErr := roots.Resolve(targetGrant.Target.RootID, targetGrant.Target.Relative)
	if destinationErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	// 4. The destination is one new child of the LIBRARY root: never the root
	//    itself, never a name that is not a plain relative path (the same rule
	//    restoreTarget applies, production_cloud_backup_linux.go:921-926), and
	//    never a nested name. Depth 1 is not a convenience: a project is an
	//    immediate child of the library and nothing else is ever scanned for one
	//    (internal/projectfs/list.go, ListCanonicalProjects), so a copy written
	//    one level down would be a tree no shelf and no catalogue can see.
	if destination.RootID != librarypath.LibraryRootID || destination.Relative == "" ||
		!filepath.IsLocal(destination.Relative) ||
		strings.ContainsRune(destination.Relative, '/') {
		return deny(productionLegacyImportFail(productionLegacyImportTargetUnsafeCode))
	}
	// 5. Neither tree may contain the other. The desktop checks this before it
	//    checks existence (legacy-project-copy-import.ts:320-321), and the order
	//    matters: a destination that IS the source is an overlap, not a name that
	//    happens to be taken, and the two rules keep the copy from writing into
	//    the tree it is reading.
	if productionLegacyImportWithin(source.Path, destination.Path) || productionLegacyImportWithin(destination.Path, source.Path) {
		return deny(productionLegacyImportFail(productionLegacyImportOverlapCode))
	}
	if destination.Exists {
		return deny(productionLegacyImportFail(productionLegacyImportTargetExistsCode))
	}
	// The one ancestor that is left is the library root itself, and it must not be
	// a project tree: that is the judgement librarypath's requireOutput makes for
	// every output write (io.go:196-210), and a copy is an output too.
	parent, parentErr := roots.Resolve(librarypath.LibraryRootID, "")
	if parentErr != nil || !parent.Exists || !parent.IsDir {
		return deny(productionLegacyImportFail(productionLegacyImportTargetUnsafeCode))
	}
	// 6. The source tree, through one pinned os.Root. Every later read, digest and
	//    copy is relative to this handle, so no request string and no symlink can
	//    reach outside the granted directory.
	sourceRoot, sourceOpenErr := productionLegacyImportOpen(source.Path)
	if sourceOpenErr != nil {
		return deny(sourceOpenErr)
	}
	defer func() { _ = sourceRoot.Close() }()
	projectID, shapeErr := productionLegacyImportSource(sourceRoot)
	if shapeErr != nil {
		return deny(shapeErr)
	}
	// 7. The digest taken BEFORE anything is written. It is the promise the copy
	//    is measured against, and the same measurement the desktop takes first.
	before, beforeErr := productionLegacyImportDigest(sourceRoot, &productionLegacyImportBudget{})
	if beforeErr != nil {
		return deny(beforeErr)
	}
	// 8. The library root, pinned, and one mkdir through it. Nothing above the
	//    final component is created or replaced.
	parentRoot, parentOpenErr := productionLegacyImportOpen(parent.Path)
	if parentOpenErr != nil {
		return deny(parentOpenErr)
	}
	defer func() { _ = parentRoot.Close() }()
	if _, markerErr := parentRoot.Lstat(projectfs.ManifestName); markerErr == nil || !errors.Is(markerErr, fs.ErrNotExist) {
		// A library root that carries a project manifest is a project tree, and an
		// output below one is author data (librarypath/io.go requireOutput). It is
		// also the one case where the "new child" this call would create could land
		// inside somebody's book.
		return deny(productionLegacyImportFail(productionLegacyImportTargetUnsafeCode))
	}
	name := filepath.Base(destination.Path)
	if name == "" || name == "." || name == ".." {
		return deny(productionLegacyImportFail(productionLegacyImportTargetUnsafeCode))
	}
	if mkdirErr := parentRoot.Mkdir(name, productionProjectCreateDirectoryMode); mkdirErr != nil {
		if errors.Is(mkdirErr, fs.ErrExist) {
			return deny(productionLegacyImportFail(productionLegacyImportTargetExistsCode))
		}
		return deny(productionLegacyImportFail(productionLegacyImportIOFailedCode))
	}
	// From here on a refusal must undo itself. Every removal goes through the same
	// pinned parent, and it removes exactly the one directory this call created.
	rollback := true
	defer func() {
		if !rollback {
			return
		}
		if removeErr := parentRoot.RemoveAll(name); removeErr != nil {
			log.Printf("novel-server: %s: %s", productionPortabilityLegacyCopyName, productionLegacyImportRollbackCode)
		}
	}()
	destinationRoot, destinationOpenErr := parentRoot.OpenRoot(name)
	if destinationOpenErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportIOFailedCode))
	}
	defer func() { _ = destinationRoot.Close() }()
	// 9. The copy itself, then the three measurements: the destination's digest
	//    against the source's, and the source's own digest against the one taken
	//    before any byte was written.
	if copyErr := productionLegacyImportCopy(sourceRoot, destinationRoot, "", &productionLegacyImportBudget{}); copyErr != nil {
		return deny(copyErr)
	}
	copied, copiedErr := productionLegacyImportDigest(destinationRoot, &productionLegacyImportBudget{})
	if copiedErr != nil {
		return deny(copiedErr)
	}
	if copied != before {
		return deny(productionLegacyImportFail(productionLegacyImportCopyChangedCode))
	}
	after, afterErr := productionLegacyImportDigest(sourceRoot, &productionLegacyImportBudget{})
	if afterErr != nil {
		return deny(afterErr)
	}
	if after != before {
		return deny(productionLegacyImportFail(productionLegacyImportSourceChangedCode))
	}
	// 10. Only now are the two capabilities spent: a request that never reached a
	//     byte costs the caller nothing, and two imports racing over one token
	//     cannot both land (Registry.Spend re-runs every check under the lock that
	//     decrements).
	if _, spendErr := registry.Spend(sourceID, binding, librarypath.OpRead); spendErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	if _, spendErr := registry.Spend(targetID, binding, librarypath.OpRead); spendErr != nil {
		return deny(productionLegacyImportFail(productionLegacyImportPathInvalidCode))
	}
	rollback = false
	return map[string]any{
		"state": productionLegacyImportReadyState,
		// The legacy identity the copied bytes carry, read out of the manifest they
		// carry it in. A pre-manifest legacy project has none, and this lane mints
		// no new one because it performs no conversion.
		"projectId": projectID,
		// The addressable name of the new directory, spelled exactly as the picker
		// spelled its destination. Target.Path is a host path and librarypath
		// forbids putting one on a wire (production_cloud_backup_linux.go:930-932),
		// so the answer carries the identity the next request re-resolves.
		"targetRoot": productionPickerIdentity(destination),
	}, nil
}

// productionLegacyImportArguments reads the two grant ids. Anything else - a
// different arity, a non-string, a value that cannot be a token, or one token
// twice - is the desktop's PATH_INVALID refusal, because a request this shape
// never named two distinct capabilities.
func productionLegacyImportArguments(args []any) (string, string, error) {
	if len(args) != productionLegacyImportArity {
		return "", "", productionLegacyImportFail(productionLegacyImportPathInvalidCode)
	}
	source, sourceOK := args[0].(string)
	target, targetOK := args[1].(string)
	if !sourceOK || !targetOK || source == target ||
		!productionPickerOpaqueID(source) || !productionPickerOpaqueID(target) {
		return "", "", productionLegacyImportFail(productionLegacyImportPathInvalidCode)
	}
	return source, target, nil
}

// productionLegacyImportWithin reports whether child is parent or sits below it.
// Both paths are resolved, clean and absolute, so a separator-terminated prefix
// is the whole test; the separator is appended so "/a/b" is not "inside" "/a/bc".
func productionLegacyImportWithin(parent, child string) bool {
	if parent == "" || child == "" {
		return false
	}
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+string(filepath.Separator))
}

// productionLegacyImportOpen opens one directory as an os.Root and proves the
// descriptor really is that directory: Lstat before, the root's own Stat, and an
// Lstat after it is opened must all name the same inode. It is the discipline
// librarypath/io.go:264-291 applies, repeated here because that helper is
// unexported and this file may not change that package.
func productionLegacyImportOpen(directory string) (*os.Root, error) {
	if directory == "" {
		return nil, productionLegacyImportFail(productionLegacyImportTargetUnsafeCode)
	}
	before, err := os.Lstat(directory)
	if err != nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return nil, productionLegacyImportFail(productionLegacyImportUnsafeCode)
	}
	if !before.IsDir() {
		return nil, productionLegacyImportFail(productionLegacyImportTargetUnsafeCode)
	}
	root, openErr := os.OpenRoot(directory)
	if openErr != nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	opened, statErr := root.Stat(".")
	after, afterErr := os.Lstat(directory)
	if statErr != nil || afterErr != nil || !opened.IsDir() ||
		after.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(before, opened) || !os.SameFile(before, after) {
		_ = root.Close()
		return nil, productionLegacyImportFail(productionLegacyImportUnsafeCode)
	}
	return root, nil
}

// productionLegacyImportSource validates the one shape this lane insists on and
// answers the legacy project id the source carries, or the empty string when the
// source predates the manifest. The rules are the desktop's (:325-338), in its
// order: a current project's own markers are refused first, the old storage
// directory and its database must be there, and a present manifest is validated
// exactly rather than believed. The walk below refuses a marker at any other
// depth for the same reason.
func productionLegacyImportSource(root *os.Root) (string, error) {
	if root == nil {
		return "", productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	// A source that already carries this lane's own project is the self-copy the
	// desktop refuses first (legacy-project-copy-import.ts:325): copying it would
	// duplicate one of the library's projects into the library under a second
	// name, and the two copies would never sync.
	for _, marker := range []string{projectfs.StorageDirectory, productionLegacyImportMigrationDirectory} {
		_, markerErr := root.Lstat(marker)
		if markerErr == nil {
			return "", productionLegacyImportFail(productionLegacyImportOwnProjectCode)
		}
		if !errors.Is(markerErr, fs.ErrNotExist) {
			return "", productionLegacyImportFail(productionLegacyImportIOFailedCode)
		}
	}
	legacy, legacyErr := root.Lstat(productionLegacyImportLegacyDirectory)
	if legacyErr != nil {
		return "", productionLegacyImportFail(productionLegacyImportSourceCode)
	}
	if legacy.Mode()&os.ModeSymlink != 0 || !legacy.IsDir() {
		return "", productionLegacyImportFail(productionLegacyImportSourceCode)
	}
	databasePath := productionLegacyImportLegacyDirectory + "/" + productionLegacyImportLegacyDatabase
	database, databaseErr := root.Lstat(databasePath)
	if databaseErr != nil {
		return "", productionLegacyImportFail(productionLegacyImportSourceCode)
	}
	if database.Mode()&os.ModeSymlink != 0 || !database.Mode().IsRegular() {
		return "", productionLegacyImportFail(productionLegacyImportSourceCode)
	}
	manifestPath := productionLegacyImportLegacyDirectory + "/" + projectfs.ManifestName
	manifest, manifestErr := root.Lstat(manifestPath)
	switch {
	case manifestErr == nil:
		if manifest.Mode()&os.ModeSymlink != 0 || !manifest.Mode().IsRegular() ||
			manifest.Size() < 0 || manifest.Size() > productionLegacyImportMaxManifestBytes {
			return "", productionLegacyImportFail(productionLegacyImportManifestCode)
		}
		raw, readErr := productionLegacyImportRead(root, manifestPath, manifest.Size())
		if readErr != nil {
			return "", readErr
		}
		return productionLegacyImportManifest(raw)
	case errors.Is(manifestErr, fs.ErrNotExist):
		// The pre-manifest layout: the desktop imports it too, and starts a new
		// lineage at the new identity. This lane converts nothing, so the copy has
		// no identity to report and says so with the empty string.
		return "", nil
	default:
		return "", productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
}

// productionLegacyImportRead reads one bounded regular file through the pinned
// root. It is used for the legacy manifest only; contents travel by streaming.
func productionLegacyImportRead(root *os.Root, relative string, size int64) ([]byte, error) {
	if root == nil || size < 0 || size > productionLegacyImportMaxManifestBytes {
		return nil, productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	file, openErr := root.Open(relative)
	if openErr != nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	defer func() { _ = file.Close() }()
	data, readErr := io.ReadAll(io.LimitReader(file, size+1))
	if readErr != nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	if int64(len(data)) != size {
		return nil, productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	return data, nil
}

// productionLegacyImportManifest validates the old manifest and answers its
// projectId. The desktop's rules, one for one
// (legacy-project-copy-import.ts:330-339): exactly the four keys
// createdAt/kind/projectId/schemaVersion, schemaVersion 1, kind
// 'ai-novel-project', a version 4 UUID and a parseable date.
func productionLegacyImportManifest(raw []byte) (string, error) {
	decoded := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	keys := make([]string, 0, len(decoded))
	for key := range decoded {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "createdAt,kind,projectId,schemaVersion" {
		return "", productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	var version int
	if err := json.Unmarshal(decoded["schemaVersion"], &version); err != nil || version != 1 {
		return "", productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	var kind string
	if err := json.Unmarshal(decoded["kind"], &kind); err != nil || kind != "ai-novel-project" {
		return "", productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	var projectID string
	if err := json.Unmarshal(decoded["projectId"], &projectID); err != nil || !productionLegacyImportUUID(projectID) {
		return "", productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	var created string
	if err := json.Unmarshal(decoded["createdAt"], &created); err != nil || !productionLegacyImportTimestamp(created) {
		return "", productionLegacyImportFail(productionLegacyImportManifestCode)
	}
	return projectID, nil
}

// productionLegacyImportUUID is the UUID grammar the desktop's manifest check
// applies (legacy-project-copy-import.ts:36): eight, four, four, four and twelve
// hex digits, version 1-5 and variant 8/9/a/b, case insensitive.
func productionLegacyImportUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for index := 0; index < len(value); index++ {
		switch index {
		case 8, 13, 18, 23:
			if value[index] != '-' {
				return false
			}
		default:
			if !productionLegacyImportHexByte(value[index]) {
				return false
			}
		}
	}
	if value[14] < '1' || value[14] > '5' {
		return false
	}
	switch value[19] | 0x20 {
	case '8', '9', 'a', 'b':
		return true
	}
	return false
}

// productionLegacyImportHexByte reports one hexadecimal digit.
func productionLegacyImportHexByte(value byte) bool {
	return value >= '0' && value <= '9' || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

// productionLegacyImportTimestamp accepts the two spellings a legacy manifest is
// written with: the canonical ISO instant (three fractional digits and Z, which
// is what a desktop manifest holds) and the SQLite text timestamp the old editor
// wrote beside it. Both are what Date.parse accepts in the reference.
func productionLegacyImportTimestamp(value string) bool {
	if value == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return true
	}
	if _, err := time.Parse("2006-01-02 15:04:05", value); err == nil {
		return true
	}
	return false
}

// productionLegacyImportDigest is one whole tree as a hex SHA-256: every name in
// byte order, its kind, a file's size and a file's bytes. Two equal digests mean
// two trees with the same names, the same kinds and the same contents, which is
// the promise the copy is published on.
func productionLegacyImportDigest(root *os.Root, budget *productionLegacyImportBudget) (string, error) {
	digest := sha256.New()
	if err := productionLegacyImportHash(root, "", digest, budget); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// productionLegacyImportHash walks one directory depth first in byte order and
// feeds the digest. The order is this walk's own on both sides, so the two trees
// are compared by the same rule; it never depends on readdir order.
func productionLegacyImportHash(root *os.Root, relative string, digest hash.Hash, budget *productionLegacyImportBudget) error {
	entries, entriesErr := productionLegacyImportEntries(root, relative, budget)
	if entriesErr != nil {
		return entriesErr
	}
	for _, entry := range entries {
		child := entry.name
		if relative != "" {
			child = relative + "/" + entry.name
		}
		header := make([]byte, 0, len(child)+24)
		header = append(header, child...)
		header = append(header, 0)
		if entry.dir {
			header = append(header, 'd')
		} else {
			header = append(header, 'f')
			header = strconv.AppendInt(header, entry.size, 10)
			header = append(header, 0)
		}
		if err := productionLegacyImportFeed(digest, header); err != nil {
			return err
		}
		if entry.dir {
			if err := productionLegacyImportHash(root, child, digest, budget); err != nil {
				return err
			}
			continue
		}
		if err := productionLegacyImportHashFile(root, child, entry.size, digest); err != nil {
			return err
		}
	}
	return nil
}

// productionLegacyImportFeed writes one digest input. A hash never fails, but the
// error is handled anyway so no write is silently dropped.
func productionLegacyImportFeed(digest hash.Hash, data []byte) error {
	if digest == nil {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	if _, err := digest.Write(data); err != nil {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	return nil
}

// productionLegacyImportHashFile streams one regular file into the digest and
// requires the bytes read to be exactly the size the walk recorded: a file that
// grew or shrank under the walk is a source that changed, not a copy to publish.
func productionLegacyImportHashFile(root *os.Root, relative string, size int64, digest hash.Hash) error {
	file, openErr := root.Open(relative)
	if openErr != nil {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	defer func() { _ = file.Close() }()
	buffer := make([]byte, productionLegacyImportBufferBytes)
	var total int64
	for {
		count, readErr := file.Read(buffer)
		if count > 0 {
			total += int64(count)
			if total > size {
				return productionLegacyImportFail(productionLegacyImportSourceChangedCode)
			}
			if err := productionLegacyImportFeed(digest, buffer[:count]); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return productionLegacyImportFail(productionLegacyImportIOFailedCode)
		}
	}
	if total != size {
		return productionLegacyImportFail(productionLegacyImportSourceChangedCode)
	}
	return nil
}

// productionLegacyImportEntries reads one directory in bounded batches and
// answers its usable children in byte order. It refuses, rather than skips, the
// first entry this lane will not copy: a copy that silently dropped a symlink or a
// socket would not be the tree the author asked for.
func productionLegacyImportEntries(root *os.Root, relative string, budget *productionLegacyImportBudget) ([]productionLegacyImportEntry, error) {
	if root == nil || budget == nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	handle, openErr := root.Open(productionLegacyImportName(relative))
	if openErr != nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	entries := make([]productionLegacyImportEntry, 0, productionLegacyImportReadBatch)
	for {
		batch, readErr := handle.ReadDir(productionLegacyImportReadBatch)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			_ = handle.Close()
			return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
		}
		for _, item := range batch {
			entry, entryErr := productionLegacyImportReadEntry(root, relative, item.Name(), budget)
			if entryErr != nil {
				_ = handle.Close()
				return nil, entryErr
			}
			entries = append(entries, entry)
		}
		if errors.Is(readErr, io.EOF) || len(batch) == 0 {
			break
		}
	}
	if closeErr := handle.Close(); closeErr != nil {
		return nil, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].name < entries[right].name })
	return entries, nil
}

// productionLegacyImportReadEntry classifies one name. A name that is not one
// component, a symlink, or anything that is neither a directory nor a single
// regular file is LEGACY_IMPORT_UNSAFE_PATH; a current project's markers are the
// self-copy refusal, at any depth.
func productionLegacyImportReadEntry(root *os.Root, relative, name string, budget *productionLegacyImportBudget) (productionLegacyImportEntry, error) {
	entry := productionLegacyImportEntry{}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") {
		return entry, productionLegacyImportFail(productionLegacyImportUnsafeCode)
	}
	if name == projectfs.StorageDirectory || name == productionLegacyImportMigrationDirectory {
		return entry, productionLegacyImportFail(productionLegacyImportOwnProjectCode)
	}
	child := name
	if relative != "" {
		child = relative + "/" + name
	}
	info, err := root.Lstat(child)
	if err != nil {
		return entry, productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return entry, productionLegacyImportFail(productionLegacyImportUnsafeCode)
	}
	switch {
	case info.IsDir():
		entry.name, entry.dir = name, true
		if chargeErr := budget.charge(0); chargeErr != nil {
			return entry, chargeErr
		}
	case info.Mode().IsRegular():
		entry.name, entry.size = name, info.Size()
		if chargeErr := budget.charge(entry.size); chargeErr != nil {
			return entry, chargeErr
		}
	default:
		return entry, productionLegacyImportFail(productionLegacyImportUnsafeCode)
	}
	return entry, nil
}

// productionLegacyImportCopy writes one source directory into the matching
// destination directory, entry by entry, in the same byte order the digest walk
// uses. Directories are created one component at a time through the destination
// root, files are created exclusive and streamed, and neither call can name
// anything outside the root it was opened on.
func productionLegacyImportCopy(source, destination *os.Root, relative string, budget *productionLegacyImportBudget) error {
	if source == nil || destination == nil || budget == nil {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	entries, entriesErr := productionLegacyImportEntries(source, relative, budget)
	if entriesErr != nil {
		return entriesErr
	}
	for _, entry := range entries {
		child := entry.name
		if relative != "" {
			child = relative + "/" + entry.name
		}
		if entry.dir {
			if mkdirErr := destination.Mkdir(child, productionProjectCreateDirectoryMode); mkdirErr != nil {
				return productionLegacyImportFail(productionLegacyImportIOFailedCode)
			}
			if copyErr := productionLegacyImportCopy(source, destination, child, budget); copyErr != nil {
				return copyErr
			}
			continue
		}
		if copyErr := productionLegacyImportCopyFile(source, destination, child, entry.size); copyErr != nil {
			return copyErr
		}
	}
	return nil
}

// productionLegacyImportCopyFile streams one regular file. The read is cut one
// byte past the recorded size, so a file that grew is refused instead of copied
// short, and the bytes written must be exactly those recorded.
func productionLegacyImportCopyFile(source, destination *os.Root, relative string, size int64) error {
	input, inputErr := source.Open(relative)
	if inputErr != nil {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	output, createErr := destination.OpenFile(relative, os.O_WRONLY|os.O_CREATE|os.O_EXCL, productionProjectCreateFileMode)
	if createErr != nil {
		_ = input.Close()
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	written, copyErr := io.Copy(output, io.LimitReader(input, size+1))
	if copyErr == nil {
		copyErr = output.Sync()
	}
	closeErr := output.Close()
	_ = input.Close()
	if copyErr != nil || closeErr != nil {
		return productionLegacyImportFail(productionLegacyImportIOFailedCode)
	}
	if written != size {
		return productionLegacyImportFail(productionLegacyImportSourceChangedCode)
	}
	return nil
}

// productionLegacyImportName is the name one directory is opened by: the root
// itself for the empty relative path, the relative path otherwise.
func productionLegacyImportName(relative string) string {
	if relative == "" {
		return "."
	}
	return relative
}

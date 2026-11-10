//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"syscall"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's DOCUMENT slice of the knowledge-base family
// (nineteen inventoried names, web/operations.json:244-262). With
// production_knowledge_import_linux.go (two served) and
// production_knowledge_search_linux.go (three served, one refused) it accounts for
// every name in the family:
//
//	kb:import-planning-text   BOUND    arity 3  [text, fileName, projectPath]
//	kb:save-document-copy     BOUND    arity 4  [docId, content, expectedContentHash, projectPath]
//	kb:reindex-document-copy  BOUND    arity 2  [docId, projectPath]
//	kb:remove-document        BOUND    arity 2  [docId, projectPath]
//	kb:clear-all              BOUND    arity 1  [projectPath]
//	kb:import-document        REFUSED  arity 2  [grantId, projectPath]
//	kb:import-folder          REFUSED  arity 2  [grantId, projectPath]
//	kb:import-reference-text  REFUSED  arity 3  [chapterNumber, runId, executionAuthority]
//
// # The reference
//
//	electron/controllers/kb-controller.ts:441-445 save-document-copy
//	electron/controllers/kb-controller.ts:447-450 reindex-document-copy
//	electron/controllers/kb-controller.ts:452-467 remove-document
//	electron/controllers/kb-controller.ts:469-475 clear-all
//	electron/controllers/kb-controller.ts:323-336 import-planning-text
//	electron/controllers/kb-controller.ts:213-228 the kb: envelope every handler runs in
//	electron/knowledge-base.ts:67-77              knowledgeFailureDetails
//	src/shared/ipc-channels.ts:1205-1238          the wire shapes
//	electron/knowledge-base.ts:418-433, :273-338  importText / importTextInternal
//	electron/knowledge-base.ts:192-222            saveDocumentCopy / reindexDocumentCopy
//	electron/knowledge-base.ts:224-238            removeDocument / clearAll
//	electron/vector-store.ts:132-176              the copy record, its reader and its predicate
//	electron/vector-store.ts:1226-1260            replaceDocumentChunks
//	electron/vector-store.ts:1265-1316            removeDocument
//	electron/vector-store.ts:1321-1343            clearAll
//
// # kb:import-planning-text is kb:import-text on this lane, by construction
//
// The reference's only difference between the two channels is the option it
// passes: importPlanningText is importText(..., { mode: 'fts-only' })
// (kb-controller.ts:323-336). `fts-only` is the ONLY mode this lane has - the
// store it writes carries `registry: null`, no embedding-space table and no
// vector row (production_knowledge_import_linux.go:78-102, and
// productionKnowledgeDecodeIndex refuses any table but documents/chunks) - so the
// served answer is importTextInternal's, field for field: same chunker, same
// chapter meta, same pruning, same copy. Delegating keeps the two names from
// drifting apart; a second copy of that write is a second place a chunker default
// could change.
//
// # What each mutation does to the two stores
//
// The reference keeps canonical rows, copy files and, whenever a model is
// configured, a vector table. Every mutation below touches all three there; this
// lane has the first two and can prove the absence of the third, so each port is
// the reference's own effect minus the vector half:
//
//	save-document-copy     the copy file only. The CAS on expectedContentHash and
//	                       the edited/indexDirty flags are
//	                       knowledgecopy.SaveManagedCopy, which preserves unknown
//	                       properties and refuses a missing or unmanaged
//	                       identity. No chunk row changes: a saved copy is
//	                       deliberately stale against the index until a reindex,
//	                       which is what indexDirty means
//	                       (vector-store.ts:168-176 isDocumentCopyIndexCurrent).
//	reindex-document-copy  the chunk rows and the document's chunkCount, then the
//	                       copy's indexedHash/indexDirty. Row identity is
//	                       replaceDocumentChunks' (:1226-1264): every chunk gets a
//	                       fresh id, the document id is unchanged, and
//	                       fileName/importedAt/chapter/corpusKind are inherited
//	                       from the row set being replaced. It clears no vector
//	                       rows because this store has none, and it does not
//	                       repair a torn chunk set: `ordered` refuses a gap, a
//	                       duplicate index or a stale totalChunks
//	                       (KNOWLEDGE_BASE_INDEX_INVALID) rather than silently
//	                       rewriting it.
//	remove-document        the document row, its chunk rows and its copy file. An
//	                       identity the store does not hold answers success and
//	                       writes nothing at all, exactly as
//	                       removeDocumentInternal's two "no rows remain" checks
//	                       do (:1265-1316).
//	clear-all              the canonical snapshot, the embedding-space registry if
//	                       one exists, and the knowledge-copies tree. The tree is
//	                       judged before anything is removed, so an unsafe tree
//	                       refuses with the store still whole (:1326-1331
//	                       KNOWLEDGE_COPY_UNSAFE_PATH).
//
// Every mutation is one atomic snapshot write (productionKnowledgeWriteAtomic)
// plus at most one copy-file replacement, so a crash leaves either the old store
// or the new one and never a mixture. The per-project source gate
// (productionKnowledgeImports.enter) serialises these five names with each other;
// the sibling import binder owns its own gate instance, so this type also accepts
// an existing *productionKnowledgeImports (newProductionKnowledgeDocumentsFrom)
// when the parent wants both binders to share one gate per project.
//
// # The three refusals, and the port each is missing
//
// kb:import-document and kb:import-folder take a grantId and nothing else: an
// externalFileGrants id minted after a native picker by
// dialog:select-knowledge-files / dialog:select-knowledge-folder
// (kb-controller.ts:523-562), then resolved with operation 'read' or 'list'. This
// lane binds neither picker - both are in the deliberately-501 list of
// synthetic_project_files_http_linux_test.go:368-370 - and the grant registry it
// does own (internal/librarypath) is minted only by the fs:grant-* binder, which
// is itself unbound. No token that can name a knowledge file can exist here, and
// substituting one (a path in the body, or a library token read as a knowledge
// grant) would hand the renderer a path authority the desktop never gave it. So
// the honest answer is 501, and the missing port is "a minter for the knowledge
// file and folder pickers". The import itself is already served: kb:import-text
// takes the text a picker would have read.
//
// kb:import-reference-text is refused because serving it cannot be complete. Its
// authority and its receipt are ImportRunRepository.resolveReferenceImportAuthority
// (electron/repositories/import-run-repository.ts:1454-1503) and
// commitReferenceImportReceipt (:1505-1600): the run must be a live
// reference-purpose run in the knowledge stage, the chapter row must pass
// assertFrozenChapterSnapshot (:762-776), and two tables must be written -
// import_reference_documents and import_run_knowledge_receipts. This lane has
// neither port: internal/store keeps importRunAssertExecutionAuthority unexported
// (internal/store/importrun.go:639-652) and answers it only through its own
// mutating methods, and no Go code writes either receipt table (the DDL is at
// internal/admission/baseline_v7_ddl.go:290-300 and :313-326; their only other
// reader is the knowledge-stage checkpoint validation at
// internal/store/importrun.go:404-470). Writing the document without the receipt
// would be strictly worse than refusing: the run's very next
// db:import-run-complete-batch for the knowledge stage validates every chapter
// against a committed receipt row and would answer
// IMPORT_RUN_REFERENCE_RECEIPT_INCOMPLETE forever, stranding the author's import
// with a document that no run can claim. The missing ports are therefore (1) an
// exported execution-authority assertion and (2) writers for those two tables.
//
// # Envelopes
//
// All five served names keep the reference's business envelope: HTTP 200 with
// {success, ...}, never a thrown IPC error, for every refusal this file can
// predict. Codes are symbolic because the browser's failure text is generated
// from registered codes; the reference's i18n prose is not reproduced here. A
// request whose shape is wrong (arity, argument type, a project path that
// disagrees with the frozen context) gets the lane's usual 400/409 instead of a
// business failure, because the reference's requireProjectPath threw for exactly
// those cases before any store was opened.
type productionKnowledgeDocuments struct {
	imports *productionKnowledgeImports
}

// newProductionKnowledgeDocuments is the constructor the binder block wires.
func newProductionKnowledgeDocuments(durable *productionDurableOpen) *productionKnowledgeDocuments {
	return &productionKnowledgeDocuments{imports: newProductionKnowledgeImports(durable)}
}

// newProductionKnowledgeDocumentsFrom shares one source gate with an existing
// import bridge. The parent wires it this way when it also binds kb:import-text,
// so a planning import and a copy save cannot interleave two read-modify-writes
// of one snapshot.
func newProductionKnowledgeDocumentsFrom(imports *productionKnowledgeImports) *productionKnowledgeDocuments {
	return &productionKnowledgeDocuments{imports: imports}
}

// Bind installs exactly the five document names. A nil bridge installs nothing,
// so they keep failing closed at GO_OPERATION_NOT_MIGRATED instead of being
// published as handlers that can only refuse.
func (k *productionKnowledgeDocuments) Bind(server *transport.Server) error {
	if k == nil || k.imports == nil || k.imports.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	// The arities count the inventoried arguments only; the frozen three-key
	// context is the extra trailing wire argument (ipc-channels.ts:1205-1238).
	bound := []struct {
		name  string
		arity int
		call  func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)
	}{
		{productionKnowledgePlanningTextName, 3, k.importPlanningText},
		{productionKnowledgeSaveCopyName, 4, k.saveCopy},
		{productionKnowledgeReindexCopyName, 2, k.reindexCopy},
		{productionKnowledgeRemoveDocumentName, 2, k.removeDocument},
		{productionKnowledgeClearAllName, 1, k.clearAll},
	}
	for _, entry := range bound {
		if err := server.Bind(entry.name, k.imports.operation(entry.arity, entry.call)); err != nil {
			return err
		}
	}
	return nil
}

const (
	productionKnowledgePlanningTextName   = "kb:import-planning-text"
	productionKnowledgeSaveCopyName       = "kb:save-document-copy"
	productionKnowledgeReindexCopyName    = "kb:reindex-document-copy"
	productionKnowledgeRemoveDocumentName = "kb:remove-document"
	productionKnowledgeClearAllName       = "kb:clear-all"
)

const (
	// productionKnowledgeSpaceRegistryName is the desktop's embedding-space
	// registry (vector-store.ts:126). This lane never writes one; clear-all still
	// removes it, because that is the reference's own clear step (:1335-1339) and a
	// store that has one is a store this lane cannot otherwise serve.
	productionKnowledgeSpaceRegistryName = "embedding-spaces.json"
	// productionKnowledgeTreeDepth bounds the only recursive removal in this
	// family: the knowledge-copies tree, whose files are flat in the reference
	// (vector-store.ts:135-141).
	productionKnowledgeTreeDepth = 8
)

const (
	productionKnowledgeCodeCopyRequestInvalid = "KNOWLEDGE_BASE_COPY_REQUEST_INVALID"
	productionKnowledgeCodeCopyUnavailable    = "KNOWLEDGE_BASE_COPY_UNAVAILABLE"
	productionKnowledgeCodeCopyChanged        = "KNOWLEDGE_BASE_COPY_CHANGED"
	productionKnowledgeCodeCopyEmpty          = "KNOWLEDGE_BASE_COPY_EMPTY"
	productionKnowledgeCodeCopyInvalid        = "KNOWLEDGE_BASE_COPY_INVALID"
	productionKnowledgeCodeCopyUnsafe         = "KNOWLEDGE_COPY_UNSAFE_PATH"
	productionKnowledgeCodeChunksMissing      = "KNOWLEDGE_DOCUMENT_CHUNKS_MISSING"
	productionKnowledgeCodeRemoveFailed       = "KNOWLEDGE_BASE_DOCUMENT_REMOVE_FAILED"
	productionKnowledgeCodeClearFailed        = "KNOWLEDGE_BASE_CLEAR_FAILED"
)

// productionKnowledgeCopyRefusal is the reference's {success:false,error} body for
// the four mutations whose return type is {success:boolean, error?:string}
// (src/shared/ipc-channels.ts:1229-1238).
func productionKnowledgeCopyRefusal(code string) map[string]any {
	return map[string]any{"success": false, "error": code}
}

// productionKnowledgeDocumentStore is one mutation's whole scope: the shared
// per-project gate, the confined storage root and the validated index. Close is
// the only way out, and it releases the gate even when closing the root fails.
type productionKnowledgeDocumentStore struct {
	storage *productionKnowledgeStorage
	index   productionKnowledgeIndex
	release func()
}

// open takes the project gate, opens the storage root, refuses a legacy vector
// store and decodes the canonical index. Every served name in this file starts
// here, so none of them writes beside a store it did not read.
func (k *productionKnowledgeDocuments) open(ctx context.Context,
	projectPath string) (*productionKnowledgeDocumentStore, error) {
	if k == nil || k.imports == nil {
		return nil, errProductionKnowledgeStorageUnsafe
	}
	release, err := k.imports.enter(projectPath)
	if err != nil {
		return nil, err
	}
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		release()
		return nil, err
	}
	// The migration barrier comes first, in the order the import adapter uses
	// (production_knowledge_import_linux.go:793-796): a legacy vector store blocks
	// every mutation here, whatever state its canonical snapshot is in.
	if err := storage.refuseLegacyVectors(); err != nil {
		_ = storage.Close()
		release()
		return nil, err
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		_ = storage.Close()
		release()
		return nil, err
	}
	return &productionKnowledgeDocumentStore{storage: storage, index: index, release: release}, nil
}

// Close releases the gate and the storage root. A nil store is a no-op so a
// handler's defer is unconditional.
func (s *productionKnowledgeDocumentStore) Close() error {
	if s == nil {
		return nil
	}
	var failure error
	if s.storage != nil {
		failure = s.storage.Close()
		s.storage = nil
	}
	if s.release != nil {
		release := s.release
		s.release = nil
		release()
	}
	return failure
}

// importPlanningText serves `kb:import-planning-text`. It shares importText's
// request path and envelope on purpose: see the header.
func (k *productionKnowledgeDocuments) importPlanningText(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	text, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	fileName, ok := args[1].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[2], contextValue)
	if err != nil {
		return nil, err
	}
	answer, err := k.imports.write(ctx, projectPath, productionKnowledgeRequest{Text: text, FileName: fileName})
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	return answer, nil
}

// saveCopy serves `kb:save-document-copy`, the only caller of
// knowledgecopy.SaveManagedCopy. The four checks the reference makes before
// writing - three strings, the migration barrier, a managed document, and the
// content-hash CAS - are the same four, in the same order.
func (k *productionKnowledgeDocuments) saveCopy(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	documentID, identityOK := args[0].(string)
	content, contentOK := args[1].(string)
	expectedHash, hashOK := args[2].(string)
	if !identityOK || !contentOK || !hashOK {
		// The reference answers here, before any store is opened
		// (knowledge-base.ts:192-200), and the answer is a business failure.
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeCopyRequestInvalid), nil
	}
	projectPath, err := productionKnowledgeRequestPath(args[3], contextValue)
	if err != nil {
		return nil, err
	}
	store, err := k.open(ctx, projectPath)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	defer func() { _ = store.Close() }()
	document, present := store.index.find(documentID)
	if !present || document.FilePath != productionKnowledgeCopyMarker+documentID {
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeCopyUnavailable), nil
	}
	err = knowledgecopy.SaveManagedCopy(ctx, store.storage.storage, productionKnowledgeCopyReferences(store.index),
		productionKnowledgeCopyBudget(), documentID, content, expectedHash)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeCopyRefusal(productionKnowledgeCopyFailureCode(err)), nil
	}
	if err := store.storage.verify(); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	return map[string]any{"success": true}, nil
}

// reindexCopy serves `kb:reindex-document-copy`: the copy's text becomes the
// canonical chunk set, and only then does the copy stop being dirty.
func (k *productionKnowledgeDocuments) reindexCopy(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	documentID, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[1], contextValue)
	if err != nil {
		return nil, err
	}
	store, err := k.open(ctx, projectPath)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	defer func() { _ = store.Close() }()
	document, present := store.index.find(documentID)
	if !present || document.FilePath != productionKnowledgeCopyMarker+documentID {
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeCopyUnavailable), nil
	}
	// One managed reference is the whole metadata proof this mutation needs, and
	// it is the reference's own read (readKnowledgeCopy, vector-store.ts:144-166)
	// expressed through the package that owns the format. The marker check is the
	// same one the import lane makes (production_knowledge_import_linux.go:925).
	copied, err := productionKnowledgeReadCopy(ctx, store.storage.storage, documentID)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeCopyRefusal(productionKnowledgeCopyFailureCode(err)), nil
	}
	chunks, err := productionKnowledgeChunksOf(ctx, copied.Content)
	if err != nil {
		if errors.Is(err, errProductionKnowledgeTextEmpty) {
			// `!copy.content.trim()` (knowledge-base.ts:213): a copy holding nothing but
			// whitespace has no chunk set to reindex.
			return productionKnowledgeCopyRefusal(productionKnowledgeCodeCopyEmpty), nil
		}
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	previous, err := store.index.ordered(documentID)
	if err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	if len(previous) == 0 {
		// replaceDocumentChunks refuses a document whose rows are gone (:1240).
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeChunksMissing), nil
	}
	rows, err := productionKnowledgeReindexRows(documentID, previous[0], chunks)
	if err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	if err := productionKnowledgeWriteIndex(ctx, store.storage.storage, store.index.withChunks(documentID, rows)); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	// The copy is written last, exactly as the reference does: indexDirty clears
	// only once the rows it describes are durable.
	copied.IndexedHash = productionKnowledgeHash(copied.Content)
	copied.IndexDirty = false
	if err := productionKnowledgeWriteCopyFile(store.storage.storage, documentID, copied); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	if err := productionKnowledgeVerifyReindex(ctx, store.storage.storage, documentID, chunks); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	if err := store.storage.verify(); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeFailureCode(err)), nil
	}
	return map[string]any{"success": true, "docId": documentID, "chunkCount": int64(len(chunks))}, nil
}

// removeDocument serves `kb:remove-document`. An identity this store does not
// hold is answered, not refused, and nothing is written for it.
func (k *productionKnowledgeDocuments) removeDocument(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	documentID, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[1], contextValue)
	if err != nil {
		return nil, err
	}
	store, err := k.open(ctx, projectPath)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	defer func() { _ = store.Close() }()
	if err := productionKnowledgeRemoveFromStorage(ctx, store.storage, store.index, documentID); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeRemoveFailed), nil
	}
	return map[string]any{"success": true}, nil
}

// productionKnowledgeRemoveFromStorage is removeDocumentInternal's canonical and
// copy halves over an already-opened, already-validated store
// (vector-store.ts:1265-1290): the document row and every chunk of that identity
// leave the snapshot in one atomic write, a document the store never held writes
// no snapshot at all, then the reference-format copy file is removed whatever the
// row set held, and the storage inode is re-proved before the caller is told the
// removal happened. It is the one implementation of that sequence, shared by
// kb:remove-document and the finalized-chapter deletion cleaner.
func productionKnowledgeRemoveFromStorage(ctx context.Context, storage *productionKnowledgeStorage,
	index productionKnowledgeIndex, documentID string) error {
	if storage == nil || storage.storage == nil {
		return errProductionKnowledgeStorageUnsafe
	}
	if next, removed := index.without(documentID); removed {
		if err := productionKnowledgeWriteIndex(ctx, storage.storage, next); err != nil {
			return err
		}
	}
	if err := productionKnowledgeRemoveCopy(storage.storage, documentID); err != nil {
		return err
	}
	return storage.verify()
}

// productionKnowledgeRemoveLinkedDocument is the deletion family's
// removeKnowledgeDocument (electron/services/chapter-deletion-service.ts:27-35)
// for the store this lane owns: it takes the same per-project source gate every
// kb mutation takes (productionKnowledgeImports.enter is the
// withKnowledgeSourceGate of vector-store.ts:1261-1263), refuses an unmigrated
// legacy vector store with the reference's own blocked code, decodes the
// canonical snapshot through the readers' validator, and then removes the
// identity and its copy. An identity the snapshot does not hold is a successful
// no-op, exactly as removeDocumentInternal's two zero-row checks are. The empty
// identity is a caller bug and never a removal: no document has that id, and the
// copy path it would name is not a document's.
func productionKnowledgeRemoveLinkedDocument(ctx context.Context, imports *productionKnowledgeImports,
	projectPath, documentID string) error {
	if imports == nil || projectPath == "" || documentID == "" {
		return errProductionKnowledgeStorageUnsafe
	}
	release, err := imports.enter(projectPath)
	if err != nil {
		return err
	}
	defer release()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		return err
	}
	defer func() { _ = storage.Close() }()
	// ensureMigration before anything else, in the order kb:remove-document and
	// the import adapter take it (production_knowledge_documents_linux.go:284-289).
	if err := storage.refuseLegacyVectors(); err != nil {
		return err
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		return err
	}
	return productionKnowledgeRemoveFromStorage(ctx, storage, index, documentID)
}

// clearAll serves `kb:clear-all`. The copies tree is judged before the snapshot
// is removed, so an unsafe tree refuses with the store still whole.
func (k *productionKnowledgeDocuments) clearAll(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	projectPath, err := productionKnowledgeRequestPath(args[0], contextValue)
	if err != nil {
		return nil, err
	}
	store, err := k.open(ctx, projectPath)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	defer func() { _ = store.Close() }()
	if err := productionKnowledgeCopyTreeSafe(store.storage.storage); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeCopyUnsafe), nil
	}
	for _, name := range []string{
		productionKnowledgeIndexName,
		productionKnowledgeSpaceRegistryName,
	} {
		if err := store.storage.storage.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return productionKnowledgeCopyRefusal(productionKnowledgeCodeClearFailed), nil
		}
	}
	if err := productionKnowledgeRemoveTree(store.storage.storage, productionKnowledgeCopyDirectory,
		productionKnowledgeTreeDepth); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeClearFailed), nil
	}
	if err := store.storage.verify(); err != nil {
		return productionKnowledgeCopyRefusal(productionKnowledgeCodeClearFailed), nil
	}
	return map[string]any{"success": true}, nil
}

// productionKnowledgeCopyReferences is the complete admitted document list as the
// copy package wants it. A caller-supplied id alone never grants authority over a
// file, so the full list is what makes SaveManagedCopy's managed check meaningful.
func productionKnowledgeCopyReferences(index productionKnowledgeIndex) []knowledgecopy.DocumentRef {
	references := make([]knowledgecopy.DocumentRef, 0, len(index.Documents))
	for _, document := range index.Documents {
		references = append(references, knowledgecopy.DocumentRef{ID: document.ID, FilePath: document.FilePath})
	}
	return references
}

// productionKnowledgeReadCopy is readKnowledgeCopy for one identity, through the
// package that owns the format. A copy that is absent is not an error: it is the
// reference's `null`, which every caller here turns into COPY_UNAVAILABLE.
func productionKnowledgeReadCopy(ctx context.Context, storage *os.Root,
	documentID string) (knowledgecopy.Copy, error) {
	references := []knowledgecopy.DocumentRef{{ID: documentID, FilePath: productionKnowledgeCopyMarker}}
	copies, err := knowledgecopy.ReadManagedCopies(ctx, storage, references, productionKnowledgeCopyBudget())
	if err != nil {
		return knowledgecopy.Copy{}, err
	}
	copied, present := copies[documentID]
	if !present {
		return knowledgecopy.Copy{}, knowledgecopy.ErrCopyUnavailable
	}
	return copied, nil
}

// productionKnowledgeCopyFailureCode maps one copy error to one symbolic code.
// The first two are the reference's own registered codes; the rest are this
// lane's, because the reference would have thrown a driver or filesystem message
// there and the browser only renders registered codes.
func productionKnowledgeCopyFailureCode(err error) string {
	switch {
	case errors.Is(err, knowledgecopy.ErrCopyUnavailable):
		return productionKnowledgeCodeCopyUnavailable
	case errors.Is(err, knowledgecopy.ErrCopyChanged):
		return productionKnowledgeCodeCopyChanged
	case errors.Is(err, knowledgecopy.ErrBudget),
		errors.Is(err, knowledgecopy.ErrInvalid),
		errors.Is(err, knowledgecopy.ErrInput),
		errors.Is(err, knowledgecopy.ErrUnsupported),
		errors.Is(err, knowledgecopy.ErrLegacyUnsupported):
		return productionKnowledgeCodeCopyInvalid
	}
	return productionKnowledgeFailureCode(err)
}

// withChunks is replaceDocumentChunks' canonical half: the document keeps its
// identity, fileName, importedAt and chapter rows, its chunk set becomes exactly
// the new rows, and its chunkCount is their number.
func (i productionKnowledgeIndex) withChunks(documentID string, rows []productionKnowledgeChunk) productionKnowledgeIndex {
	next := productionKnowledgeIndex{
		Documents: make([]productionKnowledgeDocument, 0, len(i.Documents)),
		Chunks:    make([]productionKnowledgeChunk, 0, len(i.Chunks)+len(rows)),
	}
	for _, document := range i.Documents {
		if document.ID == documentID {
			document.ChunkCount = int64(len(rows))
		}
		next.Documents = append(next.Documents, document)
	}
	for _, chunk := range i.Chunks {
		if chunk.DocID == documentID {
			continue
		}
		next.Chunks = append(next.Chunks, chunk)
	}
	next.Chunks = append(next.Chunks, rows...)
	return next
}

// without is removeDocumentInternal's canonical half: the document row and every
// chunk of that identity are dropped, and nothing else is touched. The boolean
// says whether anything was there to drop, so removing an identity the store
// never held writes no file.
func (i productionKnowledgeIndex) without(documentID string) (productionKnowledgeIndex, bool) {
	if documentID == "" {
		return i, false
	}
	removed := false
	next := productionKnowledgeIndex{
		Documents: make([]productionKnowledgeDocument, 0, len(i.Documents)),
		Chunks:    make([]productionKnowledgeChunk, 0, len(i.Chunks)),
	}
	for _, document := range i.Documents {
		if document.ID == documentID {
			removed = true
			continue
		}
		next.Documents = append(next.Documents, document)
	}
	for _, chunk := range i.Chunks {
		if chunk.DocID == documentID {
			removed = true
			continue
		}
		next.Chunks = append(next.Chunks, chunk)
	}
	if !removed {
		return i, false
	}
	return next, true
}

// productionKnowledgeReindexRows mints the replacement chunk rows: one fresh id
// per chunk, the new text and position, and every other column inherited from the
// row set being replaced (replaceDocumentChunks :1241).
func productionKnowledgeReindexRows(documentID string, seed productionKnowledgeChunk,
	chunks []string) ([]productionKnowledgeChunk, error) {
	rows := make([]productionKnowledgeChunk, 0, len(chunks))
	for position, text := range chunks {
		identity, err := productionKnowledgeUUID()
		if err != nil {
			return nil, errProductionKnowledgeStorageUnsafe
		}
		rows = append(rows, productionKnowledgeChunk{
			ID:            identity,
			DocID:         documentID,
			FileName:      seed.FileName,
			Text:          text,
			ChunkIndex:    int64(position),
			TotalChunks:   int64(len(chunks)),
			ImportedAt:    seed.ImportedAt,
			ChapterNumber: seed.ChapterNumber,
			ChapterTitle:  seed.ChapterTitle,
			CorpusKind:    seed.CorpusKind,
		})
	}
	return rows, nil
}

// productionKnowledgeVerifyReindex re-reads the committed index and proves the
// document's chunk set is exactly the new text in order - the readback the import
// adapter performs after a write, so a reindex receipt is never a restatement of
// the request.
func productionKnowledgeVerifyReindex(ctx context.Context, storage *os.Root, documentID string,
	chunks []string) error {
	committed, err := productionKnowledgeReadIndex(ctx, storage)
	if err != nil {
		return err
	}
	ordered, err := committed.ordered(documentID)
	if err != nil {
		return err
	}
	document, present := committed.find(documentID)
	if !present || document.ChunkCount != int64(len(chunks)) || len(ordered) != len(chunks) {
		return errProductionKnowledgeIndexInvalid
	}
	for position, chunk := range ordered {
		if chunk.Text != chunks[position] || chunk.ChunkIndex != int64(position) ||
			chunk.TotalChunks != int64(len(chunks)) || chunk.DocID != documentID {
			return errProductionKnowledgeIndexInvalid
		}
	}
	return nil
}

// productionKnowledgeWriteCopyFile writes a whole copy record, which the import
// adapter's three-argument writer cannot express: a reindex keeps `edited` and
// clears `indexDirty` (replaceDocumentChunks :1246). Property order matches the
// reference's JSON.stringify of the same four keys.
func productionKnowledgeWriteCopyFile(storage *os.Root, documentID string, copied knowledgecopy.Copy) error {
	body, err := json.Marshal(struct {
		Content     string `json:"content"`
		IndexedHash string `json:"indexedHash"`
		Edited      bool   `json:"edited"`
		IndexDirty  bool   `json:"indexDirty"`
	}{
		Content:     copied.Content,
		IndexedHash: copied.IndexedHash,
		Edited:      copied.Edited,
		IndexDirty:  copied.IndexDirty,
	})
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	child, err := productionKnowledgeCopyRoot(storage, true)
	if err != nil {
		return err
	}
	if child == nil {
		return errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = child.Close() }()
	return productionKnowledgeWriteAtomic(child, productionKnowledgeCopyName(documentID), body)
}

// productionKnowledgeCopyTreeSafe judges the knowledge-copies tree before
// clear-all removes anything: the reference's lstat guard
// (vector-store.ts:1335-1338) answers KNOWLEDGE_COPY_UNSAFE_PATH for a symlink or
// a non-directory, and this lane refuses that tree one step earlier so the
// refusal cannot leave a half-cleared store.
func productionKnowledgeCopyTreeSafe(storage *os.Root) error {
	info, err := storage.Lstat(productionKnowledgeCopyDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errProductionKnowledgeStorageUnsafe
	}
	return nil
}

// productionKnowledgeRemoveTree removes one directory tree below the confined
// root, depth-bounded. It never follows a symlink: Lstat decides, and Remove
// refuses anything that is not a file or an empty directory.
func productionKnowledgeRemoveTree(storage *os.Root, name string, depth int) error {
	if depth <= 0 {
		return errProductionKnowledgeStorageUnsafe
	}
	info, err := storage.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errProductionKnowledgeStorageUnsafe
	}
	child, err := storage.OpenRoot(name)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	if err := productionKnowledgeEmptyTree(child, depth-1); err != nil {
		_ = child.Close()
		return err
	}
	if err := child.Close(); err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	if err := storage.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errProductionKnowledgeStorageUnsafe
	}
	return nil
}

// productionKnowledgeEmptyTree removes every entry of an opened directory. A
// nested directory is emptied recursively; a symlink is removed by name like any
// other entry, because Lstat never follows it - which is what the reference's
// fs.rmSync(recursive) does too.
func productionKnowledgeEmptyTree(root *os.Root, depth int) error {
	if depth <= 0 {
		return errProductionKnowledgeStorageUnsafe
	}
	directory, err := root.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	entries, readErr := directory.ReadDir(-1)
	closeErr := directory.Close()
	if readErr != nil || closeErr != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	for _, entry := range entries {
		name := entry.Name()
		info, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return errProductionKnowledgeStorageUnsafe
		}
		if info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			child, err := root.OpenRoot(name)
			if err != nil {
				return errProductionKnowledgeStorageUnsafe
			}
			if err := productionKnowledgeEmptyTree(child, depth-1); err != nil {
				_ = child.Close()
				return err
			}
			if err := child.Close(); err != nil {
				return errProductionKnowledgeStorageUnsafe
			}
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errProductionKnowledgeStorageUnsafe
		}
	}
	return nil
}

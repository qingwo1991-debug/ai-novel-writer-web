//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's READ slice of the knowledge-base family.
// With production_knowledge_import_linux.go (two served),
// production_knowledge_uploads_linux.go (three served),
// production_knowledge_documents_linux.go (five served) and
// production_knowledge_search_linux.go (three served, one refused) it accounts
// for every inventoried name in that family:
//
//	kb:list-documents        BOUND here  arity 1 [projectPath]
//	kb:stats                 BOUND here  arity 1 [projectPath]
//	kb:get-vectorless-count  BOUND here  arity 1 [projectPath]
//	kb:read-document-copy    BOUND here  arity 2 [docId, projectPath]
//	kb:search                BOUND here  arity 3 [query, topK|undefined, projectPath]
//
// Until this file the author could put material into the knowledge base and then
// had no served way to see it, read it back or search it: the five import-side
// names served, kb:backfill-vectors was refused on purpose
// (production_knowledge_search_linux.go:12-20), and all five read names here
// answered 501 GO_OPERATION_NOT_MIGRATED.
//
// # The reference
//
//	electron/controllers/kb-controller.ts:424-434   list-documents + the finalized annotation
//	electron/controllers/kb-controller.ts:436-439   read-document-copy
//	electron/controllers/kb-controller.ts:477-480   stats
//	electron/controllers/kb-controller.ts:482-485   get-vectorless-count
//	electron/controllers/kb-controller.ts:379-389   search
//	electron/controllers/kb-controller.ts:213-228   the kb: envelope every handler runs in
//	electron/knowledge-base.ts:176-190              readDocumentCopy, and its two refusals
//	electron/knowledge-base.ts:240-248              getKnowledgeStats
//	electron/knowledge-base.ts:734-737              getVectorlessCount
//	electron/knowledge-base.ts:848-862              searchKnowledgeFTS
//	electron/repositories/finalization-repository.ts:405-410 knowledgeDocumentChapters
//	src/shared/ipc-channels.ts:1221-1242             the frozen wire shapes
//	web/operations.json:250, :253, :254, :259, :260  the five names
//
// # The decision: adapters over the frozen transport binders, not new handlers
//
// All five names are served through (*transport.Server).BindKnowledge
// (internal/transport/knowledge.go:36) and (*transport.Server).BindKnowledgeSearch
// (internal/transport/knowledge_search.go:31), which already carry the envelope
// each name was frozen with: the argument and context agreement checks, the
// `[]map[string]any` list, the three stats keys, the `{count}` object, the
// `{available,content,contentHash,edited,indexStatus}` copy read, and the
// `[]SearchResult` with its 0..100 topK bound and its ErrInput -> 400 mapping.
// Re-encoding those here would be a second copy of a tested contract and a
// second place a wire key could drift, so this file is only a provider: it
// implements KnowledgeProvider and KnowledgeSearchProvider and hands the
// transport a knowledgequery.View and a knowledgequery.SearchCapture.
//
// Every interface method is honourable, so no name is refused:
//
//	Documents(ctx)      -> kb:list-documents (the transport adds finalizedChapter)
//	Stats(ctx)          -> kb:stats
//	VectorlessCount(ctx)-> kb:get-vectorless-count
//	ReadCopy(ctx, id)   -> kb:read-document-copy
//	SearchFTS(...)      -> kb:search (the transport's own call, this file's capture)
//
// # One store, decoded per request, so an import is visible the moment it returns
//
// Each call re-decodes .ai-novel/knowledge-snapshot.json through the same
// productionKnowledgeReadIndex the import lane validates before it writes, so a
// document the writer just committed is readable on the very next request. The
// alternative - internal/projectsession's KnowledgeManager, which mounts one
// capture at project:open (internal/projectsession/knowledge.go:57-95) - would
// freeze the store at open time and hide every later import, which is exactly the
// bug this file exists to fix. `registry` is JSON null in this store, so
// knowledgequery reads zero spaces and no vector dimension: FTS-only, which is
// the reference's own mode whenever it has no query vector
// (knowledge-base.ts:143-160, :848-862).
//
// The path is never a request value. begin borrows the admitted author database
// for the exact cookie session, tab and lease (productionDurableOpen
// .borrowAuthorDatabase, production_durable_linux.go:313-360): that borrow is the
// authority for every one of the five, and a foreign or stale context is refused
// before any knowledge file is touched. The database is also what makes
// kb:list-documents' annotation honest - the finalized map is
// finalization_outbox.knowledge_document_id -> chapter_number read over that same
// borrowed handle, i.e. FinalizationRepository.knowledgeDocumentChapters through
// its port internal/store, so a document linked by kb:import-finalized-text shows
// its chapter and one imported by kb:import-text has no chapter at all.
//
// # What the two counts mean in a store with no vectors
//
// kb:stats answers the truth of this store, not zeroes: documentCount and
// totalChunks are the `documents` and `chunks` row counts the writer just wrote,
// and vectorDimension is 0 because no embedding-space registry exists
// (knowledgequery/view.go:137-152 reads the active generation's dimension, and
// there is none). kb:get-vectorless-count answers every chunk, because a chunk
// with no vector row is exactly what `registry: null` means. Reporting 0 chunks
// here - the value the reference returns when it has not looked
// (kb-controller.ts:492-500) - would deny the author's own documents; reporting
// all chunks as vectorless is the sentence the renderer needs to stop offering a
// vector rebuild.
//
// # kb:read-document-copy refuses a missing copy by name
//
// A document whose filePath is not the knowledge-copy marker, or whose copy file
// is gone, answers {available:false,indexStatus:'unavailable'} with no `content`
// key at all. That is readDocumentCopy's own two `return {available:false...}`
// arms (knowledge-base.ts:182-184), not an empty document: the renderer's editor
// must not open a blank buffer for content the lane cannot vouch for, and an empty
// `content` would be indistinguishable from an author who saved an empty copy.
//
// # kb:search cannot disagree with kb:search-with-scope
//
// It is the same query path: the same borrow, the same gate, the same
// productionKnowledgeReadIndex decode, the same knowledgequery.NewSearchCapture,
// and the same SearchFTS ranking with the same 0.5 score, (fileName,text) dedupe
// and stable order (knowledgequery/search.go:92-180). Only the arguments differ,
// and kb:search is the family's default scope: it passes neither a chapter scope
// nor an excluded corpus kind, which is verbatim what the reference's kb:search
// does (kb-controller.ts:379-389), while kb:search-writing-context excludes
// 'reference' and kb:search-with-scope passes a range. The test below asserts the
// three answers byte-for-byte equal on a corpus where the exclusions cannot bite.
// The one envelope where the three can differ is a legacy vector store
// (deviation 1): the hand-written search binders can answer the reference's 200
// `{success:false,errorCode}`, while a View-shaped binder cannot, so this file's
// kb:search refuses with the same code at HTTP 409 and their two keep the body.
//
// # Three deviations this file has to state
//
//  1. A store carrying an unmigrated vectors.json is the one envelope the frozen
//     transport cannot reproduce. The reference answers list/stats/count/copy with
//     `{success:false, errorCode:'LEGACY_VECTOR_MIGRATION_BLOCKED', error:prose}`
//     (kb-controller.ts:143-152, :219-221), a body the View-shaped binder has no
//     way to return; the honest substitute is the same registered code as an HTTP
//     409 refusal, which is a named refusal rather than a lie about an empty
//     store. A View-shaped binder has no way to answer that body, so the code is
//     carried by the one shape the binder does map: an *security.HTTPError. All
//     four document reads refuse through productionKnowledgeReadRefusal for that
//     reason - a sentinel alone is invisible to knowledgeReadError's allowlist and
//     would surface as an unmapped 500 - and none of them is exempt. kb:search
//     takes the same seam. Every other kb: name in this lane keeps the 200 body:
//     kb:search-with-scope and kb:search-writing-context are hand-written map
//     handlers (production_knowledge_search_linux.go:243-247), so they keep the
//     reference's 200 for the same store where kb:search, served by the frozen
//     binder, can only refuse. The query path, ranking and degradation are still
//     identical; the envelope is not, and this is the only case.
//  2. A store whose snapshot this reader cannot decode answers [] from
//     kb:search - never a refusal and never a partial index - because that is
//     verbatim what searchWithScopeInternal does with an unreadable table
//     (vector-store.ts:1419, :1512-1520) and because kb:search and
//     kb:search-with-scope must degrade identically. The four document reads
//     refuse instead: the reference lets the equivalent throw escape
//     (kb-controller.ts:213-228), and zeros for a store that exists but cannot be
//     read would be the same lie as zeros for a store nobody looked at. On the
//     wire that refusal is the transport's own KNOWLEDGE_READ_FAILED - the generic
//     read failure every page already renders, and the only code this file has for
//     a store that exists and cannot be decoded. It is a refusal with a code, not
//     an unmapped error, and the tests assert it by name.
//  3. The finalized annotation is read on all four document reads, not only
//     kb:list-documents, because the frozen binder hands back one View and one
//     chapter map for the whole group. The cost is one indexed SELECT; the
//     difference is reachable only for a project whose own finalization_outbox is
//     unreadable, where kb:stats and kb:read-document-copy refuse instead of
//     answering. Every id this lane writes is a UUID or a hex digest, which the
//     outbox read accepts, so its own documents are never refused by it.
//
// A refusal this adapter returns reaches the browser through
// transport.knowledgeReadError (internal/transport/knowledge.go:147-168), which
// keeps only an *security.HTTPError plus the context, ownership, provider and
// knowledgequery symbols; any other error answers KNOWLEDGE_READ_FAILED rather
// than prose. That is the point of that function - a repository error can carry
// private content - so no refusal below tries to smuggle a message past it, and
// every refusal that has a registered code is built by productionKnowledgeReadRefusal
// instead of being returned as a bare sentinel.
//
// Read-only: nothing here writes a byte. Every open is a confined os.Root read,
// the only database access is one SELECT, and a refused call leaves both stores
// untouched (the tests digest them across the refusal arms).
type productionKnowledgeReads struct {
	imports *productionKnowledgeImports
}

// newProductionKnowledgeReads is the constructor a standalone binder block wires.
func newProductionKnowledgeReads(durable *productionDurableOpen) *productionKnowledgeReads {
	return &productionKnowledgeReads{imports: newProductionKnowledgeImports(durable)}
}

// newProductionKnowledgeReadsFrom shares one source gate with an existing import
// bridge, so a read cannot decode the snapshot while an import is replacing it
// through a second mutex of its own. This is the constructor the binder block
// wires, with the same *productionKnowledgeImports the import, upload, document
// and search binders already hold.
func newProductionKnowledgeReadsFrom(imports *productionKnowledgeImports) *productionKnowledgeReads {
	return &productionKnowledgeReads{imports: imports}
}

// Bind installs exactly the five read names. A nil bridge installs nothing, so
// they keep failing closed at GO_OPERATION_NOT_MIGRATED instead of being
// published as handlers that can only refuse. BindKnowledge and
// BindKnowledgeSearch each validate every name before installing any of them, so
// a collision leaves the server with none of the five rather than a partial set.
func (k *productionKnowledgeReads) Bind(server *transport.Server) error {
	if k == nil || k.imports == nil || k.imports.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if err := server.BindKnowledge(k); err != nil {
		return err
	}
	return server.BindKnowledgeSearch(k)
}

const (
	productionKnowledgeListDocumentsName   = "kb:list-documents"
	productionKnowledgeStatsName           = "kb:stats"
	productionKnowledgeVectorlessCountName = "kb:get-vectorless-count"
	productionKnowledgeReadCopyName        = "kb:read-document-copy"
	productionKnowledgeSearchChannelName   = "kb:search"
)

// productionKnowledgeReadStore is one call's whole scope: the borrowed author
// database (authority, and the finalized map), the per-project knowledge gate,
// the confined storage root, the decoded index and the outbox annotation. Close
// is the only way out, and it releases the gate and the borrow exactly once.
type productionKnowledgeReadStore struct {
	storage  *productionKnowledgeStorage
	index    productionKnowledgeIndex
	chapters map[string]int64
	once     sync.Once
	release  func()
}

// open is the shared prologue of all five names: parse the frozen context,
// borrow the author database, take the project gate, open the storage root,
// refuse a legacy vector store, decode the canonical snapshot, and read the
// outbox annotation. The gate is taken after the borrow and released before it,
// the same order the import lane commits in (production_knowledge_import_linux.go:
// 783-796), so a read can never hold the gate while waiting on the database.
func (k *productionKnowledgeReads) open(ctx context.Context,
	caller transport.Caller, expected any) (*productionKnowledgeReadStore, error) {
	if k == nil || k.imports == nil || k.imports.durable == nil {
		return nil, errors.New("KNOWLEDGE_PROVIDER_UNAVAILABLE")
	}
	contextValue, err := projectsession.ParseContext(expected)
	if err != nil {
		return nil, err
	}
	db, borrowed, err := k.imports.durable.borrowAuthorDatabase(ctx, caller, expected)
	if err != nil {
		return nil, err
	}
	if db == nil || borrowed == nil {
		if borrowed != nil {
			borrowed()
		}
		return nil, errors.New("KNOWLEDGE_PROVIDER_UNAVAILABLE")
	}
	gate, err := k.imports.enter(contextValue.ProjectPath)
	if err != nil {
		borrowed()
		return nil, err
	}
	scope := &productionKnowledgeReadStore{release: func() {
		gate()
		borrowed()
	}}
	storage, err := productionKnowledgeOpenStorage(contextValue.ProjectPath)
	if err != nil {
		scope.Close()
		return nil, err
	}
	scope.storage = storage
	// The migration barrier comes first, in the order every served kb: name uses
	// (knowledge-base.ts:60-65 throws for every read): a store this lane must not
	// touch is refused before its snapshot is decoded.
	if err := storage.refuseLegacyVectors(); err != nil {
		scope.Close()
		return nil, err
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		scope.Close()
		return nil, err
	}
	scope.index = index
	// One SELECT over the borrowed handle: the reference's own annotation source
	// (finalization-repository.ts:405-410). The owner proof is the lane's existing
	// verifier, which a read never consults - it exists so a future caller cannot
	// reach a mutating method through a nil verifier.
	chapters, err := store.NewFinalizationOutbox(db, store.FinalizationScope{
		ProjectID: contextValue.ProjectID,
		Epoch:     contextValue.LeaseID,
	}, durableFinalizationOwner{expected: contextValue}).KnowledgeDocumentChapters(ctx)
	if err != nil {
		scope.Close()
		return nil, err
	}
	scope.chapters = chapters
	return scope, nil
}

// Close releases the storage root, the gate and the borrow exactly once, so a
// failure path and the transport's deferred release cannot run it twice.
func (s *productionKnowledgeReadStore) Close() {
	if s == nil {
		return
	}
	s.once.Do(func() {
		if s.storage != nil {
			_ = s.storage.Close()
			s.storage = nil
		}
		if s.release != nil {
			release := s.release
			s.release = nil
			release()
		}
	})
}

// BeginKnowledge serves the four document reads: one immutable View of the store
// as it stands right now, plus the finalized chapter map the transport annotates
// list-documents with. A nonnil release holds the borrow and the gate until the
// transport has finished building the answer.
func (k *productionKnowledgeReads) BeginKnowledge(ctx context.Context, caller transport.Caller,
	expected any) (*knowledgequery.View, map[string]int64, func(), error) {
	scope, err := k.open(ctx, caller, expected)
	if err != nil {
		return nil, nil, nil, productionKnowledgeReadRefusal(err)
	}
	view, err := knowledgequery.New(ctx, productionKnowledgeSnapshotOf(scope.index),
		scope.storage.storage, productionKnowledgeCopyBudget())
	if err != nil {
		scope.Close()
		if contextError := ctx.Err(); contextError != nil {
			return nil, nil, nil, contextError
		}
		// Even this arm leaves through the seam: a refusal with a registered code
		// added here later must not be able to fall out as a bare sentinel.
		return nil, nil, nil, productionKnowledgeReadRefusal(err)
	}
	return view, scope.chapters, scope.Close, nil
}

// BeginKnowledgeSearch serves kb:search: the FTS projection of that same store,
// built by the same recipe the sibling search binder uses
// (production_knowledge_search_linux.go:220-257), so the two cannot rank or
// degrade differently for the same query.
func (k *productionKnowledgeReads) BeginKnowledgeSearch(ctx context.Context, caller transport.Caller,
	expected any) (*knowledgequery.SearchCapture, func(), error) {
	scope, err := k.open(ctx, caller, expected)
	if err != nil {
		if errors.Is(err, errProductionKnowledgeLegacyBlocked) {
			// The one envelope the frozen binder cannot reproduce: see deviation 1.
			return nil, nil, productionKnowledgeReadRefusal(err)
		}
		if errors.Is(err, errProductionKnowledgeStorageUnsafe) ||
			errors.Is(err, errProductionKnowledgeIndexInvalid) {
			// A store with no canonical chunk table: the reference answers [] here
			// (vector-store.ts:1419, :1512-1520), and kb:search-with-scope does the
			// same through production_knowledge_search_linux.go:227-234, :243-257.
			return productionKnowledgeReadEmptyCapture()
		}
		return nil, nil, productionKnowledgeReadRefusal(err)
	}
	capture, err := knowledgequery.NewSearchCapture(ctx, productionKnowledgeSnapshotOf(scope.index),
		scope.storage.storage, productionKnowledgeCopyBudget())
	if err != nil {
		scope.Close()
		if contextError := ctx.Err(); contextError != nil {
			return nil, nil, contextError
		}
		return productionKnowledgeReadEmptyCapture()
	}
	return capture, scope.Close, nil
}

// productionKnowledgeReadRefusal moves a refusal onto the one seam the frozen
// binders actually map. transport.knowledgeReadError (internal/transport/
// knowledge.go:147-168) keeps an *security.HTTPError with its own status and code
// and replaces anything else with KNOWLEDGE_READ_FAILED, so a bare sentinel such
// as errProductionKnowledgeLegacyBlocked - whose string is not in that function's
// finite allowlist - would reach the page as an unmapped 500 instead of the
// registered migration code. Every View-shaped read therefore refuses through
// this function; kb:list-documents has no exemption, and neither have the other
// three. Only the sentinels with a code the pages really render are translated:
// LEGACY_VECTOR_MIGRATION_BLOCKED is registered in src/i18n/app-errors.ts:11,
// while KNOWLEDGE_BASE_INDEX_INVALID and KNOWLEDGE_BASE_STORAGE_UNSAFE are not -
// so a store that exists but cannot be decoded keeps the transport's generic
// KNOWLEDGE_READ_FAILED. Both would land on the same fallback text, and this way
// the refusal does not invent a second vocabulary for the same sentence.
func productionKnowledgeReadRefusal(err error) error {
	if errors.Is(err, errProductionKnowledgeLegacyBlocked) {
		return security.NewHTTPError(http.StatusConflict, productionKnowledgeCodeLegacyBlocked)
	}
	return err
}

// productionKnowledgeReadEmptyCapture is the empty FTS projection: no chunks, no
// stale copies and neither optional column, so SearchFTS ranks nothing and
// answers [] for every admissible query (knowledgequery/search.go:92-112). It is
// the same value a capture over an empty snapshot would hold, and it needs no
// storage root, which is what lets a store this lane cannot open answer the
// reference's own empty search instead of an error no kb:search caller expects.
func productionKnowledgeReadEmptyCapture() (*knowledgequery.SearchCapture, func(), error) {
	return &knowledgequery.SearchCapture{}, func() {}, nil
}

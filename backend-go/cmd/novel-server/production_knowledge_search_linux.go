//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's RETRIEVAL slice of the knowledge-base family
// (web/operations.json:244-262). With production_knowledge_import_linux.go (two
// served) and production_knowledge_documents_linux.go (five served, three refused)
// it accounts for every name in the family:
//
//	kb:search-writing-context     BOUND    arity 3 [query, topK|undefined, projectPath]
//	kb:search-with-scope          BOUND    arity 5 [query, fromChapter, toChapter, topK|undefined, projectPath]
//	kb:get-vector-rebuild-status  BOUND    arity 1 [projectPath]
//	kb:backfill-vectors           BOUND    arity 1 [projectPath]
//
// # The reference
//
//	electron/controllers/kb-controller.ts:391-409                 search-writing-context
//	electron/controllers/kb-controller.ts:411-422                 search-with-scope
//	electron/controllers/kb-controller.ts:487-514                 get-vector-rebuild-status
//	electron/controllers/kb-controller.ts:516-521                 backfill-vectors
//	electron/controllers/kb-controller.ts:213-228                 the kb: envelope every handler runs in
//	electron/knowledge-base.ts:67-77                              knowledgeFailureDetails
//	src/shared/ipc-channels.ts:1222-1257                          the wire shapes
//	electron/knowledge-base.ts:848-862                            searchKnowledgeFTS
//	electron/knowledge-base.ts:143-160                            the degrade when no embedding is available
//	electron/knowledge-base.ts:240-272                            getKnowledgeStats
//	electron/knowledge-base.ts:734-742                            getVectorlessCount
//	electron/vector-store.ts:1394-1540                            searchWithScope / searchWithScopeInternal
//	electron/vector-store.ts:1481-1500                            the canonical-table FTS filter, score and order
//	src/services/workflows/commands/generate-draft.command.ts:680 writing-context caller
//	src/services/workflows/commands/import-novel.command.ts:348   writing-context caller
//
// # Retrieval here is FTS over the canonical copy, which is the reference's own mode
//
// Every read below is internal/knowledgequery, the byte-exact port of the
// reference's fallback: the same term extraction (letters, numbers and hyphen;
// terms of two or more code points; case-insensitive dedupe; at most eight), the
// same SQL-LIKE-on-original-case match followed by the lowercased relevance pass,
// the same 0.5 score (vector-store.ts:1497), the same (fileName,text) dedupe and
// the same stable relevance order. `fts-only` is the branch the reference takes
// whenever it has no query vector: searchKnowledgeFTS is that call outright
// (knowledge-base.ts:848-862), and searchKnowledge falls into it when the
// embedding request fails (knowledge-base.ts:143-160). This lane is that branch
// permanently, because its store carries `registry: null` and no space table
// (production_knowledge_import_linux.go:78-102) and its bridge has no vector query
// operation at all (internal/knowledgequery/vector_plan.go:19-27). So the answer
// is the reference's FTS answer, not an approximation of it.
//
// kb:search-writing-context excludes the 'reference' corpus kind exactly as the
// controller does (kb-controller.ts:391-409), which is what keeps an imported
// reference novel out of a draft's writing context; kb:search-with-scope passes
// the chapter range instead and excludes nothing.
//
// # Three deviations this file has to state
//
//  1. topK is `topK ?? 5`, but the reader bounds it: SearchFTS admits 0..100
//     (internal/knowledgequery/search.go:92-97). A larger topK answers 400 rather
//     than being silently truncated.
//  2. An inverted or negative chapter range (from > to, from < 0) is refused with
//     400 by the same reader. The reference would have built a Lance filter that
//     matches nothing and answered []. Refusing is the only faithful option open
//     to a handler whose reader will not accept the range, and answering [] would
//     claim an empty result for a query nobody ran.
//  3. A store the reader cannot decode answers [] rather than an error, because
//     that is verbatim what searchWithScopeInternal does with every exception: it
//     answers [] when the store has no canonical table (:1419) and [] again from
//     both of its catch blocks (:1512-1514, :1518-1520). The one refusal the
//     reference does register - the legacy vector store - is still answered as a
//     business failure (ensureMigration, knowledge-base.ts:60-65).
//
// # kb:get-vector-rebuild-status reports this store, and refuses nothing
//
// The reference's status read has two halves: with no usable embedding
// configuration it answers all-zero counts without touching the store
// (kb-controller.ts:492-500), and otherwise it reads the canonical stats and the
// vectorless count. This lane is permanently in the first half - it binds no
// embedding dispatch anywhere (production_draft_context_linux.go:508-514 says so
// for the one other place that would want one), so `embeddingConfigured` is false
// and `canRebuild` is false: nothing here could rebuild a generation, and
// offering the button would produce a failure on the next click. What this file
// does NOT copy is the zeros: the reference zeroes the counts only because it
// never looked, while this lane can look, and reporting 0 chunks for a store it
// just wrote would deny the author's own documents. The counts are therefore read
// from the served reader - totalChunks is View.Stats' total, vectorlessCount is
// View.VectorlessCount's count, which is every chunk because no active space
// exists - and activeVectorDimension is the registry's own dimension, which is 0.
// That is the truthful "no vector index exists here, every document is FTS-only"
// answer the browser needs to stop offering a rebuild.
//
// # kb:backfill-vectors is served as an honest no-op
//
// The reference's enabled path (kb.backfillVectors, knowledge-base.ts:768-832)
// generates embeddings and rebuilds an embedding generation - a capability this
// lane does not have: there is no embedding-space writer, no registry generation
// and no vector query executor (vector_plan.go:19-27), and the store this lane
// writes carries `registry: null` with FTS as its only search index
// (production_knowledge_import_linux.go:78-102). So there are no vectors to
// backfill, and the truthful answer is the reference's own "up-to-date" branch:
// `{ success: true, processed: 0, failed: 0 }` (knowledge-base.ts:788-790). That
// is a statement of what this lane did - zero vectors backfilled, zero rows
// written - never a claim that vectors exist. The renderer reads `processed` as
// advisory (use-knowledge-base.ts:96-99): a zero count shows "check complete, no
// rebuild needed", so the honest answer cannot stall it. The handler lives in
// production_kb_backfill_linux.go.
type productionKnowledgeSearches struct {
	imports *productionKnowledgeImports
}

// newProductionKnowledgeSearches is the constructor the binder block wires.
func newProductionKnowledgeSearches(durable *productionDurableOpen) *productionKnowledgeSearches {
	return &productionKnowledgeSearches{imports: newProductionKnowledgeImports(durable)}
}

// newProductionKnowledgeSearchesFrom shares one source gate with an existing
// import bridge, so a search cannot read a store while that bridge is rewriting it
// through a second mutex of its own.
func newProductionKnowledgeSearchesFrom(imports *productionKnowledgeImports) *productionKnowledgeSearches {
	return &productionKnowledgeSearches{imports: imports}
}

// Bind installs exactly the four retrieval names. A nil bridge installs nothing,
// so they keep failing closed at GO_OPERATION_NOT_MIGRATED.
func (k *productionKnowledgeSearches) Bind(server *transport.Server) error {
	if k == nil || k.imports == nil || k.imports.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	bound := []struct {
		name  string
		arity int
		call  func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)
	}{
		{productionKnowledgeSearchWritingContextName, 3, k.searchWritingContext},
		{productionKnowledgeSearchWithScopeName, 5, k.searchWithScope},
		{productionKnowledgeVectorStatusName, 1, k.vectorRebuildStatus},
		{productionKnowledgeBackfillVectorsName, 1, k.backfillVectors},
	}
	for _, entry := range bound {
		if err := server.Bind(entry.name, k.imports.operation(entry.arity, entry.call)); err != nil {
			return err
		}
	}
	return nil
}

const (
	productionKnowledgeSearchWritingContextName = "kb:search-writing-context"
	productionKnowledgeSearchWithScopeName      = "kb:search-with-scope"
	productionKnowledgeVectorStatusName         = "kb:get-vector-rebuild-status"
	productionKnowledgeBackfillVectorsName      = "kb:backfill-vectors"

	// productionKnowledgeSearchDefaultTopK is `topK ?? 5` (kb-controller.ts:407).
	productionKnowledgeSearchDefaultTopK = 5
	// productionKnowledgeSearchMaxTopK is SearchFTS' own bound
	// (internal/knowledgequery/search.go:92-97); a larger request is refused rather
	// than truncated.
	productionKnowledgeSearchMaxTopK = 100
	// productionKnowledgeReferenceCorpusKind is the corpus kind
	// kb:search-writing-context excludes (KnowledgeCorpusKind, vector-store.ts:36).
	productionKnowledgeReferenceCorpusKind = "reference"
)

// searchWritingContext serves `kb:search-writing-context` - the name a draft
// generation and the novel-import workflow both call.
func (k *productionKnowledgeSearches) searchWritingContext(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	query, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	topK, ok := productionKnowledgeSearchTopK(args[1])
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[2], contextValue)
	if err != nil {
		return nil, err
	}
	return k.search(ctx, projectPath, query, topK, nil, []string{productionKnowledgeReferenceCorpusKind})
}

// searchWithScope serves `kb:search-with-scope`: the same retrieval restricted to
// a chapter range, with no corpus exclusion.
func (k *productionKnowledgeSearches) searchWithScope(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	query, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	scope, ok := productionKnowledgeSearchScope(args[1], args[2])
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	topK, ok := productionKnowledgeSearchTopK(args[3])
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[4], contextValue)
	if err != nil {
		return nil, err
	}
	return k.search(ctx, projectPath, query, topK, scope, nil)
}

// search is the one implementation both names use, so the two can never disagree
// about exclusions, ordering or degradation.
func (k *productionKnowledgeSearches) search(ctx context.Context, projectPath, query string, topK int,
	scope *knowledgequery.ChapterScope, excludedCorpusKinds []string) (any, error) {
	release, err := k.imports.enter(projectPath)
	if err != nil {
		return nil, err
	}
	defer release()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		// No storage directory means no canonical chunk table: the reference
		// answers an empty search for a store it cannot open (:1419) and for one
		// whose read throws (:1512-1520), and an empty array is the wire value the
		// renderer iterates.
		return []knowledgequery.SearchResult{}, nil
	}
	defer func() { _ = storage.Close() }()
	if err := storage.refuseLegacyVectors(); err != nil {
		// ensureMigration throws for every knowledge read (knowledge-base.ts:60-65,
		// :140) and the controller answers with the one registered code
		// (knowledge-base.ts:67-77; every kb: handler runs inside the
		// kb-controller.ts:213-228 envelope, which passes that result through).
		return productionKnowledgeImportFailure(err), nil
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return []knowledgequery.SearchResult{}, nil
	}
	capture, err := knowledgequery.NewSearchCapture(ctx, productionKnowledgeSnapshotOf(index), storage.storage,
		productionKnowledgeCopyBudget())
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return []knowledgequery.SearchResult{}, nil
	}
	results, err := capture.SearchFTS(ctx, query, topK, scope, excludedCorpusKinds)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		if errors.Is(err, knowledgequery.ErrInput) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		return []knowledgequery.SearchResult{}, nil
	}
	return results, nil
}

// vectorRebuildStatus serves `kb:get-vector-rebuild-status`. See the header for
// what is copied from the reference and what is deliberately not.
func (k *productionKnowledgeSearches) vectorRebuildStatus(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	projectPath, err := productionKnowledgeRequestPath(args[0], contextValue)
	if err != nil {
		return nil, err
	}
	answer := map[string]any{
		"embeddingConfigured":   false,
		"canRebuild":            false,
		"totalChunks":           int64(0),
		"vectorlessCount":       int64(0),
		"activeVectorDimension": int64(0),
	}
	release, err := k.imports.enter(projectPath)
	if err != nil {
		return nil, err
	}
	defer release()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		return answer, nil
	}
	defer func() { _ = storage.Close() }()
	if err := storage.refuseLegacyVectors(); err != nil {
		// A store this lane must not touch reports what a store it never looked at
		// would: the reference's own not-configured branch.
		return answer, nil
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return answer, nil
	}
	view, err := knowledgequery.New(ctx, productionKnowledgeSnapshotOf(index), storage.storage,
		productionKnowledgeCopyBudget())
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return answer, nil
	}
	stats, err := view.Stats(ctx)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return answer, nil
	}
	counts, err := view.VectorlessCount(ctx)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return answer, nil
	}
	if total, ok := productionKnowledgeSafeIntegerOf(stats["totalChunks"]); ok {
		answer["totalChunks"] = total
	}
	if dimension, ok := productionKnowledgeSafeIntegerOf(stats["vectorDimension"]); ok {
		answer["activeVectorDimension"] = dimension
	}
	if vectorless, ok := productionKnowledgeSafeIntegerOf(counts["count"]); ok {
		answer["vectorlessCount"] = vectorless
	}
	return answer, nil
}

// productionKnowledgeSearchTopK is `topK ?? 5` plus the bounded reader's own
// range: an absent argument takes the reference's default, and anything that is
// not an integer inside 0..100 is refused instead of silently truncated.
func productionKnowledgeSearchTopK(value any) (int, bool) {
	if value == nil {
		return productionKnowledgeSearchDefaultTopK, true
	}
	topK, ok := productionKnowledgeSafeIntegerOf(value)
	if !ok || topK < 0 || topK > productionKnowledgeSearchMaxTopK {
		return 0, false
	}
	return int(topK), true
}

// productionKnowledgeSearchScope is the chapter range kb:search-with-scope adds.
// The reader refuses a negative start or an inverted range, so those requests are
// answered 400 rather than with a filter that matches nothing.
func productionKnowledgeSearchScope(from, to any) (*knowledgequery.ChapterScope, bool) {
	fromChapter, ok := productionKnowledgeSafeIntegerOf(from)
	if !ok {
		return nil, false
	}
	toChapter, ok := productionKnowledgeSafeIntegerOf(to)
	if !ok {
		return nil, false
	}
	return &knowledgequery.ChapterScope{From: fromChapter, To: toChapter}, true
}

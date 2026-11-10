//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/embedding"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the two knowledge-base import names the author's browser
// workflow needs:
//
//	kb:import-text            BOUND here (arity 3: text, fileName, projectPath)
//	kb:import-finalized-text  BOUND here (arity 4: text, fileName, source, projectPath)
//
// Both are listed with scope `project` at web/operations.json:246-247, inside
// the kb family block at :244-262, and both are blockers: 终稿 (finalize) calls
// kb:import-finalized-text, or kb:import-text when the frozen source carries no
// finalized receipt, and throws when the answer is not a success
// (src/services/workflows/commands/finalize-chapter.command.ts:130-135
// `requireIpcSuccess(result, '导入知识库')`).
//
// # The reference
//
//	electron/controllers/kb-controller.ts:307-312
//	  operations.handle('kb:import-text', async (_event, text, fileName, expectedProjectPath) => {
//	    const projectPath = requireProjectPath(expectedProjectPath) ...
//	    return knowledgeBaseLoader.run((kb) => kb.importText(text, fileName, projectPath, protocol, model)) })
//	electron/controllers/kb-controller.ts:315-321
//	  operations.handle('kb:import-finalized-text', async (_event, text, fileName, source, expectedProjectPath) => {
//	    ... kb.importFinalizedText(text, fileName, source, projectPath, ...) })
//	src/shared/ipc-channels.ts:1206-1212  (args, and the added `idempotent?`)
//	src/shared/finalized-continuity.ts:12-16  (FinalizedSourceIdentity)
//	electron/knowledge-base.ts:418-433  importText -> importTextInternal(..., options)
//	electron/knowledge-base.ts:273-335  importTextInternal
//	electron/knowledge-base.ts:339-416  importFinalizedText
//	electron/knowledge-base.ts:60-74    ensureMigration, knowledgeFailureDetails
//	electron/knowledge-base.ts:262-293  parseChapterMetaFromFileName, chapterMeta, chunking
//	electron/knowledge-base.ts:372-375  the deterministic finalized document id
//	electron/embedding.ts:348-416       chunkText(text, maxChars, overlap)
//	src/shared/embedding-options.ts:16-20  DEFAULT_EMBEDDING_OPTIONS (500 / 50)
//	electron/vector-store.ts:1080-1180  addChunks (chunk rows, document row, prune)
//	electron/vector-store.ts:1011-1049  pruneSupersededDocuments
//	electron/vector-store.ts:1226-1264  replaceDocumentChunks (stable identity)
//	electron/vector-store.ts:130-174    KNOWLEDGE_COPY_MARKER, read/writeKnowledgeCopy
//	electron/vector-store.ts:1787-1789  hashCanonicalChunkSet
//	electron/vector-store.ts:1792-1797  getDocumentIntegrity
//	electron/repositories/finalization-repository.ts:386-395  isAuthoritative
//	electron/services/portable-runtime-freeze.ts:112-129      readPortableRuntimeFreeze
//	electron/services/knowledge-source-gate.ts                withKnowledgeSourceGate
//
// # The one structural decision: there is no Lance table to write
//
// The reference stores documents and chunks in LanceDB. This lane cannot:
// internal/projectaccess/native_knowledge.go is behind `lance_native && cgo`, and
// no build script sets that tag, so the shipped binary has no Lance reader and no
// Lance writer; the module also has no Go Arrow/IPC writer (go.mod carries only
// mattn/go-sqlite3 and golang.org/x/text). Writing a second, private Lance format
// here would produce a store no served read could ever open, and the point of
// this batch is that what the import writes is what kb:search will find.
//
// So the import writes the store this lane's own reads already decode: the
// canonical snapshot knowledgedomain.Snapshot - tables `documents` and `chunks`,
// every row carrying every schema key - kept at
// {projectPath}/.ai-novel/knowledge-snapshot.json. `registry` is stored as JSON
// null, which internal/knowledgedomain reads as "no registry asset"
// (model.go:26-28) and internal/knowledgequery reads as zero spaces, so every
// chunk in the store stays vectorless. That is the reference's FTS-only shape,
// which it reaches by asking for it explicitly (kb:import-planning-text passes
// `{ mode: 'fts-only' }` at kb-controller.ts:323-335) or by degrading when no
// embedding call can run (knowledge-base.ts:296-309), and it is why this import
// must succeed with no embedding provider configured: it never calls one.
// kb:search, kb:list-documents and kb:stats decode the same file through
// knowledgequery.New / NewSearchCapture, i.e. through the same Validate this
// file runs before it writes, so the write lands where those reads look once
// this lane binds the kb read names - which this batch does not do.
//
// The reference-format knowledge copy is still written, compatibly, at
// .ai-novel/knowledge-copies/<sha256hex(docId)>.json (vector-store.ts:139-170),
// and it is the copy internal/knowledgecopy validates and captures; the
// finalization link is still a real UPDATE of finalization_outbox through
// internal/store.FinalizationOutbox with the same owner proof the served
// db:finalization-link-knowledge-document uses.
//
// Two consequences are deliberate refusals, never silent repairs:
//
//	* A project that carries an unmigrated legacy vector file refuses the import
//	  with LEGACY_VECTOR_MIGRATION_BLOCKED (knowledge-base.ts:60-65). This lane
//	  owns no legacy migration, and writing an index beside vectors.json would
//	  strand the author's old data half-migrated.
//	* A store holding any other table refuses. knowledgedomain admits only
//	  documents, chunks and *registered* embedding spaces, so an unregistered
//	  space table would have to be answered with an invented registry
//	  generation. This lane never writes one, so the refusal costs nothing and
//	  prevents a divergent registry.
//
// # What is reproduced exactly, and what is not
//
//	chunking         embedding.ChunkText, the existing byte-exact port of
//	                 chunkText (JS UTF-16 code units, 500/50)
//	new document id  randomUUID() per call (knowledge-base.ts:287), a v4 UUID
//	pruning          a second kb:import-text of the same fileName leaves ONE
//	                 document, with the NEW id: addChunks prunes every other
//	                 document with the same (fileName, corpusKind) and its copy
//	                 (vector-store.ts:1011-1049), and the reference really does
//	                 mint a new id per call. Reproduced, not "improved".
//	finalized id     `finalization-` + sha256hex(JSON.stringify([lane,
//	                 finalizationId, draftId, chapterNumber, contentHash]))
//	idempotency      a complete canonical import answers idempotent:true and only
//	                 re-links; damaged evidence answers
//	                 FINALIZATION_KNOWLEDGE_REPAIR_REQUIRED and writes nothing
//	receipt          { success, docId, chunkCount } and { ...that, idempotent } -
//	                 the browser dereferences docId, so it is always present on
//	                 success
//	integrity        chunkCount + sha256(JSON.stringify(chunks)) + the copy's
//	                 content and indexedHash
//
// Not reproduced, because this lane has no counterpart: the embedding call and
// its space/vector bookkeeping, the reference's Lance migrations, and its
// message-text failures. Every failure here is a symbolic code - never driver
// prose, a path or a document id.

const (
	productionKnowledgeImportTextName      = "kb:import-text"
	productionKnowledgeImportFinalizedName = "kb:import-finalized-text"

	// productionKnowledgeIndexName is the canonical snapshot this lane's
	// knowledge reads decode. It sits in the project's storage directory beside
	// project.db and the reference's other knowledge assets
	// (internal/projectfs/knowledge.go:80-83 names lancedb/, embedding-spaces.json,
	// knowledge-copies/ and vectors.json.migrated).
	productionKnowledgeIndexName = "knowledge-snapshot.json"
	// productionKnowledgeCopyDirectory and productionKnowledgeCopyMarker are
	// vector-store.ts:130-174: copies live in knowledge-copies/, and a document
	// row is managed exactly when its filePath starts with the marker.
	productionKnowledgeCopyDirectory = "knowledge-copies"
	productionKnowledgeCopyMarker    = "knowledge-copy:"
	productionKnowledgeCopySuffix    = ".json"
	// productionKnowledgeCorpusKind is the only corpus kind these two channels
	// write (knowledge-base.ts:331, :399).
	productionKnowledgeCorpusKind = "project-knowledge"
	// productionKnowledgeFinalizationPrefix and productionKnowledgeIdentityLane
	// are the deterministic identity of knowledge-base.ts:372-375.
	productionKnowledgeFinalizationPrefix = "finalization-"
	productionKnowledgeIdentityLane       = "finalization-knowledge:v1"
	// productionKnowledgeChunkSize and productionKnowledgeChunkOverlap are
	// DEFAULT_EMBEDDING_OPTIONS (src/shared/embedding-options.ts:16-20). The
	// reference reads them from the configured model; this lane has no model, so
	// the defaults are the only values it may claim.
	productionKnowledgeChunkSize    = 500
	productionKnowledgeChunkOverlap = 50
	// productionKnowledgeTableDocuments / Chunks are the two table names
	// internal/knowledgedomain admits for a canonical base snapshot.
	productionKnowledgeTableDocuments = "documents"
	productionKnowledgeTableChunks    = "chunks"
	// productionKnowledgeFinalizationTable is the portable-freeze table this
	// mutation is gated on (knowledge-base.ts:365).
	productionKnowledgeFinalizationTable = "finalization_outbox"
	// productionKnowledgeLegacyVectorName and its journal are the two files
	// ensureMigration refuses to work beside (knowledge-base.ts:60-65).
	productionKnowledgeLegacyVectorName    = "vectors.json"
	productionKnowledgeLegacyVectorJournal = "vectors.json.migration-journal.json"
	// productionKnowledgeIndexMaxBytes bounds the snapshot read. The domain
	// validator's aggregate text budget is the same 32 MiB
	// (internal/knowledgedomain/model.go:48-53), so a larger file could never
	// validate anyway.
	productionKnowledgeIndexMaxBytes = int64(knowledgedomain.HardMaxTextBytes)
	// productionKnowledgeTextMaxBytes is this lane's per-import text bound. The
	// reference has none, but chunkText caps its output at 32 MiB and
	// internal/knowledgecopy charges copy content against a 64 MiB budget, so
	// without a bound one import could make every later capture refuse.
	productionKnowledgeTextMaxBytes = 8 << 20
	// productionKnowledgeRowLimit mirrors knowledgedomain.HardMaxRows: the
	// snapshot holds every chunk of every document in one table.
	productionKnowledgeRowLimit = knowledgedomain.HardMaxRows - 1
	// productionKnowledgeSafeInteger is Number.MAX_SAFE_INTEGER.
	productionKnowledgeSafeInteger = int64(1)<<53 - 1

	productionKnowledgeCodeTextEmpty      = "KNOWLEDGE_BASE_TEXT_EMPTY"
	productionKnowledgeCodeTextTooLarge   = "KNOWLEDGE_BASE_TEXT_TOO_LARGE"
	productionKnowledgeCodeChunksInvalid  = "KNOWLEDGE_BASE_CHUNKS_INVALID"
	productionKnowledgeCodeIndexInvalid   = "KNOWLEDGE_BASE_INDEX_INVALID"
	productionKnowledgeCodeStorageUnsafe  = "KNOWLEDGE_BASE_STORAGE_UNSAFE"
	productionKnowledgeCodeFailed         = "KNOWLEDGE_BASE_IMPORT_FAILED"
	productionKnowledgeCodeLegacyBlocked  = "LEGACY_VECTOR_MIGRATION_BLOCKED"
	productionKnowledgeCodeSourceChanged  = "FINALIZATION_KNOWLEDGE_SOURCE_CHANGED"
	productionKnowledgeCodeRepairRequired = "FINALIZATION_KNOWLEDGE_REPAIR_REQUIRED"
	productionKnowledgeCodeLinkFailed     = "FINALIZATION_KNOWLEDGE_LINK_FAILED"
	productionKnowledgeCodeFrozen         = "PORTABLE_RUNTIME_FROZEN"
	productionKnowledgeCodeFreezeInvalid  = "PORTABLE_RUNTIME_FREEZE_INVALID"
)

// The refusals. Each is a symbolic value; productionKnowledgeFailureCode is the
// only place that turns any of them into a wire code.
var (
	errProductionKnowledgeTextEmpty      = errors.New(productionKnowledgeCodeTextEmpty)
	errProductionKnowledgeTextTooLarge   = errors.New(productionKnowledgeCodeTextTooLarge)
	errProductionKnowledgeChunksInvalid  = errors.New(productionKnowledgeCodeChunksInvalid)
	errProductionKnowledgeIndexInvalid   = errors.New(productionKnowledgeCodeIndexInvalid)
	errProductionKnowledgeStorageUnsafe  = errors.New(productionKnowledgeCodeStorageUnsafe)
	errProductionKnowledgeLegacyBlocked  = errors.New(productionKnowledgeCodeLegacyBlocked)
	errProductionKnowledgeSourceChanged  = errors.New(productionKnowledgeCodeSourceChanged)
	errProductionKnowledgeRepairRequired = errors.New(productionKnowledgeCodeRepairRequired)
	errProductionKnowledgeLinkFailed     = errors.New(productionKnowledgeCodeLinkFailed)
	errProductionKnowledgeFrozen         = errors.New(productionKnowledgeCodeFrozen)
)

// productionKnowledgeFileNamePattern is parseChapterMetaFromFileName
// (knowledge-base.ts:262-271) with the ECMAScript `\s` set spelled out, because
// Go's `\s` is ASCII space and the reference's is not. RE2 has no backslash-u
// escape, so the set uses \x{...}. The non-greedy (.+?) means what it means in
// JS: the shortest title that still lets `\s+(正文|要点|蓝图)\.md` end the string.
var productionKnowledgeFileNamePattern = regexp.MustCompile(
	`^第([0-9]+)章[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+` +
		`(.+?)[\t\n\v\f\r \x{00a0}\x{1680}\x{2000}-\x{200a}\x{2028}\x{2029}\x{202f}\x{205f}\x{3000}\x{feff}]+` +
		`(?:正文|要点|蓝图)\.md$`)

// productionKnowledgeImports serves the two names over the durable lane's
// admitted project. It holds no path and no database: both come from the frozen
// three-key context of each request.
type productionKnowledgeImports struct {
	durable *productionDurableOpen

	// model is the embedding seam the vector writer resolves through. A nil
	// model means this bridge has no embedding configuration: import and search
	// then behave exactly as the FTS-only lane always did, and no vector file is
	// ever created.
	model productionKnowledgeModel

	// mu guards gates, the analogue of withKnowledgeSourceGate
	// (electron/services/knowledge-source-gate.ts): the knowledge files are
	// written by this process only, so one mutex per project path is what keeps
	// two imports of one project from interleaving a read-modify-write of the
	// index. Cross-process exclusion is the durable writer lease the borrow
	// proves.
	mu    sync.Mutex
	gates map[string]*sync.Mutex
}

// newProductionKnowledgeImports is the constructor the binder block wires.
func newProductionKnowledgeImports(durable *productionDurableOpen) *productionKnowledgeImports {
	return &productionKnowledgeImports{durable: durable, gates: make(map[string]*sync.Mutex)}
}

// Bind installs exactly the two import names. A nil bridge installs nothing, so
// both keep failing closed at GO_OPERATION_NOT_MIGRATED instead of being
// published as handlers that can only refuse.
func (k *productionKnowledgeImports) Bind(server *transport.Server) error {
	if k == nil || k.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	// The reference registers 3 and 4 arguments (src/shared/ipc-channels.ts:
	// 1206-1212); the frozen context follows them.
	if err := server.Bind(productionKnowledgeImportTextName, k.operation(3, k.importText)); err != nil {
		return err
	}
	return server.Bind(productionKnowledgeImportFinalizedName, k.operation(4, k.importFinalized))
}

// operation is the adapter both names share: the arity check, the frozen
// three-key context, the borrow, and the single failure mapping. Nothing in it
// can take a project path, an epoch or a database from the request.
func (k *productionKnowledgeImports) operation(arity int,
	call func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if ctx == nil || len(args) != arity+1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		contextValue, err := projectsession.ParseContext(args[arity])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		db, release, err := k.durable.borrowAuthorDatabase(ctx, caller, args[arity])
		if err != nil {
			// Already symbolic: a lease, ownership or closed-lane refusal.
			return nil, err
		}
		if db == nil || release == nil {
			// The bridge never returns a half-open borrow; if it ever does, the
			// lane fails closed rather than writing a database nobody admitted.
			return nil, security.NewHTTPError(http.StatusServiceUnavailable, "GO_OPERATION_UNAVAILABLE")
		}
		defer release()
		return call(ctx, caller, db, contextValue, args[:arity])
	}
}

// importText serves `kb:import-text`. The scope is the lease's own project path
// and the body may only agree with it: requireProjectPath (kb-controller.ts:308)
// is the reference's agreement check, and the lane spells a disagreement
// PROJECT_CONTEXT_CHANGED (production_chapter_deletion_linux.go:58).
func (k *productionKnowledgeImports) importText(ctx context.Context, caller transport.Caller, db *sql.DB,
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
	request := productionKnowledgeRequest{Text: text, FileName: fileName}
	answer, err := k.write(ctx, projectPath, request)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	return answer, nil
}

// importFinalized serves `kb:import-finalized-text`. The body's `source` is a
// FinalizedSourceIdentity (src/shared/finalized-continuity.ts:12-16); it is
// never authority, only the identity the receipt must match.
func (k *productionKnowledgeImports) importFinalized(ctx context.Context, caller transport.Caller, db *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	text, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	fileName, ok := args[1].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	source, ok := productionKnowledgeSourceOf(args[2])
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[3], contextValue)
	if err != nil {
		return nil, err
	}
	request := productionKnowledgeRequest{Text: text, FileName: fileName}
	answer, err := productionKnowledgeImportFinalized(ctx, k, db, contextValue, projectPath, request, source)
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, contextError
		}
		return productionKnowledgeImportFailure(err), nil
	}
	return answer, nil
}

// productionKnowledgeRequestPath resolves the body's expected project path
// against the lease's own. requireProjectPath (kb-controller.ts:308) refuses a
// request that names no project; assertRequiredExpectedProjectPath
// (knowledge-base.ts:349) refuses one that names a different project.
func productionKnowledgeRequestPath(raw any, contextValue projectsession.Context) (string, error) {
	projectPath, ok := raw.(string)
	if !ok || strings.TrimSpace(projectPath) == "" {
		return "", security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if contextValue.ProjectPath == "" || projectPath != contextValue.ProjectPath {
		return "", security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	return projectPath, nil
}

// productionKnowledgeRequest is one import, already separated from its wire.
// DocumentID and the chapter pair are the reference's `options` overrides
// (knowledge-base.ts:279, :399); empty/nil means "derive it the way
// importTextInternal does".
type productionKnowledgeRequest struct {
	Text          string
	FileName      string
	DocumentID    string
	ChapterNumber *int64
	ChapterTitle  *string
}

// productionKnowledgeReceipt is the success answer of both names. docId and
// chunkCount are always present on success because the browser dereferences
// them (finalize-chapter.command.ts:130-135); idempotent is added by
// kb:import-finalized-text only.
func productionKnowledgeReceipt(documentID string, chunkCount int64, idempotent *bool) map[string]any {
	answer := map[string]any{
		"success":    true,
		"docId":      documentID,
		"chunkCount": chunkCount,
	}
	if idempotent != nil {
		answer["idempotent"] = *idempotent
	}
	return answer
}

// productionKnowledgeImportFailure is the one and only mapping layer of this
// family. The reference answers `{ success: false, ...knowledgeFailureDetails }`
// for every business failure and never throws (knowledge-base.ts:329-331,
// :413-415), so this returns the wire body, not an error.
func productionKnowledgeImportFailure(err error) map[string]any {
	code := productionKnowledgeFailureCode(err)
	answer := map[string]any{"success": false, "error": code}
	if code == productionKnowledgeCodeLegacyBlocked {
		// knowledgeFailureDetails (:68-70) repeats the code in errorCode for the
		// registered legacy-migration refusal and for no other case.
		answer["errorCode"] = code
	}
	return answer
}

// productionKnowledgeFailureCode maps one error to one symbolic code. It is the
// only errors.Is fan-out in this file.
func productionKnowledgeFailureCode(err error) string {
	switch {
	case errors.Is(err, errProductionKnowledgeTextEmpty):
		return productionKnowledgeCodeTextEmpty
	case errors.Is(err, errProductionKnowledgeTextTooLarge), errors.Is(err, embedding.ErrChunkLimit):
		return productionKnowledgeCodeTextTooLarge
	case errors.Is(err, errProductionKnowledgeChunksInvalid),
		errors.Is(err, embedding.ErrChunkInput),
		errors.Is(err, embedding.ErrChunkUTF16Boundary):
		return productionKnowledgeCodeChunksInvalid
	case errors.Is(err, errProductionKnowledgeLegacyBlocked):
		return productionKnowledgeCodeLegacyBlocked
	case errors.Is(err, errProductionKnowledgeSourceChanged):
		return productionKnowledgeCodeSourceChanged
	case errors.Is(err, errProductionKnowledgeRepairRequired):
		return productionKnowledgeCodeRepairRequired
	case errors.Is(err, errProductionKnowledgeLinkFailed):
		return productionKnowledgeCodeLinkFailed
	case errors.Is(err, errProductionKnowledgeIndexInvalid),
		errors.Is(err, knowledgecopy.ErrBudget),
		errors.Is(err, knowledgecopy.ErrInvalid),
		errors.Is(err, knowledgecopy.ErrChanged):
		return productionKnowledgeCodeIndexInvalid
	case errors.Is(err, errProductionKnowledgeStorageUnsafe),
		errors.Is(err, knowledgecopy.ErrUnsafe):
		return productionKnowledgeCodeStorageUnsafe
	case errors.Is(err, errProductionKnowledgeFrozen):
		return productionKnowledgeCodeFrozen
	case errors.Is(err, productionChapterDeletionErrFreezeInvalid):
		return productionKnowledgeCodeFreezeInvalid
	}
	return productionKnowledgeCodeFailed
}

// productionKnowledgeSource is FinalizedSourceIdentity
// (src/shared/finalized-continuity.ts:12-16).
type productionKnowledgeSource struct {
	DraftID        int64
	FinalizationID string
	ChapterNumber  int64
	ContentHash    string
}

// productionKnowledgeSourceOf reads the identity out of the request body. It
// validates shapes only: authority is the database's, never the body's.
func productionKnowledgeSourceOf(value any) (productionKnowledgeSource, bool) {
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return productionKnowledgeSource{}, false
	}
	draft, draftOK := productionKnowledgeSafeIntegerOf(object["draftId"])
	finalization, finalizationOK := object["finalizationId"].(string)
	chapter, chapterOK := productionKnowledgeSafeIntegerOf(object["chapterNumber"])
	hash, hashOK := object["contentHash"].(string)
	if !draftOK || !finalizationOK || strings.TrimSpace(finalization) == "" || !chapterOK || !hashOK ||
		!productionKnowledgeHashValid(hash) {
		return productionKnowledgeSource{}, false
	}
	return productionKnowledgeSource{
		DraftID:        draft,
		FinalizationID: finalization,
		ChapterNumber:  chapter,
		ContentHash:    hash,
	}, true
}

// productionKnowledgeImportFinalized is the finalized arm, kept out of the binder
// so the two served names stay one failing line each. Its order is
// importFinalizedText's (:348-416): assert authority, take the source gate,
// assert authority again over the snapshot that will be imported, decide
// idempotency, import only when there is no evidence, re-verify from the file,
// and link.
func productionKnowledgeImportFinalized(ctx context.Context, imports *productionKnowledgeImports, db *sql.DB,
	contextValue projectsession.Context, projectPath string, request productionKnowledgeRequest,
	source productionKnowledgeSource) (any, error) {
	if contextValue.ProjectPath != projectPath {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	// :349-367 - the identity is frozen before the gate opens.
	frozen, err := productionKnowledgeFinalizedAuthority(ctx, db, contextValue, source, request.Text, request.FileName)
	if err != nil {
		return nil, err
	}
	chunks, err := productionKnowledgeChunksOf(ctx, frozen.Content)
	if err != nil {
		return nil, err
	}
	documentID, err := productionKnowledgeFinalizationDocumentID(source)
	if err != nil {
		return nil, err
	}
	// :378 - a second authority read inside the gate, so a source that changed
	// while the import waited is refused rather than imported stale.
	again, err := productionKnowledgeFinalizedAuthority(ctx, db, contextValue, source, request.Text, request.FileName)
	if err != nil {
		return nil, err
	}
	if again.Content != frozen.Content {
		return nil, errProductionKnowledgeSourceChanged
	}
	number := source.ChapterNumber
	title := again.ChapterTitle
	request.DocumentID = documentID
	request.ChapterNumber = &number
	request.ChapterTitle = &title
	answer, err := imports.finalize(ctx, db, contextValue, projectPath, request, frozen.Content, chunks, source)
	if err != nil {
		return nil, err
	}
	// :410 - linkKnowledgeDocument after the canonical evidence exists.
	if err := productionKnowledgeLink(ctx, db, contextValue, source, documentID); err != nil {
		return nil, err
	}
	return answer, nil
}

// productionKnowledgeFinalized is the frozen evidence one finalized import may
// use: the source identity, the snapshot's own content, and its chapter title.
type productionKnowledgeFinalized struct {
	Source       productionKnowledgeSource
	Content      string
	ChapterTitle string
}

// productionKnowledgeFinalizedAuthority is assertAuthority
// (knowledge-base.ts:351-367): every clause, in order, against the admitted
// database. It returns the snapshot the import must use, so a caller can never
// import text that merely looks equal.
func productionKnowledgeFinalizedAuthority(ctx context.Context, db *sql.DB, contextValue projectsession.Context,
	source productionKnowledgeSource, text, fileName string) (productionKnowledgeFinalized, error) {
	if db == nil || strings.TrimSpace(fileName) == "" || !utf8.ValidString(text) || !utf8.ValidString(fileName) {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	summary, err := store.NewSummary(db).ReadFinalizedSource(ctx, source.DraftID)
	if err != nil {
		return productionKnowledgeFinalized{}, err
	}
	if summary == nil || summary["status"] != "valid" {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	snapshot, ok := summary["snapshot"].(map[string]any)
	if !ok || snapshot == nil {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	content, ok := snapshot["content"].(string)
	if !ok || content != text {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	identity, ok := snapshot["source"].(map[string]any)
	if !ok || identity == nil {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	finalization, finalizationOK := identity["finalizationId"].(string)
	draft, draftOK := productionKnowledgeSafeIntegerOf(identity["draftId"])
	chapter, chapterOK := productionKnowledgeSafeIntegerOf(identity["chapterNumber"])
	hash, hashOK := identity["contentHash"].(string)
	if !finalizationOK || finalization != source.FinalizationID || !draftOK || draft != source.DraftID ||
		!chapterOK || chapter != source.ChapterNumber || !hashOK || hash != source.ContentHash {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	// copyHash(text) !== source.contentHash and
	// !FinalizationRepository.isAuthoritative(source.draftId) (:362).
	if productionKnowledgeHash(content) != source.ContentHash {
		return productionKnowledgeFinalized{}, errProductionKnowledgeSourceChanged
	}
	if err := productionKnowledgeAssertAuthoritative(ctx, db, source.DraftID); err != nil {
		return productionKnowledgeFinalized{}, err
	}
	// readPortableRuntimeFreeze(projectPath).assertMutable('finalization_outbox',
	// source.finalizationId) (:365).
	if err := productionKnowledgeAssertMutable(contextValue.ProjectPath, source.FinalizationID); err != nil {
		return productionKnowledgeFinalized{}, err
	}
	chapterTitle, _ := snapshot["chapterTitle"].(string)
	return productionKnowledgeFinalized{Source: source, Content: content, ChapterTitle: chapterTitle}, nil
}

// productionKnowledgeAssertAuthoritative is FinalizationRepository
// .isAuthoritative (finalization-repository.ts:386-395) over the borrowed
// handle: the draft must still be the chapter's authoritative finalized draft.
func productionKnowledgeAssertAuthoritative(ctx context.Context, db *sql.DB, draftID int64) error {
	var authoritative int64
	err := db.QueryRowContext(ctx, `SELECT id FROM drafts WHERE status='finalized'
		AND chapter_number=(SELECT chapter_number FROM drafts WHERE id=?)
		ORDER BY chapter_number ASC, version DESC, id DESC LIMIT 1`, draftID).Scan(&authoritative)
	if errors.Is(err, sql.ErrNoRows) {
		return errProductionKnowledgeSourceChanged
	}
	if err != nil {
		return err
	}
	if authoritative != draftID {
		return errProductionKnowledgeSourceChanged
	}
	return nil
}

// productionKnowledgeAssertMutable is the portable-runtime freeze gate of
// knowledge-base.ts:365, using the lane's existing reader
// (production_chapter_deletion_linux.go:638-666) so a freeze document is parsed
// by exactly one implementation.
func productionKnowledgeAssertMutable(projectPath, finalizationID string) error {
	if projectPath == "" || finalizationID == "" {
		return errProductionKnowledgeSourceChanged
	}
	project, err := os.OpenRoot(projectPath)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = project.Close() }()
	freeze, err := productionChapterDeletionReadFreeze(project)
	if err != nil {
		return err
	}
	if err := freeze.guard.AssertMutable(productionKnowledgeFinalizationTable, finalizationID); err != nil {
		return errProductionKnowledgeFrozen
	}
	return nil
}

// productionKnowledgeFinalizationDocumentID is the deterministic document
// identity of knowledge-base.ts:372-375:
//
//	identity = JSON.stringify(['finalization-knowledge:v1', finalizationId,
//	                            draftId, chapterNumber, contentHash])
//	docId    = `finalization-${sha256hex(identity)}`
//
// encoding/json emits exactly JSON.stringify's bytes for this array: no spaces,
// integers as integers, and both the id and the hash are hex, so no escaping can
// differ.
func productionKnowledgeFinalizationDocumentID(source productionKnowledgeSource) (string, error) {
	identity, err := json.Marshal([]any{
		productionKnowledgeIdentityLane,
		source.FinalizationID,
		source.DraftID,
		source.ChapterNumber,
		source.ContentHash,
	})
	if err != nil {
		return "", errProductionKnowledgeSourceChanged
	}
	digest := sha256.Sum256(identity)
	return productionKnowledgeFinalizationPrefix + hex.EncodeToString(digest[:]), nil
}

// productionKnowledgeLink is FinalizationRepository.linkKnowledgeDocument
// (:410) through the same owner-proofed repository the served
// db:finalization-link-knowledge-document uses. The proof carries the identity
// the request froze and the document this import wrote; the store re-reads the
// outbox row and CASes knowledge_document_id inside one IMMEDIATE transaction
// (internal/store/finalization_outbox.go:179-227), and durableFinalizationOwner
// refuses any other frozen identity.
func productionKnowledgeLink(ctx context.Context, db *sql.DB, contextValue projectsession.Context,
	source productionKnowledgeSource, documentID string) error {
	if db == nil {
		return errProductionKnowledgeSourceChanged
	}
	outbox := store.NewFinalizationOutbox(db, store.FinalizationScope{
		ProjectID: contextValue.ProjectID,
		Epoch:     contextValue.LeaseID,
	}, durableFinalizationOwner{expected: contextValue})
	proof := transport.FinalizationProof{
		ProjectID:           contextValue.ProjectID,
		LeaseID:             contextValue.LeaseID,
		ProjectPath:         contextValue.ProjectPath,
		Action:              "knowledge-link",
		DraftID:             source.DraftID,
		FinalizationID:      source.FinalizationID,
		ContentHash:         source.ContentHash,
		KnowledgeDocumentID: documentID,
	}
	if _, err := outbox.LinkKnowledgeDocumentWithOwnerProof(ctx, source.DraftID, documentID, proof); err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		// The reference lets the repository's message reach the renderer. This
		// lane never passes store prose, so the refusal is symbolic.
		return errProductionKnowledgeLinkFailed
	}
	return nil
}

// productionKnowledgeLinked is
// FinalizationRepository.getByDraftId(draftId)?.knowledgeDocumentId === docId
// (:400-402).
func productionKnowledgeLinked(ctx context.Context, db *sql.DB, contextValue projectsession.Context,
	source productionKnowledgeSource, documentID string) (bool, error) {
	if db == nil {
		return false, errProductionKnowledgeSourceChanged
	}
	outbox := store.NewFinalizationOutbox(db, store.FinalizationScope{
		ProjectID: contextValue.ProjectID,
		Epoch:     contextValue.LeaseID,
	}, durableFinalizationOwner{expected: contextValue})
	record, err := outbox.GetByDraftID(ctx, source.DraftID)
	if err != nil {
		return false, err
	}
	if record == nil {
		return false, nil
	}
	return record.KnowledgeDocumentID == documentID, nil
}

// productionKnowledgeChunksOf is the chunking half of importTextInternal
// (:290-291): chunkText over the exact text with DEFAULT_EMBEDDING_OPTIONS.
// embedding.ChunkText is the existing byte-exact port of electron/embedding.ts:
// 348-416, so the boundaries - overlap included - are the reference's.
func productionKnowledgeChunksOf(ctx context.Context, text string) ([]string, error) {
	if len(text) > productionKnowledgeTextMaxBytes {
		return nil, errProductionKnowledgeTextTooLarge
	}
	if !utf8.ValidString(text) {
		return nil, errProductionKnowledgeChunksInvalid
	}
	chunks, err := embedding.ChunkText(ctx, text, productionKnowledgeChunkSize, productionKnowledgeChunkOverlap)
	if err != nil {
		return nil, err
	}
	// chunkText returns [text.trim()] for text holding only whitespace, and
	// importTextInternal refuses exactly that with KNOWLEDGE_BASE_TEXT_EMPTY
	// (:282). Every other return is non-empty by construction.
	if len(chunks) == 0 || (len(chunks) == 1 && chunks[0] == "") {
		return nil, errProductionKnowledgeTextEmpty
	}
	if len(chunks) > productionKnowledgeRowLimit {
		return nil, errProductionKnowledgeChunksInvalid
	}
	return chunks, nil
}

// write is importTextInternal's commit half for `kb:import-text`
// (knowledge-base.ts:272-329) over this lane's store.
func (k *productionKnowledgeImports) write(ctx context.Context, projectPath string,
	request productionKnowledgeRequest) (map[string]any, error) {
	if !utf8.ValidString(request.FileName) {
		return nil, errProductionKnowledgeChunksInvalid
	}
	chunks, err := productionKnowledgeChunksOf(ctx, request.Text)
	if err != nil {
		return nil, err
	}
	documentID := request.DocumentID
	if documentID == "" {
		// :287 - a fresh randomUUID() per call, which is what makes a plain
		// re-import a prune-and-insert rather than an update.
		documentID, err = productionKnowledgeUUID()
		if err != nil {
			return nil, errProductionKnowledgeStorageUnsafe
		}
	}
	release, err := k.enter(projectPath)
	if err != nil {
		return nil, err
	}
	defer release()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = storage.Close() }()
	if err := storage.refuseLegacyVectors(); err != nil {
		return nil, err
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		return nil, err
	}
	document, rows, err := productionKnowledgeNewDocument(request, documentID, chunks, time.Now())
	if err != nil {
		return nil, err
	}
	if err := productionKnowledgeCommit(ctx, storage.storage, index, document, rows); err != nil {
		return nil, err
	}
	// The vectors are written after the canonical snapshot is committed and can
	// never fail this import (production_knowledge_vectors_linux.go).
	k.refreshVectorsBestEffort(ctx, storage)
	// The directories this write committed into must still be the ones the lane
	// opened (projectfs.checkIdentityLocked's rule around every write).
	if err := storage.verify(); err != nil {
		return nil, err
	}
	return productionKnowledgeReceipt(documentID, int64(len(chunks)), nil), nil
}

// finalize is importFinalizedText's decision table (knowledge-base.ts:379-411)
// over this lane's store: an idempotent receipt, a repair refusal, or one import
// followed by a re-read of the committed rows.
func (k *productionKnowledgeImports) finalize(ctx context.Context, db *sql.DB, contextValue projectsession.Context,
	projectPath string, request productionKnowledgeRequest, content string, chunks []string,
	source productionKnowledgeSource) (map[string]any, error) {
	release, err := k.enter(projectPath)
	if err != nil {
		return nil, err
	}
	defer release()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = storage.Close() }()
	if err := storage.refuseLegacyVectors(); err != nil {
		return nil, err
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		return nil, err
	}
	if document, found := index.find(request.DocumentID); found {
		// :379-391 - a document exists, so the answer is a receipt only when the
		// canonical rows and the copy are both complete.
		if err := k.complete(ctx, storage.storage, index, document, content, chunks); err != nil {
			return nil, err
		}
		k.refreshVectorsBestEffort(ctx, storage)
		idempotent := true
		return productionKnowledgeReceipt(request.DocumentID, int64(len(chunks)), &idempotent), nil
	}
	// :392-397 - a link or a copy without an index is missing commit evidence,
	// not permission to spend the work again.
	linked, err := productionKnowledgeLinked(ctx, db, contextValue, source, request.DocumentID)
	if err != nil {
		return nil, err
	}
	present, err := productionKnowledgeCopyPresent(storage.storage, request.DocumentID)
	if err != nil {
		return nil, err
	}
	if linked || present {
		return nil, errProductionKnowledgeRepairRequired
	}
	document, rows, err := productionKnowledgeNewDocument(request, request.DocumentID, chunks, time.Now())
	if err != nil {
		return nil, err
	}
	if err := productionKnowledgeCommit(ctx, storage.storage, index, document, rows); err != nil {
		return nil, err
	}
	// :402-409 - the fresh import is re-verified from the committed rows, never
	// from the values that produced them.
	committed, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		return nil, err
	}
	written, found := committed.find(request.DocumentID)
	if !found {
		return nil, errProductionKnowledgeRepairRequired
	}
	if err := productionKnowledgeCanonical(committed, written, chunks); err != nil {
		return nil, err
	}
	// The fresh import is already complete; the vectors are optional and can
	// never turn it into a failure.
	k.refreshVectorsBestEffort(ctx, storage)
	if err := storage.verify(); err != nil {
		return nil, err
	}
	idempotent := false
	return productionKnowledgeReceipt(request.DocumentID, int64(len(chunks)), &idempotent), nil
}

// complete is the :379-391 evidence check: the canonical rows AND the copy must
// both be complete. A torn copy is a repair, never a receipt.
func (k *productionKnowledgeImports) complete(ctx context.Context, storage *os.Root,
	index productionKnowledgeIndex, document productionKnowledgeDocument, content string, chunks []string) error {
	if err := productionKnowledgeCanonical(index, document, chunks); err != nil {
		return err
	}
	copies, err := knowledgecopy.ReadManagedCopies(ctx, storage,
		[]knowledgecopy.DocumentRef{{ID: document.ID, FilePath: document.FilePath}},
		productionKnowledgeCopyBudget())
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		// A store this lane cannot read is uncertainty, so it is the same repair
		// refusal the reference uses for a damaged copy.
		return errProductionKnowledgeRepairRequired
	}
	copied, ok := copies[document.ID]
	if !ok {
		return errProductionKnowledgeRepairRequired
	}
	// :381-384 - content, indexDirty and indexedHash, exactly those three.
	if copied.Content != content || copied.IndexDirty || copied.IndexedHash != productionKnowledgeHash(content) {
		return errProductionKnowledgeRepairRequired
	}
	return nil
}

// productionKnowledgeCanonical is getDocumentIntegrity's canonicalComplete
// (vector-store.ts:1826-1834): one document row, exactly the expected chunk
// count, ordered chunk indexes with no gaps or duplicates, one totalChunks and
// one corpus kind per document, and the sha256 of JSON.stringify(chunks)
// agreeing with the row set. Embedding generations are vacuous here: this lane
// writes none.
func productionKnowledgeCanonical(index productionKnowledgeIndex, document productionKnowledgeDocument, chunks []string) error {
	if document.CorpusKind != productionKnowledgeCorpusKind ||
		document.ChunkCount != int64(len(chunks)) ||
		document.FilePath != productionKnowledgeCopyMarker+document.ID {
		return errProductionKnowledgeRepairRequired
	}
	rows, err := index.ordered(document.ID)
	if err != nil || len(rows) != len(chunks) {
		return errProductionKnowledgeRepairRequired
	}
	texts := make([]string, 0, len(rows))
	seen := make(map[string]bool, len(rows))
	for position, row := range rows {
		if row.ChunkIndex != int64(position) || row.TotalChunks != int64(len(rows)) ||
			row.CorpusKind != document.CorpusKind || row.FileName != document.FileName {
			return errProductionKnowledgeRepairRequired
		}
		if seen[row.ID] {
			return errProductionKnowledgeRepairRequired
		}
		seen[row.ID] = true
		texts = append(texts, row.Text)
	}
	if productionKnowledgeChunkSetHash(texts) != productionKnowledgeChunkSetHash(chunks) {
		return errProductionKnowledgeRepairRequired
	}
	return nil
}

// productionKnowledgeCommit writes one document: its copy first, then the index,
// then the removals the prune decided. That order is the reference's
// (writeKnowledgeCopy at :311-313 precedes addChunks at :315), and it is why a
// crash in between leaves a copy with no index - the state :392-397 treats as a
// repair, never as permission to re-import.
func productionKnowledgeCommit(ctx context.Context, storage *os.Root, index productionKnowledgeIndex,
	document productionKnowledgeDocument, rows []productionKnowledgeChunk) error {
	// indexedHash is sha256 of the copy's own text
	// (knowledge-base.ts:311-313), not its timestamp: it is what the repair and
	// idempotency checks compare against.
	created, err := productionKnowledgeWriteCopy(storage, document.ID, document.Content,
		productionKnowledgeHash(document.Content))
	if err != nil {
		return err
	}
	next, pruned := index.replaced(document, rows)
	if err := productionKnowledgeWriteIndex(ctx, storage, next); err != nil {
		if created {
			_ = productionKnowledgeRemoveCopy(storage, document.ID)
		}
		return err
	}
	// pruneSupersededDocuments also removes the superseded copy
	// (removeDocumentInternal, vector-store.ts:1265-1290). A removal that fails
	// would leave an unreferenced file, so it is a refusal, not a warning: this
	// lane never leaves a copy a later capture would have to guess about.
	for _, documentID := range pruned {
		if err := productionKnowledgeRemoveCopy(storage, documentID); err != nil {
			return err
		}
	}
	return nil
}

// productionKnowledgeNewDocument builds the document row and its chunk rows
// (addChunks, vector-store.ts:1098-1180): a fresh UUID per chunk, one ISO
// importedAt for every row, chunkIndex from 0, totalChunks = len(chunks), the
// chapter pair when the caller has one, and this channel's corpus kind.
func productionKnowledgeNewDocument(request productionKnowledgeRequest, documentID string, chunks []string,
	now time.Time) (productionKnowledgeDocument, []productionKnowledgeChunk, error) {
	stamp := productionKnowledgeTimestamp(now)
	chapterNumber, chapterTitle := request.ChapterNumber, request.ChapterTitle
	if chapterNumber == nil && chapterTitle == nil {
		// parseChapterMetaFromFileName (knowledge-base.ts:262-271, :293) runs
		// only when the caller supplied no override.
		if number, title, ok := productionKnowledgeChapterMeta(request.FileName); ok {
			chapterNumber, chapterTitle = &number, &title
		}
	}
	document := productionKnowledgeDocument{
		ID:         documentID,
		FileName:   request.FileName,
		ImportedAt: stamp,
		ChunkCount: int64(len(chunks)),
		FilePath:   productionKnowledgeCopyMarker + documentID,
		CorpusKind: productionKnowledgeCorpusKind,
		Content:    request.Text,
	}
	rows := make([]productionKnowledgeChunk, 0, len(chunks))
	for position, text := range chunks {
		id, err := productionKnowledgeUUID()
		if err != nil {
			return productionKnowledgeDocument{}, nil, errProductionKnowledgeStorageUnsafe
		}
		rows = append(rows, productionKnowledgeChunk{
			ID:            id,
			DocID:         documentID,
			FileName:      request.FileName,
			Text:          text,
			ChunkIndex:    int64(position),
			TotalChunks:   int64(len(chunks)),
			ImportedAt:    stamp,
			ChapterNumber: chapterNumber,
			ChapterTitle:  chapterTitle,
			CorpusKind:    productionKnowledgeCorpusKind,
		})
	}
	return document, rows, nil
}

// productionKnowledgeChapterMeta is parseChapterMetaFromFileName
// (knowledge-base.ts:262-271). parseInt runs on [0-9]+, and the result is only
// usable when it fits the int32 chapter column (internal/knowledgedomain treats
// chapterNumber as a numeric field); an out-of-range 章号 is dropped exactly as
// an unmatched name is.
func productionKnowledgeChapterMeta(fileName string) (int64, string, bool) {
	match := productionKnowledgeFileNamePattern.FindStringSubmatch(fileName)
	if match == nil {
		return 0, "", false
	}
	number, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil || number < 0 || number > int64(1<<31-1) {
		return 0, "", false
	}
	return number, match[2], true
}

// productionKnowledgeTimestamp is Date.prototype.toISOString for one instant:
// UTC, always three fractional digits, always Z (vector-store.ts uses
// `new Date().toISOString()` for importedAt).
func productionKnowledgeTimestamp(now time.Time) string {
	return now.UTC().Format("2006-01-02T15:04:05.000Z")
}

// productionKnowledgeUUID is crypto.randomUUID(): 122 random bits with the
// version-4 and RFC 4122 variant nibbles set, lowercase, hyphenated.
func productionKnowledgeUUID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	encoded := hex.EncodeToString(raw[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}

// productionKnowledgeHash is createHash('sha256').update(text).digest('hex')
// (:312, inside writeKnowledgeCopy) over the UTF-8 bytes.
func productionKnowledgeHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// productionKnowledgeHashValid is the reference's 64-lowercase-hex content hash.
func productionKnowledgeHashValid(hash string) bool {
	if len(hash) != 64 {
		return false
	}
	for _, digit := range hash {
		if (digit < '0' || digit > '9') && (digit < 'a' || digit > 'f') {
			return false
		}
	}
	return true
}

// productionKnowledgeChunkSetHash is hashCanonicalChunkSet
// (vector-store.ts:1787-1789): sha256 over JSON.stringify(chunks). The quoting is
// ECMAScript's, not encoding/json's - Go escapes <, >, &, U+2028 and U+2029 by
// default and JSON.stringify does not, so an author's 引号 or angle bracket would
// otherwise change the digest this store calls integrity.
func productionKnowledgeChunkSetHash(chunks []string) string {
	var builder strings.Builder
	builder.WriteByte('[')
	for position, chunk := range chunks {
		if position > 0 {
			builder.WriteByte(',')
		}
		productionKnowledgeQuote(&builder, chunk)
	}
	builder.WriteByte(']')
	digest := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(digest[:])
}

// productionKnowledgeQuote is JSON.stringify's string case.
func productionKnowledgeQuote(builder *strings.Builder, text string) {
	const digits = "0123456789abcdef"
	builder.WriteByte('"')
	start := 0
	for position := 0; position < len(text); position++ {
		character := text[position]
		if character >= 0x20 && character != '"' && character != '\\' {
			continue
		}
		builder.WriteString(text[start:position])
		switch character {
		case '"':
			builder.WriteString("\\\"")
		case '\\':
			builder.WriteString("\\\\")
		case '\b':
			builder.WriteString("\\b")
		case '\f':
			builder.WriteString("\\f")
		case '\n':
			builder.WriteString("\\n")
		case '\r':
			builder.WriteString("\\r")
		case '\t':
			builder.WriteString("\\t")
		default:
			builder.WriteString("\\u00")
			builder.WriteByte(digits[character>>4])
			builder.WriteByte(digits[character&0x0f])
		}
		start = position + 1
	}
	builder.WriteString(text[start:])
	builder.WriteByte('"')
}

// productionKnowledgeSafeIntegerOf is Number.isSafeInteger over a decoded JSON
// value: integral, and within +/-(2^53-1).
func productionKnowledgeSafeIntegerOf(value any) (int64, bool) {
	var number int64
	switch typed := value.(type) {
	case int64:
		number = typed
	case int:
		number = int64(typed)
	case int32:
		number = int64(typed)
	case float64:
		if typed != float64(int64(typed)) {
			return 0, false
		}
		number = int64(typed)
	case json.Number:
		parsed, err := strconv.ParseInt(string(typed), 10, 64)
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if number > productionKnowledgeSafeInteger || number < -productionKnowledgeSafeInteger {
		return 0, false
	}
	return number, true
}

// productionKnowledgeDocument is one row of the `documents` table, in the
// reference's column set. Content is not a column: the copy holds it, exactly as
// the reference's row holds only the marker.
type productionKnowledgeDocument struct {
	ID         string
	FileName   string
	ImportedAt string
	ChunkCount int64
	FilePath   string
	CorpusKind string

	// Content is the document's text as this import wrote it. It is never read
	// back from the index; the copy is.
	Content string
}

// productionKnowledgeChunk is one row of the `chunks` table. Index is 0-based,
// TotalChunks is the document's count, and the chapter pair is nullable because
// a fileName that names no chapter has no meta.
type productionKnowledgeChunk struct {
	ID            string
	DocID         string
	FileName      string
	Text          string
	ChunkIndex    int64
	TotalChunks   int64
	ImportedAt    string
	ChapterNumber *int64
	ChapterTitle  *string
	CorpusKind    string
}

// productionKnowledgeIndex is the whole store: what the snapshot file holds, in
// typed form.
type productionKnowledgeIndex struct {
	Documents []productionKnowledgeDocument
	Chunks    []productionKnowledgeChunk
}

// find is the document-row lookup.
func (i productionKnowledgeIndex) find(documentID string) (productionKnowledgeDocument, bool) {
	for _, document := range i.Documents {
		if document.ID == documentID {
			return document, true
		}
	}
	return productionKnowledgeDocument{}, false
}

// ordered returns one document's chunks in chunkIndex order, refusing a gap, a
// duplicate index or a stale totalChunks - the three checks getDocumentIntegrity
// makes before it calls a document complete.
func (i productionKnowledgeIndex) ordered(documentID string) ([]productionKnowledgeChunk, error) {
	rows := make([]productionKnowledgeChunk, 0, len(i.Chunks))
	for _, chunk := range i.Chunks {
		if chunk.DocID == documentID {
			rows = append(rows, chunk)
		}
	}
	position := make(map[int64]bool, len(rows))
	for _, row := range rows {
		if row.ChunkIndex < 0 || position[row.ChunkIndex] {
			return nil, errProductionKnowledgeIndexInvalid
		}
		position[row.ChunkIndex] = true
	}
	ordered := make([]productionKnowledgeChunk, len(rows))
	for _, row := range rows {
		if row.ChunkIndex >= int64(len(rows)) || row.TotalChunks != int64(len(rows)) {
			return nil, errProductionKnowledgeIndexInvalid
		}
		ordered[row.ChunkIndex] = row
	}
	return ordered, nil
}

// replaced is addChunks' write half plus pruneSupersededDocuments
// (vector-store.ts:1011-1049, :1080-1180): the written document replaces its own
// rows in place (stable identity, replaceDocumentChunks :1226), and every other
// document with the same (fileName, corpusKind) is dropped with its chunks. The
// second return value names the documents whose copies must be removed.
func (i productionKnowledgeIndex) replaced(document productionKnowledgeDocument,
	rows []productionKnowledgeChunk) (productionKnowledgeIndex, []string) {
	pruned := make([]string, 0, 1)
	dropped := make(map[string]bool)
	next := productionKnowledgeIndex{
		Documents: make([]productionKnowledgeDocument, 0, len(i.Documents)+1),
		Chunks:    make([]productionKnowledgeChunk, 0, len(i.Chunks)+len(rows)),
	}
	for _, existing := range i.Documents {
		if existing.ID == document.ID {
			continue
		}
		if existing.FileName == document.FileName && existing.CorpusKind == document.CorpusKind {
			pruned = append(pruned, existing.ID)
			dropped[existing.ID] = true
			continue
		}
		next.Documents = append(next.Documents, existing)
	}
	for _, chunk := range i.Chunks {
		if chunk.DocID == document.ID || dropped[chunk.DocID] {
			continue
		}
		next.Chunks = append(next.Chunks, chunk)
	}
	next.Documents = append(next.Documents, document)
	next.Chunks = append(next.Chunks, rows...)
	return next, pruned
}

// productionKnowledgeStorage is the project's storage directory, opened as a
// confined root with both inodes pinned. projectPath is not a request value: it
// is the frozen context's path, which borrowAuthorDatabase has already proved is
// an admitted project (production_durable_linux.go:313-364).
type productionKnowledgeStorage struct {
	project     *os.Root
	storage     *os.Root
	projectPath string
	projectInfo fs.FileInfo
	storageInfo fs.FileInfo
}

// productionKnowledgeOpenStorage opens {projectPath}/.ai-novel as a child root.
func productionKnowledgeOpenStorage(projectPath string) (*productionKnowledgeStorage, error) {
	project, err := os.OpenRoot(projectPath)
	if err != nil {
		return nil, errProductionKnowledgeStorageUnsafe
	}
	projectInfo, err := project.Stat(".")
	if err != nil || projectInfo == nil || !projectInfo.IsDir() {
		_ = project.Close()
		return nil, errProductionKnowledgeStorageUnsafe
	}
	storage, err := project.OpenRoot(projectfs.StorageDirectory)
	if err != nil {
		_ = project.Close()
		return nil, errProductionKnowledgeStorageUnsafe
	}
	storageInfo, err := storage.Stat(".")
	if err != nil || storageInfo == nil || !storageInfo.IsDir() {
		_ = storage.Close()
		_ = project.Close()
		return nil, errProductionKnowledgeStorageUnsafe
	}
	return &productionKnowledgeStorage{
		project:     project,
		storage:     storage,
		projectPath: projectPath,
		projectInfo: projectInfo,
		storageInfo: storageInfo,
	}, nil
}

// Close releases both roots; the first failure is the answer.
func (s *productionKnowledgeStorage) Close() error {
	if s == nil {
		return nil
	}
	var failure error
	if s.storage != nil {
		if err := s.storage.Close(); err != nil {
			failure = err
		}
	}
	if s.project != nil {
		if err := s.project.Close(); err != nil && failure == nil {
			failure = err
		}
	}
	return failure
}

// verify re-opens both directories by path and requires the same inodes, the
// check projectfs.checkIdentityLocked makes around a write. A replaced directory
// is uncertainty, never a successful import.
func (s *productionKnowledgeStorage) verify() error {
	if s == nil || s.projectPath == "" {
		return errProductionKnowledgeStorageUnsafe
	}
	project, err := os.OpenRoot(s.projectPath)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = project.Close() }()
	projectInfo, err := project.Stat(".")
	if err != nil || !os.SameFile(projectInfo, s.projectInfo) {
		return errProductionKnowledgeStorageUnsafe
	}
	storage, err := project.OpenRoot(projectfs.StorageDirectory)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = storage.Close() }()
	storageInfo, err := storage.Stat(".")
	if err != nil || !os.SameFile(storageInfo, s.storageInfo) {
		return errProductionKnowledgeStorageUnsafe
	}
	return nil
}

// refuseLegacyVectors is ensureMigration's refusal (knowledge-base.ts:60-65): an
// unmigrated legacy vector file blocks every import. This lane owns no
// migration, so the only honest answer is the reference's own blocked code.
func (s *productionKnowledgeStorage) refuseLegacyVectors() error {
	if s == nil || s.storage == nil {
		return errProductionKnowledgeStorageUnsafe
	}
	for _, name := range []string{productionKnowledgeLegacyVectorName, productionKnowledgeLegacyVectorJournal} {
		_, err := s.storage.Lstat(name)
		if err == nil {
			return errProductionKnowledgeLegacyBlocked
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return errProductionKnowledgeStorageUnsafe
		}
	}
	return nil
}

// productionKnowledgeReadIndex reads and validates the snapshot. An absent file
// is an empty store: the reference's empty Lance directory answers the same way.
func productionKnowledgeReadIndex(ctx context.Context, storage *os.Root) (productionKnowledgeIndex, error) {
	if storage == nil {
		return productionKnowledgeIndex{}, errProductionKnowledgeStorageUnsafe
	}
	file, err := storage.OpenFile(productionKnowledgeIndexName, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return productionKnowledgeIndex{}, nil
	}
	if err != nil {
		return productionKnowledgeIndex{}, errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > productionKnowledgeIndexMaxBytes {
		return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(file, productionKnowledgeIndexMaxBytes+1))
	if err != nil || int64(len(raw)) > productionKnowledgeIndexMaxBytes {
		return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
	}
	return productionKnowledgeDecodeIndex(ctx, raw)
}

// productionKnowledgeDecodeIndex turns the file's bytes into the typed index,
// through the same knowledgedomain.Validate the served reads run. A store this
// lane wrote therefore cannot be readable here and invisible to kb:search.
func productionKnowledgeDecodeIndex(ctx context.Context, raw []byte) (productionKnowledgeIndex, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var snapshot knowledgedomain.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
	}
	if _, err := knowledgedomain.Validate(ctx, snapshot, productionKnowledgeDomainBudget()); err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return productionKnowledgeIndex{}, contextError
		}
		return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
	}
	index := productionKnowledgeIndex{Documents: make([]productionKnowledgeDocument, 0, 8)}
	for _, table := range snapshot.Tables {
		switch table.Name {
		case productionKnowledgeTableDocuments:
			for _, row := range table.Records {
				document, ok := productionKnowledgeDocumentOf(row)
				if !ok {
					return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
				}
				index.Documents = append(index.Documents, document)
			}
		case productionKnowledgeTableChunks:
			for _, row := range table.Records {
				chunk, ok := productionKnowledgeChunkOf(row)
				if !ok {
					return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
				}
				index.Chunks = append(index.Chunks, chunk)
			}
		default:
			// knowledgedomain already refused an unregistered space table, so a
			// table this writer does not own cannot arrive here.
			return productionKnowledgeIndex{}, errProductionKnowledgeIndexInvalid
		}
	}
	return index, nil
}

// productionKnowledgeDocumentOf projects one validated documents row. The copy
// content is not in the row and stays empty; the writer sets it from the request.
func productionKnowledgeDocumentOf(row map[string]any) (productionKnowledgeDocument, bool) {
	id, idOK := row["id"].(string)
	fileName, fileNameOK := row["fileName"].(string)
	importedAt, importedAtOK := row["importedAt"].(string)
	count, countOK := productionKnowledgeSafeIntegerOf(row["chunkCount"])
	filePath, filePathOK := row["filePath"].(string)
	if !idOK || !fileNameOK || !importedAtOK || !countOK || !filePathOK {
		return productionKnowledgeDocument{}, false
	}
	corpusKind, _ := row["corpusKind"].(string)
	return productionKnowledgeDocument{
		ID:         id,
		FileName:   fileName,
		ImportedAt: importedAt,
		ChunkCount: count,
		FilePath:   filePath,
		CorpusKind: corpusKind,
	}, true
}

// productionKnowledgeChunkOf projects one validated chunks row. chapterNumber
// and chapterTitle are the two nullable columns, so a JSON null is normal.
func productionKnowledgeChunkOf(row map[string]any) (productionKnowledgeChunk, bool) {
	id, idOK := row["id"].(string)
	documentID, documentOK := row["docId"].(string)
	fileName, fileNameOK := row["fileName"].(string)
	text, textOK := row["text"].(string)
	index, indexOK := productionKnowledgeSafeIntegerOf(row["chunkIndex"])
	total, totalOK := productionKnowledgeSafeIntegerOf(row["totalChunks"])
	importedAt, importedAtOK := row["importedAt"].(string)
	if !idOK || !documentOK || !fileNameOK || !textOK || !indexOK || !totalOK || !importedAtOK {
		return productionKnowledgeChunk{}, false
	}
	chunk := productionKnowledgeChunk{
		ID:          id,
		DocID:       documentID,
		FileName:    fileName,
		Text:        text,
		ChunkIndex:  index,
		TotalChunks: total,
		ImportedAt:  importedAt,
	}
	if value, present := row["corpusKind"]; present && value != nil {
		corpusKind, ok := value.(string)
		if !ok {
			return productionKnowledgeChunk{}, false
		}
		chunk.CorpusKind = corpusKind
	}
	if value, present := row["chapterNumber"]; present && value != nil {
		number, ok := productionKnowledgeSafeIntegerOf(value)
		if !ok {
			return productionKnowledgeChunk{}, false
		}
		chunk.ChapterNumber = &number
	}
	if value, present := row["chapterTitle"]; present && value != nil {
		title, ok := value.(string)
		if !ok {
			return productionKnowledgeChunk{}, false
		}
		chunk.ChapterTitle = &title
	}
	return chunk, true
}

// productionKnowledgeWriteIndex validates the snapshot it is about to write with
// the readers' own validator, then writes it atomically. Validating first is the
// point: a row this lane could write but kb:search could not admit would be a
// silent hole in the author's workflow.
func productionKnowledgeWriteIndex(ctx context.Context, storage *os.Root, index productionKnowledgeIndex) error {
	if storage == nil {
		return errProductionKnowledgeStorageUnsafe
	}
	snapshot := productionKnowledgeSnapshotOf(index)
	if _, err := knowledgedomain.Validate(ctx, snapshot, productionKnowledgeDomainBudget()); err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return contextError
		}
		return errProductionKnowledgeIndexInvalid
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return errProductionKnowledgeIndexInvalid
	}
	return productionKnowledgeWriteAtomic(storage, productionKnowledgeIndexName, raw)
}

// productionKnowledgeSnapshotOf is the wire form of the whole store. `registry`
// stays nil, which marshals to JSON null: that is the FTS-only lane, and it is
// why every chunk in this store is vectorless (internal/knowledgequery counts
// them in VectorlessCount).
func productionKnowledgeSnapshotOf(index productionKnowledgeIndex) knowledgedomain.Snapshot {
	documents := make([]map[string]any, 0, len(index.Documents))
	for _, document := range index.Documents {
		documents = append(documents, map[string]any{
			"id":         document.ID,
			"fileName":   document.FileName,
			"importedAt": document.ImportedAt,
			"chunkCount": document.ChunkCount,
			"filePath":   document.FilePath,
			"corpusKind": document.CorpusKind,
		})
	}
	chunks := make([]map[string]any, 0, len(index.Chunks))
	for _, chunk := range index.Chunks {
		row := map[string]any{
			"id":            chunk.ID,
			"docId":         chunk.DocID,
			"fileName":      chunk.FileName,
			"text":          chunk.Text,
			"chunkIndex":    chunk.ChunkIndex,
			"totalChunks":   chunk.TotalChunks,
			"importedAt":    chunk.ImportedAt,
			"corpusKind":    chunk.CorpusKind,
			"chapterNumber": nil,
			"chapterTitle":  nil,
		}
		if chunk.ChapterNumber != nil {
			row["chapterNumber"] = *chunk.ChapterNumber
		}
		if chunk.ChapterTitle != nil {
			row["chapterTitle"] = *chunk.ChapterTitle
		}
		chunks = append(chunks, row)
	}
	field := func(name, kind string, nullable bool) knowledgedomain.Field {
		return knowledgedomain.Field{Name: name, Type: kind, Nullable: nullable}
	}
	documentSchema := []knowledgedomain.Field{
		field("id", "utf8", false),
		field("fileName", "utf8", false),
		field("importedAt", "utf8", false),
		field("chunkCount", "int32", false),
		field("filePath", "utf8", false),
		field("corpusKind", "utf8", false),
	}
	chunkSchema := []knowledgedomain.Field{
		field("id", "utf8", false),
		field("docId", "utf8", false),
		field("fileName", "utf8", false),
		field("text", "utf8", false),
		field("chunkIndex", "int32", false),
		field("totalChunks", "int32", false),
		field("importedAt", "utf8", false),
		field("corpusKind", "utf8", false),
		field("chapterNumber", "int32", true),
		field("chapterTitle", "utf8", true),
	}
	return knowledgedomain.Snapshot{
		Tables: []knowledgedomain.Table{
			{
				Name:    productionKnowledgeTableDocuments,
				Version: 1,
				Schema:  documentSchema,
				Records: documents,
			},
			{
				Name:    productionKnowledgeTableChunks,
				Version: 1,
				Schema:  chunkSchema,
				Records: chunks,
			},
		},
	}
}

// productionKnowledgeDomainBudget is the served readers' own budget: the hard
// limits internal/knowledgedomain publishes.
func productionKnowledgeDomainBudget() knowledgedomain.Budget {
	return knowledgedomain.Budget{
		MaxTables:    knowledgedomain.HardMaxTables,
		MaxRows:      knowledgedomain.HardMaxRows,
		MaxCells:     knowledgedomain.HardMaxCells,
		MaxTextBytes: knowledgedomain.HardMaxTextBytes,
	}
}

// productionKnowledgeCopyBudget is the capture budget internal/knowledgecopy
// publishes; the limits are the package's own constants.
func productionKnowledgeCopyBudget() knowledgecopy.Budget {
	return knowledgecopy.Budget{
		MaxMetadataBytes: knowledgecopy.MaxMetadataBytes,
		MaxCopies:        knowledgecopy.MaxCopies,
	}
}

// productionKnowledgeWriteCopy is writeKnowledgeCopy (vector-store.ts:156-170):
// {content, indexedHash, edited:false, indexDirty:false} at
// knowledge-copies/<sha256hex(docId)>.json (vector-store.ts:156-166), in that key order, written to a
// temporary name and renamed into place. It reports whether this call created the
// file, so a failed index write can take its own copy back out.
func productionKnowledgeWriteCopy(storage *os.Root, documentID, content, indexedHash string) (bool, error) {
	child, err := productionKnowledgeCopyRoot(storage, true)
	if err != nil {
		return false, err
	}
	if child == nil {
		return false, errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = child.Close() }()
	name := productionKnowledgeCopyName(documentID)
	created := false
	if _, err := child.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		created = true
	} else if err != nil {
		return false, errProductionKnowledgeStorageUnsafe
	}
	body, err := json.Marshal(struct {
		Content     string `json:"content"`
		IndexedHash string `json:"indexedHash"`
		Edited      bool   `json:"edited"`
		IndexDirty  bool   `json:"indexDirty"`
	}{Content: content, IndexedHash: indexedHash})
	if err != nil {
		return created, errProductionKnowledgeStorageUnsafe
	}
	if err := productionKnowledgeWriteAtomic(child, name, body); err != nil {
		return created, err
	}
	return created, nil
}

// productionKnowledgeCopyName is isDocumentCopyIndexCurrent's lookup: lowercase
// sha256 of the exact UTF-8 document id plus ".json" (vector-store.ts:135-142).
func productionKnowledgeCopyName(documentID string) string {
	digest := sha256.Sum256([]byte(documentID))
	return hex.EncodeToString(digest[:]) + productionKnowledgeCopySuffix
}

// productionKnowledgeCopyRoot opens knowledge-copies/ as a child root. create is
// the writer's; a reader gets (nil, nil) when the directory does not exist.
func productionKnowledgeCopyRoot(storage *os.Root, create bool) (*os.Root, error) {
	if storage == nil {
		return nil, errProductionKnowledgeStorageUnsafe
	}
	info, err := storage.Lstat(productionKnowledgeCopyDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		if !create {
			return nil, nil
		}
		if err := storage.Mkdir(productionKnowledgeCopyDirectory, 0700); err != nil {
			return nil, errProductionKnowledgeStorageUnsafe
		}
		info, err = storage.Lstat(productionKnowledgeCopyDirectory)
	}
	if err != nil || info == nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errProductionKnowledgeStorageUnsafe
	}
	child, err := storage.OpenRoot(productionKnowledgeCopyDirectory)
	if err != nil {
		return nil, errProductionKnowledgeStorageUnsafe
	}
	return child, nil
}

// productionKnowledgeRemoveCopy is removeDocumentInternal's copy removal
// (vector-store.ts:1265-1290): the superseded document's copy goes with it.
func productionKnowledgeRemoveCopy(storage *os.Root, documentID string) error {
	child, err := productionKnowledgeCopyRoot(storage, false)
	if err != nil {
		return err
	}
	if child == nil {
		return nil
	}
	defer func() { _ = child.Close() }()
	if err := child.Remove(productionKnowledgeCopyName(documentID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errProductionKnowledgeStorageUnsafe
	}
	return nil
}

// productionKnowledgeCopyPresent answers whether a copy file exists for this
// document. It is the reference's `readKnowledgeCopy(...)` truthiness test
// (:393), which needs existence rather than content.
func productionKnowledgeCopyPresent(storage *os.Root, documentID string) (bool, error) {
	child, err := productionKnowledgeCopyRoot(storage, false)
	if err != nil {
		return false, err
	}
	if child == nil {
		return false, nil
	}
	defer func() { _ = child.Close() }()
	info, err := child.Lstat(productionKnowledgeCopyName(documentID))
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errProductionKnowledgeStorageUnsafe
	}
	return info != nil && info.Mode().IsRegular(), nil
}

// productionKnowledgeWriteAtomic is the lane's durable write: an exclusive
// temporary file beside the target, an fsync of its bytes, a rename, and an
// fsync of the directory. A reader therefore sees the old file or the new one,
// never a half-written one.
func productionKnowledgeWriteAtomic(root *os.Root, name string, body []byte) error {
	if root == nil {
		return errProductionKnowledgeStorageUnsafe
	}
	token, err := productionKnowledgeUUID()
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	temporary := name + "." + token + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	renamed := false
	defer func() {
		if !renamed {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return errProductionKnowledgeStorageUnsafe
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errProductionKnowledgeStorageUnsafe
	}
	if err := file.Close(); err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	if err := root.Rename(temporary, name); err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	renamed = true
	return productionKnowledgeSyncDirectory(root)
}

// productionKnowledgeSyncDirectory fsyncs a directory root, which is what makes
// the rename durable rather than merely visible.
func productionKnowledgeSyncDirectory(root *os.Root) error {
	directory, err := root.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return errProductionKnowledgeStorageUnsafe
	}
	return nil
}

// enter takes the project's knowledge gate and returns its release. It is the
// per-project serialization withKnowledgeSourceGate provides, so the
// read-modify-write of the index cannot interleave with a second import.
func (k *productionKnowledgeImports) enter(projectPath string) (func(), error) {
	if k == nil || projectPath == "" {
		return nil, errProductionKnowledgeStorageUnsafe
	}
	k.mu.Lock()
	if k.gates == nil {
		k.gates = make(map[string]*sync.Mutex)
	}
	gate, present := k.gates[projectPath]
	if !present {
		gate = &sync.Mutex{}
		k.gates[projectPath] = gate
	}
	k.mu.Unlock()
	gate.Lock()
	return gate.Unlock, nil
}

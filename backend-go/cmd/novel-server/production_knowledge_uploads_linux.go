//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the three knowledge-base IMPORTS a browser can honestly
// perform. kb:backfill-vectors lives in production_knowledge_search_linux.go.
//
//	kb:import-document        arity 2  [uploadId, projectPath]
//	kb:import-folder          arity 2  [uploadId, projectPath]
//	kb:import-reference-text  arity 2  [uploadId, stableKey]
//
// # Why these three were refused before, and what changed
//
// production_knowledge_documents_linux.go:14-22 and
// production_knowledge_search_linux.go:12-20 refused them because the desktop
// resolved a *grant* to a host path (an opaque capability minted by a native
// folder dialog) and because the reference import required a run-authority receipt
// this lane cannot write. A browser has neither a native dialog nor that receipt.
// It does have the upload path this repo already uses for every browser file:
// POST /api/uploads stages the bytes under the caller's session and tab
// (internal/transferhttp/uploads.go:132-215), and the productionPickerUploads seam
// hands them back to a handler that knows the caller
// (production_browser_pickers_linux.go:316-323). So the grant id becomes an upload
// id: an opaque token that is only redeemable by the session and tab that staged
// it, and that no caller can turn into a host path.
//
// Everything after the bytes arrive is the ALREADY-SERVED writer, unchanged:
// `kb:import-text`'s own importTextInternal half (production_knowledge_import_linux.go:
// 765-813) - the same chunker, the same 500/50 boundaries, the same
// .ai-novel/knowledge-snapshot.json with `registry: null`, the same FTS-only
// tables, and the same prune rule. That is the whole point of binding these here
// rather than in a second writer: one canonical snapshot, several doors.
//
// # The id rules are the reference's, reused rather than reinvented
//
//	kb:import-document  : a fresh randomUUID per call (:287) plus
//	                      pruneSupersededDocuments over (fileName, corpusKind)
//	                      (vector-store.ts:1011-1049) - so importing the same file
//	                      twice leaves ONE document, never two.
//	kb:import-folder    : the same call once per file, so the same rule.
//	kb:import-reference-text : sha256hex("reference-import:" + stableKey)
//	                      (electron/knowledge-base.ts:672-694) with
//	                      replacementMode 'stable-id', which replaces only that
//	                      document's own rows and prunes nothing
//	                      (vector-store.ts:1207-1210).
//
// # What diverges from the desktop, and why that is not a fake success
//
// kb:import-reference-text's desktop arity is [chapterNumber, runId,
// executionAuthority]: runId and executionAuthority are import-run receipts that
// only internal/store's assertExecutionAuthority can mint, and that function is
// unexported with no writer for the receipt tables anywhere in backend-go
// (production_knowledge_documents_linux.go:18-22). A browser cannot obtain them and
// this lane must not accept them from a request body, so the browser form takes
// what a browser actually has: the uploaded work and the author's stable import
// key. The canonical snapshot it writes is identical - same docId rule, same
// chunks, same copy file, same corpusKind 'reference' - and the answer reports
// `idempotent`, so a page can tell a replay from a fresh import. The receipt
// tables the desktop also wrote are NOT written here; that is stated plainly
// rather than faked.
//
// # A refusal costs nothing
//
// Every bound and every structural check runs before the first write: the upload
// must be text, in-bounds, single-component, and inside the folder budget, and a
// folder member's relative path must survive productionPickerUploadMember. Only
// then is the existing writer called, and its own commit is already
// copy-then-index with a rollback of the copy it just created
// (production_knowledge_import_linux.go:956-987).

const (
	productionKnowledgeImportDocumentName = "kb:import-document"
	productionKnowledgeImportFolderName   = "kb:import-folder"
	productionKnowledgeUploadReferenceName = "kb:import-reference-text"

	// productionKnowledgeUploadFolderFiles is the reference's own file cap
	// (MAX_SECURE_KNOWLEDGE_IMPORT_FILES, 16_384) reduced to the upload owner's
	// bound, since a folder upload cannot carry more than that anyway.
	productionKnowledgeUploadFolderFiles = productionPickerMaxUploadFiles

	// productionKnowledgeUploadFolderBytes bounds one folder import in total. The
	// reference has no such bound (it reads whatever the folder holds), but a
	// browser request must be bounded, and 32 MiB of text is already far past the
	// per-document 8 MiB.
	productionKnowledgeUploadFolderBytes = 32 << 20

	// productionKnowledgeUploadNameBytes bounds the one filename component a
	// knowledge document is named by.
	productionKnowledgeUploadNameBytes = productionPickerMaxMemberBytes
)

// The refusal codes this file adds. Each names the one thing that was wrong, so a
// page never has to parse a sentence. The reference's own code is reused where the
// reference had one (KNOWLEDGE_BASE_REFERENCE_KEY_INVALID, knowledge-base.ts:684).
const (
	productionKnowledgeCodeUploadRequired    = "KNOWLEDGE_BASE_UPLOAD_REQUIRED"
	productionKnowledgeCodeUploadUnavailable = "KNOWLEDGE_BASE_UPLOAD_UNAVAILABLE"
	productionKnowledgeCodeUploadUnknown     = "KNOWLEDGE_BASE_UPLOAD_UNKNOWN"
	productionKnowledgeCodeUploadTooLarge    = "KNOWLEDGE_BASE_UPLOAD_TOO_LARGE"
	productionKnowledgeCodeUploadLimit       = "KNOWLEDGE_BASE_UPLOAD_LIMIT"
	productionKnowledgeCodeUploadNotText     = "KNOWLEDGE_BASE_UPLOAD_NOT_TEXT"
	productionKnowledgeCodeUploadNameInvalid = "KNOWLEDGE_BASE_UPLOAD_NAME_INVALID"
	productionKnowledgeCodeReferenceKey      = "KNOWLEDGE_BASE_REFERENCE_KEY_INVALID"
)

var (
	errProductionKnowledgeUploadRequired    = errors.New(productionKnowledgeCodeUploadRequired)
	errProductionKnowledgeUploadUnavailable = errors.New(productionKnowledgeCodeUploadUnavailable)
	errProductionKnowledgeUploadUnknown     = errors.New(productionKnowledgeCodeUploadUnknown)
	errProductionKnowledgeUploadTooLarge    = errors.New(productionKnowledgeCodeUploadTooLarge)
	errProductionKnowledgeUploadLimit       = errors.New(productionKnowledgeCodeUploadLimit)
	errProductionKnowledgeUploadNotText     = errors.New(productionKnowledgeCodeUploadNotText)
	errProductionKnowledgeUploadNameInvalid = errors.New(productionKnowledgeCodeUploadNameInvalid)
	errProductionKnowledgeReferenceKey      = errors.New(productionKnowledgeCodeReferenceKey)
)

// productionKnowledgeReferenceKeyPattern is the reference's
// `/^[\w:.-]+$/u` (knowledge-base.ts:684).
var productionKnowledgeReferenceKeyPattern = regexp.MustCompile(`^[\w:.-]+$`)

// productionKnowledgeUploads serves the three import names. It owns no store: the
// writer, the chunker and the canonical snapshot all belong to
// productionKnowledgeImports, so an import through this file and one through
// `kb:import-text` cannot diverge.
type productionKnowledgeUploads struct {
	imports *productionKnowledgeImports

	// uploads is the upload owner. A nil seam is the truth about this process, and
	// every name that needs bytes answers an honest refusal rather than a
	// successful no-op.
	uploads productionPickerUploads
}

// newProductionKnowledgeUploads is the constructor the binder block wires.
func newProductionKnowledgeUploads(imports *productionKnowledgeImports,
	uploads productionPickerUploads) *productionKnowledgeUploads {
	return &productionKnowledgeUploads{imports: imports, uploads: uploads}
}

// Bind installs exactly the three names that can be honoured.
func (k *productionKnowledgeUploads) Bind(server *transport.Server) error {
	if k == nil || k.imports == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, binding := range []struct {
		name  string
		arity int
		call  func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)
	}{
		{productionKnowledgeImportDocumentName, 2, k.importDocument},
		{productionKnowledgeImportFolderName, 2, k.importFolder},
		{productionKnowledgeUploadReferenceName, 2, k.importReference},
	} {
		if err := server.Bind(binding.name, k.operation(binding.arity, binding.call)); err != nil {
			return err
		}
	}
	return nil
}

// operation is the import lane's adapter shape: the arity check, the frozen
// three-key context, the durable borrow (which proves the lease), and the handler.
func (k *productionKnowledgeUploads) operation(arity int,
	call func(context.Context, transport.Caller, *sql.DB, projectsession.Context, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if ctx == nil || len(args) != arity+1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		contextValue, err := projectsession.ParseContext(args[arity])
		if err != nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
		}
		db, release, err := k.imports.durable.borrowAuthorDatabase(ctx, caller, args[arity])
		if err != nil {
			return nil, err
		}
		if db == nil || release == nil {
			return nil, security.NewHTTPError(http.StatusServiceUnavailable, "GO_OPERATION_UNAVAILABLE")
		}
		defer release()
		return call(ctx, caller, db, contextValue, args[:arity])
	}
}

// importDocument is kb:import-document: one staged text file becomes one
// knowledge document through the served writer.
func (k *productionKnowledgeUploads) importDocument(ctx context.Context, caller transport.Caller, _ *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	uploadID, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[1], contextValue)
	if err != nil {
		return nil, err
	}
	name, data, err := k.read(caller, uploadID)
	if err != nil {
		return productionKnowledgeUploadFailure(err), nil
	}
	fileName, ok := productionKnowledgeUploadFileName(name)
	if !ok {
		return productionKnowledgeUploadFailure(errProductionKnowledgeUploadNameInvalid), nil
	}
	text, err := productionKnowledgeUploadText(data)
	if err != nil {
		return productionKnowledgeUploadFailure(err), nil
	}
	answer, err := k.imports.write(ctx, projectPath, productionKnowledgeRequest{Text: text, FileName: fileName})
	if err != nil {
		return productionKnowledgeUploadFailure(err), nil
	}
	return answer, nil
}

// importFolder is kb:import-folder: every text file inside one staged folder
// becomes its own document, exactly as the reference imports folder members one by
// one (electron/controllers/kb-controller.ts:256-306). The whole folder is
// validated before the first member is imported, so a refused folder writes
// nothing, and a folder with no text member reports zero imports rather than a
// failure - the reference's own answer.
func (k *productionKnowledgeUploads) importFolder(ctx context.Context, caller transport.Caller, _ *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	uploadID, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	projectPath, err := productionKnowledgeRequestPath(args[1], contextValue)
	if err != nil {
		return nil, err
	}
	owner, err := productionTabOwner(caller)
	if err != nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	if k.uploads == nil {
		return productionKnowledgeUploadFolderFailure(errProductionKnowledgeUploadUnavailable), nil
	}
	if uploadID == "" {
		return productionKnowledgeUploadFolderFailure(errProductionKnowledgeUploadRequired), nil
	}
	_, files, err := k.uploads.ReadFolder(owner.SessionID, owner.ClientID, uploadID)
	if err != nil {
		return productionKnowledgeUploadFolderFailure(errProductionKnowledgeUploadUnknown), nil
	}
	members, err := productionKnowledgeUploadMembers(files)
	if err != nil {
		return productionKnowledgeUploadFolderFailure(err), nil
	}
	failedFiles := make([]any, 0)
	imported := 0
	for _, member := range members {
		fileName, ok := productionKnowledgeUploadFileName(productionPickerBaseName(member.Relative))
		if !ok {
			failedFiles = append(failedFiles, member.Relative)
			continue
		}
		text, textErr := productionKnowledgeUploadText(member.Data)
		if textErr != nil {
			failedFiles = append(failedFiles, member.Relative)
			continue
		}
		if _, writeErr := k.imports.write(ctx, projectPath,
			productionKnowledgeRequest{Text: text, FileName: fileName}); writeErr != nil {
			failedFiles = append(failedFiles, member.Relative)
			continue
		}
		imported++
	}
	return map[string]any{
		"success":       true,
		"importedCount": int64(imported),
		"failedFiles":   failedFiles,
	}, nil
}

// importReference is kb:import-reference-text: one staged reference work, imported
// under the author's stable key so re-importing the same work updates it instead of
// duplicating it.
func (k *productionKnowledgeUploads) importReference(ctx context.Context, caller transport.Caller, _ *sql.DB,
	contextValue projectsession.Context, args []any) (any, error) {
	uploadID, ok := args[0].(string)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	stableKey, ok := productionKnowledgeReferenceKey(args[1])
	if !ok {
		return productionKnowledgeUploadFailure(errProductionKnowledgeReferenceKey), nil
	}
	name, data, err := k.read(caller, uploadID)
	if err != nil {
		return productionKnowledgeUploadFailure(err), nil
	}
	fileName, ok := productionKnowledgeUploadFileName(name)
	if !ok {
		return productionKnowledgeUploadFailure(errProductionKnowledgeUploadNameInvalid), nil
	}
	text, err := productionKnowledgeUploadText(data)
	if err != nil {
		return productionKnowledgeUploadFailure(err), nil
	}
	answer, err := k.writeReference(ctx, contextValue.ProjectPath, stableKey, fileName, text)
	if err != nil {
		return productionKnowledgeUploadFailure(err), nil
	}
	return answer, nil
}

// writeReference is the reference import's commit half. It differs from
// productionKnowledgeImports.write in exactly one respect, and deliberately: the
// document id is derived from the stable key, and the write replaces only that
// document's own rows (replacementMode 'stable-id', vector-store.ts:1207-1210), so
// a second import of the same work cannot leave a duplicate document or a
// duplicate chunk.
func (k *productionKnowledgeUploads) writeReference(ctx context.Context, projectPath, stableKey,
	fileName, text string) (map[string]any, error) {
	chunks, err := productionKnowledgeChunksOf(ctx, text)
	if err != nil {
		return nil, err
	}
	documentID := productionKnowledgeReferenceDocumentID(stableKey)
	release, err := k.imports.enter(projectPath)
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
	if existing, found := index.find(documentID); found {
		// A document under this stable id whose rows and copy are both complete is
		// a replay: answer the receipt and write nothing.
		if err := productionKnowledgeReferenceComplete(storage.storage, index, existing, text, chunks); err == nil {
			idempotent := true
			return productionKnowledgeReceipt(documentID, int64(len(chunks)), &idempotent), nil
		}
		// Anything else is a repair. The same stable id is rewritten in place -
		// never a second document, which is the whole reason the id is derived.
	}
	document, rows, err := productionKnowledgeReferenceDocument(
		productionKnowledgeRequest{Text: text, FileName: fileName}, documentID, chunks, time.Now())
	if err != nil {
		return nil, err
	}
	if err := productionKnowledgeCommitStable(ctx, storage.storage, index, document, rows); err != nil {
		return nil, err
	}
	if err := storage.verify(); err != nil {
		return nil, err
	}
	idempotent := false
	return productionKnowledgeReceipt(documentID, int64(len(chunks)), &idempotent), nil
}

// read is the seam call every name shares, with this file's bounds applied before
// the bytes are used: an unowned seam, a missing id and an id from another session
// or tab are three different symbolic answers.
func (k *productionKnowledgeUploads) read(caller transport.Caller, uploadID string) (string, []byte, error) {
	if k.uploads == nil {
		return "", nil, errProductionKnowledgeUploadUnavailable
	}
	if uploadID == "" {
		return "", nil, errProductionKnowledgeUploadRequired
	}
	owner, err := productionTabOwner(caller)
	if err != nil {
		return "", nil, errProductionKnowledgeUploadUnknown
	}
	name, data, err := k.uploads.ReadUpload(owner.SessionID, owner.ClientID, uploadID)
	if err != nil {
		return "", nil, errProductionKnowledgeUploadUnknown
	}
	if int64(len(data)) > productionKnowledgeTextMaxBytes {
		return "", nil, errProductionKnowledgeUploadTooLarge
	}
	return name, data, nil
}

// productionKnowledgeUploadText is the pre-write bound on one member: text this
// store can index, and nothing else. A binary file named .md is refused here rather
// than truncated later.
func productionKnowledgeUploadText(data []byte) (string, error) {
	if int64(len(data)) > productionKnowledgeTextMaxBytes {
		return "", errProductionKnowledgeUploadTooLarge
	}
	if !utf8.Valid(data) {
		return "", errProductionKnowledgeUploadNotText
	}
	return string(data), nil
}

// productionKnowledgeUploadFileName is the one-component name rule: a document is
// named by a file, never by a path.
func productionKnowledgeUploadFileName(name string) (string, bool) {
	if !productionPickerSafeName(name) || len(name) > productionKnowledgeUploadNameBytes {
		return "", false
	}
	if name == "." || name == ".." {
		return "", false
	}
	return name, true
}

// productionKnowledgeUploadMembers validates a folder upload as a whole: bounded in
// count and total bytes, every relative path a real member path, every text member
// readable as UTF-8, and every member a text file by the reference's own filter
// (`/\.(txt|md|markdown)$/i`, knowledge-base.ts:470-479). A folder with no text
// member is not an error: readGrantedKnowledgeFolder answers no files and the
// reference reports zero imports. It sorts, so the import order is deterministic.
func productionKnowledgeUploadMembers(files []productionPickerUploaded) ([]productionPickerUploaded, error) {
	if len(files) > productionKnowledgeUploadFolderFiles {
		return nil, errProductionKnowledgeUploadLimit
	}
	members := make([]productionPickerUploaded, 0, len(files))
	total := int64(0)
	for _, file := range files {
		if !productionPickerUploadMember(file.Relative) {
			return nil, errProductionKnowledgeUploadNameInvalid
		}
		if !productionPickerTextName(file.Relative) {
			// A non-text member is skipped, not refused: the reference imports only
			// the text files it finds (kb-controller.ts:470-479).
			continue
		}
		if int64(len(file.Data)) > productionKnowledgeTextMaxBytes {
			return nil, errProductionKnowledgeUploadTooLarge
		}
		if !utf8.Valid(file.Data) {
			// The reference reads every discovered text file before it imports
			// any (readGrantedKnowledgeFolder), so one unreadable member refuses
			// the folder instead of importing the others.
			return nil, errProductionKnowledgeUploadNotText
		}
		total += int64(len(file.Data))
		if total > productionKnowledgeUploadFolderBytes {
			return nil, errProductionKnowledgeUploadLimit
		}
		members = append(members, file)
	}
	sort.SliceStable(members, func(left, right int) bool {
		return members[left].Relative < members[right].Relative
	})
	return members, nil
}

// productionKnowledgeReferenceKey is the reference's stable-key rule
// (knowledge-base.ts:676-686): trimmed, non-empty, at most 512 bytes, and drawn
// from the one character class a key may use.
func productionKnowledgeReferenceKey(value any) (string, bool) {
	raw, ok := value.(string)
	if !ok {
		return "", false
	}
	key := strings.TrimSpace(raw)
	if key == "" || len(key) > 512 || !productionKnowledgeReferenceKeyPattern.MatchString(key) {
		return "", false
	}
	return key, true
}

// productionKnowledgeReferenceDocumentID is the reference's deterministic id:
// sha256hex("reference-import:" + stableKey) (knowledge-base.ts:688). Two imports of
// the same work land on the same document; two different works cannot collide.
func productionKnowledgeReferenceDocumentID(stableKey string) string {
	digest := sha256.Sum256([]byte("reference-import:" + stableKey))
	return hex.EncodeToString(digest[:])
}

// productionKnowledgeReferenceDocument builds one reference document row and its
// chunk rows. It is productionKnowledgeNewDocument with corpusKind 'reference':
// the same fresh UUID per chunk, the same single ISO importedAt, the same chapter
// pair derived from the file name when the caller supplied none, and the same copy
// marker as filePath.
func productionKnowledgeReferenceDocument(request productionKnowledgeRequest, documentID string,
	chunks []string, now time.Time) (productionKnowledgeDocument, []productionKnowledgeChunk, error) {
	stamp := productionKnowledgeTimestamp(now)
	chapterNumber, chapterTitle := request.ChapterNumber, request.ChapterTitle
	if chapterNumber == nil && chapterTitle == nil {
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
		CorpusKind: productionKnowledgeReferenceCorpusKind,
		Content:    request.Text,
	}
	rows := make([]productionKnowledgeChunk, 0, len(chunks))
	for position, text := range chunks {
		id, err := productionKnowledgeUUID()
		if err != nil {
			// A lane without a source of chunk identities cannot write a canonical
			// snapshot, so it says so instead of writing one without chunk ids.
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
			CorpusKind:    productionKnowledgeReferenceCorpusKind,
		})
	}
	return document, rows, nil
}

// productionKnowledgeCommitStable writes one reference document: its copy first,
// then the index, and no pruning at all. A failed index write takes the copy it
// just created back out, so a refusal leaves the store as it was.
func productionKnowledgeCommitStable(ctx context.Context, storage *os.Root, index productionKnowledgeIndex,
	document productionKnowledgeDocument, rows []productionKnowledgeChunk) error {
	if document.ID == "" || len(rows) == 0 || document.ChunkCount != int64(len(rows)) {
		return errProductionKnowledgeStorageUnsafe
	}
	created, err := productionKnowledgeWriteCopy(storage, document.ID, document.Content,
		productionKnowledgeHash(document.Content))
	if err != nil {
		return err
	}
	next := productionKnowledgeStableReplaced(index, document, rows)
	if err := productionKnowledgeWriteIndex(ctx, storage, next); err != nil {
		if created {
			_ = productionKnowledgeRemoveCopy(storage, document.ID)
		}
		return err
	}
	return nil
}

// productionKnowledgeStableReplaced is addChunks' replacementMode 'stable-id'
// (vector-store.ts:1207-1210): this document's own rows are replaced and no other
// document is touched, so an import under a stable id can never prune a sibling.
func productionKnowledgeStableReplaced(index productionKnowledgeIndex, document productionKnowledgeDocument,
	rows []productionKnowledgeChunk) productionKnowledgeIndex {
	next := productionKnowledgeIndex{
		Documents: make([]productionKnowledgeDocument, 0, len(index.Documents)+1),
		Chunks:    make([]productionKnowledgeChunk, 0, len(index.Chunks)+len(rows)),
	}
	for _, existing := range index.Documents {
		if existing.ID == document.ID {
			continue
		}
		next.Documents = append(next.Documents, existing)
	}
	for _, chunk := range index.Chunks {
		if chunk.DocID == document.ID {
			continue
		}
		next.Chunks = append(next.Chunks, chunk)
	}
	next.Documents = append(next.Documents, document)
	next.Chunks = append(next.Chunks, rows...)
	return next
}

// productionKnowledgeReferenceComplete is the replay test: the committed rows AND
// the copy must both be complete and hold exactly this text. Anything else is a
// repair, never a receipt.
func productionKnowledgeReferenceComplete(storage *os.Root, index productionKnowledgeIndex,
	document productionKnowledgeDocument, content string, chunks []string) error {
	if err := productionKnowledgeReferenceCanonical(index, document, chunks); err != nil {
		return err
	}
	copied, indexedHash, ok := productionKnowledgeCopyContent(storage, document.ID)
	if !ok || copied != content || indexedHash != productionKnowledgeHash(content) {
		return errProductionKnowledgeRepairRequired
	}
	return nil
}

// productionKnowledgeReferenceCanonical is productionKnowledgeCanonical for the
// reference corpus. It repeats the check rather than calling it because that one
// pins corpusKind to 'project-knowledge' by design
// (production_knowledge_import_linux.go:922-928); every other rule - one row per
// chunk index, no gap, one totalChunks, one corpus kind, the chunk-set hash -
// is the same.
func productionKnowledgeReferenceCanonical(index productionKnowledgeIndex,
	document productionKnowledgeDocument, chunks []string) error {
	if document.CorpusKind != productionKnowledgeReferenceCorpusKind ||
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

// productionKnowledgeCopyContent reads one knowledge copy's text and indexedHash
// (the file productionKnowledgeWriteCopy wrote). A missing, oversized, symlinked or
// unparseable copy is (no, no, false) - a repair, never a receipt.
func productionKnowledgeCopyContent(storage *os.Root, documentID string) (string, string, bool) {
	child, err := productionKnowledgeCopyRoot(storage, false)
	if err != nil || child == nil {
		return "", "", false
	}
	defer func() { _ = child.Close() }()
	file, err := child.OpenFile(productionKnowledgeCopyName(documentID),
		os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return "", "", false
	}
	info, statErr := file.Stat()
	if statErr != nil || info == nil || !info.Mode().IsRegular() ||
		info.Size() <= 0 || info.Size() > productionKnowledgeTextMaxBytes*2 {
		_ = file.Close()
		return "", "", false
	}
	body, readErr := io.ReadAll(io.LimitReader(file, productionKnowledgeTextMaxBytes*2))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return "", "", false
	}
	var copy struct {
		Content     string `json:"content"`
		IndexedHash string `json:"indexedHash"`
		Edited      bool   `json:"edited"`
		IndexDirty  bool   `json:"indexDirty"`
	}
	if err := json.Unmarshal(body, &copy); err != nil {
		return "", "", false
	}
	return copy.Content, copy.IndexedHash, true
}

// productionKnowledgeUploadFailure is this slice's one mapping layer. The import
// lane's own table answers everything it already knew; the eight refusals this file
// introduces are named here and nowhere else.
func productionKnowledgeUploadFailure(err error) map[string]any {
	switch {
	case errors.Is(err, errProductionKnowledgeUploadRequired):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadRequired)
	case errors.Is(err, errProductionKnowledgeUploadUnavailable):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadUnavailable)
	case errors.Is(err, errProductionKnowledgeUploadUnknown):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadUnknown)
	case errors.Is(err, errProductionKnowledgeUploadTooLarge):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadTooLarge)
	case errors.Is(err, errProductionKnowledgeUploadLimit):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadLimit)
	case errors.Is(err, errProductionKnowledgeUploadNotText):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadNotText)
	case errors.Is(err, errProductionKnowledgeUploadNameInvalid):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeUploadNameInvalid)
	case errors.Is(err, errProductionKnowledgeReferenceKey):
		return productionKnowledgeFailureAnswer(productionKnowledgeCodeReferenceKey)
	}
	return productionKnowledgeImportFailure(err)
}

// productionKnowledgeUploadFolderFailure is the same answer with the two keys
// kb:import-folder always carries (kb-controller.ts:264, :305), so a refused folder
// and an imported folder have one shape.
func productionKnowledgeUploadFolderFailure(err error) map[string]any {
	answer := productionKnowledgeUploadFailure(err)
	answer["importedCount"] = int64(0)
	answer["failedFiles"] = []any{}
	return answer
}

// productionKnowledgeFailureAnswer is the lane's single failure envelope: a bare
// symbolic code under `error`, never prose.
func productionKnowledgeFailureAnswer(code string) map[string]any {
	return map[string]any{"success": false, "error": code}
}

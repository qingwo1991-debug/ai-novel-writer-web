//go:build linux

package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the two author-manuscript import operations the store just
// made possible, and nothing else:
//
//	db:draft-import-finalized-batch  store.Drafts.FinalizedImportCommit   internal/store/finalized_draft_import.go:874
//	db:import-run-author-preview     store.Drafts.FinalizedImportPreview  internal/store/finalized_draft_import.go:811
//
// # The reference, channel by channel
//
//	electron/controllers/db-controller.ts:716-728
//	  register('db:draft-import-finalized-batch', (event, request, expectedProjectPath) =>
//	    assertRequiredExpectedProjectPath(getCurrentProjectPath(), expectedProjectPath)
//	    return { success: true, receipt: FinalizedDraftImportRepository.commit(currentProjectPath, request) })
//	electron/repositories/finalized-draft-import-repository.ts:391-505  commit
//	electron/repositories/finalized-draft-import-repository.ts:311-389  preview
//
//	electron/controllers/db-controller.ts:367-384
//	  register('db:import-run-author-preview', (event, inspectionId, expectedProjectPath) =>
//	    assertRequiredExpectedProjectPath(getCurrentProjectPath(), expectedProjectPath)
//	    const inspection = importInspectionStore.peek(inspectionId, event.sender.id, 'author-manuscript')
//	    return FinalizedDraftImportRepository.preview(inspection.chapters.map(...)))
//
// The wire declarations are src/shared/ipc-channels.ts:1019-1022 and :930-933, and
// the payload shapes are src/shared/finalized-draft-import.ts:1-36 and
// src/shared/author-manuscript-import.ts:1-33 - all of which
// internal/store/finalized_draft_import.go already reproduces field for field, in
// the reference's own order, so both answers are the store's own values rather
// than a re-naming DTO.
//
// # Argument order, and the one key-validation rule this file owns
//
// Both channels take exactly two wire arguments - the payload and the frozen
// project path - plus the frozen three-key session this lane appends to every
// production call, so both arities are 3:
//
//	db:draft-import-finalized-batch  (request 0, path 1, context 2)  3
//	db:import-run-author-preview     (payload 0, path 1, context 2)  3
//
// The controller enforces no request-key allowlist on either name, so an extra
// key in the request object is ignored here too, exactly as the reference ignores
// it. What this file must validate is the part JavaScript's own operators
// validated and a Go struct decode cannot: `typeof` and `Number.isInteger`. The
// store says so itself (finalized_draft_import.go:141-148): "ChapterNumber is
// int64, so `Number.isInteger` at :66 is the binder's obligation", ":73-75 - the
// title must be a string. The field cannot hold anything else here, so the binder
// owes this check, not the store", and WordCount is a *int64 "because the original
// distinguishes a missing/non-integer wordCount (:79) from a supplied one" -
// finalized_draft_import.go:157-159 adds the fingerprint rule: "A nil fingerprint
// is the original's `undefined`; the wire must not send an explicit null".
// productionFinalizedDraftImportChapters and
// productionFinalizedDraftImportRequest are exactly that binder, and nothing in
// them invents a rule the reference does not have: an empty batch, a duplicate
// chapter number, a blank body and a wrong word count are all left to the store's
// own normalizeChapters (:335-370), which answers the same code.
//
// # The admission, and where the scope comes from
//
// The scope is the frozen project epoch, never anything from the body:
// productionDurableOpen.borrowAuthorDatabase (production_durable_linux.go:313-360)
// parses the three-key context, proves the caller and the tab, refuses a project
// this lane did not open, and re-proves the live lease through the runtime before
// handing over one (*sql.DB, release) pair; the release re-verifies the permit, so
// a read commits nothing and a write adopts the identity it committed. A stale,
// foreign, closed or absent lease is refused there before a row is read, which is
// the Go side of the reference's assertRequiredExpectedProjectPath plus its
// current-project assertion (db-controller.ts:721-723, :372).
//
// Neither handler takes a project id, a lease id or a path from the payload: the
// request object has no such field in either shared type, and the one path the
// wire carries is compared against the frozen context before the borrow
// (PROJECT_CONTEXT_REQUIRED / PROJECT_CONTEXT_CHANGED), which is the pair of codes
// electron/utils/project-context.ts:12-25 throws.
//
// # The long write: one reserved connection, one BEGIN IMMEDIATE, no request context
//
// db:draft-import-finalized-batch is a write that takes minutes on a real
// manuscript - two INSERTs per chapter plus the outbox row, inside a single
// transaction. The store already owns the transaction shape:
// FinalizedImportCommit calls NewReviewRevisions(db).immediate
// (finalized_draft_import.go:900), which reserves ONE *sql.Conn, takes BEGIN
// IMMEDIATE, runs the whole body on that connection and commits it there
// (internal/store/reviewrevision.go:39-61), rolling back on a background context so
// a cancelled request can never leak an open transaction. The adapter therefore
// opens no transaction, no second connection and no per-chapter boundary of its
// own: "batch" is one atomic commit, and a refusal on chapter 7 leaves chapters 1-6
// unwritten.
//
// Two things the adapter must add on top of that:
//
//   - The write must not run on the request context.
//     productionFinalizedDraftImportCommit derives its context with
//     context.WithoutCancel, which keeps the request's values but drops its
//     cancellation, and bounds it with its own budget. The reference is a
//     synchronous better-sqlite3 transaction (`db.transaction(() => ...)()`)
//     that no disconnect can abort, so detaching is the faithful port: a browser
//     tab that navigates away during a 400-chapter import must not roll back
//     thousands of rows that already took minutes, and it does not have to be
//     trusted to come back, because the operation id makes the retry a replay
//     (:406-413) instead of a second batch.
//   - Nothing past the commit touches the request context. The handlers borrow with
//     the request context (admission IS per-request), hand the write its own, and
//     the release the borrow returned re-verifies the permit on a background
//     context with its own timeout (production_durable_linux.go:352-358). After
//     FinalizedImportCommit returns, the receipt is returned as it is, so the
//     response cannot be built on a context the commit already outlived.
//
// The preview is the opposite case and says so in code: it is a pure read
// (:801-810), so it runs on the request context and dies with the request.
//
// A closed lane never panics. A nil adapter, a nil bridge and a nil borrowed
// database each answer a symbolic code (PRODUCTION_CLOSED, or the reference's own
// PROJECT_NOT_OPEN out of the store's nil-receiver guard at :812-815), and the
// borrow's own refusals are published through the identical mapper, so no path in
// this file dereferences a handle it did not prove.
//
// # The two forms of db:import-run-author-preview
//
// The reference's first argument is an inspection id, not chapters
// (db-controller.ts:367-377). It is resolved by the main process's in-memory
// author-manuscript inspection store, which freezes the parsed bodies of one file
// selection for one tab. That owner now exists in this lane: the picker's
// importsource.Store is hoisted out of its wiring closure and reaches this file as
// productionImportRunPrepareInspectionServe (production_importrun_prepare_inspection_serve_linux.go),
// so the id form is served here - resolved through that store under the same
// caller/generation binding the picker minted it with, then previewed by the same
// store read the chapters form uses.
//
// The chapters form is unchanged and still the one a caller that already holds the
// bodies uses: FinalizedImportPreview is a pure read over whatever chapters it is
// handed (:811-821). A lane that was built without the picker - production_open's
// staging route can fail without stopping the server - keeps the chapters form and
// answers the id form with 501 IMPORT_INSPECTION_OWNER_UNAVAILABLE, which is now a
// statement about that lane and not about the backend.
//
// The id is never read as a path, a run or authority: it is compared with the
// receipt the store minted, and the store decides.
//
// # The refusals
//
// The reference throws; this lane answers the same symbolic code. The store's own
// values already carry those strings and productionFinalizedDraftImportRefusals is
// the one place that says so, so the wire code and the store value cannot drift
// apart unnoticed. The admission fence's own symbolic refusals (PRODUCTION_CLOSED,
// PROJECT_NOT_OPEN, PROJECT_SESSION_STALE, AUTHOR_WRITE_STALE,
// PROJECT_HANDLE_CLOSED) survive verbatim through the family's one arm
// (production_importrun_write_linux.go:475-495). Nothing else is mapped: a SQLite
// diagnostic, a path, a chapter body or a provider message is replaced by
// REQUEST_FAILED, the transport's own unmapped-failure code
// (internal/transport/public.go:20), which is exactly what an error no binder names
// is published as anyway (production_batch_reads_linux.go:303).
//
// The mapping runs exactly once, in productionFinalizedDraftImportFailure, and
// every helper below hands its errors back untouched: mapping twice would map the
// mapped value - a plain error carrying only the code - and publish REQUEST_FAILED
// for a refusal that has a name. The two request-shaped guards are 400s carrying
// the reference's own code, because a malformed `chapters` array is the caller's
// mistake, not a server failure; the store raises the same two codes for the parts
// only it can check, and a code is the contract either way.
//
// # The names this file does not touch
//
// db:import-run-prepare-inspection is served by
// production_importrun_prepare_inspection_serve_linux.go (it needs this file's
// preview and the picker's receipt owner, both of which it borrows from here).
// db:import-run-effect-receipt-prepare and db:import-run-effect-receipt-commit need
// a generation-source proof this repository does not have, exactly as
// production_importrun_residual_linux.go:26-90 documents them. This file binds two
// names and opens nothing else.
//
// # Wiring
//
// The binder is installed by production_open_linux.go's
// `for _, bind := range []func() error{` slice, next to the other binders that need
// nothing but open.durable; until that line exists this file is compiled and tested
// but installs nothing at startup.
const (
	// productionFinalizedDraftImportBatchName is the channel the commit serves
	// (src/shared/ipc-channels.ts:1019).
	productionFinalizedDraftImportBatchName = "db:draft-import-finalized-batch"
	// productionFinalizedDraftImportPreviewName is the channel the preview serves
	// (src/shared/ipc-channels.ts:930).
	productionFinalizedDraftImportPreviewName = "db:import-run-author-preview"
	// productionFinalizedDraftImportArity is the wire arity of both names: one
	// payload, one frozen project path and the frozen session.
	productionFinalizedDraftImportArity = 3
	// productionFinalizedDraftImportPathIndex is where the explicit path sits.
	productionFinalizedDraftImportPathIndex = 1
	// productionFinalizedDraftImportContextIndex is where the frozen three-key
	// session sits - always its own last argument, never inside the payload.
	productionFinalizedDraftImportContextIndex = 2
	// productionFinalizedDraftImportOperationCode is requireNonEmptyOperationId's
	// refusal (finalized-draft-import-repository.ts:52-58). The binder answers it
	// for a missing or non-string id, where the reference's `.trim()` throws a
	// TypeError: the nearest symbolic code is the one its own guard raises.
	productionFinalizedDraftImportOperationCode = "FINALIZED_IMPORT_OPERATION_INVALID"
	// productionFinalizedDraftImportChaptersCode is normalizeChapters' one refusal
	// (:60-90), answered here for a payload that is not an array of chapter objects.
	productionFinalizedDraftImportChaptersCode = "FINALIZED_IMPORT_CHAPTERS_INVALID"
	// productionFinalizedDraftImportFailedCode is the transport's unmapped-failure
	// code (internal/transport/public.go:20): a SQLite sentence, a path or a body
	// never reaches the browser.
	productionFinalizedDraftImportFailedCode = "REQUEST_FAILED"
	// productionFinalizedDraftImportInspectionCode is the reference's own argument
	// form for the preview, answered when this lane has no receipt owner injected
	// (Bind installs the preview either way; only the id form needs the owner).
	productionFinalizedDraftImportInspectionCode = "IMPORT_INSPECTION_OWNER_UNAVAILABLE"
	// productionFinalizedDraftImportMinSafeInteger is -Number.MAX_SAFE_INTEGER, the
	// bound productionImportRunWriteSafeInteger accepts.
	productionFinalizedDraftImportMinSafeInteger = -9007199254740991
	// productionFinalizedDraftImportWriteBudget bounds one batch. The reference has
	// no budget because better-sqlite3 blocks the main process; a server-side write
	// needs one so a wedged transaction cannot pin the reserved connection forever.
	// It is deliberately generous: a full manuscript on a slow disk still commits.
	productionFinalizedDraftImportWriteBudget = 15 * time.Minute
)

// productionFinalizedDraftImportRefusals is the store's refusal set with the code
// each one is published under. Every code is the value's own string
// (internal/store/finalized_draft_import.go:116-134); the table exists so the
// mapping is stated once, next to the line that raises it.
var productionFinalizedDraftImportRefusals = [...]struct {
	refusal error
	code    string
}{
	// :55 - an empty or over-long operation id.
	{store.ErrFinalizedImportOperationInvalid, "FINALIZED_IMPORT_OPERATION_INVALID"},
	// :62, :67, :70, :74, :77, :80 - the chapter payload is not importable.
	{store.ErrFinalizedImportChaptersInvalid, "FINALIZED_IMPORT_CHAPTERS_INVALID"},
	// :216, :219, :229, :245, :255 - the stored receipt does not describe itself.
	{store.ErrFinalizedImportReceiptCorrupt, "FINALIZED_IMPORT_RECEIPT_CORRUPT"},
	// :278 - a committed draft's frozen facts moved since the receipt.
	{store.ErrFinalizedImportFactsChanged, "FINALIZED_IMPORT_FACTS_CHANGED"},
	// :408 - the operation id is already bound to a different payload.
	{store.ErrFinalizedImportOperationConflict, "FINALIZED_IMPORT_OPERATION_CONFLICT"},
	// :419, :425 - the manifest or authority the caller inspected has moved.
	{store.ErrFinalizedImportPreviewStale, "FINALIZED_IMPORT_PREVIEW_STALE"},
	// :429 - the authority is current but the chapters would not append.
	{store.ErrFinalizedImportAuthorityConflict, "FINALIZED_IMPORT_AUTHORITY_CONFLICT"},
	// :437 - a chapter number this batch targets is already finalized.
	{store.ErrFinalizedImportChapterAlreadyFinalized, "FINALIZED_IMPORT_CHAPTER_ALREADY_FINALIZED"},
	// :144, :239, :297, :393 - the reference's PROJECT_NOT_OPEN, which this package
	// already exports as ErrProjectNotOpen (internal/store/projectcore.go:12).
	{store.ErrProjectNotOpen, "PROJECT_NOT_OPEN"},
}

// productionFinalizedDraftImports serves both names over the one durable lane.
// inspections is the picker's receipt owner, injected when the lane has one; a lane
// without it keeps the chapters form and refuses the id form with
// IMPORT_INSPECTION_OWNER_UNAVAILABLE.
type productionFinalizedDraftImports struct {
	durable     *productionDurableOpen
	inspections *productionImportRunPrepareInspectionServe
}

// newProductionFinalizedDraftImports wraps the lane's durable bridge, the only
// object that can name the author database. It resolves nothing and opens nothing
// at construction time. The receipt owner is optional so the chapters form of the
// preview keeps working in a lane that never built the import picker.
func newProductionFinalizedDraftImports(durable *productionDurableOpen,
	inspections ...*productionImportRunPrepareInspectionServe) *productionFinalizedDraftImports {
	imports := &productionFinalizedDraftImports{durable: durable}
	if len(inspections) > 0 {
		imports.inspections = inspections[0]
	}
	return imports
}

// Bind installs the two names, one server.Bind per name. A nil adapter or a nil
// bridge installs nothing, so both names keep failing closed at 501
// GO_OPERATION_NOT_MIGRATED instead of being published as handlers that can only
// refuse. Bind itself refuses an uninventoried name (OPERATION_NOT_EXPOSED) and an
// already bound one (OPERATION_ALREADY_BOUND), so a name this file should not
// publish, or one a sibling published first, is a startup error rather than dead
// code.
func (r *productionFinalizedDraftImports) Bind(server *transport.Server) error {
	if r == nil || r.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	// Both names are installed. The preview answers both of its argument forms: the
	// chapters a caller already holds, and the reference's inspection id resolved
	// through the picker's receipt owner when the lane injected one. Without that
	// owner the id form reaches IMPORT_INSPECTION_OWNER_UNAVAILABLE, which names the
	// absent capability instead of the name's old GO_OPERATION_NOT_MIGRATED.
	handlers := [...]struct {
		name    string
		handler transport.Operation
	}{
		{name: productionFinalizedDraftImportBatchName, handler: r.batch},
		{name: productionFinalizedDraftImportPreviewName, handler: r.preview},
	}
	for _, entry := range handlers {
		if err := server.Bind(entry.name, entry.handler); err != nil {
			return err
		}
	}
	return nil
}

// batch is db:draft-import-finalized-batch. The request is validated before the
// borrow, the borrow before the write, and the write is the store's single
// transaction; the answer is the reference's envelope (db-controller.ts:724-727).
func (r *productionFinalizedDraftImports) batch(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	expected, err := productionFinalizedDraftImportAdmit(args)
	if err != nil {
		return nil, err
	}
	// The id and the payload are validated before the transaction, so a malformed
	// batch never even takes the write lock (finalized_draft_import.go:879-880).
	request, err := productionFinalizedDraftImportRequest(args[0])
	if err != nil {
		return nil, err
	}
	db, release, err := r.borrow(ctx, caller, expected)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	if db == nil {
		// The bridge never returns a half-open borrow; if it ever does the lane
		// fails closed rather than writing a database nobody admitted.
		return nil, productionFinalizedDraftImportFailure(errors.New("PRODUCTION_CLOSED"))
	}
	receipt, err := productionFinalizedDraftImportCommit(ctx, db, request)
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	return map[string]any{"success": true, "receipt": receipt}, nil
}

// preview is db:import-run-author-preview: the store's read, answered as itself
// (src/shared/author-manuscript-import.ts:20-33), never wrapped in an envelope,
// because the channel's declared return is the preview and the renderer reads its
// fields directly (src/components/dialogs/ImportNovelDialog.tsx:134-154).
func (r *productionFinalizedDraftImports) preview(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	expected, err := productionFinalizedDraftImportAdmit(args)
	if err != nil {
		return nil, err
	}
	// The reference's argument is an inspection id (db-controller.ts:367-377). It is
	// resolved by the shared receipt owner and previewed by the same store read the
	// chapters form below uses, so the two forms cannot disagree about a
	// classification, a gap or a fingerprint.
	if inspectionID, isInspection := args[0].(string); isInspection {
		if r.inspections == nil {
			// A lane built without the picker: the chapters form still works, and the
			// id form names the absent owner instead of pretending the id is wrong.
			return nil, security.NewHTTPError(http.StatusNotImplemented,
				productionFinalizedDraftImportInspectionCode)
		}
		return r.inspections.previewFromInspection(ctx, caller, expected, inspectionID)
	}
	chapters, err := productionFinalizedDraftImportPreviewChapters(args[0])
	if err != nil {
		return nil, err
	}
	db, release, err := r.borrow(ctx, caller, expected)
	if release != nil {
		defer release()
	}
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	if db == nil {
		return nil, productionFinalizedDraftImportFailure(errors.New("PRODUCTION_CLOSED"))
	}
	preview, err := productionFinalizedDraftImportPreview(ctx, db, chapters)
	if err != nil {
		return nil, productionFinalizedDraftImportFailure(err)
	}
	return preview, nil
}

// borrow is this file's single admission: the same borrowAuthorDatabase fence every
// other production read and write uses, with a refusal that names the absent or
// closed lane instead of dereferencing a nil bridge.
func (r *productionFinalizedDraftImports) borrow(ctx context.Context, caller transport.Caller, expected any) (*sql.DB, func(), error) {
	if r == nil || r.durable == nil {
		return nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	return r.durable.borrowAuthorDatabase(ctx, caller, expected)
}

// productionFinalizedDraftImportAdmit is the arity, the frozen three-key session
// and the expected path, in that order: a stale lease or a path in the wrong
// position is a malformed request, never authority. The three codes are the
// controller's and the path assertion's own
// (electron/utils/project-context.ts:12-25).
func productionFinalizedDraftImportAdmit(args []any) (map[string]any, error) {
	if len(args) != productionFinalizedDraftImportArity {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	expected, err := productionImportRunWriteExpectedContext(args, productionFinalizedDraftImportContextIndex)
	if err != nil {
		return nil, err
	}
	path, ok := args[productionFinalizedDraftImportPathIndex].(string)
	if !ok || path == "" || !utf8.ValidString(path) {
		return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != expected["projectPath"] {
		return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	return expected, nil
}

// productionFinalizedDraftImportGuard is one of the reference's request guards as
// a client error: the code the browser must see, refused with 400 the way this
// lane's other request-shaped guards are. It is an HTTPError, so the handler
// returns it unchanged rather than running it through the refusal mapper, which
// knows nothing about it and would publish REQUEST_FAILED instead.
func productionFinalizedDraftImportGuard(code string) error {
	return security.NewHTTPError(http.StatusBadRequest, code)
}

// productionFinalizedDraftImportPreviewChapters is the preview's chapters form: the
// frozen chapters the caller already holds. The reference's string form is resolved
// before this point (preview), so a value that reaches here is never an id.
func productionFinalizedDraftImportPreviewChapters(value any) ([]store.FinalizedDraftImportChapter, error) {
	object, ok := value.(map[string]any)
	if !ok {
		// An author-manuscript payload is an object with a chapters array; anything
		// else has no chapters, which is the reference's own chapters guard.
		return nil, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
	}
	return productionFinalizedDraftImportChapters(object["chapters"])
}

// productionFinalizedDraftImportRequest is FinalizedDraftImportRequest
// (src/shared/finalized-draft-import.ts:8-17) bound from JSON, in the reference's
// own validation order: the operation id (:396), then the chapters (:397), then the
// three expectations, which the reference only ever compares later (:415-431).
func productionFinalizedDraftImportRequest(value any) (store.FinalizedDraftImportRequest, error) {
	object, ok := value.(map[string]any)
	if !ok {
		// `request.chapters` of a non-object is a TypeError in the reference; the
		// nearest symbolic code is its own chapters guard.
		return store.FinalizedDraftImportRequest{}, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
	}
	operationID, ok := object["operationId"].(string)
	if !ok || !utf8.ValidString(operationID) {
		return store.FinalizedDraftImportRequest{}, productionFinalizedDraftImportGuard(productionFinalizedDraftImportOperationCode)
	}
	chapters, err := productionFinalizedDraftImportChapters(object["chapters"])
	if err != nil {
		return store.FinalizedDraftImportRequest{}, err
	}
	authority, err := productionFinalizedDraftImportFingerprint(object, "expectedAuthorityFingerprint")
	if err != nil {
		return store.FinalizedDraftImportRequest{}, err
	}
	manifest, err := productionFinalizedDraftImportFingerprint(object, "expectedManifestFingerprint")
	if err != nil {
		return store.FinalizedDraftImportRequest{}, err
	}
	commit, err := productionFinalizedDraftImportFingerprint(object, "expectedCommitManifestFingerprint")
	if err != nil {
		return store.FinalizedDraftImportRequest{}, err
	}
	return store.FinalizedDraftImportRequest{OperationID: operationID,
		ExpectedAuthorityFingerprint:      authority,
		ExpectedManifestFingerprint:       manifest,
		ExpectedCommitManifestFingerprint: commit,
		Chapters:                          chapters}, nil
}

// productionFinalizedDraftImportFingerprint keeps the reference's undefined/null
// distinction: an absent key is `undefined` and forbids nothing, while a key the
// wire sent as null or as a non-string is refused instead of being read as absent -
// the original compares an explicit null as a value and would call it stale, and the
// store states this wire must not send one (finalized_draft_import.go:157-159).
func productionFinalizedDraftImportFingerprint(object map[string]any, key string) (*string, error) {
	raw, present := object[key]
	if !present {
		return nil, nil
	}
	value, ok := raw.(string)
	if !ok || !utf8.ValidString(value) {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	fingerprint := value
	return &fingerprint, nil
}

// productionFinalizedDraftImportChapters projects the wire's chapters array into
// the store's own type, doing only what a Go decode cannot: `typeof` and
// Number.isInteger (finalized_draft_import.go:141-154). Everything the array means -
// emptiness, a duplicate chapter number, a number below one, a blank body, a count
// that is not the body's own - is left to the store's normalizeChapters, which
// applies the reference's order and answers the one chapters code for all of them.
func productionFinalizedDraftImportChapters(value any) ([]store.FinalizedDraftImportChapter, error) {
	items, ok := value.([]any)
	if !ok {
		// :61-63 - `!Array.isArray(chapters) || chapters.length === 0`. An absent
		// key is a nil here and takes the same guard.
		return nil, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
	}
	chapters := make([]store.FinalizedDraftImportChapter, 0, len(items))
	for _, item := range items {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
		}
		// :66 - Number.isInteger(chapterNumber) && chapterNumber >= 1.
		number, ok := productionImportRunWriteSafeInteger(object["chapterNumber"], 1)
		if !ok {
			return nil, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
		}
		// :73-75 - the title must be a string; the store cannot check that.
		title, ok := object["title"].(string)
		if !ok || !utf8.ValidString(title) {
			return nil, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
		}
		// :76 - the body must be a string. Blankness is the store's own check (:76-78).
		content, ok := object["content"].(string)
		if !ok || !utf8.ValidString(content) {
			return nil, productionFinalizedDraftImportGuard(productionFinalizedDraftImportChaptersCode)
		}
		// :79 - a missing or non-integer count stays nil, and the store refuses it
		// after its own integer comparison, so the code is the same either way.
		var wordCount *int64
		if raw, present := object["wordCount"]; present {
			if count, ok := productionImportRunWriteSafeInteger(raw, productionFinalizedDraftImportMinSafeInteger); ok {
				wordCount = &count
			}
		}
		chapters = append(chapters, store.FinalizedDraftImportChapter{ChapterNumber: number,
			Title: title, Content: content, WordCount: wordCount})
	}
	return chapters, nil
}

// productionFinalizedDraftImportCommit is db:draft-import-finalized-batch's whole
// write. The store opens the reserved connection, the single BEGIN IMMEDIATE and the
// commit (finalized_draft_import.go:900-1052); this call adds exactly one thing the
// reference got for free from better-sqlite3: a transaction the request's
// cancellation cannot abort.
//
// context.WithoutCancel keeps the request's values and drops its cancellation, so a
// tab that goes away mid-batch cannot roll back rows that took minutes; the budget
// then bounds the detached write so a wedged commit cannot hold the connection
// forever. The write is safe to detach because the operation id makes the retry a
// replay (:406-413) - the caller does not have to have been listening when it
// committed, it only has to ask again.
func productionFinalizedDraftImportCommit(ctx context.Context, db *sql.DB, request store.FinalizedDraftImportRequest) (*store.FinalizedDraftImportReceipt, error) {
	if ctx == nil || db == nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), productionFinalizedDraftImportWriteBudget)
	defer cancel()
	return store.NewDrafts(db).FinalizedImportCommit(write, request)
}

// productionFinalizedDraftImportPreview is db:import-run-author-preview's whole
// read. It stays on the request context deliberately: a pure read that nobody is
// waiting for any more has no reason to finish, and it writes nothing a detach
// could rescue (finalized_draft_import.go:801-810).
func productionFinalizedDraftImportPreview(ctx context.Context, db *sql.DB, chapters []store.FinalizedDraftImportChapter) (*store.AuthorManuscriptImportPreview, error) {
	if ctx == nil || db == nil {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	return store.NewDrafts(db).FinalizedImportPreview(ctx, chapters)
}

// productionFinalizedDraftImportCode is the reference's symbolic code for a store
// refusal, or "" for anything this lane does not name.
func productionFinalizedDraftImportCode(err error) string {
	for _, entry := range productionFinalizedDraftImportRefusals {
		if errors.Is(err, entry.refusal) {
			return entry.code
		}
	}
	return ""
}

// productionFinalizedDraftImportFailure is the refusal the browser receives: the
// transport's own error unchanged (which covers the request guards above), the
// store refusal's symbolic code, or the transport's unmapped-failure code. Never a
// SQLite message, a path, a chapter body or a provider sentence. This is the only
// place errors.Is runs, so no refusal can be mapped twice.
func productionFinalizedDraftImportFailure(err error) error {
	if err == nil {
		return nil
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if code := productionFinalizedDraftImportCode(err); code != "" {
		return errors.New(code)
	}
	// The admission fence refuses with symbolic codes of its own
	// (PRODUCTION_CLOSED, PROJECT_NOT_OPEN, PROJECT_SESSION_STALE,
	// AUTHOR_WRITE_STALE, PROJECT_HANDLE_CLOSED), and those are the codes the
	// reference's caller must see. This is the family's own arm
	// (production_importrun_write_linux.go:475-495), reused rather than
	// re-derived, so the two binders cannot disagree about what survives.
	if productionImportRunWriteSymbolicCode.MatchString(err.Error()) {
		return errors.New(err.Error())
	}
	return errors.New(productionFinalizedDraftImportFailedCode)
}

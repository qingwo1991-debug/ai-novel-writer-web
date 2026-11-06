//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"net/http"
	"os"
	"path/filepath"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file serves the four `chapter:*` operations of the finalized-chapter
// deletion family (web/operations.json:62-65) over the outbox state machine in
// internal/store/finalized_chapter_deletion.go: `delete-finalized`,
// `retry-deletion`, `confirm-legacy-knowledge-absent` and
// `list-incomplete-deletions`. It installs those four names and nothing else.
//
// # The reference
//
//	electron/controllers/chapter-lifecycle-controller.ts:36-76
//	  pops the frozen session, asserts it, appends the lease's rootPath, and then
//	  assertRequiredExpectedProjectPath(projectRoot, expectedProjectPath)
//	  (electron/utils/project-context.ts:21-30)
//	electron/services/chapter-deletion-service.ts:68-190
//	  delete, confirmLegacyKnowledgeAbsent, retry, listIncomplete and resume
//	src/shared/chapter-deletion.ts:10-37
//	  ChapterDeletionOperation (19 fields) and ChapterDeletionResult
//
// The answer of the three mutations is ChapterDeletionResult, and the record
// inside it is the store's own ChapterDeletionOperation - field for field and in
// the reference's order (src/shared/chapter-deletion.ts:10-30) - so it is returned
// as it is rather than re-named into a DTO. `list-incomplete-deletions` answers
// { success, operations }: an empty array for a project with no deletions, never
// null, because the renderer does `result.operations ?? []`
// (src/components/panels/sidebar/use-incomplete-deletions.ts:29).
//
// # Scope, and why the body cannot name it
//
// The store is built over the database borrowAuthorDatabase hands back for the
// frozen three-key session (production_durable_linux.go:313-357), so the admitted
// project and the live lease are the scope and nothing in the request body is:
// `draftId` and `chapterNumber` only select the fact to delete, and `operationId`
// only selects a row this project's outbox already holds. The wire's own
// `expectedProjectPath` is compared against the admitted path and nothing else
// (:38, :82): a caller that names another project is PROJECT_CONTEXT_CHANGED,
// exactly like internal/transport/drafts.go:87.
//
// # The two things the store left to this adapter
//
// The store decides and records; it removes no file and no knowledge document
// (finalized_chapter_deletion.go:11-21). This file is that cleaner, and it reports
// one verdict per projection through MarkProjection (:161-190):
//
//   - the published manuscript. `removePublishedManuscript`
//     (electron/services/manuscript-publisher.ts:65-74) resolves the frozen
//     target name to a DIRECT CHILD of the project root - a name that is not its
//     own base name is MANUSCRIPT_STORED_TARGET_INVALID (:88-94) - treats a
//     missing file as already cleaned, and unlinks it otherwise. This lane can
//     own that: it holds the exclusive writer lease and the admitted project root,
//     and the removal is one unlink of one verified regular child. Doing it is the
//     whole point: deleting the author's chapter row while leaving the manuscript
//     on disk is the lie this operation exists to avoid.
//   - the managed knowledge-base document. `removeKnowledgeDocument`
//     (:27-35) goes through knowledgeBaseLoader and the native knowledge base,
//     which this lane has no writer for - internal/knowledgequery is a read-only
//     View and no package under internal/ removes a document. So the knowledge
//     projection is LEFT PENDING, not marked completed and not marked failed. A
//     'failed' verdict would claim a cleaner ran and threw; nothing ran and this
//     lane structurally cannot run it. 'pending' is the column's own meaning of
//     "still owed" (finalized_chapter_deletion.go:38-44), it keeps the operation in
//     list-incomplete-deletions (:380-387) and in the retry affordance the
//     renderer shows for every non-completed operation
//     (use-incomplete-deletions.ts:47-62), and it is what lets the lane that does
//     own the knowledge base finish the job. The cost is stated rather than
//     hidden: such a delete answers committed:true, success:false and NO error
//     code, because Refusal() names only 'failed' and the legacy guard
//     (finalized_chapter_deletion.go:223-234), and the author sees the operation
//     in the incomplete list with its knowledge projection still pending.
//
// # The portable-runtime freeze
//
// readPortableRuntimeFreeze (electron/services/portable-runtime-freeze.ts:46-58)
// is `CHAPTER_DELETE_LEGACY_OPERATION_FROZEN` for one operation
// (chapter-deletion-service.ts:54-57, taken at :78, :113, :126 and :142) and the
// filter listIncomplete applies (:132-135). It lives here:
// productionChapterDeletionReadFreeze below reads the two installed sidecars at
// the project boundary and parses the document with archiveformat.ParseRuntimeFreeze,
// the shipped port of the same codec (:65-106), so a frozen operation is refused
// with the reference's own code and a frozen row is filtered out of the list.
//
// It is inert for every admitted project, and that is a proof, not a hope: every
// request of this lane re-runs projectfs.Project.VerifyNoPortableAuthority
// (internal/authorruntime/read_linux.go:138, internal/projectcatalog/catalog_linux.go:228),
// which refuses ANY presence at the installed runtime-freeze name before the
// request is served, so a project carrying one cannot reach this binder. The read
// still happens: if a marker appeared after admission, it is honoured instead of
// being assumed away, and a document that does not parse is refused with
// PORTABLE_RUNTIME_FREEZE_INVALID rather than treated as empty.
//
// # Envelope
//
// A refusal of the state machine (a thrown symbolic code in the reference, caught
// at chapter-lifecycle-controller.ts:26-33) is answered HTTP 200 with
// success:false, committed:false and the symbolic code, because that is what the
// renderer reads: src/components/panels/sidebar/finalized-chapter-deletion.ts:186-193
// inspects result.operation.legacyKnowledgeAuthorization even when success is
// false, and :206-215 reads result.error. A transport-level refusal (arity, the
// frozen session, the expected path, a stale or foreign lease) stays an HTTP
// error, like every other binder of this lane. The one conversion site for the
// former is productionChapterDeletionCode; anything it does not name is answered
// with CHAPTER_DELETE_REQUEST_FAILED, never with a SQLite message, a path or a
// draft body.
const (
	// The four bound names (web/operations.json:62-65).
	productionChapterDeletionDeleteName  = "chapter:delete-finalized"
	productionChapterDeletionRetryName   = "chapter:retry-deletion"
	productionChapterDeletionConfirmName = "chapter:confirm-legacy-knowledge-absent"
	productionChapterDeletionListName    = "chapter:list-incomplete-deletions"

	// Wire arities: the arguments of src/shared/ipc-channels.ts:1274-1290 plus the
	// frozen three-key project session, which src/services/ipc-client.ts:53-68
	// appends to every production call.
	//
	//	'chapter:delete-finalized'                  [request, expectedProjectPath] 3
	//	'chapter:retry-deletion'                    [operationId, expectedProjectPath] 3
	//	'chapter:confirm-legacy-knowledge-absent'   [operationId, expectedProjectPath] 3
	//	'chapter:list-incomplete-deletions'         [expectedProjectPath] 2
	productionChapterDeletionDeleteArity  = 3
	productionChapterDeletionRetryArity   = 3
	productionChapterDeletionConfirmArity = 3
	productionChapterDeletionListArity    = 2

	// productionChapterDeletionCodeFrozen is frozenLegacyOperation's answer
	// (chapter-deletion-service.ts:54-57).
	productionChapterDeletionCodeFrozen = "CHAPTER_DELETE_LEGACY_OPERATION_FROZEN"
	// productionChapterDeletionCodeFreezeInvalid is the codec's refusal
	// (portable-runtime-freeze.ts:29-32).
	productionChapterDeletionCodeFreezeInvalid = "PORTABLE_RUNTIME_FREEZE_INVALID"
	// productionChapterDeletionCodeRequestFailed is this lane's unmapped-failure
	// code. The reference publishes error.message here (:26-33), which for a SQLite
	// failure would be a driver sentence and a path; this lane answers a symbolic
	// code instead.
	productionChapterDeletionCodeRequestFailed = "CHAPTER_DELETE_REQUEST_FAILED"
	// productionChapterDeletionCodeTargetInvalid is MANUSCRIPT_STORED_TARGET_INVALID
	// (manuscript-publisher.ts:88-92): the frozen receipt does not name a legal
	// direct child of the project root.
	productionChapterDeletionCodeTargetInvalid = "MANUSCRIPT_STORED_TARGET_INVALID"
	// productionChapterDeletionCodeManuscriptFailed is the per-projection reason
	// recorded when the unlink itself failed. The reference stores the filesystem
	// message (:154-159), which carries the absolute path; this lane stores a
	// symbolic code because neither the author's database nor the browser may
	// receive a host path.
	productionChapterDeletionCodeManuscriptFailed = "MANUSCRIPT_DELETE_FAILED"

	// productionChapterDeletionTable is the freeze table name of this family
	// (portable-runtime-freeze.ts:37, chapter-deletion-service.ts:55).
	productionChapterDeletionTable = "chapter_deletion_operations"
	// The two installed sidecars (portable-runtime-freeze.ts:17-18), mirrored from
	// the unexported names in internal/projectfs/authority.go:20-21.
	productionChapterDeletionFreezeName       = "portable-runtime-freeze.json"
	productionChapterDeletionTransferName     = "portable-transfer-authority.json"
	productionChapterDeletionSafeIntegerLimit = 9007199254740991
)

// productionChapterDeletionErrTargetInvalid is the refusal of an unusable frozen
// target name; it is mapped to its own projection message, never to a wire code.
var productionChapterDeletionErrTargetInvalid = errors.New(productionChapterDeletionCodeTargetInvalid)

// productionChapterDeletionErrFreezeInvalid is the freeze codec's refusal, named
// once so productionChapterDeletionCode can map it like a store refusal.
var productionChapterDeletionErrFreezeInvalid = errors.New(productionChapterDeletionCodeFreezeInvalid)

// productionChapterDeletionResult is ChapterDeletionResult
// (src/shared/chapter-deletion.ts:32-37). Operation is the store's own record, so
// the wire object and the database row cannot drift apart.
type productionChapterDeletionResult struct {
	Success   bool                              `json:"success"`
	Committed bool                              `json:"committed"`
	Operation *store.ChapterDeletionOperation   `json:"operation,omitempty"`
	Error     string                            `json:"error,omitempty"`
}

// productionChapterDeletionListResult is the list channel's answer
// (src/shared/ipc-channels.ts:1287-1290). Operations is never nil: the reference
// answers an empty array, not null.
type productionChapterDeletionListResult struct {
	Success    bool                              `json:"success"`
	Operations []store.ChapterDeletionOperation  `json:"operations"`
	Error      string                            `json:"error,omitempty"`
}

// productionChapterDeletionRequest is DeleteFinalizedChapterRequest
// (src/shared/chapter-deletion.ts:5-8). It carries no authority: the scope is the
// admitted project and lease, and these two numbers only select the fact.
type productionChapterDeletionRequest struct {
	DraftID       int64
	ChapterNumber int64
}

// productionChapterDeletions serves the four names over one durable lane.
type productionChapterDeletions struct {
	durable *productionDurableOpen
}

// newProductionChapterDeletions wraps the lane's durable open. A nil open installs
// nothing, so the four names keep failing closed at 501 GO_OPERATION_NOT_MIGRATED
// instead of being published as handlers that could only refuse.
func newProductionChapterDeletions(durable *productionDurableOpen) *productionChapterDeletions {
	return &productionChapterDeletions{durable: durable}
}

// Bind installs the four names.
func (d *productionChapterDeletions) Bind(server *transport.Server) error {
	if d == nil || d.durable == nil {
		return nil
	}
	if server == nil {
		return errors.New("PRODUCTION_CHAPTER_DELETION_BINDER_INVALID")
	}
	if err := server.Bind(productionChapterDeletionDeleteName, d.deleteOperation()); err != nil {
		return err
	}
	if err := server.Bind(productionChapterDeletionRetryName, d.retryOperation()); err != nil {
		return err
	}
	if err := server.Bind(productionChapterDeletionConfirmName, d.confirmOperation()); err != nil {
		return err
	}
	return server.Bind(productionChapterDeletionListName, d.listOperation())
}

// deleteOperation is chapter:delete-finalized.
func (d *productionChapterDeletions) deleteOperation() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if len(args) != productionChapterDeletionDeleteArity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		// The request is projected before the borrow, so a malformed call never
		// opens a database this lane would have to fence afterwards.
		request, err := productionChapterDeletionRequestProjection(args[0])
		if err != nil {
			return nil, err
		}
		db, root, release, err := d.admit(ctx, caller, args, productionChapterDeletionDeleteArity)
		if err != nil {
			return nil, err
		}
		defer release()
		answer, err := d.delete(ctx, db, root, request)
		if err != nil {
			return productionChapterDeletionFailure(err), nil
		}
		return answer, nil
	}
}

// retryOperation is chapter:retry-deletion. Its wire argument is one operation id
// (src/shared/ipc-channels.ts:1279-1282).
func (d *productionChapterDeletions) retryOperation() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if len(args) != productionChapterDeletionRetryArity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		operationID, err := productionChapterDeletionOperationID(args[0])
		if err != nil {
			return nil, err
		}
		db, root, release, err := d.admit(ctx, caller, args, productionChapterDeletionRetryArity)
		if err != nil {
			return nil, err
		}
		defer release()
		answer, err := d.retry(ctx, db, root, operationID)
		if err != nil {
			return productionChapterDeletionFailure(err), nil
		}
		return answer, nil
	}
}

// confirmOperation is chapter:confirm-legacy-knowledge-absent, which takes the same
// single operation id (src/shared/ipc-channels.ts:1283-1286).
func (d *productionChapterDeletions) confirmOperation() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		if len(args) != productionChapterDeletionConfirmArity {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		operationID, err := productionChapterDeletionOperationID(args[0])
		if err != nil {
			return nil, err
		}
		db, root, release, err := d.admit(ctx, caller, args, productionChapterDeletionConfirmArity)
		if err != nil {
			return nil, err
		}
		defer release()
		answer, err := d.confirm(ctx, db, root, operationID)
		if err != nil {
			return productionChapterDeletionFailure(err), nil
		}
		return answer, nil
	}
}

// listOperation is chapter:list-incomplete-deletions: one expected project path
// and nothing else (src/shared/ipc-channels.ts:1287-1290).
func (d *productionChapterDeletions) listOperation() transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		db, root, release, err := d.admit(ctx, caller, args, productionChapterDeletionListArity)
		if err != nil {
			return nil, err
		}
		defer release()
		answer, err := d.list(ctx, db, root)
		if err != nil {
			return productionChapterDeletionListFailure(err), nil
		}
		return answer, nil
	}
}

// admit is the shared fence of the four names: arity, the frozen three-key
// session, the wire's expected project path against the admitted one, the
// database borrow and the project root. It returns the caller's release, which
// closes the root and hands the borrow back, so a handler never leaks either.
func (d *productionChapterDeletions) admit(ctx context.Context, caller transport.Caller, args []any, arity int) (*sql.DB, *os.Root, func(), error) {
	if d == nil || d.durable == nil {
		return nil, nil, nil, errors.New("PRODUCTION_CLOSED")
	}
	if len(args) != arity || arity < 2 {
		return nil, nil, nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	admitted, err := projectsession.ParseContext(args[arity-1])
	if err != nil {
		return nil, nil, nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	}
	if err := productionChapterDeletionExpectedPath(args[arity-2], admitted.ProjectPath); err != nil {
		return nil, nil, nil, err
	}
	if ctx == nil {
		return nil, nil, nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	db, release, err := d.durable.borrowAuthorDatabase(ctx, caller, args[arity-1])
	if err != nil {
		return nil, nil, nil, err
	}
	if db == nil || release == nil {
		return nil, nil, nil, errors.New(productionChapterDeletionCodeRequestFailed)
	}
	// The root the lease proves, opened once per request. os.Root confines every
	// name below to this directory, and the manuscript cleaner still checks that
	// the frozen name is a direct child before it removes anything.
	root, err := os.OpenRoot(admitted.ProjectPath)
	if err != nil {
		release()
		return nil, nil, nil, errors.New(productionChapterDeletionCodeRequestFailed)
	}
	return db, root, func() {
		_ = root.Close()
		release()
	}, nil
}

// delete is ChapterDeletionService.delete (chapter-deletion-service.ts:68-97).
func (d *productionChapterDeletions) delete(ctx context.Context, db *sql.DB, root *os.Root, request productionChapterDeletionRequest) (productionChapterDeletionResult, error) {
	deletions := store.NewChapterDeletions(db, nil)
	existing, err := deletions.GetByDraftID(ctx, request.DraftID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	if existing != nil {
		// :73-76 - the receipt exists but names another chapter of this draft.
		if existing.DraftID != request.DraftID || existing.ChapterNumber != request.ChapterNumber {
			return productionChapterDeletionResult{}, store.ErrChapterDeletionRequestMismatch
		}
		return d.resume(ctx, deletions, root, existing.OperationID)
	}
	// :96 - the guard is computed before Begin, because it is a read.
	required, err := deletions.RequiresLegacyKnowledgeAuthorization(ctx, request.DraftID, request.ChapterNumber)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	begun, err := deletions.Begin(ctx, store.ChapterDeletionBegin{
		DraftID:                              request.DraftID,
		ChapterNumber:                        request.ChapterNumber,
		LegacyKnowledgeAuthorizationRequired: required,
	})
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	if required || begun.LegacyKnowledgeAuthorization == store.ChapterDeletionLegacyKnowledgeRequired {
		// :91-93 - begin stopped at the guard, so no fact was deleted and nothing
		// is cleaned; the author has to confirm the legacy state first.
		return productionChapterDeletionLegacyRequired(begun), nil
	}
	return d.resume(ctx, deletions, root, begun.OperationID)
}

// retry is ChapterDeletionService.retry (:107-120). Its own prefix - get, a
// missing operation is CHAPTER_DELETE_OPERATION_NOT_FOUND, the freeze, the legacy
// guard - is exactly the prefix resume begins with, so it is not written twice.
func (d *productionChapterDeletions) retry(ctx context.Context, db *sql.DB, root *os.Root, operationID string) (productionChapterDeletionResult, error) {
	return d.resume(ctx, store.NewChapterDeletions(db, nil), root, operationID)
}

// confirm is ChapterDeletionService.confirmLegacyKnowledgeAbsent (:99-105): the
// freeze on the row as it stands, the confirmation, then the ordinary resume.
func (d *productionChapterDeletions) confirm(ctx context.Context, db *sql.DB, root *os.Root, operationID string) (productionChapterDeletionResult, error) {
	deletions := store.NewChapterDeletions(db, nil)
	existing, err := deletions.Get(ctx, operationID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	if existing != nil {
		frozen, err := productionChapterDeletionFrozen(root, operationID)
		if err != nil {
			return productionChapterDeletionResult{}, err
		}
		if frozen {
			return productionChapterDeletionFrozenAnswer(), nil
		}
	}
	if _, err := deletions.ConfirmLegacyKnowledgeAbsent(ctx, operationID); err != nil {
		return productionChapterDeletionResult{}, err
	}
	return d.resume(ctx, deletions, root, operationID)
}

// resume is ChapterDeletionService.resume (:122-159): the freeze and the legacy
// guard again, the completed replay, one attempt, one verdict per projection of
// every member, and the settle that aggregates them.
func (d *productionChapterDeletions) resume(ctx context.Context, deletions *store.ChapterDeletions, root *os.Root, operationID string) (productionChapterDeletionResult, error) {
	operation, err := deletions.Get(ctx, operationID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	if operation == nil {
		// :124-125, :131
		return productionChapterDeletionResult{}, store.ErrChapterDeletionOperationNotFound
	}
	frozen, err := productionChapterDeletionFrozen(root, operationID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	if frozen {
		return productionChapterDeletionFrozenAnswer(), nil
	}
	if operation.LegacyKnowledgeAuthorization == store.ChapterDeletionLegacyKnowledgeRequired {
		// :126-128 - still waiting for the author's confirmation.
		return productionChapterDeletionLegacyRequired(operation), nil
	}
	if operation.Status == store.ChapterDeletionCompleted {
		// :142-144 - a completed receipt is answered, never re-opened.
		return productionChapterDeletionSettled(operation), nil
	}
	operation, started, err := deletions.StartAttempt(ctx, operationID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	if !started {
		// A finished row answers itself without a write; StartAttempt documents
		// that a caller which sees started false must not clean and must not
		// settle (finalized_chapter_deletion.go:1015-1020).
		return productionChapterDeletionSettled(operation), nil
	}
	// :147-149 - the superseded receipts of the same chapter are cleaned too, and
	// their projections are part of the primary's aggregate.
	companions, err := deletions.ListCompanions(ctx, operationID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	members := make([]store.ChapterDeletionOperation, 0, len(companions)+1)
	members = append(members, companions...)
	members = append(members, *operation)
	for _, member := range members {
		if err := productionChapterDeletionClean(ctx, deletions, root, member); err != nil {
			return productionChapterDeletionResult{}, err
		}
	}
	settled, err := deletions.Settle(ctx, operationID)
	if err != nil {
		return productionChapterDeletionResult{}, err
	}
	// :151-159
	return productionChapterDeletionSettled(settled), nil
}

// list is ChapterDeletionService.listIncomplete (:130-135): every row that is not
// completed, minus the ones the portable-runtime freeze pins.
func (d *productionChapterDeletions) list(ctx context.Context, db *sql.DB, root *os.Root) (productionChapterDeletionListResult, error) {
	freeze, err := productionChapterDeletionReadFreeze(root)
	if err != nil {
		return productionChapterDeletionListResult{}, err
	}
	operations, err := store.NewChapterDeletions(db, nil).ListIncomplete(ctx)
	if err != nil {
		return productionChapterDeletionListResult{}, err
	}
	kept := make([]store.ChapterDeletionOperation, 0, len(operations))
	for _, operation := range operations {
		if freeze.isFrozen(operation.OperationID) {
			continue
		}
		kept = append(kept, operation)
	}
	return productionChapterDeletionListResult{Success: true, Operations: kept}, nil
}

// productionChapterDeletionSettled is the answer of a resume: success only for a
// completed row, committed because the outbox row and the facts were decided, and
// the store's own refusal when the projections are not settled.
func productionChapterDeletionSettled(operation *store.ChapterDeletionOperation) productionChapterDeletionResult {
	answer := productionChapterDeletionResult{
		Success:   operation.Status == store.ChapterDeletionCompleted,
		Committed: true,
		Operation: operation,
	}
	if refusal := operation.Refusal(); refusal != nil {
		answer.Error = refusal.Error()
	}
	return answer
}

// productionChapterDeletionLegacyRequired is legacyKnowledgeAuthorizationRequired
// (:43-52): the operation is committed but not cleaned, and the author's next move
// is the confirmation.
func productionChapterDeletionLegacyRequired(operation *store.ChapterDeletionOperation) productionChapterDeletionResult {
	return productionChapterDeletionResult{
		Committed: false,
		Operation: operation,
		Error:     store.ErrChapterDeletionLegacyKnowledgeCheckRequired.Error(),
	}
}

// productionChapterDeletionFrozenAnswer is frozenLegacyOperation (:54-57): no
// operation, because the frozen row is not this lane's to report on.
func productionChapterDeletionFrozenAnswer() productionChapterDeletionResult {
	return productionChapterDeletionResult{Error: productionChapterDeletionCodeFrozen}
}

// productionChapterDeletionClean is cleanProjections (:161-190) for one member.
//
// The manuscript gets a verdict. The knowledge document does not: this lane owns
// no knowledge-base writer, so it is left exactly as Begin set it - 'pending' -
// and the operation stays in the incomplete list until a lane that can remove it
// reports through the same MarkProjection. Reporting 'completed' here would be the
// lie this operation exists to prevent.
func productionChapterDeletionClean(ctx context.Context, deletions *store.ChapterDeletions, root *os.Root, member store.ChapterDeletionOperation) error {
	if member.ManuscriptStatus == store.ChapterDeletionProjectionPending || member.ManuscriptStatus == store.ChapterDeletionProjectionFailed {
		verdict, reason := store.ChapterDeletionProjectionCompleted, ""
		if err := productionChapterDeletionRemoveManuscript(root, member.TargetFileName); err != nil {
			verdict = store.ChapterDeletionProjectionFailed
			reason = productionChapterDeletionCleanupReason(err)
		}
		if _, err := deletions.MarkProjection(ctx, member.OperationID, store.ChapterDeletionManuscript, verdict, reason); err != nil {
			return err
		}
	}
	return nil
}

// productionChapterDeletionRemoveManuscript is removePublishedManuscript
// (manuscript-publisher.ts:65-74) over the admitted project root.
func productionChapterDeletionRemoveManuscript(root *os.Root, targetFileName string) error {
	if root == nil {
		return errors.New(productionChapterDeletionCodeRequestFailed)
	}
	// resolveStoredManuscriptTarget (:88-94): a frozen name must be a base name, so
	// a receipt can never name a child, a parent or an absolute path.
	if targetFileName == "" || targetFileName != filepath.Base(targetFileName) {
		return productionChapterDeletionErrTargetInvalid
	}
	info, err := root.Lstat(targetFileName)
	if errors.Is(err, fs.ErrNotExist) {
		// :71-73 - a missing file means the projection is already cleaned.
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		// Never unlink a directory, a link or a device because a receipt named it.
		return productionChapterDeletionErrTargetInvalid
	}
	// :73 - the unlink itself, confined by the root and by the base-name check.
	return root.Remove(targetFileName)
}

// productionChapterDeletionCleanupReason is the per-projection reason recorded on
// manuscript_error: a symbolic code, never the driver's message and never a path.
func productionChapterDeletionCleanupReason(err error) string {
	if errors.Is(err, productionChapterDeletionErrTargetInvalid) {
		return productionChapterDeletionCodeTargetInvalid
	}
	return productionChapterDeletionCodeManuscriptFailed
}

// productionChapterDeletionFreeze is readPortableRuntimeFreeze's guard for this
// lane: one parsed document, or an inactive guard when the project carries none.
type productionChapterDeletionFreeze struct {
	guard *archiveformat.FreezeGuard
}

// isFrozen is PortableRuntimeFreezeGuard.isFrozen (portable-runtime-freeze.ts:56).
// A nil guard is the inactive guard (:37-41) and answers false.
func (f productionChapterDeletionFreeze) isFrozen(operationID string) bool {
	return f.guard.IsFrozen(productionChapterDeletionTable, operationID)
}

// productionChapterDeletionFrozen is the one-call form resume, confirm and the
// delete replay use.
func productionChapterDeletionFrozen(root *os.Root, operationID string) (bool, error) {
	freeze, err := productionChapterDeletionReadFreeze(root)
	if err != nil {
		return false, err
	}
	return freeze.isFrozen(operationID), nil
}

// productionChapterDeletionReadFreeze is readPortableRuntimeFreeze (:46-58): no
// storage directory or no marker at all is a normal project and an inactive
// guard; a transfer authority without a freeze document, an unreadable marker and
// a document that does not parse are all PORTABLE_RUNTIME_FREEZE_INVALID, never an
// empty guard.
func productionChapterDeletionReadFreeze(root *os.Root) (productionChapterDeletionFreeze, error) {
	if root == nil {
		return productionChapterDeletionFreeze{}, productionChapterDeletionErrFreezeInvalid
	}
	authority, err := productionChapterDeletionReadMarker(root, productionChapterDeletionTransferName,
		archiveformat.MaxTransferAuthorityBytes)
	if err != nil {
		return productionChapterDeletionFreeze{}, productionChapterDeletionErrFreezeInvalid
	}
	document, err := productionChapterDeletionReadMarker(root, productionChapterDeletionFreezeName,
		archiveformat.MaxRuntimeHistoryBytes)
	if err != nil {
		return productionChapterDeletionFreeze{}, productionChapterDeletionErrFreezeInvalid
	}
	if len(document) == 0 {
		if len(authority) != 0 {
			// :54-55 - an authority with no freeze document is a half-installed
			// portable project, which is not a normal one.
			return productionChapterDeletionFreeze{}, productionChapterDeletionErrFreezeInvalid
		}
		return productionChapterDeletionFreeze{}, nil
	}
	guard, err := archiveformat.ParseRuntimeFreeze(document)
	if err != nil {
		return productionChapterDeletionFreeze{}, productionChapterDeletionErrFreezeInvalid
	}
	return productionChapterDeletionFreeze{guard: guard}, nil
}

// productionChapterDeletionReadMarker reads one installed sidecar of the canonical
// storage directory. Absent is (nil, nil): the caller decides what absence means.
// A directory, a link, an oversized file or an inspection error is a refusal,
// because a marker this lane cannot read is uncertainty, never proof of absence
// (internal/projectfs/authority.go:84-92).
func productionChapterDeletionReadMarker(root *os.Root, name string, limit int64) ([]byte, error) {
	relative := filepath.Join(projectfs.StorageDirectory, name)
	info, err := root.Lstat(relative)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, productionChapterDeletionErrFreezeInvalid
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !opened.Mode().IsRegular() || opened.Size() > limit {
		return nil, productionChapterDeletionErrFreezeInvalid
	}
	body, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, productionChapterDeletionErrFreezeInvalid
	}
	return body, nil
}

// productionChapterDeletionExpectedPath is assertRequiredExpectedProjectPath
// (electron/utils/project-context.ts:21-30) against the admitted path: the wire
// has to name the project the lease proves, and it can never widen the scope.
func productionChapterDeletionExpectedPath(value any, admitted string) error {
	path, ok := value.(string)
	if !ok || path == "" {
		return security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
	}
	if path != admitted {
		return security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	}
	return nil
}

// productionChapterDeletionOperationID is the one string argument of retry and
// confirm. An empty id is not refused here: the store answers
// CHAPTER_DELETE_OPERATION_NOT_FOUND for it, exactly like the reference's get.
func productionChapterDeletionOperationID(value any) (string, error) {
	id, ok := value.(string)
	if !ok {
		return "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	return id, nil
}

// productionChapterDeletionRequestProjection unwraps { draftId, chapterNumber }
// (src/shared/chapter-deletion.ts:5-8). Both are the reference's `number`; a value
// that is not its own integer is not a request any caller can send, so it is
// INVALID_ARGUMENTS rather than a truncated id. The two are NOT range-checked:
// the reference has no request guard here, and an unknown or mismatched pair is
// answered by the repository's own refusals (DRAFT_NOT_FOUND,
// CHAPTER_DELETE_TARGET_MISMATCH), which this lane keeps.
func productionChapterDeletionRequestProjection(value any) (productionChapterDeletionRequest, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return productionChapterDeletionRequest{}, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	draftID, ok := productionChapterDeletionInteger(object["draftId"])
	if !ok {
		return productionChapterDeletionRequest{}, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	chapterNumber, ok := productionChapterDeletionInteger(object["chapterNumber"])
	if !ok {
		return productionChapterDeletionRequest{}, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	return productionChapterDeletionRequest{DraftID: draftID, ChapterNumber: chapterNumber}, nil
}

// productionChapterDeletionInteger is Number.isSafeInteger: never a string, never
// a boolean, never a fraction, never a value outside the safe range, and no
// rounding on the way in.
func productionChapterDeletionInteger(value any) (int64, bool) {
	var number float64
	switch raw := value.(type) {
	case float64:
		number = raw
	case json.Number:
		if len(string(raw)) > 64 {
			return 0, false
		}
		parsed, err := raw.Float64()
		if err != nil {
			return 0, false
		}
		number = parsed
	default:
		return 0, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number ||
		number < -productionChapterDeletionSafeIntegerLimit || number > productionChapterDeletionSafeIntegerLimit {
		return 0, false
	}
	return int64(number), true
}

// productionChapterDeletionRefusals is the family's symbolic table. Every entry's
// code is the store's own error string (finalized_chapter_deletion.go:164-184), so
// the wire cannot invent a refusal the outbox never answers.
var productionChapterDeletionRefusals = [...]error{
	store.ErrChapterDeletionDraftNotFound,
	store.ErrChapterDeletionTargetMismatch,
	store.ErrChapterDeletionNotFinalized,
	store.ErrChapterDeletionRequestMismatch,
	store.ErrChapterDeletionConfirmationConsumed,
	store.ErrChapterDeletionSourceChanged,
	store.ErrChapterDeletionReceiptCorrupt,
	store.ErrChapterDeletionOperationNotFound,
	store.ErrChapterDeletionChapterInvalid,
	store.ErrChapterDeletionLegacyKnowledgeCheckRequired,
	store.ErrChapterDeletionCleanupIncomplete,
	productionChapterDeletionErrFreezeInvalid,
}

// productionChapterDeletionCode is this file's single refusal mapping: the store's
// symbolic code via errors.Is, or the lane's unmapped-failure code.
func productionChapterDeletionCode(err error) string {
	for _, refusal := range productionChapterDeletionRefusals {
		if errors.Is(err, refusal) {
			return refusal.Error()
		}
	}
	return productionChapterDeletionCodeRequestFailed
}

// productionChapterDeletionFailure is the mutation envelope of any refusal the
// state machine raised: HTTP 200, success:false, committed:false and the code.
func productionChapterDeletionFailure(err error) any {
	return productionChapterDeletionResult{Error: productionChapterDeletionCode(err)}
}

// productionChapterDeletionListFailure is the same refusal in the list channel's
// own envelope (src/shared/ipc-channels.ts:1289).
func productionChapterDeletionListFailure(err error) any {
	return productionChapterDeletionListResult{Error: productionChapterDeletionCode(err)}
}

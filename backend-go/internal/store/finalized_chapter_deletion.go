package store

// The four `chapter:*` operations of web/operations.json - delete-finalized,
// retry-deletion, confirm-legacy-knowledge-absent and
// list-incomplete-deletions - are one outbox-style state machine over
// chapter_deletion_operations. This file is its transaction layer, ported from
// electron/repositories/chapter-deletion-repository.ts (429 lines) with the wire
// contract of electron/controllers/chapter-lifecycle-controller.ts:40-76 and
// src/shared/chapter-deletion.ts:10-30.
//
// # The boundary: decisions here, cleaning in the adapter
//
// ChapterDeletionService (chapter-deletion-service.ts:68-190) mixes two things.
// It decides what the answer is, and it removes the two projections a database
// cannot remove: the published manuscript file (removePublishedManuscript,
// :26) and the managed knowledge-base document (:27-35, through
// knowledgeBaseLoader). Only the decisions are ported here. Cleaning needs the
// project directory and the native knowledge base, neither of which this
// package has, so the adapter calls MarkProjection once its cleaner has
// succeeded or failed - which is exactly what :161-190 does with the same two
// calls.
//
// # The state machine, and the reference line for every state name
//
// chapter_deletion_operations.status
//
//	'pending'                :300 (a fresh deletion, nothing cleaned yet),
//	                         :167 (a superseded companion with nothing to
//	                         clean), :350 (after the legacy confirmation),
//	                         :404 (every retry re-pends the row)
//	'authorization_required' :300 (begin stopped at the legacy guard)
//	'failed'                 :238 (aggregate: some projection is 'failed')
//	'completed'              :237 (aggregate: every projection is 'completed'
//	                         or 'not_required')
//
// manuscript_status, knowledge_status
//
//	'pending'      :147-148 (a superseded draft still owns that projection),
//	               :295-298 (the target still owns it)
//	'not_required' :147-148, :237 (nothing to clean), :349 (the legacy
//	               confirmation proves there is no document to clean)
//	'completed'    :414 (the cleaner removed it)
//	'failed'       :414 (the cleaner threw; manuscript_error/knowledge_error
//	               carry its message, :416-419)
//
// legacy_knowledge_authorization
//
//	'not_required' :155 (companions), :299 (no legacy projection to authorize)
//	'required'     :299 (begin waits for the author)
//	'consumed'     :347 (the author confirmed the chapter has no legacy
//	               knowledge document)
//
// # Transitions, in the order the wire operations drive them
//
//   - delete-finalized - begin (:258-312): the duplicate probe, the finalized
//     target probe, the frozen run ids, one INSERT, and deleteChapterFacts
//     (:304) unless the guard raised 'required' (:303). Then resume
//     (:137-159) = startAttempt (:400-409), one markProjection per projection
//     (:411-428), settle (:389-394).
//   - retry-deletion - resume again from startAttempt: it re-pends the row,
//     counts the attempt, and the two projections are cleaned again (:163,
//     :177 - a 'failed' or 'pending' projection is retried, a 'completed' or
//     'not_required' one is not).
//   - confirm-legacy-knowledge-absent - confirmLegacyKnowledgeAbsent
//     (:314-366): 'required' -> 'consumed', knowledge_status ->
//     'not_required', status -> 'pending' (:345-354), then deleteChapterFacts
//     (:358) and resume.
//   - list-incomplete-deletions - listIncomplete (:380-387): every row whose
//     status is not 'completed', companions excluded.
//
// # Every table and column the statements below touch
//
//   - chapter_deletion_operations: operation_id, draft_id, chapter_number,
//     chapter_title, finalization_id, target_file_name, knowledge_document_id,
//     post_process_run_ids, manuscript_status, manuscript_error,
//     knowledge_status, knowledge_error, legacy_knowledge_authorization,
//     legacy_knowledge_authorized_at, status, attempt_count, created_at,
//     updated_at, completed_at. (baseline_v7_ddl.go:74-94)
//   - drafts: id, chapter_number, version, status, content_id (read, and
//     DELETE by id). (baseline_v7_ddl.go:205-218)
//   - contents: id (DELETE only). (baseline_v7_ddl.go:183-187)
//   - finalization_outbox: draft_id, finalization_id, chapter_title,
//     target_file_name, knowledge_document_id (LEFT JOIN read; the rows leave
//     with the draft's ON DELETE CASCADE, baseline_v7_ddl.go:231-233).
//   - post_process_runs: id, trigger_source_type, trigger_source_id,
//     created_at, rowid (read, and DELETE by id).
//   - post_process_steps: run_id, step_key, id (the legacy guard's kb_import
//     probe; rows leave with their run's ON DELETE CASCADE).
//   - revisions: base_draft_id, content_id (read).
//   - reviews: base_draft_id, content_id (read).
//   - continuity_projection_meta: id, generation, stale_from_chapter
//     (summary-repository.ts:389-397).
//
// # Documented reductions
//
//   - readPortableRuntimeFreeze is a file read of the portable-runtime freeze
//     document, not a database fact (chapter-deletion-service.ts:54-57,
//     :112-114, :124-125, :133-134). This package has no portable-runtime
//     reader, so CHAPTER_DELETE_LEGACY_OPERATION_FROZEN and the freeze filter
//     on listIncomplete are not ported. The adapter must apply both before it
//     calls anything here; nothing below weakens another guard to compensate.
//   - operationError/CHAPTER_DELETE_CLEANUP_INCOMPLETE (:38-41) and the legacy
//     guard's CHAPTER_DELETE_LEGACY_KNOWLEDGE_CHECK_REQUIRED (:43-52) are pure
//     projections of the operation row, so they are kept as Refusal below
//     instead of being re-derived by the adapter.
//   - reachable only through them: no refusal is invented for a row that
//     cannot exist. startAttempt's WHERE keeps the original guard (:404-405)
//     and adds the one the service applies before calling it (:142-144), so a
//     finished receipt is answered, never mutated.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	sqlite3 "github.com/mattn/go-sqlite3"
)

// The chapter_deletion_operations.status values, each the reference's own
// literal (:300, :404, :237, :238).
const (
	ChapterDeletionAuthorizationRequired = "authorization_required"
	ChapterDeletionPending               = "pending"
	ChapterDeletionFailed                = "failed"
	ChapterDeletionCompleted             = "completed"
)

// The manuscript_status/knowledge_status values (:147-148, :237, :414).
const (
	ChapterDeletionProjectionPending     = "pending"
	ChapterDeletionProjectionCompleted   = "completed"
	ChapterDeletionProjectionFailed      = "failed"
	ChapterDeletionProjectionNotRequired = "not_required"
)

// The legacy_knowledge_authorization values (:155, :299, :347).
const (
	ChapterDeletionLegacyKnowledgeNotRequired = "not_required"
	ChapterDeletionLegacyKnowledgeRequired    = "required"
	ChapterDeletionLegacyKnowledgeConsumed    = "consumed"
)

// ChapterDeletionProjection names which of the two projections markProjection
// writes (:412). It is the reference's `'manuscript' | 'knowledge'` union.
type ChapterDeletionProjection string

const (
	ChapterDeletionManuscript ChapterDeletionProjection = "manuscript"
	ChapterDeletionKnowledge  ChapterDeletionProjection = "knowledge"
)

// The refusal codes of this state machine, each one the literal the reference
// throws or answers with. The original never catches the repository's throws
// (chapter-lifecycle-controller.ts:32-37 turns them into one message), so they
// reach the browser as symbolic text with no prefix.
var (
	// chapter-deletion-repository.ts:75
	ErrChapterDeletionDraftNotFound = errors.New("DRAFT_NOT_FOUND")
	// chapter-deletion-repository.ts:76
	ErrChapterDeletionTargetMismatch = errors.New("CHAPTER_DELETE_TARGET_MISMATCH")
	// chapter-deletion-repository.ts:78
	ErrChapterDeletionNotFinalized = errors.New("CHAPTER_DELETE_NOT_FINALIZED")
	// chapter-deletion-repository.ts:272 (and chapter-deletion-service.ts:75)
	ErrChapterDeletionRequestMismatch = errors.New("CHAPTER_DELETE_REQUEST_MISMATCH")
	// chapter-deletion-repository.ts:325 and :356
	ErrChapterDeletionConfirmationConsumed = errors.New("CHAPTER_DELETE_LEGACY_CONFIRMATION_CONSUMED")
	// chapter-deletion-repository.ts:335 and :342
	ErrChapterDeletionSourceChanged = errors.New("CHAPTER_DELETE_SOURCE_CHANGED")
	// chapter-deletion-repository.ts:339
	ErrChapterDeletionReceiptCorrupt = errors.New("CHAPTER_DELETE_RECEIPT_CORRUPT")
	// chapter-deletion-repository.ts:320, :364, :392, :407, :425
	ErrChapterDeletionOperationNotFound = errors.New("CHAPTER_DELETE_OPERATION_NOT_FOUND")
	// electron/repositories/summary-repository.ts:387
	ErrChapterDeletionChapterInvalid = errors.New("CONTINUITY_INVALIDATION_CHAPTER_INVALID")
	// chapter-deletion-service.ts:50, answered by Refusal
	ErrChapterDeletionLegacyKnowledgeCheckRequired = errors.New("CHAPTER_DELETE_LEGACY_KNOWLEDGE_CHECK_REQUIRED")
	// chapter-deletion-service.ts:40, answered by Refusal
	ErrChapterDeletionCleanupIncomplete = errors.New("CHAPTER_DELETE_CLEANUP_INCOMPLETE")
)

// chapterDeletionSupersededSuffix is SUPERSEDED_SUFFIX
// (chapter-deletion-repository.ts:96). A chapter that was finalized more than
// once keeps one companion operation per superseded draft so each of their
// projections is removed exactly once.
const chapterDeletionSupersededSuffix = ":superseded:"

// ChapterDeletionOperation is the 19-field wire object
// (src/shared/chapter-deletion.ts:10-30). PostProcessRunIDs is never nil: the
// reference answers a corrupt receipt with an empty list (:205).
type ChapterDeletionOperation struct {
	OperationID                  string   `json:"operationId"`
	DraftID                      int64    `json:"draftId"`
	ChapterNumber                int64    `json:"chapterNumber"`
	ChapterTitle                 string   `json:"chapterTitle"`
	FinalizationID               string   `json:"finalizationId"`
	TargetFileName               string   `json:"targetFileName"`
	KnowledgeDocumentID          string   `json:"knowledgeDocumentId"`
	PostProcessRunIDs            []string `json:"postProcessRunIds"`
	ManuscriptStatus             string   `json:"manuscriptStatus"`
	ManuscriptError              string   `json:"manuscriptError"`
	KnowledgeStatus              string   `json:"knowledgeStatus"`
	KnowledgeError               string   `json:"knowledgeError"`
	LegacyKnowledgeAuthorization string   `json:"legacyKnowledgeAuthorization"`
	LegacyKnowledgeAuthorizedAt  string   `json:"legacyKnowledgeAuthorizedAt"`
	Status                       string   `json:"status"`
	AttemptCount                 int64    `json:"attemptCount"`
	CreatedAt                    string   `json:"createdAt"`
	UpdatedAt                    string   `json:"updatedAt"`
	CompletedAt                  string   `json:"completedAt"`
}

// Refusal is the symbolic error this operation must be answered with, or nil.
// It ports the service's two pure projections of the row: the legacy guard
// (chapter-deletion-service.ts:43-52, taken whenever the authorization is still
// 'required', :79 and :126) and an incomplete cleanup (:38-41, the 'failed'
// aggregate). A nil Refusal with a 'completed' status is the replay at :142-144.
func (o *ChapterDeletionOperation) Refusal() error {
	if o == nil {
		return nil
	}
	if o.LegacyKnowledgeAuthorization == ChapterDeletionLegacyKnowledgeRequired {
		return ErrChapterDeletionLegacyKnowledgeCheckRequired
	}
	if o.Status == ChapterDeletionFailed {
		return ErrChapterDeletionCleanupIncomplete
	}
	return nil
}

// ChapterDeletionBegin is the repository's begin input
// (chapter-deletion-repository.ts:259-264) minus operationId, which this store
// mints the way the service does (:64, randomUUID). DraftID and ChapterNumber
// are DeleteFinalizedChapterRequest (src/shared/chapter-deletion.ts:6-9);
// LegacyKnowledgeAuthorizationRequired is the service's own pre-computation
// (:84-90) and must come from RequiresLegacyKnowledgeAuthorization below.
type ChapterDeletionBegin struct {
	DraftID                              int64 `json:"draftId"`
	ChapterNumber                        int64 `json:"chapterNumber"`
	LegacyKnowledgeAuthorizationRequired bool  `json:"-"`
}

// ChapterDeletions is the transaction layer of the deletion outbox. It borrows
// an admitted project database: it never opens a path, migrates, or owns the
// connection pool, and it removes no file and no knowledge document.
type ChapterDeletions struct {
	db          *sql.DB
	operationID func() (string, error)
}

// NewChapterDeletions performs no I/O. A nil operationID defaults to the
// package's secureUUID, the same seam GenerationLedger uses for its ids.
func NewChapterDeletions(db *sql.DB, operationID func() string) *ChapterDeletions {
	id := secureUUID
	if operationID != nil {
		id = func() (string, error) { return operationID(), nil }
	}
	return &ChapterDeletions{db: db, operationID: id}
}

// chapterDeletionQueryer is satisfied by both *sql.DB and the reserved
// *sql.Conn a write transaction holds, so the transaction-internal reads below
// cannot be served by a second connection.
type chapterDeletionQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// immediate runs fn inside one BEGIN IMMEDIATE on one reserved connection, the
// shape of the neighbouring writes (draft_commit_tx.go:143, drafts.go:237,
// reviewrevision.go:46). A cancelled request must not return a connection that
// still holds a manual transaction to the pool.
func (d *ChapterDeletions) immediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error) {
	if d == nil || d.db == nil {
		return ErrProjectNotOpen
	}
	conn, err := d.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(cleanup, "ROLLBACK"); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("rollback chapter deletion: %w", err))
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// chapterDeletionOperationColumns is the reference's `SELECT *`
// (:230-232), written out so a schema change cannot silently re-order the scan.
const chapterDeletionOperationColumns = `operation_id, draft_id, chapter_number, chapter_title,
 finalization_id, target_file_name, knowledge_document_id, post_process_run_ids,
 manuscript_status, manuscript_error, knowledge_status, knowledge_error,
 legacy_knowledge_authorization, legacy_knowledge_authorized_at,
 status, attempt_count, COALESCE(created_at, ''), COALESCE(updated_at, ''), completed_at`

// chapterDeletionTargetColumns is requireFinalizedTarget's projection
// (chapter-deletion-repository.ts:64-68).
const chapterDeletionTargetColumns = `drafts.id, drafts.chapter_number, drafts.status, drafts.content_id,
 finalization_outbox.finalization_id, finalization_outbox.chapter_title,
 finalization_outbox.target_file_name, finalization_outbox.knowledge_document_id`

// chapterDeletionRow is ChapterDeletionRow (:8-28). created_at and updated_at
// are nullable in the DDL, so they are read through COALESCE above; every row
// this file writes gets them from the column defaults or datetime('now').
type chapterDeletionRow struct {
	operationID                  string
	draftID                      int64
	chapterNumber                int64
	chapterTitle                 string
	finalizationID               string
	targetFileName               string
	knowledgeDocumentID          string
	postProcessRunIDs            string
	manuscriptStatus             string
	manuscriptError              string
	knowledgeStatus              string
	knowledgeError               string
	legacyKnowledgeAuthorization string
	legacyKnowledgeAuthorizedAt  string
	status                       string
	attemptCount                 int64
	createdAt                    string
	updatedAt                    string
	completedAt                  string
}

// chapterDeletionScanner is satisfied by *sql.Row and *sql.Rows.
type chapterDeletionScanner interface {
	Scan(dest ...any) error
}

func chapterDeletionScan(scanner chapterDeletionScanner) (chapterDeletionRow, error) {
	var row chapterDeletionRow
	err := scanner.Scan(&row.operationID, &row.draftID, &row.chapterNumber, &row.chapterTitle,
		&row.finalizationID, &row.targetFileName, &row.knowledgeDocumentID, &row.postProcessRunIDs,
		&row.manuscriptStatus, &row.manuscriptError, &row.knowledgeStatus, &row.knowledgeError,
		&row.legacyKnowledgeAuthorization, &row.legacyKnowledgeAuthorizedAt,
		&row.status, &row.attemptCount, &row.createdAt, &row.updatedAt, &row.completedAt)
	if err != nil {
		return chapterDeletionRow{}, err
	}
	return row, nil
}

// chapterDeletionParseRunIDs is parsePostProcessRunIds (:47-56): a value that is
// not a JSON array of strings is corrupt, not empty.
func chapterDeletionParseRunIDs(value string) ([]string, bool) {
	var decoded any
	if err := json.Unmarshal([]byte(value), &decoded); err != nil {
		return nil, false
	}
	// Array.isArray: a JSON null is corrupt here, not an empty list.
	parsed, ok := decoded.([]any)
	if !ok {
		return nil, false
	}
	ids := make([]string, 0, len(parsed))
	for _, candidate := range parsed {
		text, ok := candidate.(string)
		if !ok {
			return nil, false
		}
		ids = append(ids, text)
	}
	return ids, true
}

// chapterDeletionRowToOperation is rowToOperation (:204-227).
func chapterDeletionRowToOperation(row chapterDeletionRow) *ChapterDeletionOperation {
	runIDs, ok := chapterDeletionParseRunIDs(row.postProcessRunIDs)
	if !ok {
		// :205 - `?? []`, and a corrupt receipt stays corrupt for the
		// confirmation that reads it (:338-339).
		runIDs = make([]string, 0)
	}
	return &ChapterDeletionOperation{
		OperationID:                  row.operationID,
		DraftID:                      row.draftID,
		ChapterNumber:                row.chapterNumber,
		ChapterTitle:                 row.chapterTitle,
		FinalizationID:               row.finalizationID,
		TargetFileName:               row.targetFileName,
		KnowledgeDocumentID:          row.knowledgeDocumentID,
		PostProcessRunIDs:            runIDs,
		ManuscriptStatus:             row.manuscriptStatus,
		ManuscriptError:              row.manuscriptError,
		KnowledgeStatus:              row.knowledgeStatus,
		KnowledgeError:               row.knowledgeError,
		LegacyKnowledgeAuthorization: row.legacyKnowledgeAuthorization,
		LegacyKnowledgeAuthorizedAt:  row.legacyKnowledgeAuthorizedAt,
		Status:                       row.status,
		AttemptCount:                 row.attemptCount,
		CreatedAt:                    row.createdAt,
		UpdatedAt:                    row.updatedAt,
		CompletedAt:                  row.completedAt,
	}
}

func chapterDeletionReadRow(ctx context.Context, q chapterDeletionQueryer, operationID string) (*chapterDeletionRow, error) {
	row, err := chapterDeletionScan(q.QueryRowContext(ctx, `SELECT `+chapterDeletionOperationColumns+`
 FROM chapter_deletion_operations WHERE operation_id = ?`, operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

func chapterDeletionReadRowByDraft(ctx context.Context, q chapterDeletionQueryer, draftID int64) (*chapterDeletionRow, error) {
	row, err := chapterDeletionScan(q.QueryRowContext(ctx, `SELECT `+chapterDeletionOperationColumns+`
 FROM chapter_deletion_operations WHERE draft_id = ?`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &row, nil
}

// chapterDeletionCompanionRows is companionRows (:107-114), including its
// substr prefix match, so a companion of a companion is found too.
func chapterDeletionCompanionRows(ctx context.Context, q chapterDeletionQueryer, operationID string) ([]chapterDeletionRow, error) {
	prefix := operationID + chapterDeletionSupersededSuffix
	rows, err := q.QueryContext(ctx, `SELECT `+chapterDeletionOperationColumns+`
 FROM chapter_deletion_operations
 WHERE substr(operation_id, 1, length(?)) = ?
 ORDER BY draft_id`, prefix, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	companions := make([]chapterDeletionRow, 0)
	for rows.Next() {
		row, err := chapterDeletionScan(rows)
		if err != nil {
			return nil, err
		}
		companions = append(companions, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return companions, nil
}

// chapterDeletionListRunIDs is listPostProcessRunIds (:83-94). The empty slice
// marshals to "[]", which is what JSON.stringify([]) wrote (:294).
func chapterDeletionListRunIDs(ctx context.Context, q chapterDeletionQueryer, chapterNumber int64, finalizationID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM post_process_runs
 WHERE trigger_source_type = 'chapter_finalize' AND trigger_source_id IN (?, ?)
 ORDER BY id`, strconv.FormatInt(chapterNumber, 10), "finalization:"+finalizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return ids, nil
}

// chapterDeletionListFinalizationRunIDs is the superseded branch of
// deleteChapterFacts (:141-146): only the replaced receipt's own runs.
func chapterDeletionListFinalizationRunIDs(ctx context.Context, q chapterDeletionQueryer, finalizationID string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM post_process_runs
 WHERE trigger_source_type = 'chapter_finalize' AND trigger_source_id = ?
 ORDER BY id`, "finalization:"+finalizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return ids, nil
}

// chapterDeletionTarget is FinalizedChapterTarget (:30-39).
type chapterDeletionTarget struct {
	draftID             int64
	chapterNumber       int64
	status              string
	contentID           int64
	finalizationID      sql.NullString
	chapterTitle        sql.NullString
	targetFileName      sql.NullString
	knowledgeDocumentID sql.NullString
}

func chapterDeletionScanTarget(scanner chapterDeletionScanner) (chapterDeletionTarget, error) {
	var target chapterDeletionTarget
	err := scanner.Scan(&target.draftID, &target.chapterNumber, &target.status, &target.contentID,
		&target.finalizationID, &target.chapterTitle, &target.targetFileName, &target.knowledgeDocumentID)
	if err != nil {
		return chapterDeletionTarget{}, err
	}
	return target, nil
}

// chapterDeletionRequireTarget is requireFinalizedTarget (:58-81), including the
// order of its three refusals.
func chapterDeletionRequireTarget(ctx context.Context, q chapterDeletionQueryer, draftID, chapterNumber int64) (chapterDeletionTarget, error) {
	target, err := chapterDeletionScanTarget(q.QueryRowContext(ctx, `SELECT `+chapterDeletionTargetColumns+`
 FROM drafts
 LEFT JOIN finalization_outbox ON finalization_outbox.draft_id = drafts.id
 WHERE drafts.id = ?`, draftID))
	if errors.Is(err, sql.ErrNoRows) {
		// :75 - no such draft at all.
		return chapterDeletionTarget{}, ErrChapterDeletionDraftNotFound
	}
	if err != nil {
		return chapterDeletionTarget{}, err
	}
	if target.chapterNumber != chapterNumber {
		// :76 - the request names a chapter this draft does not belong to.
		return chapterDeletionTarget{}, ErrChapterDeletionTargetMismatch
	}
	// :77-78 - `!finalization_id` is true for an empty string as well.
	if target.status != "finalized" || !target.finalizationID.Valid || target.finalizationID.String == "" {
		return chapterDeletionTarget{}, ErrChapterDeletionNotFinalized
	}
	return target, nil
}

// chapterDeletionAggregateStatus is aggregateStatus (:235-239): every projection
// completed or not required is 'completed', any 'failed' is 'failed', anything
// else is still 'pending'.
func chapterDeletionAggregateStatus(rows []chapterDeletionRow) string {
	projections := make([]string, 0, len(rows)*2)
	for _, row := range rows {
		projections = append(projections, row.manuscriptStatus, row.knowledgeStatus)
	}
	complete := true
	failed := false
	for _, status := range projections {
		if status != ChapterDeletionProjectionCompleted && status != ChapterDeletionProjectionNotRequired {
			complete = false
		}
		if status == ChapterDeletionProjectionFailed {
			failed = true
		}
	}
	if complete {
		return ChapterDeletionCompleted
	}
	if failed {
		return ChapterDeletionFailed
	}
	return ChapterDeletionPending
}

// chapterDeletionRefreshAggregateStatus is refreshAggregateStatus (:241-256). A
// companion carries only its own aggregate; the primary also carries every
// companion's, which is why the two are refreshed together.
func chapterDeletionRefreshAggregateStatus(ctx context.Context, conn *sql.Conn, operationID string) error {
	primaryID := chapterDeletionPrimaryOperationID(operationID)
	ids := []string{operationID}
	if primaryID != operationID {
		ids = []string{operationID, primaryID}
	}
	for _, id := range ids {
		row, err := chapterDeletionReadRow(ctx, conn, id)
		if err != nil {
			return err
		}
		if row == nil {
			// :247
			return ErrChapterDeletionOperationNotFound
		}
		members := []chapterDeletionRow{*row}
		if id == primaryID {
			companions, err := chapterDeletionCompanionRows(ctx, conn, id)
			if err != nil {
				return err
			}
			members = append(members, companions...)
		}
		status := chapterDeletionAggregateStatus(members)
		if _, err := conn.ExecContext(ctx, `UPDATE chapter_deletion_operations
 SET status = ?, completed_at = CASE WHEN ? = 'completed' THEN datetime('now') ELSE '' END,
     updated_at = datetime('now')
 WHERE operation_id = ?`, status, status, id); err != nil {
			return err
		}
	}
	return nil
}

// chapterDeletionCompanionOperationID is companionOperationId (:98-100).
func chapterDeletionCompanionOperationID(operationID string, draftID int64) string {
	return operationID + chapterDeletionSupersededSuffix + strconv.FormatInt(draftID, 10)
}

// chapterDeletionPrimaryOperationID is primaryOperationId (:102-105).
func chapterDeletionPrimaryOperationID(operationID string) string {
	index := strings.Index(operationID, chapterDeletionSupersededSuffix)
	if index < 0 {
		return operationID
	}
	return operationID[:index]
}

// chapterDeletionForeignKeyViolation is the swallowed case of :194-201: a
// content row another live fact still references is not this deletion's target,
// and the RESTRICT on contents(id) (baseline_v7_ddl.go:216) is what says so.
// The reference catches every error of that one statement; here only the
// documented one is swallowed and anything else still fails the transaction.
func chapterDeletionForeignKeyViolation(err error) bool {
	var sqliteError sqlite3.Error
	if !errors.As(err, &sqliteError) {
		return false
	}
	// mattn/go-sqlite3 has no ErrCode field: Code is the primary result code
	// (ErrNo) and ExtendedCode the extended one (ErrNoExtended), and the two
	// constant families are different types, so each is compared with its own.
	// Every constraint violation, including a foreign-key one, carries
	// Code == ErrConstraint; the foreign-key case additionally extends it.
	return sqliteError.Code == sqlite3.ErrConstraint ||
		sqliteError.ExtendedCode == sqlite3.ErrConstraintForeignKey
}

// chapterDeletionDeleteDraftFacts is deleteDraftFacts (:175-202): the receipt's
// runs, the draft (whose finalization_outbox row leaves through ON DELETE
// CASCADE), and the contents that only this draft, its revisions and its reviews
// held.
func chapterDeletionDeleteDraftFacts(ctx context.Context, conn *sql.Conn, draftID, draftContentID int64, runIDs []string) error {
	contentIDs := make([]int64, 0, 4)
	seen := make(map[int64]bool, 4)
	add := func(id int64) {
		if seen[id] {
			return
		}
		seen[id] = true
		contentIDs = append(contentIDs, id)
	}
	add(draftContentID)
	for _, query := range []string{
		"SELECT content_id FROM revisions WHERE base_draft_id = ?",
		"SELECT content_id FROM reviews WHERE base_draft_id = ?",
	} {
		rows, err := conn.QueryContext(ctx, query, draftID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var contentID int64
			if err := rows.Scan(&contentID); err != nil {
				rows.Close()
				return err
			}
			add(contentID)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
	}
	for _, runID := range runIDs {
		if _, err := conn.ExecContext(ctx, "DELETE FROM post_process_runs WHERE id = ?", runID); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, "DELETE FROM drafts WHERE id = ?", draftID); err != nil {
		return err
	}
	for _, contentID := range contentIDs {
		if _, err := conn.ExecContext(ctx, "DELETE FROM contents WHERE id = ?", contentID); err != nil {
			if chapterDeletionForeignKeyViolation(err) {
				continue
			}
			return err
		}
	}
	return nil
}

// chapterDeletionInvalidateContinuity is invalidateContinuityProjectionFrom
// (electron/repositories/summary-repository.ts:382-398), the first statement of
// deleteChapterFacts (:122). A chapter number below one is refused before the
// UPDATE, exactly as :386-388 does.
func chapterDeletionInvalidateContinuity(ctx context.Context, conn *sql.Conn, chapterNumber int64) error {
	if chapterNumber < 1 {
		return ErrChapterDeletionChapterInvalid
	}
	_, err := conn.ExecContext(ctx, `UPDATE continuity_projection_meta
 SET generation = generation + 1,
     stale_from_chapter = CASE
       WHEN stale_from_chapter IS NULL OR stale_from_chapter > ? THEN ?
       ELSE stale_from_chapter
     END
 WHERE id = 'main'`, chapterNumber, chapterNumber)
	return err
}

// chapterDeletionDeleteFacts is deleteChapterFacts (:116-173). Every superseded
// draft of the same chapter becomes a companion operation first, because its
// manuscript and knowledge document are projections the adapter still has to
// remove; only then does this draft's own fact go.
func chapterDeletionDeleteFacts(ctx context.Context, conn *sql.Conn, operationID string, target chapterDeletionTarget, postProcessRunIDs []string) error {
	if err := chapterDeletionInvalidateContinuity(ctx, conn, target.chapterNumber); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, `SELECT `+chapterDeletionTargetColumns+`
 FROM drafts
 LEFT JOIN finalization_outbox ON finalization_outbox.draft_id = drafts.id
 WHERE drafts.chapter_number = ? AND drafts.status = 'superseded'
 ORDER BY drafts.id`, target.chapterNumber)
	if err != nil {
		return err
	}
	superseded := make([]chapterDeletionTarget, 0)
	for rows.Next() {
		row, err := chapterDeletionScanTarget(rows)
		if err != nil {
			rows.Close()
			return err
		}
		superseded = append(superseded, row)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, row := range superseded {
		runIDs := make([]string, 0)
		if row.finalizationID.String != "" {
			runIDs, err = chapterDeletionListFinalizationRunIDs(ctx, conn, row.finalizationID.String)
			if err != nil {
				return err
			}
		}
		// :147-149
		manuscriptStatus := ChapterDeletionProjectionNotRequired
		if row.targetFileName.String != "" {
			manuscriptStatus = ChapterDeletionProjectionPending
		}
		knowledgeStatus := ChapterDeletionProjectionNotRequired
		if row.knowledgeDocumentID.String != "" {
			knowledgeStatus = ChapterDeletionProjectionPending
		}
		finished := manuscriptStatus == ChapterDeletionProjectionNotRequired &&
			knowledgeStatus == ChapterDeletionProjectionNotRequired
		stored, err := json.Marshal(runIDs)
		if err != nil {
			return err
		}
		status := ChapterDeletionPending
		completed := 0
		if finished {
			status = ChapterDeletionCompleted
			completed = 1
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO chapter_deletion_operations (
  operation_id, draft_id, chapter_number, chapter_title, finalization_id,
  target_file_name, knowledge_document_id, post_process_run_ids,
  manuscript_status, knowledge_status, legacy_knowledge_authorization, status, completed_at
 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'not_required', ?, CASE WHEN ? THEN datetime('now') ELSE '' END)`,
			chapterDeletionCompanionOperationID(operationID, row.draftID),
			row.draftID, target.chapterNumber, row.chapterTitle.String, row.finalizationID.String,
			row.targetFileName.String, row.knowledgeDocumentID.String, string(stored),
			manuscriptStatus, knowledgeStatus, status, completed); err != nil {
			return err
		}
		// :170
		if err := chapterDeletionDeleteDraftFacts(ctx, conn, row.draftID, row.contentID, runIDs); err != nil {
			return err
		}
	}
	// :172
	return chapterDeletionDeleteDraftFacts(ctx, conn, target.draftID, target.contentID, postProcessRunIDs)
}

// Begin is the repository's begin (:258-312), the first durable step of
// delete-finalized. One IMMEDIATE transaction: the duplicate probe, the finalized
// target, its frozen run ids, the operation row, and - unless the legacy
// knowledge guard is raised - the deletion of the finalized fact itself.
//
// A duplicate draft answers the operation that already exists (:270-275) and
// writes nothing, so a repeated delete-finalized for the same draft is a replay
// of the row, not a second deletion.
//
// The reference wraps the same statements in better-sqlite3's deferred
// db.transaction() (:266); this port takes the write lock with BEGIN IMMEDIATE
// up front, which only widens a lock the original always ended up taking.
func (d *ChapterDeletions) Begin(ctx context.Context, request ChapterDeletionBegin) (*ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	// :86 - the service mints the id before the transaction opens.
	operationID, err := d.operationID()
	if err != nil {
		return nil, err
	}
	resolved := ""
	err = d.immediate(ctx, func(conn *sql.Conn) error {
		existing, err := chapterDeletionReadRowByDraft(ctx, conn, request.DraftID)
		if err != nil {
			return err
		}
		if existing != nil {
			if existing.chapterNumber != request.ChapterNumber {
				// :272 - the draft already has a deletion for another chapter.
				return ErrChapterDeletionRequestMismatch
			}
			resolved = existing.operationID
			return nil
		}
		target, err := chapterDeletionRequireTarget(ctx, conn, request.DraftID, request.ChapterNumber)
		if err != nil {
			return err
		}
		runIDs, err := chapterDeletionListRunIDs(ctx, conn, request.ChapterNumber, target.finalizationID.String)
		if err != nil {
			return err
		}
		stored, err := json.Marshal(runIDs)
		if err != nil {
			return err
		}
		// :295-300
		manuscriptStatus := ChapterDeletionProjectionNotRequired
		if target.targetFileName.String != "" {
			manuscriptStatus = ChapterDeletionProjectionPending
		}
		knowledgeStatus := ChapterDeletionProjectionNotRequired
		if request.LegacyKnowledgeAuthorizationRequired || target.knowledgeDocumentID.String != "" {
			knowledgeStatus = ChapterDeletionProjectionPending
		}
		authorization := ChapterDeletionLegacyKnowledgeNotRequired
		status := ChapterDeletionPending
		if request.LegacyKnowledgeAuthorizationRequired {
			authorization = ChapterDeletionLegacyKnowledgeRequired
			status = ChapterDeletionAuthorizationRequired
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO chapter_deletion_operations (
  operation_id, draft_id, chapter_number, chapter_title, finalization_id,
  target_file_name, knowledge_document_id, post_process_run_ids,
  manuscript_status, knowledge_status, legacy_knowledge_authorization, status
 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			operationID, target.draftID, target.chapterNumber, target.chapterTitle.String,
			target.finalizationID.String, target.targetFileName.String,
			target.knowledgeDocumentID.String, string(stored),
			manuscriptStatus, knowledgeStatus, authorization, status); err != nil {
			return err
		}
		if request.LegacyKnowledgeAuthorizationRequired {
			// :303 - the fact stays until the author confirms.
			resolved = operationID
			return nil
		}
		// :304
		if err := chapterDeletionDeleteFacts(ctx, conn, operationID, target, runIDs); err != nil {
			return err
		}
		resolved = operationID
		return nil
	})
	if err != nil {
		return nil, err
	}
	// :309-311
	return d.requireOperation(ctx, resolved)
}

// ConfirmLegacyKnowledgeAbsent is confirmLegacyKnowledgeAbsent (:314-366), the
// whole write of chapter:confirm-legacy-knowledge-absent. The author's
// confirmation may only be consumed by a row that is still waiting for it, and
// only if the finalized fact is byte-for-byte the one the receipt froze; then
// the knowledge projection becomes 'not_required' and the fact is deleted in the
// same transaction.
//
// The reference needs no knowledge-base read here: the guard is the frozen
// receipt, the current finalized target and the current run ids. What this layer
// cannot prove is that the document is gone from the knowledge base - the
// adapter's cleaner must, and :179-188 records its verdict.
func (d *ChapterDeletions) ConfirmLegacyKnowledgeAbsent(ctx context.Context, operationID string) (*ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	err := d.immediate(ctx, func(conn *sql.Conn) error {
		row, err := chapterDeletionReadRow(ctx, conn, operationID)
		if err != nil {
			return err
		}
		if row == nil {
			// :320
			return ErrChapterDeletionOperationNotFound
		}
		if row.status != ChapterDeletionAuthorizationRequired ||
			row.legacyKnowledgeAuthorization != ChapterDeletionLegacyKnowledgeRequired {
			// :325 - nothing is waiting for a confirmation.
			return ErrChapterDeletionConfirmationConsumed
		}
		// :328
		target, err := chapterDeletionRequireTarget(ctx, conn, row.draftID, row.chapterNumber)
		if err != nil {
			return err
		}
		if target.finalizationID.String != row.finalizationID ||
			target.chapterTitle.String != row.chapterTitle ||
			target.targetFileName.String != row.targetFileName ||
			target.knowledgeDocumentID.String != "" {
			// :335 - the fact, its title, its file or its knowledge identity moved.
			return ErrChapterDeletionSourceChanged
		}
		frozenRunIDs, ok := chapterDeletionParseRunIDs(row.postProcessRunIDs)
		if !ok {
			// :339
			return ErrChapterDeletionReceiptCorrupt
		}
		currentRunIDs, err := chapterDeletionListRunIDs(ctx, conn, row.chapterNumber, row.finalizationID)
		if err != nil {
			return err
		}
		frozen, err := json.Marshal(frozenRunIDs)
		if err != nil {
			return err
		}
		current, err := json.Marshal(currentRunIDs)
		if err != nil {
			return err
		}
		if string(frozen) != string(current) {
			// :342 - a run was added or removed since the receipt was frozen.
			return ErrChapterDeletionSourceChanged
		}
		// :345-354
		result, err := conn.ExecContext(ctx, `UPDATE chapter_deletion_operations
 SET legacy_knowledge_authorization = 'consumed',
     legacy_knowledge_authorized_at = datetime('now'),
     knowledge_status = 'not_required', knowledge_error = '',
     status = 'pending', updated_at = datetime('now')
 WHERE operation_id = ?
   AND status = 'authorization_required'
   AND legacy_knowledge_authorization = 'required'`, operationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			// :356
			return ErrChapterDeletionConfirmationConsumed
		}
		// :358
		return chapterDeletionDeleteFacts(ctx, conn, operationID, target, frozenRunIDs)
	})
	if err != nil {
		return nil, err
	}
	// :362-365
	return d.requireOperation(ctx, operationID)
}

// StartAttempt is startAttempt (:400-409), the write of chapter:retry-deletion.
// The UPDATE is the original's, including its `status != 'authorization_required'`
// guard: a row still waiting for the legacy confirmation cannot be attempted, so
// a retry answers CHAPTER_DELETE_OPERATION_NOT_FOUND for it.
//
// started is false when the operation was already 'completed'. That is not a new
// rule: the service answers a completed receipt without touching it
// (chapter-deletion-service.ts:142-144) and never reaches startAttempt for it,
// so this transaction answers the same row and writes nothing rather than
// bumping attempt_count on a finished deletion. A caller that sees started false
// must not clean again and must not settle.
func (d *ChapterDeletions) StartAttempt(ctx context.Context, operationID string) (*ChapterDeletionOperation, bool, error) {
	if d == nil || d.db == nil {
		return nil, false, ErrProjectNotOpen
	}
	started := false
	err := d.immediate(ctx, func(conn *sql.Conn) error {
		row, err := chapterDeletionReadRow(ctx, conn, operationID)
		if err != nil {
			return err
		}
		if row == nil {
			// :407
			return ErrChapterDeletionOperationNotFound
		}
		if row.status == ChapterDeletionCompleted {
			// :142-144 - a finished receipt is answered, never re-opened.
			return nil
		}
		result, err := conn.ExecContext(ctx, `UPDATE chapter_deletion_operations
 SET attempt_count = attempt_count + 1, status = 'pending', updated_at = datetime('now')
 WHERE operation_id = ? AND status != 'authorization_required'`, operationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			// :407
			return ErrChapterDeletionOperationNotFound
		}
		started = true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	operation, err := d.requireOperation(ctx, operationID)
	if err != nil {
		return nil, false, err
	}
	return operation, started, nil
}

// MarkProjection is markProjection (:411-428): the adapter's cleaner reports one
// projection, and the aggregate status is refreshed in the same transaction so a
// concurrent list-incomplete-deletions cannot see a completed projection under a
// stale status. The only statuses a cleaner may report are 'completed' and
// 'failed' (:414); 'pending' is not a verdict.
func (d *ChapterDeletions) MarkProjection(ctx context.Context, operationID string, projection ChapterDeletionProjection, status, message string) (*ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	statement := ""
	switch projection {
	case ChapterDeletionManuscript:
		statement = `UPDATE chapter_deletion_operations
 SET manuscript_status = ?, manuscript_error = ?, updated_at = datetime('now')
 WHERE operation_id = ?`
	case ChapterDeletionKnowledge:
		statement = `UPDATE chapter_deletion_operations
 SET knowledge_status = ?, knowledge_error = ?, updated_at = datetime('now')
 WHERE operation_id = ?`
	default:
		// The reference's union makes this unrepresentable; it is a caller bug,
		// not a wire refusal, and it must never pick a column by accident.
		return nil, fmt.Errorf("chapter deletion projection %v is neither manuscript nor knowledge", projection)
	}
	err := d.immediate(ctx, func(conn *sql.Conn) error {
		result, err := conn.ExecContext(ctx, statement, status, message, operationID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			// :425
			return ErrChapterDeletionOperationNotFound
		}
		// :426
		return chapterDeletionRefreshAggregateStatus(ctx, conn, operationID)
	})
	if err != nil {
		return nil, err
	}
	return d.requireOperation(ctx, operationID)
}

// Settle is settle (:389-394): the last write of a delete or a retry, the
// aggregate status over the operation and its companions, and nothing else.
func (d *ChapterDeletions) Settle(ctx context.Context, operationID string) (*ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	err := d.immediate(ctx, func(conn *sql.Conn) error {
		return chapterDeletionRefreshAggregateStatus(ctx, conn, operationID)
	})
	if err != nil {
		return nil, err
	}
	// :391-393
	return d.requireOperation(ctx, operationID)
}

// Get is get (:368-371). A missing operation is (nil, nil), the original's null.
func (d *ChapterDeletions) Get(ctx context.Context, operationID string) (*ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	row, err := chapterDeletionReadRow(ctx, d.db, operationID)
	if err != nil || row == nil {
		return nil, err
	}
	return chapterDeletionRowToOperation(*row), nil
}

// GetByDraftID is getByDraftId (:373-378), the probe delete-finalized uses to
// decide between a replay and a fresh begin (chapter-deletion-service.ts:72-83).
func (d *ChapterDeletions) GetByDraftID(ctx context.Context, draftID int64) (*ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	row, err := chapterDeletionReadRowByDraft(ctx, d.db, draftID)
	if err != nil || row == nil {
		return nil, err
	}
	return chapterDeletionRowToOperation(*row), nil
}

// ListIncomplete is listIncomplete (:380-387): every deletion that is not
// completed, companions excluded, oldest first.
//
// The freeze filter the service applies around it
// (chapter-deletion-service.ts:132-135) is not applied here; see the reductions
// at the top of this file.
func (d *ChapterDeletions) ListIncomplete(ctx context.Context) ([]ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	rows, err := d.db.QueryContext(ctx, `SELECT `+chapterDeletionOperationColumns+`
 FROM chapter_deletion_operations
 WHERE status != 'completed' AND instr(operation_id, ?) = 0
 ORDER BY created_at, operation_id`, chapterDeletionSupersededSuffix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := make([]ChapterDeletionOperation, 0)
	for rows.Next() {
		row, err := chapterDeletionScan(rows)
		if err != nil {
			return nil, err
		}
		operations = append(operations, *chapterDeletionRowToOperation(row))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return operations, nil
}

// ListCompanions is listCompanions (:396-398), in draft_id order: the
// superseded receipts of one chapter whose projections the cleaner must also
// remove (chapter-deletion-service.ts:147).
func (d *ChapterDeletions) ListCompanions(ctx context.Context, operationID string) ([]ChapterDeletionOperation, error) {
	if d == nil || d.db == nil {
		return nil, ErrProjectNotOpen
	}
	rows, err := chapterDeletionCompanionRows(ctx, d.db, operationID)
	if err != nil {
		return nil, err
	}
	operations := make([]ChapterDeletionOperation, 0, len(rows))
	for _, row := range rows {
		operations = append(operations, *chapterDeletionRowToOperation(row))
	}
	return operations, nil
}

// RequiresLegacyKnowledgeAuthorization is the service's pre-computation
// (chapter-deletion-service.ts:97-104), taken before Begin because the decision
// itself is a read: a chapter whose finalization has no knowledge document but
// whose post-process run did project knowledge has a legacy document the author
// must declare absent.
//
// It ports FinalizationRepository.getByDraftId
// (electron/repositories/finalization-repository.ts:412-418) and
// PostProcessRepository.getLatestRun/getSteps
// (electron/repositories/post-process-repository.ts:91-139), including the
// numeric-chapter source remap at :94-97, so an unknown run reads as "no
// knowledge projection" - `!run ||` at :101 - and raises the guard.
func (d *ChapterDeletions) RequiresLegacyKnowledgeAuthorization(ctx context.Context, draftID, chapterNumber int64) (bool, error) {
	if d == nil || d.db == nil {
		return false, ErrProjectNotOpen
	}
	// :98-99
	var knowledgeDocumentID sql.NullString
	err := d.db.QueryRowContext(ctx, `SELECT knowledge_document_id FROM finalization_outbox WHERE draft_id = ?`, draftID).Scan(&knowledgeDocumentID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if knowledgeDocumentID.String != "" {
		return false, nil
	}
	// :100 with the :94-97 remap: a numeric chapter number is looked up through
	// the latest finalized draft of that chapter, when there is one.
	sourceID := strconv.FormatInt(chapterNumber, 10)
	if chapterNumber > 0 {
		var finalizationID string
		err = d.db.QueryRowContext(ctx, `SELECT o.finalization_id FROM drafts d
 JOIN finalization_outbox o ON o.draft_id = d.id
 WHERE d.chapter_number = ? AND d.status = 'finalized'
 ORDER BY d.version DESC, d.id DESC LIMIT 1`, chapterNumber).Scan(&finalizationID)
		if err == nil {
			sourceID = "finalization:" + finalizationID
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
	}
	var runID string
	err = d.db.QueryRowContext(ctx, `SELECT id FROM post_process_runs
 WHERE trigger_source_type = ? AND trigger_source_id = ?
 ORDER BY created_at DESC, rowid DESC LIMIT 1`, "chapter_finalize", sourceID).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		// :101 - no run at all is a knowledge projection that was promised.
		return true, nil
	}
	if err != nil {
		return false, err
	}
	// :101-102
	rows, err := d.db.QueryContext(ctx, "SELECT step_key FROM post_process_steps WHERE run_id = ? ORDER BY id ASC", runID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var stepKey string
		if err := rows.Scan(&stepKey); err != nil {
			return false, err
		}
		if stepKey == "kb_import" {
			if err := rows.Close(); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	return false, nil
}

// requireOperation is the `get(); if (!operation) throw
// CHAPTER_DELETE_OPERATION_NOT_FOUND` tail of begin, confirm and settle
// (:309-311, :362-365, :391-393).
func (d *ChapterDeletions) requireOperation(ctx context.Context, operationID string) (*ChapterDeletionOperation, error) {
	row, err := chapterDeletionReadRow(ctx, d.db, operationID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrChapterDeletionOperationNotFound
	}
	return chapterDeletionRowToOperation(*row), nil
}

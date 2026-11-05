//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's thin adapter from the durable author
// admission to the three transport providers the review/revision read binders
// borrow: transport.EditorReviewReadProvider (BindEditorReviewReads, the three
// metadata lists), transport.EditorReviewFullReadProvider
// (BindEditorReviewFullReads, the two nullable full-row reads) and
// transport.EditorReviewCycleReadProvider (BindEditorReviewCycleAbsentRead, the
// one nullable cycle read).
//
// All six operations go through borrowAuthorDatabase, the identical fence the
// draft reads, the character reads, the editor stage reads, the blueprint read
// and the continuity/post-process/plot-tree reads already use: the request's
// frozen three-key context is parsed, the caller's own cookie/tab identity is
// named, the project path must be one the startup inventory issued a catalog key
// for, and authorruntime re-proves the lease, the admission permit and the
// handle identity before lending the author's own database. Nothing here opens a
// database, resolves a path, or makes an admission decision, and no repository is
// constructed over a database that failed the pinned v7 admission.
//
// store.NewReviewRevisions(db) is exactly the repository the review/revision
// binder borrows through transport.ReviewRevisionProvider
// (internal/transport/reviewrevision.go). Five of the six names are served by its
// pure SELECT projections - ListReviews, ListRevisions twice (pending and all),
// FullReview and FullRevision - and the sixth by ReadAbsentReviewCycleTx, which
// only asks review_cycles whether a cycle row exists. None of them reserves,
// inserts, backfills or updates anything: there is no INSERT OR IGNORE, no UPDATE
// and no BEGIN IMMEDIATE on this path, so a read here commits nothing and the
// borrow's release closure is a re-verification, not a write acknowledge. The review and
// revision mutations (create, replace-pending, merge, mark-merged,
// mark-discarded) are absent here by design: they belong to the write batch
// (production_review_write_linux.go), which installs them only behind the author
// write lease. The remaining review reads (get-latest, next-index) are served by
// the singleton reads, and a lane that binds neither keeps answering
// GO_OPERATION_NOT_MIGRATED.
//
// db:review-cycle-get keeps the original's exact nullable contract: a review
// with no saved cycle answers null, and a review whose cycle row exists is
// refused by the binder itself with GO_OPERATION_NOT_MIGRATED
// (store.ErrReviewCycleProjectionUnavailable), because the complete M03 finding
// and recheck projection is not ported. Answering null there would falsely tell
// the editor that no cycle exists.
type productionReviewReads struct {
	durable *productionDurableOpen
}

// newProductionReviewReads lends the three review/revision read providers. The
// adapter is stateless: every call borrows the admitted author database for its
// own request, so a lane opened before the runtime exists (or after a refused
// startup) answers the symbolic PRODUCTION_CLOSED refusal instead of holding a
// handle this process no longer owns.
func newProductionReviewReads(durable *productionDurableOpen) *productionReviewReads {
	return &productionReviewReads{durable: durable}
}

// ReadReviewRevisionJSON serves db:review-list, db:revision-list and
// db:revision-get-pending, the three metadata lists a draft tab loads. Rows are
// serialized while the borrow is still held, so the JSON the browser receives
// was copied out of the author's own database under the live lease; a stale or
// foreign lease is refused by the fence before any SQL runs.
func (r *productionReviewReads) ReadReviewRevisionJSON(ctx context.Context, caller transport.Caller, raw any, operation string, baseDraftID int64) (json.RawMessage, error) {
	if r == nil {
		return nil, store.ErrReviewRevisionInvalidInput
	}
	switch operation {
	case "db:review-list", "db:revision-list", "db:revision-get-pending":
	default:
		return nil, store.ErrReviewRevisionInvalidInput
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, err
	}
	defer release()
	repository := store.NewReviewRevisions(db)
	var rows []map[string]any
	switch operation {
	case "db:review-list":
		rows, err = repository.ListReviews(ctx, baseDraftID)
	case "db:revision-list":
		rows, err = repository.ListRevisions(ctx, baseDraftID, false)
	case "db:revision-get-pending":
		rows, err = repository.ListRevisions(ctx, baseDraftID, true)
	}
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(store.ReviewRevisionListWire(rows))
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// ReadReviewRevisionFullJSON serves db:review-get-full and
// db:revision-get-full: one row plus the frozen source draft it was generated
// against, joined to the stored body in the same SELECT. A missing row keeps the
// original's nullable result (null) instead of becoming an error.
func (r *productionReviewReads) ReadReviewRevisionFullJSON(ctx context.Context, caller transport.Caller, raw any, operation string, id int64) (json.RawMessage, error) {
	if r == nil {
		return nil, store.ErrReviewRevisionInvalidInput
	}
	switch operation {
	case "db:review-get-full", "db:revision-get-full":
	default:
		return nil, store.ErrReviewRevisionInvalidInput
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, err
	}
	defer release()
	repository := store.NewReviewRevisions(db)
	var row map[string]any
	switch operation {
	case "db:review-get-full":
		row, err = repository.FullReview(ctx, id)
	case "db:revision-get-full":
		row, err = repository.FullRevision(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	wire, err := json.Marshal(store.ReviewRevisionFullWire(row))
	if err != nil {
		return nil, err
	}
	return wire, nil
}

// ReadAbsentReviewCycleJSON serves db:review-cycle-get. store exposes this read
// only as a caller-owned transaction projection, so the borrow lends the
// database and this method opens one deferred read-only transaction on it: the
// legacy writer lock is exclusive, so no other writer can exist and the check
// sees a committed snapshot. The transaction is rolled back, never committed,
// and the borrow is released only after that.
func (r *productionReviewReads) ReadAbsentReviewCycleJSON(ctx context.Context, caller transport.Caller, raw any, reviewID int64) (json.RawMessage, error) {
	if r == nil {
		return nil, store.ErrReviewRevisionInvalidInput
	}
	db, release, err := r.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	readErr := store.ReadAbsentReviewCycleTx(ctx, tx, reviewID)
	rollbackErr := tx.Rollback()
	if readErr != nil {
		// ErrReviewCycleProjectionUnavailable must survive as the sentinel the
		// binder maps to GO_OPERATION_NOT_MIGRATED, so it is returned unwrapped.
		return nil, readErr
	}
	if rollbackErr != nil {
		return nil, rollbackErr
	}
	return json.RawMessage("null"), nil
}

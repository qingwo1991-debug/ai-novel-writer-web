//go:build linux

package main

import (
	"context"
	"encoding/json"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The review/revision read half of the editor, served from the author's own
// admitted database under the caller's live lease.
//
// Six inventoried names are bound here and nothing else:
//
//	db:review-list, db:revision-list, db:revision-get-pending   (metadata lists)
//	db:review-get-full, db:revision-get-full                    (one full row)
//	db:review-cycle-get                                        (absent cycle)
//
// Each one is a SELECT-only projection of rows an earlier app version already
// committed: the store functions behind them
// (store.ReadReviewRevisionListTx, store.ReadReviewRevisionFullTx,
// store.ReadAbsentReviewCycleTx) issue no INSERT, no UPDATE and no DELETE, open
// no provider client, reserve no ledger attempt and dispatch nothing. They run
// inside authorruntime.ReadLifecycle.Read, so the lease fence, the source proof
// and the whole statement run while the read-only SQLite transaction is held: a
// stale, foreign or absent lease is refused before any row is read, and a
// database that failed the pinned v7 admission is never opened because this
// adapter owns no connection of its own.
//
// Everything else in the family keeps failing closed at 501:
//   - the review/revision mutations (db:review-create, db:revision-create,
//     db:revision-replace-pending, db:revision-merge, db:revision-mark-merged,
//     db:revision-mark-discarded) write the author's database;
//   - db:review-get-latest and db:review-next-index are reads, but their only
//     transport seam is BindReviewRevisionRemainder, which borrows a
//     write-capable handle through BeginReviewRevisions instead of the read-only
//     lifecycle transaction. Binding them through that seam would answer a read
//     on a write permit, so they stay unbound until a read-tx path exists;
//   - db:review-cycle-list-pending-rechecks and review-revision:read-recovery
//     have no store implementation in this tree;
//   - finalization-generation:read is a projection of the generation graph
//     ledger (synthetic_generation_linux.go), not of the author database.
//
// A store that is asked for a review cycle that really exists still answers
// 501 REVIEW_CYCLE_PROJECTION_UNAVAILABLE (BindEditorReviewCycleAbsentRead), so
// the bound cycle read can only return the honest "no cycle yet" null.

// ReadReviewRevisionJSON serves db:review-list, db:revision-list and
// db:revision-get-pending for one base draft.
func (d *productionDurableOpen) ReadReviewRevisionJSON(ctx context.Context, caller transport.Caller, raw any, operation string, baseDraftID int64) (json.RawMessage, error) {
	return d.readDurableJSON(ctx, caller, raw, func(lifecycle *authorruntime.ReadLifecycle, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
		return lifecycle.ReadReviewRevisionJSON(ctx, owner, expected, operation, baseDraftID)
	})
}

// ReadReviewRevisionFullJSON serves db:review-get-full and
// db:revision-get-full for one row id.
func (d *productionDurableOpen) ReadReviewRevisionFullJSON(ctx context.Context, caller transport.Caller, raw any, operation string, id int64) (json.RawMessage, error) {
	return d.readDurableJSON(ctx, caller, raw, func(lifecycle *authorruntime.ReadLifecycle, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
		return lifecycle.ReadReviewRevisionFullJSON(ctx, owner, expected, operation, id)
	})
}

// ReadAbsentReviewCycleJSON serves db:review-cycle-get: null only for a review
// with no saved cycle, a refusal for one that has a cycle.
func (d *productionDurableOpen) ReadAbsentReviewCycleJSON(ctx context.Context, caller transport.Caller, raw any, reviewID int64) (json.RawMessage, error) {
	return d.readDurableJSON(ctx, caller, raw, func(lifecycle *authorruntime.ReadLifecycle, owner projectsession.Owner, expected projectsession.Context) (json.RawMessage, error) {
		return lifecycle.ReadAbsentReviewCycleJSON(ctx, owner, expected, reviewID)
	})
}

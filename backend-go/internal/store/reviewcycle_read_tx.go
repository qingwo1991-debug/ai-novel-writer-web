package store

import (
	"context"
	"database/sql"
	"errors"
)

// ErrReviewCycleProjectionUnavailable marks a saved review cycle whose M03
// finding and recheck projection has not yet been ported. Returning null for
// that row would falsely tell the editor that no cycle exists.
var ErrReviewCycleProjectionUnavailable = errors.New("REVIEW_CYCLE_PROJECTION_UNAVAILABLE")

// ReadAbsentReviewCycleTx preserves the original nullable result only when
// the requested review has no cycle. Existing cycles require the complete M03
// projection, including receipt checks and finding-to-report item mapping.
func ReadAbsentReviewCycleTx(ctx context.Context, tx *sql.Tx, reviewID int64) error {
	if ctx == nil || tx == nil || reviewID < 1 || reviewID > 9007199254740991 {
		return ErrReviewRevisionInvalidInput
	}
	var present int
	err := tx.QueryRowContext(ctx, "SELECT 1 FROM review_cycles WHERE review_id=? LIMIT 1", reviewID).Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return ErrReviewCycleProjectionUnavailable
}

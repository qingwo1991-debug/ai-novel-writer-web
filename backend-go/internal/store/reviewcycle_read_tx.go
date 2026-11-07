package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

// reviewPendingRecheckLimit bounds the candidate set this read projects. The
// reference calls .all() on a query the editor runs for one merge flow, so the
// practical answer is a handful of rows; a corrupt or adversarial database must
// not be able to make one browser read allocate an unbounded result. The bound
// is applied after the reference's own predicate and preserves its order, so it
// can only truncate the tail, never reorder or invent a row.
const reviewPendingRecheckLimit = 200

// reviewPendingRecheckColumns is the reference's SELECT list
// (electron/repositories/review-cycle-repository.ts:507-508) with one addition:
// rc.root_action_id, which the reference reads from the cycle row inside
// planRecheck rather than from this query, and which the wire shape needs.
const reviewPendingRecheckColumns = `rc.cycle_id AS cycleId,rc.review_id AS reviewId,
 rc.root_action_id AS rootActionId,rc.merged_hash AS mergedHash,
 d.id AS id,d.chapter_number AS chapterNumber,d.version AS version,
 d.status AS status,c.body AS content`

// reviewPendingRecheckSQL is the reference's query verbatim, plus the LIMIT the
// reference does not have. The quoted reference
// (electron/repositories/review-cycle-repository.ts:507-512) is:
//
//	SELECT rc.cycle_id,rc.review_id,rc.merged_hash,
//	    d.id,d.chapter_number,d.version,d.status,c.body
//	  FROM review_cycles rc JOIN revisions v ON v.id=rc.revision_id
//	  JOIN drafts d ON d.id=v.merged_to_draft_id JOIN contents c ON c.id=d.content_id
//	  WHERE rc.revision_status='merge-committed' AND rc.recheck_count=0 AND d.status='revised'
//	  ORDER BY d.chapter_number,d.id,rc.cycle_id
const reviewPendingRecheckSQL = `SELECT ` + reviewPendingRecheckColumns + `
 FROM review_cycles rc JOIN revisions v ON v.id=rc.revision_id
 JOIN drafts d ON d.id=v.merged_to_draft_id JOIN contents c ON c.id=d.content_id
 WHERE rc.revision_status='merge-committed' AND rc.recheck_count=0 AND d.status='revised'
 ORDER BY d.chapter_number,d.id,rc.cycle_id LIMIT ?`

// reviewCycleCurrentMergeSQL is the reference's isCurrentMergedCycle query
// (electron/repositories/review-cycle-repository.ts:446-450): the revision's own
// index, the newest merged index of its base draft, and the base draft body the
// merged hash must describe.
const reviewCycleCurrentMergeSQL = `SELECT c.body AS body,v.revision_index AS revisionIndex,
 (SELECT MAX(newer.revision_index) FROM revisions newer
   WHERE newer.base_draft_id=v.base_draft_id AND newer.status='merged') AS latestMergedIndex
 FROM review_cycles rc JOIN revisions v ON v.id=rc.revision_id
 JOIN drafts d ON d.id=v.base_draft_id JOIN contents c ON c.id=d.content_id
 WHERE rc.cycle_id=?`

// ListPendingReviewRechecks is db:review-cycle-list-pending-rechecks: the review
// cycles the reference's own query calls pending, in the reference's own order,
// each one still proved to be the current merged cycle of its draft.
//
// It is one read: a table-existence probe, one SELECT over the four tables the
// reference joins, and one isCurrentMergedCycle SELECT per bounded candidate. No
// transaction is opened, nothing is inserted, updated or deleted, and the caller's
// borrowed handle is never written. The table probe mirrors the reference's own
// early return when review_cycles does not exist (:506): a legacy database
// answers an empty list instead of an error, which is the one place the
// reference prefers absence to failure.
//
// Deliberately NOT ported: the reference's planRecheck disposition gate
// (:518-519, :747-756). That gate re-proves the merged body against the stored
// merged hash or the review_cycle_merges body, verifies the complete M03 finding
// and confirmation projection, and classifies each finding's evidence to decide
// whether a recheck is "required" at all. None of that owner exists in this
// package, so this list is the reference's candidate set after its
// isCurrentMergedCycle gate - a superset of the reference's answer for a cycle
// whose findings' evidence did not change. The command layer that exposes this
// list must therefore report the disposition as required (the only disposition
// this lane can prove) and must not claim that the reference's evidence
// classification ran.
func ListPendingReviewRechecks(ctx context.Context, db *sql.DB) ([]map[string]any, error) {
	if ctx == nil || db == nil {
		return nil, ErrReviewRevisionProjectNotOpen
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var present int
	err := db.QueryRowContext(ctx,
		"SELECT 1 FROM sqlite_master WHERE type='table' AND name='review_cycles' LIMIT 1").Scan(&present)
	if errors.Is(err, sql.ErrNoRows) {
		return []map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := rrMaps(ctx, db, reviewPendingRecheckSQL, reviewPendingRecheckLimit)
	if err != nil {
		return nil, err
	}
	kept := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		current, err := reviewCycleCurrentMerge(ctx, db, glText(row["cycleId"]), glText(row["mergedHash"]))
		if err != nil {
			return nil, err
		}
		if !current {
			continue
		}
		kept = append(kept, row)
	}
	return kept, nil
}

// reviewCycleCurrentMerge is the reference's isCurrentMergedCycle
// (electron/repositories/review-cycle-repository.ts:445-453): the cycle's
// revision must be the newest merged revision of its base draft, and the
// merged_hash must be the hex SHA-256 of that base draft's body. A missing hash,
// a superseded revision, a missing merge, or a hash that no longer describes the
// draft all answer false instead of a partial row. An error is never folded into
// false: a read that cannot prove the row must not make the row disappear
// silently.
func reviewCycleCurrentMerge(ctx context.Context, db *sql.DB, cycleID, mergedHash string) (bool, error) {
	if cycleID == "" || mergedHash == "" {
		return false, nil
	}
	rows, err := rrMaps(ctx, db, reviewCycleCurrentMergeSQL, cycleID)
	if err != nil {
		return false, err
	}
	if len(rows) != 1 {
		return false, nil
	}
	body, bodyOK := rows[0]["body"].(string)
	index, indexOK := rrInteger(rows[0]["revisionIndex"], 0)
	latest, latestOK := rrInteger(rows[0]["latestMergedIndex"], 0)
	if !bodyOK || !indexOK || !latestOK || index != latest {
		return false, nil
	}
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:]) == mergedHash, nil
}

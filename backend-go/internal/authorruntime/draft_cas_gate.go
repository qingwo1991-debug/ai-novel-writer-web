//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

var ErrDraftCASAuthorityIncomplete = errors.New("AUTHOR_DRAFT_CAS_AUTHORITY_INCOMPLETE")

// DraftCASIntent is a checked request for the original draft-body CAS contract.
// It holds no database handle and cannot commit the request to the author tree.
// A future publisher must add receipt fencing to the same source transaction
// as store.Drafts.UpdateContentIfCurrent, then prove readback and complete
// project rollback before releasing exclusion.
type DraftCASIntent struct {
	plan            *SQLitePublicationPlan
	id              int64
	content         string
	wordCount       int64
	expectedContent string
}

// PrepareDraftCAS rejects an absent baseline, stale or read-only draft, a
// finalization receipt, and shared content ownership in the admitted copy.
// The check is advisory until repeated inside the future source transaction.
func (plan *SQLitePublicationPlan) PrepareDraftCAS(ctx context.Context, id int64, content string, wordCount int64, expectedContent *string) (*DraftCASIntent, error) {
	if expectedContent == nil {
		return nil, store.ErrDraftContentConflict
	}
	if id <= 0 {
		return nil, store.ErrDraftNotFound
	}
	intent := &DraftCASIntent{plan: plan, id: id, content: content, wordCount: wordCount, expectedContent: *expectedContent}
	if err := intent.Verify(ctx); err != nil {
		return nil, err
	}
	return intent, nil
}

// Verify rechecks the entire publication plan on both sides of one read-only
// transaction and confirms the frozen CAS request still matches that snapshot.
func (intent *DraftCASIntent) Verify(ctx context.Context) error {
	if intent == nil || intent.plan == nil || ctx == nil {
		return ErrDraftCASAuthorityIncomplete
	}
	if err := intent.plan.Verify(ctx); err != nil {
		return err
	}
	err := intent.plan.reader.Read(ctx, func(tx *sql.Tx) error {
		var rawStatus, rawBody any
		var contentID int64
		err := tx.QueryRowContext(ctx, `SELECT d.status, d.content_id, c.body
 FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, intent.id).Scan(&rawStatus, &contentID, &rawBody)
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrDraftNotFound
		}
		if err != nil {
			return ErrDraftCASAuthorityIncomplete
		}
		status, statusOK := rawStatus.(string)
		body, bodyOK := rawBody.(string)
		if !statusOK || !bodyOK || !store.IsWorkingDraftStatus(status) || body != intent.expectedContent {
			return store.ErrDraftContentConflict
		}
		// A receipt makes the body authoritative even if stale metadata still
		// labels it a working draft. Missing receipt schema is an unknown state.
		var receipts int64
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id = ?", intent.id).Scan(&receipts); err != nil {
			return ErrDraftCASAuthorityIncomplete
		}
		if receipts != 0 {
			return store.ErrDraftContentConflict
		}
		for _, check := range []struct {
			query string
			args  []any
		}{
			{"SELECT COUNT(*) FROM drafts WHERE content_id = ? AND id <> ?", []any{contentID, intent.id}},
			{"SELECT COUNT(*) FROM reviews WHERE content_id = ?", []any{contentID}},
			{"SELECT COUNT(*) FROM revisions WHERE content_id = ?", []any{contentID}},
		} {
			var aliases int64
			if err := tx.QueryRowContext(ctx, check.query, check.args...).Scan(&aliases); err != nil {
				return ErrDraftCASAuthorityIncomplete
			}
			if aliases != 0 {
				return store.ErrDraftContentConflict
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return intent.plan.Verify(ctx)
}

// PublicationAuthority remains closed. A private-copy CAS preflight does not
// provide a writable source, a journaled restore, or knowledge publication.
func (intent *DraftCASIntent) PublicationAuthority(ctx context.Context) error {
	if err := intent.Verify(ctx); err != nil {
		return err
	}
	return ErrPublicationAuthorityUnavailable
}

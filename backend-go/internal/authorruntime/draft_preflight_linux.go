//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const maxPreflightDraftBytes = 16 << 20
const maxPreflightSafeInteger int64 = 9007199254740991

// PreflightDraftCAS rejects an ordinary stale or read-only save before the
// one-shot publisher enters CommitDrained. Its snapshot is advisory: the
// publisher repeats the CAS against the private staged source under its fence.
func (l *ReadLifecycle) PreflightDraftCAS(ctx context.Context, owner projectsession.Owner,
	expected projectsession.Context, draftID int64, content string, wordCount int64,
	expectedContent *string) error {
	if l == nil || ctx == nil || draftID <= 0 || draftID > maxPreflightSafeInteger ||
		wordCount < 0 || wordCount > maxPreflightSafeInteger || expectedContent == nil ||
		!utf8.ValidString(content) || !utf8.ValidString(*expectedContent) ||
		len(content) > maxPreflightDraftBytes || len(*expectedContent) > maxPreflightDraftBytes {
		return ErrInvalid
	}
	return l.Read(ctx, owner, expected, func(tx *sql.Tx) error {
		return preflightDraftCASReadTx(ctx, tx, draftID, *expectedContent)
	})
}

func preflightDraftCASReadTx(ctx context.Context, tx *sql.Tx, draftID int64, expectedContent string) error {
	if ctx == nil || tx == nil || draftID <= 0 {
		return ErrInvalid
	}
	var rawStatus, rawBody any
	var contentID int64
	err := tx.QueryRowContext(ctx, `SELECT d.status, d.content_id, c.body
 FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = ?`, draftID).Scan(&rawStatus, &contentID, &rawBody)
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrDraftNotFound
	}
	if err != nil {
		return ErrDraftCASAuthorityIncomplete
	}
	status, statusOK := rawStatus.(string)
	body, bodyOK := rawBody.(string)
	if !statusOK || !bodyOK || !store.IsWorkingDraftStatus(status) || body != expectedContent {
		return store.ErrDraftContentConflict
	}
	var receipts int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM finalization_outbox WHERE draft_id = ?", draftID).Scan(&receipts); err != nil {
		return ErrDraftCASAuthorityIncomplete
	}
	if receipts != 0 {
		return store.ErrDraftContentConflict
	}
	var aliases int64
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM drafts WHERE content_id = ? AND id <> ?", contentID, draftID).Scan(&aliases); err != nil {
		return ErrDraftCASAuthorityIncomplete
	}
	if aliases != 0 {
		return store.ErrDraftContentConflict
	}
	// Supported pre-review layouts may not have these tables. Follow the
	// source CAS: skip only an absence confirmed by sqlite_master. A present
	// table without the expected ownership column is an unknown state.
	for _, table := range []string{"reviews", "revisions"} {
		var exists int
		err := tx.QueryRowContext(ctx, "SELECT 1 FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&exists)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil || exists != 1 {
			return ErrDraftCASAuthorityIncomplete
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table+" WHERE content_id = ?", contentID).Scan(&aliases); err != nil {
			return ErrDraftCASAuthorityIncomplete
		}
		if aliases != 0 {
			return store.ErrDraftContentConflict
		}
	}
	return nil
}

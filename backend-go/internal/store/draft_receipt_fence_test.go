package store

import (
	"context"
	"errors"
	"testing"
)

func TestUpdateContentIfCurrentRejectsReceiptOnWorkingDraft(t *testing.T) {
	for _, status := range []string{"draft", "revised", "reviewed"} {
		t.Run(status, func(t *testing.T) {
			db, drafts, _ := newFixture(t)
			seed(t, db, 91, 4, 1, status, "old body")
			mustExec(t, db, `INSERT INTO finalization_outbox
				(finalization_id, draft_id, chapter_number, chapter_title, content_hash, content_snapshot)
				VALUES('frozen-91', 91, 4, 'chapter', ?, 'old body')`, textHash("old body"))
			err := drafts.UpdateContentIfCurrent(context.Background(), 91, "new body", 88, pointer("old body"))
			if !errors.Is(err, ErrDraftContentConflict) {
				t.Fatalf("receipted working draft save = %v, want conflict", err)
			}
			assertStored(t, db, 91, "old body", 11, "updated-original")
			var receipt string
			if err := db.QueryRow("SELECT content_snapshot FROM finalization_outbox WHERE draft_id = 91").Scan(&receipt); err != nil || receipt != "old body" {
				t.Fatalf("receipt changed or missing: %q, %v", receipt, err)
			}
		})
	}
}

func TestUpdateContentIfCurrentRefusesUnknownReceiptSchema(t *testing.T) {
	db, drafts, _ := newFixture(t)
	seed(t, db, 92, 4, 1, "draft", "old body")
	mustExec(t, db, "DROP TABLE finalization_outbox")
	err := drafts.UpdateContentIfCurrent(context.Background(), 92, "new body", 88, pointer("old body"))
	if err == nil || errors.Is(err, ErrDraftContentConflict) {
		t.Fatalf("unknown receipt schema returned %v", err)
	}
	assertStored(t, db, 92, "old body", 11, "updated-original")
}

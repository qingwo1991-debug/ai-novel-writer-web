package store

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestFinalizationKnowledgeLinkNeedsObservedOwnerReceipt(t *testing.T) {
	db, repo, proof := foFixture(t)
	ctx := context.Background()
	if _, err := repo.CommitWithOwnerProof(ctx, foInput(), proof); err != nil {
		t.Fatal(err)
	}
	before := foState(t, db)
	if _, err := repo.LinkKnowledgeDocumentWithOwnerProof(ctx, foInput().DraftID, "  kb-doc-1  ", nil); !errors.Is(err, ErrFinalizationOwnerRequired) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, foState(t, db)) {
		t.Fatal("unauthorized knowledge linkage wrote")
	}
	if _, err := repo.LinkKnowledgeDocumentWithOwnerProof(ctx, foInput().DraftID, "\ufeff  ", proof); !errors.Is(err, ErrFinalizationKnowledgeDocumentRequired) {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, foState(t, db)) {
		t.Fatal("empty knowledge linkage wrote")
	}

	called := 0
	repo.owner = foTestVerifier(func(ctx context.Context, conn *sql.Conn, scope FinalizationScope, mutation FinalizationMutation, actual any) error {
		called++
		if actual != proof || scope != foTestScope || mutation.Action != "knowledge-link" || mutation.KnowledgeDocumentID != "kb-doc-1" || mutation.Existing == nil || mutation.Existing.ContentSnapshot != foInput().Content {
			t.Fatal("unbound knowledge receipt", mutation)
		}
		var frozen string
		if err := conn.QueryRowContext(ctx, "SELECT content_snapshot FROM finalization_outbox WHERE draft_id=?", foInput().DraftID).Scan(&frozen); err != nil || frozen != mutation.Existing.ContentSnapshot {
			t.Fatal("verifier lost transaction snapshot", err)
		}
		return nil
	})
	linked, err := repo.LinkKnowledgeDocumentWithOwnerProof(ctx, foInput().DraftID, "  kb-doc-1  ", proof)
	if err != nil || linked.KnowledgeDocumentID != "kb-doc-1" || called != 1 {
		t.Fatal(linked, err, called)
	}
	chapters, err := repo.KnowledgeDocumentChapters(ctx)
	if err != nil || chapters["kb-doc-1"] != foInput().ChapterNumber || len(chapters) != 1 {
		t.Fatal(chapters, err)
	}
}

func TestFinalizationKnowledgeLinkRejectsCorruptOrFailedOutbox(t *testing.T) {
	for _, problem := range []string{"corrupt-hash", "missing", "update-abort"} {
		t.Run(problem, func(t *testing.T) {
			db, repo, proof := foFixture(t)
			ctx := context.Background()
			if _, err := repo.CommitWithOwnerProof(ctx, foInput(), proof); err != nil {
				t.Fatal(err)
			}
			var expected error
			switch problem {
			case "corrupt-hash":
				mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=?", strings.Repeat("0", 64), foInput().DraftID)
				expected = ErrFinalizationSnapshotHashMismatch
			case "missing":
				mustExec(t, db, "DELETE FROM finalization_outbox WHERE draft_id=?", foInput().DraftID)
				expected = ErrFinalizationNotFound
			case "update-abort":
				mustExec(t, db, "CREATE TRIGGER synthetic_link_abort BEFORE UPDATE OF knowledge_document_id ON finalization_outbox BEGIN SELECT RAISE(ABORT,'synthetic-link-abort'); END")
			}
			before := foState(t, db)
			_, err := repo.LinkKnowledgeDocumentWithOwnerProof(ctx, foInput().DraftID, "kb-doc-1", proof)
			if expected != nil && !errors.Is(err, expected) || expected == nil && (err == nil || !strings.Contains(err.Error(), "synthetic-link-abort")) {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, foState(t, db)) {
				t.Fatal("failed knowledge link changed database")
			}
		})
	}
}

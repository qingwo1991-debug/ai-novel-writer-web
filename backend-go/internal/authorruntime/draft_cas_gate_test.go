//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestDraftCASIntentPreservesLegacyBaselineAndRejectsReceipts(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	var nextChapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number), 0) + 1 FROM drafts").Scan(&nextChapter); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	contentIDs := make([]int64, 4)
	for i, body := range []string{"", "final", "receipted", "shared"} {
		result, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", body)
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
		contentIDs[i], err = result.LastInsertId()
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
	}
	draftIDs := make([]int64, 5)
	for i, row := range []struct {
		status    string
		contentID int64
	}{
		{"draft", contentIDs[0]},
		{"finalized", contentIDs[1]},
		{"draft", contentIDs[2]},
		{"reviewed", contentIDs[3]},
		{"draft", contentIDs[3]},
	} {
		result, err := seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(?,?,?,?)", nextChapter+int64(i), 1, row.status, row.contentID)
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
		draftIDs[i], err = result.LastInsertId()
		if err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
	}
	if _, err := seed.Exec(`INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,content_hash,content_revision,target_file_name)
 VALUES(?,?,?,?,?,?)`, "stale-working-receipt", draftIDs[2], nextChapter+2, "synthetic-hash", 1, "three.txt"); err != nil {
		_ = seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	config.Knowledge = emptyKnowledgeConfig()
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	parent := t.TempDir()
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	backup, err := reader.SealDatabaseSnapshot(context.Background(), parent, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	plan, err := reader.PrepareSQLitePublicationPlan(context.Background(), config.Schema, backup)
	if err != nil {
		t.Fatal(err)
	}
	empty := ""
	intent, err := plan.PrepareDraftCAS(context.Background(), draftIDs[0], "new text", 2, &empty)
	if err != nil {
		t.Fatal(err)
	}
	if err := intent.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := intent.PublicationAuthority(context.Background()); !errors.Is(err, ErrPublicationAuthorityUnavailable) {
		t.Fatalf("private CAS preflight granted author write: %v", err)
	}
	for _, check := range []struct {
		name     string
		id       int64
		baseline *string
		want     error
	}{
		{"missing-baseline", draftIDs[0], nil, store.ErrDraftContentConflict},
		{"stale-baseline", draftIDs[0], pointerDraftBaseline("old"), store.ErrDraftContentConflict},
		{"finalized", draftIDs[1], pointerDraftBaseline("final"), store.ErrDraftContentConflict},
		{"receipted-working", draftIDs[2], pointerDraftBaseline("receipted"), store.ErrDraftContentConflict},
		{"shared-content", draftIDs[3], pointerDraftBaseline("shared"), store.ErrDraftContentConflict},
		{"missing-draft", draftIDs[4] + 1000000, pointerDraftBaseline(""), store.ErrDraftNotFound},
	} {
		t.Run(check.name, func(t *testing.T) {
			if _, err := plan.PrepareDraftCAS(context.Background(), check.id, "forbidden", 9, check.baseline); !errors.Is(err, check.want) {
				t.Fatalf("unexpected CAS preflight result: %v", err)
			}
		})
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("CAS intent changed author bytes: %v", err)
	}
}

func pointerDraftBaseline(value string) *string { return &value }

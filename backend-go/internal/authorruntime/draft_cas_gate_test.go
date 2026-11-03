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
	statements := []string{
		"INSERT INTO contents(id,body) VALUES(1,''),(2,'final'),(3,'receipted'),(4,'shared')",
		`INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES
 (1,1,1,'draft',1),(2,2,1,'finalized',2),(3,3,1,'draft',3),
 (4,4,1,'reviewed',4),(5,5,1,'draft',4)`,
		`INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,content_hash,content_revision,target_file_name)
 VALUES('stale-working-receipt',3,3,'synthetic-hash',1,'three.txt')`,
	}
	for _, statement := range statements {
		if _, err := seed.Exec(statement); err != nil {
			_ = seed.Close()
			t.Fatal(err)
		}
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
	intent, err := plan.PrepareDraftCAS(context.Background(), 1, "new text", 2, &empty)
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
		{"missing-baseline", 1, nil, store.ErrDraftContentConflict},
		{"stale-baseline", 1, pointerDraftBaseline("old"), store.ErrDraftContentConflict},
		{"finalized", 2, pointerDraftBaseline("final"), store.ErrDraftContentConflict},
		{"receipted-working", 3, pointerDraftBaseline("receipted"), store.ErrDraftContentConflict},
		{"shared-content", 4, pointerDraftBaseline("shared"), store.ErrDraftContentConflict},
		{"missing-draft", 99, pointerDraftBaseline(""), store.ErrDraftNotFound},
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

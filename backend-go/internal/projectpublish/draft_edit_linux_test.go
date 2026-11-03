//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

const privateDraftSchema = `
CREATE TABLE contents (id INTEGER PRIMARY KEY, body TEXT NOT NULL);
CREATE TABLE drafts (
 id INTEGER PRIMARY KEY, chapter_number INTEGER NOT NULL, version INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'draft', source TEXT NOT NULL DEFAULT 'write',
 content_id INTEGER NOT NULL, word_count INTEGER NOT NULL DEFAULT 0,
 source_dependencies TEXT NOT NULL DEFAULT '[]',
 created_at TEXT NOT NULL DEFAULT 'created-original',
 updated_at TEXT NOT NULL DEFAULT 'updated-original'
);
CREATE TABLE finalization_outbox (
 finalization_id TEXT PRIMARY KEY, draft_id INTEGER NOT NULL UNIQUE,
 chapter_number INTEGER NOT NULL, chapter_title TEXT NOT NULL DEFAULT '',
 content_hash TEXT NOT NULL, content_snapshot TEXT
);
INSERT INTO contents(id,body) VALUES(1,'old draft');
INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count)
 VALUES(1,1,1,'draft',1,9);`

func privateDraftFixture(t *testing.T) fixture {
	t.Helper()
	f := newFixture(t)
	path := filepath.Join(f.storage, projectfs.DatabaseName)
	for _, suffix := range []string{"", "-wal"} {
		if err := os.Remove(path + suffix); err != nil {
			t.Fatal(err)
		}
	}
	db, err := sql.Open("sqlite3", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(privateDraftSchema); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPrivateDraftCASDoesNotTouchAuthorSource(t *testing.T) {
	f := privateDraftFixture(t)
	path := filepath.Join(f.storage, projectfs.DatabaseName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	baseline := "old draft"
	evidence, err := snapshot.EditDraftCASPrivate(context.Background(), 1, "new private words", 17, &baseline)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.DraftID != 1 || evidence.WordCount != 17 ||
		evidence.BeforeStageTreeSHA256 == evidence.AfterStageTreeSHA256 ||
		evidence.SourceTreeSHA256 != evidence.BeforeStageTreeSHA256 {
		t.Fatalf("incomplete private CAS evidence: %+v", evidence)
	}
	if err := evidence.PublicationAuthority(context.Background()); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("private edit authorized source publication: %v", err)
	}
	if _, err := evidence.VerifyEditedDraftCAS(context.Background(), EditedStageFacts{ProjectID: "foreign"}); !errors.Is(err, ErrChanged) {
		t.Fatalf("foreign edited-stage facts accepted: %v", err)
	}
	journal, err := PrepareEditedExchangeJournal(context.Background(), snapshot,
		HandleDrainFunc(func(context.Context, string) error { return nil }), evidence)
	if err != nil || journal == nil {
		t.Fatalf("private CAS evidence could not bind a PREPARED marker: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("author DB mutated: %v", err)
	}
	if err := snapshot.VerifySource(context.Background()); err != nil {
		t.Fatalf("author source changed: %v", err)
	}
	if err := snapshot.VerifyPrivateCopy(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("edited private stage still considered original snapshot: %v", err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	read, err := openPrivateDraftDB(filepath.Join(stage, projectfs.DatabaseName), "ro")
	if err != nil {
		t.Fatal(err)
	}
	var body string
	if err := read.QueryRow("SELECT body FROM contents WHERE id=1").Scan(&body); err != nil {
		_ = read.Close()
		t.Fatal(err)
	}
	if err := read.Close(); err != nil || body != "new private words" {
		t.Fatalf("private CAS not persisted: %q %v", body, err)
	}
}

func TestPrivateDraftCASRefusesConflictsAndChangedTrees(t *testing.T) {
	for _, variant := range []string{"missing-baseline", "stale-baseline", "receipted", "stage-changed", "source-changed"} {
		t.Run(variant, func(t *testing.T) {
			f := privateDraftFixture(t)
			if variant == "receipted" {
				db, err := sql.Open("sqlite3", filepath.Join(f.storage, projectfs.DatabaseName))
				if err != nil {
					t.Fatal(err)
				}
				_, err = db.Exec(`INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,content_hash) VALUES('receipt',1,1,'hash')`)
				closeErr := db.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()
			baseline := "old draft"
			var expected *string = &baseline
			switch variant {
			case "missing-baseline":
				expected = nil
			case "stale-baseline":
				wrong := "stale"
				expected = &wrong
			case "stage-changed":
				stage, err := snapshot.PrivatePath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(stage, "unknown-author-file.bin"), []byte("tampered"), 0600); err != nil {
					t.Fatal(err)
				}
			case "source-changed":
				if err := os.WriteFile(filepath.Join(f.storage, "unknown-author-file.bin"), []byte("changed source"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err = snapshot.EditDraftCASPrivate(context.Background(), 1, "forbidden", 9, expected)
			want := store.ErrDraftContentConflict
			if variant == "missing-baseline" {
				want = ErrInvalid
			}
			if variant == "stage-changed" || variant == "source-changed" {
				want = ErrChanged
			}
			if !errors.Is(err, want) {
				t.Fatalf("%s accepted or wrong denial: %v", variant, err)
			}
		})
	}
}

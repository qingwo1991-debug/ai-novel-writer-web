package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// Deliberately synthetic, minimal SQL, with no project opens or migrations.
const fixtureSchema = `
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
);`

func newFixture(t *testing.T) (*sql.DB, *Drafts, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "synthetic #? 中文.sqlite")
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, fixtureSchema)
	return db, NewDrafts(db), path
}

func mustExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seed(t *testing.T, db *sql.DB, id, chapter, version int64, status, body string) {
	t.Helper()
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, body)
	mustExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count)
 VALUES(?,?,?,?,?,?)`, id, chapter, version, status, id, 11)
}

func textHash(text string) string {
	hash := sha256.Sum256([]byte(text))
	return hex.EncodeToString(hash[:])
}

func deps(t *testing.T, db *sql.DB, id int64, dependencies ...DraftSourceDependency) {
	t.Helper()
	body, err := json.Marshal(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, db, "UPDATE drafts SET source_dependencies=? WHERE id=?", string(body), id)
}

func pointer(value string) *string { return &value }
func number(value int64) *int64    { return &value }

func assertStored(t *testing.T, db *sql.DB, id int64, body string, count int64, updated string) {
	t.Helper()
	var gotBody, gotUpdated string
	var gotCount int64
	err := db.QueryRow(`SELECT c.body, d.word_count, d.updated_at FROM drafts d
 JOIN contents c ON c.id=d.content_id WHERE d.id=?`, id).Scan(&gotBody, &gotCount, &gotUpdated)
	if err != nil {
		t.Fatal(err)
	}
	if gotBody != body || gotCount != count || (updated != "" && gotUpdated != updated) {
		t.Fatalf("stored = (%q,%d,%q), want (%q,%d,%q)", gotBody, gotCount, gotUpdated, body, count, updated)
	}
}

func assertStale(t *testing.T, repository *Drafts, id int64, stale bool) {
	t.Helper()
	meta, err := repository.GetMeta(context.Background(), id)
	if err != nil || meta == nil {
		t.Fatalf("GetMeta(%d) = %v, %v", id, meta, err)
	}
	if meta.DependenciesStale != stale {
		t.Fatalf("GetMeta(%d).DependenciesStale = %v, want %v", id, meta.DependenciesStale, stale)
	}
}

func TestDraftReadsOrderingTitlesAndContent(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 2, 1, "finalized", "第二章\r\n😀")
	seed(t, db, 2, 1, 2, "superseded", "被替换")
	seed(t, db, 3, 1, 1, "reviewed", "候选")
	seed(t, db, 4, 3, 1, "finalized", "空白标题")
	mustExec(t, db, `INSERT INTO finalization_outbox VALUES
 ('receipt-1',1,2,' 权威标题 ',?,?),('receipt-2',2,1,'替换标题',?,?),
 ('receipt-3',3,1,'不可暴露',?,?),('receipt-4',4,3,?, ?,?)`,
		textHash("第二章\r\n😀"), "第二章\r\n😀", textHash("被替换"), "被替换",
		textHash("候选"), "候选", "\ufeff \t", textHash("空白标题"), "空白标题")
	all, err := repository.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].ID != 3 || all[1].ID != 2 || all[2].ID != 1 || all[3].ID != 4 {
		t.Fatalf("unexpected order: %+v", all)
	}
	if all[0].ChapterTitle != "" || all[1].ChapterTitle != "替换标题" || all[2].ChapterTitle != " 权威标题 " || all[3].ChapterTitle != "" {
		t.Fatalf("unexpected receipt titles: %+v", all)
	}
	for _, meta := range all {
		if meta.SourceDependencies == nil || len(meta.SourceDependencies) != 0 || meta.DependenciesStale {
			t.Fatalf("bad empty lineage metadata: %+v", meta)
		}
	}
	chapter, err := repository.ListByChapter(context.Background(), 1)
	if err != nil || len(chapter) != 2 || chapter[0].Version != 1 || chapter[1].Version != 2 {
		t.Fatalf("ListByChapter: %+v, %v", chapter, err)
	}
	full, err := repository.GetFull(context.Background(), 1)
	if err != nil || full == nil || full.Content != "第二章\r\n😀" || full.ChapterTitle != " 权威标题 " || full.WordCount != 11 {
		t.Fatalf("GetFull: %+v, %v", full, err)
	}
	encoded, err := json.Marshal(full)
	if err != nil || !strings.Contains(string(encoded), `"sourceDependencies":[]`) || strings.Contains(string(encoded), `"DraftMeta"`) {
		t.Fatalf("JSON shape: %s, %v", encoded, err)
	}
}

func TestDraftReadsMissingDraftAndBody(t *testing.T) {
	db, repository, _ := newFixture(t)
	ctx := context.Background()
	if full, err := repository.GetFull(ctx, 123); err != nil || full != nil {
		t.Fatalf("missing GetFull: %+v, %v", full, err)
	}
	if meta, err := repository.GetMeta(ctx, 123); err != nil || meta != nil {
		t.Fatalf("missing GetMeta: %+v, %v", meta, err)
	}
	all, err := repository.ListAll(ctx)
	if err != nil || all == nil || len(all) != 0 {
		t.Fatalf("empty list: %+v, %v", all, err)
	}
	chapter, err := repository.ListByChapter(ctx, 123)
	if err != nil || chapter == nil || len(chapter) != 0 {
		t.Fatalf("empty chapter: %+v, %v", chapter, err)
	}
	seed(t, db, 1, 1, 1, "draft", "body")
	mustExec(t, db, "DELETE FROM contents WHERE id=1")
	full, err := repository.GetFull(ctx, 1)
	if err != nil || full == nil || full.Content != "" {
		t.Fatalf("missing body fallback: %+v, %v", full, err)
	}
	if err := repository.UpdateContentIfCurrent(ctx, 1, "new", 1, pointer("")); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("missing content save: %v", err)
	}
}

func TestUpdateContentIfCurrentWorkingStatuses(t *testing.T) {
	for _, status := range []string{"draft", "revised", "reviewed"} {
		t.Run(status, func(t *testing.T) {
			db, repository, _ := newFixture(t)
			seed(t, db, 1, 1, 1, status, "原稿\r\n😀")
			if err := repository.UpdateContentIfCurrent(context.Background(), 1, "新稿\u0000\n𠮷", 123, pointer("原稿\r\n😀")); err != nil {
				t.Fatal(err)
			}
			assertStored(t, db, 1, "新稿\u0000\n𠮷", 123, "")
			var updated, gotStatus string
			if err := db.QueryRow("SELECT updated_at,status FROM drafts WHERE id=1").Scan(&updated, &gotStatus); err != nil {
				t.Fatal(err)
			}
			if updated == "updated-original" || gotStatus != status {
				t.Fatalf("bad metadata save: updated=%q status=%q", updated, gotStatus)
			}
		})
	}
}

func TestUpdateContentIfCurrentRejectsReadOnlyAndUnknown(t *testing.T) {
	for _, status := range []string{"finalized", "superseded", "archived", "unknown-status", "", "Draft", "draft "} {
		t.Run(status, func(t *testing.T) {
			db, repository, _ := newFixture(t)
			seed(t, db, 1, 1, 1, status, "original")
			if err := repository.UpdateContentIfCurrent(context.Background(), 1, "overwrite", 99, pointer("original")); !errors.Is(err, ErrDraftContentConflict) {
				t.Fatalf("want conflict, got %v", err)
			}
			assertStored(t, db, 1, "original", 11, "updated-original")
		})
	}
}

func TestUpdateContentIfCurrentMissingAndEmptyBaselines(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 1, 1, "draft", "")
	ctx := context.Background()
	if err := repository.UpdateContentIfCurrent(ctx, 1, "must not write", 99, nil); !errors.Is(err, ErrDraftContentConflict) {
		t.Fatalf("nil baseline: %v", err)
	}
	assertStored(t, db, 1, "", 11, "updated-original")
	if err := repository.UpdateContentIfCurrent(ctx, 1, "empty is valid", 9, pointer("")); err != nil {
		t.Fatalf("empty baseline: %v", err)
	}
	assertStored(t, db, 1, "empty is valid", 9, "")
	if err := repository.UpdateContentIfCurrent(ctx, 999, "new", 9, pointer("")); !errors.Is(err, ErrDraftNotFound) {
		t.Fatalf("missing draft: %v", err)
	}
	if err := repository.UpdateContentIfCurrent(ctx, 999, "new", 9, nil); !errors.Is(err, ErrDraftContentConflict) {
		t.Fatalf("nil baseline takes precedence: %v", err)
	}
}

func TestUpdateContentIfCurrentStaleDoesNoWrites(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 1, 1, "draft", "é\r\n😀 ")
	// The triggers make even an attempted write observable as a different error.
	mustExec(t, db, `CREATE TRIGGER reject_content BEFORE UPDATE ON contents
 BEGIN SELECT RAISE(ABORT,'should not attempt content write'); END;
 CREATE TRIGGER reject_metadata BEFORE UPDATE ON drafts
 BEGIN SELECT RAISE(ABORT,'should not attempt metadata write'); END;`)
	for _, baseline := range []string{"old", "e\u0301\r\n😀 ", "é\n😀 ", "é\r\n😀"} {
		if err := repository.UpdateContentIfCurrent(context.Background(), 1, "new", 99, &baseline); !errors.Is(err, ErrDraftContentConflict) {
			t.Fatalf("baseline %q: %v", baseline, err)
		}
	}
	assertStored(t, db, 1, "é\r\n😀 ", 11, "updated-original")
}

func TestUpdateContentIfCurrentMetadataFailureRollsBack(t *testing.T) {
	for _, operation := range []string{"ABORT", "ROLLBACK"} {
		t.Run(operation, func(t *testing.T) {
			db, repository, _ := newFixture(t)
			seed(t, db, 1, 1, 1, "draft", "original")
			mustExec(t, db, fmt.Sprintf(`CREATE TRIGGER reject_metadata BEFORE UPDATE OF word_count ON drafts
 BEGIN SELECT RAISE(%s,'metadata failure'); END;`, operation))
			err := repository.UpdateContentIfCurrent(context.Background(), 1, "half write forbidden", 99, pointer("original"))
			if err == nil || !strings.Contains(err.Error(), "metadata failure") {
				t.Fatalf("want metadata failure, got %v", err)
			}
			assertStored(t, db, 1, "original", 11, "updated-original")
			mustExec(t, db, "DROP TRIGGER reject_metadata")
			if err := repository.UpdateContentIfCurrent(context.Background(), 1, "recovery works", 4, pointer("original")); err != nil {
				t.Fatalf("connection not reusable after rollback: %v", err)
			}
		})
	}
}

func TestUpdateContentIfCurrentTwoConcurrentWritersOnlyOneWins(t *testing.T) {
	db, repository, path := newFixture(t)
	seed(t, db, 1, 1, 1, "draft", "same baseline")
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	other, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.SetMaxOpenConns(1)
	// Independent pools prove correctness comes from SQLite's write lock,
	// not a process-local mutex or implicit single-connection serialization.
	repositories := []*Drafts{repository, NewDrafts(other)}
	start := make(chan struct{})
	results := make(chan error, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	for i, repo := range repositories {
		go func(index int, r *Drafts) {
			ready.Done()
			<-start
			results <- r.UpdateContentIfCurrent(context.Background(), 1, fmt.Sprintf("winner-%d", index), int64(20+index), pointer("same baseline"))
		}(i, repo)
	}
	ready.Wait()
	close(start)
	wins, conflicts := 0, 0
	for range 2 {
		err := <-results
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrDraftContentConflict):
			conflicts++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	full, err := repository.GetFull(context.Background(), 1)
	if err != nil || full == nil ||
		!(full.Content == "winner-0" && full.WordCount == 20 || full.Content == "winner-1" && full.WordCount == 21) {
		t.Fatalf("torn winner: %+v, %v", full, err)
	}
}

func TestDependencyCandidateTransitiveAndCycles(t *testing.T) {
	db, repository, _ := newFixture(t)
	for id := int64(1); id <= 3; id++ {
		seed(t, db, id, id, 1, "draft", fmt.Sprintf("body-%d", id))
	}
	deps(t, db, 2, DraftSourceDependency{DraftID: 1, ContentHash: textHash("body-1")})
	deps(t, db, 3, DraftSourceDependency{DraftID: 2, ContentHash: textHash("body-2"), Kind: "candidate"})
	assertStale(t, repository, 3, false)
	mustExec(t, db, "UPDATE contents SET body='changed' WHERE id=1")
	assertStale(t, repository, 2, true)
	assertStale(t, repository, 3, true)
	mustExec(t, db, "UPDATE contents SET body='body-1' WHERE id=1")
	deps(t, db, 1, DraftSourceDependency{DraftID: 2, ContentHash: textHash("body-2")})
	assertStale(t, repository, 1, true)
	assertStale(t, repository, 2, true)
	assertStale(t, repository, 3, true)
}

func TestDependencyMissingAndMalformedFailClosed(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 1, 1, "draft", "body")
	deps(t, db, 1, DraftSourceDependency{DraftID: 999, ContentHash: textHash("missing")})
	assertStale(t, repository, 1, true)
	mustExec(t, db, "UPDATE drafts SET source_dependencies='invalid-json' WHERE id=1")
	meta, err := repository.GetMeta(context.Background(), 1)
	if err != nil || meta == nil || !meta.DependenciesStale || meta.SourceDependencies == nil || len(meta.SourceDependencies) != 0 {
		t.Fatalf("invalid lineage: %+v, %v", meta, err)
	}
}

func TestDependencySealedFinalizedIgnoresHistoricalLineage(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 1, 1, "draft", "changed predecessor")
	seed(t, db, 2, 2, 1, "finalized", "confirmed body")
	seed(t, db, 3, 3, 1, "draft", "downstream")
	deps(t, db, 2, DraftSourceDependency{DraftID: 1, ContentHash: textHash("old predecessor")})
	deps(t, db, 3, DraftSourceDependency{DraftID: 2, ContentHash: textHash("confirmed body"), Kind: "finalized", ChapterNumber: number(2), FinalizationID: "receipt"})
	mustExec(t, db, "INSERT INTO finalization_outbox VALUES('receipt',2,2,'chapter',?,?)", textHash("confirmed body"), "confirmed body")
	assertStale(t, repository, 2, true)
	assertStale(t, repository, 3, false)
	mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot='tampered' WHERE draft_id=2")
	assertStale(t, repository, 3, true)
	mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot=NULL WHERE draft_id=2")
	assertStale(t, repository, 3, false)
	mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=2", textHash("wrong"))
	assertStale(t, repository, 3, true)
	mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=2", textHash("confirmed body"))
	mustExec(t, db, "UPDATE finalization_outbox SET finalization_id='other-receipt' WHERE draft_id=2")
	assertStale(t, repository, 3, true)
	mustExec(t, db, "UPDATE finalization_outbox SET finalization_id='receipt' WHERE draft_id=2")
	mustExec(t, db, "UPDATE drafts SET status='archived' WHERE id=2")
	assertStale(t, repository, 3, true)
}

func TestDependencyAuthoritativeReplacementAndLegacy(t *testing.T) {
	db, repository, _ := newFixture(t)
	seed(t, db, 1, 1, 1, "finalized", "identical")
	seed(t, db, 2, 2, 1, "draft", "downstream")
	dep := DraftSourceDependency{DraftID: 1, ContentHash: textHash("identical"), Kind: "legacy-finalized", ChapterNumber: number(1)}
	deps(t, db, 2, dep)
	assertStale(t, repository, 2, false)
	mustExec(t, db, "INSERT INTO finalization_outbox VALUES('new-receipt',1,1,'title',?,?)", textHash("identical"), "identical")
	assertStale(t, repository, 2, true)
	dep.Kind, dep.FinalizationID = "finalized", "new-receipt"
	deps(t, db, 2, dep)
	assertStale(t, repository, 2, false)
	seed(t, db, 3, 1, 1, "finalized", "identical") // Equal version, higher ID wins.
	assertStale(t, repository, 2, true)
	mustExec(t, db, "UPDATE drafts SET version=2 WHERE id=1")
	assertStale(t, repository, 2, false)
	mustExec(t, db, "UPDATE contents SET body='changed' WHERE id=1")
	assertStale(t, repository, 2, true)
	mustExec(t, db, "DELETE FROM drafts WHERE id=1")
	assertStale(t, repository, 2, true)
}

func TestDependencyLoadingChunksOver500(t *testing.T) {
	db, repository, _ := newFixture(t)
	// Each persisted row stays below the 500-edge limit; transitive closure
	// deliberately crosses the query chunk boundary.
	for id := int64(1); id <= 502; id++ {
		seed(t, db, id, id, 1, "draft", "same")
		if id > 1 {
			deps(t, db, id, DraftSourceDependency{DraftID: id - 1, ContentHash: textHash("same")})
		}
	}
	all, err := repository.ListAll(context.Background())
	if err != nil || len(all) != 502 {
		t.Fatalf("chunked list count=%d err=%v", len(all), err)
	}
	for _, meta := range all {
		if meta.DependenciesStale {
			t.Fatalf("unexpected stale at %d", meta.ID)
		}
	}
	assertStale(t, repository, 502, false)
}

func TestExistingDSNNoCreationNoJournalChange(t *testing.T) {
	for _, path := range []string{"", ":memory:"} {
		if _, err := ExistingDSN(path); err == nil {
			t.Fatalf("accepted invalid path %q", path)
		}
	}
	missing := filepath.Join(t.TempDir(), "must-not-create.sqlite")
	dsn, err := ExistingDSN(missing)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Ping(); err == nil {
		t.Error("mode=rw unexpectedly opened a missing database")
	}
	_ = db.Close()
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing database was created or cannot stat: %v", err)
	}
	fixture, _, path := newFixture(t)
	var before string
	if err := fixture.QueryRow("PRAGMA journal_mode").Scan(&before); err != nil {
		t.Fatal(err)
	}
	dsn, err = ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.Path != path || parsed.Query().Get("mode") != "rw" || parsed.Query().Get("_busy_timeout") != "5000" || strings.Contains(dsn, "journal") {
		t.Fatalf("unsafe DSN %q, %v", dsn, err)
	}
	reopened, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var after string
	if err := reopened.QueryRow("PRAGMA journal_mode").Scan(&after); err != nil || before != after {
		t.Fatalf("journal changed: before=%s after=%s err=%v", before, after, err)
	}
}

func TestNewDraftsDoesNotInitializeOrMigrate(t *testing.T) {
	db, _, _ := newFixture(t)
	mustExec(t, db, "DROP TABLE finalization_outbox; DROP TABLE drafts; DROP TABLE contents; PRAGMA user_version=99")
	repository := NewDrafts(db)
	if _, err := repository.ListAll(context.Background()); err == nil {
		t.Fatal("unsupported schema must error, not create or migrate tables")
	}
	var tables, version int
	if err := db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if tables != 0 || version != 99 {
		t.Fatalf("constructor/read modified schema: tables=%d version=%d", tables, version)
	}
}

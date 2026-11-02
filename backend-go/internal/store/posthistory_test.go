package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// Never maintain a parallel schema: every SQL test starts from a private byte
// copy of the original TypeScript baseline. Absence is fatal in GitHub Actions.
// Public nil-db tests below deliberately do not depend on an oracle or skip.
func phFixture(t *testing.T) (*sql.DB, *PostHistory, string) {
	t.Helper()
	contract := os.Getenv("GO_SQLITE_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("GO_SQLITE_CONTRACT is mandatory for post/history tests") }
		t.Skip("GitHub-only original TypeScript schema oracle")
	}
	data, err := os.ReadFile(contract)
	if err != nil { t.Fatal(err) }
	var manifest struct { Fixtures []struct { Name string `json:"name"`; File string `json:"file"` } `json:"fixtures"` }
	if err := json.Unmarshal(data, &manifest); err != nil { t.Fatal(err) }
	var source string
	for _, fixture := range manifest.Fixtures {
		if fixture.Name == "baseline-v7" { source = filepath.Join(filepath.Dir(contract), fixture.File); break }
	}
	if source == "" { t.Fatal("baseline-v7 original oracle missing") }
	original, err := os.ReadFile(source)
	if err != nil { t.Fatal(err) }
	path := filepath.Join(t.TempDir(), "post history #? 中文.db")
	if err := os.WriteFile(path, original, 0600); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		after, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(original, after) { t.Errorf("original oracle fixture mutated: %v", err) }
	})
	dsn, err := ExistingDSN(path)
	if err != nil { t.Fatal(err) }
	db, err := sql.Open("sqlite3", dsn)
	if err != nil { t.Fatal(err) }
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	// Clear only the private test copy. Original fixture bytes are never opened
	// for SQL and are checked again after the connection pool closes.
	mustExec(t, db, "DELETE FROM post_process_steps; DELETE FROM post_process_runs; DELETE FROM llm_calls")
	p := NewPostHistory(db)
	sequence := 0
	p.newRunID = func() (string, error) { sequence++; return fmt.Sprintf("test-run-%03d", sequence), nil }
	return db, p, path
}

func phInput() map[string]any {
	return map[string]any{"triggerSourceType": "arch_extract", "triggerSourceId": "source-1", "sourceLabel": "来源\r\n😀\x00",
		"steps": []any{map[string]any{"key": "critical", "label": "关键", "critical": true},
			map[string]any{"key": "optional", "label": "可选", "critical": false}}}
}

func phCreated(t *testing.T, p *PostHistory) string {
	t.Helper()
	id, err := p.CreateRun(context.Background(), phInput())
	if err != nil { t.Fatal(err) }
	return id
}

func phCount(t *testing.T, db *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&count); err != nil { t.Fatal(err) }
	return count
}

func phRows(t *testing.T, db *sql.DB, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := rrMaps(context.Background(), db, query, args...)
	if err != nil { t.Fatal(err) }
	return rows
}

func phSourceFixture(t *testing.T, db *sql.DB) map[string]any {
	t.Helper()
	body := "原稿\r\n😀\x00é "
	digest := sha256.Sum256([]byte(body))
	hash := hex.EncodeToString(digest[:])
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(910001,?)", body)
	mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(910001,19,2,'finalized',910001)")
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,
 content_hash,content_revision,content_snapshot,target_file_name,knowledge_document_id)
 VALUES('final-19',910001,19,?,1,?,'第19章.txt','must-not-read-knowledge')`, hash, body)
	input := phInput()
	input["triggerSourceType"], input["triggerSourceId"] = "chapter_finalize", "19"
	input["finalizedSource"] = map[string]any{"draftId": int64(910001), "chapterNumber": int64(19), "finalizationId": "final-19", "contentHash": hash}
	return input
}

func TestPostHistoryPublicNilDefaults(t *testing.T) {
	ctx := context.Background()
	for _, p := range []*PostHistory{nil, NewPostHistory(nil), {}} {
		if id, err := p.CreateRun(ctx, nil); id != "" || !errors.Is(err, ErrProjectNotOpen) { t.Fatalf("create %q %v", id, err) }
		if run, err := p.LatestRun(ctx, "chapter_finalize", "1"); run != nil || err != nil { t.Fatalf("latest %v %v", run, err) }
		if rows, err := p.Steps(ctx, "run"); rows == nil || len(rows) != 0 || err != nil { t.Fatalf("steps %v %v", rows, err) }
		if rows, err := p.History(ctx, 50); rows == nil || len(rows) != 0 || err != nil { t.Fatalf("history %v %v", rows, err) }
		if labels, err := p.FailedStepLabels(ctx, "run"); labels == nil || len(labels) != 0 || err != nil { t.Fatalf("labels %v %v", labels, err) }
		if passed, err := p.AllPassed(ctx, "type", "id"); passed || err != nil { t.Fatalf("passed %v %v", passed, err) }
		if err := p.MarkStepOK(ctx, "run", "key"); err != nil { t.Fatal(err) }
		if err := p.MarkStepFailed(ctx, "run", "key", "message"); err != nil { t.Fatal(err) }
		if err := p.LogCall(ctx, nil); err != nil { t.Fatal(err) }
		stats, err := p.Stats(ctx)
		want := map[string]any{"totalCalls": int64(0), "successfulCalls": int64(0), "failedCalls": int64(0), "knownUsageCalls": int64(0),
			"totalTokens": nil, "totalPromptTokens": nil, "totalCompletionTokens": nil}
		if err != nil || !reflect.DeepEqual(stats, want) { t.Fatalf("stats %v %v", stats, err) }
	}
}

func TestPostHistoryUUID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 64; i++ {
		id, err := secureUUID()
		if err != nil { t.Fatal(err) }
		if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' || id[14] != '4' || !strings.ContainsRune("89ab", rune(id[19])) { t.Fatalf("UUID layout %q", id) }
		if _, err := hex.DecodeString(strings.ReplaceAll(id, "-", "")); err != nil { t.Fatal(err) }
		if seen[id] { t.Fatalf("duplicate UUID %s", id) }
		seen[id] = true
	}
}

func TestPostHistoryRunOrderingAndReceipts(t *testing.T) {
	db, p, _ := phFixture(t)
	ctx := context.Background()
	if run, err := p.LatestRun(ctx, "arch_extract", "source-1"); run != nil || err != nil { t.Fatalf("missing %v %v", run, err) }
	first := phCreated(t, p)
	if passed, err := p.AllPassed(ctx, "arch_extract", "source-1"); passed || err != nil { t.Fatalf("initial summary %v %v", passed, err) }
	labels, err := p.FailedStepLabels(ctx, first)
	if err != nil || !reflect.DeepEqual(labels, []any{"关键"}) { t.Fatalf("labels %v %v", labels, err) }
	if err := p.MarkStepOK(ctx, first, "critical"); err != nil { t.Fatal(err) }
	if passed, err := p.AllPassed(ctx, "arch_extract", "source-1"); !passed || err != nil { t.Fatalf("success summary %v %v", passed, err) }
	if err := p.MarkStepFailed(ctx, first, "critical", "第一次\r\n😀\x00"); err != nil { t.Fatal(err) }
	if passed, err := p.AllPassed(ctx, "arch_extract", "source-1"); passed || err != nil { t.Fatalf("failed summary %v %v", passed, err) }
	rows, err := p.Steps(ctx, first)
	if err != nil || len(rows) != 2 || rows[0]["ok"] != false || rows[0]["attemptCount"] != int64(2) || rows[0]["completedAt"] != "" || rows[0]["lastAttemptAt"] == "" || rows[0]["errorMsg"] != "第一次\r\n😀\x00" { t.Fatalf("failure receipt %v %v", rows, err) }
	if err := p.MarkStepOK(ctx, first, "critical"); err != nil { t.Fatal(err) }
	rows, err = p.Steps(ctx, first)
	if err != nil || rows[0]["ok"] != true || rows[0]["attemptCount"] != int64(3) || rows[0]["errorMsg"] != "" || rows[0]["completedAt"] == "" { t.Fatalf("retry receipt %v %v", rows, err) }
	second := phCreated(t, p)
	mustExec(t, db, "UPDATE post_process_runs SET created_at='fixed' WHERE id IN (?,?)", first, second)
	run, err := p.LatestRun(ctx, "arch_extract", "source-1")
	if err != nil || run["id"] != second { t.Fatalf("rowid tiebreak %v %v", run, err) }
	mustExec(t, db, "UPDATE post_process_runs SET created_at='z-newest' WHERE id=?", first)
	run, err = p.LatestRun(ctx, "arch_extract", "source-1")
	if err != nil || run["id"] != first { t.Fatalf("timestamp before rowid %v %v", run, err) }
	// Empty critical set starts false and becomes true only on a real mark.
	input := phInput(); input["triggerSourceId"] = "optional-only"
	input["steps"] = []map[string]any{{"key": "one", "label": "one", "critical": false}}
	id, err := p.CreateRun(ctx, input)
	if err != nil { t.Fatal(err) }
	if err := p.MarkStepFailed(ctx, id, "one", "optional failure"); err != nil { t.Fatal(err) }
	if passed, err := p.AllPassed(ctx, "arch_extract", "optional-only"); !passed || err != nil { t.Fatalf("no critical summary %v %v", passed, err) }
}

func TestPostHistoryLegacyNullAndRawStorage(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background(); id := phCreated(t, p)
	mustExec(t, db, `UPDATE post_process_steps SET label=NULL,error_msg=NULL,completed_at=NULL,
 last_attempt_at=NULL,attempt_count=NULL WHERE run_id=? AND step_key='critical'`, id)
	labels, err := p.FailedStepLabels(ctx, id)
	if err != nil || !reflect.DeepEqual(labels, []any{nil}) { t.Fatalf("raw NULL labels %v %v", labels, err) }
	rows, err := p.Steps(ctx, id)
	if err != nil || rows[0]["label"] != "" || rows[0]["errorMsg"] != "" || rows[0]["completedAt"] != "" || rows[0]["lastAttemptAt"] != "" || rows[0]["attemptCount"] != nil { t.Fatalf("nullable steps %v %v", rows, err) }
	mustExec(t, db, `UPDATE post_process_steps SET label=CAST('blob-label' AS BLOB),critical=CAST('1' AS BLOB),ok=2 WHERE run_id=? AND step_key='critical'`, id)
	rows, err = p.Steps(ctx, id)
	if err != nil || !bytes.Equal(rows[0]["label"].([]byte), []byte("blob-label")) || rows[0]["critical"] != false || rows[0]["ok"] != false { t.Fatalf("raw steps %v %v", rows, err) }
	mustExec(t, db, "UPDATE post_process_runs SET source_label=CAST('blob-source' AS BLOB),all_critical_passed=CAST('1' AS BLOB),created_at=NULL,updated_at=NULL WHERE id=?", id)
	run, err := p.LatestRun(ctx, "arch_extract", "source-1")
	if err != nil || !bytes.Equal(run["sourceLabel"].([]byte), []byte("blob-source")) || run["allCriticalPassed"] != false || run["createdAt"] != nil || run["updatedAt"] != nil { t.Fatalf("raw run %v %v", run, err) }
	mustExec(t, db, "UPDATE post_process_steps SET critical=1,ok=0 WHERE run_id=? AND step_key='critical'", id)
	labels, err = p.FailedStepLabels(ctx, id)
	if err != nil || !bytes.Equal(labels[0].([]byte), []byte("blob-label")) { t.Fatalf("raw BLOB labels %v %v", labels, err) }
	if err := p.MarkStepOK(ctx, id, "critical"); err != nil { t.Fatal(err) }
	rows, err = p.Steps(ctx, id)
	if err != nil || rows[0]["attemptCount"] != nil { t.Fatalf("NULL attempt remains NULL %v %v", rows, err) }
}

func TestPostHistoryFinalizedSourceDrift(t *testing.T) {
	cases := []string{"draftId", "chapterNumber", "finalizationId", "contentHash", "partial", "wrong-type", "wrong-source-type", "wrong-source-id", "leading-zero-id",
		"no-row", "no-outbox", "status", "chapter", "body", "snapshot", "outbox-id", "outbox-hash", "hash-mismatch", "new-version", "latest-tie",
		"blob-body", "blob-snapshot", "blob-status", "blob-chapter", "blob-outbox-id", "blob-outbox-hash"}
	for _, name := range cases { t.Run(name, func(t *testing.T) {
		db, p, _ := phFixture(t); input := phSourceFixture(t, db); source := input["finalizedSource"].(map[string]any)
		switch name {
		case "draftId": source[name] = int64(910002)
		case "chapterNumber": source[name] = int64(20)
		case "finalizationId", "contentHash": source[name] = "changed"
		case "partial": delete(source, "draftId")
		case "wrong-type": source["chapterNumber"] = "19"
		case "wrong-source-type": input["triggerSourceType"] = "arch_extract"
		case "wrong-source-id": input["triggerSourceId"] = "20"
		case "leading-zero-id": input["triggerSourceId"] = "019"
		case "no-row": mustExec(t, db, "DELETE FROM drafts WHERE id=910001")
		case "no-outbox": mustExec(t, db, "DELETE FROM finalization_outbox WHERE draft_id=910001")
		case "status": mustExec(t, db, "UPDATE drafts SET status='archived' WHERE id=910001")
		case "chapter": mustExec(t, db, "UPDATE drafts SET chapter_number=20 WHERE id=910001")
		case "body": mustExec(t, db, "UPDATE contents SET body='changed' WHERE id=910001")
		case "snapshot": mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot='changed' WHERE draft_id=910001")
		case "outbox-id": mustExec(t, db, "UPDATE finalization_outbox SET finalization_id='changed' WHERE draft_id=910001")
		case "outbox-hash": mustExec(t, db, "UPDATE finalization_outbox SET content_hash='changed' WHERE draft_id=910001")
		case "hash-mismatch":
			source["contentHash"] = "matching-but-not-body-hash"
			mustExec(t, db, "UPDATE finalization_outbox SET content_hash=? WHERE draft_id=910001", source["contentHash"])
		case "new-version", "latest-tie":
			version := 3
			if name == "latest-tie" { mustExec(t, db, "DROP INDEX idx_drafts_chapter_version"); version = 2 }
			mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(910002,19,?,'finalized',910001)", version)
		case "blob-body": mustExec(t, db, "UPDATE contents SET body=CAST(body AS BLOB) WHERE id=910001")
		case "blob-snapshot": mustExec(t, db, "UPDATE finalization_outbox SET content_snapshot=CAST(content_snapshot AS BLOB) WHERE draft_id=910001")
		case "blob-status": mustExec(t, db, "UPDATE drafts SET status=CAST(status AS BLOB) WHERE id=910001")
		case "blob-chapter": mustExec(t, db, "UPDATE drafts SET chapter_number=CAST(chapter_number AS BLOB) WHERE id=910001")
		case "blob-outbox-id": mustExec(t, db, "UPDATE finalization_outbox SET finalization_id=CAST(finalization_id AS BLOB) WHERE draft_id=910001")
		case "blob-outbox-hash": mustExec(t, db, "UPDATE finalization_outbox SET content_hash=CAST(content_hash AS BLOB) WHERE draft_id=910001")
		}
		before := phRows(t, db, "SELECT * FROM finalization_outbox ORDER BY rowid")
		if _, err := p.CreateRun(context.Background(), input); !errors.Is(err, ErrPostProcessFinalizationSourceChanged) { t.Fatalf("drift %s: %v", name, err) }
		if phCount(t, db, "post_process_runs") != 0 || phCount(t, db, "post_process_steps") != 0 { t.Fatal("failed proof wrote receipts") }
		if after := phRows(t, db, "SELECT * FROM finalization_outbox ORDER BY rowid"); !reflect.DeepEqual(before, after) { t.Fatal("proof changed source receipt") }
	}) }
}

func TestPostHistoryFinalizedLookupAndNoSourceSideEffects(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background(); input := phSourceFixture(t, db)
	before := map[string][]map[string]any{}
	for _, table := range []string{"contents", "drafts", "finalization_outbox", "characters", "blueprints"} { before[table] = phRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid") }
	id, err := p.CreateRun(ctx, input)
	if err != nil { t.Fatal(err) }
	for table, rows := range before {
		if after := phRows(t, db, "SELECT * FROM "+table+" ORDER BY rowid"); !reflect.DeepEqual(rows, after) { t.Fatalf("CreateRun changed %s", table) }
	}
	for _, sourceID := range []string{"19", "019", "finalization:final-19"} {
		run, err := p.LatestRun(ctx, "chapter_finalize", sourceID)
		if err != nil || run["id"] != id || run["triggerSourceId"] != "finalization:final-19" { t.Fatalf("lookup %q: %v %v", sourceID, run, err) }
	}
	// New draft/nonfinalized versions do not invalidate latest finalized proof.
	mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(910002,19,3,'draft',910001)")
	if _, err := p.CreateRun(ctx, input); err != nil { t.Fatal(err) }
	// The original latest lookup JOIN skips an outbox-less finalized version,
	// although CreateRun's independent latest-finalized proof rejects old work.
	mustExec(t, db, "UPDATE drafts SET status='finalized' WHERE id=910002")
	if run, err := p.LatestRun(ctx, "chapter_finalize", "19"); err != nil || run == nil { t.Fatalf("outbox-less lookup %v %v", run, err) }
	mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,
 content_hash,content_revision,content_snapshot,target_file_name) SELECT 'final-new',910002,19,content_hash,1,content_snapshot,'new.txt' FROM finalization_outbox WHERE draft_id=910001`)
	if run, err := p.LatestRun(ctx, "chapter_finalize", "19"); err != nil || run != nil { t.Fatalf("new receipt must not return old run %v %v", run, err) }
	// Equal-version ties use the higher draft ID, not insertion time/outbox ID.
	mustExec(t, db, "DROP INDEX idx_drafts_chapter_version; UPDATE drafts SET version=2 WHERE id=910002")
	if run, err := p.LatestRun(ctx, "chapter_finalize", "19"); err != nil || run != nil { t.Fatalf("latest tie %v %v", run, err) }
}

func TestPostHistorySourceOptionalNullAndNoInventedChecks(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background()
	for _, absent := range []any{nil, false, "", float64(0)} {
		input := phInput(); input["finalizedSource"] = absent
		if _, err := p.CreateRun(ctx, input); err != nil { t.Fatalf("original falsy optional %v: %v", absent, err) }
		input["triggerSourceId"] = "finalization:"
		if _, err := p.CreateRun(ctx, input); !errors.Is(err, ErrPostProcessFinalizationSourceRequired) { t.Fatalf("reserved prefix %v", err) }
	}
	input := phSourceFixture(t, db)
	// These fields are not part of the original proof; no knowledge reads or
	// publication-state requirement may be added by this receipt-only migration.
	mustExec(t, db, "UPDATE finalization_outbox SET chapter_number=99,knowledge_document_id='missing',publication_status='pending',content_revision=17 WHERE draft_id=910001")
	if _, err := p.CreateRun(ctx, input); err != nil { t.Fatalf("invented source restriction %v", err) }
	// Without a current receipt, numeric lookup retains its legacy source ID.
	mustExec(t, db, "DELETE FROM finalization_outbox WHERE draft_id=910001")
	legacy := phInput(); legacy["triggerSourceType"], legacy["triggerSourceId"] = "chapter_finalize", "19"
	id, err := p.CreateRun(ctx, legacy)
	if err != nil { t.Fatal(err) }
	if run, err := p.LatestRun(ctx, "chapter_finalize", "19"); err != nil || run["id"] != id { t.Fatalf("legacy fallback %v %v", run, err) }
	mustExec(t, db, "DROP TABLE finalization_outbox")
	if run, err := p.LatestRun(ctx, "chapter_finalize", "19"); err != nil || run["id"] != id { t.Fatalf("pre-outbox fallback %v %v", run, err) }
}

func TestPostHistoryAtomicMarkAndCreateRollback(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background(); id := phCreated(t, p)
	for _, mark := range []func() error{func() error { return p.MarkStepOK(ctx, id, "missing") }, func() error { return p.MarkStepFailed(ctx, "missing-run", "critical", "safe") }} {
		before := phRows(t, db, "SELECT * FROM post_process_runs ORDER BY rowid")
		if err := mark(); !errors.Is(err, ErrPostProcessStepNotFound) { t.Fatalf("missing step %v", err) }
		if after := phRows(t, db, "SELECT * FROM post_process_runs ORDER BY rowid"); !reflect.DeepEqual(before, after) { t.Fatal("missing step refreshed summary") }
	}
	mustExec(t, db, "CREATE TRIGGER fail_summary BEFORE UPDATE ON post_process_runs BEGIN SELECT RAISE(ABORT,'summary blocked'); END")
	for _, mark := range []func() error{func() error { return p.MarkStepOK(ctx, id, "critical") }, func() error { return p.MarkStepFailed(ctx, id, "critical", "safe") }} {
		before := phRows(t, db, "SELECT * FROM post_process_steps ORDER BY id")
		if err := mark(); err == nil || !strings.Contains(err.Error(), "summary blocked") { t.Fatalf("summary trigger %v", err) }
		if after := phRows(t, db, "SELECT * FROM post_process_steps ORDER BY id"); !reflect.DeepEqual(before, after) { t.Fatal("step leaked through summary abort") }
	}
	mustExec(t, db, "DROP TRIGGER fail_summary")
	input := phInput(); input["steps"] = []any{map[string]any{"key": "same", "label": "a", "critical": true}, map[string]any{"key": "same", "label": "b", "critical": true}}
	duplicateID, err := p.CreateRun(ctx, input)
	if err != nil { t.Fatalf("original permits duplicate step keys: %v", err) }
	for _, mark := range []func() error{func() error { return p.MarkStepOK(ctx, duplicateID, "same") }, func() error { return p.MarkStepFailed(ctx, duplicateID, "same", "failure") }} {
		before := phRows(t, db, "SELECT * FROM post_process_steps WHERE run_id=? ORDER BY id", duplicateID)
		if err := mark(); !errors.Is(err, ErrPostProcessStepNotFound) { t.Fatalf("changes=2: %v", err) }
		if after := phRows(t, db, "SELECT * FROM post_process_steps WHERE run_id=? ORDER BY id", duplicateID); !reflect.DeepEqual(before, after) { t.Fatal("duplicate mark did not roll back") }
	}
	mustExec(t, db, "CREATE TRIGGER fail_second_step BEFORE INSERT ON post_process_steps WHEN NEW.step_key='optional' BEGIN SELECT RAISE(ABORT,'step insert blocked'); END")
	runs, steps := phCount(t, db, "post_process_runs"), phCount(t, db, "post_process_steps")
	if _, err := p.CreateRun(ctx, phInput()); err == nil || !strings.Contains(err.Error(), "step insert blocked") { t.Fatalf("insert trigger %v", err) }
	if phCount(t, db, "post_process_runs") != runs || phCount(t, db, "post_process_steps") != steps { t.Fatal("partial CreateRun persisted") }
	mustExec(t, db, "DROP TRIGGER fail_second_step")
	entropyErr := errors.New("entropy unavailable")
	p.newRunID = func() (string, error) { return "", entropyErr }
	if _, err := p.CreateRun(ctx, phInput()); !errors.Is(err, entropyErr) { t.Fatalf("random failure %v", err) }
	if phCount(t, db, "post_process_runs") != runs { t.Fatal("random failure wrote") }
	if err := p.MarkStepOK(ctx, id, "critical"); err != nil { t.Fatalf("pool left unusable %v", err) }
}

func TestPostHistoryIndependentPoolsConcurrentMark(t *testing.T) {
	db, p, path := phFixture(t); id := phCreated(t, p)
	dsn, err := ExistingDSN(path)
	if err != nil { t.Fatal(err) }
	other, err := sql.Open("sqlite3", dsn)
	if err != nil { t.Fatal(err) }
	defer other.Close()
	other.SetMaxOpenConns(3)
	second := NewPostHistory(other)
	const attempts = 24
	start := make(chan struct{})
	failures := make(chan error, attempts)
	var group sync.WaitGroup
	for i := 0; i < attempts; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done(); <-start
			repository := p
			if i%2 == 0 { repository = second }
			if i%3 == 0 { failures <- repository.MarkStepFailed(context.Background(), id, "critical", "retry") } else { failures <- repository.MarkStepOK(context.Background(), id, "critical") }
		}(i)
	}
	close(start); group.Wait(); close(failures)
	for err := range failures { if err != nil { t.Fatal(err) } }
	rows, err := p.Steps(context.Background(), id)
	if err != nil || rows[0]["attemptCount"] != int64(attempts) { t.Fatalf("lost attempts %v %v", rows, err) }
	run, err := p.LatestRun(context.Background(), "arch_extract", "source-1")
	if err != nil || run["allCriticalPassed"] != rows[0]["ok"] { t.Fatalf("torn summary %v %v %v", run, rows, err) }
	// Verify a cancelled acquisition cannot poison either borrowed pool.
	ctx, cancel := context.WithCancel(context.Background()); cancel()
	if err := second.MarkStepOK(ctx, id, "critical"); err == nil { t.Fatal("cancelled write succeeded") }
	if err := p.MarkStepOK(context.Background(), id, "critical"); err != nil { t.Fatal(err) }
	if phCount(t, db, "post_process_runs") != 1 { t.Fatal("mark allocated runs") }
}

func phCall(success bool, message any) map[string]any {
	return map[string]any{"modelId": "model-1", "modelName": "模型😀", "purpose": "chapter-draft", "promptTokens": nil,
		"completionTokens": nil, "totalTokens": nil, "durationMs": float64(500.5), "success": success, "errorMessage": message}
}

func TestPostHistoryLLMNullableUsageAndExactSums(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background()
	stats, err := p.Stats(ctx)
	if err != nil || stats["totalCalls"] != int64(0) || stats["successfulCalls"] != int64(0) || stats["failedCalls"] != int64(0) || stats["knownUsageCalls"] != int64(0) || stats["totalTokens"] != nil || stats["totalPromptTokens"] != nil || stats["totalCompletionTokens"] != nil { t.Fatalf("empty sums %v %v", stats, err) }
	if err := p.LogCall(ctx, phCall(true, nil)); err != nil { t.Fatal(err) }
	stats, err = p.Stats(ctx)
	if err != nil || stats["knownUsageCalls"] != int64(0) || stats["totalTokens"] != nil { t.Fatalf("unknown usage %v %v", stats, err) }
	zero := phCall(false, nil); zero["promptTokens"], zero["completionTokens"], zero["totalTokens"] = float64(0), float64(0), float64(0)
	if err := p.LogCall(ctx, zero); err != nil { t.Fatal(err) }
	stats, err = p.Stats(ctx)
	if err != nil || stats["knownUsageCalls"] != int64(1) || stats["totalTokens"] != int64(0) { t.Fatalf("known zero %v %v", stats, err) }
	partial := phCall(false, "finish:length"); partial["promptTokens"], partial["completionTokens"] = 1.5, -2.0
	if err := p.LogCall(ctx, partial); err != nil { t.Fatal(err) }
	stats, err = p.Stats(ctx)
	if err != nil || stats["totalCalls"] != int64(3) || stats["successfulCalls"] != int64(1) || stats["failedCalls"] != int64(2) || stats["knownUsageCalls"] != int64(1) || stats["totalTokens"] != int64(0) || stats["totalPromptTokens"] != 1.5 || stats["totalCompletionTokens"] != int64(-2) { t.Fatalf("independent sums %v %v", stats, err) }
	mustExec(t, db, `INSERT INTO llm_calls(model_id,success,prompt_tokens,completion_tokens,total_tokens,error_message)
 VALUES('legacy',NULL,NULL,NULL,CAST('7.25' AS BLOB),'private error')`)
	stats, err = p.Stats(ctx)
	if err != nil || stats["knownUsageCalls"] != int64(2) || stats["totalTokens"] != 7.25 || stats["successfulCalls"] != int64(1) || stats["failedCalls"] != int64(2) { t.Fatalf("raw SQLite SUM %v %v", stats, err) }
	history, err := p.History(ctx, -1)
	if err != nil || len(history) != 4 || history[0]["success"] != nil || !bytes.Equal(history[0]["totalTokens"].([]byte), []byte("7.25")) || history[1]["totalTokens"] != nil || history[3]["promptTokens"] != nil { t.Fatalf("raw nullable history %v %v", history, err) }
	if raw := phRows(t, db, "SELECT error_message FROM llm_calls WHERE model_id='model-1' ORDER BY id"); raw[0]["error_message"] != "" || raw[1]["error_message"] != "" { t.Fatalf("optional nil errorMessage %v", raw) }
}

func TestPostHistoryLLMFinishReasonsAndProjection(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background()
	cases := []struct { success bool; message any; reason any }{
		{true, "finish:length", "stop"}, {false, "finish:length", "length"}, {false, "finish:content_filter", "content_filter"},
		{false, "finish:cancelled", "cancelled"}, {false, "cancelled", "cancelled"}, {false, "finish:error", "error"},
		{false, "finish:unknown", "unknown"}, {false, nil, nil}, {false, "Authorization: Bearer private-provider-error", nil},
		{false, "finish:Length", nil}, {false, "finish:length ", nil},
	}
	for _, test := range cases { if err := p.LogCall(ctx, phCall(test.success, test.message)); err != nil { t.Fatal(err) } }
	// The history projection must never grow raw prompt/error fields, including
	// when an older schema happens to carry provider-specific prompt columns.
	mustExec(t, db, "ALTER TABLE llm_calls ADD COLUMN raw_prompt TEXT; UPDATE llm_calls SET raw_prompt='secret prompt'")
	rows, err := p.History(ctx, 100)
	if err != nil || len(rows) != len(cases) { t.Fatalf("history %v %v", rows, err) }
	for i, row := range rows {
		test := cases[len(cases)-1-i]
		if row["finishReason"] != test.reason { t.Fatalf("finish %q = %v want %v", test.message, row["finishReason"], test.reason) }
		flag := int64(0); if test.success { flag = 1 }
		if row["success"] != flag { t.Fatalf("raw numeric success %v", row) }
		if len(row) != 11 { t.Fatalf("unexpected projection columns %v", row) }
		if _, exists := row["error_message"]; exists { t.Fatal("raw error leaked") }
		if _, exists := row["raw_prompt"]; exists { t.Fatal("raw prompt leaked") }
	}
	encoded, err := json.Marshal(rows)
	if err != nil || strings.Contains(string(encoded), "Bearer") || strings.Contains(string(encoded), "secret prompt") { t.Fatalf("sensitive history %s %v", encoded, err) }
	if rows, err := p.History(ctx, 0); err != nil || rows == nil || len(rows) != 0 { t.Fatalf("limit zero %v %v", rows, err) }
	if rows, err := p.History(ctx, 2); err != nil || len(rows) != 2 { t.Fatalf("limit two %v %v", rows, err) }
	mustExec(t, db, `INSERT INTO llm_calls(model_id,success,error_message,model_name) VALUES('raw',2,'finish:length',CAST('raw-name' AS BLOB)),('blob-error',0,CAST('finish:length' AS BLOB),'name')`)
	rows, err = p.History(ctx, 2)
	if err != nil || rows[0]["finishReason"] != nil || rows[1]["finishReason"] != "length" || rows[1]["success"] != int64(2) || !bytes.Equal(rows[1]["modelName"].([]byte), []byte("raw-name")) { t.Fatalf("exact CASE/raw storage %v %v", rows, err) }
}

func TestPostHistoryLegacyNumericSourceAndJSChapterText(t *testing.T) {
	for _, test := range []struct { chapter float64; text string }{
		{19.5, "19.5"}, {-19, "-19"}, {0, "0"}, {0.000001, "0.000001"}, {0.0000001, "1e-7"}, {1e21, "1e+21"},
	} { t.Run(test.text, func(t *testing.T) {
		db, p, _ := phFixture(t); input := phSourceFixture(t, db)
		mustExec(t, db, "UPDATE drafts SET chapter_number=? WHERE id=910001", test.chapter)
		input["triggerSourceId"] = test.text
		input["finalizedSource"].(map[string]any)["chapterNumber"] = test.chapter
		if _, err := p.CreateRun(context.Background(), input); err != nil { t.Fatalf("legacy numeric source %v", err) }
	}) }
	// INTEGER PRIMARY KEY IDs cannot be fractional, but normal JSON numbers
	// and retained json.Number must compare numerically without string coercion.
	db, p, _ := phFixture(t); input := phSourceFixture(t, db)
	input["finalizedSource"].(map[string]any)["draftId"] = json.Number("910001")
	input["finalizedSource"].(map[string]any)["chapterNumber"] = json.Number("19")
	if _, err := p.CreateRun(context.Background(), input); err != nil { t.Fatal(err) }
}

func TestPostHistoryTypedInputAndCallerCannotChooseID(t *testing.T) {
	db, p, _ := phFixture(t); ctx := context.Background()
	for _, test := range []struct { key string; value any }{{"triggerSourceType", false}, {"triggerSourceId", 19}, {"sourceLabel", nil}, {"steps", nil}, {"steps", []any{nil}}, {"steps", []any{map[string]any{"key": "x", "label": "x", "critical": 1}}}} {
		input := phInput(); input[test.key] = test.value
		if _, err := p.CreateRun(ctx, input); !errors.Is(err, ErrPostHistoryInvalidInput) { t.Fatalf("create %s: %v", test.key, err) }
	}
	if phCount(t, db, "post_process_runs") != 0 { t.Fatal("invalid input wrote") }
	input := phInput(); input["id"], input["runId"] = "caller-id", "caller-id"
	id, err := p.CreateRun(ctx, input)
	if err != nil || id == "caller-id" { t.Fatalf("caller ID accepted %q %v", id, err) }
	for _, test := range []struct { key string; value any }{{"modelId", []byte("model")}, {"modelName", nil}, {"purpose", false}, {"success", 1}, {"durationMs", nil}, {"promptTokens", "5"}, {"totalTokens", math.NaN()}, {"completionTokens", math.Inf(1)}, {"errorMessage", false}} {
		call := phCall(true, nil); call[test.key] = test.value
		if err := p.LogCall(ctx, call); !errors.Is(err, ErrPostHistoryInvalidInput) { t.Fatalf("call %s: %v", test.key, err) }
	}
	if phCount(t, db, "llm_calls") != 0 { t.Fatal("invalid LLM input wrote") }
}

package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// All repository tests use a private byte copy of the original TypeScript
// schema oracle. CI may never silently substitute hand-maintained fixture DDL.
func rrFixture(t *testing.T) (*sql.DB, *ReviewRevisions, string) {
	t.Helper()
	contract := os.Getenv("GO_SQLITE_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" { t.Fatal("GO_SQLITE_CONTRACT is mandatory for review/revision tests") }
		t.Skip("GitHub-only original TypeScript schema oracle")
	}
	index, err := os.ReadFile(contract)
	if err != nil { t.Fatal(err) }
	var manifest struct { Fixtures []struct { Name string `json:"name"`; File string `json:"file"` } `json:"fixtures"` }
	if err := json.Unmarshal(index, &manifest); err != nil { t.Fatal(err) }
	var source string
	for _, fixture := range manifest.Fixtures {
		if fixture.Name == "baseline-v7" { source = filepath.Join(filepath.Dir(contract), fixture.File); break }
	}
	if source == "" { t.Fatal("baseline-v7 original oracle missing") }
	original, err := os.ReadFile(source)
	if err != nil { t.Fatal(err) }
	path := filepath.Join(t.TempDir(), "review revision #? 中文.db")
	if err := os.WriteFile(path, original, 0600); err != nil { t.Fatal(err) }
	t.Cleanup(func() {
		after, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(original, after) { t.Errorf("original oracle fixture mutated: %v", err) }
	})
	uri := url.URL{Scheme: "file", Path: path}
	uri.RawQuery = "mode=rw&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil { t.Fatal(err) }
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	mustExec(t, db, `INSERT INTO contents(id,body) VALUES(900001,?)`, "原稿\r\n😀\x00é ")
	mustExec(t, db, `INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count,created_at,updated_at)
 VALUES(900001,9,2,'draft',900001,11,'created-original','updated-original')`)
	return db, NewReviewRevisions(db), path
}

func rrInput() map[string]any {
	return map[string]any{"baseDraftId": int64(900001), "content": "报告/修稿\r\n😀", "revisionType": "refine", "wordCount": int64(19),
		"expectedSource": map[string]any{"id": int64(900001), "chapterNumber": int64(9), "version": int64(2), "status": "draft", "content": "原稿\r\n😀\x00é "}}
}

func rrMerge(id int64) map[string]any {
	return map[string]any{"revisionId": id, "targetDraftId": int64(900001), "expectedDraftContent": "原稿\r\n😀\x00é ", "mergedContent": "合并\r\n𠮷\x00", "wordCount": int64(23)}
}

func rrCreated(t *testing.T, r *ReviewRevisions) int64 {
	t.Helper()
	out, err := r.CreateRevision(context.Background(), rrInput())
	if err != nil { t.Fatal(err) }
	return out["id"].(int64)
}

func rrCounts(t *testing.T, db *sql.DB) string {
	t.Helper()
	var contents, reviews, revisions int64
	if err := db.QueryRow("SELECT (SELECT COUNT(*) FROM contents),(SELECT COUNT(*) FROM reviews),(SELECT COUNT(*) FROM revisions)").Scan(&contents, &reviews, &revisions); err != nil { t.Fatal(err) }
	return fmt.Sprintf("%d/%d/%d", contents, reviews, revisions)
}

func TestReviewRevisionCRUDNullableAndIndex(t *testing.T) {
	db, r, _ := rrFixture(t)
	ctx := context.Background()
	if got, err := r.ListReviews(ctx, 900001); err != nil || got == nil || len(got) != 0 { t.Fatalf("empty reviews %v %v", got, err) }
	if got, err := r.LatestReview(ctx, 900001); err != nil || got != nil { t.Fatalf("missing latest %v %v", got, err) }
	if got, err := r.FullRevision(ctx, 999999); err != nil || got != nil { t.Fatalf("missing revision %v %v", got, err) }
	for i := int64(1); i <= 2; i++ {
		input := rrInput(); input["reviewIndex"] = int64(999)
		out, err := r.CreateReview(ctx, input)
		if err != nil || out["reviewIndex"] != i { t.Fatalf("allocate ignores caller: %v %v", out, err) }
		full, err := r.FullReview(ctx, out["id"].(int64))
		if err != nil || full["content"] != input["content"] || !reflect.DeepEqual(full["sourceDraft"], input["expectedSource"]) { t.Fatalf("review full: %v %v", full, err) }
	}
	if n, err := r.NextReviewIndex(ctx, 900001); err != nil || n != 3 { t.Fatalf("next review %d %v", n, err) }
	id := rrCreated(t, r)
	full, err := r.FullRevision(ctx, id)
	if err != nil || full["mergedToDraftId"] != nil || full["reviewSourceId"] != nil || full["userPrompt"] != "" { t.Fatalf("nullable metadata: %v %v", full, err) }
	mustExec(t, db, "UPDATE revisions SET source_content=NULL,user_prompt=NULL,word_count=NULL,updated_at=NULL WHERE id=?", id)
	full, err = r.FullRevision(ctx, id)
	if err != nil || full["sourceDraft"] != nil || full["wordCount"] != nil || full["updatedAt"] != nil || full["userPrompt"] != "" { t.Fatalf("legacy nulls %v %v", full, err) }
	replacement, err := r.ReplacePending(ctx, rrInput())
	if err != nil || replacement["revisionIndex"] != int64(2) || !reflect.DeepEqual(replacement["discardedRevisionIds"], []int64{id}) { t.Fatalf("replace %v %v", replacement, err) }
	pending, err := r.ListRevisions(ctx, 900001, true)
	if err != nil || len(pending) != 1 || pending[0]["id"] != replacement["id"] { t.Fatalf("pending %v %v", pending, err) }
	if err := r.MarkDiscarded(ctx, replacement["id"].(int64)); err != nil { t.Fatal(err) }
	if err := r.MarkDiscarded(ctx, replacement["id"].(int64)); !errors.Is(err, ErrRevisionNotPending) { t.Fatalf("discard twice %v", err) }
	last := rrCreated(t, r)
	if err := r.MarkMerged(ctx, last, 900001); err != nil { t.Fatal(err) }
	if n, err := r.NextRevisionIndex(ctx, 900001); err != nil || n != 4 { t.Fatalf("next revision %d %v", n, err) }
}

func TestReviewRevisionExactSourceCAS(t *testing.T) {
	for _, field := range []string{"id", "chapterNumber", "version", "status", "content", "missing", "partial", "missing-row", "missing-body", "blob-body", "null-status"} {
		t.Run(field, func(t *testing.T) {
			db, r, _ := rrFixture(t)
			input := rrInput(); expected := input["expectedSource"].(map[string]any)
			switch field {
			case "id": expected[field] = int64(900002)
			case "chapterNumber", "version": expected[field] = int64(100)
			case "status": expected[field] = "revised"
			case "content": expected[field] = "原稿\n😀\x00e\u0301 "
			case "missing": delete(input, "expectedSource")
			case "partial": delete(expected, "version")
			case "missing-row": mustExec(t, db, "DELETE FROM drafts WHERE id=900001")
			case "missing-body": mustExec(t, db, "DELETE FROM contents WHERE id=900001")
			case "blob-body": mustExec(t, db, "UPDATE contents SET body=CAST(body AS BLOB) WHERE id=900001")
			case "null-status": mustExec(t, db, "UPDATE drafts SET status=NULL WHERE id=900001")
			}
			before := rrCounts(t, db)
			for _, method := range []func(context.Context, map[string]any) (map[string]any, error){r.CreateReview, r.CreateRevision, r.ReplacePending} {
				if _, err := method(context.Background(), input); !errors.Is(err, ErrSourceDraftChanged) { t.Fatalf("source %s: %v", field, err) }
			}
			if after := rrCounts(t, db); after != before { t.Fatalf("source failure wrote: %s -> %s", before, after) }
		})
	}
	for _, status := range []string{"finalized", "archived", "reviewed"} {
		t.Run(status, func(t *testing.T) {
			db, r, _ := rrFixture(t); mustExec(t, db, "UPDATE drafts SET status=? WHERE id=900001", status)
			input := rrInput(); input["expectedSource"].(map[string]any)["status"] = status
			if _, err := r.CreateReview(context.Background(), input); err != nil { t.Fatal(err) }
			if _, err := r.CreateRevision(context.Background(), input); err != nil { t.Fatal(err) }
		})
	}
}

func TestReviewRevisionInputValidationAndOwnerPresence(t *testing.T) {
	db, r, _ := rrFixture(t)
	for _, test := range []struct { key string; value any }{{"baseDraftId", "900001"}, {"content", false}, {"revisionType", "other"}, {"wordCount", math.NaN()}, {"wordCount", 1.5}, {"wordCount", -1}, {"reviewSourceId", "1"}, {"userPrompt", []any{}}} {
		input := rrInput(); input[test.key] = test.value
		before := rrCounts(t, db)
		if _, err := r.CreateRevision(context.Background(), input); !errors.Is(err, ErrReviewRevisionInvalidInput) { t.Fatalf("%s=%v: %v", test.key, test.value, err) }
		if after := rrCounts(t, db); before != after { t.Fatal("invalid input wrote") }
	}
	for _, key := range []string{"reviewCycleId", "generationRunHandle", "authorRecovery"} {
		input := rrInput(); input[key] = nil
		for _, method := range []func(context.Context, map[string]any) (map[string]any, error){r.CreateReview, r.CreateRevision, r.ReplacePending, r.MergeRevision} {
			if _, err := method(context.Background(), input); !errors.Is(err, ErrReviewCycleOwnerRequired) { t.Fatalf("owner %s: %v", key, err) }
		}
	}
}

func TestReviewRevisionMergeReplayAndNoOverwriteAfterAuthorEdit(t *testing.T) {
	db, r, _ := rrFixture(t); ctx := context.Background(); id := rrCreated(t, r)
	request := rrMerge(id)
	out, err := r.MergeRevision(ctx, request)
	if err != nil || out["idempotent"] != false || out["chapterNumber"] != int64(9) || out["version"] != int64(2) { t.Fatalf("merge %v %v", out, err) }
	request["expectedDraftContent"] = "stale baseline is ignored only on exact replay"
	out, err = r.MergeRevision(ctx, request)
	if err != nil || out["idempotent"] != true { t.Fatalf("replay %v %v", out, err) }
	if err := NewDrafts(db).UpdateContentIfCurrent(ctx, 900001, "作者后续编辑", 31, pointer(request["mergedContent"].(string))); err != nil { t.Fatal(err) }
	if _, err := r.MergeRevision(ctx, request); !errors.Is(err, ErrRevisionAlreadyMerged) { t.Fatalf("edited replay %v", err) }
	assertStored(t, db, 900001, "作者后续编辑", 31, "")
}

func TestReviewRevisionMergePrecedenceAndSourceDrift(t *testing.T) {
	cases := []struct { name, sql string; request func(map[string]any); want error }{
		{"missing target first", "DELETE FROM drafts WHERE id=900001; DELETE FROM revisions", nil, ErrDraftNotFound},
		{"missing revision", "DELETE FROM revisions", nil, ErrRevisionNotFound},
		{"target mismatch", "UPDATE revisions SET base_draft_id=900002,status='discarded'", nil, ErrRevisionTargetMismatch},
		{"not pending", "UPDATE revisions SET status='discarded'; UPDATE drafts SET status='finalized'", nil, ErrRevisionNotPending},
		{"not editable", "UPDATE drafts SET status='finalized'", func(m map[string]any) { m["expectedDraftContent"] = "wrong" }, ErrRevisionTargetNotEditable},
		{"target changed", "UPDATE revisions SET source_content=NULL", func(m map[string]any) { m["expectedDraftContent"] = "wrong" }, ErrRevisionTargetChanged},
		{"source missing", "UPDATE revisions SET source_content=NULL", nil, ErrRevisionSourceMissing},
		{"source chapter", "UPDATE drafts SET chapter_number=10", nil, ErrRevisionSourceMismatch},
		{"source version", "UPDATE drafts SET version=3", nil, ErrRevisionSourceMismatch},
		{"source status", "UPDATE drafts SET status='reviewed'", nil, ErrRevisionSourceMismatch},
		{"source body", "UPDATE contents SET body='edited' WHERE id=900001", func(m map[string]any) { m["expectedDraftContent"] = "edited" }, ErrRevisionSourceMismatch},
		{"source blob", "UPDATE revisions SET source_content=CAST(source_content AS BLOB)", nil, ErrRevisionSourceMismatch},
		{"source blob metadata", "UPDATE drafts SET chapter_number=CAST('9' AS BLOB); UPDATE revisions SET source_draft_chapter_number=CAST('9' AS BLOB)", nil, ErrRevisionSourceMismatch},
		{"merged target", "UPDATE revisions SET status='merged',merged_to_draft_id=900002", nil, ErrRevisionAlreadyMerged},
	}
	for _, test := range cases { t.Run(test.name, func(t *testing.T) {
		db, r, _ := rrFixture(t); id := rrCreated(t, r); mustExec(t, db, test.sql)
		request := rrMerge(id); if test.request != nil { test.request(request) }
		before := rrCounts(t, db)
		if _, err := r.MergeRevision(context.Background(), request); !errors.Is(err, test.want) { t.Fatalf("got %v want %v", err, test.want) }
		if after := rrCounts(t, db); before != after { t.Fatal("failed merge allocated rows") }
	}) }
}

func TestReviewRevisionAtomicRollbackTriggers(t *testing.T) {
	for _, operation := range []string{"ABORT", "ROLLBACK"} {
		for _, table := range []string{"reviews", "revisions"} { t.Run(operation+"/insert/"+table, func(t *testing.T) {
			db, r, _ := rrFixture(t); oldID := rrCreated(t, r); before := rrCounts(t, db)
			mustExec(t, db, fmt.Sprintf("CREATE TRIGGER fail_insert BEFORE INSERT ON %s BEGIN SELECT RAISE(%s,'metadata failure'); END", table, operation))
			method := r.CreateReview; if table == "revisions" { method = r.ReplacePending }
			if _, err := method(context.Background(), rrInput()); err == nil || !strings.Contains(err.Error(), "metadata failure") { t.Fatalf("trigger %v", err) }
			if after := rrCounts(t, db); before != after { t.Fatalf("leaked pool row %s %s", before, after) }
			old, err := r.FullRevision(context.Background(), oldID); if err != nil || old["status"] != "pending" { t.Fatalf("old pending changed %v %v", old, err) }
			mustExec(t, db, "DROP TRIGGER fail_insert")
			if _, err := method(context.Background(), rrInput()); err != nil { t.Fatalf("connection leaked %v", err) }
		}) }
		for _, table := range []string{"drafts", "revisions"} { t.Run(operation+"/merge/"+table, func(t *testing.T) {
			db, r, _ := rrFixture(t); id := rrCreated(t, r)
			mustExec(t, db, fmt.Sprintf("CREATE TRIGGER fail_merge BEFORE UPDATE ON %s BEGIN SELECT RAISE(%s,'merge metadata failure'); END", table, operation))
			if _, err := r.MergeRevision(context.Background(), rrMerge(id)); err == nil { t.Fatal("merge trigger ignored") }
			assertStored(t, db, 900001, "原稿\r\n😀\x00é ", 11, "updated-original")
			full, err := r.FullRevision(context.Background(), id); if err != nil || full["status"] != "pending" || full["mergedToDraftId"] != nil { t.Fatalf("half merged %v %v", full, err) }
			mustExec(t, db, "DROP TRIGGER fail_merge")
			if _, err := r.MergeRevision(context.Background(), rrMerge(id)); err != nil { t.Fatalf("recovery %v", err) }
		}) }
	}
}

func rrSecondPool(t *testing.T, path string) *sql.DB {
	t.Helper(); uri := url.URL{Scheme: "file", Path: path}; uri.RawQuery = "mode=rw&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String()); if err != nil { t.Fatal(err) }
	db.SetMaxOpenConns(2); t.Cleanup(func() { _ = db.Close() }); return db
}

func TestReviewRevisionIndependentPoolConcurrentIndex(t *testing.T) {
	for _, review := range []bool{true, false} { t.Run(fmt.Sprint(review), func(t *testing.T) {
		_, first, path := rrFixture(t); second := NewReviewRevisions(rrSecondPool(t, path))
		start := make(chan struct{}); results := make(chan map[string]any, 12); failures := make(chan error, 12); var ready sync.WaitGroup
		for i := 0; i < 12; i++ {
			r := first; if i%2 == 1 { r = second }; ready.Add(1)
			go func(r *ReviewRevisions) { ready.Done(); <-start; var out map[string]any; var err error
				if review { out, err = r.CreateReview(context.Background(), rrInput()) } else { out, err = r.CreateRevision(context.Background(), rrInput()) }
				results <- out; failures <- err
			}(r)
		}
		ready.Wait(); close(start); indices := map[int64]bool{}; key := "revisionIndex"; if review { key = "reviewIndex" }
		for i := 0; i < 12; i++ { out := <-results; if err := <-failures; err != nil { t.Fatal(err) }; indices[out[key].(int64)] = true }
		for i := int64(1); i <= 12; i++ { if !indices[i] { t.Fatalf("missing index %d: %v", i, indices) } }
	}) }
}

func TestReviewRevisionIndependentPoolConcurrentMerge(t *testing.T) {
	for _, sameRevision := range []bool{false, true} { t.Run(fmt.Sprint(sameRevision), func(t *testing.T) {
		db, first, path := rrFixture(t); second := NewReviewRevisions(rrSecondPool(t, path)); id1 := rrCreated(t, first); id2 := id1
		if !sameRevision { id2 = rrCreated(t, first) }
		start := make(chan struct{}); type result struct { out map[string]any; err error }; results := make(chan result, 2)
		for i, r := range []*ReviewRevisions{first, second} { id := id1; if i == 1 { id = id2 }; go func(r *ReviewRevisions, id int64) { <-start; out, err := r.MergeRevision(context.Background(), rrMerge(id)); results <- result{out, err} }(r, id) }
		close(start); success, replay, conflict := 0, 0, 0
		for i := 0; i < 2; i++ { result := <-results; if result.err == nil { success++; if result.out["idempotent"] == true { replay++ } } else if errors.Is(result.err, ErrRevisionTargetChanged) { conflict++ } else { t.Fatalf("unexpected race error %v", result.err) } }
		if sameRevision { if success != 2 || replay != 1 { t.Fatalf("same revision %d/%d", success, replay) } } else if success != 1 || conflict != 1 { t.Fatalf("different revisions %d/%d", success, conflict) }
		assertStored(t, db, 900001, "合并\r\n𠮷\x00", 23, "")
	}) }
}

// Cycle rows are inserted into the real original schema, with FK enforcement
// deliberately left at the driver's default for isolated malformed-binding tests.
func rrBindCycle(t *testing.T, db *sql.DB, revisionID, reviewID int64, confirmation bool) {
	t.Helper()
	var revisionValue any
	status := "not-generated"
	if revisionID != 0 { revisionValue = revisionID; status = "generated" }
	var confirmationID, confirmationHash any
	if confirmation { confirmationID = reviewID; confirmationHash = strings.Repeat("b", 64) }
	rootReview := reviewID; if confirmation { rootReview = 999999 }
	mustExec(t, db, `INSERT INTO review_cycles(cycle_id,root_action_id,review_id,review_content_hash,
 confirmation_review_id,confirmation_content_hash,revision_id,source_hash,finding_set_hash,comparison_version,revision_status,recheck_count)
 VALUES('test-cycle','test-root',?,?,?,?,?,?,?,1,?,0)`, rootReview, strings.Repeat("a", 64), confirmationID, confirmationHash, revisionValue, strings.Repeat("c", 64), strings.Repeat("d", 64), status)
}

func TestReviewRevisionCycleBindingsFailClosedWithoutDetach(t *testing.T) {
	for _, binding := range []string{"revision", "review", "confirmation"} { t.Run(binding, func(t *testing.T) {
		db, r, _ := rrFixture(t); ctx := context.Background()
		review, err := r.CreateReview(ctx, rrInput()); if err != nil { t.Fatal(err) }
		reviewID := review["id"].(int64); input := rrInput()
		if binding != "revision" { input["reviewSourceId"] = reviewID }
		created, err := r.CreateRevision(ctx, input); if err != nil { t.Fatal(err) }; id := created["id"].(int64)
		cycleRevision := int64(0); if binding == "revision" { cycleRevision = id }
		rrBindCycle(t, db, cycleRevision, reviewID, binding == "confirmation")
		before := rrCounts(t, db)
		if _, err := r.MergeRevision(ctx, rrMerge(id)); !errors.Is(err, ErrReviewCycleOwnerRequired) { t.Fatalf("merge cycle %v", err) }
		if err := r.MarkDiscarded(ctx, id); !errors.Is(err, ErrReviewCycleOwnerRequired) { t.Fatalf("discard cycle %v", err) }
		if _, err := r.ReplacePending(ctx, rrInput()); !errors.Is(err, ErrReviewCycleOwnerRequired) { t.Fatalf("replace cycle %v", err) }
		want := ErrReviewCycleOwnerRequired; if binding == "revision" { want = ErrRevisionMergeRequired }
		if err := r.MarkMerged(ctx, id, 900001); !errors.Is(err, want) { t.Fatalf("direct mark cycle %v", err) }
		if binding != "revision" { if _, err := r.CreateRevision(ctx, input); !errors.Is(err, ErrReviewCycleOwnerRequired) { t.Fatalf("create linked %v", err) } }
		if after := rrCounts(t, db); before != after { t.Fatal("cycle failure leaked writes") }
		full, err := r.FullRevision(ctx, id); if err != nil || full["status"] != "pending" { t.Fatalf("detached/discarded %v %v", full, err) }
		assertStored(t, db, 900001, "原稿\r\n😀\x00é ", 11, "updated-original")
		var status string; if err := db.QueryRow("SELECT revision_status FROM review_cycles WHERE cycle_id='test-cycle'").Scan(&status); err != nil { t.Fatal(err) }
		if status == "merge-committed" || (binding == "revision" && status != "generated") { t.Fatalf("cycle changed %s", status) }
	}) }
}

func TestReviewRevisionBlobAndRealReadParity(t *testing.T) {
	db, r, _ := rrFixture(t); id := rrCreated(t, r)
	mustExec(t, db, `UPDATE revisions SET source_draft_chapter_number=9.5,source_draft_version=2.5,user_prompt=CAST('prompt' AS BLOB) WHERE id=?`, id)
	full, err := r.FullRevision(context.Background(), id)
	if err != nil { t.Fatal(err) }
	source, ok := full["sourceDraft"].(map[string]any)
	if !ok || source["chapterNumber"] != 9.5 || source["version"] != 2.5 { t.Fatalf("REAL source dropped %v", full) }
	if body, ok := full["userPrompt"].([]byte); !ok || string(body) != "prompt" { t.Fatalf("BLOB coerced %v", full) }
	mustExec(t, db, "UPDATE revisions SET source_content=CAST(source_content AS BLOB) WHERE id=?", id)
	full, err = r.FullRevision(context.Background(), id)
	if err != nil || full["sourceDraft"] != nil { t.Fatalf("BLOB source exposed as string %v %v", full, err) }
}

func TestReviewRevisionContentAliasesFailClosed(t *testing.T) {
	for _, alias := range []string{"draft", "review", "revision"} { t.Run(alias, func(t *testing.T) {
		db, r, _ := rrFixture(t); id := rrCreated(t, r)
		switch alias {
		case "draft": mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(900002,10,1,'finalized',900001)")
		case "review": mustExec(t, db, "INSERT INTO reviews(base_draft_id,review_index,content_id) VALUES(900001,1,900001)")
		case "revision": mustExec(t, db, "UPDATE revisions SET content_id=900001 WHERE id=?", id)
		}
		if _, err := r.MergeRevision(context.Background(), rrMerge(id)); !errors.Is(err, ErrContentOwnershipConflict) { t.Fatalf("alias %s: %v", alias, err) }
		assertStored(t, db, 900001, "原稿\r\n😀\x00é ", 11, "updated-original")
		full, err := r.FullRevision(context.Background(), id); if err != nil || full["status"] != "pending" { t.Fatalf("alias half merged %v %v", full, err) }
	}) }
}

func TestReviewRevisionCancellationRollsBackAndPoolRecovers(t *testing.T) {
	db, r, _ := rrFixture(t); db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	err := r.immediate(ctx, func(conn *sql.Conn) error {
		if _, err := conn.ExecContext(ctx, "INSERT INTO contents(body) VALUES('cancelled write')"); err != nil { return err }
		cancel(); return ctx.Err()
	})
	if !errors.Is(err, context.Canceled) { t.Fatalf("cancel %v", err) }
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM contents WHERE body='cancelled write'").Scan(&count); err != nil || count != 0 { t.Fatalf("cancel leaked %d %v", count, err) }
	if _, err := r.CreateReview(context.Background(), rrInput()); err != nil { t.Fatalf("pool recovery %v", err) }
}

func TestReviewRevisionNilDatabaseMatchesOriginalReads(t *testing.T) {
	r := NewReviewRevisions(nil); ctx := context.Background()
	if rows, err := r.ListReviews(ctx, 1); err != nil || rows == nil || len(rows) != 0 { t.Fatalf("nil list %v %v", rows, err) }
	if full, err := r.FullReview(ctx, 1); err != nil || full != nil { t.Fatalf("nil full %v %v", full, err) }
	if n, err := r.NextRevisionIndex(ctx, 1); err != nil || n != 1 { t.Fatalf("nil next %d %v", n, err) }
	if _, err := r.CreateReview(ctx, rrInput()); !errors.Is(err, ErrReviewRevisionProjectNotOpen) { t.Fatalf("nil create %v", err) }
}

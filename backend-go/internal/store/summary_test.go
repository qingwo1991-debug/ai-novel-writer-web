package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
)

// Reuse the original TypeScript baseline-v7 byte-copy, never an invented schema.
func summaryFixture(t *testing.T) (*sql.DB, *Summary, string) {
	t.Helper()
	db, _, path := narrativeFixture(t)
	mustExec(t, db, "DELETE FROM summary_snapshots; UPDATE continuity_projection_meta SET generation=0,stale_from_chapter=NULL WHERE id='main'")
	return db, NewSummary(db), path
}
func summarySeed(t *testing.T, db *sql.DB, id, chapter, version int, status, content string, receipt bool) map[string]any {
	t.Helper()
	mustExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, content)
	mustExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id,word_count) VALUES(?,?,?,?,?,?)", id, chapter, version, status, id, len(content))
	source := map[string]any{"draftId": id, "chapterNumber": chapter, "finalizationId": fmt.Sprintf("summary-final-%d", id), "contentHash": summaryHash(content)}
	if receipt { mustExec(t, db, `INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,content_hash,content_revision,content_snapshot,target_file_name) VALUES(?,?,?,?,?,1,?,'summary.txt')`, source["finalizationId"], id, chapter, "冻结标题😀", source["contentHash"], content) }
	return source
}
func summaryProjection(t *testing.T, db *sql.DB, source map[string]any, bound bool) {
	t.Helper()
	finalization, hash := "", ""
	if bound { finalization, hash = summaryText(source["finalizationId"]), summaryText(source["contentHash"]) }
	mustExec(t, db, `INSERT INTO summary_snapshots(draft_id,chapter_number,chapter_notes,source_finalization_id,source_content_hash) VALUES(?,?,' 摘要 ',?,?)`, source["draftId"], source["chapterNumber"], finalization, hash)
}
func summaryFreeze(t *testing.T, db *sql.DB, source map[string]any, characters []any) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"kind": summarySnapshotKind, "source": source, "identityRevision": 0, "characters": characters})
	if err != nil { t.Fatal(err) }
	mustExec(t, db, `INSERT OR REPLACE INTO character_identity_proposals(proposal_id,source_key,source_hash,raw_value,candidate_ids_json) VALUES(?,?,?,?, '[]')`, "fcs:"+summaryText(source["finalizationId"]), summarySnapshotKind, summaryHash(string(raw)), string(raw))
}
func summaryList(t *testing.T, s *Summary) []map[string]any {
	t.Helper(); rows, err := s.ListBefore(context.Background(), 990000); if err != nil { t.Fatal(err) }; return rows
}
func summaryWant(t *testing.T, err, want error) { t.Helper(); if !errors.Is(err, want) { t.Fatalf("error=%v want=%v", err, want) } }

func TestSummaryMissingProjectAndStrictInput(t *testing.T) {
	ctx := context.Background()
	for _, s := range []*Summary{nil, NewSummary(nil), {}} {
		summaryWant(t, s.SaveSnapshot(ctx, true, string([]byte{255})), nil)
		if row, err := s.LatestSnapshot(ctx); row != nil || err != nil { t.Fatalf("latest %v %v", row, err) }
		if row, err := s.ReadFinalizedSource(ctx, true); err != nil || !reflect.DeepEqual(row, map[string]any{"status":"invalid"}) { t.Fatalf("source %v %v", row, err) }
		if rows, err := s.ListBefore(ctx, true); err != nil || rows == nil || len(rows) != 0 { t.Fatalf("list %v %v", rows, err) }
	}
	_, s, _ := summaryFixture(t)
	for _, value := range []any{nil, true, "1", 1.5, math.NaN(), math.Inf(1), int64(9007199254740992), int64(-9007199254740992), []any{1}, map[string]any{}} {
		summaryWant(t, s.SaveSnapshot(ctx, value, "states"), ErrSummaryInputUnsupported)
		if rows, err := s.ListBefore(ctx, value); rows != nil || !errors.Is(err, ErrSummaryTargetChapterInvalid) { t.Fatalf("list input %v: %v %v", value, rows, err) }
		if row, err := s.ReadFinalizedSource(ctx, value); row != nil || !errors.Is(err, ErrSummarySourceDraftInvalid) { t.Fatalf("source input %v: %v %v", value, row, err) }
	}
	for _, value := range []any{0, -1} {
		if rows, err := s.ListBefore(ctx, value); rows != nil || !errors.Is(err, ErrSummaryTargetChapterInvalid) { t.Fatalf("nonpositive list %v %v", rows, err) }
		if row, err := s.ReadFinalizedSource(ctx, value); row != nil || !errors.Is(err, ErrSummarySourceDraftInvalid) { t.Fatalf("nonpositive source %v %v", row, err) }
	}
	for _, text := range []string{string([]byte{255}), strings.Repeat("x", (32<<20)+1)} { summaryWant(t, s.SaveSnapshot(ctx, 1, text), ErrSummaryInputUnsupported) }
}
func TestSummaryLegacySaveIsolationLatestAndCancellation(t *testing.T) {
	db, s, _ := summaryFixture(t); ctx := context.Background(); db.SetMaxOpenConns(1)
	if row, err := s.LatestSnapshot(ctx); err != nil || row != nil { t.Fatalf("empty %v %v", row, err) }
	for _, chapter := range []any{int32(-2), json.Number("0"), float64(4), int64(-9007199254740991)} { summaryWant(t, s.SaveSnapshot(ctx, chapter, " 原样😀\x00 "), nil) }
	row, err := s.LatestSnapshot(ctx)
	if err != nil || row["chapterNumber"] != int64(-9007199254740991) || row["characterStates"] != " 原样😀\x00 " || len(row) != 2 { t.Fatalf("latest insertion order %v %v", row, err) }
	source := summarySeed(t, db, 880001, 880001, 1, "finalized", "正文", true); summaryProjection(t, db, source, true)
	latest, err := s.LatestSnapshot(ctx); if err != nil || !reflect.DeepEqual(row, latest) { t.Fatalf("formal row leaked %v %v", latest, err) }
	cancelled, cancel := context.WithCancel(ctx); cancel()
	summaryWant(t, s.SaveSnapshot(cancelled, 77, "cancelled"), context.Canceled)
	if row, err := s.LatestSnapshot(cancelled); row != nil || !errors.Is(err, context.Canceled) { t.Fatalf("cancel latest %v %v", row, err) }
	if row, err := s.ReadFinalizedSource(cancelled, 880001); row != nil || !errors.Is(err, context.Canceled) { t.Fatalf("cancel source %v %v", row, err) }
	if rows, err := s.ListBefore(cancelled, 990000); rows != nil || !errors.Is(err, context.Canceled) { t.Fatalf("cancel list %v %v", rows, err) }
	summaryWant(t, s.SaveSnapshot(ctx, 78, "connection reusable"), nil)
}
func TestSummarySaveTriggerRollbackAndReadBack(t *testing.T) {
	db, s, _ := summaryFixture(t); ctx := context.Background()
	summaryWant(t, s.SaveSnapshot(ctx, 3, "preserved"), nil)
	for _, trigger := range []string{
		`CREATE TRIGGER summary_reject AFTER INSERT ON summary_snapshots BEGIN UPDATE summary_snapshots SET character_states='side effect' WHERE chapter_number=3; SELECT RAISE(FAIL,'summary fail'); END`,
		`CREATE TRIGGER summary_reject AFTER INSERT ON summary_snapshots BEGIN UPDATE summary_snapshots SET chapter_number=123 WHERE id=NEW.id; END`,
		`CREATE TRIGGER summary_reject AFTER INSERT ON summary_snapshots BEGIN UPDATE summary_snapshots SET character_states='rewrite' WHERE id=NEW.id; END`,
		`CREATE TRIGGER summary_reject AFTER INSERT ON summary_snapshots BEGIN UPDATE summary_snapshots SET chapter_notes='formal authority' WHERE id=NEW.id; END`,
		`CREATE TRIGGER summary_reject BEFORE INSERT ON summary_snapshots BEGIN SELECT RAISE(ABORT,'summary abort'); END`,
	} {
		mustExec(t, db, trigger)
		if err := s.SaveSnapshot(ctx, 5, "rejected"); err == nil { t.Fatal("trigger mutation accepted") }
		mustExec(t, db, "DROP TRIGGER summary_reject")
		rows, err := rrMaps(ctx, db, "SELECT chapter_number,character_states FROM summary_snapshots")
		if err != nil || len(rows) != 1 || rows[0]["chapter_number"] != int64(3) || rows[0]["character_states"] != "preserved" { t.Fatalf("not rolled back %v %v", rows, err) }
	}
}
func TestSummaryFinalizedSourceActualReceiptIntegrity(t *testing.T) {
	db, s, _ := summaryFixture(t); ctx := context.Background()
	if row, err := s.ReadFinalizedSource(ctx, json.Number("880100")); err != nil || row["status"] != "invalid" { t.Fatalf("missing %v %v", row, err) }
	legacy := summarySeed(t, db, 880100, 880100, 1, "finalized", "旧正文😀", false)
	mustExec(t, db, "INSERT INTO blueprints(chapter_number,title) VALUES(880100,'蓝图标题')")
	row, err := s.ReadFinalizedSource(ctx, 880100)
	if err != nil || row["status"] != "legacy" || row["content"] != "旧正文😀" || row["chapterTitle"] != "蓝图标题" || len(row) != 5 { t.Fatalf("legacy %v %v %v", row, err, legacy) }
	summarySeed(t, db, 880101, 880101, 1, "finalized", "定稿😀\u2028\u2029\x00正文", true)
	row, err = s.ReadFinalizedSource(ctx, 880101)
	if err != nil || row["status"] != "valid" { t.Fatalf("valid %v %v", row, err) }
	snapshot := row["snapshot"].(map[string]any)
	if snapshot["content"] != "定稿😀\u2028\u2029\x00正文" || snapshot["chapterTitle"] != "冻结标题😀" || snapshot["projectionGeneration"] != int64(0) { t.Fatal(snapshot) }
	for _, change := range []string{
		"UPDATE drafts SET status='superseded' WHERE id=880101",
		"UPDATE finalization_outbox SET finalization_id=' \ufeff ' WHERE draft_id=880101",
		"UPDATE finalization_outbox SET content_hash=upper(content_hash) WHERE draft_id=880101",
		"UPDATE finalization_outbox SET content_snapshot='different' WHERE draft_id=880101",
		"UPDATE contents SET body='edited' WHERE id=880101",
		"DELETE FROM continuity_projection_meta WHERE id='main'",
	} {
		mustExec(t, db, change)
		row, err = s.ReadFinalizedSource(ctx, 880101)
		if err != nil || row["status"] != "invalid" { t.Fatalf("corruption %s: %v %v", change, row, err) }
		mustExec(t, db, "UPDATE drafts SET status='finalized' WHERE id=880101")
		mustExec(t, db, "UPDATE contents SET body=? WHERE id=880101", "定稿😀\u2028\u2029\x00正文")
		mustExec(t, db, "UPDATE finalization_outbox SET finalization_id='summary-final-880101',content_snapshot=?,content_hash=? WHERE draft_id=880101", "定稿😀\u2028\u2029\x00正文", summaryHash("定稿😀\u2028\u2029\x00正文"))
		mustExec(t, db, "INSERT OR IGNORE INTO continuity_projection_meta(id) VALUES('main')")
	}
}
func TestSummaryListOriginalSelectionAndGeneration(t *testing.T) {
	db, s, _ := summaryFixture(t)
	old := summarySeed(t, db, 880201, 880200, 1, "superseded", "旧正文", true); summaryProjection(t, db, old, true)
	current := summarySeed(t, db, 880202, 880200, 2, "finalized", "新正文😀", true)
	rows := summaryList(t, s)
	if len(rows) != 1 || rows[0]["sourceStatus"] != "stale" || rows[0]["currentFinalizedDraftId"] != int64(880202) { t.Fatalf("superseded fallback %v", rows) }
	summaryProjection(t, db, current, true)
	rows = summaryList(t, s)
	if len(rows) != 1 || rows[0]["draftId"] != int64(880202) || rows[0]["sourceStatus"] != "legacy" { t.Fatalf("newest summary, identity snapshot absent %v", rows) }
	summaryFreeze(t, db, current, []any{})
	rows = summaryList(t, s); if rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
	mustExec(t, db, "UPDATE continuity_projection_meta SET generation=1,stale_from_chapter=880201 WHERE id='main'")
	rows = summaryList(t, s); if rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
	mustExec(t, db, "UPDATE continuity_projection_meta SET stale_from_chapter=880200 WHERE id='main'")
	rows = summaryList(t, s); if rows[0]["sourceStatus"] != "stale" { t.Fatal(rows) }
	mustExec(t, db, "UPDATE summary_snapshots SET projection_generation=1 WHERE draft_id=880202")
	rows = summaryList(t, s); if rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
	// Same-version authority chooses id DESC, even though the normal index
	// prevents ties. Dropping only this fixture index models historical input.
	mustExec(t, db, "DROP INDEX idx_drafts_chapter_version")
	summarySeed(t, db, 880203, 880200, 2, "finalized", "同版新定稿", true)
	rows = summaryList(t, s); if rows[0]["currentFinalizedDraftId"] != int64(880203) || rows[0]["sourceStatus"] != "stale" { t.Fatal(rows) }
	// The original latest-nonempty-summary suppression sees legacy NULL rows too.
	mustExec(t, db, "INSERT INTO summary_snapshots(chapter_number,chapter_notes) VALUES(880200,'legacy suppressor')")
	if rows = summaryList(t, s); len(rows) != 0 { t.Fatal(rows) }
	mustExec(t, db, "UPDATE summary_snapshots SET chapter_notes='' WHERE draft_id IS NULL")
	if rows = summaryList(t, s); len(rows) != 1 { t.Fatal(rows) }
	mustExec(t, db, "UPDATE drafts SET status='archived' WHERE id=880202")
	if rows = summaryList(t, s); len(rows) != 0 { t.Fatal(rows) }
}
func TestSummaryFactsAndCandidateParserOriginalQuirks(t *testing.T) {
	facts := summaryFacts(`[{"category":["plot"],"entities":[" 阿青 ","阿青"],"statement":" 陈述 ","sourceChapter":7,"evidence":" 证据 ","characterRefs":[{"characterId":" id ","displayNameSnapshot":"阿青","extra":1}]}]`, int64(7))
	if len(facts) != 1 || facts[0]["statement"] != "陈述" || !reflect.DeepEqual(facts[0]["entities"], []string{"阿青"}) || !reflect.DeepEqual(facts[0]["category"], []any{"plot"}) { t.Fatal(facts) }
	for _, raw := range []string{`null`, `[] trailing`, `[{}]`, `[{"category":"plot","sourceChapter":"7","statement":"x","evidence":"x"}]`, `[{"category":"plot","sourceChapter":7,"statement":"x","evidence":"x","entities":[" "]}]`, `[{"category":"plot","sourceChapter":7,"statement":"x","evidence":"x","entities":["x"],"characterRefs":[{"characterId":"c","displayNameSnapshot":"other"}]}]`} {
		if out := summaryFacts(raw, int64(7)); out == nil || len(out) != 0 { t.Fatalf("facts %s -> %v", raw, out) }
	}
	source := `{"draftId":1,"chapterNumber":7,"finalizationId":"f","contentHash":"`+strings.Repeat("a",64)+`"}`
	raw := `{"version":2,"pending":[{"characterName":" 阿青 ","field":["location"],"value":" trim ","rawValue":null,"evidence":false},{"characterName":"阿青","field":"location","value":"  bound  ","candidateKey":"k","source":`+source+`,"expectedFieldRevision":0,"expectedFieldValueHash":"`+strings.Repeat("b",64)+`","rawValue":{"n":1.25,"a":[true,null]},"evidence":{"start":0,"end":2,"text":"证据"}},{"characterName":"阿青","field":"location","value":"drop","source":null},{"characterName":"阿青","field":"bad","value":"drop"}],"receipts":[{}]}`
	candidates := summaryCandidates(raw)
	if len(candidates) != 2 || candidates[0]["value"] != "trim" || candidates[1]["value"] != "  bound  " { t.Fatal(candidates) }
	if value, exists := candidates[0]["rawValue"]; !exists || value != nil { t.Fatal(candidates) }
	if _, exists := candidates[0]["evidence"]; exists { t.Fatal(candidates) }
	if candidates[1]["rawValue"].(map[string]any)["n"] != json.Number("1.25") { t.Fatal(candidates) }
	for _, raw := range []string{`[] trailing`, `{"version":1,"pending":[{}]}`, `null`} { if out := summaryCandidates(raw); out == nil || len(out) != 0 { t.Fatal(out) } }
}
func TestSummaryNativeJSONUnicodeBoundary(t *testing.T) {
	for _, text := range []string{`{"v":"\ud83d\ude00"}`, `{"v":"\\ud800"}`, `"plain😀"`, `not JSON`, `"\ud800" trailing`} {
		if err := summaryJSONSupported(text); err != nil { t.Fatalf("supported %q: %v", text, err) }
	}
	for _, text := range []string{`"\ud800"`, `"\udc00"`, `{"v":"\ud800x"}`, `"\ud800\u0041"`, string([]byte{255})} {
		summaryWant(t, summaryJSONSupported(text), ErrSummaryInputUnsupported)
	}
	db, s, _ := summaryFixture(t)
	source := summarySeed(t, db, 880401, 880401, 1, "finalized", "正文😀", true); summaryProjection(t, db, source, true)
	mustExec(t, db, "UPDATE summary_snapshots SET character_state_candidates=? WHERE draft_id=880401", `[{"characterName":"甲","field":"location","value":"x","rawValue":"\ud800"}]`)
	rows, err := s.ListBefore(context.Background(), 990000)
	if rows != nil || !errors.Is(err, ErrSummaryInputUnsupported) { t.Fatalf("lone surrogate silently rewritten %v %v", rows, err) }
	mustExec(t, db, "UPDATE summary_snapshots SET character_state_candidates='[]' WHERE draft_id=880401")
	mustExec(t, db, "UPDATE contents SET body=? WHERE id=880401", string([]byte{255}))
	row, err := s.ReadFinalizedSource(context.Background(), 880401)
	if row != nil || !errors.Is(err, ErrSummaryInputUnsupported) { t.Fatalf("bad UTF-8 source %v %v", row, err) }
}
func TestSummaryFrozenIdentityValidationAndReadOnlyBytes(t *testing.T) {
	db, s, path := summaryFixture(t); ctx := context.Background(); db.SetMaxOpenConns(1)
	source := summarySeed(t, db, 880301, 880301, 1, "finalized", "阿青到了城门😀", true); summaryProjection(t, db, source, true)
	characters := []any{map[string]any{"characterId":"char-a","displayNameSnapshot":"阿青","aliases":[]any{"小青"}}}
	summaryFreeze(t, db, source, characters)
	fact := `[{"category":"character-state","sourceChapter":880301,"entities":["阿青"],"statement":"到了城门","evidence":"阿青"}]`
	mustExec(t, db, "UPDATE summary_snapshots SET continuity_facts=? WHERE draft_id=880301", fact)
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "legacy" { t.Fatal(rows) }
	fact = strings.Replace(fact, `"statement"`, `"characterRefs":[{"characterId":"char-a","displayNameSnapshot":"阿青"}],"statement"`, 1)
	mustExec(t, db, "UPDATE summary_snapshots SET continuity_facts=? WHERE draft_id=880301", fact)
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
	mustExec(t, db, "UPDATE summary_snapshots SET continuity_facts=replace(continuity_facts,'char-a','wrong') WHERE draft_id=880301")
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "stale" { t.Fatal(rows) }
	mustExec(t, db, "UPDATE summary_snapshots SET continuity_facts=? WHERE draft_id=880301", fact)
	mustExec(t, db, "UPDATE character_identity_proposals SET source_hash=? WHERE proposal_id=?", strings.Repeat("0",64), "fcs:"+summaryText(source["finalizationId"]))
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "stale" { t.Fatal(rows) }
	summaryFreeze(t, db, source, append(characters, characters[0]))
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "stale" { t.Fatal(rows) }
	summaryFreeze(t, db, source, characters)
	before, err := os.ReadFile(path); if err != nil { t.Fatal(err) }
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
	if row, err := s.ReadFinalizedSource(ctx, 880301); err != nil || row["status"] != "valid" { t.Fatalf("source %v %v", row, err) }
	if _, err := s.LatestSnapshot(ctx); err != nil { t.Fatal(err) }
	after, err := os.ReadFile(path); if err != nil || !bytes.Equal(before, after) { t.Fatalf("read methods changed database bytes: %v", err) }
	// Availability is table existence, not a nonzero revision or populated meta.
	mustExec(t, db, "DELETE FROM character_identity_meta")
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
	mustExec(t, db, "DROP TABLE character_identity_meta")
	mustExec(t, db, "UPDATE character_identity_proposals SET source_hash='bad' WHERE proposal_id=?", "fcs:"+summaryText(source["finalizationId"]))
	if rows := summaryList(t, s); rows[0]["sourceStatus"] != "current" { t.Fatal(rows) }
}

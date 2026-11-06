package store

// These tests provision a real SQLite database from the pinned asset in
// internal/admission (WriteBaselineV7, baseline_v7_ddl.go:744) and drive the
// lifecycle writes of importrun_lifecycle.go against the real schema: the
// parsing run, its sources and their frozen chapters (import_run_sources,
// import_run_source_chapters), the stable chapter map
// (import_source_chapter_map, PRIMARY KEY (purpose, source_id,
// source_chapter_number) and UNIQUE (purpose, chapter_number)) and the adopted
// chapters (import_run_chapters).
//
// Every effect is proved twice over: once by a separate mode=ro connection that
// could not have performed the write, and - for every refusal - once by the
// file's own bytes, which a refused call must leave untouched. Each refusal is
// exercised on its own fresh database, so one refusal cannot hide behind a
// previous one's state.
//
// Two refusals are deliberately not exercised because no state can reach them:
//
//   - BeginParsing's IMPORT_RUN_ID_CONFLICT (:1690): the explicit-run read at
//     the head of the same transaction already answered for that id.
//   - Restart's IMPORT_RUN_EXECUTOR_BUSY: the fence compares the epoch this
//     same transaction just read, and no other writer can run inside it.
//
// IMPORT_RUN_CONTENT_TOO_LARGE needs more than 128 MiB of chapter text before
// the aggregate bound trips, which this suite does not allocate.

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

const (
	irlRunID         = "run-parsing-1"
	irlNextRunID     = "run-parsing-2"
	irlBaseRunID     = "run-base-1"
	irlSourceID      = "5c1f2a9e-6b1d-4c2f-9a3e-1d2b3c4d5e6f"
	irlOtherSourceID = "0f8e7d6c-5b4a-4392-8170-6f5e4d3c2b1a"
	irlOwner         = "owner-parsing"
	irlDisplayName   = "第一章-山雨.txt"
)

// irlChapters is one source's parsed output, with the fingerprints the caller is
// supposed to have computed over the frozen bytes.
var irlChapters = []ImportRunChapterInput{
	irlChapter(1, "第一章 山雨", "山雨欲来，风满楼。"),
	irlChapter(2, "第二章 夜渡", "夜色沉沉，江上无人。"),
}

func irlChapter(number int64, title, content string) ImportRunChapterInput {
	sum := sha256.Sum256([]byte(content))
	return ImportRunChapterInput{Number: number, Title: title, Content: content,
		ContentFingerprint: hex.EncodeToString(sum[:]), ContentSize: int64(len(content))}
}

// irlFixture provisions the pinned v7 schema in a fresh file, then opens it the
// way a project database is opened (ExistingDSN: mode=rw, foreign keys on).
func irlFixture(t *testing.T) (*sql.DB, *ImportRuns, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "import lifecycle #? 中文.db")
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000&_foreign_keys=on"
	seed, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	if err := admission.WriteBaselineV7(path, seed); err != nil {
		_ = seed.Close()
		t.Fatalf("provision baseline v7: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	dsn, err := ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	return db, NewImportRuns(db), path
}

// irlReadOnly opens the same file a second time, read-only, so a write is proved
// by a connection that could not have performed it.
func irlReadOnly(t *testing.T, path string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	uri.RawQuery = "mode=ro&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Ping(); err != nil {
		t.Fatalf("read-only connection: %v", err)
	}
	return db
}

func irlExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

func irlQueryRows(t *testing.T, db *sql.DB, query string, args ...any) []map[string]any {
	t.Helper()
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([]map[string]any, 0)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range values {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatal(err)
		}
		entry := make(map[string]any, len(columns))
		for index, column := range columns {
			if raw, ok := values[index].([]byte); ok {
				entry[column] = string(raw)
				continue
			}
			entry[column] = values[index]
		}
		result = append(result, entry)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

func irlQueryOne(t *testing.T, db *sql.DB, query string, args ...any) map[string]any {
	t.Helper()
	rows := irlQueryRows(t, db, query, args...)
	if len(rows) != 1 {
		t.Fatalf("query %q returned %d rows, want 1", query, len(rows))
	}
	return rows[0]
}

func irlCount(t *testing.T, db *sql.DB, query string, args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func irlText(t *testing.T, row map[string]any, column string) string {
	t.Helper()
	text, ok := row[column].(string)
	if !ok {
		t.Fatalf("%s is %T, want text", column, row[column])
	}
	return text
}

func irlInt(t *testing.T, row map[string]any, column string) int64 {
	t.Helper()
	number, ok := row[column].(int64)
	if !ok {
		t.Fatalf("%s is %T, want an integer", column, row[column])
	}
	return number
}

func irlObject(t *testing.T, value any, label string) map[string]any {
	t.Helper()
	record, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%s is %T, want an object", label, value)
	}
	return record
}

func irlNested(t *testing.T, response map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := response[key]
	if !ok {
		t.Fatalf("response has no %s", key)
	}
	return irlObject(t, value, key)
}

// irlDigest is the whole file's bytes, so a refusal that wrote anything at all
// fails even if it rolled the logical rows back.
func irlDigest(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// irlRefusal runs one refused call and proves both the code and that the
// database is byte-identical afterwards.
func irlRefusal(t *testing.T, path, code string, call func() error) {
	t.Helper()
	before := irlDigest(t, path)
	err := call()
	if err == nil {
		t.Fatalf("%s: expected a refusal, got none", code)
	}
	if err.Error() != code {
		t.Fatalf("%s: got %q", code, err.Error())
	}
	if after := irlDigest(t, path); after != before {
		t.Fatalf("%s changed the database", code)
	}
}

// irlUTF16Len is JavaScript's .length: what the original bounds a stored failure
// reason by.
func irlUTF16Len(value string) int {
	length := 0
	for _, r := range value {
		if r > 0xFFFF {
			length += 2
			continue
		}
		length++
	}
	return length
}

// irlBeginRequest is a one-source reference request: the shape
// db:import-run-prepare-inspection binds from the inspection it just showed.
func irlBeginRequest(runID, sourceID, displayName string) ImportRunBeginParsingRequest {
	return ImportRunBeginParsingRequest{RunID: runID, Purpose: "reference",
		SourceFingerprint: strings.Repeat("a", 64),
		SourceIDs: []string{sourceID},
		SourceFingerprints: []string{strings.Repeat("b", 64)},
		SourceDisplay: []ImportRunSourceDisplay{{DisplayName: displayName, MediaType: "text/plain", Size: 128}},
		Locale: "zh-CN"}
}

// irlRunSeed is one import_runs row to seed. Empty strings take the schema's own
// defaults, so a seed only states what the test is about.
type irlRunSeed struct {
	id                  string
	stage               string
	status              string
	purpose             string
	locale              string
	lastError           string
	owner               string
	manifestFingerprint string
	resumable           int64
	epoch               int64
	leaseExpiresAt      int64
	totalChapters       int64
	totalContentSize    int64
	manifestChapters    int64
	manifestSize        int64
	manifestWords       int64
	completedChapters   int64
	baseRunID           sql.NullString
}

func irlSeedRun(t *testing.T, db *sql.DB, seed irlRunSeed) {
	t.Helper()
	purpose := seed.purpose
	if purpose == "" {
		purpose = "reference"
	}
	locale := seed.locale
	if locale == "" {
		locale = "zh-CN"
	}
	manifest := seed.manifestFingerprint
	if manifest == "" {
		manifest = strings.Repeat("0", 64)
	}
	irlExec(t, db, `INSERT INTO import_runs (
 id, purpose, root_run_id, effect_namespace, source_fingerprint, manifest_fingerprint,
 locale, stage, status, last_error, resumable, execution_owner, execution_epoch,
 lease_expires_at, total_chapters, total_content_size, manifest_chapter_count,
 manifest_content_size, manifest_word_count, completed_chapters, base_run_id
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		seed.id, purpose, seed.id, "import:"+purpose+":"+seed.id, strings.Repeat("a", 64), manifest,
		locale, seed.stage, seed.status, seed.lastError, seed.resumable, seed.owner, seed.epoch,
		seed.leaseExpiresAt, seed.totalChapters, seed.totalContentSize, seed.manifestChapters,
		seed.manifestSize, seed.manifestWords, seed.completedChapters, seed.baseRunID)
}

// irlSeedSource is one import_run_sources row, in source order, with the parse
// bookkeeping the run's own totals are summed from.
func irlSeedSource(t *testing.T, db *sql.DB, runID, sourceID string, index int64, status string, chapters, contentSize, words int64) {
	t.Helper()
	irlExec(t, db, `INSERT INTO import_run_sources (
 run_id, source_index, source_id, source_fingerprint, legacy_source_fingerprint, display_json,
 status, manifest_fingerprint, chapter_count, content_size, word_count, last_error
) VALUES (?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, '')`,
		runID, index, sourceID, strings.Repeat("b", 64),
		`{"displayName":"`+irlDisplayName+`","mediaType":"text/plain","size":128}`,
		status, strings.Repeat("c", 64), chapters, contentSize, words)
}

// irlSeedRunChapter freezes one already-adopted chapter of a run past parsing.
func irlSeedRunChapter(t *testing.T, db *sql.DB, runID, sourceID string, number int64, chapter ImportRunChapterInput) {
	t.Helper()
	irlExec(t, db, `INSERT INTO import_run_chapters (
 run_id, chapter_number, source_id, source_chapter_number,
 title, content_fingerprint, content_size, content_snapshot
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, number, sourceID, number, chapter.Title, chapter.ContentFingerprint,
		chapter.ContentSize, chapter.Content)
}

// irlSeedSourceChapter freezes one parsed chapter of a run still parsing.
func irlSeedSourceChapter(t *testing.T, db *sql.DB, runID, sourceID string, number int64, chapter ImportRunChapterInput) {
	t.Helper()
	irlExec(t, db, `INSERT INTO import_run_source_chapters (
 run_id, source_id, source_chapter_number, title, content_fingerprint, content_size,
 word_count, content_snapshot
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, sourceID, number, chapter.Title, chapter.ContentFingerprint, chapter.ContentSize,
		draftalg.CountDraftUnits(chapter.Content), chapter.Content)
}

// TestImportRunLifecycleFinalizeParsing drives the whole parsing lane:
// BeginParsing, CommitParsedSource, FinalizeParsing - and then proves the lane is
// not idempotent, because the original's is not.
func TestImportRunLifecycleFinalizeParsing(t *testing.T) {
	_, runs, path := irlFixture(t)
	ctx := context.Background()

	created, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, irlSourceID, irlDisplayName))
	if err != nil {
		t.Fatalf("begin parsing: %v", err)
	}
	ro := irlReadOnly(t, path)
	if got := irlText(t, created, "stage"); got != "parsing" {
		t.Fatalf("stage %q, want parsing", got)
	}
	if got := irlText(t, created, "status"); got != "ready" {
		t.Fatalf("status %q, want ready", got)
	}
	if got := irlText(t, created, "rootRunId"); got != irlRunID {
		t.Fatalf("rootRunId %q, want the run's own id", got)
	}
	if got := irlText(t, created, "effectNamespace"); got != "import:reference:"+irlRunID {
		t.Fatalf("effectNamespace %q", got)
	}
	if got := irlInt(t, created, "totalChapters"); got != 0 {
		t.Fatalf("totalChapters %d, want 0", got)
	}
	if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_runs`); got != 1 {
		t.Fatalf("%d runs, want 1", got)
	}
	source := irlQueryOne(t, ro, `SELECT source_index, source_fingerprint, status, display_json
 FROM import_run_sources WHERE run_id = ? AND source_id = ?`, irlRunID, irlSourceID)
	if got := irlInt(t, source, "source_index"); got != 0 {
		t.Fatalf("source_index %d, want 0", got)
	}
	if got := irlText(t, source, "status"); got != "pending" {
		t.Fatalf("source status %q, want pending", got)
	}
	if got := irlText(t, source, "source_fingerprint"); got != strings.Repeat("b", 64) {
		t.Fatalf("source_fingerprint %q", got)
	}
	if got := irlText(t, source, "display_json"); !strings.Contains(got, irlDisplayName) {
		t.Fatalf("display_json %q lost the display name", got)
	}

	if _, err := runs.CommitParsedSource(ctx, irlRunID, irlSourceID, irlChapters); err != nil {
		t.Fatalf("commit parsed source: %v", err)
	}
	chapters := irlQueryRows(t, ro, `SELECT source_chapter_number, title, content_size, word_count,
 content_snapshot FROM import_run_source_chapters WHERE run_id = ? ORDER BY source_chapter_number`, irlRunID)
	if len(chapters) != len(irlChapters) {
		t.Fatalf("%d frozen chapters, want %d", len(chapters), len(irlChapters))
	}
	wantWords := int64(0)
	wantBytes := int64(0)
	for index, chapter := range irlChapters {
		wantWords += int64(draftalg.CountDraftUnits(chapter.Content))
		wantBytes += chapter.ContentSize
		if got := irlText(t, chapters[index], "content_snapshot"); got != chapter.Content {
			t.Fatalf("chapter %d froze %q", index+1, got)
		}
		if got := irlInt(t, chapters[index], "word_count"); got != int64(draftalg.CountDraftUnits(chapter.Content)) {
			t.Fatalf("chapter %d word_count %d", index+1, got)
		}
	}
	parsed := irlQueryOne(t, ro, `SELECT status, chapter_count, content_size, word_count, manifest_fingerprint
 FROM import_run_sources WHERE run_id = ? AND source_id = ?`, irlRunID, irlSourceID)
	if got := irlText(t, parsed, "status"); got != "completed" {
		t.Fatalf("source status %q, want completed", got)
	}
	if got := irlInt(t, parsed, "chapter_count"); got != int64(len(irlChapters)) {
		t.Fatalf("chapter_count %d", got)
	}
	if got := irlInt(t, parsed, "content_size"); got != wantBytes {
		t.Fatalf("content_size %d, want %d", got, wantBytes)
	}
	if got := irlInt(t, parsed, "word_count"); got != wantWords {
		t.Fatalf("word_count %d, want %d", got, wantWords)
	}
	if got := irlText(t, parsed, "manifest_fingerprint"); got == strings.Repeat("0", 64) {
		t.Fatal("a completed source kept the zero manifest fingerprint")
	}
	running := irlQueryOne(t, ro, `SELECT status, total_chapters, total_content_size FROM import_runs WHERE id = ?`, irlRunID)
	if got := irlText(t, running, "status"); got != "ready" {
		t.Fatalf("run status %q, want ready again after the source completed", got)
	}
	if got := irlInt(t, running, "total_chapters"); got != int64(len(irlChapters)) {
		t.Fatalf("run total_chapters %d", got)
	}
	if got := irlInt(t, running, "total_content_size"); got != wantBytes {
		t.Fatalf("run total_content_size %d", got)
	}

	finalized, err := runs.FinalizeParsing(ctx, irlRunID)
	if err != nil {
		t.Fatalf("finalize parsing: %v", err)
	}
	if got := irlText(t, finalized, "classification"); got != "new" {
		t.Fatalf("classification %q, want new", got)
	}
	numbers, ok := finalized["newChapterNumbers"].([]int64)
	if !ok || len(numbers) != len(irlChapters) {
		t.Fatalf("newChapterNumbers %v", finalized["newChapterNumbers"])
	}
	prepared := irlNested(t, finalized, "run")
	if got := irlText(t, prepared, "stage"); got != "prepared" {
		t.Fatalf("finalized stage %q, want prepared", got)
	}
	if got := irlText(t, prepared, "status"); got != "ready" {
		t.Fatalf("finalized status %q", got)
	}
	if got := irlInt(t, prepared, "manifestChapterCount"); got != int64(len(irlChapters)) {
		t.Fatalf("manifestChapterCount %d", got)
	}
	if got := irlInt(t, prepared, "totalChapters"); got != int64(len(irlChapters)) {
		t.Fatalf("totalChapters %d", got)
	}
	if got := irlInt(t, prepared, "completedChapters"); got != 0 {
		t.Fatalf("completedChapters %d, want 0", got)
	}
	if _, ok := prepared["baseRunId"]; ok {
		t.Fatal("a first import has no base run")
	}
	mappings := irlQueryRows(t, ro, `SELECT chapter_number, source_id FROM import_source_chapter_map
 WHERE purpose = 'reference' ORDER BY chapter_number`)
	if len(mappings) != len(irlChapters) {
		t.Fatalf("%d mappings, want %d", len(mappings), len(irlChapters))
	}
	if got := irlText(t, mappings[0], "source_id"); got != irlSourceID {
		t.Fatalf("mapping source_id %q", got)
	}
	adopted := irlQueryRows(t, ro, `SELECT chapter_number, content_snapshot FROM import_run_chapters
 WHERE run_id = ? ORDER BY chapter_number`, irlRunID)
	if len(adopted) != len(irlChapters) {
		t.Fatalf("%d adopted chapters, want %d", len(adopted), len(irlChapters))
	}
	if got := irlText(t, adopted[0], "content_snapshot"); got != irlChapters[0].Content {
		t.Fatalf("adopted chapter 1 froze %q", got)
	}

	irlRefusal(t, path, "IMPORT_RUN_PARSING_CLOSED", func() error {
		_, err := runs.FinalizeParsing(ctx, irlRunID)
		return err
	})
}

// TestImportRunLifecycleBeginParsingReentry proves both re-entries of
// beginParsing reuse the run that already froze these sources instead of minting
// a second one.
func TestImportRunLifecycleBeginParsingReentry(t *testing.T) {
	_, runs, path := irlFixture(t)
	ctx := context.Background()

	if _, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, irlSourceID, irlDisplayName)); err != nil {
		t.Fatalf("begin parsing: %v", err)
	}
	ro := irlReadOnly(t, path)
	t.Run("same id refreshes the display facts", func(t *testing.T) {
		renewed, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, irlSourceID, "重选-山雨.txt"))
		if err != nil {
			t.Fatalf("re-authorize: %v", err)
		}
		if got := irlText(t, renewed, "id"); got != irlRunID {
			t.Fatalf("id %q, want the same run", got)
		}
		if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_runs`); got != 1 {
			t.Fatalf("%d runs, want 1", got)
		}
		display := irlText(t, irlQueryOne(t, ro, `SELECT display_json FROM import_run_sources
 WHERE run_id = ? AND source_id = ?`, irlRunID, irlSourceID), "display_json")
		if !strings.Contains(display, "重选-山雨.txt") {
			t.Fatalf("source display_json %q was not refreshed", display)
		}
		if !strings.Contains(irlRunDisplay(t, ro), "重选-山雨.txt") {
			t.Fatal("the run's own display list was not refreshed")
		}
	})
	t.Run("another id re-indexes the same sources", func(t *testing.T) {
		reused, err := runs.BeginParsing(ctx, irlBeginRequest(irlNextRunID, irlSourceID, irlDisplayName))
		if err != nil {
			t.Fatalf("re-index: %v", err)
		}
		if got := irlText(t, reused, "id"); got != irlRunID {
			t.Fatalf("id %q, want the existing parsing run %q", got, irlRunID)
		}
		if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_runs`); got != 1 {
			t.Fatalf("%d runs, want 1", got)
		}
		if got := irlInt(t, irlQueryOne(t, ro, `SELECT source_index FROM import_run_sources
 WHERE run_id = ? AND source_id = ?`, irlRunID, irlSourceID), "source_index"); got != 0 {
			t.Fatalf("source_index %d after re-index, want 0", got)
		}
	})
}

// irlRunDisplay is the run's own source_display_json, which the re-entry has to
// rewrite along with the per-source rows.
func irlRunDisplay(t *testing.T, db *sql.DB) string {
	t.Helper()
	return irlText(t, irlQueryOne(t, db, `SELECT source_display_json FROM import_runs WHERE id = ?`, irlRunID),
		"source_display_json")
}

// TestImportRunLifecycleRefusals exercises every reachable refusal on its own
// fresh database and proves each one leaves the file's bytes untouched.
func TestImportRunLifecycleRefusals(t *testing.T) {
	ctx := context.Background()
	prepared := func(name string) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: name, status: "ready", resumable: 1,
				totalChapters: 0, manifestChapters: 0})
		}
	}
	parsing := func(mutate func(*irlRunSeed)) func(*testing.T, *sql.DB) {
		return func(t *testing.T, db *sql.DB) {
			seed := irlRunSeed{id: irlRunID, stage: "parsing", status: "ready", resumable: 1,
				manifestChapters: 0}
			if mutate != nil {
				mutate(&seed)
			}
			irlSeedRun(t, db, seed)
		}
	}
	cases := []struct {
		name  string
		code  string
		seed  func(*testing.T, *sql.DB)
		legal func(*testing.T, *ImportRuns) error
	}{
		{name: "run id invalid", code: "IMPORT_RUN_ID_INVALID",
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.BeginParsing(ctx, irlBeginRequest("   ", irlSourceID, irlDisplayName))
				return err
			}},
		{name: "purpose invalid", code: "IMPORT_RUN_PURPOSE_INVALID",
			legal: func(t *testing.T, runs *ImportRuns) error {
				request := irlBeginRequest(irlRunID, irlSourceID, irlDisplayName)
				request.Purpose = "author-manuscript"
				_, err := runs.BeginParsing(ctx, request)
				return err
			}},
		{name: "locale invalid", code: "IMPORT_RUN_LOCALE_INVALID",
			legal: func(t *testing.T, runs *ImportRuns) error {
				request := irlBeginRequest(irlRunID, irlSourceID, irlDisplayName)
				request.Locale = "fr-FR"
				_, err := runs.BeginParsing(ctx, request)
				return err
			}},
		{name: "source display invalid", code: "IMPORT_RUN_SOURCE_DISPLAY_INVALID",
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, irlSourceID, "正文/第一章.txt"))
				return err
			}},
		{name: "source ids invalid", code: "IMPORT_RUN_SOURCE_IDS_INVALID",
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, "第一章.txt", irlDisplayName))
				return err
			}},
		{name: "source fingerprint invalid", code: "IMPORT_RUN_SOURCE_FINGERPRINT_INVALID",
			legal: func(t *testing.T, runs *ImportRuns) error {
				request := irlBeginRequest(irlRunID, irlSourceID, irlDisplayName)
				request.SourceFingerprints = []string{"not-a-fingerprint"}
				_, err := runs.BeginParsing(ctx, request)
				return err
			}},
		{name: "reauthorize unavailable", code: "IMPORT_RUN_REAUTHORIZE_UNAVAILABLE",
			seed: prepared("prepared"),
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, irlSourceID, irlDisplayName))
				return err
			}},
		{name: "reauthorize sources mismatch", code: "IMPORT_RUN_REAUTHORIZE_SOURCES_MISMATCH",
			seed: func(t *testing.T, db *sql.DB) {
				parsing(nil)(t, db)
				irlSeedSource(t, db, irlRunID, irlOtherSourceID, 0, "pending", 0, 0, 0)
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.BeginParsing(ctx, irlBeginRequest(irlRunID, irlSourceID, irlDisplayName))
				return err
			}},
		{name: "parsed source invalid", code: "IMPORT_RUN_PARSED_SOURCE_INVALID",
			seed: parsing(nil),
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.CommitParsedSource(ctx, irlRunID, irlOtherSourceID, irlChapters)
				return err
			}},
		{name: "parsed source empty", code: "IMPORT_RUN_PARSED_SOURCE_EMPTY",
			seed: parsing(nil),
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.CommitParsedSource(ctx, irlRunID, irlSourceID, nil)
				return err
			}},
		{name: "parsed source mismatch", code: "IMPORT_RUN_PARSED_SOURCE_MISMATCH",
			seed: parsing(nil),
			legal: func(t *testing.T, runs *ImportRuns) error {
				chapters := []ImportRunChapterInput{irlChapter(1, "第一章 山雨", "改过的正文")}
				chapters[0].ContentFingerprint = strings.Repeat("e", 64)
				_, err := runs.CommitParsedSource(ctx, irlRunID, irlSourceID, chapters)
				return err
			}},
		{name: "chapters invalid", code: "IMPORT_RUN_CHAPTERS_INVALID",
			seed: parsing(nil),
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.CommitParsedSource(ctx, irlRunID, irlSourceID,
					[]ImportRunChapterInput{irlChapter(0, "第零章", "无")})
				return err
			}},
		{name: "failure reason invalid", code: "IMPORT_RUN_FAILURE_REASON_INVALID",
			seed: parsing(nil),
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.FailParsedSource(ctx, irlRunID, irlSourceID, "   ")
				return err
			}},
		{name: "parsing closed", code: "IMPORT_RUN_PARSING_CLOSED",
			seed: prepared("prepared"),
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.CommitParsedSource(ctx, irlRunID, irlSourceID, irlChapters)
				return err
			}},
		{name: "parsing incomplete", code: "IMPORT_RUN_PARSING_INCOMPLETE",
			seed: func(t *testing.T, db *sql.DB) {
				parsing(nil)(t, db)
				irlSeedSource(t, db, irlRunID, irlSourceID, 0, "pending", 0, 0, 0)
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.FinalizeParsing(ctx, irlRunID)
				return err
			}},
		{name: "manifest invalid", code: "IMPORT_RUN_MANIFEST_INVALID",
			seed: func(t *testing.T, db *sql.DB) {
				parsing(nil)(t, db)
				irlSeedSource(t, db, irlRunID, irlSourceID, 0, "completed",
					int64(len(irlChapters))+1, irlChapters[0].ContentSize, 8)
				irlSeedSourceChapter(t, db, irlRunID, irlSourceID, 1, irlChapters[0])
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.FinalizeParsing(ctx, irlRunID)
				return err
			}},
		{name: "not restartable", code: "IMPORT_RUN_NOT_RESTARTABLE",
			seed: func(t *testing.T, db *sql.DB) {
				irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "knowledge", status: "ready", resumable: 1,
					manifestChapters: 0})
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.Restart(ctx, irlRunID, irlNextRunID)
				return err
			}},
		{name: "author restart forbidden", code: "IMPORT_RUN_AUTHOR_RESTART_FORBIDDEN",
			seed: func(t *testing.T, db *sql.DB) {
				irlSeedRun(t, db, irlRunSeed{id: irlRunID, purpose: "author-manuscript", stage: "author-commit",
					status: "failed", resumable: 1, manifestChapters: 0})
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.Restart(ctx, irlRunID, irlNextRunID)
				return err
			}},
		{name: "id conflict", code: "IMPORT_RUN_ID_CONFLICT",
			seed: func(t *testing.T, db *sql.DB) {
				irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "knowledge", status: "failed", resumable: 1,
					manifestChapters: 0})
				irlSeedRun(t, db, irlRunSeed{id: irlNextRunID, stage: "knowledge", status: "ready",
					manifestChapters: 0})
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.Restart(ctx, irlRunID, irlNextRunID)
				return err
			}},
		{name: "execution stale", code: "IMPORT_RUN_EXECUTION_STALE",
			seed: func(t *testing.T, db *sql.DB) {
				irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "knowledge", status: "running",
					resumable: 1, owner: irlOwner, epoch: 7,
					leaseExpiresAt: time.Now().UnixMilli() + 15*60*1000, manifestChapters: 0})
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.Fail(ctx, irlRunID, "knowledge", "IMPORT_RUN_OPERATION_FAILED",
					ImportRunExecution{Owner: irlOwner, Epoch: 8})
				return err
			}},
		{name: "stage mismatch", code: "IMPORT_RUN_STAGE_MISMATCH",
			seed: func(t *testing.T, db *sql.DB) {
				irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "knowledge", status: "running",
					resumable: 1, owner: irlOwner, epoch: 7,
					leaseExpiresAt: time.Now().UnixMilli() + 15*60*1000, manifestChapters: 0})
			},
			legal: func(t *testing.T, runs *ImportRuns) error {
				_, err := runs.Fail(ctx, irlRunID, "style", "IMPORT_RUN_OPERATION_FAILED",
					ImportRunExecution{Owner: irlOwner, Epoch: 7})
				return err
			}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			db, runs, path := irlFixture(t)
			if testCase.seed != nil {
				testCase.seed(t, db)
			}
			irlRefusal(t, path, testCase.code, func() error { return testCase.legal(t, runs) })
		})
	}
}

// TestImportRunLifecycleRestart proves the row, stage and attempt bookkeeping the
// original keeps when a run is restarted: the old run is fenced but not deleted,
// and exactly one new run inherits its frozen facts.
func TestImportRunLifecycleRestart(t *testing.T) {
	ctx := context.Background()
	t.Run("a run past parsing copies what it committed", func(t *testing.T) {
		db, runs, path := irlFixture(t)
		lease := time.Now().UnixMilli() + 15*60*1000
		irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "knowledge", status: "failed", resumable: 1,
			lastError: "IMPORT_RUN_OPERATION_FAILED", owner: irlOwner, epoch: 7,
			leaseExpiresAt: lease, manifestFingerprint: strings.Repeat("c", 64),
			totalChapters: 2, totalContentSize: 96, manifestChapters: 2, manifestSize: 96,
			manifestWords: 40, completedChapters: 1})
		irlSeedSource(t, db, irlRunID, irlSourceID, 0, "completed", 2, 96, 40)
		for index, chapter := range irlChapters {
			irlSeedRunChapter(t, db, irlRunID, irlSourceID, int64(index)+1, chapter)
		}
		before := irlDigest(t, path)

		snapshot, err := runs.Restart(ctx, irlRunID, "  "+irlNextRunID+"  ")
		if err != nil {
			t.Fatalf("restart: %v", err)
		}
		if got := irlText(t, snapshot, "id"); got != irlNextRunID {
			t.Fatalf("restarted id %q, want the trimmed next id", got)
		}
		ro := irlReadOnly(t, path)
		if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_runs`); got != 2 {
			t.Fatalf("%d runs, want exactly one new run", got)
		}
		old := irlQueryOne(t, ro, `SELECT resumable, cancel_requested, last_error, execution_owner,
 execution_epoch, lease_expires_at, stage, status, completed_chapters, total_chapters
 FROM import_runs WHERE id = ?`, irlRunID)
		if got := irlInt(t, old, "resumable"); got != 0 {
			t.Fatalf("old resumable %d, want 0", got)
		}
		if got := irlInt(t, old, "cancel_requested"); got != 0 {
			t.Fatalf("old cancel_requested %d, want 0", got)
		}
		if got := irlText(t, old, "last_error"); got != "Restarted by user" {
			t.Fatalf("old last_error %q", got)
		}
		if got := irlText(t, old, "execution_owner"); got != "" {
			t.Fatalf("old execution_owner %q, want none", got)
		}
		if got := irlInt(t, old, "execution_epoch"); got != 8 {
			t.Fatalf("old execution_epoch %d, want 8 (fenced once)", got)
		}
		if got := irlInt(t, old, "lease_expires_at"); got != 0 {
			t.Fatalf("old lease %d, want 0", got)
		}
		if got := irlText(t, old, "stage"); got != "knowledge" {
			t.Fatalf("old stage %q, want it unchanged", got)
		}
		if got := irlText(t, old, "status"); got != "failed" {
			t.Fatalf("old status %q, want it unchanged", got)
		}
		if got := irlInt(t, old, "completed_chapters"); got != 1 {
			t.Fatalf("old completed_chapters %d, want it unchanged", got)
		}
		if got := irlInt(t, old, "total_chapters"); got != 2 {
			t.Fatalf("old total_chapters %d, want it unchanged", got)
		}
		fresh := irlQueryOne(t, ro, `SELECT purpose, root_run_id, effect_namespace, stage, status,
 manifest_fingerprint, total_chapters, total_content_size, completed_chapters, base_run_id,
 manifest_chapter_count, manifest_content_size, manifest_word_count, resumable, execution_epoch
 FROM import_runs WHERE id = ?`, irlNextRunID)
		if got := irlText(t, fresh, "purpose"); got != "reference" {
			t.Fatalf("new purpose %q", got)
		}
		if got := irlText(t, fresh, "root_run_id"); got != irlRunID {
			t.Fatalf("new root_run_id %q, want the restarted run's root", got)
		}
		if got := irlText(t, fresh, "effect_namespace"); got != "import:reference:"+irlNextRunID {
			t.Fatalf("new effect_namespace %q", got)
		}
		if got := irlText(t, fresh, "stage"); got != "knowledge" {
			t.Fatalf("new stage %q, want the stage it failed at", got)
		}
		if got := irlText(t, fresh, "status"); got != "ready" {
			t.Fatalf("new status %q, want ready", got)
		}
		if got := irlText(t, fresh, "manifest_fingerprint"); got != strings.Repeat("c", 64) {
			t.Fatalf("new manifest_fingerprint %q", got)
		}
		if got := irlInt(t, fresh, "total_chapters"); got != 2 {
			t.Fatalf("new total_chapters %d", got)
		}
		if got := irlInt(t, fresh, "total_content_size"); got != 96 {
			t.Fatalf("new total_content_size %d", got)
		}
		if got := irlInt(t, fresh, "manifest_chapter_count"); got != 2 {
			t.Fatalf("new manifest_chapter_count %d", got)
		}
		if got := irlInt(t, fresh, "manifest_word_count"); got != 40 {
			t.Fatalf("new manifest_word_count %d", got)
		}
		if got := irlInt(t, fresh, "completed_chapters"); got != 0 {
			t.Fatalf("new completed_chapters %d, want 0", got)
		}
		if got := irlInt(t, fresh, "resumable"); got != 1 {
			t.Fatalf("new resumable %d, want the schema default", got)
		}
		if got := irlInt(t, fresh, "execution_epoch"); got != 0 {
			t.Fatalf("new execution_epoch %d, want a fresh attempt", got)
		}
		if fresh["base_run_id"] != nil {
			t.Fatalf("new base_run_id is %v, want null", fresh["base_run_id"])
		}
		copied := irlQueryRows(t, ro, `SELECT source_id, status, chapter_count, content_size, word_count
 FROM import_run_sources WHERE run_id = ?`, irlNextRunID)
		if len(copied) != 1 {
			t.Fatalf("%d copied sources, want 1", len(copied))
		}
		if got := irlText(t, copied[0], "status"); got != "completed" {
			t.Fatalf("copied source status %q, want the committed status", got)
		}
		if got := irlInt(t, copied[0], "chapter_count"); got != 2 {
			t.Fatalf("copied source chapter_count %d", got)
		}
		if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_run_chapters WHERE run_id = ?`, irlNextRunID); got != 2 {
			t.Fatalf("%d copied chapters, want 2", got)
		}
		if after := irlDigest(t, path); after == before {
			t.Fatal("a successful restart must write")
		}
	})
	t.Run("a parsing run restarts with nothing committed", func(t *testing.T) {
		db, runs, path := irlFixture(t)
		irlSeedRun(t, db, irlRunSeed{id: irlBaseRunID, stage: "completed", status: "completed",
			resumable: 0, manifestFingerprint: strings.Repeat("c", 64), totalChapters: 2,
			totalContentSize: 96, manifestChapters: 2, manifestSize: 96, manifestWords: 40,
			completedChapters: 2})
		irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "parsing", status: "failed", resumable: 1,
			lastError: "IMPORT_RUN_OPERATION_FAILED", manifestFingerprint: strings.Repeat("d", 64),
			totalChapters: 2, totalContentSize: 96, manifestChapters: 2, manifestSize: 96,
			manifestWords: 40, baseRunID: sql.NullString{String: irlBaseRunID, Valid: true}})
		irlSeedSource(t, db, irlRunID, irlSourceID, 0, "completed", 2, 96, 40)
		irlSeedSourceChapter(t, db, irlRunID, irlSourceID, 1, irlChapters[0])
		irlSeedRunChapter(t, db, irlRunID, irlSourceID, 1, irlChapters[0])

		before := irlDigest(t, path)
		snapshot, err := runs.Restart(ctx, irlRunID, irlNextRunID)
		if err != nil {
			t.Fatalf("restart parsing run: %v", err)
		}
		if got := irlText(t, snapshot, "stage"); got != "parsing" {
			t.Fatalf("restarted stage %q, want parsing", got)
		}
		ro := irlReadOnly(t, path)
		if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_runs`); got != 3 {
			t.Fatalf("%d runs, want the base, the old and exactly one new", got)
		}
		fresh := irlQueryOne(t, ro, `SELECT stage, status, manifest_fingerprint, total_chapters,
 total_content_size, manifest_chapter_count, manifest_content_size, manifest_word_count,
 completed_chapters, base_run_id FROM import_runs WHERE id = ?`, irlNextRunID)
		if got := irlText(t, fresh, "manifest_fingerprint"); got != strings.Repeat("0", 64) {
			t.Fatalf("new manifest_fingerprint %q, want the zero fingerprint", got)
		}
		if got := irlInt(t, fresh, "total_chapters"); got != 0 {
			t.Fatalf("new total_chapters %d, want 0", got)
		}
		if got := irlInt(t, fresh, "total_content_size"); got != 0 {
			t.Fatalf("new total_content_size %d, want 0", got)
		}
		if got := irlInt(t, fresh, "manifest_chapter_count"); got != 0 {
			t.Fatalf("new manifest_chapter_count %d, want 0", got)
		}
		if got := irlInt(t, fresh, "manifest_content_size"); got != 0 {
			t.Fatalf("new manifest_content_size %d, want 0", got)
		}
		if got := irlInt(t, fresh, "manifest_word_count"); got != 0 {
			t.Fatalf("new manifest_word_count %d, want 0", got)
		}
		if got := irlInt(t, fresh, "completed_chapters"); got != 0 {
			t.Fatalf("new completed_chapters %d, want 0", got)
		}
		if fresh["base_run_id"] != nil {
			t.Fatalf("new base_run_id is %v, want null", fresh["base_run_id"])
		}
		sources := irlQueryRows(t, ro, `SELECT source_id, status, manifest_fingerprint, chapter_count,
 content_size, word_count, last_error FROM import_run_sources WHERE run_id = ?`, irlNextRunID)
		if len(sources) != 1 {
			t.Fatalf("%d copied sources, want 1", len(sources))
		}
		if got := irlText(t, sources[0], "status"); got != "pending" {
			t.Fatalf("copied source status %q, want pending", got)
		}
		if got := irlText(t, sources[0], "manifest_fingerprint"); got != "" {
			t.Fatalf("copied source manifest_fingerprint %q, want cleared", got)
		}
		if got := irlInt(t, sources[0], "chapter_count"); got != 0 {
			t.Fatalf("copied source chapter_count %d, want 0", got)
		}
		if got := irlInt(t, sources[0], "content_size"); got != 0 {
			t.Fatalf("copied source content_size %d, want 0", got)
		}
		if got := irlInt(t, sources[0], "word_count"); got != 0 {
			t.Fatalf("copied source word_count %d, want 0", got)
		}
		if got := irlText(t, sources[0], "last_error"); got != "" {
			t.Fatalf("copied source last_error %q, want cleared", got)
		}
		if got := irlCount(t, ro, `SELECT COUNT(*) FROM import_run_chapters WHERE run_id = ?`, irlNextRunID); got != 0 {
			t.Fatalf("%d chapters copied, want none: nothing was ever adopted", got)
		}
		if after := irlDigest(t, path); after == before {
			t.Fatal("a successful restart must write")
		}
	})
}

// TestImportRunLifecycleFail proves db:import-run-fail stores the caller's own
// sentence, cuts it the way the original does, and retires the lease.
func TestImportRunLifecycleFail(t *testing.T) {
	ctx := context.Background()
	seedRunning := func(t *testing.T, db *sql.DB) {
		irlSeedRun(t, db, irlRunSeed{id: irlRunID, stage: "knowledge", status: "running",
			owner: irlOwner, epoch: 7, leaseExpiresAt: time.Now().UnixMilli() + 15*60*1000,
			manifestChapters: 0})
	}
	t.Run("records the reason and retires the lease", func(t *testing.T) {
		db, runs, path := irlFixture(t)
		seedRunning(t, db)
		reason := strings.Repeat("章", 2500)
		snapshot, err := runs.Fail(ctx, irlRunID, "knowledge", reason,
			ImportRunExecution{Owner: irlOwner, Epoch: 7})
		if err != nil {
			t.Fatalf("fail: %v", err)
		}
		if got := irlText(t, snapshot, "status"); got != "failed" {
			t.Fatalf("status %q, want failed", got)
		}
		ro := irlReadOnly(t, path)
		if got := irlText(t, snapshot, "stage"); got != "knowledge" {
			t.Fatalf("stage %q, want the failing stage", got)
		}
		row := irlQueryOne(t, ro, `SELECT status, resumable, last_error, execution_owner,
 execution_epoch, lease_expires_at FROM import_runs WHERE id = ?`, irlRunID)
		if got := irlText(t, row, "status"); got != "failed" {
			t.Fatalf("status %q", got)
		}
		if got := irlInt(t, row, "resumable"); got != 1 {
			t.Fatalf("resumable %d, want 1", got)
		}
		if got := irlText(t, row, "execution_owner"); got != "" {
			t.Fatalf("execution_owner %q, want none", got)
		}
		if got := irlInt(t, row, "execution_epoch"); got != 8 {
			t.Fatalf("execution_epoch %d, want the retired attempt 8", got)
		}
		if got := irlInt(t, row, "lease_expires_at"); got != 0 {
			t.Fatalf("lease %d, want 0", got)
		}
		stored := irlText(t, row, "last_error")
		if got := irlUTF16Len(stored); got != 2000 {
			t.Fatalf("stored reason is %d UTF-16 units, want 2000", got)
		}
		if !strings.HasPrefix(reason, stored) {
			t.Fatal("the stored reason is not a prefix of the reported one")
		}
	})
	t.Run("cuts between code points, never inside one", func(t *testing.T) {
		db, runs, path := irlFixture(t)
		seedRunning(t, db)
		reason := strings.Repeat("😀", 1500)
		if _, err := runs.Fail(ctx, irlRunID, "knowledge", reason,
			ImportRunExecution{Owner: irlOwner, Epoch: 7}); err != nil {
			t.Fatalf("fail: %v", err)
		}
		ro := irlReadOnly(t, path)
		stored := irlText(t, irlQueryOne(t, ro, `SELECT last_error FROM import_runs WHERE id = ?`, irlRunID), "last_error")
		if got := irlUTF16Len(stored); got != 2000 {
			t.Fatalf("stored reason is %d UTF-16 units, want 2000", got)
		}
		if got := len([]rune(stored)); got != 1000 {
			t.Fatalf("stored reason is %d code points, want 1000 intact ones", got)
		}
		if !strings.HasPrefix(reason, stored) {
			t.Fatal("the stored reason is not a prefix of the reported one")
		}
	})
	t.Run("refuses a non-string reason after the stage check", func(t *testing.T) {
		db, runs, path := irlFixture(t)
		seedRunning(t, db)
		irlRefusal(t, path, "IMPORT_RUN_FAILURE_REASON_INVALID", func() error {
			_, err := runs.Fail(ctx, irlRunID, "knowledge", nil, ImportRunExecution{Owner: irlOwner, Epoch: 7})
			return err
		})
	})
}

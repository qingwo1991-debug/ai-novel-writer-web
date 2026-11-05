//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The fixture is one disposable t.TempDir() (syntheticProductionDataDir): a
// committed global generation plus one admitted author project whose real baseline
// rows carry two reviews, seven revisions and two pending recovery candidates.
// Every write below is asserted by READING THE AUTHOR DATABASE BACK over a second,
// independent connection, so an acknowledgement cannot stand in for a committed row.
const (
	productionFixtureProjectName = "review-write-novel"
	productionFixtureProjectID   = "4a8f2c16-7d3e-4b09-8c25-6e1d3a9f7b42"
	productionFixtureChapter     = "第一章 门"

	productionFixtureDraftBody  = "第一章原稿：他推开门。"
	productionFixtureSecondBody = "第二章原稿：窗外有风。"
	productionFixtureThirdBody  = "第三章原稿：灯下无人。"
	productionFixtureFourthBody = "第四章原稿：雨停了。"

	productionFixtureCreatedReview   = "新审读：门缝里有光。"
	productionFixtureCreatedRevision = "新改稿：她推开门。"
	productionFixtureThirdReplace    = "第三章替换稿：灯忽然亮了。"
	productionFixtureMergedBody      = "第二章合并稿：门开着。"
	productionFixtureStaleMergedBody = "不应落库的正文。"

	productionFixtureCreatedCandidate = "新候选正文：门后有伞。"
	productionFixtureUpdatedCandidate = "改写后的候选正文：门后有钟。"
	productionFixtureSecondCandidate  = "待判候选正文：门后有影。"

	productionFixturePendingMergeID   = int64(5)
	productionFixturePendingDiscardID = int64(6)
	productionFixtureConflictID       = int64(7)
	productionFixtureMergeRevisionID  = int64(2)
	productionFixtureCandidateID      = "ci-write-candidate-1"
	productionFixtureResolveID        = "ci-write-candidate-2"
)

// productionFixtureSource is the exact source snapshot the store compares a
// candidate against (internal/store/recovery.go through internal/store/plottree.go):
// the normalized chapter-1 blueprint in this quoted key order. It is the seeded
// blueprint row, so the STORE, not the test, decides that both candidates are
// source-current.
const productionFixtureSource = `{"chapterNumber":1,"title":"` + productionFixtureChapter +
	`","role":"","purpose":"","keyEvents":"","characters":[],"suspenseHook":"","userGuidance":""}`

// productionFixtureSchema is the smallest real schema these nine writes touch. The
// reviews table carries the fixture's own write guard: CHECK (base_draft_id != 4)
// lets a review of draft 4 pass every transport and store pre-check, insert its
// body, and then fail INSIDE the store's BEGIN IMMEDIATE transaction, so the
// rollback - not an early refusal - is what the byte-identity assertion measures.
//
// The guard is spelled "!=" and never "<>": the admission verifier encodes
// sqlite_master with JSON.stringify escaping (internal/admission/snapshot.go
// appendJSString), which leaves "<" and ">" literal, while encoding/json turns
// them into \u003c and \u003e. A "<>" in this text would therefore fingerprint the
// same database two different ways and the fixture's own project would be
// rejected as SCHEMA_UNRECOGNIZED before a single write ran. SQLite treats "!="
// and "<>" identically, and this file's DDL stays free of every character the
// two encoders disagree about.
//
// Two columns deliberately carry the shipped baseline's default instead of a test
// value: revisions.status and recovery_candidates.status are DEFAULT 'pending'.
// The store does not name them in its INSERTs - internal/store/reviewrevision.go
// lists revision_type but never status, and internal/store/recovery.go inserts a
// candidate and then re-reads it expecting status "pending" - so the baseline, not
// the writer, is what makes those rows pending. A fixture without the default
// writes NULL and the recovery write fails its own integrity re-read.
var productionFixtureSchema = []string{
	"CREATE TABLE blueprints (chapter_number INTEGER PRIMARY KEY, title TEXT, role TEXT, purpose TEXT, key_events TEXT, characters TEXT, suspense_hook TEXT, user_guidance TEXT, notes TEXT, notes_updated_at TEXT)",
	"CREATE TABLE contents (id INTEGER PRIMARY KEY, body TEXT)",
	"CREATE TABLE drafts (id INTEGER PRIMARY KEY, chapter_number INTEGER, version INTEGER, status TEXT, source TEXT, content_id INTEGER, word_count INTEGER, source_dependencies TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience INTEGER, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE recovery_candidates (candidate_id TEXT PRIMARY KEY, run_id TEXT, step_id TEXT, project_id TEXT, chapter_number INTEGER, chapter_title TEXT, source_snapshot TEXT, source_hash TEXT, source_draft_id INTEGER, source_draft_version INTEGER, source_draft_identity_captured INTEGER, visible_text TEXT, content_hash TEXT, failure_code TEXT, failure_reason TEXT, replaces_candidate_id TEXT, status TEXT NOT NULL DEFAULT 'pending', resolved_at TEXT, created_at TEXT)",
	"CREATE TABLE reviews (id INTEGER PRIMARY KEY, base_draft_id INTEGER, review_index INTEGER, content_id INTEGER, created_at TEXT, source_draft_chapter_number INTEGER, source_draft_version INTEGER, source_draft_status TEXT, source_content TEXT, CHECK (base_draft_id != 4))",
	"CREATE TABLE revisions (id INTEGER PRIMARY KEY, base_draft_id INTEGER, revision_index INTEGER, revision_type TEXT, status TEXT DEFAULT 'pending', merged_to_draft_id INTEGER, user_prompt TEXT, review_source_id INTEGER, content_id INTEGER, word_count INTEGER, created_at TEXT, updated_at TEXT, source_draft_chapter_number INTEGER, source_draft_version INTEGER, source_draft_status TEXT, source_content TEXT)",
}

// productionFixtureContract is the trusted contract for productionFixtureSchema.
// The verifier hashes sqlite_master in (type, name) order, so the entries are
// built from the same DDL text and sorted the same way.
func productionFixtureContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionFixtureSchema))
	for _, statement := range productionFixtureSchema {
		// json.Marshal escapes "<", ">" and "&" as \u003c, \u003e and \u0026;
		// the verifier's sqlite_master encoder (internal/admission/snapshot.go
		// appendJSString) leaves them literal. One of these in the DDL would
		// fingerprint the same file two ways and every project would be refused
		// as SCHEMA_UNRECOGNIZED, so the fixture says so here instead.
		if strings.ContainsAny(statement, "<>&") {
			t.Fatalf("schema statement holds a character the verifier encodes differently from encoding/json: %s", statement)
		}
		if !strings.HasPrefix(statement, prefix) {
			t.Fatalf("unparseable schema statement: %s", statement)
		}
		name, _, found := strings.Cut(strings.TrimPrefix(statement, prefix), " ")
		if !found || name == "" {
			t.Fatalf("unparseable schema statement: %s", statement)
		}
		entries = append(entries, productionSchemaEntry{Kind: "table", Name: name, Table: name, SQL: statement})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: []string{hex.EncodeToString(digest[:])}}}
}

// productionFixtureDigest is the candidate integrity hash the store verifies
// (internal/store/recovery.go): sha256 hex of the column text.
func productionFixtureDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// productionFixtureDataRoot seeds the disposable data root and the author rows.
func productionFixtureDataRoot(t *testing.T) (root, projectPath string) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionFixtureProjectName, productionFixtureProjectID,
		admission.CurrentSchemaVersion, productionFixtureSchema)
	projectPath = filepath.Join(library, productionFixtureProjectName)
	productionFixtureSeedRows(t, projectPath)
	return root, projectPath
}

// productionFixtureReadback opens its OWN read-only connection to the author
// database. The lane's admitted handle is never consulted, so a write that never
// reached the file cannot pass: this reads the bytes SQLite committed.
func productionFixtureReadback(t *testing.T, projectPath, query string, args ...any) []map[string]any {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	rows, err := db.Query(query, args...)
	if err != nil {
		t.Fatalf("readback %q: %v", query, err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	result := make([]map[string]any, 0, 1)
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for i := range values {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("readback %q: %v", query, err)
		}
		row := make(map[string]any, len(columns))
		for i, column := range columns {
			row[column] = values[i]
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("readback %q: %v", query, err)
	}
	return result
}

// productionFixtureSingle is productionFixtureReadback pinned to exactly one row.
func productionFixtureSingle(t *testing.T, projectPath, query string, args ...any) map[string]any {
	t.Helper()
	rows := productionFixtureReadback(t, projectPath, query, args...)
	if len(rows) != 1 {
		t.Fatalf("readback %q returned %d rows; want exactly 1", query, len(rows))
	}
	return rows[0]
}

func productionFixtureText(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	value, ok := row[key].(string)
	if !ok {
		t.Fatalf("%s is %T (%v), not text", key, row[key], row[key])
	}
	return value
}

func productionFixtureNumber(t *testing.T, row map[string]any, key string) int64 {
	t.Helper()
	switch value := row[key].(type) {
	case int64:
		return value
	case float64:
		if math.Trunc(value) != value {
			t.Fatalf("%s is not a whole number: %v", key, value)
		}
		return int64(value)
	}
	t.Fatalf("%s is %T (%v), not a number", key, row[key], row[key])
	return 0
}

// productionFixtureRefused asserts the lane's mutation refusal: HTTP 200 with
// success:false (the envelope every mutation in this lane returns,
// internal/transport/reviewrevision.go and internal/transport/recovery.go), one of
// the expected symbolic codes, and no SQLite diagnostic in the answer.
func productionFixtureRefused(t *testing.T, client *productionDurableClient, operation string, args []any, want ...string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s (a mutation answers with a 200 business result)", operation, response.Code, response.Body.String())
	}
	lower := strings.ToLower(response.Body.String())
	for _, leak := range []string{"constraint", "sqlite", "pragma", "select ", "/tmp/"} {
		if strings.Contains(lower, leak) {
			t.Fatalf("%s leaked a diagnostic (%q): %s", operation, leak, response.Body.String())
		}
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if success, _ := wire.Result["success"].(bool); success {
		t.Fatalf("%s committed under a refusal it must not accept: %s", operation, response.Body.String())
	}
	errorText, _ := wire.Result["error"].(string)
	errorCode, _ := wire.Result["errorCode"].(string)
	for _, code := range want {
		if strings.Contains(errorText, code) || errorCode == code {
			return
		}
	}
	t.Fatalf("%s refused with %q; want one of %v", operation, errorText, want)
}

// productionFixtureSeedRows writes the rows these nine writes act on. Every base
// draft that a write must leave alone gets its own draft so one subtest can never
// silently depend on another's committed state.
func productionFixtureSeedRows(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO blueprints (chapter_number, title, characters, notes) VALUES (1, ?, '[]', '')`,
			[]any{productionFixtureChapter}},
		{`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, created_at, updated_at)
 VALUES ('main', 'Review Write Novel', 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO contents (id, body) VALUES (1, ?)`, []any{productionFixtureDraftBody}},
		{`INSERT INTO contents (id, body) VALUES (2, ?)`, []any{"审读正文：门后有人。"}},
		{`INSERT INTO contents (id, body) VALUES (3, ?)`, []any{productionFixtureSecondBody}},
		{`INSERT INTO contents (id, body) VALUES (4, ?)`, []any{productionFixtureThirdBody}},
		{`INSERT INTO contents (id, body) VALUES (5, ?)`, []any{productionFixtureFourthBody}},
		{`INSERT INTO contents (id, body) VALUES (6, ?)`, []any{"第二章审读：窗外有风。"}},
		{`INSERT INTO contents (id, body) VALUES (7, ?)`, []any{"第一章改稿：她推开门。"}},
		{`INSERT INTO contents (id, body) VALUES (8, ?)`, []any{"第二章改稿：风停了。"}},
		{`INSERT INTO contents (id, body) VALUES (9, ?)`, []any{"第三章改稿甲。"}},
		{`INSERT INTO contents (id, body) VALUES (10, ?)`, []any{"第三章改稿乙。"}},
		{`INSERT INTO contents (id, body) VALUES (11, ?)`, []any{"第一章待判改稿。"}},
		{`INSERT INTO contents (id, body) VALUES (12, ?)`, []any{"第一章待弃改稿。"}},
		{`INSERT INTO contents (id, body) VALUES (13, ?)`, []any{"第四章待冲突改稿。"}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'draft', 'write', 1, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (2, 2, 1, 'draft', 'write', 3, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (3, 3, 1, 'draft', 'write', 4, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (4, 4, 1, 'draft', 'write', 5, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id, created_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (1, 1, 1, 2, '2024-05-06 07:08:09', 1, 1, 'draft', ?)`, []any{productionFixtureDraftBody}},
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id, created_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (2, 2, 5, 6, '2024-05-06 07:08:10', 2, 1, 'draft', ?)`, []any{productionFixtureSecondBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (1, 1, 1, 'refine', 'pending', NULL, '润色', 1, 7, 11, '2024-05-06 07:08:09',
 '2024-05-06 07:08:09', 1, 1, 'draft', ?)`, []any{productionFixtureDraftBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (2, 2, 1, 'refine', 'pending', NULL, '润色', NULL, 8, 11, '2024-05-06 07:08:10',
 '2024-05-06 07:08:10', 2, 1, 'draft', ?)`, []any{productionFixtureSecondBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (3, 3, 1, 'refine', 'pending', NULL, NULL, NULL, 9, 11, '2024-05-06 07:08:11',
 '2024-05-06 07:08:11', 3, 1, 'draft', ?)`, []any{productionFixtureThirdBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (4, 3, 2, 'refine', 'pending', NULL, NULL, NULL, 10, 11, '2024-05-06 07:08:12',
 '2024-05-06 07:08:12', 3, 1, 'draft', ?)`, []any{productionFixtureThirdBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (5, 1, 2, 'refine', 'pending', NULL, NULL, NULL, 11, 11, '2024-05-06 07:08:13',
 '2024-05-06 07:08:13', 1, 1, 'draft', ?)`, []any{productionFixtureDraftBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (6, 1, 3, 'refine', 'pending', NULL, NULL, NULL, 12, 11, '2024-05-06 07:08:14',
 '2024-05-06 07:08:14', 1, 1, 'draft', ?)`, []any{productionFixtureDraftBody}},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (7, 4, 1, 'refine', 'pending', NULL, NULL, NULL, 13, 11, '2024-05-06 07:08:15',
 '2024-05-06 07:08:15', 4, 1, 'draft', ?)`, []any{productionFixtureFourthBody}},
		{`INSERT INTO recovery_candidates (candidate_id, run_id, step_id, project_id,
 chapter_number, chapter_title, source_snapshot, source_hash, source_draft_id,
 source_draft_version, source_draft_identity_captured, visible_text, content_hash,
 failure_code, failure_reason, replaces_candidate_id, status, resolved_at, created_at)
 VALUES (?, 'ci-write-run-1', 'ci-write-step-1', ?, 1, ?, ?, ?, 1, 1, 1, ?, ?, 'GENERATION_FAILED',
 '合成失败', NULL, 'pending', NULL, '2024-05-06 07:08:09')`,
			[]any{productionFixtureCandidateID, productionFixtureProjectID, productionFixtureChapter,
				productionFixtureSource, productionFixtureDigest(productionFixtureSource),
				"候选正文：门后有灯。", productionFixtureDigest("候选正文：门后有灯。")}},
		{`INSERT INTO recovery_candidates (candidate_id, run_id, step_id, project_id,
 chapter_number, chapter_title, source_snapshot, source_hash, source_draft_id,
 source_draft_version, source_draft_identity_captured, visible_text, content_hash,
 failure_code, failure_reason, replaces_candidate_id, status, resolved_at, created_at)
 VALUES (?, 'ci-write-run-2', 'ci-write-step-2', ?, 1, ?, ?, ?, 1, 1, 1, ?, ?, 'GENERATION_FAILED',
 '合成失败', NULL, 'pending', NULL, '2024-05-06 07:08:11')`,
			[]any{productionFixtureResolveID, productionFixtureProjectID, productionFixtureChapter,
				productionFixtureSource, productionFixtureDigest(productionFixtureSource),
				productionFixtureSecondCandidate, productionFixtureDigest(productionFixtureSecondCandidate)}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
}

// productionFixtureCandidateInput is one complete db:recovery-candidate-record
// payload: the normalized chapter-1 source and the captured draft identity of
// draft 1, so the store's own source-current proof succeeds.
func productionFixtureCandidateInput(runID, stepID, text string) map[string]any {
	return map[string]any{
		"runId":         runID,
		"stepId":        stepID,
		"chapterTitle":  productionFixtureChapter,
		"visibleText":   text,
		"failureCode":   "GENERATION_FAILED",
		"failureReason": "合成失败",
		"chapterNumber": float64(1),
		"source": map[string]any{
			"chapterNumber": float64(1),
			"title":         productionFixtureChapter,
			"role":          "",
			"purpose":       "",
			"keyEvents":     "",
			"characters":    []any{},
			"suspenseHook":  "",
			"userGuidance":  "",
		},
		"sourceDraft": map[string]any{"id": float64(1), "version": float64(1)},
	}
}

// TestProductionReviewAndRecoveryWritesCommitUnderLiveLease is the write half of
// this lane: every one of the nine bound names writes through the same borrow the
// lane's existing mutations use, the committed row is read back over a second
// connection, a stale lease refuses all nine, a stale CAS keeps the store's own
// code, and a write that fails inside its transaction leaves the file
// byte-identical.
func TestProductionReviewAndRecoveryWritesCommitUnderLiveLease(t *testing.T) {
	root, projectPath := productionFixtureDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionFixtureContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "review-write-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	if opened.Project["path"] != projectPath || opened.Project["id"] != productionFixtureProjectID {
		t.Fatalf("project:open returned the wrong identity: %v", opened.Project)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	stale := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     "stale-lease-0001",
	}
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	fileNow := func() []byte {
		t.Helper()
		current, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		return current
	}

	// db:review-create - one new review row and its own body.
	t.Run(productionWriteReviewCreateName, func(t *testing.T) {
		var created map[string]any
		author.decode(t, productionWriteReviewCreateName, []any{
			map[string]any{
				"baseDraftId": float64(1),
				"content":     productionFixtureCreatedReview,
				"expectedSource": map[string]any{
					"id":            float64(1),
					"chapterNumber": float64(1),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureDraftBody,
				},
			},
			projectPath, frozen,
		}, &created)
		if created["success"] != true {
			t.Fatalf("db:review-create refused a current review: %v", created)
		}
		if created["reviewIndex"] != float64(2) {
			t.Fatalf("db:review-create answered reviewIndex %v; want 2 (review 2 sits on another draft): %v",
				created["reviewIndex"], created)
		}
		id := productionFixtureNumber(t, created, "id")
		row := productionFixtureSingle(t, projectPath,
			`SELECT r.review_index, r.base_draft_id, r.source_draft_chapter_number,
 r.source_draft_version, r.source_draft_status, r.source_content, c.body
 FROM reviews r JOIN contents c ON c.id = r.content_id WHERE r.id = ?`, id)
		if got := productionFixtureNumber(t, row, "review_index"); got != 2 {
			t.Fatalf("committed review_index = %d; want 2", got)
		}
		if got := productionFixtureNumber(t, row, "base_draft_id"); got != 1 {
			t.Fatalf("committed base_draft_id = %d; want 1", got)
		}
		if got := productionFixtureNumber(t, row, "source_draft_version"); got != 1 {
			t.Fatalf("committed source_draft_version = %d; want 1", got)
		}
		if got := productionFixtureText(t, row, "source_draft_status"); got != "draft" {
			t.Fatalf("committed source status = %q; want draft", got)
		}
		if got := productionFixtureText(t, row, "source_content"); got != productionFixtureDraftBody {
			t.Fatalf("committed source content = %q; want the frozen baseline", got)
		}
		if got := productionFixtureText(t, row, "body"); got != productionFixtureCreatedReview {
			t.Fatalf("committed review body = %q; want %q", got, productionFixtureCreatedReview)
		}
	})

	// db:revision-create - the next per-draft revision index, with prompt and
	// review link, and nothing else moved.
	t.Run(productionWriteRevisionCreateName, func(t *testing.T) {
		var created map[string]any
		author.decode(t, productionWriteRevisionCreateName, []any{
			map[string]any{
				"baseDraftId":    float64(1),
				"revisionType":   "refine",
				"wordCount":      float64(9),
				"userPrompt":     "润色",
				"reviewSourceId": float64(1),
				"content":        productionFixtureCreatedRevision,
				"expectedSource": map[string]any{
					"id":            float64(1),
					"chapterNumber": float64(1),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureDraftBody,
				},
			},
			projectPath, frozen,
		}, &created)
		if created["success"] != true {
			t.Fatalf("db:revision-create refused a current revision: %v", created)
		}
		if created["revisionIndex"] != float64(4) {
			t.Fatalf("db:revision-create answered revisionIndex %v; want 4: %v", created["revisionIndex"], created)
		}
		id := productionFixtureNumber(t, created, "id")
		row := productionFixtureSingle(t, projectPath,
			`SELECT v.revision_index, v.base_draft_id, v.revision_type, v.status, v.user_prompt,
 v.review_source_id, v.word_count, v.source_draft_chapter_number, v.source_draft_version,
 v.source_draft_status, v.source_content, c.body
 FROM revisions v JOIN contents c ON c.id = v.content_id WHERE v.id = ?`, id)
		if got := productionFixtureNumber(t, row, "revision_index"); got != 4 {
			t.Fatalf("committed revision_index = %d; want 4", got)
		}
		if got := productionFixtureText(t, row, "status"); got != "pending" {
			t.Fatalf("committed revision status = %q; want pending", got)
		}
		if got := productionFixtureText(t, row, "revision_type"); got != "refine" {
			t.Fatalf("committed revision_type = %q; want refine", got)
		}
		if got := productionFixtureText(t, row, "user_prompt"); got != "润色" {
			t.Fatalf("committed user_prompt = %q; want the prompt the request named", got)
		}
		if got := productionFixtureNumber(t, row, "review_source_id"); got != 1 {
			t.Fatalf("committed review_source_id = %d; want 1", got)
		}
		if got := productionFixtureNumber(t, row, "word_count"); got != 9 {
			t.Fatalf("committed word_count = %d; want 9", got)
		}
		if got := productionFixtureNumber(t, row, "source_draft_chapter_number"); got != 1 {
			t.Fatalf("committed source chapter = %d; want 1", got)
		}
		if got := productionFixtureText(t, row, "body"); got != productionFixtureCreatedRevision {
			t.Fatalf("committed revision body = %q; want %q", got, productionFixtureCreatedRevision)
		}
		// The store decides the index itself (SELECT COALESCE(MAX(revision_index),0)+1
		// inside BEGIN IMMEDIATE), so this row must be the fourth pending revision of
		// base 1: three were seeded, and nothing else in this subtest can move the
		// ledger. status is not written by that INSERT - the fixture's baseline
		// default supplies 'pending' - so the ledger count, not the single value, is
		// what proves a new pending row reached the file.
		if got := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			`SELECT COUNT(*) AS total FROM revisions WHERE base_draft_id = 1 AND status = 'pending'`), "total"); got != 4 {
			t.Fatalf("base 1 holds %d pending revisions; want 4 after the create", got)
		}
	})

	// db:revision-replace-pending - the new pending revision AND the two old
	// pending ones of the same base, discarded in the same transaction.
	t.Run(productionWriteRevisionReplacePendingName, func(t *testing.T) {
		var created map[string]any
		author.decode(t, productionWriteRevisionReplacePendingName, []any{
			map[string]any{
				"baseDraftId":  float64(3),
				"revisionType": "refine",
				"wordCount":    float64(5),
				"content":      productionFixtureThirdReplace,
				"expectedSource": map[string]any{
					"id":            float64(3),
					"chapterNumber": float64(3),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureThirdBody,
				},
			},
			projectPath, frozen,
		}, &created)
		if created["success"] != true {
			t.Fatalf("db:revision-replace-pending refused a current revision: %v", created)
		}
		if created["revisionIndex"] != float64(3) {
			t.Fatalf("db:revision-replace-pending answered revisionIndex %v; want 3: %v", created["revisionIndex"], created)
		}
		id := productionFixtureNumber(t, created, "id")
		row := productionFixtureSingle(t, projectPath,
			`SELECT v.revision_index, v.status, v.word_count, c.body
 FROM revisions v JOIN contents c ON c.id = v.content_id WHERE v.id = ?`, id)
		if got := productionFixtureText(t, row, "status"); got != "pending" {
			t.Fatalf("the replacement revision status = %q; want pending", got)
		}
		if got := productionFixtureNumber(t, row, "revision_index"); got != 3 {
			t.Fatalf("the replacement revision_index = %d; want 3", got)
		}
		if got := productionFixtureText(t, row, "body"); got != productionFixtureThirdReplace {
			t.Fatalf("the replacement body = %q; want %q", got, productionFixtureThirdReplace)
		}
		// Base 3's two previous pending revisions are the ones this call dissolved.
		for _, revisionID := range []int64{3, 4} {
			dissolved := productionFixtureSingle(t, projectPath,
				`SELECT status FROM revisions WHERE id = ?`, revisionID)
			if got := productionFixtureText(t, dissolved, "status"); got != "discarded" {
				t.Fatalf("revision %d status = %q; want discarded by replace-pending", revisionID, got)
			}
		}
		// A different base's pending revision is untouched: the replace is scoped
		// to the draft the request named.
		untouched := productionFixtureSingle(t, projectPath, `SELECT status FROM revisions WHERE id = 1`)
		if got := productionFixtureText(t, untouched, "status"); got != "pending" {
			t.Fatalf("revision 1 status = %q; a replace on base 3 must not touch base 1", got)
		}
	})

	// db:revision-merge - the CAS-committed merge, then the receipt and the three
	// rows it changed, read back from the file.
	t.Run(productionWriteRevisionMergeName, func(t *testing.T) {
		var merged map[string]any
		author.decode(t, productionWriteRevisionMergeName, []any{
			map[string]any{
				"revisionId":           float64(productionFixtureMergeRevisionID),
				"targetDraftId":        float64(2),
				"expectedDraftContent": productionFixtureSecondBody,
				"mergedContent":        productionFixtureMergedBody,
				"wordCount":            float64(13),
			},
			projectPath, frozen,
		}, &merged)
		if merged["success"] != true {
			t.Fatalf("db:revision-merge refused a current revision: %v", merged)
		}
		receipt, ok := merged["receipt"].(map[string]any)
		if !ok {
			t.Fatalf("db:revision-merge answered no receipt: %v", merged)
		}
		if receipt["status"] != "revised" || receipt["idempotent"] != false ||
			receipt["revisionId"] != float64(productionFixtureMergeRevisionID) ||
			receipt["targetDraftId"] != float64(2) || receipt["wordCount"] != float64(13) {
			t.Fatalf("db:revision-merge receipt does not describe the commit: %v", receipt)
		}
		draft := productionFixtureSingle(t, projectPath,
			`SELECT d.status, d.word_count, c.body FROM drafts d
 JOIN contents c ON c.id = d.content_id WHERE d.id = 2`)
		if got := productionFixtureText(t, draft, "status"); got != "revised" {
			t.Fatalf("merged draft status = %q; want revised", got)
		}
		if got := productionFixtureNumber(t, draft, "word_count"); got != 13 {
			t.Fatalf("merged draft word_count = %d; want 13", got)
		}
		if got := productionFixtureText(t, draft, "body"); got != productionFixtureMergedBody {
			t.Fatalf("merged draft body = %q; want %q", got, productionFixtureMergedBody)
		}
		revision := productionFixtureSingle(t, projectPath,
			`SELECT status, merged_to_draft_id FROM revisions WHERE id = ?`, productionFixtureMergeRevisionID)
		if got := productionFixtureText(t, revision, "status"); got != "merged" {
			t.Fatalf("merged revision status = %q; want merged", got)
		}
		if got := productionFixtureNumber(t, revision, "merged_to_draft_id"); got != 2 {
			t.Fatalf("merged revision merged_to_draft_id = %d; want 2", got)
		}
		// The draft a merge must not touch still carries its own body.
		other := productionFixtureSingle(t, projectPath,
			`SELECT c.body FROM drafts d JOIN contents c ON c.id = d.content_id WHERE d.id = 1`)
		if got := productionFixtureText(t, other, "body"); got != productionFixtureDraftBody {
			t.Fatalf("draft 1 body = %q; the merge of draft 2 rewrote another draft", got)
		}
	})

	// db:revision-mark-merged - status and target, nothing else.
	t.Run(productionWriteRevisionMarkMergedName, func(t *testing.T) {
		var marked map[string]any
		author.decode(t, productionWriteRevisionMarkMergedName, []any{
			float64(productionFixturePendingMergeID), float64(1), projectPath, frozen,
		}, &marked)
		if marked["success"] != true {
			t.Fatalf("db:revision-mark-merged refused a pending revision: %v", marked)
		}
		row := productionFixtureSingle(t, projectPath,
			`SELECT status, merged_to_draft_id FROM revisions WHERE id = ?`, productionFixturePendingMergeID)
		if got := productionFixtureText(t, row, "status"); got != "merged" {
			t.Fatalf("marked revision status = %q; want merged", got)
		}
		if got := productionFixtureNumber(t, row, "merged_to_draft_id"); got != 1 {
			t.Fatalf("marked revision merged_to_draft_id = %d; want 1", got)
		}
	})

	// db:revision-mark-discarded - a discarded revision names no target.
	t.Run(productionWriteRevisionMarkDiscardedName, func(t *testing.T) {
		var marked map[string]any
		author.decode(t, productionWriteRevisionMarkDiscardedName, []any{
			float64(productionFixturePendingDiscardID), projectPath, frozen,
		}, &marked)
		if marked["success"] != true {
			t.Fatalf("db:revision-mark-discarded refused a pending revision: %v", marked)
		}
		row := productionFixtureSingle(t, projectPath,
			`SELECT status, merged_to_draft_id FROM revisions WHERE id = ?`, productionFixturePendingDiscardID)
		if got := productionFixtureText(t, row, "status"); got != "discarded" {
			t.Fatalf("discarded revision status = %q; want discarded", got)
		}
		if row["merged_to_draft_id"] != nil {
			t.Fatalf("discarded revision carries a merge target: %v", row["merged_to_draft_id"])
		}
	})

	// db:recovery-candidate-record - the injected identity, the source proof and
	// the content hash the store recomputes.
	t.Run(productionWriteRecoveryRecordName, func(t *testing.T) {
		var recorded map[string]any
		author.decode(t, productionWriteRecoveryRecordName, []any{
			productionFixtureCandidateInput("ci-write-run-3", "ci-write-step-3", productionFixtureCreatedCandidate),
			projectPath, frozen,
		}, &recorded)
		if recorded["success"] != true {
			t.Fatalf("db:recovery-candidate-record refused a current source: %v", recorded)
		}
		candidate, ok := recorded["candidate"].(map[string]any)
		if !ok {
			t.Fatalf("db:recovery-candidate-record answered no candidate: %v", recorded)
		}
		if candidate["sourceCurrent"] != true {
			t.Fatalf("the recorded candidate is not source-current: %v", candidate)
		}
		if candidate["projectId"] != productionFixtureProjectID {
			t.Fatalf("the recorded projectId is %v; the frozen identity must be injected, not the renderer's",
				candidate["projectId"])
		}
		id, ok := candidate["candidateId"].(string)
		if !ok || id == "" {
			t.Fatalf("the recorded candidate has no id: %v", candidate)
		}
		row := productionFixtureSingle(t, projectPath,
			`SELECT run_id, step_id, project_id, chapter_number, chapter_title, source_hash,
 source_draft_id, source_draft_version, source_draft_identity_captured, visible_text,
 content_hash, status, resolved_at FROM recovery_candidates WHERE candidate_id = ?`, id)
		if got := productionFixtureText(t, row, "run_id"); got != "ci-write-run-3" {
			t.Fatalf("committed run_id = %q", got)
		}
		if got := productionFixtureText(t, row, "project_id"); got != productionFixtureProjectID {
			t.Fatalf("committed project_id = %q; the frozen identity must be the only source", got)
		}
		if got := productionFixtureNumber(t, row, "chapter_number"); got != 1 {
			t.Fatalf("committed chapter_number = %d; want 1", got)
		}
		if got := productionFixtureText(t, row, "visible_text"); got != productionFixtureCreatedCandidate {
			t.Fatalf("committed visible_text = %q", got)
		}
		if got := productionFixtureText(t, row, "content_hash"); got != productionFixtureDigest(productionFixtureCreatedCandidate) {
			t.Fatalf("committed content_hash = %q; want the digest of the committed text", got)
		}
		if got := productionFixtureText(t, row, "source_hash"); got != productionFixtureDigest(productionFixtureSource) {
			t.Fatalf("committed source_hash = %q; want the frozen chapter source digest", got)
		}
		if got := productionFixtureNumber(t, row, "source_draft_version"); got != 1 {
			t.Fatalf("committed source_draft_version = %d; want 1", got)
		}
		if got := productionFixtureNumber(t, row, "source_draft_identity_captured"); got != 1 {
			t.Fatalf("committed source_draft_identity_captured = %d; want 1", got)
		}
		if got := productionFixtureText(t, row, "status"); got != "pending" {
			t.Fatalf("committed candidate status = %q; want pending", got)
		}
		if row["resolved_at"] != nil {
			t.Fatalf("a freshly recorded candidate is already resolved: %v", row["resolved_at"])
		}
	})

	// db:recovery-candidate-update - the body AND its digest move together.
	t.Run(productionWriteRecoveryUpdateName, func(t *testing.T) {
		var updated map[string]any
		author.decode(t, productionWriteRecoveryUpdateName, []any{
			productionFixtureCandidateID, productionFixtureUpdatedCandidate, projectPath, frozen,
		}, &updated)
		if updated["success"] != true {
			t.Fatalf("db:recovery-candidate-update refused a source-current candidate: %v", updated)
		}
		if candidate, ok := updated["candidate"].(map[string]any); !ok || candidate["visibleText"] != productionFixtureUpdatedCandidate {
			t.Fatalf("db:recovery-candidate-update answered the wrong candidate: %v", updated)
		}
		row := productionFixtureSingle(t, projectPath,
			`SELECT visible_text, content_hash, source_hash, status
 FROM recovery_candidates WHERE candidate_id = ?`, productionFixtureCandidateID)
		if got := productionFixtureText(t, row, "visible_text"); got != productionFixtureUpdatedCandidate {
			t.Fatalf("committed visible_text = %q; want the edited body", got)
		}
		if got := productionFixtureText(t, row, "content_hash"); got != productionFixtureDigest(productionFixtureUpdatedCandidate) {
			t.Fatalf("committed content_hash = %q; the digest did not follow the body", got)
		}
		if got := productionFixtureText(t, row, "source_hash"); got != productionFixtureDigest(productionFixtureSource) {
			t.Fatalf("committed source_hash = %q; an edit must not rewrite the frozen source", got)
		}
		if got := productionFixtureText(t, row, "status"); got != "pending" {
			t.Fatalf("committed status = %q; an edit must not resolve the candidate", got)
		}
	})

	// db:recovery-candidate-resolve - the status and the resolution stamp.
	t.Run(productionWriteRecoveryResolveName, func(t *testing.T) {
		var resolved map[string]any
		author.decode(t, productionWriteRecoveryResolveName, []any{
			productionFixtureResolveID, "continued", projectPath, frozen,
		}, &resolved)
		if resolved["success"] != true {
			t.Fatalf("db:recovery-candidate-resolve refused a source-current candidate: %v", resolved)
		}
		row := productionFixtureSingle(t, projectPath,
			`SELECT status, resolved_at, visible_text FROM recovery_candidates WHERE candidate_id = ?`,
			productionFixtureResolveID)
		if got := productionFixtureText(t, row, "status"); got != "continued" {
			t.Fatalf("committed status = %q; want continued", got)
		}
		if row["resolved_at"] == nil {
			t.Fatal("committed resolved_at is NULL; the resolution was not stamped")
		}
		if got := productionFixtureText(t, row, "visible_text"); got != productionFixtureSecondCandidate {
			t.Fatalf("committed visible_text = %q; a resolve must not rewrite the body", got)
		}
	})

	// A stale CAS keeps the STORE's code and writes nothing.
	t.Run("stale-cas", func(t *testing.T) {
		before := fileNow()
		productionFixtureRefused(t, author, productionWriteRevisionMergeName, []any{
			map[string]any{
				"revisionId":           float64(productionFixtureConflictID),
				"targetDraftId":        float64(4),
				"expectedDraftContent": "过期正文：不是这一版。",
				"mergedContent":        productionFixtureStaleMergedBody,
				"wordCount":            float64(12),
			},
			projectPath, frozen,
		}, "REVISION_TARGET_CHANGED")
		if after := fileNow(); !bytes.Equal(before, after) {
			t.Fatal("a stale-CAS refusal changed the admitted author database")
		}
		draft := productionFixtureSingle(t, projectPath,
			`SELECT d.status, d.word_count, c.body FROM drafts d
 JOIN contents c ON c.id = d.content_id WHERE d.id = 4`)
		if got := productionFixtureText(t, draft, "body"); got != productionFixtureFourthBody {
			t.Fatalf("a stale-CAS refusal overwrote the draft body with %q", got)
		}
		if got := productionFixtureText(t, draft, "status"); got != "draft" {
			t.Fatalf("a stale-CAS refusal changed the draft status to %q", got)
		}
		if got := productionFixtureNumber(t, draft, "word_count"); got != 11 {
			t.Fatalf("a stale-CAS refusal changed word_count to %d", got)
		}
		revision := productionFixtureSingle(t, projectPath, `SELECT status FROM revisions WHERE id = ?`, productionFixtureConflictID)
		if got := productionFixtureText(t, revision, "status"); got != "pending" {
			t.Fatalf("a stale-CAS refusal changed the revision status to %q", got)
		}
	})

	// A write that fails INSIDE its transaction leaves the file byte-identical: the
	// fixture's CHECK guard rejects the review row AFTER the body row was inserted,
	// so this measures the store's rollback.
	t.Run("failed-write", func(t *testing.T) {
		before := fileNow()
		contentsBefore := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			`SELECT COUNT(*) AS total FROM contents`), "total")
		reviewsBefore := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			`SELECT COUNT(*) AS total FROM reviews`), "total")
		productionFixtureRefused(t, author, productionWriteReviewCreateName, []any{
			map[string]any{
				"baseDraftId": float64(4),
				"content":     productionFixtureCreatedReview,
				"expectedSource": map[string]any{
					"id":            float64(4),
					"chapterNumber": float64(4),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureFourthBody,
				},
			},
			projectPath, frozen,
		}, "REVIEW_REVISION_OPERATION_FAILED")
		if after := fileNow(); !bytes.Equal(before, after) {
			t.Fatal("a failed write left the admitted author database changed")
		}
		contentsAfter := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			`SELECT COUNT(*) AS total FROM contents`), "total")
		reviewsAfter := productionFixtureNumber(t, productionFixtureSingle(t, projectPath,
			`SELECT COUNT(*) AS total FROM reviews`), "total")
		if contentsAfter != contentsBefore || reviewsAfter != reviewsBefore {
			t.Fatalf("a failed write left rows behind: contents %d -> %d, reviews %d -> %d",
				contentsBefore, contentsAfter, reviewsBefore, reviewsAfter)
		}
	})

	// Every handler refuses without the live write lease, with the lane's own fence
	// code, and none of the nine touches a byte.
	t.Run("stale-lease", func(t *testing.T) {
		before := fileNow()
		for _, probe := range []struct {
			operation string
			args      []any
		}{
			{productionWriteReviewCreateName, []any{map[string]any{
				"baseDraftId": float64(1),
				"content":     productionFixtureCreatedReview,
				"expectedSource": map[string]any{
					"id":            float64(1),
					"chapterNumber": float64(1),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureDraftBody,
				},
			}, projectPath, stale}},
			{productionWriteRevisionCreateName, []any{map[string]any{
				"baseDraftId":  float64(1),
				"revisionType": "refine",
				"wordCount":    float64(9),
				"content":      productionFixtureCreatedRevision,
				"expectedSource": map[string]any{
					"id":            float64(1),
					"chapterNumber": float64(1),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureDraftBody,
				},
			}, projectPath, stale}},
			{productionWriteRevisionReplacePendingName, []any{map[string]any{
				"baseDraftId":  float64(1),
				"revisionType": "refine",
				"wordCount":    float64(9),
				"content":      productionFixtureThirdReplace,
				"expectedSource": map[string]any{
					"id":            float64(1),
					"chapterNumber": float64(1),
					"version":       float64(1),
					"status":        "draft",
					"content":       productionFixtureDraftBody,
				},
			}, projectPath, stale}},
			{productionWriteRevisionMergeName, []any{map[string]any{
				"revisionId":           float64(productionFixtureConflictID),
				"targetDraftId":        float64(4),
				"expectedDraftContent": productionFixtureFourthBody,
				"mergedContent":        productionFixtureStaleMergedBody,
				"wordCount":            float64(12),
			}, projectPath, stale}},
			{productionWriteRevisionMarkMergedName, []any{
				float64(productionFixtureConflictID), float64(4), projectPath, stale}},
			{productionWriteRevisionMarkDiscardedName, []any{
				float64(productionFixtureConflictID), projectPath, stale}},
			{productionWriteRecoveryRecordName, []any{
				productionFixtureCandidateInput("ci-write-run-9", "ci-write-step-9", productionFixtureCreatedCandidate),
				projectPath, stale}},
			{productionWriteRecoveryUpdateName, []any{
				productionFixtureCandidateID, productionFixtureUpdatedCandidate, projectPath, stale}},
			{productionWriteRecoveryResolveName, []any{
				productionFixtureCandidateID, "discarded", projectPath, stale}},
		} {
			productionFixtureRefused(t, author, probe.operation, probe.args,
				"PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE")
		}
		if after := fileNow(); !bytes.Equal(before, after) {
			t.Fatal("a lease-refused write changed the admitted author database")
		}
	})

	// The nine names are bound, not fail-closed: this lane answers them, and every
	// frame above proved the answer came from the author's own database.
	for _, name := range []string{
		productionWriteReviewCreateName,
		productionWriteRevisionCreateName,
		productionWriteRevisionReplacePendingName,
		productionWriteRevisionMergeName,
		productionWriteRevisionMarkMergedName,
		productionWriteRevisionMarkDiscardedName,
		productionWriteRecoveryRecordName,
		productionWriteRecoveryUpdateName,
		productionWriteRecoveryResolveName,
	} {
		if response := author.call(t, name, []any{}); response.Code == http.StatusNotImplemented ||
			strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED: %s", name, response.Body.String())
		}
	}
}

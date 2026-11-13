//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The three destroy operations below run against a disposable t.TempDir() only.
// No real author data root is read or written: the writer lease, the project
// databases and every readback live inside the test's own temporary tree.
const (
	productionDBMaintenanceProjectName = "db-maintenance-novel"
	productionDBMaintenanceProjectID   = "0f4c2a86-1d7b-4e35-9a52-6c8e1f7b3d09"
	productionDBMaintenanceTab         = "client_dbmainten01"
	productionDBMaintenanceOpenID      = "db-maintenance-open-1"
	productionDBMaintenanceStaleLease  = "db-maintenance-stale-lease"

	// The two bodies every draft in the fixture stands on, so a cleared draft is
	// proved by its content disappearing rather than by a row count alone.
	productionDBMaintenanceBody1 = "第一章成稿：他推开门。"
	productionDBMaintenanceBody2 = "第二章成稿：雨停了。"

	// The import operation id the renderer builds for one import run
	// (src/services/workflows/commands/import-novel.command.ts:509).
	productionDBMaintenanceImportOperation = "novel-import-global-run-0001"

	// The character names the author wrote. A character whose name carries a
	// non-ASCII byte proves the resolution and proposal rows are UTF-8 safe.
	productionDBMaintenanceCharacter1 = "甲作者角色"
	productionDBMaintenanceCharacter2 = "乙作者角色"
)

// productionDBMaintenanceRoot builds the disposable data root with one admitted
// project. The schema is the pinned v7 baseline itself
// (internal/admission/baseline_v7_ddl.go), not a hand-written reduction: admission
// hashes sqlite_master and then proves the character-identity domain, which reads
// character_identity_origins and character_identity_proposals unconditionally, so
// a reduced schema fails at admission with SCHEMA_DOMAIN_INVARIANT_FAILED before
// any handler runs. The baseline also carries the generation ledger the in-flight
// guard reads.
//
// The database is created with that schema and then seeded; the lane opens it
// afterwards, because the admission permit hashes the file at open.
func productionDBMaintenanceRoot(t *testing.T) (root, projectPath, database string, contract admission.SchemaContract) {
	t.Helper()
	root = syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionDBMaintenanceProjectName, productionDBMaintenanceProjectID,
		admission.CurrentSchemaVersion, admission.BaselineV7Statements())
	projectPath = filepath.Join(library, productionDBMaintenanceProjectName)
	database = filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	productionDBMaintenanceSeed(t, database)
	pinned, err := admission.PinnedSchemaContract()
	if err != nil {
		t.Fatal(err)
	}
	return root, projectPath, database, pinned
}

// productionDBMaintenanceSeed writes two rows into every table the three
// operations touch: two drafts on two bodies, two reviews, two revisions, two
// summaries, two post-process runs and steps, two import operations, two
// blueprint chapters with their two commit and two sync operations, two characters
// with two aliases, two chapter-deletion receipts, one project core row whose every
// column carries the author's own text, and one finished chapter-blueprint-directory
// generation run, so the in-flight guard is proved to be about a run's status and
// not about a run existing at all.
func productionDBMaintenanceSeed(t *testing.T, database string) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, premise, worldbuilding, synopsis, characters_arch,
 created_at, updated_at)
 VALUES ('main', '清除验证小说', 'zh-CN', 'fantasy', '宫廷', '成年读者', 12, 3000, 'auto',
 3, 'three_act', 'third_limited', '作者核心大纲', '作者世界设定', '作者金手指',
 '作者主角档案', '作者全局指导', '作者文风', '作者参考作品', '{"甲":"状态"}',
 '作者前提', '作者世界观', '作者总大纲', '作者人物群像',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO contents (id, body) VALUES (1, ?), (2, ?)`,
			[]any{productionDBMaintenanceBody1, productionDBMaintenanceBody2}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies) VALUES (1, 1, 1, 'finalized', 'write', 1, 11, '[]')`, nil},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies) VALUES (2, 2, 1, 'revised', 'rewrite', 2, 9, '[]')`, nil},
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id) VALUES (1, 1, 1, 1)`, nil},
		{`INSERT INTO reviews (id, base_draft_id, review_index, content_id) VALUES (2, 2, 1, 2)`, nil},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status, content_id)
 VALUES (1, 1, 1, 'review-fix', 'merged', 1)`, nil},
		{`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status, content_id)
 VALUES (2, 2, 1, 'refine', 'pending', 2)`, nil},
		{`INSERT INTO summary_snapshots (id, draft_id, chapter_number, chapter_notes)
 VALUES (1, 1, 1, '第一章摘要')`, nil},
		{`INSERT INTO summary_snapshots (id, draft_id, chapter_number, chapter_notes)
 VALUES (2, 2, 2, '第二章摘要')`, nil},
		{`INSERT INTO post_process_runs (id, trigger_source_type, trigger_source_id)
 VALUES ('pp-1', 'chapter_finalize', '1'), ('pp-2', 'chapter_finalize', '2')`, nil},
		{`INSERT INTO post_process_steps (run_id, step_key) VALUES ('pp-1', 'notes'), ('pp-2', 'notes')`, nil},
		{`INSERT INTO finalized_draft_import_operations (operation_id, payload_hash, receipt_json)
 VALUES ('import-op-1', 'hash-1', '{}'), ('import-op-2', 'hash-2', '{}')`, nil},
		{`INSERT INTO blueprints (chapter_number, title, characters) VALUES (1, '既有第一章', '["甲作者角色"]')`, nil},
		{`INSERT INTO blueprints (chapter_number, title, characters) VALUES (2, '既有第二章', '["乙作者角色"]')`, nil},
		{`INSERT INTO blueprint_commit_operations (operation_id, payload_hash, mode, start_chapter, end_chapter, character_sync_input)
 VALUES ('commit-1', 'payload-1', 'full', 1, 2, '[]'), ('commit-2', 'payload-2', 'replace-range', 1, 1, '[]')`, nil},
		{`INSERT INTO blueprint_character_sync_operations (operation_id, blueprint_commit_operation_id,
 blueprint_commit_payload_hash, start_chapter, end_chapter, character_sync_input)
 VALUES ('blueprint-sync-commit-1', 'commit-1', 'payload-1', 1, 2, '[]'),
 ('blueprint-sync-commit-2', 'commit-2', 'payload-2', 1, 1, '[]')`, nil},
		{`INSERT INTO characters (character_id, legacy_key, name, role, gender, age, appearance,
 personality, background, abilities, motivation, relationships, arc, notes,
 cs_provenance, static_provenance, identity_revision, retired)
 VALUES ('character-1', NULL, ?, 'protagonist', '', '', '', '', '', '', '', '', '', '',
 '{}', '{"kind":"author"}', 1, 0)`, []any{productionDBMaintenanceCharacter1}},
		{`INSERT INTO characters (character_id, legacy_key, name, role, gender, age, appearance,
 personality, background, abilities, motivation, relationships, arc, notes,
 cs_provenance, static_provenance, identity_revision, retired)
 VALUES ('character-2', NULL, ?, 'supporting', '', '', '', '', '', '', '', '', '', '',
 '{}', '{"kind":"author"}', 1, 0)`, []any{productionDBMaintenanceCharacter2}},
		{`INSERT INTO character_aliases (character_id, name, source_key, valid_from, valid_through)
 VALUES ('character-1', ?, 'author:manual', 1, NULL), ('character-2', ?, 'author:manual', 1, NULL)`,
			[]any{productionDBMaintenanceCharacter1, productionDBMaintenanceCharacter2}},
		{`INSERT INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 2, 0)`, nil},
		{`INSERT INTO character_roster_meta (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'ready', '', '', '')`, nil},
		{`INSERT INTO chapter_deletion_operations (operation_id, draft_id, chapter_number, chapter_title, finalization_id)
 VALUES ('delete-1', 91, 91, '已删第一章', 'final-91'), ('delete-2', 92, 92, '已删第二章', 'final-92')`, nil},
		// One finished directory run, so the success arm proves the in-flight guard is
		// about a run's status and not about a run existing at all.
		{`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json, revision)
 VALUES ('root-finished-1', 'idem-finished-1', '{}', '{}', 0)`, nil},
		{`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES ('run-finished-1', 'root-finished-1', '{"sourceManifest":{"operation":"chapter-blueprint-directory"}}',
 'completed', 1700000000000, 'open-finished-1')`, nil},
	}
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	seed, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	seed.SetMaxOpenConns(1)
	productionDBMaintenanceExec(t, seed, statements)
	productionDBMaintenanceCheckpoint(t, seed, database)
}

// productionDBMaintenanceCheckpoint folds any journal sidecar back into the file
// before the lane admits it. The lane admits the file itself, so a sidecar holding
// seeded rows would be rows the admitted digest never covered, and every readback
// below would be reading a different database from the one the handlers opened.
func productionDBMaintenanceCheckpoint(t *testing.T, db *sql.DB, database string) {
	t.Helper()
	var backfill, logFrames, checkpointed int64
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&backfill, &logFrames, &checkpointed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		t.Fatalf("checkpoint the seeded database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-shm", "-wal", "-journal"} {
		sidecar := database + suffix
		if _, err := os.Lstat(sidecar); err == nil {
			if err := os.Remove(sidecar); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// productionDBMaintenanceOpen opens one connection to the fixture database.
func productionDBMaintenanceOpen(t *testing.T, database, mode string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = mode
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func productionDBMaintenanceExec(t *testing.T, db *sql.DB, statements []struct {
	query string
	args  []any
}) {
	t.Helper()
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed statement failed: %v", err)
		}
	}
}

// productionDBMaintenanceLane opens the author runtime and mounts a transport
// server with project:open plus the binder this batch adds. The binder line itself
// belongs in production_open_linux.go, which this batch may not edit; installing
// the same binder on the same server type through the same durable bridge is what
// lets these tests run before that wiring lands.
func productionDBMaintenanceLane(t *testing.T, root string, contract admission.SchemaContract) *transport.Server {
	t.Helper()
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-db-maintenance")
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	if err := newProductionDBMaintenance(open.durable).Bind(server); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	return server
}

// productionDBMaintenanceAdmit opens the fixture project and returns the frozen
// three-key context every request must carry.
func productionDBMaintenanceAdmit(t *testing.T, server *transport.Server, projectPath string) (*productionDurableClient, map[string]any) {
	t.Helper()
	client := newProductionDurableClient(t, server, productionDBMaintenanceTab)
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionDBMaintenanceOpenID, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the fixture project: %+v", opened)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return client, map[string]any{
		"projectId": opened.Project["id"], "projectPath": opened.Project["path"], "leaseId": lease,
	}
}

// productionDBMaintenanceDigest is a digest of the whole database file read while a
// SEPARATE mode=ro connection holds it. The digest is taken over the main file and
// its WAL sidecar when one exists, and the read-only connection is opened first so
// the check itself cannot be the write that changes the file.
func productionDBMaintenanceDigest(t *testing.T, database string) string {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	reader, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	reader.SetMaxOpenConns(1)
	defer func() {
		if closeErr := reader.Close(); closeErr != nil {
			t.Fatalf("close read-only digest connection: %v", closeErr)
		}
	}()
	// Prove the read-only connection really attached this file before hashing it.
	var version int
	if err := reader.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read-only digest connection: %v", err)
	}
	digest := sha256.New()
	for _, path := range []string{database, database + "-wal"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			t.Fatal(err)
		}
		if _, err := digest.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// productionDBMaintenanceUnchanged proves a refusal left the whole file exactly as
// it was: the same digest the successful path is proved to change.
func productionDBMaintenanceUnchanged(t *testing.T, database, before, label string) {
	t.Helper()
	if after := productionDBMaintenanceDigest(t, database); after != before {
		t.Fatalf("%s rewrote the admitted author database", label)
	}
}

// productionDBMaintenanceResult returns the result member of a 200 envelope.
func productionDBMaintenanceResult(t *testing.T, response *httptest.ResponseRecorder) json.RawMessage {
	t.Helper()
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("decode operation result: %v", err)
	}
	return wire.Result
}

// productionDBMaintenanceRefusal is the refusal a call answered: the symbolic code
// for an error answer, or the code after `Error: ` for the two handlers whose
// controller catches everything and answers { success: false }.
func productionDBMaintenanceRefusal(t *testing.T, client *productionDurableClient, operation string, args []any) string {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusOK {
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: %v %s", operation, err, response.Body.String())
		}
		return body.Error.Code
	}
	var wire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Result.Success {
		t.Fatalf("%s answered 200 success for a request that had to be refused", operation)
	}
	return strings.TrimPrefix(wire.Result.Error, "Error: ")
}

// productionDBMaintenanceLeaseRefusal requires the refusal to be one of the lease
// or project-identity answers the borrow can produce, so a SQL failure can never
// be mistaken for a refusal.
func productionDBMaintenanceLeaseRefusal(t *testing.T, code, label string) {
	t.Helper()
	for _, accepted := range []string{"PROJECT_SESSION", "PROJECT_NOT_OPEN", "AUTHOR_WRITE_STALE",
		"PROJECT_CONTEXT_CHANGED", "PROJECT_CONTEXT_REQUIRED", "PRODUCTION_DB_MAINTENANCE_UNAVAILABLE"} {
		if strings.Contains(code, accepted) {
			return
		}
	}
	t.Fatalf("%s refused with %v, which is not a lease or project-identity refusal", label, code)
}

// productionDBMaintenanceCount is a table's row count read over a read-only
// connection, so a claim about preserved rows never trusts the lane's own handle.
func productionDBMaintenanceCount(t *testing.T, database, table string) int64 {
	t.Helper()
	db := productionDBMaintenanceOpen(t, database, "mode=ro")
	defer func() { _ = db.Close() }()
	var count int64
	if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return count
}

// productionDBMaintenanceText is one text column read over a read-only connection.
func productionDBMaintenanceText(t *testing.T, database, query string, args ...any) string {
	t.Helper()
	db := productionDBMaintenanceOpen(t, database, "mode=ro")
	defer func() { _ = db.Close() }()
	var value string
	if err := db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatalf("read-only query failed: %v", err)
	}
	return value
}

// productionDBMaintenanceCore is the whole project_core main row as one canonical
// string, so "the project core is still there" means every column, not a count.
func productionDBMaintenanceCore(t *testing.T, database string) string {
	t.Helper()
	db := productionDBMaintenanceOpen(t, database, "mode=ro")
	defer func() { _ = db.Close() }()
	var id, name, language, genre, subGenre, audience, structure, pov, outline, setting,
		finger, profile, guidance, style, references, states, premise, worldbuilding,
		synopsis, charactersArch string
	var chapters, words, dormant int64
	err := db.QueryRow(`SELECT id,project_name,writing_language,genre,sub_genre,target_audience,
 total_chapters,words_per_chapter,narrative_thread_dormant_threshold,plot_structure,narrative_pov,
 core_outline,world_setting,golden_finger,protagonist_profile,global_guidance,writing_style,
 reference_works,character_states,premise,worldbuilding,synopsis,characters_arch
 FROM project_core WHERE id='main'`).Scan(&id, &name, &language, &genre, &subGenre, &audience,
		&chapters, &words, &dormant, &structure, &pov, &outline, &setting, &finger, &profile,
		&guidance, &style, &references, &states, &premise, &worldbuilding, &synopsis, &charactersArch)
	if err != nil {
		t.Fatalf("read project core: %v", err)
	}
	return strings.Join([]string{id, name, language, genre, subGenre, audience, structure, pov,
		outline, setting, finger, profile, guidance, style, references, states, premise,
		worldbuilding, synopsis, charactersArch}, "\x1f")
}

// productionDBMaintenanceImportRequest is the renderer's commit request
// (import-novel.command.ts:510-539) with two character cards, each carrying one
// relationship and the second carrying a currentState with a number, so the
// payload hash has to survive a JSON number.
func productionDBMaintenanceImportRequest(operationID string, rosterRevision int64, genre string) map[string]any {
	return map[string]any{
		"operationId":            operationID,
		"expectedRosterRevision": rosterRevision,
		"core": map[string]any{
			"genre":              genre,
			"subGenre":           "宫廷",
			"targetAudience":     "成年读者",
			"totalChapters":      24,
			"wordsPerChapter":    3200,
			"plotStructure":      "three_act",
			"narrativePov":       "third_limited",
			"goldenFinger":       "导入金手指",
			"globalGuidance":     "导入全局指导",
			"coreOutline":        "导入核心大纲",
			"worldSetting":       "导入世界设定",
			"protagonistProfile": "导入主角档案",
			"premise":            "导入前提",
			"worldbuilding":      "导入世界观",
			"synopsis":           "导入总大纲",
		},
		"characterEntries": []any{
			map[string]any{
				"name": "导入甲", "role": "protagonist", "gender": "男", "age": "二十",
				"appearance": "高", "personality": "冷", "background": "边关", "abilities": "剑",
				"motivation": "复仇", "arc": "归来", "notes": "",
				"relationships": []any{map[string]any{"target": "导入乙", "relation": "同门"}},
			},
			map[string]any{
				"name": "导入乙", "role": "supporting", "gender": "女", "age": "十九",
				"appearance": "瘦", "personality": "暖", "background": "药铺", "abilities": "医",
				"motivation": "守城", "arc": "留下", "notes": "",
				"relationships": []any{map[string]any{"target": "导入甲", "relation": "同门"}},
				"currentState":  map[string]any{"location": "城门", "updatedAtChapter": 3},
			},
		},
	}
}

// TestProductionDBMaintenanceBlueprintClearAllKeepsEveryDraft proves
// db:blueprint-clear-all over real HTTP through the frozen session: the three
// blueprint tables are emptied (two rows each before the call), and the chapters'
// drafts, their bodies, reviews, revisions, summaries, the characters, the
// project core and the chapter-deletion receipts are all still there afterwards.
//
// Calling it twice is the same success, because clearAll has no receipt and no
// return value (blueprint-repository.ts:806-811): the second call deletes nothing.
// TestProductionDBMaintenanceBlueprintClearRefusesWhileARunIsOpen is the guard this
// lane adds to the reference's clearAll: the reference retires the recovery run inside
// the transaction (blueprint-repository.ts:613-619) and this lane has no port for that
// classifier, so a live directory run could put back every row the clear removed.
//
// The running row is seeded BEFORE the lane opens the project: the write permit pins
// the database's content identity at admit time, so a row inserted afterwards is a
// different refusal (AUTHOR_WRITE_STALE) and would test the permit instead of the guard.
func TestProductionDBMaintenanceBlueprintClearRefusesWhileARunIsOpen(t *testing.T) {
	root, projectPath, database, contract := productionDBMaintenanceRoot(t)
	productionDBMaintenanceExec(t, productionDBMaintenanceOpen(t, database, "mode=rw"), []struct {
		query string
		args  []any
	}{
		{`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json, revision)
 VALUES ('root-running-1', 'idem-running-1', '{}', '{}', 0)`, nil},
		{`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES ('run-running-1', 'root-running-1', '{"sourceManifest":{"operation":"chapter-blueprint-directory"}}',
 'running', 1700000000001, 'open-running-1')`, nil},
	})
	server := productionDBMaintenanceLane(t, root, contract)
	author, frozen := productionDBMaintenanceAdmit(t, server, projectPath)
	if count := productionDBMaintenanceCount(t, database, "blueprints"); count != 2 {
		t.Fatalf("the fixture carries %d blueprints, want 2", count)
	}
	before := productionDBMaintenanceDigest(t, database)
	if code := productionDBMaintenanceRefusal(t, author, productionDBMaintenanceBlueprintClearName,
		[]any{projectPath, frozen}); code != "BLUEPRINT_CLEAR_RUN_IN_FLIGHT" {
		t.Fatalf("the in-flight-run clear refused with %v, want BLUEPRINT_CLEAR_RUN_IN_FLIGHT", code)
	}
	productionDBMaintenanceUnchanged(t, database, before, "the in-flight-run blueprint clear")
	if count := productionDBMaintenanceCount(t, database, "blueprints"); count != 2 {
		t.Fatalf("the refused clear removed blueprints: %d left, want 2", count)
	}
}

func TestProductionDBMaintenanceBlueprintClearAllKeepsEveryDraft(t *testing.T) {
	root, projectPath, database, contract := productionDBMaintenanceRoot(t)
	server := productionDBMaintenanceLane(t, root, contract)
	author, frozen := productionDBMaintenanceAdmit(t, server, projectPath)

	core := productionDBMaintenanceCore(t, database)
	drafts := productionDBMaintenanceCount(t, database, "drafts")
	if productionDBMaintenanceCount(t, database, "blueprints") != 2 ||
		productionDBMaintenanceCount(t, database, "blueprint_commit_operations") != 2 ||
		productionDBMaintenanceCount(t, database, "blueprint_character_sync_operations") != 2 {
		t.Fatal("fixture does not carry two rows in each blueprint table")
	}

	before := productionDBMaintenanceDigest(t, database)
	response := author.call(t, productionDBMaintenanceBlueprintClearName, []any{projectPath, frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionDBMaintenanceBlueprintClearName, response.Code, response.Body.String())
	}
	var cleared struct {
		Success bool `json:"success"`
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, response), &cleared); err != nil || !cleared.Success {
		t.Fatalf("blueprint clear did not answer success: %v %s", err, response.Body.String())
	}
	// The clearing half: all three tables are empty.
	for _, table := range []string{"blueprints", "blueprint_commit_operations", "blueprint_character_sync_operations"} {
		if count := productionDBMaintenanceCount(t, database, table); count != 0 {
			t.Fatalf("%s kept %d rows after clear-all", table, count)
		}
	}
	// The preservation half: every draft, its body and every other table survive.
	if count := productionDBMaintenanceCount(t, database, "drafts"); count != drafts {
		t.Fatalf("clear-all removed drafts: %d left, want %d", count, drafts)
	}
	if value := productionDBMaintenanceText(t, database, `SELECT body FROM contents WHERE id=1`); value != productionDBMaintenanceBody1 {
		t.Fatalf("clear-all changed draft 1 body to %v", value)
	}
	if value := productionDBMaintenanceText(t, database, `SELECT body FROM contents WHERE id=2`); value != productionDBMaintenanceBody2 {
		t.Fatalf("clear-all changed draft 2 body to %v", value)
	}
	for _, table := range []string{"reviews", "revisions", "summary_snapshots", "post_process_runs", "characters", "character_aliases", "chapter_deletion_operations"} {
		if count := productionDBMaintenanceCount(t, database, table); count != 2 {
			t.Fatalf("clear-all touched %s: %d rows left, want 2", table, count)
		}
	}
	if after := productionDBMaintenanceCore(t, database); after != core {
		t.Fatal("clear-all rewrote the project core")
	}
	if before == productionDBMaintenanceDigest(t, database) {
		t.Fatal("the successful clear did not change the database file at all")
	}

	// Idempotence: the same call succeeds again and deletes nothing.
	again := author.call(t, productionDBMaintenanceBlueprintClearName, []any{projectPath, frozen})
	if again.Code != http.StatusOK {
		t.Fatalf("second clear-all: %d %s", again.Code, again.Body.String())
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, again), &cleared); err != nil || !cleared.Success {
		t.Fatalf("second clear-all did not answer success: %v %s", err, again.Body.String())
	}
	if count := productionDBMaintenanceCount(t, database, "drafts"); count != drafts {
		t.Fatalf("second clear-all removed drafts: %d left, want %d", count, drafts)
	}

	// Refusals: a foreign project and a lease this lane never issued. Both must
	// leave the whole file byte-identical.
	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	for _, envelope := range []struct {
		label string
		args  []any
	}{
		{"foreign project", []any{foreign, map[string]any{
			"projectId": frozen["projectId"], "projectPath": foreign, "leaseId": frozen["leaseId"]}}},
		{"stale lease", []any{projectPath, map[string]any{
			"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": productionDBMaintenanceStaleLease}}},
	} {
		snapshot := productionDBMaintenanceDigest(t, database)
		code := productionDBMaintenanceRefusal(t, author, productionDBMaintenanceBlueprintClearName, envelope.args)
		productionDBMaintenanceLeaseRefusal(t, code, envelope.label)
		productionDBMaintenanceUnchanged(t, database, snapshot, "the refused blueprint clear")
	}

	// Wrong arity is refused by the argument fence, and the file is untouched.
	aritySnapshot := productionDBMaintenanceDigest(t, database)
	response = author.call(t, productionDBMaintenanceBlueprintClearName, []any{projectPath})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("arity refusal answered %d, want 400: %s", response.Code, response.Body.String())
	}
	productionDBMaintenanceUnchanged(t, database, aritySnapshot, "the arity-refused blueprint clear")
}

// TestProductionDBMaintenanceProjectClearKeepsAuthorContent proves
// db:project-clear-generated-data over real HTTP through the frozen session. The
// generatedText scope empties the eight tables project-clear-repository.ts:102-109
// names, and the author's own content - the whole project_core row, both
// characters with their aliases, both blueprints (that scope was not requested)
// and both chapter-deletion receipts - survives untouched.
//
// Calling it twice is the same success with the same cleared list: the deletes have
// no receipt and the second call finds nothing to remove, so
// `return { cleared, physicalFilesDeleted }` (project-clear-repository.ts:165)
// answers the same scopes and reports zero physical files both times.
func TestProductionDBMaintenanceProjectClearKeepsAuthorContent(t *testing.T) {
	root, projectPath, database, contract := productionDBMaintenanceRoot(t)
	server := productionDBMaintenanceLane(t, root, contract)
	author, frozen := productionDBMaintenanceAdmit(t, server, projectPath)

	core := productionDBMaintenanceCore(t, database)
	characters := productionDBMaintenanceCount(t, database, "characters")
	if characters != 2 {
		t.Fatalf("fixture carries %d characters, want 2", characters)
	}
	for _, table := range []string{"finalized_draft_import_operations", "post_process_steps",
		"post_process_runs", "reviews", "revisions", "drafts", "contents", "summary_snapshots"} {
		if productionDBMaintenanceCount(t, database, table) < 2 {
			t.Fatalf("fixture does not carry two rows in %s", table)
		}
	}

	request := map[string]any{"generatedText": true, "blueprints": false, "creativeFields": false}
	before := productionDBMaintenanceDigest(t, database)
	response := author.call(t, productionDBMaintenanceProjectClearName, []any{request, projectPath, frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionDBMaintenanceProjectClearName, response.Code, response.Body.String())
	}
	var cleared struct {
		Success              bool     `json:"success"`
		Cleared              []string `json:"cleared"`
		PhysicalFilesDeleted int64    `json:"physicalFilesDeleted"`
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, response), &cleared); err != nil {
		t.Fatalf("decode clear result: %v %s", err, response.Body.String())
	}
	if !cleared.Success || len(cleared.Cleared) != 1 || cleared.Cleared[0] != "generatedText" {
		t.Fatalf("clear answered %+v, want the generatedText scope only", cleared)
	}
	if cleared.PhysicalFilesDeleted != 0 {
		t.Fatalf("clear claimed %d physical files deleted; this lane moves none", cleared.PhysicalFilesDeleted)
	}
	// The clearing half: every generated-text table is empty.
	for _, table := range []string{"finalized_draft_import_operations", "post_process_steps",
		"post_process_runs", "reviews", "revisions", "drafts", "contents", "summary_snapshots"} {
		if count := productionDBMaintenanceCount(t, database, table); count != 0 {
			t.Fatalf("%s kept %d rows after the generated-text clear", table, count)
		}
	}
	// The preservation half: the project core, the characters the author wrote and
	// the tables the scope never names are untouched.
	if after := productionDBMaintenanceCore(t, database); after != core {
		t.Fatal("the generated-text clear rewrote the project core")
	}
	if count := productionDBMaintenanceCount(t, database, "characters"); count != characters {
		t.Fatalf("the clear removed characters: %d left, want %d", count, characters)
	}
	for _, preserved := range []struct {
		table string
		want  int64
	}{
		{"character_aliases", 2}, {"blueprints", 2}, {"blueprint_commit_operations", 2},
		{"blueprint_character_sync_operations", 2}, {"chapter_deletion_operations", 2},
		{"character_roster_meta", 1}, {"import_global_fact_operations", 0},
	} {
		if count := productionDBMaintenanceCount(t, database, preserved.table); count != preserved.want {
			t.Fatalf("%s holds %d rows after the clear, want %d", preserved.table, count, preserved.want)
		}
	}
	if before == productionDBMaintenanceDigest(t, database) {
		t.Fatal("the successful clear did not change the database file at all")
	}

	// Idempotence: the same scopes, the same success, nothing left to delete.
	again := author.call(t, productionDBMaintenanceProjectClearName, []any{request, projectPath, frozen})
	if again.Code != http.StatusOK {
		t.Fatalf("second generated-text clear: %d %s", again.Code, again.Body.String())
	}
	var repeated struct {
		Success              bool     `json:"success"`
		Cleared              []string `json:"cleared"`
		PhysicalFilesDeleted int64    `json:"physicalFilesDeleted"`
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, again), &repeated); err != nil {
		t.Fatalf("decode second clear: %v %s", err, again.Body.String())
	}
	if !repeated.Success || len(repeated.Cleared) != 1 || repeated.Cleared[0] != "generatedText" ||
		repeated.PhysicalFilesDeleted != 0 {
		t.Fatalf("second clear answered %+v, want the same single scope", repeated)
	}
	if after := productionDBMaintenanceCore(t, database); after != core {
		t.Fatal("the second clear rewrote the project core")
	}

	// Refusals: a foreign project, a stale lease, and an options argument that is
	// not an object. Every one of them leaves the file byte-identical.
	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	for _, envelope := range []struct {
		label string
		args  []any
	}{
		{"foreign project", []any{request, foreign, map[string]any{
			"projectId": frozen["projectId"], "projectPath": foreign, "leaseId": frozen["leaseId"]}}},
		{"stale lease", []any{request, projectPath, map[string]any{
			"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": productionDBMaintenanceStaleLease}}},
	} {
		snapshot := productionDBMaintenanceDigest(t, database)
		code := productionDBMaintenanceRefusal(t, author, productionDBMaintenanceProjectClearName, envelope.args)
		productionDBMaintenanceLeaseRefusal(t, code, envelope.label)
		productionDBMaintenanceUnchanged(t, database, snapshot, "the refused generated-text clear")
	}
	snapshot := productionDBMaintenanceDigest(t, database)
	response = author.call(t, productionDBMaintenanceProjectClearName, []any{"generatedText", projectPath, frozen})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("options refusal answered %d, want 400: %s", response.Code, response.Body.String())
	}
	productionDBMaintenanceUnchanged(t, database, snapshot, "the options-refused generated-text clear")
}

// TestProductionDBMaintenanceImportGlobalFactsCommitIsOneTransaction proves
// db:import-global-facts-commit over real HTTP through the frozen session. One
// commit writes the ledger row, the fifteen imported core fields and one staged
// character proposal, and leaves every character, alias, draft and blueprint the
// author already had exactly where it was. The second identical call is the same
// success with `idempotent: true` (import-global-facts-repository.ts:178), and a
// changed payload under the same operation id is a named refusal (:175) that
// leaves the file byte-identical.
func TestProductionDBMaintenanceImportGlobalFactsCommitIsOneTransaction(t *testing.T) {
	root, projectPath, database, contract := productionDBMaintenanceRoot(t)
	server := productionDBMaintenanceLane(t, root, contract)
	author, frozen := productionDBMaintenanceAdmit(t, server, projectPath)

	core := productionDBMaintenanceCore(t, database)
	characters := productionDBMaintenanceCount(t, database, "characters")
	drafts := productionDBMaintenanceCount(t, database, "drafts")

	request := productionDBMaintenanceImportRequest(productionDBMaintenanceImportOperation, 0, "wuxia")
	response := author.call(t, productionDBMaintenanceImportFactsName, []any{request, projectPath, frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionDBMaintenanceImportFactsName, response.Code, response.Body.String())
	}
	var committed struct {
		Success bool `json:"success"`
		Receipt struct {
			OperationID       string         `json:"operationId"`
			PayloadHash       string         `json:"payloadHash"`
			Idempotent        bool           `json:"idempotent"`
			Core              map[string]any `json:"core"`
			CharacterProposal struct {
				ProposalBatchID string `json:"proposalBatchId"`
				SourceHash      string `json:"sourceHash"`
			} `json:"characterProposal"`
			ProposalSource struct {
				OperationID            string `json:"operationId"`
				ExpectedRosterRevision int64  `json:"expectedRosterRevision"`
			} `json:"proposalSource"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, response), &committed); err != nil {
		t.Fatalf("decode commit receipt: %v %s", err, response.Body.String())
	}
	if !committed.Success {
		t.Fatal("the import commit did not answer success")
	}
	if committed.Receipt.OperationID != productionDBMaintenanceImportOperation ||
		committed.Receipt.ProposalSource.OperationID != productionDBMaintenanceImportOperation {
		t.Fatalf("the receipt names %v / %v", committed.Receipt.OperationID, committed.Receipt.ProposalSource.OperationID)
	}
	if committed.Receipt.Idempotent {
		t.Fatal("the first commit reported itself as a replay")
	}
	if len(committed.Receipt.PayloadHash) != 64 ||
		!strings.HasPrefix(committed.Receipt.CharacterProposal.ProposalBatchID, "cpb:") ||
		len(committed.Receipt.CharacterProposal.SourceHash) != 64 {
		t.Fatalf("the receipt hashes are not the reference's shape: %+v", committed.Receipt)
	}
	if committed.Receipt.ProposalSource.ExpectedRosterRevision != 0 {
		t.Fatalf("the receipt lost the roster revision: %+v", committed.Receipt.ProposalSource)
	}
	if value, _ := committed.Receipt.Core["genre"].(string); value != "wuxia" {
		t.Fatalf("the receipt core genre = %v, want wuxia", committed.Receipt.Core["genre"])
	}

	// The written half: the fifteen imported columns, one ledger row and one
	// staged proposal.
	if value := productionDBMaintenanceText(t, database, `SELECT genre FROM project_core WHERE id='main'`); value != "wuxia" {
		t.Fatalf("project core genre = %v, want wuxia", value)
	}
	if value := productionDBMaintenanceText(t, database, `SELECT synopsis FROM project_core WHERE id='main'`); value != "导入总大纲" {
		t.Fatalf("project core synopsis = %v", value)
	}
	if count := productionDBMaintenanceCount(t, database, "import_global_fact_operations"); count != 1 {
		t.Fatalf("the ledger holds %d rows, want 1", count)
	}
	if count := productionDBMaintenanceCount(t, database, "character_identity_proposals"); count != 1 {
		t.Fatalf("the proposal ledger holds %d rows, want 1", count)
	}
	if key := productionDBMaintenanceText(t, database,
		`SELECT source_key FROM character_identity_proposals WHERE proposal_id=?`,
		committed.Receipt.CharacterProposal.ProposalBatchID); key != "character-proposal-v1:"+productionDBMaintenanceProjectID {
		t.Fatalf("the staged proposal source key = %v", key)
	}

	// The preserved half: the author's characters, their aliases, the drafts and
	// the blueprints are untouched, and so are the core columns the import does
	// not own (writing_style, character_states, characters_arch).
	if count := productionDBMaintenanceCount(t, database, "characters"); count != characters {
		t.Fatalf("the import changed the character count: %d, want %d", count, characters)
	}
	if count := productionDBMaintenanceCount(t, database, "character_aliases"); count != 2 {
		t.Fatalf("the import changed the alias count to %d", count)
	}
	if count := productionDBMaintenanceCount(t, database, "drafts"); count != drafts {
		t.Fatalf("the import changed the draft count to %d", count)
	}
	if count := productionDBMaintenanceCount(t, database, "blueprints"); count != 2 {
		t.Fatalf("the import changed the blueprint count to %d", count)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT writing_style||'|'||character_states||'|'||characters_arch FROM project_core WHERE id='main'`); value != "作者文风|{\"甲\":\"状态\"}|作者人物群像" {
		t.Fatalf("the import touched columns it does not own: %v", value)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT name FROM characters WHERE character_id='character-1'`); value != productionDBMaintenanceCharacter1 {
		t.Fatalf("the import changed a character name to %v", value)
	}
	if after := productionDBMaintenanceCore(t, database); after == core {
		t.Fatal("the successful import did not change the project core")
	}

	// Idempotence: the same operation and payload is a replay, not a refusal, and
	// it writes nothing new.
	before := productionDBMaintenanceDigest(t, database)
	replay := author.call(t, productionDBMaintenanceImportFactsName, []any{request, projectPath, frozen})
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	var replayed struct {
		Success bool `json:"success"`
		Receipt struct {
			Idempotent        bool   `json:"idempotent"`
			PayloadHash       string `json:"payloadHash"`
			CharacterProposal struct {
				ProposalBatchID string `json:"proposalBatchId"`
			} `json:"characterProposal"`
		} `json:"receipt"`
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, replay), &replayed); err != nil {
		t.Fatalf("decode replay: %v %s", err, replay.Body.String())
	}
	if !replayed.Success || !replayed.Receipt.Idempotent {
		t.Fatalf("the replay answered %+v, want idempotent success", replayed)
	}
	if replayed.Receipt.PayloadHash != committed.Receipt.PayloadHash ||
		replayed.Receipt.CharacterProposal.ProposalBatchID != committed.Receipt.CharacterProposal.ProposalBatchID {
		t.Fatalf("the replay invented a different receipt: %+v", replayed.Receipt)
	}
	if count := productionDBMaintenanceCount(t, database, "import_global_fact_operations"); count != 1 {
		t.Fatalf("the replay wrote %d ledger rows, want 1", count)
	}
	if count := productionDBMaintenanceCount(t, database, "character_identity_proposals"); count != 1 {
		t.Fatalf("the replay wrote %d proposal rows, want 1", count)
	}
	if after := productionDBMaintenanceDigest(t, database); after != before {
		t.Fatal("the replayed import rewrote the database file")
	}

	// A changed payload under the same operation id is the reference's conflict.
	conflict := productionDBMaintenanceImportRequest(productionDBMaintenanceImportOperation, 0, "xianxia")
	for _, envelope := range []struct {
		label    string
		request  map[string]any
		context  map[string]any
		accepted []string
	}{
		{"operation conflict", conflict, frozen, []string{"IMPORT_GLOBAL_FACTS_OPERATION_CONFLICT"}},
		{"stale roster revision", productionDBMaintenanceImportRequest("novel-import-global-run-0002", 7, "wuxia"),
			frozen, []string{"CHARACTER_ROSTER_REVISION_STALE"}},
	} {
		snapshot := productionDBMaintenanceDigest(t, database)
		code := productionDBMaintenanceRefusal(t, author, productionDBMaintenanceImportFactsName,
			[]any{envelope.request, projectPath, envelope.context})
		if len(envelope.accepted) > 0 && code != envelope.accepted[0] {
			t.Fatalf("%s refused with %v, want %v", envelope.label, code, envelope.accepted[0])
		}
		productionDBMaintenanceUnchanged(t, database, snapshot, "the refused import commit")
	}

	// A foreign project and a stale lease are refused by the borrow before any
	// SQL runs, and both leave the file byte-identical.
	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	for _, envelope := range []struct {
		label string
		path  string
		ctx   map[string]any
	}{
		{"foreign project", foreign, map[string]any{
			"projectId": frozen["projectId"], "projectPath": foreign, "leaseId": frozen["leaseId"]}},
		{"stale lease", projectPath, map[string]any{
			"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": productionDBMaintenanceStaleLease}},
	} {
		snapshot := productionDBMaintenanceDigest(t, database)
		code := productionDBMaintenanceRefusal(t, author, productionDBMaintenanceImportFactsName,
			[]any{productionDBMaintenanceImportRequest("novel-import-global-run-0003", 0, "wuxia"), envelope.path, envelope.ctx})
		productionDBMaintenanceLeaseRefusal(t, code, envelope.label)
		productionDBMaintenanceUnchanged(t, database, snapshot, "the refused import borrow")
	}
}

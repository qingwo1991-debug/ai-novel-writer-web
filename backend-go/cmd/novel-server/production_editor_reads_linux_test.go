//go:build linux

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

const (
	productionEditorProjectName = "editor-reads-novel"
	productionEditorProjectID   = "3ce1f7a2-5d84-4b6f-a0c9-7e2b1d4f6a85"
	productionEditorTab         = "client_1234567890"
	productionEditorOtherTab    = "client_0987654321"
	productionEditorNovelName   = "编辑阅读小说"
	productionEditorSeed        = "第一章原稿：他推开门。"
	productionEditorCharacter   = "甲"
	productionEditorChapter     = "第一章 门"
)

// productionEditorReadsRoot builds a disposable production data root whose only
// admitted project is a real original-schema v7 database (the shipped baseline)
// carrying synthetic rows: one core row, one content, one working draft, one
// character card and one chapter blueprint. Those are exactly the rows the
// editor's read surface has to hand back, so a 200 that returns an empty list, a
// default shape or a fabricated stage cannot pass.
func productionEditorReadsRoot(t *testing.T) (root, projectPath string, contract admission.SchemaContract, seeded []byte) {
	t.Helper()
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("the original v7 schema oracle is required for the production editor read journey")
		}
		t.Skip("GitHub original schema oracle required")
	}
	file, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	contract, err = admission.ParseContract(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join(filepath.Dir(oracle), "baseline-v7.db"))
	if err != nil {
		t.Fatal(err)
	}
	root = syntheticProductionDataDir(t)
	projectPath = filepath.Join(root, productionLibraryDirectory, productionEditorProjectName)
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"` + productionEditorProjectID +
		`","createdAt":"2024-05-06T07:08:09.000Z","storageFormat":"ai-novel","storageVersion":1}`
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(storage, projectfs.DatabaseName)
	if err := os.WriteFile(database, baseline, 0600); err != nil {
		t.Fatal(err)
	}
	seedProductionEditorRows(t, database)
	if seeded, err = os.ReadFile(database); err != nil {
		t.Fatal(err)
	}
	return root, projectPath, contract, seeded
}

// seedProductionEditorRows writes the synthetic author rows into the copied
// baseline through the same column sets the admitted runtime selects. The
// character row keeps its schema defaults (retired=0, identity_revision=0), so
// the identity invariant the admission verifier enforces still holds.
func seedProductionEditorRows(t *testing.T, database string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		"DELETE FROM finalization_outbox",
		"DELETE FROM drafts",
		"DELETE FROM contents",
		"DELETE FROM project_core",
		"DELETE FROM blueprints",
		"DELETE FROM characters",
		`INSERT INTO project_core (id, project_name, writing_language, genre, total_chapters,
 words_per_chapter, creative_strategy, narrative_thread_dormant_threshold, character_states,
 created_at, updated_at)
 VALUES ('main', '` + productionEditorNovelName + `', 'zh-CN', 'fantasy', 12, 3000, 'auto', 3, '{}',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO contents (id, body) VALUES (1, '` + productionEditorSeed + `')`,
		`INSERT INTO drafts (id, chapter_number, version, status, source, content_id, word_count,
 source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'draft', 'write', 1, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO characters (character_id, name, role, retired, created_at, updated_at)
 VALUES ('ci-character-1', '` + productionEditorCharacter + `', 'protagonist', 0,
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO blueprints (chapter_number, title, role, purpose, key_events, characters,
 suspense_hook, user_guidance, notes, notes_updated_at, created_at, updated_at)
 VALUES (1, '` + productionEditorChapter + `', 'setup', '开篇', '["他推开门"]', '["` + productionEditorCharacter + `"]',
 '风声', '', '', '', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	// The lane admits the file itself, so the seeded write must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	var backfill, logFrames, checkpointed int64
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&backfill, &logFrames, &checkpointed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		_ = db.Close()
		t.Fatal(err)
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

// productionEditorCall performs one operation and returns its result payload. A
// 501 is named explicitly: every operation this test claims would fail silently
// there, because the binder installs nothing and startup never complains.
func productionEditorCall(t *testing.T, client *productionDurableClient, operation string, args []any) json.RawMessage {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented {
		t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return wire.Result
}

func productionEditorDecode(t *testing.T, client *productionDurableClient, operation string, args []any, into any) {
	t.Helper()
	if err := json.Unmarshal(productionEditorCall(t, client, operation, args), into); err != nil {
		t.Fatalf("%s: %v", operation, err)
	}
}

// productionEditorRefused asserts one refusal without pinning a single code: a
// stale or absent lease, a foreign tab and a closed session are all honest
// refusals, and none of them may answer 200 or return data.
func productionEditorRefused(t *testing.T, client *productionDurableClient, label, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s: %s answered 200 under a refused lease: %s", label, operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %s: %v %s", label, operation, err, response.Body.String())
	}
	switch body.Error.Code {
	case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_NOT_OPEN",
		"AUTHOR_WRITE_STALE", "PROJECT_SESSION_REQUIRED", "PROJECT_SESSION_CONTEXT_REQUIRED":
	default:
		t.Fatalf("%s: %s refused with %q, which is not a lease refusal: %s", label, operation, body.Error.Code, response.Body.String())
	}
}

// TestProductionEditorReadsServeAdmittedAuthorDatabase is the read half of this
// lane: every editor read that can be answered from the author's own database is
// bound, answers 200 with the seeded rows, and refuses a stale, foreign or
// absent lease without touching the admitted bytes.
func TestProductionEditorReadsServeAdmittedAuthorDatabase(t *testing.T) {
	root, projectPath, contract, seeded := productionEditorReadsRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "editor-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	if opened.Project["id"] != productionEditorProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}

	// Characters: all three reads come back from the author database.
	var characters []map[string]any
	productionEditorDecode(t, author, "db:character-get-all", []any{projectPath, frozen}, &characters)
	if len(characters) != 1 || characters[0]["name"] != productionEditorCharacter ||
		characters[0]["characterId"] != "ci-character-1" || characters[0]["role"] != "protagonist" {
		t.Fatalf("db:character-get-all did not read the seeded character: %+v", characters)
	}
	var roster map[string]any
	productionEditorDecode(t, author, "db:character-roster-read", []any{projectPath, frozen}, &roster)
	entries, ok := roster["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Fatalf("db:character-roster-read returned no roster entry: %+v", roster)
	}
	if entry, ok := entries[0].(map[string]any); !ok || entry["name"] != productionEditorCharacter {
		t.Fatalf("db:character-roster-read read the wrong character: %+v", entries[0])
	}
	if _, present := roster["identityRevision"]; !present {
		t.Fatalf("db:character-roster-read returned no identity revision: %+v", roster)
	}
	var identity map[string]any
	productionEditorDecode(t, author, "character-identity:read", []any{frozen}, &identity)
	identities, ok := identity["characters"].([]any)
	if !ok || len(identities) != 1 {
		t.Fatalf("character-identity:read returned no identity: %+v", identity)
	}
	if fields, ok := identities[0].(map[string]any)["fields"].(map[string]any); !ok || fields["name"] != productionEditorCharacter {
		t.Fatalf("character-identity:read read the wrong character: %+v", identities[0])
	}

	// Stage reads: the project core and the whole blueprint list the editor's
	// chapter and volume navigation is built from.
	var core map[string]any
	productionEditorDecode(t, author, "db:project-core-get", []any{projectPath, frozen}, &core)
	if core["projectName"] != productionEditorNovelName || core["totalChapters"] != float64(12) ||
		core["wordsPerChapter"] != float64(3000) {
		t.Fatalf("db:project-core-get did not read the seeded core row: %+v", core)
	}
	var blueprints []map[string]any
	productionEditorDecode(t, author, "db:blueprint-get-all", []any{projectPath, frozen}, &blueprints)
	if len(blueprints) != 1 || blueprints[0]["title"] != productionEditorChapter ||
		blueprints[0]["chapterNumber"] != float64(1) {
		t.Fatalf("db:blueprint-get-all did not read the seeded blueprint: %+v", blueprints)
	}
	var blueprint map[string]any
	productionEditorDecode(t, author, "db:blueprint-get", []any{int64(1), projectPath, frozen}, &blueprint)
	if blueprint["title"] != productionEditorChapter || blueprint["role"] != "setup" {
		t.Fatalf("db:blueprint-get did not read the seeded blueprint: %+v", blueprint)
	}

	// The overview aggregate is computed under the live lease from rows that
	// really exist, not from a default project shape.
	var overview map[string]any
	productionEditorDecode(t, author, "project:overview-current", []any{frozen}, &overview)
	if overview["state"] != "ready" || overview["name"] != productionEditorNovelName ||
		overview["characters"] != float64(1) || overview["plannedChapters"] != float64(12) {
		t.Fatalf("project:overview-current did not aggregate the admitted project: %+v", overview)
	}
	if stages, ok := overview["stages"].([]any); !ok || len(stages) != 6 {
		t.Fatalf("project:overview-current returned the wrong stage list: %+v", overview["stages"])
	}
	if overview["excerpt"] != productionEditorSeed {
		t.Fatalf("project:overview-current returned the wrong excerpt: %+v", overview["excerpt"])
	}

	// The reads above are reads: the admitted bytes did not move.
	afterReads, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterReads, seeded) {
		t.Fatal("an editor read changed the admitted author database")
	}

	// The two character-sync blueprint reads stay fail-closed: their store
	// implementation backfills with INSERT OR IGNORE, which is a write.
	for _, operation := range []string{"db:blueprint-character-sync-get", "db:blueprint-character-sync-list-pending", "fs:list-dir"} {
		if response := author.call(t, operation, []any{int64(1), projectPath, frozen}); response.Code != http.StatusNotImplemented {
			t.Fatalf("%s is bound in the production lane but must stay fail-closed: %d %s", operation, response.Code, response.Body.String())
		}
	}

	// Refusals: a foreign tab, a stale lease and an absent lease are all
	// refused by the same fence, for every newly bound operation.
	reads := []struct {
		operation string
		args      func(map[string]any) []any
	}{
		{"db:character-get-all", func(m map[string]any) []any { return []any{projectPath, m} }},
		{"db:character-roster-read", func(m map[string]any) []any { return []any{projectPath, m} }},
		{"character-identity:read", func(m map[string]any) []any { return []any{m} }},
		{"db:project-core-get", func(m map[string]any) []any { return []any{projectPath, m} }},
		{"db:blueprint-get-all", func(m map[string]any) []any { return []any{projectPath, m} }},
		{"db:blueprint-get", func(m map[string]any) []any { return []any{int64(1), projectPath, m} }},
		{"project:overview-current", func(m map[string]any) []any { return []any{m} }},
	}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
	absent := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath}
	foreign := newProductionDurableClient(t, handler, productionEditorOtherTab)
	for _, read := range reads {
		productionEditorRefused(t, foreign, "foreign tab", read.operation, read.args(frozen))
		productionEditorRefused(t, author, "stale lease", read.operation, read.args(stale))
		productionEditorRefused(t, author, "absent lease", read.operation, read.args(absent))
	}

	// Refusals are refusals: the admitted bytes still did not move.
	afterRefusals, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRefusals, seeded) {
		t.Fatal("a refused editor read changed the admitted author database")
	}
}

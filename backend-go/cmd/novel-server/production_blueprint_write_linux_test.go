//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// The production write surface below runs against a disposable t.TempDir() only.
// No real author data root is read or written: the writer lease, the project
// database and every readback live inside the test's own temporary tree.
const (
	productionWriteProjectName = "write-novel"
	productionWriteProjectID   = "9b2c16d4-73e8-4af5-b1d0-3c6e8f9a2b47"
	productionWriteTab         = "client_writes0001"
	productionWriteCharacterID = "ci-write-character-1"
	productionWriteSeedName    = "甲"
	productionWriteNextName    = "乙"
	productionWriteRangeID     = "production-write-range-1"
	productionWriteStaleLease  = "stale-write-lease-0001"
)

// productionWriteSchema is the smallest author schema the seven bound writes
// really touch: the project core row and its CAS source columns, the blueprint
// table plus the two operation ledgers commit-range writes and backfills, and the
// identity / roster tables the author roster pipeline reads and writes.
//
// Every statement is written exactly as it must appear in sqlite_master, because
// the pinned contract hashes that text; productionWriteContract builds the
// fingerprint from these same strings instead of trusting a hand-written digest.
// The tables that make admission run the desktop character-identity invariant
// (character_identity_meta) are seeded consistently with it: revision 0, no
// legacy origins, no character above the meta revision.
var productionWriteSchema = []string{
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, premise TEXT, worldbuilding TEXT, synopsis TEXT, characters_arch TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE blueprints (chapter_number INTEGER PRIMARY KEY, title TEXT, role TEXT, purpose TEXT, key_events TEXT, characters TEXT, suspense_hook TEXT, user_guidance TEXT, notes TEXT, notes_updated_at TEXT, created_at TEXT DEFAULT '', updated_at TEXT DEFAULT '')",
	"CREATE TABLE blueprint_commit_operations (operation_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL, mode TEXT NOT NULL, start_chapter INTEGER NOT NULL, end_chapter INTEGER NOT NULL, character_sync_input TEXT NOT NULL)",
	"CREATE TABLE blueprint_character_sync_operations (operation_id TEXT PRIMARY KEY, blueprint_commit_operation_id TEXT NOT NULL, blueprint_commit_payload_hash TEXT NOT NULL, start_chapter INTEGER NOT NULL, end_chapter INTEGER NOT NULL, character_sync_input TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'pending', completion_receipt TEXT, created_at TEXT DEFAULT 'created', updated_at TEXT DEFAULT 'updated', completed_at TEXT)",
	"CREATE TABLE characters (character_id TEXT PRIMARY KEY, legacy_key TEXT, name TEXT, role TEXT DEFAULT 'supporting', gender TEXT DEFAULT '', age TEXT DEFAULT '', appearance TEXT DEFAULT '', personality TEXT DEFAULT '', background TEXT DEFAULT '', abilities TEXT DEFAULT '', motivation TEXT DEFAULT '', relationships TEXT DEFAULT '', arc TEXT DEFAULT '', notes TEXT DEFAULT '', cs_location TEXT DEFAULT '', cs_power_level TEXT DEFAULT '', cs_physical_state TEXT DEFAULT '', cs_mental_state TEXT DEFAULT '', cs_key_items TEXT DEFAULT '', cs_recent_events TEXT DEFAULT '', cs_updated_at_chapter INTEGER DEFAULT NULL, cs_provenance TEXT NOT NULL DEFAULT '{}', static_provenance TEXT NOT NULL DEFAULT '{}', identity_revision INTEGER NOT NULL DEFAULT 0, retired INTEGER NOT NULL DEFAULT 0, created_at TEXT DEFAULT 'created', updated_at TEXT DEFAULT 'updated')",
	"CREATE TABLE character_identity_meta (id TEXT PRIMARY KEY, revision INTEGER NOT NULL, legacy_count INTEGER NOT NULL)",
	"CREATE TABLE character_aliases (character_id TEXT, name TEXT, source_key TEXT, valid_from INTEGER, valid_through INTEGER, PRIMARY KEY (character_id, name, source_key, valid_from))",
	"CREATE TABLE character_relationships (relationship_id TEXT PRIMARY KEY, source_character_id TEXT, target_character_id TEXT, relation TEXT, source_display_snapshot TEXT, target_display_snapshot TEXT, provenance_json TEXT, approval_id TEXT)",
	"CREATE TABLE character_identity_proposals (proposal_id TEXT PRIMARY KEY, owner_character_id TEXT, source_key TEXT, source_hash TEXT, raw_value TEXT, candidate_ids_json TEXT, resolved_character_id TEXT, approval_id TEXT)",
	"CREATE TABLE character_identity_approvals (operation_id TEXT PRIMARY KEY, payload_hash TEXT, receipt_json TEXT)",
	"CREATE TABLE character_identity_origins (character_id TEXT PRIMARY KEY, source_key TEXT, original_row_json TEXT, original_hash TEXT)",
	"CREATE TABLE character_roster_meta (id TEXT PRIMARY KEY, schema_version INTEGER, revision INTEGER, migration_state TEXT, legacy_markdown TEXT, projection_hash TEXT, fact_hash TEXT, updated_at TEXT DEFAULT 'unchanged')",
}

// productionWriteContract is the trusted contract for productionWriteSchema. The
// admission verifier hashes sqlite_master in (type, name) order, so the entries
// are sorted the same way instead of in DDL order.
func productionWriteContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionWriteSchema))
	for _, statement := range productionWriteSchema {
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
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: []string{sha256Hex(string(encoded))}}}
}

// productionWriteSeedRows writes the author rows the subtests start from: one
// project core row carrying a non-NULL baseline for every column the synopsis CAS
// compares, three blueprint chapters, and one character whose roster revision
// matches character_identity_meta.
func productionWriteSeedRows(t *testing.T, projectPath string) {
	t.Helper()
	statements := []string{
		`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, premise, worldbuilding, synopsis, characters_arch,
 created_at, updated_at)
 VALUES ('main', '写入验证小说', 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '', '', '', '',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)`,
		`INSERT INTO character_roster_meta (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'empty', '', '', '')`,
		`INSERT INTO characters (character_id, legacy_key, name, role, gender, age, appearance,
 personality, background, abilities, motivation, relationships, arc, notes, cs_provenance,
 static_provenance, identity_revision, retired)
 VALUES ('` + productionWriteCharacterID + `', NULL, '` + productionWriteSeedName + `', 'protagonist',
 '', '', '', '', '', '', '', '', '', '', '{}', '{}', 0, 0)`,
		`INSERT INTO blueprints (chapter_number, title, role, purpose, key_events, characters,
 suspense_hook, user_guidance, notes, notes_updated_at, created_at, updated_at)
 VALUES (1, '原始第一章', 'setup', '开篇', '["推开门"]', '["甲"]', '风声', '', '作者摘要', '',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO blueprints (chapter_number, title, role, purpose, key_events, characters,
 suspense_hook, user_guidance, notes, notes_updated_at, created_at, updated_at)
 VALUES (2, '原始第二章', 'development', '推进', '["第二件事"]', '["甲"]', '', '', '', '',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
		`INSERT INTO blueprints (chapter_number, title, role, purpose, key_events, characters,
 suspense_hook, user_guidance, notes, notes_updated_at, created_at, updated_at)
 VALUES (3, '原始第三章', 'climax', '高潮', '["第三件事"]', '["乙"]', '', '', '', '',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`,
	}
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	// The seeded roster is normalized by the store's OWN projection refresh, so the
	// fixture starts from the self-consistent "ready" state a real author project
	// has instead of hand-computed roster hashes. It runs over the same file and
	// before admission, exactly like the seed above.
	if err := store.NewCharacters(db).RefreshIdentityProjection(context.Background()); err != nil {
		_ = db.Close()
		t.Fatalf("normalize the seeded roster: %v", err)
	}
	// The lane admits the file itself, so the seeded write must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}

// productionWriteFixture is one opened production lane with one opened project.
type productionWriteFixture struct {
	t           *testing.T
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
	// rosterRevision is the roster revision the seeded project opens with, and
	// baseline is the frozen synopsis CAS source read out of the project core row.
	rosterRevision int64
	baseline       map[string]any
}

// newProductionWriteFixture builds the temporary data root, opens the real
// production handler over it and opens the one admitted project for one browser
// tab. Opening the project is what issues the lease every write below names.
func newProductionWriteFixture(t *testing.T) *productionWriteFixture {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionWriteProjectName, productionWriteProjectID,
		admission.CurrentSchemaVersion, productionWriteSchema)
	projectPath := filepath.Join(library, productionWriteProjectName)
	productionWriteSeedRows(t, projectPath)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	rosterRevision := productionWriteRosterRevision(t, database)
	baseline := productionWriteBaseline(t, database)
	// The address is the origin allowlist productionDurableClient.call sends; the
	// handler is served in-process by httptest, so no listener is opened.
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionWriteContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	author := newProductionDurableClient(t, handler, productionWriteTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "open-token-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted write project: %+v", opened)
	}
	if opened.Project["id"] != productionWriteProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	if lease, _ := opened.Project["sessionLease"].(string); lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return &productionWriteFixture{
		t:              t,
		projectPath:    projectPath,
		database:       database,
		author:         author,
		rosterRevision: rosterRevision,
		baseline:       baseline,
		frozen: map[string]any{
			"projectId":   opened.Project["id"],
			"projectPath": opened.Project["path"],
			"leaseId":     opened.Project["sessionLease"],
		},
	}
}

// productionWriteBaseline is the frozen synopsis CAS source, read back out of the
// seeded project core row so the test never guesses what the eleven-baseline
// comparison expects: the target body plus every source column, non-NULL.
func productionWriteBaseline(t *testing.T, database string) map[string]any {
	t.Helper()
	rows := productionWriteQuery(t, database, `SELECT synopsis, premise, characters_arch, worldbuilding,
 genre, total_chapters, words_per_chapter, writing_language, plot_structure, narrative_pov,
 global_guidance FROM project_core WHERE id = 'main'`)
	if len(rows) != 1 {
		t.Fatalf("the seeded project core row is missing: %v", rows)
	}
	row := rows[0]
	for _, column := range []string{"total_chapters", "words_per_chapter"} {
		if row[column] == nil {
			t.Fatalf("the seeded project core %s is NULL, so the CAS could never match", column)
		}
	}
	return map[string]any{
		"synopsis":        productionWriteText(row, "synopsis"),
		"premise":         productionWriteText(row, "premise"),
		"charactersArch":  productionWriteText(row, "characters_arch"),
		"worldbuilding":   productionWriteText(row, "worldbuilding"),
		"genre":           productionWriteText(row, "genre"),
		"totalChapters":   row["total_chapters"],
		"wordsPerChapter": row["words_per_chapter"],
		"writingLanguage": productionWriteText(row, "writing_language"),
		"plotStructure":   productionWriteText(row, "plot_structure"),
		"narrativePov":    productionWriteText(row, "narrative_pov"),
		"globalGuidance":  productionWriteText(row, "global_guidance"),
	}
}

// productionWriteRosterRevision is the roster revision the seeded project opens
// with, which is also what the roster commit has to name as its expected revision.
func productionWriteRosterRevision(t *testing.T, database string) int64 {
	t.Helper()
	rows := productionWriteQuery(t, database, `SELECT revision FROM character_roster_meta WHERE id = 'main'`)
	if len(rows) != 1 {
		t.Fatalf("the seeded roster meta row is missing: %v", rows)
	}
	revision := productionWriteNumber(rows[0], "revision")
	if revision < 0 {
		t.Fatalf("the seeded roster revision is not an integer: %v", rows[0])
	}
	return revision
}

// staleContext is the same frozen session with a lease this lane never issued.
func (f *productionWriteFixture) staleContext() map[string]any {
	return map[string]any{
		"projectId":   f.frozen["projectId"],
		"projectPath": f.frozen["projectPath"],
		"leaseId":     productionWriteStaleLease,
	}
}

// result sends one operation over the real HTTP handler and returns its decoded
// result. A non-200 is fatal: every write on this lane answers as a business
// result, so a transport error would mean the binder was never reached.
func (f *productionWriteFixture) result(operation string, args []any) any {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code != http.StatusOK {
		f.t.Fatalf("%s: %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	var decoded any
	if err := json.Unmarshal(wire.Result, &decoded); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s echoed the project path: %s", operation, response.Body.String())
	}
	return decoded
}

// business sends one write and requires the DTO shape every durable write binder
// uses: {success, error, ...}. A refusal is therefore assertable instead of fatal.
func (f *productionWriteFixture) business(operation string, args []any) map[string]any {
	f.t.Helper()
	result, ok := f.result(operation, args).(map[string]any)
	if !ok {
		f.t.Fatalf("%s did not answer with a business result", operation)
	}
	return result
}

func productionWriteAssertSuccess(t *testing.T, operation string, result map[string]any) {
	t.Helper()
	if result["success"] != true || result["error"] != nil {
		t.Fatalf("%s was not reported as a commit: %v", operation, result)
	}
}

func productionWriteAssertRefusal(t *testing.T, operation string, result map[string]any, code string) {
	t.Helper()
	if result["success"] != false {
		t.Fatalf("%s was not reported as a refusal: %v", operation, result)
	}
	if result["error"] != code {
		t.Fatalf("%s refused with %v; want %q", operation, result["error"], code)
	}
}

// productionWriteQuery reads through a SEPARATE read-only connection on the
// author's own database file, so a commit is only "persisted" when the file the
// lane wrote says so and not when a handler echoed its own input.
func productionWriteQuery(t *testing.T, database, query string, args ...any) []map[string]any {
	t.Helper()
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
		t.Fatalf("read %q: %v", query, err)
	}
	defer func() { _ = rows.Close() }()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	out := []map[string]any{}
	for rows.Next() {
		values := make([]any, len(columns))
		pointers := make([]any, len(columns))
		for index := range pointers {
			pointers[index] = &values[index]
		}
		if err := rows.Scan(pointers...); err != nil {
			t.Fatalf("read %q: %v", query, err)
		}
		row := make(map[string]any, len(columns))
		for index, name := range columns {
			row[name] = values[index]
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %q: %v", query, err)
	}
	return out
}

func productionWriteText(row map[string]any, column string) string {
	text, ok := row[column].(string)
	if !ok {
		return ""
	}
	return text
}

func productionWriteNumber(row map[string]any, column string) int64 {
	number, ok := row[column].(int64)
	if !ok {
		return -1
	}
	return number
}

// productionWriteSnapshot is the exact byte image of the admitted database file.
// A refused write must leave this image identical: no partial row, no journal.
func productionWriteSnapshot(t *testing.T, database string) []byte {
	t.Helper()
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func productionWriteAssertUnchanged(t *testing.T, database string, before []byte, why string) {
	t.Helper()
	if after := productionWriteSnapshot(t, database); !bytes.Equal(before, after) {
		t.Fatalf("%s changed the author database file", why)
	}
}

// productionWriteBlueprint is the full author input one committed blueprint row
// carries. commit-range re-reads what it wrote, so the input has to be complete.
func productionWriteBlueprint(chapter int, title string) map[string]any {
	return map[string]any{
		"chapterNumber": chapter, "title": title, "role": "main", "purpose": "范围目的",
		"keyEvents": "范围事件", "characters": []any{"甲"}, "suspenseHook": "范围悬念",
		"userGuidance": "", "notes": "", "notesUpdatedAt": "",
	}
}

// productionWriteRosterRequest is the author's roster edit of the one seeded
// character: the same eleven static fields the store compares entry by entry, all
// but the renamed one unchanged. Only the operation ID and the expected revision
// vary, so a replay and a stale snapshot are the same request twice.
func productionWriteRosterRequest(operationID string, expectedRevision int64) map[string]any {
	return map[string]any{
		"intent": "manual_edit", "schemaVersion": 1,
		"operationId":              operationID,
		"expectedRevision":         expectedRevision,
		"expectedIdentityRevision": 0,
		"entries": []any{map[string]any{
			"characterId": productionWriteCharacterID, "name": productionWriteNextName,
			"role": "protagonist", "gender": "", "age": "", "appearance": "", "personality": "",
			"background": "", "abilities": "", "motivation": "", "arc": "", "notes": "",
			"relationships": []any{},
		}},
	}
}

// TestProductionLaneServesEditorWriteSurface binds the seven write names whose
// stores and transport binders already exist, and proves each one against the
// author's own database: a write is committed, read back through a separate
// connection, and a refused write leaves the file byte-identical.
func TestProductionLaneServesEditorWriteSurface(t *testing.T) {
	t.Run("db:blueprint-upsert", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		written := fixture.business("db:blueprint-upsert", []any{map[string]any{
			"chapterNumber": 4, "title": "单章写入标题", "role": "main", "purpose": "单章目的",
			"keyEvents": "单章事件", "characters": []any{"甲"}, "suspenseHook": "单章悬念",
			"userGuidance": "", "notes": "单章备注", "notesUpdatedAt": "",
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:blueprint-upsert", written)
		rows := productionWriteQuery(t, fixture.database,
			`SELECT chapter_number, title, role, notes FROM blueprints ORDER BY chapter_number`)
		if len(rows) != 4 {
			t.Fatalf("db:blueprint-upsert left %d rows; want the three seeded chapters plus one", len(rows))
		}
		if productionWriteText(rows[3], "title") != "单章写入标题" || productionWriteText(rows[3], "notes") != "单章备注" {
			t.Fatalf("db:blueprint-upsert did not persist the row: %v", rows[3])
		}
		// The upsert names one chapter, so the seeded chapters keep their content.
		for index, title := range []string{"原始第一章", "原始第二章", "原始第三章"} {
			if productionWriteText(rows[index], "title") != title {
				t.Fatalf("db:blueprint-upsert changed an unnamed chapter: %v", rows[index])
			}
		}
	})

	t.Run("db:blueprint-upsert-many", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		fifth := productionWriteBlueprint(5, "批量第五章")
		sixth := productionWriteBlueprint(6, "批量第六章")
		sixth["role"] = "supporting"
		written := fixture.business("db:blueprint-upsert-many", []any{[]any{fifth, sixth},
			fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:blueprint-upsert-many", written)
		rows := productionWriteQuery(t, fixture.database,
			`SELECT chapter_number, title, role FROM blueprints WHERE chapter_number IN (5, 6) ORDER BY chapter_number`)
		if len(rows) != 2 || productionWriteText(rows[0], "title") != "批量第五章" ||
			productionWriteText(rows[1], "title") != "批量第六章" || productionWriteText(rows[1], "role") != "supporting" {
			t.Fatalf("db:blueprint-upsert-many did not persist both rows: %v", rows)
		}
	})

	t.Run("db:blueprint-commit-range", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		written := fixture.business("db:blueprint-commit-range", []any{map[string]any{
			"mode": "replace-range", "operationId": productionWriteRangeID,
			"startChapter": 2, "endChapter": 3,
			"blueprints": []any{
				productionWriteBlueprint(2, "范围提交第二章"),
				productionWriteBlueprint(3, "范围提交第三章"),
			},
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:blueprint-commit-range", written)
		receipt, _ := written["receipt"].(map[string]any)
		if chapters, _ := receipt["chapterNumbers"].([]any); len(chapters) != 2 ||
			chapters[0] != float64(2) || chapters[1] != float64(3) {
			t.Fatalf("db:blueprint-commit-range returned the wrong range: %v", written["receipt"])
		}
		// Every row the range named changed, and the row outside it did not.
		rows := productionWriteQuery(t, fixture.database,
			`SELECT chapter_number, title, purpose FROM blueprints ORDER BY chapter_number`)
		if len(rows) != 3 {
			t.Fatalf("db:blueprint-commit-range changed the row count: %v", rows)
		}
		if productionWriteText(rows[1], "title") != "范围提交第二章" || productionWriteText(rows[2], "title") != "范围提交第三章" {
			t.Fatalf("db:blueprint-commit-range did not rewrite every row in the range: %v", rows)
		}
		if productionWriteText(rows[0], "title") != "原始第一章" {
			t.Fatalf("db:blueprint-commit-range rewrote a chapter outside the range: %v", rows[0])
		}
		// The commit is one transaction over the rows and both ledgers.
		operations := productionWriteQuery(t, fixture.database,
			`SELECT operation_id, mode, start_chapter, end_chapter FROM blueprint_commit_operations`)
		if len(operations) != 1 || productionWriteText(operations[0], "operation_id") != productionWriteRangeID ||
			productionWriteNumber(operations[0], "start_chapter") != 2 || productionWriteNumber(operations[0], "end_chapter") != 3 {
			t.Fatalf("db:blueprint-commit-range did not record its operation: %v", operations)
		}
		syncs := productionWriteQuery(t, fixture.database,
			`SELECT operation_id, status FROM blueprint_character_sync_operations`)
		if len(syncs) != 1 || productionWriteText(syncs[0], "operation_id") != "blueprint-sync-"+productionWriteRangeID ||
			productionWriteText(syncs[0], "status") != "pending" {
			t.Fatalf("db:blueprint-commit-range did not record the pending character sync: %v", syncs)
		}
	})

	t.Run("db:blueprint-delete", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		written := fixture.business("db:blueprint-delete", []any{2, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:blueprint-delete", written)
		rows := productionWriteQuery(t, fixture.database,
			`SELECT chapter_number, title FROM blueprints ORDER BY chapter_number`)
		if len(rows) != 2 {
			t.Fatalf("db:blueprint-delete left %d rows; want two", len(rows))
		}
		for _, row := range rows {
			if productionWriteNumber(row, "chapter_number") == 2 {
				t.Fatalf("db:blueprint-delete did not remove the named chapter: %v", rows)
			}
		}
		if productionWriteText(rows[0], "title") != "原始第一章" || productionWriteText(rows[1], "title") != "原始第三章" {
			t.Fatalf("db:blueprint-delete changed an unnamed chapter: %v", rows)
		}
	})

	// db:blueprint-update-notes is NOT bound, and that is the correct wire: the
	// original refuses it unconditionally because derived finalization notes are
	// committed with their artifact and ACK in one main transaction, which this
	// lane does not own. The refusal therefore has to stay fail-closed instead of
	// becoming a handler that could only ever refuse.
	t.Run("db:blueprint-update-notes-stays-unbound", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		before := productionWriteSnapshot(t, fixture.database)
		response := fixture.author.call(t, "db:blueprint-update-notes",
			[]any{1, "不得写入的备注", fixture.projectPath, fixture.frozen})
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("db:blueprint-update-notes was bound on the production lane: %d %s",
				response.Code, response.Body.String())
		}
		rows := productionWriteQuery(t, fixture.database, `SELECT notes FROM blueprints WHERE chapter_number = 1`)
		if len(rows) != 1 || productionWriteText(rows[0], "notes") != "作者摘要" {
			t.Fatalf("the refused db:blueprint-update-notes changed the stored notes: %v", rows)
		}
		productionWriteAssertUnchanged(t, fixture.database, before, "the refused db:blueprint-update-notes")
	})

	t.Run("db:project-core-update", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		written := fixture.business("db:project-core-update", []any{map[string]any{
			"premise": "合成前提", "worldbuilding": "合成世界观", "totalChapters": 130,
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:project-core-update", written)
		rows := productionWriteQuery(t, fixture.database,
			`SELECT premise, worldbuilding, total_chapters, genre FROM project_core WHERE id = 'main'`)
		if len(rows) != 1 || productionWriteText(rows[0], "premise") != "合成前提" ||
			productionWriteText(rows[0], "worldbuilding") != "合成世界观" ||
			productionWriteNumber(rows[0], "total_chapters") != 130 {
			t.Fatalf("db:project-core-update did not persist the patch: %v", rows)
		}
		if productionWriteText(rows[0], "genre") != "fantasy" {
			t.Fatalf("db:project-core-update changed a field it did not name: %v", rows[0])
		}
	})

	t.Run("db:project-core-synopsis-commit", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		const synopsis = "合成梗概：一次提交"
		written := fixture.business("db:project-core-synopsis-commit", []any{map[string]any{
			"synopsis": synopsis, "expected": fixture.baseline,
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:project-core-synopsis-commit", written)
		rows := productionWriteQuery(t, fixture.database, `SELECT synopsis, premise FROM project_core WHERE id = 'main'`)
		if len(rows) != 1 || productionWriteText(rows[0], "synopsis") != synopsis {
			t.Fatalf("db:project-core-synopsis-commit did not persist the body: %v", rows)
		}
	})

	t.Run("db:character-roster-commit", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		// The request is the author's own roster edit: the revision pair the roster
		// read holds, one entry naming the stored character, and the eleven static
		// fields the original compares entry by entry.
		written := fixture.business("db:character-roster-commit", []any{
			productionWriteRosterRequest("production-write-roster-1", fixture.rosterRevision),
			fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:character-roster-commit", written)
		receipt, _ := written["receipt"].(map[string]any)
		if receipt == nil || receipt["revision"] != float64(fixture.rosterRevision+1) || receipt["idempotent"] != false {
			t.Fatalf("db:character-roster-commit receipt malformed: %v", written)
		}
		characters := productionWriteQuery(t, fixture.database,
			`SELECT character_id, name, identity_revision, retired FROM characters`)
		if len(characters) != 1 || productionWriteText(characters[0], "name") != productionWriteNextName ||
			productionWriteNumber(characters[0], "identity_revision") != 1 ||
			productionWriteNumber(characters[0], "retired") != 0 {
			t.Fatalf("db:character-roster-commit did not persist the edited identity: %v", characters)
		}
		aliases := productionWriteQuery(t, fixture.database,
			`SELECT name, source_key, valid_from, valid_through FROM character_aliases`)
		if len(aliases) != 1 || productionWriteText(aliases[0], "name") != productionWriteNextName ||
			productionWriteNumber(aliases[0], "valid_from") != 1 || aliases[0]["valid_through"] != nil {
			t.Fatalf("db:character-roster-commit did not open the renamed alias: %v", aliases)
		}
		identities := productionWriteQuery(t, fixture.database, `SELECT revision FROM character_identity_meta WHERE id = 'main'`)
		if len(identities) != 1 || productionWriteNumber(identities[0], "revision") != 1 {
			t.Fatalf("db:character-roster-commit did not bump the identity revision: %v", identities)
		}
		metas := productionWriteQuery(t, fixture.database,
			`SELECT revision, migration_state FROM character_roster_meta WHERE id = 'main'`)
		if len(metas) != 1 || productionWriteNumber(metas[0], "revision") != fixture.rosterRevision+1 ||
			productionWriteText(metas[0], "migration_state") != "ready" {
			t.Fatalf("db:character-roster-commit did not refresh the roster meta: %v", metas)
		}
		cores := productionWriteQuery(t, fixture.database, `SELECT characters_arch FROM project_core WHERE id = 'main'`)
		if len(cores) != 1 || !strings.Contains(productionWriteText(cores[0], "characters_arch"), productionWriteNextName) {
			t.Fatalf("db:character-roster-commit did not refresh the projection: %v", cores)
		}
		approvals := productionWriteQuery(t, fixture.database,
			`SELECT operation_id FROM character_identity_approvals`)
		if len(approvals) != 1 || productionWriteText(approvals[0], "operation_id") !=
			"author-roster-effect:production-write-roster-1" {
			t.Fatalf("db:character-roster-commit did not record its approval: %v", approvals)
		}
		// The commit's own nonce is idempotent: the same operation ID and payload
		// answers the stored receipt and writes nothing a second time.
		settled := productionWriteSnapshot(t, fixture.database)
		replayed := fixture.business("db:character-roster-commit", []any{productionWriteRosterRequest(
			"production-write-roster-1", fixture.rosterRevision), fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:character-roster-commit", replayed)
		if receipt, _ := replayed["receipt"].(map[string]any); receipt == nil ||
			receipt["idempotent"] != true || receipt["revision"] != float64(fixture.rosterRevision+1) {
			t.Fatalf("the roster replay was not answered from the stored receipt: %v", replayed)
		}
		productionWriteAssertUnchanged(t, fixture.database, settled, "the replayed roster commit")
		// A fresh operation ID that names the revision this commit replaced is a
		// stale snapshot, refused with the store's own code before it writes.
		stale := fixture.business("db:character-roster-commit", []any{productionWriteRosterRequest(
			"production-write-roster-stale", fixture.rosterRevision), fixture.projectPath, fixture.frozen})
		productionWriteAssertRefusal(t, "db:character-roster-commit", stale, "Error: CHARACTER_ID_REVISION_CONFLICT")
		productionWriteAssertUnchanged(t, fixture.database, settled, "the stale roster revision")
	})

	// Every one of the seven bound names has to refuse a lease this lane never
	// issued, and none of those refusals may touch a byte of the author database.
	t.Run("stale-write-lease-refuses-every-name", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		before := productionWriteSnapshot(t, fixture.database)
		stale := fixture.staleContext()
		blueprint := productionWriteBlueprint(2, "不得写入的第二章")
		for _, refusal := range []struct {
			operation string
			code      string
			args      []any
		}{
			{"db:blueprint-upsert", "Error: BLUEPRINT_OPERATION_FAILED", []any{blueprint, fixture.projectPath, stale}},
			{"db:blueprint-upsert-many", "Error: BLUEPRINT_OPERATION_FAILED", []any{[]any{blueprint}, fixture.projectPath, stale}},
			{"db:blueprint-delete", "Error: BLUEPRINT_OPERATION_FAILED", []any{2, fixture.projectPath, stale}},
			{"db:blueprint-commit-range", "Error: BLUEPRINT_OPERATION_FAILED", []any{map[string]any{
				"mode": "replace-range", "operationId": "production-write-stale-range",
				"startChapter": 2, "endChapter": 2,
				"blueprints": []any{blueprint},
			}, fixture.projectPath, stale}},
			{"db:project-core-update", "Error: PROJECT_CORE_OPERATION_FAILED", []any{map[string]any{
				"premise": "不得写入的前提",
			}, fixture.projectPath, stale}},
			{"db:project-core-synopsis-commit", "Error: PROJECT_CORE_OPERATION_FAILED", []any{map[string]any{
				"synopsis": "不得写入的梗概", "expected": fixture.baseline,
			}, fixture.projectPath, stale}},
			{"db:character-roster-commit", "Error: PROJECT_SESSION_NOT_OWNED", []any{map[string]any{
				"intent": "manual_edit", "schemaVersion": 1,
				"operationId":              "production-write-stale-roster",
				"expectedRevision":         fixture.rosterRevision,
				"expectedIdentityRevision": 0,
				"entries":                  []any{},
			}, fixture.projectPath, stale}},
		} {
			result := fixture.business(refusal.operation, refusal.args)
			productionWriteAssertRefusal(t, refusal.operation, result, refusal.code)
		}
		productionWriteAssertUnchanged(t, fixture.database, before, "the stale-lease write batch")
		rows := productionWriteQuery(t, fixture.database,
			`SELECT chapter_number, title FROM blueprints ORDER BY chapter_number`)
		if len(rows) != 3 || productionWriteText(rows[1], "title") != "原始第二章" {
			t.Fatalf("a stale-lease write reached the blueprint rows: %v", rows)
		}
		cores := productionWriteQuery(t, fixture.database, `SELECT premise, synopsis FROM project_core WHERE id = 'main'`)
		if len(cores) != 1 || productionWriteText(cores[0], "premise") != "" || productionWriteText(cores[0], "synopsis") != "" {
			t.Fatalf("a stale-lease write reached the project core row: %v", cores)
		}
	})

	// The CAS semantics every one of these writes carries are the store's own:
	// a replayed blueprint operation with a different payload is a conflict, and a
	// synopsis commit whose baseline moved is a lost compare-and-set. Both are
	// reported with the store's code and leave the committed row untouched.
	t.Run("stale-revision-refuses-at-the-store-code", func(t *testing.T) {
		fixture := newProductionWriteFixture(t)
		committed := fixture.business("db:blueprint-commit-range", []any{map[string]any{
			"mode": "replace-range", "operationId": productionWriteRangeID,
			"startChapter": 2, "endChapter": 3,
			"blueprints": []any{
				productionWriteBlueprint(2, "范围提交第二章"),
				productionWriteBlueprint(3, "范围提交第三章"),
			},
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:blueprint-commit-range", committed)
		settled := productionWriteSnapshot(t, fixture.database)
		// The same operation ID with a different payload is not a replay: the
		// store refuses the nonce conflict instead of writing a second time.
		replayed := fixture.business("db:blueprint-commit-range", []any{map[string]any{
			"mode": "replace-range", "operationId": productionWriteRangeID,
			"startChapter": 2, "endChapter": 3,
			"blueprints": []any{
				productionWriteBlueprint(2, "冲突第二章"),
				productionWriteBlueprint(3, "冲突第三章"),
			},
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertRefusal(t, "db:blueprint-commit-range", replayed, "Error: BLUEPRINT_OPERATION_CONFLICT")
		productionWriteAssertUnchanged(t, fixture.database, settled, "the refused blueprint replay")
		rows := productionWriteQuery(t, fixture.database,
			`SELECT title FROM blueprints WHERE chapter_number = 2`)
		if len(rows) != 1 || productionWriteText(rows[0], "title") != "范围提交第二章" {
			t.Fatalf("the refused blueprint replay overwrote the committed row: %v", rows)
		}

		const synopsis = "合成梗概：一次提交"
		first := fixture.business("db:project-core-synopsis-commit", []any{map[string]any{
			"synopsis": synopsis, "expected": fixture.baseline,
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertSuccess(t, "db:project-core-synopsis-commit", first)
		settled = productionWriteSnapshot(t, fixture.database)
		// The baseline the first commit replaced is now stale: the CAS loses and
		// the loss is the store's own bare code, never a silent overwrite.
		lost := fixture.business("db:project-core-synopsis-commit", []any{map[string]any{
			"synopsis": "不得写入的梗概", "expected": fixture.baseline,
		}, fixture.projectPath, fixture.frozen})
		productionWriteAssertRefusal(t, "db:project-core-synopsis-commit", lost, "PROJECT_CORE_SYNOPSIS_CHANGED")
		productionWriteAssertUnchanged(t, fixture.database, settled, "the refused synopsis CAS")
		cores := productionWriteQuery(t, fixture.database, `SELECT synopsis FROM project_core WHERE id = 'main'`)
		if len(cores) != 1 || productionWriteText(cores[0], "synopsis") != synopsis {
			t.Fatalf("the refused synopsis CAS overwrote the committed body: %v", cores)
		}
	})
}

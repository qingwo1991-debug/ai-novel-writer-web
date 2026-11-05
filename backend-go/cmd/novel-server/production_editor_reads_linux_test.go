//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
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
	// The committed generation history seeded into the same project database: one
	// settled run frozen under a lease that the live session no longer holds, so
	// the read has to serve history instead of refusing a stale epoch.
	productionEditorGenerationEpoch     = "lease-0000000001-history"
	productionEditorGenerationOperation = "chapter-draft"
	productionEditorGenerationNonce     = "ci-generation-nonce-1"
	productionEditorGenerationText      = "草稿候选：门后有人。"
)

// productionEditorReadsRoot builds a disposable production data root whose only
// admitted project is a real original-schema v7 database (the shipped baseline)
// carrying synthetic rows: one core row, one content, one working draft, one
// character card and one chapter blueprint, plus one settled generation run in the
// project's own ledger. Those are exactly the rows the editor's read surface has
// to hand back, so a 200 that returns an empty list, a default shape or a
// fabricated stage cannot pass.
func productionEditorReadsRoot(t *testing.T) (root, projectPath string, contract admission.SchemaContract, seeded []byte, generation productionEditorGeneration) {
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
	generation = seedProductionGenerationRun(t, database)
	if seeded, err = os.ReadFile(database); err != nil {
		t.Fatal(err)
	}
	return root, projectPath, contract, seeded, generation
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
	productionEditorCheckpoint(t, db, database)
}

// productionEditorCheckpoint folds the write-ahead log into the admitted file and
// removes the sidecars, so the bytes the test reads are the bytes the lane admits.
func productionEditorCheckpoint(t *testing.T, db *sql.DB, database string) {
	t.Helper()
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

// productionEditorGeneration is the identity of the one settled run seeded into
// the admitted project database, so the assertions compare against what the ledger
// actually wrote instead of a hard-coded guess.
type productionEditorGeneration struct {
	RootActionID string
	RunID        string
	AttemptID    string
	Epoch        string
	Text         string
}

// productionEditorGenerationRequest is the frozen run binding the ledger opens: a
// real drafting binding with the nine hashes, an opaque source manifest and a
// budget, exactly the shape the shipped ledger accepts. It is written as JSON text
// because the ledger parses it with its own order-preserving decoder.
func productionEditorGenerationRequest() string {
	hash := strings.Repeat("a", 64)
	return `{"projectId":"` + productionEditorProjectID + `","epoch":"` + productionEditorGenerationEpoch +
		`","operation":"` + productionEditorGenerationOperation + `","uiActionNonce":"` + productionEditorGenerationNonce +
		`","frozenInputHash":"` + hash + `","fingerprint":{"chapterBriefHash":"` + hash + `","authorGuidanceHash":"` + hash +
		`","dependencyHash":"` + hash + `","contextSnapshotHash":"` + hash + `","templateHash":"` + hash +
		`","skillSnapshotHash":"` + hash + `","modelLeaseRevision":"` + hash + `","policyHash":"` + hash +
		`","outputContractHash":"` + hash + `"},"contextSnapshotId":"ci-context-snapshot-1","sourceManifest":{"operation":"` +
		productionEditorGenerationOperation + `"},"sourceRefs":[],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":4000,` +
		`"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}}`
}

// seedProductionGenerationRun writes one complete settlement chain through the real
// ledger - open, reserve, dispatch, snapshot, settle - so the generation reads
// project rows the shipped ledger wrote. A hand-written row set would prove only
// that the reader can read itself.
func seedProductionGenerationRun(t *testing.T, database string) productionEditorGeneration {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("production-ledger-%02d", ordinal)
	})
	ctx := context.Background()
	value, err := ledger.CallJSON(ctx, "open", json.RawMessage("["+productionEditorGenerationRequest()+"]"))
	if err != nil {
		t.Fatalf("seed generation open: %v", err)
	}
	run, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("seed generation open returned %T", value)
	}
	runID, _ := run["runId"].(string)
	rootID, _ := run["rootActionId"].(string)
	if runID == "" || rootID == "" {
		t.Fatalf("seed generation open returned no identity: %v", run)
	}
	reserved, err := ledger.Call(ctx, "reserve", []any{runID, productionEditorGenerationNonce, strings.Repeat("b", 64), 200, 100})
	if err != nil {
		t.Fatalf("seed generation reserve: %v", err)
	}
	receipt, _ := reserved.(map[string]any)
	attempt, _ := receipt["attempt"].(map[string]any)
	attemptID, _ := attempt["attemptId"].(string)
	if attemptID == "" {
		t.Fatalf("seed generation reserve returned no attempt: %v", reserved)
	}
	if _, err := ledger.Call(ctx, "markDispatched", []any{attemptID}); err != nil {
		t.Fatalf("seed generation markDispatched: %v", err)
	}
	if _, err := ledger.Call(ctx, "snapshot", []any{attemptID, 0, productionEditorGenerationText}); err != nil {
		t.Fatalf("seed generation snapshot: %v", err)
	}
	settled, err := ledger.Call(ctx, "settle", []any{attemptID, map[string]any{"trusted": true, "actualTokens": 120}, "stop"})
	if err != nil {
		t.Fatalf("seed generation settle: %v", err)
	}
	if settled == nil {
		t.Fatal("seed generation settle returned no receipt")
	}
	productionEditorCheckpoint(t, db, database)
	return productionEditorGeneration{
		RootActionID: rootID,
		RunID:        runID,
		AttemptID:    attemptID,
		Epoch:        productionEditorGenerationEpoch,
		Text:         productionEditorGenerationText,
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
	root, projectPath, contract, seeded, _ := productionEditorReadsRoot(t)
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

// TestProductionLaneServesCommittedGenerationAndModelReads is the generation
// family's read half in production. generation:list and generation:read project
// the author's own committed ledger rows - including a settled run frozen under a
// lease this session no longer holds - and the four llm reads project the pinned
// global generation the lane already admitted. Every execution name and the three
// config setters keep failing closed at 501, and no claimed read may answer 501:
// a silently empty diagnostics list would be worse than the refusal.
func TestProductionLaneServesCommittedGenerationAndModelReads(t *testing.T) {
	root, projectPath, contract, seeded, generation := productionEditorReadsRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "generation-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}

	// generation:list is the project's committed run list, not an empty shelf.
	var runs []map[string]any
	productionEditorDecode(t, author, "generation:list", []any{frozen}, &runs)
	if len(runs) != 1 {
		t.Fatalf("generation:list returned %d runs; want the one seeded run: %v", len(runs), runs)
	}
	handle, ok := runs[0]["handle"].(map[string]any)
	if !ok {
		t.Fatalf("generation:list returned no handle: %v", runs[0])
	}
	if handle["runId"] != generation.RunID || handle["rootActionId"] != generation.RootActionID ||
		handle["epoch"] != generation.Epoch || handle["projectId"] != productionEditorProjectID {
		t.Fatalf("generation:list returned the wrong run identity: %v", handle)
	}
	if runs[0]["status"] != "running" || runs[0]["operation"] != productionEditorGenerationOperation {
		t.Fatalf("generation:list returned the wrong summary: %v", runs[0])
	}

	// generation:read projects that run through the ledger's own receipts. The
	// settling attempt is the diagnostics evidence, so an empty list here is the
	// exact defect this assertion exists for.
	var view map[string]any
	productionEditorDecode(t, author, "generation:read", []any{handle, frozen}, &view)
	if view["status"] != "running" || view["operation"] != productionEditorGenerationOperation {
		t.Fatalf("generation:read returned the wrong run: %v", view)
	}
	diagnostics, ok := view["budgetDiagnostics"].([]any)
	if !ok || len(diagnostics) != 1 {
		t.Fatalf("generation:read returned %v budget diagnostics; want the settled attempt", view["budgetDiagnostics"])
	}
	if attempt, ok := diagnostics[0].(map[string]any); !ok || attempt["attemptId"] != generation.AttemptID {
		t.Fatalf("generation:read diagnosed the wrong attempt: %v", diagnostics[0])
	}
	if view["inFlight"] != false {
		t.Fatalf("a settled run reported an in-flight attempt: %v", view["inFlight"])
	}
	// The run is frozen under an older epoch: the original reports that as
	// non-replayable instead of refusing to serve history.
	if view["nonReplayable"] != true {
		t.Fatalf("a run frozen under an older epoch is not reported non-replayable: %v", view["nonReplayable"])
	}
	candidates, ok := view["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("generation:read lost the settled candidate: %v", view["candidates"])
	}
	if candidate, ok := candidates[0].(map[string]any); !ok || candidate["text"] != generation.Text {
		t.Fatalf("generation:read did not carry the committed draft text: %v", candidates[0])
	}

	// A handle that does not name this run is refused, not answered with a view.
	foreignHandle := map[string]any{}
	for key, value := range handle {
		foreignHandle[key] = value
	}
	foreignHandle["runId"] = "another-run"
	if response := author.call(t, "generation:read", []any{foreignHandle, frozen}); response.Code == http.StatusOK {
		t.Fatalf("generation:read answered a handle for another run: %s", response.Body.String())
	}

	// llm:list-models projects the committed private store with the credential
	// stripped; the three routing reads echo the committed assignments.
	var models []map[string]any
	productionEditorDecode(t, author, "llm:list-models", []any{}, &models)
	if len(models) != 2 || models[0]["id"] != productionTestModelID || models[0]["hasCredential"] != true ||
		models[0]["modelName"] != "ci-story-model" || models[1]["id"] != productionTestEmbeddingModelID {
		t.Fatalf("llm:list-models did not project the committed model store: %v", models)
	}
	if listing := author.call(t, "llm:list-models", []any{}); strings.Contains(listing.Body.String(), productionTestModelSecret) {
		t.Fatalf("llm:list-models leaked the stored credential: %s", listing.Body.String())
	}
	var stageModels map[string]any
	productionEditorDecode(t, author, "llm:get-stage-models", []any{}, &stageModels)
	if stageModels["drafting"] != productionTestModelID {
		t.Fatalf("llm:get-stage-models did not read the committed assignment: %v", stageModels)
	}
	var defaultModel any
	productionEditorDecode(t, author, "llm:get-default-model", []any{}, &defaultModel)
	if defaultModel != productionTestModelID {
		t.Fatalf("llm:get-default-model did not read the committed default: %v", defaultModel)
	}
	var defaultEmbedding any
	productionEditorDecode(t, author, "llm:get-default-embedding-model", []any{}, &defaultEmbedding)
	if defaultEmbedding != productionTestEmbeddingModelID {
		t.Fatalf("llm:get-default-embedding-model did not read the committed default: %v", defaultEmbedding)
	}

	// The reads did not move the admitted bytes: a ledger read that checkpointed
	// or repaired the author database would show up here.
	afterReads, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterReads, seeded) {
		t.Fatal("a generation read changed the admitted author database")
	}

	// The fence is the same one the editor reads use: a foreign tab, a stale lease
	// and an absent lease are refused for the newly bound generation reads too.
	reads := []struct {
		operation string
		args      func(map[string]any) []any
	}{
		{"generation:list", func(m map[string]any) []any { return []any{m} }},
		{"generation:read", func(m map[string]any) []any { return []any{handle, m} }},
	}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
	absent := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath}
	foreign := newProductionDurableClient(t, handler, productionEditorOtherTab)
	for _, read := range reads {
		productionEditorRefused(t, foreign, "foreign tab", read.operation, read.args(frozen))
		productionEditorRefused(t, author, "stale lease", read.operation, read.args(stale))
		productionEditorRefused(t, author, "absent lease", read.operation, read.args(absent))
	}

	// The rest of the execution family and the three config setters are not
	// installed here, so they keep answering the lane's fail-closed 501 instead of
	// a fake success. generation:begin and generation:execute are the two bound
	// names: the run admission is asserted by
	// TestProductionLaneAdmitsGenerationBeginUnderTheLiveLease and dispatch by
	// TestProductionLaneExecutesAdmittedRunThroughTheCommittedAllowlist.
	for _, operation := range []string{
		"generation:cancel",
		"llm:set-stage-model", "llm:set-default-model", "llm:set-default-embedding-model", "llm:save-model",
	} {
		if response := author.call(t, operation, []any{handle, frozen}); response.Code != http.StatusNotImplemented ||
			!strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s answered %d instead of the lane's fail-closed 501: %s", operation, response.Code, response.Body.String())
		}
	}

	// Refusals and fail-closed answers are still reads: the bytes did not move.
	afterRefusals, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRefusals, seeded) {
		t.Fatal("a refused generation read changed the admitted author database")
	}
}

// productionCommitGenerationPurpose rewrites the fixture's private model store
// before the lane pins it: the one story profile has to name the generation
// purpose the run admission requires, and nothing else about the committed
// document changes.
func productionCommitGenerationPurpose(t *testing.T, root, modelID string) {
	t.Helper()
	path := productionCommittedModelsPath(root)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var records []map[string]any
	if err := json.Unmarshal(raw, &records); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, record := range records {
		if id, _ := record["id"].(string); id == modelID {
			record["purposes"] = []any{"generation"}
			found = true
		}
	}
	if !found {
		t.Fatalf("the fixture commits no model profile %q", modelID)
	}
	encoded, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
}

// productionEditorBeginRequest is the reduced BeginGenerationRequest the run
// admission accepts: one real draft of the seeded project, the visible-text
// output contract and the canonical prompt keys.
func productionEditorBeginRequest(nonce string) map[string]any {
	return map[string]any{
		"operation":                       productionEditorGenerationOperation,
		"uiActionNonce":                   nonce,
		"modelId":                         productionTestModelID,
		"chapterNumber":                   1,
		"selectedDraftIds":                []any{1},
		"selectedFinalizedDraftIds":       []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":                      []any{"opening"},
		"skillStages":                     []any{},
		"authorInputs":                    []any{},
		"output":                          "visible-text",
	}
}

// productionEditorBeginRefusal asserts one mutating generation:begin answer
// without pinning a single code: every honest refusal - malformed argument, stale
// lease, missing committed config - is the business DTO with success false.
func productionEditorBeginRefusal(t *testing.T, label string, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", label, response.Code, response.Body.String())
	}
	var body struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", label, err, response.Body.String())
	}
	if body.Result.Success || !strings.HasPrefix(body.Result.Error, "Error: ") {
		t.Fatalf("%s: generation:begin reported a usable run: %s", label, response.Body.String())
	}
	return body.Result.Error
}

// TestProductionLaneAdmitsGenerationBeginUnderTheLiveLease is the run admission
// this lane owns. generation:begin must answer 200 over a real admitted project
// (not the fail-closed 501), write the ledger root budget row and the run row the
// reused S1 admission writes, hand back a run whose budget projection reads back
// through generation:read, refuse a stale lease, and leave the author database
// byte-identical after a malformed call. Opening a run dispatches nothing: the run
// it opens has no attempt until generation:execute is called, and dispatch itself
// is asserted by TestProductionLaneExecutesAdmittedRunThroughTheCommittedAllowlist.
func TestProductionLaneAdmitsGenerationBeginUnderTheLiveLease(t *testing.T) {
	root, projectPath, contract, _, generation := productionEditorReadsRoot(t)
	productionCommitGenerationPurpose(t, root, productionTestModelID)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "generation-begin-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}

	// The name is bound: it is not the lane's fail-closed 501, and the admission
	// really opens a run instead of refusing everything.
	response := author.call(t, "generation:begin", []any{productionEditorBeginRequest("production-begin-nonce-1"), frozen})
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:begin is still fail-closed in the production lane: %d %s", response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("generation:begin: HTTP %d %s", response.Code, response.Body.String())
	}
	var beginWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &beginWire); err != nil || beginWire.Result == nil {
		t.Fatalf("generation:begin envelope: %v %s", err, response.Body.String())
	}
	run := beginWire.Result
	if run["status"] != "running" || run["operation"] != productionEditorGenerationOperation {
		t.Fatalf("generation:begin returned the wrong run: %v", run)
	}
	if run["nonReplayable"] != false || run["inFlight"] != false {
		t.Fatalf("a freshly opened run is not replayable: %v", run)
	}
	handle, ok := run["handle"].(map[string]any)
	if !ok {
		t.Fatalf("generation:begin returned no handle: %v", run)
	}
	if handle["projectId"] != productionEditorProjectID || handle["epoch"] != frozen["leaseId"] ||
		handle["runId"] == "" || handle["rootActionId"] == "" {
		t.Fatalf("generation:begin returned the wrong run identity: %v", handle)
	}
	if handle["runId"] == generation.RunID {
		t.Fatalf("generation:begin reused the seeded run instead of opening one: %v", handle)
	}
	// Nothing was dispatched, so the run has no attempt to diagnose yet.
	if diagnostics, ok := run["budgetDiagnostics"].([]any); !ok || len(diagnostics) != 0 {
		t.Fatalf("generation:begin dispatched an attempt: %v", run["budgetDiagnostics"])
	}

	// The ledger rows the admission wrote are readable back through
	// generation:read: the root budget row first, then the run row. An empty
	// budget projection here is exactly the insert-order defect this asserts
	// against, so every bound it carries must come back populated.
	var view map[string]any
	productionEditorDecode(t, author, "generation:read", []any{handle, frozen}, &view)
	if view["status"] != "running" || view["operation"] != productionEditorGenerationOperation {
		t.Fatalf("generation:read returned the wrong run: %v", view)
	}
	if view["nonReplayable"] != false {
		t.Fatalf("generation:read reports the live run as non-replayable: %v", view)
	}
	budget, ok := view["budget"].(map[string]any)
	if !ok {
		t.Fatalf("generation:read returned no budget projection: %v", view)
	}
	if budget["maxAttempts"] != float64(syntheticMaxPhysicalRequests) {
		t.Fatalf("generation:read lost the ledger's attempt bound: %v", budget)
	}
	if tokenCap, ok := budget["maxRequestedOutputTokens"].(float64); !ok || tokenCap <= 0 {
		t.Fatalf("generation:read lost the ledger's token liability bound: %v", budget)
	}
	if deadline, ok := budget["deadlineAt"].(float64); !ok || deadline <= 0 {
		t.Fatalf("generation:read lost the active-elapsed deadline: %v", budget)
	}
	ledger, ok := view["ledger"].(map[string]any)
	if !ok {
		t.Fatalf("generation:read returned no ledger diagnostics: %v", view)
	}
	policy, ok := ledger["policy"].(map[string]any)
	if !ok {
		t.Fatalf("generation:read returned no committed budget policy: %v", ledger)
	}
	if policy["maxPhysicalRequests"] != float64(syntheticMaxPhysicalRequests) ||
		policy["maxActiveElapsedMs"] != float64(syntheticMaxActiveElapsedMs) {
		t.Fatalf("generation:read returned the wrong budget policy: %v", policy)
	}
	if outputCap, ok := policy["maxOutputPerRequest"].(float64); !ok || outputCap <= 0 {
		t.Fatalf("generation:read returned no per-request output cap: %v", policy)
	}
	// Opening a run reserved nothing: no attempt, no dispatch, no liability.
	if ledger["physicalRequests"] != float64(0) || ledger["tokenLiability"] != float64(0) {
		t.Fatalf("generation:begin reserved a request without dispatching: %v", ledger)
	}

	// A stale lease is refused under the same fence the read half uses, and it
	// must not open a second run.
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
	if code := productionEditorBeginRefusal(t, "stale lease",
		author.call(t, "generation:begin", []any{productionEditorBeginRequest("production-begin-nonce-2"), stale})); code == "" {
		t.Fatal("a stale-lease generation:begin was not refused")
	}

	// A malformed argument is refused before the admission touches the database:
	// the author database bytes are unchanged and the ledger gained no run.
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	before, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	malformed := productionEditorBeginRequest("production-begin-nonce-3")
	malformed["unknownAuthority"] = "renderer-supplied"
	code := productionEditorBeginRefusal(t, "malformed request",
		author.call(t, "generation:begin", []any{malformed, frozen}))
	if !strings.Contains(code, "GENERATION_BEGIN_INVALID") {
		t.Fatalf("a malformed generation:begin refused with %q instead of the admission's own code", code)
	}
	after, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a malformed generation:begin changed the admitted author database")
	}
	var runs []map[string]any
	productionEditorDecode(t, author, "generation:list", []any{frozen}, &runs)
	if len(runs) != 2 {
		t.Fatalf("generation:list returned %d runs; want the seeded run and the one admitted run: %v", len(runs), runs)
	}

	// generation:execute is bound in this lane, so this request is answered by the
	// lane's own fence instead of the fail-closed 501. The handle names a run this
	// ledger never admitted, so the fence refuses before any attempt row and before
	// any dispatch: this test never dials a provider, and the dispatch itself is
	// asserted by TestProductionLaneExecutesAdmittedRunThroughTheCommittedAllowlist
	// through the executor's injectable seam.
	unknown := map[string]any{
		"projectId": handle["projectId"], "epoch": frozen["leaseId"],
		"rootActionId": "root-never-issued", "runId": "run-never-issued",
	}
	unboundRequest := map[string]any{
		"handle":          unknown,
		"invocationNonce": "production-execute-unknown-nonce",
		"task": map[string]any{
			"purpose":  "chapter-draft",
			"output":   "visible-text",
			"messages": []any{map[string]any{"role": "user", "content": "续写第一章"}},
		},
	}
	if response := author.call(t, "generation:execute", []any{unboundRequest, frozen}); response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), `"success":false`) {
		t.Fatalf("generation:execute answered %d instead of the lane's business refusal: %s", response.Code, response.Body.String())
	}
}

// The review rows an earlier app version committed into the same admitted
// database: one review and one pending revision against the seeded draft, each
// with its frozen source snapshot, and deliberately no review_cycles row so the
// cycle read has to answer the honest "no cycle yet" null.
const (
	productionEditorReviewID     = 17
	productionEditorRevisionID   = 23
	productionEditorReviewBody   = "审查报告正文：门后的脚步声。"
	productionEditorRevisionBody = "候选修订正文：他听见门后有人。"
)

// productionReviewReadsRoot is the editor read fixture plus those committed
// review rows. The returned bytes are re-read after seeding, because the two
// tests above compare against the fixture exactly as the lane admits it.
func productionReviewReadsRoot(t *testing.T) (root, projectPath string, contract admission.SchemaContract, seeded []byte) {
	t.Helper()
	root, projectPath, contract, _, _ = productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	seedProductionReviewRows(t, database)
	var err error
	if seeded, err = os.ReadFile(database); err != nil {
		t.Fatal(err)
	}
	return root, projectPath, contract, seeded
}

// seedProductionReviewRows writes the review/revision rows through the same
// column sets the admitted read transaction selects, then checkpoints them into
// the admitted file so no sidecar carries rows the digest does not cover.
func seedProductionReviewRows(t *testing.T, database string) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		`DELETE FROM revisions`,
		`DELETE FROM reviews`,
		`INSERT INTO contents (id, body) VALUES (2, '` + productionEditorReviewBody + `')`,
		`INSERT INTO contents (id, body) VALUES (3, '` + productionEditorRevisionBody + `')`,
		fmt.Sprintf(`INSERT INTO reviews (id, base_draft_id, review_index, content_id, created_at,
 source_draft_chapter_number, source_draft_version, source_draft_status, source_content)
 VALUES (%d, 1, 1, 2, '2024-05-06 07:08:09', 1, 1, 'draft', '%s')`,
			productionEditorReviewID, productionEditorSeed),
		fmt.Sprintf(`INSERT INTO revisions (id, base_draft_id, revision_index, revision_type, status,
 merged_to_draft_id, user_prompt, review_source_id, content_id, word_count, created_at,
 updated_at, source_draft_chapter_number, source_draft_version, source_draft_status,
 source_content)
 VALUES (%d, 1, 1, 'refine', 'pending', NULL, '', %d, 3, 6, '2024-05-06 07:08:09',
 '2024-05-06 07:08:09', 1, 1, 'draft', '%s')`,
			productionEditorRevisionID, productionEditorReviewID, productionEditorSeed),
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	productionEditorCheckpoint(t, db, database)
}

// TestProductionEditorReviewReadsServeAdmittedAuthorDatabase binds the review
// half of the editor. Each of the six names must answer 200 over the admitted
// project with the committed rows - a metadata list, a full row with its frozen
// source, and the absent-cycle null - and each must refuse a stale, foreign or
// absent lease instead of reading anything. The review/revision mutations and
// the two reads that only have a write-capable seam stay at 501.
func TestProductionEditorReviewReadsServeAdmittedAuthorDatabase(t *testing.T) {
	root, projectPath, contract, seeded := productionReviewReadsRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "review-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}

	// The metadata lists are the committed rows for the seeded draft, not an
	// empty shelf and not another draft's history.
	var reviews []map[string]any
	productionEditorDecode(t, author, "db:review-list", []any{int64(1), projectPath, frozen}, &reviews)
	if len(reviews) != 1 || reviews[0]["id"] != float64(productionEditorReviewID) ||
		reviews[0]["baseDraftId"] != float64(1) || reviews[0]["reviewIndex"] != float64(1) ||
		reviews[0]["contentId"] != float64(2) {
		t.Fatalf("db:review-list did not read the seeded review: %+v", reviews)
	}
	var revisions []map[string]any
	productionEditorDecode(t, author, "db:revision-list", []any{int64(1), projectPath, frozen}, &revisions)
	if len(revisions) != 1 || revisions[0]["id"] != float64(productionEditorRevisionID) ||
		revisions[0]["revisionType"] != "refine" || revisions[0]["status"] != "pending" ||
		revisions[0]["reviewSourceId"] != float64(productionEditorReviewID) {
		t.Fatalf("db:revision-list did not read the seeded revision: %+v", revisions)
	}
	var pending []map[string]any
	productionEditorDecode(t, author, "db:revision-get-pending", []any{int64(1), projectPath, frozen}, &pending)
	if len(pending) != 1 || pending[0]["id"] != float64(productionEditorRevisionID) ||
		pending[0]["reviewSourceId"] != float64(productionEditorReviewID) {
		t.Fatalf("db:revision-get-pending lost the pending revision: %+v", pending)
	}

	// The full-row reads carry the committed body and the frozen source snapshot
	// the review was taken against, which a row read from the wrong table (or one
	// that skipped the contents join) could not return.
	var review map[string]any
	productionEditorDecode(t, author, "db:review-get-full", []any{int64(productionEditorReviewID), projectPath, frozen}, &review)
	if review["content"] != productionEditorReviewBody || review["baseDraftId"] != float64(1) {
		t.Fatalf("db:review-get-full did not read the committed review body: %+v", review)
	}
	source, ok := review["sourceDraft"].(map[string]any)
	if !ok || source["id"] != float64(1) || source["chapterNumber"] != float64(1) ||
		source["version"] != float64(1) || source["status"] != "draft" ||
		source["content"] != productionEditorSeed {
		t.Fatalf("db:review-get-full lost the frozen source snapshot: %+v", review["sourceDraft"])
	}
	var revision map[string]any
	productionEditorDecode(t, author, "db:revision-get-full", []any{int64(productionEditorRevisionID), projectPath, frozen}, &revision)
	if revision["content"] != productionEditorRevisionBody || revision["revisionType"] != "refine" ||
		revision["wordCount"] != float64(6) || revision["reviewSourceId"] != float64(productionEditorReviewID) {
		t.Fatalf("db:revision-get-full did not read the committed revision: %+v", revision)
	}
	source, ok = revision["sourceDraft"].(map[string]any)
	if !ok || source["id"] != float64(1) || source["content"] != productionEditorSeed {
		t.Fatalf("db:revision-get-full lost the frozen source snapshot: %+v", revision["sourceDraft"])
	}

	// The cycle read is bound and answers the honest null: the seeded project has
	// no review_cycles row for this review, and a non-null answer here would mean
	// the lane invented a cycle.
	var cycle any
	productionEditorDecode(t, author, "db:review-cycle-get", []any{int64(productionEditorReviewID), projectPath, frozen}, &cycle)
	if cycle != nil {
		t.Fatalf("db:review-cycle-get reported a cycle for a review that has none: %+v", cycle)
	}

	// The reads above are reads: the admitted bytes did not move.
	afterReads, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterReads, seeded) {
		t.Fatal("a review read changed the admitted author database")
	}

	// Every newly bound name is fenced exactly like the other editor reads.
	reads := []struct {
		operation string
		args      func(map[string]any) []any
	}{
		{"db:review-list", func(m map[string]any) []any { return []any{int64(1), projectPath, m} }},
		{"db:revision-list", func(m map[string]any) []any { return []any{int64(1), projectPath, m} }},
		{"db:revision-get-pending", func(m map[string]any) []any { return []any{int64(1), projectPath, m} }},
		{"db:review-get-full", func(m map[string]any) []any { return []any{int64(productionEditorReviewID), projectPath, m} }},
		{"db:revision-get-full", func(m map[string]any) []any { return []any{int64(productionEditorRevisionID), projectPath, m} }},
		{"db:review-cycle-get", func(m map[string]any) []any { return []any{int64(productionEditorReviewID), projectPath, m} }},
	}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
	absent := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath}
	foreign := newProductionDurableClient(t, handler, productionEditorOtherTab)
	for _, read := range reads {
		productionEditorRefused(t, foreign, "foreign tab", read.operation, read.args(frozen))
		productionEditorRefused(t, author, "stale lease", read.operation, read.args(stale))
		productionEditorRefused(t, author, "absent lease", read.operation, read.args(absent))
	}

	// The rest of the family keeps failing closed instead of being faked:
	// db:review-get-latest and db:review-next-index are reads, but this lane has
	// no read-only-transaction seam for them, and db:review-create is a write.
	for _, operation := range []string{"db:review-get-latest", "db:review-next-index", "db:review-create"} {
		response := author.call(t, operation, []any{int64(1), projectPath, frozen})
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s answered %d instead of the lane's fail-closed 501: %s", operation, response.Code, response.Body.String())
		}
	}

	// A refused read is also a read: the admitted bytes still did not move.
	afterRefusals, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterRefusals, seeded) {
		t.Fatal("a refused review read changed the admitted author database")
	}
}

//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the name production_project_core_generated_linux.go binds,
// db:project-core-commit-generated, over real HTTP through the frozen session.
//
// # Why the lane is assembled here
//
// The binder line belongs in production_open_linux.go, which this batch may not
// edit; installing the same binder on the same server type through the same
// durable bridge is what lets these tests run before that wiring lands (the same
// arrangement production_db_maintenance_linux_test.go:225-250 uses). The one
// addition is generation:begin: the reference's first proof is requireRun, and the
// live epoch assertSourcesCurrent re-proves is the lease the project was opened
// under, so the run under test has to be one this lane really opened rather than a
// row seeded with an epoch no session holds. A seeded run is still exercised below
// - as the GENERATION_EPOCH_STALE refusal it produces.
//
// # Why the fixture is the real baseline
//
// The project is productionDBMaintenanceRoot's: the admitted v7 baseline, seeded
// by that batch, whose project_core row already carries an author value in every
// column this write could touch. A hand-written schema cannot be used here at all:
// admission hashes sqlite_master AND proves the character-identity and ledger
// domains, so a reduced schema is refused with SCHEMA_DOMAIN_INVARIANT_FAILED
// before a handler runs (internal/admission/snapshot.go:165-169). This file only
// adds the two rows no handler in the lane can create: a decoy project_core row, a
// decoy generation run with its two attempts, and an agent-workflow child run.
//
// # What "the whole database file" means here
//
// productionDBMaintenanceDigest reads the main file and its WAL sidecar while a
// separate mode=ro connection holds the database, so every refusal below is
// compared over the bytes rather than over the rows a wrong statement might have
// left behind. A refused call must also leave every row it could have touched
// exactly as it was, which is why the decoy rows are read back after every arm: an
// UPDATE or a receipt write whose WHERE clause was wrong would show there.

const (
	// The project is the maintenance fixture's, so the seeded author row and this
	// file's expectations cannot drift apart.
	productionProjectCoreGeneratedProjectID  = productionDBMaintenanceProjectID
	productionProjectCoreGeneratedTab        = "client_coregen0001"
	productionProjectCoreGeneratedOpenID     = "project-core-generated-open-1"
	productionProjectCoreGeneratedStaleLease = "project-core-generated-stale-lease"
	productionProjectCoreGeneratedNonce      = "project-core-generated-begin-1"

	// The author's own text (the maintenance seed's project_core row), the decoy
	// row's text, and the text the commit writes. Every assertion below is a value
	// comparison, never a row count alone.
	productionProjectCoreGeneratedTitle        = "清除验证小说"
	productionProjectCoreGeneratedPremise      = "作者前提"
	productionProjectCoreGeneratedSynopsis     = "作者总大纲"
	productionProjectCoreGeneratedArch         = "作者人物群像"
	productionProjectCoreGeneratedStyle        = "作者文风"
	productionProjectCoreGeneratedStates       = `{"甲":"状态"}`
	productionProjectCoreGeneratedDecoyGenre   = "旁路题材"
	productionProjectCoreGeneratedDecoyPremise = "旁路前提"
	productionProjectCoreGeneratedWrittenText  = "生成前提"
	productionProjectCoreGeneratedWrittenWorld = "生成世界设定"
	productionProjectCoreGeneratedWrittenGenre = "wuxia"

	// The seeded epoch no session holds, and the two runs that carry it: one
	// ordinary run (the stale-epoch refusal) and one agent-workflow child (the
	// stamp this lane refuses to invent).
	productionProjectCoreGeneratedSeededEpoch    = "seeded-epoch"
	productionProjectCoreGeneratedDecoyRun       = "run-core-generated-decoy"
	productionProjectCoreGeneratedDecoyRoot      = "root-core-generated-decoy"
	productionProjectCoreGeneratedRegisteredRun  = "run-core-generated-registered"
	productionProjectCoreGeneratedRegisteredRoot = "root-core-generated-registered"
	productionProjectCoreGeneratedRegistrationID = "agent-workflow:" +
		"7b1e0c9a4d2f5863a0b4c7d9e2f1530869acbd4e7f2013b6c8d5a9e04f7b2c61"

	// The two attempts of the decoy run: a receipt write that stamped every
	// attempt, or every attempt of every run, would move one of them.
	productionProjectCoreGeneratedAttemptOne = "attempt-core-generated-1"
	productionProjectCoreGeneratedAttemptTwo = "attempt-core-generated-2"
)

// productionProjectCoreGeneratedRoot is the admitted baseline fixture plus the
// rows only this file needs. The database is seeded before the lane admits it,
// because the admission permit hashes the file at open.
func productionProjectCoreGeneratedRoot(t *testing.T) (root, projectPath, database string, contract admission.SchemaContract) {
	t.Helper()
	root, projectPath, database, contract = productionDBMaintenanceRoot(t)
	productionProjectCoreGeneratedSeed(t, database)
	return root, projectPath, database, contract
}

// productionProjectCoreGeneratedSeed adds the decoy core row, the two seeded runs
// under an epoch no session holds - one ordinary, one agent-workflow child - each
// with its root, and two attempts on the ordinary one.
func productionProjectCoreGeneratedSeed(t *testing.T, database string) {
	t.Helper()
	binding := func(registration string) string {
		manifest := `{"operation":"generate-core-seed","modelId":"` + productionTestModelID + `"`
		if registration != "" {
			manifest += `,"agentWorkflowRegistrationId":"` + registration + `"`
		}
		manifest += `}`
		encoded, err := json.Marshal(map[string]any{
			"projectId":         productionProjectCoreGeneratedProjectID,
			"epoch":             productionProjectCoreGeneratedSeededEpoch,
			"fingerprint":       map[string]any{},
			"contextSnapshotId": "seeded-context",
			"sourceManifest":    json.RawMessage(manifest),
			"sourceRefs":        []any{},
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	action := func(rootID string) string {
		encoded, err := json.Marshal(map[string]any{
			"projectId":       productionProjectCoreGeneratedProjectID,
			"epoch":           productionProjectCoreGeneratedSeededEpoch,
			"operation":       "generate-core-seed",
			"uiActionNonce":   rootID + "-nonce",
			"frozenInputHash": rootID + "-hash",
			"rootActionId":    rootID,
			"status":          "active",
		})
		if err != nil {
			t.Fatal(err)
		}
		return string(encoded)
	}
	budget := `{"maxPhysicalRequests":3,"maxTokenLiability":4000,"maxOutputPerRequest":1000,"maxActiveElapsedMs":600000}`
	statements := []struct {
		query string
		args  []any
	}{
		// The decoy core row: the UPDATE is WHERE id = 'main', so a missing WHERE
		// would rewrite this one too.
		{`INSERT INTO project_core (id, project_name, writing_language, genre, premise,
 core_outline, updated_at) VALUES ('side', '旁路小说', 'zh-CN', ?, ?, '旁路大纲',
 '2024-01-02 03:04:05')`, []any{productionProjectCoreGeneratedDecoyGenre, productionProjectCoreGeneratedDecoyPremise}},
		{`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json)
 VALUES (?, ?, ?, ?)`, []any{productionProjectCoreGeneratedDecoyRoot, productionProjectCoreGeneratedDecoyRoot,
			action(productionProjectCoreGeneratedDecoyRoot), budget}},
		{`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json)
 VALUES (?, ?, ?, ?)`, []any{productionProjectCoreGeneratedRegisteredRoot, productionProjectCoreGeneratedRegisteredRoot,
			action(productionProjectCoreGeneratedRegisteredRoot), budget}},
		{`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000000, ?)`, []any{productionProjectCoreGeneratedDecoyRun,
			productionProjectCoreGeneratedDecoyRoot, binding(""), productionProjectCoreGeneratedDecoyRun}},
		{`INSERT INTO generation_runs (run_id, root_action_id, binding_json, status, created_at_ms, open_key)
 VALUES (?, ?, ?, 'running', 1700000000001, ?)`, []any{productionProjectCoreGeneratedRegisteredRun,
			productionProjectCoreGeneratedRegisteredRoot, binding(productionProjectCoreGeneratedRegistrationID),
			productionProjectCoreGeneratedRegisteredRun}},
		{`INSERT INTO generation_attempts (attempt_id, reservation_id, run_id, root_action_id,
 attempt_json, usage_receipt_json, invocation_nonce)
 VALUES (?, ?, ?, ?, '{"status":"settled"}', '{"purpose":"generate-core-seed"}', ?)`,
			[]any{productionProjectCoreGeneratedAttemptOne, productionProjectCoreGeneratedAttemptOne,
				productionProjectCoreGeneratedDecoyRun, productionProjectCoreGeneratedDecoyRoot,
				productionProjectCoreGeneratedAttemptOne}},
		{`INSERT INTO generation_attempts (attempt_id, reservation_id, run_id, root_action_id,
 attempt_json, usage_receipt_json, invocation_nonce)
 VALUES (?, ?, ?, ?, '{"status":"settled"}', '{"purpose":"generate-core-seed"}', ?)`,
			[]any{productionProjectCoreGeneratedAttemptTwo, productionProjectCoreGeneratedAttemptTwo,
				productionProjectCoreGeneratedDecoyRun, productionProjectCoreGeneratedDecoyRoot,
				productionProjectCoreGeneratedAttemptTwo}},
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

// productionProjectCoreGeneratedLane opens the author runtime over the fixture and
// mounts the binders these tests need on one transport server: project:open, the
// reduced generation:begin (so the run under test freezes the live lease), and the
// name under test.
func productionProjectCoreGeneratedLane(t *testing.T, root string, contract admission.SchemaContract) *transport.Server {
	t.Helper()
	// The committed model profile has to name an origin before the lane pins its
	// egress policy: generation:begin refuses a lane with no committed policy, and
	// this fixture dials nothing.
	_, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
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
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-project-core-generated")
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	owner, err := newProductionGeneration(open, server, nil)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation admission is unavailable: %v", err)
	}
	if err := server.BindGenerationBegin(&transport.GenerationProvider{Begin: owner.Begin}); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	if err := newProductionProjectCoreGenerated(open.durable).Bind(server); err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	return server
}

// productionProjectCoreGeneratedSession opens the fixture project and returns the
// frozen three-key context every request must carry.
func productionProjectCoreGeneratedSession(t *testing.T, server *transport.Server, projectPath string) (*productionDurableClient, map[string]any) {
	t.Helper()
	client := newProductionDurableClient(t, server, productionProjectCoreGeneratedTab)
	var opened productionDurableOpenWire
	client.decode(t, "project:open", []any{projectPath, productionProjectCoreGeneratedOpenID, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the fixture project: %v %v", opened.Success, opened.DBReady)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return client, map[string]any{
		"projectId": opened.Project["id"], "projectPath": opened.Project["path"], "leaseId": lease,
	}
}

// productionProjectCoreGeneratedBegin opens the run under test through the served
// generation:begin. The selection names no draft, blueprint or prompt, so the run
// freezes project-core:main as its only source - the manifest an architecture run
// really has when it commits a generated field.
func productionProjectCoreGeneratedBegin(t *testing.T, client *productionDurableClient, frozen map[string]any, nonce string) map[string]any {
	t.Helper()
	selection := map[string]any{
		"operation":                       "generate-core-seed",
		"uiActionNonce":                   nonce,
		"modelId":                         productionTestModelID,
		"output":                          "visible-text",
		"selectedDraftIds":                []any{},
		"selectedFinalizedDraftIds":       []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":                      []any{},
		"skillStages":                     []any{},
		"authorInputs":                    []any{},
	}
	var result map[string]any
	productionEditorDecode(t, client, "generation:begin", []any{selection, frozen}, &result)
	handle, ok := result["handle"].(map[string]any)
	if !ok || handle == nil {
		t.Fatalf("generation:begin returned no run handle: %v", result)
	}
	for _, key := range []string{"projectId", "epoch", "rootActionId", "runId"} {
		if value, _ := handle[key].(string); value == "" {
			t.Fatalf("generation:begin returned a handle without %v: %v", key, handle)
		}
	}
	return handle
}

// productionProjectCoreGeneratedRequest is the renderer's request object
// (src/shared/ipc-channels.ts:913-916): the partial core data and the generation
// run handle that produced it.
func productionProjectCoreGeneratedRequest(handle map[string]any, data map[string]any) map[string]any {
	return map[string]any{"data": data, "generationRunHandle": handle}
}

// productionProjectCoreGeneratedAnswer decodes one answer and returns its success
// flag and its code, which is the envelope's error member without the `Error: `
// prefix the controller's catch arm prepends.
func productionProjectCoreGeneratedAnswer(t *testing.T, response *httptest.ResponseRecorder) (bool, string) {
	t.Helper()
	var wire struct {
		Success bool   `json:"success"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(productionDBMaintenanceResult(t, response), &wire); err != nil {
		t.Fatalf("decode answer: %v", err)
	}
	return wire.Success, strings.TrimPrefix(wire.Error, "Error: ")
}

// productionProjectCoreGeneratedRefused requires a refusal envelope and returns
// its code. The controller catches everything, so a refusal is a 200 answer with
// success false; a 5xx here would be a bug in the adapter, not a refusal.
func productionProjectCoreGeneratedRefused(t *testing.T, label string, response *httptest.ResponseRecorder) string {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("%v answered %d, want the controller's 200 envelope: %s", label, response.Code, response.Body.String())
	}
	success, code := productionProjectCoreGeneratedAnswer(t, response)
	if success {
		t.Fatalf("%v answered success for a request that had to be refused", label)
	}
	if code == "" {
		t.Fatalf("%v answered no code", label)
	}
	return code
}

// productionProjectCoreGeneratedReceipts is every attempt receipt of the fixture
// as one canonical string, read over a connection of the test's own. It is the
// witness for "no receipt was written": the reference stamps agentSourceTransition
// only for a registered agent child, and this lane serves none.
func productionProjectCoreGeneratedReceipts(t *testing.T, database string) string {
	t.Helper()
	return productionDBMaintenanceText(t, database, `SELECT COALESCE(group_concat(
 attempt_id||'='||COALESCE(usage_receipt_json,''),'|'),'') FROM (
 SELECT attempt_id, usage_receipt_json FROM generation_attempts ORDER BY attempt_id)`)
}

// productionProjectCoreGeneratedWanted reports whether a refusal named one of the
// codes the arm accepts. The lease arms can answer more than one honest code (a
// stale lease, an unknown project, a changed context), so they list the shapes and
// the match is by containment; every other arm lists one full symbolic code, where
// containment is the same as equality.
func productionProjectCoreGeneratedWanted(code string, wanted []string) bool {
	for _, candidate := range wanted {
		if code == candidate || strings.Contains(code, candidate) {
			return true
		}
	}
	return false
}

// TestProductionProjectCoreGeneratedCommitsTheCoreRow proves the served write over
// real HTTP through the frozen session: one transaction writes exactly the columns
// the request names plus updated_at, leaves every other column, the decoy core row
// and both attempt receipts untouched, and answers the reference's own
// `{ success: true }` (db-controller.ts:208).
//
// The replay arm calls the same request again under the same run handle. This
// channel has no operation ledger and no duplicate branch - the reference's
// `ProjectCoreRepository.update` is an unconditional UPDATE - so a replay answers
// the same success (:208) and writes no new row.
//
// The empty-data arm is the reference's early return at
// project-core-repository.ts:201: a request that names no allowlisted field writes
// nothing at all, not even updated_at, so the whole file stays byte-identical.
func TestProductionProjectCoreGeneratedCommitsTheCoreRow(t *testing.T) {
	root, projectPath, database, contract := productionProjectCoreGeneratedRoot(t)
	server := productionProjectCoreGeneratedLane(t, root, contract)
	author, frozen := productionProjectCoreGeneratedSession(t, server, projectPath)
	handle := productionProjectCoreGeneratedBegin(t, author, frozen, productionProjectCoreGeneratedNonce)
	if epoch, _ := handle["epoch"].(string); epoch != frozen["leaseId"] {
		t.Fatalf("the run froze epoch %v, want the live lease the session holds", epoch)
	}
	receipts := productionProjectCoreGeneratedReceipts(t, database)
	runs := productionDBMaintenanceCount(t, database, "generation_runs")
	if value := productionDBMaintenanceText(t, database,
		`SELECT premise FROM project_core WHERE id='main'`); value != productionProjectCoreGeneratedPremise {
		t.Fatalf("the fixture's core premise is %v, want the author's own", value)
	}

	data := map[string]any{
		"premise":                                productionProjectCoreGeneratedWrittenText,
		"worldSetting":                           productionProjectCoreGeneratedWrittenWorld,
		"genre":                                  productionProjectCoreGeneratedWrittenGenre,
		"totalChapters":                          42,
		"narrativeThreadDormantChapterThreshold": 7,
	}
	before := productionDBMaintenanceDigest(t, database)
	response := author.call(t, productionProjectCoreGeneratedName, []any{
		productionProjectCoreGeneratedRequest(handle, data), projectPath, frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", productionProjectCoreGeneratedName, response.Code, response.Body.String())
	}
	success, code := productionProjectCoreGeneratedAnswer(t, response)
	if !success || code != "" {
		t.Fatalf("the commit answered success=%v code=%v", success, code)
	}
	// The written half: the five named columns, in the row the reference writes.
	written := productionDBMaintenanceText(t, database, `SELECT total_chapters||'|'||
 narrative_thread_dormant_threshold||'|'||genre||'|'||premise||'|'||world_setting
 FROM project_core WHERE id='main'`)
	if written != "42|7|"+productionProjectCoreGeneratedWrittenGenre+"|"+
		productionProjectCoreGeneratedWrittenText+"|"+productionProjectCoreGeneratedWrittenWorld {
		t.Fatalf("the core row reads %v", written)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT updated_at FROM project_core WHERE id='main'`); value == "2024-05-06 07:08:09" {
		t.Fatal("the commit did not write updated_at")
	}
	// The preserved half: the columns the request did not name.
	preserved := productionDBMaintenanceText(t, database, `SELECT project_name||'|'||synopsis||'|'||
 characters_arch||'|'||writing_style||'|'||character_states FROM project_core WHERE id='main'`)
	if preserved != productionProjectCoreGeneratedTitle+"|"+productionProjectCoreGeneratedSynopsis+"|"+
		productionProjectCoreGeneratedArch+"|"+productionProjectCoreGeneratedStyle+"|"+
		productionProjectCoreGeneratedStates {
		t.Fatalf("the commit touched columns it was not given: %v", preserved)
	}
	// The decoy row: the same UPDATE with a wrong WHERE would have rewritten it.
	decoy := productionDBMaintenanceText(t, database,
		`SELECT genre||'|'||premise||'|'||updated_at FROM project_core WHERE id='side'`)
	if decoy != productionProjectCoreGeneratedDecoyGenre+"|"+productionProjectCoreGeneratedDecoyPremise+"|2024-01-02 03:04:05" {
		t.Fatalf("the commit rewrote the decoy core row: %v", decoy)
	}
	// The receipt table: this lane stamps no receipt, so both attempts and their
	// usage are exactly what the fixture wrote.
	if after := productionProjectCoreGeneratedReceipts(t, database); after != receipts {
		t.Fatalf("the commit wrote an attempt receipt: %v", after)
	}
	if after := productionDBMaintenanceCount(t, database, "generation_runs"); after != runs {
		t.Fatalf("the commit opened a run: %d rows, want %d", after, runs)
	}
	if after := productionDBMaintenanceDigest(t, database); after == before {
		t.Fatal("the successful commit did not change the database file at all")
	}

	// Replay: the same request under the same run handle is the same success and
	// the same row.
	replay := author.call(t, productionProjectCoreGeneratedName, []any{
		productionProjectCoreGeneratedRequest(handle, data), projectPath, frozen})
	if replay.Code != http.StatusOK {
		t.Fatalf("replay: %d %s", replay.Code, replay.Body.String())
	}
	replayed, replayCode := productionProjectCoreGeneratedAnswer(t, replay)
	if !replayed || replayCode != "" {
		t.Fatalf("the replay answered success=%v code=%v", replayed, replayCode)
	}
	if again := productionDBMaintenanceText(t, database, `SELECT total_chapters||'|'||genre||'|'||premise
 FROM project_core WHERE id='main'`); again != "42|"+productionProjectCoreGeneratedWrittenGenre+"|"+
		productionProjectCoreGeneratedWrittenText {
		t.Fatalf("the replay wrote a different row: %v", again)
	}
	if after := productionProjectCoreGeneratedReceipts(t, database); after != receipts {
		t.Fatalf("the replay wrote an attempt receipt: %v", after)
	}
	if after := productionDBMaintenanceCount(t, database, "generation_runs"); after != runs {
		t.Fatalf("the replay opened a run: %d rows, want %d", after, runs)
	}

	// The empty update: project-core-repository.ts:201 returns before appending
	// updated_at, so nothing is written and the file does not move.
	stamp := productionDBMaintenanceText(t, database, `SELECT updated_at FROM project_core WHERE id='main'`)
	empty := productionDBMaintenanceDigest(t, database)
	response = author.call(t, productionProjectCoreGeneratedName, []any{
		productionProjectCoreGeneratedRequest(handle, map[string]any{}), projectPath, frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("the empty update: %d %s", response.Code, response.Body.String())
	}
	emptySuccess, emptyCode := productionProjectCoreGeneratedAnswer(t, response)
	if !emptySuccess || emptyCode != "" {
		t.Fatalf("the empty update answered success=%v code=%v", emptySuccess, emptyCode)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT updated_at FROM project_core WHERE id='main'`); value != stamp {
		t.Fatalf("the empty update wrote updated_at: %v became %v", stamp, value)
	}
	productionDBMaintenanceUnchanged(t, database, empty, "the empty update")
}

// TestProductionProjectCoreGeneratedRefusalsLeaveTheFileIdentical proves every
// refusal this channel can answer - the argument fence, the admission, the run
// identity, the live epoch, the registered agent child, and the two inputs the
// reference itself rejects - leaves the whole database file byte-identical and
// every attempt receipt exactly as it was.
func TestProductionProjectCoreGeneratedRefusalsLeaveTheFileIdentical(t *testing.T) {
	root, projectPath, database, contract := productionProjectCoreGeneratedRoot(t)
	server := productionProjectCoreGeneratedLane(t, root, contract)
	author, frozen := productionProjectCoreGeneratedSession(t, server, projectPath)
	handle := productionProjectCoreGeneratedBegin(t, author, frozen, productionProjectCoreGeneratedNonce)
	receipts := productionProjectCoreGeneratedReceipts(t, database)
	foreign := filepath.Join(root, productionLibraryDirectory, "foreign-novel")
	projectID, _ := frozen["projectId"].(string)
	lease, _ := frozen["leaseId"].(string)
	runRoot, _ := handle["rootActionId"].(string)
	runID, _ := handle["runId"].(string)
	if projectID == "" || lease == "" || runRoot == "" || runID == "" {
		t.Fatalf("the frozen session or the run handle is incomplete: %v %v", projectID, runID)
	}
	handleOf := func(project, epoch, root, run string) map[string]any {
		return map[string]any{"projectId": project, "epoch": epoch, "rootActionId": root, "runId": run}
	}
	// The seeded run and the seeded agent-workflow child, both under the epoch no
	// session holds.
	seeded := handleOf(productionProjectCoreGeneratedProjectID, productionProjectCoreGeneratedSeededEpoch,
		productionProjectCoreGeneratedDecoyRoot, productionProjectCoreGeneratedDecoyRun)
	registered := handleOf(productionProjectCoreGeneratedProjectID, productionProjectCoreGeneratedSeededEpoch,
		productionProjectCoreGeneratedRegisteredRoot, productionProjectCoreGeneratedRegisteredRun)
	update := map[string]any{"premise": productionProjectCoreGeneratedWrittenText}

	for _, arm := range []struct {
		label string
		args  []any
		want  []string
	}{
		{"a foreign project", []any{productionProjectCoreGeneratedRequest(handle, update), foreign,
			map[string]any{"projectId": projectID, "projectPath": foreign, "leaseId": lease}},
			[]string{"PROJECT_NOT_OPEN"}},
		{"a stale lease", []any{productionProjectCoreGeneratedRequest(handle, update), projectPath,
			map[string]any{"projectId": projectID, "projectPath": projectPath,
				"leaseId": productionProjectCoreGeneratedStaleLease}},
			[]string{"PROJECT_SESSION", "PROJECT_NOT_OPEN", "AUTHOR_WRITE_STALE", "PROJECT_CONTEXT"}},
		{"a path the context does not name", []any{productionProjectCoreGeneratedRequest(handle, update), projectPath,
			map[string]any{"projectId": projectID, "projectPath": foreign, "leaseId": lease}},
			[]string{"PROJECT_CONTEXT_CHANGED"}},
		{"the wrong arity", []any{productionProjectCoreGeneratedRequest(handle, update)}, []string{"INVALID_ARGUMENTS"}},
		{"a request that is not an object", []any{"premise", projectPath, frozen}, []string{"INVALID_ARGUMENTS"}},
		{"data that is not an object", []any{map[string]any{"data": "premise",
			"generationRunHandle": handle}, projectPath, frozen}, []string{"INVALID_ARGUMENTS"}},
		{"no generation run handle", []any{map[string]any{"data": update}, projectPath, frozen},
			[]string{"GENERATION_OWNER_REQUIRED"}},
		{"a run this database does not hold", []any{productionProjectCoreGeneratedRequest(
			handleOf(projectID, lease, "root-absent", "run-absent"),
			update), projectPath, frozen}, []string{"GENERATION_RUN_MISSING"}},
		{"another project's handle", []any{productionProjectCoreGeneratedRequest(
			handleOf("00000000-0000-4000-8000-000000000000", lease, runRoot, runID),
			update), projectPath, frozen}, []string{"GENERATION_RUN_IDENTITY_MISMATCH"}},
		{"a run frozen under another epoch", []any{productionProjectCoreGeneratedRequest(seeded, update),
			projectPath, frozen}, []string{"GENERATION_EPOCH_STALE"}},
		{"an agent workflow child", []any{productionProjectCoreGeneratedRequest(registered, update),
			projectPath, frozen}, []string{"GENERATION_AGENT_CHILD_EFFECT_UNPORTED"}},
		{"charactersArch", []any{productionProjectCoreGeneratedRequest(handle,
			map[string]any{"charactersArch": nil}), projectPath, frozen},
			[]string{"PROJECT_CORE_CHARACTERS_ARCH_READ_ONLY"}},
		{"a boolean column value", []any{productionProjectCoreGeneratedRequest(handle,
			map[string]any{"premise": true}), projectPath, frozen},
			[]string{"PROJECT_CORE_INVALID_BINDING"}},
	} {
		before := productionDBMaintenanceDigest(t, database)
		code := productionProjectCoreGeneratedRefused(t, arm.label, author.call(t, productionProjectCoreGeneratedName, arm.args))
		if !productionProjectCoreGeneratedWanted(code, arm.want) {
			t.Fatalf("%v refused with %v, want one of %v", arm.label, code, arm.want)
		}
		productionDBMaintenanceUnchanged(t, database, before, "the refused "+arm.label)
		if after := productionProjectCoreGeneratedReceipts(t, database); after != receipts {
			t.Fatalf("%v wrote an attempt receipt: %v", arm.label, after)
		}
	}
	// After every refusal the author's row is still the author's.
	if value := productionDBMaintenanceText(t, database,
		`SELECT premise FROM project_core WHERE id='main'`); value != productionProjectCoreGeneratedPremise {
		t.Fatalf("a refusal rewrote the core premise to %v", value)
	}
}

// TestProductionProjectCoreGeneratedAbortedWriteLeavesNoRow proves the write is
// one transaction: a statement that fails inside it leaves the core row exactly as
// the author wrote it and the whole file byte-identical, and the same request
// succeeds once the impossible column is dropped.
//
// The failure is a NOT NULL constraint on project_core.project_name, which the
// admitted baseline declares. It fires on the UPDATE itself - after the run
// identity, the live epoch and the root proof have all passed - which is the point
// a forced rollback has to reach. A trigger would prove the same thing and is not
// usable here: admission hashes sqlite_master and refuses a schema it did not pin,
// so a trigger added to the fixture would be refused at open, before any handler.
//
// The stamp half of the transaction is the same statement for every run this lane
// can begin: the reference's withChildEffect is `if (!id) return effect()`
// (electron/services/agent-generation.ts:349) for a child with no
// agentWorkflowRegistrationId, and generation:begin cannot mint that key, so the
// core row IS the effect here. A registered child never reaches the UPDATE - it is
// refused before the write, which the previous test's agent-child arm proves.
func TestProductionProjectCoreGeneratedAbortedWriteLeavesNoRow(t *testing.T) {
	root, projectPath, database, contract := productionProjectCoreGeneratedRoot(t)
	server := productionProjectCoreGeneratedLane(t, root, contract)
	author, frozen := productionProjectCoreGeneratedSession(t, server, projectPath)
	handle := productionProjectCoreGeneratedBegin(t, author, frozen, productionProjectCoreGeneratedNonce)
	receipts := productionProjectCoreGeneratedReceipts(t, database)
	runs := productionDBMaintenanceCount(t, database, "generation_runs")

	before := productionDBMaintenanceDigest(t, database)
	response := author.call(t, productionProjectCoreGeneratedName, []any{
		productionProjectCoreGeneratedRequest(handle, map[string]any{
			"projectName": nil,
			"premise":     productionProjectCoreGeneratedWrittenText,
		}), projectPath, frozen})
	code := productionProjectCoreGeneratedRefused(t, "the aborted core write", response)
	if code != productionProjectCoreGeneratedFailed {
		t.Fatalf("the aborted write refused with %v, want the family fallback %v",
			code, productionProjectCoreGeneratedFailed)
	}
	// The committed row is absent: the author's values are unchanged in the main
	// row, the decoy row is untouched, and no row anywhere carries the new value.
	if value := productionDBMaintenanceText(t, database,
		`SELECT premise FROM project_core WHERE id='main'`); value != productionProjectCoreGeneratedPremise {
		t.Fatalf("the aborted write left the premise at %v", value)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT project_name FROM project_core WHERE id='main'`); value != productionProjectCoreGeneratedTitle {
		t.Fatalf("the aborted write changed the project name to %v", value)
	}
	if count := productionDBMaintenanceCount(t, database,
		"project_core WHERE premise='"+productionProjectCoreGeneratedWrittenText+"'"); count != 0 {
		t.Fatalf("%d rows carry the aborted value", count)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT premise FROM project_core WHERE id='side'`); value != productionProjectCoreGeneratedDecoyPremise {
		t.Fatalf("the aborted write reached the decoy row: %v", value)
	}
	if after := productionProjectCoreGeneratedReceipts(t, database); after != receipts {
		t.Fatalf("the aborted write stamped an attempt receipt: %v", after)
	}
	if after := productionDBMaintenanceCount(t, database, "generation_runs"); after != runs {
		t.Fatalf("the aborted write opened a run: %d rows, want %d", after, runs)
	}
	productionDBMaintenanceUnchanged(t, database, before, "the aborted core write")

	// The same request without the impossible column lands, so the refusal above
	// was the constraint and not a broken lane.
	retry := author.call(t, productionProjectCoreGeneratedName, []any{
		productionProjectCoreGeneratedRequest(handle, map[string]any{
			"premise": productionProjectCoreGeneratedWrittenText,
		}), projectPath, frozen})
	if retry.Code != http.StatusOK {
		t.Fatalf("the retry: %d %s", retry.Code, retry.Body.String())
	}
	retried, retryCode := productionProjectCoreGeneratedAnswer(t, retry)
	if !retried || retryCode != "" {
		t.Fatalf("the retry answered success=%v code=%v", retried, retryCode)
	}
	if value := productionDBMaintenanceText(t, database,
		`SELECT premise FROM project_core WHERE id='main'`); value != productionProjectCoreGeneratedWrittenText {
		t.Fatalf("the retry left the premise at %v", value)
	}
	if after := productionDBMaintenanceDigest(t, database); after == before {
		t.Fatal("the retried write did not change the database file at all")
	}
}

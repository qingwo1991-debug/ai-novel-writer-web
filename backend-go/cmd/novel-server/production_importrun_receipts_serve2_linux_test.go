//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file drives the two names production_importrun_receipts_serve2_linux.go
// serves over the real HTTP transport and the frozen project session:
//
//	generation:begin + importSlot  -> a REAL import-bound run
//	import-generation:execute      -> one settled attempt with an artifact
//	db:import-run-effect-receipt-prepare -> the receipt the run's proof earns
//	db:import-run-effect-receipt-commit  -> the effect, read back over mode=ro
//
// The proof under test is the one the store's widened callback feeds
// (internal/store/importrun_receipts.go:130,
// internal/store/importrun_receipts_generation.go): the run is read on the receipt
// transaction's own connection and handed to the prover as data, so nothing in the
// assert waits for the connection the BEGIN IMMEDIATE reserved.
//
// # Why the success arm begins for real
//
// A receipt prepared against a hand-written binding would prove nothing: the two
// halves that used to be impossible are exactly the halves a hand-written row
// skips. So the success arm begins a run through generation:begin with the style
// import selection, dispatches one attempt through import-generation:execute, and
// only then prepares. The registry lease record, the frozen prompt refs, the
// reproduction and the stopped artifact are all the lane's own.
//
// # Why the first prepare refuses
//
// Between the begin and the execute the run has NO attempt, and the reference
// refuses that (electron/services/main-generation-owner.ts:1465-1471:
// !attempts.length). That is the arm that proves the store's evidence is
// load-bearing rather than decorative: a prover that never looked at the attempts
// would answer success there.
//
// # The byte-identical arms
//
// A stale execution lease and a foreign project path are refused by the same fence
// the four served residual names use, so the bytes are compared against those names
// executing the identical request: db:import-run-fail for the stale lease and
// db:import-run-finalize-parsing for the foreign path. The absent generation run
// has no sibling that answers it, so prepare and commit are compared with each other
// and with their own repeat: the same code, the same bytes, every time.

const (
	productionImportRunReceiptsServe2OpenNonce = "import-run-receipts-serve2-open-1"
	// The three refusal runs beside the success run. Each is a running style run
	// with a live execution lease, so nothing below is a disguised stale lease or a
	// missing import run.
	productionImportRunReceiptsServe2AbsentPrepareRun = "receipt-serve2-absent-prepare"
	productionImportRunReceiptsServe2AbsentCommitRun  = "receipt-serve2-absent-commit"
	productionImportRunReceiptsServe2StaleRun         = "receipt-serve2-stale"
	// productionImportRunReceiptsServe2GhostRun is a generation run id no
	// generation_runs row carries: the absent-run arm.
	productionImportRunReceiptsServe2GhostRun = "receipt-serve2-ghost-generation-run"
	// productionImportRunReceiptsServe2EffectKey and ...Kind are the receipt's own
	// identity for the style stage.
	productionImportRunReceiptsServe2EffectKey = "writing-style"
	productionImportRunReceiptsServe2Kind      = "project-writing-style"
	// productionImportRunReceiptsServe2Style is the writing style the committed
	// receipt carries into project_core.
	productionImportRunReceiptsServe2Style = "克制而具体的叙述"
)

// productionImportRunReceiptsServe2Lane is one opened lane: the served receipt
// names, the generation names they need, the four residual names the byte-identity
// arms compare against, and the frozen session every call carries.
type productionImportRunReceiptsServe2Lane struct {
	projectPath string
	database    string
	projectID   string
	leaseID     string
	author      *productionDurableClient
	frozen      map[string]any
	seam        *productionImportGenerationSeam
}

// productionImportRunReceiptsServe2LaneOpen builds the fixture, seeds the import
// runs and their chapters, opens the production lane and mounts the names this file
// drives on one transport server.
func productionImportRunReceiptsServe2LaneOpen(t *testing.T) *productionImportRunReceiptsServe2Lane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	_, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	productionImportRunReceiptsServe2Seed(t, projectPath)
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
	seam := &productionImportGenerationSeam{text: productionImportGenerationServedText}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-import-run-receipts2")
	owner, err := newProductionGeneration(open, server, seam.dispatch)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation admission is unavailable: %v", err)
	}
	for _, bind := range []struct {
		label string
		bind  func() error
	}{
		{"project:open", func() error { return server.BindProjects(open.durable) }},
		{"generation:begin", func() error {
			return server.BindGenerationBegin(&transport.GenerationProvider{Begin: owner.Begin})
		}},
		{"generation:execute", func() error {
			return server.BindGenerationExecute(&transport.GenerationProvider{Execute: owner.Execute})
		}},
		// The import family's two dispatch names, so the success arm can settle the
		// attempt the receipt's proof requires.
		{"import-generation", func() error { return newProductionImportGenerationServe(owner).Bind(server) }},
		// The residual slice: its four served names, and with them the two reference
		// names the stale-lease and foreign-path arms compare against.
		{"db:import-run-residuals", func() error { return newProductionImportRunResiduals(open.durable).Bind(server) }},
		// The two names this file is about.
		{"db:import-run-effect-receipt", func() error {
			return newProductionImportRunReceiptsServe2(owner).Bind(server)
		}},
	} {
		if err := bind.bind(); err != nil {
			_ = open.Close()
			t.Fatalf("%s: %v", bind.label, err)
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	t.Cleanup(func() {
		server.Close()
		_ = open.Close()
	})
	author, frozen := productionExecuteSession(t, mux, projectPath, productionImportRunReceiptsServe2OpenNonce)
	return &productionImportRunReceiptsServe2Lane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		projectID:   syntheticString(frozen["projectId"]),
		leaseID:     syntheticString(frozen["leaseId"]),
		author:      author,
		frozen:      frozen,
		seam:        seam,
	}
}

// productionImportRunReceiptsServe2Seed writes the runs the three lane tests drive:
// the success and identity arms use the import-generation fixture's own style run,
// the slot arm begins the fixture's own global run, and the three refusal arms use
// two running style runs plus one more for the absent-prepare shape. Every run gets
// the two chapters its stage's freeze would capture, so the prompt refs and the
// language are the ones that lane already proved.
func productionImportRunReceiptsServe2Seed(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	defer func() { _ = db.Close() }()
	for _, run := range []struct {
		id    string
		stage string
	}{
		{productionImportGenerationStyleRunID, "style"},
		{productionImportGenerationGlobalRunID, "global"},
		{productionImportRunReceiptsServe2AbsentPrepareRun, "style"},
		{productionImportRunReceiptsServe2AbsentCommitRun, "style"},
		{productionImportRunReceiptsServe2StaleRun, "style"},
	} {
		// effect_namespace is the store's own derivation: "import:" + purpose + ":" + id
		// (internal/store/importrun_receipts.go importRunReceiptParse). The seed must
		// write exactly that, or the store's own read-back validation refuses the
		// receipt as IMPORT_RUN_RECEIPT_CORRUPT before the lane is even opened.
		if _, err := db.Exec(`INSERT INTO import_runs (id, purpose, root_run_id, effect_namespace,
 source_fingerprint, manifest_fingerprint, locale, stage, status, execution_owner, execution_epoch,
 lease_expires_at, total_chapters, manifest_chapter_count, manifest_word_count, cancel_requested)
 VALUES (?, 'reference', ?, ?, ?, ?, 'zh-CN', ?, 'running', ?, ?, ?, 2, 2, 12, 0)`,
			run.id, run.id+"-root", "import:reference:"+run.id,
			strings.Repeat("a", 64), productionImportGenerationManifestFingerprint,
			run.stage, productionImportGenerationExecutionOwner, productionImportGenerationExecutionEpoch,
			productionImportGenerationLeaseExpiry); err != nil {
			t.Fatalf("seed import run %s: %v", run.id, err)
		}
		for number, chapter := range []struct {
			title string
			body  string
		}{
			{"第一章", productionImportGenerationStyleFirst},
			{"第二章", productionImportGenerationStyleLast},
		} {
			if _, err := db.Exec(`INSERT INTO import_run_chapters (run_id, chapter_number, title,
 content_fingerprint, content_size, content_snapshot) VALUES (?, ?, ?, ?, ?, ?)`,
				run.id, int64(number+1), chapter.title, syntheticHash(chapter.body),
				int64(len(chapter.body)), chapter.body); err != nil {
				t.Fatalf("seed import chapter %s/%d: %v", run.id, number+1, err)
			}
		}
	}
	// The two prepared receipts the refusal arms need. They are written with the
	// store's own prepare BEFORE the lane admits the file, because the stale arm only
	// reaches the execution check once a prepared row exists, and the absent-run arm
	// only reaches the proof once its handle is already frozen into a receipt. The nil
	// proof is the reference's own nil-argument case: no proof, so only the database
	// binding decides whether a handle is required.
	repository := store.NewImportRuns(db)
	execution := store.ImportRunExecution{
		Owner: productionImportGenerationExecutionOwner, Epoch: productionImportGenerationExecutionEpoch,
	}
	for _, seed := range []struct {
		runID   string
		payload map[string]any
	}{
		{productionImportRunReceiptsServe2StaleRun,
			map[string]any{"writingStyle": productionImportRunReceiptsServe2Style}},
		{productionImportRunReceiptsServe2AbsentCommitRun, map[string]any{
			"writingStyle": productionImportRunReceiptsServe2Style,
			"generationRunHandle": map[string]any{
				"projectId":    "receipt-serve2-ghost-project",
				"epoch":        "receipt-serve2-ghost-epoch",
				"rootActionId": "receipt-serve2-ghost-root",
				"runId":        productionImportRunReceiptsServe2GhostRun,
			},
		}},
	} {
		prepared, err := repository.PrepareEffectReceipt(context.Background(), execution,
			store.ImportRunPrepareEffectReceiptRequest{
				RunID: seed.runID, Stage: "style", BatchID: "done",
				EffectKey: productionImportRunReceiptsServe2EffectKey,
				Kind:      productionImportRunReceiptsServe2Kind, Payload: seed.payload,
			}, nil)
		if err != nil {
			t.Fatalf("seed prepared receipt for %s: %v", seed.runID, err)
		}
		receipt, _ := prepared.(map[string]any)
		if receipt == nil || productionImportRunWriteText(receipt, "state") != "prepared" {
			t.Fatalf("the seeded receipt for %s is %v, want prepared", seed.runID, prepared)
		}
	}
}

// productionImportRunReceiptsServe2Result decodes one successful call's result.
func productionImportRunReceiptsServe2Result(t *testing.T, lane *productionImportRunReceiptsServe2Lane,
	operation string, args []any, into any) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%v: HTTP %v %v", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%v: %v %v", operation, err, response.Body.String())
	}
	if err := json.Unmarshal(wire.Result, into); err != nil {
		t.Fatalf("%v: %v %v", operation, err, string(wire.Result))
	}
}

// productionImportRunReceiptsServe2Refusal is one refused call: the HTTP status, the
// exact body, and the symbolic code the transport published.
func productionImportRunReceiptsServe2Refusal(t *testing.T, lane *productionImportRunReceiptsServe2Lane,
	operation string, args []any) (int, string, string) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	code := ""
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err == nil {
		code = wire.Error.Code
	}
	return response.Code, response.Body.String(), code
}

// productionImportRunReceiptsServe2ExpectRefusal drives one call and requires the
// exact status and symbolic code, returning the body for the byte comparisons.
func productionImportRunReceiptsServe2ExpectRefusal(t *testing.T, lane *productionImportRunReceiptsServe2Lane,
	operation string, args []any, status int, code, why string) string {
	t.Helper()
	gotStatus, body, gotCode := productionImportRunReceiptsServe2Refusal(t, lane, operation, args)
	if gotStatus != status || gotCode != code {
		t.Fatalf("%v answered %v %v under %v, want %v %v: %v",
			operation, gotStatus, gotCode, why, status, code, body)
	}
	return body
}

// productionImportRunReceiptsServe2Lease is the execution lease wire shape for one
// epoch. JSON numbers arrive as float64, so the epoch is one too.
func productionImportRunReceiptsServe2Lease(epoch int64) map[string]any {
	return map[string]any{"owner": productionImportGenerationExecutionOwner, "epoch": float64(epoch)}
}

// productionImportRunReceiptsServe2Handle is the four-key generationRunHandle the
// renderer sends (electron/services/__tests__/import-generation.test.ts:197).
func productionImportRunReceiptsServe2Handle(projectID, epoch, rootActionID, runID string) map[string]any {
	return map[string]any{
		"projectId": projectID, "epoch": epoch, "rootActionId": rootActionID, "runId": runID,
	}
}

// productionImportRunReceiptsServe2PrepareArgs is the prepare channel's exact wire
// argument list: request, execution, path, frozen context.
func productionImportRunReceiptsServe2PrepareArgs(runID string, payload map[string]any,
	execution map[string]any, projectPath string, frozen map[string]any) []any {
	return []any{
		map[string]any{
			"runId": runID, "stage": "style", "batchId": "done",
			"effectKey": productionImportRunReceiptsServe2EffectKey,
			"kind":      productionImportRunReceiptsServe2Kind,
			"payload":   payload,
		},
		execution,
		projectPath,
		frozen,
	}
}

// productionImportRunReceiptsServe2CommitArgs is the commit channel's exact wire
// argument list: runId, stage, batchId, execution, path, frozen context.
func productionImportRunReceiptsServe2CommitArgs(runID string, execution map[string]any,
	projectPath string, frozen map[string]any) []any {
	return []any{runID, "style", "done", execution, projectPath, frozen}
}

// productionImportRunReceiptsServe2Receipt reads the receipt row back over a
// SEPARATE mode=ro connection, so the assertion is about committed bytes rather than
// about the handle the lane wrote through.
func productionImportRunReceiptsServe2Receipt(t *testing.T, database, runID, what string) map[string]any {
	t.Helper()
	rows := productionImportRunWriteQuery(t, database, `SELECT run_id, state, effect_key, payload_hash,
 created_at, updated_at, effect_receipt_json FROM import_run_receipts
 WHERE run_id = ? AND stage = 'style' AND batch_id = 'done'`, runID)
	if len(rows) != 1 {
		t.Fatalf("%s: the receipt table holds %v rows for %s, want one", what, len(rows), runID)
	}
	return rows[0]
}

// productionImportRunReceiptsServe2Count is one COUNT(*) over a mode=ro connection.
func productionImportRunReceiptsServe2Count(t *testing.T, database, query string, args ...any) int64 {
	t.Helper()
	rows := productionImportRunWriteQuery(t, database, query, args...)
	if len(rows) != 1 {
		t.Fatalf("the count query answered %v rows", len(rows))
	}
	return productionImportRunWriteNumber(rows[0], "total")
}

// TestProductionImportRunReceiptsServe2Served is the whole served journey for both
// names on one run the lane really began and dispatched.
func TestProductionImportRunReceiptsServe2Served(t *testing.T) {
	lane := productionImportRunReceiptsServe2LaneOpen(t)
	var begun map[string]any
	productionImportRunReceiptsServe2Result(t, lane, "generation:begin",
		[]any{productionImportGenerationSelection("style", "receipt-serve2-begin-style"), lane.frozen}, &begun)
	handle := syntheticMap(begun["handle"])
	if handle == nil || syntheticString(handle["runId"]) == "" || syntheticString(handle["rootActionId"]) == "" {
		t.Fatalf("generation:begin answered no run handle for the style import slot: %v", begun)
	}
	generationRunID := syntheticString(handle["runId"])
	rootActionID := syntheticString(handle["rootActionId"])
	if syntheticString(handle["epoch"]) != lane.leaseID || syntheticString(handle["projectId"]) != lane.projectID {
		t.Fatalf("the begun handle is not this session's: %v", handle)
	}
	payload := map[string]any{
		"writingStyle": productionImportRunReceiptsServe2Style,
		"generationRunHandle": productionImportRunReceiptsServe2Handle(lane.projectID,
			syntheticString(handle["epoch"]), rootActionID, generationRunID),
	}
	live := productionImportRunReceiptsServe2Lease(productionImportGenerationExecutionEpoch)

	// The run has no attempt yet, and the reference refuses that. Nothing is written:
	// the proof runs before the receipt's INSERT.
	t.Run("prepare refuses a run with no stopped attempt", func(t *testing.T) {
		before := productionImportRunResidualImage(t, lane.database)
		body := productionImportRunReceiptsServe2ExpectRefusal(t, lane,
			productionImportRunReceiptsPrepareOperation,
			productionImportRunReceiptsServe2PrepareArgs(productionImportGenerationStyleRunID, payload,
				live, lane.projectPath, lane.frozen),
			http.StatusInternalServerError, productionImportRunReceiptsServe2SourceChanged,
			"a begun run with no attempt")
		if !strings.Contains(body, productionImportRunReceiptsServe2SourceChanged) {
			t.Fatalf("the no-attempt refusal does not carry its code: %v", body)
		}
		productionImportRunResidualAssertUnchanged(t, lane.database, before, "the no-attempt refusal")
		if total := productionImportRunReceiptsServe2Count(t, lane.database,
			`SELECT COUNT(*) AS total FROM import_run_receipts WHERE run_id = ?`,
			productionImportGenerationStyleRunID); total != 0 {
			t.Fatalf("the no-attempt refusal wrote %v receipts", total)
		}
	})

	// One real attempt, settled by the lane's own dispatch seam.
	var executed map[string]any
	productionImportRunReceiptsServe2Result(t, lane, "import-generation:execute",
		[]any{productionImportGenerationExecuteRequest(handle, "visible-text", 0), lane.frozen}, &executed)
	outcome := syntheticMap(executed["outcome"])
	if outcome == nil || syntheticString(outcome["finishReason"]) != "stop" ||
		syntheticString(outcome["status"]) != "completed" {
		t.Fatalf("the import dispatch did not settle one stopped attempt: %v", executed)
	}
	if total := productionImportRunReceiptsServe2Count(t, lane.database,
		`SELECT COUNT(*) AS total FROM generation_attempts WHERE run_id = ?`, generationRunID); total != 1 {
		t.Fatalf("the run holds %v attempts, want one", total)
	}

	var prepared map[string]any
	t.Run("prepare writes the receipt", func(t *testing.T) {
		// The project's own writing style BEFORE the pair runs. Prepare records the
		// receipt; the effect is the commit's, so this value must survive it.
		settledStyle := productionImportRunWriteQuery(t, lane.database,
			`SELECT writing_style FROM project_core WHERE id = 'main'`)
		if len(settledStyle) != 1 {
			t.Fatalf("the fixture has %v project_core rows, want one", len(settledStyle))
		}
		productionImportRunReceiptsServe2Result(t, lane, productionImportRunReceiptsPrepareOperation,
			productionImportRunReceiptsServe2PrepareArgs(productionImportGenerationStyleRunID, payload,
				live, lane.projectPath, lane.frozen), &prepared)
		if syntheticBool(prepared["success"]) != true {
			t.Fatalf("prepare did not answer a success envelope: %v", prepared)
		}
		receipt := syntheticMap(prepared["receipt"])
		if receipt == nil || syntheticString(receipt["state"]) != "prepared" ||
			syntheticString(receipt["effectKey"]) != productionImportRunReceiptsServe2EffectKey ||
			syntheticString(receipt["runId"]) != productionImportGenerationStyleRunID {
			t.Fatalf("prepare answered %v", prepared)
		}
		if syntheticString(receipt["payloadHash"]) == "" {
			t.Fatalf("the prepared receipt carries no payload hash: %v", receipt)
		}
		// Read back over its own mode=ro connection: the row is committed, the receipt
		// is not, and the ledger's shadow row is partial.
		row := productionImportRunReceiptsServe2Receipt(t, lane.database, productionImportGenerationStyleRunID, "after prepare")
		if productionImportRunWriteText(row, "state") != "prepared" ||
			productionImportRunWriteText(row, "payload_hash") != syntheticString(receipt["payloadHash"]) {
			t.Fatalf("the committed receipt row is %v", row)
		}
		ledger := productionImportRunWriteQuery(t, lane.database, `SELECT state, evidence_kind
 FROM import_effect_ledger WHERE run_id = ? AND stage = 'style' AND batch_id = 'done'`,
			productionImportGenerationStyleRunID)
		if len(ledger) != 1 || productionImportRunWriteText(ledger[0], "state") != "partial" ||
			productionImportRunWriteText(ledger[0], "evidence_kind") != "receipt" {
			t.Fatalf("the prepared effect ledger row is %v", ledger)
		}
		// The project's own writing style is untouched by the prepare: prepare records
		// the receipt, it does not apply the effect.
		style := productionImportRunWriteQuery(t, lane.database,
			`SELECT writing_style FROM project_core WHERE id = 'main'`)
		if len(style) != 1 ||
			productionImportRunWriteText(style[0], "writing_style") != productionImportRunWriteText(settledStyle[0], "writing_style") {
			t.Fatalf("prepare changed the project's writing style: before %v after %v", settledStyle, style)
		}
	})

	t.Run("prepare replay answers the same receipt and writes nothing", func(t *testing.T) {
		before := productionImportRunResidualImage(t, lane.database)
		settled := productionImportRunReceiptsServe2Receipt(t, lane.database, productionImportGenerationStyleRunID, "before prepare replay")
		var replayed map[string]any
		productionImportRunReceiptsServe2Result(t, lane, productionImportRunReceiptsPrepareOperation,
			productionImportRunReceiptsServe2PrepareArgs(productionImportGenerationStyleRunID, payload,
				live, lane.projectPath, lane.frozen), &replayed)
		first := syntheticMap(prepared["receipt"])
		replay := syntheticMap(replayed["receipt"])
		if replay == nil || first == nil ||
			syntheticString(replay["payloadHash"]) != syntheticString(first["payloadHash"]) ||
			syntheticString(replay["createdAt"]) != syntheticString(first["createdAt"]) ||
			syntheticString(replay["state"]) != "prepared" {
			t.Fatalf("the prepare replay answered %v, want the prepared receipt", replayed)
		}
		after := productionImportRunReceiptsServe2Receipt(t, lane.database, productionImportGenerationStyleRunID, "after prepare replay")
		if productionImportRunWriteText(after, "payload_hash") != productionImportRunWriteText(settled, "payload_hash") ||
			productionImportRunWriteText(after, "created_at") != productionImportRunWriteText(settled, "created_at") ||
			productionImportRunWriteText(after, "state") != "prepared" {
			t.Fatalf("the prepare replay rewrote the receipt: before %v after %v", settled, after)
		}
		productionImportRunResidualAssertUnchanged(t, lane.database, before, "the prepare replay")
	})

	t.Run("commit applies the effect and records the receipt", func(t *testing.T) {
		var committed map[string]any
		productionImportRunReceiptsServe2Result(t, lane, productionImportRunReceiptsCommitOperation,
			productionImportRunReceiptsServe2CommitArgs(productionImportGenerationStyleRunID, live,
				lane.projectPath, lane.frozen), &committed)
		if syntheticBool(committed["success"]) != true {
			t.Fatalf("commit did not answer a success envelope: %v", committed)
		}
		result := syntheticMap(committed["result"])
		if result == nil {
			t.Fatalf("commit answered no result: %v", committed)
		}
		receipt := syntheticMap(result["receipt"])
		if receipt == nil || syntheticString(receipt["state"]) != "committed" {
			t.Fatalf("the committed receipt is %v", result["receipt"])
		}
		if syntheticBool(result["cancelApplied"]) {
			t.Fatalf("commit reported a cancelled import run: %v", result)
		}
		run := syntheticMap(result["run"])
		if run == nil || syntheticString(run["id"]) != productionImportGenerationStyleRunID {
			t.Fatalf("commit projected another run: %v", result["run"])
		}
		row := productionImportRunReceiptsServe2Receipt(t, lane.database, productionImportGenerationStyleRunID, "after commit")
		if productionImportRunWriteText(row, "state") != "committed" ||
			!strings.Contains(productionImportRunWriteText(row, "effect_receipt_json"), productionImportRunReceiptsServe2Style) {
			t.Fatalf("the committed receipt row is %v", row)
		}
		style := productionImportRunWriteQuery(t, lane.database,
			`SELECT writing_style FROM project_core WHERE id = 'main'`)
		if len(style) != 1 || productionImportRunWriteText(style[0], "writing_style") != productionImportRunReceiptsServe2Style {
			t.Fatalf("the commit did not write the project's writing style: %v", style)
		}
		ledger := productionImportRunWriteQuery(t, lane.database, `SELECT state
 FROM import_effect_ledger WHERE run_id = ? AND stage = 'style' AND batch_id = 'done'`,
			productionImportGenerationStyleRunID)
		if len(ledger) != 1 || productionImportRunWriteText(ledger[0], "state") != "complete" {
			t.Fatalf("the committed effect ledger row is %v", ledger)
		}
	})

	t.Run("commit replay answers the committed receipt and writes nothing", func(t *testing.T) {
		before := productionImportRunResidualImage(t, lane.database)
		settled := productionImportRunReceiptsServe2Receipt(t, lane.database, productionImportGenerationStyleRunID, "before commit replay")
		var replayed map[string]any
		productionImportRunReceiptsServe2Result(t, lane, productionImportRunReceiptsCommitOperation,
			productionImportRunReceiptsServe2CommitArgs(productionImportGenerationStyleRunID, live,
				lane.projectPath, lane.frozen), &replayed)
		result := syntheticMap(replayed["result"])
		receipt := syntheticMap(result["receipt"])
		if receipt == nil || syntheticString(receipt["state"]) != "committed" {
			t.Fatalf("the commit replay answered %v, want the committed receipt", replayed)
		}
		after := productionImportRunReceiptsServe2Receipt(t, lane.database, productionImportGenerationStyleRunID, "after commit replay")
		if productionImportRunWriteText(after, "effect_receipt_json") != productionImportRunWriteText(settled, "effect_receipt_json") ||
			productionImportRunWriteText(after, "updated_at") != productionImportRunWriteText(settled, "updated_at") {
			t.Fatalf("the commit replay rewrote the receipt: before %v after %v", settled, after)
		}
		productionImportRunResidualAssertUnchanged(t, lane.database, before, "the commit replay")
	})
}

// TestProductionImportRunReceiptsServe2RefusesAForeignRun is the identity and slot
// half of the reference's assert over runs the lane really began. Both refusals need
// the store's evidence: a prover that accepted any handle would answer success in
// both.
func TestProductionImportRunReceiptsServe2RefusesAForeignRun(t *testing.T) {
	lane := productionImportRunReceiptsServe2LaneOpen(t)
	var begun map[string]any
	productionImportRunReceiptsServe2Result(t, lane, "generation:begin",
		[]any{productionImportGenerationSelection("style", "receipt-serve2-foreign-begin"), lane.frozen}, &begun)
	handle := syntheticMap(begun["handle"])
	generationRunID := syntheticString(handle["runId"])
	rootActionID := syntheticString(handle["rootActionId"])
	epoch := syntheticString(handle["epoch"])
	if generationRunID == "" || rootActionID == "" || epoch == "" {
		t.Fatalf("generation:begin answered no run handle: %v", begun)
	}
	// The slot arm needs a REAL run frozen for the global slot. It is begun here,
	// BEFORE the byte image is captured, because a begin legitimately writes its own
	// root and run; only the two refusals must leave the image untouched.
	var global map[string]any
	productionImportRunReceiptsServe2Result(t, lane, "generation:begin",
		[]any{productionImportGenerationSelection("global", "receipt-serve2-global-begin"), lane.frozen}, &global)
	globalHandle := syntheticMap(global["handle"])
	if globalHandle == nil || syntheticString(globalHandle["runId"]) == "" {
		t.Fatalf("the global begin answered no run handle: %v", global)
	}
	live := productionImportRunReceiptsServe2Lease(productionImportGenerationExecutionEpoch)
	before := productionImportRunResidualImage(t, lane.database)

	// The handle names this run, but not its own frozen epoch: requireRun's identity
	// comparison (electron/services/main-generation-owner.ts:158-161).
	drifted := productionImportRunReceiptsServe2ExpectRefusal(t, lane,
		productionImportRunReceiptsPrepareOperation,
		productionImportRunReceiptsServe2PrepareArgs(productionImportGenerationStyleRunID,
			map[string]any{"writingStyle": productionImportRunReceiptsServe2Style,
				"generationRunHandle": productionImportRunReceiptsServe2Handle(lane.projectID,
					epoch+"-drifted", rootActionID, generationRunID)},
			live, lane.projectPath, lane.frozen),
		http.StatusInternalServerError, productionImportRunReceiptsServe2Identity,
		"a handle with a drifted epoch")
	if !strings.Contains(drifted, productionImportRunReceiptsServe2Identity) {
		t.Fatalf("the identity refusal does not carry its code: %v", drifted)
	}

	// The handle is a real run, but frozen for another import slot: :1462.
	slotChanged := productionImportRunReceiptsServe2ExpectRefusal(t, lane,
		productionImportRunReceiptsPrepareOperation,
		productionImportRunReceiptsServe2PrepareArgs(productionImportGenerationStyleRunID,
			map[string]any{"writingStyle": productionImportRunReceiptsServe2Style,
				"generationRunHandle": productionImportRunReceiptsServe2Handle(lane.projectID,
					syntheticString(globalHandle["epoch"]), syntheticString(globalHandle["rootActionId"]),
					syntheticString(globalHandle["runId"]))},
			live, lane.projectPath, lane.frozen),
		http.StatusInternalServerError, productionImportRunReceiptsServe2SlotChanged,
		"a handle frozen for the global slot")
	if !strings.Contains(slotChanged, productionImportRunReceiptsServe2SlotChanged) {
		t.Fatalf("the slot refusal does not carry its code: %v", slotChanged)
	}

	// Both refusals happen inside the transaction and neither wrote anything.
	productionImportRunResidualAssertUnchanged(t, lane.database, before, "the identity and slot refusals")
}

// TestProductionImportRunReceiptsServe2RefusalsAreByteIdentical compares each served
// name's refusal with the name that already owns the same fence, and the absent-run
// refusal of the two names with each other.
func TestProductionImportRunReceiptsServe2RefusalsAreByteIdentical(t *testing.T) {
	lane := productionImportRunReceiptsServe2LaneOpen(t)
	stale := productionImportRunReceiptsServe2Lease(99)
	live := productionImportRunReceiptsServe2Lease(productionImportGenerationExecutionEpoch)
	style := productionImportRunReceiptsServe2Style
	absentPayload := map[string]any{"writingStyle": style,
		"generationRunHandle": productionImportRunReceiptsServe2Handle(lane.projectID,
			"receipt-serve2-ghost-epoch", "receipt-serve2-ghost-root", productionImportRunReceiptsServe2GhostRun)}
	seeded := productionImportRunReceiptsServe2Receipt(t, lane.database,
		productionImportRunReceiptsServe2AbsentCommitRun, "the seeded absent-run receipt")
	if productionImportRunWriteText(seeded, "state") != "prepared" {
		t.Fatalf("the absent-run receipt is %s, want prepared", productionImportRunWriteText(seeded, "state"))
	}
	settled := productionImportRunResidualImage(t, lane.database)

	t.Run("stale execution lease matches db:import-run-fail", func(t *testing.T) {
		before := productionImportRunResidualImage(t, lane.database)
		reference := productionImportRunReceiptsServe2ExpectRefusal(t, lane, "db:import-run-fail",
			[]any{productionImportRunReceiptsServe2StaleRun, "style", "the executor went away",
				stale, lane.projectPath, lane.frozen},
			http.StatusInternalServerError, "IMPORT_RUN_EXECUTION_STALE", "the stale-lease reference")
		for _, run := range []struct {
			name string
			args []any
		}{
			{productionImportRunReceiptsPrepareOperation, productionImportRunReceiptsServe2PrepareArgs(
				productionImportRunReceiptsServe2StaleRun,
				map[string]any{"writingStyle": style}, stale, lane.projectPath, lane.frozen)},
			{productionImportRunReceiptsCommitOperation, productionImportRunReceiptsServe2CommitArgs(
				productionImportRunReceiptsServe2StaleRun, stale, lane.projectPath, lane.frozen)},
		} {
			body := productionImportRunReceiptsServe2ExpectRefusal(t, lane, run.name, run.args,
				http.StatusInternalServerError, "IMPORT_RUN_EXECUTION_STALE", "a stale execution lease")
			if body != reference {
				t.Fatalf("%v answered a different body than db:import-run-fail under a stale lease:\n reference: %v\n got:       %v",
					run.name, reference, body)
			}
		}
		productionImportRunResidualAssertUnchanged(t, lane.database, before, "the stale-lease arm")
	})

	t.Run("foreign project path matches db:import-run-finalize-parsing", func(t *testing.T) {
		before := productionImportRunResidualImage(t, lane.database)
		foreign := lane.projectPath + "-foreign"
		reference := productionImportRunReceiptsServe2ExpectRefusal(t, lane, "db:import-run-finalize-parsing",
			[]any{productionImportRunReceiptsServe2StaleRun, foreign, lane.frozen},
			http.StatusConflict, "PROJECT_CONTEXT_CHANGED", "the foreign-path reference")
		for _, run := range []struct {
			name string
			args []any
		}{
			{productionImportRunReceiptsPrepareOperation, productionImportRunReceiptsServe2PrepareArgs(
				productionImportRunReceiptsServe2StaleRun,
				map[string]any{"writingStyle": style}, live, foreign, lane.frozen)},
			{productionImportRunReceiptsCommitOperation, productionImportRunReceiptsServe2CommitArgs(
				productionImportRunReceiptsServe2StaleRun, live, foreign, lane.frozen)},
		} {
			body := productionImportRunReceiptsServe2ExpectRefusal(t, lane, run.name, run.args,
				http.StatusConflict, "PROJECT_CONTEXT_CHANGED", "a foreign project path")
			if body != reference {
				t.Fatalf("%v answered a different body than db:import-run-finalize-parsing under a foreign path:\n reference: %v\n got:       %v",
					run.name, reference, body)
			}
		}
		productionImportRunResidualAssertUnchanged(t, lane.database, before, "the foreign-path arm")
	})

	t.Run("absent generation run is one answer on both names", func(t *testing.T) {
		before := productionImportRunResidualImage(t, lane.database)
		prepared := productionImportRunReceiptsServe2ExpectRefusal(t, lane,
			productionImportRunReceiptsPrepareOperation,
			productionImportRunReceiptsServe2PrepareArgs(productionImportRunReceiptsServe2AbsentPrepareRun,
				absentPayload, live, lane.projectPath, lane.frozen),
			http.StatusInternalServerError, productionImportRunReceiptsServe2RunMissing,
			"a handle naming no run")
		// The same refusal, twice, is the same bytes: the assert does not vary its
		// answer with anything but the evidence.
		again := productionImportRunReceiptsServe2ExpectRefusal(t, lane,
			productionImportRunReceiptsPrepareOperation,
			productionImportRunReceiptsServe2PrepareArgs(productionImportRunReceiptsServe2AbsentPrepareRun,
				absentPayload, live, lane.projectPath, lane.frozen),
			http.StatusInternalServerError, productionImportRunReceiptsServe2RunMissing,
			"a repeated absent-run prepare")
		if again != prepared {
			t.Fatalf("two absent-run prepares answered different bytes:\n first:  %v\n second: %v", prepared, again)
		}
		committed := productionImportRunReceiptsServe2ExpectRefusal(t, lane,
			productionImportRunReceiptsCommitOperation,
			productionImportRunReceiptsServe2CommitArgs(productionImportRunReceiptsServe2AbsentCommitRun,
				live, lane.projectPath, lane.frozen),
			http.StatusInternalServerError, productionImportRunReceiptsServe2RunMissing,
			"a prepared receipt whose run is absent")
		if committed != prepared {
			t.Fatalf("prepare and commit answered an absent run differently:\n prepare: %v\n commit:  %v",
				prepared, committed)
		}
		if !strings.Contains(prepared, productionImportRunReceiptsServe2RunMissing) {
			t.Fatalf("the absent-run refusal does not carry its code: %v", prepared)
		}
		productionImportRunResidualAssertUnchanged(t, lane.database, before, "the absent-run arm")
	})

	t.Run("nothing was written and the seeded receipts stayed prepared", func(t *testing.T) {
		productionImportRunResidualAssertUnchanged(t, lane.database, settled, "the whole refusal matrix")
		for _, run := range []struct {
			id   string
			want int64
		}{
			{productionImportRunReceiptsServe2AbsentPrepareRun, 0},
			{productionImportRunReceiptsServe2AbsentCommitRun, 1},
			{productionImportRunReceiptsServe2StaleRun, 1},
		} {
			total := productionImportRunReceiptsServe2Count(t, lane.database,
				`SELECT COUNT(*) AS total FROM import_run_receipts WHERE run_id = ?`, run.id)
			if total != run.want {
				t.Fatalf("%s holds %v receipts after the refusals, want %v", run.id, total, run.want)
			}
		}
	})
}

// TestProductionImportRunReceiptsServe2BinderGatesTheOwner pins the binder's
// contract: an unavailable owner installs nothing (so both names stay at 501 rather
// than being published as handlers that could only refuse), a nil server is refused,
// and every name it installs is inventoried.
func TestProductionImportRunReceiptsServe2BinderGatesTheOwner(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	t.Cleanup(server.Close)
	if err := newProductionImportRunReceiptsServe2(nil).Bind(server); err != nil {
		t.Fatalf("a nil owner's binder failed: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the ownerless binder installed %v, want nothing", names)
	}
	if err := newProductionImportRunReceiptsServe2(&productionGeneration{}).Bind(server); err != nil {
		t.Fatalf("an empty owner's binder failed: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the empty owner's binder installed %v, want nothing", names)
	}
	if err := newProductionImportRunReceiptsServe2(nil).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
	if !productionImportRunReceiptsIsFamily(productionImportRunReceiptsPrepareOperation) ||
		!productionImportRunReceiptsIsFamily(productionImportRunReceiptsCommitOperation) ||
		!productionImportRunReceiptsIsFamily(productionImportRunReceiptsInspectionOperation) {
		t.Fatal("the family check no longer admits the three import-run receipt names")
	}
	// Every name this binder installs must be inventoried, or its unbound answer would
	// be 404 OPERATION_NOT_EXPOSED instead of 501.
	fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	t.Cleanup(fresh.Close)
	for _, name := range []string{
		productionImportRunReceiptsPrepareOperation,
		productionImportRunReceiptsCommitOperation,
	} {
		if err := fresh.Bind(name, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%v is not inventoried: %v", name, err)
		}
	}
}

// TestProductionImportRunReceiptsServe2WiringRecord pins the one edit the shipped
// lane still needs, in both directions: the owner block and its anchor are where the
// record says they are, and the reported line is the literal call the binder takes.
// It deliberately does not assert that the line is absent - the pass that installs it
// must not have to edit this test - and it logs whether it is already installed
// instead of failing on either answer.
func TestProductionImportRunReceiptsServe2WiringRecord(t *testing.T) {
	raw, err := os.ReadFile("production_open_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, marker := range []string{
		productionImportRunReceiptsOwnerBlockLine,
		productionImportRunReceiptsOwnerLateOwnerLine,
		productionImportRunReceiptsOwnerResidualBinder,
	} {
		if !productionSeamContains(source, marker) {
			t.Fatalf("production_open_linux.go no longer contains %v", marker)
		}
	}
	compact := productionSeamCompact(source)
	block := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerBlockLine))
	anchor := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerLateOwnerLine))
	if block < 0 || anchor < 0 || block >= anchor {
		t.Fatalf("the owner block no longer opens before its last owner-bound binder: %v %v", block, anchor)
	}
	wiring := productionImportRunReceiptsOwnerWiringLineOf()
	if wiring.Path != "production_open_linux.go" || wiring.Anchor != productionImportRunReceiptsOwnerLateOwnerLine ||
		wiring.Line != productionImportRunReceiptsOwnerServe2Binder {
		t.Fatalf("the wiring record drifted: %v", wiring)
	}
	report := productionImportRunReceiptsOwnerReport()
	if !strings.Contains(report, productionImportRunReceiptsOwnerServe2Binder) ||
		!strings.Contains(report, wiring.Path) {
		t.Fatalf("the wiring report does not name the file and the line: %v", report)
	}
	if !strings.Contains(productionImportRunReceiptsOwnerBinderInstall, productionImportRunReceiptsOwnerServe2Binder) {
		t.Fatalf("the install idiom does not name the binder line: %v", productionImportRunReceiptsOwnerBinderInstall)
	}
	t.Logf("receipt binder installed in the shipped lane: %v", productionImportRunReceiptsOwnerInstalled())
}

// TestProductionImportRunReceiptsServe2SeamIsGone pins the seam and the decision
// table against the two served names: neither may keep a seam row, both must be
// decided served, and the one remaining refusal must still have its row.
func TestProductionImportRunReceiptsServe2SeamIsGone(t *testing.T) {
	served := map[string]bool{
		productionImportRunReceiptsPrepareOperation: true,
		productionImportRunReceiptsCommitOperation:  true,
	}
	decided := make(map[string]productionImportRunReceiptServeDecision)
	for _, decision := range productionImportRunReceiptServeDecisions() {
		decided[decision.Operation] = decision
	}
	for name := range served {
		decision, present := decided[name]
		if !present {
			t.Fatalf("%v has no serve decision", name)
		}
		if !decision.Served {
			t.Fatalf("%v is decided refused (%v) while the family serves it", name, decision.Reason)
		}
		if decision.Reason != "" {
			t.Fatalf("%v is served and still records a reason: %v", name, decision.Reason)
		}
		if _, seamed := productionImportRunReceiptSeamFor(name); seamed {
			t.Fatalf("%v is served and still has a seam row", name)
		}
	}
	expected := []string{
		productionImportRunReceiptsCommitOperation,
		productionImportRunReceiptsPrepareOperation,
	}
	got := productionImportRunReceiptServedOperations()
	if len(got) != len(expected) {
		t.Fatalf("the served set is %v, want %v", got, expected)
	}
	for index := range expected {
		if got[index] != expected[index] {
			t.Fatalf("the served set is %v, want %v", got, expected)
		}
	}
	inspection, seamed := productionImportRunReceiptSeamFor(productionImportRunReceiptsInspectionOperation)
	if !seamed {
		t.Fatalf("%v lost its seam row", productionImportRunReceiptsInspectionOperation)
	}
	if decided[inspection.Operation].Served {
		t.Fatalf("%v is decided served while nothing serves it", inspection.Operation)
	}
	if err := productionImportRunReceiptServeAgrees(productionImportRunReceiptServeDecisions(),
		productionImportRunReceiptSeams()); err != nil {
		t.Fatalf("the decision and seam tables disagree: %v", err)
	}
}

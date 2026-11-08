//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file drives the two names production_import_generation_serve_linux.go
// serves over the real HTTP transport and the frozen project session, with the
// model stubbed at the lane's own dispatch seam:
//
//	generation:begin + importSlot  -> import-generation:read   -> the same run
//	import-generation:execute      -> one settled attempt      -> {outcome, run}
//
// The fixture is the editor-read root, which is a copy of the admitted v7
// baseline: import_runs and import_run_chapters already exist there, so the
// import material is seeded as rows rather than as schema, and the admission
// fingerprint is untouched. The two sibling names bound beside the served ones
// (generation:execute and finalization-generation:read) are the reference for
// the byte-identity arms below: the import family must answer a stale lease and
// a foreign project with exactly what those names answer, because it is the same
// fence and the same envelope.
const (
	productionImportGenerationLaneOpenNonce = "import-generation-served-open-1"
	// productionImportGenerationExecutionOwner and ...ExecutionEpoch are the
	// execution lease the seeded import runs hold.
	productionImportGenerationExecutionOwner = "import-generation-served-owner"
	productionImportGenerationExecutionEpoch = int64(7)
	// productionImportGenerationLeaseExpiry is a safe-integer instant no test run
	// reaches, so the seeded lease is live.
	productionImportGenerationLeaseExpiry = int64(9007199254740991)
	// productionImportGenerationManifestFingerprint is the durable manifest hash
	// captureImportGenerationContext requires (a 64-hex digest).
	productionImportGenerationManifestFingerprint = "9f2c0f6a5b1d4c3e8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d2e1f0a9b8c7d6e5f"
	// The two seeded import runs: one per stage this lane can freeze.
	productionImportGenerationGlobalRunID = "import-generation-served-global"
	productionImportGenerationStyleRunID  = "import-generation-served-style"
	// The seeded chapter bodies. A two-chapter run is inside every stage's
	// selection window, so the global stage freezes both and the style stage all.
	productionImportGenerationGlobalFirst = "第一章 参考正文：门后有人。"
	productionImportGenerationGlobalLast  = "第二章 参考正文：灯下无影。"
	productionImportGenerationStyleFirst  = "第一章 参考文风：雨夜。"
	productionImportGenerationStyleLast   = "第二章 参考文风：长街。"
	// productionImportGenerationServedText is what the stubbed model answers.
	productionImportGenerationServedText = "导入推演：门后有人，灯下无影。"
)

// productionImportGenerationSeam substitutes the native model dispatch for every
// dispatch of this lane and always stops, so a stage's ordinal chain is exactly
// one completed attempt per execute.
type productionImportGenerationSeam struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (s *productionImportGenerationSeam) dispatch(_ context.Context, _ any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if callbacks.OnVisible != nil {
		callbacks.OnVisible(generation.VisibleEvent{Kind: "cumulative", Text: s.text})
	}
	// The reduced lane plans an "unknown" reasoning policy, so the settled usage
	// has to satisfy total == prompt+completion and reasoning <= completion.
	return generation.DispatchResult{
		Usage:        map[string]any{"trusted": true, "promptTokens": 4, "completionTokens": 16, "reasoningTokens": 0, "totalTokens": 20},
		FinishReason: "stop",
	}, nil
}

func (s *productionImportGenerationSeam) dispatched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// productionImportGenerationLane is one opened lane: the served generation names,
// the two import names, and the frozen session every call carries.
type productionImportGenerationLane struct {
	projectPath string
	database    string
	projectID   string
	leaseID     string
	author      *productionDurableClient
	frozen      map[string]any
	seam        *productionImportGenerationSeam
}

// productionImportGenerationLaneOpen builds the fixture, seeds the import rows,
// opens the production lane and mounts the names this file drives on one
// transport server. The dispatch seam replaces the model call; nothing dials.
func productionImportGenerationLaneOpen(t *testing.T) *productionImportGenerationLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	_, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	productionImportGenerationSeed(t, projectPath)
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
		"go-production-import-generation")
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
		{"import-generation:read", func() error { return newProductionImportGenerationServe(owner).Bind(server) }},
		{"finalization-generation:read", func() error { return newProductionGraphReads(open.durable).Bind(server) }},
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
	author, frozen := productionExecuteSession(t, mux, projectPath, productionImportGenerationLaneOpenNonce)
	return &productionImportGenerationLane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		projectID:   syntheticString(frozen["projectId"]),
		leaseID:     syntheticString(frozen["leaseId"]),
		author:      author,
		frozen:      frozen,
		seam:        seam,
	}
}

// productionImportGenerationSeed writes the two import runs and their chapters
// through the baseline's own columns. The rows are the material
// captureImportGenerationContext reads; the schema is the admitted baseline, so
// the fingerprint of the file is unchanged.
func productionImportGenerationSeed(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	type chapter struct {
		number  int64
		title   string
		content string
	}
	for _, run := range []struct {
		id       string
		stage    string
		chapters []chapter
	}{
		{productionImportGenerationGlobalRunID, "global", []chapter{
			{1, "第一章", productionImportGenerationGlobalFirst},
			{2, "第二章", productionImportGenerationGlobalLast},
		}},
		{productionImportGenerationStyleRunID, "style", []chapter{
			{1, "第一章", productionImportGenerationStyleFirst},
			{2, "第二章", productionImportGenerationStyleLast},
		}},
	} {
		if _, err := db.Exec(`INSERT INTO import_runs (id, purpose, root_run_id, effect_namespace,
 source_fingerprint, manifest_fingerprint, locale, stage, status, execution_owner, execution_epoch,
 lease_expires_at, total_chapters, manifest_chapter_count, manifest_word_count, corpus_kind, state,
 cancel_requested)
 VALUES (?, 'reference', ?, ?, ?, ?, 'zh-CN', ?, 'running', ?, ?, ?, ?, ?, ?, 'reference', 'committed', 0)`,
			run.id, run.id+"-root", "import:"+run.id,
			strings.Repeat("a", 64), productionImportGenerationManifestFingerprint,
			run.stage, productionImportGenerationExecutionOwner, productionImportGenerationExecutionEpoch,
			productionImportGenerationLeaseExpiry, int64(len(run.chapters)), int64(len(run.chapters)),
			int64(12)); err != nil {
			t.Fatalf("seed import run %s: %v", run.id, err)
		}
		for _, chapter := range run.chapters {
			if _, err := db.Exec(`INSERT INTO import_run_chapters (run_id, chapter_number, title,
 content_fingerprint, content_size, content_snapshot) VALUES (?, ?, ?, ?, ?, ?)`,
				run.id, chapter.number, chapter.title, syntheticHash(chapter.content),
				int64(len(chapter.content)), chapter.content); err != nil {
				t.Fatalf("seed import chapter %s/%v: %v", run.id, chapter.number, err)
			}
		}
	}
}

// productionImportGenerationSlot is one seed's slot: a global or style stage with
// the "done" batch id validateImportGenerationSlot requires.
func productionImportGenerationSlot(runID, stage string) map[string]any {
	return map[string]any{"runId": runID, "stage": stage, "batchId": "done"}
}

// productionImportGenerationSelection is the reference's begin selection for one
// stage, in the shape the renderer sends
// (src/services/workflows/commands/import-novel.command.ts:211,
// src/services/workflows/commands/analyze-style.command.ts:44).
func productionImportGenerationSelection(stage, nonce string) map[string]any {
	selection := map[string]any{
		"uiActionNonce":                   nonce,
		"modelId":                         productionTestModelID,
		"selectedDraftIds":                []any{},
		"selectedFinalizedDraftIds":       []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"authorInputs":                    []any{},
		"importExecution": map[string]any{
			"owner": productionImportGenerationExecutionOwner,
			"epoch": float64(productionImportGenerationExecutionEpoch),
		},
	}
	if stage == "style" {
		selection["operation"] = productionImportWritingStyleOperation
		selection["promptKeys"] = []any{"analyze_writing_style"}
		selection["skillStages"] = []any{}
		selection["output"] = "visible-text"
		selection["importSlot"] = productionImportGenerationSlot(productionImportGenerationStyleRunID, "style")
		return selection
	}
	selection["operation"] = productionImportGlobalFactsOperation
	selection["promptKeys"] = []any{"infer_novel_config_with_vectors", "infer_novel_config"}
	selection["skillStages"] = []any{"planning"}
	selection["output"] = "structured-data"
	selection["importSlot"] = productionImportGenerationSlot(productionImportGenerationGlobalRunID, "global")
	return selection
}

// productionImportGenerationDecoded is one successful call's result payload.
func productionImportGenerationDecoded(t *testing.T, lane *productionImportGenerationLane, operation string, args []any, into any) {
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

// productionImportGenerationRefusal is one refused business call: the HTTP status
// and the exact body, which is what a byte-identical refusal is compared on.
func productionImportGenerationRefusal(t *testing.T, lane *productionImportGenerationLane, operation string, args []any) (int, string) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	return response.Code, response.Body.String()
}

// productionImportGenerationBusinessRefusal asserts the mutating envelope's
// symbolic refusal and returns it.
func productionImportGenerationBusinessRefusal(t *testing.T, lane *productionImportGenerationLane, operation string, args []any) string {
	t.Helper()
	response := lane.author.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%v: HTTP %v %v", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%v: %v %v", operation, err, response.Body.String())
	}
	if wire.Result.Success || !strings.HasPrefix(wire.Result.Error, "Error: ") {
		t.Fatalf("%v was not refused through the business envelope: %v", operation, response.Body.String())
	}
	return strings.TrimPrefix(wire.Result.Error, "Error: ")
}

// TestProductionImportGenerationServed is the whole served journey for both
// stages this lane can freeze: begin with the import selection, read the run the
// slot owns, execute one attempt and settle it.
func TestProductionImportGenerationServed(t *testing.T) {
	lane := productionImportGenerationLaneOpen(t)
	for _, stage := range []struct {
		name       string
		runID      string
		kind       string
		operation  string
		output     string
		chapters   int
		first      string
		last       string
		nonce      string
	}{
		{"global", productionImportGenerationGlobalRunID, "global", productionImportGlobalFactsOperation,
			"structured-data", 2, productionImportGenerationGlobalFirst, productionImportGenerationGlobalLast,
			"import-generation-served-begin-global"},
		{"style", productionImportGenerationStyleRunID, "style", productionImportWritingStyleOperation,
			"visible-text", 2, productionImportGenerationStyleFirst, productionImportGenerationStyleLast,
			"import-generation-served-begin-style"},
	} {
		stage := stage
		t.Run(stage.name, func(t *testing.T) {
			slot := productionImportGenerationSlot(stage.runID, stage.kind)
			var begun map[string]any
			productionImportGenerationDecoded(t, lane, "generation:begin",
				[]any{productionImportGenerationSelection(stage.kind, stage.nonce), lane.frozen}, &begun)
			handle := syntheticMap(begun["handle"])
			if handle == nil || syntheticString(handle["runId"]) == "" ||
				syntheticString(handle["rootActionId"]) == "" || syntheticString(handle["epoch"]) != lane.leaseID {
				t.Fatalf("generation:begin answered no handle for this lease: %v", begun)
			}
			if syntheticString(begun["operation"]) != stage.operation {
				t.Fatalf("the begun run froze operation %v, want %v", begun["operation"], stage.operation)
			}
			// The three frozen members the ledger now admits for this operation.
			if rows := productionFinalizationGenerationCount(t, lane.database,
				`SELECT COUNT(*) FROM generation_runs WHERE run_id=? AND json_extract(binding_json,'$.sourceManifest.importSlotKey') IS NOT NULL
 AND json_extract(binding_json,'$.sourceManifest.importSlot.runId')=? AND json_extract(binding_json,'$.sourceManifest.importSlot.stage')=?
 AND json_extract(binding_json,'$.sourceManifest.importContext.slot.runId')=?`,
				syntheticString(handle["runId"]), stage.runID, stage.kind, stage.runID); rows != 1 {
				t.Fatalf("the frozen manifest carries %v import members, want one complete set", rows)
			}

			// read hands back the run the begin opened, its model and the frozen
			// context the renderer renders from.
			var recovery struct {
				View          map[string]any `json:"view"`
				ModelID       string         `json:"modelId"`
				FrozenContext map[string]any `json:"frozenContext"`
			}
			productionImportGenerationDecoded(t, lane, productionImportGenerationRead,
				[]any{map[string]any{"slot": slot}, lane.frozen}, &recovery)
			readHandle := syntheticMap(recovery.View["handle"])
			if readHandle == nil || syntheticString(readHandle["runId"]) != syntheticString(handle["runId"]) {
				t.Fatalf("import-generation:read answered %v, want the run begin opened (%v)", readHandle, handle)
			}
			if recovery.ModelID != productionTestModelID {
				t.Fatalf("import-generation:read answered model %v, want the frozen model", recovery.ModelID)
			}
			frozenSlot := syntheticMap(recovery.FrozenContext["slot"])
			if syntheticString(frozenSlot["runId"]) != stage.runID || syntheticString(frozenSlot["stage"]) != stage.kind ||
				syntheticString(frozenSlot["batchId"]) != "done" {
				t.Fatalf("the frozen context names another slot: %v", frozenSlot)
			}
			if syntheticString(recovery.FrozenContext["manifestFingerprint"]) != productionImportGenerationManifestFingerprint {
				t.Fatalf("the frozen context lost the import manifest: %v", recovery.FrozenContext["manifestFingerprint"])
			}
			if syntheticNumber(recovery.FrozenContext["totalChapters"]) != int64(stage.chapters) ||
				syntheticNumber(recovery.FrozenContext["totalWords"]) != 12 {
				t.Fatalf("the frozen context lost the run's counts: %v", recovery.FrozenContext)
			}
			if syntheticMap(recovery.FrozenContext["core"]) == nil {
				t.Fatalf("the frozen context carries no project core: %v", recovery.FrozenContext)
			}
			chapters := syntheticSlice(recovery.FrozenContext["chapters"])
			if len(chapters) != stage.chapters {
				t.Fatalf("the frozen context carries %v chapters, want %v", len(chapters), stage.chapters)
			}
			first := syntheticMap(chapters[0])
			last := syntheticMap(chapters[len(chapters)-1])
			if syntheticString(first["content"]) != stage.first || syntheticString(last["content"]) != stage.last {
				t.Fatalf("the frozen chapters are not the seeded ones: %v", chapters)
			}
			if syntheticHash(syntheticString(first["content"])) != syntheticString(first["contentFingerprint"]) {
				t.Fatalf("the frozen chapter lost its fingerprint: %v", first)
			}

			// execute dispatches the browser's task over the run's frozen lease.
			var receipt map[string]any
			productionImportGenerationDecoded(t, lane, productionImportGenerationExecute,
				[]any{productionImportGenerationExecuteRequest(handle, stage.output, 0), lane.frozen}, &receipt)
			outcome := syntheticMap(receipt["outcome"])
			if outcome == nil || syntheticString(outcome["status"]) != "completed" ||
				syntheticString(outcome["finishReason"]) != "stop" ||
				syntheticString(outcome["content"]) != productionImportGenerationServedText {
				t.Fatalf("import-generation:execute outcome = %v", receipt["outcome"])
			}
			run := syntheticMap(receipt["run"])
			if run == nil || syntheticString(run["operation"]) != stage.operation {
				t.Fatalf("import-generation:execute returned the wrong run: %v", receipt["run"])
			}
			if attempts := productionFinalizationGenerationCount(t, lane.database,
				`SELECT COUNT(*) FROM generation_attempts WHERE run_id=? AND invocation_nonce='import:0'`,
				syntheticString(handle["runId"])); attempts != 1 {
				t.Fatalf("the execute settled %v attempts at import:0, want one", attempts)
			}
		})
	}
	if lane.seam.dispatched() != 2 {
		t.Fatalf("the two stages dispatched %v times through the seam, want two", lane.seam.dispatched())
	}
}

// productionImportGenerationExecuteRequest is the reference's execute request
// (src/shared/import-generation.ts:29): the handle, the ordinal, the task the
// renderer rendered and the import execution lease.
func productionImportGenerationExecuteRequest(handle map[string]any, output string, ordinal int) map[string]any {
	purpose := productionImportGlobalFactsOperation
	if output == "visible-text" {
		purpose = productionImportWritingStyleOperation
	}
	return map[string]any{
		"handle":  handle,
		"ordinal": float64(ordinal),
		"task": map[string]any{
			"purpose": purpose,
			"output":  output,
			"messages": []any{
				map[string]any{"role": "user", "content": "推演参考小说"},
			},
		},
		"execution": map[string]any{
			"owner": productionImportGenerationExecutionOwner,
			"epoch": float64(productionImportGenerationExecutionEpoch),
		},
	}
}

// TestProductionImportGenerationRefusesForeignOperationSelection pins the
// admission gate: an import slot under any operation other than the stage's own
// keeps the refusal that request answered before this widening, and nothing is
// frozen for it.
func TestProductionImportGenerationRefusesForeignOperationSelection(t *testing.T) {
	lane := productionImportGenerationLaneOpen(t)
	before := productionFinalizationGenerationCount(t, lane.database, `SELECT COUNT(*) FROM generation_runs`)
	for _, refused := range []struct {
		operation string
		code      string
	}{
		// An ordinary operation with an import slot: the begin-shape refusal.
		{"chapter-draft", "GENERATION_BEGIN_INVALID"},
		{"agent-round", "GENERATION_BEGIN_INVALID"},
		// A planning name is refused by the planning gate before any import shape
		// is read, exactly as it was before.
		{"chapter-blueprint-directory", "GENERATION_OPERATION_UNSUPPORTED"},
		{"import-blueprints", "GENERATION_OPERATION_UNSUPPORTED"},
	} {
		selection := productionImportGenerationSelection("global", "import-generation-refused-"+refused.operation)
		selection["operation"] = refused.operation
		if code := productionImportGenerationBusinessRefusal(t, lane, "generation:begin",
			[]any{selection, lane.frozen}); code != refused.code {
			t.Fatalf("%v with an import slot refused %v, want %v", refused.operation, code, refused.code)
		}
	}
	if after := productionFinalizationGenerationCount(t, lane.database, `SELECT COUNT(*) FROM generation_runs`); after != before {
		t.Fatalf("a refused import selection wrote %v runs, want none", after-before)
	}
	if bound := productionFinalizationGenerationCount(t, lane.database,
		`SELECT COUNT(*) FROM generation_runs WHERE json_extract(binding_json,'$.sourceManifest.importSlotKey') IS NOT NULL`); bound != 0 {
		t.Fatalf("a refused import selection froze %v import-bound runs, want none", bound)
	}
}

// TestProductionImportGenerationRefusalsAreByteIdentical proves the fence the
// import names answer with is the one the generation family already answers:
// a stale lease and a foreign project produce exactly the bytes of the sibling
// name that owns the same check, for both the read and the execute half.
func TestProductionImportGenerationRefusalsAreByteIdentical(t *testing.T) {
	lane := productionImportGenerationLaneOpen(t)
	var begun map[string]any
	productionImportGenerationDecoded(t, lane, "generation:begin",
		[]any{productionImportGenerationSelection("style", "import-generation-identity-begin"), lane.frozen}, &begun)
	handle := syntheticMap(begun["handle"])
	if handle == nil || syntheticString(handle["runId"]) == "" {
		t.Fatalf("generation:begin answered no handle: %v", begun)
	}
	stale := map[string]any{"projectId": lane.projectID, "projectPath": lane.projectPath,
		"leaseId": productionImportGenerationStaleLease}
	foreign := map[string]any{"projectId": productionFinalizationGenerationWriteForeignProject,
		"projectPath": lane.projectPath, "leaseId": lane.leaseID}
	slot := map[string]any{"slot": productionImportGenerationSlot(productionImportGenerationStyleRunID, "style")}

	// The read half: the import read and the finalization read share admit, so a
	// stale lease and a foreign session answer the same bytes on both.
	for _, shape := range []struct {
		label    string
		expected map[string]any
	}{
		{"stale lease", stale},
		{"foreign session", foreign},
	} {
		importCode, importBody := productionImportGenerationRefusal(t, lane, productionImportGenerationRead,
			[]any{slot, shape.expected})
		finalizationSlot := map[string]any{
			"source": map[string]any{"draftId": float64(1), "chapterNumber": float64(1),
				"finalizationId": "import-generation-identity", "contentHash": strings.Repeat("d", 64)},
			"stepKey": "chapter_notes",
		}
		finalCode, finalBody := productionImportGenerationRefusal(t, lane, "finalization-generation:read",
			[]any{map[string]any{"slot": finalizationSlot}, shape.expected})
		if importCode != finalCode || importBody != finalBody {
			t.Fatalf("the import read and the finalization read disagree under %v:\n import:      %v %v\n finalization: %v %v",
				shape.label, importCode, importBody, finalCode, finalBody)
		}
	}

	// The execute half: the same stale lease and the same foreign-project handle
	// answer exactly what generation:execute answers.
	for _, shape := range []struct {
		label     string
		handle    map[string]any
		expected  map[string]any
	}{
		{"stale lease", handle, stale},
		{"foreign project handle", map[string]any{"projectId": productionFinalizationGenerationWriteForeignProject,
			"epoch": lane.leaseID, "rootActionId": syntheticString(handle["rootActionId"]),
			"runId": syntheticString(handle["runId"])}, lane.frozen},
	} {
		importCode, importBody := productionImportGenerationRefusal(t, lane, productionImportGenerationExecute,
			[]any{productionImportGenerationExecuteRequest(shape.handle, "visible-text", 0), shape.expected})
		generationCode, generationBody := productionImportGenerationRefusal(t, lane, "generation:execute",
			[]any{map[string]any{
				"handle":          shape.handle,
				"invocationNonce": "import-generation-identity",
				"task":            map[string]any{"purpose": productionImportWritingStyleOperation, "output": "visible-text", "messages": []any{map[string]any{"role": "user", "content": "推演参考小说"}}},
			}, shape.expected})
		if importCode != generationCode || importBody != generationBody {
			t.Fatalf("the import execute and generation:execute disagree under %v:\n import:     %v %v\n generation: %v %v",
				shape.label, importCode, importBody, generationCode, generationBody)
		}
	}
	if lane.seam.dispatched() != 0 {
		t.Fatalf("a refused import execute dispatched %v times, want none", lane.seam.dispatched())
	}
}

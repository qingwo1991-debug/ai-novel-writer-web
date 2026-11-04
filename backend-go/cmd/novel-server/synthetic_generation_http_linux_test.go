//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// syntheticFakeDispatch substitutes the native model dispatch. The first call
// returns a length stop with no visible text (a failed draft-short-outline
// attempt), every later call streams one cumulative visible chunk and stops.
// The usage satisfies the reduced lane's "unknown" accounting policy exactly
// (total == prompt+completion, reasoning <= completion).
func syntheticFakeDispatch(streamedText string) generation.Dispatch {
	var mu sync.Mutex
	calls := 0
	return func(ctx context.Context, request any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
		mu.Lock()
		index := calls
		calls++
		mu.Unlock()
		usage := map[string]any{"trusted": true, "promptTokens": 4, "completionTokens": 16, "reasoningTokens": 0, "totalTokens": 20}
		if index == 0 {
			return generation.DispatchResult{Usage: usage, FinishReason: "length"}, nil
		}
		if callbacks.OnVisible != nil {
			callbacks.OnVisible(generation.VisibleEvent{Kind: "cumulative", Text: streamedText})
		}
		return generation.DispatchResult{Usage: usage, FinishReason: "stop"}, nil
	}
}

func sha256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// TestSyntheticGenerationHTTPDurableLane drives the reduced S1 generation
// provider over the real HTTP transport with a fake dispatch: begin →
// execute (length stop) → retry-draft-short-outline (completed) → read → list
// → begin review run → bind-material-decision → execute → pause → resume →
// restart → discard-candidate. It asserts the browser DTO field names, the
// textHash invariant, the read-only SQLite ledger rows, and that all 32
// non-S1 generation-family operations still return GO_OPERATION_NOT_MIGRATED.
func TestSyntheticGenerationHTTPDurableLane(t *testing.T) {
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original schema oracle required")
		}
		t.Skip("GitHub original schema oracle required")
	}
	contractFile, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	contract, parseErr := admission.ParseContract(contractFile)
	closeErr := contractFile.Close()
	if parseErr != nil || closeErr != nil {
		t.Fatal(parseErr, closeErr)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	durableDir := filepath.Join(root, "durable-projects")
	if err := os.Mkdir(durableDir, 0700); err != nil {
		t.Fatal(err)
	}
	modelsDir := filepath.Join(root, "durable-models")
	if err := os.Mkdir(modelsDir, 0700); err != nil {
		t.Fatal(err)
	}
	const modelID = "synthetic-model"
	const streamedText = "synthetic durable generated text"
	modelsJSON := `[{"id":"` + modelID + `","provider":"custom","protocol":"openai","baseUrl":"https://example.invalid/v1","modelName":"synthetic-story","temperature":0.7,"maxTokens":512,"capabilities":{},"purposes":["generation"],"apiKey":"synthetic-secret-key"}]`
	if err := os.WriteFile(filepath.Join(modelsDir, "models.json"), []byte(modelsJSON), 0600); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithDurable(migrationRegistry(t), "127.0.0.1:16082", "", durableDir, syntheticDurableConfig{
		Schema:       contract,
		Baseline:     filepath.Join(filepath.Dir(oracle), syntheticBaselineDatabaseName),
		GlobalRoot:   modelsDir,
		ModelOrigins: []string{"https://example.invalid"},
		Dispatch:     syntheticFakeDispatch(streamedText),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	projectPath := filepath.Join(durableDir, syntheticDurableProjectName)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	if _, err := os.Stat(database); err != nil {
		t.Fatalf("real synthetic project.db missing: %v", err)
	}
	draftID, _ := seedSyntheticDurableRows(t, database)
	bootRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootRequest.Header.Set("X-Novel-User", "synthetic-author")
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, bootRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootWire struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootWire); err != nil || bootWire.CSRFToken == "" {
		t.Fatal(err, boot.Body.String())
	}
	cookie := boot.Result().Cookies()[0]
	const tab = "client_1234567890"
	rpc := func(operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	open := rpc("project:open", []any{projectPath, "durable-open-1", nil})
	var openWire struct {
		Result struct {
			Success bool `json:"success"`
			Project struct {
				ID          string `json:"id"`
				LeaseID     string `json:"sessionLease"`
				ProjectPath string `json:"path"`
			} `json:"project"`
			RequestToken      string `json:"requestToken"`
			ActiveProjectPath string `json:"activeProjectPath"`
		} `json:"result"`
	}
	if err := json.Unmarshal(open.Body.Bytes(), &openWire); err != nil || !openWire.Result.Success {
		t.Fatal(err, open.Body.String())
	}
	expected := map[string]any{
		"projectId":   openWire.Result.Project.ID,
		"leaseId":     openWire.Result.Project.LeaseID,
		"projectPath": openWire.Result.Project.ProjectPath,
	}
	// A stale lease is refused before any generation write (read ops surface
	// the symbolic code as an HTTP error).
	stale := rpc("generation:list", []any{map[string]any{
		"projectId": openWire.Result.Project.ID, "leaseId": "not-the-frozen-lease", "projectPath": openWire.Result.Project.ProjectPath,
	}})
	if stale.Code != http.StatusInternalServerError || !strings.Contains(stale.Body.String(), "PROJECT_SESSION_NOT_OWNED") {
		t.Fatalf("stale lease list = %d %s", stale.Code, stale.Body.String())
	}

	// --- chapter-draft run: begin → execute (length stop) → retry. ---
	hex64 := func(seed string) string {
		var builder strings.Builder
		for i := 0; i < 64; i++ {
			builder.WriteString(seed)
		}
		return builder.String()[:64]
	}
	beginRequest := map[string]any{
		"operation":             "chapter-draft",
		"uiActionNonce":         "nonce-1",
		"modelId":               modelID,
		"chapterNumber":         1,
		"selectedDraftIds":      []any{float64(draftID)},
		"selectedFinalizedDraftIds": []any{},
		"selectedBlueprintChapterNumbers": []any{},
		"promptKeys":            []any{"opening"},
		"skillStages":           []any{},
		"authorInputs":          []any{},
		"output":                "visible-text",
		"materialDecision": map[string]any{
			"version":                  1,
			"verdict":                  "admitted",
			"promptHash":               hex64("a"),
			"shortOutlinePromptHash":   hex64("b"),
			"capacity":                 map[string]any{"maxInputUnits": 4096, "methodVersion": "utf8-bytes-v1", "admittedUnits": 4096},
			"coverage":                 map[string]any{"required": 1, "included": 1, "complete": true},
			"included": []any{map[string]any{"sourceId": "draft:" + strconv.FormatInt(draftID, 10), "revision": 1, "contentHash": hex64("c"), "category": "author", "required": true, "units": 100}},
			"omitted":                 []any{},
		},
	}
	begin := rpc("generation:begin", []any{beginRequest, expected})
	if begin.Code != http.StatusOK || !strings.Contains(begin.Body.String(), `"result"`) {
		t.Fatalf("begin = %d %s", begin.Code, begin.Body.String())
	}
	runA := decodeResult(t, begin)
	handleA := runA["handle"].(map[string]any)
	assertStringField(t, runA, "handle.projectId", openWire.Result.Project.ID)
	assertStringField(t, runA, "handle.epoch", openWire.Result.Project.LeaseID)
	for _, key := range []string{"status", "operation", "budgetDiagnostics", "inFlight", "nonReplayable", "budget", "ledger", "artifacts", "candidates"} {
		if _, ok := runA[key]; !ok {
			t.Fatalf("begin view missing %q", key)
		}
	}
	read := rpc("generation:read", []any{handleA, expected})
	viewA := decodeResult(t, read)
	if viewA["operation"] != "chapter-draft" || viewA["status"] != "running" {
		t.Fatalf("read run A = %#v", viewA)
	}
	execute := rpc("generation:execute", []any{map[string]any{
		"handle": handleA, "invocationNonce": "invoke-1",
		"task": map[string]any{
			"purpose": "draft-short-outline", "output": "visible-text",
			"messages": []any{map[string]any{"role": "user", "content": "generate the short outline"}},
		},
	}, expected})
	executeWire := decodeResult(t, execute)
	outcomeA := executeWire["outcome"].(map[string]any)
	if outcomeA["status"] != "incomplete" || outcomeA["finishReason"] != "length" {
		t.Fatalf("first execute outcome = %#v", outcomeA)
	}
	attemptIDs := attemptsOf(t, executeWire["run"].(map[string]any))
	if len(attemptIDs) == 0 {
		t.Fatalf("execute run reports no attempts: %s", execute.Body.String())
	}
	firstAttemptID := attemptIDs[0]
	if firstAttemptID == "" {
		t.Fatal("first attempt missing")
	}
	retry := rpc("generation:retry-draft-short-outline", []any{map[string]any{
		"handle": handleA, "failedAttemptId": firstAttemptID,
	}, expected})
	retryWire := decodeResult(t, retry)
	retryOutcome := retryWire["outcome"].(map[string]any)
	if retryOutcome["status"] != "completed" || retryOutcome["finishReason"] != "stop" {
		t.Fatalf("retry outcome = %#v", retryOutcome)
	}
	if retryOutcome["content"] != streamedText {
		t.Fatalf("retry content = %q", retryOutcome["content"])
	}
	read = rpc("generation:read", []any{handleA, expected})
	viewA = decodeResult(t, read)
	candidatesA := viewA["candidates"].([]any)
	if len(candidatesA) == 0 {
		t.Fatal("run A has no candidates")
	}
	completed := candidatesA[len(candidatesA)-1].(map[string]any)
	assertStringField(t, completed, "textHash", sha256Hex(streamedText))
	for _, key := range []string{"projectId", "epoch", "rootActionId", "runId", "artifactId", "attemptId", "revision", "durableRevision", "text", "textHash", "status", "compositionEligible", "fingerprint", "nonReplayable"} {
		if _, ok := completed[key]; !ok {
			t.Fatalf("candidate snapshot missing %q", key)
		}
	}
	if completed["status"] != "completed" || completed["compositionEligible"] != true {
		t.Fatalf("candidate = %#v", completed)
	}
	if completed["text"] != streamedText || completed["textHash"] != sha256Hex(streamedText) {
		t.Fatalf("textHash invariant violated: %#v", completed)
	}

	// --- list returns the run. ---
	list := rpc("generation:list", []any{expected})
	listEntries := decodeResultList(t, list)
	if len(listEntries) == 0 {
		t.Fatal("generation:list returned no runs")
	}
	first := listEntries[0].(map[string]any)
	if first["status"] != "running" && first["status"] != "completed" {
		t.Fatalf("list first entry = %#v", first)
	}
	listHandle := first["handle"].(map[string]any)
	if listHandle["runId"] != handleA["runId"] {
		t.Fatalf("list handle mismatch: %#v", listHandle)
	}

	// --- review-chapter run: begin → bind-material-decision → execute. ---
	reviewBegin := map[string]any{
		"operation": "review-chapter", "uiActionNonce": "nonce-2", "modelId": modelID,
		"selectedDraftIds": []any{float64(draftID)}, "selectedFinalizedDraftIds": []any{},
		"selectedBlueprintChapterNumbers": []any{}, "promptKeys": []any{"review"},
		"skillStages": []any{}, "authorInputs": []any{}, "output": "visible-text",
	}
	review := rpc("generation:begin", []any{reviewBegin, expected})
	runB := decodeResult(t, review)
	handleB := runB["handle"].(map[string]any)
	decision := map[string]any{
		"version": 1, "verdict": "admitted", "promptHash": hex64("d"),
		"reconciliationPromptHash": hex64("e"),
		"capacity":                 map[string]any{"maxInputUnits": 4096, "methodVersion": "utf8-bytes-v1", "admittedUnits": 4096},
		"coverage":                 map[string]any{"required": 1, "included": 1, "complete": true},
		"included": []any{map[string]any{"sourceId": "draft:" + strconv.FormatInt(draftID, 10), "revision": 1, "contentHash": hex64("c"), "category": "author", "required": true, "units": 100}},
		"omitted":                  []any{},
	}
	bind := rpc("generation:bind-material-decision", []any{map[string]any{"handle": handleB, "materialDecision": decision}, expected})
	if bind.Code != http.StatusOK || !strings.Contains(bind.Body.String(), `"result"`) {
		t.Fatalf("bind = %d %s", bind.Code, bind.Body.String())
	}
	sameBind := rpc("generation:bind-material-decision", []any{map[string]any{"handle": handleB, "materialDecision": decision}, expected})
	if sameBind.Code != http.StatusOK || !strings.Contains(sameBind.Body.String(), `"result"`) {
		t.Fatalf("idempotent bind = %d %s", sameBind.Code, sameBind.Body.String())
	}
	conflictDecision := map[string]any{
		"version": 1, "verdict": "admitted", "promptHash": hex64("f"),
		"capacity":                 map[string]any{"maxInputUnits": 4096, "methodVersion": "utf8-bytes-v1", "admittedUnits": 4096},
		"coverage":                 map[string]any{"required": 1, "included": 1, "complete": true},
		"included": []any{map[string]any{"sourceId": "draft:" + strconv.FormatInt(draftID, 10), "revision": 1, "contentHash": hex64("c"), "category": "author", "required": true, "units": 100}},
		"omitted":                  []any{},
	}
	conflict := rpc("generation:bind-material-decision", []any{map[string]any{"handle": handleB, "materialDecision": conflictDecision}, expected})
	if !strings.Contains(conflict.Body.String(), "GENERATION_MATERIAL_DECISION_CONFLICT") {
		t.Fatalf("conflict bind = %d %s", conflict.Code, conflict.Body.String())
	}
	executeB := rpc("generation:execute", []any{map[string]any{
		"handle": handleB, "invocationNonce": "invoke-2",
		"task": map[string]any{
			"purpose": "review-chapter", "output": "visible-text",
			"messages": []any{map[string]any{"role": "user", "content": "review the chapter"}},
		},
	}, expected})
	executeBWire := decodeResult(t, executeB)
	if executeBWire["outcome"].(map[string]any)["status"] != "completed" {
		t.Fatalf("run B execute outcome = %#v", executeBWire["outcome"])
	}
	pause := rpc("generation:pause", []any{handleB, expected})
	paused := decodeResult(t, pause)
	if paused["status"] != "paused" {
		t.Fatalf("pause = %#v", paused)
	}
	resume := rpc("generation:resume", []any{handleB, expected})
	resumed := decodeResult(t, resume)
	if resumed["status"] != "running" {
		t.Fatalf("resume = %#v", resumed)
	}
	// Restart with a new nonce opens a fresh run and cancels the old root.
	restartSelection := map[string]any{
		"operation": "review-chapter", "uiActionNonce": "nonce-3", "modelId": modelID,
		"selectedDraftIds": []any{float64(draftID)}, "selectedFinalizedDraftIds": []any{},
		"selectedBlueprintChapterNumbers": []any{}, "promptKeys": []any{"review"},
		"skillStages": []any{}, "authorInputs": []any{}, "output": "visible-text",
	}
	restart := rpc("generation:restart", []any{handleB, restartSelection, expected})
	runC := decodeResult(t, restart)
	handleC := runC["handle"].(map[string]any)
	if handleC["runId"] == handleB["runId"] {
		t.Fatal("restart reused the old run")
	}
	executeC := rpc("generation:execute", []any{map[string]any{
		"handle": handleC, "invocationNonce": "invoke-3",
		"task": map[string]any{
			"purpose": "review-chapter", "output": "visible-text",
			"messages": []any{map[string]any{"role": "user", "content": "review again"}},
		},
	}, expected})
	executeCWire := decodeResult(t, executeC)
	viewC := executeCWire["run"].(map[string]any)
	artifactsC := viewC["artifacts"].([]any)
	if len(artifactsC) != 1 {
		t.Fatalf("run C artifacts = %#v", artifactsC)
	}
	artifactC := artifactsC[0].(map[string]any)
	if artifactC["text"] != streamedText || artifactC["textHash"] != sha256Hex(streamedText) {
		t.Fatalf("run C artifact = %#v", artifactC)
	}
	discard := rpc("generation:discard-candidate", []any{handleC, artifactC["artifactId"], expected})
	afterDiscard := decodeResult(t, discard)
	if len(afterDiscard["candidates"].([]any)) != 0 {
		t.Fatalf("discarded candidate still listed: %#v", afterDiscard["candidates"])
	}

	// --- generation rows are visible to a read-only SQLite reader. ---
	var runCount int
	durableQuery(t, database, "SELECT COUNT(*) FROM generation_runs", &runCount)
	if runCount < 3 {
		t.Fatalf("generation_runs = %d, want >= 3", runCount)
	}
	var attemptCount int
	durableQuery(t, database, "SELECT COUNT(*) FROM generation_attempts", &attemptCount)
	if attemptCount < 4 {
		t.Fatalf("generation_attempts = %d, want >= 4", attemptCount)
	}
	var artifactCount int
	durableQuery(t, database, "SELECT COUNT(*) FROM generation_artifacts", &artifactCount)
	if artifactCount < 4 {
		t.Fatalf("generation_artifacts = %d, want >= 4", artifactCount)
	}
	var discardedCount int
	durableQuery(t, database, "SELECT COUNT(*) FROM generation_artifacts WHERE status='discarded'", &discardedCount)
	if discardedCount != 1 {
		t.Fatalf("discarded artifacts = %d, want 1", discardedCount)
	}
	var rootCount int
	durableQuery(t, database, "SELECT COUNT(*) FROM generation_roots", &rootCount)
	if rootCount < 3 {
		t.Fatalf("generation_roots = %d, want >= 3", rootCount)
	}

	// --- the four recovery candidate operations are bound to the durable lane. ---
	recoveryProjectPath, _ := expected["projectPath"].(string)
	candidates := rpc("db:recovery-candidate-list", []any{recoveryProjectPath, expected})
	if candidates.Code != http.StatusOK || !strings.Contains(candidates.Body.String(), `"result"`) {
		t.Fatalf("recovery candidate list = %d %s", candidates.Code, candidates.Body.String())
	}

	// --- continuity and summary snapshots share the durable author admission. ---
	latestSummary := rpc("db:get-latest-summary", []any{recoveryProjectPath, expected})
	if latestSummary.Code != http.StatusOK || !strings.Contains(latestSummary.Body.String(), `"result"`) {
		t.Fatalf("latest summary = %d %s", latestSummary.Code, latestSummary.Body.String())
	}

	// --- review and revision writes reach the repository through the lane. ---
	nextIndex := rpc("db:revision-next-index", []any{float64(draftID), recoveryProjectPath, expected})
	if nextIndex.Code != http.StatusOK || !strings.Contains(nextIndex.Body.String(), `"result"`) {
		t.Fatalf("revision next index = %d %s", nextIndex.Code, nextIndex.Body.String())
	}
	// A create without the exact expected source must be refused by the
	// repository's own CAS, which proves the binding, lease and provider path.
	unprotected := rpc("db:revision-create", []any{map[string]any{
		"baseDraftId": float64(draftID), "revisionType": "refine", "content": "改稿", "wordCount": float64(2),
	}, recoveryProjectPath, expected})
	if unprotected.Code != http.StatusOK || !strings.Contains(unprotected.Body.String(), `"success":false`) ||
		!strings.Contains(unprotected.Body.String(), "SOURCE_DRAFT_CHANGED") {
		t.Fatalf("unprotected revision create = %d %s", unprotected.Code, unprotected.Body.String())
	}

	// --- all 32 non-S1 generation-family operations stay unmigrated. ---
	unmigrated := append([]string{
		"generation:begin-batch", "generation:commit-draft", "generation:compose-visible",
		"generation:confirm-batch-finalization", "generation:find-blueprint-recovery",
		"generation:list-batches", "generation:list-directory-progress", "generation:list-recovery",
		"generation:prepare-draft-context", "generation:read-batch", "generation:read-context",
		"generation:read-visible-composition",
	}, []string{
		"agent-generation:begin", "agent-generation:claim-tool", "agent-generation:commit-domain-tool",
		"agent-generation:finish-tool", "agent-generation:read", "agent-generation:register-workflow",
		"agent-generation:resume", "agent-generation:round",
		"finalization-generation:begin", "finalization-generation:commit",
		"finalization-generation:execute", "finalization-generation:read",
		"graph-generation:begin", "graph-generation:confirm",
		"graph-generation:execute",
		"import-generation:execute", "import-generation:read",
	}...)
	if len(unmigrated) != 29 {
		t.Fatalf("unmigrated list = %d, want 29", len(unmigrated))
	}
	for _, operation := range unmigrated {
		response := rpc(operation, []any{expected})
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s = %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
		}
	}

	// --- graph-generation:read is bound: it validates the `{ handle }` envelope
	// and then refuses a run that does not exist, which is proof the request
	// shape and the context lookup both ran. ---
	{
		response := rpc("graph-generation:read", []any{map[string]any{"handle": map[string]any{
			"projectId": "synthetic-project", "epoch": "synthetic-epoch",
			"rootActionId": "missing-root", "runId": "missing-run",
		}}, expected})
		body := response.Body.String()
		if response.Code == http.StatusNotImplemented || strings.Contains(body, "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("graph-generation:read is still unmigrated: %d %s", response.Code, body)
		}
		if strings.Contains(body, "REQUEST_INVALID") {
			t.Fatalf("graph-generation:read rejected the { handle } envelope: %d %s", response.Code, body)
		}
	}

	// --- both family cancels now reach the ledger instead of the 501 gate. ---
	for _, operation := range []string{"graph-generation:cancel", "finalization-generation:cancel"} {
		response := rpc(operation, []any{map[string]any{"handle": map[string]any{
			"projectId": "synthetic-project", "epoch": "synthetic-epoch",
			"rootActionId": "missing-root", "runId": "missing-run",
		}}, expected})
		if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s is still unmigrated: %d %s", operation, response.Code, response.Body.String())
		}
		// The family channels take `{ handle }`, not the bare handle. Rejecting
		// the envelope would surface as a REQUEST_INVALID code, and the run it
		// names does not exist, so reaching the run lookup is the proof that the
		// shape was understood.
		if body := response.Body.String(); strings.Contains(body, "REQUEST_INVALID") {
			t.Fatalf("%s rejected the { handle } envelope: %d %s", operation, response.Code, body)
		}
	}

	// --- credentials never leak into any DTO or error body. ---
	for _, response := range []*httptest.ResponseRecorder{begin, execute, retry, bind, read, list, pause, resume, restart, discard, conflict} {
		if strings.Contains(response.Body.String(), "synthetic-secret-key") {
			t.Fatalf("api key leaked in response: %s", response.Body.String())
		}
	}
}

// decodeResult unwraps {"result": ...} as an object.
func decodeResult(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	object, ok := decodeResultAny(t, response).(map[string]any)
	if !ok {
		t.Fatalf("result is not an object: %#v", response.Body.String())
	}
	return object
}

// decodeResultList unwraps {"result": ...} as an array.
func decodeResultList(t *testing.T, response *httptest.ResponseRecorder) []any {
	t.Helper()
	list, ok := decodeResultAny(t, response).([]any)
	if !ok {
		t.Fatalf("result is not an array: %#v", response.Body.String())
	}
	return list
}

func decodeResultAny(t *testing.T, response *httptest.ResponseRecorder) any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %d %s", response.Code, response.Body.String())
	}
	var wire struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatal(err, response.Body.String())
	}
	return wire.Result
}

// attemptsOf returns attempt ids in ledger order from a run view's
// budgetDiagnostics.
func attemptsOf(t *testing.T, view map[string]any) []string {
	t.Helper()
	diagnostics, ok := view["budgetDiagnostics"].([]any)
	if !ok {
		t.Fatalf("view missing budgetDiagnostics: %#v", view)
	}
	out := make([]string, 0, len(diagnostics))
	for _, entry := range diagnostics {
		attempt := entry.(map[string]any)
		out = append(out, attempt["attemptId"].(string))
	}
	return out
}

// assertStringField walks a dotted path into nested objects and asserts the
// leaf is the wanted non-empty string.
func assertStringField(t *testing.T, object map[string]any, key, want string) {
	t.Helper()
	var current any = object
	for _, part := range strings.Split(key, ".") {
		nested, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("%s = %#v, want %q", key, current, want)
		}
		current = nested[part]
	}
	if got, ok := current.(string); !ok || got != want {
		t.Fatalf("%s = %#v, want %q", key, current, want)
	}
}

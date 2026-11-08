//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the six served agent-generation tool names over the real
// HTTP handler with the model stubbed at the lane's dispatch seam. The seam
// returns a completed agent response whose tool calls become the actions the
// four tool names operate on, and every written action-map entry is read back
// over a separate mode=ro connection.

const (
	productionAgentGenerationToolsMessage = "我先登记一个草稿工作流。"
	productionAgentGenerationToolsNonce   = "agent-generation-tools-nonce"
	productionAgentGenerationToolsStale   = "agent-generation-tools-stale-lease"
	productionAgentGenerationToolsForeign  = "project-agent-generation-tools-foreign"
	productionAgentGenerationToolsWorkflow = "start_workflow"
	productionAgentGenerationToolsDomain   = "propose_novel_config"
)

// productionAgentGenerationToolSeam is the round's dispatch stub: it returns a
// completed stop whose usage carries the parsed agent response, and it counts
// calls so a replay that redispatched would fail.
type productionAgentGenerationToolSeam struct {
	mu       sync.Mutex
	calls    int
	text     string
	toolCalls []map[string]any
}

func (s *productionAgentGenerationToolSeam) dispatch(_ context.Context, _ any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	s.calls++
	s.mu.Unlock()
	if callbacks.OnVisible != nil {
		callbacks.OnVisible(generation.VisibleEvent{Kind: "cumulative", Text: s.text})
	}
	return generation.DispatchResult{
		Usage: map[string]any{
			"trusted": true, "promptTokens": 4, "completionTokens": 16, "reasoningTokens": 0, "totalTokens": 20,
			"agentResponse": map[string]any{
				"version":     float64(1),
				"visibleText": s.text,
				"toolCalls":   s.toolCalls,
			},
		},
		FinishReason: "stop",
	}, nil
}

func (s *productionAgentGenerationToolSeam) dispatched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// productionAgentGenerationToolsRequest is the begin input whose frozen tools
// authorize the two actions the seam returns.
func productionAgentGenerationToolsRequest(message string) map[string]any {
	tool := func(name string) map[string]any {
		return map[string]any{"name": name, "description": "tool " + name,
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"requiresConfirmation": true, "isReadOnly": false, "source": "builtin"}
	}
	return map[string]any{
		"uiActionNonce": productionAgentGenerationToolsNonce,
		"modelId":       productionTestModelID,
		"input": map[string]any{
			"mode":            "planning",
			"uiLocale":        "zh-CN",
			"historyMessages": []any{},
			"userMessage":     message,
			"tools":           []any{tool(productionAgentGenerationToolsWorkflow), tool(productionAgentGenerationToolsDomain)},
		},
	}
}

// productionAgentGenerationToolsOpen builds the lane with the dispatch seam and
// the full served agent-generation surface bound.
func productionAgentGenerationToolsOpen(t *testing.T, dispatch generation.Dispatch) *productionAgentGenerationServeLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	_, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatalf("the lane layout: %v", err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatalf("the lane could not open the admitted project: %v", err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-admitted")
	if err := server.BindNavigation(); err != nil {
		_ = open.Close()
		t.Fatalf("the lane could not bind navigation: %v", err)
	}
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatalf("the lane could not bind the project lifecycle: %v", err)
	}
	owner, err := newProductionGeneration(open, server, dispatch)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation owner: %v", err)
	}
	if err := server.BindGenerationLifecycle(&transport.GenerationProvider{Cancel: owner.Cancel, Pause: owner.Pause, Resume: owner.Resume}); err != nil {
		_ = open.Close()
		t.Fatalf("the lifecycle bind failed: %v", err)
	}
	if err := newProductionAgentGenerationServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the agent-generation serve failed to bind: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	t.Cleanup(func() {
		server.Close()
		_ = open.Close()
	})
	author, frozen := productionExecuteSession(t, mux, projectPath, productionAgentGenerationToolsNonce)
	return &productionAgentGenerationServeLane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		author:      author,
		frozen:      frozen,
		projectID:   syntheticString(frozen["projectId"]),
		leaseID:     syntheticString(frozen["leaseId"]),
	}
}

// productionAgentGenerationToolsAnswer posts one name and returns its decoded
// result plus the raw result body.
func productionAgentGenerationToolsAnswer(t *testing.T, lane *productionAgentGenerationServeLane, operation string, args []any) (map[string]any, string) {
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
	var decoded map[string]any
	if err := json.Unmarshal(wire.Result, &decoded); err != nil {
		t.Fatalf("%v: %v %v", operation, err, string(wire.Result))
	}
	return decoded, string(wire.Result)
}

// productionAgentGenerationToolsRefusal posts one name and returns its status
// and body, which byte-identical refusals are compared on.
func productionAgentGenerationToolsRefusal(t *testing.T, lane *productionAgentGenerationServeLane, operation string, args []any) (int, string) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%v was admitted with %v", operation, response.Body.String())
	}
	return response.Code, response.Body.String()
}

// productionAgentGenerationToolsUsage reads one attempt's usage receipt over a
// read-only connection of the test's own.
func productionAgentGenerationToolsUsage(t *testing.T, lane *productionAgentGenerationServeLane, attemptID string) map[string]any {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(lane.database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	defer func() { _ = db.Close() }()
	var raw string
	if err := db.QueryRow("SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=?", attemptID).Scan(&raw); err != nil {
		t.Fatalf("read the attempt receipt: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("decode the attempt receipt: %v", err)
	}
	return decoded
}

// productionAgentGenerationToolsActionState reads one action's stored state from
// the action map over a read-only connection.
func productionAgentGenerationToolsActionState(t *testing.T, lane *productionAgentGenerationServeLane, attemptID, toolCallID string) map[string]any {
	t.Helper()
	usage := productionAgentGenerationToolsUsage(t, lane, attemptID)
	return syntheticMap(syntheticMap(usage["agentActions"])[toolCallID])
}

// productionAgentGenerationToolsRound begins a session and dispatches round 0,
// returning the completed recovery, the attempt id and the two action refs.
func productionAgentGenerationToolsRound(t *testing.T, lane *productionAgentGenerationServeLane, seam *productionAgentGenerationToolSeam) (productionAgentGenerationServeRecovery, string, map[string]any, map[string]any) {
	t.Helper()
	begun, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationBegin,
		[]any{productionAgentGenerationToolsRequest(productionAgentGenerationToolsMessage), lane.frozen})
	handle := productionAgentGenerationWireHandle(syntheticHandle{
		ProjectID: begun.Handle.ProjectID, Epoch: begun.Handle.Epoch,
		RootActionID: begun.Handle.RootActionID, RunID: begun.Handle.RunID,
	})
	recovery, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationRound,
		[]any{map[string]any{"handle": handle, "index": 0}, lane.frozen})
	if len(recovery.Rounds) != 1 || recovery.Rounds[0].Status != agentgen.RoundCompleted || len(recovery.Rounds[0].Actions) != 2 {
		t.Fatalf("round 0 projected %+v, want one completed round with two actions", recovery.Rounds)
	}
	if seam.dispatched() != 1 {
		t.Fatalf("round 0 dispatched %v times, want exactly 1", seam.dispatched())
	}
	attemptID := recovery.Rounds[0].AttemptID
	workflowRef := map[string]any{"handle": handle, "attemptId": attemptID,
		"toolCallId": syntheticString(syntheticMap(recovery.Rounds[0].Actions[0]["ref"])["toolCallId"])}
	domainRef := map[string]any{"handle": handle, "attemptId": attemptID,
		"toolCallId": syntheticString(syntheticMap(recovery.Rounds[0].Actions[1]["ref"])["toolCallId"])}
	return recovery, attemptID, workflowRef, domainRef
}

// TestProductionAgentGenerationRoundServed proves round dispatches the frozen
// agent task with its tool manifest, replays without a second dispatch, refuses
// a foreign project and a stale lease byte-identically, and that a non-agent
// operation still refuses a non-nil tool manifest.
func TestProductionAgentGenerationRoundServed(t *testing.T) {
	seam := &productionAgentGenerationToolSeam{
		text: productionAgentGenerationToolsMessage,
		toolCalls: []map[string]any{
			{"name": productionAgentGenerationToolsWorkflow,
				"arguments": map[string]any{"workflow": agentgen.WorkflowDraft, "chapter_number": float64(3)}},
			{"name": productionAgentGenerationToolsDomain,
				"arguments": map[string]any{"changes": map[string]any{"genre": "都市"}}},
		},
	}
	lane := productionAgentGenerationToolsOpen(t, seam.dispatch)
	recovery, attemptID, workflowRef, _ := productionAgentGenerationToolsRound(t, lane, seam)

	// The dispatched round's artifact text is the visible text, and the settled
	// usage carried the parsed agent response the projection read.
	if recovery.Rounds[0].VisibleText != productionAgentGenerationToolsMessage ||
		recovery.Rounds[0].AttemptID != attemptID {
		t.Fatalf("round 0 answered %+v", recovery.Rounds[0])
	}
	usage := productionAgentGenerationToolsUsage(t, lane, attemptID)
	response := syntheticMap(syntheticMap(syntheticMap(usage["result"])["usage"])["agentResponse"])
	if syntheticNumber(response["version"]) != 1 || syntheticString(response["visibleText"]) != productionAgentGenerationToolsMessage {
		t.Fatalf("the settled usage lost the agent response: %v", usage)
	}

	// A replay of round 0 is idempotent and redispatches nothing.
	handle := productionAgentGenerationWireHandle(syntheticHandle{ProjectID: recovery.Handle.ProjectID,
		Epoch: recovery.Handle.Epoch, RootActionID: recovery.Handle.RootActionID, RunID: recovery.Handle.RunID})
	replayed, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationRound,
		[]any{map[string]any{"handle": handle, "index": 0}, lane.frozen})
	if replayed.Rounds[0].AttemptID != attemptID || seam.dispatched() != 1 {
		t.Fatalf("round 0 replay answered attempt %v with %v dispatches", replayed.Rounds[0].AttemptID, seam.dispatched())
	}

	// Foreign project and stale lease refuse, byte-identically to the read name.
	stale := map[string]any{"projectId": lane.projectID, "projectPath": lane.projectPath, "leaseId": productionAgentGenerationToolsStale}
	readStatus, readBody := productionAgentGenerationToolsRefusal(t, lane, productionAgentGenerationRead,
		[]any{map[string]any{"handle": handle}, stale})
	roundStatus, roundBody := productionAgentGenerationToolsRefusal(t, lane, productionAgentGenerationRound,
		[]any{map[string]any{"handle": handle, "index": 0}, stale})
	if readStatus != roundStatus || readBody != roundBody {
		t.Fatalf("a stale lease answered differently per name: read %v %v, round %v %v", readStatus, readBody, roundStatus, roundBody)
	}
	foreignHandle := syntheticHandle{ProjectID: productionAgentGenerationToolsForeign, Epoch: recovery.Handle.Epoch,
		RootActionID: recovery.Handle.RootActionID, RunID: recovery.Handle.RunID}
	foreignStatus, foreignBody := productionAgentGenerationToolsRefusal(t, lane, productionAgentGenerationRound,
		[]any{map[string]any{"handle": productionAgentGenerationWireHandle(foreignHandle), "index": 0}, lane.frozen})
	if foreignStatus != http.StatusInternalServerError || !strings.Contains(foreignBody, agentgen.CodeIdentityMismatch) {
		t.Fatalf("a foreign project answered %v %v, want %v", foreignStatus, foreignBody, agentgen.CodeIdentityMismatch)
	}

	// The non-agent gate: a non-agent run still refuses a non-nil tool manifest.
	t.Run("non-agent-operation-refuses-tool-manifest", func(t *testing.T) {
		productionAgentGenerationToolsNonAgentGate(t)
	})
	_ = workflowRef
}

// productionAgentGenerationToolsNonAgentGate proves seam (a): a non-agent run
// still refuses a non-nil tool manifest through the shared dispatch service.
func productionAgentGenerationToolsNonAgentGate(t *testing.T) {
	t.Helper()
	root, projectPath, database, contract := productionGraphReadsRoot(t)
	_ = root
	_ = projectPath
	_ = contract
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return "agent-tools-nonagent-" + strconv.Itoa(ordinal)
	})
	ctx := context.Background()
	run := productionGraphReadsSeedRun(t, ctx, ledger, productionGraphReadsSeedRequest(t,
		"chapter-draft", productionGraphReadsProjectID, productionGraphReadsFrozenEpoch,
		map[string]any{"operation": "chapter-draft"}))
	runID := syntheticString(run["runId"])
	if runID == "" {
		t.Fatalf("the non-agent seed produced no run: %v", run)
	}
	service := generation.New(generation.Dependencies{Ledger: ledger, Dispatch: func(context.Context, any, generation.Callbacks) (generation.DispatchResult, error) {
		return generation.DispatchResult{}, nil
	}})
	request := generation.Request{RunID: runID, InvocationNonce: "non-agent-invoke",
		RequestHash: strings.Repeat("c", 64), AgentToolNames: []string{"start_workflow"}}
	if _, err := service.Execute(ctx, request); err == nil || err.Error() != "GENERATION_OPERATION_UNSUPPORTED" {
		t.Fatalf("a non-agent run answered %v, want GENERATION_OPERATION_UNSUPPORTED", err)
	}
}

// TestProductionAgentGenerationToolOpsServed proves claim-tool, finish-tool,
// register-workflow, commit-domain-tool and resume write their entries into the
// agent action map on the run's receipt, read back over a read-only connection.
func TestProductionAgentGenerationToolOpsServed(t *testing.T) {
	seam := &productionAgentGenerationToolSeam{
		text: productionAgentGenerationToolsMessage,
		toolCalls: []map[string]any{
			{"name": productionAgentGenerationToolsWorkflow,
				"arguments": map[string]any{"workflow": agentgen.WorkflowDraft, "chapter_number": float64(3)}},
			{"name": productionAgentGenerationToolsDomain,
				"arguments": map[string]any{"changes": map[string]any{"genre": "都市"}}},
		},
	}
	lane := productionAgentGenerationToolsOpen(t, seam.dispatch)
	recovery, attemptID, workflowRef, domainRef := productionAgentGenerationToolsRound(t, lane, seam)
	handle := productionAgentGenerationWireHandle(syntheticHandle{ProjectID: recovery.Handle.ProjectID,
		Epoch: recovery.Handle.Epoch, RootActionID: recovery.Handle.RootActionID, RunID: recovery.Handle.RunID})
	workflowTool := syntheticString(workflowRef["toolCallId"])
	domainTool := syntheticString(domainRef["toolCallId"])

	// claim-tool on start_workflow: pending -> running.
	claim, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationClaimTool,
		[]any{map[string]any{"ref": workflowRef, "confirmed": true}, lane.frozen})
	if syntheticString(syntheticMap(claim["action"])["status"]) != agentgen.ActionRunning || claim["execute"] != true {
		t.Fatalf("claim answered %v", claim)
	}
	if state := productionAgentGenerationToolsActionState(t, lane, attemptID, workflowTool); syntheticString(state["status"]) != agentgen.ActionRunning {
		t.Fatalf("the claim wrote %v", state)
	}

	// register-workflow stamps the registration idempotently.
	registration, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationRegister,
		[]any{map[string]any{"ref": workflowRef}, lane.frozen})
	registrationID := syntheticString(registration["registrationId"])
	if syntheticString(registration["workflow"]) != agentgen.WorkflowDraft || !strings.HasPrefix(registrationID, "agent-workflow:") {
		t.Fatalf("register answered %v", registration)
	}
	replayedRegistration, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationRegister,
		[]any{map[string]any{"ref": workflowRef}, lane.frozen})
	if syntheticString(replayedRegistration["registrationId"]) != registrationID {
		t.Fatalf("register replay answered %v, want %v", replayedRegistration, registrationID)
	}

	// finish-tool settles the workflow action.
	finished, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationFinishTool,
		[]any{map[string]any{"ref": workflowRef, "status": agentgen.ActionCompleted, "observation": "草稿工作流已登记"}, lane.frozen})
	if syntheticString(finished["status"]) != agentgen.ActionCompleted {
		t.Fatalf("finish answered %v", finished)
	}
	if state := productionAgentGenerationToolsActionState(t, lane, attemptID, workflowTool); syntheticString(state["status"]) != agentgen.ActionCompleted {
		t.Fatalf("the finish wrote %v", state)
	}

	// claim-tool on propose_novel_config, then commit its domain effect.
	domainClaim, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationClaimTool,
		[]any{map[string]any{"ref": domainRef, "confirmed": true}, lane.frozen})
	if syntheticString(syntheticMap(domainClaim["action"])["status"]) != agentgen.ActionRunning {
		t.Fatalf("the domain claim answered %v", domainClaim)
	}
	effect, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationCommitDomain,
		[]any{map[string]any{"ref": domainRef}, lane.frozen})
	if syntheticString(effect["kind"]) != "config" || syntheticString(effect["toolCallId"]) != domainTool {
		t.Fatalf("commit answered %v", effect)
	}
	if state := productionAgentGenerationToolsActionState(t, lane, attemptID, domainTool); syntheticMap(state["domainEffect"]) == nil {
		t.Fatalf("the commit wrote no domain effect: %v", state)
	}
	effectReplay, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationCommitDomain,
		[]any{map[string]any{"ref": domainRef}, lane.frozen})
	if syntheticString(effectReplay["toolCallId"]) != domainTool {
		t.Fatalf("the commit replay answered %v", effectReplay)
	}

	// Pause then resume: the paused run is re-admitted and the control epoch is
	// patched onto the receipt.
	var paused map[string]any
	productionEditorDecode(t, lane.author, "generation:pause", []any{handle, lane.frozen}, &paused)
	if syntheticString(paused["status"]) != "paused" {
		t.Fatalf("pause answered %v", paused)
	}
	resumed, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationResume,
		[]any{map[string]any{"handle": handle}, lane.frozen})
	if resumed.Handle.RunID != recovery.Handle.RunID || resumed.SourceStatus != productionAgentGenerationStatusCurrent {
		t.Fatalf("resume answered %+v", resumed)
	}
	usage := productionAgentGenerationToolsUsage(t, lane, attemptID)
	if syntheticString(usage["agentControlEpoch"]) != lane.leaseID {
		t.Fatalf("the resume wrote no control epoch: %v", usage)
	}
	// A completed round resumes control idempotently: a replay re-admits nothing
	// and patches the same epoch without an error.
	resumeReplay, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationResume,
		[]any{map[string]any{"handle": handle}, lane.frozen})
	if resumeReplay.Handle.RunID != recovery.Handle.RunID {
		t.Fatalf("the resume replay answered %+v", resumeReplay)
	}

	// Replays of claim, finish and commit are idempotent and change nothing.
	claimReplay, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationClaimTool,
		[]any{map[string]any{"ref": workflowRef, "confirmed": true}, lane.frozen})
	if syntheticString(syntheticMap(claimReplay["action"])["status"]) != agentgen.ActionCompleted {
		t.Fatalf("the claim replay answered %v", claimReplay)
	}
	finishReplay, _ := productionAgentGenerationToolsAnswer(t, lane, productionAgentGenerationFinishTool,
		[]any{map[string]any{"ref": workflowRef, "status": agentgen.ActionCompleted, "observation": "草稿工作流已登记"}, lane.frozen})
	if syntheticString(finishReplay["status"]) != agentgen.ActionCompleted {
		t.Fatalf("the finish replay answered %v", finishReplay)
	}

	// Foreign-project and stale-lease refusals are byte-identical to read's.
	stale := map[string]any{"projectId": lane.projectID, "projectPath": lane.projectPath, "leaseId": productionAgentGenerationToolsStale}
	readStatus, readBody := productionAgentGenerationToolsRefusal(t, lane, productionAgentGenerationRead,
		[]any{map[string]any{"handle": handle}, stale})
	foreignHandle := productionAgentGenerationWireHandle(syntheticHandle{ProjectID: productionAgentGenerationToolsForeign,
		Epoch: recovery.Handle.Epoch, RootActionID: recovery.Handle.RootActionID, RunID: recovery.Handle.RunID})
	for _, operation := range []string{productionAgentGenerationClaimTool, productionAgentGenerationFinishTool,
		productionAgentGenerationRegister, productionAgentGenerationCommitDomain, productionAgentGenerationResume} {
		var args []any
		switch operation {
		case productionAgentGenerationResume:
			args = []any{map[string]any{"handle": handle}, stale}
		default:
			args = []any{map[string]any{"ref": map[string]any{"handle": foreignHandle, "attemptId": attemptID, "toolCallId": workflowTool}}, stale}
		}
		status, body := productionAgentGenerationToolsRefusal(t, lane, operation, args)
		if status != readStatus || body != readBody {
			t.Fatalf("%v answered a stale lease differently than read: %v %v vs %v %v", operation, status, body, readStatus, readBody)
		}
		foreignStatus, foreignBody := productionAgentGenerationToolsRefusal(t, lane, operation,
			[]any{map[string]any{"ref": map[string]any{"handle": foreignHandle, "attemptId": attemptID, "toolCallId": workflowTool}}, lane.frozen})
		if foreignStatus != http.StatusInternalServerError || !strings.Contains(foreignBody, agentgen.CodeIdentityMismatch) {
			t.Fatalf("%v answered a foreign project %v %v", operation, foreignStatus, foreignBody)
		}
	}
}

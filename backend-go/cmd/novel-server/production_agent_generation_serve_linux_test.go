//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/agentgen"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves the two served agent-generation names over the real HTTP
// handler, through the frozen project session, with a real admitted author
// database and a real model lease. There is no provider call to stub here:
// begin mints a lease from the committed profile and read opens no egress at
// all, so the model seam of this slice is the committed model record - the
// loopback origin the fixture commits is only ever inspected, never dialed.
//
// The lane under test is built by this file rather than by
// productionHandlerWithContract, because the binder line that installs the two
// served names into the product lane lives in production_open_linux.go's owner
// block and that file does not belong to this change. What it does install is
// the same real machinery: productionLayoutFor, openProduction (the single
// writer lease), transport.New with the shipped operation registry,
// server.BindProjects for project:open, and
// newProductionAgentGenerationServe(owner).Bind(server). The one thing this
// lane does not have is the other 260 binders, and none of them is reachable
// from the two names under test.
//
// Every success is read back through a connection of the test's own: the run's
// binding_json, its sourceManifest, the frozen session, the context digest and
// the round the recovery reported.

const (
	// productionAgentGenerationServeUser is the message the frozen input carries;
	// it is the marker a context that echoed another request would lose.
	productionAgentGenerationServeUser = "请检查第一章的连续性"
	// productionAgentGenerationServeOther is a different message for the
	// nonce-conflict case.
	productionAgentGenerationServeOther = "请改写第二章的结尾"
	// productionAgentGenerationServeNonce is the one uiActionNonce this file spends.
	productionAgentGenerationServeNonce = "agent-generation-serve-nonce"
	// productionAgentGenerationServeToolName is the one tool the input freezes.
	productionAgentGenerationServeToolName = "read_project_core"
	// productionAgentGenerationServeStaleLease is a lease no session holds.
	productionAgentGenerationServeStaleLease = "agent-generation-serve-stale-lease"
	// productionAgentGenerationServeForeignProject is a project this session is
	// not admitted for.
	productionAgentGenerationServeForeignProject = "project-agent-generation-foreign"
)

// productionAgentGenerationServeLane is one admitted project, one committed model
// origin, and the served two names over it.
type productionAgentGenerationServeLane struct {
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
	projectID   string
	leaseID     string
}

// productionAgentGenerationServeOpen builds the lane. It fails rather than skips
// when the owner cannot be built: the fixture already skips without the shipped
// schema oracle.
func productionAgentGenerationServeOpen(t *testing.T) *productionAgentGenerationServeLane {
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
	owner, err := newProductionGeneration(open, server, nil)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation owner: %v", err)
	}
	if err := newProductionAgentGenerationServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the agent-generation serve failed to bind: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	// The lane closes in the one order productionHandlerWithContract's cleanup
	// uses: the server, then the admitted runtime that holds the writer lease.
	t.Cleanup(func() {
		server.Close()
		_ = open.Close()
	})
	author, frozen := productionExecuteSession(t, mux, projectPath, productionAgentGenerationServeNonce)
	return &productionAgentGenerationServeLane{
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		author:      author,
		frozen:      frozen,
		projectID:   syntheticString(frozen["projectId"]),
		leaseID:     syntheticString(frozen["leaseId"]),
	}
}

// productionAgentGenerationServeRequest is the reference's begin envelope
// (electron/services/agent-generation.ts:141-145): a nonce, a model id and the
// validated input, with the tool manifest the protocol needs.
func productionAgentGenerationServeRequest(nonce, message string) map[string]any {
	return map[string]any{
		"uiActionNonce": nonce,
		"modelId":       productionTestModelID,
		"input": map[string]any{
			"mode":     "planning",
			"uiLocale": "zh-CN",
			"historyMessages": []any{
				map[string]any{"role": "user", "content": "先读项目事实"},
				map[string]any{"role": "assistant", "content": "已读取项目配置。"},
			},
			"userMessage": message,
			"tools": []any{map[string]any{
				"name":                 productionAgentGenerationServeToolName,
				"description":          "read the admitted project core",
				"inputSchema":          map[string]any{"type": "object", "properties": map[string]any{}},
				"requiresConfirmation": false,
				"isReadOnly":           true,
				"source":               "builtin",
			}},
		},
	}
}

// productionAgentGenerationServeRecovery is the wire shape read and begin answer
// with (src/shared/agent-generation.ts:104-112).
type productionAgentGenerationServeRecovery struct {
	Handle struct {
		ProjectID    string `json:"projectId"`
		Epoch        string `json:"epoch"`
		RootActionID string `json:"rootActionId"`
		RunID        string `json:"runId"`
	} `json:"handle"`
	ModelID string `json:"modelId"`
	Context struct {
		Version         int64            `json:"version"`
		Input           map[string]any   `json:"input"`
		WritingLanguage string           `json:"writingLanguage"`
		InitialMessages []map[string]any `json:"initialMessages"`
	} `json:"context"`
	Rounds []struct {
		Index        int64            `json:"index"`
		Status       string           `json:"status"`
		VisibleText  string           `json:"visibleText"`
		ProtocolText string           `json:"protocolText"`
		AttemptID    string           `json:"attemptId"`
		Actions      []map[string]any `json:"actions"`
		Handle       map[string]any   `json:"handle"`
	} `json:"rounds"`
	SourceStatus string         `json:"sourceStatus"`
	NextRound    *int64         `json:"nextRound"`
	Run          map[string]any `json:"run"`
}

// productionAgentGenerationServeAnswer is one successful call: the decoded
// recovery, plus the raw result body for a caller that wants to compare the
// exact envelope.
func productionAgentGenerationServeAnswer(t *testing.T, lane *productionAgentGenerationServeLane, operation string, args []any) (productionAgentGenerationServeRecovery, string) {
	t.Helper()
	var recovery productionAgentGenerationServeRecovery
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
	if err := json.Unmarshal(wire.Result, &recovery); err != nil {
		t.Fatalf("%v: %v %v", operation, err, string(wire.Result))
	}
	return recovery, string(wire.Result)
}

// productionAgentGenerationServeRefusal is one refused call: the status and the
// body, which is what a byte-identical refusal is compared on.
func productionAgentGenerationServeRefusal(t *testing.T, lane *productionAgentGenerationServeLane, operation string, args []any) (int, string) {
	t.Helper()
	response := lane.author.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%v was admitted with %v", operation, response.Body.String())
	}
	return response.Code, response.Body.String()
}

// productionAgentGenerationServeRuns counts the runs under one root over a
// read-only connection, so "a replay opened nothing" is asserted on the file.
func productionAgentGenerationServeRuns(t *testing.T, lane *productionAgentGenerationServeLane, rootActionID string) int {
	t.Helper()
	return productionAgentGenerationCount(t, lane.database,
		"SELECT COUNT(*) FROM generation_runs WHERE root_action_id=?", rootActionID)
}

// TestProductionAgentGenerationServeBegin proves begin freezes the shipped
// context, opens one ledger run under the session, and answers the reference's
// recovery with one not-started round.
func TestProductionAgentGenerationServeBegin(t *testing.T) {
	lane := productionAgentGenerationServeOpen(t)
	recovery, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationBegin,
		[]any{productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeUser), lane.frozen})

	if recovery.Handle.ProjectID != lane.projectID || recovery.Handle.RunID == "" || recovery.Handle.RootActionID == "" {
		t.Fatalf("begin answered a handle that does not name this project's run: %+v", recovery.Handle)
	}
	if recovery.Handle.Epoch != lane.leaseID {
		t.Fatalf("the run froze epoch %v, want the admitted lease", recovery.Handle.Epoch)
	}
	if recovery.ModelID != productionTestModelID {
		t.Fatalf("the recovery reports model %v, want the frozen model", recovery.ModelID)
	}
	if recovery.Context.Version != 1 || recovery.Context.WritingLanguage != prompttemplate.LanguageChinese {
		t.Fatalf("the frozen context is not version 1 zh-CN: %+v", recovery.Context)
	}
	// The request's own fields, in the reference's initialMessages order: the
	// composed system message, the frozen history, then the user message.
	if len(recovery.Context.InitialMessages) != 4 {
		t.Fatalf("the context froze %v messages, want system + 2 history + user", len(recovery.Context.InitialMessages))
	}
	if syntheticString(recovery.Context.InitialMessages[0]["role"]) != "system" {
		t.Fatalf("the first frozen message is not the system message: %v", recovery.Context.InitialMessages[0])
	}
	system := syntheticString(recovery.Context.InitialMessages[0]["content"])
	// Every section is read out of the shipped asset, never restated here: the
	// point is that the frozen message carries that prose and the tool manifest.
	shipped, err := prompttemplate.AgentIdentityPrompt(recovery.Context.WritingLanguage)
	if err != nil {
		t.Fatalf("%v", err)
	}
	for _, want := range []string{
		strings.TrimSpace(shipped.Template.SystemRole),
		shipped.SystemContract,
		shipped.ModePlanning,
		shipped.Template.SystemSuffix,
		shipped.AuthorGuidanceHeading,
		shipped.ProjectSettingsHeading,
		shipped.ToolHeading,
		productionAgentGenerationServeToolName,
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("the frozen system message does not carry %v", want)
		}
	}
	if syntheticString(recovery.Context.InitialMessages[3]["content"]) != productionAgentGenerationServeUser {
		t.Fatal("the frozen user message is not the request's")
	}
	if len(recovery.Rounds) != 1 {
		t.Fatalf("a fresh session answered %v rounds", len(recovery.Rounds))
	}
	if recovery.Rounds[0].Index != 0 || recovery.Rounds[0].Status != agentgen.RoundNotStarted ||
		len(recovery.Rounds[0].Actions) != 0 || recovery.Rounds[0].ProtocolText != "" {
		t.Fatalf("round 0 is not the reference's not-started round: %+v", recovery.Rounds[0])
	}
	if recovery.Rounds[0].AttemptID != "" {
		t.Fatalf("a round that never dispatched carries attempt %v", recovery.Rounds[0].AttemptID)
	}
	if recovery.NextRound == nil {
		t.Fatal("nextRound is null, want 0")
	}
	if *recovery.NextRound != 0 {
		t.Fatalf("nextRound is %v, want 0", *recovery.NextRound)
	}
	if recovery.SourceStatus != productionAgentGenerationStatusCurrent {
		t.Fatalf("sourceStatus is %v, want current", recovery.SourceStatus)
	}
	if syntheticString(syntheticMap(recovery.Run["handle"])["runId"]) != recovery.Handle.RunID {
		t.Fatalf("the run view is not the run the handle names: %v", recovery.Run)
	}

	// The ledger row, read back over a connection of the test's own.
	manifest := productionAgentGenerationFrozen(t, lane.database, recovery.Handle.RunID)
	if syntheticString(manifest["operation"]) != agentgen.Operation ||
		syntheticString(manifest["modelId"]) != productionTestModelID ||
		syntheticString(manifest["output"]) != productionAgentGenerationVisibleText {
		t.Fatalf("the run froze the wrong manifest: %v", manifest)
	}
	key, err := agentgen.SessionKey(lane.projectID, productionAgentGenerationServeNonce)
	if err != nil {
		t.Fatal(err)
	}
	session := syntheticMap(manifest["agentSession"])
	if syntheticString(session["key"]) != key || syntheticNumber(session["roundIndex"]) != 0 {
		t.Fatalf("the run froze session %v, want the derived key at round 0", session)
	}
	if prompts := syntheticSlice(manifest["promptKeys"]); len(prompts) != 1 ||
		syntheticString(prompts[0]) != prompttemplate.AgentIdentityKey {
		t.Fatalf("the run froze prompt keys %v, want the identity template", prompts)
	}
	// The context digest is over the exact stored text, and the frozen input is
	// the request's own.
	stored := productionAgentGenerationServeStoredContext(t, lane, recovery.Handle.RunID)
	if syntheticHash(stored) != syntheticString(manifest["agentContextHash"]) {
		t.Fatal("the frozen context digest is not the digest of the stored context")
	}
	var contextValue struct {
		Input struct {
			UserMessage string `json:"userMessage"`
			Mode        string `json:"mode"`
			Tools       []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"input"`
	}
	if err := json.Unmarshal([]byte(stored), &contextValue); err != nil {
		t.Fatalf("%v %s", err, stored)
	}
	if contextValue.Input.UserMessage != productionAgentGenerationServeUser || contextValue.Input.Mode != "planning" ||
		len(contextValue.Input.Tools) != 1 || contextValue.Input.Tools[0].Name != productionAgentGenerationServeToolName {
		t.Fatalf("the frozen input is not the request's: %+v", contextValue.Input)
	}
	// The session's nonce is derived, not spent: `agent:<key>:0`.
	var actionJSON string
	if err := productionAgentGenerationReadDB(t, lane.database, "SELECT action_json FROM generation_roots WHERE root_action_id=?",
		[]any{recovery.Handle.RootActionID}, &actionJSON); err != nil {
		t.Fatalf("%v", err)
	}
	var action map[string]any
	if err := json.Unmarshal([]byte(actionJSON), &action); err != nil {
		t.Fatalf("%v", err)
	}
	if syntheticString(action["uiActionNonce"]) != productionAgentGenerationNoncePrefix+key+":0" {
		t.Fatalf("the root froze nonce %v, want the derived agent nonce", action["uiActionNonce"])
	}
	if runs := productionAgentGenerationServeRuns(t, lane, recovery.Handle.RootActionID); runs != 1 {
		t.Fatalf("begin opened %v runs, want 1", runs)
	}
}

// TestProductionAgentGenerationServeRead proves read answers the same recovery
// for the begun run, is idempotent over the projection the renderer reads, and
// projects the run's state rather than the request's.
func TestProductionAgentGenerationServeRead(t *testing.T) {
	lane := productionAgentGenerationServeOpen(t)
	begun, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationBegin,
		[]any{productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeUser), lane.frozen})
	handle := productionAgentGenerationWireHandle(syntheticHandle{
		ProjectID:    begun.Handle.ProjectID,
		Epoch:        begun.Handle.Epoch,
		RootActionID: begun.Handle.RootActionID,
		RunID:        begun.Handle.RunID,
	})
	first, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationRead, []any{map[string]any{"handle": handle}, lane.frozen})
	if first.Handle.RunID != begun.Handle.RunID || first.Handle.Epoch != begun.Handle.Epoch {
		t.Fatalf("read answered %+v, want the begun run's handle", first.Handle)
	}
	if first.ModelID != begun.ModelID || first.SourceStatus != productionAgentGenerationStatusCurrent ||
		first.NextRound == nil || *first.NextRound != 0 {
		t.Fatalf("read answered a different recovery than begin: %+v", first)
	}
	if len(first.Rounds) != 1 || first.Rounds[0].Status != agentgen.RoundNotStarted {
		t.Fatalf("read projected %+v, want the one not-started round", first.Rounds)
	}
	// The context read reports is the context begin froze, byte for byte: a read
	// that recomposed it would drift as soon as the project core changed.
	begunContext, err := json.Marshal(begun.Context)
	if err != nil {
		t.Fatal(err)
	}
	readContext, err := json.Marshal(first.Context)
	if err != nil {
		t.Fatal(err)
	}
	if string(begunContext) != string(readContext) {
		t.Fatal("read answered a context that begin did not freeze")
	}
	// A replay of the same read is idempotent over every field the renderer
	// consumes (src/shared/agent-generation.ts:104-112, AgentGenerationRecovery):
	// handle, modelId, context, rounds, sourceStatus and nextRound. The only
	// member that legitimately differs is run.budget.deadlineAt - the
	// MainGenerationRunView's live deadline (viewFor composes it from
	// time.Now().UnixMilli()), which the renderer reads for display, never for
	// replay identity - so the raw envelope is not compared byte for byte here.
	replayed, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationRead, []any{map[string]any{"handle": handle}, lane.frozen})
	if replayed.Handle != first.Handle || replayed.ModelID != first.ModelID ||
		replayed.SourceStatus != first.SourceStatus {
		t.Fatalf("a replayed read changed the recovery identity: %+v -> %+v", first, replayed)
	}
	if (replayed.NextRound == nil) != (first.NextRound == nil) ||
		(replayed.NextRound != nil && *replayed.NextRound != *first.NextRound) {
		t.Fatal("a replayed read changed nextRound")
	}
	replayContext, err := json.Marshal(replayed.Context)
	if err != nil {
		t.Fatal(err)
	}
	if string(begunContext) != string(replayContext) {
		t.Fatal("a replayed read changed the frozen context")
	}
	firstRounds, err := json.Marshal(first.Rounds)
	if err != nil {
		t.Fatal(err)
	}
	replayRounds, err := json.Marshal(replayed.Rounds)
	if err != nil {
		t.Fatal(err)
	}
	if string(firstRounds) != string(replayRounds) {
		t.Fatal("a replayed read changed the projected rounds")
	}
	if syntheticString(syntheticMap(replayed.Run["handle"])["runId"]) != first.Handle.RunID {
		t.Fatal("a replayed read changed the run it projects")
	}
	if runs := productionAgentGenerationServeRuns(t, lane, begun.Handle.RootActionID); runs != 1 {
		t.Fatalf("a replayed read left %v runs", runs)
	}
}

// TestProductionAgentGenerationServeReplay proves a spent nonce answers the run
// it already opened, and that a nonce that froze a different input refuses.
func TestProductionAgentGenerationServeReplay(t *testing.T) {
	lane := productionAgentGenerationServeOpen(t)
	request := productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeUser)
	first, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationBegin, []any{request, lane.frozen})
	replayed, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationBegin, []any{request, lane.frozen})
	if replayed.Handle.RunID != first.Handle.RunID {
		t.Fatalf("the replay answered run %v, want %v", replayed.Handle.RunID, first.Handle.RunID)
	}
	if runs := productionAgentGenerationServeRuns(t, lane, first.Handle.RootActionID); runs != 1 {
		t.Fatalf("the replay opened a second run: %v", runs)
	}
	// The same nonce with another message is not the same session.
	conflict := productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeOther)
	status, body := productionAgentGenerationServeRefusal(t, lane, productionAgentGenerationBegin, []any{conflict, lane.frozen})
	if !strings.Contains(body, agentgen.CodeNonceConflict) {
		t.Fatalf("a conflicting nonce answered %v %v, want %v", status, body, agentgen.CodeNonceConflict)
	}
}

// TestProductionAgentGenerationServeRefusals pins the refusals both served names
// answer: a lease this server never issued, a handle of another project, and a
// handle whose epoch is not the run's.
func TestProductionAgentGenerationServeRefusals(t *testing.T) {
	lane := productionAgentGenerationServeOpen(t)
	begun, _ := productionAgentGenerationServeAnswer(t, lane, productionAgentGenerationBegin,
		[]any{productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeUser), lane.frozen})
	handle := syntheticHandle{ProjectID: begun.Handle.ProjectID, Epoch: begun.Handle.Epoch,
		RootActionID: begun.Handle.RootActionID, RunID: begun.Handle.RunID}

	// The stale lease is refused by the session fence before either name looks at
	// its own request, and the two names answer the same bytes for it.
	stale := map[string]any{"projectId": lane.projectID, "projectPath": lane.projectPath,
		"leaseId": productionAgentGenerationServeStaleLease}
	beginStatus, beginBody := productionAgentGenerationServeRefusal(t, lane, productionAgentGenerationBegin,
		[]any{productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeUser), stale})
	readStatus, readBody := productionAgentGenerationServeRefusal(t, lane, productionAgentGenerationRead,
		[]any{map[string]any{"handle": productionAgentGenerationWireHandle(handle)}, stale})
	if beginStatus != readStatus || beginBody != readBody {
		t.Fatalf("a stale lease answered differently per name: begin %v %v, read %v %v", beginStatus, beginBody, readStatus, readBody)
	}
	if beginBody == "" {
		t.Fatal("a stale lease answered an empty body")
	}

	// A handle of another project, and a handle whose epoch is not the run's, are
	// the same refusal: neither names this run.
	foreign := handle
	foreign.ProjectID = productionAgentGenerationServeForeignProject
	foreignStatus, foreignBody := productionAgentGenerationServeRefusal(t, lane, productionAgentGenerationRead,
		[]any{map[string]any{"handle": productionAgentGenerationWireHandle(foreign)}, lane.frozen})
	if !strings.Contains(foreignBody, agentgen.CodeIdentityMismatch) {
		t.Fatalf("a foreign project answered %v %v, want %v", foreignStatus, foreignBody, agentgen.CodeIdentityMismatch)
	}
	wrongEpoch := handle
	wrongEpoch.Epoch = productionAgentGenerationServeStaleLease
	epochStatus, epochBody := productionAgentGenerationServeRefusal(t, lane, productionAgentGenerationRead,
		[]any{map[string]any{"handle": productionAgentGenerationWireHandle(wrongEpoch)}, lane.frozen})
	if epochStatus != foreignStatus || epochBody != foreignBody {
		t.Fatalf("a foreign epoch answered differently than a foreign project: %v %v vs %v %v", epochStatus, epochBody, foreignStatus, foreignBody)
	}

	// A malformed begin is its own refusal, before any run is opened. A widened
	// envelope is refused rather than silently ignored.
	widened := productionAgentGenerationServeRequest(productionAgentGenerationServeNonce, productionAgentGenerationServeUser)
	widened["index"] = 0
	for label, request := range map[string]map[string]any{
		"widened":     widened,
		"no nonce":    {"modelId": productionTestModelID, "input": productionAgentGenerationServeRequest("", productionAgentGenerationServeUser)["input"]},
		"no model":    {"uiActionNonce": productionAgentGenerationServeNonce, "input": productionAgentGenerationServeRequest("", productionAgentGenerationServeUser)["input"]},
		"no input":    {"uiActionNonce": productionAgentGenerationServeNonce, "modelId": productionTestModelID},
		"empty input": {"uiActionNonce": productionAgentGenerationServeNonce, "modelId": productionTestModelID, "input": map[string]any{}},
	} {
		status, body := productionAgentGenerationServeRefusal(t, lane, productionAgentGenerationBegin, []any{request, lane.frozen})
		if !strings.Contains(body, agentgen.CodeBeginInvalid) && !strings.Contains(body, agentgen.CodeInputInvalid) {
			t.Fatalf("%v begin answered %v %v, want a begin or input refusal", label, status, body)
		}
	}
	if runs := productionAgentGenerationServeRuns(t, lane, begun.Handle.RootActionID); runs != 1 {
		t.Fatalf("a refused begin left %v runs", runs)
	}
}

// TestProductionAgentGenerationServeBinder pins the served half's shape: a nil
// owner installs nothing, a nil server is refused, both names are inventoried,
// and neither is still described as refused.
func TestProductionAgentGenerationServeBinder(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionAgentGenerationServe(nil).Bind(server); err != nil {
		t.Fatalf("the serve binder failed with a nil owner: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the serve binder installed %v with a nil owner, want nothing", names)
	}
	if err := newProductionAgentGenerationServe(nil).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
	for _, operation := range productionAgentGenerationServedOperations() {
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%v is not inventoried, so its absent handler would not be 501 either: %v", operation, err)
		}
		if _, refused := productionAgentGenerationSeamFor(operation); refused {
			t.Fatalf("%v is served and still described as refused", operation)
		}
		if ledger, found := productionAgentGenerationLedgerOperation(operation); found {
			t.Fatalf("%v is served but still maps to ledger operation %v", operation, ledger)
		}
	}
	for _, operation := range productionAgentGenerationOperations() {
		seam, found := productionAgentGenerationSeamFor(operation)
		if !found || seam.Ledger != agentgen.Operation || len(seam.Missing) == 0 {
			t.Fatalf("%v is refused without a documented seam", operation)
		}
	}
}

// TestProductionAgentGenerationServeComposition proves the context this lane
// freezes is the reference's buildAgentGenerationContext: the section order, the
// mode substitution, the optional sections, and the input validator's refusals.
func TestProductionAgentGenerationServeComposition(t *testing.T) {
	core := `{"id":"main","totalChapters":12}`
	for _, language := range []string{prompttemplate.LanguageChinese, prompttemplate.LanguageEnglish} {
		language := language
		t.Run(language, func(t *testing.T) {
			shipped, err := prompttemplate.AgentIdentityPrompt(language)
			if err != nil {
				t.Fatalf("%v", err)
			}
			input, err := agentgen.ParseInput(map[string]any{
				"mode":            "fast",
				"uiLocale":        language,
				"historyMessages": []any{},
				"userMessage":     productionAgentGenerationServeUser,
				"editorContext":   "选中的第一段",
				"tools":           []any{},
			})
			if err != nil {
				t.Fatalf("%v", err)
			}
			composed, err := agentgen.BuildContext(input, core, language, productionAgentGenerationPrompt(shipped))
			if err != nil {
				t.Fatalf("%v", err)
			}
			if composed.Version != 1 || len(composed.Messages) != 2 ||
				composed.Messages[0].Role != "system" || composed.Messages[1].Role != "user" {
				t.Fatalf("the composed context is not system+user: %+v", composed.Messages)
			}
			system := composed.Messages[0].Content
			// Every section, in the reference's order.
			order := []string{
				strings.TrimSpace(shipped.Template.SystemRole),
				shipped.SystemContract,
				shipped.ModeFast,
				shipped.AuthorGuidanceHeading,
				shipped.Template.SystemSuffix,
				shipped.ProjectSettingsHeading,
				core,
				shipped.EditorContextHeading,
				"选中的第一段",
			}
			previous := -1
			for _, fragment := range order {
				at := strings.Index(system, fragment)
				if at < 0 {
					t.Fatalf("the system message lost %v: %s", fragment, system)
				}
				if at <= previous {
					t.Fatalf("the system message reordered %v: %s", fragment, system)
				}
				previous = at
			}
			if strings.Contains(system, "{{mode_instruction}}") {
				t.Fatal("the system message kept the unfilled variable")
			}
			if strings.Contains(system, shipped.ModePlanning) {
				t.Fatal("a fast input froze the planning instruction")
			}
			// The no-tools case carries no manifest block.
			if strings.Contains(system, shipped.ToolHeading) {
				t.Fatal("an input with no tool froze a tool manifest")
			}
			// The context round-trips through its own reader.
			encoded, err := composed.Text()
			if err != nil {
				t.Fatalf("%v", err)
			}
			decoded, err := agentgen.ParseContext([]byte(encoded))
			if err != nil {
				t.Fatalf("%v", err)
			}
			again, err := decoded.Text()
			if err != nil || again != encoded {
				t.Fatalf("the context did not round-trip: %v", err)
			}
			// A planning input freezes the other instruction, and an editor context
			// is optional rather than an empty section.
			planning := input
			planning.Mode = "planning"
			planning.HasEditorContext = false
			planning.EditorContext = ""
			value, err := agentgen.BuildContext(planning, core, language, productionAgentGenerationPrompt(shipped))
			if err != nil {
				t.Fatalf("%v", err)
			}
			if !strings.Contains(value.Messages[0].Content, shipped.ModePlanning) ||
				strings.Contains(value.Messages[0].Content, shipped.EditorContextHeading) {
				t.Fatal("the planning input or the absent editor context composed the wrong sections")
			}
		})
	}

	// The input validator refuses every shape the reference refuses.
	valid := map[string]any{
		"mode":            "fast",
		"uiLocale":        prompttemplate.LanguageChinese,
		"historyMessages": []any{},
		"userMessage":     productionAgentGenerationServeUser,
		"tools":           []any{},
	}
	if _, err := agentgen.ParseInput(valid); err != nil {
		t.Fatalf("a valid input was refused: %v", err)
	}
	for label, mutate := range map[string]func(map[string]any){
		"another mode":   func(value map[string]any) { value["mode"] = "turbo" },
		"another locale": func(value map[string]any) { value["uiLocale"] = "fr-FR" },
		"blank message":  func(value map[string]any) { value["userMessage"] = "   " },
		"unknown member": func(value map[string]any) { value["extra"] = true },
		"too much history": func(value map[string]any) {
			history := make([]any, 33)
			for index := range history {
				history[index] = map[string]any{"role": "user", "content": "x"}
			}
			value["historyMessages"] = history
		},
		"system history": func(value map[string]any) {
			value["historyMessages"] = []any{map[string]any{"role": "system", "content": "x"}}
		},
		"unconfirmed start_workflow": func(value map[string]any) {
			value["tools"] = []any{map[string]any{"name": "start_workflow", "description": "",
				"inputSchema": map[string]any{}, "requiresConfirmation": false, "isReadOnly": false, "source": "builtin"}}
		},
		"duplicate tool": func(value map[string]any) {
			tool := map[string]any{"name": productionAgentGenerationServeToolName, "description": "",
				"inputSchema": map[string]any{}, "requiresConfirmation": false, "isReadOnly": true, "source": "builtin"}
			value["tools"] = []any{tool, tool}
		},
		"array schema": func(value map[string]any) {
			value["tools"] = []any{map[string]any{"name": "read_core", "description": "",
				"inputSchema": []any{}, "requiresConfirmation": false, "isReadOnly": true, "source": "builtin"}}
		},
	} {
		value := map[string]any{}
		for key, item := range valid {
			value[key] = item
		}
		mutate(value)
		if _, err := agentgen.ParseInput(value); err == nil {
			t.Fatalf("%v was accepted", label)
		}
	}
}

// productionAgentGenerationServeStoredContext reads the frozen agentContext
// member of one run with a connection of the test's own.
func productionAgentGenerationServeStoredContext(t *testing.T, lane *productionAgentGenerationServeLane, runID string) string {
	t.Helper()
	var binding string
	if err := productionAgentGenerationReadDB(t, lane.database, "SELECT binding_json FROM generation_runs WHERE run_id=?",
		[]any{runID}, &binding); err != nil {
		t.Fatalf("%v", err)
	}
	manifest, ok := productionEditorInlineRawMember([]byte(binding), "sourceManifest")
	if !ok {
		t.Fatal("the run froze no source manifest")
	}
	raw, ok := productionEditorInlineRawMember(manifest, "agentContext")
	if !ok {
		t.Fatal("the run froze no agent context")
	}
	return string(raw)
}

// productionAgentGenerationReadDB reads one column of one row over a read-only
// connection, so the assertion is made on the file and not on the lane.
func productionAgentGenerationReadDB(t *testing.T, database, query string, args []any, into *string) error {
	t.Helper()
	db := productionAgentGenerationReadOnly(t, database)
	defer func() { _ = db.Close() }()
	return db.QueryRow(query, args...).Scan(into)
}

// TestProductionAgentGenerationServeUnbound keeps the one property the residual
// test cannot assert once the binder line lands: a served name is never answered
// by the refusing binder alone.
func TestProductionAgentGenerationServeUnbound(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-admitted")
	if err := bindProductionAgentGeneration(server); err != nil {
		t.Fatalf("the refusal binder failed: %v", err)
	}
	for _, served := range productionAgentGenerationServedOperations() {
		for _, bound := range server.BoundOperations() {
			if bound == served {
				t.Fatalf("%v was installed by the refusing binder", served)
			}
		}
	}
	// A served name is not in the refused table, and a refused name is.
	for _, served := range productionAgentGenerationServedOperations() {
		if _, refused := productionAgentGenerationSeamFor(served); refused {
			t.Fatalf("%v is both served and refused", served)
		}
	}
}

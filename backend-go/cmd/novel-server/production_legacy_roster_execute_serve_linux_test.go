//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file proves legacy-roster:execute over the real HTTP handler, through the
// frozen project session, with a real admitted author database, a real model
// lease, and the model stubbed at the lane's own seam
// (productionConfig.Dispatch -> generationowner.Executor.DispatchFunc). The lane
// is built by this file rather than by productionHandlerWithContract because the
// binder line that installs the name into the product lane lives in
// production_open_linux.go's owner block and that file does not belong to this
// change; the binder itself is the one under test.
//
// Every success is read back through a connection of the test's own: the frozen
// legacyRosterTask from the run's binding_json, and the replayTask every attempt
// stored on its usage receipt. That is how "the dispatched task is the shipped
// asset, not a Go literal" is asserted - the prompt is never restated here.

// productionLegacyRosterExecuteRoster is one candidate the model stub answers
// with. It is fixture data, not prompt prose: the shipped prompt text reaches
// this package only through internal/prompttemplate.
const productionLegacyRosterExecuteRoster = `{"schemaVersion":1,"entries":[` +
	`{"name":"甲","role":"protagonist","gender":"男","age":"二十","appearance":"青衫","personality":"谨慎",` +
	`"background":"山村少年","abilities":"剑术","motivation":"复仇",` +
	`"relationships":[{"target":"乙","relation":"宿敌"}],"arc":"成长","notes":"（待确认）",` +
	`"currentState":{"location":"山村","powerLevel":"初境","physicalState":"健康","mentalState":"坚定",` +
	`"keyItems":"木剑","recentEvents":"灭门","updatedAtChapter":0}},` +
	`{"name":"乙","role":"antagonist","gender":"女","age":"二十","appearance":"红衣","personality":"狠辣",` +
	`"background":"魔教","abilities":"毒术","motivation":"夺权",` +
	`"relationships":[{"target":"甲","relation":"宿敌"}],"arc":"覆灭","notes":"（待确认）",` +
	`"currentState":{"location":"魔教","powerLevel":"化境","physicalState":"健康","mentalState":"倨傲",` +
	`"keyItems":"毒囊","recentEvents":"出关","updatedAtChapter":0}}` +
	`]}`

// productionLegacyRosterExecuteMarkdown is the preserved legacy text this file
// freezes. It deliberately carries none of U+2028, U+2029, '<', '>' or '&':
// the served begin serializes the frozen source with encoding/json, which HTML/
// U+2028-escapes those five characters, while the ledger stores the manifest back
// with plotQuote, which does not. A preserved text carrying any of them would
// make the frozen context hash diverge from the stored bytes and refuse begin
// with GENERATION_LEGACY_CONTEXT_INVALID. The loop this file proves is the
// dispatch/repair half, not the begin context's escaping, so the fixture avoids
// them.
const productionLegacyRosterExecuteMarkdown = "旧角色图谱：甲是主角，性格谨慎。\n乙是配角，与甲为敌。\n"

// productionLegacyRosterExecuteBroken is an answer parseLegacyRosterJson cannot
// read, which is what the repair stage exists for.
const productionLegacyRosterExecuteBroken = "模型没有返回 JSON，只返回了一段解释。"

// productionLegacyRosterExecuteStaleLease is a lease this session never held.
const productionLegacyRosterExecuteStaleLease = "legacy-roster-execute-stale-lease"

// productionLegacyRosterExecuteForeignProject is a project this session is not
// admitted for.
const productionLegacyRosterExecuteForeignProject = "legacy-roster-execute-foreign-project"

// errProductionLegacyRosterExecuteSeamOverflow fails the lane when the loop asks
// for more answers than the test queued: a lane that invented a dispatch cannot
// pass as the reference's loop.
var errProductionLegacyRosterExecuteSeamOverflow = errors.New("PRODUCTION_LEGACY_ROSTER_EXECUTE_SEAM_OVERFLOW")

// productionLegacyRosterExecuteSeam is the executor's injectable dispatch seam.
// It answers the queued visible texts in order and counts every dispatch, so a
// replay that dialed again is visible from the test's side.
type productionLegacyRosterExecuteSeam struct {
	mu       sync.Mutex
	answers  []string
	finishes []string
	calls    int
}

func (s *productionLegacyRosterExecuteSeam) dispatch(_ context.Context, _ any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.mu.Unlock()
	if index >= len(s.answers) {
		return generation.DispatchResult{}, errProductionLegacyRosterExecuteSeamOverflow
	}
	finish := "stop"
	if index < len(s.finishes) && s.finishes[index] != "" {
		finish = s.finishes[index]
	}
	if callbacks.OnVisible != nil {
		callbacks.OnVisible(generation.VisibleEvent{Kind: "cumulative", Text: s.answers[index]})
	}
	// The reduced lane plans an "unknown" reasoning policy, so the settled usage
	// has to satisfy total == prompt+completion and reasoning <= completion.
	return generation.DispatchResult{
		Usage:        map[string]any{"trusted": true, "promptTokens": 4, "completionTokens": 16, "reasoningTokens": 0, "totalTokens": 20},
		FinishReason: finish,
	}, nil
}

// answered is the number of dispatches the loop made.
func (s *productionLegacyRosterExecuteSeam) answered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// productionLegacyRosterExecuteMessage is one task message as the ledger stores it.
type productionLegacyRosterExecuteMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// productionLegacyRosterExecuteTask is the task shape both the frozen binding and
// the attempt's replayTask carry.
type productionLegacyRosterExecuteTask struct {
	Purpose        string                                 `json:"purpose"`
	ReasoningStage string                                 `json:"reasoningStage"`
	Output         string                                 `json:"output"`
	Messages       []productionLegacyRosterExecuteMessage `json:"messages"`
}

// productionLegacyRosterExecuteAttempt is one attempt of the run under test.
type productionLegacyRosterExecuteAttempt struct {
	Nonce string
	Task  productionLegacyRosterExecuteTask
}

// productionLegacyRosterExecuteServeLane is one admitted pending-repair project,
// one committed model origin, and the family's served names over it.
type productionLegacyRosterExecuteServeLane struct {
	projectPath string
	database    string
	listener    net.Listener
	origin      string
	author      *productionDurableClient
	frozen      map[string]any
	runID       string
	seam        *productionLegacyRosterExecuteSeam
}

// productionLegacyRosterExecuteServeOpen builds the lane. It fails rather than
// skips when the owner cannot be built: the fixture already skips without the
// shipped schema oracle.
func productionLegacyRosterExecuteServeOpen(t *testing.T, answers, finishes []string, nonce string) *productionLegacyRosterExecuteServeLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	// The one project state execute can own: the preserved roster is pending, no
	// cards exist, and the writing language pins the zh-CN replacement prompt.
	productionLegacyRosterSeed(t, database, []productionLegacyRosterStatement{
		{Query: "INSERT OR REPLACE INTO character_identity_meta (id, revision, legacy_count) VALUES ('main', 0, 0)"},
		{Query: `INSERT OR REPLACE INTO character_roster_meta
 (id, schema_version, revision, migration_state, legacy_markdown, projection_hash, fact_hash)
 VALUES ('main', 1, 0, 'legacy_markdown_pending', ?, '', '')`,
			Args: []any{productionLegacyRosterExecuteMarkdown}},
		{Query: "DELETE FROM characters"},
		{Query: "UPDATE project_core SET writing_language='zh-CN' WHERE id='main'"},
	})
	listener, origin := productionGenerationWriteListener(t)
	productionGenerationWriteModel(t, root, origin)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatalf("%v", err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatalf("the lane could not open the admitted project: %v", err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-legacy-roster-execute")
	if err := server.BindNavigation(); err != nil {
		_ = open.Close()
		t.Fatalf("the lane could not bind navigation: %v", err)
	}
	if err := server.BindProjects(open.durable); err != nil {
		_ = open.Close()
		t.Fatalf("the lane could not bind the project lifecycle: %v", err)
	}
	seam := &productionLegacyRosterExecuteSeam{answers: answers, finishes: finishes}
	owner, err := newProductionGeneration(open, server, seam.dispatch)
	if err != nil {
		_ = open.Close()
		t.Fatalf("the generation owner: %v", err)
	}
	if err := newProductionLegacyRosterServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the legacy-roster serve failed to bind: %v", err)
	}
	if err := newProductionLegacyRosterExecuteServe(owner).Bind(server); err != nil {
		_ = open.Close()
		t.Fatalf("the legacy-roster execute serve failed to bind: %v", err)
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
	author, frozen := productionExecuteSession(t, mux, projectPath, nonce+"-open")
	var recovery struct {
		View    map[string]any `json:"view"`
		ModelID string         `json:"modelId"`
	}
	author.decode(t, productionLegacyRosterBeginName,
		[]any{map[string]any{"modelId": productionTestModelID, "uiActionNonce": nonce}, frozen}, &recovery)
	handle := syntheticMap(recovery.View["handle"])
	if handle == nil || syntheticString(handle["runId"]) == "" || syntheticString(handle["rootActionId"]) == "" {
		t.Fatalf("legacy-roster:begin returned no run handle: %v", recovery.View)
	}
	return &productionLegacyRosterExecuteServeLane{
		projectPath: projectPath,
		database:    database,
		listener:    listener,
		origin:      origin,
		author:      author,
		frozen:      frozen,
		runID:       syntheticString(handle["runId"]),
		seam:        seam,
	}
}

// handle is the wire handle of the run this lane began.
func (l *productionLegacyRosterExecuteServeLane) handle(t *testing.T) map[string]any {
	t.Helper()
	run := l.queryRun(t)
	return map[string]any{
		"projectId":    syntheticString(l.frozen["projectId"]),
		"epoch":        run.Epoch,
		"rootActionId": run.RootActionID,
		"runId":        l.runID,
	}
}

// runRow is the identity part of the run's stored row.
type productionLegacyRosterExecuteRunRow struct {
	RootActionID string
	Epoch        string
	Task         productionLegacyRosterExecuteTask
}

// queryRun reads the run's binding_json after the binders have settled, so the
// handle and the frozen task come from the run itself, not from this test's
// expectations.
func (l *productionLegacyRosterExecuteServeLane) queryRun(t *testing.T) productionLegacyRosterExecuteRunRow {
	t.Helper()
	db := productionLegacyRosterWriter(t, l.database)
	defer func() { _ = db.Close() }()
	var bindingJSON string
	if err := db.QueryRow(`SELECT binding_json FROM generation_runs WHERE run_id=?`, l.runID).Scan(&bindingJSON); err != nil {
		t.Fatalf("read the run binding: %v", err)
	}
	var binding struct {
		Epoch          string `json:"epoch"`
		SourceManifest struct {
			// The frozen task is spliced into the manifest as the ordered JSON
			// object itself, not as a string holding one.
			Task json.RawMessage `json:"legacyRosterTask"`
		} `json:"sourceManifest"`
	}
	if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
		t.Fatalf("decode the run binding: %v", err)
	}
	var rootActionID string
	if err := db.QueryRow(`SELECT root_action_id FROM generation_runs WHERE run_id=?`, l.runID).Scan(&rootActionID); err != nil {
		t.Fatalf("read the run root: %v", err)
	}
	if len(binding.SourceManifest.Task) == 0 {
		t.Fatal("the run binding carries no frozen legacyRosterTask")
	}
	var task productionLegacyRosterExecuteTask
	if err := json.Unmarshal(binding.SourceManifest.Task, &task); err != nil {
		t.Fatalf("decode the frozen legacyRosterTask: %v", err)
	}
	return productionLegacyRosterExecuteRunRow{RootActionID: rootActionID, Epoch: binding.Epoch, Task: task}
}

// attempts reads every attempt the run stored, in insertion order.
func (l *productionLegacyRosterExecuteServeLane) attempts(t *testing.T) []productionLegacyRosterExecuteAttempt {
	t.Helper()
	db := productionLegacyRosterWriter(t, l.database)
	defer func() { _ = db.Close() }()
	rows, err := db.Query(`SELECT invocation_nonce, usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid`, l.runID)
	if err != nil {
		t.Fatalf("read the run attempts: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var attempts []productionLegacyRosterExecuteAttempt
	for rows.Next() {
		var nonce, usageJSON string
		if err := rows.Scan(&nonce, &usageJSON); err != nil {
			t.Fatalf("scan the run attempt: %v", err)
		}
		var usage struct {
			ReplayTask json.RawMessage `json:"replayTask"`
		}
		if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
			t.Fatalf("decode the attempt usage: %v", err)
		}
		if len(usage.ReplayTask) == 0 {
			t.Fatalf("%s stored no replayTask", nonce)
		}
		var task productionLegacyRosterExecuteTask
		if err := json.Unmarshal(usage.ReplayTask, &task); err != nil {
			t.Fatalf("decode the attempt replayTask: %v", err)
		}
		attempts = append(attempts, productionLegacyRosterExecuteAttempt{Nonce: nonce, Task: task})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("%v", err)
	}
	return attempts
}

// assertNoDial proves nothing reached the committed endpoint: a dial would
// already be queued on the listener, which never Accepted before this point.
func (l *productionLegacyRosterExecuteServeLane) assertNoDial(t *testing.T) {
	t.Helper()
	if tcp, ok := l.listener.(*net.TCPListener); ok {
		if err := tcp.SetDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
			t.Fatalf("%v", err)
		}
	}
	connection, err := l.listener.Accept()
	if err == nil {
		_ = connection.Close()
		t.Fatalf("a dispatch dialed the committed origin %s", l.origin)
	}
}

// call posts one execute request and returns the raw response.
func (l *productionLegacyRosterExecuteServeLane) call(t *testing.T, handle map[string]any) []byte {
	t.Helper()
	response := l.author.call(t, productionLegacyRosterExecuteName, []any{map[string]any{"handle": handle}, l.frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("%s answered %d %s", productionLegacyRosterExecuteName, response.Code, response.Body.String())
	}
	return response.Body.Bytes()
}

// answer posts one execute request and returns its decoded result envelope.
func (l *productionLegacyRosterExecuteServeLane) answer(t *testing.T, handle map[string]any) map[string]any {
	t.Helper()
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(l.call(t, handle), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s answered no result: %v", productionLegacyRosterExecuteName, err)
	}
	return wire.Result
}

// assertTaskMatches is the prompt proof: the dispatched task's messages are the
// ones the caller computed from the shipped asset, byte for byte.
func productionLegacyRosterExecuteAssertTask(t *testing.T, label string, task, want productionLegacyRosterExecuteTask) {
	t.Helper()
	if task.Purpose != want.Purpose || task.ReasoningStage != want.ReasoningStage || task.Output != want.Output {
		t.Fatalf("%s: the dispatched task is not the expected one: %v", label, task)
	}
	if len(task.Messages) != len(want.Messages) {
		t.Fatalf("%s: the dispatched task carries %d messages, want %d", label, len(task.Messages), len(want.Messages))
	}
	for index := range want.Messages {
		if task.Messages[index] != want.Messages[index] {
			t.Fatalf("%s: message %d is not the shipped one", label, index)
		}
	}
}

// TestProductionLegacyRosterExecuteServesTheFrozenTask is the happy path: a
// usable roster candidate settles in one attempt, and the dispatched task is the
// run's own frozen task, whose system message is the shipped roster prompt.
func TestProductionLegacyRosterExecuteServesTheFrozenTask(t *testing.T) {
	lane := productionLegacyRosterExecuteServeOpen(t,
		[]string{productionLegacyRosterExecuteRoster}, nil, "legacy-roster-execute-success")
	handle := lane.handle(t)
	result := lane.answer(t, handle)
	outcome := syntheticMap(result["outcome"])
	if outcome == nil {
		t.Fatalf("%s answered no outcome: %v", productionLegacyRosterExecuteName, result)
	}
	if syntheticString(outcome["status"]) != productionLegacyRosterStatusCompleted ||
		syntheticString(outcome["finishReason"]) != productionLegacyRosterFinishStop {
		t.Fatalf("the settled outcome is not a stop: %v", outcome)
	}
	if syntheticString(outcome["content"]) != productionLegacyRosterExecuteRoster {
		t.Fatalf("the settled content is not the answer the model stub sent")
	}
	if view := syntheticMap(result["run"]); view == nil || syntheticString(view["operation"]) != productionLegacyRosterOperation {
		t.Fatalf("the answer carries no run view for this family: %v", result["run"])
	}
	if answered := lane.seam.answered(); answered != 1 {
		t.Fatalf("the loop dispatched %d times, want one", answered)
	}

	attempts := lane.attempts(t)
	if len(attempts) != 1 {
		t.Fatalf("the run stored %d attempts, want one", len(attempts))
	}
	if attempts[0].Nonce != "legacy:initial:0" {
		t.Fatalf("the attempt nonce is %s, want the reference's legacy:initial:0", attempts[0].Nonce)
	}
	// The dispatched task is the frozen task: purpose, stage, output and both
	// messages, read back from the run's own binding.
	frozen := lane.queryRun(t)
	productionLegacyRosterExecuteAssertTask(t, "initial", attempts[0].Task, frozen.Task)
	if len(attempts[0].Task.Messages) != 2 || len(frozen.Task.Messages) != 2 {
		t.Fatalf("the initial task carries %d dispatched and %d frozen messages, want two each",
			len(attempts[0].Task.Messages), len(frozen.Task.Messages))
	}
	roster, err := prompttemplate.LegacyRosterTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	if frozen.Task.Messages[0].Content != roster.SystemPrompt {
		t.Fatal("the frozen system message is not the shipped roster system prompt")
	}
	if attempts[0].Task.Messages[0].Content != roster.SystemPrompt {
		t.Fatal("the dispatched system message is not the shipped roster system prompt")
	}
	if !strings.Contains(attempts[0].Task.Messages[1].Content, roster.Contract) {
		t.Fatal("the dispatched user message does not carry the shipped roster contract")
	}
	if !strings.Contains(attempts[0].Task.Messages[1].Content, productionLegacyRosterExecuteMarkdown) {
		t.Fatal("the dispatched user message does not carry the preserved markdown")
	}
	lane.assertNoDial(t)
}

// TestProductionLegacyRosterExecuteRepairsAnUnparsableAnswer is the first
// non-happy branch: the stop answer does not parse, so the loop dispatches the
// shipped repair task and settles the repair answer.
func TestProductionLegacyRosterExecuteRepairsAnUnparsableAnswer(t *testing.T) {
	lane := productionLegacyRosterExecuteServeOpen(t,
		[]string{productionLegacyRosterExecuteBroken, productionLegacyRosterExecuteRoster}, nil, "legacy-roster-execute-repair")
	result := lane.answer(t, lane.handle(t))
	outcome := syntheticMap(result["outcome"])
	if syntheticString(outcome["status"]) != productionLegacyRosterStatusCompleted ||
		syntheticString(outcome["content"]) != productionLegacyRosterExecuteRoster {
		t.Fatalf("the repair did not settle the valid answer: %v", outcome)
	}
	if answered := lane.seam.answered(); answered != 2 {
		t.Fatalf("the loop dispatched %d times, want the initial and the repair", answered)
	}
	attempts := lane.attempts(t)
	if len(attempts) != 2 {
		t.Fatalf("the run stored %d attempts, want two", len(attempts))
	}
	if attempts[0].Nonce != "legacy:initial:0" || attempts[1].Nonce != "legacy:repair:0" {
		t.Fatalf("the attempt nonces are %s/%s, want legacy:initial:0 and legacy:repair:0", attempts[0].Nonce, attempts[1].Nonce)
	}

	// The repair task is the shipped one: buildLegacyRosterJsonRepairTask's
	// purpose, stage and output, the shipped repair system prompt, and the shipped
	// wrapper around the answer that did not parse.
	loop, err := prompttemplate.LegacyRosterLoopTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	repaired, err := loop.Repair(productionLegacyRosterExecuteBroken)
	if err != nil {
		t.Fatalf("%v", err)
	}
	want := productionLegacyRosterExecuteTask{
		Purpose:        "legacy-character-roster-json-repair",
		ReasoningStage: "planning",
		Output:         "structured-data",
		Messages: []productionLegacyRosterExecuteMessage{
			{Role: "system", Content: loop.RepairSystem},
			{Role: "user", Content: repaired},
		},
	}
	productionLegacyRosterExecuteAssertTask(t, "repair", attempts[1].Task, want)

	// The shipped repair prose is not written in Go: the first long line of the
	// asset's repair system prompt must not appear in any Go file of this package.
	var marker string
	for _, line := range strings.Split(loop.RepairSystem, "\n") {
		if line = strings.TrimSpace(line); len([]rune(line)) > 24 {
			marker = line
			break
		}
	}
	if marker == "" {
		t.Fatal("the shipped repair system prompt has no long line to scan for")
	}
	for _, source := range productionEditorInlineWriteSources(t) {
		if strings.Contains(source.body, marker) {
			t.Fatalf("%s restates the shipped repair prompt in Go; it must come from the committed asset", source.name)
		}
	}
	lane.assertNoDial(t)
}

// TestProductionLegacyRosterExecuteReplacesALengthStop is the second non-happy
// branch: a length stop carries the replacement prompt composed from the shipped
// asset, and the replacement's answer settles.
func TestProductionLegacyRosterExecuteReplacesALengthStop(t *testing.T) {
	lane := productionLegacyRosterExecuteServeOpen(t,
		[]string{"{\"schemaVersion\":1,\"entries\":[", productionLegacyRosterExecuteRoster},
		[]string{"length", "stop"}, "legacy-roster-execute-length")
	result := lane.answer(t, lane.handle(t))
	outcome := syntheticMap(result["outcome"])
	if syntheticString(outcome["status"]) != productionLegacyRosterStatusCompleted ||
		syntheticString(outcome["content"]) != productionLegacyRosterExecuteRoster {
		t.Fatalf("the replacement did not settle the complete answer: %v", outcome)
	}
	if answered := lane.seam.answered(); answered != 2 {
		t.Fatalf("the loop dispatched %d times, want the truncated and the replacement", answered)
	}
	attempts := lane.attempts(t)
	if len(attempts) != 2 {
		t.Fatalf("the run stored %d attempts, want two", len(attempts))
	}
	if attempts[1].Nonce != "legacy:initial:1" {
		t.Fatalf("the replacement nonce is %s, want legacy:initial:1", attempts[1].Nonce)
	}
	loop, err := prompttemplate.LegacyRosterLoopTemplate()
	if err != nil {
		t.Fatalf("%v", err)
	}
	replacement, err := loop.Replacement(productionLegacyRosterLanguageZhCN)
	if err != nil {
		t.Fatalf("%v", err)
	}
	frozen := lane.queryRun(t)
	want := frozen.Task
	want.Messages = append([]productionLegacyRosterExecuteMessage(nil), frozen.Task.Messages...)
	if len(want.Messages) != 2 {
		t.Fatalf("the frozen task carries %d messages", len(want.Messages))
	}
	want.Messages[1].Content = replacement.Render(frozen.Task.Messages[1].Content, "{\"schemaVersion\":1,\"entries\":[")
	productionLegacyRosterExecuteAssertTask(t, "replacement", attempts[1].Task, want)
	lane.assertNoDial(t)
}

// TestProductionLegacyRosterExecuteReplayAnswersTheSettledAttempt re-posts the
// same handle: the ledger answers the attempt it already settled, and the seam is
// not dialed a second time.
func TestProductionLegacyRosterExecuteReplayAnswersTheSettledAttempt(t *testing.T) {
	lane := productionLegacyRosterExecuteServeOpen(t,
		[]string{productionLegacyRosterExecuteRoster}, nil, "legacy-roster-execute-replay")
	handle := lane.handle(t)
	first := lane.answer(t, handle)
	firstOutcome := syntheticMap(first["outcome"])
	replayed := lane.answer(t, handle)
	replayedOutcome := syntheticMap(replayed["outcome"])
	if syntheticString(firstOutcome["content"]) != syntheticString(replayedOutcome["content"]) ||
		syntheticString(firstOutcome["status"]) != syntheticString(replayedOutcome["status"]) ||
		syntheticString(firstOutcome["finishReason"]) != syntheticString(replayedOutcome["finishReason"]) {
		t.Fatalf("the replay answered a different outcome:\nfirst: %v\nreplay: %v", firstOutcome, replayedOutcome)
	}
	if view := syntheticMap(replayed["run"]); view == nil || syntheticString(syntheticMap(view["handle"])["runId"]) != lane.runID {
		t.Fatalf("the replay answered another run: %v", replayed["run"])
	}
	if answered := lane.seam.answered(); answered != 1 {
		t.Fatalf("the replay dispatched %d times, want the one the first call settled", answered)
	}
	if attempts := lane.attempts(t); len(attempts) != 1 {
		t.Fatalf("the replay stored %d attempts, want one", len(attempts))
	}
}

// TestProductionLegacyRosterExecuteRefusesStaleLeaseAndForeignProject pins the
// two context refusals: the same request bytes under a lease this session never
// held, and a handle that names another project, answer exactly what the
// family's served read answers for them - and neither dials the model.
func TestProductionLegacyRosterExecuteRefusesStaleLeaseAndForeignProject(t *testing.T) {
	lane := productionLegacyRosterExecuteServeOpen(t, nil, nil, "legacy-roster-execute-refusals")
	handle := lane.handle(t)
	stale := map[string]any{"projectId": syntheticString(lane.frozen["projectId"]),
		"projectPath": lane.projectPath, "leaseId": productionLegacyRosterExecuteStaleLease}
	foreignHandle := map[string]any{
		"projectId":    productionLegacyRosterExecuteForeignProject,
		"epoch":        syntheticString(handle["epoch"]),
		"rootActionId": syntheticString(handle["rootActionId"]),
		"runId":        syntheticString(handle["runId"]),
	}
	for _, refusal := range []struct {
		label string
		args  []any
		code  string
	}{
		{"stale lease", []any{map[string]any{"handle": handle}, stale}, ""},
		{"foreign project", []any{map[string]any{"handle": foreignHandle}, lane.frozen}, productionLegacyRosterIdentityMismatch},
	} {
		refusal := refusal
		t.Run(refusal.label, func(t *testing.T) {
			execute := lane.author.call(t, productionLegacyRosterExecuteName, refusal.args)
			read := lane.author.call(t, productionLegacyRosterReadName, refusal.args)
			if execute.Code == http.StatusOK {
				t.Fatalf("%s was served for a %s: %s", productionLegacyRosterExecuteName, refusal.label, execute.Body.String())
			}
			if execute.Code != read.Code || !bytes.Equal(execute.Body.Bytes(), read.Body.Bytes()) {
				t.Fatalf("a %s answered differently per name: execute %d %s, read %d %s",
					refusal.label, execute.Code, execute.Body.String(), read.Code, read.Body.String())
			}
			if refusal.code != "" && !strings.Contains(execute.Body.String(), refusal.code) {
				t.Fatalf("a %s answered %s, want %s", refusal.label, execute.Body.String(), refusal.code)
			}
		})
	}
	if answered := lane.seam.answered(); answered != 0 {
		t.Fatalf("a refused execute dispatched %d times", answered)
	}
	if attempts := lane.attempts(t); len(attempts) != 0 {
		t.Fatalf("a refused execute stored %d attempts", len(attempts))
	}
}

// TestProductionLegacyRosterExecuteBinder pins the fail-closed shape: no owner,
// no name, and a nil server is refused.
func TestProductionLegacyRosterExecuteBinder(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	if err := newProductionLegacyRosterExecuteServe(nil).Bind(server); err != nil {
		t.Fatalf("the binder failed with a nil owner: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the binder installed %v with a nil owner, want nothing", names)
	}
	if err := newProductionLegacyRosterExecuteServe(nil).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
}

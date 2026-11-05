//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
)

// errProductionExecuteDispatchedTwice fails a lane whose one admitted run reached
// the injectable dispatch seam more than once. The seam counts its own calls, so a
// lane that silently retried a dispatch cannot pass as one attempt.
var errProductionExecuteDispatchedTwice = errors.New("PRODUCTION_EXECUTE_DISPATCHED_TWICE")

// productionGenerationSeam is the executor's injectable dispatch seam
// (generationowner.Executor.DispatchFunc) for this lane's tests. It replaces the
// whole native dispatch - authority checks, credential resolution and the egress
// HTTP call - so the ledger can be asserted without a network call, and it counts
// its calls so "recorded before dispatch" is provable from the other side too.
type productionGenerationSeam struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (s *productionGenerationSeam) dispatch(_ context.Context, _ any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	s.calls++
	calls := s.calls
	s.mu.Unlock()
	if calls > 1 {
		return generation.DispatchResult{}, errProductionExecuteDispatchedTwice
	}
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

func (s *productionGenerationSeam) dispatched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// productionExecuteSession opens the seeded project for one tab and returns the
// frozen three-key context the generation fence requires.
func productionExecuteSession(t *testing.T, handler http.Handler, projectPath, nonce string) (*productionDurableClient, map[string]any) {
	t.Helper()
	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, nonce, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == nil || frozen["leaseId"] == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return author, frozen
}

// productionExecuteBegin opens one admitted run and returns its browser handle.
// generation:begin is the same reduced admission the lane already proves; this
// helper only spares the dispatch tests the repeated wire code.
func productionExecuteBegin(t *testing.T, author *productionDurableClient, frozen map[string]any, nonce string) map[string]any {
	t.Helper()
	var begin map[string]any
	productionEditorDecode(t, author, "generation:begin", []any{productionEditorBeginRequest(nonce), frozen}, &begin)
	handle, ok := begin["handle"].(map[string]any)
	if !ok || handle["runId"] == nil || handle["runId"] == "" ||
		handle["rootActionId"] == nil || handle["rootActionId"] == "" || handle["epoch"] == "" {
		t.Fatalf("generation:begin returned no run handle: %v", begin)
	}
	return handle
}

// productionExecuteRequest is the reduced GenerationExecuteRequest: the admitted
// run handle, a fresh invocation nonce and the visible-text task the executor
// plans for. It carries no model, budget, provider or credential authority - all
// of that is frozen at begin time - so the browser cannot widen the dispatch.
func productionExecuteRequest(handle map[string]any, nonce string) map[string]any {
	return map[string]any{
		"handle":          handle,
		"invocationNonce": nonce,
		"task": map[string]any{
			"purpose": "chapter-draft",
			"output":  "visible-text",
			"messages": []any{
				map[string]any{"role": "user", "content": "续写第一章"},
			},
		},
	}
}

// productionExecuteAnswer posts one generation:execute request and returns its
// decoded business result, failing when the name still answers the lane's
// fail-closed 501 or leaves the family's 200 envelope.
func productionExecuteAnswer(t *testing.T, author *productionDurableClient, request map[string]any, frozen map[string]any) map[string]any {
	t.Helper()
	response := author.call(t, "generation:execute", []any{request, frozen})
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:execute is still fail-closed in the production lane: %d %s", response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("generation:execute: HTTP %d %s", response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("generation:execute envelope: %v %s", err, response.Body.String())
	}
	return wire.Result
}

// TestProductionLaneExecutesAdmittedRunThroughTheCommittedAllowlist is the
// dispatch half of this lane. generation:execute must answer 200 (not the
// fail-closed 501) over a real admitted run, hand the prepared plan to the one
// executor through its injectable seam instead of the network, and leave the
// ledger saying exactly what happened: one attempt row that was reserved and
// marked dispatched before the dispatch and settled after it, with non-empty
// budget diagnostics, and a completed outcome carrying the dispatched text.
func TestProductionLaneExecutesAdmittedRunThroughTheCommittedAllowlist(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	productionCommitGenerationPurpose(t, root, productionTestModelID)
	const generated = "生产执行：门后有人。"
	seam := &productionGenerationSeam{text: generated}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes, Dispatch: seam.dispatch})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author, frozen := productionExecuteSession(t, handler, projectPath, "generation-execute-open-1")
	handle := productionExecuteBegin(t, author, frozen, "production-execute-begin-1")

	result := productionExecuteAnswer(t, author, productionExecuteRequest(handle, "production-execute-invoke-1"), frozen)

	outcome, ok := result["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned no outcome: %v", result)
	}
	if outcome["status"] != "completed" || outcome["finishReason"] != "stop" || outcome["content"] != generated {
		t.Fatalf("generation:execute outcome = %v", outcome)
	}
	if seam.dispatched() != 1 {
		t.Fatalf("the admitted run dispatched %d times through the seam; want exactly 1", seam.dispatched())
	}
	// The run row and the attempt row the reservation wrote, projected from the
	// author's own ledger: one attempt, settled, with its budget diagnostics.
	run, ok := result["run"].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned no run: %v", result)
	}
	if run["status"] != "running" || run["operation"] != productionEditorGenerationOperation {
		t.Fatalf("generation:execute returned the wrong run: %v", run)
	}
	diagnostics, ok := run["budgetDiagnostics"].([]any)
	if !ok || len(diagnostics) != 1 {
		t.Fatalf("generation:execute returned %d budget diagnostics; want the one dispatched attempt: %v", len(diagnostics), run["budgetDiagnostics"])
	}
	attempt, ok := diagnostics[0].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned a malformed attempt diagnostic: %v", diagnostics[0])
	}
	if attempt["attemptId"] == nil || attempt["attemptId"] == "" {
		t.Fatalf("the dispatched attempt has no attemptId: %v", attempt)
	}
	if attempt["actualState"] != "settled" || attempt["failureCode"] != nil {
		t.Fatalf("the dispatched attempt was not settled successfully: %v", attempt)
	}
	if attempt["finishReason"] != "stop" {
		t.Fatalf("the dispatched attempt lost its finish reason: %v", attempt)
	}
	if attempt["requestedOutputTokens"] == float64(0) || attempt["reservedTokens"] == float64(0) {
		t.Fatalf("the dispatched attempt reserved nothing: %v", attempt)
	}
	actual, ok := attempt["actual"].(map[string]any)
	if !ok || len(actual) == 0 {
		t.Fatalf("the dispatched attempt carries no actual usage: %v", attempt["actual"])
	}
	if actual["total"] != float64(20) {
		t.Fatalf("the dispatched attempt settled the wrong usage: %v", actual)
	}
	ledger, ok := run["ledger"].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned no ledger accounting: %v", run)
	}
	if ledger["physicalRequests"] != float64(1) {
		t.Fatalf("the ledger did not record exactly one physical request: %v", ledger)
	}
	if liability, ok := ledger["tokenLiability"].(float64); !ok || liability <= 0 {
		t.Fatalf("the ledger recorded no token liability for the dispatched attempt: %v", ledger)
	}

	// generation:read replays the same rows from the ledger rather than from the
	// receipt the execute call happened to hold.
	var view map[string]any
	productionEditorDecode(t, author, "generation:read", []any{handle, frozen}, &view)
	if view["status"] != "running" || view["operation"] != productionEditorGenerationOperation {
		t.Fatalf("generation:read returned the wrong run after execute: %v", view)
	}
	readDiagnostics, ok := view["budgetDiagnostics"].([]any)
	if !ok || len(readDiagnostics) != 1 {
		t.Fatalf("generation:read lost the attempt row: %v", view["budgetDiagnostics"])
	}
	if replayed, ok := readDiagnostics[0].(map[string]any); !ok || replayed["attemptId"] != attempt["attemptId"] {
		t.Fatalf("generation:read returned another attempt: %v", readDiagnostics[0])
	}

	// A stale lease is refused by the same fence the reads use, before the ledger
	// is touched: the run keeps exactly one attempt and nothing was dispatched.
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}
	staleResponse := author.call(t, "generation:execute", []any{productionExecuteRequest(handle, "production-execute-invoke-stale"), stale})
	if staleResponse.Code == http.StatusNotImplemented || strings.Contains(staleResponse.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:execute is unbound again: %d %s", staleResponse.Code, staleResponse.Body.String())
	}
	if staleResponse.Code != http.StatusOK || !strings.Contains(staleResponse.Body.String(), `"success":false`) {
		t.Fatalf("a stale-lease generation:execute was not refused through the business envelope: %d %s", staleResponse.Code, staleResponse.Body.String())
	}
	if seam.dispatched() != 1 {
		t.Fatalf("a stale-lease generation:execute dispatched %d times; want the one admitted dispatch", seam.dispatched())
	}
	var afterRefusal map[string]any
	productionEditorDecode(t, author, "generation:read", []any{handle, frozen}, &afterRefusal)
	if diagnostics, ok := afterRefusal["budgetDiagnostics"].([]any); !ok || len(diagnostics) != 1 {
		t.Fatalf("a refused generation:execute wrote an attempt row: %v", afterRefusal["budgetDiagnostics"])
	}
}

// TestProductionLaneRecordsAnOriginOutsideTheAllowlistAsAFailedAttempt proves the
// committed allowlist really is the dispatch boundary: the lane's own policy admits
// one origin, the committed model endpoint is another, and generation:execute is
// answered by the egress client before any socket is dialed. The failure is a
// recorded failed attempt, never a completed outcome - a wrong success here would
// spend the author's money and hand back text no provider produced.
func TestProductionLaneRecordsAnOriginOutsideTheAllowlistAsAFailedAttempt(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	productionCommitGenerationPurpose(t, root, productionTestModelID)
	// The declared allowlist is authoritative, so the committed model endpoint
	// (https://synthetic.invalid/v1) stays outside it. No dispatch seam is
	// installed: the refusal asserted below is the one a real deployment produces.
	productionCommitModelOrigins(t, root, []string{"https://allowed.example.invalid"})
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author, frozen := productionExecuteSession(t, handler, projectPath, "generation-refused-open-1")
	handle := productionExecuteBegin(t, author, frozen, "production-refused-begin-1")

	result := productionExecuteAnswer(t, author, productionExecuteRequest(handle, "production-refused-invoke-1"), frozen)
	outcome, ok := result["outcome"].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned no outcome: %v", result)
	}
	if outcome["status"] == "completed" || outcome["content"] != "" {
		t.Fatalf("a refused origin produced a completed outcome: %v", outcome)
	}
	run, ok := result["run"].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned no run: %v", result)
	}
	diagnostics, ok := run["budgetDiagnostics"].([]any)
	if !ok || len(diagnostics) != 1 {
		t.Fatalf("a refused dispatch wrote %d attempt diagnostics; want one failed attempt: %v", len(diagnostics), run["budgetDiagnostics"])
	}
	attempt, ok := diagnostics[0].(map[string]any)
	if !ok {
		t.Fatalf("the refused attempt is malformed: %v", diagnostics[0])
	}
	if attempt["attemptId"] == nil || attempt["attemptId"] == "" {
		t.Fatalf("the refused attempt has no attemptId: %v", attempt)
	}
	// The attempt is terminal but never claims trustworthy usage: the ledger marks
	// a failed dispatch "unknown" (no provider usage it can vouch for) with the
	// failure code beside it, instead of a settled attempt or a completed outcome.
	if attempt["actualState"] != "unknown" {
		t.Fatalf("the refused attempt was not settled terminally: %v", attempt)
	}
	// The egress refusal is mapped by the native adapter, so the ledger records a
	// failure code and no trusted usage instead of a fabricated success.
	if attempt["failureCode"] != "NETWORK_ERROR" {
		t.Fatalf("the refused attempt recorded %v instead of the egress failure", attempt["failureCode"])
	}
	if attempt["actual"] != nil {
		t.Fatalf("a refused dispatch settled provider usage: %v", attempt["actual"])
	}
	ledger, ok := run["ledger"].(map[string]any)
	if !ok {
		t.Fatalf("generation:execute returned no ledger accounting: %v", run)
	}
	if ledger["physicalRequests"] != float64(1) {
		t.Fatalf("the refusal was not recorded as one physical request: %v", ledger)
	}
}

// TestProductionLaneRefusesExecuteWithoutAnEgressPolicy proves the second gate:
// a committed config that names no usable origin leaves the lane with no executor,
// and generation:execute refuses with the policy's own symbolic code instead of
// answering 501 or dispatching without a boundary.
func TestProductionLaneRefusesExecuteWithoutAnEgressPolicy(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	// The same two documents the lane's own egress refusal test commits: no
	// allowlist and no committed model endpoint, so no policy exists at all.
	if err := os.WriteFile(productionCommittedConfigPath(root),
		[]byte(`{"theme":"dark","defaultModelId":null}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(productionCommittedModelsPath(root), []byte(`[]`), 0600); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatalf("a lane without an allowlist failed startup: %v", err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionEditorTab)
	// The gate runs before the run is even looked up, so a fabricated frozen
	// context is enough to prove the refusal code: no project has to be opened and
	// no run has to exist for the lane to refuse dispatch without a boundary.
	frozen := map[string]any{
		"projectId":   productionEditorProjectID,
		"projectPath": projectPath,
		"leaseId":     "lease-never-issued",
	}
	handle := map[string]any{
		"projectId": productionEditorProjectID, "epoch": "lease-never-issued",
		"rootActionId": "root-never-issued", "runId": "run-never-issued",
	}
	response := author.call(t, "generation:execute", []any{productionExecuteRequest(handle, "production-no-egress-invoke-1"), frozen})
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:execute is unbound without an egress policy: %d %s", response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK ||
		!strings.Contains(response.Body.String(), "PRODUCTION_MODEL_EGRESS_UNCONFIGURED") ||
		!strings.Contains(response.Body.String(), `"success":false`) {
		t.Fatalf("generation:execute without an egress policy answered %d: %s", response.Code, response.Body.String())
	}
}

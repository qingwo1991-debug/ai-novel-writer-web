package generation

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// Durability tests use a private byte-copy of the actual centrally migrated
// TypeScript baseline-v7. No synthetic schema or fake repository substitutes.
func serviceFixture(t *testing.T) (*sql.DB, *store.GenerationLedger, string) {
	t.Helper()
	contract := os.Getenv("GO_SQLITE_CONTRACT")
	if contract == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("GO_SQLITE_CONTRACT is mandatory for native generation service durability")
		}
		t.Skip("GitHub-only actual TypeScript baseline fixture")
	}
	data, err := os.ReadFile(contract)
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Fixtures []struct {
			Name string `json:"name"`
			File string `json:"file"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	var source string
	for _, fixture := range manifest.Fixtures {
		if fixture.Name == "baseline-v7" {
			source = filepath.Join(filepath.Dir(contract), fixture.File)
			break
		}
	}
	if source == "" {
		t.Fatal("actual baseline-v7 missing")
	}
	original, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "generation #? 正文.db")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		after, err := os.ReadFile(source)
		if err != nil || !bytes.Equal(original, after) {
			t.Errorf("original baseline mutated: %v", err)
		}
	})
	dsn, err := store.ExistingDSN(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`DELETE FROM generation_artifacts; DELETE FROM generation_attempts; DELETE FROM generation_runs; DELETE FROM generation_roots`); err != nil {
		t.Fatal(err)
	}
	var sequence int64
	ledger := store.NewGenerationLedger(db, func() int64 { return 1000 }, func() string { return fmt.Sprintf("service-id-%d", atomic.AddInt64(&sequence, 1)) })
	hash := strings.Repeat("a", 64)
	open := `[{"projectId":"project","epoch":"epoch","operation":"chapter-draft","uiActionNonce":"service","frozenInputHash":"` + hash + `","fingerprint":{"chapterBriefHash":"` + hash + `","authorGuidanceHash":"` + hash + `","dependencyHash":"` + hash + `","contextSnapshotHash":"` + hash + `","templateHash":"` + hash + `","skillSnapshotHash":"` + hash + `","modelLeaseRevision":"` + hash + `","policyHash":"` + hash + `","outputContractHash":"` + hash + `"},"contextSnapshotId":"context","sourceManifest":{"operation":"chapter-draft","opaque":{"z":1,"a":2}},"sourceRefs":[],"budget":{"maxPhysicalRequests":8,"maxTokenLiability":1,"maxOutputPerRequest":100,"maxActiveElapsedMs":60000}}]`
	value, err := ledger.CallJSON(context.Background(), "open", json.RawMessage(open))
	if err != nil {
		t.Fatal(err)
	}
	return db, ledger, stext(smap(value)["runId"])
}
func serviceRequest(runID string) Request {
	return Request{RunID: runID, InvocationNonce: "invoke", RequestHash: strings.Repeat("b", 64), ReservedTokens: 200, RequestedOutputTokens: 100, InputUpperBoundTokens: 90, ReasoningUpperBoundTokens: 0, UsagePolicy: map[string]any{"estimatorVersion": "test-v1", "safetyMarginTokens": int64(10), "reasoning": "included-in-completion", "canBoundTotalLiability": true}}
}
func serviceUsage() map[string]any {
	return map[string]any{"trusted": true, "promptTokens": float64(90), "completionTokens": float64(300), "totalTokens": float64(390), "reasoningTokens": float64(10), "accounting": "included-in-completion", "totalIncludesReasoning": true}
}
func mustServiceExecute(t *testing.T, s *Service, request Request) map[string]any {
	t.Helper()
	result, err := s.Execute(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
func wantServiceCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || err.Error() != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}
func TestServiceValidationAgentProtocolBeforeAnyLedgerEffect(t *testing.T) {
	request := serviceRequest("missing")
	s := New(Dependencies{})
	request.AgentToolNames = []string{}
	_, err := s.Execute(context.Background(), request)
	wantServiceCode(t, err, "GENERATION_OPERATION_UNSUPPORTED")
	request.AgentToolNames = nil
	request.RequestHash = "private-provider-error"
	_, err = s.Execute(context.Background(), request)
	wantServiceCode(t, err, "GENERATION_REQUEST_HASH_INVALID")
	request = serviceRequest("missing")
	request.InputUpperBoundTokens = 201
	err = validateRequest(request)
	wantServiceCode(t, err, "GENERATION_RESERVATION_INVALID")
	request = serviceRequest("missing")
	request.UsagePolicy["safetyMarginTokens"] = math.NaN()
	err = validateRequest(request)
	wantServiceCode(t, err, "GENERATION_RESERVATION_INVALID")
}
func TestServiceOrdinaryDurableFullBodyReplayAndLateFence(t *testing.T) {
	db, ledger, runID := serviceFixture(t)
	var dispatches int
	var late Callbacks
	var snapshots []Snapshot
	s := New(Dependencies{Ledger: ledger, OnSnapshot: func(snapshot Snapshot) { snapshots = append(snapshots, snapshot) }, Dispatch: func(ctx context.Context, request any, callbacks Callbacks) (DispatchResult, error) {
		dispatches++
		late = callbacks
		var status string
		if err := db.QueryRow(`SELECT json_extract(attempt_json,'$.status') FROM generation_attempts`).Scan(&status); err != nil || status != "dispatch-marked" {
			return DispatchResult{}, errors.New("reservation not durable before dispatch")
		}
		prefix := strings.Repeat("正", 1366) // 4098 UTF-8 bytes; only 1366 runes.
		callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: prefix, EventID: "one"})
		callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: prefix, EventID: "one"})
		callbacks.OnReasoning("private reasoning")
		callbacks.OnVisible(VisibleEvent{Kind: "cumulative", Text: prefix + "😀\r\n<&>", EventID: "two"})
		return DispatchResult{Usage: serviceUsage(), FinishReason: "stop"}, nil
	}})
	request := serviceRequest(runID)
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Execute(cancelledCtx, request)
	wantServiceCode(t, err, "GENERATION_CANCELLED")
	result := mustServiceExecute(t, s, request)
	artifact := smap(result["artifact"])
	expected := strings.Repeat("正", 1366) + "😀\r\n<&>"
	if artifact["text"] != expected {
		t.Fatal("full-body artifact changed", artifact)
	}
	if n, _ := sint(artifact["revision"]); n != 2 {
		t.Fatal("byte threshold/final flush revision", artifact)
	}
	if n, _ := sint(smap(result["attempt"])["actualTokens"]); n != 390 {
		t.Fatal("usage clipped to estimate or root liability became false cap", result)
	}
	if smap(result["attempt"])["status"] != "settled" {
		t.Fatal(result)
	}
	late.OnVisible(VisibleEvent{Kind: "delta", Text: "late"})
	late.OnReasoning("late thought")
	replay := mustServiceExecute(t, s, request)
	if smap(replay["artifact"])["text"] != expected || dispatches != 1 {
		t.Fatal("late callback/replay dispatched again", replay, dispatches)
	}
	if len(snapshots) < 2 || snapshots[0].DurableText != "" || snapshots[1].DurableText != strings.Repeat("正", 1366) {
		t.Fatal("4096 UTF-8 byte flush missing")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private reasoning") {
		t.Fatal("reasoning persisted")
	}
}
func TestServiceChangedEventCumulativeAndSplitThinkFailClosed(t *testing.T) {
	for _, mode := range []string{"changed-event", "non-prefix", "split-think"} {
		t.Run(mode, func(t *testing.T) {
			_, ledger, runID := serviceFixture(t)
			s := New(Dependencies{Ledger: ledger, Dispatch: func(ctx context.Context, request any, callbacks Callbacks) (DispatchResult, error) {
				switch mode {
				case "changed-event":
					callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "visible", EventID: "id"})
					callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "changed", EventID: "id"})
				case "non-prefix":
					callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "visible"})
					callbacks.OnVisible(VisibleEvent{Kind: "cumulative", Text: "replacement"})
				case "split-think":
					callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "<thi"})
					callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "nk>private"})
				}
				callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "ignored"})
				return DispatchResult{Usage: serviceUsage(), FinishReason: "stop"}, nil
			}})
			result := mustServiceExecute(t, s, serviceRequest(runID))
			if result["failureCode"] != "GENERATION_STREAM_INVALID" || smap(result["attempt"])["status"] != "unknown" {
				t.Fatal(result)
			}
			if strings.Contains(stext(smap(result["artifact"])["text"]), "private") {
				t.Fatal("think leaked")
			}
		})
	}
}
func TestServiceConcurrentNonceJoinConflictAndReentrantPause(t *testing.T) {
	_, ledger, runID := serviceFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var count atomic.Int64
	var s *Service
	s = New(Dependencies{Ledger: ledger, Dispatch: func(ctx context.Context, request any, callbacks Callbacks) (DispatchResult, error) {
		count.Add(1)
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return DispatchResult{Usage: serviceUsage(), FinishReason: "stop"}, nil
	}})
	request := serviceRequest(runID)
	results := make(chan map[string]any, 2)
	errs := make(chan error, 2)
	run := func() { value, err := s.Execute(context.Background(), request); results <- value; errs <- err }
	go run()
	<-started
	conflict := request
	conflict.RequestHash = strings.Repeat("c", 64)
	_, err := s.Execute(context.Background(), conflict)
	wantServiceCode(t, err, "GENERATION_INVOCATION_CONFLICT")
	go run()
	close(release)
	first, second := <-results, <-results
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	if smap(first["attempt"])["attemptId"] != smap(second["attempt"])["attemptId"] || count.Load() != 1 {
		t.Fatal("nonce did not join")
	}
	// A renderer notification may synchronously issue root pause without deadlocking.
	request.InvocationNonce = "reentrant"
	var once sync.Once
	s.deps.OnSnapshot = func(snapshot Snapshot) {
		once.Do(func() {
			_, err := s.Pause(context.Background(), stext(smap(first["run"])["rootActionId"]))
			if err != nil {
				t.Error(err)
			}
		})
	}
	s.deps.Dispatch = func(ctx context.Context, request any, callbacks Callbacks) (DispatchResult, error) {
		callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "retained"})
		<-ctx.Done()
		return DispatchResult{}, ctx.Err()
	}
	result := mustServiceExecute(t, s, request)
	if result["failureCode"] != "GENERATION_CANCELLED" {
		t.Fatal(result)
	}
}
func TestServiceSnapshotAndBlockFailureRetainsEntireTailAndFencesRoot(t *testing.T) {
	db, ledger, runID := serviceFixture(t)
	for _, stmt := range []string{
		`CREATE TRIGGER service_snapshot_failure BEFORE UPDATE ON generation_artifacts BEGIN SELECT RAISE(ABORT,'secret provider URL credential'); END`,
		`CREATE TRIGGER service_block_failure BEFORE UPDATE ON generation_roots WHEN NEW.blocked_code IS NOT NULL BEGIN SELECT RAISE(ABORT,'secret block failure'); END`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	var count int
	s := New(Dependencies{Ledger: ledger, Dispatch: func(ctx context.Context, request any, callbacks Callbacks) (DispatchResult, error) {
		count++
		callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: strings.Repeat("中", 1400) + "tail😀"})
		callbacks.OnVisible(VisibleEvent{Kind: "delta", Text: "late ignored"})
		return DispatchResult{Usage: serviceUsage(), FinishReason: "stop"}, nil
	}})
	request := serviceRequest(runID)
	result := mustServiceExecute(t, s, request)
	if result["failureCode"] != "GENERATION_STORAGE_FAILED" {
		t.Fatal(result)
	}
	tails := s.ReadUnsavedTails()
	if len(tails) != 1 || tails[0].Text != strings.Repeat("中", 1400)+"tail😀" || tails[0].DurableRevision != 0 {
		t.Fatal("complete remaining tail lost", tails)
	}
	tails[0].Text = "mutated copy"
	if s.ReadUnsavedTails()[0].Text == "mutated copy" {
		t.Fatal("tail alias")
	}
	request.InvocationNonce = "blocked"
	_, err := s.Execute(context.Background(), request)
	wantServiceCode(t, err, "GENERATION_STORAGE_FAILED")
	if count != 1 {
		t.Fatal("failed root redispatched")
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("storage error leaked")
	}
	_, err = s.SuspendForProjectClose(context.Background())
	wantServiceCode(t, err, "GENERATION_UNSAVED_TAIL_PRESENT")
	_, err = s.DiscardCandidate(context.Background(), tails[0].ArtifactID)
	wantServiceCode(t, err, "GENERATION_STORAGE_FAILED")
	if len(s.ReadUnsavedTails()) != 1 {
		t.Fatal("failed discard lost tail")
	}
	if _, err := db.Exec("DROP TRIGGER service_snapshot_failure; DROP TRIGGER service_block_failure"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DiscardCandidate(context.Background(), tails[0].ArtifactID); err != nil {
		t.Fatal(err)
	}
	if len(s.ReadUnsavedTails()) != 0 {
		t.Fatal("explicit durable discard did not release tail")
	}
}
func TestServiceSuspendFlushRecoveryAndNoPostCloseLedgerAccess(t *testing.T) {
	db, ledger, runID := serviceFixture(t)
	ready := make(chan struct{})
	release := make(chan struct{})
	returned := make(chan map[string]any, 1)
	var callbacks Callbacks
	var snapshots []Snapshot
	s := New(Dependencies{Ledger: ledger, FlushInterval: time.Hour, OnSnapshot: func(snapshot Snapshot) { snapshots = append(snapshots, snapshot) }, Dispatch: func(ctx context.Context, request any, c Callbacks) (DispatchResult, error) {
		callbacks = c
		c.OnVisible(VisibleEvent{Kind: "delta", Text: "unflushed😀"})
		close(ready)
		<-release
		c.OnVisible(VisibleEvent{Kind: "delta", Text: "late"})
		return DispatchResult{Usage: serviceUsage(), FinishReason: "stop"}, nil
	}})
	go func() {
		result, err := s.Execute(context.Background(), serviceRequest(runID))
		if err != nil {
			t.Error(err)
		}
		returned <- result
	}()
	<-ready
	receipts, err := s.SuspendForProjectClose(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 || snapshots[0].DurableText != "" || snapshots[1].DurableText != "unflushed😀" {
		t.Fatal("suspend dirty-flush notifications", snapshots)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(snapshots) != 2 {
		t.Fatal("clean/terminal flush emitted redundant snapshot")
	}

	if len(receipts) != 1 || smap(receipts[0]["attempt"])["status"] != "unknown" || smap(receipts[0]["artifact"])["text"] != "unflushed😀" {
		t.Fatal(receipts)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	callbacks.OnDiagnostics(map[string]any{"phase": "stream"})
	callbacks.OnReasoning("late")
	close(release)
	result := <-returned
	if result["failureCode"] != "GENERATION_PROJECT_CLOSED" || smap(result["artifact"])["text"] != "unflushed😀" {
		t.Fatal(result)
	}
	_, err = s.Execute(context.Background(), serviceRequest(runID))
	wantServiceCode(t, err, "GENERATION_PROJECT_CLOSED")
}
func TestServicePreCancelledControlDoesNotCloseOrPoisonHealthyRoot(t *testing.T) {
	_, ledger, runID := serviceFixture(t)
	s := New(Dependencies{Ledger: ledger, Dispatch: func(ctx context.Context, request any, c Callbacks) (DispatchResult, error) {
		c.OnVisible(VisibleEvent{Kind: "delta", Text: "healthy"})
		return DispatchResult{Usage: serviceUsage(), FinishReason: "stop"}, nil
	}})
	raw, _ := json.Marshal([]any{runID})
	runValue, err := ledger.CallJSON(context.Background(), "get", raw)
	if err != nil {
		t.Fatal(err)
	}
	rootID := stext(smap(runValue)["rootActionId"])
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.Pause(ctx, rootID)
	wantServiceCode(t, err, "GENERATION_CANCELLED")
	_, err = s.Cancel(ctx, rootID)
	wantServiceCode(t, err, "GENERATION_CANCELLED")
	_, err = s.SuspendForProjectClose(ctx)
	wantServiceCode(t, err, "GENERATION_CANCELLED")
	result := mustServiceExecute(t, s, serviceRequest(runID))
	if result["failureCode"] != nil || smap(result["attempt"])["status"] != "settled" || smap(result["artifact"])["text"] != "healthy" {
		t.Fatal("cancelled UI control poisoned root", result)
	}
}
func TestServiceDiagnosticsFiniteAllowlistAndRecoveryDefaultDeny(t *testing.T) {
	_, ledger, runID := serviceFixture(t)
	s := New(Dependencies{Ledger: ledger, Dispatch: func(ctx context.Context, request any, c Callbacks) (DispatchResult, error) {
		c.OnDiagnostics(map[string]any{"startedAt": float64(1), "elapsedMs": math.Inf(1), "httpStatus": float64(502), "phase": "stream", "errorName": "private name", "errorCode": "ECONNRESET private credentials", "causeCode": "ECONNRESET", "message": "private provider body", "visibleEvents": float64(1)})
		return DispatchResult{}, errors.New("secret URL apiKey and prompt")
	}})
	result := mustServiceExecute(t, s, serviceRequest(runID))
	if result["failureCode"] != "NETWORK_ERROR" {
		t.Fatal(result)
	}
	encoded, _ := json.Marshal(result)
	if strings.Contains(string(encoded), "private") || strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "apiKey") {
		t.Fatal("untrusted diagnostics persisted", string(encoded))
	}
	runValue, err := ledger.CallJSON(context.Background(), "get", json.RawMessage(`["`+runID+`"]`))
	if err != nil {
		t.Fatal(err)
	}
	next, _ := json.Marshal(smap(runValue)["binding"])
	_, err = s.Resume(context.Background(), runID, next)
	wantServiceCode(t, err, "GENERATION_RECOVERY_UNAUTHORIZED")
}

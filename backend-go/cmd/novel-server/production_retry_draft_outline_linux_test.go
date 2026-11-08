//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
)

// This file proves the retry name production_retry_draft_outline_linux.go serves:
// the frozen task is recorded on the failed attempt at dispatch, the retry
// re-dispatches that exact task rather than a new one, a run with no failed
// attempt is refused by name, and a tampered replayTask or request hash is refused
// by the proof. Every test runs over the real HTTP handler with the model stubbed
// at the lane's dispatch seam (productionGenerationSeam is replaced by a seam that
// answers a length stop first and a completed stop after), and every assertion on
// persisted state reads through a connection of the test's own.

const (
	productionRetryBeginNonce    = "generation-retry-begin"
	productionRetryInvokeNonce   = "generation-retry-invoke"
	productionRetryOutlinePrompt = "generate the short outline"
	productionRetryGenerated     = "重试生成：短纲定稿。"
)

// productionRetrySeam is the executor's dispatch seam for the retry tests: the
// first call is the failed short-outline attempt (a length stop with no visible
// text), every later call streams one cumulative chunk and stops. It counts its
// calls so a refused retry can be proved to have dispatched nothing.
type productionRetrySeam struct {
	mu    sync.Mutex
	calls int
	text  string
}

func (s *productionRetrySeam) dispatch(_ context.Context, _ any, callbacks generation.Callbacks) (generation.DispatchResult, error) {
	s.mu.Lock()
	index := s.calls
	s.calls++
	s.mu.Unlock()
	usage := map[string]any{"trusted": true, "promptTokens": 4, "completionTokens": 16, "reasoningTokens": 0, "totalTokens": 20}
	if index == 0 {
		return generation.DispatchResult{Usage: usage, FinishReason: "length"}, nil
	}
	if callbacks.OnVisible != nil {
		callbacks.OnVisible(generation.VisibleEvent{Kind: "cumulative", Text: s.text})
	}
	return generation.DispatchResult{Usage: usage, FinishReason: "stop"}, nil
}

func (s *productionRetrySeam) dispatched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// productionRetryBeginRequest is the reduced chapter-draft begin request with the
// renderer's material decision carrying the short-outline prompt hash, frozen at
// begin time exactly the way the renderer does.
func productionRetryBeginRequest(nonce, shortOutlinePromptHash string) map[string]any {
	request := productionEditorBeginRequest(nonce)
	request["materialDecision"] = map[string]any{
		"version":               float64(1),
		"verdict":               "admitted",
		"promptHash":            strings.Repeat("a1", 32),
		"shortOutlinePromptHash": shortOutlinePromptHash,
		"capacity":              map[string]any{"maxInputUnits": float64(4096), "methodVersion": "ci-material-1"},
		"coverage":              map[string]any{"complete": true},
		"included": []any{map[string]any{
			"sourceId": "draft:1", "category": "draft", "contentHash": strings.Repeat("b2", 32),
		}},
		"omitted": []any{},
	}
	return request
}

// productionRetryExecuteRequest is the short-outline dispatch request: one user
// prompt whose hash must equal the material decision's short-outline hash.
func productionRetryExecuteRequest(handle map[string]any, nonce, prompt string) map[string]any {
	return map[string]any{
		"handle":          handle,
		"invocationNonce": nonce,
		"task": map[string]any{
			"purpose":  "draft-short-outline",
			"output":   "visible-text",
			"messages": []any{map[string]any{"role": "user", "content": prompt}},
		},
	}
}

// productionRetryFailedAttempt opens one lane, dispatches the short-outline task
// to a length stop and returns the failed attempt's id plus its stored usage
// receipt. It is the shared fixture for the record, re-dispatch and tamper tests.
func productionRetryFailedAttempt(t *testing.T, seam *productionRetrySeam) (*productionGenerationWriteLane, map[string]any, string, map[string]any) {
	t.Helper()
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	handle := lane.begin(t, productionRetryBeginRequest(productionRetryBeginNonce, syntheticHash(productionRetryOutlinePrompt)))
	runID := syntheticString(handle["runId"])
	executed := productionExecuteAnswer(t, lane.author, productionRetryExecuteRequest(handle, productionRetryInvokeNonce, productionRetryOutlinePrompt), lane.frozen)
	if outcome := syntheticMap(executed["outcome"]); syntheticString(outcome["status"]) != "incomplete" || syntheticString(outcome["finishReason"]) != "length" {
		t.Fatalf("the first dispatch did not settle as a length stop: %v", executed)
	}
	if seam.dispatched() != 1 {
		t.Fatalf("the failed attempt dispatched %d times; want 1", seam.dispatched())
	}
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var failedAttemptID, usageJSON string
	productionStoryStateWriteScan(t, readback, "SELECT attempt_id, usage_receipt_json FROM generation_attempts WHERE run_id=?", []any{runID}, &failedAttemptID, &usageJSON)
	if failedAttemptID == "" {
		t.Fatal("the failed attempt has no id")
	}
	var usage map[string]any
	if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
		t.Fatal(err)
	}
	return lane, handle, failedAttemptID, usage
}

// productionRetryTamperedDB builds a minimal in-memory generation_attempts table
// holding one usage receipt, so the proof can be exercised against a stored value
// without a live lane (whose author permit refuses any external write as
// AUTHOR_WRITE_STALE before the proof would ever read it).
func productionRetryTamperedDB(t *testing.T, usageJSON string) (*sql.DB, string) {
	t.Helper()
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE generation_attempts (attempt_id TEXT PRIMARY KEY, reservation_id TEXT NOT NULL, run_id TEXT NOT NULL, root_action_id TEXT NOT NULL, attempt_json TEXT NOT NULL, usage_receipt_json TEXT, invocation_nonce TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	attemptID := "ci-tampered-attempt"
	if _, err := db.Exec("INSERT INTO generation_attempts (attempt_id, reservation_id, run_id, root_action_id, attempt_json, usage_receipt_json, invocation_nonce) VALUES (?,?,?,?,?,?,?)",
		attemptID, "ci-reservation", "ci-run", "ci-root", "{}", usageJSON, "ci-nonce"); err != nil {
		t.Fatal(err)
	}
	return db, attemptID
}

// TestProductionRetryDraftShortOutline drives the four proofs over the real HTTP
// handler with the model stubbed at the dispatch seam.
func TestProductionRetryDraftShortOutline(t *testing.T) {
	t.Run("records the frozen task at dispatch", testProductionRetryRecordsTaskAtDispatch)
	t.Run("re-dispatches the exact frozen task", testProductionRetryRedispatchesFrozenTask)
	t.Run("refuses a run with no failed attempt", testProductionRetryRefusesNoFailedAttempt)
	t.Run("refuses a tampered attempt", testProductionRetryRefusesTamperedAttempt)
}

// testProductionRetryRecordsTaskAtDispatch proves the dispatch writes the frozen
// short-outline task onto the attempt's usage receipt as replayTask, with the
// exact purpose, output and single user prompt.
func testProductionRetryRecordsTaskAtDispatch(t *testing.T) {
	seam := &productionRetrySeam{text: productionRetryGenerated}
	_, _, _, usage := productionRetryFailedAttempt(t, seam)
	replay := syntheticMap(usage["replayTask"])
	if replay == nil || syntheticString(replay["purpose"]) != "draft-short-outline" || syntheticString(replay["output"]) != "visible-text" {
		t.Fatalf("the failed attempt recorded no short-outline replayTask: %v", usage)
	}
	messages := syntheticSlice(replay["messages"])
	if len(messages) != 1 || syntheticString(syntheticMap(messages[0])["role"]) != "user" ||
		syntheticString(syntheticMap(messages[0])["content"]) != productionRetryOutlinePrompt {
		t.Fatalf("the recorded replayTask carries the wrong messages: %v", replay)
	}
}

// testProductionRetryRedispatchesFrozenTask proves the retry re-dispatches the
// exact task the failed attempt froze: the retry attempt's recorded replayTask
// equals the failed attempt's, and the seam answered a completed stop, never a
// newly authored message.
func testProductionRetryRedispatchesFrozenTask(t *testing.T) {
	seam := &productionRetrySeam{text: productionRetryGenerated}
	lane, handle, failedAttemptID, usage := productionRetryFailedAttempt(t, seam)
	runID := syntheticString(handle["runId"])

	retried := productionStoryStateWriteCall(t, lane.author, productionGenerationRetryName, []any{
		map[string]any{"handle": handle, "failedAttemptId": failedAttemptID}, lane.frozen,
	})
	if outcome := syntheticMap(retried["outcome"]); syntheticString(outcome["status"]) != "completed" ||
		syntheticString(outcome["finishReason"]) != "stop" || syntheticString(outcome["content"]) != productionRetryGenerated {
		t.Fatalf("the retry did not re-dispatch to a completed stop: %v", retried)
	}
	if seam.dispatched() != 2 {
		t.Fatalf("the retry dispatched %d total times; want the failed attempt plus one retry", seam.dispatched())
	}

	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var retryUsageJSON string
	productionStoryStateWriteScan(t, readback,
		"SELECT usage_receipt_json FROM generation_attempts WHERE run_id=? AND invocation_nonce=?",
		[]any{runID, syntheticRetryNoncePrefix + failedAttemptID}, &retryUsageJSON)
	var retryUsage map[string]any
	if err := json.Unmarshal([]byte(retryUsageJSON), &retryUsage); err != nil {
		t.Fatal(err)
	}
	before, aErr := syntheticCanonical(usage["replayTask"])
	after, bErr := syntheticCanonical(retryUsage["replayTask"])
	if aErr != nil || bErr != nil || before != after {
		t.Fatalf("the retry re-dispatched a different task: %v -> %v", usage["replayTask"], retryUsage["replayTask"])
	}
	lane.assertNoDial(t)
}

// testProductionRetryRefusesNoFailedAttempt proves a run that never dispatched a
// failed short-outline attempt is refused by name, without a provider call.
func testProductionRetryRefusesNoFailedAttempt(t *testing.T) {
	seam := &productionRetrySeam{text: productionRetryGenerated}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	handle := lane.begin(t, productionRetryBeginRequest(productionRetryBeginNonce, syntheticHash(productionRetryOutlinePrompt)))
	code := productionLifecycleRefusal(t, "retry with no failed attempt",
		lane.author.call(t, productionGenerationRetryName, []any{
			map[string]any{"handle": handle, "failedAttemptId": "ci-not-an-attempt"}, lane.frozen,
		}))
	if code != "GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE" {
		t.Fatalf("a run with no failed attempt refused with %q; want GENERATION_DRAFT_SHORT_OUTLINE_RETRY_UNAVAILABLE", code)
	}
	if seam.dispatched() != 0 {
		t.Fatalf("a refused retry dispatched the provider seam: %d calls", seam.dispatched())
	}
	lane.assertNoDial(t)
}

// testProductionRetryRefusesTamperedAttempt proves both halves of the proof: a
// tampered replayTask prompt and a tampered request hash are each read back from
// the stored usage receipt and refused with the one symbolic code.
func testProductionRetryRefusesTamperedAttempt(t *testing.T) {
	const correctPrompt = "generate the short outline"
	shortOutlinePromptHash := syntheticHash(correctPrompt)
	for _, tc := range []struct {
		name      string
		usageJSON string
		proofHash string
	}{
		{
			name: "replay task prompt",
			usageJSON: `{"requestHash":` + strconv.Quote(strings.Repeat("a", 64)) +
				`,"replayTask":{"purpose":"draft-short-outline","output":"visible-text","messages":[{"role":"user","content":"tampered prompt"}]}}`,
			proofHash: "",
		},
		{
			name: "request hash",
			usageJSON: `{"requestHash":` + strconv.Quote(strings.Repeat("0", 64)) +
				`,"replayTask":{"purpose":"draft-short-outline","output":"visible-text","messages":[{"role":"user","content":` + strconv.Quote(correctPrompt) + `}]}}`,
			proofHash: strings.Repeat("a", 64),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, attemptID := productionRetryTamperedDB(t, tc.usageJSON)
			task, _, storedHash, err := productionRetryReadFrozenTask(context.Background(), db, attemptID)
			if err != nil {
				t.Fatalf("read the tampered task: %v", err)
			}
			if err := productionRetryProveTask(task, shortOutlinePromptHash, tc.proofHash, storedHash); err == nil || err.Error() != "GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID" {
				t.Fatalf("a tampered attempt was not refused with GENERATION_DRAFT_SHORT_OUTLINE_TASK_INVALID: %v", err)
			}
		})
	}
}

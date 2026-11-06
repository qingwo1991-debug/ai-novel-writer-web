//go:build linux

package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// The two runs this file needs are the two a recovery list has to tell apart: one
// admitted and never dispatched, and one dispatched and settled.
const (
	productionRecoveryInterruptedNonce = "generation-recovery-interrupted"
	productionRecoveryCompletedNonce   = "generation-recovery-completed"
	productionRecoverySettledNonce     = "generation-recovery-settled"
)

// TestProductionLaneRefusesGenerationRecovery pins generation:list-recovery,
// generation:find-blueprint-recovery and generation:retry-draft-short-outline at the
// registry's fail-closed 501 over a session that really holds both states they would
// project: a run admitted between admission and dispatch, and a run that settled with
// a stop. The point is that the refusal is not vacuous - there is state to answer
// about, one attempt id to retry, and a live lease to read it under - and that
// reaching the three names still writes nothing, dispatches nothing and leaves the
// author database byte-identical.
func TestProductionLaneRefusesGenerationRecovery(t *testing.T) {
	seam := &productionGenerationSeam{text: "雨停之前，她把书翻到最后一页。"}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	// The interrupted run: admitted, no attempt, which is the state a crash between
	// admission and dispatch leaves behind.
	interrupted := lane.begin(t, productionEditorBeginRequest(productionRecoveryInterruptedNonce))
	// The completed run: one attempt, settled with a stop.
	completed := lane.begin(t, productionEditorBeginRequest(productionRecoveryCompletedNonce))
	receipt := productionExecuteAnswer(t, lane.author,
		productionExecuteRequest(completed, productionRecoverySettledNonce), lane.frozen)
	outcome, ok := receipt["outcome"].(map[string]any)
	if !ok || outcome["status"] != "completed" || outcome["finishReason"] != "stop" {
		t.Fatalf("the second run did not settle with a stop: %v", receipt)
	}
	if seam.dispatched() != 1 {
		t.Fatalf("admitting and executing one run dispatched %d times, want 1", seam.dispatched())
	}

	// The two states are proved through the test's own mode=ro connection, not from
	// anything the lane returned, so the fixture cannot agree with a wrong answer.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var interruptedAttempts, completedAttempts, runs, attempts int
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts WHERE run_id=?",
		[]any{syntheticString(interrupted["runId"])}, &interruptedAttempts)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts WHERE run_id=?",
		[]any{syntheticString(completed["runId"])}, &completedAttempts)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_runs", nil, &runs)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts", nil, &attempts)
	if interruptedAttempts != 0 || completedAttempts != 1 || runs != 2 || attempts != 1 {
		t.Fatalf("the fixture is not the two states under test: attempts %d/%d of %d, runs %d",
			interruptedAttempts, completedAttempts, attempts, runs)
	}
	var settledStatus string
	productionStoryStateWriteScan(t, readback,
		"SELECT json_extract(attempt_json,'$.status') FROM generation_attempts WHERE run_id=?",
		[]any{syntheticString(completed["runId"])}, &settledStatus)
	if settledStatus != "settled" {
		t.Fatalf("the completed run has no settled attempt: %v", settledStatus)
	}
	var attemptID string
	productionStoryStateWriteScan(t, readback, "SELECT attempt_id FROM generation_attempts WHERE run_id=?",
		[]any{syntheticString(completed["runId"])}, &attemptID)
	if attemptID == "" {
		t.Fatal("the settled attempt has no id to retry")
	}

	// The retry request is the browser's own shape
	// (src/shared/generation-owner-contract.ts:310): a handle and the failed attempt.
	retry := map[string]any{"handle": completed, "failedAttemptId": attemptID}
	before := productionMCPDatabaseDigest(t, lane.projectPath)
	productionRecoveryUnbound(t, lane.author, productionGenerationRecoveryList, []any{lane.frozen})
	productionRecoveryUnbound(t, lane.author, productionGenerationRecoveryFind, []any{lane.frozen})
	productionRecoveryUnbound(t, lane.author, productionGenerationRecoveryRetry, []any{retry, lane.frozen})
	after := productionMCPDatabaseDigest(t, lane.projectPath)
	if before != after {
		t.Fatal("a refused recovery or retry moved a byte of the author database")
	}
	// The three refusals ran after the whole-file digest and before it was taken
	// again, so a retry that dispatched behind the 501 would show up here even when
	// its own row never committed.
	if seam.dispatched() != 1 {
		t.Fatalf("a refused retry dispatched the provider seam: %d calls", seam.dispatched())
	}
	var runsAfter, attemptsAfter int
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_runs", nil, &runsAfter)
	productionStoryStateWriteScan(t, readback, "SELECT COUNT(*) FROM generation_attempts", nil, &attemptsAfter)
	if runsAfter != runs || attemptsAfter != attempts {
		t.Fatalf("a refused name added rows: runs %d, attempts %d", runsAfter, attemptsAfter)
	}
	lane.assertNoDial(t)
}

// productionRecoveryUnbound asserts one name still answers the registry's fail-closed
// 501, which is what an inventoried but unbound operation must do.
func productionRecoveryUnbound(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v = %d %s, want 501 GO_OPERATION_NOT_MIGRATED", operation, response.Code, response.Body.String())
	}
}

// TestProductionGenerationRecoverySeams re-reads the code every seam row points at.
// The marker is searched for across the whole file rather than only at the recorded
// line: a seam that is still documented but has been shifted by an unrelated edit is
// not a regression, while a seam that was deleted is. The recorded line is still
// reported, so a reader finds it quickly.
func TestProductionGenerationRecoverySeams(t *testing.T) {
	seen := make(map[string]bool, 3)
	for _, operation := range productionGenerationRecoveryOperations() {
		if seen[operation] {
			t.Fatalf("%v is listed twice", operation)
		}
		seen[operation] = true
		seam, found := productionGenerationRecoverySeamFor(operation)
		if !found || len(seam.Missing) == 0 {
			t.Fatalf("%v has no documented seam", operation)
		}
		for _, line := range seam.Missing {
			if line.Line < 1 || line.Marker == "" {
				t.Fatalf("%v has an incomplete seam row for %v", operation, line.Path)
			}
			raw, err := os.ReadFile(line.Path)
			if err != nil {
				t.Fatalf("%v: %v: %v", operation, line.Path, err)
			}
			if !strings.Contains(string(raw), line.Marker) {
				t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %d)",
					operation, line.Marker, line.Path, line.Line)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("the refusal list covers %d names, want 3", len(seen))
	}
	// The table is keyed, not positional: a name this file does not refuse must not
	// come back with someone else's seam.
	if _, found := productionGenerationRecoverySeamFor("generation:read"); found {
		t.Fatal("the seam table claims a name this file does not refuse")
	}
	// The binder installs nothing and cannot be wired to a nil server.
	if err := bindProductionGenerationRecovery(nil); err == nil || err.Error() != "TRANSPORT_SERVER_REQUIRED" {
		t.Fatalf("the binder accepted a nil server: %v", err)
	}
}

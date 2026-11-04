package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Uses the byte-copy of the original TypeScript schema through the existing
// GO_SQLITE_CONTRACT fixture chain, not a synthetic ledger-only replacement.
func generationLedgerFixture(t *testing.T) (*sql.DB, *GenerationLedger, *int64) {
	t.Helper()
	db, _, _ := blueprintFixture(t)
	mustExec(t, db, `DELETE FROM generation_artifacts; DELETE FROM generation_attempts; DELETE FROM generation_runs; DELETE FROM generation_roots`)
	db.SetMaxOpenConns(1)
	now := int64(1000)
	ordinal := 0
	ledger := NewGenerationLedger(db, func() int64 { return now }, func() string { ordinal++; return fmt.Sprintf("ledger-id-%d", ordinal) })
	return db, ledger, &now
}
func generationLedgerRequest(nonce string) string {
	hash := strings.Repeat("a", 64)
	return `{"projectId":"project","epoch":"epoch","operation":"chapter-draft","uiActionNonce":"` + nonce + `","frozenInputHash":"` + hash + `","fingerprint":{"chapterBriefHash":"` + hash + `","authorGuidanceHash":"` + hash + `","dependencyHash":"` + hash + `","contextSnapshotHash":"` + hash + `","templateHash":"` + hash + `","skillSnapshotHash":"` + hash + `","modelLeaseRevision":"` + hash + `","policyHash":"` + hash + `","outputContractHash":"` + hash + `"},"contextSnapshotId":"context","sourceManifest":{"operation":"chapter-draft","opaque":{"z":1,"a":2}},"sourceRefs":[],"budget":{"maxPhysicalRequests":3,"maxTokenLiability":1,"maxOutputPerRequest":100,"maxActiveElapsedMs":1000}}`
}
func generationLedgerCall(t *testing.T, g *GenerationLedger, method string, args ...any) any {
	t.Helper()
	out, err := g.Call(context.Background(), method, args)
	if err != nil {
		t.Fatalf("%s: %v", method, err)
	}
	return out
}
func generationLedgerOpen(t *testing.T, g *GenerationLedger, nonce string) map[string]any {
	t.Helper()
	out, err := g.CallJSON(context.Background(), "open", json.RawMessage("["+generationLedgerRequest(nonce)+"]"))
	if err != nil {
		t.Fatal(err)
	}
	return out.(map[string]any)
}
func generationLedgerWant(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || err.Error() != code {
		t.Fatalf("got %v want %s", err, code)
	}
}
func TestGenerationLedgerOrdinaryLifecycleClockAndFullBody(t *testing.T) {
	_, g, now := generationLedgerFixture(t)
	run := generationLedgerOpen(t, g, "lifecycle")
	replayed := generationLedgerOpen(t, g, "lifecycle")
	if replayed["runId"] != run["runId"] {
		t.Fatal("nonce made another run")
	}
	receipt := generationLedgerCall(t, g, "reserve", run["runId"], "invoke", strings.Repeat("b", 64), 200, 100).(map[string]any)
	attempt := glMap(receipt["attempt"])
	attemptID := attempt["attemptId"]
	// Token liability is bookkeeping, NOT a brake (ADR-0023).
	if generationLedgerCall(t, g, "hasInFlight", run["runId"]) != true {
		t.Fatal("reservation not live")
	}
	generationLedgerCall(t, g, "markDispatched", attemptID)
	*now = 1250
	artifact := generationLedgerCall(t, g, "snapshot", attemptID, 0, "正文😀\u2028<&>").(map[string]any)
	if artifact["textHash"] != recoveryHash("正文😀\u2028<&>") {
		t.Fatal("full body hash changed")
	}
	generationLedgerCall(t, g, "snapshot", attemptID, 0, "正文😀\u2028<&>")
	settled := generationLedgerCall(t, g, "settle", attemptID, map[string]any{"trusted": true, "actualTokens": 250}, "stop").(map[string]any)
	if glMap(settled["attempt"])["status"] != "settled" || !rrEqual(glMap(settled["budget"])["activeElapsedMs"], int64(250)) {
		t.Fatalf("settlement %v", settled)
	}
	*now = 2000
	budget := generationLedgerCall(t, g, "budget", run["rootActionId"]).(map[string]any)
	if !rrEqual(budget["activeElapsedMs"], int64(250)) {
		t.Fatal("idle clock still ticking")
	}
	// The budget must be keyed by the run's own root action id and must carry the
	// attempt rows, not an empty list: an empty attempts slice is exactly the shape
	// that produced empty budgetDiagnostics in the HTTP lane while the store was
	// already correct.
	attempts, ok := budget["attempts"].([]map[string]any)
	if !ok || len(attempts) == 0 {
		t.Fatalf("budget lost its attempt rows: %#v", budget["attempts"])
	}
	defer func() {
		for _, attempt := range attempts {
			if attempt == nil {
				t.Fatalf("budget carried a nil attempt row")
			}
		}
	}()
	// Exact snapshot replay precedes terminal rejection in the original.
	generationLedgerCall(t, g, "snapshot", attemptID, 0, "正文😀\u2028<&>")
	_, err := g.Call(context.Background(), "snapshot", []any{attemptID, "bad", "changed"})
	generationLedgerWant(t, err, "ARTIFACT_ATTEMPT_TERMINAL")
	found := generationLedgerCall(t, g, "findInvocation", run["runId"], "invoke", strings.Repeat("b", 64)).(map[string]any)
	if glMap(found["attempt"])["attemptId"] != attemptID {
		t.Fatal("invocation replay changed identity")
	}
	_, err = g.Call(context.Background(), "findInvocation", []any{run["runId"], "invoke", strings.Repeat("c", 64)})
	generationLedgerWant(t, err, "GENERATION_INVOCATION_CONFLICT")
}
func TestGenerationLedgerRollbackAndInterruptedRecovery(t *testing.T) {
	db, g, now := generationLedgerFixture(t)
	run := generationLedgerOpen(t, g, "rollback")
	mustExec(t, db, `CREATE TRIGGER ledger_artifact_failure BEFORE INSERT ON generation_artifacts BEGIN SELECT RAISE(ABORT,'ledger synthetic failure'); END`)
	_, err := g.Call(context.Background(), "reserve", []any{run["runId"], "invoke", strings.Repeat("b", 64), 200, 100})
	if err == nil {
		t.Fatal("synthetic failure ignored")
	}
	if !rrEqual(generationLedgerCall(t, g, "attemptCount", run["runId"]), int64(0)) {
		t.Fatal("reservation escaped rollback")
	}
	mustExec(t, db, "DROP TRIGGER ledger_artifact_failure")
	receipt := generationLedgerCall(t, g, "reserve", run["runId"], "invoke", strings.Repeat("b", 64), 200, 100).(map[string]any)
	attemptID := glMap(receipt["attempt"])["attemptId"]
	generationLedgerCall(t, g, "markDispatched", attemptID)
	*now = 1300
	generationLedgerCall(t, g, "recoverInterrupted")
	receipt = generationLedgerCall(t, g, "receipt", attemptID).(map[string]any)
	if glMap(receipt["attempt"])["status"] != "unknown" || glMap(receipt["run"])["status"] != "paused" {
		t.Fatalf("restart %v", receipt)
	}
	if !rrEqual(glMap(receipt["budget"])["activeElapsedMs"], int64(300)) {
		t.Fatal("restart lost active clock")
	}
}
func TestGenerationLedgerOrderingEffectsAndPortableGuard(t *testing.T) {
	db, g, _ := generationLedgerFixture(t)
	run := generationLedgerOpen(t, g, "guard")
	// Reordering budget keys is an original nonce conflict, not equivalent JSON.
	original := generationLedgerRequest("guard")
	permuted := strings.Replace(original, `"maxPhysicalRequests":3,"maxTokenLiability":1`, `"maxTokenLiability":1,"maxPhysicalRequests":3`, 1)
	_, err := g.CallJSON(context.Background(), "open", json.RawMessage("["+permuted+"]"))
	generationLedgerWant(t, err, "GENERATION_NONCE_CONFLICT")
	var input map[string]any
	if err := json.Unmarshal([]byte(original), &input); err != nil {
		t.Fatal(err)
	}
	_, err = g.Call(context.Background(), "open", []any{input})
	generationLedgerWant(t, err, "GENERATION_JSON_ORDER_REQUIRED")
	for _, table := range []string{"generation_roots", "generation_runs", "generation_attempts"} {
		// Freeze a root or run; attempt freeze exercised after creating one below.
		if table == "generation_attempts" {
			continue
		}
		id := glText(run["rootActionId"])
		if table == "generation_runs" {
			id = glText(run["runId"])
		}
		g.WithFreezeGuard(func(candidate, record string) bool { return candidate == table && record == id })
		_, err = g.Call(context.Background(), "pause", []any{run["rootActionId"]})
		generationLedgerWant(t, err, "PORTABLE_RUNTIME_FROZEN")
		generationLedgerCall(t, g, "get", run["runId"])
	}
	g.WithFreezeGuard(nil)
	receipt := generationLedgerCall(t, g, "reserve", run["runId"], "freeze", strings.Repeat("b", 64), 200, 100).(map[string]any)
	attemptID := glText(glMap(receipt["attempt"])["attemptId"])
	g.WithFreezeGuard(func(table, id string) bool { return table == "generation_attempts" && id == attemptID })
	_, err = g.Call(context.Background(), "markDispatched", []any{attemptID})
	generationLedgerWant(t, err, "PORTABLE_RUNTIME_FROZEN")
	g.WithFreezeGuard(nil)
	mustExec(t, db, `UPDATE generation_runs SET binding_json=json_set(binding_json,'$."$runEffects"',json('null')) WHERE run_id=?`, run["runId"])
	read := generationLedgerCall(t, g, "get", run["runId"]).(map[string]any)
	if _, ok := glMap(read["binding"])["$runEffects"]; ok {
		t.Fatal("effects leaked")
	}
	_, err = g.Call(context.Background(), "pause", []any{run["rootActionId"]})
	generationLedgerWant(t, err, "GENERATION_EFFECT_UNSUPPORTED")
	_, err = g.Call(context.Background(), "recordDirectoryProgress", nil)
	generationLedgerWant(t, err, "GENERATION_OPERATION_UNSUPPORTED")
}
func TestGenerationLedgerJSONGuardsAndCorruptIdentity(t *testing.T) {
	db, g, _ := generationLedgerFixture(t)
	_, err := g.CallJSON(context.Background(), "open", json.RawMessage(`["\ud800"]`))
	generationLedgerWant(t, err, "GENERATION_JSON_INVALID")
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	_, err = g.Call(context.Background(), "open", []any{cyclic})
	generationLedgerWant(t, err, "GENERATION_INPUT_INVALID")
	run := generationLedgerOpen(t, g, "corrupt")
	receipt := generationLedgerCall(t, g, "reserve", run["runId"], "corrupt", strings.Repeat("b", 64), 200, 100).(map[string]any)
	attemptID := glMap(receipt["attempt"])["attemptId"]
	mustExec(t, db, `UPDATE generation_artifacts SET artifact_json=json_set(artifact_json,'$.epoch',json('{}')) WHERE attempt_id=?`, attemptID)
	mustExec(t, db, `UPDATE generation_attempts SET usage_receipt_json=json_set(usage_receipt_json,'$.artifactIdentity.epoch',json('{}')) WHERE attempt_id=?`, attemptID)
	_, err = g.Call(context.Background(), "receipt", []any{attemptID})
	generationLedgerWant(t, err, "ARTIFACT_INTEGRITY_FAILED")
}

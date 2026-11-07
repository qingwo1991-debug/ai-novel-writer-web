//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// productionGraphPlanOutput is the model text the dispatch seam answers for a
// graph run, in the shape ParseNarrativeThreadPlanCandidates consumes
// (internal/graphgen/parse.go:104). The chapters sit inside the fixture's
// twelve-chapter blueprint so the derivation admits them.
const productionGraphPlanOutput = `{"candidates":[{"title":"门后的秘密","type":"伏笔","targetStartChapter":1,"targetEndChapter":3,"authorIntent":"为后续章节埋下伏笔"}]}`

// productionGraphExecute posts graph-generation:execute and returns its business
// result, failing when the name is unbound or refused.
func productionGraphExecute(t *testing.T, lane *productionGenerationWriteLane, handle map[string]any) map[string]any {
	t.Helper()
	return productionStoryStateWriteCall(t, lane.author, "graph-generation:execute",
		[]any{map[string]any{"handle": handle}, lane.frozen})
}

// productionGraphConfirm posts graph-generation:confirm and returns its business
// result.
func productionGraphConfirm(t *testing.T, lane *productionGenerationWriteLane, handle, artifact map[string]any, index float64) map[string]any {
	t.Helper()
	return productionStoryStateWriteCall(t, lane.author, "graph-generation:confirm",
		[]any{map[string]any{"handle": handle, "artifact": artifact, "index": index}, lane.frozen})
}

// productionGraphExecutedArtifact is the first candidate snapshot of one executed
// run: the artifact reference the confirm request carries.
func productionGraphExecutedArtifact(t *testing.T, executed map[string]any) map[string]any {
	t.Helper()
	run := productionGraphGenerationMap(t, "run", executed["run"])
	candidates, ok := run["candidates"].([]any)
	if !ok || len(candidates) != 1 {
		t.Fatalf("the executed run reports %v candidates; want the one settled artifact", run["candidates"])
	}
	candidate := productionGraphGenerationMap(t, "candidate", candidates[0])
	for _, key := range []string{"artifactId", "revision", "textHash"} {
		if candidate[key] == nil || candidate[key] == "" {
			t.Fatalf("the executed artifact carries no %s: %v", key, candidate)
		}
	}
	return map[string]any{
		"artifactId": candidate["artifactId"],
		"revision":   candidate["revision"],
		"textHash":   candidate["textHash"],
	}
}

// productionGraphEffects reads the graphEffects array one run already stamped on
// its attempt, through a separate read-only connection.
func productionGraphEffects(t *testing.T, projectPath, runID string) []any {
	t.Helper()
	db := productionStoryStateWriteReadback(t, projectPath)
	var usageJSON string
	productionStoryStateWriteScan(t, db,
		"SELECT usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", []any{runID}, &usageJSON)
	var usage map[string]any
	if err := json.Unmarshal([]byte(usageJSON), &usage); err != nil {
		t.Fatalf("usage receipt: %v", err)
	}
	effects, _ := usage["graphEffects"].([]any)
	return effects
}

// TestProductionLaneExecutesGraphRun proves graph-generation:execute dispatches the
// frozen graph task through the committed seam exactly once, persists the settled
// artifact, replays idempotently, and refuses a foreign or stale lease without
// moving a byte of the author database.
func TestProductionLaneExecutesGraphRun(t *testing.T) {
	seam := &productionGenerationSeam{text: productionGraphPlanOutput}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	handle := productionGraphGenerationBegin(t, lane)

	executed := productionGraphExecute(t, lane, handle)
	outcome := productionGraphGenerationMap(t, "outcome", executed["outcome"])
	if outcome["status"] != "completed" || outcome["finishReason"] != "stop" || outcome["content"] != productionGraphPlanOutput {
		t.Fatalf("graph-generation:execute outcome = %v", outcome)
	}
	if seam.dispatched() != 1 {
		t.Fatalf("the graph run dispatched %d times; want exactly 1", seam.dispatched())
	}
	artifact := productionGraphExecutedArtifact(t, executed)
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var artifactJSON string
	var status string
	productionStoryStateWriteScan(t, readback,
		"SELECT artifact_json,status FROM generation_artifacts WHERE artifact_id=?", []any{artifact["artifactId"]}, &artifactJSON, &status)
	if status == "discarded" {
		t.Fatalf("the settled artifact is discarded: %s", artifactJSON)
	}
	var storedArtifact map[string]any
	if err := json.Unmarshal([]byte(artifactJSON), &storedArtifact); err != nil {
		t.Fatalf("the settled artifact json: %v", err)
	}
	if text, _ := storedArtifact["text"].(string); text != productionGraphPlanOutput {
		t.Fatalf("the settled artifact does not carry the dispatched text: %v", storedArtifact)
	}

	// A replay of the same handle is idempotent, not a second dispatch: the frozen
	// task and the fixed invocation nonce make the request hash deterministic, and the
	// ledger's findInvocation answers the settled attempt
	// (electron/services/main-generation-owner.ts:1216-1217).
	replayed := productionGraphExecute(t, lane, handle)
	if seam.dispatched() != 1 {
		t.Fatalf("a replayed execute dispatched %d times; want the one settled dispatch", seam.dispatched())
	}
	if replayedArtifact := productionGraphExecutedArtifact(t, replayed); replayedArtifact["artifactId"] != artifact["artifactId"] {
		t.Fatalf("the replay answered another artifact: %v", replayedArtifact)
	}

	// A foreign-project handle is refused by the identity fence before any row moves.
	baseline := lane.bytes(t)
	foreign := map[string]any{
		"projectId":    productionGraphReadsForeignProjectID,
		"epoch":        lane.frozen["leaseId"],
		"rootActionId": handle["rootActionId"],
		"runId":        handle["runId"],
	}
	productionStoryStateWriteBusinessRefusal(t, lane.author, "foreign-project execute",
		"graph-generation:execute", []any{map[string]any{"handle": foreign}, lane.frozen}, "GENERATION_RUN_IDENTITY_MISMATCH")
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a foreign-project execute changed the committed database file")
	}

	// A stale lease is refused by the admission before the ledger is touched.
	stale := lane.stale()
	productionLifecycleRefusal(t, "stale-lease execute",
		lane.author.call(t, "graph-generation:execute", []any{map[string]any{"handle": handle}, stale}))
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a stale-lease execute changed the committed database file")
	}
	lane.assertNoDial(t)
}

// TestProductionLaneConfirmsGraphRun proves graph-generation:confirm derives the
// plan candidate from the settled artifact, writes the chosen plan and the graph
// effect in one transaction, replays the same choice idempotently, refuses a
// different choice, and rolls the plan back when the effect stamp fails.
func TestProductionLaneConfirmsGraphRun(t *testing.T) {
	seam := &productionGenerationSeam{text: productionGraphPlanOutput}
	lane := productionGenerationWriteOpen(t, seam.dispatch, nil)
	handle := productionGraphGenerationBegin(t, lane)
	executed := productionGraphExecute(t, lane, handle)
	artifact := productionGraphExecutedArtifact(t, executed)

	confirmed := productionGraphConfirm(t, lane, handle, artifact, 0)
	if confirmed["success"] != true || confirmed["kind"] != "plan" || confirmed["index"] != float64(0) {
		t.Fatalf("graph-generation:confirm answered %v", confirmed)
	}
	plan := productionGraphGenerationMap(t, "plan", confirmed["plan"])
	if plan["title"] != "门后的秘密" || plan["targetStartChapter"] != float64(1) || plan["targetEndChapter"] != float64(3) {
		t.Fatalf("the confirmed plan is not the chosen candidate: %v", plan)
	}
	if syntheticNumber(plan["id"]) < 1 {
		t.Fatalf("the confirmed plan has no row id: %v", plan)
	}

	// The plan row and the effect row are both read back through a second connection.
	readback := productionStoryStateWriteReadback(t, lane.projectPath)
	var planCount int
	productionStoryStateWriteScan(t, readback,
		"SELECT COUNT(*) FROM narrative_thread_plans WHERE title=? AND target_start_chapter=? AND target_end_chapter=?",
		[]any{"门后的秘密", float64(1), float64(3)}, &planCount)
	if planCount != 1 {
		t.Fatalf("the confirm wrote %d plan rows; want 1", planCount)
	}
	effects := productionGraphEffects(t, lane.projectPath, syntheticString(handle["runId"]))
	if len(effects) != 1 {
		t.Fatalf("the confirm stamped %d graph effects; want 1", len(effects))
	}
	effect := productionGraphGenerationMap(t, "effect", effects[0])
	if effect["version"] != float64(1) || effect["index"] != float64(0) {
		t.Fatalf("the stamped effect is malformed: %v", effect)
	}

	// Replay: the same index with the same artifact answers the saved receipt; the
	// reference line that decides idempotent-vs-conflict is
	// `if (!isDeepStrictEqual(saved.artifact, reference)) throw ...`
	// (electron/services/graph-generation.ts:106).
	replayed := productionGraphConfirm(t, lane, handle, artifact, 0)
	if syntheticString(productionGraphGenerationMap(t, "replay plan", replayed["plan"])["id"]) != syntheticString(plan["id"]) {
		t.Fatalf("the replay answered a different plan: %v", replayed)
	}
	if now := productionGraphEffects(t, lane.projectPath, syntheticString(handle["runId"])); len(now) != 1 {
		t.Fatalf("the replay wrote a second effect: %d", len(now))
	}

	// Conflict: the same index with a different artifact refuses instead of writing.
	baseline := lane.bytes(t)
	other := map[string]any{"artifactId": artifact["artifactId"], "revision": artifact["revision"], "textHash": strings.Repeat("b", 64)}
	productionStoryStateWriteBusinessRefusal(t, lane.author, "conflicting confirm",
		"graph-generation:confirm", []any{map[string]any{"handle": handle, "artifact": other, "index": float64(0)}, lane.frozen},
		"GENERATION_GRAPH_EFFECT_CONFLICT")
	if !bytes.Equal(lane.bytes(t), baseline) {
		t.Fatal("a conflicting confirm changed the committed database file")
	}

	// The transaction arm: a corrupt graphEffects value makes the stamp fail after the
	// domain write, and the plan row must roll back with it.
	productionGraphCorruptEffects(t, lane, handle)
	code := productionLifecycleRefusal(t, "corrupt-effect confirm",
		lane.author.call(t, "graph-generation:confirm",
			[]any{map[string]any{"handle": handle, "artifact": artifact, "index": float64(0)}, lane.frozen}))
	if code != "GENERATION_GRAPH_EFFECT_INVALID" {
		t.Fatalf("the corrupt-effect confirm refused with %s; want GENERATION_GRAPH_EFFECT_INVALID", code)
	}
	productionStoryStateWriteScan(t, readback,
		"SELECT COUNT(*) FROM narrative_thread_plans WHERE title=? AND target_start_chapter=? AND target_end_chapter=?",
		[]any{"门后的秘密", float64(1), float64(3)}, &planCount)
	if planCount != 1 {
		t.Fatalf("the failed transaction left %d plan rows; want the one from the success arm", planCount)
	}

	// A foreign-project confirm and a stale-lease confirm never reach the ledger.
	foreign := map[string]any{
		"projectId":    productionGraphReadsForeignProjectID,
		"epoch":        lane.frozen["leaseId"],
		"rootActionId": handle["rootActionId"],
		"runId":        handle["runId"],
	}
	productionStoryStateWriteBusinessRefusal(t, lane.author, "foreign-project confirm",
		"graph-generation:confirm", []any{map[string]any{"handle": foreign, "artifact": artifact, "index": float64(0)}, lane.frozen},
		"GENERATION_RUN_IDENTITY_MISMATCH")
	stale := lane.stale()
	productionLifecycleRefusal(t, "stale-lease confirm",
		lane.author.call(t, "graph-generation:confirm",
			[]any{map[string]any{"handle": handle, "artifact": artifact, "index": float64(0)}, stale}))
	lane.assertNoDial(t)
}

// productionGraphCorruptEffects pre-seeds the run's attempt with a graphEffects
// value that is not an array, so the confirm stamp refuses after the plan write
// and the transaction rolls back.
func productionGraphCorruptEffects(t *testing.T, lane *productionGenerationWriteLane, handle map[string]any) {
	t.Helper()
	db := productionGraphWriteOpen(t, lane.projectPath)
	ledger := store.NewGenerationLedger(db, nil, nil)
	value, err := ledger.Call(context.Background(), "get", []any{syntheticString(handle["runId"])})
	if err != nil {
		t.Fatalf("ledger get for tamper: %v", err)
	}
	run := syntheticMap(value)
	budget, err := ledger.Call(context.Background(), "budget", []any{syntheticString(run["rootActionId"])})
	if err != nil {
		t.Fatalf("ledger budget for tamper: %v", err)
	}
	attempts := syntheticSlice(syntheticMap(budget)["attempts"])
	if len(attempts) != 1 {
		t.Fatalf("the run has %d attempts to tamper; want 1", len(attempts))
	}
	attemptID := syntheticString(syntheticMap(attempts[0])["attemptId"])
	if _, err := ledger.Call(context.Background(), "recordEffect",
		[]any{syntheticString(handle["runId"]), attemptID, "graphEffects", map[string]any{"corrupt": true}}); err != nil {
		t.Fatalf("tamper recordEffect: %v", err)
	}
}

// productionGraphWriteOpen opens a writable connection to the committed author
// database for test-side tampering.
func productionGraphWriteOpen(t *testing.T, projectPath string) *sql.DB {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the two now-served recovery names over real HTTP. The first
// test seeds an editors run and a batches run so both real buckets are non-empty,
// pins the three empty buckets and partial=false, and pins the reference's
// writeState:'unavailable' answer for the retired directory run the batch seed
// writes. The second test pins the honest null answer and the byte-identical
// refusals, and the third stamps a valid planning continuation to prove the reader
// really reads the stored binding rather than returning a constant.

// productionRecoveryServe3ListView is GenerationRecoveryList
// (src/shared/generation-owner-contract.ts:254-262) as the browser reads it.
type productionRecoveryServe3ListView struct {
	Planning []map[string]any `json:"planning"`
	Editors  []map[string]any `json:"editors"`
	Reviews  []map[string]any `json:"reviews"`
	Batches  []map[string]any `json:"batches"`
	Drafts   []map[string]any `json:"drafts"`
	Partial  bool             `json:"partial"`
}

// productionRecoveryServe3SeedEditor freezes one editor-inline run through the
// shipped ledger, the way the editor-inline family freezes it, so the editors
// bucket of list-recovery has something to project.
func productionRecoveryServe3SeedEditor(t *testing.T, database string) {
	t.Helper()
	db := productionBatchDatabase(t, database)
	ordinal := 0
	ledger := store.NewGenerationLedger(db, func() int64 { return 1700000000000 }, func() string {
		ordinal++
		return fmt.Sprintf("production-recovery-editor-%02d", ordinal)
	})
	document := productionEditorInlineFixtureDocument
	selection := productionEditorInlineFixtureSelected
	contextText := productionEditorInlineContextOrdered(document, selection, 10, 11)
	inputText := productionEditorInlineInputText("refine", document, selection, 10, 11)
	productionEditorInlineSeedOne(t, db, ledger, "recovery-editor", productionEditorProjectID,
		productionEditorInlineFixtureEpoch, productionEditorInlineOperation, contextText, inputText, false)
	productionEditorCheckpoint(t, db, database)
}

// productionRecoveryServe3StampContinuation writes the frozen planner-continuation
// effect the desktop writes with recordPlanningContinuation
// (generation-run-repository.ts:226-236), pointing one directory run at another.
func productionRecoveryServe3StampContinuation(t *testing.T, database, sourceRoot, sourceRun, nextRoot, nextRun string) {
	t.Helper()
	receipt := map[string]any{
		"kind": "planning-continuation",
		"sourceHandle": map[string]any{
			"projectId":    productionEditorProjectID,
			"epoch":        productionRecoveryDirectoryEpoch,
			"rootActionId": sourceRoot,
			"runId":        sourceRun,
		},
		"nextHandle": map[string]any{
			"projectId":    productionEditorProjectID,
			"epoch":        productionRecoveryDirectoryEpoch,
			"rootActionId": nextRoot,
			"runId":        nextRun,
		},
		"requestHash": sha256Hex("planning-continuation-request"),
	}
	effect, err := json.Marshal(map[string]any{"planningContinuation": receipt})
	if err != nil {
		t.Fatal(err)
	}
	db := productionBatchDatabase(t, database)
	if _, err := db.Exec(`UPDATE generation_runs SET binding_json=json_set(binding_json,'$."$runEffects"',json(?))
 WHERE run_id=?`, string(effect), sourceRun); err != nil {
		_ = db.Close()
		t.Fatalf("stamp the planning continuation of %v: %v", sourceRun, err)
	}
	productionEditorCheckpoint(t, db, database)
}

// TestProductionGenerationRecoveryServe3ListBucketsAndRetired is the served list
// answer for a project that really holds an editors run and a batches run. The
// batch seed also writes a retired chapter-blueprint-directory run, so
// find-blueprint-recovery answers the reference's writeState:'unavailable' branch
// (main-generation-owner.ts:935-937) instead of null.
func TestProductionGenerationRecoveryServe3ListBucketsAndRetired(t *testing.T) {
	lane := productionRecoveryOpen(t, func(t *testing.T, database string) {
		seedProductionBatch(t, database)
		productionRecoveryServe3SeedEditor(t, database)
	})

	var list productionRecoveryServe3ListView
	lane.client.decode(t, productionGenerationRecoveryList, []any{lane.frozen}, &list)
	if list.Partial {
		t.Fatal("list-recovery answered partial=true; every bucket is a real read")
	}
	if list.Planning == nil || list.Reviews == nil || list.Drafts == nil {
		t.Fatal("an empty bucket answered null; the contract says []")
	}
	if len(list.Planning) != 0 || len(list.Reviews) != 0 || len(list.Drafts) != 0 {
		t.Fatalf("planning/drafts/reviews should be empty: %d/%d/%d",
			len(list.Planning), len(list.Drafts), len(list.Reviews))
	}
	if len(list.Editors) == 0 {
		t.Fatal("the editors bucket is empty although the project holds an editor-inline run")
	}
	if len(list.Batches) == 0 {
		t.Fatal("the batches bucket is empty although the project holds a batch root")
	}

	// The retired directory run the batch seed writes is answered, not skipped:
	// find returns the unavailable context the reference builds at :935-937.
	var context map[string]any
	lane.client.decode(t, productionGenerationRecoveryFind, []any{lane.frozen}, &context)
	blueprint, _ := context["blueprintRecovery"].(map[string]any)
	if blueprint == nil || blueprint["writeState"] != "unavailable" {
		t.Fatalf("find-blueprint-recovery did not answer the retired run as unavailable: %v", context)
	}
	diagnostic, _ := blueprint["diagnostic"].(map[string]any)
	if diagnostic == nil || diagnostic["kind"] != "retired" || diagnostic["code"] != "GENERATION_BLUEPRINTS_CLEARED" {
		t.Fatalf("the unavailable context carried the wrong diagnostic: %v", blueprint)
	}
}

// TestProductionGenerationRecoveryServe3FindNullAndRefusals is the honest null
// arm: a project with a readable directory run, no planning continuation and no
// retired run answers null, and both names refuse a foreign project and a stale
// lease with byte-identical bodies.
func TestProductionGenerationRecoveryServe3FindNullAndRefusals(t *testing.T) {
	lane := productionRecoveryOpen(t, func(t *testing.T, database string) {
		productionRecoverySeedDirectory(t, database, []productionRecoveryDirectorySeed{{
			RunID:       productionRecoveryResumableRun,
			RootID:      productionRecoveryResumableRoot,
			ProjectID:   productionEditorProjectID,
			OperationID: productionRecoveryResumableOperation,
			Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
			Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
		}})
	})

	// The reader really read the seeded run's stored binding and reported the
	// continuation absent, so find answers the reference's own null.
	var find any
	lane.client.decode(t, productionGenerationRecoveryFind, []any{lane.frozen}, &find)
	if find != nil {
		t.Fatalf("find-blueprint-recovery answered %v, want null", find)
	}

	shapes := map[string]map[string]any{
		"foreign": {"projectId": productionRecoveryForeignProject, "projectPath": lane.projectPath, "leaseId": lane.frozen["leaseId"]},
		"stale":   {"projectId": lane.frozen["projectId"], "projectPath": lane.projectPath, "leaseId": productionRecoveryServeStaleLease},
	}
	for _, shapeName := range []string{"foreign", "stale"} {
		listResponse := lane.client.call(t, productionGenerationRecoveryList, []any{shapes[shapeName]})
		findResponse := lane.client.call(t, productionGenerationRecoveryFind, []any{shapes[shapeName]})
		if listResponse.Code == http.StatusOK || findResponse.Code == http.StatusOK {
			t.Fatalf("a %v session was served: list %d, find %d", shapeName, listResponse.Code, findResponse.Code)
		}
		if listResponse.Body.String() != findResponse.Body.String() {
			t.Fatalf("list-recovery and find-blueprint-recovery refused the %v session with different bytes:\n%s\n%s",
				shapeName, listResponse.Body.String(), findResponse.Body.String())
		}
	}
}

// TestProductionGenerationRecoveryServe3ContinuationRead proves the reader is a
// real read: a valid planning continuation is refused as unprojectable, not
// skipped into null.
func TestProductionGenerationRecoveryServe3ContinuationRead(t *testing.T) {
	lane := productionRecoveryOpen(t, func(t *testing.T, database string) {
		productionRecoverySeedDirectory(t, database, []productionRecoveryDirectorySeed{
			{
				RunID:       productionRecoveryResumableRun,
				RootID:      productionRecoveryResumableRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoveryResumableOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 3},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
			{
				RunID:       productionRecoverySettledRun,
				RootID:      productionRecoverySettledRoot,
				ProjectID:   productionEditorProjectID,
				OperationID: productionRecoverySettledOperation,
				Requested:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
				Committed:   productionRecoveryRangeView{StartChapter: 1, EndChapter: 2},
			},
		})
		productionRecoveryServe3StampContinuation(t, database,
			productionRecoveryResumableRoot, productionRecoveryResumableRun,
			productionRecoverySettledRoot, productionRecoverySettledRun)
	})

	response := lane.client.call(t, productionGenerationRecoveryFind, []any{lane.frozen})
	if response.Code != http.StatusInternalServerError ||
		!strings.Contains(response.Body.String(), "GENERATION_PLANNING_CONTINUATION_UNPROJECTABLE") {
		t.Fatalf("find-blueprint-recovery over a valid planning continuation = %d %s; the reader did not read the stored binding",
			response.Code, response.Body.String())
	}
}

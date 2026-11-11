//go:build linux

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file proves the two now-served recovery names over real HTTP. The first
// test seeds an editors run and a batches run so both real buckets are non-empty,
// pins the planning bucket's retired context and partial=false, and pins the
// reference's writeState:'unavailable' answer for the retired directory run the
// batch seed writes. The second test pins the honest null answer and the
// byte-identical refusals, and the third stamps a proved planning continuation to
// prove the continued context, its planning bucket entry and the author-edit
// receipt are projected rather than refused.

// productionRecoveryServe3ListView is GenerationRecoveryList
// (src/shared/generation-owner-contract.ts:254-262) as the browser reads it.
type productionRecoveryServe3ListView struct {
	Planning []productionRecoveryServe3ContextView `json:"planning"`
	Editors  []map[string]any                      `json:"editors"`
	Reviews  []map[string]any                      `json:"reviews"`
	Batches  []map[string]any                      `json:"batches"`
	Drafts   []map[string]any                      `json:"drafts"`
	Partial  bool                                  `json:"partial"`
}

// productionRecoveryServe3RangeView is the {from,to} range the planning
// continuation and the blueprint author-edit receipt use
// (src/shared/plot-outline-contract.ts:39, generation-owner-contract.ts:176-184).
type productionRecoveryServe3RangeView struct {
	From int64 `json:"from"`
	To   int64 `json:"to"`
}

// productionRecoveryServe3PlanningView is PlanningContinuation
// (generation-owner-contract.ts:200-207) as the browser reads it.
type productionRecoveryServe3PlanningView struct {
	SourceHandle   productionRecoveryHandleView      `json:"sourceHandle"`
	RemainingRange *productionRecoveryServe3RangeView `json:"remainingRange"`
	TargetUnits    int64                             `json:"targetUnits"`
	State          string                            `json:"state"`
	NextHandle     *productionRecoveryHandleView     `json:"nextHandle"`
}

// productionRecoveryServe3SavedView is BlueprintAuthorEditReceipt
// (generation-owner-contract.ts:176-184) as blueprintRecovery.saved carries it.
type productionRecoveryServe3SavedView struct {
	Kind           string                            `json:"kind"`
	OperationID    string                            `json:"operationId"`
	SourceHandle   productionRecoveryHandleView      `json:"sourceHandle"`
	RequestedRange productionRecoveryServe3RangeView `json:"requestedRange"`
	CommittedRange productionRecoveryServe3RangeView `json:"committedRange"`
	RemainingRange *productionRecoveryServe3RangeView `json:"remainingRange"`
	PayloadHash    string                            `json:"payloadHash"`
}

// productionRecoveryServe3BlueprintView is the BlueprintRecovery union
// (generation-owner-contract.ts:185-199) as the browser reads it.
type productionRecoveryServe3BlueprintView struct {
	SourceHandle productionRecoveryHandleView       `json:"sourceHandle"`
	LeaseEpoch   string                             `json:"leaseEpoch"`
	Draft        string                             `json:"draft"`
	EditRange    *productionRecoveryServe3RangeView `json:"editRange"`
	Saved        *productionRecoveryServe3SavedView `json:"saved"`
	WriteState   string                             `json:"writeState"`
	Diagnostic   *productionRecoveryDiagnosticView  `json:"diagnostic"`
}

// productionRecoveryServe3ContextView is the fields of GenerationRecoveryContext
// (generation-owner-contract.ts:139-169) these tests assert.
type productionRecoveryServe3ContextView struct {
	ModelID                         string                              `json:"modelId"`
	Handle                          productionRecoveryHandleView        `json:"handle"`
	Operation                       string                              `json:"operation"`
	AuthorInputs                    []productionRecoveryInputView       `json:"authorInputs"`
	SelectedBlueprintChapterNumbers []int64                             `json:"selectedBlueprintChapterNumbers"`
	DraftSave                       map[string]any                      `json:"draftSave"`
	PlanningContinuation            *productionRecoveryServe3PlanningView `json:"planningContinuation"`
	BlueprintRecovery               *productionRecoveryServe3BlueprintView `json:"blueprintRecovery"`
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

// productionRecoveryServe3StampAuthorEdit writes the frozen blueprint
// author-edit effect recordBlueprintAuthorEdit stores
// (generation-run-repository.ts:216-236), agreeing with the run's requested
// range and the blueprint commit operation productionRecoverySeedDirectory
// already wrote. It must run after StampContinuation, which replaces the whole
// $runEffects object.
func productionRecoveryServe3StampAuthorEdit(t *testing.T, database, sourceRoot, sourceRun, operationID string, requestedTo, committedFrom, committedTo int64) {
	t.Helper()
	payloadHash := sha256Hex(operationID)
	var remaining any
	if committedTo < requestedTo {
		remaining = map[string]any{"from": committedTo + 1, "to": requestedTo}
	}
	receipt := map[string]any{
		"kind":      "blueprint-author-edit",
		"operationId": operationID,
		"sourceHandle": map[string]any{
			"projectId":    productionEditorProjectID,
			"epoch":        productionRecoveryDirectoryEpoch,
			"rootActionId": sourceRoot,
			"runId":        sourceRun,
		},
		"requestedRange": map[string]any{"from": int64(1), "to": requestedTo},
		"committedRange": map[string]any{"from": committedFrom, "to": committedTo},
		"remainingRange": remaining,
		"payloadHash":    payloadHash,
	}
	effect, err := json.Marshal(map[string]any{"blueprintAuthorEdit": receipt})
	if err != nil {
		t.Fatal(err)
	}
	db := productionBatchDatabase(t, database)
	if _, err := db.Exec(`UPDATE generation_runs SET binding_json=json_set(binding_json,'$."$runEffects".blueprintAuthorEdit',json(?))
 WHERE run_id=?`, string(effect), sourceRun); err != nil {
		_ = db.Close()
		t.Fatalf("stamp the blueprint author edit of %v: %v", sourceRun, err)
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
	if len(list.Planning) != 1 {
		t.Fatalf("planning should hold the retired directory context the batch seed writes: %v", list.Planning)
	}
	if len(list.Reviews) != 0 || len(list.Drafts) != 0 {
		t.Fatalf("drafts/reviews should be empty: %d/%d", len(list.Drafts), len(list.Reviews))
	}
	retiredPlanning := list.Planning[0]
	if retiredPlanning.BlueprintRecovery == nil || retiredPlanning.BlueprintRecovery.WriteState != "unavailable" {
		t.Fatalf("the planning bucket did not carry the retired run's unavailable context: %+v", retiredPlanning)
	}
	if diagnostic := retiredPlanning.BlueprintRecovery.Diagnostic; diagnostic == nil ||
		diagnostic.Kind != "retired" || diagnostic.Code != "GENERATION_BLUEPRINTS_CLEARED" {
		t.Fatalf("the planning bucket carried the wrong retired diagnostic: %+v", retiredPlanning.BlueprintRecovery)
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

// TestProductionGenerationRecoveryServe3ContinuationProjects proves a proved
// planning continuation is projected, not refused: the reference's continued
// short-circuit (main-generation-owner.ts:787) needs neither the nine-field
// fingerprint comparison nor the live owner. Both arms are covered: a run with
// only the continuation, and one that also carries the blueprint author-edit
// receipt the recovery reader now returns as `saved`.
func TestProductionGenerationRecoveryServe3ContinuationProjects(t *testing.T) {
	seed := func(t *testing.T, database string) {
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
	}
	wantNext := productionRecoveryHandleView{
		ProjectID:    productionEditorProjectID,
		Epoch:        productionRecoveryDirectoryEpoch,
		RootActionID: productionRecoverySettledRoot,
		RunID:        productionRecoverySettledRun,
	}

	t.Run("continued", func(t *testing.T) {
		lane := productionRecoveryOpen(t, seed)

		var context productionRecoveryServe3ContextView
		lane.client.decode(t, productionGenerationRecoveryFind, []any{lane.frozen}, &context)
		planning := context.PlanningContinuation
		if planning == nil || planning.State != "continued" {
			t.Fatalf("find-blueprint-recovery did not project the continued planning state: %+v", context)
		}
		if planning.TargetUnits != 600 {
			t.Fatalf("the continued planning context carried targetUnits %d, want the 600 default", planning.TargetUnits)
		}
		if planning.RemainingRange == nil || *planning.RemainingRange != (productionRecoveryServe3RangeView{From: 3, To: 3}) {
			t.Fatalf("the continued planning context carried remainingRange %+v, want 3..3", planning.RemainingRange)
		}
		if planning.NextHandle == nil || *planning.NextHandle != wantNext {
			t.Fatalf("the continued planning context carried nextHandle %+v, want %+v", planning.NextHandle, wantNext)
		}
		blueprint := context.BlueprintRecovery
		if blueprint == nil || blueprint.WriteState != "continued" {
			t.Fatalf("the continued context did not answer blueprintRecovery.writeState=continued: %+v", blueprint)
		}
		if blueprint.Saved != nil {
			t.Fatalf("a run without an author edit projected saved=%+v", blueprint.Saved)
		}
		if blueprint.EditRange == nil || *blueprint.EditRange != (productionRecoveryServe3RangeView{From: 3, To: 3}) {
			t.Fatalf("blueprintRecovery.editRange = %+v, want 3..3", blueprint.EditRange)
		}

		var list productionRecoveryServe3ListView
		lane.client.decode(t, productionGenerationRecoveryList, []any{lane.frozen}, &list)
		if len(list.Planning) != 1 {
			t.Fatalf("list-recovery planning bucket holds %d contexts, want the continued run only: %+v", len(list.Planning), list.Planning)
		}
		planned := list.Planning[0]
		if planned.Handle.RunID != productionRecoveryResumableRun || planned.PlanningContinuation == nil ||
			planned.PlanningContinuation.State != "continued" {
			t.Fatalf("the planning bucket did not carry the continued context: %+v", planned)
		}
	})

	t.Run("author-saved", func(t *testing.T) {
		lane := productionRecoveryOpen(t, func(t *testing.T, database string) {
			seed(t, database)
			productionRecoveryServe3StampAuthorEdit(t, database,
				productionRecoveryResumableRoot, productionRecoveryResumableRun,
				productionRecoveryResumableOperation, 3, 1, 2)
		})

		var context productionRecoveryServe3ContextView
		lane.client.decode(t, productionGenerationRecoveryFind, []any{lane.frozen}, &context)
		planning := context.PlanningContinuation
		if planning == nil || planning.State != "continued" {
			t.Fatalf("find-blueprint-recovery did not keep planningContinuation.state=continued with a saved author edit: %+v", context)
		}
		blueprint := context.BlueprintRecovery
		if blueprint == nil || blueprint.WriteState != "author-saved" {
			t.Fatalf("blueprintRecovery.writeState = %+v, want author-saved", blueprint)
		}
		if blueprint.Saved == nil {
			t.Fatal("blueprintRecovery.saved is null although the run carries an author-edit receipt")
		}
		wantSaved := productionRecoveryServe3SavedView{
			Kind:           "blueprint-author-edit",
			OperationID:    productionRecoveryResumableOperation,
			SourceHandle:   productionRecoveryHandleView{ProjectID: productionEditorProjectID, Epoch: productionRecoveryDirectoryEpoch, RootActionID: productionRecoveryResumableRoot, RunID: productionRecoveryResumableRun},
			RequestedRange: productionRecoveryServe3RangeView{From: 1, To: 3},
			CommittedRange: productionRecoveryServe3RangeView{From: 1, To: 2},
			RemainingRange: &productionRecoveryServe3RangeView{From: 3, To: 3},
			PayloadHash:    sha256Hex(productionRecoveryResumableOperation),
		}
		if *blueprint.Saved != wantSaved {
			t.Fatalf("blueprintRecovery.saved = %+v, want %+v", blueprint.Saved, wantSaved)
		}

		var list productionRecoveryServe3ListView
		lane.client.decode(t, productionGenerationRecoveryList, []any{lane.frozen}, &list)
		if len(list.Planning) != 1 {
			t.Fatalf("list-recovery planning bucket holds %d contexts, want the saved continuation only: %+v", len(list.Planning), list.Planning)
		}
		if list.Planning[0].BlueprintRecovery == nil || list.Planning[0].BlueprintRecovery.Saved == nil ||
			list.Planning[0].BlueprintRecovery.Saved.OperationID != productionRecoveryResumableOperation {
			t.Fatalf("the planning bucket did not carry the author-edit receipt: %+v", list.Planning[0])
		}
	})
}

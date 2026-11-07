//go:build linux

package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// These tests pin the serve DECISION for the import-run receipt pair and the
// inspection name, and they prove two things the seam file's own test does not:
// that the refusal bodies are byte-identical across the live, stale and foreign
// request shapes (the authority matrix the batch asks for, inverted to the
// refused answer), and that the two claims that changed this pass - ReproduceBinding
// and the store's existing generation_runs read - are real declarations, not
// prose.
//
// There is deliberately no success subtest and no replay-as-success subtest: no
// name here is bound, so no request can reach a handler. The reference's own
// replay quote that decides idempotent-versus-conflict is quoted in
// production_importrun_receipts_linux_test.go ("The replay quote"); those lines
// (electron/repositories/import-run-repository.ts:2366-2374 and :2427-2432) return
// before the generation-source assert, which is exactly why a replay reader is
// NOT a served operation and why this file still refuses.

// TestProductionImportRunReceiptServeDecisionsRefuseAll pins the decision table:
// every name still refused, no served set, a symbolic reason per refusal, and the
// two-table agreement check that the binder runs at startup.
func TestProductionImportRunReceiptServeDecisionsRefuseAll(t *testing.T) {
	decisions := productionImportRunReceiptServeDecisions()
	seams := productionImportRunReceiptSeams()
	if len(decisions) != len(seams) {
		t.Fatalf("decision table holds %v rows, seam table %v", len(decisions), len(seams))
	}
	if served := productionImportRunReceiptServedOperations(); len(served) != 0 {
		t.Fatalf("served set is %v, want empty", served)
	}
	seen := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if seen[decision.Operation] {
			t.Fatalf("%v is decided twice", decision.Operation)
		}
		seen[decision.Operation] = true
		if decision.Served {
			t.Fatalf("%v is marked served; it must stay refused", decision.Operation)
		}
		if !productionImportRunReceiptSeamSymbolic(decision.Reason) {
			t.Fatalf("%v records %v, which is not a symbolic reason", decision.Operation, decision.Reason)
		}
	}
	if err := productionImportRunReceiptServeAgrees(decisions, seams); err != nil {
		t.Fatalf("the real decision and seam tables disagree: %v", err)
	}
	// A served name that is still listed in the seam is a stale note; a refused
	// name with no seam is a refusal nobody documented. Both must be caught.
	servedDecision := append([]productionImportRunReceiptServeDecision(nil), decisions...)
	servedDecision[0].Served = true
	if err := productionImportRunReceiptServeAgrees(servedDecision, seams); err == nil {
		t.Fatal("a served name still present in the seam table was accepted")
	}
	unseamed := decisions[1:]
	if err := productionImportRunReceiptServeAgrees(unseamed, seams); err == nil {
		t.Fatal("a seam row with no decision was accepted")
	}
	undecided := seams[1:]
	if err := productionImportRunReceiptServeAgrees(decisions, undecided); err == nil {
		t.Fatal("a refused name with no seam row was accepted")
	}
}

// TestProductionImportRunReceiptRefusalsAreByteIdentical drives the three names
// through the real frozen-session lane with a live lease, a stale lease and a
// foreign project path, and asserts every body is the exact same 501 bytes. The
// dispatch that answers GO_OPERATION_NOT_MIGRATED runs before any handler reads
// the path or the lease (internal/transport/server.go:111-116), so the refusal
// cannot depend on them; proving the bytes are equal is proving that independence,
// and proving the database image is unchanged is proving the refusals wrote
// nothing even though the seeded receipts are committed.
func TestProductionImportRunReceiptRefusalsAreByteIdentical(t *testing.T) {
	f := productionImportRunReceiptSeed(t)
	settled := productionImportRunResidualImage(t, f.database)
	execution := productionImportRunResidualLeaseWire(productionImportRunResidualStyleEpoch)
	shapes := []struct {
		label     string
		path      string
		execution map[string]any
	}{
		{label: "live lease", path: f.projectPath, execution: execution},
		{label: "stale lease", path: f.projectPath, execution: productionImportRunResidualLeaseWire(99)},
		{label: "foreign project", path: f.projectPath + "-foreign", execution: execution},
	}
	for _, seam := range productionImportRunReceiptSeams() {
		seam := seam
		t.Run(seam.Operation, func(t *testing.T) {
			var reference string
			for index, shape := range shapes {
				response := f.author.call(t, seam.Operation,
					productionImportRunReceiptWire(seam, shape.path, f.frozen, shape.execution))
				if response.Code != http.StatusNotImplemented {
					t.Fatalf("%v under %v answered %v, want 501: %v",
						seam.Operation, shape.label, response.Code, response.Body.String())
				}
				body := response.Body.String()
				if !strings.Contains(body, "GO_OPERATION_NOT_MIGRATED") {
					t.Fatalf("%v under %v answered 501 without the code: %v",
						seam.Operation, shape.label, body)
				}
				if index == 0 {
					reference = body
					continue
				}
				if body != reference {
					t.Fatalf("%v under %v differs from the live-lease body:\n live:  %v\n other: %v",
						seam.Operation, shape.label, reference, body)
				}
			}
		})
	}
	productionImportRunResidualAssertUnchanged(t, f.database, settled,
		"the three byte-identical refusals")
}

// TestProductionImportRunReceiptServeProofMarkers re-reads the declarations this
// pass's correction rests on. It does not duplicate the seam table's own marker
// walk; it pins only the new evidence: ReproduceBinding exists but still resolves
// a registry lease record, the reference's requireRun has no live-epoch check for
// this assert, the reference derives its model receipt from a read-only evidence
// profile rather than a lease, and the served picker still drops the preview CAS.
func TestProductionImportRunReceiptServeProofMarkers(t *testing.T) {
	markers := []struct {
		path   string
		marker string
	}{
		{"../../internal/generationowner/binding.go", "func (r *Registry) ReproduceBinding("},
		{"../../internal/generationowner/lease.go", "// resolveReproduce is resolve minus the live-session currency check."},
		{"../../internal/generationowner/lease.go", "MODEL_EXECUTION_LEASE_INVALID"},
		{"../../internal/store/character_proposal_pending.go", "FROM generation_runs WHERE run_id=?"},
		{"../../internal/store/character_proposal_pending.go", "ErrCharacterProposalPendingProofUnavailable"},
		{"../../internal/store/character_proposal_pending.go", "which is a deadlock rather than a proof"},
		{"../../internal/store/importrun.go", "func (r *ImportRuns) importRunImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error)"},
		{"../../internal/store/importrun_receipts.go", "return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})"},
		{"../../internal/store/generation_ledger.go", `"importSlot"`},
		{"production_open_linux.go", "newProductionImportRunResiduals(open.durable).Bind(server)"},
		{"production_generation_begin_linux.go", "records, err := config.ListPrivateModels()"},
		{"../../../electron/services/main-generation-owner.ts", "if (execution && run.binding.epoch !== deps.epoch)"},
		{"../../../electron/services/main-generation-owner.ts", "function currentModelReceipt(binding: RunBinding): SafeGenerationModelReceipt"},
		{"../../../electron/services/main-generation-owner.ts", "createModelExecutionLeaseReceipt(model, { leaseId: 'read-only-evidence'"},
		{"../../../electron/services/import-generation-source.ts", "captureImportGenerationContext"},
	}
	for _, entry := range markers {
		raw, err := os.ReadFile(entry.path)
		if err != nil {
			t.Fatalf("%v: %v", entry.path, err)
		}
		if !productionSeamContains(string(raw), entry.marker) {
			t.Fatalf("%v no longer contains %v", entry.path, entry.marker)
		}
	}

	// The served picker still answers an inspection id but no preview CAS, while
	// the store still computes one and still demands it at consume time. That is
	// the whole prepare-inspection refusal, pinned in both directions.
	selection, err := os.ReadFile("../../internal/transport/import_selection.go")
	if err != nil {
		t.Fatal(err)
	}
	if !productionSeamContains(string(selection), `"inspectionId": summary.InspectionID`) {
		t.Fatal("the picker no longer answers inspectionId")
	}
	if strings.Contains(string(selection), "previewCAS") {
		t.Fatal("dialog:select-novel-files now puts the preview CAS on the wire; prepare-inspection has to be re-argued")
	}
	inspection, err := os.ReadFile("../../internal/importsource/inspection.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{`PreviewCAS string ` + "`json:\"previewCAS\"`", "expectedPreviewCAS", "IMPORT_INSPECTION_PREVIEW_CHANGED"} {
		if !productionSeamContains(string(inspection), marker) {
			t.Fatalf("internal/importsource/inspection.go no longer carries %v", marker)
		}
	}
}

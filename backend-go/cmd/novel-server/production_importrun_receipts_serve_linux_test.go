//go:build linux

package main

import (
	"net/http"
	"os"
	"strings"
	"testing"
)

// These tests pin the serve DECISION for the import-run receipt family and prove
// the parts the served-name tests do not: that the decision and seam tables agree in
// both directions (with fabricated tables, so a broken guard is caught without
// editing a constant), that the one name still refused answers the identical 501
// bytes for every request shape, and that the declarations the decision rests on are
// real.
//
// The two receipt names are served by production_importrun_receipts_serve2_linux.go
// and driven end to end in production_importrun_receipts_serve2_linux_test.go. They
// are absent from the seam table on purpose: a seam is a documented refusal, and a
// served name that kept a row would make the table a stale note - which is exactly
// what productionImportRunReceiptServeAgrees refuses at startup.

// TestProductionImportRunReceiptServeDecisionAgreesWithTheSeam pins the two tables:
// one served row per receipt name, one refused row for the inspection name, a
// symbolic reason for that refusal, and the agreement check in both directions.
func TestProductionImportRunReceiptServeDecisionAgreesWithTheSeam(t *testing.T) {
	decisions := productionImportRunReceiptServeDecisions()
	seams := productionImportRunReceiptSeams()
	if len(decisions) != 3 {
		t.Fatalf("decision table holds %v rows, want three", len(decisions))
	}
	served := productionImportRunReceiptServedOperations()
	if len(served) != 2 {
		t.Fatalf("served set is %v, want the two receipt names", served)
	}
	seen := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if seen[decision.Operation] {
			t.Fatalf("%v is decided twice", decision.Operation)
		}
		seen[decision.Operation] = true
		if decision.Served {
			if decision.Reason != "" {
				t.Fatalf("%v is served and records %v as a reason", decision.Operation, decision.Reason)
			}
			continue
		}
		if !productionImportRunReceiptSeamSymbolic(decision.Reason) {
			t.Fatalf("%v records %v, which is not a symbolic reason", decision.Operation, decision.Reason)
		}
	}
	if err := productionImportRunReceiptServeAgrees(decisions, seams); err != nil {
		t.Fatalf("the real decision and seam tables disagree: %v", err)
	}
	// A served name that kept a seam row is a stale note; a refused name with no seam
	// is a refusal nobody documented. Both must be caught, and a served row carrying a
	// reason must be caught too.
	stillSeamed := append([]productionImportRunReceiptSeam(nil), seams...)
	stillSeamed = append(stillSeamed, productionImportRunReceiptSeam{
		Operation: productionImportRunReceiptsPrepareOperation, Arity: 4, PathIndex: 2,
		Gate: "GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED", Port: "a stale note",
		Missing: []productionImportRunReceiptLine{
			productionImportRunReceiptLineOf("production_open_linux.go", 0, "package main"),
		},
	})
	if err := productionImportRunReceiptServeAgrees(decisions, stillSeamed); err == nil {
		t.Fatal("a served name that still has a seam row was accepted")
	}
	if err := productionImportRunReceiptServeAgrees(decisions, seams[1:]); err == nil {
		t.Fatal("a refused name with no seam row was accepted")
	}
	withReason := append([]productionImportRunReceiptServeDecision(nil), decisions...)
	for index := range withReason {
		if withReason[index].Served {
			withReason[index].Reason = productionImportRunReceiptInspectionUnreachable
			break
		}
	}
	if err := productionImportRunReceiptServeAgrees(withReason, seams); err == nil {
		t.Fatal("a served name carrying a reason was accepted")
	}
}

// TestProductionImportRunReceiptInspectionRefusalsAreByteIdentical drives the one
// still-refused name through the real frozen-session lane with a live lease, a stale
// lease and a foreign project path, and asserts every body is the exact same 501
// bytes. The dispatch that answers GO_OPERATION_NOT_MIGRATED runs before any handler
// reads the path or the lease (internal/transport/server.go:111-116), so the refusal
// cannot depend on them; proving the bytes are equal is proving that independence,
// and proving the database image is unchanged is proving the refusals wrote nothing.
func TestProductionImportRunReceiptInspectionRefusalsAreByteIdentical(t *testing.T) {
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
	seams := productionImportRunReceiptSeams()
	if len(seams) != 1 || seams[0].Operation != productionImportRunReceiptsInspectionOperation {
		t.Fatalf("the seam table holds %v rows, want the inspection name only", len(seams))
	}
	var reference string
	for index, shape := range shapes {
		response := f.author.call(t, seams[0].Operation,
			productionImportRunReceiptWire(seams[0], shape.path, f.frozen, shape.execution))
		if response.Code != http.StatusNotImplemented {
			t.Fatalf("%v under %v answered %v, want 501: %v",
				seams[0].Operation, shape.label, response.Code, response.Body.String())
		}
		body := response.Body.String()
		if !strings.Contains(body, "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%v under %v answered 501 without the code: %v",
				seams[0].Operation, shape.label, body)
		}
		if index == 0 {
			reference = body
			continue
		}
		if body != reference {
			t.Fatalf("%v under %v differs from the live-lease body:\n live:  %v\n other: %v",
				seams[0].Operation, shape.label, reference, body)
		}
	}
	productionImportRunResidualAssertUnchanged(t, f.database, settled,
		"the three byte-identical refusals")
}

// TestProductionImportRunReceiptServeProofMarkers re-reads the declarations the
// decision quotes. It pins that the store hands the run to the assert as data, that
// the read happens on the receipt's own connection, that the registry lease record
// ReproduceBinding resolves is still real, and that the served picker still drops the
// preview CAS - which is the whole reason the inspection name stays refused.
func TestProductionImportRunReceiptServeProofMarkers(t *testing.T) {
	markers := []struct {
		path   string
		marker string
	}{
		{"../../internal/generationowner/binding.go", "func (r *Registry) ReproduceBinding("},
		{"../../internal/generationowner/lease.go", "// resolveReproduce is resolve minus the live-session currency check."},
		{"../../internal/generationowner/lease.go", "MODEL_EXECUTION_LEASE_INVALID"},
		{"../../internal/store/importrun_receipts.go", "type ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot, run ImportRunGenerationEvidence) error"},
		{"../../internal/store/importrun_receipts.go", "return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID}, run)"},
		{"../../internal/store/importrun_receipts_generation.go", "func importRunGenerationEvidenceRead(ctx context.Context, q importRunQueryer, runID string) (ImportRunGenerationEvidence, error)"},
		{"../../internal/store/importrun_receipts_generation.go", "SELECT run_id, root_action_id, binding_json FROM generation_runs WHERE run_id = ?"},
		{"../../internal/store/importrun_receipts_generation.go", "type ImportRunGenerationEvidence struct {"},
		{"../../internal/store/importrun.go", "func (r *ImportRuns) importRunImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error)"},
		{"../../internal/store/character_proposal_pending.go", "FROM generation_runs WHERE run_id=?"},
		{"../../internal/store/character_proposal_pending.go", "which is a deadlock rather than a proof"},
		{"../../internal/store/generation_ledger.go", `"importSlot"`},
		{"production_open_linux.go", "newProductionImportRunResiduals(open.durable).Bind(server)"},
		{"production_open_linux.go", "newProductionImportGenerationServe(owner).Bind(server)"},
		{"production_generation_begin_linux.go", "records, err := config.ListPrivateModels()"},
		{"../../../electron/services/main-generation-owner.ts", "if (execution && run.binding.epoch !== deps.epoch)"},
		{"../../../electron/services/main-generation-owner.ts", "function currentModelReceipt(binding: RunBinding): SafeGenerationModelReceipt"},
		{"../../../electron/services/main-generation-owner.ts", "createModelExecutionLeaseReceipt(model, { leaseId: 'read-only-evidence'"},
		{"../../../electron/services/import-generation-source.ts", "captureImportGenerationContext"},
		{"production_importrun_receipts_serve2_linux.go", "productionImportRunReceiptsServe2Sources()"},
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

	// The served picker still answers an inspection id but no preview CAS, while the
	// store still computes one and still demands it at consume time. That is the whole
	// prepare-inspection refusal, pinned in both directions.
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

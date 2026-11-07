//go:build linux

package main

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests pin the import-run family's last three names where this batch
// leaves them: refused by name, with the missing proof and, for the two receipt
// channels, the exact execution lease that would have made a served handler
// succeed.
//
// # What is deliberately absent
//
// There is no success subtest, no replay-as-success subtest and no
// foreign-project-vs-stale-lease authority matrix, because no name in this file
// is bound. The 501 GO_OPERATION_NOT_MIGRATED is decided before the handler reads
// the request envelope, the path or the lease; only the JSON body and its
// argument shape are parsed first (internal/transport/server.go:111-116). A
// "success" assertion here would have to be written against a handler that does
// not exist. The calls below therefore use the reference's real wire shape and a
// REAL live execution lease, and prove that even the branch that needs no
// generation proof - the replay of an already committed receipt - is not served.
// The success-path requirements in the batch description apply to names that are
// served; none of these three is.
//
// # Why the requests are still fully formed
//
// A refusal that only holds for a malformed request is not a refusal. Every
// argument list below is built from the seam row's own arity and path index, the
// frozen three-key session is its own last argument, and the receipt request
// carries a four-key generationRunHandle in the payload - the shape the reference
// really sends (electron/services/__tests__/import-generation.test.ts:197). The
// seeded runs are running with a live wall-clock lease, so the 501 cannot be a
// disguised IMPORT_RUN_EXECUTION_STALE or IMPORT_RUN_NOT_FOUND.
//
// # The replay quote
//
// The reference decides idempotent-versus-conflict in two places:
//
//	electron/repositories/import-run-repository.ts:2366
//	  if (existing) {
//	:2372
//	  ) throw new Error('IMPORT_RUN_RECEIPT_CONFLICT')
//	:2374
//	  return rowToEffectReceipt(existing, run)
//	:2427
//	  if (row.state === 'committed') {
//	:2429-2432
//	  return { receipt: validatedReceipt, run: this.get(runId)!, cancelApplied: run.cancel_requested === 1 }
//
// Both return before the generation-source assert. That is why "serve only the
// replay" was considered and refused: it would answer 200 for a receipt the
// desktop wrote while answering 501 for the prepare that produced it, and it
// would publish a success the reference's own prepare would have refused with
// GENERATION_IMPORT_EFFECT_HANDLE_REQUIRED. The replay subtest below drives that
// exact shape and asserts the lane still answers 501 and leaves the committed
// row untouched.
//
// # The byte image
//
// Every refused call is compared against the admitted database file, its -wal and
// its -journal (productionImportRunResidualImage). The -shm file is excluded for
// the reason the residual test records: it is transient lock state, not admitted
// data. A handler that opened a transaction and rolled it back would still change
// the image, so this is stronger than a row readback.

const (
	// productionImportRunReceiptsSecondRunID is the second style-stage run. Two
	// seeded receipts make a WHERE mistake visible: the commit request names the
	// first run, and both rows are read back afterwards.
	productionImportRunReceiptsSecondRunID = "import-receipt-style-second"
	productionImportRunReceiptsSecondEpoch = int64(3)
	// productionImportRunReceiptsInspectionID is the opaque inspection handle the
	// reference's request carries. The name is unbound, so it is never looked up.
	productionImportRunReceiptsInspectionID = "import-receipt-inspection-0"
)

// productionImportRunReceiptWire builds the exact argument list the renderer
// sends for one seam. The arity, the path index and the trailing frozen context
// come from the seam row; the payload is the reference's own shape, including the
// generationRunHandle whose proof this lane does not have.
func productionImportRunReceiptWire(seam productionImportRunReceiptSeam, projectPath string,
	expected map[string]any, execution map[string]any) []any {
	args := make([]any, seam.Arity)
	args[seam.PathIndex] = projectPath
	args[seam.Arity-1] = expected
	switch seam.Operation {
	case productionImportRunReceiptsPrepareOperation:
		args[0] = map[string]any{
			"runId":    productionImportRunResidualStyleRunID,
			"stage":    "style",
			"batchId":  "done",
			"effectKey": "writing-style",
			"kind":     "project-writing-style",
			"payload": map[string]any{
				"writingStyle": "克制而具体",
				"generationRunHandle": map[string]any{
					"projectId":    productionImportRunResidualProjectID,
					"epoch":        "receipt-epoch",
					"rootActionId": "receipt-root",
					"runId":        "receipt-generation-run",
				},
			},
		}
		args[1] = execution
	case productionImportRunReceiptsCommitOperation:
		args[0] = productionImportRunResidualStyleRunID
		args[1] = "style"
		args[2] = "done"
		args[3] = execution
	case productionImportRunReceiptsInspectionOperation:
		args[0] = map[string]any{
			"inspectionId": productionImportRunReceiptsInspectionID,
			"runId":        productionImportRunResidualParseRunID,
			"purpose":      "reference",
			"locale":       "zh-CN",
		}
	}
	return args
}

// productionImportRunReceiptRefused asserts the one answer this lane gives: 501
// with the migration code, never a 4xx that could pass for a malformed request
// and never a 200 from a handler that was installed accidentally. The failure
// message formats the body with the unquoted verb, because the CI log truncates
// at the first quote.
func productionImportRunReceiptRefused(t *testing.T, client *productionDurableClient,
	operation string, args []any, why string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%v answered %v under %v; want 501 GO_OPERATION_NOT_MIGRATED: %v",
			operation, response.Code, why, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%v answered 501 without the migration code under %v: %v",
			operation, why, response.Body.String())
	}
}

// productionImportRunReceiptSeed is the three-row world the refusals are tested
// against: two running style runs with live leases and a parsing run for the
// inspection shape.
func productionImportRunReceiptSeed(t *testing.T) *productionImportRunResidualFixture {
	t.Helper()
	first := productionImportRunResidualSeed{
		ID: productionImportRunResidualStyleRunID, Purpose: "reference", Stage: "style",
		Status: "running", Resumable: 1, ExecutionOwner: productionImportRunResidualOwner,
		ExecutionEpoch: productionImportRunResidualStyleEpoch,
		LeaseExpiresAt: productionImportRunResidualLiveLease(),
	}
	second := productionImportRunResidualSeed{
		ID: productionImportRunReceiptsSecondRunID, Purpose: "reference", Stage: "style",
		Status: "running", Resumable: 1, ExecutionOwner: productionImportRunResidualOwner,
		ExecutionEpoch: productionImportRunReceiptsSecondEpoch,
		LeaseExpiresAt: productionImportRunResidualLiveLease(),
	}
	parsing := productionImportRunResidualSeed{
		ID: productionImportRunResidualParseRunID, Purpose: "reference", Stage: "parsing",
		Status: "ready", Resumable: 1,
	}
	projectPath := productionImportRunResidualSeedProject(t, first, second, parsing)
	// The receipts are written by the store, exactly as the desktop leaves them,
	// BEFORE the lane admits the file. The handler therefore never sees a file
	// whose receipt rows the test wrote after admission.
	productionImportRunResidualSeedReceipt(t, projectPath, productionImportRunResidualStyleRunID,
		productionImportRunResidualStyleEpoch)
	productionImportRunResidualSeedReceipt(t, projectPath, productionImportRunReceiptsSecondRunID,
		productionImportRunReceiptsSecondEpoch)
	return openProductionImportRunResidualFixture(t, projectPath, "import-run-receipt-open-1")
}

// TestProductionImportRunReceiptSeamsStayRefused is the per-name pin. One subtest
// per name keeps a failure naming the operation; inside it, the live, stale and
// foreign shapes prove the 501 does not depend on the lease, the path or the run.
func TestProductionImportRunReceiptSeamsStayRefused(t *testing.T) {
	f := productionImportRunReceiptSeed(t)
	settled := productionImportRunResidualImage(t, f.database)
	execution := productionImportRunResidualLeaseWire(productionImportRunResidualStyleEpoch)
	for _, seam := range productionImportRunReceiptSeams() {
		seam := seam
		t.Run(seam.Operation, func(t *testing.T) {
			for _, shape := range []struct {
				label     string
				path      string
				execution map[string]any
			}{
				{label: "live lease", path: f.projectPath, execution: execution},
				{label: "stale lease", path: f.projectPath, execution: productionImportRunResidualLeaseWire(99)},
				{label: "foreign project", path: f.projectPath + "-foreign", execution: execution},
			} {
				before := productionImportRunResidualImage(t, f.database)
				productionImportRunReceiptRefused(t, f.author, seam.Operation,
					productionImportRunReceiptWire(seam, shape.path, f.frozen, shape.execution), shape.label)
				productionImportRunResidualAssertUnchanged(t, f.database, before,
					"the refused "+shape.label+" "+seam.Operation)
			}
		})
	}
	productionImportRunResidualAssertUnchanged(t, f.database, settled, "the three refused names")
}

// TestProductionImportRunReceiptReplayIsNotASubstitute drives the one branch that
// does not need the generation-source proof: both runs already have a committed
// project-writing-style receipt, so a prepare replay and a commit replay are the
// exact shapes the reference would answer 200 for. The lane still answers 501,
// and the two committed rows are read back over a separate mode=ro connection to
// prove the refusal did not replay, re-commit or re-apply either effect.
func TestProductionImportRunReceiptReplayIsNotASubstitute(t *testing.T) {
	f := productionImportRunReceiptSeed(t)
	rows := productionImportRunWriteQuery(t, f.database, `SELECT run_id, state, effect_receipt_json
 FROM import_run_receipts WHERE stage = 'style' AND batch_id = 'done' ORDER BY run_id`)
	if len(rows) != 2 {
		t.Fatalf("the fixture holds %v committed style receipts, want 2", len(rows))
	}
	before := make(map[string]string, len(rows))
	for _, row := range rows {
		runID := productionImportRunWriteText(row, "run_id")
		if productionImportRunWriteText(row, "state") != "committed" {
			t.Fatalf("the seeded receipt for %v is %v, want committed",
				runID, productionImportRunWriteText(row, "state"))
		}
		body := productionImportRunWriteText(row, "effect_receipt_json")
		if !strings.Contains(body, productionImportRunResidualStyle) {
			t.Fatalf("the seeded receipt for %v carries %v, want the writing style",
				runID, body)
		}
		before[runID] = body
	}
	execution := productionImportRunResidualLeaseWire(productionImportRunResidualStyleEpoch)
	seams := []productionImportRunReceiptSeam{
		{Operation: productionImportRunReceiptsPrepareOperation, Arity: 4, PathIndex: 2},
		{Operation: productionImportRunReceiptsCommitOperation, Arity: 6, PathIndex: 4},
	}
	settled := productionImportRunResidualImage(t, f.database)
	// The seeded receipt was prepared with exactly this payload, so the prepare
	// call is the reference's idempotent branch (import-run-repository.ts:2366-2374),
	// not its CONFLICT branch. The handle-carrying reference shape is exercised in
	// TestProductionImportRunReceiptSeamsStayRefused; here the point is that even
	// an exact replay is not served by this lane.
	prepareArgs := productionImportRunReceiptWire(seams[0], f.projectPath, f.frozen, execution)
	prepareRequest, ok := prepareArgs[0].(map[string]any)
	if !ok {
		t.Fatalf("the prepare request is %T, not an object", prepareArgs[0])
	}
	prepareRequest["payload"] = map[string]any{"writingStyle": productionImportRunResidualStyle}
	productionImportRunReceiptRefused(t, f.author, seams[0].Operation, prepareArgs, "an already committed prepare replay")
	productionImportRunReceiptRefused(t, f.author, seams[1].Operation,
		productionImportRunReceiptWire(seams[1], f.projectPath, f.frozen, execution), "an already committed commit replay")
	productionImportRunResidualAssertUnchanged(t, f.database, settled, "the two replay shapes")
	after := productionImportRunWriteQuery(t, f.database, `SELECT run_id, state, effect_receipt_json
 FROM import_run_receipts WHERE stage = 'style' AND batch_id = 'done' ORDER BY run_id`)
	if len(after) != 2 {
		t.Fatalf("the replay refusal changed the receipt rows: %v", after)
	}
	for _, row := range after {
		runID := productionImportRunWriteText(row, "run_id")
		if productionImportRunWriteText(row, "state") != "committed" ||
			productionImportRunWriteText(row, "effect_receipt_json") != before[runID] {
			t.Fatalf("the replay refusal changed the committed receipt for %v: %v", runID, row)
		}
	}
}

// TestProductionImportRunReceiptSeamProof re-reads every declaration the seam
// table names. The marker search is productionSeamContains, not a line compare,
// because CI formats the Go source before these tests run; the recorded line is
// only a diagnosis. The negative check on the two live-run files is the part that
// answers the "look at the store's own read first" objection: if either file ever
// grows a generation_runs, binding_json, importSlot or generationRunHandle read,
// this test fails and the seam's claim has to be re-argued instead of silently
// becoming stale.
func TestProductionImportRunReceiptSeamProof(t *testing.T) {
	want := map[string]bool{
		productionImportRunReceiptsPrepareOperation:    true,
		productionImportRunReceiptsCommitOperation:     true,
		productionImportRunReceiptsInspectionOperation: true,
	}
	seams := productionImportRunReceiptSeams()
	if len(seams) != len(want) {
		t.Fatalf("the seam table holds %v rows, want %v", len(seams), len(want))
	}
	for _, seam := range seams {
		if !want[seam.Operation] {
			t.Fatalf("%v is not one of the three refused names", seam.Operation)
		}
		delete(want, seam.Operation)
		if seam.Arity < 2 || seam.PathIndex < 0 || seam.PathIndex > seam.Arity-2 {
			t.Fatalf("%v records arity %v and path index %v, which cannot describe a real request",
				seam.Operation, seam.Arity, seam.PathIndex)
		}
		if seam.Port == "" || len(seam.Missing) == 0 {
			t.Fatalf("%v records no missing port or no declaration behind it", seam.Operation)
		}
		if !productionImportRunReceiptSeamSymbolic(seam.Gate) {
			t.Fatalf("%v records %v as its gate, which is not a symbolic code", seam.Operation, seam.Gate)
		}
		for _, line := range seam.Missing {
			raw, err := os.ReadFile(line.Path)
			if err != nil {
				t.Fatalf("%v: %v: %v", seam.Operation, line.Path, err)
			}
			if !productionSeamContains(string(raw), line.Marker) {
				t.Fatalf("%v no longer documents %v anywhere in %v (recorded at line %v): %v",
					seam.Operation, line.Marker, line.Path, line.Line,
					productionSeamDiagnosis(string(raw), line.Marker, line.Line))
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("%v refused names have no seam row", len(want))
	}
	for _, path := range []string{"../../internal/store/importrun.go", "../../internal/store/importrun_lifecycle.go"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v: %v", path, err)
		}
		// sourceManifest alone is not the signal: importrun_lifecycle.go has a
		// local string variable of that name for a parsed source's manifest
		// fingerprint. These four tokens are the generation-run proof itself, and
		// none of them appears in either live-run file today.
		for _, forbidden := range []string{"generation_runs", "binding_json", "importSlot", "generationRunHandle"} {
			if strings.Contains(string(raw), forbidden) {
				t.Fatalf("%v now names %v; the store's live-run read may have grown the generation proof this seam says it lacks", path, forbidden)
			}
		}
	}
}

// TestProductionImportRunReceiptsBinderInstallsNothing pins the binder's contract:
// it is live in the real lane (production_importrun_residual_linux.go calls it),
// installs no operation, refuses a nil adapter or server, and still has every name
// inventoried so an absent handler is exactly 501 GO_OPERATION_NOT_MIGRATED and
// never 404 OPERATION_NOT_EXPOSED.
func TestProductionImportRunReceiptsBinderInstallsNothing(t *testing.T) {
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
	t.Cleanup(server.Close)
	if err := newProductionImportRunReceipts(&productionDurableOpen{}).Bind(server); err != nil {
		t.Fatalf("the seam binder failed: %v", err)
	}
	if names := server.BoundOperations(); len(names) != 0 {
		t.Fatalf("the seam binder installed %v, want nothing", names)
	}
	if err := newProductionImportRunReceipts(nil).Bind(server); err == nil {
		t.Fatal("a nil adapter was accepted")
	}
	if err := newProductionImportRunReceipts(&productionDurableOpen{}).Bind(nil); err == nil {
		t.Fatal("a nil server was accepted")
	}
	for _, seam := range productionImportRunReceiptSeams() {
		fresh := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "")
		if err := fresh.Bind(seam.Operation, func(context.Context, transport.Caller, []any) (any, error) {
			return nil, nil
		}); err != nil {
			t.Fatalf("%v is not inventoried, so its unbound answer would not be 501: %v", seam.Operation, err)
		}
		fresh.Close()
	}
	if !productionImportRunReceiptSeamSymbolic("GENERATION_GUARD_REQUIRED") ||
		productionImportRunReceiptSeamSymbolic("generation_guard_required") ||
		productionImportRunReceiptSeamSymbolic("GENERATION-GUARD") {
		t.Fatal("the symbolic-code shape check accepted a non-symbolic code or refused a symbolic one")
	}
	if productionImportRunReceiptsIsFamily("db:import-run-effect-receipt-get") ||
		!productionImportRunReceiptsIsFamily(productionImportRunReceiptsInspectionOperation) {
		t.Fatal("the family check misclassified an import-run name")
	}
}

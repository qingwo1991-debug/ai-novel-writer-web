//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// These tests pin why production_importrun_receipts_owner_linux.go publishes no
// handler, and nothing here installs one. They are the executable half of that
// file's audit:
//
//	1. the assert's first step cannot read inside the receipt transaction, because
//	   the transaction holds the author database's only connection and the callback
//	   is handed no connection of its own;
//	2. the owner is in scope at the receipt binder's call site and is still not
//	   passed to it, so the fix is one argument in a file this pass may not edit;
//	3. a real import-bound generation run does exist (the batch's second premise is
//	   dead) and its frozen source selection is prompt-only, which is what makes the
//	   fingerprint reproduction - the missing piece - need no database read at all.
//
// # Why the deadlock is measured instead of argued
//
// Claim 1 used to be prose. It is now a real store call against a real sqlite
// database opened with the production pool shape (one connection, exactly what
// projectfs gives the author handle), a real running import run with a live
// execution lease, and a real generation run bound to the requested slot. The
// assert is the reference's own first step, so the call reaches it: everything
// before it - the key check, the stage check, the payload canonicalization, the
// execution lease, the checkpoint and the bound-run query - passes. What fails is
// the read, and it fails with the request context's deadline, after the deadline,
// which is the shape of a wait that has no other connection to complete on.
// Nothing is written: the transaction rolls back, and the receipt and ledger
// tables are read back over a separate mode=ro connection.
//
// # Why the refusal tests are shaped this way
//
// No subtest here asserts a 200, a success envelope or a replay-as-success,
// because neither name is bound. The success-path requirements in a served-name
// batch apply to served names; production_importrun_receipts_linux_test.go:20-33
// records the same rule for the pair.

const (
	// productionImportRunReceiptsOwnerProofRunID is the import run the deadlock
	// proof is driven against: style stage, running, live lease.
	productionImportRunReceiptsOwnerProofRunID = "owner-proof-style-run"
	// productionImportRunReceiptsOwnerProofRootID is the generation root the frozen
	// binding names.
	productionImportRunReceiptsOwnerProofRootID = "owner-proof-root"
	// productionImportRunReceiptsOwnerProofGenerationRunID is the generation run
	// bound to the proof run's slot.
	productionImportRunReceiptsOwnerProofGenerationRunID = "owner-proof-generation-run"
	// productionImportRunReceiptsOwnerProofEpoch is the execution lease both the
	// seeded import run and the request carry.
	productionImportRunReceiptsOwnerProofEpoch = int64(5)
	// productionImportRunReceiptsOwnerProofLease is the run's frozen binding epoch,
	// the value requireRun compares against the handle.
	productionImportRunReceiptsOwnerProofLease = "owner-proof-lease"
	// productionImportRunReceiptsOwnerProofWait bounds the wait. It is long enough
	// that reaching the assert is certain and short enough that a deadlock is a
	// failing assertion rather than a hung test binary.
	productionImportRunReceiptsOwnerProofWait = 300 * time.Millisecond
)

// TestProductionImportRunReceiptsOwnerAssertDeadlocksOnTheReceiptConnection
// drives store.ImportRuns.PrepareEffectReceipt with the reference's first assert
// step and pins the only outcome it can have today: a wait for the connection the
// receipt transaction is holding, ended by the request context, with nothing
// written.
func TestProductionImportRunReceiptsOwnerAssertDeadlocksOnTheReceiptConnection(t *testing.T) {
	projectPath := productionImportRunResidualSeedProject(t, productionImportRunResidualSeed{
		ID: productionImportRunReceiptsOwnerProofRunID, Purpose: "reference", Stage: "style",
		Status: "running", Resumable: 1,
		ExecutionOwner: productionImportRunResidualOwner,
		ExecutionEpoch: productionImportRunReceiptsOwnerProofEpoch,
		LeaseExpiresAt: productionImportRunResidualLiveLease(),
	})
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	// The production pool, exactly: one connection, which the receipt's BEGIN
	// IMMEDIATE reserves for the whole of the callback.
	db.SetMaxOpenConns(1)
	// The residual schema is the import family's; the generation run the assert
	// reads is the binding's own two columns plus the row identity.
	if _, err := db.Exec(`CREATE TABLE generation_runs (
 run_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL, binding_json TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	binding, err := json.Marshal(map[string]any{
		"projectId": productionImportRunResidualProjectID,
		"epoch":     productionImportRunReceiptsOwnerProofLease,
		"sourceManifest": map[string]any{
			"operation": "analyze-writing-style",
			"importSlot": map[string]any{
				"runId":   productionImportRunReceiptsOwnerProofRunID,
				"stage":   "style",
				"batchId": "done",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO generation_runs (run_id, root_action_id, binding_json)
 VALUES (?, ?, ?)`, productionImportRunReceiptsOwnerProofGenerationRunID,
		productionImportRunReceiptsOwnerProofRootID, string(binding)); err != nil {
		t.Fatal(err)
	}
	request := store.ImportRunPrepareEffectReceiptRequest{
		RunID: productionImportRunReceiptsOwnerProofRunID, Stage: "style", BatchID: "done",
		EffectKey: "writing-style", Kind: "project-writing-style",
		Payload: map[string]any{
			"writingStyle": productionImportRunResidualStyle,
			"generationRunHandle": map[string]any{
				"projectId":    productionImportRunResidualProjectID,
				"epoch":        productionImportRunReceiptsOwnerProofLease,
				"rootActionId": productionImportRunReceiptsOwnerProofRootID,
				"runId":        productionImportRunReceiptsOwnerProofGenerationRunID,
			},
		},
	}
	execution := store.ImportRunExecution{
		Owner: productionImportRunResidualOwner, Epoch: productionImportRunReceiptsOwnerProofEpoch,
	}
	ctx, cancel := context.WithTimeout(context.Background(), productionImportRunReceiptsOwnerProofWait)
	defer cancel()
	started := time.Now()
	_, err = store.NewImportRuns(db).PrepareEffectReceipt(ctx, execution, request,
		productionImportRunReceiptsOwnerAssert(ctx, db))
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("the assert's first read returned inside the receipt transaction, so the audit's first blocker is gone")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the assert failed with %v, not with the connection wait the audit names", err)
	}
	if elapsed < productionImportRunReceiptsOwnerProofWait {
		t.Fatalf("the assert returned after %v, before the deadline that had to expire", elapsed)
	}
	// A refused or rolled-back prepare writes neither the receipt nor its ledger
	// row. Both are read over a separate mode=ro connection, so the assertion is
	// about committed bytes and not about this handle's view.
	for _, table := range []string{"import_run_receipts", "import_effect_ledger"} {
		rows := productionImportRunWriteQuery(t, database, "SELECT COUNT(*) AS total FROM "+table)
		if len(rows) != 1 || productionImportRunWriteNumber(rows[0], "total") != 0 {
			t.Fatalf("the timed-out prepare left rows in %v: %v", table, rows)
		}
	}
}

// TestProductionImportRunReceiptsOwnerIsOneArgumentInsideTheOwnerBlock pins the
// wiring the audit asks for. The owner block opens at production_open_linux.go:591
// and the receipt family's binder is constructed from inside it, before an
// owner-bound binder that already receives `owner`; handing the owner over would
// be exactly one extra argument on that call, and the moment the argument is
// added this refusal has to be re-argued instead of silently outliving its
// reason.
func TestProductionImportRunReceiptsOwnerIsOneArgumentInsideTheOwnerBlock(t *testing.T) {
	raw, err := os.ReadFile("production_open_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	for _, marker := range []string{
		productionImportRunReceiptsOwnerBlockLine,
		productionImportRunReceiptsOwnerWiringLine,
		productionImportRunReceiptsOwnerLateOwnerLine,
	} {
		if !productionSeamContains(source, marker) {
			t.Fatalf("production_open_linux.go no longer contains the owner-block line the audit names: %v", marker)
		}
	}
	compact := productionSeamCompact(source)
	block := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerBlockLine))
	wiring := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerWiringLine))
	late := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerLateOwnerLine))
	if block < 0 || wiring < 0 || late < 0 || block >= wiring || wiring >= late {
		t.Fatalf("the receipt binder is no longer between the owner block and an owner-bound binder: %v %v %v",
			block, wiring, late)
	}
	if productionSeamContains(source, productionImportRunReceiptsOwnerWiringFixed) {
		t.Fatal("the receipt binder now receives the owner; the refusal in production_importrun_receipts_owner_linux.go must be re-argued")
	}
	residual, err := os.ReadFile("production_importrun_residual_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if !productionSeamContains(string(residual), productionImportRunReceiptsOwnerBinderLine) {
		t.Fatal("the residual binder no longer calls the receipt seam binder with the durable alone")
	}
	// The audit quotes the store's own evidence, so a change there is a change to
	// the reason the pair is refused.
	for _, fact := range productionImportRunReceiptsOwnerFacts() {
		content, err := os.ReadFile(fact.Path)
		if err != nil {
			t.Fatalf("%v: %v", fact.Path, err)
		}
		if !productionSeamContains(string(content), fact.Marker) {
			t.Fatalf("%v no longer documents %v (recorded at line %v): %v",
				fact.Path, fact.Marker, fact.Line, productionSeamDiagnosis(string(content), fact.Marker, fact.Line))
		}
	}
	// Both names are still refused by decision and by seam, and the seam no longer
	// blames the generation ledger: it admits the import keys now, so the ledger is
	// not what keeps the pair unbound.
	for _, name := range []string{
		productionImportRunReceiptsPrepareOperation,
		productionImportRunReceiptsCommitOperation,
	} {
		seam, present := productionImportRunReceiptSeamFor(name)
		if !present {
			t.Fatalf("%v has no seam row, so its refusal has no documented reason", name)
		}
		for _, line := range seam.Missing {
			if line.Path == "../../internal/store/generation_ledger.go" {
				t.Fatalf("%v still blames the generation ledger, which now admits the import manifest keys", name)
			}
		}
		served := false
		for _, decision := range productionImportRunReceiptServeDecisions() {
			if decision.Operation == name && decision.Served {
				served = true
			}
		}
		if served {
			t.Fatalf("%v is recorded as served while no handler is bound for it", name)
		}
	}
}

// TestProductionImportRunReceiptsOwnerRunIsPromptOnly begins a real import-bound
// generation run through the frozen session and reads its frozen manifest back.
// It kills the batch's second premise - such a run can exist - and it records the
// one fact that makes the missing reproduction cheap: the frozen selection is
// prompt refs only, and the prompt reader is pure, so ReproduceBinding over this
// run needs no database read at all.
func TestProductionImportRunReceiptsOwnerRunIsPromptOnly(t *testing.T) {
	lane := productionImportGenerationLaneOpen(t)
	var begun map[string]any
	productionImportGenerationDecoded(t, lane, "generation:begin",
		[]any{productionImportGenerationSelection("style", "import-receipt-owner-style-begin"), lane.frozen}, &begun)
	handle := syntheticMap(begun["handle"])
	if handle == nil || syntheticString(handle["runId"]) == "" {
		t.Fatalf("generation:begin answered no run handle for the style import slot: %v", begun)
	}
	runID := syntheticString(handle["runId"])
	rows := productionImportRunWriteQuery(t, lane.database,
		`SELECT binding_json FROM generation_runs WHERE run_id = ?`, runID)
	if len(rows) != 1 {
		t.Fatalf("the frozen style run is %v rows in generation_runs, want one", len(rows))
	}
	var binding struct {
		ProjectID  string `json:"projectId"`
		Epoch      string `json:"epoch"`
		SourceRefs []struct {
			SourceID string `json:"sourceId"`
			Revision int64  `json:"revision"`
			Hash     string `json:"contentHash"`
		} `json:"sourceRefs"`
		SourceManifest struct {
			ModelLeaseID             string `json:"modelLeaseId"`
			GoOwnerSourceFingerprint string `json:"goOwnerSourceFingerprint"`
			ImportSlot               struct {
				RunID   string `json:"runId"`
				Stage   string `json:"stage"`
				BatchID string `json:"batchId"`
			} `json:"importSlot"`
		} `json:"sourceManifest"`
	}
	if err := json.Unmarshal([]byte(productionImportRunWriteText(rows[0], "binding_json")), &binding); err != nil {
		t.Fatalf("the frozen binding does not decode: %v", err)
	}
	// The run is frozen against the stage's own slot, for this project and this
	// session lease. The lease the receipt request carries is the binding's epoch,
	// so the epoch half of the reproduction scope is already on the wire.
	if binding.SourceManifest.ImportSlot.RunID != productionImportGenerationStyleRunID ||
		binding.SourceManifest.ImportSlot.Stage != "style" ||
		binding.SourceManifest.ImportSlot.BatchID != "done" {
		t.Fatalf("the frozen binding names another slot: %v", binding.SourceManifest.ImportSlot)
	}
	if binding.ProjectID != lane.projectID || binding.Epoch != lane.leaseID {
		t.Fatalf("the frozen binding is scoped to %v/%v, want %v/%v",
			binding.ProjectID, binding.Epoch, lane.projectID, lane.leaseID)
	}
	// The lease record ReproduceBinding resolves, and the digest it compares with.
	if binding.SourceManifest.ModelLeaseID == "" {
		t.Fatal("the frozen manifest carries no modelLeaseId, so ReproduceBinding has no lease to resolve")
	}
	if !syntheticHashPattern.MatchString(binding.SourceManifest.GoOwnerSourceFingerprint) {
		t.Fatalf("the frozen manifest carries no binding fingerprint: %v",
			binding.SourceManifest.GoOwnerSourceFingerprint)
	}
	// The constructive half: every frozen source is a prompt capability, whose
	// reader is synthetic. A reproduction over these refs touches no table.
	if len(binding.SourceRefs) == 0 {
		t.Fatal("the frozen binding carries no source refs")
	}
	for _, ref := range binding.SourceRefs {
		if !strings.HasPrefix(ref.SourceID, "prompt:") {
			t.Fatalf("the style run froze %v, which the pure prompt reader cannot reproduce", ref.SourceID)
		}
		if ref.Revision < 0 || !syntheticHashPattern.MatchString(ref.Hash) {
			t.Fatalf("the frozen ref %v carries revision %v and hash %v", ref.SourceID, ref.Revision, ref.Hash)
		}
	}
	// The import run the slot names is the seeded one, still running under the
	// lease the receipt request would carry.
	for _, row := range productionImportRunWriteQuery(t, lane.database,
		`SELECT stage, status, execution_owner, execution_epoch FROM import_runs WHERE id = ?`,
		productionImportGenerationStyleRunID) {
		if productionImportRunWriteText(row, "stage") != "style" ||
			productionImportRunWriteText(row, "status") != "running" {
			t.Fatalf("the slot's import run is not a running style run: %v", row)
		}
	}
}

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

// These tests are the executable half of
// production_importrun_receipts_owner_linux.go's wiring record, and they pin the two
// things that changed:
//
//	1. the store's generation-source proof now RECEIVES the run. A real
//	   PrepareEffectReceipt call against a real sqlite database opened with the
//	   production pool shape (one connection, exactly what projectfs gives the author
//	   handle) completes inside the transaction, and the assert is handed the
//	   binding_json, the root's status and the stopped attempt's artifact instead of
//	   opening a read of its own. The previous pass measured that same call ending in
//	   context.DeadlineExceeded; this one ends in a prepared receipt.
//	2. the wiring record still describes the shipped source: the owner block opens
//	   where it says, the anchor binder it names is still installed from that block,
//	   and the literal line this family needs is the call the served binder takes. It
//	   does not assert that the line is absent, because the pass that installs it must
//	   not have to edit this file.
//
// There is no "the assert deadlocks" subtest any more: the callback no longer takes
// a database handle, so the shape it measured cannot be constructed. That is the
// point of the change, and asserting it in prose would be weaker than the positive
// test below.

const (
	// productionImportRunReceiptsOwnerProofRunID is the import run the store-level
	// proof is driven against: style stage, running, live lease.
	productionImportRunReceiptsOwnerProofRunID = "owner-proof-style-run"
	// productionImportRunReceiptsOwnerProofRootID is the generation root the frozen
	// binding names.
	productionImportRunReceiptsOwnerProofRootID = "owner-proof-root"
	// productionImportRunReceiptsOwnerProofGenerationRunID is the generation run
	// bound to the proof run's slot.
	productionImportRunReceiptsOwnerProofGenerationRunID = "owner-proof-generation-run"
	// productionImportRunReceiptsOwnerProofAttemptID and ...ArtifactID are the one
	// settled attempt and the artifact the proof requires.
	productionImportRunReceiptsOwnerProofAttemptID  = "owner-proof-attempt"
	productionImportRunReceiptsOwnerProofArtifactID = "owner-proof-artifact"
	// productionImportRunReceiptsOwnerProofEpoch is the execution lease both the
	// seeded import run and the request carry.
	productionImportRunReceiptsOwnerProofEpoch = int64(5)
	// productionImportRunReceiptsOwnerProofLease is the run's frozen binding epoch,
	// the value requireRun compares against the handle.
	productionImportRunReceiptsOwnerProofLease = "owner-proof-lease"
	// productionImportRunReceiptsOwnerProofText is the artifact's visible text. Its
	// sha256 is the textHash the reference re-derives.
	productionImportRunReceiptsOwnerProofText = "导入推演：门后有人，灯下无影。"
	// productionImportRunReceiptsOwnerProofWait bounds the call. A proof that took a
	// second connection would wait for the one the transaction holds and end here;
	// a proof that reads the evidence finishes in milliseconds.
	productionImportRunReceiptsOwnerProofWait = 5 * time.Second
)

// productionImportRunReceiptsOwnerProofEvidence is what the witness assert saw. It is
// filled inside the store's transaction, so a field it read is a field the store
// handed over.
type productionImportRunReceiptsOwnerProofEvidence struct {
	calls    int
	slot     store.ImportRunGenerationSlot
	run      store.ImportRunGenerationEvidence
	handleID string
}

// TestProductionImportRunReceiptsOwnerProofReadsOnTheReceiptConnection drives
// store.ImportRuns.PrepareEffectReceipt with a witness proof over a real database
// opened exactly like the author pool, and pins the outcome the widened callback
// makes possible: the proof is called inside the transaction, it is handed the run,
// and nothing waits for a second connection.
func TestProductionImportRunReceiptsOwnerProofReadsOnTheReceiptConnection(t *testing.T) {
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
	// The generation ledger's four tables, with the columns the store's evidence read
	// selects. They are written here rather than created by the generation lane
	// because this test is about the receipt transaction's read, not about a begin.
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
			"promptKeys":               []any{"analyze_writing_style"},
			"modelLeaseId":             "owner-proof-model-lease",
			"goOwnerSourceFingerprint": syntheticHash("owner-proof-fingerprint"),
		},
		"sourceRefs": []any{},
	})
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := json.Marshal(map[string]any{
		"artifactId": productionImportRunReceiptsOwnerProofArtifactID,
		"attemptId":  productionImportRunReceiptsOwnerProofAttemptID,
		"text":       productionImportRunReceiptsOwnerProofText,
		"textHash":   syntheticHash(productionImportRunReceiptsOwnerProofText),
		"revision":   int64(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	action, err := json.Marshal(map[string]any{"status": "active", "rootActionId": productionImportRunReceiptsOwnerProofRootID})
	if err != nil {
		t.Fatal(err)
	}
	usage, err := json.Marshal(map[string]any{
		"purpose": "analyze-writing-style",
		"result":  map[string]any{"finishReason": "stop"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`CREATE TABLE generation_runs (
 run_id TEXT PRIMARY KEY, root_action_id TEXT NOT NULL, binding_json TEXT NOT NULL)`, nil},
		{`CREATE TABLE generation_roots (
 root_action_id TEXT PRIMARY KEY, idempotency_key TEXT NOT NULL, action_json TEXT NOT NULL,
 budget_json TEXT NOT NULL, active_elapsed_ms INTEGER NOT NULL DEFAULT 0, active_since_ms INTEGER,
 blocked_code TEXT, revision INTEGER NOT NULL DEFAULT 0)`, nil},
		{`CREATE TABLE generation_attempts (
 attempt_id TEXT PRIMARY KEY, reservation_id TEXT NOT NULL UNIQUE, run_id TEXT NOT NULL,
 root_action_id TEXT NOT NULL, attempt_json TEXT NOT NULL, usage_receipt_json TEXT,
 invocation_nonce TEXT NOT NULL)`, nil},
		{`CREATE TABLE generation_artifacts (
 artifact_id TEXT PRIMARY KEY, attempt_id TEXT NOT NULL UNIQUE, run_id TEXT NOT NULL,
 artifact_json TEXT NOT NULL, revision INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'partial')`, nil},
		{`INSERT INTO generation_runs (run_id, root_action_id, binding_json) VALUES (?, ?, ?)`,
			[]any{productionImportRunReceiptsOwnerProofGenerationRunID,
				productionImportRunReceiptsOwnerProofRootID, string(binding)}},
		{`INSERT INTO generation_roots (root_action_id, idempotency_key, action_json, budget_json)
 VALUES (?, ?, ?, ?)`,
			[]any{productionImportRunReceiptsOwnerProofRootID, "owner-proof-key", string(action), "{}"}},
		{`INSERT INTO generation_attempts (attempt_id, reservation_id, run_id, root_action_id,
 attempt_json, usage_receipt_json, invocation_nonce) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			[]any{productionImportRunReceiptsOwnerProofAttemptID, "owner-proof-reservation",
				productionImportRunReceiptsOwnerProofGenerationRunID,
				productionImportRunReceiptsOwnerProofRootID,
				`{"status":"settled"}`, string(usage), "import:0"}},
		{`INSERT INTO generation_artifacts (artifact_id, attempt_id, run_id, artifact_json, revision, status)
 VALUES (?, ?, ?, ?, ?, ?)`,
			[]any{productionImportRunReceiptsOwnerProofArtifactID,
				productionImportRunReceiptsOwnerProofAttemptID,
				productionImportRunReceiptsOwnerProofGenerationRunID, string(artifact), int64(1), "partial"}},
	} {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %v: %v", statement.query, err)
		}
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
	witness := &productionImportRunReceiptsOwnerProofEvidence{}
	proof := func(handle any, slot store.ImportRunGenerationSlot, run store.ImportRunGenerationEvidence) error {
		witness.calls++
		witness.slot = slot
		witness.run = run
		witness.handleID = syntheticString(syntheticMap(handle)["runId"])
		// A proof with no database handle cannot read one: this is the whole point of
		// the widened callback, and any attempt to open a second connection here would
		// have to come from the test, not from the store.
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), productionImportRunReceiptsOwnerProofWait)
	defer cancel()
	started := time.Now()
	prepared, err := store.NewImportRuns(db).PrepareEffectReceipt(ctx, execution, request, proof)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("the receipt proof failed inside its own transaction: %v", err)
	}
	if elapsed >= productionImportRunReceiptsOwnerProofWait {
		t.Fatalf("the receipt proof took %v, which is the deadline that had to expire", elapsed)
	}
	if witness.calls != 1 {
		t.Fatalf("the store called the proof %v times, want one", witness.calls)
	}
	if witness.handleID != productionImportRunReceiptsOwnerProofGenerationRunID {
		t.Fatalf("the proof was handed handle %v", witness.handleID)
	}
	// The slot is the receipt's own, and it is what the proof compares the frozen one
	// with.
	if witness.slot.RunID != productionImportRunReceiptsOwnerProofRunID || witness.slot.Stage != "style" ||
		witness.slot.BatchID != "done" {
		t.Fatalf("the proof was handed slot %v", witness.slot)
	}
	// The evidence: the run, its binding verbatim, the root's status and the settled
	// attempt with its artifact.
	if !witness.run.RunPresent || witness.run.RunID != productionImportRunReceiptsOwnerProofGenerationRunID ||
		witness.run.RootActionID != productionImportRunReceiptsOwnerProofRootID {
		t.Fatalf("the proof was handed run %v", witness.run)
	}
	if string(witness.run.BindingJSON) != string(binding) {
		t.Fatalf("the proof was handed a rewritten binding: %v", string(witness.run.BindingJSON))
	}
	if !witness.run.RootPresent || witness.run.RootStatus != "active" {
		t.Fatalf("the proof was handed root %v present=%v status=%v",
			witness.run.RootActionID, witness.run.RootPresent, witness.run.RootStatus)
	}
	if len(witness.run.Attempts) != 1 {
		t.Fatalf("the proof was handed %v attempts, want one", len(witness.run.Attempts))
	}
	attempt := witness.run.Attempts[0]
	if attempt.AttemptID != productionImportRunReceiptsOwnerProofAttemptID || !attempt.ArtifactPresent ||
		attempt.ArtifactID != productionImportRunReceiptsOwnerProofArtifactID ||
		attempt.ArtifactStatus != "partial" {
		t.Fatalf("the proof was handed attempt %v", attempt)
	}
	if string(attempt.UsageReceiptJSON) != string(usage) || string(attempt.ArtifactJSON) != string(artifact) {
		t.Fatalf("the proof was handed a rewritten attempt payload: %v", attempt)
	}
	// And the receipt the proof earned is committed by the same transaction. It is
	// read back over a separate mode=ro connection.
	receipt, _ := prepared.(map[string]any)
	if receipt == nil || productionImportRunWriteText(receipt, "state") != "prepared" {
		t.Fatalf("the prepared receipt is %v", prepared)
	}
	rows := productionImportRunWriteQuery(t, database, `SELECT state, payload_hash FROM import_run_receipts
 WHERE run_id = ? AND stage = 'style' AND batch_id = 'done'`, productionImportRunReceiptsOwnerProofRunID)
	if len(rows) != 1 || productionImportRunWriteText(rows[0], "state") != "prepared" {
		t.Fatalf("the committed receipt row is %v", rows)
	}
}

// TestProductionImportRunReceiptsOwnerEvidenceReadIsAnOwnConnectionRead pins the
// store's own read from the public surface: in a project that never wrote a
// generation run there is no generation_runs table at all, and the read reports NO
// RUN rather than failing with a driver error. The prover therefore owns every
// refusal code, and the absent-run answer is GENERATION_RUN_MISSING on both legs of
// the receipt.
func TestProductionImportRunReceiptsOwnerEvidenceReadIsAnOwnConnectionRead(t *testing.T) {
	projectPath := productionImportRunResidualSeedProject(t, productionImportRunResidualSeed{
		ID: productionImportRunReceiptsOwnerProofRunID, Purpose: "reference", Stage: "style",
		Status: "running", Resumable: 1,
		ExecutionOwner: productionImportRunResidualOwner,
		ExecutionEpoch: productionImportRunReceiptsOwnerProofEpoch,
		LeaseExpiresAt: productionImportRunResidualLiveLease(),
	})
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	// The residual schema has no generation table, so this database is exactly the
	// "the author never generated anything" shape.
	existing := productionImportRunWriteQuery(t, database, `SELECT COUNT(*) AS total FROM sqlite_master
 WHERE type = 'table' AND name = 'generation_runs'`)
	if len(existing) != 1 || productionImportRunWriteNumber(existing[0], "total") != 0 {
		t.Fatalf("the residual schema now carries generation_runs: %v", existing)
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
	seen := 0
	_, err = store.NewImportRuns(db).PrepareEffectReceipt(context.Background(), execution, request,
		func(handle any, slot store.ImportRunGenerationSlot, run store.ImportRunGenerationEvidence) error {
			seen++
			if run.RunPresent || len(run.Attempts) != 0 || run.BindingJSON != nil {
				t.Fatalf("the read invented a run in a database with no generation table: %v", run)
			}
			return errors.New("GENERATION_RUN_MISSING")
		})
	if err == nil || err.Error() != "GENERATION_RUN_MISSING" {
		t.Fatalf("the absent-run prepare answered %v, want the prover's own code", err)
	}
	if seen != 1 {
		t.Fatalf("the proof ran %v times, want one", seen)
	}
	// Nothing was written: the proof runs before the receipt's INSERT.
	rows := productionImportRunWriteQuery(t, database, `SELECT COUNT(*) AS total FROM import_run_receipts
 WHERE run_id = ?`, productionImportRunReceiptsOwnerProofRunID)
	if len(rows) != 1 || productionImportRunWriteNumber(rows[0], "total") != 0 {
		t.Fatalf("the refused prepare wrote %v", rows)
	}
}

// TestProductionImportRunReceiptsOwnerFactsAreReal re-reads every declaration the
// wiring record names, and checks the owner block's ordering: the block opens before
// the owner-bound binder the new line is anchored to. The marker search is
// productionSeamContains, not a line compare, because CI formats the Go source before
// these tests run.
func TestProductionImportRunReceiptsOwnerFactsAreReal(t *testing.T) {
	for _, fact := range productionImportRunReceiptsOwnerFacts() {
		raw, err := os.ReadFile(fact.Path)
		if err != nil {
			t.Fatalf("%v: %v", fact.Path, err)
		}
		if !productionSeamContains(string(raw), fact.Marker) {
			t.Fatalf("%v no longer documents %v (recorded at line %v): %v",
				fact.Path, fact.Marker, fact.Line,
				productionSeamDiagnosis(string(raw), fact.Marker, fact.Line))
		}
	}
	raw, err := os.ReadFile("production_open_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	compact := productionSeamCompact(string(raw))
	block := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerBlockLine))
	anchor := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerLateOwnerLine))
	residual := strings.Index(compact, productionSeamCompact(productionImportRunReceiptsOwnerResidualBinder))
	if block < 0 || anchor < 0 || residual < 0 || block >= residual || block >= anchor {
		t.Fatalf("the owner block no longer contains both binders in order: %v %v %v", block, residual, anchor)
	}
	// The record's line is the binder's own constructor call, so a rename on either
	// side fails here instead of drifting.
	serve, err := os.ReadFile("production_importrun_receipts_serve2_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	if !productionSeamContains(string(serve), productionImportRunReceiptsOwnerServe2Binder) {
		t.Fatalf("the served binder no longer takes the owner: %v", productionImportRunReceiptsOwnerServe2Binder)
	}
	if !productionSeamContains(string(serve), "func newProductionImportRunReceiptsServe2(owner *productionGeneration)") {
		t.Fatal("the served binder's constructor no longer takes the generation owner")
	}
}

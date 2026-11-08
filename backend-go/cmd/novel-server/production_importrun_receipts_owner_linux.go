//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// This file is the OWNER half of the import-run receipt audit, and it publishes
// no handler. The pair stays at GO_OPERATION_NOT_MIGRATED for two reasons that
// are about THIS pass's write scope and about the store's own calling
// convention, not about the two premises the batch invalidated. Both of those
// premises are recorded as dead below, because leaving them in place would be
// the stale note the seam file exists to prevent.
//
// # Premise 1, dead: the owner is now reachable at the binder's own call site
//
// The generation owner is created by
//
//	production_open_linux.go:591  if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {
//
// and the import-run residual binder - the one that calls this family's receipt
// seam binder - is installed from INSIDE that block:
//
//	production_open_linux.go:627  if bindErr := newProductionImportRunResiduals(open.durable).Bind(server); bindErr != nil {
//	production_importrun_residual_linux.go:247  if err := newProductionImportRunReceipts(r.durable).Bind(server); err != nil {
//
// So the earlier wording ("the owner is a local variable this adapter is not
// given") was right about the argument and wrong about the reach: `owner` is a
// bound identifier at :627. The binder does NOT have to move into the owner
// block - it is already there - it has to be HANDED the owner: exactly one
// extra argument at :627, then one more at :247, and
// production_importrun_receipts_owner_linux_test.go's ordering pin has to be
// re-argued. The model-profile loader it needs is the closure built at
// production_generation_begin_linux.go:133 over config.ListPrivateModels, and
// both the registry and that loader hang off the returned *productionGeneration
// (production_generation_begin_linux.go:42-45) through its syntheticGeneration
// (cmd/novel-server/synthetic_generation_linux.go:37-43).
//
// That one line is in a file this pass may not edit, and an owner-bound binder
// that no lane constructs is dead code of exactly the kind
// production_importrun_residual_linux.go:239-247 refuses to publish. So the
// pair cannot be served from the shipped lane in this pass, and this pass does
// not pretend otherwise.
//
// # Premise 2, dead: an import-bound generation run can exist
//
// The generation ledger now admits the three import manifest members for the two
// operations that own them (internal/store/generation_ledger.go, the
// "importSlot", "importSlotKey", "importContext" case of
// glFamilyManifestAllowed), generation:begin admits an importSlot selection and
// freezes it (production_import_generation_serve_linux.go:288-368
// productionImportGenerationSelect and :429-487 productionImportGenerationFreeze),
// and production_import_generation_serve_linux_test.go freezes one through the
// real HTTP transport for each stage. The old claim that no such run can exist,
// and the probe that used to pin the ledger's refusal, are both gone.
// TestProductionImportRunReceiptsOwnerRunIsPromptOnly below begins such a run
// through the frozen session and reads its frozen manifest back.
//
// # What actually blocks, named
//
// The reference's assert has three steps that matter here, and the first one
// reads state:
//
//	electron/services/main-generation-owner.ts:1454  const run = requireRun(handle)
//	electron/services/main-generation-owner.ts:1462  imports.context(run).slot
//	electron/services/main-generation-owner.ts:1463  deps.rebuildBinding(run.binding, currentModelReceipt(run.binding))
//
// In the desktop that read is legal because the owner is a dispatcher over the
// one better-sqlite3 connection and the controller refuses the call unless the
// database is already in a transaction
// (electron/controllers/generation-controller.ts:74). The Go receipt path has
// the same convention: the callback is invoked from INSIDE
// importRunImmediate's BEGIN IMMEDIATE (internal/store/importrun_receipts.go:987
// and :1005, reserved at internal/store/importrun.go:948). But the author
// database is opened with exactly one connection
// (internal/projectfs/author_linux.go:121 db.SetMaxOpenConns(1)), which the store
// itself names: "it is a deadlock rather than a proof"
// (internal/store/character_proposal_pending.go:37). A callback that reads the
// generation run through the borrowed handle therefore waits for the connection
// the receipt transaction is holding, and the
// store.ImportRunGenerationSourceAssert signature it is called through
// (internal/store/importrun_receipts.go:118) carries no connection and no
// evidence: only (handle, slot).
//
// So the assert cannot take its first step today, and no wiring change fixes
// that. The constructive half: an import run's frozen source selection is
// PROMPT-ONLY (the two stage selections carry promptKeys and no drafts and no
// blueprints, production_import_generation_serve_linux.go:133-168), and the
// prompt reader in syntheticBuildSources is pure
// (cmd/novel-server/synthetic_generation_linux.go:657-675). ReproduceBinding
// over those refs therefore needs NO database read at all - only the registry
// lease record the loader's revision feeds (internal/generationowner/lease.go:164).
// The single store read the assert needs is the run itself: binding_json, the
// root's status and the stopped attempts' artifacts. That is one addition to the
// store's calling convention (read the run on the receipt's own connection and
// hand it to the prover), and it only becomes useful in the same change that
// hands the binder the owner at production_open_linux.go:627.
//
// # What this pass does not publish, and why each substitute is refused
//
//   - a nil proof: prepare would accept a handle without any proof
//     (internal/store/importrun_receipts.go:983-985) and commit would refuse it
//     (internal/store/importrun_receipts.go:1000-1004), so the lane could write
//     receipts it could never commit;
//   - an assert that reads the borrowed handle BEFORE the transaction: it would
//     pass the deadlock by giving up the reference's atomicity - the sources and
//     the run could change between the read and the effect, which is exactly the
//     window GENERATION_IMPORT_EFFECT_SOURCE_CHANGED exists to close;
//   - an assert that opens its own connection to the author's database file: the
//     lane's adapters resolve no path and open no database, and a second handle
//     would bypass the identity and sidecar checks projectfs performs.
//
// The two names therefore keep their 501, the db family's residual table keeps
// naming them, and the served count stays 273. This file contributes the audit
// and the executable probe below, both of which the next pass needs and neither
// of which installs a handler.

const (
	// productionImportRunReceiptsOwnerBlockLine opens the block that owns the
	// generation registry, its model-profile loader and its source readers.
	productionImportRunReceiptsOwnerBlockLine = "if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {"
	// productionImportRunReceiptsOwnerWiringLine is the literal call that must gain
	// the owner. It is inside the block above, so the fix is an argument, not a move.
	productionImportRunReceiptsOwnerWiringLine = "newProductionImportRunResiduals(open.durable).Bind(server)"
	// productionImportRunReceiptsOwnerWiringFixed is what the line becomes. Its
	// absence is pinned by the test, because making it is the change that has to
	// re-argue this refusal instead of silently invalidating it.
	productionImportRunReceiptsOwnerWiringFixed = "newProductionImportRunResiduals(open.durable, owner).Bind(server)"
	// productionImportRunReceiptsOwnerBinderLine is the family's own seam binder,
	// which today receives the durable open alone.
	productionImportRunReceiptsOwnerBinderLine = "newProductionImportRunReceipts(r.durable).Bind(server)"
	// productionImportRunReceiptsOwnerLateOwnerLine is an owner-bound binder that is
	// already installed from the same block. The ordering pin uses it so a move of
	// the receipt binder out of the block fails the test.
	productionImportRunReceiptsOwnerLateOwnerLine = "newProductionImportGenerationServe(owner).Bind(server)"
	// productionImportRunReceiptsOwnerAssertSignature is the callback the store
	// hands no connection and no evidence to.
	productionImportRunReceiptsOwnerAssertSignature = "type ImportRunGenerationSourceAssert func(handle any, slot ImportRunGenerationSlot) error"
	// productionImportRunReceiptsOwnerScopeLine is the registry look-up
	// ReproduceBinding performs: a lease record, not the current model profile.
	productionImportRunReceiptsOwnerScopeLine = "if !ok||scope!=record.scope||purpose!=record.purpose{"
	// productionImportRunReceiptsOwnerPoolLine is the one connection the receipt
	// transaction holds while the assert runs.
	productionImportRunReceiptsOwnerPoolLine = "db.SetMaxOpenConns(1)"
	// productionImportRunReceiptsOwnerDeadlockLine is how the store already
	// describes a prover that takes a second connection.
	productionImportRunReceiptsOwnerDeadlockLine = "which is a deadlock rather than a proof"
)

// The three refusals this file's probe can answer. The first two are the
// reference's own codes (main-generation-owner.ts:156-167); the third is the
// slot comparison's (:1462). None of them is reachable through the lane today,
// because the read that decides them never returns.
var (
	productionImportRunReceiptsOwnerRunMissing  = errors.New("GENERATION_RUN_NOT_FOUND")
	productionImportRunReceiptsOwnerIdentity    = errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	productionImportRunReceiptsOwnerSlotChanged = errors.New("GENERATION_IMPORT_EFFECT_SLOT_CHANGED")
)

// productionImportRunReceiptsOwnerRun is the store's own view of one generation
// run: the row identity plus the frozen binding the reproduction re-fingerprints.
type productionImportRunReceiptsOwnerRun struct {
	RunID        string
	RootActionID string
	ProjectID    string
	Epoch        string
	BindingJSON  []byte
}

// productionImportRunReceiptsOwnerReadRun is requireRun's read
// (main-generation-owner.ts:156-167) written the only way this lane can write
// it inside the receipt transaction: against the handle the borrow returned. It
// is the exact call that cannot get a connection, and it is kept executable so
// the deadlock is measured by the test rather than asserted in prose.
func productionImportRunReceiptsOwnerReadRun(ctx context.Context, db *sql.DB, runID string) (productionImportRunReceiptsOwnerRun, error) {
	if db == nil || runID == "" {
		return productionImportRunReceiptsOwnerRun{}, productionImportRunReceiptsOwnerRunMissing
	}
	var rootActionID, bindingJSON string
	err := db.QueryRowContext(ctx, `SELECT root_action_id, binding_json FROM generation_runs WHERE run_id = ?`,
		runID).Scan(&rootActionID, &bindingJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return productionImportRunReceiptsOwnerRun{}, productionImportRunReceiptsOwnerRunMissing
	}
	if err != nil {
		return productionImportRunReceiptsOwnerRun{}, err
	}
	run := productionImportRunReceiptsOwnerRun{
		RunID: runID, RootActionID: rootActionID, BindingJSON: []byte(bindingJSON),
	}
	var binding struct {
		ProjectID string `json:"projectId"`
		Epoch     string `json:"epoch"`
	}
	if err := json.Unmarshal([]byte(bindingJSON), &binding); err != nil {
		return productionImportRunReceiptsOwnerRun{}, productionImportRunReceiptsOwnerIdentity
	}
	run.ProjectID = binding.ProjectID
	run.Epoch = binding.Epoch
	return run, nil
}

// productionImportRunReceiptsOwnerAssert is the store.ImportRunGenerationSourceAssert
// a served handler would have to install: requireRun's identity, then the frozen
// import slot. It stops at step 4 of the reference's assert - the fingerprint
// reproduction - because reproducing it needs the owner's registry, which no
// argument of this closure can carry. Its purpose here is the first step: called
// from inside the receipt's transaction it must block, and the test pins that.
func productionImportRunReceiptsOwnerAssert(ctx context.Context, db *sql.DB) store.ImportRunGenerationSourceAssert {
	return func(handle any, slot store.ImportRunGenerationSlot) error {
		if ctx == nil || db == nil {
			return productionImportRunReceiptsOwnerRunMissing
		}
		object := syntheticMap(handle)
		if object == nil {
			return productionImportRunReceiptsOwnerIdentity
		}
		runID := syntheticString(object["runId"])
		if runID == "" {
			return productionImportRunReceiptsOwnerIdentity
		}
		run, err := productionImportRunReceiptsOwnerReadRun(ctx, db, runID)
		if err != nil {
			return err
		}
		// :158-166 the handle is the run's own identity, on both the row side and
		// the frozen binding side.
		if run.RunID != runID || run.RootActionID != syntheticString(object["rootActionId"]) ||
			run.ProjectID != syntheticString(object["projectId"]) ||
			run.Epoch != syntheticString(object["epoch"]) {
			return productionImportRunReceiptsOwnerIdentity
		}
		// :1462 the frozen import slot must name exactly the requested one.
		var binding struct {
			SourceManifest struct {
				ImportSlot struct {
					RunID   string `json:"runId"`
					Stage   string `json:"stage"`
					BatchID string `json:"batchId"`
				} `json:"importSlot"`
			} `json:"sourceManifest"`
		}
		if err := json.Unmarshal(run.BindingJSON, &binding); err != nil {
			return productionImportRunReceiptsOwnerIdentity
		}
		frozen := binding.SourceManifest.ImportSlot
		if frozen.RunID != slot.RunID || frozen.Stage != slot.Stage || frozen.BatchID != slot.BatchID {
			return productionImportRunReceiptsOwnerSlotChanged
		}
		return nil
	}
}

// productionImportRunReceiptsOwnerFact is one declaration the audit above quotes,
// with the file and line this pass last saw it at. The test matches the marker
// with productionSeamContains, because CI formats the source before it runs, so
// the recorded line is a diagnosis rather than an assertion.
type productionImportRunReceiptsOwnerFact struct {
	Path   string
	Line   int
	Marker string
}

// productionImportRunReceiptsOwnerFacts is the audit, as a list. Every row is
// one sentence of the header: the owner's call site and the argument it does not
// pass, the store's callback that carries no connection, the one connection the
// receipt transaction holds, the store's own verdict on a second connection, and
// the reference's three steps.
func productionImportRunReceiptsOwnerFacts() []productionImportRunReceiptsOwnerFact {
	return []productionImportRunReceiptsOwnerFact{
		{"production_open_linux.go", 591, productionImportRunReceiptsOwnerBlockLine},
		{"production_open_linux.go", 627, productionImportRunReceiptsOwnerWiringLine},
		{"production_importrun_residual_linux.go", 247, productionImportRunReceiptsOwnerBinderLine},
		{"production_open_linux.go", 720, productionImportRunReceiptsOwnerLateOwnerLine},
		{"production_generation_begin_linux.go", 133, "func(_ context.Context, modelID string) (generationowner.Profile, bool, error)"},
		{"../../internal/store/importrun_receipts.go", 118, productionImportRunReceiptsOwnerAssertSignature},
		{"../../internal/store/importrun_receipts.go", 987, "return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})"},
		{"../../internal/store/importrun_receipts.go", 1005, "return assertSources(handle, ImportRunGenerationSlot{RunID: runID, Stage: stage, BatchID: batchID})"},
		{"../../internal/store/importrun.go", 948, "func (r *ImportRuns) importRunImmediate(ctx context.Context, fn func(*sql.Conn) error) (retErr error)"},
		{"../../internal/projectfs/author_linux.go", 121, productionImportRunReceiptsOwnerPoolLine},
		{"../../internal/store/character_proposal_pending.go", 37, productionImportRunReceiptsOwnerDeadlockLine},
		{"../../internal/generationowner/binding.go", 63, "func (r *Registry) ReproduceBinding("},
		{"../../internal/generationowner/lease.go", 164, productionImportRunReceiptsOwnerScopeLine},
		{"../../../electron/controllers/generation-controller.ts", 74, "if (!entry || !database?.inTransaction) throw new Error('GENERATION_OWNER_REQUIRED')"},
		{"../../../electron/services/main-generation-owner.ts", 1454, "const run = requireRun(handle)"},
		{"../../../electron/services/main-generation-owner.ts", 1462, "if (!slot || !isDeepStrictEqual(imports.context(run).slot, slot)) throw new Error('GENERATION_IMPORT_EFFECT_SLOT_CHANGED')"},
	}
}

//go:build linux

package main

import (
	"errors"
	"sort"
)

// This file is the import-run receipt pair's SERVE DECISION. It answers the
// question the previous pass left open - can `db:import-run-effect-receipt-prepare`
// and `db:import-run-effect-receipt-commit` now be served from the store's own
// read plus generationowner.ReproduceBinding, and can
// `db:import-run-prepare-inspection` be served now that the picker is live - and
// it records the answer per name with the declaration that decides it.
//
// The answer is still NO for all three, but not for the reason the seam used to
// give. Two of that reason's claims are now FALSE and are corrected here:
//
//	claim 1: "the source-currency half cannot be reproduced from a row"
//	         FALSE for the fingerprint itself. generationowner.ReproduceBinding
//	         (internal/generationowner/binding.go:63) rebuilds a binding from the
//	         CURRENT sources for the run's OWN scope and epoch without the
//	         live-session `Current` check (internal/generationowner/lease.go:159:
//	         "resolveReproduce is resolve minus the live-session currency check").
//	         That is the half that looked impossible: the reference's
//	         requireRun(handle) is called with execution=false
//	         (electron/services/main-generation-owner.ts:1454), and the epoch
//	         comparison there is gated on `execution`
//	         (electron/services/main-generation-owner.ts:164), so the
//	         reference's assert never compares the run's epoch with the live one.
//	         The "needs a live lease check" objection to the assert is therefore
//	         dead, exactly as the batch said.
//	claim 2: "the store never reads generation_runs, binding_json or an importSlot"
//	         FALSE as a statement about the store. internal/store/
//	         character_proposal_pending.go:545 reads
//	         "FROM generation_runs WHERE run_id=?" for a handle out of an
//	         import_run_receipts payload and then compares projectId, rootActionId,
//	         operation and the whole {runId,stage,batchId} importSlot against the
//	         row (:546-575). Steps 2 and 3 of the assert are already store code.
//	         It is TRUE only of internal/store/importrun.go and
//	         internal/store/importrun_lifecycle.go, which is what the earlier
//	         wording should have said.
//
// What survives, and why each name still cannot be served:
//
//   - ReproduceBinding is a method on *generationowner.Registry and still
//     RESOLVES A LEASE RECORD inside that registry
//     (internal/generationowner/lease.go:162-163: the `r.records[id]` look-up,
//     then MODEL_EXECUTION_LEASE_INVALID when it is absent). The
//     reference does not do that: currentModelReceipt derives a read-only
//     receipt from the CURRENT model profile
//     (electron/services/main-generation-owner.ts:303-307), no lease lookup at
//     all. So ReproduceBinding substitutes "a lease this process minted" for
//     "the current model profile", and no lane in this module mints an import
//     run's lease.
//   - The registry, the model-profile loader and the frozen-source readers all
//     belong to the one owner production_open_linux.go:591 creates as a local
//     variable. The receipt lane is constructed at production_open_linux.go:627
//     with `open.durable` only, so it can reach none of them, and the profile
//     loader itself is a closure over the private model list
//     (production_generation_begin_linux.go:134).
//   - The assert is invoked from INSIDE the receipt's own BEGIN IMMEDIATE on the
//     one reserved connection (internal/store/importrun_receipts.go:987 and :1005
//     are called from importRunImmediate, internal/store/importrun.go:948). A
//     package-main prover holding the borrowed *sql.DB would take a second
//     connection while that transaction holds the only one; the store already
//     names that shape "a deadlock rather than a proof"
//     (internal/store/character_proposal_pending.go:34-37). The proof therefore
//     has to be store-native, and the store cannot reach a Registry.
//   - No import run can exist in this lane at all: the generation ledger refuses
//     the `importSlot` manifest key for EVERY operation
//     (internal/store/generation_ledger.go:505 with glFamilyManifestAllowed
//     :426 admitting graph and finalization keys only), and glPlanning (:492)
//     refuses an "import-blueprints" operation outright. The probe in
//     production_import_finalization_generation_linux_test.go:326-364 asserts
//     exactly that refusal for all three import operations - the ledger did NOT
//     widen to admit import-run keys. The widening the batch remembers covers
//     graphGenerationContext and the finalization slot, not this family.
//   - The import material's own capture, `captureImportGenerationContext`
//     (electron/services/import-generation-source.ts:20, which the manifest
//     comparison depends on), still has no Go port.
//
// So the honest verdict stays: without a generation owner in this lane there is
// no binding fingerprint, and a fail-closed proof installed anyway would refuse
// every real request twice over while a nil proof would let prepare write a
// receipt commit must refuse (internal/store/importrun_receipts.go:983-985
// against :1000-1004). Neither is a served operation.
//
// db:import-run-prepare-inspection has its own, unrelated blocker, re-checked in
// the seam file: the served `dialog:select-novel-files` still answers only
// inspectionId, purpose, the counts, the display names and the eight-chapter
// preview (internal/transport/import_selection.go:112-116). importsource.Summary
// does carry a keyed previewCAS (internal/importsource/inspection.go:49) and
// Consume still demands it (:165-167), but the picker's response drops it, so
// the value the CAS exists to prove would have to be invented by the handler.
//
// Nothing in this file binds a name. The lane's served count stays 255
// (production_served_count_linux_test.go:24); this file only makes the three
// refusals a checked decision instead of a paragraph.

// productionImportRunReceiptServeDecision is one name's verdict: the operation,
// whether this lane installs a handler for it, and the symbolic code naming why
// not. Served is a plain bool rather than a code so a future pass that serves a
// name has to flip the field and remove the seam row in the same change, which
// productionImportRunReceiptServeAgrees enforces at startup.
type productionImportRunReceiptServeDecision struct {
	Operation string
	Served    bool
	Reason    string
}

// The two reason codes. They are this file's own vocabulary, in the symbolic
// shape this lane publishes, and they are never returned to a renderer: the
// renderer only ever sees GO_OPERATION_NOT_MIGRATED from an unbound name
// (internal/transport/server.go:111-116). They exist so the startup check and the
// test can name a decision without parsing a sentence.
const (
	// productionImportRunReceiptSourceProofUnowned is prepare and commit: the
	// fingerprint rebuild needs the generation owner's registry, and this lane is
	// built with the durable open alone.
	productionImportRunReceiptSourceProofUnowned = "IMPORT_RUN_RECEIPT_SOURCE_PROOF_UNOWNED"
	// productionImportRunReceiptInspectionUnreachable is prepare-inspection: the
	// inspection store is not reachable from this adapter and the only served
	// mint drops the preview CAS.
	productionImportRunReceiptInspectionUnreachable = "IMPORT_RUN_PREPARE_INSPECTION_UNREACHABLE"
)

// productionImportRunReceiptServeDecisions is the whole answer of this file, in
// the order the seam table reports the three names. The operation strings are the
// seam file's own constants, so the decision and the refusal cannot spell a name
// two different ways.
func productionImportRunReceiptServeDecisions() []productionImportRunReceiptServeDecision {
	return []productionImportRunReceiptServeDecision{
		{Operation: productionImportRunReceiptsPrepareOperation, Served: false,
			Reason: productionImportRunReceiptSourceProofUnowned},
		{Operation: productionImportRunReceiptsCommitOperation, Served: false,
			Reason: productionImportRunReceiptSourceProofUnowned},
		{Operation: productionImportRunReceiptsInspectionOperation, Served: false,
			Reason: productionImportRunReceiptInspectionUnreachable},
	}
}

// productionImportRunReceiptServedOperations is the set this file binds: empty
// today, derived from the table rather than written by hand so a flipped Served
// field is visible here without a second edit. It is sorted, because a caller
// that logs it should get one stable line.
func productionImportRunReceiptServedOperations() []string {
	decisions := productionImportRunReceiptServeDecisions()
	served := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		if decision.Served {
			served = append(served, decision.Operation)
		}
	}
	sort.Strings(served)
	return served
}

// productionImportRunReceiptServeAgrees checks the two tables against each other.
// A refused name must have a seam row (or its refusal has no documented reason)
// and a served name must NOT have one (or the seam would be a stale note about an
// operation this lane answers). It is called from the seam binder at startup and
// from the test with fabricated tables, so both directions are exercised without
// editing a constant.
func productionImportRunReceiptServeAgrees(decisions []productionImportRunReceiptServeDecision,
	seams []productionImportRunReceiptSeam) error {
	seamByOperation := make(map[string]bool, len(seams))
	for _, seam := range seams {
		seamByOperation[seam.Operation] = true
	}
	decided := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if decision.Operation == "" {
			return errors.New("IMPORT_RUN_RECEIPT_DECISION_INVALID")
		}
		if decided[decision.Operation] {
			return errors.New("IMPORT_RUN_RECEIPT_DECISION_DUPLICATE")
		}
		decided[decision.Operation] = true
		if !decision.Served && !seamByOperation[decision.Operation] {
			return errors.New("IMPORT_RUN_RECEIPT_DECISION_UNSEAMED")
		}
		if decision.Served && seamByOperation[decision.Operation] {
			return errors.New("IMPORT_RUN_RECEIPT_DECISION_STILL_SEAMED")
		}
		if decision.Served && decision.Reason != "" {
			return errors.New("IMPORT_RUN_RECEIPT_DECISION_SERVED_REASON")
		}
		if !decision.Served && !productionImportRunReceiptSeamSymbolic(decision.Reason) {
			return errors.New("IMPORT_RUN_RECEIPT_DECISION_REASON_INVALID")
		}
	}
	for _, seam := range seams {
		if !decided[seam.Operation] {
			return errors.New("IMPORT_RUN_RECEIPT_SEAM_UNDECIDED")
		}
	}
	return nil
}

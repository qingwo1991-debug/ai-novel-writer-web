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
// What still decides the answer, and what does not:
//
//   - ReproduceBinding is no longer blocked on a lease being absent: an import
//     run begun in this process mints one (registry.Begin, called by
//     syntheticGeneration.openRun), and resolveReproduce re-derives the revision
//     from the CURRENT model profile through LoadModel before it answers
//     (internal/generationowner/lease.go:164), so it reads the profile rather
//     than substituting for it. What it adds beyond the reference's rebuild is
//     the record's own lifetime (the registry's four-hour TTL and process
//     memory) and an exact scope equality. That is a narrow divergence in a
//     commit that arrives hours after its begin, and it is recorded here rather
//     than claimed as a blocker.
//   - The registry, the model-profile loader and the frozen-source readers all
//     belong to the one owner production_open_linux.go:591 creates. The receipt
//     lane is constructed at production_open_linux.go:627 - from INSIDE that
//     block, so the owner is in scope there - with `open.durable` only. The fix
//     is one argument at :627 (and one at
//     production_importrun_residual_linux.go:247), not a move; this pass may not
//     edit that file, so the shipped lane still cannot be served.
//   - The connection is the second blocker, and no wiring removes it. The assert
//     is invoked from INSIDE the receipt's own BEGIN IMMEDIATE, which holds the
//     author database's only connection (internal/projectfs/author_linux.go:121
//     opens the pool with SetMaxOpenConns(1)); the store's own comment calls a
//     prover that takes a second connection a deadlock rather than a proof
//     (internal/store/character_proposal_pending.go:37). The callback type
//     (internal/store/importrun_receipts.go:118) carries (handle, slot) and
//     nothing else, so the reference's FIRST step - requireRun's read at
//     electron/services/main-generation-owner.ts:1454 - cannot run. Store-native
//     composition of a receipt and a projection on that connection is proven
//     (:27-37); a binding reproduction is not a projection.
//   - An import-bound generation run CAN exist, and this bullet used to say the
//     opposite. The ledger admits importSlot, importSlotKey and importContext for
//     import-global-facts and analyze-writing-style
//     (internal/store/generation_ledger.go's glFamilyManifestAllowed import
//     case), generation:begin freezes all three
//     (production_import_generation_serve_linux.go:288 and :429), and the probe at
//     production_import_finalization_generation_linux_test.go:326-364 now asserts
//     that acceptance instead of a refusal.
//   - The import material's own capture is ported too: captureImportGenerationContext
//     (electron/services/import-generation-source.ts:20) is
//     productionImportGenerationFreeze
//     (production_import_generation_serve_linux.go:429).
//
// So the honest verdict stays, for the two reasons above rather than for the ones
// this file used to give: the lane cannot be handed the owner without editing a
// file outside this pass, and the assert cannot read the run inside the
// transaction it is invoked in. A fail-closed proof installed anyway would refuse
// every real request twice over, a nil proof would let prepare write a receipt
// commit must refuse (internal/store/importrun_receipts.go:983-985 against
// :1000-1004), and an unwired owner-bound binder would be dead code. None is a
// served operation. production_importrun_receipts_owner_linux.go records the
// design the next pass needs: one store read on the receipt's own connection, and
// the one argument at production_open_linux.go:627.
//
// db:import-run-prepare-inspection has its own, unrelated blocker, re-checked in
// the seam file: the served `dialog:select-novel-files` still answers only
// inspectionId, purpose, the counts, the display names and the eight-chapter
// preview (internal/transport/import_selection.go:112-116). importsource.Summary
// does carry a keyed previewCAS (internal/importsource/inspection.go:49) and
// Consume still demands it (:165-167), but the picker's response drops it, so
// the value the CAS exists to prove would have to be invented by the handler.
//
// Nothing in this file binds a name. The lane's served count stays 273
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

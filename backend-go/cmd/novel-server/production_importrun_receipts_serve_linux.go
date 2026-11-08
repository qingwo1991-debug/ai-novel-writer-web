//go:build linux

package main

import (
	"errors"
	"sort"
)

// This file is the import-run family's SERVE DECISION: one row per name the
// family's seam used to hold, with the answer and, for a refusal, the symbolic
// reason. It is read by the seam binder at startup
// (production_importrun_receipts_linux.go Bind, through
// productionImportRunReceiptServeAgrees), so a decision that no longer matches the
// seam table stops the lane instead of leaving a stale note behind.
//
//	db:import-run-effect-receipt-prepare   SERVED  productionImportRunReceiptsServe2
//	db:import-run-effect-receipt-commit    SERVED  productionImportRunReceiptsServe2
//	db:import-run-prepare-inspection       refused the inspection CAS never reaches the wire
//
// # Why the pair flipped
//
// The previous pass answered NO for both, for two reasons it had measured: the
// assert ran inside the receipt's BEGIN IMMEDIATE holding the author database's
// only connection (internal/projectfs/author_linux.go:121 SetMaxOpenConns(1)) with
// no way to read the run, and the generation owner was never handed to the
// receipt binder. Both are closed now, and neither closure weakens the proof:
//
//   - The store reads the run on the receipt transaction's own connection and
//     hands it to the assert as data (internal/store/importrun_receipts.go:130,
//     internal/store/importrun_receipts_generation.go). The prover still refuses an
//     absent, foreign, re-slotted or non-reproducing run with the reference's own
//     codes; it simply no longer opens a connection to see it.
//   - production_importrun_receipts_serve2_linux.go is an OWNER-BOUND binder, the
//     shape four sibling families already use from the same block
//     (newProductionFinalizationServe(owner), newProductionLegacyRosterServe(owner),
//     newProductionAgentGenerationServe(owner), newProductionImportGenerationServe(owner)).
//     generationowner.ReproduceBinding needs that owner's registry
//     (internal/generationowner/binding.go:63): the lease record the begin minted is
//     process memory, so it cannot be rebuilt from a durable open, and no binder
//     that receives only `open.durable` can serve these names.
//
// The one wiring line is reported, not hidden:
//
//	production_open_linux.go, inside the owner block that opens with
//	"if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {",
//	directly after newProductionImportGenerationServe(owner).Bind(server) and before
//	the block's closing "} else {":
//	  newProductionImportRunReceiptsServe2(owner).Bind(server)
//
// With it the served count moves from 274 to 276; without it both names stay at
// GO_OPERATION_NOT_MIGRATED, because the binder installs nothing when it has no
// owner. That is the reason the two decisions say "served" while this lane can
// still answer 501: the binder exists and is the family's answer, and its gate is
// the owner, not a missing proof. A fail-closed handler would have been the wrong
// substitute - it would refuse every real request twice over - and so would a nil
// proof, which lets prepare accept a handle commit would refuse
// (internal/store/importrun_receipts.go:1002 against :1029).
//
// # The one refusal
//
// db:import-run-prepare-inspection keeps its own blocker, re-checked in the seam
// file: the served `dialog:select-novel-files` still answers only inspectionId,
// purpose, the counts, the display names and the eight-chapter preview
// (internal/transport/import_selection.go:112-116). importsource.Summary does carry
// a keyed previewCAS (internal/importsource/inspection.go:49) and Consume still
// demands it (:165-167), but the picker's response drops it, so the value the CAS
// exists to prove would have to be invented by the handler.

// productionImportRunReceiptServeDecision is one name's verdict: the operation,
// whether this family installs a handler for it, and the symbolic code naming why
// not. Served is a plain bool rather than a code so a future pass that serves a
// name has to flip the field and remove the seam row in the same change, which
// productionImportRunReceiptServeAgrees enforces at startup.
type productionImportRunReceiptServeDecision struct {
	Operation string
	Served    bool
	Reason    string
}

// productionImportRunReceiptInspectionUnreachable is the one reason code left. It
// is this file's own vocabulary, in the symbolic shape this lane publishes, and it
// is never returned to a renderer: the renderer only ever sees
// GO_OPERATION_NOT_MIGRATED from an unbound name
// (internal/transport/server.go:111-116). It exists so the startup check and the
// test can name a decision without parsing a sentence.
const productionImportRunReceiptInspectionUnreachable = "IMPORT_RUN_PREPARE_INSPECTION_UNREACHABLE"

// productionImportRunReceiptServeDecisions is the whole answer of this file, in
// the order the family reports its names. The operation strings are the seam
// file's own constants, so the decision and the refusal cannot spell a name two
// different ways.
func productionImportRunReceiptServeDecisions() []productionImportRunReceiptServeDecision {
	return []productionImportRunReceiptServeDecision{
		{Operation: productionImportRunReceiptsPrepareOperation, Served: true},
		{Operation: productionImportRunReceiptsCommitOperation, Served: true},
		{Operation: productionImportRunReceiptsInspectionOperation, Served: false,
			Reason: productionImportRunReceiptInspectionUnreachable},
	}
}

// productionImportRunReceiptServedOperations is the set this file answers for,
// derived from the table rather than written by hand so a flipped Served field is
// visible here without a second edit. It is sorted, because a caller that logs it
// should get one stable line.
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
// operation this family answers). It is called from the seam binder at startup and
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

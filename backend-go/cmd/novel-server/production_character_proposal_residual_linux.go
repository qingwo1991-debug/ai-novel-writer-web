//go:build linux

package main

// The residual record of `character-proposal`, rewritten after the re-test of the
// claim that used to keep its last two names unbound.
//
// production_character_proposals_linux.go binds stage, read, approve and cancel
// (:131-148) and leaves list-pending-finalized and read-pending-finalized unbound
// (web/operations.json:58-59). This file used to say they were refused because the
// receipt and the projection "cannot be composed in one transaction": package main
// can only begin a DEFERRED transaction, the family's reader and writer take a
// *sql.DB, and the per-row read went through the module's unexported rrQueryer.
//
// That claim is wrong, and the re-test retires it. Package store composes a receipt
// with a projection of character_identity_proposals inside one BEGIN IMMEDIATE
// three times, on the connection that transaction reserved:
//
//	internal/store/summary.go:681
//	  err := NewReviewRevisions(s.db).immediate(ctx, func(conn *sql.Conn) error {
//	internal/store/summary.go:519
//	  rows, err := rrMaps(ctx, conn, "SELECT source_key,source_hash,raw_value FROM character_identity_proposals WHERE proposal_id=?", ...)
//	internal/store/finalized_draft_import.go:900
//	  err = NewReviewRevisions(r.db).immediate(ctx, func(conn *sql.Conn) error {
//	internal/store/finalized_draft_import.go:903
//	  existing, present, err := finalizedImportReadOperation(ctx, conn, operationID)
//	internal/store/character_identity.go:421
//	  proposals, err := rrMaps(ctx, conn, "SELECT resolved_character_id,... FROM character_identity_proposals WHERE proposal_id=?", ...)
//
// The unexported immediate handle (internal/store/reviewrevision.go:41) is not a
// wall around package store: a new exported method inside the package calls it the
// way FinalizedImportCommit does. The unexported rrQueryer
// (internal/store/reviewrevision.go:95) is not a wall either: the *sql.Conn that
// handle hands over satisfies it, which is how summary.go:519 reads a whole proposal
// envelope on the transaction connection. Neither the transaction nor a missing
// *sql.Tx twin keeps these two names unbound.
//
// What does is the one thing the re-test did not move: the proof. readEnvelope
// re-proves the source before it serves a row (production_character_proposals_linux.go
// :1704-1716) and openly refuses every kind this family cannot prove with
// CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE (:1800). For the two kinds these names
// page, the proof entry points have no Go port - proveFinalizedCharacterGeneration /
// normalizeFinalizedCharacterProposalSource for finalized-generation, and
// importCharacterEntriesFromArtifacts / decodeImportInferenceJson for import - and
// the comparison is byte-exact, so an approximate port would turn every valid row
// into a false CHARACTER_PROPOSAL_SOURCE_CHANGED.
//
// production_character_proposal_pending_linux.go carries the sharpened half: the two
// missing entry points, the gate that refuses them today, and the writers that leave
// a served list with nothing to page. This file keeps the verdict, the retraction
// evidence and the seam list, so the family has one answer rather than two
// disagreeing notes.

// productionCharacterProposalResidualVerdict is the one sentence this record exists
// to keep true. It names the retraction and the surviving blocker, so the sentence
// cannot be read as the old transactional claim and cannot outlive either fact: the
// test asserts the cited lines are still the ones below.
const productionCharacterProposalResidualVerdict = "the transactional refusal is retracted - internal/store/summary.go:681 already composes a BEGIN IMMEDIATE with the character_identity_proposals read on the same connection at :519 - and the two names stay unbound for the proof instead: proveFinalizedCharacterGeneration (finalized-character-generation-proof.ts:13) and importCharacterEntriesFromArtifacts (generation-character-proposal-proof.ts:16) have no Go port, so readEnvelope cannot re-prove either source kind these names are defined to page"

// productionCharacterProposalResidualRetraction is the evidence that retires the old
// claim: three store call sites that already open the module's one immediate handle
// and read or write character_identity_proposals on the connection it reserved, and
// the two links that show the receipt being written and read back inside one such
// transaction. Every line is re-read by the test, so exporting or moving the handle
// is a test failure rather than a silent change of facts.
func productionCharacterProposalResidualRetraction() []productionCharacterProposalSeamLine {
	return []productionCharacterProposalSeamLine{
		{
			Path: "../../internal/store/summary.go", Line: 681,
			Marker: "immediate(ctx, func(conn *sql.Conn) error",
		},
		{
			Path: "../../internal/store/summary.go", Line: 519,
			Marker: "FROM character_identity_proposals",
		},
		{
			Path: "../../internal/store/finalized_draft_import.go", Line: 900,
			Marker: "immediate(ctx, func(conn *sql.Conn) error",
		},
		{
			Path: "../../internal/store/finalized_draft_import.go", Line: 903,
			Marker: "finalizedImportReadOperation(ctx, conn, operationID)",
		},
		{
			Path: "../../internal/store/character_identity.go", Line: 421,
			Marker: "FROM character_identity_proposals",
		},
	}
}

// productionCharacterProposalResidualSeams answers the two names this record still
// refuses. It is a view of productionCharacterProposalPendingProofSeams rather than
// a second table: the family's refusal has one list, and a retraction that kept its
// own copy could drift into disagreeing with the record it points at.
func productionCharacterProposalResidualSeams() []productionCharacterProposalSeam {
	return productionCharacterProposalPendingProofSeams()
}

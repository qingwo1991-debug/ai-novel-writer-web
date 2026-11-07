//go:build linux

package main

// The two `character-proposal` names this family still cannot serve, re-tested
// against the code instead of against the note that used to refuse them.
//
//	character-proposal:list-pending-finalized  web/operations.json:58  refused here
//	character-proposal:read-pending-finalized  web/operations.json:59  refused here
//
// # The reference
//
//	electron/controllers/generation-controller.ts:213-214
//	  register('character-proposal:list-pending-finalized', 0, owner => owner.characterProposals.listPendingFinalized())
//	  register('character-proposal:read-pending-finalized', 1, (owner, request) => owner.characterProposals.readPendingFinalized(request.proposalBatchId))
//	electron/services/character-proposal-service.ts:131-154
//	  :131 listPendingFinalized, :149 readPendingFinalized, :60 readEnvelope, :103 resolveItems
//	  :136-138 the list keeps a row only when it is pending AND its provenance carries a source
//	  :146 a row whose envelope does not re-prove is skipped, not listed
//	  :151-153 the read is read() plus the pending-finalized guard
//	src/shared/character-proposal.ts:77-78
//	  the list answers PendingFinalizedCharacterProposalSummary[], the read answers CharacterProposalBatch
//	src/components/characters/use-pending-character-proposals.ts:30, :47
//	  the only consumer: page the summaries, then read one by its proposalBatchId
//
// The list pages character_identity_proposals by source_key
// (`character-proposal-v1:<projectId>`) in proposal_id order (:132-133) and re-reads
// every row through readEnvelope; the read is the ordinary `read` (:114) plus the
// two-field guard (:151-153). Neither body can carry a scope: the list takes no
// request at all (wire :77), the read takes exactly `{ proposalBatchId }` (:78).
// In this lane that would mean the frozen context as the list's only argument and
// as the read's second one, borrowed through the same
// productionDurableOpen.borrowAuthorDatabase (production_durable_linux.go:313) the
// four served names already use (production_character_proposals_linux.go:572).
//
// # What the earlier record got wrong
//
// production_character_proposal_residual_linux.go used to say that the receipt and
// the projection "cannot be composed in one transaction", because the module's only
// BEGIN IMMEDIATE handle is unexported (internal/store/reviewrevision.go:41) and the
// family's reader and writer take a *sql.DB. The first half is true and irrelevant;
// the second is false. Package store composes exactly that composition three times
// today:
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
// An unexported method is not a wall around package store: a new exported method
// inside the package calls it exactly the way FinalizedImportCommit does, and the
// unexported rrQueryer (reviewrevision.go:95) is satisfied by the *sql.Conn that
// handle hands over - which is why summary.go:519 can read a whole proposal
// envelope on the very connection its BEGIN IMMEDIATE reserved. So a store method
// that returns the receipt row (source_key, source_hash) together with the
// projection re-resolved inside the same transaction is a few lines, not a
// redesign. The transaction is not the obstacle.
//
// # What is
//
// readEnvelope re-proves the source before it serves anything (:69-79), and this
// lane has no port for either kind these two names page. That matters twice over,
// because the proof is the one thing that would have to run *inside* the transaction:
// the family's prover (productionCharacterProposalSourceProof,
// production_character_proposals_linux.go:1794) takes a *sql.DB and drives the
// generation ledger on the pool, so handing it to a store method's immediate
// transaction would make it a second, unlocked read after the receipt - the exact
// shape this family must not serve. A store method alone therefore does not produce a
// servable pair; the proof has to be able to run on the transaction connection first.
//
// And there is nothing to run, for either kind:
//
//   - finalized-generation needs proveFinalizedCharacterGeneration
//     (electron/services/finalized-character-generation-proof.ts:13) and
//     normalizeFinalizedCharacterProposalSource (:66). The two ends of that chain
//     do exist in Go - internal/characterstate.ParseResponse
//     (internal/characterstate/state_response.go:307) and
//     store.ReadFinalizedCharacterContextTx
//     (internal/store/finalized_character_context.go:210) - but nothing joins them:
//     the author input named 'finalized-character-context', its hash against
//     sourceManifest.finalizedCharacterContextHash, selectedFinalizedDraftIds,
//     modelReceipt.modelRevision and sourceRefs are keys no begin in this lane
//     writes, which is what production_finalized_character_commit_linux.go:97-102
//     already records for the sibling commit.
//   - import needs importCharacterEntriesFromArtifacts
//     (electron/services/generation-character-proposal-proof.ts:16), which decodes
//     the settled import-inference artifact through decodeImportInferenceJson
//     (src/services/workflows/commands/import-inference-contract.ts:257). No Go
//     package ports that decoder - internal/store reads the receipt
//     (import_global_fact_operations) but never the artifact chain - so the
//     `generated` provenance arm of the import proof cannot be produced here.
//
// That gap is not repairable by approximation. readEnvelope re-encodes the stored
// proof and the fresh one and refuses unless they are equal
// (production_character_proposals_linux.go:1708-1716), so a port that differs by one
// byte - a dropped unknown key, another number formatting - would answer
// CHARACTER_PROPOSAL_SOURCE_CHANGED for every valid row a desktop wrote, and the
// only fixture that can tell a right port from a wrong one is an envelope produced
// by the desktop's own encoder.
//
// A served list would also be empty on every database this lane writes. The list
// keeps a row only when its provenance carries a source (:136-138), and of the four
// writers of character_identity_proposals
//
//	internal/store/dbmaintenance.go:669            import, with "provenance": nil (:655)
//	internal/store/finalization_outbox.go:342      the fcs: freeze receipt, not a proposal
//	production_character_proposals_linux.go:691    generation/architecture
//	internal/admission/upgrade_v2.go:463           the M02 schema upgrade, which copies
//	                                               rows an older database already had
//
// the two that stage a proposal write architecture-generation and import, never the
// finalized-generation kind, and the import writer always writes the null provenance
// the list skips. The upgrade can copy a finalized-generation row an older desktop
// wrote, which is exactly the row this lane cannot re-prove. So the emptiness is
// structural rather than a missing fixture: this lane's import request
// (internal/store/dbmaintenance.go:393-398) carries no generationRunHandle, which is
// exactly the reference arm that answers provenance null
// (generation-character-proposal-proof.ts:90).
//
// # The decision
//
// Both names stay unbound. The reason is the proof, not the transaction: a binder
// installed today could only page the architecture batches this lane stages - which
// both names are defined to refuse - and would have to drop the proof gate to page
// anything else. productionCharacterProposalPendingProofSeams names the ports, and
// productionCharacterProposalPendingEnvelopeWriters holds the writer fact, so a
// future pass can re-test this record instead of trusting it.
//
// What would lift it: one Go port of each proof chain above, plus a writer able to
// stage a finalized-generation or provenance-bearing import proposal. The store half
// is the small part, and
// productionCharacterProposalResidualRetraction
// (production_character_proposal_residual_linux.go) cites the existing lines that
// already do it.

// productionCharacterProposalPendingProofSeams is this record's half of the
// family's refusal table: for each name, the unported proof entry point that has to
// land first, the architecture-only gate that refuses both kinds today, and - for
// the list - the writer whose null provenance leaves nothing to page. Each entry is
// a line some other file has to change, and the record's test re-reads every one,
// so this table cannot outlive the code it describes.
func productionCharacterProposalPendingProofSeams() []productionCharacterProposalSeam {
	finalizedProof := productionCharacterProposalSeamLine{
		Path: "../../../electron/services/finalized-character-generation-proof.ts", Line: 13,
		Marker: "export function proveFinalizedCharacterGeneration(",
	}
	finalizedNormalize := productionCharacterProposalSeamLine{
		Path: "../../../electron/services/finalized-character-generation-proof.ts", Line: 66,
		Marker: "export function normalizeFinalizedCharacterProposalSource(",
	}
	importItems := productionCharacterProposalSeamLine{
		Path: "../../../electron/services/generation-character-proposal-proof.ts", Line: 16,
		Marker: "function importCharacterEntriesFromArtifacts(",
	}
	importDecode := productionCharacterProposalSeamLine{
		Path: "../../../src/services/workflows/commands/import-inference-contract.ts", Line: 257,
		Marker: "export function decodeImportInferenceJson(",
	}
	gate := productionCharacterProposalSeamLine{
		Path: "production_character_proposals_linux.go", Line: 1800,
		Marker: "productionCharacterProposalSourceProofUnavailable",
	}
	writer := productionCharacterProposalSeamLine{
		Path: "../../internal/store/dbmaintenance.go", Line: 655,
		Marker: `"provenance": nil`,
	}
	return []productionCharacterProposalSeam{
		{
			Operation: productionCharacterProposalListPendingName,
			Missing: []productionCharacterProposalSeamLine{
				finalizedProof, finalizedNormalize, importItems, importDecode, gate, writer,
			},
		},
		{
			Operation: productionCharacterProposalReadPendingName,
			Missing: []productionCharacterProposalSeamLine{
				finalizedProof, finalizedNormalize, importItems, importDecode, gate,
			},
		},
	}
}

// productionCharacterProposalPendingEnvelopeWriter is one writer of the
// character_identity_proposals table. Kind is the source kind (or row family) it
// stores, and Provenance is the proof provenance it stores beside it: `nil` for a
// row the pending list cannot keep, `generated` for the architecture arm, `none`
// for a row that is not a proposal at all, and `stored` for a migration that copies
// what it found. ProvenanceLine is where that provenance is spelled, so the test can
// prove it belongs to this writer's own function instead of trusting a field name.
// Together they are the writer fact: no writer in this lane stages a
// finalized-generation proposal, and no import writer produces the provenance the
// list requires.
type productionCharacterProposalPendingEnvelopeWriter struct {
	Path           string
	Line           int
	Marker         string
	Kind           string
	Provenance     string
	ProvenanceLine int
}

// productionCharacterProposalPendingEnvelopeWriters is every INSERT into
// character_identity_proposals in this module, checked against
// internal/admission/baseline_v7_ddl.go:130. The record's test fails when a fifth
// appears, which is the only way a new writer could quietly change the list's
// answer.
func productionCharacterProposalPendingEnvelopeWriters() []productionCharacterProposalPendingEnvelopeWriter {
	return []productionCharacterProposalPendingEnvelopeWriter{
		{
			Path: "../../internal/store/dbmaintenance.go", Line: 669,
			Marker: "INSERT INTO character_identity_proposals",
			Kind:   "import", Provenance: "nil", ProvenanceLine: 655,
		},
		{
			Path: "../../internal/store/finalization_outbox.go", Line: 342,
			Marker: "INSERT INTO character_identity_proposals",
			Kind:   "finalized-character-snapshot", Provenance: "none",
		},
		{
			Path: "production_character_proposals_linux.go", Line: 691,
			Marker: "INSERT INTO character_identity_proposals",
			Kind:   "generation", Provenance: "generated",
		},
		{
			Path: "../../internal/admission/upgrade_v2.go", Line: 463,
			Marker: "INSERT INTO character_identity_proposals",
			Kind:   "migration", Provenance: "stored",
		},
	}
}

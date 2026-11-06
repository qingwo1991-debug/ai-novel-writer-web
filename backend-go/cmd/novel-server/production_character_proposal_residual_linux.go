//go:build linux

package main

// The residual record of `character-proposal`, and a re-examination of the one
// claim that keeps its last two names unbound.
//
// production_character_proposals_linux.go binds stage, read, approve and cancel
// (:131-148) and refuses list-pending-finalized and read-pending-finalized with the
// seam table at :2282. That table's shape is not in question here: it cites the
// unported source proof and the store read the two names would page through. What
// it asserts in prose, and what this file re-examines, is the transactional claim -
// that "the receipt and the projection cannot be composed in one transaction", i.e.
// that a single store call could not write the receipt row and read the proposal
// envelope back inside one BEGIN IMMEDIATE.
//
// The claim survives the re-examination, but not for the reason the prose implies.
// Package main CAN open a transaction on the borrowed author database
// (db.BeginTx), and the adapter already issues its own proposal SQL
// (production_character_proposals_linux.go:691). The composition is refused by the
// exported surface, in three separately checkable ways:
//
//  1. The module's one immediate-transaction handle is unexported:
//     internal/store/reviewrevision.go:41 is `(*ReviewRevisions).immediate`, the
//     only place that reserves a *sql.Conn and issues BEGIN IMMEDIATE. A caller in
//     package main can begin a DEFERRED transaction, which the module's write
//     discipline exists to avoid: the first write upgrades the read snapshot and
//     SQLite may answer SQLITE_BUSY instead of waiting. Every write in this store
//     is immediate for that reason.
//  2. The read and write halves of this family take a *sql.DB, never a transaction:
//     productionCharacterProposalReadEnvelope (:1667) and
//     productionCharacterProposalWrite (:1762), and the envelope's per-row read
//     goes through the unexported rrQueryer interface
//     (internal/store/reviewrevision.go:95). A transaction handle cannot be handed
//     to either. The store already demonstrates the fix - the exported *sql.Tx
//     twins, e.g. ReadFinalizedCharacterContextTx
//     (internal/store/finalized_character_context.go:210) - but the proposal
//     reader and writer have no such twin, so "one transaction" would mean writing
//     both again rather than calling what exists.
//  3. Decisively, a transaction would not be enough either way: both names exist to
//     page the source kinds whose proof is unported. list-pending-finalized and
//     read-pending-finalized enumerate `finalized-generation` and `import` batches,
//     and neither can be proved here - normalizeFinalizedCharacterProposalSource
//     (electron/services/finalized-character-generation-proof.ts:66) turns a
//     finalized context into proposal items, and proveCharacterProposal
//     (electron/services/generation-character-proposal-proof.ts:43) reads the
//     import receipt from import_global_fact_operations (:69). A name that could
//     write and read in one transaction would still answer
//     CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE for every row it is defined to
//     page, which is exactly what the family's stage already refuses with
//     (production_character_proposals_linux.go:1800).
//
// So both names stay unbound. What changes is the record: the transaction handle
// now has a line number instead of an assertion, and a future reader can see that
// the missing piece is an exported immediate-transaction seam plus *sql.Tx twins -
// and that neither would be sufficient on its own.

// productionCharacterProposalResidualVerdict is the one sentence this record exists
// to keep true. The test asserts the line it cites is still the immediate handle, so
// the sentence cannot outlive the fact.
const productionCharacterProposalResidualVerdict = "the two names stay refused because the source proof is unported and because no exported store call composes the receipt and the projection in one transaction: the only BEGIN IMMEDIATE handle is unexported (internal/store/reviewrevision.go:41) and the proposal reader and writer take a *sql.DB (production_character_proposals_linux.go:1667, :1762)"

// productionCharacterProposalResidualSeams is the sharpened half of the family's
// seam record: for each refused name, the transaction-handle evidence and the two
// unported proofs, with the location of each. It reuses the family's own seam type
// (production_character_proposals_linux.go:2240) so there is one shape per family,
// and it does not replace productionCharacterProposalPendingSeams - that table is
// asserted by its own test and stays as the family's answer for what the two names
// would have to page.
func productionCharacterProposalResidualSeams() []productionCharacterProposalSeam {
	immediate := productionCharacterProposalSeamLine{
		Path: "../../internal/store/reviewrevision.go", Line: 41,
		Marker: "func (r *ReviewRevisions) immediate(",
	}
	queryer := productionCharacterProposalSeamLine{
		Path: "../../internal/store/reviewrevision.go", Line: 95,
		Marker: "type rrQueryer interface",
	}
	envelope := productionCharacterProposalSeamLine{
		Path: "production_character_proposals_linux.go", Line: 1667,
		Marker: "func productionCharacterProposalReadEnvelope(",
	}
	write := productionCharacterProposalSeamLine{
		Path: "production_character_proposals_linux.go", Line: 1762,
		Marker: "func productionCharacterProposalWrite(",
	}
	twin := productionCharacterProposalSeamLine{
		Path: "../../internal/store/finalized_character_context.go", Line: 210,
		Marker: "func ReadFinalizedCharacterContextTx(",
	}
	finalizedProof := productionCharacterProposalSeamLine{
		Path: "../../../electron/services/finalized-character-generation-proof.ts", Line: 66,
		Marker: "export function normalizeFinalizedCharacterProposalSource(",
	}
	importProof := productionCharacterProposalSeamLine{
		Path: "../../../electron/services/generation-character-proposal-proof.ts", Line: 43,
		Marker: "export function proveCharacterProposal(",
	}
	importReceipt := productionCharacterProposalSeamLine{
		Path: "../../../electron/services/generation-character-proposal-proof.ts", Line: 69,
		Marker: "import_global_fact_operations",
	}
	// The listing half needs the envelope read; the read half needs the write too,
	// because it stages the row a projection is later composed back from.
	compose := []productionCharacterProposalSeamLine{immediate, queryer, envelope, twin}
	return []productionCharacterProposalSeam{
		{
			Operation: productionCharacterProposalListPendingName,
			Missing:   append(append([]productionCharacterProposalSeamLine{}, compose...), finalizedProof, importProof, importReceipt),
		},
		{
			Operation: productionCharacterProposalReadPendingName,
			Missing:   append(append([]productionCharacterProposalSeamLine{}, compose...), write, finalizedProof, importProof, importReceipt),
		},
	}
}

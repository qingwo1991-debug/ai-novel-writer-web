//go:build linux

package main

// The residual record of `character-proposal`, rewritten after the proof was
// re-tested and both remaining names were served.
//
// production_character_proposals_linux.go binds stage, read, approve and cancel
// (:131-148). The two names it leaves unbound (web/operations.json:58-59) are
// now served by production_character_proposal_pending_linux.go:
//
//	character-proposal:list-pending-finalized  productionCharacterProposalPendingList
//	character-proposal:read-pending-finalized  productionCharacterProposalPendingRead
//
// The old record refused them for two reasons. Both are retracted here, because
// both were about the code rather than about the requirement, and the code moved:
//
//  1. "the receipt and the projection cannot be composed in one transaction".
//     Package store has composed exactly that for as long as the review-revision
//     and finalized-import batches have existed: one BEGIN IMMEDIATE on a
//     reserved *sql.Conn (internal/store/reviewrevision.go:41), with the
//     character_identity_proposals read on that same connection
//     (internal/store/summary.go:519) and the receipt written and read back
//     inside it (internal/store/finalized_draft_import.go:900-903). The new
//     store file opens that same handle. What made a package-main prover
//     unusable was never the missing transaction but the pool: the borrowed
//     author database has one connection
//     (internal/projectfs/author_linux.go:121), so a second transaction opened
//     while this one runs would deadlock.
//
//  2. "the provenance proof is byte-exact and has no Go port". The comparison the
//     reference performs is not byte-exact: readEnvelope compares the stored
//     proof with a re-proved one by value (isDeepStrictEqual,
//     character-proposal-service.ts:69-79), and the only exact string is the
//     sourceHash, one SHA-256 over JSON.stringify of the source and its items.
//     internal/store/character_proposal_pending.go hashes the *stored*
//     document's own re-encoding - parsed with the package's order-preserving
//     decoder (internal/store/character_roster.go:140-199) and emitted by its
//     JSON.stringify-compatible serializer (internal/store/plottree.go:30-49) -
//     so the desktop's key order, and therefore the desktop's hash, is what is
//     reproduced. Both proof chains (proveFinalizedCharacterGeneration and the
//     import arm) are ported there.
//
// What remains refused is narrow and is not one of these two names: the raised
// `generated` arm of the import proof needs decodeImportInferenceJson, which no
// Go package ports, so an import proposal whose effect receipt carries a
// generationRunHandle answers CHARACTER_PROPOSAL_SOURCE_PROOF_UNAVAILABLE
// instead of a guessed provenance.
//
// production_character_proposal_pending_linux.go states the reference chain and
// the wiring line. This file keeps only the retraction evidence, so the family
// has one answer rather than two disagreeing notes.

// productionCharacterProposalResidualVerdict is the one sentence this record
// exists to keep true. It names both retractions and the file that serves the
// names, so it can neither be read as the old transactional claim nor outlive
// the proof port it points at.
const productionCharacterProposalResidualVerdict = "both refusals are retracted - internal/store/summary.go:681 already composes a BEGIN IMMEDIATE with the character_identity_proposals read on the same connection at :519, and internal/store/character_proposal_pending.go re-proves finalized-generation and import sources with the package's own ordered JSON encoder - so production_character_proposal_pending_linux.go serves list-pending-finalized and read-pending-finalized instead of refusing them"

// productionCharacterProposalResidualLine is one line of the retraction
// evidence. The test re-reads every marker in the file it names, so the record
// cannot outlive the declarations it cites.
type productionCharacterProposalResidualLine struct {
	Path   string
	Line   int
	Marker string
}

// productionCharacterProposalResidualRetraction is the evidence that retires the
// old claims: three store call sites that open the module's one immediate handle
// and read or write character_identity_proposals on the connection it reserved,
// the two links that show a receipt written and read back inside one such
// transaction, and the still-unexported handle the new store file keeps using
// rather than exporting.
func productionCharacterProposalResidualRetraction() []productionCharacterProposalResidualLine {
	return []productionCharacterProposalResidualLine{
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
		{
			Path: "../../internal/store/reviewrevision.go", Line: 41,
			Marker: "func (r *ReviewRevisions) immediate(",
		},
	}
}

// productionCharacterProposalResidualServed is every name this record used to
// keep unbound and now expects to be served. The test checks each one against
// the family inventory, so a name that left the inventory fails here instead of
// silently shrinking the claim.
func productionCharacterProposalResidualServed() []string {
	return []string{
		productionCharacterProposalListPendingName,
		productionCharacterProposalReadPendingName,
	}
}

// productionCharacterProposalResidualBinder is the binder line the integrator
// adds to production_open_linux.go beside the family's own binder. The test
// reads this string, so the record and the wiring report are one fact.
const productionCharacterProposalResidualBinder = "func() error { return newProductionCharacterProposalPending(open.durable).Bind(server) },"

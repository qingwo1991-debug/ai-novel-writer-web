//go:build linux

// This file is the re-test of the one legacy-roster name the family still
// refuses, and it is where its seam lives:
//
//	legacy-roster:execute  web/operations.json:267  served (production_legacy_roster_execute_serve_linux.go)
//	legacy-roster:stage    web/operations.json:268  refused (the artifact proof is not derivable)
//
// execute moved here from the refusal list when its two prompts shipped as
// internal/prompttemplate/legacy-roster-loop-v1.json - the length-stop
// replacement prompt (src/shared/structured-replacement-prompt.ts:3-41) and the
// JSON-repair system prompt plus its <invalid-json> wrapper
// (src/shared/legacy-roster-generation-pure.ts:38-39, :50-52). The six-step
// initial/repair loop itself lives in production_legacy_roster_execute_serve_linux.go,
// which records what it dispatches, how it counts attempts and why its candidate
// gate is stricter than the desktop's parse-only settle.
//
// # stage: the adoption that landed is not the proof
//
// stage hands the settled artifact to CharacterProposalService.stage
// (electron/services/legacy-roster-generation.ts:52-68), and every term of it is an
// artifact fact:
//
//	:54  previous = this.saved(run)                   the effect already stamped on an attempt
//	:59  this.artifactFor(run) must equal the request's artifact
//	:60  readLegacyRosterGenerationProof(...)          the artifact proof
//	:61  assertSources(run)                            the frozen binding still reproduces
//	:62  this.proposals.stage(proof.source)            one character_identity_proposals row
//	:64-65 the attempt's usage receipt gains the legacyRosterProposal effect
//	:53/:67 all of it inside one .immediate() transaction
//
// readLegacyRosterGenerationProof (electron/services/legacy-roster-generation-proof.ts:
// 11-45) reads the generation_artifacts row (:21-22), re-hashes the artifact text and
// requires a settled-or-unknown attempt, no failure code and a `stop` finish (:24-25),
// parses the candidate and validates it as a legacy_repair roster (:27-28), builds
// the items (:30-36) and hashes them with the frozen context hash (:39), and
// requires the run's own modelReceipt.modelRevision (:40-41).
//
// The adoption that landed is none of that.
// AdoptLegacyRoster (internal/store/character_roster_adoption.go:73) refreshes the
// identity projection and writes a character_identity_approvals envelope
// {version, kind, request, requestHash, receipt, receiptHash} (:151-164). It creates
// no proposal batch, names no artifact and carries no model revision, so a stage
// built on the adoption projection would answer a proposal the desktop never
// stages. Three ports are still missing, and a fourth fact makes them insufficient
// even when they land:
//
//   - the artifact proof: no Go port of parseLegacyRosterJson,
//     validateLegacyRosterCandidate (electron/repositories/character-roster-repository.ts:
//     307-311, which routes the candidate through normalizeRequest with intent
//     legacy_repair), or the items/sourceHash mapping. The store's roster
//     normalizer is the author one and refuses anything that is not manual_edit
//     (internal/store/character_roster_author.go:736);
//   - the writer: the legacy arm of CharacterProposalService.stage
//     (electron/services/character-proposal-service.ts:161-165, the INSERT at
//     :191-192). internal/store writes character_identity_proposals for the import
//     kind (internal/store/dbmaintenance.go:671) and the finalized-character-snapshot
//     kind (internal/store/finalization_outbox.go:342) only, and the sibling lane in
//     this package serves the architecture kind and refuses a legacy source by name
//     (production_character_proposals_linux.go:268);
//   - the effect: the legacyRosterProposal key
//     (electron/services/legacy-roster-generation.ts:65) has no arm in the ledger's
//     usage allowlist (internal/store/generation_ledger.go:601) nor in
//     glEffectKeyAllowed (:475-502), so the next mutation of that root would be
//     refused GENERATION_EFFECT_UNSUPPORTED;
//   - and no run this lane opened could ever be proved. productionLegacyRosterManifestText
//     freezes modelId and modelLeaseId, not the modelReceipt the proof's last step
//     requires, so GENERATION_LEGACY_PROOF_INVALID is the answer for every artifact
//     a browser run could own. execute now settles artifacts on this lane's runs, but
//     none of them carries the model receipt the proof reads, so the only artifacts
//     stage could accept are a desktop build's, whose proposal rows this lane cannot
//     read back either.
//
// # The wire shape a serve would have to accept
//
// src/shared/legacy-roster-generation.ts:27-28 declares the name as one request
// argument - { handle, artifact } for stage - and this transport appends the frozen
// context, so a serve would take exactly two arguments, the shape execute now reads
// through the family's channel (production_legacy_roster_serve_linux.go) and
// adopt-existing already reads (production_legacy_roster_adopt_linux.go:40-45).
// handle is the four-key MainGenerationRunHandle and artifact is the three-key
// LegacyRosterGenerationArtifact, both of which the proof checks key by key
// (legacy-roster-generation-proof.ts:13-15, :20).
//
// # What this file is, then
//
// It binds nothing. It holds the seam list the refusal is composed from -
// productionLegacyRosterStageFragments, which productionLegacyRosterSeams publishes
// and the lane's seam test re-reads line by line - and nothing else. When a later
// change ships the proof and the writer, the seam test fails first, which is the
// signal to serve stage instead of refusing it.
package main

// The fragments of the reference the refused name is composed from. Each is
// spelled once so the seam table, the header above and the tests cannot drift.
const (
	// The parse and the artifact proof.
	productionLegacyRosterProofFragment     = "readLegacyRosterGenerationProof"
	productionLegacyRosterArtifactFragment  = "generation_artifacts"
	productionLegacyRosterParseFragment     = "parseLegacyRosterJson"
	productionLegacyRosterCandidateFragment = "validateLegacyRosterCandidate"
	productionLegacyRosterModelFragment     = "modelReceipt.modelRevision"

	// The proposal writer's legacy arm.
	productionLegacyRosterStageFragment  = "CharacterProposalService.stage"
	productionLegacyRosterStageLegacy    = "legacy-roster-generation"
	productionLegacyRosterProposalFragment = "character_identity_proposals"

	// The effect stamp and the ledger allowlist that has no arm for it.
	productionLegacyRosterEffectFragment     = "legacyRosterProposal"
	productionLegacyRosterLedgerEffect       = "glEffectKeyAllowed"
	productionLegacyRosterLedgerUsageKeys    = "generation ledger usage keys"
	productionLegacyRosterStageRefusalMarker = "GENERATION_LEGACY_ADMISSION_REQUIRED"
)

// productionLegacyRosterFragment is one term a refused name waits for: the term's
// name and the line that authors it. Path is relative to this package, exactly like
// productionLegacyRosterSeamLine, so productionLegacyRosterSeamProof can re-read it.
type productionLegacyRosterFragment struct {
	Fragment string
	Path     string
	Line     int
	Marker   string
}

// seam is the fragment as the lane's seam table publishes it.
func (f productionLegacyRosterFragment) seam() productionLegacyRosterSeamLine {
	return productionLegacyRosterSeamLine{Path: f.Path, Line: f.Line, Marker: f.Marker}
}

// productionLegacyRosterFragmentSeams converts a fragment list to the seam rows the
// table publishes, so a fragment is spelled once and re-read from its own line.
func productionLegacyRosterFragmentSeams(fragments []productionLegacyRosterFragment) []productionLegacyRosterSeamLine {
	seams := make([]productionLegacyRosterSeamLine, 0, len(fragments))
	for _, fragment := range fragments {
		seams = append(seams, fragment.seam())
	}
	return seams
}

// productionLegacyRosterStageFragments is stage's seam: the artifact proof, the
// proposal writer's legacy arm, the effect stamp and the ledger allowlist that has
// no arm for it. The adoption projection is not among them because it proves
// nothing these terms need.
func productionLegacyRosterStageFragments() []productionLegacyRosterFragment {
	const (
		proof    = "../../../electron/services/legacy-roster-generation-proof.ts"
		proposal = "../../../electron/services/character-proposal-service.ts"
	)
	return []productionLegacyRosterFragment{
		{productionLegacyRosterProofFragment, proof, 11,
			"export function readLegacyRosterGenerationProof("},
		{productionLegacyRosterArtifactFragment, proof, 21,
			"FROM generation_artifacts WHERE artifact_id=?"},
		{productionLegacyRosterParseFragment, proof, 27,
			"parseLegacyRosterJson(artifact.text)"},
		{productionLegacyRosterCandidateFragment, proof, 27,
			"validateLegacyRosterCandidate(candidate)"},
		{productionLegacyRosterModelFragment, proof, 40,
			"run.binding.sourceManifest.modelReceipt"},
		{productionLegacyRosterStageFragment, proposal, 159,
			"stage(source: CharacterProposalSource): CharacterProposalBatch {"},
		{productionLegacyRosterStageLegacy, proposal, 161,
			"if (source?.kind === 'legacy-roster-generation') {"},
		{productionLegacyRosterProposalFragment, proposal, 191,
			"INSERT INTO character_identity_proposals"},
		{productionLegacyRosterEffectFragment, "../../../electron/services/legacy-roster-generation.ts", 65,
			"legacyRosterProposal:effect"},
		{productionLegacyRosterLedgerEffect, "../../internal/store/generation_ledger.go", 475,
			"func glEffectKeyAllowed(key, operation string) bool {"},
		{productionLegacyRosterLedgerUsageKeys, "../../internal/store/generation_ledger.go", 601,
			`case "requestHash", "usagePolicy", "purpose", "replayTask", "budgetDecision", "reconciliationInjected", "artifactIdentity", "result", "draftCommit", "directoryProgress", "visibleComposition":`},
		{productionLegacyRosterStageRefusalMarker, "production_character_proposals_linux.go", 268,
			"GENERATION_LEGACY_ADMISSION_REQUIRED"},
	}
}

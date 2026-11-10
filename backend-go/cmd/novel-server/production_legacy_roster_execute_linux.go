//go:build linux

// This file held the stage re-test seam: productionLegacyRosterStageFragments,
// the fragment type that published it, and the twelve terms the refusal was
// composed from - the artifact proof (readLegacyRosterGenerationProof,
// parseLegacyRosterJson, validateLegacyRosterCandidate,
// modelReceipt.modelRevision), the proposal writer's legacy arm
// (CharacterProposalService.stage, the character_identity_proposals INSERT), the
// legacyRosterProposal effect and the two ledger allowlists that had no arm for
// it (glFamilyManifestAllowed, glEffectKeyAllowed).
//
// stage is now served, so every one of those terms is in place and the list is
// gone rather than left pointing at a name that no longer refuses:
//
//	legacy-roster:execute  web/operations.json:267  served (production_legacy_roster_execute_serve_linux.go)
//	legacy-roster:stage    web/operations.json:268  served (production_legacy_roster_stage_serve_linux.go)
//
// The proof, the proposal write and the recovery projection live in
// internal/store/legacy_roster_proposal.go; the ledger carries both arms
// (internal/store/generation_ledger.go: the "legacyRosterContext" case of
// glFamilyManifestAllowed and the "legacyRosterProposal" case of
// glEffectKeyAllowed). productionLegacyRosterSeams in
// production_legacy_roster_linux.go is the lane's seam table, it now publishes
// nothing, and TestProductionLaneRefusesLegacyRosterNames asserts that. This file
// is a marker only.
package main

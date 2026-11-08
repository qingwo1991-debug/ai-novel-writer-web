//go:build linux

// This file used to hold the stage re-test seam - productionLegacyRosterStageRevisionSymbol,
// productionLegacyRosterStageFact and the two fact lists the refusal turned on.
// stage is now served: the model revision is frozen into the manifest at begin
// (production_legacy_roster_serve_linux.go), the store stages the proposal and
// the effect in one transaction (internal/store/legacy_roster_proposal.go), and
// the handler lives in production_legacy_roster_stage_serve_linux.go. Nothing of
// the refusal note survives, so this file is a marker only.
package main

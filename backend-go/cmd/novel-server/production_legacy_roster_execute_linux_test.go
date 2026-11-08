//go:build linux

package main

// This file held the two stage-refusal re-tests
// (TestProductionLegacyRosterStageSeamIsNotTheAdoptionProjection and
// TestProductionLegacyRosterStageStaysUnbound) and the store-kind scanner they
// used. stage is now served: the real-HTTP success and refusal tests live in
// production_legacy_roster_stage_serve_linux_test.go, and the full-lane unbound
// answer is still pinned by TestProductionLaneRefusesLegacyRosterNames. This
// file is a marker only.

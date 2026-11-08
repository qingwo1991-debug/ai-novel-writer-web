//go:build linux

// This file is the re-test of the one legacy-roster name the family still
// refuses, against the execute serve that landed this session
// (production_legacy_roster_execute_serve_linux.go):
//
//	legacy-roster:stage  web/operations.json:268  refused (the model revision is not frozen)
//
// # What execute changed
//
// execute now dispatches the frozen task through generationowner.OrdinaryService
// and settles one attempt per loop iteration:
//
//	service.Execute(ctx, frozen, handle.RunID, nonce)  (production_legacy_roster_execute_serve_linux.go:334)
//
// The ledger's settle arms write the two facts the stage proof reads:
//
//	- the attempt row settles as "settled" (trusted usage) or "unknown", with the
//	  result {usage, finishReason[, failureCode]} patched onto its usage receipt
//	  (internal/store/generation_ledger.go:1155, :1179-1190);
//	- the generation_artifacts row is inserted at reserve and updated by snapshot,
//	  so a settled attempt owns an artifact whose text, textHash and revision the
//	  proof can read (internal/store/generation_ledger.go:1085, :1143-1150).
//
// A run this lane opened can therefore now carry a settled/unknown attempt with a
// generation_artifacts row, a stop finish reason, and a text whose hash is the
// artifact's own. readLegacyRosterGenerationProof's first three terms
// (electron/services/legacy-roster-generation-proof.ts:21-28) are satisfiable in
// Go: the candidate gate productionLegacyRosterCandidateFromText already parses and
// validates the settled text (production_legacy_roster_execute_serve_linux.go:467-473).
//
// # What still fails: the model revision is never frozen
//
// The proof's last step reads the run's own frozen model receipt:
//
//	const model=run.binding.sourceManifest.modelReceipt as {modelRevision:string}   (:40)
//	if (!model?.modelRevision) throw new Error('GENERATION_LEGACY_PROOF_INVALID')  (:41)
//
// No run this lane can begin freezes that member. productionLegacyRosterManifestText
// (production_legacy_roster_serve_linux.go:429) writes the reduced selection's
// modelId and modelLeaseId and the six legacyRoster members, and nothing else. The
// settled attempt's usage receipt carries requestHash, usagePolicy, purpose,
// replayTask, budgetDecision, reconciliationInjected, artifactIdentity and the
// settle's result (generation_ledger.go:1064-1074, :1179-1190), and the artifact
// JSON carries artifactId/attemptId/rootActionId/projectId/epoch/fingerprint/
// revision/text/textHash (generation_ledger.go:1063) - neither carries a model
// revision. The desktop build reads the same member back for any run
// (main-generation-owner.ts:139-142) and answers GENERATION_MODEL_BINDING_INVALID
// for a manifest without it, so a run this lane froze is unstageable on the desktop
// too.
//
// The exact Go symbol that would hold the value is generationowner.Binding.modelRevision
// (internal/generationowner/binding.go:33), filled from the lease receipt at
// BindSources/ReproduceBinding (:65, :72) and folded into the binding digest (:67)
// but exposed by no accessor - Binding's whole exported method set is Refs,
// Fingerprint and Metadata (:76-83). The only other route, generationowner.Registry.
// Resolve, reads the in-memory lease record (a four-hour TTL, dropped on restart)
// and re-checks the live session currency, so it is a live lease fact, not the
// run's frozen fact. The revision IS compared to the run's frozen one, but only
// inside the digest: reproduce compares binding.Fingerprint() against the frozen
// goOwnerSourceFingerprint (production_legacy_roster_serve_linux.go:779-801), which
// proves the model is unchanged without disclosing the value.
//
// # Why stage is not served on a substitute
//
// Fabricating the provenance is not an option: a generated provenance without a
// 64-hex model revision is refused at commit (internal/store/summary.go:39-47,
// internal/store/character_identity.go:250-252), so a proposal staged with an empty
// or guessed revision would be unapprovable. And even a real value would not
// complete the reference's one transaction: internal/store still writes
// character_identity_proposals only for the import and finalized-snapshot kinds, and
// the ledger's effect allowlist still has no legacyRosterProposal arm
// (internal/store/generation_ledger.go:485-502), so the attempt's sealing stamp the
// next mutation checks (productionLegacyRosterAssertMutable) could not be written
// and a staged run could be re-dispatched. The name therefore keeps its fail-closed
// 501 GO_OPERATION_NOT_MIGRATED, and productionLegacyRosterSeams still lists it.
package main

// productionLegacyRosterStageRevisionSymbol is the exact Go symbol that would hold
// the run's model revision. It is a package-internal field with no accessor, so no
// proof in this package can read the value; the durable manifest this lane freezes
// names modelId and modelLeaseId only.
const productionLegacyRosterStageRevisionSymbol = "generationowner.Binding.modelRevision"

// productionLegacyRosterStageFact is one line the refusal turns on: a term of the
// artifact proof this lane either now satisfies, or still cannot read. Path is
// relative to this package, exactly like productionLegacyRosterSeamLine.
type productionLegacyRosterStageFact struct {
	Term   string
	Path   string
	Line   int
	Marker string
}

// productionLegacyRosterStageSettled is the half of the proof execute's landing
// now satisfies: a settled attempt, its terminal artifact row, and the stop finish
// reason the proof requires.
func productionLegacyRosterStageSettled() []productionLegacyRosterStageFact {
	return []productionLegacyRosterStageFact{
		{"artifact-row", "../../internal/store/generation_ledger.go", 1085,
			`INSERT INTO generation_artifacts VALUES(?,?,?,?,?,?)`},
		{"settle", "../../internal/store/generation_ledger.go", 1155,
			`case "settle":`},
		{"terminal-artifact", "../../internal/store/generation_ledger.go", 1134,
			`ARTIFACT_ATTEMPT_TERMINAL`},
		{"execute-settles", "production_legacy_roster_execute_serve_linux.go", 334,
			`service.Execute(ctx, frozen, handle.RunID, nonce)`},
	}
}

// productionLegacyRosterStageUnreadable is the half of the proof that still cannot
// be read: the frozen manifest carries no modelReceipt, the value's only holder is
// an unexported binding field folded into a digest, and the proof reads it from the
// manifest.
func productionLegacyRosterStageUnreadable() []productionLegacyRosterStageFact {
	return []productionLegacyRosterStageFact{
		{"manifest-writer", "production_legacy_roster_serve_linux.go", 429,
			`func productionLegacyRosterManifestText(`},
		{"binding-field", "../../internal/generationowner/binding.go", 33,
			`modelRevision string`},
		{"binding-digest", "../../internal/generationowner/binding.go", 67,
			`binding.modelRevision,binding.Refs()`},
		{"proof-model-step", "../../../electron/services/legacy-roster-generation-proof.ts", 40,
			`run.binding.sourceManifest.modelReceipt`},
	}
}

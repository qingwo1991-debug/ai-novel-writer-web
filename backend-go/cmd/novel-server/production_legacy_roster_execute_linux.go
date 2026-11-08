//go:build linux

// This file is the re-test of the two legacy-roster names the family still
// refuses, and it is where their seams now live:
//
//	legacy-roster:execute  web/operations.json:267  refused (the repair prompt does not ship)
//	legacy-roster:stage    web/operations.json:268  refused (the artifact proof is not derivable)
//
// # execute: the loop is buildable, two of its prompts are not shipped
//
// The reference's execute is the six-step initial/repair loop at
// electron/services/main-generation-owner.ts:1143-1177:
//
//	for (const stage of ['initial','repair'] as const) {
//	  for (let ordinal = 0; ordinal < 3; ordinal++, total++) {
//	    const task = previousText === undefined ? stageTask : buildLegacyRosterReplacementTask(stageTask, previousText, context.source.writingLanguage)
//	    ...
//	    result = await execute({handle:current.handle,invocationNonce,task}, false, false, false, false, false, true)
//	    if (result.outcome.finishReason === 'length') { ... previousText = candidate.text; continue }
//	    try { parseLegacyRosterJson(candidate.text); return result } catch {
//	      stageTask = buildLegacyRosterJsonRepairTask(candidate.text); total++; break } } }
//
// Three of its terms already exist in this module:
//
//  1. the initial task, byte for byte. productionLegacyRosterTaskText freezes
//     buildLegacyRosterTask (src/shared/legacy-roster-generation-pure.ts:55-66) at
//     begin, and resolve re-derives it from the stored legacyRosterTask member, so
//     the loop's first task needs no prompt author at all;
//  2. the dispatch and readback. production_retry_draft_outline_linux.go is the
//     worked pattern - read the frozen task, reproduce the binding, Prepare,
//     Execute, viewOf - and the loop re-dispatches the frozen roster task rather
//     than authoring roster prose;
//  3. the parse. parseLegacyRosterJson (legacy-roster-generation-pure.ts:87-103) is
//     a parser; a Go port of it is code, not prose.
//
// What is missing is prose, and it is exactly the loop's two non-happy branches:
//
//	:1146 buildLegacyRosterReplacementTask -> buildStructuredReplacementPrompt
//	      (src/shared/structured-replacement-prompt.ts:3-41), the zh-CN and en-US
//	      continuation prompt a `length` stop switches to;
//	:1171 buildLegacyRosterJsonRepairTask -> CHARACTER_ROSTER_JSON_REPAIR_SYSTEM
//	      (legacy-roster-generation-pure.ts:38-39) plus
//	      buildCharacterRosterJsonRepairPrompt (:50-52), the JSON-syntax repair
//	      system prompt and its <invalid-json> wrapper.
//
// Neither prompt ships. internal/prompttemplate/legacy-roster-v1.json is
// {formatVersion, kind, key, systemPrompt, contract}, and LegacyRosterTemplate
// answers those two fragments only; the repair system prompt and the replacement
// prompt are not in it. Writing either in Go would break the rule the serve test
// enforces for the roster prompt (production_legacy_roster_serve_linux_test.go:
// the shipped prose is not restated in any Go file), and this change may not add a
// prompttemplate asset, so execute stays at the fail-closed 501
// GO_OPERATION_NOT_MIGRATED.
//
// A serve built from the shipped fragments alone would be a divergence, not a
// subset: on an unparsable `stop` answer, or on a `length` stop with retries
// left, the desktop repairs or re-asks and this lane would have to answer the
// same 200 with an artifact the desktop would never have settled on. The missing
// port is therefore the repair and replacement prose, named by
// productionLegacyRosterUnshippedPrompts below.
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
// The adoption that landed this session is none of that.
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
//     a browser run could own. Since execute is unbound, this lane's runs own no
//     artifact at all; the only artifacts stage could see are a desktop build's,
//     whose proposal rows this lane cannot read back either.
//
// # The wire shapes a serve would have to accept
//
// src/shared/legacy-roster-generation.ts:27-28 declares both names as one request
// argument - { handle } for execute, { handle, artifact } for stage - and this
// transport appends the frozen context, so a serve would take exactly two
// arguments, the shape adopt-existing already reads
// (production_legacy_roster_adopt_linux.go:40-45). handle is the four-key
// MainGenerationRunHandle, and stage's artifact is the three-key
// LegacyRosterGenerationArtifact, both of which the proof checks key by key
// (legacy-roster-generation-proof.ts:13-15, :20).
//
// # What this file is, then
//
// It binds nothing. It holds the two seam lists the refusal is composed from -
// productionLegacyRosterExecuteFragments and productionLegacyRosterStageFragments,
// which productionLegacyRosterSeams publishes and the lane's seam test re-reads
// line by line - and it holds the one derivation that decides execute:
// productionLegacyRosterUnshippedPrompts, checked against the shipped asset. When a
// later change ships that prose, the asset pin in the guard test fails first, which
// is the signal to serve execute instead of refusing it.
package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/prompttemplate"
)

// The fragments of the reference the two refused names are composed from. Each is
// spelled once so the seam table, the header above and the tests cannot drift.
const (
	// The loop itself (main-generation-owner.ts:1143) and its parse
	// (legacy-roster-generation-pure.ts:87).
	productionLegacyRosterLoopFragment  = "for (const stage of ['initial','repair'] as const)"
	productionLegacyRosterParseFragment = "parseLegacyRosterJson"

	// The prompt terms execute waits for: the two prose fragments that do not ship
	// and the three builders that compose the loop's repair and replacement tasks.
	productionLegacyRosterRepairFragment           = "CHARACTER_ROSTER_JSON_REPAIR_SYSTEM"
	productionLegacyRosterRepairPromptFragment     = "buildCharacterRosterJsonRepairPrompt"
	productionLegacyRosterRepairTaskFragment       = "buildLegacyRosterJsonRepairTask"
	productionLegacyRosterReplacementFragment      = "buildStructuredReplacementPrompt"
	productionLegacyRosterReplacementTaskFragment  = "buildLegacyRosterReplacementTask"

	// The two fragments that do ship, and the only two LegacyRosterTemplate
	// answers (internal/prompttemplate/legacyroster.go). Their names are the
	// asset's own TypeScript constants, not prose.
	productionLegacyRosterSystemFragment   = "LEGACY_ROSTER_SYSTEM_PROMPT"
	productionLegacyRosterContractFragment = "CHARACTER_ROSTER_JSON_CONTRACT"

	// The stage terms.
	productionLegacyRosterProofFragment      = "readLegacyRosterGenerationProof"
	productionLegacyRosterArtifactFragment   = "generation_artifacts"
	productionLegacyRosterCandidateFragment  = "validateLegacyRosterCandidate"
	productionLegacyRosterModelFragment      = "modelReceipt.modelRevision"
	productionLegacyRosterStageFragment      = "CharacterProposalService.stage"
	productionLegacyRosterStageLegacy        = "legacy-roster-generation"
	productionLegacyRosterProposalFragment   = "character_identity_proposals"
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

// productionLegacyRosterExecuteFragments is execute's seam: the loop, the parse,
// and the two prompt authors the loop's non-happy branches call. The loop and the
// parse are ports; the prompt fragments are the ones that do not ship, which is why
// the name is still refused.
func productionLegacyRosterExecuteFragments() []productionLegacyRosterFragment {
	const pure = "../../../src/shared/legacy-roster-generation-pure.ts"
	return []productionLegacyRosterFragment{
		{productionLegacyRosterLoopFragment, "../../../electron/services/main-generation-owner.ts", 1143,
			"for (const stage of ['initial','repair'] as const)"},
		{productionLegacyRosterRepairFragment, pure, 38,
			"export const CHARACTER_ROSTER_JSON_REPAIR_SYSTEM ="},
		{productionLegacyRosterRepairPromptFragment, pure, 50,
			"export function buildCharacterRosterJsonRepairPrompt("},
		{productionLegacyRosterRepairTaskFragment, pure, 67,
			"export function buildLegacyRosterJsonRepairTask("},
		{productionLegacyRosterReplacementFragment, "../../../src/shared/structured-replacement-prompt.ts", 3,
			"export function buildStructuredReplacementPrompt("},
		{productionLegacyRosterReplacementTaskFragment, pure, 79,
			"export function buildLegacyRosterReplacementTask("},
		{productionLegacyRosterParseFragment, pure, 87,
			"export function parseLegacyRosterJson("},
	}
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

// productionLegacyRosterShippedPrompts is the shipped prompt inventory: the two
// fragments LegacyRosterTemplate answers, in the asset's own order. It is derived
// from the asset rather than restated, so a second prompt ship changes it.
func productionLegacyRosterShippedPrompts() ([]string, error) {
	prompt, err := prompttemplate.LegacyRosterTemplate()
	if err != nil {
		return nil, errors.New(productionLegacyRosterTaskInvalid)
	}
	if prompt.SystemPrompt == "" || prompt.Contract == "" {
		return nil, errors.New(productionLegacyRosterTaskInvalid)
	}
	return []string{productionLegacyRosterSystemFragment, productionLegacyRosterContractFragment}, nil
}

// productionLegacyRosterLoopPromptFragments is what the loop's two non-happy
// branches compose: the repair system prompt and its wrapper, and the replacement
// prompt a `length` stop switches to. The loop's first task is the shipped roster
// task and is not in this list.
func productionLegacyRosterLoopPromptFragments() []string {
	return []string{productionLegacyRosterRepairFragment, productionLegacyRosterRepairPromptFragment,
		productionLegacyRosterReplacementFragment}
}

// productionLegacyRosterUnshippedPrompts is the execute derivation: the fragments
// the loop composes that the loader does not reach. It is empty only when a later
// change teaches prompttemplate to answer the repair and replacement prose, at which
// point execute can be served and its refusal replaced.
//
// The loader is the only route from the asset to this package, so a future asset
// member it does not expose would be invisible here; the lane's guard test therefore
// also reads the committed asset file and pins its key set.
func productionLegacyRosterUnshippedPrompts() []string {
	shipped, err := productionLegacyRosterShippedPrompts()
	if err != nil {
		// An unreadable asset already refuses every run name
		// (productionLegacyRosterTaskText), so nothing the loop needs can ship.
		return productionLegacyRosterLoopPromptFragments()
	}
	present := make(map[string]bool, len(shipped))
	for _, name := range shipped {
		present[name] = true
	}
	unshipped := make([]string, 0, 3)
	for _, name := range productionLegacyRosterLoopPromptFragments() {
		if !present[name] {
			unshipped = append(unshipped, name)
		}
	}
	return unshipped
}

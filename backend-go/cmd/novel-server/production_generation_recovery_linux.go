//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is where the generation recovery and retry names would be served, and
// it serves none of them:
//
//	generation:list-recovery           NOT BOUND, planner projection missing
//	generation:find-blueprint-recovery NOT BOUND, planner projection missing
//	generation:retry-draft-short-outline NOT BOUND, no frozen task to re-dispatch
//
// The names, their arities and their wire types are the reference's
// (src/shared/generation-owner-contract.ts:309-315): the two reads take no argument
// at all (electron/controllers/generation-controller.ts:279-280) and the retry
// takes `{ handle, failedAttemptId }` (:264).
//
// # The note that skipped them, and whether it is stale
//
// production_generation_reads_linux.go:7-19 says this lane lends exactly the two
// committed-state reads - the run list and one run view - because those two are
// SELECT-only projections of the author's own ledger rows that reserve no attempt,
// touch no model profile, open no egress endpoint and dispatch to no provider, and
// it adds that every other execution name (resume/restart/pause/cancel and the
// candidate mutations) stays unbound.
//
// The second half of that note IS stale: resume, pause, restart, cancel,
// discard-candidate, bind-material-decision, the two family cancels, the batch
// reads, the draft context pair and graph-generation:begin are all bound now
// (production_generation_lifecycle_linux.go, production_generation_write_linux.go,
// production_draft_context_linux.go, production_batch_reads_linux.go and
// production_generation_begin_linux.go), so "every other execution name stays
// unbound" no longer describes this lane. That staleness is why this file exists
// rather than a copy of that paragraph.
//
// The reason it gives for the two recovery reads is NOT stale, and it is not a
// reason about the ledger at all. Both names are projections of the planner half of
// readContext (electron/services/main-generation-owner.ts:912), and that half is
// exactly what this lane's own generation:read-context refuses to build:
// productionDraftContextSupported refuses GENERATION_CONTEXT_UNSUPPORTED for any
// manifest whose operation is not chapter-draft
// (production_draft_context_linux.go:719-721). So the port that does not exist is
// one port, described precisely below, and every landed layer - the ledger, the
// read lane, the llm execution lane - is what makes that description exact instead
// of speculative.
//
// # Missing port: the planner recovery projection
//
// readContext answers GenerationRecoveryContext (src/shared/generation-owner-contract.ts:139-169),
// and for a planning run its content is built from three things this module has no
// reader for:
//
//  1. blueprintRecovery - repository.readBlueprintRecovery (main-generation-owner.ts:916),
//     the incrementally committed blueprint draft rows plus the writeState the page
//     resumes from. Nothing under internal/store names a blueprint draft recovery
//     row, and the manifest key that would carry the continuation state is a
//     planning key the ledger's own gate refuses (internal/store/generation_ledger.go:492
//     glPlanning, applied to every open at :694). No planning run can even be opened
//     in this lane, which is why find-blueprint-recovery has no population here: the
//     only operation it reads, chapter-blueprint-directory
//     (main-generation-owner.ts:1067), cannot exist.
//  2. plotOutlineRecovery and planningContinuation - the same readContext body
//     (:938), with no Go builder.
//  3. the draft half of readContext - a run carrying a short-outline material
//     decision, or a settled reconcile attempt, is refused by this lane's own
//     projection (production_draft_context_linux.go:723-729), so a recovery list
//     built here would drop exactly the retry state an author needs after a length
//     stop.
//
// listRecovery also mixes in four buckets this lane can partly answer (reviews,
// editors, batches, drafts) and one it cannot (planning) - and a list that omits a
// resumable blueprint run after a crash is worse than a plain 501, because the
// author reads the omission as "there is nothing to resume". So neither name is
// installed: both keep failing closed at GO_OPERATION_NOT_MIGRATED
// (internal/transport/server.go:116).
//
// # Refused: generation:retry-draft-short-outline, and why it is not a prompt author
//
// The existing derivation of this refusal (production_generation_write_linux.go:92)
// says the operation "synthesizes its own task and message". That is still true of
// the only implementation in this module: syntheticGeneration.RetryDraftShortOutline
// builds one hardcoded user message (synthetic_generation_linux.go:1450) and
// dispatches it, so lending it would make this lane an author of the text the
// provider receives.
//
// The reference does not author anything: it re-dispatches the task the failed
// attempt itself froze, read back from the attempt's usage receipt
// (main-generation-owner.ts:189 `const task = usage.replayTask`), and proves it
// against the material decision's short-outline hash and the stored request hash
// (:190-196). Porting that would not be prompt authoring - so the honest statement
// of the gap is narrower than the old note's: this lane's own dispatch never records
// a replayTask, because OrdinaryService.Execute hands the generation service a
// request with no ReplayTask field at all
// (internal/generationowner/ordinary.go:149). Every attempt this lane writes has
// nothing for a faithful retry to re-dispatch, so the missing port is the frozen
// task record on the dispatch path; until it exists, the only available retry is
// the authored one, and that one this lane refuses.
//
// Nothing here writes, so a refused call leaves the author database byte-identical;
// the tests prove that against a live lease with a genuinely interrupted run, a
// completed one and a real failed-attempt id, so the refusal cannot be explained
// away as "there was nothing to answer".

const (
	productionGenerationRecoveryList  = "generation:list-recovery"
	productionGenerationRecoveryFind  = "generation:find-blueprint-recovery"
	productionGenerationRecoveryRetry = "generation:retry-draft-short-outline"
)

// productionGenerationRecoveryLine is one declaration that has to move before a
// name can be served: the path of the file, the line inside it, and a marker that
// must be on that line. TestProductionLaneRefusesGenerationRecovery re-reads every
// one of them, so a moved declaration fails this lane's test instead of leaving a
// note that points at nothing.
type productionGenerationRecoveryLine struct {
	Path   string
	Line   int
	Marker string
}

// productionGenerationRecoverySeam is one refused name and the missing piece that
// keeps it unbound.
type productionGenerationRecoverySeam struct {
	Operation string
	Missing   []productionGenerationRecoveryLine
}

// productionGenerationRecoveryOperations is the three names, reads first and the
// retry last, spelled once so the binder and the test cannot drift from the
// controller that registers them
// (electron/controllers/generation-controller.ts:264, :279-280).
func productionGenerationRecoveryOperations() []string {
	return []string{
		productionGenerationRecoveryList,
		productionGenerationRecoveryFind,
		productionGenerationRecoveryRetry,
	}
}

// productionGenerationRecoverySeams is the whole answer of this file: for each
// refused name, the exact code that has to move first. The markers are quotes from
// the files, so the test proves the gap is still there and still described the way
// this table describes it.
func productionGenerationRecoverySeams() []productionGenerationRecoverySeam {
	return []productionGenerationRecoverySeam{
		{
			Operation: productionGenerationRecoveryList,
			Missing: []productionGenerationRecoveryLine{
				// The reference body this name is: a per-run readContext over the
				// runs the owner knows, bucketed by operation kind.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1013, Marker: "const listRecovery = (): GenerationRecoveryList"},
				// Its first bucket is the planner projection this lane cannot build.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1021, Marker: "planning: (handle: MainGenerationRunHandle)"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 912, Marker: "const readContext = (handle: MainGenerationRunHandle): GenerationRecoveryContext"},
				// This lane's read-context refuses exactly that half.
				{Path: "production_draft_context_linux.go", Line: 720, Marker: "!= productionDraftContextOperation"},
				// And no planning run can be opened at all, so the bucket has no rows.
				{Path: "../../internal/store/generation_ledger.go", Line: 492, Marker: "func glPlanning(operation string) bool"},
			},
		},
		{
			Operation: productionGenerationRecoveryFind,
			Missing: []productionGenerationRecoveryLine{
				// The reference walks the newest chapter-blueprint-directory runs and
				// returns the first one whose blueprint draft is not finished.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1065, Marker: "const findBlueprintRecovery = (): GenerationRecoveryContext | null"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1070, Marker: "recovery.blueprintRecovery.writeState !== 'complete'"},
				// The recovery record itself: rows plus writeState, with no Go reader.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 916, Marker: "repository.readBlueprintRecovery(run.runId)"},
				// The ledger refuses to open the one operation this name reads, so the
				// population is empty rather than merely unprojected.
				{Path: "../../internal/store/generation_ledger.go", Line: 694, Marker: `if glPlanning(glText(request["operation"])) {`},
			},
		},
		{
			Operation: productionGenerationRecoveryRetry,
			Missing: []productionGenerationRecoveryLine{
				// The only retry in this module authors its own message.
				{Path: "synthetic_generation_linux.go", Line: 1450, Marker: "retry draft short outline after length stop"},
				// The reference re-dispatches the task the failed attempt froze.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 189, Marker: "const task = usage.replayTask"},
				// Which this lane's dispatch never records.
				{Path: "../../internal/generationowner/ordinary.go", Line: 149, Marker: "RunID: runID, InvocationNonce: nonce, RequestHash: hash,"},
				// The refusal this file keeps, for the narrower reason above.
				{Path: "production_generation_write_linux.go", Line: 92, Marker: "It is not a ledger transition, it is a provider call"},
			},
		},
	}
}

// productionGenerationRecoverySeamFor looks one name up, so a typo in the binder
// refuses at startup instead of being silently served or silently forgotten.
func productionGenerationRecoverySeamFor(operation string) (productionGenerationRecoverySeam, bool) {
	for _, seam := range productionGenerationRecoverySeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionGenerationRecoverySeam{}, false
}

// bindProductionGenerationRecovery installs nothing, so wiring it moves no count:
// the served set stays where this batch put it. It is wired the way the agent and
// finalization refusal slices are - one entry in the binder list in
// production_open_linux.go, next to bindProductionAgentGeneration - and what that
// buys is the seam check running in the real lane instead of only in this test.
func bindProductionGenerationRecovery(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionGenerationRecoveryOperations() {
		seam, found := productionGenerationRecoverySeamFor(operation)
		if !found || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SEAM_INCOMPLETE")
		}
	}
	// Nothing is bound: the three names keep failing closed at
	// GO_OPERATION_NOT_MIGRATED.
	return nil
}

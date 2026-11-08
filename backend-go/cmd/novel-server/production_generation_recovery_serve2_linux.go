//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file answers the one question the earlier passes left open about the last
// two recovery names, and records what the answer does and does not license:
//
//	generation:list-recovery            REFUSED  the binder this family is wired
//	                                            with holds the server, not the store
//	generation:find-blueprint-recovery  REFUSED  that same call site, plus the
//	                                            writeState its membership reads
//
// # The question: with no planning continuation, does the reference throw?
//
// It does not. Both names answer absence honestly, and the wire contract says so:
//
//   - list-recovery starts every bucket empty and returns that list untouched when
//     it has nothing to fill it from: main-generation-owner.ts:1015 is
//     `{ planning: [], editors: [], reviews: [], batches: [], drafts: [], partial:
//     false }` and :1049 is `if (!batchRoots.length) return list`. A run whose own
//     read fails is dropped with the list's own incompleteness flag instead of a
//     thrown call (:1046 `try { collect[kind](run.handle) } catch { list.partial =
//     true }`). A project with no batch root returns before the one batch read
//     that is not per-run (`draftEffects.listedBatchCommits()`, :1050), so the
//     empty case never reaches a throwing read at all. The wire type agrees field
//     by field: GenerationRecoveryList
//     (src/shared/generation-owner-contract.ts:254-262) declares all five buckets
//     as required arrays - `planning: GenerationRecoveryContext[]` through
//     `partial: boolean` - so no bucket is optional and none admits null. An empty
//     bucket is `[]`, never `null`.
//   - find-blueprint-recovery is declared nullable and returns null when no run
//     qualifies: :1065 `const findBlueprintRecovery = ():
//     GenerationRecoveryContext | null => {` and :1072 `return null`.
//
// So the seam's older sentence - that answering null "would tell the blueprint
// page there is nothing to recover" - described this lane's guess, not the
// reference: the reference itself answers null when there is nothing to recover.
// production_generation_recovery_linux.go carries that correction now.
//
// # What the honest answer does not settle: membership
//
// Both names choose WHICH runs to answer about with the planner continuation, not
// with the wire shape. list-recovery keeps a planning run only when its recovery
// union exists and the continuation is not complete (:1023), and
// find-blueprint-recovery returns a run only when its blueprint state is not
// complete and the continuation is not complete (:1070). "complete" is not a
// stored flag: it is the end of the state chain at :810
// `if (!remainingRange) return { ...base, state: 'complete' }`, reached only after
// the currency comparison at :806 passes. A run whose range is exhausted but whose
// sources drifted is "source-changed" and IS listed.
//
// There is a second, narrower case where "no continuation" does not mean null: a
// chapter-blueprint-directory run whose recovery is not available - retired
// blueprints, an invalid receipt - is answered with a context whose
// blueprintRecovery is `writeState: 'unavailable'` plus its diagnostic
// (main-generation-owner.ts:935-937), and find returns it because 'unavailable' is
// not 'complete' (:1070). The store can read that kind and diagnostic
// (bpRecoveryResult, internal/store/batch_progress.go:947), but the answer is still
// a whole GenerationRecoveryContext: modelId, handle, the four manifest arrays,
// composition, lastCompositionFinishReason, attemptedPurposes, draftSave. So even
// the one branch that needs no continuation needs readContext, which is exactly the
// projection this lane has no port of (production_draft_context_linux.go:719-732).
//
// This lane can read the range. It cannot make the comparison:
// generationowner.ReproduceBinding answers "do the current sources reproduce THIS
// process's digest", which is false for every run the desktop froze, so a state
// derived here would list completed runs and would report healthy runs as
// source-changed. That is the SUBSTITUTED verdict
// production_generation_recovery_serve_linux.go already records, and the
// null/empty question does not touch it: an empty planning bucket is only the
// reference's answer when no planning run qualifies, and this lane cannot decide
// that from an unbuildable state.
//
// # The five buckets: two real, three not
//
//	editors   REAL   served by editor-inline:read-recovery
//	                 (production_editor_inline_linux.go:216)
//	batches   REAL   served by generation:list-batches through
//	                 store.ListGenerationBatchHistory
//	                 (internal/store/batch_progress.go:207)
//	planning  NOT    the per-run context whose writeState is the state above
//	drafts    NOT    read-context refuses the short-outline retry state
//	                 (production_draft_context_linux.go:720)
//	reviews   NOT    review-revision:read-recovery is unbound
//	                 (production_review_revision_linux.go:32)
//
// The two real buckets are projections of rows, not placeholders: they are empty
// only when the project holds no such run, which is the reference's own empty case
// (:1049). That is also why a list built here cannot answer `[]` from an empty
// table it never read: it has to reach the author database the siblings reach.
//
// # The wiring, exactly
//
// The family's only entry in the finished lane is
// `bindProductionGenerationRecovery(server)` (production_open_linux.go:784), one
// closure among the generator's read binders. It receives the server and nothing
// else, while the two real buckets need more than the server:
//
//   - batches needs the admitted author runtime: the neighbouring closure already
//     passes it (`newProductionBatchReads(open.durable).Bind(server)`, :779);
//   - editors is projected by productionEditorInline.read, which holds `e.owner`
//     (production_editor_inline_linux.go:177-197). That owner is created inside
//     the block at :595, whose else clause closes at :749 - before this binder
//     runs, and out of its scope.
//
// Both names are request-arity 0 (electron/controllers/generation-controller.ts:
// 279-281), so the only project they could be scoped to is the one the frozen
// session names, and only a borrowed author database resolves that
// (production_batch_reads_linux.go:164-186 is the shape such a handler needs).
//
// Serving them is therefore a call-site change plus a decision flip, not a port:
// `bindProductionGenerationRecovery(open.durable, server)` for the batches half,
// and a call site inside the owner block for the editors half. Neither is this
// pass's to write - production_open_linux.go is out of scope - so the served count
// stays 279 (production_served_count_linux_test.go:24) and both names keep failing
// closed at GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116).
//
// # What a later pass has to do
//
//  1. Read the two real buckets where the store is in scope and project them into
//     the list's editors and batches buckets; the planning, drafts and reviews
//     buckets stay `[]` and the list's own `partial` flag carries the fact that
//     runs of those kinds were not projected - that is the reference's own way of
//     saying "the list is incomplete" (:1046).
//  2. Return null from find-blueprint-recovery only where the reference does: no
//     chapter-blueprint-directory run needs recovery. A run whose recovery is
//     unavailable is answered, not skipped (:935-937), and naming any answered
//     run's state needs the :806 comparison first.
//  3. Flip productionRecoveryServeDecisions, drop the two seam rows, and move
//     productionServedOperations from 279 to 281 in the same change - the binder
//     below refuses a lane that installs either name without doing so.

// The six answers this file publishes. They are the whole finding: the first two
// settle the question (absence is answered, not thrown), the next two are what an
// answer would still have to decide and carry, and the last two record the state
// the lane is actually in.
const (
	// productionRecoveryServe2EmptyNotNull is list-recovery: an empty bucket is an
	// empty array, never null and never a thrown call.
	productionRecoveryServe2EmptyNotNull = "EMPTY_NOT_NULL"
	// productionRecoveryServe2NullOnAbsent is find-blueprint-recovery: the name is
	// nullable and answers null when no run qualifies.
	productionRecoveryServe2NullOnAbsent = "NULL_ON_ABSENT"
	// productionRecoveryServe2StateMembership is the membership rule both names
	// share: a run is listed or returned by its continuation state, and "complete"
	// is decided after a source comparison this lane cannot make.
	productionRecoveryServe2StateMembership = "MEMBERSHIP_STATE_DRIVEN"
	// productionRecoveryServe2OptionalProjection is the context's own shape: the
	// planning continuation is an optional FIELD of the context, so a context
	// without one is exactly what the null case is built from.
	productionRecoveryServe2OptionalProjection = "OPTIONAL_FIELD"
	// productionRecoveryServe2StorelessBinder is the call site: the binder that
	// would install either name holds the server and cannot reach the author
	// database or the owner.
	productionRecoveryServe2StorelessBinder = "BINDER_HOLDS_NO_STORE"
	// productionRecoveryServe2CountUnchanged is the consequence: the lane's served
	// count did not move this pass.
	productionRecoveryServe2CountUnchanged = "LANE_COUNT_UNCHANGED"
)

// The claim names, one per statement this file makes. They exist as constants so
// the binder and the test cannot drift from the table.
const (
	productionRecoveryServe2ClaimListBuckets = "list-recovery buckets"
	productionRecoveryServe2ClaimFindNull    = "find-blueprint-recovery null"
	productionRecoveryServe2ClaimMembership  = "recovery membership"
	productionRecoveryServe2ClaimContext     = "GenerationRecoveryContext shape"
	productionRecoveryServe2ClaimCallSite    = "recovery binder call site"
	productionRecoveryServe2ClaimCount       = "served operation count"
)

// productionRecoveryServe2Claim is one statement, the answer this file publishes
// for it, and the declarations that decide it. Every row is re-read from disk by
// the test with productionSeamContains, so a moved or deleted declaration fails
// this lane's test instead of leaving a note that points at nothing.
type productionRecoveryServe2Claim struct {
	Name     string
	Answer   string
	Evidence []productionGenerationRecoveryLine
}

// productionRecoveryServe2Claims is the whole answer of this pass, in the order
// the question was asked.
func productionRecoveryServe2Claims() []productionRecoveryServe2Claim {
	return []productionRecoveryServe2Claim{
		{
			// The question, part one: an empty bucket is [] and the list is never
			// thrown away because a bucket has nothing in it.
			Name:   productionRecoveryServe2ClaimListBuckets,
			Answer: productionRecoveryServe2EmptyNotNull,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1015,
					Marker: "const list: GenerationRecoveryList = { planning: [], editors: [], reviews: [], batches: [], drafts: [], partial: false }"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1049,
					Marker: "if (!batchRoots.length) return list"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1046,
					Marker: "try { collect[kind](run.handle) } catch { list.partial = true }"},
				// The wire type: five required arrays and a boolean, no optional
				// bucket and no nullable bucket.
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 254,
					Marker: "export interface GenerationRecoveryList {"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 255,
					Marker: "planning: GenerationRecoveryContext[]"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 261,
					Marker: "partial: boolean"},
			},
		},
		{
			// The question, part two: find answers null, and the return type says
			// it may. Both halves are in one marker because a signature without
			// its null return is not the behaviour under test.
			Name:   productionRecoveryServe2ClaimFindNull,
			Answer: productionRecoveryServe2NullOnAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1065,
					Marker: "const findBlueprintRecovery = (): GenerationRecoveryContext | null => {"},
				// The marker spans the loop's close and the null return, because a
				// signature without its null return is not the behaviour under test.
				// productionSeamContains ignores white space, so both lines may be
				// written as one.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1070,
					Marker: "if (recovery.blueprintRecovery && recovery.blueprintRecovery.writeState !== 'complete' && recovery.planningContinuation?.state !== 'complete') return recovery } return null"},
				// The other side of the same decision: a run whose recovery is not
				// available is answered with a context, not skipped, so "no
				// continuation" is not the same question as "null".
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 937,
					Marker: "editRange: null, saved: null, writeState: 'unavailable' as const, diagnostic: blueprint.diagnostic,"},
			},
		},
		{
			// Why honest null/empty is not a handler: completeness is decided by
			// state, and the state needs the comparison below it.
			Name:   productionRecoveryServe2ClaimMembership,
			Answer: productionRecoveryServe2StateMembership,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1023,
					Marker: "if ((recovery.plotOutlineRecovery || recovery.blueprintRecovery) && recovery.planningContinuation?.state !== 'complete'"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 810,
					Marker: "if (!remainingRange) return { ...base, state: 'complete' }"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 806,
					Marker: "current = current && isDeepStrictEqual(projection(run.binding), projection(deps.rebuildBinding(run.binding, currentModelReceipt(run.binding))))"},
				// And the receipt the state chain starts from.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 767,
					Marker: "const planningContinuation = (run: DurableGenerationRun): PlanningContinuation | undefined => {"},
			},
		},
		{
			// The context an empty planning bucket would have to contain is
			// buildable without the continuation only because the field is
			// optional - which is the branch a null answer comes from.
			Name:   productionRecoveryServe2ClaimContext,
			Answer: productionRecoveryServe2OptionalProjection,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 139,
					Marker: "export interface GenerationRecoveryContext {"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 142,
					Marker: "planningContinuation?: PlanningContinuation"},
			},
		},
		{
			// The decisive obstacle this pass found: the call site.
			Name:   productionRecoveryServe2ClaimCallSite,
			Answer: productionRecoveryServe2StorelessBinder,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_open_linux.go", Line: 784,
					Marker: "bindProductionGenerationRecovery(server)"},
				// The store the real buckets need, in the same list.
				{Path: "production_open_linux.go", Line: 779,
					Marker: "newProductionBatchReads(open.durable).Bind(server)"},
				// The owner block the editors bucket needs, opened here...
				{Path: "production_open_linux.go", Line: 595,
					Marker: "if owner, ownerErr := newProductionGeneration(open, server, config.Dispatch); ownerErr == nil {"},
				{Path: "production_open_linux.go", Line: 696,
					Marker: "newProductionEditorInline(owner).Bind(server)"},
				// ... and the binder both names project through.
				{Path: "production_editor_inline_linux.go", Line: 216,
					Marker: "server.Bind(productionEditorInlineReadRecoveryName, read)"},
				// The two real buckets' ports, each taking what this binder lacks.
				{Path: "../../internal/store/batch_progress.go", Line: 207,
					Marker: "func ListGenerationBatchHistory(ctx context.Context, db *sql.DB, projectID string) ([]GenerationBatchHistory, error) {"},
				// Zero-request-arity at the renderer: no handle, only the session.
				{Path: "../../../electron/controllers/generation-controller.ts", Line: 279,
					Marker: "register('generation:list-recovery', 0, owner => owner.listRecovery())"},
			},
		},
		{
			// The count, left where it was, with the constant that says so.
			Name:   productionRecoveryServe2ClaimCount,
			Answer: productionRecoveryServe2CountUnchanged,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_served_count_linux_test.go", Line: 24,
					Marker: "const productionServedOperations = 279"},
			},
		},
	}
}

// productionRecoveryServe2AnswerKnown reports whether one answer is one this file
// publishes. A typo therefore fails startup instead of being recorded as a new
// kind of finding.
func productionRecoveryServe2AnswerKnown(answer string) bool {
	switch answer {
	case productionRecoveryServe2EmptyNotNull, productionRecoveryServe2NullOnAbsent,
		productionRecoveryServe2StateMembership, productionRecoveryServe2OptionalProjection,
		productionRecoveryServe2StorelessBinder, productionRecoveryServe2CountUnchanged:
		return true
	}
	return false
}

// productionRecoveryServe2SettledClaims is the two claims that answer the question
// a later pass must not have to ask again: an empty bucket is [] and a missing
// blueprint recovery is null. Both have to be present whatever else the table
// grows.
func productionRecoveryServe2SettledClaims() []string {
	return []string{
		productionRecoveryServe2ClaimListBuckets,
		productionRecoveryServe2ClaimFindNull,
	}
}

// productionRecoveryServe2ClaimFor looks one claim up by name, so the test can
// assert a specific answer without depending on the table's order.
func productionRecoveryServe2ClaimFor(name string) (productionRecoveryServe2Claim, bool) {
	for _, claim := range productionRecoveryServe2Claims() {
		if claim.Name == name {
			return claim, true
		}
	}
	return productionRecoveryServe2Claim{}, false
}

// bindProductionGenerationRecoveryServe2 installs nothing and moves no count: it
// makes this file's answer a startup fact. It runs beside the family's binder
// (production_generation_recovery_linux.go), in the same server-only call site
// this file describes, so the claim about that call site cannot be true while a
// handler is installed.
//
// The last check is the tripwire: the day either name is bound anywhere in the
// lane, the claim at productionRecoveryServe2ClaimCallSite has expired and this
// file has to be revised in the same change - the same rule
// production_generation_recovery_reads_linux.go applies to the two names it
// records as unbound.
func bindProductionGenerationRecoveryServe2(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	claims := productionRecoveryServe2Claims()
	if len(claims) == 0 {
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_CLAIMS_MISSING")
	}
	named := make(map[string]bool, len(claims))
	for _, claim := range claims {
		if claim.Name == "" || named[claim.Name] || len(claim.Evidence) == 0 ||
			!productionRecoveryServe2AnswerKnown(claim.Answer) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_CLAIM_INVALID")
		}
		named[claim.Name] = true
	}
	for _, name := range productionRecoveryServe2SettledClaims() {
		if !named[name] {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_ANSWER_MISSING")
		}
	}
	bound := server.BoundOperations()
	for _, operation := range productionGenerationRecoveryRefusedOperations() {
		if productionRecoveryBoundContains(bound, operation) {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE2_STALE_CLAIM")
		}
	}
	return nil
}

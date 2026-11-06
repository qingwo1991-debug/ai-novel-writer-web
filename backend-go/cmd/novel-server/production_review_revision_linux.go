//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is where the four review-revision operations would be served, and it
// serves none of them. Each one keeps failing closed at the 501
// GO_OPERATION_NOT_MIGRATED (internal/transport/server.go:116) for a reason that
// is not "no time yet": the declaration each name needs does not exist in this
// module, and a handler installed over a missing declaration is worse than the
// plain 501 that tells the browser the server cannot do this yet.
//
//	review-revision:prepare          arity 1  electron/controllers/generation-controller.ts:240
//	review-revision:commit-review    arity 1  electron/controllers/generation-controller.ts:241
//	review-revision:commit-revision  arity 1  electron/controllers/generation-controller.ts:242
//	review-revision:read-recovery    arity 1  electron/controllers/generation-controller.ts:243
//
// Every one of the four takes exactly one argument and no session argument: the
// controller appends the project session itself (generation-controller.ts:178-190)
// and the wire declarations are src/shared/review-revision-generation.ts:173-178:
//
//	'review-revision:prepare': { args: [PrepareReviewRevisionRequest]; return: PreparedReviewRevisionContext }
//	'review-revision:commit-review': { args: [ReviewGenerationCommitRequest]; return: ReviewRevisionCommitReceipt }
//	'review-revision:commit-revision': { args: [RevisionGenerationCommitRequest]; return: ReviewRevisionCommitReceipt }
//	'review-revision:read-recovery': { args: [{ handle: MainGenerationRunHandle }]; return: ReviewRevisionRecovery }
//
// The renderer is the only consumer: the chapter command prepares one context and
// commits a review (src/services/workflows/commands/review-chapter.command.ts:68,
// :173), the refine commands commit a revision (review-revision-command.ts:68,
// :172), and the recovery workflow reads a run back (review-revision-recovery-workflow.ts:14).
//
// # The premise of this batch, checked before a line was written
//
// The brief said the store half already exists in internal/store/reviewrevision.go
// (511 lines): CreateReview, CreateRevision, ReplacePending, MergeRevision,
// MarkDiscarded, MarkMerged, ListReviews, FullReview, ListRevisions, FullRevision,
// NextReviewIndex, NextRevisionIndex and the rrAssertSource/rrUnbound guards. That
// sentence is TRUE, and it is about a different family. Those twelve methods are
// the one-to-one port of the two repositories the desktop reads and writes under
// the plain db names:
//
//	electron/repositories/review-repository.ts:54-148   5 methods
//	  create :54           -> internal/store/reviewrevision.go:266  CreateReview
//	  listByDraft :94      -> :148  ListReviews
//	  getLatestByDraft :108-> :153  LatestReview
//	  getFull :125         -> :159  FullReview
//	  getNextIndex :139    -> :178  NextReviewIndex
//	electron/repositories/revision-repository.ts:102-445  9 methods
//	  create :102          -> :297  CreateRevision
//	  replacePending :153  -> :301  ReplacePending
//	  listByDraft :210     -> :165  ListRevisions
//	  getPending :224      -> :165  the same call with pending=true
//	  getFull :238         -> :172  FullRevision
//	  getNextIndex :252    -> :185  NextRevisionIndex
//	  mergeIntoDraft :268  -> :383  MergeRevision
//	  markMerged :409      -> :480  MarkMerged
//	  markDiscarded :429   -> :476  MarkDiscarded
//
// with rrAssertSource (:230) as draft-source-guard.ts's assertExpectedDraftSource
// and rrUnbound (:250) as the pending-status fence. Nine TS revision methods
// become eight Go ones only because getPending folded into ListRevisions' pending
// argument; nothing else was dropped, and nothing else was added. The leaf is
// already served: production_review_write_linux.go:107-112 publishes
// db:review-create and the five db:revision-* names over it, and
// production_review_reads_linux.go publishes the six read names.
//
// So the store half of THIS family is not internal/store/reviewrevision.go. It is
// the frozen review-revision context plus the run-ledger effect that records the
// commit, and neither exists. That is the whole finding; the four seams below are
// the same finding name by name.
//
// # prepare - the freeze this lane cannot produce
//
// prepare answers a PreparedReviewRevisionContext: a contextId plus the frozen
// ReviewRevisionContext (src/shared/review-revision-generation.ts:60-79). The
// freeze is electron/services/review-revision-context.ts:39-155, and five of its
// inputs have no Go port at all:
//
//	frozenGoals        review-revision-context.ts:150  src/shared/chapter-goal-review.ts:25  freezeChapterGoals
//	preflightFindings  :152                            src/shared/consistency-preflight.ts:84  findBlueprintContinuityRisks
//	history[].projection :96                           electron/repositories/summary-repository.ts:706  listFinalizedContinuityBefore
//	confirmation       :80                             parseHumanConfirmedReviewSnapshot (only the unexported admission.m03ParseConfirmationSnapshot exists)
//	recheck            :63                             ReviewCycleRepository.planRecheck (only the unexported store.rcPlanRecheckDisposition exists)
//
// and the answer to "which field is missing" is therefore the honest one: it is
// not one field. frozenGoals is the FIRST field that cannot be computed, then
// preflightFindings, then history[].projection, then confirmation/recheck. The
// three-key begin this lane can write freezes neither the context nor its hash
// (syntheticManifest, synthetic_generation_linux.go:729-755, writes no
// reviewRevisionContext and no reviewRevisionContextHash; the selection allowlist
// at :404-416 has no reviewRevisionContextId), and the contextId registry
// (review-revision-generation.ts:30-39, 64 entries, disposable by design) has no
// issuer. A port may not substitute a narrower freeze: the context is what the
// report body, the goal review and the preflight findings are built from.
//
// # Both commits - the freeze they re-prove, and the effect they cannot stamp
//
// commitReview re-proves the freeze three times: this.context(run) recomputes the
// manifest hash (review-revision-generation.ts:40-49, GENERATION_REVIEW_CONTEXT_UNPROVEN),
// this.assertPrepared compares the caller's contextId against that same context
// (:182-184, GENERATION_REVIEW_CONTEXT_REQUIRED), and assertSources re-captures
// the sources (main-generation-owner.ts:729, :1518). With prepare unbound there is
// no contextId to present and no frozen manifest to compare, so a commit here
// must REFUSE rather than accept the weaker proof the review/revision tables alone
// would allow - the reference itself refuses that request. That is why neither
// commit is bound: an installed handler could only ever answer
// GENERATION_REVIEW_CONTEXT_REQUIRED, which is the "handler over a missing piece"
// this lane's two precedents exist to avoid (production_finalization_generation_linux.go:22-36,
// synthetic_durable_http_linux_test.go:682-700).
//
// The second, independent gap is the effect. record() stamps
// reviewRevisionEffect into the attempt's usage receipt
// (review-revision-generation.ts:185-191); that stamp IS the commit receipt
// (:93-118 saved) and the retry guard (:55-58 assertMutable). No store writes it
// and no ledger admits it:
//
//	internal/store/generation_ledger.go:463-484  glEffectKeyAllowed knows graphEffects, finalizationEffect and visibleComposition
//	internal/store/generation_ledger.go:556      the root guard's effect list is graphEffects, finalizationEffect
//	internal/store/generation_ledger.go:583      mutableRoot's usage allowlist refuses any other key
//	internal/store/generation_ledger.go:1219     recordEffect refuses the key for every operation
//
// A commit written around that seam would create a second review row on retry,
// which is exactly what assertMutable prevents in the reference. commitReview
// additionally stamps reviewCycleRecheck (:221-226) and calls
// ReviewCycleRepository.create (:228), and neither has a Go writer: no
// non-test INSERT INTO review_cycles exists in this module.
//
// # commit-revision - three unported gates on top of the same two
//
// Even with the freeze and the effect, commitRevision needs
// readVisibleComposition (:244, GENERATION_REVIEW_COMPOSITION_REQUIRED), which
// this lane only has as the unexported store dcComposition
// (internal/store/draft_commit_tx.go:827) for the draft-commit run it owns;
// assertMateriallyCompleteRevision (:251, refinement-completeness.ts:29) and
// isNoopRevision (:252, src/shared/review-cycle.ts:95), neither of which has a Go
// port; and ReviewCycleRepository.attachGeneratedRevision (:264), also unwritten.
// store.ReplacePending (reviewrevision.go:301) is only the row insert.
//
// # read-recovery - two shapes, and this lane can produce neither
//
// The reference answers a ReviewRevisionRecovery
// (src/shared/review-revision-generation.ts:156-172), not a status word:
//
//   - "no pending work" is a real recovery object whose saved field is absent and
//     whose canResume says whether the frozen source is still current
//     (review-revision-generation.ts:272-300). The renderer then decides: it
//     refuses with GENERATION_REVIEW_SOURCE_CHANGED when the source moved
//     (review-revision-recovery-workflow.ts:20) and with
//     GENERATION_REVIEW_RECOVERY_COPY_ONLY when the candidate is no longer
//     summarisable (:21).
//   - the refusal is a THROW out of requireRun or context():
//     GENERATION_RUN_MISSING or GENERATION_REVIEW_CONTEXT_UNPROVEN
//     (review-revision-generation.ts:274, :47).
//
// Those are different shapes and a port must not collapse them into one - but
// both are built from the same frozen context (context, modelId, sourceStatus from
// the binding rebuild at main-generation-owner.ts:1005-1007, composition from
// readVisibleComposition, saved from the effect receipt). A project with no run is
// the throw; a run with a pending revision is the object with saved absent and
// canResume true; a run whose stored revision no longer matches its frozen source
// is the same object with sourceStatus "conflict". Without the context and the
// effect, this lane can produce none of the three answers, so it produces the 501
// instead of a fourth shape.
//
// # The decision, and the seam table
//
// Nothing is bound. productionServedOperations therefore stays at 158
// (production_served_count_linux_test.go:23): no handler was installed, so no
// count moved. A batch that binds one of these names has to move it, and has to
// put the name through the same single-writer borrow as its neighbours
// (production_durable_linux.go:313-364) with exactly one errors.Is mapping layer -
// both commits are writes, and read-recovery mints a contextId.
//
// productionReviewRevisionSeams is that whole answer as data: for each name, the
// declaration (file, line, marker) that has to exist first. The test re-reads
// every one of them, so a moved declaration fails this lane's test instead of
// leaving a note that points at nothing.

// The four names this file refuses, spelled once so a test and the seam table
// cannot drift from the binder.
const (
	productionReviewRevisionPrepare        = "review-revision:prepare"
	productionReviewRevisionCommitReview   = "review-revision:commit-review"
	productionReviewRevisionCommitRevision = "review-revision:commit-revision"
	productionReviewRevisionReadRecovery   = "review-revision:read-recovery"
)

// productionReviewRevisionLine is one declaration a name depends on, written as
// the path of the file, the line inside it, and a marker that must be on that
// line. The marker is what makes the note checkable: a test re-reads every entry,
// so a declaration that moves fails this lane's test instead of leaving a comment
// that points at whatever line 266 happens to hold by then.
type productionReviewRevisionLine struct {
	Path   string
	Line   int
	Marker string
}

// productionReviewRevisionSeam is one refused name together with the declarations
// that keep it unbound.
type productionReviewRevisionSeam struct {
	Operation string
	Required  []productionReviewRevisionLine
}

// productionReviewRevisionOperations is the family's four names, in the order the
// desktop controller registers them (electron/controllers/generation-controller.ts:240-243).
func productionReviewRevisionOperations() []string {
	return []string{
		productionReviewRevisionPrepare,
		productionReviewRevisionCommitReview,
		productionReviewRevisionCommitRevision,
		productionReviewRevisionReadRecovery,
	}
}

// productionReviewRevisionSeams is the whole answer of this file: for each
// refused name, the declarations it needs and does not have. Most entries are
// code this module has never had and cannot install above a seam; the store
// entries (reviewrevision.go:266, :301) are the exception and are listed because
// they are the methods a commit would call - they exist, and they are the leaf,
// not the commit. Every entry is written at the place it would have to appear, so
// the next change starts from the file that has to change rather than from a
// guess.
func productionReviewRevisionSeams() []productionReviewRevisionSeam {
	return []productionReviewRevisionSeam{
		{
			Operation: productionReviewRevisionPrepare,
			Required: []productionReviewRevisionLine{
				{Path: "../../../electron/services/review-revision-context.ts", Line: 39, Marker: "export function captureReviewRevisionContext("},
				{Path: "../../../src/shared/chapter-goal-review.ts", Line: 25, Marker: "export function freezeChapterGoals("},
				{Path: "../../../src/shared/consistency-preflight.ts", Line: 84, Marker: "export function findBlueprintContinuityRisks("},
				{Path: "../../../electron/repositories/summary-repository.ts", Line: 706, Marker: "static listFinalizedContinuityBefore("},
				{Path: "synthetic_generation_linux.go", Line: 729, Marker: "func syntheticManifest("},
			},
		},
		{
			Operation: productionReviewRevisionCommitReview,
			Required: []productionReviewRevisionLine{
				{Path: "../../../electron/services/review-revision-generation.ts", Line: 182, Marker: "private assertPrepared("},
				{Path: "../../internal/store/generation_ledger.go", Line: 463, Marker: "func glEffectKeyAllowed("},
				{Path: "../../internal/store/generation_ledger.go", Line: 583, Marker: `case "graphEffects", "finalizationEffect":`},
				{Path: "../../internal/store/generation_ledger.go", Line: 1219, Marker: "glEffectKeyAllowed(key"},
				{Path: "../../../electron/services/review-revision-generation.ts", Line: 228, Marker: "ReviewCycleRepository.create("},
				{Path: "../../internal/store/reviewrevision.go", Line: 266, Marker: "func (r *ReviewRevisions) CreateReview("},
			},
		},
		{
			Operation: productionReviewRevisionCommitRevision,
			Required: []productionReviewRevisionLine{
				{Path: "../../../electron/services/review-revision-generation.ts", Line: 244, Marker: "readVisibleComposition("},
				{Path: "../../internal/store/draft_commit_tx.go", Line: 831, Marker: "func dcComposition("},
				{Path: "../../../src/services/workflows/commands/refinement-completeness.ts", Line: 29, Marker: "export function assertMateriallyCompleteRevision("},
				{Path: "../../../src/shared/review-cycle.ts", Line: 95, Marker: "export function isNoopRevision("},
				{Path: "../../../electron/services/review-revision-generation.ts", Line: 264, Marker: "ReviewCycleRepository.attachGeneratedRevision("},
				{Path: "../../internal/store/generation_ledger.go", Line: 463, Marker: "func glEffectKeyAllowed("},
				{Path: "../../internal/store/reviewrevision.go", Line: 301, Marker: "func (r *ReviewRevisions) ReplacePending("},
			},
		},
		{
			Operation: productionReviewRevisionReadRecovery,
			Required: []productionReviewRevisionLine{
				{Path: "../../../src/shared/review-revision-generation.ts", Line: 156, Marker: "export interface ReviewRevisionRecovery"},
				{Path: "../../../electron/services/review-revision-generation.ts", Line: 272, Marker: "readRecovery(handle: MainGenerationRunHandle"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1005, Marker: "const readReviewRevisionRecovery ="},
				{Path: "../../../electron/services/review-revision-generation.ts", Line: 185, Marker: "private record("},
				{Path: "../../internal/store/generation_ledger.go", Line: 463, Marker: "func glEffectKeyAllowed("},
			},
		},
	}
}

// productionReviewRevisionSeam returns the documented missing piece for one name,
// or false when the name is not one of the four.
func productionReviewRevisionSeamOf(operation string) (productionReviewRevisionSeam, bool) {
	for _, seam := range productionReviewRevisionSeams() {
		if seam.Operation == operation {
			return seam, true
		}
	}
	return productionReviewRevisionSeam{}, false
}

// bindProductionReviewRevision installs the four review-revision names. It
// installs nothing, and it refuses to start a lane whose refusal is undocumented:
// every one of the four must carry at least one named missing piece, so the day
// one of them gains a body the table has to change with it instead of silently
// becoming a lie.
//
// The nil-server refusal is the same one every other binder in this package
// answers, so wiring this into production_open_linux.go cannot mask a construction
// bug. Until the seams above are real, the honest answer for all four names is the
// transport's own 501, and installing a handler that can only refuse would claim a
// service the lane does not have.
func bindProductionReviewRevision(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	for _, operation := range productionReviewRevisionOperations() {
		seam, found := productionReviewRevisionSeam(operation)
		if !found || len(seam.Required) == 0 {
			return errors.New("PRODUCTION_REVIEW_REVISION_SEAM_INCOMPLETE")
		}
	}
	// Nothing is bound: the four names keep failing closed at GO_OPERATION_NOT_MIGRATED
	// (internal/transport/server.go:116), which is the durable binding the browser
	// already sees, not a 404.
	return nil
}

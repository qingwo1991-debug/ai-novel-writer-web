//go:build linux

package main

import (
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the SERVE DECISION for the two recovery names the seam
// (production_generation_recovery_linux.go) refused, re-tested against the code
// this session added. The answer is still no for both, but three of the seam's
// statements no longer hold - the author-edit receipt HAS a Go reader (and that
// reader refuses the shape the desktop writes), the binding rebuild is only
// substituted, and the blueprint rows reader has landed - so all three are
// corrected here instead of being left in prose:
//
//	generation:find-blueprint-recovery  REFUSED  the recovery CONTEXT has four
//	                                            owner-only fields, and the
//	                                            writeState behind one of them is a
//	                                            comparison this lane cannot make
//	generation:list-recovery            REFUSED  the planning bucket is that same
//	                                            context; drafts and reviews keep
//	                                            their own missing ports
//
// # The pieces the seam named, one row each
//
// productionRecoveryServePieces() is the re-test. Every row quotes the
// declarations that decide it, so the test re-reads them from disk instead of
// trusting this paragraph.
//
//   - readBlueprintAuthorEdit (generation-run-repository.ts:194) is REFUTED, not
//     missing. A Go reader exists - bpAuthorEditCode
//     (internal/store/batch_progress.go:1194-1276), run by bpRecovery for every
//     run it reads (:992) - but it is not faithful: it parses the receipt's
//     committedRange with bpRangeObject (:1235), the blueprint-progress accessor
//     that requires startChapter/endChapter (:1138), while the receipt the
//     desktop writes carries {from,to} and the reference reads .from/.to
//     (generation-run-repository.ts:205-206, :211). Every real author-saved
//     blueprint run is therefore diagnosed
//     GENERATION_BLUEPRINT_AUTHOR_RECEIPT_INVALID instead of being listed, and
//     through a SERVED name - generation:list-directory-progress. The HTTP test
//     below seeds exactly that receipt and pins the diagnosis; when the accessor
//     is fixed the pin flips to progress and this verdict becomes VALIDATED.
//     Even then the value is dropped: the store's own comment says the receipt
//     "is not part of the catalog" (:1192), so writeState's `saved` field would
//     still need it returned.
//   - ReproduceBinding is NOT the answer to readBlueprintAuthorEdit. The two
//     answer different questions. readBlueprintAuthorEdit proves a FROZEN effect
//     agrees with the frozen manifest and with the commit-operation row; it never
//     reads a current source. ReproduceBinding (internal/generationowner/
//     binding.go:63) re-reads CURRENT sources and compares no stored receipt at
//     all. Neither one substitutes for the other.
//   - deps.rebuildBinding (:806) is SUBSTITUTED, and that is the decisive row.
//     ReproduceBinding rebuilds a single digest over {projectId, epoch,
//     modelRevision, sourceRefs} (binding.go:67), while the comparison writeState
//     needs is the frozen NINE-field fingerprint minus two keys, plus
//     contextSnapshotId and de-epoch-ed sourceRefs (:803-806,
//     electron/services/generation-source-binding.ts:622-624). This lane's own
//     currency checks therefore work only for a run the GO owner froze, keyed by
//     a goOwnerSourceFingerprint that freeze wrote into the manifest
//     (synthetic_generation_linux.go:741, read back at
//     production_agent_generation_serve_linux.go:916). A
//     chapter-blueprint-directory run in an author database written by the
//     desktop carries no such key, so ReproduceBinding can only answer "not
//     reproduced" for exactly the runs this name reads - which would report every
//     real recovery as source-changed, a recovery the page can never commit. That
//     is the risk this file refuses to take.
//   - BlueprintRepository.getAll (:800) is PORTED: store.Blueprints.All
//     (internal/store/blueprints.go:69) reads the same eight compared fields over
//     the same table, and the browser already reaches it through
//     db:blueprint-get-all (internal/transport/editor_stage_read.go:42). This is
//     the one obstacle the session really removed; it is not the decisive one.
//
// # Why writeState is still not derivable
//
// planningContinuation (:767) is not a function of the run row. For a
// chapter-blueprint-directory run its state chain is continued -> in-flight ->
// current (:786-812), and `current` is the rebuildBinding comparison above. The
// receipts it reads are outside this lane's reach as well: a blueprint run's own
// continuation is $runEffects.planningContinuation (:179), and the Go ledger
// refuses the whole effect bag for an ordinary binding
// (internal/store/generation_ledger.go:511-513, GENERATION_EFFECT_UNSUPPORTED)
// and refuses every planning operation outright (:719-720). So every run these
// names can answer about was frozen by the desktop, and the one question the
// answer needs is the one this process cannot ask.
//
// The one receipt that IS read here - the blueprint author edit, which supplies
// `saved` and the author-saved state - is the one bpAuthorEditCode refuses on
// real data (REFUTED above). So `saved` would have to be fixed before it could be
// projected, not merely returned.
//
// # The wire type is the whole context, not the recovery union
//
// find-blueprint-recovery returns a GenerationRecoveryContext
// (src/shared/generation-owner-contract.ts:139-169), and list-recovery puts those
// contexts in its planning and drafts buckets (:254-262). The blueprint union
// inside one is five fields (:185-199), and four of them come from the live
// owner: leaseEpoch is deps.epoch, draft is the newest candidate text, writeState
// is the state above, and editRange is its remainingRange (:939-942). Only
// `saved` is a stored receipt. The lane's own read-context port is deliberately
// narrower - one operation, none of these fields
// (production_draft_context_linux.go:719-732) - so there is nothing to borrow.
//
// # What this session added, and what it did not
//
//   - generation:retry-draft-short-outline now reads the frozen task back from
//     the failed attempt (production_retry_draft_outline_linux.go). That is a
//     re-dispatch readback, not the drafts bucket: the bucket needs readContext's
//     draftShortOutline (:1037), which the served read-context refuses for the
//     same run.
//   - review-revision:read-recovery is still unbound
//     (production_review_revision_linux.go:32), so the reviews bucket is still
//     unbuildable.
//   - The two buckets that ARE served stay served by their sibling names:
//     editors by editor-inline:read-recovery, batches by
//     generation:list-batches. Nothing here re-binds them, and a list that
//     dropped the three unbuildable buckets would read as "there is nothing to
//     resume", which is still worse than a 501.
//
// # Wiring
//
// This file installs nothing, so the served count stays 271
// (production_served_count_linux_test.go:24). Serving either name would need a
// binder that holds the owner or the durable handle, and the recovery binder is
// wired with the server only (production_open_linux.go:754, after the owner block
// closes at :717) while the one owner is created as a local variable inside that
// block (:591). That call site is where a future wiring has to change; it is
// deliberately not changed here.

// productionRecoveryServeDecision is one name's verdict: the operation, whether
// this lane installs a handler for it, and the symbolic code naming why not. As
// in the import-run receipt decision, Served is a plain bool so a later pass that
// serves a name has to flip it and drop the seam row in the same change -
// productionRecoveryServeAgrees enforces that at startup.
type productionRecoveryServeDecision struct {
	Operation string
	Served    bool
	Reason    string
}

// The two reason codes. They are this file's own vocabulary, in the symbolic
// shape this lane publishes, and they never reach a renderer: an unbound name
// answers GO_OPERATION_NOT_MIGRATED from the transport itself
// (internal/transport/server.go:116).
const (
	// productionRecoveryServeReasonWriteState is find-blueprint-recovery: the
	// returned BlueprintRecovery needs leaseEpoch, draft and writeState from the
	// live owner, writeState's `current` question is the rebuildBinding one, and
	// its `saved` field rests on the receipt reader this file records as REFUTED.
	productionRecoveryServeReasonWriteState = "PRODUCTION_GENERATION_RECOVERY_WRITESTATE_UNOWNED"
	// productionRecoveryServeReasonBuckets is list-recovery: its planning bucket
	// is the same context, and the drafts and reviews buckets keep their own
	// missing ports.
	productionRecoveryServeReasonBuckets = "PRODUCTION_GENERATION_RECOVERY_BUCKETS_UNBUILDABLE"
)

// productionRecoveryServeDecisions is the whole verdict of this file, spelled
// once so the binder and the test cannot drift.
func productionRecoveryServeDecisions() []productionRecoveryServeDecision {
	return []productionRecoveryServeDecision{
		{Operation: productionGenerationRecoveryFind, Served: false, Reason: productionRecoveryServeReasonWriteState},
		{Operation: productionGenerationRecoveryList, Served: false, Reason: productionRecoveryServeReasonBuckets},
	}
}

// The five verdicts a piece can carry.
//
//	PORTED       a Go reader reproduces the reference declaration exactly
//	VALIDATED    a Go reader reimplements its checks but drops its value
//	REFUTED      a Go reader exists, and it refuses the shape the reference writes
//	ABSENT       nothing in this module produces or reads it
//	SUBSTITUTED  a Go function exists, but it answers a different question
const (
	productionRecoveryServePorted      = "PORTED"
	productionRecoveryServeValidated   = "VALIDATED"
	productionRecoveryServeRefuted     = "REFUTED"
	productionRecoveryServeAbsent      = "ABSENT"
	productionRecoveryServeSubstituted = "SUBSTITUTED"
)

// The three piece names with their own constant, because the decision's
// correctness depends on them specifically: the two the seam called missing and
// the one the task pairs together.
const (
	productionRecoveryServePieceAuthorEdit   = "readBlueprintAuthorEdit"
	productionRecoveryServePieceContinuation = "readPlanningContinuation"
	productionRecoveryServePieceRebuild      = "deps.rebuildBinding"
)

// productionRecoveryServePiece is one piece of the recovery projection the seam
// named, whether Go can answer it, and the declarations that decide the verdict.
type productionRecoveryServePiece struct {
	Name     string
	Verdict  string
	Evidence []productionGenerationRecoveryLine
}

// productionRecoveryServePieces is the re-test, in the order the seam named the
// pieces. The evidence rows are read back by the test with
// productionSeamContains, so a moved or deleted declaration fails this lane's
// test instead of leaving a verdict nobody can check.
func productionRecoveryServePieces() []productionRecoveryServePiece {
	return []productionRecoveryServePiece{
		{
			// The receipt the seam said had "no Go reader at all". It has one, and
			// that reader refuses every receipt the reference writes: the commit
			// range is parsed with the blueprint-progress accessor rather than the
			// {from,to} the receipt carries.
			Name:    productionRecoveryServePieceAuthorEdit,
			Verdict: productionRecoveryServeRefuted,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 194,
					Marker: "readBlueprintAuthorEdit(runId: string): BlueprintAuthorEditReceipt | null"},
				// The recovery reader that makes it an input of the projection.
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 564,
					Marker: "const authorEdit = this.readBlueprintAuthorEdit(runId);"},
				// The Go port, and the comment that says the receipt is dropped.
				{Path: "../../internal/store/batch_progress.go", Line: 1194,
					Marker: "func bpAuthorEditCode(ctx context.Context, conn *sql.Conn, run *bpRun) string {"},
				{Path: "../../internal/store/batch_progress.go", Line: 1192,
					Marker: "The receipt itself is not part of the catalog; it is validated"},
				{Path: "../../internal/store/batch_progress.go", Line: 1200,
					Marker: `value, present := effects["blueprintAuthorEdit"]`},
				// The defect, in three lines: the receipt's committedRange is read
				// with the startChapter/endChapter accessor ...
				{Path: "../../internal/store/batch_progress.go", Line: 1235,
					Marker: `committed, committedOK := bpRangeObject(receipt["committedRange"])`},
				{Path: "../../internal/store/batch_progress.go", Line: 1138,
					Marker: `start, startOK := rrInteger(object["startChapter"], -9007199254740991)`},
				// ... while the sibling range on the same receipt is read with the
				// from/to accessor, which is the one the reference uses for both.
				{Path: "../../internal/store/batch_progress.go", Line: 1241,
					Marker: `requested.StartChapter != bpFirstInt(requestedObject, "from", -1) ||`},
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 205,
					Marker: "|| !receipt.committedRange || !Number.isSafeInteger(receipt.committedRange.from) || !Number.isSafeInteger(receipt.committedRange.to)"},
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 211,
					Marker: "if (!committed || committed.payload_hash !== receipt.payloadHash || committed.start_chapter !== receipt.committedRange.from || committed.end_chapter !== receipt.committedRange.to)"},
				// The commit-operation agreement, the last check both sides make.
				{Path: "../../internal/store/batch_progress.go", Line: 1263,
					Marker: "rows, err := rrMaps(ctx, conn, `SELECT payload_hash,start_chapter,end_chapter FROM blueprint_commit_operations\n WHERE operation_id=?`, operationID)"},
				// And the call that makes the check part of every recovery read.
				{Path: "../../internal/store/batch_progress.go", Line: 992,
					Marker: `if code := bpAuthorEditCode(ctx, conn, run); code != "" {`},
			},
		},
		{
			// The planner continuation receipt. Nothing in this module reads it,
			// and nothing here can write it either.
			Name:    productionRecoveryServePieceContinuation,
			Verdict: productionRecoveryServeAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/repositories/generation-run-repository.ts", Line: 179,
					Marker: "readPlanningContinuation(runId: string): PlanningContinuationReceipt | null"},
				// The one state it short-circuits.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 787,
					Marker: "if (continued) return { ...base, state: 'continued', nextHandle: continued.nextHandle }"},
				// The effect bag it lives in is refused for an ordinary binding, so
				// no run frozen by this lane can ever carry it.
				{Path: "../../internal/store/generation_ledger.go", Line: 511,
					Marker: `for _, k := range []string{"$runEffects", "$blueprintRetirement"} {`},
				{Path: "../../internal/store/generation_ledger.go", Line: 513,
					Marker: `return glError("GENERATION_EFFECT_UNSUPPORTED")`},
				// And the planning operation that would read it cannot be opened.
				{Path: "../../internal/store/generation_ledger.go", Line: 719,
					Marker: `if glPlanning(glText(request["operation"])) {`},
			},
		},
		{
			// The binding rebuild. It exists, and it is a different answer.
			Name:    productionRecoveryServePieceRebuild,
			Verdict: productionRecoveryServeSubstituted,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 806,
					Marker: "current = current && isDeepStrictEqual(projection(run.binding), projection(deps.rebuildBinding(run.binding, currentModelReceipt(run.binding))))"},
				// The nine-field fingerprint the projection compares, minus two keys.
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 804,
					Marker: "fingerprint: Object.fromEntries(Object.entries(binding.fingerprint).filter(([key]) => !['contextSnapshotHash', 'chapterBriefHash'].includes(key)))"},
				// The reference's whole-binding comparison, for the other branch.
				{Path: "../../../electron/services/generation-source-binding.ts", Line: 623,
					Marker: "return previous.projectId === next.projectId && isDeepStrictEqual(previous.fingerprint, next.fingerprint) && previous.contextSnapshotId === next.contextSnapshotId && isDeepStrictEqual(previous.sourceManifest, next.sourceManifest)"},
				// The Go stand-in, and the single digest it produces instead.
				{Path: "../../internal/generationowner/binding.go", Line: 63,
					Marker: "func (r *Registry) ReproduceBinding(ctx context.Context,scope Scope,leaseID string,sources Sources,selection []SourceKey)(*Binding,error){"},
				{Path: "../../internal/generationowner/binding.go", Line: 67,
					Marker: "binding.digest=hashJSON("},
				// The only currency key this lane can compare, written by the Go
				// owner's own freeze and absent from a desktop-frozen run.
				{Path: "synthetic_generation_linux.go", Line: 741,
					Marker: `"goOwnerSourceFingerprint":   fingerprint,`},
				{Path: "production_agent_generation_serve_linux.go", Line: 916,
					Marker: `fingerprint := syntheticString(resolved.Manifest["goOwnerSourceFingerprint"])`},
			},
		},
		{
			// The blueprint rows reader: the one obstacle the session removed.
			Name:    "BlueprintRepository.getAll",
			Verdict: productionRecoveryServePorted,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 800,
					Marker: "const stored = new Map(BlueprintRepository.getAll(deps.database).map(item => [item.chapterNumber, item]))"},
				{Path: "../../internal/store/blueprints.go", Line: 69,
					Marker: "func (b *Blueprints) All(ctx context.Context) ([]map[string]any, error) {"},
				{Path: "../../internal/transport/editor_stage_read.go", Line: 42,
					Marker: `names := []string{"db:project-core-get", "db:blueprint-get-all"}`},
			},
		},
		{
			// The projection itself: four of its five fields are the owner's.
			Name:    "blueprintRecovery",
			Verdict: productionRecoveryServeAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 939,
					Marker: "sourceHandle: handleOf(run), leaseEpoch: deps.epoch, draft: viewOf(run).candidates?.at(-1)?.text ?? '',"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 942,
					Marker: "writeState: blueprintSaved ? 'author-saved' as const : continuation.state === 'save-prefix' ? 'ready' as const : continuation.state as Exclude<PlanningContinuation['state'], 'save-prefix' | 'source-invalid'>,"},
				// The wire shape the projection has to fill.
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 185,
					Marker: "export type BlueprintRecovery = {"},
				{Path: "../../../src/shared/generation-owner-contract.ts", Line: 187,
					Marker: "writeState: 'ready' | 'in-flight' | 'source-changed' | 'author-saved' | 'continued' | 'complete'"},
				// And the lane's own context read stops one operation short of it.
				{Path: "production_draft_context_linux.go", Line: 720,
					Marker: "if syntheticString(manifest[\"operation\"]) != productionDraftContextOperation {"},
				{Path: "production_draft_context_linux.go", Line: 721,
					Marker: `return errors.New("GENERATION_CONTEXT_UNSUPPORTED")`},
			},
		},
		{
			// The short-outline retry readback this session added: real, and not
			// the drafts bucket.
			Name:    "draftShortOutline",
			Verdict: productionRecoveryServeSubstituted,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_retry_draft_outline_linux.go", Line: 101,
					Marker: "task, nonce, storedHash, err := productionRetryReadFrozenTask(ctx, db, failedAttemptID)"},
				{Path: "../../../electron/services/main-generation-owner.ts", Line: 1037,
					Marker: "if (view.inFlight || recovery.composition || recovery.draftShortOutline?.completedOutput || recovery.draftShortOutline?.retry?.kind === 'available'"},
			},
		},
		{
			// The reviews bucket's only port, still unbound.
			Name:    "review-revision:read-recovery",
			Verdict: productionRecoveryServeAbsent,
			Evidence: []productionGenerationRecoveryLine{
				{Path: "production_review_revision_linux.go", Line: 32,
					Marker: "review-revision:read-recovery    NOT bound (see below)"},
			},
		},
	}
}

// productionRecoveryServeVerdictKnown reports whether one verdict is one this
// file publishes. A typo therefore fails startup instead of being recorded as a
// new kind of answer.
func productionRecoveryServeVerdictKnown(verdict string) bool {
	switch verdict {
	case productionRecoveryServePorted, productionRecoveryServeValidated,
		productionRecoveryServeRefuted, productionRecoveryServeAbsent,
		productionRecoveryServeSubstituted:
		return true
	}
	return false
}

// productionRecoveryServePieceFor looks one piece up by name, so the test can
// assert the decisive verdict without depending on the table's order.
func productionRecoveryServePieceFor(name string) (productionRecoveryServePiece, bool) {
	for _, piece := range productionRecoveryServePieces() {
		if piece.Name == name {
			return piece, true
		}
	}
	return productionRecoveryServePiece{}, false
}

// productionRecoveryServeAgrees checks the decision table against the seam table
// it belongs to. A refused name must be a refused seam (otherwise the refusal has
// no documented gap), a served name must NOT be one (otherwise the seam is stale),
// and every seam must be decided - so a name can never be refused twice or
// quietly forgotten.
func productionRecoveryServeAgrees(decisions []productionRecoveryServeDecision,
	seams []productionGenerationRecoverySeam, pieces []productionRecoveryServePiece) error {
	if len(decisions) == 0 || len(pieces) == 0 {
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_INCOMPLETE")
	}
	seamByOperation := make(map[string]bool, len(seams))
	for _, seam := range seams {
		if seam.Operation == "" || len(seam.Missing) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SEAM_INCOMPLETE")
		}
		seamByOperation[seam.Operation] = true
	}
	decided := make(map[string]bool, len(decisions))
	for _, decision := range decisions {
		if decision.Operation == "" || decided[decision.Operation] {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_INVALID")
		}
		decided[decision.Operation] = true
		if !decision.Served && !seamByOperation[decision.Operation] {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_UNSEAMED")
		}
		if decision.Served && seamByOperation[decision.Operation] {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_STILL_SEAMED")
		}
		if decision.Served && decision.Reason != "" {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_SERVED_REASON")
		}
		if !decision.Served && decision.Reason != productionRecoveryServeReasonWriteState &&
			decision.Reason != productionRecoveryServeReasonBuckets {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_REASON_INVALID")
		}
	}
	for operation := range seamByOperation {
		if !decided[operation] {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_SEAM_UNDECIDED")
		}
	}
	named := make(map[string]bool, len(pieces))
	for _, piece := range pieces {
		if piece.Name == "" || named[piece.Name] || !productionRecoveryServeVerdictKnown(piece.Verdict) ||
			len(piece.Evidence) == 0 {
			return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_PIECE_INVALID")
		}
		named[piece.Name] = true
	}
	// The two pieces this decision rests on have to be present, whatever else the
	// table grows: a verdict that is missing the very receipt it says is validated
	// (or the rebuild it says is substituted) is not a decision.
	if !named[productionRecoveryServePieceAuthorEdit] || !named[productionRecoveryServePieceRebuild] ||
		!named[productionRecoveryServePieceContinuation] {
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_PIECE_MISSING")
	}
	return nil
}

// bindProductionGenerationRecoveryServe installs nothing and makes the decision
// above a startup fact: each verdict must agree with the finished lane's bound
// set, and each piece must still be spelled out. It runs beside the recovery
// binder (production_generation_recovery_linux.go) and moves no count.
func bindProductionGenerationRecoveryServe(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	decisions := productionRecoveryServeDecisions()
	if err := productionRecoveryServeAgrees(decisions, productionGenerationRecoverySeams(),
		productionRecoveryServePieces()); err != nil {
		return err
	}
	bound := server.BoundOperations()
	for _, decision := range decisions {
		// A decision that disagrees with the lane is a startup error: "refused" has
		// to mean the name is really unbound (fail-closed at
		// GO_OPERATION_NOT_MIGRATED), and "served" has to mean this lane really
		// installed a handler for it.
		if productionRecoveryBoundContains(bound, decision.Operation) == decision.Served {
			continue
		}
		return errors.New("PRODUCTION_GENERATION_RECOVERY_SERVE_DECISION_DRIFT")
	}
	return nil
}

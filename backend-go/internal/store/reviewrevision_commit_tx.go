package store

// The two review-revision names this lane can serve, and nothing else:
//
//	review-revision:commit-revision   ReviewRevisionGeneration.commitRevision
//	review-revision:read-recovery     ReviewRevisionGeneration.readRecovery
//
// References: electron/services/review-revision-generation.ts:234-271
// (commitRevision) and :272-300 (readRecovery); the repositories behind them are
// electron/repositories/revision-repository.ts:150-212 (replacePending),
// electron/repositories/generation-run-repository.ts:460-516 / :659-674 (the
// visible composition and its read) and :85-93 (textHash).
//
// # Why this file exists
//
// commit-revision is ONE transaction: the revision row, its content row, the
// discarded-pending sweep and the effect stamped on the attempt that settled the
// last composed artifact. Every one of those writes is a statement in this file,
// executed on the reserved connection that holds BEGIN IMMEDIATE, so a refusal at
// any point leaves the author's database file byte for byte as it was found.
// store.ReplacePending (reviewrevision.go:301) opens its own immediate
// transaction, so it cannot be joined to the stamp; rvcReplacePending below is the
// same four statements on the connection that already holds the write lock.
//
// read-recovery is a read. It projects the run's own frozen context, the receipt a
// previous commit stamped (if any), the composition, the run's attempts and the
// latest candidate, and it answers the reference's ReviewRevisionRecovery shape
// with the reference's own nulls. It opens one deferred read transaction on one
// reserved connection, so the receipt it answers is a projection of the same
// snapshot as everything beside it: no second unlocked read follows a receipt.
//
// # The proof this lane can and cannot reproduce
//
// The previous pass recorded that commit-revision needs "the visible-text gate"
// with no port. That half was wrong. The gate is two pure functions:
//
//	src/shared/visible-text-integrity.ts:26 assertMechanicallyCompleteVisibleText
//	src/services/workflows/commands/refinement-completeness.ts:31
//	  assertMateriallyCompleteRevision
//
// and both are reproducible from what this package already has. The visible prose
// count `/[\p{L}\p{N}]/gu` is dcLetterDigitCount (draft_commit_tx.go:1129); the
// refinement unit count (English words + BMP Han + every other visible code unit)
// is draftalg.CountLegacyDraftUnitsV1, which is the same expression
// draft-units.ts:25-34 spells and which this package already ships; the
// JavaScript \s set, String#trim, the paragraph folds and the duplicate-paragraph
// rule are the constants below. isNoopRevision (src/shared/review-cycle.ts:95-98)
// is a two-line projection over White_Space and Cf. So rvcAssertMateriallyComplete
// and rvcIsNoopRevision are ports, not inventions, and this name is served.
//
// What is NOT reproducible is read-recovery's `sourceStatus`. The reference asks
// `compareGenerationSourceBindings(run.binding, deps.rebuildBinding(run.binding,
// currentModelReceipt(run.binding)))` (main-generation-owner.ts:1005-1007), whose
// rebuild is buildGenerationSourceBinding over every material the run froze -
// drafts, finalized history, blueprints, roster, templates, installed skills,
// policy and the model lease (generation-source-binding.ts:520-620). No Go port of
// that rebuild exists, and a narrower comparison would answer "current" where the
// reference answers "conflict", which is the unsafe direction: the renderer reads
// `canResume` to decide whether to spend another model call. This file therefore
// answers the reference's own fallback - 'conflict' from the catch at
// review-revision-generation.ts:276 - and canResume is false by the reference's
// own formula (:286 `current && !inFlight`). A committed run is still fully
// readable: `saved` short-circuits the renderer's source gate
// (review-revision-recovery-workflow.ts:20), and it is proved here against the
// revision row, its frozen source and the composition it was composed from.
//
// # The review cycle a revision belongs to
//
// The reference attaches a generated revision to a cycle only through
// `context.confirmation` (review-revision-generation.ts:260-268 ->
// ReviewCycleRepository.attachGeneratedRevision). This lane's producer refuses to
// freeze a confirmation (review_revision_context.go:130-132), so a context that
// carries one - written by the desktop app into the same database - is refused
// here as unproven (rccFrozenContext :354-357) instead of committing a revision
// whose cycle binding cannot be proved. That is the whole of the cycle contract
// this lane can honour: no revision it writes is ever attached to a cycle, and it
// never claims one is.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
)

var (
	// ErrRevisionCommitCompositionRequired is
	// GENERATION_REVIEW_COMPOSITION_REQUIRED: the run has no stored composition,
	// or the one it has is not the composition the caller named (:245-246).
	ErrRevisionCommitCompositionRequired = errors.New("GENERATION_REVIEW_COMPOSITION_REQUIRED")
	// ErrRevisionCommitEmpty is GENERATION_REVIEW_REVISION_EMPTY: the composed text
	// is blank after the reference's own trim (:249-250).
	ErrRevisionCommitEmpty = errors.New("GENERATION_REVIEW_REVISION_EMPTY")
	// ErrRevisionCommitIncomplete is the symbolic stand-in for the reference's
	// localized refusal from assertMateriallyCompleteRevision
	// (refinement-completeness.ts:48-52). The reference throws a human sentence
	// with no code; this lane may only publish codes, so the one refusal is named.
	ErrRevisionCommitIncomplete = errors.New("GENERATION_REVIEW_REVISION_INCOMPLETE")
	// ErrRevisionCommitNoop is GENERATION_REVIEW_REVISION_NOOP: the revision is the
	// source once White_Space and Cf are removed (:252).
	ErrRevisionCommitNoop = errors.New("GENERATION_REVIEW_REVISION_NOOP")
)

// ReviewRevisionCommitRequest is RevisionGenerationCommitRequest
// (src/shared/review-revision-generation.ts:139-143) plus the admitted identity:
// exactly `{contextId, handle, expectedCompositionHash}` on the wire, with the
// project and the live lease taken from the frozen session.
type ReviewRevisionCommitRequest struct {
	ProjectID               string
	LiveEpoch               string
	Handle                  ReviewCommitHandle
	ExpectedCompositionHash string
	// AssertPrepared is called once, after the run identity, the operation and any
	// saved receipt have been proved and before anything is written. It keeps the
	// prepared-context registry in the layer that owns it.
	AssertPrepared func(operation, frozenText string) error
}

// ReviewRevisionReceipt is ReviewRevisionCommitReceipt
// (src/shared/review-revision-generation.ts:144-155) for kind "revision".
type ReviewRevisionReceipt struct {
	Success        bool               `json:"success"`
	Kind           string             `json:"kind"`
	ID             int64              `json:"id"`
	Index          int64              `json:"index"`
	Content        string             `json:"content"`
	ContentHash    string             `json:"contentHash"`
	Source         ReviewCommitSource `json:"source"`
	RevisionStatus string             `json:"revisionStatus,omitempty"`
}

// ReviewRevisionRecoveryRequest is `{handle}` plus the admitted project.
type ReviewRevisionRecoveryRequest struct {
	ProjectID string
	Handle    ReviewCommitHandle
}

// ReviewRevisionRecovery is ReviewRevisionRecovery
// (src/shared/review-revision-generation.ts:156-172). Every optional field keeps
// the reference's own absence: `saved`, `composition` and `latestArtifact` are
// omitted when the reference omits them, while the two finish reasons are always
// present and null when unknown.
type ReviewRevisionRecovery struct {
	Handle                      map[string]any        `json:"handle"`
	Context                     json.RawMessage       `json:"context"`
	ModelID                     string                `json:"modelId"`
	SourceStatus                string                `json:"sourceStatus"`
	InFlight                    bool                  `json:"inFlight"`
	CanResume                   bool                  `json:"canResume"`
	ContextID                   string                `json:"contextId,omitempty"`
	Saved                       *ReviewRevisionReceipt `json:"saved,omitempty"`
	Composition                 map[string]any        `json:"composition,omitempty"`
	LastCompositionFinishReason *string               `json:"lastCompositionFinishReason"`
	LatestArtifact              map[string]any        `json:"latestArtifact,omitempty"`
	LatestArtifactFinishReason  *string               `json:"latestArtifactFinishReason"`
	AttemptedPurposes           []string              `json:"attemptedPurposes"`
}

// The reference's two answers for sourceStatus (:276, :297).
const (
	// ReviewRevisionSourceCurrent is 'current'.
	ReviewRevisionSourceCurrent = "current"
	// ReviewRevisionSourceConflict is 'conflict', also the answer when the
	// reference's own rebuild throws.
	ReviewRevisionSourceConflict = "conflict"
)

// rvcEffectRow is one attempt that already carries a reviewRevisionEffect.
type rvcEffectRow struct {
	attemptID string
	effect    map[string]any
}

// CommitRevision is ReviewRevisionGeneration.commitRevision: one transaction that
// writes the pending revision, its content, the discarded-pending sweep and the
// effect on the attempt that settled the last composed artifact, then re-reads the
// receipt from the rows it just wrote and answers that.
func (r *ReviewRevisions) CommitRevision(ctx context.Context, request ReviewRevisionCommitRequest) (*ReviewRevisionReceipt, error) {
	if r == nil || r.db == nil {
		return nil, ErrReviewRevisionProjectNotOpen
	}
	if ctx == nil || summaryTrim(request.ProjectID) == "" || summaryTrim(request.LiveEpoch) == "" ||
		summaryTrim(request.Handle.ProjectID) == "" || summaryTrim(request.Handle.Epoch) == "" ||
		summaryTrim(request.Handle.RootActionID) == "" || summaryTrim(request.Handle.RunID) == "" ||
		summaryTrim(request.ExpectedCompositionHash) == "" {
		return nil, ErrReviewCommitInvalid
	}
	var receipt *ReviewRevisionReceipt
	err := r.immediate(ctx, func(conn *sql.Conn) error {
		value, err := rvcCommitBody(ctx, conn, request)
		if err != nil {
			return err
		}
		receipt = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return receipt, nil
}

// rvcCommitBody is the transaction body, in the reference's own order.
func rvcCommitBody(ctx context.Context, conn *sql.Conn, request ReviewRevisionCommitRequest) (*ReviewRevisionReceipt, error) {
	ledger := NewGenerationLedger(nil, nil, nil)
	if err := ledger.mutableRoot(ctx, conn, request.Handle.RootActionID); err != nil {
		return nil, err
	}
	rootID, binding, err := rccOrderedBinding(ctx, conn, request.Handle.RunID)
	if err != nil {
		return nil, err
	}
	// requireRun: the handle must name this run, field for field.
	if rootID != request.Handle.RootActionID || request.Handle.ProjectID != request.ProjectID ||
		glText(glField(binding, "projectId")) != request.ProjectID ||
		glText(glField(binding, "epoch")) != request.Handle.Epoch {
		return nil, ErrReviewCommitIdentity
	}
	frozen, text, hash, err := rccFrozenContext(binding)
	if err != nil {
		return nil, err
	}
	if frozen.Operation == ReviewRevisionOperationReviewChapter {
		return nil, ErrReviewCommitInvalid
	}
	if !rvcOperation(frozen.Operation) {
		return nil, ErrReviewCommitContextUnproven
	}
	// saved() answers a replay before any freshness proof, exactly as the reference
	// does (:239-242 runs before assertPrepared at :243).
	saved, savedRef, err := rvcSavedReceipt(ctx, conn, request.ProjectID, request.Handle.RunID, rootID, binding, frozen, text, hash)
	if err != nil {
		return nil, err
	}
	if saved != nil {
		if savedRef == nil || savedRef.compositionHash != request.ExpectedCompositionHash {
			return nil, ErrReviewCommitConflict
		}
		return saved, nil
	}
	if request.AssertPrepared != nil {
		if err := request.AssertPrepared(frozen.Operation, text); err != nil {
			return nil, err
		}
	}
	// assertSourcesCurrent: the caller must still be leased on the run's epoch and
	// the root must not be cancelled.
	if request.LiveEpoch != glText(glField(binding, "epoch")) {
		return nil, ErrReviewCommitContextChanged
	}
	action, err := rccRootAction(ctx, conn, request.Handle.RootActionID)
	if err != nil {
		return nil, err
	}
	if glText(action["status"]) == "cancelled" {
		return nil, ErrReviewCommitCancelled
	}
	// The frozen context must still be exactly what a capture of the same request
	// produces, before the first write.
	if err := rvcReprooveFrozen(ctx, conn, request.ProjectID, frozen, text, hash); err != nil {
		return nil, err
	}
	composition, err := rvcComposition(ctx, conn, request.Handle.RunID, rootID, binding)
	if err != nil {
		return nil, err
	}
	if composition == nil || composition.Algorithm != draftalg.VisibleContinuationVersion ||
		composition.TextHash != request.ExpectedCompositionHash || len(composition.Sources) == 0 {
		return nil, ErrRevisionCommitCompositionRequired
	}
	last := composition.Sources[len(composition.Sources)-1]
	artifact := ReviewCommitArtifact{ArtifactID: last.artifactID, Revision: last.revision, TextHash: last.textHash}
	// proveArtifact(:248) runs before the content is read out of the composition,
	// so an artifact the run does not own can never become the author's chapter.
	// The attempt it resolves is the row the effect is stamped on (:186-189).
	_, attemptID, err := rccProveArtifact(ctx, conn, request.Handle.RunID, binding, artifact)
	if err != nil {
		return nil, err
	}
	content := trimJS(composition.Text)
	if content == "" {
		return nil, ErrRevisionCommitEmpty
	}
	if err := rvcAssertMateriallyComplete(frozen.Source.Content, content, rvcTargetUnits(text)); err != nil {
		return nil, err
	}
	if rvcIsNoopRevision(frozen.Source.Content, content) {
		return nil, ErrRevisionCommitNoop
	}
	source := rrSource{
		id:      frozen.Source.ID,
		chapter: frozen.Source.ChapterNumber,
		version: frozen.Source.Version,
		status:  frozen.Source.Status,
		content: frozen.Source.Content,
	}
	kind := "refine"
	if frozen.Operation == ReviewRevisionOperationRefineFromReview {
		kind = "review-fix"
	}
	revisionID, index, err := rvcReplacePending(ctx, conn, source, kind,
		rvcUserPrompt(frozen.AuthorInputs), content, int64(draftalg.CountDraftUnits(content)))
	if err != nil {
		return nil, err
	}
	contentHash := recoveryHash(content)
	effect := plotObject{
		{"kind", "revision"},
		{"id", revisionID},
		{"index", index},
		{"contentHash", contentHash},
		{"compositionHash", composition.TextHash},
		{"artifact", plotObject{
			{"artifactId", artifact.ArtifactID},
			{"revision", artifact.Revision},
			{"textHash", artifact.TextHash},
		}},
		{"contextHash", hash},
	}
	if err := rccStampEffect(ctx, conn, request.Handle.RunID, attemptID, effect); err != nil {
		return nil, err
	}
	// Re-read the receipt from the rows just written: the caller never sees an
	// answer this transaction did not prove.
	receipt, _, err := rvcSavedReceipt(ctx, conn, request.ProjectID, request.Handle.RunID, rootID, binding, frozen, text, hash)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, ErrReviewCommitReceipt
	}
	return receipt, nil
}

// rvcOperation reports whether an operation belongs to REVIEW_REVISION_OPERATIONS
// (review-revision-context.ts:19).
func rvcOperation(operation string) bool {
	return operation == ReviewRevisionOperationReviewChapter ||
		operation == ReviewRevisionOperationRefineDraft ||
		operation == ReviewRevisionOperationRefineFromReview
}

// rvcTargetUnits is `context.config.wordsPerChapter` as the reference's
// Number.isSafeInteger/target>0 gate reads it: a value that is not a positive
// integer is not a target, and the gate then bounds by the source alone.
func rvcTargetUnits(text string) int64 {
	var frozen struct {
		Config struct {
			WordsPerChapter any `json:"wordsPerChapter"`
		} `json:"config"`
	}
	if err := json.Unmarshal([]byte(text), &frozen); err != nil {
		return 0
	}
	value, ok := rrInteger(frozen.Config.WordsPerChapter, 1)
	if !ok {
		return 0
	}
	return value
}

// rvcUserPrompt is `context.authorInputs.find(item => item.id === 'user-prompt')
// ?.text ?? ''` (review-revision-generation.ts:256). The producer admits that id
// for refine-draft only (review-revision-context.ts:46), so a context without one
// stores the empty prompt the reference stores.
func rvcUserPrompt(inputs []any) string {
	for _, value := range inputs {
		entry := glMap(value)
		if entry == nil || glText(entry["id"]) != "user-prompt" {
			continue
		}
		if text, ok := entry["text"].(string); ok {
			return text
		}
	}
	return ""
}

// rvcReprooveFrozen captures the same request again on the transaction's own
// connection and requires the frozen text and hash to match exactly. It is the
// same proof rccReprooveFrozen makes for commit-review; it is repeated here
// because the review-cycle file's helper takes the review commit request.
func rvcReprooveFrozen(ctx context.Context, conn *sql.Conn, projectID string, frozen rccFrozen, text, hash string) error {
	values, err := rrctxRequest(map[string]any{
		"operation": frozen.Operation,
		"draftId":   frozen.Source.ID,
		"expectedDraft": map[string]any{
			"chapterNumber": frozen.Source.ChapterNumber,
			"version":       frozen.Source.Version,
			"status":        frozen.Source.Status,
			"contentHash":   frozen.SourceHash,
		},
		"authorInputs": frozen.AuthorInputs,
		"uiLocale":     frozen.UILocale,
	})
	if err != nil {
		return ErrReviewCommitContextChanged
	}
	fresh, err := rrctxCapture(ctx, conn, projectID, values)
	if err != nil {
		return ErrReviewCommitContextChanged
	}
	if fresh.Text != text || fresh.ContextHash != hash || fresh.SourceHash != frozen.SourceHash {
		return ErrReviewCommitContextChanged
	}
	return nil
}

// rvcComposition is readVisibleComposition(runId)
// (generation-run-repository.ts:659-674): the newest stored visibleComposition
// receipt of the run, recomposed from the run's own artifacts and required to
// still describe them. A run with no receipt has no composition.
func rvcComposition(ctx context.Context, conn *sql.Conn, runID, rootID string, binding any) (*dcComposed, error) {
	run := &bpRun{runID: runID, rootID: rootID, binding: glMap(binding)}
	return dcComposition(ctx, conn, run)
}

// rvcCompositionReceipt is the VisibleCompositionReceipt projection the recovery
// answers (src/shared/generation-owner-contract.ts:39-46): algorithm, textHash,
// artifactIds and the sources, never the text.
func rvcCompositionReceipt(composed *dcComposed) map[string]any {
	if composed == nil {
		return nil
	}
	sources := make([]any, 0, len(composed.Sources))
	for _, source := range composed.Sources {
		sources = append(sources, map[string]any{
			"artifactId": source.artifactID,
			"revision":   source.revision,
			"textHash":   source.textHash,
		})
	}
	return map[string]any{
		"algorithm":   composed.Algorithm,
		"textHash":    composed.TextHash,
		"artifactIds": append([]string{}, composed.ArtifactIDs...),
		"sources":     sources,
	}
}

// rvcSaved is saved(): the run's single receipt re-proved against the rows it
// names, plus the composition the receipt was stamped with.
type rvcSaved struct {
	receipt         *ReviewRevisionReceipt
	compositionHash string
}

// rvcSavedReceipt is saved() for either half of the family
// (review-revision-generation.ts:93-118). A review-chapter context answers the
// review receipt rccSavedReview already proves for commit-review; a refine context
// answers the revision receipt rvcSavedRevision proves below. The reference reads
// the row through ReviewRepository or RevisionRepository by the effect's own kind,
// and this dispatches on the operation the frozen context names, which the effect
// is required to match.
func rvcSavedReceipt(ctx context.Context, conn *sql.Conn, projectID, runID, rootID string, binding any,
	frozen rccFrozen, text, hash string) (*ReviewRevisionReceipt, *rvcSaved, error) {
	if frozen.Operation == ReviewRevisionOperationReviewChapter {
		receipt, _, err := rccSavedReview(ctx, conn, ReviewCommitRequest{
			ProjectID: projectID,
			Handle:    ReviewCommitHandle{RunID: runID},
		}, binding, frozen, text, hash)
		if err != nil || receipt == nil {
			return nil, nil, err
		}
		return &ReviewRevisionReceipt{
			Success:     receipt.Success,
			Kind:        receipt.Kind,
			ID:          receipt.ID,
			Index:       receipt.Index,
			Content:     receipt.Content,
			ContentHash: receipt.ContentHash,
			Source:      receipt.Source,
		}, &rvcSaved{}, nil
	}
	return rvcSavedRevision(ctx, conn, runID, rootID, binding, frozen, hash)
}

// rvcSavedRevision is saved() for a revision effect
// (review-revision-generation.ts:93-118). It returns nil when the run has no
// receipt yet, and a refusal for any receipt whose rows no longer prove it.
func rvcSavedRevision(ctx context.Context, conn *sql.Conn, runID, rootID string, binding any,
	frozen rccFrozen, hash string) (*ReviewRevisionReceipt, *rvcSaved, error) {
	rows, err := rrMaps(ctx, conn,
		"SELECT attempt_id,usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, nil, err
	}
	found := make([]rvcEffectRow, 0, 1)
	for _, row := range rows {
		usage, err := glStored(row["usage_receipt_json"])
		if err != nil {
			return nil, nil, ErrReviewCommitReceipt
		}
		value, exists := usage["reviewRevisionEffect"]
		if !exists || value == nil {
			continue
		}
		effect := glMap(value)
		if effect == nil {
			return nil, nil, ErrReviewCommitReceipt
		}
		found = append(found, rvcEffectRow{attemptID: glText(row["attempt_id"]), effect: effect})
	}
	if len(found) > 1 {
		return nil, nil, ErrReviewCommitReceipt
	}
	if len(found) == 0 {
		return nil, nil, nil
	}
	effect := found[0].effect
	artifact := glMap(effect["artifact"])
	if artifact == nil || len(artifact) != 3 || glText(effect["kind"]) != "revision" {
		return nil, nil, ErrReviewCommitReceipt
	}
	// The reference refuses a revision effect that also carries a report version
	// (:99-100): a report version belongs to a review.
	if _, present := effect["reportVersion"]; present {
		return nil, nil, ErrReviewCommitReceipt
	}
	revision, revisionOK := rrInteger(artifact["revision"], 0)
	if !revisionOK || !rccHashDigest(glText(artifact["textHash"])) ||
		summaryTrim(glText(artifact["artifactId"])) == "" {
		return nil, nil, ErrReviewCommitReceipt
	}
	ref := ReviewCommitArtifact{
		ArtifactID: glText(artifact["artifactId"]),
		Revision:   revision,
		TextHash:   glText(artifact["textHash"]),
	}
	revisionID, idOK := rrInteger(effect["id"], 1)
	index, indexOK := rrInteger(effect["index"], 1)
	contentHash := glText(effect["contentHash"])
	compositionHash := glText(effect["compositionHash"])
	if !idOK || !indexOK || !rccHashDigest(contentHash) || !rccHashDigest(compositionHash) ||
		glText(effect["contextHash"]) != hash {
		return nil, nil, ErrReviewCommitReceipt
	}
	_, attemptID, err := rccProveArtifact(ctx, conn, runID, binding, ref)
	if err != nil {
		return nil, nil, err
	}
	if attemptID != found[0].attemptID {
		return nil, nil, ErrReviewCommitReceipt
	}
	composition, err := rvcComposition(ctx, conn, runID, rootID, binding)
	if err != nil {
		return nil, nil, err
	}
	if composition == nil || composition.Algorithm != draftalg.VisibleContinuationVersion ||
		composition.TextHash != compositionHash || len(composition.ArtifactIDs) == 0 ||
		composition.ArtifactIDs[len(composition.ArtifactIDs)-1] != ref.ArtifactID {
		return nil, nil, ErrReviewCommitReceipt
	}
	body := trimJS(composition.Text)
	row, err := rrFull(rrMaps(ctx, conn, "SELECT "+rrRevisionMeta+rrSourceColumns+
		" FROM revisions r LEFT JOIN contents c ON c.id=r.content_id WHERE r.id=?", revisionID))
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		return nil, nil, ErrReviewCommitReceipt
	}
	content, isText := row["content"].(string)
	baseID, baseOK := rrInteger(row["baseDraftId"], 1)
	storedIndex, storedOK := rrInteger(row["revisionIndex"], 1)
	status, statusOK := row["status"].(string)
	if !isText || !baseOK || !storedOK || !statusOK || baseID != frozen.Source.ID || storedIndex != index ||
		content != body || recoveryHash(content) != contentHash ||
		!glSame(row["sourceDraft"], rccSourceMap(frozen.Source)) {
		return nil, nil, ErrReviewCommitReceipt
	}
	if status != "pending" && status != "merged" && status != "discarded" {
		return nil, nil, ErrReviewCommitReceipt
	}
	receipt := &ReviewRevisionReceipt{
		Success:     true,
		Kind:        "revision",
		ID:          revisionID,
		Index:       index,
		Content:     content,
		ContentHash: contentHash,
		Source: ReviewCommitSource{
			ID:            frozen.Source.ID,
			ChapterNumber: frozen.Source.ChapterNumber,
			Version:       frozen.Source.Version,
			Status:        frozen.Source.Status,
			Content:       frozen.Source.Content,
		},
		RevisionStatus: status,
	}
	return receipt, &rvcSaved{receipt: receipt, compositionHash: compositionHash}, nil
}

// rvcReplacePending is RevisionRepository.replacePending
// (revision-repository.ts:150-212) on the caller's connection: the same source
// assertion, the same MAX(revision_index)+1, the same content row, the same
// revision row and the same discard sweep. The sweep refuses to discard a pending
// revision that a review cycle already owns, exactly as the ported
// store.ReplacePending does (reviewrevision.go:334-345): only the cycle owner may
// detach one.
func rvcReplacePending(ctx context.Context, conn *sql.Conn, source rrSource, kind, prompt, content string,
	count int64) (int64, int64, error) {
	if err := rrAssertSource(ctx, conn, source.id, source); err != nil {
		return 0, 0, err
	}
	// This lane never names a review for a generated revision: a confirmation
	// cannot be frozen, so the reference's reviewSourceId arm is unreachable here.
	if err := rrUnbound(ctx, conn, 0, 0, source.id); err != nil {
		return 0, 0, err
	}
	pending, err := rrMaps(ctx, conn,
		"SELECT id,review_source_id AS reviewSourceId FROM revisions WHERE base_draft_id=? AND status='pending' ORDER BY id",
		source.id)
	if err != nil {
		return 0, 0, err
	}
	for _, row := range pending {
		id, ok := rrInteger(row["id"], 1)
		if !ok {
			return 0, 0, ErrReviewRevisionInvalidBinding
		}
		reviewID, err := rrStoredReviewID(row["reviewSourceId"])
		if err != nil {
			return 0, 0, err
		}
		if err := rrUnbound(ctx, conn, id, reviewID, source.id); err != nil {
			return 0, 0, err
		}
	}
	var index int64
	if err := conn.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(revision_index),0)+1 FROM revisions WHERE base_draft_id=?", source.id).Scan(&index); err != nil {
		return 0, 0, err
	}
	inserted, err := conn.ExecContext(ctx, "INSERT INTO contents(body) VALUES(?)", content)
	if err != nil {
		return 0, 0, err
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO revisions(base_draft_id,revision_index,revision_type,
 user_prompt,review_source_id,source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id,word_count)
 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, source.id, index, kind, prompt, nil, source.chapter, source.version,
		source.status, source.content, contentID, count)
	if err != nil {
		return 0, 0, err
	}
	revisionID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	if _, err := conn.ExecContext(ctx, `UPDATE revisions SET status='discarded',updated_at=datetime('now')
 WHERE base_draft_id=? AND status='pending' AND id<>?`, source.id, revisionID); err != nil {
		return 0, 0, err
	}
	return revisionID, index, nil
}

// ReadRevisionRecovery is ReviewRevisionGeneration.readRecovery: one deferred read
// transaction over the run, its frozen context, its receipt, its composition, its
// attempts and its latest candidate.
func (r *ReviewRevisions) ReadRevisionRecovery(ctx context.Context, request ReviewRevisionRecoveryRequest) (*ReviewRevisionRecovery, error) {
	if r == nil || r.db == nil {
		return nil, ErrReviewRevisionProjectNotOpen
	}
	if ctx == nil || summaryTrim(request.ProjectID) == "" || summaryTrim(request.Handle.ProjectID) == "" ||
		summaryTrim(request.Handle.Epoch) == "" || summaryTrim(request.Handle.RootActionID) == "" ||
		summaryTrim(request.Handle.RunID) == "" {
		return nil, ErrReviewCommitInvalid
	}
	var recovery *ReviewRevisionRecovery
	err := r.rvcRead(ctx, func(conn *sql.Conn) error {
		value, err := rvcRecoveryBody(ctx, conn, request)
		if err != nil {
			return err
		}
		recovery = value
		return nil
	})
	if err != nil {
		return nil, err
	}
	return recovery, nil
}

// rvcRecoveryBody is the read body, in the reference's own order.
func rvcRecoveryBody(ctx context.Context, conn *sql.Conn, request ReviewRevisionRecoveryRequest) (*ReviewRevisionRecovery, error) {
	rootID, binding, err := rccOrderedBinding(ctx, conn, request.Handle.RunID)
	if err != nil {
		return nil, err
	}
	if rootID != request.Handle.RootActionID || request.Handle.ProjectID != request.ProjectID ||
		glText(glField(binding, "projectId")) != request.ProjectID ||
		glText(glField(binding, "epoch")) != request.Handle.Epoch {
		return nil, ErrReviewCommitIdentity
	}
	frozen, text, hash, err := rccFrozenContext(binding)
	if err != nil {
		return nil, err
	}
	if !rvcOperation(frozen.Operation) {
		return nil, ErrReviewCommitContextUnproven
	}
	manifest := glField(binding, "sourceManifest")
	// The reference freezes the model receipt as sourceManifest.modelReceipt
	// (main-generation-owner.ts:296 reads `.modelReceipt.modelId`). This lane's own
	// admission freezes the same value under the key "modelId"
	// (synthetic_generation_linux.go:739), exactly as it freezes the output
	// contract under "output" rather than "outputContract"
	// (production_composition_linux.go:370-386). Reading the reference key first and
	// falling back to this lane's keeps the reference semantics for a real
	// Electron-written database while still serving a run this lane opened.
	modelID := glText(glField(glField(manifest, "modelReceipt"), "modelId"))
	if summaryTrim(modelID) == "" {
		modelID = glText(glField(manifest, "modelId"))
	}
	if summaryTrim(modelID) == "" {
		return nil, ErrReviewCommitContextUnproven
	}
	saved, _, err := rvcSavedReceipt(ctx, conn, request.ProjectID, request.Handle.RunID, rootID, binding, frozen, text, hash)
	if err != nil {
		return nil, err
	}
	composition, err := rvcComposition(ctx, conn, request.Handle.RunID, rootID, binding)
	if err != nil {
		return nil, err
	}
	inFlight, err := (&GenerationLedger{}).inFlight(ctx, conn, request.Handle.RunID)
	if err != nil {
		return nil, err
	}
	purposes, err := rvcAttemptedPurposes(ctx, conn, request.Handle.RunID)
	if err != nil {
		return nil, err
	}
	latest, latestFinish, err := rvcLatestArtifact(ctx, conn, request.Handle.RunID, request.Handle.RootActionID, binding)
	if err != nil {
		return nil, err
	}
	var lastFinish *string
	if composition != nil && len(composition.ArtifactIDs) > 0 {
		finish, err := rvcFinishReason(ctx, conn, composition.ArtifactIDs[len(composition.ArtifactIDs)-1])
		if err != nil {
			return nil, err
		}
		lastFinish = finish
	}
	recovery := &ReviewRevisionRecovery{
		Handle: map[string]any{
			"projectId":    glText(glField(binding, "projectId")),
			"epoch":        glText(glField(binding, "epoch")),
			"rootActionId": rootID,
			"runId":        request.Handle.RunID,
		},
		Context:      json.RawMessage(text),
		ModelID:      modelID,
		SourceStatus: ReviewRevisionSourceConflict,
		InFlight:     inFlight,
		// canResume is `current && !inFlight` (:286) with current false, which is
		// the reference's own formula rather than a second refusal: the two arms
		// that follow it in the reference (:287-295) can only clear the flag, never
		// set it. So no contextId is minted, and the renderer's copy-only path is
		// the honest answer for a run this lane cannot re-prove.
		CanResume:                   false,
		Saved:                       saved,
		Composition:                 rvcCompositionReceipt(composition),
		LastCompositionFinishReason: lastFinish,
		LatestArtifact:              latest,
		LatestArtifactFinishReason:  latestFinish,
		AttemptedPurposes:           purposes,
	}
	return recovery, nil
}

// rvcAttemptedPurposes is `attempts(runId).map(row => JSON.parse(
// row.usage_receipt_json).purpose ?? 'unknown')` (:284).
func rvcAttemptedPurposes(ctx context.Context, conn *sql.Conn, runID string) ([]string, error) {
	rows, err := rrMaps(ctx, conn,
		"SELECT usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", runID)
	if err != nil {
		return nil, err
	}
	purposes := make([]string, 0, len(rows))
	for _, row := range rows {
		usage, err := glStored(row["usage_receipt_json"])
		if err != nil {
			return nil, ErrReviewCommitReceipt
		}
		purpose := glText(usage["purpose"])
		if purpose == "" {
			purpose = "unknown"
		}
		purposes = append(purposes, purpose)
	}
	return purposes, nil
}

// rvcFinishReason is finish(id) (:279-283): the finish reason of the artifact's
// own attempt receipt, or null when the artifact or its attempt is gone.
func rvcFinishReason(ctx context.Context, conn *sql.Conn, artifactID string) (*string, error) {
	rows, err := rrMaps(ctx, conn, `SELECT t.usage_receipt_json AS usage FROM generation_artifacts a
 JOIN generation_attempts t ON t.attempt_id=a.attempt_id WHERE a.artifact_id=?`, artifactID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	usage, err := glStored(rows[0]["usage"])
	if err != nil {
		return nil, ErrReviewCommitReceipt
	}
	result := glMap(usage["result"])
	if result == nil {
		return nil, nil
	}
	finish, ok := result["finishReason"].(string)
	if !ok {
		return nil, nil
	}
	return &finish, nil
}

// rvcLatestArtifact is `snapshot(run)` (:277 -> main-generation-owner.ts:1007
// `viewOf(run).candidates?.at(-1)`) reduced to its read: the last attempt of the
// run whose artifact is not discarded, projected into MainGenerationSnapshot
// (generation-runtime.ts:83-94). It runs on the recovery transaction's own
// connection, so the snapshot cannot describe a later state than the receipt.
func rvcLatestArtifact(ctx context.Context, conn *sql.Conn, runID, rootID string, binding any) (map[string]any, *string, error) {
	rows, err := rrMaps(ctx, conn, `SELECT t.attempt_id AS attemptId,t.attempt_json AS attemptJson,
 t.usage_receipt_json AS usage,a.artifact_id AS artifactId,a.artifact_json AS artifactJson,
 a.revision AS revision,a.status AS artifactStatus
 FROM generation_attempts t LEFT JOIN generation_artifacts a ON a.attempt_id=t.attempt_id
 WHERE t.run_id=? ORDER BY t.rowid`, runID)
	if err != nil {
		return nil, nil, err
	}
	action, err := rccRootAction(ctx, conn, rootID)
	if err != nil {
		return nil, nil, err
	}
	rootStatus := glText(action["status"])
	var latest map[string]any
	var latestFinish *string
	for _, row := range rows {
		if glText(row["artifactId"]) == "" || glText(row["artifactStatus"]) == "discarded" {
			continue
		}
		payload, err := glStored(row["artifactJson"])
		if err != nil {
			return nil, nil, ErrReviewCommitReceipt
		}
		attempt, err := glStored(row["attemptJson"])
		if err != nil {
			return nil, nil, ErrReviewCommitReceipt
		}
		usage, err := glStored(row["usage"])
		if err != nil {
			return nil, nil, ErrReviewCommitReceipt
		}
		text, textOK := payload["text"].(string)
		textHash, hashOK := payload["textHash"].(string)
		revision, revisionOK := rrInteger(payload["revision"], 0)
		if !textOK || !hashOK || !revisionOK || recoveryHash(text) != textHash {
			return nil, nil, ErrReviewCommitReceipt
		}
		result := glMap(usage["result"])
		finish := glText(result["finishReason"])
		failure := glText(result["failureCode"])
		attemptStatus := glText(attempt["status"])
		status := "running"
		switch {
		case attemptStatus != "settled" && attemptStatus != "unknown" && attemptStatus != "cancelled-before-dispatch":
			status = "running"
		case failure != "":
			status = "failed"
		case finish == "stop":
			status = "completed"
		case attemptStatus == "cancelled-before-dispatch" || rootStatus == "cancelled":
			status = "cancelled"
		case finish != "":
			status = "failed"
		default:
			status = "unknown"
		}
		eligible := (attemptStatus == "settled" || attemptStatus == "unknown") && failure == "" &&
			rootStatus != "cancelled" && (finish == "stop" || finish == "length") && trimJS(text) != "" &&
			glText(usage["purpose"]) != dcReconcilePurpose && glText(usage["purpose"]) != dcShortOutlinePurpose
		latest = map[string]any{
			"projectId":           glText(glField(binding, "projectId")),
			"epoch":               glText(payload["epoch"]),
			"rootActionId":        rootID,
			"runId":               runID,
			"artifactId":          glText(row["artifactId"]),
			"attemptId":           glText(row["attemptId"]),
			"revision":            revision,
			"durableRevision":     revision,
			"text":                text,
			"textHash":            textHash,
			"status":              status,
			"compositionEligible": eligible,
		}
		if finish != "" {
			value := finish
			latestFinish = &value
		} else {
			latestFinish = nil
		}
	}
	return latest, latestFinish, nil
}

// rvcRead runs fn inside one deferred read transaction on one reserved
// connection. The author database is opened with MaxOpenConns(1)
// (projectfs/author_linux.go:121), so nothing inside fn may touch the pool; the
// rollback is bounded and runs on a background context so a cancelled request
// cannot leak the transaction.
func (r *ReviewRevisions) rvcRead(ctx context.Context, fn func(*sql.Conn) error) error {
	if r == nil || r.db == nil {
		return ErrReviewRevisionProjectNotOpen
	}
	conn, err := r.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN"); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cleanup, "ROLLBACK")
	}()
	if err := fn(conn); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// The visible-text gate's constants, in the reference's order.

// rvcTruncationMarkerZH and rvcTruncationMarkerEN are the trimmed forms of
// ZH_CN_TRUNCATION_MARKER / EN_US_TRUNCATION_MARKER (visible-text-integrity.ts:4-5).
const (
	rvcTruncationMarkerZH = `…[内容已按上下文预算截断]…`
	rvcTruncationMarkerEN = `…[content truncated to fit the context budget]…`
	// rvcMaxMetaOpeningUnits is MAX_META_OPENING_VISIBLE_UNITS (:6).
	rvcMaxMetaOpeningUnits = 200
	// rvcMinDuplicateUnits is MIN_OBVIOUS_DUPLICATE_PARAGRAPH_VISIBLE_UNITS (:7).
	rvcMinDuplicateUnits = 120
	// rvcMinRevisionUnits is MIN_REFINEMENT_UNITS (refinement-completeness.ts:6).
	rvcMinRevisionUnits = 200
	// rvcRevisionCompletionRatio is MIN_REFINEMENT_COMPLETION_RATIO (:5).
	rvcRevisionCompletionRatio = 0.6
)

// rvcJS is the ECMAScript \s class as a bracketed class. The producer's own
// constant is the same set (review_revision_context.go:181); Go's \s is
// ASCII-only and would let U+3000 and U+00A0 through.
var rvcJS = "[" + rrctxJSWhitespaceClass + "]"

var (
	// /(?:^|\n)\s*```/u (visible-text-integrity.ts:31).
	rvcCodeFencePattern = regexp.MustCompile("(?:^|\n)" + rvcJS + "*" + "```")
	// /<\/?\s*think(?:\s|>|$)/iu (:34).
	rvcThinkTagPattern = regexp.MustCompile("(?i)</?" + rvcJS + "*think(?:" + rvcJS + "|>|$)")
	// /\n\s*\n+/u (:39).
	rvcParagraphBreakPattern = regexp.MustCompile("\n" + rvcJS + "*\n+")
	// /\r?\n/u (:42).
	rvcLineBreakPattern = regexp.MustCompile("\r?\n")
	// /\s+/gu (:67, :73).
	rvcSpaceRunPattern = regexp.MustCompile(rvcJS + "+")
	// The four opening-meta-commentary arms (:46-49).
	rvcMetaOpeningPatterns = [...]*regexp.Regexp{
		regexp.MustCompile(`^(?:以下|下面)(?:是|为)(?:(?:根据|按照)(?:您|用户)(?:的)?(?:要求|指示))?(?:(?:修订|修改|重写|生成|完成|提供)(?:后)?的?)?(?:完整的?)?(?:正文|章节(?:正文)?|内容)` + rvcJS + `*(?:[：:]|[。.!！]?$)`),
		regexp.MustCompile(`(?i)^(?:(?:certainly|as` + rvcJS + `+requested)[,!:]?` + rvcJS + `+)?(?:here|below)` + rvcJS + `+is` + rvcJS + `+(?:(?:the|your)` + rvcJS + `+)?(?:(?:revised|rewritten|generated|complete|full|updated|requested)` + rvcJS + `+)*(?:text|chapter|content|revision|version|draft|prose)` + rvcJS + `*(?:[:：]|[.!]?$)`),
		regexp.MustCompile(`^(?:(?:根据|按照)(?:您|用户).{0,40}(?:要求|指示)|这是(?:我为您|根据您的要求)).{0,30}(?:修订|修改|重写|生成|提供).{0,10}(?:正文|章节|内容)[。！：:]?$`),
		regexp.MustCompile(`(?i)^(?:(?:as` + rvcJS + `+requested|certainly)[,!:]?` + rvcJS + `+)?i(?:'ve|` + rvcJS + `+have|` + rvcJS + `+will)` + rvcJS + `+(?:revised|rewritten|generated|provide|write|revise)` + rvcJS + `+(?:(?:the|your|this|complete|full|revised)` + rvcJS + `+)*(?:text|chapter|content|revision|version|draft|prose)[.!:：]?$`),
	}
	// The truncation paragraph arm (:58).
	rvcTruncationParagraphPattern = regexp.MustCompile(`(?i)^(?:…` + rvcJS + `*)?(?:\[(?:内容已按上下文预算截断|内容截断|输出被截断|content truncated to fit the context budget|truncated)\]|[（(]?(?:未完待续|未完)[）)]?)(?:` + rvcJS + `*…)?$`)
)

// rvcAssertMateriallyComplete is assertMateriallyCompleteRevision
// (refinement-completeness.ts:31-52): the mechanical gate, then the material
// length gate. targetUnits is context.config.wordsPerChapter; a value that is not
// a positive safe integer bounds the target by the source alone.
func rvcAssertMateriallyComplete(source, revision string, targetUnits int64) error {
	if err := rvcAssertMechanicallyComplete(revision, source); err != nil {
		return err
	}
	sourceUnits := draftalg.CountLegacyDraftUnitsV1(source)
	revisionUnits := draftalg.CountLegacyDraftUnitsV1(revision)
	boundedTarget := sourceUnits
	if targetUnits > 0 && int64(sourceUnits) > targetUnits {
		boundedTarget = int(targetUnits)
	}
	// Math.floor(boundedTarget * 0.6) on a double, exactly as the original
	// evaluates it.
	scaled := int(math.Floor(float64(boundedTarget) * rvcRevisionCompletionRatio))
	minimum := rvcMinRevisionUnits
	if scaled > minimum {
		minimum = scaled
	}
	if sourceUnits < minimum {
		minimum = sourceUnits
	}
	if revisionUnits < minimum {
		return ErrRevisionCommitIncomplete
	}
	return nil
}

// rvcAssertMechanicallyComplete is assertMechanicallyCompleteVisibleText
// (visible-text-integrity.ts:26-82). The visible prose count is
// dcLetterDigitCount, this package's port of `/[\p{L}\p{N}]/gu`; the duplicate
// rule runs over the same two groupings the original uses, with the source's own
// paragraph counts as the allowance.
func rvcAssertMechanicallyComplete(content, source string) error {
	trimmed := trimJS(content)
	if dcLetterDigitCount(trimmed) == 0 {
		return ErrRevisionCommitIncomplete
	}
	if rvcCodeFencePattern.MatchString(trimmed) {
		return ErrRevisionCommitIncomplete
	}
	if rvcThinkTagPattern.MatchString(trimmed) {
		return ErrRevisionCommitIncomplete
	}
	paragraphs := rvcParagraphs(rvcParagraphBreakPattern, trimmed)
	opening := ""
	for _, line := range rvcLineBreakPattern.Split(trimmed, -1) {
		if value := trimJS(line); value != "" {
			opening = value
			break
		}
	}
	if dcLetterDigitCount(opening) <= rvcMaxMetaOpeningUnits {
		for _, pattern := range rvcMetaOpeningPatterns {
			if pattern.MatchString(opening) {
				return ErrRevisionCommitIncomplete
			}
		}
	}
	if strings.Contains(trimmed, rvcTruncationMarkerZH) || strings.Contains(trimmed, rvcTruncationMarkerEN) {
		return ErrRevisionCommitIncomplete
	}
	for _, paragraph := range paragraphs {
		if rvcTruncationParagraphPattern.MatchString(paragraph) {
			return ErrRevisionCommitIncomplete
		}
	}
	for _, split := range []*regexp.Regexp{rvcParagraphBreakPattern, rvcLineBreakPattern} {
		allowance := make(map[string]int, len(paragraphs))
		for _, paragraph := range split.Split(source, -1) {
			allowance[rvcNormalizeParagraph(paragraph)]++
		}
		seen := make(map[string]int, len(paragraphs))
		for _, paragraph := range split.Split(trimmed, -1) {
			normalized := rvcNormalizeParagraph(paragraph)
			if dcLetterDigitCount(normalized) < rvcMinDuplicateUnits {
				continue
			}
			seen[normalized]++
			allowed := allowance[normalized]
			if allowed < 1 {
				allowed = 1
			}
			if seen[normalized] > allowed {
				return ErrRevisionCommitIncomplete
			}
		}
	}
	return nil
}

// rvcParagraphs is `.split(/\n\s*\n+/u).map(trim).filter(Boolean)`.
func rvcParagraphs(pattern *regexp.Regexp, text string) []string {
	parts := pattern.Split(text, -1)
	paragraphs := make([]string, 0, len(parts))
	for _, part := range parts {
		if value := trimJS(part); value != "" {
			paragraphs = append(paragraphs, value)
		}
	}
	return paragraphs
}

// rvcNormalizeParagraph is `paragraph.replace(/\s+/gu, ' ').trim()`.
func rvcNormalizeParagraph(paragraph string) string {
	return trimJS(rvcSpaceRunPattern.ReplaceAllString(paragraph, " "))
}

// rvcIsNoopRevision is isNoopRevision (src/shared/review-cycle.ts:95-98): the two
// texts are equal once every White_Space and Cf code point is removed.
func rvcIsNoopRevision(before, after string) bool {
	return rvcVisibleRevision(before) == rvcVisibleRevision(after)
}

// rvcVisibleRevision is `value.replace(/[\p{White_Space}\p{Cf}]/gu, '')`.
func rvcVisibleRevision(value string) string {
	var kept strings.Builder
	kept.Grow(len(value))
	for _, r := range value {
		if unicode.Is(unicode.White_Space, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		kept.WriteRune(r)
	}
	return kept.String()
}

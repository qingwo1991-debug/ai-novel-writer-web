package store

// The review-cycle write path: ReviewCycleRepository.create plus the
// review-revision commit that reaches it.
//
// References: electron/repositories/review-cycle-repository.ts (create, :577) and
// electron/services/review-revision-generation.ts (commitReview, :198). The
// pieces this file builds on already live in this package:
// reviewcycle_projection.go owns the strict saved-report parse and the finding
// discriminant, reviewrevision.go owns the review rows, and
// review_revision_context.go owns the frozen context.
//
// Everything below runs inside ONE BEGIN IMMEDIATE transaction on the caller's
// reserved connection. The author database is opened with MaxOpenConns(1)
// (internal/projectfs/author_linux.go:121), so a second connection inside an
// open transaction would deadlock: the in-transaction context proof therefore
// calls rrctxCapture on the same *sql.Conn instead of Capture, which opens its
// own connection.
//
// Refusals stay symbolic: every failure is a sentinel carrying an uppercase
// code, and the command boundary maps it exactly once.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewbuilder"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewconsistency"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewgoals"
)

// rccReportVersion is the report version this lane builds and stamps. The
// reference refuses any other version on a review effect.
const rccReportVersion = 2

var (
	// ErrReviewCycleConflict is the reference's REVIEW_CYCLE_CONFLICT: the
	// cycle row or one of its findings cannot be written or re-proved.
	ErrReviewCycleConflict = errors.New("REVIEW_CYCLE_CONFLICT")
	// ErrReviewCommitInvalid is GENERATION_REVIEW_COMMIT_INVALID: the request or
	// the run's operation cannot carry a review commit.
	ErrReviewCommitInvalid = errors.New("GENERATION_REVIEW_COMMIT_INVALID")
	// ErrReviewCommitConflict is GENERATION_REVIEW_COMMIT_CONFLICT: a saved
	// receipt exists but names a different artifact.
	ErrReviewCommitConflict = errors.New("GENERATION_REVIEW_COMMIT_CONFLICT")
	// ErrReviewCommitArtifact is GENERATION_REVIEW_ARTIFACT_INVALID: the artifact
	// is not this run's settled, provable output.
	ErrReviewCommitArtifact = errors.New("GENERATION_REVIEW_ARTIFACT_INVALID")
	// ErrReviewCommitContextRequired is GENERATION_REVIEW_CONTEXT_REQUIRED: the
	// prepared handle the caller presented is not this run's frozen context.
	ErrReviewCommitContextRequired = errors.New("GENERATION_REVIEW_CONTEXT_REQUIRED")
	// ErrReviewCommitContextUnproven is GENERATION_REVIEW_CONTEXT_UNPROVEN: the
	// run's binding does not carry a self-consistent frozen context.
	ErrReviewCommitContextUnproven = errors.New("GENERATION_REVIEW_CONTEXT_UNPROVEN")
	// ErrReviewCommitContextChanged is GENERATION_REVIEW_CONTEXT_CHANGED: a fresh
	// capture of the same request no longer reproduces the frozen context. The
	// reference reaches the same refusal through assertSourcesCurrent, which
	// throws GENERATION_SOURCE_CHANGED; both codes share one author-facing
	// message (src/i18n/app-errors.ts).
	ErrReviewCommitContextChanged = errors.New("GENERATION_REVIEW_CONTEXT_CHANGED")
	// ErrReviewCommitReceipt is GENERATION_REVIEW_RECEIPT_INVALID: the run's
	// persisted receipt or review row does not prove what it claims.
	ErrReviewCommitReceipt = errors.New("GENERATION_REVIEW_RECEIPT_INVALID")
	// ErrReviewCommitIdentity is GENERATION_RUN_IDENTITY_MISMATCH: the handle does
	// not name this run.
	ErrReviewCommitIdentity = errors.New("GENERATION_RUN_IDENTITY_MISMATCH")
	// ErrReviewCommitCancelled is GENERATION_ACTION_CANCELLED: the root action was
	// cancelled, so no new author-visible row may be written under it.
	ErrReviewCommitCancelled = errors.New("GENERATION_ACTION_CANCELLED")
)

// ReviewCommitHandle is MainGenerationRunHandle: the four fields a writer must
// already hold before it may commit under a run.
type ReviewCommitHandle struct {
	ProjectID    string
	Epoch        string
	RootActionID string
	RunID        string
}

// ReviewCommitArtifact is ReviewGenerationCommitRequest['artifact'].
type ReviewCommitArtifact struct {
	ArtifactID string
	Revision   int64
	TextHash   string
}

// ReviewCommitRequest is the whole commit-review request. LiveEpoch is the epoch
// the caller is leased on, which is what assertSourcesCurrent compares with the
// run's binding.
type ReviewCommitRequest struct {
	ProjectID string
	LiveEpoch string
	Handle    ReviewCommitHandle
	Artifact  ReviewCommitArtifact
	// AssertPrepared is called once, after the run identity, the operation and
	// any saved receipt have been proved and before anything is written. A
	// non-nil return refuses the commit. It keeps the prepared-context registry
	// in the layer that owns it while preserving the reference's refusal order.
	AssertPrepared func(operation, frozenText string) error
}

// ReviewCommitSource is the frozen source draft a review was written against.
type ReviewCommitSource struct {
	ID            int64  `json:"id"`
	ChapterNumber int64  `json:"chapterNumber"`
	Version       int64  `json:"version"`
	Status        string `json:"status"`
	Content       string `json:"content"`
}

// ReviewCommitReceipt is the shape commitReview returns to its caller.
type ReviewCommitReceipt struct {
	Success     bool               `json:"success"`
	Kind        string             `json:"kind"`
	ID          int64              `json:"id"`
	Index       int64              `json:"index"`
	Content     string             `json:"content"`
	ContentHash string             `json:"contentHash"`
	Source      ReviewCommitSource `json:"source"`
}

// rcSource is the frozen source draft exactly as the context stores it.
type rcSource struct {
	ID            int64  `json:"id"`
	ChapterNumber int64  `json:"chapterNumber"`
	Version       int64  `json:"version"`
	Status        string `json:"status"`
	Content       string `json:"content"`
}

// rccFrozen is the frozen review-chapter context as the run's binding persists
// it. Only the source and the request fields a fresh capture must reproduce are
// kept; the frozen text itself stays the authority for the hashed bytes.
type rccFrozen struct {
	Operation    string   `json:"operation"`
	Version      int64    `json:"version"`
	UILocale     string   `json:"uiLocale"`
	Source       rcSource `json:"source"`
	SourceHash   string   `json:"sourceHash"`
	AuthorInputs []any    `json:"authorInputs"`
	Confirmation any      `json:"confirmation"`
	Recheck      any      `json:"recheck"`
}

// CommitReview is ReviewRepository.create plus ReviewCycleRepository.create,
// stamped and re-proved in one transaction: it writes the review row, its cycle
// row and every finding, or nothing at all.
func (r *ReviewRevisions) CommitReview(ctx context.Context, request ReviewCommitRequest) (*ReviewCommitReceipt, error) {
	if r == nil || r.db == nil {
		return nil, ErrReviewRevisionProjectNotOpen
	}
	if ctx == nil || summaryTrim(request.ProjectID) == "" || summaryTrim(request.LiveEpoch) == "" ||
		summaryTrim(request.Handle.ProjectID) == "" || summaryTrim(request.Handle.Epoch) == "" ||
		summaryTrim(request.Handle.RootActionID) == "" || summaryTrim(request.Handle.RunID) == "" ||
		summaryTrim(request.Artifact.ArtifactID) == "" || !rccHashDigest(request.Artifact.TextHash) ||
		request.Artifact.Revision < 0 {
		return nil, ErrReviewCommitInvalid
	}
	var receipt *ReviewCommitReceipt
	err := r.immediate(ctx, func(conn *sql.Conn) error {
		value, err := rccCommitBody(ctx, conn, request)
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

func rccCommitBody(ctx context.Context, conn *sql.Conn, request ReviewCommitRequest) (*ReviewCommitReceipt, error) {
	// The same root admission every neighbouring writer takes before it mutates.
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
	if frozen.Operation != ReviewRevisionOperationReviewChapter {
		return nil, ErrReviewCommitInvalid
	}
	// saved() answers a replay before any freshness proof, exactly as the
	// reference does: the receipt is a projection of rows already written.
	saved, savedRef, err := rccSavedReview(ctx, conn, request, binding, frozen, text, hash)
	if err != nil {
		return nil, err
	}
	if saved != nil {
		if *savedRef != request.Artifact {
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
	// produces. This runs before the first write, so a stale context can never
	// land a review.
	if err := rccReprooveFrozen(ctx, conn, request, frozen, text, hash); err != nil {
		return nil, err
	}
	artifactText, attemptID, err := rccProveArtifact(ctx, conn, request.Handle.RunID, binding, request.Artifact)
	if err != nil {
		return nil, err
	}
	body, err := rccReviewBody(text, artifactText, rccReportVersion)
	if err != nil {
		return nil, ErrReviewCommitReceipt
	}
	source := rrSource{
		id:      frozen.Source.ID,
		chapter: frozen.Source.ChapterNumber,
		version: frozen.Source.Version,
		status:  frozen.Source.Status,
		content: frozen.Source.Content,
	}
	if err := rrAssertSource(ctx, conn, frozen.Source.ID, source); err != nil {
		return nil, err
	}
	reviewID, index, err := rccCreateReview(ctx, conn, frozen.Source.ID, source, body)
	if err != nil {
		return nil, err
	}
	contentHash := recoveryHash(body)
	effect := plotObject{
		{"kind", "review"},
		{"id", reviewID},
		{"index", index},
		{"contentHash", contentHash},
		{"artifact", plotObject{
			{"artifactId", request.Artifact.ArtifactID},
			{"revision", request.Artifact.Revision},
			{"textHash", request.Artifact.TextHash},
		}},
		{"contextHash", hash},
		{"reportVersion", int64(rccReportVersion)},
	}
	if err := rccStampEffect(ctx, conn, request.Handle.RunID, attemptID, effect); err != nil {
		return nil, err
	}
	cycle := reviewCycleSource{
		ProjectID:   request.ProjectID,
		Epoch:       glText(glField(binding, "epoch")),
		ID:          "draft:" + strconv.FormatInt(frozen.Source.ID, 10),
		Revision:    frozen.Source.Version,
		ContentHash: recoveryHash(frozen.Source.Content),
	}
	if err := rccCreateCycle(ctx, conn, rootID, reviewID, contentHash, body, cycle); err != nil {
		return nil, err
	}
	// Re-read the receipt from the rows just written: the caller never sees an
	// answer this transaction did not prove.
	receipt, _, err := rccSavedReview(ctx, conn, request, binding, frozen, text, hash)
	if err != nil {
		return nil, err
	}
	if receipt == nil {
		return nil, ErrReviewCommitReceipt
	}
	return receipt, nil
}

// rccOrderedBinding returns the run's root action id and its binding as an
// order-preserving value: the frozen context must round-trip byte for byte, and
// glMap would re-sort the keys.
func rccOrderedBinding(ctx context.Context, conn *sql.Conn, runID string) (string, any, error) {
	rows, err := rrMaps(ctx, conn, "SELECT root_action_id,binding_json FROM generation_runs WHERE run_id=?", runID)
	if err != nil {
		return "", nil, err
	}
	if len(rows) == 0 {
		return "", nil, ErrReviewCommitIdentity
	}
	binding, err := glParse(glText(rows[0]["binding_json"]))
	if err != nil {
		return "", nil, ErrReviewCommitContextUnproven
	}
	return glText(rows[0]["root_action_id"]), binding, nil
}

// rccRootAction reads the root's action row. Its status and identity fields are
// what the reference reads from repository.budget(rootActionId).root.
func rccRootAction(ctx context.Context, conn *sql.Conn, rootID string) (map[string]any, error) {
	row, err := glOne(ctx, conn, "SELECT action_json FROM generation_roots WHERE root_action_id=?", "", rootID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, ErrReviewCommitIdentity
	}
	action, err := glStored(row["action_json"])
	if err != nil {
		return nil, ErrReviewCommitContextUnproven
	}
	return action, nil
}

// rccFrozenContext proves the run's binding carries a self-consistent frozen
// review-chapter context and returns it with the exact bytes and hash it was
// stamped with.
func rccFrozenContext(binding any) (rccFrozen, string, string, error) {
	var frozen rccFrozen
	manifest := glField(binding, "sourceManifest")
	text, err := plotJSON(glField(manifest, "reviewRevisionContext"))
	if err != nil || text == "null" {
		return frozen, "", "", ErrReviewCommitContextUnproven
	}
	hash := glText(glField(manifest, "reviewRevisionContextHash"))
	if !rccHashDigest(hash) || recoveryHash(text) != hash {
		return frozen, "", "", ErrReviewCommitContextUnproven
	}
	if err := json.Unmarshal([]byte(text), &frozen); err != nil {
		return frozen, "", "", ErrReviewCommitContextUnproven
	}
	// The lane never freezes a confirmation or a recheck: Capture refuses both
	// arms for review-chapter. Refuse a context that carries one rather than
	// commit a review whose source this port cannot prove.
	if frozen.Confirmation != nil || frozen.Recheck != nil || frozen.Version != 1 ||
		rcLoneSurrogateEscape(text) || glText(glField(manifest, "operation")) != frozen.Operation ||
		frozen.Source.ID < 1 || frozen.Source.Version < 1 || frozen.Source.ChapterNumber < 1 ||
		summaryTrim(frozen.Source.Status) == "" || !utf8.ValidString(frozen.Source.Content) ||
		recoveryHash(frozen.Source.Content) != frozen.SourceHash ||
		(frozen.UILocale != ReviewRevisionLocaleZH && frozen.UILocale != ReviewRevisionLocaleEN) {
		return rccFrozen{}, "", "", ErrReviewCommitContextUnproven
	}
	inputs, ok := glField(manifest, "authorInputs").([]any)
	if !ok || len(inputs) != 1 || glText(glField(inputs[0], "id")) != rrctxAuthorInputID ||
		glText(glField(inputs[0], "text")) != text {
		return rccFrozen{}, "", "", ErrReviewCommitContextUnproven
	}
	return frozen, text, hash, nil
}

// rccReprooveFrozen captures the same request again on the transaction's own
// connection and requires the frozen text and hash to match exactly.
func rccReprooveFrozen(ctx context.Context, conn *sql.Conn, request ReviewCommitRequest,
	frozen rccFrozen, text, hash string) error {
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
	fresh, err := rrctxCapture(ctx, conn, request.ProjectID, values)
	if err != nil {
		return ErrReviewCommitContextChanged
	}
	if fresh.Text != text || fresh.ContextHash != hash || fresh.SourceHash != frozen.SourceHash {
		return ErrReviewCommitContextChanged
	}
	return nil
}

// rccProveArtifact is proveArtifact together with the effect verifier's
// persisted-row checks: the artifact must belong to this run and this attempt, be
// settled, carry a stop finish reason and hash to the reference the caller gave.
func rccProveArtifact(ctx context.Context, conn *sql.Conn, runID string, binding any,
	ref ReviewCommitArtifact) (string, string, error) {
	rows, err := rrMaps(ctx, conn, `SELECT t.attempt_id AS attemptId,t.run_id AS attemptRunId,
 t.attempt_json AS attemptJson,t.usage_receipt_json AS usage,a.artifact_id AS artifactId,
 a.artifact_json AS artifactJson,a.revision AS revision,a.status AS artifactStatus
 FROM generation_artifacts a JOIN generation_attempts t ON t.attempt_id=a.attempt_id
 WHERE a.artifact_id=?`, ref.ArtifactID)
	if err != nil {
		return "", "", err
	}
	if len(rows) == 0 {
		return "", "", ErrReviewCommitArtifact
	}
	row := rows[0]
	attemptID := glText(row["attemptId"])
	storedRevision, revisionOK := rrInteger(row["revision"], -9007199254740991)
	if attemptID == "" || glText(row["attemptRunId"]) != runID ||
		glText(row["artifactStatus"]) == "discarded" || !revisionOK || storedRevision != ref.Revision {
		return "", "", ErrReviewCommitArtifact
	}
	payload, err := glStored(row["artifactJson"])
	if err != nil {
		return "", "", ErrReviewCommitArtifact
	}
	text, isText := payload["text"].(string)
	payloadRevision, payloadOK := rrInteger(payload["revision"], 0)
	if !isText || !payloadOK || payloadRevision != ref.Revision ||
		glText(payload["artifactId"]) != ref.ArtifactID || glText(payload["attemptId"]) != attemptID ||
		summaryTrim(glText(payload["rootActionId"])) == "" ||
		glText(payload["projectId"]) != glText(glField(binding, "projectId")) ||
		glText(payload["textHash"]) != ref.TextHash || recoveryHash(text) != ref.TextHash ||
		!glSame(payload["fingerprint"], glField(binding, "fingerprint")) ||
		summaryTrim(glText(payload["epoch"])) == "" {
		return "", "", ErrReviewCommitArtifact
	}
	physical, err := glStored(row["attemptJson"])
	if err != nil {
		return "", "", ErrReviewCommitArtifact
	}
	if status := glText(physical["status"]); status != "settled" && status != "unknown" {
		return "", "", ErrReviewCommitArtifact
	}
	usage, err := glStored(row["usage"])
	if err != nil {
		return "", "", ErrReviewCommitArtifact
	}
	result := glMap(usage["result"])
	identity := glMap(usage["artifactIdentity"])
	if result == nil || glText(result["finishReason"]) != "stop" || result["failureCode"] != nil ||
		identity == nil || glText(identity["artifactId"]) != ref.ArtifactID ||
		!glSame(identity["epoch"], payload["epoch"]) || !glSame(identity["fingerprint"], payload["fingerprint"]) {
		return "", "", ErrReviewCommitArtifact
	}
	return text, attemptID, nil
}

// rccEffectRow is one attempt that already carries a reviewRevisionEffect.
type rccEffectRow struct {
	attemptID string
	effect    map[string]any
}

// rccSavedReview is saved(): the run's single receipt, re-proved against the rows
// it names. It returns a nil receipt when the run has no receipt yet.
func rccSavedReview(ctx context.Context, conn *sql.Conn, request ReviewCommitRequest, binding any,
	frozen rccFrozen, text, hash string) (*ReviewCommitReceipt, *ReviewCommitArtifact, error) {
	rows, err := rrMaps(ctx, conn,
		"SELECT attempt_id,usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid",
		request.Handle.RunID)
	if err != nil {
		return nil, nil, err
	}
	found := make([]rccEffectRow, 0, 1)
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
		found = append(found, rccEffectRow{attemptID: glText(row["attempt_id"]), effect: effect})
	}
	if len(found) > 1 {
		return nil, nil, ErrReviewCommitReceipt
	}
	if len(found) == 0 {
		return nil, nil, nil
	}
	effect := found[0].effect
	artifact := glMap(effect["artifact"])
	if artifact == nil || len(artifact) != 3 || glText(effect["kind"]) != "review" {
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
	version := int64(1)
	if value, exists := effect["reportVersion"]; exists {
		if version, revisionOK = rrInteger(value, 0); !revisionOK || version != int64(rccReportVersion) {
			return nil, nil, ErrReviewCommitReceipt
		}
	}
	reviewID, idOK := rrInteger(effect["id"], 1)
	index, indexOK := rrInteger(effect["index"], 1)
	contentHash := glText(effect["contentHash"])
	if !idOK || !indexOK || !rccHashDigest(contentHash) || glText(effect["contextHash"]) != hash {
		return nil, nil, ErrReviewCommitReceipt
	}
	artifactText, attemptID, err := rccProveArtifact(ctx, conn, request.Handle.RunID, binding, ref)
	if err != nil {
		return nil, nil, err
	}
	if attemptID != found[0].attemptID {
		return nil, nil, ErrReviewCommitReceipt
	}
	body, err := rccReviewBody(text, artifactText, int(version))
	if err != nil {
		return nil, nil, ErrReviewCommitReceipt
	}
	row, err := rrFull(rrMaps(ctx, conn, "SELECT "+rrReviewMeta+rrSourceColumns+
		" FROM reviews r LEFT JOIN contents c ON c.id=r.content_id WHERE r.id=?", reviewID))
	if err != nil {
		return nil, nil, err
	}
	if row == nil {
		return nil, nil, ErrReviewCommitReceipt
	}
	content, isText := row["content"].(string)
	storedIndex, storedOK := rrInteger(row["reviewIndex"], 1)
	if !isText || !storedOK || storedIndex != index || content != body ||
		recoveryHash(content) != contentHash || !glSame(row["sourceDraft"], rccSourceMap(frozen.Source)) {
		return nil, nil, ErrReviewCommitReceipt
	}
	source := ReviewCommitSource{
		ID:            frozen.Source.ID,
		ChapterNumber: frozen.Source.ChapterNumber,
		Version:       frozen.Source.Version,
		Status:        frozen.Source.Status,
		Content:       frozen.Source.Content,
	}
	receipt := &ReviewCommitReceipt{
		Success:     true,
		Kind:        "review",
		ID:          reviewID,
		Index:       index,
		Content:     content,
		ContentHash: contentHash,
		Source:      source,
	}
	return receipt, &ref, nil
}

func rccSourceMap(source rcSource) map[string]any {
	return map[string]any{
		"id":            source.ID,
		"chapterNumber": source.ChapterNumber,
		"version":       source.Version,
		"status":        source.Status,
		"content":       source.Content,
	}
}

// rccReviewBody is reviewBody: rebuild the report from the frozen context and the
// artifact text, then keep the canonical pretty copy as the row body.
func rccReviewBody(frozenText, artifactText string, reportVersion int) (string, error) {
	var frozen struct {
		Source            rcSource                    `json:"source"`
		FrozenGoals       reviewgoals.FrozenGoals     `json:"frozenGoals"`
		WritingLanguage   string                      `json:"writingLanguage"`
		UILocale          string                      `json:"uiLocale"`
		PreflightFindings []reviewconsistency.Finding `json:"preflightFindings"`
	}
	if err := json.Unmarshal([]byte(frozenText), &frozen); err != nil {
		return "", ErrReviewCommitReceipt
	}
	compact, err := reviewbuilder.Build(reviewbuilder.Input{
		Content:           artifactText,
		SourceContent:     frozen.Source.Content,
		FrozenGoals:       frozen.FrozenGoals,
		WritingLanguage:   frozen.WritingLanguage,
		UILocale:          frozen.UILocale,
		PreflightFindings: frozen.PreflightFindings,
		ReportVersion:     reportVersion,
	})
	if err != nil {
		return "", err
	}
	pretty, err := reviewbuilder.Pretty(compact)
	if err != nil {
		return "", err
	}
	return string(pretty), nil
}

// rccCreateReview is ReviewRepository.create: MAX(review_index)+1, the contents
// row, then the review row carrying the frozen source columns.
func rccCreateReview(ctx context.Context, conn *sql.Conn, sourceID int64, source rrSource,
	body string) (int64, int64, error) {
	rows, err := rrMaps(ctx, conn,
		"SELECT COALESCE(MAX(review_index),0)+1 AS next FROM reviews WHERE base_draft_id=?", sourceID)
	if err != nil {
		return 0, 0, err
	}
	if len(rows) == 0 {
		return 0, 0, ErrReviewCommitConflict
	}
	index, indexOK := rrInteger(rows[0]["next"], 1)
	if !indexOK {
		return 0, 0, ErrReviewCommitConflict
	}
	content, err := conn.ExecContext(ctx, "INSERT INTO contents(body) VALUES(?)", body)
	if err != nil {
		return 0, 0, err
	}
	contentID, err := content.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO reviews(base_draft_id,review_index,
 source_draft_chapter_number,source_draft_version,source_draft_status,source_content,content_id)
 VALUES(?,?,?,?,?,?,?)`, sourceID, index, source.chapter, source.version, source.status, source.content, contentID)
	if err != nil {
		return 0, 0, err
	}
	reviewID, err := result.LastInsertId()
	if err != nil {
		return 0, 0, err
	}
	if reviewID < 1 {
		return 0, 0, ErrReviewCommitConflict
	}
	return reviewID, index, nil
}

// rccStampEffect writes the effect through the single-key patch the ledger
// receipt uses, then proves the write landed on exactly one attempt row.
func rccStampEffect(ctx context.Context, conn *sql.Conn, runID, attemptID string, effect plotObject) error {
	rows, err := rrMaps(ctx, conn,
		"SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=? AND run_id=?", attemptID, runID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return ErrReviewCommitReceipt
	}
	patched, err := glPatch(glText(rows[0]["usage_receipt_json"]),
		map[string]any{"reviewRevisionEffect": effect})
	if err != nil {
		return ErrReviewCommitReceipt
	}
	result, err := conn.ExecContext(ctx,
		"UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=? AND run_id=?",
		patched, attemptID, runID)
	if err := rrAffected(result, err, ErrReviewCommitReceipt); err != nil {
		return err
	}
	return nil
}

// rccFinding is one persisted finding before it is projected into the identity
// the M03 verifier hashes.
type rccFinding struct {
	ID           string
	Category     string
	TargetID     *string
	Kind         string
	ProblemText  string
	ExpectedText *string
	Status       string
	SpanStart    *int64
	SpanEnd      *int64
	SpanUnit     *string
	ExcerptHash  *string
	Occurrence   *int64
}

func (f rccFinding) identity(source reviewCycleSource) admission.M03FindingIdentity {
	return admission.M03FindingIdentity{
		FindingID:         f.ID,
		SourceProjectID:   source.ProjectID,
		SourceEpoch:       source.Epoch,
		SourceID:          source.ID,
		SourceRevision:    source.Revision,
		SourceContentHash: source.ContentHash,
		SpanStart:         f.SpanStart,
		SpanEnd:           f.SpanEnd,
		SpanUnit:          f.SpanUnit,
		ExcerptHash:       f.ExcerptHash,
		Occurrence:        f.Occurrence,
		Category:          f.Category,
		TargetID:          f.TargetID,
		Kind:              f.Kind,
		ProblemText:       f.ProblemText,
		ExpectedText:      f.ExpectedText,
	}
}

// rccCreateCycle writes the cycle row and every finding, then reads both back and
// proves the stored finding-set hash against admission's canonical form. The
// schema arms of verifyM03ReviewCycle need a *sql.Tx and are pinned at open time
// by admission instead of per write.
func rccCreateCycle(ctx context.Context, conn *sql.Conn, rootActionID string, reviewID int64,
	contentHash, reportBody string, source reviewCycleSource) error {
	action, err := rccRootAction(ctx, conn, rootActionID)
	if err != nil {
		return err
	}
	// create() refuses a root that does not own this project, epoch and operation.
	if glText(action["rootActionId"]) != rootActionID || glText(action["projectId"]) != source.ProjectID ||
		glText(action["epoch"]) != source.Epoch ||
		glText(action["operation"]) != ReviewRevisionOperationReviewChapter {
		return ErrReviewCommitInvalid
	}
	reviewSource, err := rccCycleReviewSource(ctx, conn, reviewID, contentHash, source)
	if err != nil {
		return err
	}
	findings, findingSetHash, err := rccBuildFindings(reportBody, reviewSource, source)
	if err != nil {
		return err
	}
	cycleID, err := rccCycleID(rootActionID, reviewID, contentHash)
	if err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO review_cycles(cycle_id,root_action_id,review_id,
 review_content_hash,source_hash,finding_set_hash,comparison_version,revision_status,recheck_count)
 VALUES(?,?,?,?,?,?,1,'not-generated',0)`, cycleID, rootActionID, reviewID, contentHash,
		source.ContentHash, findingSetHash); err != nil {
		return ErrReviewCycleConflict
	}
	for _, finding := range findings {
		if _, err := conn.ExecContext(ctx, `INSERT INTO review_findings(cycle_id,finding_id,
 source_project_id,source_epoch,source_id,source_revision,source_content_hash,span_start,span_end,
 span_unit,excerpt_hash,occurrence,category,target_id,kind,problem_text,expected_text,status,
 evidence_hash,confirmation_item_index) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL,NULL)`,
			cycleID, finding.ID, source.ProjectID, source.Epoch, source.ID, source.Revision,
			source.ContentHash, rccAnyInt(finding.SpanStart), rccAnyInt(finding.SpanEnd),
			rccAnyText(finding.SpanUnit), rccAnyText(finding.ExcerptHash),
			rccAnyInt(finding.Occurrence), finding.Category, rccAnyText(finding.TargetID),
			finding.Kind, finding.ProblemText, rccAnyText(finding.ExpectedText),
			finding.Status); err != nil {
			return ErrReviewCycleConflict
		}
	}
	return rccVerifyCycle(ctx, conn, cycleID, contentHash, source, findingSetHash)
}

// rccCycleReviewSource re-reads the review row the cycle will point at and proves
// it is the row this commit just wrote, including its stored source body.
func rccCycleReviewSource(ctx context.Context, conn *sql.Conn, reviewID int64, contentHash string,
	source reviewCycleSource) (string, error) {
	rows, err := rrMaps(ctx, conn, `SELECT r.base_draft_id AS baseDraftId,
 r.source_draft_version AS sourceVersion,r.source_content AS sourceContent,c.body AS body
 FROM reviews r JOIN contents c ON c.id=r.content_id WHERE r.id=?`, reviewID)
	if err != nil {
		return "", err
	}
	if len(rows) != 1 {
		return "", ErrReviewCycleConflict
	}
	row := rows[0]
	baseDraftID, baseOK := rrInteger(row["baseDraftId"], 1)
	version, versionOK := rrInteger(row["sourceVersion"], 1)
	body, bodyOK := row["body"].(string)
	storedSource, sourceOK := row["sourceContent"].(string)
	if !baseOK || !versionOK || !bodyOK || !sourceOK ||
		source.ID != "draft:"+strconv.FormatInt(baseDraftID, 10) ||
		source.Revision != version || recoveryHash(storedSource) != source.ContentHash ||
		recoveryHash(body) != contentHash {
		return "", ErrReviewCycleConflict
	}
	return storedSource, nil
}

// rccVerifyCycle re-reads the persisted cycle, its review body and every finding
// and proves the stored hash is the canonical hash of exactly those rows.
func rccVerifyCycle(ctx context.Context, conn *sql.Conn, cycleID, contentHash string,
	source reviewCycleSource, findingSetHash string) error {
	rows, err := rrMaps(ctx, conn,
		"SELECT source_hash,finding_set_hash FROM review_cycles WHERE cycle_id=?", cycleID)
	if err != nil {
		return err
	}
	if len(rows) != 1 || glText(rows[0]["source_hash"]) != source.ContentHash ||
		glText(rows[0]["finding_set_hash"]) != findingSetHash {
		return ErrReviewCycleConflict
	}
	body, err := rrMaps(ctx, conn, `SELECT c.body AS body FROM review_cycles y
 JOIN reviews r ON r.id=y.review_id JOIN contents c ON c.id=r.content_id WHERE y.cycle_id=?`, cycleID)
	if err != nil {
		return err
	}
	if len(body) != 1 || recoveryHash(glText(body[0]["body"])) != contentHash {
		return ErrReviewCycleConflict
	}
	stored, err := rrMaps(ctx, conn, `SELECT finding_id,source_project_id,source_epoch,source_id,
 source_revision,source_content_hash,span_start,span_end,span_unit,excerpt_hash,occurrence,
 category,target_id,kind,problem_text,expected_text
 FROM review_findings WHERE cycle_id=? ORDER BY finding_id`, cycleID)
	if err != nil {
		return err
	}
	identities := make([]admission.M03FindingIdentity, 0, len(stored))
	for _, row := range stored {
		identity, err := rccStoredIdentity(row)
		if err != nil {
			return err
		}
		identities = append(identities, identity)
	}
	if admission.CanonicalM03FindingSetHash(identities) != findingSetHash {
		return ErrReviewCycleConflict
	}
	return nil
}

func rccStoredIdentity(row map[string]any) (admission.M03FindingIdentity, error) {
	var identity admission.M03FindingIdentity
	identity.FindingID = glText(row["finding_id"])
	identity.SourceProjectID = glText(row["source_project_id"])
	identity.SourceEpoch = glText(row["source_epoch"])
	identity.SourceID = glText(row["source_id"])
	identity.SourceContentHash = glText(row["source_content_hash"])
	identity.Category = glText(row["category"])
	identity.Kind = glText(row["kind"])
	identity.ProblemText = glText(row["problem_text"])
	revision, revisionOK := rrInteger(row["source_revision"], 0)
	if !revisionOK || identity.FindingID == "" {
		return identity, ErrReviewCycleConflict
	}
	identity.SourceRevision = revision
	start, startOK := rccOptionalInt(row["span_start"])
	end, endOK := rccOptionalInt(row["span_end"])
	occurrence, occurrenceOK := rccOptionalInt(row["occurrence"])
	unit, unitOK := rccOptionalString(row["span_unit"])
	excerpt, excerptOK := rccOptionalString(row["excerpt_hash"])
	target, targetOK := rccOptionalString(row["target_id"])
	expected, expectedOK := rccOptionalString(row["expected_text"])
	if !startOK || !endOK || !occurrenceOK || !unitOK || !excerptOK || !targetOK || !expectedOK {
		return identity, ErrReviewCycleConflict
	}
	identity.SpanStart, identity.SpanEnd = start, end
	identity.Occurrence, identity.SpanUnit, identity.ExcerptHash = occurrence, unit, excerpt
	identity.TargetID, identity.ExpectedText = target, expected
	return identity, nil
}

func rccOptionalInt(value any) (*int64, bool) {
	if value == nil {
		return nil, true
	}
	number, ok := rrInteger(value, -9007199254740991)
	if !ok {
		return nil, false
	}
	return &number, true
}

func rccOptionalString(value any) (*string, bool) {
	if value == nil {
		return nil, true
	}
	text, ok := value.(string)
	if !ok {
		return nil, false
	}
	return &text, true
}

func rccAnyInt(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func rccAnyText(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

// rccCycleID is cycleId(): a stable identifier over the tuple that makes a cycle
// unique.
func rccCycleID(rootActionID string, reviewID int64, contentHash string) (string, error) {
	encoded, err := plotJSON([]any{rootActionID, reviewID, contentHash})
	if err != nil {
		return "", err
	}
	return "cycle:" + recoveryHash(encoded), nil
}

// rccBuildFindings is buildFindings(): goals first, then the free and preflight
// items, deduped first-wins on the finding id with the unresolved-wins rule, and
// the canonical hash of the identities it produced.
func rccBuildFindings(reportBody, sourceText string,
	source reviewCycleSource) ([]rccFinding, string, error) {
	items, goals, err := rcParseSavedReport(reportBody)
	if err != nil {
		return nil, "", err
	}
	ordered := make([]reviewCycleSavedGoal, 0, len(goals))
	for _, goal := range goals {
		ordered = append(ordered, goal)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Index < ordered[j].Index })
	units := utf16.Encode([]rune(sourceText))
	findings := make([]rccFinding, 0, len(ordered)+len(items))
	seen := make(map[string]int, len(ordered)+len(items))
	add := func(finding rccFinding) {
		if index, exists := seen[finding.ID]; exists {
			if findings[index].Status != finding.Status && finding.Status == "unresolved" {
				findings[index].Status = "unresolved"
			}
			return
		}
		seen[finding.ID] = len(findings)
		findings = append(findings, finding)
	}
	for _, goal := range ordered {
		for _, evidence := range goal.Evidence {
			// Every evidence slice must still name its exact quote.
			if evidence.Start < 0 || evidence.End < evidence.Start ||
				evidence.End > int64(len(units)) ||
				int64(len(utf16.Encode([]rune(evidence.Quote)))) != evidence.End-evidence.Start ||
				string(utf16.Decode(units[evidence.Start:evidence.End])) != evidence.Quote {
				return nil, "", errReviewCycleSavedReport
			}
		}
		if goal.Status == "completed" {
			continue
		}
		var item reviewCycleSavedItem
		found := false
		for _, candidate := range items {
			if candidate.GoalID != nil && *candidate.GoalID == goal.ID {
				item = candidate
				found = true
				break
			}
		}
		if !found {
			return nil, "", errReviewCycleSavedReport
		}
		projection := goal
		finding, err := rccDescribeFinding(source, sourceText, item, &projection)
		if err != nil {
			return nil, "", err
		}
		add(finding)
	}
	for _, item := range items {
		if item.Severity == "pass" || item.GoalID != nil {
			continue
		}
		finding, err := rccDescribeFinding(source, sourceText, item, nil)
		if err != nil {
			return nil, "", err
		}
		add(finding)
	}
	identities := make([]admission.M03FindingIdentity, 0, len(findings))
	for _, finding := range findings {
		identities = append(identities, finding.identity(source))
	}
	return findings, admission.CanonicalM03FindingSetHash(identities), nil
}

// rccDescribeFinding is finding(): it decides the anchor once and derives both the
// stored columns and the finding id from that one decision, so a row can never
// disagree with the id it carries.
func rccDescribeFinding(source reviewCycleSource, sourceText string, item reviewCycleSavedItem,
	goal *reviewCycleSavedGoal) (rccFinding, error) {
	var quote *string
	var targetID *string
	kind := "literary"
	logical := "free:" + phNumberString(int64(item.Index))
	var expectedText *string
	if goal != nil {
		if len(goal.Evidence) == 1 {
			value := goal.Evidence[0].Quote
			quote = &value
		}
		value := goal.ID
		targetID = &value
		kind = "objective"
		logical = "goal:" + goal.ID
		expected := goal.Text + "\n" + goal.Description
		expectedText = &expected
	} else {
		quote = item.Quote
		if item.StableFactKey != nil {
			value := *item.StableFactKey
			targetID = &value
			kind = "objective"
			logical = "preflight:" + value
		}
	}
	var start, end, occurrence *int64
	var spanUnit, excerptHash *string
	if quote != nil && targetID != nil {
		first := strings.Index(sourceText, *quote)
		if first >= 0 && strings.Index(sourceText[first+1:], *quote) < 0 {
			startValue := int64(len(utf16.Encode([]rune(sourceText[:first]))))
			endValue := startValue + int64(len(utf16.Encode([]rune(*quote))))
			unit := "utf16-code-unit"
			excerpt := recoveryHash(*quote)
			one := int64(1)
			start, end, spanUnit, excerptHash, occurrence = &startValue, &endValue, &unit, &excerpt, &one
		} else {
			targetID = nil
		}
	} else {
		targetID = nil
	}
	status := "unresolved"
	if goal != nil && goal.Status == "unknown" {
		status = "unknown"
	}
	if targetID == nil {
		status = "unverified"
	}
	identity, err := plotJSON(plotObject{
		{"sourceProjectId", source.ProjectID},
		{"sourceEpoch", source.Epoch},
		{"sourceId", source.ID},
		{"sourceRevision", source.Revision},
		{"sourceContentHash", source.ContentHash},
		{"spanStart", rccAnyInt(start)},
		{"spanEnd", rccAnyInt(end)},
		{"spanUnit", rccAnyText(spanUnit)},
		{"excerptHash", rccAnyText(excerptHash)},
		{"occurrence", rccAnyInt(occurrence)},
		{"category", item.Category},
		{"targetId", rccAnyText(targetID)},
		{"kind", kind},
		{"logicalIdentity", logical},
	})
	if err != nil {
		return rccFinding{}, errReviewCycleSavedReport
	}
	return rccFinding{
		ID:           "finding:" + recoveryHash(identity),
		Category:     item.Category,
		TargetID:     targetID,
		Kind:         kind,
		ProblemText:  item.Description,
		ExpectedText: expectedText,
		Status:       status,
		SpanStart:    start,
		SpanEnd:      end,
		SpanUnit:     spanUnit,
		ExcerptHash:  excerptHash,
		Occurrence:   occurrence,
	}, nil
}

func rccHashDigest(text string) bool {
	if len(text) != 64 {
		return false
	}
	for index := 0; index < len(text); index++ {
		character := text[index]
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

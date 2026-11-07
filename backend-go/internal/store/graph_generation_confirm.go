package store

import (
	"context"
	"database/sql"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/graphgen"
)

// This file is the ledger half of graph-generation:confirm for the plan kind:
// one BEGIN IMMEDIATE transaction that proves the stored artifact against the
// browser's reference, derives the plan candidates from the settled text, writes
// the chosen plan row and appends the stored graph effect onto the attempt's
// usage receipt. Nothing here dials a provider and nothing here writes outside
// the one reserved connection.
//
// The reference is GraphGeneration.confirm (electron/services/graph-generation.ts:
// 100-133): require the handle, replay a saved effect, prove the artifact,
// assert the sources, write the domain row (NarrativeThreadRepository.createPlan),
// assert the effect payload, then stamp the StoredGraphEffect and return the
// receipt - all inside `.immediate()`.

// GraphConfirmArtifact is the GraphGenerationArtifact reference the confirm
// request carries (src/shared/graph-generation.ts:25): the three identity fields
// and nothing else.
type GraphConfirmArtifact struct {
	ArtifactID string
	Revision   int64
	TextHash   string
}

// GraphConfirmRequest is the graph-generation:confirm request plus the re-proved
// context facts the caller already derived from the frozen manifest.
type GraphConfirmRequest struct {
	RunID         string
	RootActionID  string
	ProjectID     string
	Artifact      GraphConfirmArtifact
	Index         int
	TotalChapters int
	ContextHash   string
}

// ConfirmGraphPlan runs the plan-kind confirmation in one BEGIN IMMEDIATE
// transaction and answers the reference's GraphGenerationEffect receipt.
func ConfirmGraphPlan(ctx context.Context, db *sql.DB, req GraphConfirmRequest) (map[string]any, error) {
	if db == nil {
		return nil, ErrProjectNotOpen
	}
	var receipt map[string]any
	err := NewReviewRevisions(db).immediate(ctx, func(conn *sql.Conn) error {
		value, err := graphConfirmPlanBody(ctx, conn, req)
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

// graphConfirmPlanBody is the transaction body, in the reference's own order.
func graphConfirmPlanBody(ctx context.Context, conn *sql.Conn, req GraphConfirmRequest) (map[string]any, error) {
	ledger := NewGenerationLedger(nil, nil, nil)
	if err := ledger.mutableRoot(ctx, conn, req.RootActionID); err != nil {
		return nil, err
	}
	run, err := glOne(ctx, conn, "SELECT run_id,root_action_id,binding_json FROM generation_runs WHERE run_id=?", "GENERATION_RUN_MISSING", req.RunID)
	if err != nil {
		return nil, err
	}
	binding, err := glStored(run["binding_json"])
	if err != nil {
		return nil, err
	}
	if glText(run["root_action_id"]) != req.RootActionID || glText(binding["projectId"]) != req.ProjectID {
		return nil, glError("GENERATION_RUN_IDENTITY_MISMATCH")
	}
	manifest := glMap(binding["sourceManifest"])
	if manifest == nil || glText(manifest["operation"]) != "narrative-thread-plan-candidate" {
		return nil, glError("GENERATION_OPERATION_UNSUPPORTED")
	}
	if glText(manifest["graphGenerationContextHash"]) != req.ContextHash {
		return nil, glError("GENERATION_GRAPH_CONTEXT_INVALID")
	}
	// A saved effect at this index answers before any proof, which is what makes a
	// replayed confirm idempotent instead of a second domain write
	// (electron/services/graph-generation.ts:104-108).
	saved, err := graphConfirmSaved(ctx, conn, req)
	if err != nil {
		return nil, err
	}
	if saved != nil {
		if !graphArtifactSame(saved.artifact, req.Artifact) {
			return nil, glError("GENERATION_GRAPH_EFFECT_CONFLICT")
		}
		return saved.receipt, nil
	}
	text, attemptID, err := graphConfirmProof(ctx, conn, ledger, req)
	if err != nil {
		return nil, err
	}
	candidates, err := graphgen.ParseNarrativeThreadPlanCandidates(text, req.TotalChapters, false)
	if err != nil {
		return nil, err
	}
	if req.Index < 0 || req.Index >= len(candidates) {
		return nil, glError("GENERATION_GRAPH_SELECTION_INVALID")
	}
	candidate := candidates[req.Index]
	plan, err := narrativeCreatePlanTx(ctx, conn, graphgenPlanInput(candidate))
	if err != nil {
		return nil, err
	}
	// The receipt is what the browser gets back, and what the effect stamps.
	receipt := map[string]any{
		"success": true,
		"index":   int64(req.Index),
		"kind":    "plan",
		"plan":    plan,
	}
	if err := graphConfirmStamp(ctx, conn, req, attemptID, receipt); err != nil {
		return nil, err
	}
	return receipt, nil
}

// graphStoredEffect is one effect already stamped on an attempt, reduced to the
// two fields a replay decision needs: the artifact reference and the receipt.
type graphStoredEffect struct {
	artifact map[string]any
	receipt  map[string]any
}

// graphConfirmSaved finds the effect already stamped at the requested index, or
// nil when none exists. It does not re-prove every stored effect the way the
// reference's storedEffects does: the replay decision only needs the index and
// the artifact, and a graphEffects value that is not an array can carry no
// matching index, so it is skipped here and refused by the stamp below instead.
func graphConfirmSaved(ctx context.Context, conn *sql.Conn, req GraphConfirmRequest) (*graphStoredEffect, error) {
	rows, err := rrMaps(ctx, conn, "SELECT usage_receipt_json FROM generation_attempts WHERE run_id=? ORDER BY rowid", req.RunID)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		usage, err := glStored(row["usage_receipt_json"])
		if err != nil {
			return nil, err
		}
		saved := usage["graphEffects"]
		if saved == nil {
			continue
		}
		effects, ok := saved.([]any)
		if !ok {
			continue
		}
		for _, item := range effects {
			effect := glMap(item)
			if effect == nil {
				continue
			}
			index, indexOK := rrInteger(effect["index"], 0)
			if !indexOK || int(index) != req.Index {
				continue
			}
			artifact := glMap(effect["artifact"])
			receipt := glMap(effect["receipt"])
			if artifact == nil || receipt == nil {
				return nil, glError("GENERATION_GRAPH_EFFECT_INVALID")
			}
			return &graphStoredEffect{artifact: artifact, receipt: receipt}, nil
		}
	}
	return nil, nil
}

// graphArtifactSame is the reference's isDeepStrictEqual(saved.artifact, reference):
// the three identity fields must match exactly.
func graphArtifactSame(saved map[string]any, ref GraphConfirmArtifact) bool {
	if glText(saved["artifactId"]) != ref.ArtifactID || glText(saved["textHash"]) != ref.TextHash {
		return false
	}
	revision, ok := rrInteger(saved["revision"], 0)
	return ok && revision == ref.Revision
}

// graphConfirmProof is GraphGeneration.proof (electron/services/graph-generation.ts:
// 38-47): the artifact row must exist for this run and be undiscarded, and the
// attempt receipt must still describe the reference - the stored revision and
// text hash, a non-empty text whose hash matches, no failure code, a settled or
// unknown attempt, and a stop finish reason. It returns the settled text and the
// attempt the effect is stamped on.
func graphConfirmProof(ctx context.Context, conn *sql.Conn, ledger *GenerationLedger, req GraphConfirmRequest) (string, string, error) {
	row, err := glOne(ctx, conn, "SELECT run_id,attempt_id,status FROM generation_artifacts WHERE artifact_id=?", "", req.Artifact.ArtifactID)
	if err != nil {
		return "", "", err
	}
	if row == nil || glText(row["run_id"]) != req.RunID || glText(row["status"]) == "discarded" {
		return "", "", glError("GENERATION_GRAPH_ARTIFACT_INVALID")
	}
	attemptID := glText(row["attempt_id"])
	receipt, err := ledger.receipt(ctx, conn, attemptID, nil)
	if err != nil {
		return "", "", err
	}
	artifact := glMap(receipt["artifact"])
	if artifact == nil {
		return "", "", glError("GENERATION_GRAPH_ARTIFACT_INVALID")
	}
	revision, revisionOK := rrInteger(artifact["revision"], 0)
	text, textOK := artifact["text"].(string)
	textHash := glText(artifact["textHash"])
	attempt := glMap(receipt["attempt"])
	result := glMap(receipt["result"])
	status := glText(attempt["status"])
	if !revisionOK || revision != req.Artifact.Revision || !textOK || textHash != req.Artifact.TextHash ||
		recoveryHash(text) != req.Artifact.TextHash || trimJS(text) == "" ||
		receipt["failureCode"] != nil || (status != "settled" && status != "unknown") ||
		glText(result["finishReason"]) != "stop" {
		return "", "", glError("GENERATION_GRAPH_ARTIFACT_INVALID")
	}
	return text, attemptID, nil
}

// graphgenPlanInput is the candidate as NarrativeThreadPlanInput
// (src/shared/narrative-thread.ts:14-20), which the plan writer stores verbatim.
func graphgenPlanInput(candidate graphgen.NarrativeThreadPlan) map[string]any {
	return map[string]any{
		"title":              candidate.Title,
		"type":               candidate.Type,
		"authorIntent":       candidate.AuthorIntent,
		"targetStartChapter": int64(candidate.TargetStartChapter),
		"targetEndChapter":   int64(candidate.TargetEndChapter),
	}
}

// narrativeCreatePlanTx is Narrative.CreatePlan on the connection that already
// holds the write lock (narrative.go:75-91): the same INSERT and the same
// read-back, so the domain row and the effect stamp commit together.
func narrativeCreatePlanTx(ctx context.Context, conn *sql.Conn, input map[string]any) (map[string]any, error) {
	args, err := narrativePlanInput(input)
	if err != nil {
		return nil, err
	}
	result, err := conn.ExecContext(ctx, `INSERT INTO narrative_thread_plans
 (title,type,target_start_chapter,target_end_chapter,author_intent) VALUES(?,?,?,?,?)`, args...)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return narrativeGetPlan(ctx, conn, id)
}

// graphConfirmStamp appends the effect onto the attempt's graphEffects array,
// mirroring the reference's update (electron/services/graph-generation.ts:
// 128-130). A graphEffects value that is not an array is refused rather than
// appended to, because JavaScript's spread of a non-array would either scatter a
// string into characters or throw, and neither is a receipt this lane can prove.
func graphConfirmStamp(ctx context.Context, conn *sql.Conn, req GraphConfirmRequest, attemptID string, receipt map[string]any) error {
	row, err := glOne(ctx, conn, "SELECT usage_receipt_json FROM generation_attempts WHERE attempt_id=? AND run_id=?", "GENERATION_ATTEMPT_MISSING", attemptID, req.RunID)
	if err != nil {
		return err
	}
	usage, err := glStored(row["usage_receipt_json"])
	if err != nil {
		return err
	}
	var effects []any
	if existing, present := usage["graphEffects"]; present && existing != nil {
		arr, ok := existing.([]any)
		if !ok {
			return glError("GENERATION_GRAPH_EFFECT_INVALID")
		}
		effects = arr
	}
	receiptJSON, err := plotJSON(receipt)
	if err != nil {
		return err
	}
	effect := map[string]any{
		"version": int64(1),
		"artifact": map[string]any{
			"artifactId": req.Artifact.ArtifactID,
			"revision":   req.Artifact.Revision,
			"textHash":   req.Artifact.TextHash,
		},
		"index":       int64(req.Index),
		"contextHash": req.ContextHash,
		"receipt":     receipt,
		"receiptHash": recoveryHash(receiptJSON),
	}
	effects = append(effects, effect)
	patched, err := glPatch(row["usage_receipt_json"], map[string]any{"graphEffects": effects})
	if err != nil {
		return err
	}
	result, err := conn.ExecContext(ctx, "UPDATE generation_attempts SET usage_receipt_json=? WHERE attempt_id=? AND run_id=?", patched, attemptID, req.RunID)
	return rrAffected(result, err, glError("GENERATION_ATTEMPT_MISSING"))
}

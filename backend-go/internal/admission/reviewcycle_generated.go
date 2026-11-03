package admission

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
)

// VerifyM03GeneratedCycles verifies populated generated cycles on a private,
// caller-owned read-only snapshot. Production admission does not call it.
func VerifyM03GeneratedCycles(ctx context.Context, tx *sql.Tx) error {
	if err := VerifyM03ReviewCycleSchema(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT cycle_id,revision_status FROM review_cycles ORDER BY cycle_id`)
	if err != nil {
		return err
	}
	type cycle struct{ id, status string }
	var cycles []cycle
	for rows.Next() {
		var current cycle
		if err := rows.Scan(&current.id, &current.status); err != nil {
			rows.Close()
			return err
		}
		cycles = append(cycles, current)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, current := range cycles {
		if current.status != "generated" {
			return errors.New("M03 generated verifier received other status")
		}
		basic, err := m03VerifyCycleBasicEvidence(ctx, tx, current.id)
		if err != nil {
			return err
		}
		if basic.revisionStatus != "generated" {
			return errors.New("M03 generated status changed")
		}
		if err := m03VerifyRevisionCycleCore(ctx, tx, basic); err != nil {
			return err
		}
	}
	return nil
}

// m03VerifyRevisionCycleCore is shared by the generated and merge-committed
// lanes. The caller proves schema, basic cycle evidence and its state first.
// This branch has no recheck receipt; committed cycles with recheck_count=1
// require a different proof and must never call this helper alone.
func m03VerifyRevisionCycleCore(ctx context.Context, tx *sql.Tx, basic m03CycleBasicEvidence) error {
	if basic.revisionStatus != "generated" && basic.revisionStatus != "merge-committed" {
		return errors.New("M03 revision cycle status unsupported")
	}
	confirmation, err := m03VerifyConfirmation(ctx, tx, basic)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return errors.New("M03 revision lacks confirmation")
	}
	revision, err := m03VerifyRevisionBinding(ctx, tx, basic, confirmation)
	if err != nil {
		return err
	}
	if revision == nil {
		return errors.New("M03 revision missing")
	}
	var reviewIndex int64
	if err := tx.QueryRowContext(ctx, `SELECT review_index FROM reviews WHERE id=?`, basic.reviewID).Scan(&reviewIndex); err != nil {
		return err
	}
	epoch, err := m03ProveReviewEffect(ctx, tx, m03ReviewEffectExpected{
		RootActionID: basic.rootActionID, ReviewID: basic.reviewID,
		ReviewIndex: reviewIndex, ContentHash: basic.reviewContentHash,
		SourceID: basic.baseDraftID, Chapter: basic.chapterNumber,
		Version: basic.sourceVersion, Status: basic.sourceStatus,
		Content: basic.sourceContent,
	}, m03BuildReviewGenerationReport)
	if err != nil {
		return err
	}
	if err := m03ProveRevisionEffect(ctx, tx, basic, confirmation, revision); err != nil {
		return err
	}
	basic.epoch = epoch // The original ties findings to the review attempt epoch.
	version, _ := m03JSPositiveSafeInteger(confirmation.Snapshot["schemaVersion"])
	proof := m03FindingRowProof{confirmation: &m03FindingConfirmation{
		contentHash: m03StringHash(confirmation.Body), schemaVersion: version,
	}}
	if id, ok := confirmation.Snapshot["cycleId"].(string); ok {
		proof.confirmation.cycleID = id
	}
	if items, ok := confirmation.Snapshot["items"].([]any); ok {
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			findingID, _ := item["findingId"].(string)
			decision, _ := item["decision"].(string)
			proof.confirmation.items = append(proof.confirmation.items,
				m03FindingConfirmationItem{findingID: findingID, decision: decision})
		}
	}
	return m03VerifyFindingRows(ctx, tx, basic, proof)
}

func m03ProveRevisionEffect(ctx context.Context, tx *sql.Tx, basic m03CycleBasicEvidence,
	confirmation *m03Confirmation, revision *m03Revision) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.attempt_id,a.run_id,a.root_action_id,r.root_action_id,
		r.binding_json,a.attempt_json,a.usage_receipt_json
		FROM generation_attempts a JOIN generation_runs r ON r.run_id=a.run_id
		WHERE a.root_action_id=? ORDER BY a.rowid`, basic.rootActionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	matches := 0
	for rows.Next() {
		var row m03EffectAttempt
		if err := rows.Scan(&row.attemptID, &row.runID, &row.rootActionID, &row.runRootActionID,
			&row.bindingJSON, &row.attemptJSON, &row.usageJSON); err != nil {
			return err
		}
		ok, err := m03RevisionEffectMatches(ctx, tx, row, basic, confirmation, revision)
		if err != nil {
			return err
		}
		if ok {
			matches++
		}
		if matches > 1 {
			return errors.New("M03 multiple revision effects")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if matches != 1 {
		return errors.New("M03 revision effect absent")
	}
	return nil
}

func m03RevisionEffectMatches(ctx context.Context, tx *sql.Tx, row m03EffectAttempt,
	basic m03CycleBasicEvidence, confirmation *m03Confirmation, revision *m03Revision) (bool, error) {
	if row.rootActionID != basic.rootActionID || row.runRootActionID != basic.rootActionID ||
		!row.usageJSON.Valid || m03LoneSurrogateEscape(row.bindingJSON) ||
		m03LoneSurrogateEscape(row.attemptJSON) || m03LoneSurrogateEscape(row.usageJSON.String) {
		return false, nil
	}
	binding, err := m03JSONObject([]byte(row.bindingJSON))
	if err != nil {
		return false, nil
	}
	physical, err := m03JSONObject([]byte(row.attemptJSON))
	if err != nil {
		return false, nil
	}
	usage, err := m03JSONObject([]byte(row.usageJSON.String))
	if err != nil {
		return false, nil
	}
	effect, err := m03JSONObject(usage["reviewRevisionEffect"])
	if err != nil || !m03ExactObjectKeys(effect,
		"artifact", "compositionHash", "contentHash", "contextHash", "id", "index", "kind") ||
		!m03JSONEqualsString(effect["kind"], "revision") ||
		!m03JSONEqualsInt(effect["id"], revision.ID) ||
		!m03JSONEqualsInt(effect["index"], revision.Index) ||
		!m03JSONEqualsString(effect["contentHash"], revision.ContentHash) ||
		!m03JSONEqualsString(effect["compositionHash"], revision.ContentHash) {
		return false, nil
	}
	contextHash, ok := m03JSONString(effect["contextHash"])
	if !ok || !m03Hash(contextHash) {
		return false, nil
	}
	artifactRef, err := m03JSONObject(effect["artifact"])
	if err != nil || !m03ExactObjectKeys(artifactRef, "artifactId", "revision", "textHash") {
		return false, nil
	}
	artifactID, ok := m03JSONString(artifactRef["artifactId"])
	if !ok || !m03Nonempty(artifactID) {
		return false, nil
	}
	artifactRevision, ok := m03JSONInt(artifactRef["revision"])
	if !ok || !m03SafeInteger(artifactRevision, 0) {
		return false, nil
	}
	artifactHash, ok := m03JSONString(artifactRef["textHash"])
	if !ok || !m03Hash(artifactHash) {
		return false, nil
	}
	manifest, err := m03JSONObject(binding["sourceManifest"])
	if err != nil {
		return false, nil
	}
	frozen := manifest["reviewRevisionContext"]
	context, err := m03JSONObject(frozen)
	if err != nil || !m03CanonicalJSJSON(frozen) || m03StringHash(string(frozen)) != contextHash {
		return false, nil
	}
	source, err := m03JSONObject(context["source"])
	if err != nil || !m03ExactObjectKeys(source, "chapterNumber", "content", "id", "status", "version") ||
		!m03JSONEqualsInt(context["version"], 1) ||
		!m03JSONEqualsString(context["operation"], "refine-from-review") ||
		!m03JSONEqualsString(context["sourceHash"], basic.sourceHash) ||
		!m03JSONEqualsInt(source["id"], basic.baseDraftID) ||
		!m03JSONEqualsInt(source["chapterNumber"], basic.chapterNumber) ||
		!m03JSONEqualsInt(source["version"], basic.sourceVersion) ||
		!m03JSONEqualsString(source["status"], basic.sourceStatus) ||
		!m03JSONEqualsString(source["content"], basic.sourceContent) ||
		!m03JSONEqualsString(manifest["operation"], "refine-from-review") ||
		!m03JSONEqualsString(manifest["reviewRevisionContextHash"], contextHash) ||
		!m03JSONEqualsString(binding["projectId"], basic.projectID) ||
		!m03JSONIsNonemptyString(binding["epoch"]) ||
		!m03JSONIsObject(context["config"]) ||
		!m03OneOf(context["writingLanguage"], "zh-CN", "en-US") ||
		!m03OneOf(context["uiLocale"], "zh-CN", "en-US") ||
		!m03JSONArray(context["authorInputs"]) || !m03JSONArray(context["history"]) ||
		!m03JSONArray(context["blueprints"]) || !m03JSONArray(context["preflightFindings"]) ||
		!m03JSONIsObject(context["frozenGoals"]) ||
		!m03JSONIsString(context["characterStates"]) ||
		!m03JSONIsString(context["worldbuilding"]) || context["recheck"] != nil {
		return false, nil
	}
	frozenConfirmation, err := m03JSONObject(context["confirmation"])
	if err != nil || !m03ExactObjectKeys(frozenConfirmation,
		"content", "originalReviewContentHash", "reviewSourceId", "snapshot") ||
		!m03JSONEqualsInt(frozenConfirmation["reviewSourceId"], confirmation.ID) ||
		!m03JSONEqualsString(frozenConfirmation["content"], confirmation.Body) ||
		!m03JSONEqualsString(frozenConfirmation["originalReviewContentHash"], basic.reviewContentHash) {
		return false, nil
	}
	var frozenSnapshot any
	if json.Unmarshal(frozenConfirmation["snapshot"], &frozenSnapshot) != nil ||
		!reflect.DeepEqual(frozenSnapshot, confirmation.Snapshot) {
		return false, nil
	}
	var authorInputs []map[string]json.RawMessage
	if json.Unmarshal(manifest["authorInputs"], &authorInputs) != nil || len(authorInputs) != 1 ||
		!m03ExactObjectKeys(authorInputs[0], "id", "text") ||
		!m03JSONEqualsString(authorInputs[0]["id"], "review-revision-context") ||
		!m03JSONEqualsString(authorInputs[0]["text"], string(frozen)) {
		return false, nil
	}
	if !m03OneOf(physical["status"], "settled", "unknown") {
		return false, nil
	}
	result, err := m03JSONObject(usage["result"])
	if err != nil || !m03JSONEqualsString(result["finishReason"], "stop") || result["failureCode"] != nil {
		return false, nil
	}
	var persisted struct {
		attemptID, runID, artifactJSON, status string
		revision                               int64
	}
	err = tx.QueryRowContext(ctx, `SELECT attempt_id,run_id,artifact_json,status,revision
		FROM generation_artifacts WHERE artifact_id=?`, artifactID).Scan(&persisted.attemptID,
		&persisted.runID, &persisted.artifactJSON, &persisted.status, &persisted.revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if persisted.attemptID != row.attemptID || persisted.runID != row.runID ||
		persisted.revision != artifactRevision || persisted.status == "discarded" ||
		m03LoneSurrogateEscape(persisted.artifactJSON) {
		return false, nil
	}
	artifact, err := m03JSONObject([]byte(persisted.artifactJSON))
	if err != nil || !m03JSONEqualsString(artifact["artifactId"], artifactID) ||
		!m03JSONEqualsString(artifact["attemptId"], row.attemptID) ||
		!m03JSONEqualsString(artifact["rootActionId"], row.rootActionID) ||
		!m03JSONEqualsString(artifact["projectId"], basic.projectID) ||
		!m03JSONIsNonemptyString(artifact["epoch"]) ||
		!m03JSONEqualsInt(artifact["revision"], artifactRevision) ||
		!m03JSONEqualsString(artifact["textHash"], artifactHash) ||
		!bytes.Equal(artifact["fingerprint"], binding["fingerprint"]) {
		return false, nil
	}
	artifactText, ok := m03JSONString(artifact["text"])
	if !ok || m03StringHash(artifactText) != artifactHash {
		return false, nil
	}
	identity, err := m03JSONObject(usage["artifactIdentity"])
	if err != nil || !m03JSONEqualsString(identity["artifactId"], artifactID) ||
		!bytes.Equal(identity["epoch"], artifact["epoch"]) ||
		!bytes.Equal(identity["fingerprint"], artifact["fingerprint"]) {
		return false, nil
	}
	if err := m03VerifyRevisionComposition(ctx, tx, m03RevisionCompositionProof{
		AttemptID: row.attemptID, RunID: row.runID, RootActionID: row.rootActionID,
		ProjectID: basic.projectID, ContentHash: revision.ContentHash,
		CurrentArtifactID: artifactID, CurrentArtifactRef: effect["artifact"],
		BindingFingerprint: binding["fingerprint"], UsageReceipt: []byte(row.usageJSON.String),
	}); err != nil {
		return false, nil
	}
	return true, nil
}

package admission

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// m03ReviewReportBuilder must reconstruct JSON.stringify(report, null, 2) from
// the original artifact and frozen reviewRevisionContext. A caller must never
// use persisted review text as the builder's input.
type m03ReviewReportBuilder func(artifactText string, frozenContext json.RawMessage, reportVersion int) (string, error)

type m03ReviewEffectExpected struct {
	RootActionID string
	ReviewID     int64
	ReviewIndex  int64
	ContentHash  string
	SourceID     int64
	Chapter      int64
	Version      int64
	Status       string
	Content      string
}

// m03ProveReviewEffect proves precisely one original review effect for the
// cycle. It is only a component of populated M03 admission: the caller still
// has to verify the cycle, finding anchors, and status-specific constraints.
func m03ProveReviewEffect(ctx context.Context, tx *sql.Tx, want m03ReviewEffectExpected, build m03ReviewReportBuilder) error {
	if build == nil || !m03Nonempty(want.RootActionID) || !m03SafeInteger(want.ReviewID, 1) ||
		!m03SafeInteger(want.ReviewIndex, 1) || !m03Hash(want.ContentHash) ||
		!m03SafeInteger(want.SourceID, 1) || !m03SafeInteger(want.Chapter, 1) ||
		!m03SafeInteger(want.Version, 1) || !m03Nonempty(want.Status) || m03LoneSurrogateEscape(want.Content) {
		return errors.New("M03 review effect input invalid")
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.attempt_id,a.run_id,a.root_action_id,r.root_action_id,
		r.binding_json,a.attempt_json,a.usage_receipt_json
		FROM generation_attempts a JOIN generation_runs r ON r.run_id=a.run_id
		WHERE a.root_action_id=? ORDER BY a.rowid`, want.RootActionID)
	if err != nil {
		return err
	}
	defer rows.Close()
	matched := 0
	for rows.Next() {
		var row m03EffectAttempt
		if err := rows.Scan(&row.attemptID, &row.runID, &row.rootActionID, &row.runRootActionID,
			&row.bindingJSON, &row.attemptJSON, &row.usageJSON); err != nil {
			return err
		}
		ok, err := m03ReviewEffectMatches(ctx, tx, row, want, build)
		if err != nil {
			return err
		}
		if ok {
			matched++
		}
		if matched > 1 {
			return errors.New("M03 multiple matching review effects")
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if matched != 1 {
		return errors.New("M03 review effect absent")
	}
	return nil
}

type m03EffectAttempt struct {
	attemptID, runID, rootActionID, runRootActionID string
	bindingJSON, attemptJSON                        string
	usageJSON                                       sql.NullString
}

func m03ReviewEffectMatches(ctx context.Context, tx *sql.Tx, row m03EffectAttempt, want m03ReviewEffectExpected,
	build m03ReviewReportBuilder) (bool, error) {
	if row.rootActionID != want.RootActionID || row.runRootActionID != want.RootActionID ||
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
	if err != nil {
		return false, nil
	}
	if !m03JSONEqualsString(effect["kind"], "review") || !m03JSONEqualsInt(effect["id"], want.ReviewID) ||
		!m03JSONEqualsInt(effect["index"], want.ReviewIndex) ||
		!m03JSONEqualsString(effect["contentHash"], want.ContentHash) {
		return false, nil
	}
	contextHash, ok := m03JSONString(effect["contextHash"])
	if !ok || !m03Hash(contextHash) {
		return false, nil
	}
	reportVersion := 1
	if raw, exists := effect["reportVersion"]; exists {
		if !m03JSONEqualsInt(raw, 2) {
			return false, nil
		}
		reportVersion = 2
	}
	wantEffectKeys := []string{"artifact", "contentHash", "contextHash", "id", "index", "kind"}
	if reportVersion == 2 {
		wantEffectKeys = append(wantEffectKeys, "reportVersion")
	}
	if !m03ExactObjectKeys(effect, wantEffectKeys...) {
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
	artifactTextHash, ok := m03JSONString(artifactRef["textHash"])
	if !ok || !m03Hash(artifactTextHash) {
		return false, nil
	}
	manifest, err := m03JSONObject(binding["sourceManifest"])
	if err != nil {
		return false, nil
	}
	frozen := manifest["reviewRevisionContext"]
	context, err := m03JSONObject(frozen)
	if err != nil {
		return false, nil
	}
	source, err := m03JSONObject(context["source"])
	if err != nil {
		return false, nil
	}
	if !m03ExactObjectKeys(source, "chapterNumber", "content", "id", "status", "version") ||
		!m03JSONEqualsInt(context["version"], 1) ||
		!m03JSONEqualsString(context["operation"], "review-chapter") ||
		!m03JSONEqualsString(context["sourceHash"], m03StringHash(want.Content)) ||
		!m03JSONEqualsInt(source["id"], want.SourceID) ||
		!m03JSONEqualsInt(source["chapterNumber"], want.Chapter) ||
		!m03JSONEqualsInt(source["version"], want.Version) ||
		!m03JSONEqualsString(source["status"], want.Status) ||
		!m03JSONEqualsString(source["content"], want.Content) ||
		!m03JSONEqualsString(manifest["operation"], "review-chapter") ||
		!m03JSONEqualsString(manifest["reviewRevisionContextHash"], contextHash) {
		return false, nil
	}
	// The original hashes JSON.stringify(context). Require that the frozen raw
	// object is already that byte representation; noncanonical encodings fail
	// closed until an ordered JS serializer is attached here.
	if !json.Valid(frozen) || m03LoneSurrogateEscape(string(frozen)) ||
		m03StringHash(string(frozen)) != contextHash {
		return false, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, frozen); err != nil || !bytes.Equal(compact.Bytes(), frozen) {
		return false, nil
	}
	if _, err := m03JSONObject(context["config"]); err != nil {
		return false, nil
	}
	if !m03OneOf(context["writingLanguage"], "zh-CN", "en-US") ||
		!m03OneOf(context["uiLocale"], "zh-CN", "en-US") ||
		!m03JSONArray(context["authorInputs"]) || !m03JSONArray(context["history"]) ||
		!m03JSONArray(context["blueprints"]) || !m03JSONArray(context["preflightFindings"]) ||
		!m03JSONIsObject(context["frozenGoals"]) || !m03JSONIsString(context["characterStates"]) ||
		!m03JSONIsString(context["worldbuilding"]) || context["confirmation"] != nil ||
		context["recheck"] != nil {
		return false, nil
	}
	var authorInputs []map[string]json.RawMessage
	if err := json.Unmarshal(manifest["authorInputs"], &authorInputs); err != nil || len(authorInputs) != 1 ||
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
		AttemptID, RunID, ArtifactJSON, Status string
		Revision                               int64
	}
	err = tx.QueryRowContext(ctx, `SELECT attempt_id,run_id,artifact_json,status,revision
		FROM generation_artifacts WHERE artifact_id=?`, artifactID).Scan(
		&persisted.AttemptID, &persisted.RunID, &persisted.ArtifactJSON, &persisted.Status, &persisted.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if persisted.AttemptID != row.attemptID || persisted.RunID != row.runID ||
		persisted.Revision != artifactRevision || persisted.Status == "discarded" ||
		m03LoneSurrogateEscape(persisted.ArtifactJSON) {
		return false, nil
	}
	artifact, err := m03JSONObject([]byte(persisted.ArtifactJSON))
	if err != nil {
		return false, nil
	}
	artifactText, ok := m03JSONString(artifact["text"])
	if !ok || !m03JSONEqualsString(artifact["artifactId"], artifactID) ||
		!m03JSONEqualsString(artifact["attemptId"], row.attemptID) ||
		!m03JSONEqualsString(artifact["rootActionId"], row.rootActionID) ||
		!m03JSONEqualsString(artifact["projectId"], m03MustJSONString(binding["projectId"])) ||
		!m03JSONIsNonemptyString(artifact["epoch"]) ||
		!m03JSONEqualsInt(artifact["revision"], artifactRevision) ||
		!m03JSONEqualsString(artifact["textHash"], artifactTextHash) ||
		m03StringHash(artifactText) != artifactTextHash ||
		!bytes.Equal(artifact["fingerprint"], binding["fingerprint"]) {
		return false, nil
	}
	identity, err := m03JSONObject(usage["artifactIdentity"])
	if err != nil || !m03JSONEqualsString(identity["artifactId"], artifactID) ||
		!bytes.Equal(identity["epoch"], artifact["epoch"]) ||
		!bytes.Equal(identity["fingerprint"], artifact["fingerprint"]) {
		return false, nil
	}
	rebuilt, err := build(artifactText, frozen, reportVersion)
	if err != nil {
		return false, nil
	}
	if m03StringHash(rebuilt) != want.ContentHash {
		return false, nil
	}
	return true, nil
}

func m03JSONObject(raw []byte) (map[string]json.RawMessage, error) {
	var value map[string]json.RawMessage
	if len(raw) == 0 || m03LoneSurrogateEscape(string(raw)) || json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, errors.New("M03 JSON object invalid")
	}
	return value, nil
}

func m03ExactObjectKeys(value map[string]json.RawMessage, keys ...string) bool {
	if len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := value[key]; !ok {
			return false
		}
	}
	return true
}

func m03JSONString(raw []byte) (string, bool) {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	return value, true
}
func m03MustJSONString(raw []byte) string { value, _ := m03JSONString(raw); return value }
func m03JSONEqualsString(raw []byte, value string) bool {
	actual, ok := m03JSONString(raw)
	return ok && actual == value
}
func m03JSONIsString(raw []byte) bool { _, ok := m03JSONString(raw); return ok }
func m03JSONIsNonemptyString(raw []byte) bool {
	value, ok := m03JSONString(raw)
	return ok && m03Nonempty(value)
}
func m03JSONInt(raw []byte) (int64, bool) {
	var number json.Number
	if len(raw) == 0 || raw[0] != '-' && (raw[0] < '0' || raw[0] > '9') ||
		json.Unmarshal(raw, &number) != nil {
		return 0, false
	}
	value, err := number.Int64()
	return value, err == nil
}
func m03JSONEqualsInt(raw []byte, value int64) bool {
	actual, ok := m03JSONInt(raw)
	return ok && actual == value
}
func m03OneOf(raw []byte, values ...string) bool {
	value, ok := m03JSONString(raw)
	if !ok {
		return false
	}
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func m03JSONArray(raw []byte) bool    { return len(raw) > 0 && raw[0] == '[' && json.Valid(raw) }
func m03JSONIsObject(raw []byte) bool { _, err := m03JSONObject(raw); return err == nil }

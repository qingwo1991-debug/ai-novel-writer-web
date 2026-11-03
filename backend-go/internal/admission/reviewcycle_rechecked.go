package admission

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewrecheck"
)

// VerifyM03RecheckedCycles proves the populated, merge-committed M03 subset
// with exactly one recheck on a caller-owned read-only snapshot. It is not
// connected to production author-data admission.
func VerifyM03RecheckedCycles(ctx context.Context, tx *sql.Tx) error {
	if err := VerifyM03ReviewCycleSchema(ctx, tx); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, "SELECT cycle_id,revision_status,recheck_count,recheck_attempt_id FROM review_cycles ORDER BY cycle_id")
	if err != nil {
		return err
	}
	type cycle struct {
		id, status string
		count      int64
		attempt    sql.NullString
	}
	var cycles []cycle
	for rows.Next() {
		var current cycle
		if err := rows.Scan(&current.id, &current.status, &current.count, &current.attempt); err != nil {
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
		if current.status != "merge-committed" || current.count != 1 ||
			!current.attempt.Valid || !m03Nonempty(current.attempt.String) {
			return errors.New("M03 rechecked cycle status unsupported")
		}
		basic, err := m03VerifyCycleBasicEvidence(ctx, tx, current.id)
		if err != nil {
			return err
		}
		if basic.revisionStatus != "merge-committed" {
			return errors.New("M03 rechecked cycle status changed")
		}
		if err := m03VerifyRecheckedCycle(ctx, tx, basic, current.attempt.String); err != nil {
			return err
		}
	}
	return nil
}

type m03RecheckedState struct {
	basic                  m03CycleBasicEvidence
	confirmation           *m03Confirmation
	mergedBody, mergedHash string
	comparisonVersion      int64
	eligible               []m03RecheckFinding
	contextFindings        []map[string]any
	eligibleSet            map[string]bool
}

func m03VerifyRecheckedCycle(ctx context.Context, tx *sql.Tx, basic m03CycleBasicEvidence, attemptID string) error {
	confirmation, err := m03VerifyConfirmation(ctx, tx, basic)
	if err != nil {
		return err
	}
	if confirmation == nil {
		return errors.New("M03 recheck revision lacks confirmation")
	}
	revision, err := m03VerifyRevisionBinding(ctx, tx, basic, confirmation)
	if err != nil {
		return err
	}
	if revision == nil {
		return errors.New("M03 recheck revision missing")
	}
	var reviewIndex int64
	if err := tx.QueryRowContext(ctx, "SELECT review_index FROM reviews WHERE id=?", basic.reviewID).Scan(&reviewIndex); err != nil {
		return err
	}
	epoch, err := m03ProveReviewEffect(ctx, tx, m03ReviewEffectExpected{
		RootActionID: basic.rootActionID, ReviewID: basic.reviewID, ReviewIndex: reviewIndex,
		ContentHash: basic.reviewContentHash, SourceID: basic.baseDraftID, Chapter: basic.chapterNumber,
		Version: basic.sourceVersion, Status: basic.sourceStatus, Content: basic.sourceContent,
	}, m03BuildReviewGenerationReport)
	if err != nil {
		return err
	}
	if err := m03ProveRevisionEffect(ctx, tx, basic, confirmation, revision); err != nil {
		return err
	}
	basic.epoch = epoch
	state, err := m03PrepareRecheckedState(ctx, tx, basic, confirmation)
	if err != nil {
		return err
	}
	evidence, err := m03ProveRecheckAttempt(ctx, tx, state, attemptID)
	if err != nil {
		return err
	}
	version, _ := m03JSPositiveSafeInteger(confirmation.Snapshot["schemaVersion"])
	proof := m03FindingRowProof{
		recheckCount: 1, eligible: state.eligibleSet, unshown: map[string]bool{},
		recheckEvidence: map[string]m03FindingRecheckEvidence{},
		confirmation: &m03FindingConfirmation{
			contentHash: m03StringHash(confirmation.Body), schemaVersion: version,
		},
	}
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
	for id, item := range evidence {
		proof.recheckEvidence[id] = m03FindingRecheckEvidence{
			targetID: item.TargetID, evidenceHash: item.EvidenceHash, resolved: item.Resolved,
		}
	}
	return m03VerifyFindingRows(ctx, tx, basic, proof)
}

func m03PrepareRecheckedState(ctx context.Context, tx *sql.Tx, basic m03CycleBasicEvidence,
	confirmation *m03Confirmation) (m03RecheckedState, error) {
	out := m03RecheckedState{basic: basic, confirmation: confirmation, eligibleSet: map[string]bool{}}
	if err := tx.QueryRowContext(ctx, "SELECT comparison_version,merged_hash FROM review_cycles WHERE cycle_id=?",
		basic.cycleID).Scan(&out.comparisonVersion, &out.mergedHash); err != nil {
		return out, err
	}
	if !m03SafeInteger(out.comparisonVersion, 1) || !m03Hash(out.mergedHash) {
		return out, errors.New("M03 recheck comparison invalid")
	}
	mergeTable, err := m03TableExists(ctx, tx, "review_cycle_merges")
	if err != nil {
		return out, err
	}
	if mergeTable {
		err = tx.QueryRowContext(ctx, "SELECT body FROM review_cycle_merges WHERE cycle_id=?", basic.cycleID).Scan(&out.mergedBody)
	} else {
		err = tx.QueryRowContext(ctx, "SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id=?",
			basic.baseDraftID).Scan(&out.mergedBody)
	}
	if err != nil {
		return out, err
	}
	if m03StringHash(out.mergedBody) != out.mergedHash {
		return out, errors.New("M03 recheck merged body differs")
	}
	// The original can synthesize unshown mustShow goals from confirmation.
	// Until that projection has a separately qualified Go port, fail closed
	// whenever the confirmation could supply such a goal.
	if items, ok := confirmation.Snapshot["items"].([]any); ok {
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			if item["origin"] == "ai" && item["decision"] == "apply" && item["goalId"] != nil && item["findingId"] != nil {
				return out, errors.New("M03 unshown mustShow recheck unsupported")
			}
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT finding_id,target_id,category,kind,problem_text,expected_text,status,span_start,span_end,span_unit,occurrence FROM review_findings WHERE cycle_id=? ORDER BY finding_id", basic.cycleID)
	if err != nil {
		return out, err
	}
	var findings []m03RecheckFinding
	for rows.Next() {
		var finding m03RecheckFinding
		var target, expected, unit sql.NullString
		var start, end, occurrence sql.NullInt64
		if err := rows.Scan(&finding.FindingID, &target, &finding.Category, &finding.Kind, &finding.Problem,
			&expected, &finding.Status, &start, &end, &unit, &occurrence); err != nil {
			rows.Close()
			return out, err
		}
		if target.Valid {
			finding.TargetID = target.String
		}
		if expected.Valid {
			finding.Expected = expected.String
		}
		if start.Valid && end.Valid && unit.Valid && unit.String == "utf16-code-unit" && occurrence.Valid {
			finding.HasSpan = true
			finding.SpanStart, finding.SpanEnd, finding.Occurrence = start.Int64, end.Int64, occurrence.Int64
		}
		findings = append(findings, finding)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return out, err
	}
	out.eligible, err = m03EligibleRecheck(basic.sourceContent, out.mergedBody, findings, nil)
	if err != nil {
		return out, err
	}
	for _, finding := range out.eligible {
		excerpt, ok := m03UTF16Slice(basic.sourceContent, finding.SpanStart, finding.SpanEnd)
		if !ok {
			return out, errors.New("M03 recheck excerpt unsupported")
		}
		row := map[string]any{
			"findingId": finding.FindingID, "targetId": finding.TargetID,
			"category": finding.Category, "kind": finding.Kind, "problem": finding.Problem,
			"sourceSpan": map[string]any{"start": float64(finding.SpanStart), "end": float64(finding.SpanEnd), "unit": "utf16-code-unit"},
			"occurrence": float64(finding.Occurrence), "sourceExcerpt": excerpt,
		}
		if finding.Expected != "" {
			row["expected"] = finding.Expected
		}
		out.contextFindings = append(out.contextFindings, row)
		out.eligibleSet[finding.FindingID] = true
	}
	return out, nil
}

func m03RecheckContextEqual(raw json.RawMessage, state m03RecheckedState, version int64) bool {
	var actual any
	if json.Unmarshal(raw, &actual) != nil {
		return false
	}
	findings := make([]any, 0, len(state.contextFindings))
	for _, finding := range state.contextFindings {
		findings = append(findings, finding)
	}
	expected := map[string]any{
		"version": float64(version), "cycleId": state.basic.cycleID,
		"comparisonVersion": float64(state.comparisonVersion),
		"mergedHash":        state.mergedHash, "findingSetHash": state.basic.findingSetHash,
		"findings": findings,
	}
	return reflect.DeepEqual(actual, expected)
}

func m03RecheckBuildReport(artifact string, state m03RecheckedState, version int64, locale string,
	reportVersion int) (reviewrecheck.Report, []byte, error) {
	findings := make([]reviewrecheck.Finding, 0, len(state.eligible))
	for _, finding := range state.eligible {
		findings = append(findings, reviewrecheck.Finding{
			FindingID: finding.FindingID, TargetID: finding.TargetID,
			Category: finding.Category, Kind: finding.Kind,
		})
	}
	report, err := reviewrecheck.Build(artifact, state.mergedBody,
		reviewrecheck.Context{Version: int(version), Findings: findings}, locale, reportVersion)
	if err != nil {
		return report, nil, err
	}
	full := report.MarshalJSJSON()
	suffix := []byte("],\"decisions\":")
	mark := bytes.LastIndex(full, suffix)
	if mark < 0 {
		return report, nil, errors.New("M03 recheck report malformed")
	}
	compact := append(append([]byte(nil), full[:mark+1]...), '}')
	var pretty bytes.Buffer
	if err := json.Indent(&pretty, compact, "", "  "); err != nil {
		return report, nil, err
	}
	return report, pretty.Bytes(), nil
}

func m03RecheckReportItems(report reviewrecheck.Report) ([]m03RecheckReportItem, error) {
	full := report.MarshalJSJSON()
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(full, &parsed); err != nil {
		return nil, err
	}
	var rawItems []json.RawMessage
	if err := json.Unmarshal(parsed["items"], &rawItems); err != nil {
		return nil, err
	}
	if len(rawItems) != len(report.Items) {
		return nil, fmt.Errorf("M03 recheck report item count differs")
	}
	out := make([]m03RecheckReportItem, len(rawItems))
	for i, raw := range rawItems {
		out[i] = m03RecheckReportItem{CanonicalJSON: string(raw), Resolved: report.Items[i].Resolved}
	}
	return out, nil
}

func m03ProveRecheckAttempt(ctx context.Context, tx *sql.Tx, state m03RecheckedState,
	attemptID string) (map[string]m03RecheckEvidence, error) {
	var row m03EffectAttempt
	err := tx.QueryRowContext(ctx, "SELECT a.attempt_id,a.run_id,a.root_action_id,r.root_action_id,r.binding_json,a.attempt_json,a.usage_receipt_json FROM generation_attempts a JOIN generation_runs r ON r.run_id=a.run_id WHERE a.attempt_id=? AND a.root_action_id=?",
		attemptID, state.basic.rootActionID).Scan(&row.attemptID, &row.runID, &row.rootActionID,
		&row.runRootActionID, &row.bindingJSON, &row.attemptJSON, &row.usageJSON)
	if err != nil {
		return nil, err
	}
	if row.rootActionID != state.basic.rootActionID || row.runRootActionID != state.basic.rootActionID ||
		!row.usageJSON.Valid || m03LoneSurrogateEscape(row.bindingJSON) ||
		m03LoneSurrogateEscape(row.attemptJSON) || m03LoneSurrogateEscape(row.usageJSON.String) {
		return nil, errors.New("M03 recheck attempt binding differs")
	}
	binding, err := m03JSONObject([]byte(row.bindingJSON))
	if err != nil {
		return nil, err
	}
	physical, err := m03JSONObject([]byte(row.attemptJSON))
	if err != nil {
		return nil, err
	}
	usage, err := m03JSONObject([]byte(row.usageJSON.String))
	if err != nil {
		return nil, err
	}
	effect, err := m03JSONObject(usage["reviewRevisionEffect"])
	if err != nil {
		return nil, err
	}
	version := int64(1)
	if raw, present := effect["reportVersion"]; present {
		if !m03JSONEqualsInt(raw, 2) {
			return nil, errors.New("M03 recheck report version invalid")
		}
		version = 2
	}
	keys := []string{"artifact", "contentHash", "contextHash", "id", "index", "kind"}
	if version == 2 {
		keys = append(keys, "reportVersion")
	}
	if !m03ExactObjectKeys(effect, keys...) || !m03JSONEqualsString(effect["kind"], "review") {
		return nil, errors.New("M03 recheck effect shape invalid")
	}
	reviewID, ok := m03JSONInt(effect["id"])
	if !ok || !m03SafeInteger(reviewID, 1) {
		return nil, errors.New("M03 recheck review ID invalid")
	}
	var sourceContent, reviewBody, sourceStatus string
	var baseID, reviewIndex, chapter, sourceVersion int64
	err = tx.QueryRowContext(ctx, "SELECT r.base_draft_id,r.review_index,r.source_draft_chapter_number,r.source_draft_version,r.source_draft_status,r.source_content,c.body FROM reviews r JOIN contents c ON c.id=r.content_id WHERE r.id=?",
		reviewID).Scan(&baseID, &reviewIndex, &chapter, &sourceVersion, &sourceStatus, &sourceContent, &reviewBody)
	if err != nil {
		return nil, err
	}
	var draftChapter, draftVersion int64
	var draftStatus string
	err = tx.QueryRowContext(ctx, "SELECT chapter_number,version,status FROM drafts WHERE id=?",
		state.basic.baseDraftID).Scan(&draftChapter, &draftVersion, &draftStatus)
	if err != nil {
		return nil, err
	}
	if baseID != state.basic.baseDraftID || chapter != draftChapter || sourceVersion != draftVersion ||
		sourceStatus != "revised" || draftStatus != "revised" || m03StringHash(sourceContent) != state.mergedHash ||
		!m03JSONEqualsInt(effect["id"], reviewID) || !m03JSONEqualsInt(effect["index"], reviewIndex) ||
		!m03JSONEqualsString(effect["contentHash"], m03StringHash(reviewBody)) {
		return nil, errors.New("M03 recheck review source differs")
	}
	contextHash, ok := m03JSONString(effect["contextHash"])
	if !ok || !m03Hash(contextHash) {
		return nil, errors.New("M03 recheck context hash invalid")
	}
	receipt, err := m03JSONObject(usage["reviewCycleRecheck"])
	if err != nil {
		return nil, err
	}
	receiptVersion, ok := m03JSONInt(receipt["version"])
	if !ok || (receiptVersion != 1 && receiptVersion != 2) {
		return nil, errors.New("M03 recheck receipt version invalid")
	}
	manifest, err := m03JSONObject(binding["sourceManifest"])
	if err != nil {
		return nil, err
	}
	frozen := manifest["reviewRevisionContext"]
	context, err := m03JSONObject(frozen)
	if err != nil || !m03CanonicalJSJSON(frozen) || m03StringHash(string(frozen)) != contextHash {
		return nil, errors.New("M03 recheck frozen context invalid")
	}
	frozenRecheck, err := m03JSONObject(context["recheck"])
	if err != nil {
		return nil, err
	}
	frozenVersion, ok := m03JSONInt(frozenRecheck["version"])
	if !ok || frozenVersion != receiptVersion && !(frozenVersion == 1 && receiptVersion == 2) {
		return nil, errors.New("M03 recheck version pair invalid")
	}
	source, err := m03JSONObject(context["source"])
	if err != nil || !m03ExactObjectKeys(source, "chapterNumber", "content", "id", "status", "version") ||
		!m03JSONEqualsInt(context["version"], 1) ||
		!m03JSONEqualsString(context["operation"], "review-chapter") ||
		!m03JSONEqualsString(context["sourceHash"], state.mergedHash) ||
		!m03JSONEqualsInt(source["id"], state.basic.baseDraftID) ||
		!m03JSONEqualsInt(source["chapterNumber"], draftChapter) ||
		!m03JSONEqualsInt(source["version"], draftVersion) ||
		!m03JSONEqualsString(source["status"], "revised") ||
		!m03JSONEqualsString(source["content"], sourceContent) ||
		!m03JSONEqualsString(manifest["operation"], "review-chapter") ||
		!m03JSONEqualsString(manifest["reviewRevisionContextHash"], contextHash) ||
		!m03JSONEqualsString(binding["projectId"], state.basic.projectID) ||
		!m03JSONIsNonemptyString(binding["epoch"]) ||
		!m03JSONIsObject(context["config"]) ||
		!m03OneOf(context["writingLanguage"], "zh-CN", "en-US") ||
		!m03OneOf(context["uiLocale"], "zh-CN", "en-US") ||
		!m03JSONArray(context["authorInputs"]) || !m03JSONArray(context["history"]) ||
		!m03JSONArray(context["blueprints"]) || !m03JSONArray(context["preflightFindings"]) ||
		!m03JSONIsObject(context["frozenGoals"]) ||
		!m03JSONIsString(context["characterStates"]) || !m03JSONIsString(context["worldbuilding"]) ||
		context["confirmation"] != nil || !m03RecheckContextEqual(context["recheck"], state, frozenVersion) {
		return nil, errors.New("M03 recheck context differs")
	}
	var authorInputs []map[string]json.RawMessage
	if json.Unmarshal(manifest["authorInputs"], &authorInputs) != nil || len(authorInputs) != 1 ||
		!m03ExactObjectKeys(authorInputs[0], "id", "text") ||
		!m03JSONEqualsString(authorInputs[0]["id"], "review-revision-context") ||
		!m03JSONEqualsString(authorInputs[0]["text"], string(frozen)) {
		return nil, errors.New("M03 recheck source manifest differs")
	}
	if !m03OneOf(physical["status"], "settled", "unknown") {
		return nil, errors.New("M03 recheck attempt status invalid")
	}
	result, err := m03JSONObject(usage["result"])
	if err != nil || !m03JSONEqualsString(result["finishReason"], "stop") || result["failureCode"] != nil {
		return nil, errors.New("M03 recheck attempt finish invalid")
	}
	artifactRef, err := m03JSONObject(effect["artifact"])
	if err != nil || !m03ExactObjectKeys(artifactRef, "artifactId", "revision", "textHash") {
		return nil, errors.New("M03 recheck artifact ref invalid")
	}
	artifactID, ok := m03JSONString(artifactRef["artifactId"])
	if !ok || !m03Nonempty(artifactID) {
		return nil, errors.New("M03 recheck artifact ID invalid")
	}
	artifactRevision, ok := m03JSONInt(artifactRef["revision"])
	if !ok || !m03SafeInteger(artifactRevision, 0) {
		return nil, errors.New("M03 recheck artifact revision invalid")
	}
	artifactHash, ok := m03JSONString(artifactRef["textHash"])
	if !ok || !m03Hash(artifactHash) {
		return nil, errors.New("M03 recheck artifact hash invalid")
	}
	var persisted struct {
		attemptID, runID, artifactJSON, status string
		revision                               int64
	}
	err = tx.QueryRowContext(ctx, "SELECT attempt_id,run_id,artifact_json,status,revision FROM generation_artifacts WHERE artifact_id=?",
		artifactID).Scan(&persisted.attemptID, &persisted.runID, &persisted.artifactJSON,
		&persisted.status, &persisted.revision)
	if err != nil {
		return nil, err
	}
	if persisted.attemptID != row.attemptID || persisted.runID != row.runID ||
		persisted.revision != artifactRevision || persisted.status == "discarded" ||
		m03LoneSurrogateEscape(persisted.artifactJSON) {
		return nil, errors.New("M03 recheck artifact row differs")
	}
	artifact, err := m03JSONObject([]byte(persisted.artifactJSON))
	if err != nil {
		return nil, err
	}
	artifactText, ok := m03JSONString(artifact["text"])
	if !ok || !m03JSONEqualsString(artifact["artifactId"], artifactID) ||
		!m03JSONEqualsString(artifact["attemptId"], row.attemptID) ||
		!m03JSONEqualsString(artifact["rootActionId"], row.rootActionID) ||
		!m03JSONEqualsString(artifact["projectId"], state.basic.projectID) ||
		!m03JSONIsNonemptyString(artifact["epoch"]) ||
		!m03JSONEqualsInt(artifact["revision"], artifactRevision) ||
		!m03JSONEqualsString(artifact["textHash"], artifactHash) ||
		m03StringHash(artifactText) != artifactHash ||
		!bytes.Equal(artifact["fingerprint"], binding["fingerprint"]) {
		return nil, errors.New("M03 recheck artifact identity differs")
	}
	identity, err := m03JSONObject(usage["artifactIdentity"])
	if err != nil || !m03JSONEqualsString(identity["artifactId"], artifactID) ||
		!bytes.Equal(identity["epoch"], artifact["epoch"]) ||
		!bytes.Equal(identity["fingerprint"], artifact["fingerprint"]) {
		return nil, errors.New("M03 recheck artifact receipt identity differs")
	}
	locale, _ := m03JSONString(context["uiLocale"])
	report, pretty, err := m03RecheckBuildReport(artifactText, state, frozenVersion, locale, int(version))
	if err != nil {
		return nil, err
	}
	if m03StringHash(string(pretty)) != m03StringHash(reviewBody) {
		return nil, errors.New("M03 recheck saved report differs")
	}
	reportItems, err := m03RecheckReportItems(report)
	if err != nil {
		return nil, err
	}
	_, evidence, err := m03RecheckReceiptProof(row.usageJSON.String, m03RecheckReceiptExpected{
		CycleID: state.basic.cycleID, ComparisonVersion: state.comparisonVersion,
		MergedHash: state.mergedHash, FindingSetHash: state.basic.findingSetHash,
		Eligible: state.eligible, ReportItems: reportItems,
	})
	if err != nil {
		return nil, err
	}
	return evidence, nil
}

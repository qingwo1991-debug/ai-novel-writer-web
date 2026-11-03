package admission

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"unicode"
	"unicode/utf16"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
)

// m03Confirmation is the canonical object returned by the original
// parseHumanConfirmedReviewSnapshot, plus the unmodified persisted body.
type m03Confirmation struct {
	ID       int64
	Body     string
	Snapshot map[string]any
}

type m03Revision struct {
	ID, Index, BaseDraftID, ReviewSourceID int64
	Status, Body, ContentHash              string
	MergedToDraftID                        sql.NullInt64
}

// m03VerifyConfirmation ports the confirmation block in verifyM03ReviewCycle.
// Its return alone does not admit a populated cycle: the caller must also
// prove the original review effect, all finding rows, and any revision effect.
func m03VerifyConfirmation(ctx context.Context, tx *sql.Tx, cycle m03CycleBasicEvidence) (*m03Confirmation, error) {
	var id sql.NullInt64
	var contentHash sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT confirmation_review_id,confirmation_content_hash
		FROM review_cycles WHERE cycle_id=?`, cycle.cycleID).Scan(&id, &contentHash); err != nil {
		return nil, err
	}
	if !id.Valid {
		if contentHash.Valid {
			return nil, errors.New("M03 orphan confirmation hash")
		}
		return nil, nil
	}
	if !m03SafeInteger(id.Int64, 1) || !contentHash.Valid || !m03Hash(contentHash.String) {
		return nil, errors.New("M03 confirmation binding invalid")
	}
	var source sql.NullString
	var body string
	if err := tx.QueryRowContext(ctx, `SELECT r.source_content,c.body FROM reviews r
		JOIN contents c ON c.id=r.content_id WHERE r.id=? AND r.base_draft_id=?`, id.Int64, cycle.baseDraftID).Scan(&source, &body); err != nil {
		return nil, err
	}
	if !source.Valid || m03StringHash(source.String) != cycle.sourceHash || m03StringHash(body) != contentHash.String {
		return nil, errors.New("M03 confirmation content differs")
	}
	snapshot, err := m03ParseConfirmationSnapshot(body)
	if err != nil || !m03JSNumberEqual(snapshot["sourceReviewId"], cycle.reviewID) {
		return nil, errors.New("M03 confirmation snapshot invalid")
	}
	draft, _ := snapshot["sourceDraft"].(map[string]any)
	if draft == nil || !m03JSNumberEqual(draft["id"], cycle.baseDraftID) ||
		!m03JSNumberEqual(draft["version"], cycle.sourceVersion) {
		return nil, errors.New("M03 confirmation source draft invalid")
	}
	content, _ := draft["content"].(string)
	if m03StringHash(content) != cycle.sourceHash {
		return nil, errors.New("M03 confirmation source draft hash differs")
	}
	return &m03Confirmation{ID: id.Int64, Body: body, Snapshot: snapshot}, nil
}

// m03VerifyRevisionBinding ports the SQL/source/status binding before effect
// and composition verification. The latter must be performed by the caller.
func m03VerifyRevisionBinding(ctx context.Context, tx *sql.Tx, cycle m03CycleBasicEvidence, confirmation *m03Confirmation) (*m03Revision, error) {
	var id sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT revision_id FROM review_cycles WHERE cycle_id=?`, cycle.cycleID).Scan(&id); err != nil {
		return nil, err
	}
	if !id.Valid {
		return nil, nil
	}
	if confirmation == nil || !m03SafeInteger(id.Int64, 1) {
		return nil, errors.New("M03 revision confirmation missing")
	}
	var revision m03Revision
	var sourceChapter, sourceVersion, reviewSource sql.NullInt64
	var sourceStatus, sourceContent sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT r.id,r.base_draft_id,r.review_source_id,r.source_draft_chapter_number,
		r.source_draft_version,r.source_draft_status,r.source_content,r.status,r.merged_to_draft_id,
		c.body,r.revision_index FROM revisions r JOIN contents c ON c.id=r.content_id WHERE r.id=?`, id.Int64).Scan(
		&revision.ID, &revision.BaseDraftID, &reviewSource, &sourceChapter, &sourceVersion,
		&sourceStatus, &sourceContent, &revision.Status, &revision.MergedToDraftID, &revision.Body, &revision.Index)
	if err != nil {
		return nil, err
	}
	if revision.ID != id.Int64 || revision.BaseDraftID != cycle.baseDraftID || !reviewSource.Valid ||
		reviewSource.Int64 != confirmation.ID || !sourceChapter.Valid || sourceChapter.Int64 != cycle.chapterNumber ||
		!sourceVersion.Valid || sourceVersion.Int64 != cycle.sourceVersion || !sourceStatus.Valid ||
		sourceStatus.String != cycle.sourceStatus || !sourceContent.Valid || sourceContent.String != cycle.sourceContent ||
		m03StringHash(sourceContent.String) != cycle.sourceHash || !m03SafeInteger(revision.Index, 0) {
		return nil, errors.New("M03 revision source differs")
	}
	if cycle.revisionStatus == "generated" && revision.Status != "pending" ||
		cycle.revisionStatus == "merge-committed" && (revision.Status != "merged" ||
			!revision.MergedToDraftID.Valid || revision.MergedToDraftID.Int64 != cycle.baseDraftID) {
		return nil, errors.New("M03 revision state differs")
	}
	revision.ReviewSourceID = reviewSource.Int64
	revision.ContentHash = m03StringHash(revision.Body)
	return &revision, nil
}

func m03ParseConfirmationSnapshot(body string) (map[string]any, error) {
	if m03LoneSurrogateEscape(body) {
		return nil, errors.New("M03 snapshot has isolated surrogate")
	}
	var raw any
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		return nil, err
	}
	object, ok := raw.(map[string]any)
	if !ok || object["kind"] != "human-confirmed-review" {
		return nil, errors.New("M03 snapshot kind invalid")
	}
	version, ok := m03JSPositiveSafeInteger(object["schemaVersion"])
	if !ok || version != 1 && version != 2 {
		return nil, errors.New("M03 snapshot version invalid")
	}
	sourceID, ok := m03JSPositiveSafeInteger(object["sourceReviewId"])
	if !ok {
		return nil, errors.New("M03 snapshot source review invalid")
	}
	items, ok := object["items"].([]any)
	if !ok {
		return nil, errors.New("M03 snapshot items invalid")
	}
	cycleID, hasCycleID := object["cycleId"]
	if hasCycleID && !m03JSNonemptyValue(cycleID) || version == 1 && hasCycleID || version == 2 && !hasCycleID {
		return nil, errors.New("M03 snapshot cycle invalid")
	}
	summary, summaryOK := object["summary"].(string)
	guidance, guidanceOK := object["authorGuidance"].(string)
	if !summaryOK || !guidanceOK {
		return nil, errors.New("M03 snapshot prose invalid")
	}
	canonical := map[string]any{"kind": "human-confirmed-review", "schemaVersion": float64(version),
		"sourceReviewId": float64(sourceID), "summary": summary, "authorGuidance": guidance}
	if hasCycleID {
		canonical["cycleId"] = m03JSTrim(cycleID.(string))
	}
	if value, present := object["sourceDraft"]; present {
		draft, err := m03ParseConfirmationSource(value)
		if err != nil {
			return nil, err
		}
		canonical["sourceDraft"] = draft
	}
	if value, present := object["goalReview"]; present {
		if err := m03ValidateChapterGoalReview(value); err != nil {
			return nil, err
		}
		// parseChapterGoalReview returns the original object, including extra
		// fields; only its nested values are validated.
		canonical["goalReview"] = value
	}
	parsedItems := make([]any, 0, len(items))
	for _, value := range items {
		item, err := m03ParseConfirmationItem(value, version)
		if err != nil {
			return nil, err
		}
		parsedItems = append(parsedItems, item)
	}
	canonical["items"] = parsedItems
	return canonical, nil
}

func m03ParseConfirmationSource(value any) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("M03 snapshot source invalid")
	}
	id, idOK := m03JSPositiveSafeInteger(object["id"])
	chapter, chapterOK := m03JSPositiveSafeInteger(object["chapterNumber"])
	version, versionOK := m03JSPositiveSafeInteger(object["version"])
	status, statusOK := object["status"].(string)
	content, contentOK := object["content"].(string)
	if !idOK || !chapterOK || !versionOK || !statusOK || !contentOK ||
		!m03OneOfText(status, "draft", "revised", "reviewed", "finalized", "archived") ||
		importsource.CountDraftUnits(content) < 1 {
		return nil, errors.New("M03 snapshot source invalid")
	}
	return map[string]any{"id": float64(id), "chapterNumber": float64(chapter), "version": float64(version),
		"status": status, "content": content}, nil
}

func m03ParseConfirmationItem(value any, version int64) (map[string]any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("M03 snapshot item invalid")
	}
	category, categoryOK := m03JSNonemptyString(object["category"])
	severity, severityOK := m03JSNonemptyString(object["severity"])
	description, descriptionOK := m03JSNonemptyString(object["description"])
	decision, decisionOK := object["decision"].(string)
	origin, originOK := object["origin"].(string)
	if !categoryOK || !severityOK || !descriptionOK || !decisionOK || !originOK ||
		!m03OneOfText(decision, "apply", "ignore", "waive") || !m03OneOfText(origin, "ai", "author") {
		return nil, errors.New("M03 snapshot item invalid")
	}
	canonical := map[string]any{"category": category, "severity": severity,
		"description": description, "decision": decision, "origin": origin}
	for _, key := range []string{"quote", "stableFactKey", "goalId", "findingId"} {
		value, present := object[key]
		if !present {
			continue
		}
		var parsed string
		var valid bool
		if key == "quote" {
			parsed, valid = value.(string)
		} else {
			parsed, valid = m03JSNonemptyString(value)
		}
		if !valid {
			return nil, fmt.Errorf("M03 snapshot item %s invalid", key)
		}
		canonical[key] = parsed
	}
	if value, present := object["sourceChapter"]; present {
		chapter, ok := m03JSPositiveSafeInteger(value)
		if !ok {
			return nil, errors.New("M03 snapshot item source chapter invalid")
		}
		canonical["sourceChapter"] = float64(chapter)
	}
	_, hasFindingID := canonical["findingId"]
	if version == 1 && (hasFindingID || decision == "waive") ||
		decision == "waive" && (version != 2 || origin != "ai" || !hasFindingID) {
		return nil, errors.New("M03 snapshot item decision invalid")
	}
	return canonical, nil
}

func m03ValidateChapterGoalReview(value any) error {
	object, ok := value.(map[string]any)
	if !ok || !m03JSNumberEqual(object["version"], 1) {
		return errors.New("M03 snapshot goal review invalid")
	}
	if _, ok := m03JSPositiveSafeInteger(object["chapterNumber"]); !ok {
		return errors.New("M03 snapshot goal review chapter invalid")
	}
	coverage, ok := object["coverage"].(string)
	if !ok || !m03OneOfText(coverage, "complete", "unknown", "not_configured") {
		return errors.New("M03 snapshot goal review coverage invalid")
	}
	items, ok := object["items"].([]any)
	if !ok {
		return errors.New("M03 snapshot goal review items invalid")
	}
	ids := make(map[string]bool)
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			return errors.New("M03 snapshot goal review item invalid")
		}
		id, idOK := item["id"].(string)
		idOK = idOK && m03Nonempty(id)
		_, textOK := m03JSNonemptyString(item["text"])
		_, descriptionOK := m03JSNonemptyString(item["description"])
		status, statusOK := item["status"].(string)
		evidence, evidenceOK := item["evidence"].([]any)
		if !idOK || ids[id] || !textOK || !descriptionOK || !statusOK ||
			!m03OneOfText(status, "completed", "unmet", "unknown") || !evidenceOK ||
			status != "unknown" && len(evidence) == 0 {
			return errors.New("M03 snapshot goal review item invalid")
		}
		ids[id] = true
		for _, value := range evidence {
			entry, ok := value.(map[string]any)
			if !ok {
				return errors.New("M03 snapshot goal evidence invalid")
			}
			quote, quoteOK := entry["quote"].(string)
			quoteOK = quoteOK && m03Nonempty(quote)
			start, startOK := m03JSNonnegativeSafeInteger(entry["start"])
			if !quoteOK || !startOK || !m03JSNumberEqual(entry["end"], start+int64(len(utf16.Encode([]rune(quote))))) {
				return errors.New("M03 snapshot goal evidence invalid")
			}
		}
	}
	return nil
}

func m03JSNumberEqual(value any, expected int64) bool {
	number, ok := value.(float64)
	return ok && number == float64(expected)
}
func m03JSPositiveSafeInteger(value any) (int64, bool) {
	return m03JSInteger(value, 1)
}
func m03JSNonnegativeSafeInteger(value any) (int64, bool) {
	return m03JSInteger(value, 0)
}
func m03JSInteger(value any, minimum int64) (int64, bool) {
	number, ok := value.(float64)
	if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < float64(minimum) ||
		number > float64(m03MaxSafeInteger) || math.Trunc(number) != number {
		return 0, false
	}
	return int64(number), true
}
func m03JSNonemptyString(value any) (string, bool) {
	stringValue, ok := value.(string)
	if !ok {
		return "", false
	}
	trimmed := m03JSTrim(stringValue)
	return trimmed, trimmed != ""
}
func m03JSNonemptyValue(value any) bool {
	_, ok := m03JSNonemptyString(value)
	return ok
}
func m03JSTrim(value string) string { return strings.TrimFunc(value, m03JSWhitespace) }
func m03OneOfText(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

// The original uses isDeepStrictEqual between the frozen context snapshot and
// parser-normalized confirmation. JSON object key order is immaterial here.
func m03SameConfirmationSnapshot(frozen any, parsed *m03Confirmation) bool {
	return parsed != nil && reflect.DeepEqual(frozen, parsed.Snapshot)
}

// m03RevisionCompositionProof supplies the values already authenticated by
// proveEffect's common context/artifact checks. It is kept separate so the
// revision path cannot accidentally promote an unproved review cycle.
type m03RevisionCompositionProof struct {
	AttemptID, RunID, RootActionID, ProjectID string
	ContentHash, CurrentArtifactID            string
	CurrentArtifactRef, BindingFingerprint    json.RawMessage
	UsageReceipt                              json.RawMessage
}

// m03VerifyRevisionComposition ports the visibleComposition branch of
// proveEffect. It rejects uncertain Unicode/JSON representations rather than
// admitting a composition that cannot be reproduced byte for byte.
func m03VerifyRevisionComposition(ctx context.Context, tx *sql.Tx, proof m03RevisionCompositionProof) error {
	if !m03Hash(proof.ContentHash) || !m03Nonempty(proof.CurrentArtifactID) {
		return errors.New("M03 composition identity invalid")
	}
	var usage map[string]json.RawMessage
	if m03LoneSurrogateEscape(string(proof.UsageReceipt)) || json.Unmarshal(proof.UsageReceipt, &usage) != nil {
		return errors.New("M03 composition receipt invalid")
	}
	var effect, composition map[string]json.RawMessage
	if json.Unmarshal(usage["reviewRevisionEffect"], &effect) != nil ||
		json.Unmarshal(usage["visibleComposition"], &composition) != nil {
		return errors.New("M03 composition effect missing")
	}
	var storedHash, algorithm, compositionHash string
	if json.Unmarshal(effect["compositionHash"], &storedHash) != nil ||
		json.Unmarshal(composition["algorithm"], &algorithm) != nil ||
		json.Unmarshal(composition["textHash"], &compositionHash) != nil ||
		!m03Hash(storedHash) || storedHash != proof.ContentHash ||
		algorithm != "visible-append-v1" || compositionHash != storedHash {
		return errors.New("M03 composition hash or algorithm differs")
	}
	var artifactIDs []string
	var sources []json.RawMessage
	if json.Unmarshal(composition["artifactIds"], &artifactIDs) != nil ||
		json.Unmarshal(composition["sources"], &sources) != nil ||
		len(artifactIDs) < 1 || len(artifactIDs) > 32 || len(sources) != len(artifactIDs) ||
		artifactIDs[len(artifactIDs)-1] != proof.CurrentArtifactID {
		return errors.New("M03 composition sources invalid")
	}
	lastRef, err := m03CompactRaw(proof.CurrentArtifactRef)
	if err != nil {
		return err
	}
	lastSource, err := m03CompactRaw(sources[len(sources)-1])
	if err != nil || !bytes.Equal(lastRef, lastSource) {
		return errors.New("M03 composition final source differs")
	}
	seen := make(map[string]bool)
	previousOrdinal := int64(-1)
	var composed string
	for i, artifactID := range artifactIDs {
		if !m03Nonempty(artifactID) || seen[artifactID] {
			return errors.New("M03 composition duplicate artifact")
		}
		seen[artifactID] = true
		var attemptID, runID, status, artifactJSON, attemptJSON, receiptJSON string
		var revision, ordinal int64
		err := tx.QueryRowContext(ctx, `SELECT a.attempt_id,a.run_id,a.status,a.artifact_json,a.revision,
			t.rowid,t.attempt_json,t.usage_receipt_json FROM generation_artifacts a
			JOIN generation_attempts t ON t.attempt_id=a.attempt_id WHERE a.artifact_id=?`, artifactID).Scan(
			&attemptID, &runID, &status, &artifactJSON, &revision, &ordinal, &attemptJSON, &receiptJSON)
		if err != nil || runID != proof.RunID || status == "discarded" || ordinal <= previousOrdinal {
			return errors.New("M03 composition artifact row invalid")
		}
		if m03LoneSurrogateEscape(artifactJSON) || m03LoneSurrogateEscape(attemptJSON) ||
			m03LoneSurrogateEscape(receiptJSON) {
			return errors.New("M03 composition JSON contains isolated surrogate")
		}
		var visible, physical, candidateUsage map[string]json.RawMessage
		if json.Unmarshal([]byte(artifactJSON), &visible) != nil ||
			json.Unmarshal([]byte(attemptJSON), &physical) != nil ||
			json.Unmarshal([]byte(receiptJSON), &candidateUsage) != nil {
			return errors.New("M03 composition JSON invalid")
		}
		var physicalStatus, finishReason, text, textHash, visibleID, visibleAttemptID, visibleRootID, visibleProjectID, epoch string
		var visibleRevision int64
		var result, identity map[string]json.RawMessage
		if json.Unmarshal(physical["status"], &physicalStatus) != nil ||
			json.Unmarshal(candidateUsage["result"], &result) != nil ||
			json.Unmarshal(result["finishReason"], &finishReason) != nil ||
			json.Unmarshal(candidateUsage["artifactIdentity"], &identity) != nil ||
			json.Unmarshal(visible["text"], &text) != nil ||
			json.Unmarshal(visible["textHash"], &textHash) != nil ||
			json.Unmarshal(visible["artifactId"], &visibleID) != nil ||
			json.Unmarshal(visible["attemptId"], &visibleAttemptID) != nil ||
			json.Unmarshal(visible["rootActionId"], &visibleRootID) != nil ||
			json.Unmarshal(visible["projectId"], &visibleProjectID) != nil ||
			json.Unmarshal(visible["epoch"], &epoch) != nil ||
			json.Unmarshal(visible["revision"], &visibleRevision) != nil {
			return errors.New("M03 composition artifact shape invalid")
		}
		if !m03OneOfText(physicalStatus, "settled", "unknown") || !m03OneOfText(finishReason, "stop", "length") ||
			!m03Nonempty(text) || m03StringHash(text) != textHash || visibleID != artifactID ||
			visibleAttemptID != attemptID || visibleRootID != proof.RootActionID ||
			visibleProjectID != proof.ProjectID || !m03Nonempty(epoch) || visibleRevision != revision {
			return errors.New("M03 composition artifact identity invalid")
		}
		var identityID, identityEpoch string
		if json.Unmarshal(identity["artifactId"], &identityID) != nil ||
			json.Unmarshal(identity["epoch"], &identityEpoch) != nil ||
			identityID != artifactID || identityEpoch != epoch ||
			!m03SameRawJSON(visible["fingerprint"], proof.BindingFingerprint) ||
			!m03SameRawJSON(identity["fingerprint"], visible["fingerprint"]) {
			return errors.New("M03 composition fingerprint differs")
		}
		ref := make([]byte, 0, 160)
		ref = append(ref, '{')
		ref = append(ref, `"artifactId":`...)
		ref = appendJSString(ref, artifactID)
		ref = append(ref, `,"revision":`...)
		ref = append(ref, []byte(fmt.Sprint(revision))...)
		ref = append(ref, `,"textHash":`...)
		ref = appendJSString(ref, textHash)
		ref = append(ref, '}')
		if !m03SameRawJSON(sources[i], ref) {
			return errors.New("M03 composition source list differs")
		}
		var next string
		if i == 0 {
			next = m03JSTrim(text)
		} else {
			next, err = m03ComposeVisibleContinuation(composed, text)
			if err != nil {
				return err
			}
		}
		if !m03Nonempty(next) || i > 0 && m03VisibleLetterNumberCount(next) <= m03VisibleLetterNumberCount(composed) {
			return errors.New("M03 composition adds no visible text")
		}
		composed = next
		previousOrdinal = ordinal
	}
	if m03StringHash(composed) != storedHash {
		return errors.New("M03 composition rebuilt hash differs")
	}
	return nil
}

func m03CompactRaw(raw []byte) ([]byte, error) {
	if len(raw) == 0 || m03LoneSurrogateEscape(string(raw)) {
		return nil, errors.New("M03 JSON missing or contains isolated surrogate")
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, err
	}
	return compact.Bytes(), nil
}
func m03SameRawJSON(left, right []byte) bool {
	a, err := m03CompactRaw(left)
	if err != nil {
		return false
	}
	b, err := m03CompactRaw(right)
	return err == nil && bytes.Equal(a, b)
}

// JS string slicing is by UTF-16 code unit. A slice that bisects a surrogate
// is deliberately refused until an exact surrogate-preserving representation
// exists; a false negative here never admits corrupt author data.
func m03UTF16Window(value string, tail bool, limit int) (string, error) {
	units := 0
	for _, r := range value {
		if r > 0xffff {
			units += 2
		} else {
			units++
		}
	}
	if units <= limit {
		return value, nil
	}
	begin, end := 0, len(value)
	if tail {
		want := units - limit
		seen := 0
		for index, r := range value {
			if seen == want {
				begin = index
				break
			}
			step := 1
			if r > 0xffff {
				step = 2
			}
			if seen < want && seen+step > want {
				return "", errors.New("M03 UTF-16 slice bisects surrogate")
			}
			seen += step
		}
	} else {
		seen := 0
		for index, r := range value {
			if seen == limit {
				end = index
				break
			}
			step := 1
			if r > 0xffff {
				step = 2
			}
			if seen < limit && seen+step > limit {
				return "", errors.New("M03 UTF-16 slice bisects surrogate")
			}
			seen += step
		}
	}
	return value[begin:end], nil
}

func m03StripJSWhitespace(value string) string {
	return strings.Map(func(r rune) rune {
		if m03JSWhitespace(r) {
			return -1
		}
		return r
	}, value)
}
func m03VisibleLetterNumberCount(value string) int {
	count := 0
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			count++
		}
	}
	return count
}
func m03ComposeVisibleContinuation(existing, addition string) (string, error) {
	visibleExisting, visibleAddition := m03JSTrim(existing), m03JSTrim(addition)
	existingTail, err := m03UTF16Window(visibleExisting, true, 1600)
	if err != nil {
		return "", err
	}
	additionHead, err := m03UTF16Window(visibleAddition, false, 1600)
	if err != nil {
		return "", err
	}
	existingTail, additionHead = m03StripJSWhitespace(existingTail), m03StripJSWhitespace(additionHead)
	existingUnits, additionUnits := utf16.Encode([]rune(existingTail)), utf16.Encode([]rune(additionHead))
	maximum := len(existingUnits)
	if n := len(additionUnits); n < maximum {
		maximum = n
	}
	overlap := 0
	for length := maximum; length >= 48; length-- {
		if reflect.DeepEqual(existingUnits[len(existingUnits)-length:], additionUnits[:length]) {
			overlap = length
			break
		}
	}
	consumed := 0
	newVisible := visibleAddition
	if overlap > 0 {
		newVisible = ""
		for index, r := range visibleAddition {
			if !m03JSWhitespace(r) {
				consumed += len(utf16.Encode([]rune{r}))
			}
			if consumed > overlap {
				return "", errors.New("M03 visible overlap bisects surrogate")
			}
			if consumed >= overlap {
				newVisible = strings.TrimLeftFunc(visibleAddition[index+len(string(r)):], m03JSWhitespace)
				break
			}
		}
	}
	parts := make([]string, 0, 2)
	if visibleExisting != "" {
		parts = append(parts, visibleExisting)
	}
	if newVisible != "" {
		parts = append(parts, newVisible)
	}
	return m03JSTrim(strings.Join(parts, "\n\n")), nil
}

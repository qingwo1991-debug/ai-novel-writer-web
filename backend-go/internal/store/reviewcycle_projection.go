package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// These helpers reconstruct the original review-item mapping. They are kept
// behind the populated-cycle 501 gate until the global M03 receipt proof and
// merged-cycle recheck proof have both been ported.
var errReviewCycleSavedReport = errors.New("REVIEW_CYCLE_INPUT_INVALID")

type reviewCycleSavedItem struct {
	Index         int
	Category      string
	Severity      string
	Description   string
	Quote         *string
	GoalID        *string
	StableFactKey *string
}

type reviewCycleSavedGoal struct {
	Index       int
	ID          string
	Text        string
	Description string
	Status      string
	Evidence    []reviewCycleGoalEvidence
}

type reviewCycleGoalEvidence struct {
	Quote string
	Start int64
	End   int64
}

func rcObject(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok
}
func rcText(value any) (string, bool) {
	result, ok := value.(string)
	return result, ok && trimJS(result) != ""
}
func rcOptionalText(row map[string]any, key string) (*string, bool) {
	value, exists := row[key]
	if !exists {
		return nil, true
	}
	result, ok := rcText(value)
	if !ok {
		return nil, false
	}
	return &result, true
}
func rcKeys(row map[string]any, required []string, allowed []string) bool {
	if len(row) < len(required) {
		return false
	}
	for _, key := range required {
		if _, ok := row[key]; !ok {
			return false
		}
	}
	allow := make(map[string]bool, len(allowed))
	for _, key := range allowed {
		allow[key] = true
	}
	for key := range row {
		if !allow[key] {
			return false
		}
	}
	return true
}
func rcInteger(value any, minimum int64) (int64, bool) { return rrInteger(value, minimum) }

// encoding/json replaces an escaped lone UTF-16 surrogate with U+FFFD, while
// JSON.parse preserves the original code unit. Reject that representation
// before decoding so a finding ID can never be computed from changed text.
func rcLoneSurrogateEscape(body string) bool {
	inString := false
	decode := func(offset int) (uint16, bool) {
		if offset+6 > len(body) || body[offset] != '\\' || body[offset+1] != 'u' {
			return 0, false
		}
		var value uint16
		for index := offset + 2; index < offset+6; index++ {
			var digit byte
			switch c := body[index]; {
			case c >= '0' && c <= '9':
				digit = c - '0'
			case c >= 'a' && c <= 'f':
				digit = c - 'a' + 10
			case c >= 'A' && c <= 'F':
				digit = c - 'A' + 10
			default:
				return 0, false
			}
			value = value<<4 | uint16(digit)
		}
		return value, true
	}
	for index := 0; index < len(body); index++ {
		if !inString {
			if body[index] == '"' {
				inString = true
			}
			continue
		}
		if body[index] == '"' {
			inString = false
			continue
		}
		if body[index] != '\\' {
			continue
		}
		if index+1 >= len(body) {
			break
		}
		if body[index+1] != 'u' {
			index++
			continue
		}
		unit, ok := decode(index)
		if !ok {
			index++
			continue
		}
		if unit >= 0xd800 && unit <= 0xdbff {
			low, paired := decode(index + 6)
			if !paired || low < 0xdc00 || low > 0xdfff {
				return true
			}
			index += 11
			continue
		}
		if unit >= 0xdc00 && unit <= 0xdfff {
			return true
		}
		index += 5
	}
	return false
}

func rcParseSavedReport(body string) ([]reviewCycleSavedItem, map[string]reviewCycleSavedGoal, error) {
	if !utf8.ValidString(body) || rcLoneSurrogateEscape(body) {
		return nil, nil, errReviewCycleSavedReport
	}
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	var raw any
	if err := decoder.Decode(&raw); err != nil {
		return nil, nil, errReviewCycleSavedReport
	}
	var tail any
	if decoder.Decode(&tail) != io.EOF {
		return nil, nil, errReviewCycleSavedReport
	}
	report, ok := rcObject(raw)
	if !ok || !rcKeys(report, []string{"summary", "items", "goalReview"}, []string{"summary", "items", "goalReview"}) {
		return nil, nil, errReviewCycleSavedReport
	}
	if _, ok := rcText(report["summary"]); !ok {
		return nil, nil, errReviewCycleSavedReport
	}
	rawItems, ok := report["items"].([]any)
	if !ok || len(rawItems) == 0 {
		return nil, nil, errReviewCycleSavedReport
	}
	goalReview, ok := rcObject(report["goalReview"])
	if !ok || !rcKeys(goalReview, []string{"version", "chapterNumber", "coverage", "items"}, []string{"version", "chapterNumber", "coverage", "items"}) {
		return nil, nil, errReviewCycleSavedReport
	}
	version, valid := rcInteger(goalReview["version"], 1)
	if !valid || version != 1 {
		return nil, nil, errReviewCycleSavedReport
	}
	if _, valid := rcInteger(goalReview["chapterNumber"], 1); !valid {
		return nil, nil, errReviewCycleSavedReport
	}
	coverage, ok := goalReview["coverage"].(string)
	if !ok || (coverage != "complete" && coverage != "unknown" && coverage != "not_configured") {
		return nil, nil, errReviewCycleSavedReport
	}
	rawGoals, ok := goalReview["items"].([]any)
	if !ok {
		return nil, nil, errReviewCycleSavedReport
	}
	goals := make(map[string]reviewCycleSavedGoal, len(rawGoals))
	for goalIndex, candidate := range rawGoals {
		row, ok := rcObject(candidate)
		if !ok || !rcKeys(row, []string{"id", "text", "status", "description", "evidence"}, []string{"id", "text", "status", "description", "evidence"}) {
			return nil, nil, errReviewCycleSavedReport
		}
		id, okID := rcText(row["id"])
		text, okText := rcText(row["text"])
		description, okDescription := rcText(row["description"])
		status, okStatus := row["status"].(string)
		evidence, okEvidence := row["evidence"].([]any)
		if !okID || !okText || !okDescription || !okStatus || !okEvidence || (status != "completed" && status != "unmet" && status != "unknown") {
			return nil, nil, errReviewCycleSavedReport
		}
		if _, exists := goals[id]; exists {
			return nil, nil, errReviewCycleSavedReport
		}
		goal := reviewCycleSavedGoal{Index: goalIndex, ID: id, Text: text, Description: description, Status: status}
		for _, proof := range evidence {
			entry, ok := rcObject(proof)
			if !ok || !rcKeys(entry, []string{"quote", "start", "end"}, []string{"quote", "start", "end"}) {
				return nil, nil, errReviewCycleSavedReport
			}
			quote, valid := rcText(entry["quote"])
			start, validStart := rcInteger(entry["start"], 0)
			end, validEnd := rcInteger(entry["end"], 0)
			if !valid || !validStart || !validEnd || end != start+int64(len(utf16.Encode([]rune(quote)))) {
				return nil, nil, errReviewCycleSavedReport
			}
			goal.Evidence = append(goal.Evidence, reviewCycleGoalEvidence{quote, start, end})
		}
		if status != "unknown" && len(goal.Evidence) == 0 {
			return nil, nil, errReviewCycleSavedReport
		}
		goals[id] = goal
	}
	items := make([]reviewCycleSavedItem, 0, len(rawItems))
	for index, candidate := range rawItems {
		row, ok := rcObject(candidate)
		if !ok || !rcKeys(row, []string{"category", "severity", "description"}, []string{"category", "severity", "description", "quote", "goalId", "stableFactKey", "sourceChapter"}) {
			return nil, nil, errReviewCycleSavedReport
		}
		category, okCategory := rcText(row["category"])
		description, okDescription := rcText(row["description"])
		severity, okSeverity := row["severity"].(string)
		quote, okQuote := rcOptionalText(row, "quote")
		goalID, okGoal := rcOptionalText(row, "goalId")
		stable, okStable := rcOptionalText(row, "stableFactKey")
		if !okCategory || !okDescription || !okSeverity || !okQuote || !okGoal || !okStable || (severity != "error" && severity != "warning" && severity != "pass" && severity != "unknown") || (goalID != nil && stable != nil) {
			return nil, nil, errReviewCycleSavedReport
		}
		if goalID != nil {
			if _, exists := goals[*goalID]; !exists {
				return nil, nil, errReviewCycleSavedReport
			}
		}
		_, hasChapter := row["sourceChapter"]
		if stable != nil {
			chapter, valid := rcInteger(row["sourceChapter"], 1)
			if !valid || chapter < 1 || severity != "warning" || quote == nil {
				return nil, nil, errReviewCycleSavedReport
			}
		} else if hasChapter {
			return nil, nil, errReviewCycleSavedReport
		}
		items = append(items, reviewCycleSavedItem{index, category, severity, description, quote, goalID, stable})
	}
	for _, goal := range goals {
		var projection *reviewCycleSavedItem
		for index := range items {
			if items[index].GoalID != nil && *items[index].GoalID == goal.ID {
				if projection != nil {
					return nil, nil, errReviewCycleSavedReport
				}
				projection = &items[index]
			}
		}
		severity := "unknown"
		if goal.Status == "completed" {
			severity = "pass"
		} else if goal.Status == "unmet" {
			severity = "error"
		}
		if projection == nil || projection.Severity != severity {
			return nil, nil, errReviewCycleSavedReport
		}
		if len(goal.Evidence) == 0 {
			if projection.Quote != nil {
				return nil, nil, errReviewCycleSavedReport
			}
		} else {
			quotes := make([]string, len(goal.Evidence))
			for index, proof := range goal.Evidence {
				quotes[index] = proof.Quote
			}
			if projection.Quote == nil || *projection.Quote != strings.Join(quotes, "\n") {
				return nil, nil, errReviewCycleSavedReport
			}
		}
	}
	return items, goals, nil
}

type reviewCycleSource struct {
	ProjectID, Epoch, ID string
	Revision             int64
	ContentHash          string
}

func rcFindingID(source reviewCycleSource, sourceText string, item reviewCycleSavedItem, goal *reviewCycleSavedGoal) (string, error) {
	if !utf8.ValidString(sourceText) || !utf8.ValidString(source.ProjectID) || !utf8.ValidString(source.Epoch) ||
		!utf8.ValidString(source.ID) || !utf8.ValidString(source.ContentHash) || !utf8.ValidString(item.Category) {
		return "", errReviewCycleSavedReport
	}
	var quote *string
	targetID := any(nil)
	kind := "literary"
	logical := "free:" + phNumberString(int64(item.Index))
	if goal != nil {
		if len(goal.Evidence) == 1 {
			quote = &goal.Evidence[0].Quote
		}
		targetID = goal.ID
		kind = "objective"
		logical = "goal:" + goal.ID
	} else {
		quote = item.Quote
		if item.StableFactKey != nil {
			targetID = *item.StableFactKey
			kind = "objective"
			logical = "preflight:" + *item.StableFactKey
		}
	}
	start, end := any(nil), any(nil)
	spanUnit, excerptHash, occurrence := any(nil), any(nil), any(nil)
	if quote != nil && targetID != nil {
		first := strings.Index(sourceText, *quote)
		if first >= 0 && strings.Index(sourceText[first+1:], *quote) < 0 {
			start = int64(len(utf16.Encode([]rune(sourceText[:first]))))
			end = start.(int64) + int64(len(utf16.Encode([]rune(*quote))))
			spanUnit = "utf16-code-unit"
			digest := sha256.Sum256([]byte(*quote))
			excerptHash = hex.EncodeToString(digest[:])
			occurrence = int64(1)
		} else {
			targetID = nil
		}
	} else {
		targetID = nil
	}
	identity := plotObject{{"sourceProjectId", source.ProjectID}, {"sourceEpoch", source.Epoch}, {"sourceId", source.ID}, {"sourceRevision", source.Revision}, {"sourceContentHash", source.ContentHash}, {"spanStart", start}, {"spanEnd", end}, {"spanUnit", spanUnit}, {"excerptHash", excerptHash}, {"occurrence", occurrence}, {"category", item.Category}, {"targetId", targetID}, {"kind", kind}, {"logicalIdentity", logical}}
	encoded, err := plotJSON(identity)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(encoded))
	return "finding:" + hex.EncodeToString(digest[:]), nil
}

// rcItemIndex mirrors the first-ID-wins Map in the TypeScript read projection.
// Persisted rows must independently pass the M03 verifier before this map is
// applied to a public DTO.
func rcItemIndex(reportBody, sourceText string, source reviewCycleSource) (map[string]int, error) {
	items, goals, err := rcParseSavedReport(reportBody)
	if err != nil {
		return nil, err
	}
	indices := make(map[string]int)
	for _, item := range items {
		if item.Severity == "pass" {
			continue
		}
		var goal *reviewCycleSavedGoal
		if item.GoalID != nil {
			candidate := goals[*item.GoalID]
			if candidate.Status == "completed" {
				continue
			}
			goal = &candidate
		}
		id, err := rcFindingID(source, sourceText, item, goal)
		if err != nil {
			return nil, err
		}
		if _, exists := indices[id]; !exists {
			indices[id] = item.Index
		}
	}
	return indices, nil
}

type reviewCycleProjectionRow struct {
	FindingID string
	Category  string
	Kind      string
	Status    string
	TargetID  *string
}

// rcProjectionDTO preserves the nullable and optional shape of getByReviewId.
// The caller must first prove the complete M03 cycle and any merge disposition.
func rcProjectionDTO(cycleID string, reviewID int64, revisionStatus string, mergedHash *string,
	recheckCount int64, disposition *string, rows []reviewCycleProjectionRow, itemIndex map[string]int) map[string]any {
	sort.Slice(rows, func(i, j int) bool { return rows[i].FindingID < rows[j].FindingID })
	findings := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		var index any
		if value, exists := itemIndex[row.FindingID]; exists {
			index = value
		}
		finding := map[string]any{"findingId": row.FindingID, "reviewItemIndex": index, "category": row.Category, "kind": row.Kind, "status": row.Status}
		if row.TargetID != nil && *row.TargetID != "" {
			finding["targetId"] = *row.TargetID
		}
		findings = append(findings, finding)
	}
	projection := map[string]any{"cycleId": cycleID, "reviewId": reviewID, "revisionStatus": revisionStatus, "recheckCount": recheckCount, "findings": findings}
	if mergedHash != nil && *mergedHash != "" {
		projection["mergedHash"] = *mergedHash
	}
	if disposition != nil {
		projection["recheckDisposition"] = *disposition
	}
	return projection
}

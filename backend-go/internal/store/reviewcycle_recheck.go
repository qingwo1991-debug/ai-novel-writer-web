package store

import (
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// This file ports the deterministic portion of TypeScript planRecheck. The
// caller must still prove M03 receipts, the source/merge snapshots, and any
// human confirmation before these helpers may affect a public read.
var errReviewCycleRecheckEvidence = errors.New("REVIEW_CYCLE_RECHECK_EVIDENCE_INVALID")

type rcRecheckFinding struct {
	FindingID  string
	TargetID   string
	Category   string
	Kind       string
	Problem    string
	Expected   string
	SpanStart  int64
	SpanEnd    int64
	Occurrence int64
	HasSpan    bool
	MustShow   bool
}

type rcRecheckRow struct {
	FindingID  string
	TargetID   *string
	Category   string
	Kind       string
	Problem    string
	Expected   *string
	Status     string
	SpanStart  *int64
	SpanEnd    *int64
	SpanUnit   *string
	Occurrence *int64
}

type rcConfirmationChoice struct {
	GoalID    string
	FindingID string
	Origin    string
	Decision  string
}

var rcMustShowGoalID = regexp.MustCompile(`^ch[0-9]+:mustShow:[0-9]+$`)

func rcObjectiveText(value string) string {
	var out strings.Builder
	for _, point := range value {
		if unicode.IsLetter(point) || unicode.IsNumber(point) {
			out.WriteRune(point)
		}
	}
	return out.String()
}

func rcVisibleRevisionText(value string) string {
	var out strings.Builder
	for _, point := range value {
		if unicode.Is(unicode.White_Space, point) || unicode.Is(unicode.Cf, point) {
			continue
		}
		out.WriteRune(point)
	}
	return out.String()
}

// A span splitting an astral character would become a lone surrogate in JS.
// Go strings cannot represent that value, so it remains ambiguous here.
func rcUTF16Slice(value string, start, end int64) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	units := utf16.Encode([]rune(value))
	if start < 0 || end <= start || end > int64(len(units)) {
		return "", false
	}
	for _, boundary := range []int64{start, end} {
		if boundary > 0 && boundary < int64(len(units)) && units[boundary-1] >= 0xd800 && units[boundary-1] <= 0xdbff && units[boundary] >= 0xdc00 && units[boundary] <= 0xdfff {
			return "", false
		}
	}
	return string(utf16.Decode(units[start:end])), true
}

// rcClassifyEvidenceChange returns TypeScript's changed/unchanged/ambiguous
// admission for one finding. It does not infer semantic resolution.
func rcClassifyEvidenceChange(source, merged string, finding rcRecheckFinding) string {
	if !utf8.ValidString(source) || !utf8.ValidString(merged) {
		return "ambiguous"
	}
	if finding.MustShow {
		if rcObjectiveText(source) == rcObjectiveText(merged) {
			return "unchanged"
		}
		return "changed"
	}
	if !finding.HasSpan || finding.Occurrence < 1 || finding.Occurrence > 9007199254740991 {
		return "ambiguous"
	}
	excerpt, valid := rcUTF16Slice(source, finding.SpanStart, finding.SpanEnd)
	if !valid || excerpt == "" || int64(strings.Count(source, excerpt)) < finding.Occurrence {
		return "ambiguous"
	}
	if rcVisibleRevisionText(source) == rcVisibleRevisionText(merged) {
		return "unchanged"
	}
	if finding.Kind == "objective" {
		objectiveExcerpt := rcObjectiveText(excerpt)
		if objectiveExcerpt != "" && strings.Contains(rcObjectiveText(merged), objectiveExcerpt) {
			return "unchanged"
		}
		return "changed"
	}
	if strings.Contains(merged, excerpt) {
		return "unchanged"
	}
	return "changed"
}

// rcUnshownMustShow reconstructs only the narrow unknown/evidence-empty goal
// that the author explicitly chose to apply in a validated v2 snapshot.
func rcUnshownMustShow(reportBody string, choices []rcConfirmationChoice, rows []rcRecheckRow) ([]rcRecheckFinding, error) {
	items, goals, err := rcParseSavedReport(reportBody)
	if err != nil {
		return nil, err
	}
	findings := []rcRecheckFinding{}
	orderedGoals := make([]reviewCycleSavedGoal, 0, len(goals))
	for _, goal := range goals {
		orderedGoals = append(orderedGoals, goal)
	}
	sort.Slice(orderedGoals, func(i, j int) bool { return orderedGoals[i].Index < orderedGoals[j].Index })
	for _, goal := range orderedGoals {
		if !rcMustShowGoalID.MatchString(goal.ID) || goal.Status != "unknown" || len(goal.Evidence) != 0 {
			continue
		}
		var projection *reviewCycleSavedItem
		projectionCount := 0
		for index := range items {
			if items[index].GoalID != nil && *items[index].GoalID == goal.ID {
				projection = &items[index]
				projectionCount++
			}
		}
		var choice *rcConfirmationChoice
		choiceCount := 0
		for index := range choices {
			candidate := &choices[index]
			if candidate.GoalID == goal.ID && candidate.Origin == "ai" && candidate.Decision == "apply" && candidate.FindingID != "" {
				choice = candidate
				choiceCount++
			}
		}
		if projectionCount != 1 || choiceCount != 1 {
			continue
		}
		expected := goal.Text + "\n" + goal.Description
		matches := 0
		for _, row := range rows {
			if row.FindingID == choice.FindingID && row.Kind == "objective" && row.Status == "unverified" && row.TargetID == nil && row.Expected != nil && *row.Expected == expected && row.Problem == projection.Description {
				matches++
			}
		}
		if matches == 1 {
			findings = append(findings, rcRecheckFinding{FindingID: choice.FindingID, TargetID: goal.ID, Category: projection.Category, Kind: "objective", Problem: projection.Description, Expected: expected, MustShow: true})
		}
	}
	return findings, nil
}

// rcPlanRecheckDisposition assumes the caller has proved the complete cycle,
// merge body, current merged revision, and confirmation snapshot. Invalid
// persisted state fails closed; absence of changed evidence is not an error.
func rcPlanRecheckDisposition(source, merged, reportBody string, count int64, attemptID *string,
	rows []rcRecheckRow, choices []rcConfirmationChoice) (string, error) {
	if !utf8.ValidString(source) || !utf8.ValidString(merged) {
		return "", errReviewCycleRecheckEvidence
	}
	if count == 1 {
		if attemptID == nil || *attemptID == "" {
			return "", errReviewCycleRecheckEvidence
		}
		return "completed", nil
	}
	if count != 0 || attemptID != nil {
		return "", errReviewCycleRecheckEvidence
	}
	unshown, err := rcUnshownMustShow(reportBody, choices, rows)
	if err != nil {
		return "", err
	}
	for _, row := range rows {
		if row.Status == "unverified" || row.Status == "resolved" || row.Status == "author-waived" || row.TargetID == nil || row.SpanStart == nil || row.SpanEnd == nil || row.SpanUnit == nil || *row.SpanUnit != "utf16-code-unit" || row.Occurrence == nil {
			continue
		}
		finding := rcRecheckFinding{FindingID: row.FindingID, TargetID: *row.TargetID, Category: row.Category, Kind: row.Kind, Problem: row.Problem, HasSpan: true, SpanStart: *row.SpanStart, SpanEnd: *row.SpanEnd, Occurrence: *row.Occurrence}
		if rcClassifyEvidenceChange(source, merged, finding) == "changed" {
			return "required", nil
		}
	}
	for _, finding := range unshown {
		if rcClassifyEvidenceChange(source, merged, finding) == "changed" {
			return "required", nil
		}
	}
	return "not-required", nil
}

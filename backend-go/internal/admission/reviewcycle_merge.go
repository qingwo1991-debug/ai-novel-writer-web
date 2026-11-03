package admission

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// This file implements the deterministic merge/recheck evidence portion of
// verifyM03ReviewCycle. Callers must prove the merged snapshot, recheck attempt,
// saved review report and effect before using its result for admission.
type m03RecheckFinding struct {
	FindingID, TargetID, Category, Kind, Problem, Expected, Status string
	SpanStart, SpanEnd, Occurrence                                 int64
	HasSpan, MustShow                                              bool
}

type m03RecheckReportItem struct {
	// CanonicalJSON is JSON.stringify(report.items[index]), with source key order.
	// The report builder supplies it after proving the full saved report body.
	CanonicalJSON string
	Resolved      *bool
}

type m03RecheckReceiptExpected struct {
	CycleID, MergedHash, FindingSetHash string
	ComparisonVersion                   int64
	Eligible                            []m03RecheckFinding
	ReportItems                         []m03RecheckReportItem
}

type m03RecheckEvidence struct {
	TargetID, EvidenceHash string
	Resolved               bool
}

var errM03Recheck = errors.New("M03 recheck evidence differs")

func m03ObjectiveText(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			out.WriteRune(r)
		}
	}
	return out.String()
}

func m03VisibleText(value string) string {
	var out strings.Builder
	for _, r := range value {
		if unicode.Is(unicode.White_Space, r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		out.WriteRune(r)
	}
	return out.String()
}

func m03UTF16Slice(value string, start, end int64) (string, bool) {
	if !utf8.ValidString(value) {
		return "", false
	}
	units := utf16.Encode([]rune(value))
	if start < 0 || end <= start || end > int64(len(units)) {
		return "", false
	}
	for _, boundary := range []int64{start, end} {
		if boundary > 0 && boundary < int64(len(units)) &&
			units[boundary-1] >= 0xd800 && units[boundary-1] <= 0xdbff &&
			units[boundary] >= 0xdc00 && units[boundary] <= 0xdfff {
			// JS can form an isolated surrogate here; fail closed in Go.
			return "", false
		}
	}
	return string(utf16.Decode(units[start:end])), true
}

func m03CountOccurrences(text, excerpt string) int64 {
	if excerpt == "" {
		return 0
	}
	var count int64
	for offset := 0; offset <= len(text); {
		found := strings.Index(text[offset:], excerpt)
		if found < 0 {
			break
		}
		count++
		offset += found + len(excerpt)
	}
	return count
}

// m03ClassifyEvidenceChange matches the saved TypeScript classifier. It never
// interprets a model's semantic resolution; ambiguous evidence is not eligible.
func m03ClassifyEvidenceChange(source, merged string, finding m03RecheckFinding) string {
	if !utf8.ValidString(source) || !utf8.ValidString(merged) {
		return "ambiguous"
	}
	if finding.MustShow {
		if m03ObjectiveText(source) == m03ObjectiveText(merged) {
			return "unchanged"
		}
		return "changed"
	}
	if !finding.HasSpan || !m03SafeInteger(finding.Occurrence, 1) {
		return "ambiguous"
	}
	excerpt, ok := m03UTF16Slice(source, finding.SpanStart, finding.SpanEnd)
	if !ok || excerpt == "" || m03CountOccurrences(source, excerpt) < finding.Occurrence {
		return "ambiguous"
	}
	if m03VisibleText(source) == m03VisibleText(merged) {
		return "unchanged"
	}
	if finding.Kind == "objective" {
		objectiveExcerpt := m03ObjectiveText(excerpt)
		if objectiveExcerpt != "" && strings.Contains(m03ObjectiveText(merged), objectiveExcerpt) {
			return "unchanged"
		}
		return "changed"
	}
	if strings.Contains(merged, excerpt) {
		return "unchanged"
	}
	return "changed"
}

// m03EligibleRecheck preserves the original finding-id order, followed by
// unshown mustShow goals in report-goal order. The caller supplies the latter
// only after validating the author confirmation and original goal projection.
func m03EligibleRecheck(source, merged string, rows, unshown []m03RecheckFinding) ([]m03RecheckFinding, error) {
	if !utf8.ValidString(source) || !utf8.ValidString(merged) {
		return nil, errM03Recheck
	}
	var eligible []m03RecheckFinding
	seen := map[string]bool{}
	appendChanged := func(f m03RecheckFinding) error {
		if !m03Nonempty(f.FindingID) || !m03Nonempty(f.TargetID) || seen[f.FindingID] {
			return errM03Recheck
		}
		if m03ClassifyEvidenceChange(source, merged, f) == "changed" {
			eligible = append(eligible, f)
			seen[f.FindingID] = true
		}
		return nil
	}
	for _, f := range rows {
		if f.Status == "unverified" || f.Status == "author-waived" || !f.HasSpan {
			continue
		}
		if err := appendChanged(f); err != nil {
			return nil, err
		}
	}
	for _, f := range unshown {
		if !f.MustShow {
			return nil, errM03Recheck
		}
		if err := appendChanged(f); err != nil {
			return nil, err
		}
	}
	return eligible, nil
}

func m03JSONSafeInteger(value any, minimum int64) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	f, err := number.Float64()
	if err != nil || math.IsInf(f, 0) || math.IsNaN(f) || math.Trunc(f) != f || f < float64(minimum) || f > m03MaxSafeInteger {
		return 0, false
	}
	return int64(f), true
}

func m03RecheckReceiptProof(rawUsage string, expected m03RecheckReceiptExpected) (int64, map[string]m03RecheckEvidence, error) {
	if m03LoneSurrogateEscape(rawUsage) || !m03Hash(expected.MergedHash) || !m03Hash(expected.FindingSetHash) ||
		!m03SafeInteger(expected.ComparisonVersion, 1) {
		return 0, nil, errM03Recheck
	}
	decoder := json.NewDecoder(strings.NewReader(rawUsage))
	decoder.UseNumber()
	var usage map[string]any
	if err := decoder.Decode(&usage); err != nil || usage == nil {
		return 0, nil, errM03Recheck
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return 0, nil, errM03Recheck
	}
	receipt, ok := usage["reviewCycleRecheck"].(map[string]any)
	if !ok {
		return 0, nil, errM03Recheck
	}
	version, valid := m03JSONSafeInteger(receipt["version"], 1)
	cycleID, cycleOK := receipt["cycleId"].(string)
	mergedHash, mergedOK := receipt["mergedHash"].(string)
	findingSetHash, findingSetOK := receipt["findingSetHash"].(string)
	if !valid || version > 2 || !cycleOK || cycleID != expected.CycleID ||
		!mergedOK || mergedHash != expected.MergedHash || !findingSetOK || findingSetHash != expected.FindingSetHash {
		return 0, nil, errM03Recheck
	}
	comparison, valid := m03JSONSafeInteger(receipt["comparisonVersion"], 1)
	if !valid || comparison != expected.ComparisonVersion {
		return 0, nil, errM03Recheck
	}
	items, ok := receipt["findings"].([]any)
	if !ok {
		return 0, nil, errM03Recheck
	}
	targets := make(map[string]string, len(expected.Eligible))
	for _, finding := range expected.Eligible {
		if _, exists := targets[finding.FindingID]; exists {
			return 0, nil, errM03Recheck
		}
		targets[finding.FindingID] = finding.TargetID
	}
	evidence := make(map[string]m03RecheckEvidence, len(items))
	usedIndices := map[int64]bool{}
	for _, item := range items {
		row, ok := item.(map[string]any)
		if !ok {
			return 0, nil, errM03Recheck
		}
		findingID, okID := row["findingId"].(string)
		targetID, okTarget := row["targetId"].(string)
		index, okIndex := m03JSONSafeInteger(row["reviewItemIndex"], 0)
		resolved, okResolved := row["resolved"].(bool)
		hashValue, okHash := row["evidenceHash"].(string)
		if !okID || !m03Nonempty(findingID) || !okTarget || !m03Nonempty(targetID) || !okIndex ||
			index >= int64(len(expected.ReportItems)) || !okResolved || version == 2 && resolved ||
			!okHash || !m03Hash(hashValue) || targets[findingID] != targetID || usedIndices[index] {
			return 0, nil, errM03Recheck
		}
		reportItem := expected.ReportItems[index]
		var savedItem map[string]json.RawMessage
		if !json.Valid([]byte(reportItem.CanonicalJSON)) || json.Unmarshal([]byte(reportItem.CanonicalJSON), &savedItem) != nil ||
			savedItem == nil || reportItem.Resolved == nil || *reportItem.Resolved != resolved {
			return 0, nil, errM03Recheck
		}
		var savedResolved bool
		if json.Unmarshal(savedItem["resolved"], &savedResolved) != nil || savedResolved != resolved {
			return 0, nil, errM03Recheck
		}
		encoded := []byte(`{"findingId":`)
		encoded = appendJSString(encoded, findingID)
		encoded = append(encoded, `,"targetId":`...)
		encoded = appendJSString(encoded, targetID)
		encoded = append(encoded, `,"reviewItemIndex":`...)
		encoded = strconv.AppendInt(encoded, index, 10)
		encoded = append(encoded, `,"item":`...)
		encoded = append(encoded, reportItem.CanonicalJSON...)
		encoded = append(encoded, `,"resolved":`...)
		if resolved {
			encoded = append(encoded, "true"...)
		} else {
			encoded = append(encoded, "false"...)
		}
		encoded = append(encoded, '}')
		digest := sha256.Sum256(encoded)
		if hashValue != hex.EncodeToString(digest[:]) {
			return 0, nil, errM03Recheck
		}
		usedIndices[index] = true
		evidence[findingID] = m03RecheckEvidence{TargetID: targetID, EvidenceHash: hashValue, Resolved: resolved}
	}
	if len(evidence) != len(items) {
		return 0, nil, errM03Recheck
	}
	return version, evidence, nil
}

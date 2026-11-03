// Package reviewbuilder assembles the original saved review report from
// frozen inputs. This package does not authorize an M03 cycle or publication.
package reviewbuilder

import (
	"bytes"
	"encoding/json"
	"errors"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewconsistency"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewgoals"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/reviewreport"
)

var ErrInvalidInput = errors.New("REVIEW_BUILDER_INVALID_INPUT")

type Input struct {
	Content           string
	SourceContent     string
	FrozenGoals       reviewgoals.FrozenGoals
	WritingLanguage   string
	UILocale          string
	PreflightFindings []reviewconsistency.Finding
	ReportVersion     int // zero selects the TypeScript default, version 2
}

func jsString(dst []byte, value string) []byte {
	const digits = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(value); i++ {
		switch c := value[i]; c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', digits[c>>4], digits[c&15])
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}

// Build returns compact JSON with the exact property insertion order of
// buildReviewGenerationReport and JSON.stringify for well-formed Unicode.
func Build(input Input) ([]byte, error) {
	if !utf8.ValidString(input.Content) || !utf8.ValidString(input.SourceContent) ||
		input.FrozenGoals.ChapterNumber < 0 || input.FrozenGoals.ChapterNumber > 1<<53-1 ||
		(input.UILocale != "zh-CN" && input.UILocale != "en-US") ||
		(input.WritingLanguage != "zh-CN" && input.WritingLanguage != "en-US") {
		return nil, ErrInvalidInput
	}
	if input.FrozenGoals.Coverage != "complete" && input.FrozenGoals.Coverage != "unknown" && input.FrozenGoals.Coverage != "not_configured" {
		return nil, ErrInvalidInput
	}
	for _, goal := range input.FrozenGoals.Items {
		if !utf8.ValidString(goal.ID) || !utf8.ValidString(goal.Text) {
			return nil, ErrInvalidInput
		}
	}
	version := input.ReportVersion
	if version == 0 {
		version = 2
	}
	parsed, err := reviewreport.Parse(input.Content, version)
	if err != nil {
		return nil, err
	}
	var raw any
	if len(parsed.GoalReviews) > 0 {
		decoder := json.NewDecoder(bytes.NewReader(parsed.GoalReviews))
		decoder.UseNumber()
		if err := decoder.Decode(&raw); err != nil {
			return nil, ErrInvalidInput
		}
	}
	goalReview := reviewgoals.Normalize(raw, input.FrozenGoals, input.SourceContent, input.WritingLanguage)
	projected := reviewgoals.Project(goalReview, input.WritingLanguage)
	summary := parsed.Summary
	unknown := false
	for _, item := range projected {
		if item.Severity == "unknown" {
			unknown = true
			break
		}
	}
	if unknown {
		if input.UILocale == "en-US" {
			summary = "The review contains unresolved items and is not an overall pass."
		} else {
			summary = "审稿包含待核实项目，不能视为全部通过。"
		}
	} else {
		for _, item := range goalReview.Items {
			if item.Status == "unmet" {
				if input.UILocale == "en-US" {
					summary = "Some chapter goals are unmet; check their evidence."
				} else {
					summary = "本章存在尚未完成的目标，请核对逐项证据。"
				}
				break
			}
		}
	}
	dst := []byte(`{"summary":`)
	dst = jsString(dst, summary)
	dst = append(dst, []byte(`,"items":[`)...)
	for index, item := range parsed.Items {
		if index > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, []byte(`{"category":`)...)
		dst = jsString(dst, item.Category)
		dst = append(dst, []byte(`,"severity":`)...)
		dst = jsString(dst, item.Severity)
		dst = append(dst, []byte(`,"description":`)...)
		dst = jsString(dst, item.Description)
		if item.Quote != nil {
			dst = append(dst, []byte(`,"quote":`)...)
			dst = jsString(dst, *item.Quote)
		}
		dst = append(dst, '}')
	}
	for index, item := range projected {
		if len(parsed.Items) > 0 || index > 0 {
			dst = append(dst, ',')
		}
		projection := reviewgoals.ProjectionJSON([]reviewgoals.PresentationItem{item})
		dst = append(dst, projection[1:len(projection)-1]...)
	}
	dst = append(dst, []byte(`],"goalReview":`)...)
	dst = append(dst, reviewgoals.ReviewJSON(goalReview)...)
	dst = append(dst, '}')
	review, err := reviewconsistency.DecodeReview(dst)
	if err != nil {
		return nil, err
	}
	merged, err := reviewconsistency.Merge(review, input.PreflightFindings, input.UILocale)
	if err != nil {
		return nil, err
	}
	return merged.MarshalJSJSON()
}

// Pretty renders the stored report body produced by JSON.stringify(report,
// null, 2). The verifier hashes the bytes of this representation.
func Pretty(compact []byte) ([]byte, error) {
	if !json.Valid(compact) {
		return nil, ErrInvalidInput
	}
	var result bytes.Buffer
	if err := json.Indent(&result, compact, "", "  "); err != nil {
		return nil, err
	}
	return result.Bytes(), nil
}

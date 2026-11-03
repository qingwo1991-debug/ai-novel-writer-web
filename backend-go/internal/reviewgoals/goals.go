// Package reviewgoals reproduces the mechanical chapter-goal evidence contract.
// It does not judge whether a model's description is true.
package reviewgoals

import (
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

type FrozenGoal struct {
	ID   string `json:"id"`
	Text string `json:"text"`
}

type FrozenGoals struct {
	ChapterNumber int64        `json:"chapterNumber"`
	Coverage      string       `json:"coverage"`
	Items         []FrozenGoal `json:"items"`
}

type Evidence struct {
	Quote string `json:"quote"`
	Start int    `json:"start"`
	End   int    `json:"end"`
}

type Item struct {
	ID          string     `json:"id"`
	Text        string     `json:"text"`
	Status      string     `json:"status"`
	Description string     `json:"description"`
	Evidence    []Evidence `json:"evidence"`
}

type Review struct {
	Version       int    `json:"version"`
	ChapterNumber int64  `json:"chapterNumber"`
	Coverage      string `json:"coverage"`
	Items         []Item `json:"items"`
}

type PresentationItem struct {
	Category    string  `json:"category"`
	GoalID      *string `json:"goalId,omitempty"`
	Severity    string  `json:"severity"`
	Description string  `json:"description"`
	Quote       *string `json:"quote,omitempty"`
}

func jsWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', '\u00a0', '\u1680', '\u2028', '\u2029', '\u202f', '\u205f', '\u3000', '\ufeff':
		return true
	}
	return r >= '\u2000' && r <= '\u200a'
}

func nonempty(s string) bool { return strings.TrimFunc(s, jsWhitespace) != "" }

func utf16Length(s string) int { return len(utf16.Encode([]rune(s))) }

func record(value any) (map[string]any, bool) {
	object, ok := value.(map[string]any)
	return object, ok && object != nil
}

func stringField(object map[string]any, key string) (string, bool) {
	value, ok := object[key].(string)
	return value, ok && utf8.ValidString(value)
}

func locate(quote, draft string) (Evidence, bool) {
	if !nonempty(quote) || !utf8.ValidString(quote) || !utf8.ValidString(draft) {
		return Evidence{}, false
	}
	candidate := quote
	start := strings.Index(draft, candidate)
	if start < 0 {
		wrapped := (strings.HasPrefix(quote, "“") && strings.HasSuffix(quote, "”")) ||
			(strings.HasPrefix(quote, `"`) && strings.HasSuffix(quote, `"`))
		if !wrapped {
			return Evidence{}, false
		}
		_, left := utf8.DecodeRuneInString(quote)
		_, right := utf8.DecodeLastRuneInString(quote)
		candidate = quote[left : len(quote)-right]
		if !nonempty(candidate) {
			return Evidence{}, false
		}
		start = strings.Index(draft, candidate)
		// JavaScript checks from start+1, including overlapping occurrences.
		if start < 0 || strings.Index(draft[start+1:], candidate) >= 0 {
			return Evidence{}, false
		}
	}
	end := start + len(candidate)
	return Evidence{Quote: draft[start:end], Start: utf16Length(draft[:start]), End: utf16Length(draft[:end])}, true
}

// Normalize follows the TypeScript mechanical checks for well-formed UTF-8
// model JSON. Bad model judgments become unknown; they do not prove a goal.
// Callers must reject isolated JSON surrogate escapes before decoding to Go.
func Normalize(raw any, goals FrozenGoals, draft, language string) Review {
	candidates, isArray := raw.([]any)
	if !isArray {
		candidates = nil
	}
	invalidCoverage := !isArray && len(goals.Items) > 0
	for _, value := range candidates {
		object, ok := record(value)
		if !ok {
			invalidCoverage = true
			continue
		}
		id, ok := stringField(object, "id")
		found := false
		if ok {
			for _, goal := range goals.Items {
				if goal.ID == id {
					found = true
					break
				}
			}
		}
		if !found {
			invalidCoverage = true
		}
	}
	items := make([]Item, 0, len(goals.Items))
	for _, goal := range goals.Items {
		matches := make([]map[string]any, 0, 1)
		for _, value := range candidates {
			object, ok := record(value)
			if !ok {
				continue
			}
			if id, ok := stringField(object, "id"); ok && id == goal.ID {
				matches = append(matches, object)
			}
		}
		var candidate map[string]any
		if len(matches) == 1 {
			candidate = matches[0]
		}
		status, statusOK := stringField(candidate, "status")
		description, descriptionOK := stringField(candidate, "description")
		rawEvidence, evidenceOK := candidate["evidence"].([]any)
		valid := candidate != nil && statusOK && (status == "completed" || status == "unmet" || status == "unknown") &&
			descriptionOK && nonempty(description) && evidenceOK
		if valid {
			for key := range candidate {
				if key != "id" && key != "status" && key != "description" && key != "evidence" {
					valid = false
				}
			}
		}
		evidence := make([]Evidence, 0)
		if valid {
			for _, value := range rawEvidence {
				entry, ok := record(value)
				if !ok || len(entry) != 1 {
					valid = false
					continue
				}
				quote, ok := stringField(entry, "quote")
				located, found := locate(quote, draft)
				if !ok || !found {
					valid = false
					continue
				}
				evidence = append(evidence, located)
			}
			if status != "unknown" && len(evidence) == 0 {
				valid = false
			}
		}
		if !valid {
			invalidCoverage = true
			status = "unknown"
			evidence = []Evidence{}
			if language == "en-US" {
				description = "This goal lacks a valid judgment or draft evidence; verify it manually."
			} else {
				description = "该目标缺少有效的逐项判断或正文证据，请人工核实。"
			}
		}
		items = append(items, Item{ID: goal.ID, Text: goal.Text, Status: status, Description: description, Evidence: evidence})
	}
	coverage := goals.Coverage
	if invalidCoverage {
		coverage = "unknown"
	}
	return Review{Version: 1, ChapterNumber: goals.ChapterNumber, Coverage: coverage, Items: items}
}

// Project creates the deterministic goal items appended to a saved report.
func Project(review Review, language string) []PresentationItem {
	category := "本章目标"
	if language == "en-US" {
		category = "Chapter goal"
	}
	items := make([]PresentationItem, 0, len(review.Items)+1)
	for _, item := range review.Items {
		severity := "unknown"
		switch item.Status {
		case "completed":
			severity = "pass"
		case "unmet":
			severity = "error"
		}
		id := item.ID
		projection := PresentationItem{Category: category, GoalID: &id, Severity: severity,
			Description: item.Text + "\n" + item.Description}
		if len(item.Evidence) > 0 {
			quotes := make([]string, len(item.Evidence))
			for i, evidence := range item.Evidence {
				quotes[i] = evidence.Quote
			}
			quote := strings.Join(quotes, "\n")
			projection.Quote = &quote
		}
		items = append(items, projection)
	}
	if review.Coverage != "complete" {
		description := "本章目标来源或逐项核对不完整，请人工核实。"
		if review.Coverage == "not_configured" {
			description = "本章未配置可核对的关键事件，未完成目标验收。"
		}
		if language == "en-US" {
			if review.Coverage == "not_configured" {
				description = "No chapter key events are configured; goal acceptance was not performed."
			} else {
				description = "Chapter goal sources or checklist coverage are incomplete; verify manually."
			}
		}
		items = append(items, PresentationItem{Category: category, Severity: "unknown", Description: description})
	}
	return items
}

// appendJSString matches JSON.stringify for well-formed Unicode strings.
func appendJSString(dst []byte, value string) []byte {
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

func appendStringField(dst []byte, name, value string, comma bool) []byte {
	if comma {
		dst = append(dst, ',')
	}
	dst = appendJSString(dst, name)
	dst = append(dst, ':')
	return appendJSString(dst, value)
}

// ReviewJSON and ProjectionJSON preserve the original object's insertion order.
// The caller may embed these fragments when assembling the full report.
func ReviewJSON(review Review) []byte {
	dst := []byte(`{"version":1,"chapterNumber":`)
	dst = strconv.AppendInt(dst, review.ChapterNumber, 10)
	dst = appendStringField(dst, "coverage", review.Coverage, true)
	dst = append(dst, ',', '"', 'i', 't', 'e', 'm', 's', '"', ':', '[')
	for i, item := range review.Items {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendStringField(dst, "id", item.ID, false)
		dst = appendStringField(dst, "text", item.Text, true)
		dst = appendStringField(dst, "status", item.Status, true)
		dst = appendStringField(dst, "description", item.Description, true)
		dst = append(dst, ',', '"', 'e', 'v', 'i', 'd', 'e', 'n', 'c', 'e', '"', ':', '[')
		for j, evidence := range item.Evidence {
			if j > 0 {
				dst = append(dst, ',')
			}
			dst = append(dst, '{')
			dst = appendStringField(dst, "quote", evidence.Quote, false)
			dst = append(dst, ',', '"', 's', 't', 'a', 'r', 't', '"', ':')
			dst = strconv.AppendInt(dst, int64(evidence.Start), 10)
			dst = append(dst, ',', '"', 'e', 'n', 'd', '"', ':')
			dst = strconv.AppendInt(dst, int64(evidence.End), 10)
			dst = append(dst, '}')
		}
		dst = append(dst, ']', '}')
	}
	return append(dst, ']', '}')
}

func ProjectionJSON(items []PresentationItem) []byte {
	dst := []byte{'['}
	for i, item := range items {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendStringField(dst, "category", item.Category, false)
		if item.GoalID != nil {
			dst = appendStringField(dst, "goalId", *item.GoalID, true)
		}
		dst = appendStringField(dst, "severity", item.Severity, true)
		dst = appendStringField(dst, "description", item.Description, true)
		if item.Quote != nil {
			dst = appendStringField(dst, "quote", *item.Quote, true)
		}
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

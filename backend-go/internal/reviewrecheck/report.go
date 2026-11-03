// Package reviewrecheck ports the deterministic saved recheck report builder.
// Its output is evidence for M03 verification, not an admission decision.
package reviewrecheck

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

var ErrUnsupportedUnicode = errors.New("recheck unicode encoding unsupported")

type Finding struct {
	FindingID string `json:"findingId"`
	TargetID  string `json:"targetId"`
	Category  string `json:"category"`
	Kind      string `json:"kind"`
	MustShow  bool   `json:"mustShow,omitempty"`
}

type Context struct {
	Version       int       `json:"version"`
	SourceContent *string   `json:"sourceContent,omitempty"`
	Findings      []Finding `json:"findings"`
}

type Item struct {
	Category    string  `json:"category"`
	Severity    string  `json:"severity"`
	FindingID   string  `json:"findingId"`
	TargetID    string  `json:"targetId"`
	Description string  `json:"description"`
	Quote       *string `json:"quote,omitempty"`
	Resolved    *bool   `json:"resolved,omitempty"`
}

type Decision struct {
	FindingID       string `json:"findingId"`
	TargetID        string `json:"targetId"`
	Status          string `json:"status"`
	ReviewItemIndex int    `json:"reviewItemIndex"`
	Resolved        *bool  `json:"resolved,omitempty"`
}

type Report struct {
	Summary          string     `json:"summary"`
	Items            []Item     `json:"items"`
	Decisions        []Decision `json:"decisions"`
	ValidModelOutput bool       `json:"validModelOutput"`
}

type modelItem struct {
	FindingID     string
	TargetID      string
	Resolved      bool
	EvidenceQuote string
	Reason        string
}

type parsedOutput struct {
	Summary string
	Items   []modelItem
	Raw     string
}

var fencedJSON = regexp.MustCompile("(?i)^```json[ \\t]*\\r?\\n([\\s\\S]*?)\\r?\\n```$")
var pairedThink = regexp.MustCompile("(?is)<think>.*?(?:</think>|$)")
var orphanThink = regexp.MustCompile("(?i)</think>")
var anyThink = regexp.MustCompile("(?i)</?think>")

func jsSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' || r == ' ' ||
		r == '\u00a0' || r == '\u1680' || r >= '\u2000' && r <= '\u200a' ||
		r == '\u2028' || r == '\u2029' || r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

func trimJS(value string) string { return strings.TrimFunc(value, jsSpace) }

func stripThinking(value string) string {
	withoutPairs := pairedThink.ReplaceAllString(value, "")
	match := orphanThink.FindStringIndex(withoutPairs)
	if match == nil {
		return trimJS(anyThink.ReplaceAllString(withoutPairs, ""))
	}
	prefix, suffix := withoutPairs[:match[0]], withoutPairs[match[1]:]
	visible := trimJS(suffix)
	structured := visible
	if strings.HasPrefix(structured, "```") {
		structured = structured[3:]
		if len(structured) >= 4 && strings.EqualFold(structured[:4], "json") {
			structured = structured[4:]
		}
		structured = strings.TrimLeftFunc(structured, jsSpace)
	}
	leading := strings.TrimLeftFunc(prefix, jsSpace)
	lower := strings.ToLower(leading)
	if strings.HasPrefix(leading, "思考") || strings.HasPrefix(leading, "推理") || strings.HasPrefix(leading, "分析") ||
		strings.HasPrefix(lower, "reasoning") || strings.HasPrefix(lower, "analysis") ||
		strings.HasPrefix(structured, "{") || strings.HasPrefix(structured, "[") {
		return trimJS(anyThink.ReplaceAllString(suffix, ""))
	}
	return trimJS(anyThink.ReplaceAllString(prefix+suffix, ""))
}

func completeEnd(source string, start int) (int, bool) {
	var closers []byte
	inString, escaped := false, false
	for i := start; i < len(source); i++ {
		char := source[i]
		if inString {
			if escaped {
				escaped = false
			} else if char == '\\' {
				escaped = true
			} else if char == '"' {
				inString = false
			}
			continue
		}
		switch char {
		case '"':
			inString = true
		case '{':
			closers = append(closers, '}')
		case '[':
			closers = append(closers, ']')
		case '}', ']':
			if len(closers) == 0 || closers[len(closers)-1] != char {
				return 0, false
			}
			closers = closers[:len(closers)-1]
			if len(closers) == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

func extractObject(content string) (string, bool) {
	source := trimJS(stripThinking(content))
	candidate := ""
	for offset := 0; offset < len(source); {
		object, array := strings.IndexByte(source[offset:], '{'), strings.IndexByte(source[offset:], '[')
		start := -1
		if object >= 0 {
			start = offset + object
		}
		if array >= 0 && (start < 0 || offset+array < start) {
			start = offset + array
		}
		if start < 0 {
			break
		}
		end, ok := completeEnd(source, start)
		if !ok {
			return "", false
		}
		part := source[start : end+1]
		var decoded any
		if json.Unmarshal([]byte(part), &decoded) == nil {
			if _, ok := decoded.(map[string]any); ok {
				if candidate != "" {
					return "", false
				}
				candidate = part
			}
		}
		offset = end + 1
	}
	return candidate, candidate != ""
}

func parse(content string, reportVersion int) *parsedOutput {
	var raw string
	if reportVersion == 1 {
		trimmed := trimJS(content)
		raw = trimmed
		if match := fencedJSON.FindStringSubmatch(trimmed); match != nil {
			raw = trimJS(match[1])
		}
	} else if reportVersion == 2 {
		var ok bool
		raw, ok = extractObject(content)
		if !ok {
			return nil
		}
	} else {
		return nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	var value any
	if decoder.Decode(&value) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil
	}
	root, ok := value.(map[string]any)
	if !ok || len(root) != 2 {
		return nil
	}
	summary, ok := root["summary"].(string)
	if !ok || trimJS(summary) == "" {
		return nil
	}
	rows, ok := root["items"].([]any)
	if !ok {
		return nil
	}
	parsed := &parsedOutput{Summary: trimJS(summary), Items: make([]modelItem, 0, len(rows)), Raw: raw}
	for _, entry := range rows {
		row, ok := entry.(map[string]any)
		if !ok || len(row) != 5 {
			return nil
		}
		id, idOK := row["findingId"].(string)
		target, targetOK := row["targetId"].(string)
		resolved, resolvedOK := row["resolved"].(bool)
		quote, quoteOK := row["evidenceQuote"].(string)
		reason, reasonOK := row["reason"].(string)
		if !idOK || trimJS(id) == "" || !targetOK || trimJS(target) == "" || !resolvedOK ||
			!quoteOK || trimJS(quote) == "" || !reasonOK || trimJS(reason) == "" {
			return nil
		}
		parsed.Items = append(parsed.Items, modelItem{id, target, resolved, quote, reason})
	}
	return parsed
}

func countOccurrences(text, excerpt string) int {
	if excerpt == "" {
		return 0
	}
	count, offset := 0, 0
	for offset <= len(text) {
		found := strings.Index(text[offset:], excerpt)
		if found < 0 {
			break
		}
		count++
		offset += found + len(excerpt)
	}
	return count
}

func objectiveText(value string) string {
	var builder strings.Builder
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func hexQuad(value string, offset int) (uint16, bool) {
	if offset+4 > len(value) {
		return 0, false
	}
	var result uint16
	for i := offset; i < offset+4; i++ {
		result <<= 4
		switch c := value[i]; {
		case c >= '0' && c <= '9':
			result |= uint16(c - '0')
		case c >= 'a' && c <= 'f':
			result |= uint16(c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			result |= uint16(c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return result, true
}

func loneSurrogateEscape(value string) bool {
	inString := false
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '"':
			inString = !inString
		case '\\':
			if !inString || i+1 >= len(value) {
				continue
			}
			if value[i+1] != 'u' {
				i++
				continue
			}
			code, ok := hexQuad(value, i+2)
			if !ok {
				continue
			}
			if code >= 0xdc00 && code <= 0xdfff {
				return true
			}
			if code >= 0xd800 && code <= 0xdbff {
				if i+11 >= len(value) || value[i+6] != '\\' || value[i+7] != 'u' {
					return true
				}
				low, ok := hexQuad(value, i+8)
				if !ok || low < 0xdc00 || low > 0xdfff {
					return true
				}
				i += 11
				continue
			}
			i += 5
		}
	}
	return false
}

// Build reconstructs buildReviewCycleRecheckReport. Unsupported surrogate
// escapes in the selected JSON fail closed instead of changing identity.
func Build(content, merged string, context Context, uiLocale string, reportVersion int) (Report, error) {
	parsed := parse(content, reportVersion)
	if parsed != nil && (!utf8.ValidString(parsed.Summary) || loneSurrogateEscape(parsed.Raw)) {
		return Report{}, ErrUnsupportedUnicode
	}
	counts := make(map[string]int)
	if parsed != nil {
		for _, item := range parsed.Items {
			counts[item.FindingID]++
		}
	}
	report := Report{Items: make([]Item, 0, len(context.Findings)), Decisions: make([]Decision, 0, len(context.Findings)), ValidModelOutput: parsed != nil}
	if parsed != nil {
		report.Summary = parsed.Summary
	} else if uiLocale == "en-US" {
		report.Summary = "The recheck output was invalid; affected findings remain unknown."
	} else {
		report.Summary = "复核输出无效；受影响项目保持待核实。"
	}
	for _, finding := range context.Findings {
		var candidate *modelItem
		if counts[finding.FindingID] == 1 {
			for i := range parsed.Items {
				if parsed.Items[i].FindingID == finding.FindingID {
					candidate = &parsed.Items[i]
					break
				}
			}
		}
		index := len(report.Items)
		valid := candidate != nil && candidate.TargetID == finding.TargetID && countOccurrences(merged, candidate.EvidenceQuote) == 1
		if valid && finding.MustShow {
			valid = context.SourceContent != nil && !strings.Contains(objectiveText(*context.SourceContent), objectiveText(candidate.EvidenceQuote))
		}
		item := Item{Category: finding.Category, FindingID: finding.FindingID, TargetID: finding.TargetID}
		decision := Decision{FindingID: finding.FindingID, TargetID: finding.TargetID, ReviewItemIndex: index}
		if !valid {
			item.Severity, decision.Status = "unknown", "unknown"
			if uiLocale == "en-US" {
				item.Description = "The recheck did not provide unique, verifiable evidence."
			} else {
				item.Description = "复核未提供唯一且可验证的新证据。"
			}
		} else if context.Version == 2 && candidate.Resolved {
			item.Severity, decision.Status = "unknown", "unknown"
			if uiLocale == "en-US" {
				item.Description = "The model supplied anchored evidence, but semantic resolution still requires author verification. " + candidate.Reason
			} else {
				item.Description = "模型提供了可定位的新证据，但是否实质解决仍需作者核实。" + candidate.Reason
			}
			item.Quote = &candidate.EvidenceQuote
			resolved := false
			item.Resolved = &resolved
		} else {
			if candidate.Resolved {
				item.Severity, decision.Status = "pass", "resolved"
			} else {
				item.Severity, decision.Status = "warning", "unresolved"
			}
			item.Description, item.Quote = candidate.Reason, &candidate.EvidenceQuote
			item.Resolved, decision.Resolved = &candidate.Resolved, &candidate.Resolved
		}
		report.Items = append(report.Items, item)
		report.Decisions = append(report.Decisions, decision)
	}
	return report, nil
}

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

func appendField(dst []byte, key, value string, comma bool) []byte {
	if comma {
		dst = append(dst, ',')
	}
	dst = appendJSString(dst, key)
	dst = append(dst, ':')
	return appendJSString(dst, value)
}

// MarshalJSJSON preserves the original object field order and JS escaping.
func (report Report) MarshalJSJSON() []byte {
	dst := []byte{'{'}
	dst = appendField(dst, "summary", report.Summary, false)
	dst = append(dst, ',', '"', 'i', 't', 'e', 'm', 's', '"', ':', '[')
	for index, item := range report.Items {
		if index > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendField(dst, "category", item.Category, false)
		dst = appendField(dst, "severity", item.Severity, true)
		dst = appendField(dst, "findingId", item.FindingID, true)
		dst = appendField(dst, "targetId", item.TargetID, true)
		dst = appendField(dst, "description", item.Description, true)
		if item.Quote != nil {
			dst = appendField(dst, "quote", *item.Quote, true)
		}
		if item.Resolved != nil {
			dst = append(dst, ',', '"', 'r', 'e', 's', 'o', 'l', 'v', 'e', 'd', '"', ':')
			if *item.Resolved {
				dst = append(dst, "true"...)
			} else {
				dst = append(dst, "false"...)
			}
		}
		dst = append(dst, '}')
	}
	dst = append(dst, ']', ',', '"', 'd', 'e', 'c', 'i', 's', 'i', 'o', 'n', 's', '"', ':', '[')
	for index, decision := range report.Decisions {
		if index > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, '{')
		dst = appendField(dst, "findingId", decision.FindingID, false)
		dst = appendField(dst, "targetId", decision.TargetID, true)
		dst = appendField(dst, "status", decision.Status, true)
		dst = append(dst, ',', '"', 'r', 'e', 'v', 'i', 'e', 'w', 'I', 't', 'e', 'm', 'I', 'n', 'd', 'e', 'x', '"', ':')
		dst = append(dst, []byte(strconv.Itoa(decision.ReviewItemIndex))...)
		if decision.Resolved != nil {
			dst = append(dst, ',', '"', 'r', 'e', 's', 'o', 'l', 'v', 'e', 'd', '"', ':')
			if *decision.Resolved {
				dst = append(dst, "true"...)
			} else {
				dst = append(dst, "false"...)
			}
		}
		dst = append(dst, '}')
	}
	dst = append(dst, ']', ',', '"', 'v', 'a', 'l', 'i', 'd', 'M', 'o', 'd', 'e', 'l', 'O', 'u', 't', 'p', 'u', 't', '"', ':')
	if report.ValidModelOutput {
		dst = append(dst, "true"...)
	} else {
		dst = append(dst, "false"...)
	}
	return append(dst, '}')
}

func (report Report) PrettyJSJSON() []byte {
	var dst bytes.Buffer
	if err := json.Indent(&dst, report.MarshalJSJSON(), "", "  "); err != nil {
		return nil
	}
	return dst.Bytes()
}

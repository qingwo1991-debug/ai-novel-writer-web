// Package reviewreport contains isolated Go ports of the original review
// report contract. Parsing alone does not prove a stored M03 review cycle.
package reviewreport

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"unicode/utf8"
)

var (
	ErrInvalidReviewContract = errors.New("invalid review contract")
	// JS JSON.parse retains isolated UTF-16 surrogate escapes. Go strings cannot
	// represent them without changing their identity, so this port fails closed.
	ErrUnsupportedUnicode = errors.New("review unicode encoding unsupported")
	pairedThink           = regexp.MustCompile(`(?is)<think>.*?(?:</think>|$)`)
	orphanThink           = regexp.MustCompile(`(?i)</think>`)
	anyThink              = regexp.MustCompile(`(?i)</?think>`)
	fencedJSON            = regexp.MustCompile("(?i)^```(?:json)?[ \\t]*\\r?\\n(?s:(.*?))\\r?\\n```$")
)

// Item keeps the original parser's field order for later ordered report
// assembly. Quote is omitted for a pass item with a blank quote.
type Item struct {
	Category    string  `json:"category"`
	Severity    string  `json:"severity"`
	Description string  `json:"description"`
	Quote       *string `json:"quote,omitempty"`
}

// Result is the bounded result of parseReviewGenerationResult, before goal
// review or consistency findings are added. GoalReviews remains opaque here.
type Result struct {
	GoalReviews json.RawMessage `json:"goalReviews,omitempty"`
	Summary     string          `json:"summary"`
	Items       []Item          `json:"items"`
}

func jsSpace(r rune) bool {
	return r == '\t' || r == '\n' || r == '\v' || r == '\f' || r == '\r' ||
		r == ' ' || r == '\u00a0' || r == '\u1680' ||
		(r >= '\u2000' && r <= '\u200a') || r == '\u2028' || r == '\u2029' ||
		r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

func trimJS(value string) string { return strings.TrimFunc(value, jsSpace) }

func stripDraftThinkingTags(value string) string {
	if value == "" {
		return value
	}
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
	hasStructured := strings.HasPrefix(structured, "{") || strings.HasPrefix(structured, "[")
	leading := strings.TrimLeftFunc(prefix, jsSpace)
	lower := strings.ToLower(leading)
	reasoning := strings.HasPrefix(leading, "思考") || strings.HasPrefix(leading, "推理") ||
		strings.HasPrefix(leading, "分析") || strings.HasPrefix(lower, "reasoning") || strings.HasPrefix(lower, "analysis")
	if reasoning || hasStructured {
		return trimJS(anyThink.ReplaceAllString(suffix, ""))
	}
	return trimJS(anyThink.ReplaceAllString(prefix+suffix, ""))
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

func decodeJSON(raw string) (any, map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, nil, ErrInvalidReviewContract
	}
	object, ok := value.(map[string]any)
	if !ok {
		return value, nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil, nil, err
	}
	_ = object
	return value, fields, nil
}

func completeJSONEnd(source string, start int) (int, bool) {
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

func extractSingleCompleteObject(content string) (string, error) {
	source := trimJS(stripDraftThinkingTags(content))
	var candidate string
	for offset := 0; offset < len(source); {
		object := strings.IndexByte(source[offset:], '{')
		array := strings.IndexByte(source[offset:], '[')
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
		end, ok := completeJSONEnd(source, start)
		if !ok {
			return "", ErrInvalidReviewContract
		}
		part := source[start : end+1]
		parsed, _, err := decodeJSON(part)
		if err == nil {
			if _, ok := parsed.(map[string]any); ok {
				if candidate != "" {
					return "", ErrInvalidReviewContract
				}
				candidate = part
			}
		}
		offset = end + 1
	}
	if candidate == "" {
		return "", ErrInvalidReviewContract
	}
	return candidate, nil
}

func boundedText(value string, max int, version int) string {
	trimmed := trimJS(value)
	if version == 2 {
		return trimmed
	}
	runes := []rune(trimmed)
	if len(runes) > max {
		runes = runes[:max]
	}
	return string(runes)
}

// Parse ports parseReviewGenerationResult for ordinary Unicode inputs. It
// rejects isolated UTF-16 surrogate escapes rather than replacing their IDs.
// No generated review is admitted for publication by this parser alone.
func Parse(content string, version int) (Result, error) {
	var zero Result
	if version != 1 && version != 2 {
		return zero, ErrInvalidReviewContract
	}
	trimmed := stripDraftThinkingTags(content)
	raw := trimmed
	if fenced := fencedJSON.FindStringSubmatch(trimmed); fenced != nil {
		raw = trimJS(fenced[1])
	}
	value, fields, err := decodeJSON(raw)
	if err != nil {
		raw, err = extractSingleCompleteObject(trimmed)
		if err != nil {
			return zero, err
		}
		value, fields, err = decodeJSON(raw)
		if err != nil {
			return zero, err
		}
	}
	// Judge only the selected JSON report. Prose and discarded malformed
	// candidates may contain UTF-16 escapes that never enter the result.
	if !utf8.ValidString(raw) || loneSurrogateEscape(raw) {
		return zero, ErrUnsupportedUnicode
	}
	object, ok := value.(map[string]any)
	if !ok || len(object) < 2 || len(object) > 3 {
		return zero, ErrInvalidReviewContract
	}
	for key := range object {
		if key != "summary" && key != "items" && key != "goalReviews" {
			return zero, ErrInvalidReviewContract
		}
	}
	summary, ok := object["summary"].(string)
	if !ok {
		return zero, ErrInvalidReviewContract
	}
	items, ok := object["items"].([]any)
	if !ok || len(items) < 1 || len(items) > 10 {
		return zero, ErrInvalidReviewContract
	}
	result := Result{Summary: boundedText(summary, 120, version), Items: make([]Item, 0, len(items))}
	if goal, exists := fields["goalReviews"]; exists {
		result.GoalReviews = bytes.Clone(goal)
	}
	if trimJS(result.Summary) == "" {
		return zero, ErrInvalidReviewContract
	}
	for _, rawItem := range items {
		fields, ok := rawItem.(map[string]any)
		if !ok || len(fields) < 3 || len(fields) > 4 {
			return zero, ErrInvalidReviewContract
		}
		for key := range fields {
			if key != "category" && key != "severity" && key != "description" && key != "quote" {
				return zero, ErrInvalidReviewContract
			}
		}
		category, categoryOK := fields["category"].(string)
		severity, severityOK := fields["severity"].(string)
		description, descriptionOK := fields["description"].(string)
		quoteValue, hasQuote := fields["quote"]
		quote, quoteOK := quoteValue.(string)
		if !categoryOK || !severityOK || !descriptionOK ||
			(severity != "error" && severity != "warning" && severity != "pass") ||
			(!hasQuote && severity != "pass") || (hasQuote && !quoteOK) || trimJS(category) == "" {
			return zero, ErrInvalidReviewContract
		}
		item := Item{Category: category, Severity: severity, Description: boundedText(description, 200, version)}
		if trimJS(item.Description) == "" {
			return zero, ErrInvalidReviewContract
		}
		if hasQuote && (severity != "pass" || trimJS(quote) != "") {
			bounded := boundedText(quote, 160, version)
			if trimJS(bounded) == "" {
				return zero, ErrInvalidReviewContract
			}
			item.Quote = &bounded
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

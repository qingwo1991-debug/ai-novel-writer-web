// Package reviewconsistency ports only the original review finding merge. It
// does not calculate findings or authorize a saved review.
package reviewconsistency

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"unicode/utf8"
)

var ErrInvalidInput = errors.New("REVIEW_CONSISTENCY_INVALID_INPUT")

// Field keeps JavaScript object insertion order. Value must be a normalized
// JSON value supplied by the review builder, not an arbitrary Go map marshal.
type Field struct {
	Key   string
	Value json.RawMessage
}

type Review struct{ Fields []Field }

type LocalizedText struct {
	ZhCN string `json:"zhCN"`
	EnUS string `json:"enUS"`
}

type Finding struct {
	StableFactKey string        `json:"stableFactKey"`
	Severity      string        `json:"severity"`
	SourceChapter int64         `json:"sourceChapter"`
	Evidence      string        `json:"evidence"`
	Issue         LocalizedText `json:"issue"`
	Suggestion    LocalizedText `json:"suggestion"`
}

// DecodeReview retains top-level property order as JSON.parse/JS object spread
// does for the non-index keys emitted by the review report builder. Duplicate
// and integer-index keys are rejected: neither occurs in that builder's shape.
func DecodeReview(data []byte) (Review, error) {
	var zero Review
	if !utf8.Valid(data) {
		return zero, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return zero, ErrInvalidInput
	}
	seen := make(map[string]bool)
	for decoder.More() {
		keyToken, err := decoder.Token()
		key, ok := keyToken.(string)
		if err != nil || !ok || seen[key] || indexKey(key) {
			return zero, ErrInvalidInput
		}
		seen[key] = true
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return zero, ErrInvalidInput
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, raw); err != nil {
			return zero, ErrInvalidInput
		}
		zero.Fields = append(zero.Fields, Field{Key: key, Value: compact.Bytes()})
	}
	end, err := decoder.Token()
	if err != nil || end != json.Delim('}') {
		return Review{}, ErrInvalidInput
	}
	if _, err := decoder.Token(); err != io.EOF {
		return Review{}, ErrInvalidInput
	}
	return zero, nil
}

// JS enumerates canonical 32-bit array-index properties before other keys.
// The trusted review report builder never emits these properties.
func indexKey(key string) bool {
	if key == "" || (len(key) > 1 && key[0] == '0') {
		return false
	}
	value, err := strconv.ParseUint(key, 10, 32)
	return err == nil && value != 4294967295 && strconv.FormatUint(value, 10) == key
}

func jsString(dst []byte, value string) []byte {
	dst = append(dst, '"')
	for _, r := range value {
		switch r {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
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
			if r < 0x20 {
				const digits = "0123456789abcdef"
				dst = append(dst, '\\', 'u', '0', '0', digits[r>>4], digits[r&15])
			} else {
				dst = utf8.AppendRune(dst, r)
			}
		}
	}
	return append(dst, '"')
}

// MarshalJSJSON preserves the builder's key order and the mapped finding item
// key order used by JSON.stringify. Values are already normalized JSON.
func (review Review) MarshalJSJSON() ([]byte, error) {
	dst := []byte{'{'}
	for index, field := range review.Fields {
		if index > 0 {
			dst = append(dst, ',')
		}
		if !utf8.ValidString(field.Key) || !json.Valid(field.Value) || indexKey(field.Key) {
			return nil, ErrInvalidInput
		}
		dst = jsString(dst, field.Key)
		dst = append(dst, ':')
		dst = append(dst, field.Value...)
	}
	return append(dst, '}'), nil
}

func mappedFinding(finding Finding, locale string) ([]byte, error) {
	if finding.Severity != "warning" || !utf8.ValidString(finding.StableFactKey) ||
		!utf8.ValidString(finding.Evidence) || !utf8.ValidString(finding.Issue.ZhCN) ||
		!utf8.ValidString(finding.Issue.EnUS) || finding.SourceChapter < 0 {
		return nil, ErrInvalidInput
	}
	category := "确定性一致性预检"
	description := finding.Issue.ZhCN
	if locale == "en-US" {
		category = "Deterministic continuity preflight"
		description = finding.Issue.EnUS
	}
	dst := []byte(`{"category":`)
	dst = jsString(dst, category)
	dst = append(dst, []byte(`,"severity":"warning","description":`)...)
	dst = jsString(dst, description)
	dst = append(dst, []byte(`,"quote":`)...)
	dst = jsString(dst, finding.Evidence)
	dst = append(dst, []byte(`,"stableFactKey":`)...)
	dst = jsString(dst, finding.StableFactKey)
	dst = append(dst, []byte(`,"sourceChapter":`)...)
	dst = strconv.AppendInt(dst, finding.SourceChapter, 10)
	return append(dst, '}'), nil
}

// Merge appends mapped preflight findings after existing review items. The
// trusted builder supplies locale and typed findings; this has no project or
// report admission authority.
func Merge(review Review, findings []Finding, locale string) (Review, error) {
	if locale != "zh-CN" && locale != "en-US" {
		return Review{}, ErrInvalidInput
	}
	out := Review{Fields: make([]Field, len(review.Fields))}
	copy(out.Fields, review.Fields)
	itemsIndex := -1
	for index, field := range out.Fields {
		if field.Key == "items" {
			if itemsIndex != -1 {
				return Review{}, ErrInvalidInput
			}
			itemsIndex = index
		}
	}
	items := make([]json.RawMessage, 0)
	if itemsIndex != -1 && len(out.Fields[itemsIndex].Value) > 0 && out.Fields[itemsIndex].Value[0] == '[' {
		if err := json.Unmarshal(out.Fields[itemsIndex].Value, &items); err != nil {
			return Review{}, ErrInvalidInput
		}
	}
	for _, finding := range findings {
		mapped, err := mappedFinding(finding, locale)
		if err != nil {
			return Review{}, err
		}
		items = append(items, mapped)
	}
	array := []byte{'['}
	for index, item := range items {
		if index > 0 {
			array = append(array, ',')
		}
		array = append(array, item...)
	}
	array = append(array, ']')
	if itemsIndex == -1 {
		out.Fields = append(out.Fields, Field{Key: "items", Value: array})
	} else {
		out.Fields[itemsIndex].Value = array
	}
	return out, nil
}

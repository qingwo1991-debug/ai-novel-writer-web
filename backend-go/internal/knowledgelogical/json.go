// Package knowledgelogical implements a bounded compatibility lane for the
// original JavaScript knowledge snapshot encodings. It owns no storage handles
// and grants no admission, writer, embedding, search, or project authority.
package knowledgelogical

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

func failure(code string) error { return errors.New("KNOWLEDGE_LOGICAL_" + code) }

// Options names the original Node default locale, independently established by
// the caller. Only en-US and the generated-ID ordering lane are supported. This
// is NOT an ICU implementation: arbitrary multiple Unicode IDs fail closed.
// No case folding or NFKC is applied to IDs, content, or table hashes.
type Options struct {
	Locale string
}

func contextOK(ctx context.Context) error {
	if ctx == nil {
		return failure("CONTEXT_REQUIRED")
	}
	return ctx.Err()
}

// member and object retain insertion order. Using map iteration or Go's JSON
// encoder would lose Arrow field order and alter HTML/U+2028/U+2029 escaping.
type member struct {
	key   string
	value any
}
type object []member

type encoder struct {
	ctx   context.Context
	limit int
	out   []byte
}

func (e *encoder) add(s string) error {
	if len(s) > e.limit-len(e.out) {
		return failure("OUTPUT_LIMIT")
	}
	e.out = append(e.out, s...)
	return nil
}
func (e *encoder) string(s string) error {
	if !utf8.ValidString(s) {
		return failure("TEXT_INVALID")
	}
	if err := e.add("\""); err != nil {
		return err
	}
	start := 0
	const hex = "0123456789abcdef"
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 32 && c != '"' && c != '\\' {
			continue
		}
		if err := e.add(s[start:i]); err != nil {
			return err
		}
		var escaped string
		switch c {
		case '"':
			escaped = "\\\""
		case '\\':
			escaped = "\\\\"
		case '\b':
			escaped = "\\b"
		case '\f':
			escaped = "\\f"
		case '\n':
			escaped = "\\n"
		case '\r':
			escaped = "\\r"
		case '\t':
			escaped = "\\t"
		default:
			escaped = string([]byte{'\\', 'u', '0', '0', hex[c>>4], hex[c&15]})
		}
		if err := e.add(escaped); err != nil {
			return err
		}
		start = i + 1
	}
	if err := e.add(s[start:]); err != nil {
		return err
	}
	return e.add("\"")
}

// numberString follows ECMAScript's finite binary64 shortest round-trip decimal
// encoding and its fixed/exponent cutovers. Numeric inputs have already crossed
// the JS Number boundary, NOT a fabricated typeof/string coercion. In particular
// unsafe integers round as binary64; only domain integer fields require safety.
func numberString(n float64) (string, error) {
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return "", failure("NUMBER_INVALID")
	}
	if n == 0 {
		return "0", nil
	} // JSON.stringify(-0) === '0'.
	abs := math.Abs(n)
	if abs >= 1e-6 && abs < 1e21 {
		return strconv.FormatFloat(n, 'f', -1, 64), nil
	}
	s := strconv.FormatFloat(n, 'e', -1, 64)
	at := strings.LastIndexByte(s, 'e')
	mantissa, exponent := s[:at], s[at+1:]
	sign := "+"
	if exponent[0] == '-' {
		sign = "-"
	}
	exponent = strings.TrimLeft(exponent[1:], "0")
	if exponent == "" {
		exponent = "0"
	}
	return mantissa + "e" + sign + exponent, nil
}

func number(value any) (float64, error) {
	var n float64
	switch v := value.(type) {
	case json.Number:
		s := string(v)
		if len(s) == 0 || len(s) > 128 || !json.Valid([]byte(s)) || (s[0] != '-' && (s[0] < '0' || s[0] > '9')) {
			return 0, failure("NUMBER_INVALID")
		}
		var err error
		n, err = strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, failure("NUMBER_INVALID")
		}
	case float64:
		n = v
	case float32:
		n = float64(v)
	case int:
		n = float64(v)
	case int8:
		n = float64(v)
	case int16:
		n = float64(v)
	case int32:
		n = float64(v)
	case int64:
		n = float64(v)
	case uint:
		n = float64(v)
	case uint8:
		n = float64(v)
	case uint16:
		n = float64(v)
	case uint32:
		n = float64(v)
	case uint64:
		n = float64(v)
	default:
		return 0, failure("NUMBER_INVALID")
	}
	if math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, failure("NUMBER_INVALID")
	}
	return n, nil
}

func (e *encoder) value(value any) error {
	if err := contextOK(e.ctx); err != nil {
		return err
	}
	switch v := value.(type) {
	case nil:
		return e.add("null")
	case bool:
		if v {
			return e.add("true")
		}
		return e.add("false")
	case string:
		return e.string(v)
	case object:
		if err := e.add("{"); err != nil {
			return err
		}
		for i, m := range v {
			if i > 0 {
				if err := e.add(","); err != nil {
					return err
				}
			}
			if err := e.string(m.key); err != nil {
				return err
			}
			if err := e.add(":"); err != nil {
				return err
			}
			if err := e.value(m.value); err != nil {
				return err
			}
		}
		return e.add("}")
	case []any:
		if err := e.add("["); err != nil {
			return err
		}
		for i, element := range v {
			if i > 0 {
				if err := e.add(","); err != nil {
					return err
				}
			}
			if err := e.value(element); err != nil {
				return err
			}
		}
		return e.add("]")
	default:
		n, err := number(value)
		if err != nil {
			return err
		}
		s, err := numberString(n)
		if err != nil {
			return err
		}
		return e.add(s)
	}
}
func encode(ctx context.Context, value any, limit int) ([]byte, error) {
	e := encoder{ctx: ctx, limit: limit}
	if err := e.value(value); err != nil {
		return nil, err
	}
	return e.out, nil
}

func utf16Length(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r > 0xffff {
			n++
		}
	}
	return n
}

// utf16Less matches JS default Array.sort's code-unit order (NOT localeCompare).
func utf16Less(a, b string) bool {
	x, y := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(x) && i < len(y); i++ {
		if x[i] != y[i] {
			return x[i] < y[i]
		}
	}
	return len(x) < len(y)
}

package graphgen

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// TextHash mirrors `textHash` in electron/repositories/generation-run-repository.ts:92:
//
//	export const textHash = (text: string) => createHash('sha256').update(text, 'utf8').digest('hex');
//
// Precisely the UTF-8 bytes of the Go string are hashed (Node's 'utf8' encoding)
// and the digest is rendered as lowercase hexadecimal without separators, so
// TextHash("") == "e3b0c442...b855". Nothing else is mixed in: callers hash either
// raw text (a draft body, a lifecycle content string) or the output of
// JSON.stringify, whose encoding rules EncodeJSON reproduces below.
func TextHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// ErrUnsupportedJSONValue reports a Go value with no faithful JSON.stringify
// counterpart. TypeScript's JSON.stringify never fails for these inputs; the Go
// port returns an error instead of guessing (see EncodeJSON).
var ErrUnsupportedJSONValue = errors.New("GRAPHGEN_UNSUPPORTED_JSON_VALUE")

// Member is a single key/value pair of an Object. The pair order is preserved,
// mirroring the insertion order of a TypeScript object literal.
type Member struct {
	Key   string
	Value any
}

// Object is an ordered JSON object. TypeScript objects are serialised by
// JSON.stringify in property insertion order, so any object the Go port must
// encode with a non-struct shape is expressed as an ordered Member list.
type Object []Member

// ObjectValue is implemented by types that need to choose their own key order
// per branch, mirroring a TypeScript object literal that is assembled
// conditionally (for example the discriminated PlotTreeSourceReference).
type ObjectValue interface {
	JSONValue() any
}

// EncodeJSON mirrors JavaScript JSON.stringify for the values graph-generation
// tasks are built from. The rules it reproduces:
//
//   - object keys keep insertion order (Go struct field order, or Object order);
//     no key sorting ever happens, because JSON.stringify never sorts;
//   - no whitespace is emitted between tokens;
//   - strings are quoted with the ECMAScript QuoteJSONString escapes only:
//     `"` and `\`, \b \f \n \r \t, other code points below U+0020 as \u00xx
//     (lowercase hex). Everything else - Chinese text, U+2028/U+2029, emoji -
//     is emitted verbatim as UTF-8. JSON.stringify never HTML-escapes, which is
//     why encoding/json is not used here (it escapes <, >, & and U+2028/9);
//   - numbers use the ECMAScript Number::toString algorithm; NaN and ±Infinity
//     are written as `null`, exactly like JSON.stringify;
//   - nil slices and nil arrays are written as `[]`: every array reaching these
//     tasks comes from a TypeScript `.map()`/row parser and is therefore always
//     an array, never null. Use a nil pointer or untyped nil for JSON null;
//   - a nil pointer/interface is written as `null`, or omitted when the field's
//     json tag carries `omitempty` (that is how a TypeScript `undefined`
//     optional property is modelled);
//   - Go maps are rejected with ErrUnsupportedJSONValue, because Go map
//     iteration order is unspecified and TypeScript insertion order cannot be
//     reproduced from it.
func EncodeJSON(value any) (string, error) {
	var builder strings.Builder
	if err := writeJSON(&builder, value); err != nil {
		return "", err
	}
	return builder.String(), nil
}

// MustEncodeJSON is EncodeJSON for inputs that are known to be encodable.
func MustEncodeJSON(value any) string {
	encoded, err := EncodeJSON(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func writeJSON(builder *strings.Builder, value any) error {
	if value == nil {
		builder.WriteString("null")
		return nil
	}
	switch typed := value.(type) {
	case ObjectValue:
		return writeJSON(builder, typed.JSONValue())
	case Object:
		builder.WriteByte('{')
		for index, member := range typed {
			if index > 0 {
				builder.WriteByte(',')
			}
			writeJSONString(builder, member.Key)
			builder.WriteByte(':')
			if err := writeJSON(builder, member.Value); err != nil {
				return err
			}
		}
		builder.WriteByte('}')
		return nil
	}
	return writeJSONReflect(builder, reflect.ValueOf(value))
}

func writeJSONReflect(builder *strings.Builder, value reflect.Value) error {
	for value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			builder.WriteString("null")
			return nil
		}
		value = value.Elem()
	}
	switch value.Kind() {
	case reflect.Bool:
		builder.WriteString(strconv.FormatBool(value.Bool()))
	case reflect.String:
		writeJSONString(builder, value.String())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return writeJSONInteger(builder, value.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return writeJSONUnsigned(builder, value.Uint())
	case reflect.Float32, reflect.Float64:
		return writeJSONNumber(builder, value.Float())
	case reflect.Slice, reflect.Array:
		builder.WriteByte('[')
		for index := 0; index < value.Len(); index++ {
			if index > 0 {
				builder.WriteByte(',')
			}
			if err := writeJSON(builder, value.Index(index).Interface()); err != nil {
				return err
			}
		}
		builder.WriteByte(']')
	case reflect.Struct:
		return writeJSONStruct(builder, value)
	case reflect.Map:
		return errors.New("GRAPHGEN_UNSUPPORTED_JSON_VALUE: Go map values cannot mirror JSON.stringify key order")
	default:
		return errors.New("GRAPHGEN_UNSUPPORTED_JSON_VALUE: unsupported Go kind " + value.Kind().String())
	}
	return nil
}

// safeJSONInteger is Number.MAX_SAFE_INTEGER: above it a JavaScript number can no
// longer represent the integer exactly, so TypeScript would print the rounded
// double instead of the exact digits.
const safeJSONInteger = 1<<53 - 1

func writeJSONInteger(builder *strings.Builder, value int64) error {
	if value > safeJSONInteger || value < -safeJSONInteger {
		return writeJSONNumber(builder, float64(value))
	}
	builder.WriteString(strconv.FormatInt(value, 10))
	return nil
}

func writeJSONUnsigned(builder *strings.Builder, value uint64) error {
	if value > safeJSONInteger {
		return writeJSONNumber(builder, float64(value))
	}
	builder.WriteString(strconv.FormatUint(value, 10))
	return nil
}

func writeJSONNumber(builder *strings.Builder, value float64) error {
	// JSON.stringify(NaN) and JSON.stringify(±Infinity) are both "null".
	if math.IsNaN(value) || math.IsInf(value, 0) {
		builder.WriteString("null")
		return nil
	}
	builder.WriteString(formatJSONNumber(value))
	return nil
}

// formatJSONNumber reproduces ECMAScript Number::toString (the algorithm
// JSON.stringify uses for finite numbers), driven by Go's shortest round-trip
// digits.
func formatJSONNumber(value float64) string {
	if value == 0 {
		// (0).toString() is "0" and (-0).toString() is "0" too.
		return "0"
	}
	sign := ""
	if value < 0 {
		sign = "-"
		value = -value
	}
	encoded := strconv.FormatFloat(value, 'e', -1, 64)
	mantissa, exponentText, _ := strings.Cut(encoded, "e")
	exponent, _ := strconv.Atoi(exponentText)
	digits := strings.Replace(mantissa, ".", "", 1)
	// k is the number of significant digits, n the position of the decimal point
	// relative to the digit string (ES2023 7.1.12.1 steps 5-10).
	k := len(digits)
	n := exponent + 1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	case k == 1:
		return sign + digits + "e" + formatExponent(n-1)
	default:
		return sign + digits[:1] + "." + digits[1:] + "e" + formatExponent(n-1)
	}
}

func formatExponent(exponent int) string {
	if exponent < 0 {
		return "-" + strconv.Itoa(-exponent)
	}
	return "+" + strconv.Itoa(exponent)
}

// writeJSONString reproduces ECMAScript QuoteJSONString: only quote, backslash
// and control code points are escaped, and the four two-character escapes are
// preferred over \u00xx.
func writeJSONString(builder *strings.Builder, value string) {
	builder.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if character < 0x20 {
				builder.WriteString(`\u00`)
				builder.WriteString(strconv.FormatInt(int64(character)>>4, 16))
				builder.WriteString(strconv.FormatInt(int64(character)&0xf, 16))
				continue
			}
			builder.WriteRune(character)
		}
	}
	builder.WriteByte('"')
}

func writeJSONStruct(builder *strings.Builder, value reflect.Value) error {
	builder.WriteByte('{')
	wroteMember := false
	if err := writeJSONStructMembers(builder, value, &wroteMember); err != nil {
		return err
	}
	builder.WriteByte('}')
	return nil
}

// writeJSONStructMembers writes the fields of a struct in declaration order.
// Fields of an untagged embedded struct are inlined, matching both Go's
// encoding/json and TypeScript interface extension.
func writeJSONStructMembers(builder *strings.Builder, value reflect.Value, wroteMember *bool) error {
	structType := value.Type()
	for index := 0; index < structType.NumField(); index++ {
		field := structType.Field(index)
		if field.PkgPath != "" {
			// Unexported field: JSON.stringify cannot see it either.
			continue
		}
		fieldValue := value.Field(index)
		if field.Anonymous && field.Tag.Get("json") == "" {
			inner := fieldValue
			usable := true
			for inner.Kind() == reflect.Pointer || inner.Kind() == reflect.Interface {
				if inner.IsNil() {
					usable = false
					break
				}
				inner = inner.Elem()
			}
			if usable && inner.Kind() == reflect.Struct {
				if err := writeJSONStructMembers(builder, inner, wroteMember); err != nil {
					return err
				}
			}
			continue
		}
		name, omitEmpty := jsonFieldName(field)
		if name == "-" {
			continue
		}
		if omitEmpty && isEmptyJSONValue(fieldValue) {
			continue
		}
		if *wroteMember {
			builder.WriteByte(',')
		}
		*wroteMember = true
		writeJSONString(builder, name)
		builder.WriteByte(':')
		if err := writeJSON(builder, fieldValue.Interface()); err != nil {
			return err
		}
	}
	return nil
}

func jsonFieldName(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("json")
	if tag == "" {
		return field.Name, false
	}
	name, options, _ := strings.Cut(tag, ",")
	if name == "" {
		name = field.Name
	}
	omitEmpty := false
	for _, option := range strings.Split(options, ",") {
		if option == "omitempty" {
			omitEmpty = true
		}
	}
	return name, omitEmpty
}

// isEmptyJSONValue follows encoding/json's isEmptyValue so that `omitempty`
// models a TypeScript optional property that is `undefined`.
func isEmptyJSONValue(value reflect.Value) bool {
	switch value.Kind() {
	case reflect.Array, reflect.Map, reflect.Slice, reflect.String:
		return value.Len() == 0
	case reflect.Bool:
		return !value.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return value.Int() == 0
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return value.Uint() == 0
	case reflect.Float32, reflect.Float64:
		return value.Float() == 0
	case reflect.Interface, reflect.Pointer:
		return value.IsNil()
	}
	return false
}

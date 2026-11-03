// Package publicresponse is the browser-output boundary, never a private-data
// transformation. Register credentials BEFORE using them; reject registration
// errors. Sanitize immediately before HTTP/SSE encoding, not before routing.
package publicresponse

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	MinSecretLength = 4
	MaxSecrets = 512
	MaxSecretLength = 64 * 1024
	MaxSecretCharacters = 1024 * 1024
	MaxDepth = 128
	// Raw JSON is decoded, never returned with a MarshalJSON bypass. These are
	// defensive Go-only input bounds, not limits on ordinary JSON-data clones.
	MaxRawJSONBytes = 8 * 1024 * 1024
	maxRawJSONDepth = 256
	redacted = "[REDACTED]"
	circular = "[Circular]"
	truncated = "[Truncated]"
)

var ErrSecretLimit = errors.New("PUBLIC_RESPONSE_SECRET_LIMIT")

// Sanitizer owns an append-only, bounded registry. Its zero value is usable.
// RegisterSecret and Sanitize may run concurrently. Each clone uses one coherent
// registry snapshot. No credential is inferred from response fields or evicted.
type Sanitizer struct {
	mu sync.RWMutex
	secrets map[string]struct{}
	units [][]uint16
	characters int
}

func New() *Sanitizer { return &Sanitizer{} }

func (s *Sanitizer) RegisterSecret(value string) (bool, error) {
	// Every UTF-16 unit occupies at most three bytes in UTF-8/WTF-8. Reject
	// unretainable inputs before allocating a potentially enormous unit slice.
	if len(value) > 3 * MaxSecretLength { return false, ErrSecretLimit }
	units := stringUnits(value)
	if len(units) < MinSecretLength { return false, nil }
	// Canonicalize the Go encoding, not the JS value: a surrogate pair in
	// WTF-8 and its ordinary UTF-8 astral rune are the same JS Set member.
	value = unitsString(units)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.secrets[value]; exists { return true, nil }
	if len(units) > MaxSecretLength || len(s.units) >= MaxSecrets || s.characters + len(units) > MaxSecretCharacters {
		return false, ErrSecretLimit
	}
	if s.secrets == nil { s.secrets = make(map[string]struct{}) }
	s.secrets[value] = struct{}{}
	s.units = append(s.units, units)
	s.characters += len(units)
	return true, nil
}

// stringUnits also accepts WTF-8 surrogate units for exact JS string fixtures.
// Ordinary Go UTF-8, including astral runes, uses JS UTF-16 length and offsets.
func stringUnits(value string) []uint16 {
	out := make([]uint16, 0, len(value))
	for len(value) > 0 {
		if len(value) >= 3 && value[0] == 0xed && value[1] >= 0xa0 && value[1] <= 0xbf && value[2] & 0xc0 == 0x80 {
			out = append(out, uint16(value[0]&15)<<12 | uint16(value[1]&63)<<6 | uint16(value[2]&63))
			value = value[3:]
			continue
		}
		r, n := utf8.DecodeRuneInString(value)
		value = value[n:]
		if r <= 0xffff { out = append(out, uint16(r)) } else { a, b := utf16.EncodeRune(r); out = append(out, uint16(a), uint16(b)) }
	}
	return out
}

func unitsString(units []uint16) string {
	out := make([]byte, 0, len(units))
	for i := 0; i < len(units); i++ {
		u := units[i]
		if u >= 0xd800 && u <= 0xdbff && i+1 < len(units) && units[i+1] >= 0xdc00 && units[i+1] <= 0xdfff {
			out = utf8.AppendRune(out, utf16.DecodeRune(rune(u), rune(units[i+1])))
			i++
		} else if u >= 0xd800 && u <= 0xdfff {
			out = append(out, byte(0xe0 | u>>12), byte(0x80 | (u>>6)&63), byte(0x80 | u&63))
		} else { out = utf8.AppendRune(out, rune(u)) }
	}
	return string(out)
}

func containsUnits(value, secret []uint16) bool {
	if len(secret) > len(value) { return false }
	for i := 0; i <= len(value)-len(secret); i++ {
		match := true
		for j, unit := range secret { if value[i+j] != unit { match = false; break } }
		if match { return true }
	}
	return false
}

type interval struct { start, end int }

// Match at every code-unit offset, including overlaps of a literal with itself.
// All literals' ranges are unioned before substituting, so no tail leaks.
func redactPass(value []uint16, secrets [][]uint16, marker []uint16) []uint16 {
	ranges := make([]interval, 0)
	for _, secret := range secrets {
		matches := make([]interval, 0)
		if len(secret) > len(value) { continue }
		// KMP retains overlapping occurrences without quadratic work for long
		// repeated literals (the registry permits 64 Ki UTF-16-unit secrets).
		prefix := make([]int, len(secret))
		for i, j := 1, 0; i < len(secret); i++ {
			for j > 0 && secret[i] != secret[j] { j = prefix[j-1] }
			if secret[i] == secret[j] { j++ }
			prefix[i] = j
		}
		for i, j := 0, 0; i < len(value); i++ {
			for j > 0 && value[i] != secret[j] { j = prefix[j-1] }
			if value[i] == secret[j] { j++ }
			if j != len(secret) { continue }
			start, end := i+1-len(secret), i+1
			if len(matches) > 0 && start <= matches[len(matches)-1].end { matches[len(matches)-1].end = end } else { matches = append(matches, interval{start, end}) }
			j = prefix[j-1]
		}
		if len(matches) == 0 { continue }
		merged := make([]interval, 0, len(ranges)+len(matches))
		for a, b := 0, 0; a < len(ranges) || b < len(matches); {
			var next interval
			if b >= len(matches) || (a < len(ranges) && ranges[a].start <= matches[b].start) { next = ranges[a]; a++ } else { next = matches[b]; b++ }
			last := len(merged)-1
			if last >= 0 && next.start <= merged[last].end { if next.end > merged[last].end { merged[last].end = next.end } } else { merged = append(merged, next) }
		}
		ranges = merged
	}
	if len(ranges) == 0 { return value }
	out := make([]uint16, 0, len(value))
	offset := 0
	for _, span := range ranges { out = append(out, value[offset:span.start]...); out = append(out, marker...); offset = span.end }
	return append(out, value[offset:]...)
}

type clone struct {
	secrets [][]uint16
	marker []uint16
	active map[identity]bool
	pointerDepth int
}
type identity struct { kind reflect.Kind; typ reflect.Type; pointer uintptr; length int }

func (c *clone) text(value string) string {
	if len(c.secrets) == 0 { return value }
	out := redactPass(stringUnits(value), c.secrets, c.marker)
	for {
		clean := redactPass(out, c.secrets, nil)
		if len(clean) == len(out) { return unitsString(out) }
		out = clean
	}
}

func unsafeKey(key string) bool { return key == "__proto__" || key == "prototype" || key == "constructor" }
func credentialKey(key string) bool {
	// JS toLowerCase expands U+0130 to i + combining dot; Go's simple fold
	// would incorrectly turn APİKEY into the credential name apikey.
	if strings.ContainsRune(key, '\u0130') { return false }
	switch strings.ToLower(key) { case "apikey", "password", "accesstoken", "secret": return true }
	return false
}

// Sanitize returns only detached JSON data; it never returns input maps, slices,
// pointers, RawMessages, marshalers, or handles, and never calls user methods.
// Unsupported Go values become nil (omitted in objects, null in arrays).
func (s *Sanitizer) Sanitize(value any) any {
	s.mu.RLock()
	secrets := append([][]uint16(nil), s.units...)
	s.mu.RUnlock()
	c := clone{secrets: secrets, marker: stringUnits(redacted), active: make(map[identity]bool)}
	for _, secret := range secrets { if containsUnits(c.marker, secret) { c.marker = nil; break } }
	out, _ := c.visit(reflect.ValueOf(value), 0)
	return out
}

var rawType = reflect.TypeOf(json.RawMessage{})
var numberType = reflect.TypeOf(json.Number(""))

func (c *clone) raw(value []byte, depth int) (any, bool) {
	if len(value) > MaxRawJSONBytes { return c.text(truncated), true }
	// Bound decoder nesting before allocating a tree. Braces in JSON strings do
	// not count; invalid syntax is rejected below without exposing source bytes.
	level, quoted, escaped := 0, false, false
	for _, b := range value {
		if quoted { if escaped { escaped = false } else if b == '\\' { escaped = true } else if b == '"' { quoted = false }; continue }
		if b == '"' { quoted = true } else if b == '{' || b == '[' { level++; if level > maxRawJSONDepth { return c.text(truncated), true } } else if b == '}' || b == ']' { level-- }
	}
	if !json.Valid(value) { return c.text(truncated), true }
	parser := rawParser{data: value}
	return c.rawVisit(parser.value(), depth)
}

func (c *clone) visit(v reflect.Value, depth int) (any, bool) {
	if !v.IsValid() { return nil, true }
	for v.Kind() == reflect.Interface { if v.IsNil() { return nil, true }; v = v.Elem() }
	if v.Type() == rawType { if v.IsNil() { return nil, true }; return c.raw(v.Bytes(), depth) }
	if v.Type() == numberType {
		number := v.Interface().(json.Number)
		if !json.Valid([]byte(number.String())) || len(number) == 0 || (number[0] != '-' && (number[0] < '0' || number[0] > '9')) { return nil, true }
		return number, true
	}
	switch v.Kind() {
	case reflect.String: return c.text(v.String()), true
	case reflect.Bool: return v.Bool(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64: return v.Int(), true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr: return v.Uint(), true
	case reflect.Float32, reflect.Float64:
		n := v.Float(); if math.IsInf(n, 0) || math.IsNaN(n) { return nil, true }; return n, true
	case reflect.Map, reflect.Slice, reflect.Pointer:
		if v.IsNil() { return nil, true }
	case reflect.Array, reflect.Struct:
	default: return nil, false
	}
	id := identity{kind: v.Kind(), typ: v.Type()}
	hasIdentity := v.Kind() == reflect.Map || v.Kind() == reflect.Slice || v.Kind() == reflect.Pointer
	if hasIdentity {
		if v.Kind() == reflect.Map { id.pointer = uintptr(v.UnsafePointer()) } else { id.pointer = v.Pointer() }
		if v.Kind() == reflect.Slice { id.length = v.Len() }
		if c.active[id] { return c.text(circular), true }
	}
	if depth > MaxDepth { return c.text(truncated), true }
	if hasIdentity { c.active[id] = true; defer delete(c.active, id) }
	switch v.Kind() {
	case reflect.Pointer:
		// Pointers are a Go representation detail, not a JSON nesting level.
		// Still bound pointer-only chains, which have no JS data equivalent.
		if c.pointerDepth >= MaxDepth+1 { return c.text(truncated), true }
		c.pointerDepth++
		defer func() { c.pointerDepth-- }()
		return c.visit(v.Elem(), depth)
	case reflect.Slice, reflect.Array:
		out := make([]any, v.Len())
		for i := 0; i < v.Len(); i++ { out[i], _ = c.visit(v.Index(i), depth+1) }
		return out, true
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String { return nil, false }
		out := make(map[string]any)
		keys := v.MapKeys()
		// Go maps have no insertion order. Stable lexical collisions are the only
		// representational exception to Object.defineProperty's last-key rule.
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		for _, key := range keys { c.put(out, key.String(), v.MapIndex(key), depth+1) }
		return out, true
	case reflect.Struct:
		return c.structure(v, depth), true
	}
	return nil, false
}

func emptyValue(v reflect.Value) bool {
	switch v.Kind() { case reflect.Array, reflect.Map, reflect.Slice, reflect.String: return v.Len() == 0; case reflect.Bool, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr, reflect.Float32, reflect.Float64, reflect.Interface, reflect.Pointer: return v.IsZero() }
	return false
}

func (c *clone) put(out map[string]any, key string, value reflect.Value, depth int) {
	if unsafeKey(key) { return }
	publicKey := c.text(key)
	if unsafeKey(publicKey) { return }
	if credentialKey(key) { out[publicKey] = ""; return }
	if item, ok := c.visit(value, depth); ok { out[publicKey] = item }
}

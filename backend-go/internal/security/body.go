package security

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// DefaultBodyLimit is the legacy eight-MiB default. Raw upload handlers must
// explicitly request their own 64-MiB limit, and apply aggregate quotas outside
// this package before persisting anything.
const DefaultBodyLimit int64 = 8 * 1024 * 1024

// ReadBody reads at most limit bytes plus one overflow-detection byte, returning
// 413 BODY_TOO_LARGE on overflow. Nonpositive limits select DefaultBodyLimit.
// It does not close Body (the HTTP server owns it), inspect Content-Type, or
// perform authentication/CSRF checks; the caller must do those first.
func ReadBody(r *http.Request, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = DefaultBodyLimit
	}
	if r.Body == nil {
		return []byte{}, nil
	}
	limited := &io.LimitedReader{R: r.Body, N: limit}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if limited.N == 0 {
		var overflow [1]byte
		n, extraErr := io.ReadFull(r.Body, overflow[:])
		if n > 0 {
			return nil, NewHTTPError(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE")
		}
		if extraErr != nil && extraErr != io.EOF {
			return nil, extraErr
		}
	}
	return data, nil
}

// JSONBody enforces exactly application/json (case-insensitive, allowing media
// parameters), a bounded body, and a single complete JSON value. Empty bodies,
// trailing documents, NaN/Infinity and syntax errors yield 400 INVALID_JSON;
// oversize bodies retain 413 BODY_TOO_LARGE, and other media types yield
// 415 JSON_REQUIRED. Like JSON.parse, it accepts top-level primitives and uses
// float64 numbers; operation handlers must separately ValidateArguments.
func JSONBody(r *http.Request, limit int64) (any, error) {
	media, ok := singleHeader(r, "Content-Type")
	if !ok || strings.ToLower(strings.TrimFunc(strings.SplitN(media, ";", 2)[0], jsWhitespace)) != "application/json" {
		return nil, NewHTTPError(http.StatusUnsupportedMediaType, "JSON_REQUIRED")
	}
	data, err := ReadBody(r, limit)
	if err != nil {
		var public *HTTPError
		if errors.As(err, &public) {
			return nil, err
		}
		return nil, NewHTTPError(http.StatusBadRequest, "INVALID_JSON")
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, NewHTTPError(http.StatusBadRequest, "INVALID_JSON")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, NewHTTPError(http.StatusBadRequest, "INVALID_JSON")
	}
	return jsNumbers(value), nil
}

// JSON.parse rounds numbers to IEEE-754, including overflow to Infinity. Keep
// that distinction so ValidateArguments returns INVALID_NUMBER, not INVALID_JSON.
func jsNumbers(value any) any {
	switch v := value.(type) {
	case json.Number:
		n, _ := strconv.ParseFloat(string(v), 64)
		return n
	case []any:
		for i, child := range v {
			v[i] = jsNumbers(child)
		}
	case map[string]any:
		for key, child := range v {
			v[key] = jsNumbers(child)
		}
	}
	return value
}

// ValidateArguments enforces the legacy RPC argument security policy: an array
// with at most 16 elements, at most 100000 total nodes (including the outer
// array), at most 32 edges deep, finite numbers, and no __proto__, prototype or
// constructor object keys at ANY depth. It consumes JSONBody-shaped values;
// unsupported Go-specific types fail closed. No argument object is mutated.
func ValidateArguments(args any) error {
	list, ok := args.([]any)
	if !ok || len(list) > 16 {
		return NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	nodes := 0
	var visit func(any, int) error
	visit = func(value any, depth int) error {
		nodes++
		if nodes > 100000 || depth > 32 {
			return NewHTTPError(http.StatusBadRequest, "ARGUMENT_COMPLEXITY")
		}
		switch v := value.(type) {
		case nil, bool, string:
			return nil
		case float64:
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return NewHTTPError(http.StatusBadRequest, "INVALID_NUMBER")
			}
		case float32:
			if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				return NewHTTPError(http.StatusBadRequest, "INVALID_NUMBER")
			}
		case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
			// Typed integral values produced inside the backend are finite.
		case []any:
			for _, child := range v {
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		case map[string]any:
			for key, child := range v {
				if key == "__proto__" || key == "prototype" || key == "constructor" {
					return NewHTTPError(http.StatusBadRequest, "INVALID_OBJECT_KEY")
				}
				if err := visit(child, depth+1); err != nil {
					return err
				}
			}
		default:
			return NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		return nil
	}
	return visit(list, 0)
}

// Package security implements the browser-facing HTTP security contract of
// web/security.mjs. It does not authenticate passwords or trust arbitrary peers:
// the HTTP listener MUST remain private to an authenticating reverse proxy which
// overwrites X-Novel-User (and X-Forwarded-Proto). Never expose it directly.
//
// The parent handler must authenticate every route except /healthz, obtain a
// session, and verify every non-GET /api/ request before consuming its body or
// performing effects. Bootstrap and downloads are session-bound GETs. SSE uses
// CheckEventOrigin, not the stricter mutation-origin policy. These deliberately
// separate helpers avoid imposing JSON on raw-byte upload routes.
package security

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"unicode/utf16"
)

// HTTPError carries the legacy HTTP status, public error code and message.
// Prefer NewHTTPError so that the default public message is the error code.
type HTTPError struct {
	Status  int
	Code    string
	Message string
}

// NewHTTPError constructs an error with the legacy default public message.
func NewHTTPError(status int, code string) *HTTPError {
	return &HTTPError{Status: status, Code: code, Message: code}
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Code
}

// ConstantEqual compares opaque UTF-8 strings using a timing-safe comparison
// when their byte lengths match, as crypto.timingSafeEqual does in the old code.
func ConstantEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// singleHeader rejects ambiguous repeated security headers. Header names are
// case insensitive on the net/http wire; requests constructed in-process should
// use Header.Set or Header.Add, as usual.
func singleHeader(r *http.Request, key string) (string, bool) {
	values := r.Header.Values(key)
	if len(values) != 1 {
		return "", false
	}
	return values[0], true
}

// Authenticate returns the EXACT trusted X-Novel-User value, not a trimmed or
// case-folded identity. It preserves the JavaScript limit of 256 UTF-16 units.
// Missing, blank, too-long or ambiguous identities fail closed with legacy 401.
// This is proxy-identity validation, NOT client-header authentication; see the
// package documentation for the mandatory proxy and listener trust boundary.
func Authenticate(r *http.Request) (string, error) {
	principal, ok := singleHeader(r, "X-Novel-User")
	if !ok || strings.TrimFunc(principal, jsWhitespace) == "" || len(utf16.Encode([]rune(principal))) > 256 {
		return "", NewHTTPError(http.StatusUnauthorized, "AUTHENTICATION_REQUIRED")
	}
	return principal, nil
}

// JavaScript String.trim whitespace (not Go's subtly different unicode set).
func jsWhitespace(r rune) bool {
	return r == '\u0009' || r == '\u000a' || r == '\u000b' || r == '\u000c' || r == '\u000d' ||
		r == '\u0020' || r == '\u00a0' || r == '\u1680' || (r >= '\u2000' && r <= '\u200a') ||
		r == '\u2028' || r == '\u2029' || r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
}

// Origins is an immutable-after-construction exact origin allowlist. No URL
// normalization, suffix matching, implicit Host trust, or wildcard is performed.
type Origins map[string]struct{}

// NewOrigins parses the same comma-separated, trimmed origin list as the Node
// server. An empty list denies all requests requiring an origin. The parent owns
// configuration/defaults; this package reads no environment or credentials.
func NewOrigins(csv string) Origins {
	origins := make(Origins)
	for _, value := range strings.Split(csv, ",") {
		value = strings.TrimFunc(value, jsWhitespace)
		if value != "" {
			origins[value] = struct{}{}
		}
	}
	return origins
}

// CheckOrigin enforces the mutation policy: an exact allowed Origin is required,
// then an exact Sec-Fetch-Site: cross-site is rejected, in that order. Absence of
// Sec-Fetch-Site is allowed for older browsers. Ambiguous headers fail closed.
func CheckOrigin(r *http.Request, allowed Origins) error {
	origin, ok := singleHeader(r, "Origin")
	if _, exists := allowed[origin]; !ok || !exists {
		return NewHTTPError(http.StatusForbidden, "ORIGIN_DENIED")
	}
	if values := r.Header.Values("Sec-Fetch-Site"); len(values) > 1 || (len(values) == 1 && values[0] == "cross-site") {
		return NewHTTPError(http.StatusForbidden, "CROSS_SITE_DENIED")
	}
	return nil
}

// CheckEventOrigin preserves the GET /api/events policy: no Origin (or an empty
// one) is permitted, but a present nonempty Origin must match the allowlist.
// Unlike mutations, the legacy SSE route does not check Sec-Fetch-Site.
func CheckEventOrigin(r *http.Request, allowed Origins) error {
	values := r.Header.Values("Origin")
	if len(values) == 0 || (len(values) == 1 && values[0] == "") {
		return nil
	}
	if len(values) == 1 {
		if _, ok := allowed[values[0]]; ok {
			return nil
		}
	}
	return NewHTTPError(http.StatusForbidden, "ORIGIN_DENIED")
}

// WriteJSON sends the legacy no-store JSON response headers. It marshals before
// committing headers so serialization failure cannot produce a partial success.
// The parent remains responsible for redacting secrets from public values.
func WriteJSON(w http.ResponseWriter, status int, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, err = w.Write(data)
	return err
}

// WriteError sends the compatible nested error envelope. Non-HTTP error messages
// are hidden; only legacy all-uppercase symbolic codes survive as error.code.
func WriteError(w http.ResponseWriter, err error) error {
	status := http.StatusInternalServerError
	code := "REQUEST_FAILED"
	message := "服务暂时无法完成此操作，请重试或查看服务器日志。"
	var public *HTTPError
	if errors.As(err, &public) && public != nil {
		status, code, message = public.Status, public.Code, public.Error()
	} else if err != nil && symbolicCode(err.Error()) {
		code = err.Error()
	}
	return WriteJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

// symbolicCode matches the legacy /^[A-Z][A-Z0-9_]+$/ errorCode fallback.
func symbolicCode(value string) bool {
	if len(value) < 2 || value[0] < 'A' || value[0] > 'Z' {
		return false
	}
	for _, c := range []byte(value[1:]) {
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '_' {
			return false
		}
	}
	return true
}

// ValidateClientID enforces the exact browser-tab token format. The parent must
// use the events ?client query parameter when present (even if empty), otherwise
// X-Client-Id; it must not silently fall back from a malformed query parameter.
func ValidateClientID(value string) error {
	if len(value) < 16 || len(value) > 128 {
		return NewHTTPError(http.StatusBadRequest, "CLIENT_ID_REQUIRED")
	}
	for _, c := range []byte(value) {
		if !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') && c != '_' && c != '-' {
			return NewHTTPError(http.StatusBadRequest, "CLIENT_ID_REQUIRED")
		}
	}
	return nil
}

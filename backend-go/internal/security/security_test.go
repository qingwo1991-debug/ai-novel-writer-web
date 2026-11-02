package security

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func request() *http.Request {
	r := httptest.NewRequest(http.MethodPost, "http://example.invalid/api/test", nil)
	r.Header.Set("X-Novel-User", "alice")
	r.Header.Set("Origin", "https://example.invalid")
	return r
}

func wantError(t *testing.T, err error, status int, code string) {
	t.Helper()
	var got *HTTPError
	if !errors.As(err, &got) || got.Status != status || got.Code != code {
		t.Fatalf("got error %v, want %d %s", err, status, code)
	}
}

func TestAuthenticate(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		valid       bool
	}{
		{"missing", "", false}, {"spaces", " \t\ufeff", false},
		{"exact identity retained", " alice ", true}, {"normal", "alice", true},
		{"limit", strings.Repeat("x", 256), true}, {"too long", strings.Repeat("x", 257), false},
		{"UTF16 limit", strings.Repeat("😀", 128), true}, {"UTF16 overflow", strings.Repeat("😀", 129), false},
		{"JS whitespace difference", "\u0085", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := request()
			r.Header.Set("X-Novel-User", tc.value)
			got, err := Authenticate(r)
			if tc.valid {
				if err != nil || got != tc.value {
					t.Fatalf("identity %q, error %v", got, err)
				}
			} else {
				wantError(t, err, 401, "AUTHENTICATION_REQUIRED")
			}
		})
	}
	r := request()
	r.Header.Add("X-Novel-User", "bob")
	_, err := Authenticate(r)
	wantError(t, err, 401, "AUTHENTICATION_REQUIRED")
}

func TestSessionCookieIdentityAndCSRF(t *testing.T) {
	store := NewSessions()
	r := request()
	r.Header.Set("X-Forwarded-Proto", "https")
	w := httptest.NewRecorder()
	alice, err := store.Get(r, w, "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(alice.ID()) != 64 || len(alice.CSRFToken()) != 64 || alice.ID() == alice.CSRFToken() || alice.Principal() != "alice" {
		t.Fatal("unexpected token/identity")
	}
	if got, want := w.Header().Get("Set-Cookie"), "novel_session="+alice.ID()+"; HttpOnly; SameSite=Strict; Path=/; Secure"; got != want {
		t.Fatalf("cookie %q, want %q", got, want)
	}
	r.Header.Set("Cookie", "other=a; novel_session="+alice.ID())
	w = httptest.NewRecorder()
	reused, err := store.Get(r, w, "alice")
	if err != nil || reused != alice || w.Header().Get("Set-Cookie") != "" {
		t.Fatalf("reuse: %v", err)
	}
	r.Header.Set("X-CSRF-Token", alice.CSRFToken())
	allowed := NewOrigins(" https://example.invalid, ,https://second.invalid ")
	if err := store.Verify(r, alice, allowed); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("X-CSRF-Token", "wrong")
	wantError(t, store.Verify(r, alice, allowed), 403, "CSRF_DENIED")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	wantError(t, store.Verify(r, alice, allowed), 403, "CROSS_SITE_DENIED")
	r.Header.Del("Origin")
	wantError(t, store.Verify(r, alice, allowed), 403, "ORIGIN_DENIED")
	r.Header.Set("Origin", "https://example.invalid")
	r.Header.Del("Sec-Fetch-Site")
	r.Header.Set("X-CSRF-Token", alice.CSRFToken())
	wantError(t, NewSessions().Verify(r, alice, allowed), 403, "CSRF_DENIED")
	wantError(t, store.Verify(r, nil, allowed), 403, "CSRF_DENIED")
	r.Header.Set("X-Novel-User", "bob")
	wantError(t, store.Verify(r, alice, allowed), 403, "CSRF_DENIED")
	bob, err := store.Get(r, httptest.NewRecorder(), "bob")
	if err != nil || bob == alice || bob.ID() == alice.ID() || bob.CSRFToken() == alice.CSRFToken() {
		t.Fatalf("principal binding failed: %v", err)
	}
	_, err = store.Get(r, httptest.NewRecorder(), "alice")
	wantError(t, err, 401, "AUTHENTICATION_REQUIRED")
}

func TestCookieParsingAndSecureExactness(t *testing.T) {
	for _, proto := range []string{"", "http", "HTTPS", "https,http", "https"} {
		s := NewSessions()
		r := request()
		r.Header.Set("X-Forwarded-Proto", proto)
		w := httptest.NewRecorder()
		session, err := s.Get(r, w, "alice")
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.HasSuffix(w.Header().Get("Set-Cookie"), "; Secure"); got != (proto == "https") {
			t.Fatalf("secure for %q: %v", proto, got)
		}
		// The FIRST cookie wins, including an unknown first cookie.
		r.Header.Set("Cookie", "novel_session=unknown; novel_session="+session.ID())
		next, err := s.Get(r, httptest.NewRecorder(), "alice")
		if err != nil || next == session {
			t.Fatalf("duplicate cookie order lost: %v", err)
		}
	}
}

func TestSessionExpiryAndPressureBoundaries(t *testing.T) {
	now := time.Unix(1700000000, 0)
	store := NewSessions()
	store.now = func() time.Time { return now }
	r := request()
	first, err := store.Get(r, httptest.NewRecorder(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Cookie", "novel_session="+first.ID())
	now = now.Add(SessionIdleTimeout)
	atBoundary, err := store.Get(r, httptest.NewRecorder(), "alice")
	if err != nil || atBoundary != first {
		t.Fatalf("24h boundary must reuse: %v", err)
	}
	now = now.Add(SessionIdleTimeout + time.Nanosecond)
	expired, err := store.Get(r, httptest.NewRecorder(), "alice")
	if err != nil || expired == first {
		t.Fatalf("24h+ boundary must rotate: %v", err)
	}
	store = NewSessions()
	store.now = func() time.Time { return now }
	r.Header.Del("Cookie")
	for i := 0; i < SessionLimit; i++ {
		if _, err := store.Get(r, httptest.NewRecorder(), "alice"); err != nil {
			t.Fatal(err)
		}
	}
	_, err = store.Get(r, httptest.NewRecorder(), "alice")
	wantError(t, err, 503, "SESSION_LIMIT")
	now = now.Add(time.Hour)
	_, err = store.Get(r, httptest.NewRecorder(), "alice")
	wantError(t, err, 503, "SESSION_LIMIT")
	now = now.Add(time.Nanosecond)
	if _, err = store.Get(r, httptest.NewRecorder(), "alice"); err != nil || len(store.sessions) != 1 {
		t.Fatalf("pressure prune failed: %v (%d)", err, len(store.sessions))
	}
}

func TestSessionConcurrentReuse(t *testing.T) {
	store := NewSessions()
	session, err := store.Get(request(), httptest.NewRecorder(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := request()
			r.Header.Set("Cookie", "novel_session="+session.ID())
			r.Header.Set("X-CSRF-Token", session.CSRFToken())
			got, err := store.Get(r, httptest.NewRecorder(), "alice")
			if err != nil || got != session {
				t.Errorf("concurrent session get: %v", err)
			}
			if err := store.Verify(r, got, NewOrigins("https://example.invalid")); err != nil {
				t.Errorf("concurrent verify: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestSessionConcurrentCreationCap(t *testing.T) {
	store := NewSessions()
	var wg sync.WaitGroup
	var countsMu sync.Mutex
	created, limited := 0, 0
	for i := 0; i < 300; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Get(request(), httptest.NewRecorder(), "alice")
			countsMu.Lock()
			defer countsMu.Unlock()
			if err == nil {
				created++
			} else {
				var public *HTTPError
				if !errors.As(err, &public) || public.Status != 503 || public.Code != "SESSION_LIMIT" {
					t.Errorf("unexpected cap error: %v", err)
				}
				limited++
			}
		}()
	}
	wg.Wait()
	if created != SessionLimit || limited != 300-SessionLimit || len(store.sessions) != SessionLimit {
		t.Fatalf("cap exceeded: created=%d limited=%d map=%d", created, limited, len(store.sessions))
	}
}

func TestSessionEntropyFailureDoesNotPublish(t *testing.T) {
	store := NewSessions()
	store.entropy = bytes.NewReader(make([]byte, 40)) // ID succeeds, CSRF fails.
	w := httptest.NewRecorder()
	if _, err := store.Get(request(), w, "alice"); err == nil {
		t.Fatal("entropy failure accepted")
	}
	if len(store.sessions) != 0 || w.Header().Get("Set-Cookie") != "" {
		t.Fatal("partially created session escaped")
	}
}

func TestOrigins(t *testing.T) {
	allowed := NewOrigins("https://example.invalid")
	for _, origin := range []string{"", "null", "https://example.invalid/", "https://EXAMPLE.invalid", "https://evil.invalid"} {
		r := request()
		r.Header.Set("Origin", origin)
		wantError(t, CheckOrigin(r, allowed), 403, "ORIGIN_DENIED")
	}
	r := request()
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	wantError(t, CheckOrigin(r, allowed), 403, "CROSS_SITE_DENIED")
	if err := CheckEventOrigin(r, allowed); err != nil {
		t.Fatalf("SSE must not acquire mutation fetch-site policy: %v", err)
	}
	r.Header.Del("Origin")
	if err := CheckEventOrigin(r, allowed); err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", "https://evil.invalid")
	wantError(t, CheckEventOrigin(r, allowed), 403, "ORIGIN_DENIED")
	r.Header.Set("Origin", "https://example.invalid")
	r.Header.Add("Origin", "https://example.invalid")
	wantError(t, CheckOrigin(r, allowed), 403, "ORIGIN_DENIED")
}

func TestBodiesAndJSON(t *testing.T) {
	for _, tc := range []struct {
		name, media, body, code string
		limit                  int64
		status                 int
	}{
		{"valid", "Application/JSON ; charset=utf-8", `{"args":["x",1,true,null]}`, "", 1024, 0},
		{"primitive", "application/json", `null`, "", 1024, 0},
		{"array", "application/json", `[]`, "", 2, 0},
		{"missing media", "", `{}`, "JSON_REQUIRED", 1024, 415},
		{"wrong media", "text/json", `{}`, "JSON_REQUIRED", 1024, 415},
		{"empty", "application/json", ``, "INVALID_JSON", 1024, 400},
		{"malformed", "application/json", `{`, "INVALID_JSON", 1024, 400},
		{"trailing", "application/json", `{} {}`, "INVALID_JSON", 1024, 400},
		{"not a JSON number", "application/json", `[NaN]`, "INVALID_JSON", 1024, 400},
		{"oversize", "application/json", `[] `, "BODY_TOO_LARGE", 2, 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.media)
			_, err := JSONBody(r, tc.limit)
			if tc.code != "" {
				wantError(t, err, tc.status, tc.code)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
	r := httptest.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader("[1e400]"))
	r.Header.Set("Content-Type", "application/json")
	value, err := JSONBody(r, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantError(t, ValidateArguments(value), 400, "INVALID_NUMBER")
	r = httptest.NewRequest(http.MethodPost, "http://example.invalid", strings.NewReader("raw"))
	data, err := ReadBody(r, 3)
	if err != nil || string(data) != "raw" {
		t.Fatalf("raw exact boundary: %q %v", data, err)
	}
	r.Body = nil
	data, err = ReadBody(r, 0)
	if err != nil || len(data) != 0 {
		t.Fatalf("nil body: %q %v", data, err)
	}
}

func TestValidateArguments(t *testing.T) {
	if err := ValidateArguments([]any{"x", float64(1), true, nil, map[string]any{"safe": []any{}}}); err != nil {
		t.Fatal(err)
	}
	wantError(t, ValidateArguments(nil), 400, "INVALID_ARGUMENTS")
	wantError(t, ValidateArguments(make([]any, 17)), 400, "INVALID_ARGUMENTS")
	for _, key := range []string{"__proto__", "prototype", "constructor"} {
		wantError(t, ValidateArguments([]any{map[string]any{"nested": map[string]any{key: nil}}}), 400, "INVALID_OBJECT_KEY")
	}
	for _, n := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		wantError(t, ValidateArguments([]any{n}), 400, "INVALID_NUMBER")
	}
	var nested any = nil
	for i := 0; i < 32; i++ {
		nested = []any{nested}
	}
	if err := ValidateArguments(nested); err != nil {
		t.Fatalf("depth32 rejected: %v", err)
	}
	wantError(t, ValidateArguments([]any{nested}), 400, "ARGUMENT_COMPLEXITY")
	if err := ValidateArguments([]any{make([]any, 99998)}); err != nil {
		t.Fatalf("100000 nodes rejected: %v", err)
	}
	wantError(t, ValidateArguments([]any{make([]any, 99999)}), 400, "ARGUMENT_COMPLEXITY")
}

func TestHTTPResponses(t *testing.T) {
	w := httptest.NewRecorder()
	if err := WriteJSON(w, 201, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	if w.Code != 201 || w.Header().Get("Content-Type") != "application/json; charset=utf-8" || w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("headers/status: %v %d", w.Header(), w.Code)
	}
	w = httptest.NewRecorder()
	if err := WriteError(w, NewHTTPError(403, "CSRF_DENIED")); err != nil {
		t.Fatal(err)
	}
	var envelope map[string]map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || w.Code != 403 || envelope["error"]["message"] != "CSRF_DENIED" || envelope["error"]["code"] != "CSRF_DENIED" {
		t.Fatalf("error envelope: %s (%v)", w.Body.String(), err)
	}
	w = httptest.NewRecorder()
	if err := WriteError(w, errors.New("private-credential-internal-path")); err != nil {
		t.Fatal(err)
	}
	if w.Code != 500 || strings.Contains(w.Body.String(), "private-credential") {
		t.Fatal("private error leaked")
	}
	w = httptest.NewRecorder()
	if err := WriteError(w, errors.New("WORKER_UNAVAILABLE")); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil || envelope["error"]["code"] != "WORKER_UNAVAILABLE" || envelope["error"]["message"] == "WORKER_UNAVAILABLE" {
		t.Fatalf("symbolic internal code contract: %s (%v)", w.Body.String(), err)
	}
}

func TestClientIDsAndConstantEqual(t *testing.T) {
	for _, good := range []string{strings.Repeat("a", 16), strings.Repeat("_", 128), "AZaz09-_abcdefghijkl"} {
		if err := ValidateClientID(good); err != nil {
			t.Fatal(err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 15), strings.Repeat("x", 129), "abcdefghijklmnop!", "abcdefghijklmnopé"} {
		wantError(t, ValidateClientID(bad), 400, "CLIENT_ID_REQUIRED")
	}
	if !ConstantEqual("", "") || !ConstantEqual("secret", "secret") || ConstantEqual("secret", "Secret") || ConstantEqual("secret", "secrets") {
		t.Fatal("constant comparison returned wrong equality")
	}
}

// The full handler example stays synthetic: no socket listener, auth credential,
// data mount, production endpoint or runtime dependency is involved.
func TestSyntheticProtectedHandler(t *testing.T) {
	store := NewSessions()
	allowed := NewOrigins("https://example.invalid")
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, err := Authenticate(r)
		if err != nil {
			_ = WriteError(w, err)
			return
		}
		session, err := store.Get(r, w, principal)
		if err == nil {
			err = store.Verify(r, session, allowed)
		}
		if err != nil {
			_ = WriteError(w, err)
			return
		}
		_ = WriteJSON(w, 200, map[string]bool{"ok": true})
	})
	r := request()
	bootstrap := httptest.NewRecorder()
	session, err := store.Get(r, bootstrap, "alice")
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Cookie", bootstrap.Result().Cookies()[0].String())
	r.Header.Set("X-CSRF-Token", session.CSRFToken())
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	r.Header.Set("X-Novel-User", "bob")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("old CSRF accepted under new identity: %d", w.Code)
	}
	if _, err := io.Copy(io.Discard, w.Result().Body); err != nil {
		t.Fatal(err)
	}
}

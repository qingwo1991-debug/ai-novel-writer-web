package transport

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

func fixture(t *testing.T) *Server {
	t.Helper()
	file, err := os.Open("../../../web/operations.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	registry, err := policy.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	server := New(registry, security.NewOrigins("http://synthetic.test"), "synthetic-generation")
	t.Cleanup(server.Close)
	return server
}
func TestAuthBootstrapAndHealth(t *testing.T) {
	server := fixture(t)
	for _, row := range []struct {
		path, principal string
		status          int
	}{{"/healthz", "", 200}, {"/api/bootstrap", "", 401}, {"/api/bootstrap", "synthetic-author", 200}, {"/", "", 401}} {
		request := httptest.NewRequest("GET", row.path, nil)
		if row.principal != "" {
			request.Header.Set("X-Novel-User", row.principal)
		}
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != row.status {
			t.Fatal(row, response.Code, response.Body.String())
		}
		if row.path == "/healthz" && !strings.Contains(response.Body.String(), `"runtime":"go"`) {
			t.Fatal(response.Body.String())
		}
		if row.path == "/api/bootstrap" && row.status == 200 {
			if !strings.Contains(response.Body.String(), `"runtime":"web"`) || len(response.Result().Cookies()) != 1 {
				t.Fatal(response.Body.String())
			}
		}
	}
}
func bootstrap(t *testing.T, server *Server) (*http.Cookie, string) {
	t.Helper()
	request := httptest.NewRequest("GET", "/api/bootstrap", nil)
	request.Header.Set("X-Novel-User", "synthetic-author")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != 200 {
		t.Fatal(response.Body.String())
	}
	var value struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return response.Result().Cookies()[0], value.CSRFToken
}
func rpc(t *testing.T, server *Server, cookie *http.Cookie, csrf, op, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest("POST", "/api/operations/"+op, strings.NewReader(body))
	request.Header.Set("X-Novel-User", "synthetic-author")
	request.Header.Set("X-Client-Id", "synthetic-tab-0001")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "http://synthetic.test")
	request.Header.Set("X-CSRF-Token", csrf)
	request.AddCookie(cookie)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	return response
}
func TestSecurityAndMigrationFailClosed(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	for _, row := range []struct {
		op, body string
		headers  map[string]string
		status   int
		code     string
	}{
		{"config:get", `{"args":[]}`, nil, 501, "GO_OPERATION_NOT_MIGRATED"},
		{"web:library-list", `{"args":[]}`, nil, 404, "OPERATION_NOT_EXPOSED"},
		{"config:get", `{"args":[]}`, map[string]string{"X-CSRF-Token": "invalid"}, 403, "CSRF_DENIED"},
		{"config:get", `{"args":[]}`, map[string]string{"Origin": "https://untrusted.invalid"}, 403, "ORIGIN_DENIED"},
		{"config:get", `{"args":[]}`, map[string]string{"Sec-Fetch-Site": "cross-site"}, 403, "CROSS_SITE_DENIED"},
		{"config:get", `{"args":{"unexpected":true}}`, nil, 400, "INVALID_ARGUMENTS"},
		{"config:get", `{"args":[{"__proto__":{}}]}`, nil, 400, "INVALID_OBJECT_KEY"},
		{"config:get", `not-json`, nil, 400, "INVALID_JSON"},
	} {
		response := rpc(t, server, cookie, csrf, row.op, row.body, row.headers)
		if response.Code != row.status || !strings.Contains(response.Body.String(), row.code) {
			t.Fatal(row, response.Code, response.Body.String())
		}
	}
}
func TestOperationEnvelopeAndNoCatchAllExposure(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	if err := server.Bind("web:library-list", func(context.Context, Caller, []any) (any, error) { return nil, nil }); err == nil {
		t.Fatal("internal handler exposed")
	}
	if err := server.Bind("config:get", func(_ context.Context, caller Caller, args []any) (any, error) {
		if caller.Principal != "synthetic-author" || caller.SessionID == "" || len(args) != 0 {
			t.Fatal(caller, args)
		}
		return map[string]any{"synthetic": true}, nil
	}); err != nil {
		t.Fatal(err)
	}
	response := rpc(t, server, cookie, csrf, "config:get", `{"args":[]}`, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), `"result":{"synthetic":true}`) {
		t.Fatal(response.Code, response.Body.String())
	}
}
func TestAuthenticatedSSEConnectAndCancel(t *testing.T) {
	server := fixture(t)
	testServer := httptest.NewServer(server)
	defer testServer.Close()
	request, _ := http.NewRequest("GET", testServer.URL+"/api/bootstrap", nil)
	request.Header.Set("X-Novel-User", "synthetic-author")
	response, err := testServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cookie := response.Cookies()[0]
	_ = response.Body.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ = http.NewRequestWithContext(ctx, "GET", testServer.URL+"/api/events?client=synthetic-tab-0001", nil)
	request.Header.Set("X-Novel-User", "synthetic-author")
	request.AddCookie(cookie)
	response, err = testServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(response.StatusCode)
	}
	first := make([]byte, len(": connected\n\n"))
	if _, err := io.ReadFull(response.Body, first); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, []byte(": connected\n\n")) {
		t.Fatal(string(first))
	}
	cancel()
}

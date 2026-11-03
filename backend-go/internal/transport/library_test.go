package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

type libraryInventoryProbe struct {
	rows  []projectfs.ListedProject
	err   error
	limit int
	calls int
}

func (probe *libraryInventoryProbe) ListCanonicalProjects(ctx context.Context, limit int) ([]projectfs.ListedProject, error) {
	probe.calls++
	probe.limit = limit
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return probe.rows, probe.err
}

func libraryRequest(route *LibraryRoute, method, path, principal, token string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if principal != "" {
		request.Header.Set("X-Novel-User", principal)
	}
	if token != "" {
		request.Header.Set("X-Client-Id", token)
	}
	response := httptest.NewRecorder()
	route.ServeHTTP(response, request)
	return response
}

func TestLibraryRouteOriginalChooserShapeIncludesInventoryRows(t *testing.T) {
	server := New(nil, security.NewOrigins(""), "synthetic")
	defer server.Close()
	probe := &libraryInventoryProbe{rows: []projectfs.ListedProject{
		{Name: "最近作品", Path: "/private/library/recent", ProjectID: "11111111-1111-4111-8111-111111111111", UpdatedAt: "2026-11-03T03:04:05.000Z"},
		{Name: "隐藏作品", Path: "/private/library/hidden", ProjectID: "22222222-2222-4222-8222-222222222222", UpdatedAt: "2026-11-02T03:04:05.000Z"},
	}}
	route, err := NewLibraryRoute(server, probe, 256)
	if err != nil {
		t.Fatal(err)
	}
	response := libraryRequest(route, http.MethodGet, "/api/library", "author", "client_1234567890")
	if response.Code != http.StatusOK || probe.calls != 1 || probe.limit != 256 {
		t.Fatalf("HTTP %d, probe %#v: %s", response.Code, probe, response.Body.String())
	}
	var body map[string][]map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	rows := body["projects"]
	if len(body) != 1 || len(rows) != 2 || rows[0]["name"] != "最近作品" || rows[1]["name"] != "隐藏作品" || rows[1]["path"] != "/private/library/hidden" {
		t.Fatalf("chooser shape changed: %#v", body)
	}
	for _, row := range rows {
		if len(row) != 4 || row["projectId"] == "" || row["updatedAt"] == "" {
			t.Fatalf("unsupported or absent fields: %#v", row)
		}
	}
	if len(response.Result().Cookies()) != 1 || !response.Result().Cookies()[0].HttpOnly {
		t.Fatal("shared authenticated session not established")
	}
}

func TestLibraryRouteRequiresProxyIdentityAndBrowserTab(t *testing.T) {
	server := New(nil, security.NewOrigins(""), "synthetic")
	defer server.Close()
	probe := &libraryInventoryProbe{}
	route, err := NewLibraryRoute(server, probe, 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, principal, token string
		status                         int
	}{
		{http.MethodGet, "/api/library", "", "client_1234567890", http.StatusUnauthorized},
		{http.MethodGet, "/api/library", "author", "", http.StatusBadRequest},
		{http.MethodGet, "/api/library", "author", "short", http.StatusBadRequest},
		{http.MethodPost, "/api/library", "author", "client_1234567890", http.StatusNotFound},
		{http.MethodGet, "/api/other", "author", "client_1234567890", http.StatusNotFound},
	} {
		response := libraryRequest(route, tc.method, tc.path, tc.principal, tc.token)
		if response.Code != tc.status || probe.calls != 0 {
			t.Fatalf("%s %s: HTTP %d, calls %d", tc.method, tc.path, response.Code, probe.calls)
		}
	}
}

func TestLibraryRouteDoesNotLeakInventoryFailureOrEmitPartialList(t *testing.T) {
	server := New(nil, security.NewOrigins(""), "synthetic")
	defer server.Close()
	probe := &libraryInventoryProbe{rows: []projectfs.ListedProject{{Name: "partial"}}, err: errors.New("private /author/library/secret.db failed")}
	route, err := NewLibraryRoute(server, probe, 256)
	if err != nil {
		t.Fatal(err)
	}
	response := libraryRequest(route, http.MethodGet, "/api/library", "author", "client_1234567890")
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "/author/library") || strings.Contains(response.Body.String(), "partial") {
		t.Fatalf("private or partial inventory escaped: HTTP %d %s", response.Code, response.Body.String())
	}
	var body map[string]map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || body["error"]["code"] != "REQUEST_FAILED" {
		t.Fatalf("wrong failure envelope: %v %#v", err, body)
	}
	probe.rows, probe.err = nil, nil
	response = libraryRequest(route, http.MethodGet, "/api/library", "author", "client_1234567890")
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"projects":[]`) {
		t.Fatalf("empty library changed shape: HTTP %d %s", response.Code, response.Body.String())
	}
}

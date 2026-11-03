package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
)

func migrationRegistry(t *testing.T) *policy.Registry {
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
	return registry
}

func TestMigrationGatewaySharesSessionAcrossStaticBootstrapAndLibrary(t *testing.T) {
	staticDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(staticDir, "index.html"), []byte("<html>synthetic Go migration</html>"), 0600); err != nil {
		t.Fatal(err)
	}
	libraryDir := t.TempDir()
	handler, cleanup, err := migrationHandler(migrationRegistry(t), "127.0.0.1:16082", staticDir, libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	request := func(path string, cookie *http.Cookie, tab string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("X-Novel-User", "synthetic-author")
		if cookie != nil {
			r.AddCookie(cookie)
		}
		if tab != "" {
			r.Header.Set("X-Client-Id", tab)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	page := request("/", nil, "")
	if page.Code != http.StatusOK || len(page.Result().Cookies()) != 1 || page.Body.String() != "<html>synthetic Go migration</html>" {
		t.Fatalf("static page: HTTP %d %s", page.Code, page.Body.String())
	}
	cookie := page.Result().Cookies()[0]
	bootstrap := request("/api/bootstrap", cookie, "")
	if bootstrap.Code != http.StatusOK || len(bootstrap.Result().Cookies()) != 0 {
		t.Fatalf("bootstrap did not reuse static session: HTTP %d %s", bootstrap.Code, bootstrap.Body.String())
	}
	library := request("/api/library", cookie, "client_1234567890")
	if library.Code != http.StatusOK || len(library.Result().Cookies()) != 0 {
		t.Fatalf("library did not reuse static session: HTTP %d %s", library.Code, library.Body.String())
	}
	var payload struct {
		Projects []any `json:"projects"`
	}
	if err := json.Unmarshal(library.Body.Bytes(), &payload); err != nil || payload.Projects == nil || len(payload.Projects) != 0 {
		t.Fatalf("empty synthetic library shape: %s, %v", library.Body.String(), err)
	}
	noIdentity := httptest.NewRecorder()
	handler.ServeHTTP(noIdentity, httptest.NewRequest(http.MethodGet, "/", nil))
	if noIdentity.Code != http.StatusUnauthorized {
		t.Fatalf("static route bypassed authentication: HTTP %d", noIdentity.Code)
	}
}

func TestMigrationGatewayKeepsLibraryUnavailableWithoutExplicitRoot(t *testing.T) {
	handler, cleanup, err := migrationHandler(migrationRegistry(t), "127.0.0.1:16082", "", "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	r := httptest.NewRequest(http.MethodGet, "/api/library", nil)
	r.Header.Set("X-Novel-User", "synthetic-author")
	r.Header.Set("X-Client-Id", "client_1234567890")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("unconfigured library became available: HTTP %d %s", w.Code, w.Body.String())
	}
}

func TestSyntheticLibraryFlagRequiresTemporaryChild(t *testing.T) {
	for _, path := range []string{"", "/", "/root/AI/ai-novel-writer-data", filepath.Join(os.TempDir(), "..", "author"), os.TempDir()} {
		if underTempRoot(path) {
			t.Fatalf("unsafe synthetic library accepted: %q", path)
		}
	}
	if !underTempRoot(t.TempDir()) {
		t.Fatal("disposable library rejected")
	}
}

func TestMigrationGatewayUploadStagingSharesCookieAndTab(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithUploads(migrationRegistry(t), "127.0.0.1:16082", "", "", root)
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	bootstrap := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrap.Header.Set("X-Novel-User", "synthetic-author")
	bootResponse := httptest.NewRecorder()
	handler.ServeHTTP(bootResponse, bootstrap)
	if bootResponse.Code != http.StatusOK || len(bootResponse.Result().Cookies()) != 1 {
		t.Fatal("bootstrap failed", bootResponse.Code, bootResponse.Body.String())
	}
	var boot struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(bootResponse.Body.Bytes(), &boot); err != nil || boot.CSRFToken == "" {
		t.Fatal("missing csrf", err)
	}
	cookie := bootResponse.Result().Cookies()[0]
	upload := func(csrf, tab string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/upload-folders", strings.NewReader(`{}`))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	valid := upload(boot.CSRFToken, "client_1234567890")
	if valid.Code != http.StatusCreated || !strings.Contains(valid.Body.String(), `"folderUploadId"`) || len(valid.Result().Cookies()) != 0 {
		t.Fatal("upload did not share bootstrap cookie and tab", valid.Code, valid.Body.String())
	}
	wrongCSRF := upload("wrong", "client_1234567890")
	if wrongCSRF.Code != http.StatusForbidden {
		t.Fatal("upload bypassed csrf", wrongCSRF.Code, wrongCSRF.Body.String())
	}
	invalidTab := upload(boot.CSRFToken, "short")
	if invalidTab.Code != http.StatusBadRequest {
		t.Fatal("upload bypassed tab admission", invalidTab.Code, invalidTab.Body.String())
	}
}

func TestMigrationGatewayUploadRequiresExplicitPrivateTempRoot(t *testing.T) {
	if _, _, err := migrationHandlerWithUploads(migrationRegistry(t), "127.0.0.1:16082", "", "", "/root/AI/ai-novel-writer-data"); err == nil {
		t.Fatal("author data accepted as synthetic upload root")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrationHandlerWithUploads(migrationRegistry(t), "127.0.0.1:16082", "", "", root); err == nil {
		t.Fatal("public upload staging root accepted")
	}
}

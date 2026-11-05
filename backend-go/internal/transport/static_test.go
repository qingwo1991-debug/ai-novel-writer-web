package transport

import (
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

func staticFixture(t *testing.T) (*StaticAssets, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "assets"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte(`<html><script>window.__go_test__ = 1</script><script src="/assets/app.js"></script></html>`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "assets", "app.js"), []byte("console.log('safe')"), 0600); err != nil {
		t.Fatal(err)
	}
	assets, err := NewStaticAssets(directory, security.NewSessions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = assets.Close() })
	return assets, directory
}

func staticRequest(assets *StaticAssets, method, path, principal string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, nil)
	if principal != "" {
		request.Header.Set("X-Novel-User", principal)
	}
	response := httptest.NewRecorder()
	assets.ServeHTTP(response, request)
	return response
}

func TestStaticAssetsAuthHeadersAndHead(t *testing.T) {
	assets, _ := staticFixture(t)
	unauthenticated := staticRequest(assets, http.MethodGet, "/", "")
	if unauthenticated.Code != http.StatusUnauthorized || !strings.Contains(unauthenticated.Body.String(), "AUTHENTICATION_REQUIRED") {
		t.Fatal(unauthenticated.Code, unauthenticated.Body.String())
	}
	index := staticRequest(assets, http.MethodGet, "/", "author")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "window.__go_test__") || index.Header().Get("Cache-Control") != "no-store" || index.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(index.Code, index.Header(), index.Body.String())
	}
	if len(index.Result().Cookies()) != 1 || !index.Result().Cookies()[0].HttpOnly {
		t.Fatal("static response did not establish the shared browser session")
	}
	digest := sha256.Sum256([]byte("window.__go_test__ = 1"))
	if !strings.Contains(index.Header().Get("Content-Security-Policy"), "'sha256-"+base64.StdEncoding.EncodeToString(digest[:])+"'") {
		t.Fatal("inline script hash absent", index.Header().Get("Content-Security-Policy"))
	}
	head := staticRequest(assets, http.MethodHead, "/assets/app.js", "author")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || head.Header().Get("Cache-Control") != "public, max-age=3600" || head.Header().Get("Content-Length") != "19" {
		t.Fatal(head.Code, head.Header(), head.Body.String())
	}
}

func TestStaticAssetsServesClientRoutesAndCachesBuildOutput(t *testing.T) {
	assets, directory := staticFixture(t)
	if err := os.WriteFile(filepath.Join(directory, "assets", "index-B2aHTjl4.js"), []byte("export default 1"), 0600); err != nil {
		t.Fatal(err)
	}
	root := staticRequest(assets, http.MethodGet, "/", "author")
	if root.Code != http.StatusOK || root.Header().Get("Content-Type") != "text/html; charset=utf-8" || root.Header().Get("Cache-Control") != "no-store" || !strings.Contains(root.Body.String(), "window.__go_test__") {
		t.Fatal(root.Code, root.Header(), root.Body.String())
	}
	for _, path := range []string{"/projects/7f2/chapter/3", "/projects/7f2/chapter/3/", "/settings"} {
		deep := staticRequest(assets, http.MethodGet, path, "author")
		if deep.Code != http.StatusOK || deep.Header().Get("Content-Type") != "text/html; charset=utf-8" || deep.Header().Get("Cache-Control") != "no-store" || deep.Body.String() != root.Body.String() {
			t.Fatalf("%s did not fall back to the app shell: HTTP %d %v %s", path, deep.Code, deep.Header(), deep.Body.String())
		}
	}
	head := staticRequest(assets, http.MethodHead, "/projects/7f2/chapter/3", "author")
	if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(head.Code, head.Header(), head.Body.String())
	}
	asset := staticRequest(assets, http.MethodGet, "/assets/index-B2aHTjl4.js", "author")
	if asset.Code != http.StatusOK || asset.Header().Get("Content-Type") != "text/javascript; charset=utf-8" || asset.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || asset.Body.String() != "export default 1" {
		t.Fatal(asset.Code, asset.Header(), asset.Body.String())
	}
	unhashed := staticRequest(assets, http.MethodGet, "/assets/app.js", "author")
	if unhashed.Code != http.StatusOK || unhashed.Header().Get("Cache-Control") != "public, max-age=3600" || unhashed.Body.String() != "console.log('safe')" {
		t.Fatal(unhashed.Code, unhashed.Header(), unhashed.Body.String())
	}
	if missing := staticRequest(assets, http.MethodGet, "/assets/missing.css", "author"); missing.Code != http.StatusNotFound || strings.Contains(missing.Body.String(), "window.__go_test__") {
		t.Fatal("missing build output was answered with the app shell:", missing.Code, missing.Body.String())
	}
	if unauthorized := staticRequest(assets, http.MethodGet, "/projects/7f2/chapter/3", ""); unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("client route bypassed authentication:", unauthorized.Code)
	}
}

func TestStaticAssetsRejectTraversalSymlinkAndUnknown(t *testing.T) {
	assets, directory := staticFixture(t)
	if err := os.Symlink(filepath.Join(directory, "index.html"), filepath.Join(directory, "assets", "alias.html")); err != nil {
		t.Fatal(err)
	}
	for _, sample := range []struct {
		path string
		code int
	}{
		{"/assets/../index.html", http.StatusBadRequest},
		{"/assets/%252e%252e/index.html", http.StatusBadRequest},
		{"/assets/alias.html", http.StatusForbidden},
		{"/assets/missing.js", http.StatusNotFound},
		{"/api/bootstrap", http.StatusNotFound},
	} {
		result := staticRequest(assets, http.MethodGet, sample.path, "author")
		if result.Code != sample.code || strings.Contains(result.Body.String(), directory) {
			t.Fatalf("%s: HTTP %d %s", sample.path, result.Code, result.Body.String())
		}
	}
	if result := staticRequest(assets, http.MethodPost, "/assets/app.js", "author"); result.Code != http.StatusMethodNotAllowed {
		t.Fatal(result.Code, result.Body.String())
	}
}

func TestStaticAssetsPinsBuildRootAcrossPathReplacement(t *testing.T) {
	assets, directory := staticFixture(t)
	old := directory + "-old"
	if err := os.Rename(directory, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "index.html"), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	response := staticRequest(assets, http.MethodGet, "/", "author")
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "replacement") || !strings.Contains(response.Body.String(), "window.__go_test__") {
		t.Fatal(response.Code, response.Body.String())
	}
}

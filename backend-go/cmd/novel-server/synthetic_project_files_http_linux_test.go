//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
)

// TestSyntheticDurableHTTPProjectFilesConfined drives the browser file surface
// of the durable lane over the real HTTP handler: the four confined fs:*
// operations and the two transfer routes.
//
// The confinement assertions are the point of this test. A permissive
// implementation here is a security defect, not a bug: a traversal, an absolute
// host path, a hidden storage path and a symlink are each attempted, and each
// must be REFUSED *and* must leave no file outside the project root. Refusal
// alone is not enough to assert, because a server that refused the response but
// wrote the bytes would pass it.
func TestSyntheticDurableHTTPProjectFilesConfined(t *testing.T) {
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original schema oracle required")
		}
		t.Skip("GitHub original schema oracle required")
	}
	contractFile, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	contract, parseErr := admission.ParseContract(contractFile)
	closeErr := contractFile.Close()
	if parseErr != nil || closeErr != nil {
		t.Fatal(parseErr, closeErr)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(root, "durable-projects")
	if err := os.Mkdir(libraryDir, 0700); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithDurable(migrationRegistry(t), "127.0.0.1:16082", "", libraryDir, syntheticDurableConfig{
		Schema: contract, Baseline: filepath.Join(filepath.Dir(oracle), syntheticBaselineDatabaseName),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	projectPath := filepath.Join(libraryDir, syntheticDurableProjectName)
	bootRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootRequest.Header.Set("X-Novel-User", "synthetic-author")
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, bootRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootWire struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootWire); err != nil || bootWire.CSRFToken == "" {
		t.Fatal(err, boot.Body.String())
	}
	cookie := boot.Result().Cookies()[0]
	const tab = "client_1234567890"
	const principal = "synthetic-author"
	const origin = "http://127.0.0.1:16082"
	call := func(operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", principal)
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
		request.Header.Set("Origin", origin)
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// result decodes the `{result: ...}` envelope and insists on HTTP 200: every
	// fs:* name this lane binds except fs:check-exists answers with a business
	// envelope, so a transport failure would be a real contract break.
	result := func(operation string, args []any) map[string]any {
		t.Helper()
		response := call(operation, args)
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("%s decode failed: %d %s %v", operation, response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	open := call("project:open", []any{projectPath, "project-files-open", nil})
	var openWire struct {
		Result struct {
			Success bool           `json:"success"`
			Project map[string]any `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(open.Body.Bytes(), &openWire); err != nil || open.Code != http.StatusOK || !openWire.Result.Success {
		t.Fatal("open failed", open.Code, open.Body.String(), err)
	}
	lease, _ := openWire.Result.Project["sessionLease"].(string)
	if lease == "" || openWire.Result.Project["path"] != projectPath {
		t.Fatal("open identity malformed", open.Body.String())
	}
	expected := map[string]any{"projectId": syntheticDurableProjectID, "leaseId": lease, "projectPath": projectPath}

	// ---- the confined subset works on a relative path -----------------------
	// fs:mkdir creates the browser directory under the project root, exactly
	// where a relative path says it is and nowhere else.
	if made := result("fs:mkdir", []any{"manuscript", projectPath, expected}); made["success"] != true {
		t.Fatalf("fs:mkdir refused a confined directory: %v", made)
	}
	if info, err := os.Stat(filepath.Join(projectPath, "manuscript")); err != nil || !info.IsDir() {
		t.Fatalf("fs:mkdir did not create the relative directory: %v", err)
	}
	const chapter = "第一章 合成正文"
	written := result("fs:write-file", []any{"manuscript/chapter-1.md", chapter, projectPath, expected})
	if written["success"] != true || written["commitState"] != "committed" {
		t.Fatalf("fs:write-file refused a confined write: %v", written)
	}
	chapterPath := filepath.Join(projectPath, "manuscript", "chapter-1.md")
	if raw, err := os.ReadFile(chapterPath); err != nil || string(raw) != chapter {
		t.Fatalf("fs:write-file did not commit the bytes: %q %v", raw, err)
	}
	// A write into a directory that does not exist yet creates the chain, like
	// the desktop fs:write-file that made the parent before writing.
	if nested := result("fs:write-file", []any{"manuscript/volume-1/chapter-2.md", "第二章", projectPath, expected}); nested["success"] != true {
		t.Fatalf("fs:write-file refused to create its parent: %v", nested)
	}
	read := result("fs:read-file", []any{"manuscript/chapter-1.md", projectPath, expected})
	if read["success"] != true || read["content"] != chapter {
		t.Fatalf("fs:read-file did not return the committed bytes: %v", read)
	}
	// fs:check-exists answers a bare boolean; a missing file is false, not an
	// error, because the desktop asked the same question about paths that need
	// not exist yet.
	exists := func(path string) bool {
		t.Helper()
		response := call("fs:check-exists", []any{path, projectPath, expected})
		var wire struct {
			Result bool `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("fs:check-exists decode failed: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	if !exists("manuscript/chapter-1.md") {
		t.Fatal("fs:check-exists missed a committed file")
	}
	if exists("manuscript/missing-chapter.md") {
		t.Fatal("fs:check-exists reported a file that does not exist")
	}
	// fs:list-dir is bound by BindEditorNavigation and still takes the desktop's
	// absolute project-root form; fs:read-json is pinned to the storage marker.
	// This slice does not rebind either, so both must still answer. The listing
	// runs before a symlink is planted below, because a visible symlink makes
	// the existing tree read fail closed by design.
	listing := call("fs:list-dir", []any{projectPath, projectPath, expected})
	if listing.Code != http.StatusOK {
		t.Fatalf("fs:list-dir no longer answers: %d %s", listing.Code, listing.Body.String())
	}
	if !strings.Contains(listing.Body.String(), "chapter-1.md") {
		t.Fatalf("fs:list-dir did not list the written chapter: %s", listing.Body.String())
	}

	// ---- confinement: traversal, absolute, hidden, symlink ------------------
	// Outside file the traversal and symlink attempts must never reach.
	outsidePath := filepath.Join(root, "outside-secret.txt")
	if err := os.WriteFile(outsidePath, []byte("sealed"), 0600); err != nil {
		t.Fatal(err)
	}
	refuse := func(label string, payload map[string]any, code string) {
		t.Helper()
		if payload["success"] != false {
			t.Fatalf("%s was not refused: %v", label, payload)
		}
		if payload["error"] != code {
			t.Fatalf("%s = %v, want %s", label, payload["error"], code)
		}
	}
	// (a) traversal out of the project root, read and write.
	refuse("traversal read", result("fs:read-file", []any{"../outside-secret.txt", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	refuse("nested traversal read", result("fs:read-file", []any{"manuscript/../../outside-secret.txt", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	refuse("traversal write", result("fs:write-file", []any{"../escaped-traversal.txt", "leak", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	if _, err := os.Stat(filepath.Join(root, "escaped-traversal.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused traversal write still created a file outside the project: %v", err)
	}
	// (b) absolute host paths. Even the project's OWN file, addressed
	// absolutely, is refused: the browser must name a relative path, so no page
	// can ever hand this server an absolute target and rely on one containment
	// comparison being right.
	refuse("absolute read", result("fs:read-file", []any{chapterPath, projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	absoluteTarget := filepath.Join(root, "escaped-absolute.txt")
	refuse("absolute write", result("fs:write-file", []any{absoluteTarget, "leak", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	if _, err := os.Stat(absoluteTarget); !os.IsNotExist(err) {
		t.Fatalf("a refused absolute write still created a host file: %v", err)
	}
	// (c) the .ai-novel storage directory (manifest and project.db) is not part
	// of the browser file surface at all.
	refuse("storage read", result("fs:read-file", []any{".ai-novel/project.json", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	refuse("storage write", result("fs:write-file", []any{".ai-novel/project.db", "leak", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	// (d) a symlink planted inside the project must not be followed out of it.
	linkPath := filepath.Join(projectPath, "escape-link")
	if err := os.Symlink(root, linkPath); err != nil {
		t.Fatal(err)
	}
	refuse("symlink escape read", result("fs:read-file", []any{"escape-link/outside-secret.txt", projectPath, expected}), "PROJECT_FILE_UNSAFE")
	// (e) mkdir cannot create a directory outside the project either.
	refuse("traversal mkdir", result("fs:mkdir", []any{"../escaped-dir", projectPath, expected}), "PROJECT_FILE_PATH_REFUSED")
	if _, err := os.Stat(filepath.Join(root, "escaped-dir")); !os.IsNotExist(err) {
		t.Fatalf("a refused mkdir still created a directory outside the project: %v", err)
	}
	// (f) fs:check-exists is the one channel that answers with a transport
	// error instead of a business envelope, exactly like the desktop handler
	// that threw instead of returning `{success:false}`.
	if escaped := call("fs:check-exists", []any{absoluteTarget, projectPath, expected}); escaped.Code != http.StatusBadRequest || !strings.Contains(escaped.Body.String(), "PROJECT_FILE_PATH_REFUSED") {
		t.Fatalf("fs:check-exists accepted an absolute path: %d %s", escaped.Code, escaped.Body.String())
	}
	// (g) the frozen session fence comes before any file work: a context for
	// another project root and a forged lease are both refused, and neither
	// writes anything.
	if changed := result("fs:write-file", []any{"manuscript/changed.md", "leak", filepath.Join(root, "other-project"), expected}); changed["success"] != false || changed["error"] != "PROJECT_CONTEXT_CHANGED" {
		t.Fatalf("fs:write-file accepted a foreign claimed root: %v", changed)
	}
	forged := map[string]any{"projectId": syntheticDurableProjectID, "leaseId": "forged-lease", "projectPath": projectPath}
	if stale := result("fs:write-file", []any{"manuscript/forged.md", "leak", projectPath, forged}); stale["success"] != false || stale["error"] != "PROJECT_SESSION_STALE" {
		t.Fatalf("fs:write-file accepted a forged lease: %v", stale)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "manuscript", "forged.md")); !os.IsNotExist(err) {
		t.Fatalf("a refused write still created a project file: %v", err)
	}

	// ---- upload and download routes ---------------------------------------
	upload := func(target, content string, csrf bool) *httptest.ResponseRecorder {
		t.Helper()
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		part, err := writer.CreateFormFile("file", "imported.txt")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
		if err := writer.Close(); err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/uploads?path="+url.QueryEscape(target), &body)
		request.Header.Set("X-Novel-User", principal)
		request.Header.Set("X-Client-Id", tab)
		if csrf {
			request.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
		}
		request.Header.Set("Origin", origin)
		request.Header.Set("Content-Type", writer.FormDataContentType())
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// A browser cannot open a native picker, so the file input uploads the bytes
	// to a confined project path and the server commits them there.
	imported := upload("manuscript/imported.txt", "导入的正文", true)
	var uploaded struct {
		Success bool   `json:"success"`
		Path    string `json:"path"`
		Name    string `json:"name"`
		Size    int    `json:"size"`
	}
	if err := json.Unmarshal(imported.Body.Bytes(), &uploaded); err != nil || imported.Code != http.StatusCreated ||
		!uploaded.Success || uploaded.Path != "manuscript/imported.txt" || uploaded.Name != "imported.txt" || uploaded.Size != len("导入的正文") {
		t.Fatalf("upload refused a confined path: %d %s %v", imported.Code, imported.Body.String(), err)
	}
	if raw, err := os.ReadFile(filepath.Join(projectPath, "manuscript", "imported.txt")); err != nil || string(raw) != "导入的正文" {
		t.Fatalf("upload did not commit the bytes into the project: %q %v", raw, err)
	}
	if raw := result("fs:read-file", []any{"manuscript/imported.txt", projectPath, expected}); raw["content"] != "导入的正文" {
		t.Fatalf("an uploaded file is not readable through fs:read-file: %v", raw)
	}
	// The upload destination is confined exactly like the operation path.
	if escaped := upload(filepath.Join(root, "escaped-upload.txt"), "leak", true); escaped.Code != http.StatusBadRequest || !strings.Contains(escaped.Body.String(), "PROJECT_FILE_PATH_REFUSED") {
		t.Fatalf("upload accepted an absolute destination: %d %s", escaped.Code, escaped.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "escaped-upload.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused upload still created a host file: %v", err)
	}
	if escaped := upload("../escaped-upload.txt", "leak", true); escaped.Code != http.StatusBadRequest {
		t.Fatalf("upload accepted a traversing destination: %d %s", escaped.Code, escaped.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "escaped-upload.txt")); !os.IsNotExist(err) {
		t.Fatalf("a refused upload still created a host file: %v", err)
	}
	// An upload mutates the project, so it carries the gateway's CSRF check.
	if denied := upload("manuscript/no-csrf.txt", "leak", false); denied.Code != http.StatusForbidden {
		t.Fatalf("upload accepted a request without a CSRF token: %d %s", denied.Code, denied.Body.String())
	}
	if _, err := os.Stat(filepath.Join(projectPath, "manuscript", "no-csrf.txt")); !os.IsNotExist(err) {
		t.Fatalf("a CSRF-less upload still wrote a project file: %v", err)
	}
	download := func(target string) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, "/api/downloads/"+target, nil)
		request.Header.Set("X-Novel-User", principal)
		request.Header.Set("X-Client-Id", tab)
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	// A browser cannot save to a native location either: the download route
	// streams the confined project file to the browser's own save mechanism.
	saved := download("manuscript/chapter-1.md")
	if saved.Code != http.StatusOK || saved.Body.String() != chapter {
		t.Fatalf("download did not stream the project file: %d %q", saved.Code, saved.Body.String())
	}
	if saved.Header().Get("Cache-Control") != "no-store" || !strings.Contains(saved.Header().Get("Content-Disposition"), "attachment") {
		t.Fatalf("download headers are not a browser attachment: %v", saved.Header())
	}
	if missing := download("manuscript/no-such-chapter.md"); missing.Code != http.StatusNotFound {
		t.Fatalf("download of a missing file = %d %s, want 404", missing.Code, missing.Body.String())
	}
	// The download path is confined like every other browser path.
	if hidden := download(".ai-novel/project.json"); hidden.Code != http.StatusBadRequest || !strings.Contains(hidden.Body.String(), "PROJECT_FILE_PATH_REFUSED") {
		t.Fatalf("download served the project storage: %d %s", hidden.Code, hidden.Body.String())
	}
	// net/http's mux cleans a literal "..", so the encoded form is what reaches
	// the handler; either way the outside bytes must not be served.
	if escaped := download("%2e%2e/outside-secret.txt"); escaped.Code == http.StatusOK || strings.Contains(escaped.Body.String(), "sealed") {
		t.Fatalf("download served a file outside the project: %d %s", escaped.Code, escaped.Body.String())
	}

	// ---- names this slice deliberately leaves at 501 ------------------------
	// fs:write-json has no bound read counterpart on this lane (fs:read-json is
	// pinned to the storage marker), and the three fs:grant-* names belong to a
	// server-issued external grant model a browser does not have. Every
	// dialog:* picker stays 501 because its consumer is not migrated.
	for _, operation := range []string{
		"fs:write-json",
		"fs:grant-read-file",
		"fs:grant-write-file",
		"fs:grant-mkdir",
		"dialog:select-legacy-project",
		"dialog:select-folder",
		"dialog:select-export-directory",
		"dialog:select-project-archive-export",
		"dialog:select-project-archive",
		"dialog:select-project-restore-target",
		"dialog:select-knowledge-files",
		"dialog:select-knowledge-folder",
		"dialog:select-novel-files",
	} {
		response := call(operation, []any{})
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s was bound on the durable lane: %d %s", operation, response.Code, response.Body.String())
		}
	}
	// The folder-upload route is not part of this slice: it keeps the gateway's
	// explicit "route not migrated" refusal rather than a made-up success.
	folders := httptest.NewRequest(http.MethodPost, "/api/upload-folders", strings.NewReader("{}"))
	folders.Header.Set("X-Novel-User", principal)
	folders.Header.Set("X-Client-Id", tab)
	folders.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
	folders.Header.Set("Origin", origin)
	folders.Header.Set("Content-Type", "application/json")
	folders.AddCookie(cookie)
	folderResponse := httptest.NewRecorder()
	handler.ServeHTTP(folderResponse, folders)
	if folderResponse.Code != http.StatusNotImplemented || !strings.Contains(folderResponse.Body.String(), "GO_ROUTE_NOT_MIGRATED") {
		t.Fatalf("/api/upload-folders was served: %d %s", folderResponse.Code, folderResponse.Body.String())
	}
}

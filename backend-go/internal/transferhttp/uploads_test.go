package transferhttp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

const uploadTestClient = "browser-tab-test-1234"

type testBrowser struct {
	principal string
	cookie    string
	csrf      string
	client    string
}

func uploadStore(t *testing.T) (*TransferStore, string, *security.Sessions) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	sessions := security.NewSessions()
	store, err := NewTransferStore(directory, sessions, security.NewOrigins("https://trusted.example"), func(w http.ResponseWriter, r *http.Request) (*security.Session, string, error) {
		principal, err := security.Authenticate(r)
		if err != nil {
			return nil, "", err
		}
		session, err := sessions.Get(r, w, principal)
		return session, r.Header.Get("X-Client-ID"), err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, directory, sessions
}

func browser(t *testing.T, sessions *security.Sessions, principal string) testBrowser {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	req.Header.Set("X-Novel-User", principal)
	w := httptest.NewRecorder()
	session, err := sessions.Get(req, w, principal)
	if err != nil {
		t.Fatal(err)
	}
	return testBrowser{principal: principal, cookie: w.Result().Cookies()[0].String(), csrf: session.CSRFToken(), client: uploadTestClient}
}

func send(t *testing.T, store *TransferStore, user testBrowser, route, body string, extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
	req.Header.Set("X-Novel-User", user.principal)
	req.Header.Set("Cookie", user.cookie)
	req.Header.Set("X-Client-ID", user.client)
	req.Header.Set("Origin", "https://trusted.example")
	req.Header.Set("X-CSRF-Token", user.csrf)
	for key, value := range extra {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	store.ServeHTTP(w, req)
	return w
}

func responseID(t *testing.T, w *httptest.ResponseRecorder, key string) string {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var value map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	id, ok := value[key].(string)
	if !ok || id == "" {
		t.Fatal("missing upload capability", value)
	}
	return id
}

func responseError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var value struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	if w.Code != status || value.Error.Code != code {
		t.Fatalf("HTTP %d %s, want %d %s", w.Code, value.Error.Code, status, code)
	}
}

func TestOwnedFolderUploadStaysPrivateAndReadableByOwner(t *testing.T) {
	store, root, sessions := uploadStore(t)
	owner := browser(t, sessions, "author")
	foreign := browser(t, sessions, "other-author")
	folder := responseID(t, send(t, store, owner, "/api/upload-folders", `{}`, map[string]string{"Content-Type": "application/json"}), "folderUploadId")
	content := "Synthetic isolated upload; no author data."
	upload := func(user testBrowser, relative string) *httptest.ResponseRecorder {
		return send(t, store, user, "/api/uploads", content, map[string]string{
			"Content-Type": "application/octet-stream", "X-Upload-Name": "boundary.txt", "X-Upload-Folder": folder,
			"X-Upload-Relative": relative,
		})
	}
	responseError(t, upload(foreign, "foreign.txt"), http.StatusForbidden, "UPLOAD_FOLDER_NOT_OWNED")
	for _, relative := range []string{"../escape.txt", "/tmp/escape.txt", `nested\escape.txt`, "nested/./escape.txt", "bad%00.txt"} {
		responseError(t, upload(owner, relative), http.StatusBadRequest, "INVALID_PATH")
	}
	id := responseID(t, upload(owner, "nested/%E5%90%88%E6%B3%95%E6%96%87%E4%BB%B6.txt"), "uploadId")
	responseError(t, upload(owner, "nested/%E5%90%88%E6%B3%95%E6%96%87%E4%BB%B6.txt"), http.StatusBadRequest, "UPLOAD_PATH_EXISTS")
	file, err := store.OpenUploadFile(sessionsFor(t, sessions, owner), owner.client, id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if string(raw) != content {
		t.Fatal("staged content changed")
	}
	if _, err := store.OpenUploadFile(sessionsFor(t, sessions, foreign), foreign.client, id); err == nil {
		t.Fatal("foreign browser opened staged file")
	}
	info, err := os.Stat(filepath.Join(root, "uploads", folder, "nested", "合法文件.txt"))
	if err != nil || info.Mode().Perm()&0077 != 0 {
		t.Fatal("staging not private", err)
	}
	nested := filepath.Join(root, "uploads", folder, "nested")
	if err := os.Rename(nested, nested+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(nested+"-old", nested); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenUploadFile(sessionsFor(t, sessions, owner), owner.client, id); err == nil {
		t.Fatal("replaced intermediate directory admitted")
	}
}

func sessionsFor(t *testing.T, sessions *security.Sessions, user testBrowser) *security.Session {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	req.Header.Set("X-Novel-User", user.principal)
	req.Header.Set("Cookie", user.cookie)
	session, err := sessions.Get(req, httptest.NewRecorder(), user.principal)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func TestUploadRequiresCSRFAndRejectsSymlinkAndExpiredFolder(t *testing.T) {
	store, root, sessions := uploadStore(t)
	owner := browser(t, sessions, "author")
	folder := responseID(t, send(t, store, owner, "/api/upload-folders", `{}`, map[string]string{"Content-Type": "application/json"}), "folderUploadId")
	withoutCSRF := owner
	withoutCSRF.csrf = "wrong-token"
	responseError(t, send(t, store, withoutCSRF, "/api/uploads", "data", map[string]string{"X-Upload-Folder": folder}), http.StatusForbidden, "CSRF_DENIED")
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "uploads", folder, "linked")); err != nil {
		t.Fatal(err)
	}
	responseError(t, send(t, store, owner, "/api/uploads", "data", map[string]string{"X-Upload-Folder": folder, "X-Upload-Relative": "linked/file.txt"}), http.StatusForbidden, "SYMLINK_DENIED")
	store.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	responseError(t, send(t, store, owner, "/api/uploads", "data", map[string]string{"X-Upload-Folder": folder}), http.StatusForbidden, "UPLOAD_FOLDER_NOT_OWNED")
}

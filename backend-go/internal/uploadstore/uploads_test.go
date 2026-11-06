package uploadstore

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// These tests own the two rules a consumer cannot observe from outside: the
// lifetime and the disk guard. Every case drives the real handler over real files,
// because the rule that matters is "the bytes are gone", not "the map was re-keyed".

const (
	storeTestClient = "browser-tab-store-0001"
	storeTestElse   = "browser-tab-store-0002"
	storeTestOrigin = "https://trusted.example"
)

type storeTestBrowser struct {
	cookie    string
	csrf      string
	sessionID string
	client    string
}

func storeTestLimits() Limits {
	limits := DefaultLimits()
	limits.MaxFileBytes = 64
	limits.MaxCallerBytes = 512
	limits.MaxFolderBytes = 256
	limits.MaxTotalBytes = 1024
	limits.MaxEntries = 64
	return limits
}

func storeTestNew(t *testing.T, limits Limits) (*Store, string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	sessions := security.NewSessions()
	store, err := New(directory, sessions, security.NewOrigins(storeTestOrigin),
		func(w http.ResponseWriter, r *http.Request) (*security.Session, string, error) {
			principal, authErr := security.Authenticate(r)
			if authErr != nil {
				return nil, "", authErr
			}
			session, sessionErr := sessions.Get(r, w, principal)
			return session, r.Header.Get("X-Client-Id"), sessionErr
		}, limits)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store, directory
}

// storeTestMintBrowser mints one cookie session, the way /api/bootstrap does, and binds
// a tab to it.
func storeTestMintBrowser(t *testing.T, store *Store, cookie, client string) storeTestBrowser {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	request.Header.Set("X-Novel-User", "author")
	if cookie != "" {
		request.Header.Set("Cookie", cookie)
	}
	recorder := httptest.NewRecorder()
	session, err := store.sessions.Get(request, recorder, "author")
	if err != nil {
		t.Fatal(err)
	}
	token := cookie
	if token == "" {
		cookies := recorder.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatal("no session cookie was minted")
		}
		token = cookies[0].String()
	}
	return storeTestBrowser{cookie: token, csrf: session.CSRFToken(), sessionID: session.ID(), client: client}
}

func storeTestSend(t *testing.T, store *Store, browser storeTestBrowser, route, name, body string,
	extra map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, route, strings.NewReader(body))
	request.Header.Set("X-Novel-User", "author")
	request.Header.Set("Cookie", browser.cookie)
	request.Header.Set("X-Client-Id", browser.client)
	request.Header.Set("Origin", storeTestOrigin)
	request.Header.Set("X-CSRF-Token", browser.csrf)
	request.Header.Set("Content-Type", "application/octet-stream")
	if name != "" {
		request.Header.Set("X-Upload-Name", name)
	}
	for key, value := range extra {
		request.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	store.ServeHTTP(recorder, request)
	return recorder
}

func storeTestID(t *testing.T, recorder *httptest.ResponseRecorder, key string) string {
	t.Helper()
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload refused: %d %s", recorder.Code, recorder.Body.String())
	}
	var answer map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatal(err)
	}
	id, ok := answer[key].(string)
	if !ok || id == "" {
		t.Fatalf("no %s in the answer: %s", key, recorder.Body.String())
	}
	return id
}

func storeTestRefused(t *testing.T, recorder *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var answer struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &answer); err != nil {
		t.Fatalf("unreadable refusal: %v %s", err, recorder.Body.String())
	}
	if recorder.Code != status || answer.Error.Code != code {
		t.Fatalf("got %d %s, want %d %s", recorder.Code, answer.Error.Code, status, code)
	}
}

// storeTestBlobs lists what is actually on disk, so "a refused upload wrote
// nothing" is a comparison against an empty directory rather than a hope.
func storeTestBlobs(t *testing.T, directory string) []string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

func TestStagedUploadExpiresAndReclaimsItsBytes(t *testing.T) {
	limits := storeTestLimits()
	limits.Lifetime = 20 * time.Millisecond
	store, directory := storeTestNew(t, limits)
	owner := storeTestMintBrowser(t, store, "", storeTestClient)
	id := storeTestID(t, storeTestSend(t, store, owner, RouteFile, "notes.txt", "hello", nil), "uploadId")
	name, data, err := store.Read(owner.sessionID, owner.client, id)
	if err != nil || name != "notes.txt" || string(data) != "hello" {
		t.Fatalf("a fresh upload is not readable: %v %s %s", err, name, data)
	}
	time.Sleep(80 * time.Millisecond)
	if _, _, err := store.Read(owner.sessionID, owner.client, id); err == nil {
		t.Fatal("an expired upload is still readable")
	}
	if names := storeTestBlobs(t, directory); len(names) != 0 {
		t.Fatalf("expiry left blobs behind: %v", names)
	}
}

func TestOldestStagingIsEvictedBeforeTheDiskGuardRefuses(t *testing.T) {
	limits := storeTestLimits()
	limits.MaxFileBytes = 10
	limits.MaxTotalBytes = 10
	store, directory := storeTestNew(t, limits)
	owner := storeTestMintBrowser(t, store, "", storeTestClient)
	first := storeTestID(t, storeTestSend(t, store, owner, RouteFile, "a.txt", "aaaa", nil), "uploadId")
	second := storeTestID(t, storeTestSend(t, store, owner, RouteFile, "b.txt", "bbbb", nil), "uploadId")
	if _, _, err := store.Read(owner.sessionID, owner.client, first); err != nil {
		t.Fatalf("a live upload is not readable: %v", err)
	}
	third := storeTestID(t, storeTestSend(t, store, owner, RouteFile, "c.txt", "cccc", nil), "uploadId")
	if _, _, err := store.Read(owner.sessionID, owner.client, first); err == nil {
		t.Fatal("the oldest upload survived the disk guard")
	}
	for _, id := range []string{second, third} {
		if _, _, err := store.Read(owner.sessionID, owner.client, id); err != nil {
			t.Fatalf("a live upload was evicted: %v", err)
		}
	}
	if names := storeTestBlobs(t, directory); len(names) != 2 {
		t.Fatalf("staging holds %d blobs, want 2: %v", len(names), names)
	}
}

func TestOversizeAndUnsafeMembersAreRefusedBeforeAnyWrite(t *testing.T) {
	limits := storeTestLimits()
	limits.MaxFileBytes = 8
	store, directory := storeTestNew(t, limits)
	owner := storeTestMintBrowser(t, store, "", storeTestClient)
	storeTestRefused(t, storeTestSend(t, store, owner, RouteFile, "big.txt", strings.Repeat("x", 32), nil),
		http.StatusRequestEntityTooLarge, codeBodyTooLarge)
	if names := storeTestBlobs(t, directory); len(names) != 0 {
		t.Fatalf("a refused oversize upload left blobs: %v", names)
	}
	folder := storeTestID(t, storeTestSend(t, store, owner, RouteFolder, "", "{}",
		map[string]string{"Content-Type": "application/json"}), "folderUploadId")
	for _, relative := range []string{"../escape.txt", "/absolute.txt", "a//b.txt", "a/b/c/d/e/f/g/h/i.txt"} {
		storeTestRefused(t, storeTestSend(t, store, owner, RouteFile, "member.txt", "ok",
			map[string]string{"X-Upload-Folder": folder, "X-Upload-Relative": relative}),
			http.StatusBadRequest, codeInvalidPath)
	}
	if names := storeTestBlobs(t, directory); len(names) != 0 {
		t.Fatalf("a refused member left blobs: %v", names)
	}
	name, members, err := store.ReadFolder(owner.sessionID, owner.client, folder)
	if err != nil || len(members) != 0 || name == "" {
		t.Fatalf("the staging folder became unreadable: %v %s", err, name)
	}
}

func TestAnotherTabCannotReadStagedBytes(t *testing.T) {
	store, _ := storeTestNew(t, storeTestLimits())
	owner := storeTestMintBrowser(t, store, "", storeTestClient)
	// The same cookie session, a different tab: the tab is half the staging key.
	other := storeTestMintBrowser(t, store, owner.cookie, storeTestElse)
	if other.sessionID != owner.sessionID {
		t.Fatal("the second tab did not share the cookie session")
	}
	id := storeTestID(t, storeTestSend(t, store, owner, RouteFile, "notes.txt", "secret", nil), "uploadId")
	if _, _, err := store.Read(other.sessionID, other.client, id); err == nil {
		t.Fatal("another tab read staged bytes")
	}
	// A different session cannot address it either, and a wrong Origin cannot stage.
	stranger := storeTestMintBrowser(t, store, "", storeTestElse)
	if _, _, err := store.Read(stranger.sessionID, stranger.client, id); err == nil {
		t.Fatal("another session read staged bytes")
	}
	storeTestRefused(t, storeTestSend(t, store, owner, RouteFile, "notes.txt", "x",
		map[string]string{"Origin": "https://untrusted.example"}), http.StatusForbidden, "ORIGIN_DENIED")
}

package transport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// Only synthetic, manually adopted repositories. This is deliberately NOT a
// fake project:open route or evidence that browser project admission is ported.
const transportDraftSchema = `
CREATE TABLE contents (id INTEGER PRIMARY KEY, body TEXT NOT NULL);
CREATE TABLE drafts (
 id INTEGER PRIMARY KEY, chapter_number INTEGER NOT NULL, version INTEGER NOT NULL,
 status TEXT NOT NULL, source TEXT NOT NULL DEFAULT 'write', content_id INTEGER NOT NULL,
 word_count INTEGER NOT NULL DEFAULT 11, source_dependencies TEXT NOT NULL DEFAULT '[]',
 created_at TEXT NOT NULL DEFAULT 'created-original',
 updated_at TEXT NOT NULL DEFAULT 'updated-original'
);
CREATE TABLE finalization_outbox (
 finalization_id TEXT PRIMARY KEY, draft_id INTEGER NOT NULL UNIQUE,
 chapter_number INTEGER NOT NULL, chapter_title TEXT NOT NULL DEFAULT '',
 content_hash TEXT NOT NULL, content_snapshot TEXT
);`

type adoptedDraftProject struct {
	expected map[string]any
	drafts   *store.Drafts
}

type adoptedDraftProvider struct {
	mu       sync.RWMutex
	projects map[Caller]adoptedDraftProject
	begins   atomic.Int64
	releases atomic.Int64
}

func newAdoptedDraftProvider() *adoptedDraftProvider {
	return &adoptedDraftProvider{projects: make(map[Caller]adoptedDraftProject)}
}

func (p *adoptedDraftProvider) adopt(caller Caller, expected map[string]any, drafts *store.Drafts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.projects[caller] = adoptedDraftProject{expected: expected, drafts: drafts}
}

func (p *adoptedDraftProvider) BeginDrafts(ctx context.Context, caller Caller, raw any) (*store.Drafts, func(), error) {
	p.begins.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	p.mu.RLock()
	project, ok := p.projects[caller]
	if !ok {
		p.mu.RUnlock()
		return nil, nil, security.NewHTTPError(409, "PROJECT_SESSION_STALE")
	}
	expected, ok := raw.(map[string]any)
	if !ok || expected["projectId"] != project.expected["projectId"] || expected["leaseId"] != project.expected["leaseId"] {
		p.mu.RUnlock()
		return nil, nil, security.NewHTTPError(409, "PROJECT_SESSION_STALE")
	}
	if expected["projectPath"] != project.expected["projectPath"] {
		p.mu.RUnlock()
		return nil, nil, security.NewHTTPError(409, "PROJECT_SESSION_ROOT_MISMATCH")
	}
	return project.drafts, func() { p.releases.Add(1); p.mu.RUnlock() }, nil
}

func transportDraftDB(t *testing.T) (*sql.DB, *store.Drafts) {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.Join(t.TempDir(), "synthetic #? 中文.sqlite")}
	uri.RawQuery = "mode=rwc&_busy_timeout=5000"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	t.Cleanup(func() { _ = db.Close() })
	draftExec(t, db, transportDraftSchema)
	return db, store.NewDrafts(db)
}

func draftExec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func draftSeed(t *testing.T, db *sql.DB, id, chapter, version int, status, body string) {
	t.Helper()
	draftExec(t, db, "INSERT INTO contents(id,body) VALUES(?,?)", id, body)
	draftExec(t, db, "INSERT INTO drafts(id,chapter_number,version,status,content_id) VALUES(?,?,?,?,?)", id, chapter, version, status, id)
}

func draftContext(project, lease, path string) map[string]any {
	return map[string]any{"projectId": project, "leaseId": lease, "projectPath": path}
}

func draftOwner(cookie *http.Cookie, tab string) Caller {
	return Caller{Principal: "synthetic-author", SessionID: cookie.Value, ClientID: tab}
}

func draftRPC(t *testing.T, server *Server, cookie *http.Cookie, csrf, tab, operation string, args ...any) *httptest.ResponseRecorder {
	t.Helper()
	if args == nil {
		args = []any{}
	}
	body, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	return rpc(t, server, cookie, csrf, operation, string(body), map[string]string{"X-Client-Id": tab})
}

func draftResult(t *testing.T, response *httptest.ResponseRecorder) any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", response.Code, response.Body.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	result, ok := envelope["result"]
	if !ok || len(envelope) != 1 {
		t.Fatalf("not the original RPC result envelope: %s", response.Body.String())
	}
	return result
}

func requireDraftFailure(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	want := map[string]any{"success": false, "error": "Error: " + code}
	if code == "DRAFT_CONTENT_CONFLICT" {
		want["errorCode"] = code
		want["error"] = "草稿已被其他编辑修改或已变为只读。请保留当前编辑内容，重新载入后比较，不要直接覆盖。"
	}
	if got := draftResult(t, response); !reflect.DeepEqual(got, want) {
		t.Fatalf("result = %#v, want %#v", got, want)
	}
}

func requireDraftHTTPError(t *testing.T, response *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	var got map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"error": map[string]any{"code": code, "message": code}}
	if response.Code != status || !reflect.DeepEqual(got, want) {
		t.Fatalf("HTTP %d %s, want %d %#v", response.Code, response.Body.String(), status, want)
	}
}

func TestDraftHTTPReadsOriginalShapes(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	db, repository := transportDraftDB(t)
	draftSeed(t, db, 1, 2, 1, "finalized", "第二章\r\n😀")
	draftSeed(t, db, 2, 1, 2, "superseded", "旧稿")
	draftSeed(t, db, 3, 1, 1, "draft", "候选")
	draftExec(t, db, `INSERT INTO finalization_outbox VALUES
 ('receipt-1',1,2,' 权威标题 ',?,?),('receipt-2',2,1,'替换标题',?,?)`,
		strings.Repeat("a", 64), "第二章\r\n😀", strings.Repeat("b", 64), "旧稿")
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	path := expected["projectPath"]
	provider.adopt(draftOwner(cookie, "synthetic-tab-0001"), expected, repository)
	meta := func(id, chapter, version int, status, title string) map[string]any {
		value := map[string]any{
			"id": float64(id), "chapterNumber": float64(chapter), "version": float64(version),
			"status": status, "source": "write", "contentId": float64(id), "wordCount": float64(11),
			"sourceDependencies": []any{}, "dependenciesStale": false,
			"createdAt": "created-original", "updatedAt": "updated-original",
		}
		if title != "" {
			value["chapterTitle"] = title
		}
		return value
	}
	for _, row := range []struct {
		operation string
		args      []any
		want      any
	}{
		{"db:draft-list", []any{1, path, expected}, []any{meta(3, 1, 1, "draft", ""), meta(2, 1, 2, "superseded", "替换标题")}},
		{"db:draft-list-all", []any{path, expected}, []any{meta(3, 1, 1, "draft", ""), meta(2, 1, 2, "superseded", "替换标题"), meta(1, 2, 1, "finalized", " 权威标题 ")}},
		{"db:draft-get-meta", []any{1, path, expected}, meta(1, 2, 1, "finalized", " 权威标题 ")},
		{"db:draft-list", []any{999, path, expected}, []any{}},
		{"db:draft-get-meta", []any{999, path, expected}, nil},
		{"db:draft-get-full", []any{999, path, expected}, nil},
	} {
		t.Run(row.operation+fmt.Sprint(row.args[0]), func(t *testing.T) {
			response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", row.operation, row.args...)
			if got := draftResult(t, response); !reflect.DeepEqual(got, row.want) {
				t.Fatalf("got %#v, want %#v", got, row.want)
			}
		})
	}
	full := meta(1, 2, 1, "finalized", " 权威标题 ")
	full["content"] = "第二章\r\n😀"
	if got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-get-full", 1, path, expected)); !reflect.DeepEqual(got, full) {
		t.Fatalf("full = %#v, want %#v", got, full)
	}
	if provider.begins.Load() != provider.releases.Load() {
		t.Fatal("operation-scoped holds were not released")
	}
}

func TestDraftHTTPRequiresFrozenContextAtOriginalWirePosition(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	for _, row := range []struct {
		operation string
		args      []any
	}{
		{"db:draft-list", []any{1}},
		{"db:draft-list", []any{1, "/synthetic/project-a"}},
		{"db:draft-list-all", []any{}},
		{"db:draft-list-all", []any{"/synthetic/project-a"}},
		{"db:draft-get-meta", []any{1, "/synthetic/project-a", nil}},
		{"db:draft-get-full", []any{1, "/synthetic/project-a", map[string]any{"projectPath": "/synthetic/project-a"}}},
		{"db:draft-get-full", []any{1, "/synthetic/project-a", []any{"project-a", "lease-a", "/synthetic/project-a"}}},
	} {
		requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", row.operation, row.args...), 400, "PROJECT_SESSION_REQUIRED")
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-get-meta", 1, "/different", expected), 409, "PROJECT_CONTEXT_CHANGED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list-all", expected["projectPath"], expected, expected), 400, "INVALID_ARGUMENTS")
	requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 1, "new", 1, "/synthetic/project-a", "old"), "PROJECT_SESSION_REQUIRED")
	if provider.begins.Load() != 0 {
		t.Fatal("invalid request reached provider")
	}
}

func TestDraftHTTPCrossTabSessionAndStaleLease(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	otherCookie, otherCSRF := bootstrap(t, server)
	db, repository := transportDraftDB(t)
	draftSeed(t, db, 1, 1, 1, "draft", "private a")
	dbB, repositoryB := transportDraftDB(t)
	draftSeed(t, dbB, 1, 1, 1, "draft", "private b")
	expected := draftContext("project-a", "lease-a", "/synthetic/shared-looking-path")
	expectedB := draftContext("project-b", "lease-b", "/synthetic/shared-looking-path")
	owner := draftOwner(cookie, "synthetic-tab-0001")
	provider.adopt(owner, expected, repository)
	provider.adopt(draftOwner(cookie, "synthetic-tab-0002"), expectedB, repositoryB)
	for _, row := range []struct {
		cookie *http.Cookie
		csrf   string
		tab    string
	}{
		{cookie, csrf, "synthetic-tab-0002"},
		{otherCookie, otherCSRF, "synthetic-tab-0001"},
	} {
		requireDraftHTTPError(t, draftRPC(t, server, row.cookie, row.csrf, row.tab, "db:draft-get-full", 1, expected["projectPath"], expected), 409, "PROJECT_SESSION_STALE")
		requireDraftFailure(t, draftRPC(t, server, row.cookie, row.csrf, row.tab, "db:draft-update-content", 1, "stolen", 1, expected["projectPath"], "private a", expected), "PROJECT_SESSION_STALE")
	}
	got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "db:draft-get-full", 1, expectedB["projectPath"], expectedB)).(map[string]any)
	if got["content"] != "private b" {
		t.Fatal(got)
	}
	rotated := draftContext("project-a", "lease-a-new", expected["projectPath"].(string))
	provider.adopt(owner, rotated, repository)
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:draft-get-full", 1, expected["projectPath"], expected), 409, "PROJECT_SESSION_STALE")
	requireDraftFailure(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:draft-update-content", 1, "stale", 1, expected["projectPath"], "private a", expected), "PROJECT_SESSION_STALE")
	forged := draftContext("project-a", "lease-a-new", "/synthetic/forged")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, owner.ClientID, "db:draft-get-full", 1, forged["projectPath"], forged), 409, "PROJECT_SESSION_ROOT_MISMATCH")
	full, err := repository.GetFull(context.Background(), 1)
	if err != nil || full.Content != "private a" || full.WordCount != 11 || full.UpdatedAt != "updated-original" {
		t.Fatalf("unauthorized write: %+v %v", full, err)
	}
}

func TestDraftHTTPAuthorSaveBaselineAndReadonly(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	db, repository := transportDraftDB(t)
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	path := expected["projectPath"]
	provider.adopt(draftOwner(cookie, "synthetic-tab-0001"), expected, repository)
	draftSeed(t, db, 1, 1, 1, "draft", "原稿\r\n😀")
	for _, args := range [][]any{
		{1, "new", 1, path, expected},
		{1, "new", 1, path, nil, expected},
		{1, "new", 1, path, 123, expected},
		{1, "new", 1, path, "原稿\n😀", expected},
	} {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", args...), "DRAFT_CONTENT_CONFLICT")
	}
	before, err := repository.GetFull(context.Background(), 1)
	if err != nil || before.Content != "原稿\r\n😀" || before.WordCount != 11 || before.UpdatedAt != "updated-original" {
		t.Fatalf("conflict changed draft: %+v %v", before, err)
	}
	requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 999, "new", 1, path, "", expected), "DRAFT_NOT_FOUND")
	for i, status := range []string{"finalized", "superseded", "discarded", "future-status"} {
		id := i + 10
		draftSeed(t, db, id, id, 1, status, "read only")
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", id, "new", 1, path, "read only", expected), "DRAFT_CONTENT_CONFLICT")
	}
	for i, status := range []string{"draft", "revised", "reviewed"} {
		id := i + 20
		draftSeed(t, db, id, id, 1, status, "")
		got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", id, "新稿\u0000\n𠮷", 0, path, "", expected))
		if !reflect.DeepEqual(got, map[string]any{"success": true}) {
			t.Fatal(got)
		}
		full, err := repository.GetFull(context.Background(), int64(id))
		if err != nil || full.Content != "新稿\u0000\n𠮷" || full.WordCount != 0 || full.Status != status || full.UpdatedAt == "updated-original" {
			t.Fatalf("saved draft: %+v %v", full, err)
		}
	}
	if provider.begins.Load() != provider.releases.Load() {
		t.Fatal("save/conflict/error leaked a project hold")
	}
}

func TestDraftHTTPNumericAndContentArgumentsFailClosed(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	path := expected["projectPath"]
	for _, value := range []any{nil, true, "1", -1, 0, 1.5, 9007199254740992.0} {
		for _, operation := range []string{"db:draft-list", "db:draft-get-meta", "db:draft-get-full"} {
			requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", operation, value, path, expected), 400, "INVALID_ARGUMENTS")
		}
	}
	for _, value := range []any{nil, true, "1", -1, 1.5, 9007199254740992.0} {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 1, "new", value, path, "old", expected), "INVALID_ARGUMENTS")
	}
	for _, value := range []any{nil, true, 12, map[string]any{"text": "new"}} {
		requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 1, value, 1, path, "old", expected), "INVALID_ARGUMENTS")
	}
	if provider.begins.Load() != 0 {
		t.Fatal("invalid arguments reached repository provider")
	}
	for _, value := range []any{json.Number("1.5"), json.Number("1.00000000000000001"), json.Number("9007199254740992"), json.Number("1e999"), json.Number("0e999999999"), json.Number("nope")} {
		if _, ok := draftSafeInteger(value, 0); ok {
			t.Fatalf("accepted invalid json.Number %q", value)
		}
	}
	for _, value := range []any{json.Number("0"), json.Number("1e3"), float64(9007199254740991)} {
		if _, ok := draftSafeInteger(value, 0); !ok {
			t.Fatalf("rejected safe integer %#v", value)
		}
	}
}

func TestDraftHTTPConcurrentCASOneWinner(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	db, repository := transportDraftDB(t)
	draftSeed(t, db, 1, 1, 1, "draft", "baseline")
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	for _, tab := range []string{"synthetic-tab-0001", "synthetic-tab-0002"} {
		provider.adopt(draftOwner(cookie, tab), expected, repository)
	}
	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var workers sync.WaitGroup
	for i, tab := range []string{"synthetic-tab-0001", "synthetic-tab-0002"} {
		workers.Add(1)
		go func(i int, tab string) {
			defer workers.Done()
			<-start
			responses <- draftRPC(t, server, cookie, csrf, tab, "db:draft-update-content", 1, fmt.Sprintf("winner-%d", i), i+1, expected["projectPath"], "baseline", expected)
		}(i, tab)
	}
	close(start)
	workers.Wait()
	close(responses)
	wins, conflicts := 0, 0
	for response := range responses {
		got := draftResult(t, response).(map[string]any)
		if got["success"] == true {
			wins++
		} else {
			requireDraftFailure(t, response, "DRAFT_CONTENT_CONFLICT")
			conflicts++
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("wins=%d conflicts=%d", wins, conflicts)
	}
	full, err := repository.GetFull(context.Background(), 1)
	if err != nil || !(full.Content == "winner-0" && full.WordCount == 1 || full.Content == "winner-1" && full.WordCount == 2) {
		t.Fatalf("CAS write/body metadata torn: %+v %v", full, err)
	}
	if provider.begins.Load() != 2 || provider.releases.Load() != 2 {
		t.Fatal("concurrent provider scope mismatch")
	}
}

func TestDraftHTTPProviderErrorsRedactedAndReleased(t *testing.T) {
	server := fixture(t)
	var releases atomic.Int64
	provider := DraftProviderFunc(func(context.Context, Caller, any) (*store.Drafts, func(), error) {
		return nil, func() { releases.Add(1) }, errors.New("SQLite failure at /private/secret-project.sqlite")
	})
	if err := server.BindDrafts(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-get-full", 1, expected["projectPath"], expected)
	if response.Code != 500 || !strings.Contains(response.Body.String(), `"code":"REQUEST_FAILED"`) || strings.Contains(response.Body.String(), "private") || strings.Contains(response.Body.String(), "SQLite") {
		t.Fatal(response.Code, response.Body.String())
	}
	requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 1, "new", 1, expected["projectPath"], "old", expected), "REQUEST_FAILED")
	if releases.Load() != 2 {
		t.Fatal("provider error leaked partial hold")
	}
}

func TestBindDraftsFailsAtomicallyWithoutReplacement(t *testing.T) {
	server := fixture(t)
	provider := newAdoptedDraftProvider()
	if err := server.BindDrafts(nil); err == nil {
		t.Fatal("nil provider accepted")
	}
	if err := server.Bind("db:draft-get-full", func(context.Context, Caller, []any) (any, error) { return "original", nil }); err != nil {
		t.Fatal(err)
	}
	if err := server.BindDrafts(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatal("replacement allowed", err)
	}
	cookie, csrf := bootstrap(t, server)
	if got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-get-full")); got != "original" {
		t.Fatal(got)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list-all")
	requireDraftHTTPError(t, response, 501, "GO_OPERATION_NOT_MIGRATED")
}

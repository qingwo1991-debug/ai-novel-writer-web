package projectsession

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func TestNarrativeBrowserAuthorEvidenceAndExemptionLifecycle(t *testing.T) {
	project, contract, source := sessionFixture(t)
	dsn, err := store.ExistingDSN(source)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE drafts SET status='finalized' WHERE id=1"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	// This synthetic source deliberately differs from editable contents: evidence
	// is bound to the frozen finalization snapshot, not whatever body is now live.
	if _, err := db.Exec(`INSERT INTO finalization_outbox(finalization_id,draft_id,chapter_number,chapter_title,content_hash,content_revision,content_snapshot,target_file_name) VALUES('synthetic-narrative-final',1,1,'First finalized','synthetic-only',1,'发现 古老 秘密 😀','synthetic.txt')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open("../../../web/operations.json")
	if err != nil {
		t.Fatal(err)
	}
	registry, err := policy.Load(file)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(registry, security.NewOrigins("http://synthetic.test"), "synthetic")
	defer server.Close()
	manager := New()
	defer manager.Close()
	provider := transport.NarrativeProviderFunc(func(ctx context.Context, c transport.Caller, raw any) (*store.Narrative, func(), error) {
		expected, err := ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		return manager.BeginNarrative(ctx, Owner{SessionID: c.SessionID, ClientID: c.ClientID}, expected)
	})
	if err := server.BindNarrative(provider); err != nil {
		t.Fatal(err)
	}
	var caller transport.Caller
	if err := server.Bind("config:get", func(_ context.Context, c transport.Caller, _ []any) (any, error) { caller = c; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	bootstrap := func() (*http.Cookie, string) {
		request := httptest.NewRequest("GET", "/api/bootstrap", nil)
		request.Header.Set("X-Novel-User", "synthetic-author")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		if response.Code != 200 {
			t.Fatal(response.Code)
		}
		var result struct {
			Token string `json:"csrfToken"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal("bootstrap")
		}
		return response.Result().Cookies()[0], result.Token
	}
	cookie, csrf := bootstrap()
	invoke := func(cookie *http.Cookie, csrf, tab, op string, args []any) (int, map[string]any, string) {
		data, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+op, bytes.NewReader(data))
		request.AddCookie(cookie)
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", csrf)
		request.Header.Set("Origin", "http://synthetic.test")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var result map[string]any
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatal(response.Body.String())
		}
		return response.Code, result, response.Body.String()
	}
	const tab = "synthetic-narrative-tab-0001"
	if status, _, _ := invoke(cookie, csrf, tab, "config:get", []any{}); status != 200 {
		t.Fatal(status)
	}
	owner := Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	expected, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	call := func(op string, args ...any) map[string]any {
		args = append(args, project.Path(), expected)
		status, result, _ := invoke(cookie, csrf, tab, op, args)
		if status != 200 {
			t.Fatal(op, status, result)
		}
		return result
	}
	business := func(result map[string]any) map[string]any {
		value, ok := result["result"].(map[string]any)
		if !ok || value["success"] != true {
			t.Fatal("business failed", result)
		}
		return value
	}
	input := map[string]any{"title": "\ufeff 古老秘密 \n", "type": " 伏笔 ", "authorIntent": " 由作者决定何时回收 ", "targetStartChapter": 1, "targetEndChapter": 1}
	plan := business(call("db:narrative-thread-plan-create", input))["plan"].(map[string]any)
	if plan["title"] != "古老秘密" || plan["type"] != "伏笔" {
		t.Fatal(plan)
	}
	id := plan["id"]
	plans := call("db:narrative-thread-list")["result"].([]any)
	if len(plans) != 1 || plans[0].(map[string]any)["status"] != "planned" {
		t.Fatal(plans)
	}
	eventInput := map[string]any{"planId": id, "draftId": 1, "type": "planted", "evidence": " 发现\t古老\n秘密 😀 ", "reason": " 作者确认原文证据 ", "selectionKey": "browser-extra-preserved"}
	event := business(call("db:narrative-thread-event-confirm", eventInput))["event"].(map[string]any)
	if event["selectionKey"] != "browser-extra-preserved" || event["chapterNumber"] != float64(1) || event["evidence"] != "发现\t古老\n秘密 😀" {
		t.Fatal(event)
	}
	contextInput := map[string]any{"chapterNumber": 9, "title": "下章", "keyEvents": "别的事件", "characters": []any{"古老秘密"}}
	relevant := call("db:narrative-thread-list-relevant", contextInput)["result"].([]any)
	if len(relevant) != 1 {
		t.Fatal("character reference did not select outside window", relevant)
	}
	// A malformed boolean plan ID must never alias plan 1 through Go's driver.
	invalidID := map[string]any{"planId": true, "draftId": 1, "type": "progressing", "evidence": "发现", "reason": "must not write"}
	badStatus, _, badRaw := invoke(cookie, csrf, tab, "db:narrative-thread-event-confirm", []any{invalidID, project.Path(), expected})
	if badStatus != 500 || !strings.Contains(badRaw, "NARRATIVE_OPERATION_FAILED") {
		t.Fatal("boolean identity accepted", badStatus, badRaw)
	}
	plans = call("db:narrative-thread-list")["result"].([]any)
	if len(plans[0].(map[string]any)["events"].([]any)) != 1 {
		t.Fatal("malformed boolean identity inserted event")
	}
	eventInput["type"] = "resolved"
	business(call("db:narrative-thread-event-confirm", eventInput))
	if len(call("db:narrative-thread-list-relevant", contextInput)["result"].([]any)) != 0 {
		t.Fatal("terminal thread remained active")
	}
	eventInput["evidence"] = "不存在的证据"
	status, _, raw := invoke(cookie, csrf, tab, "db:narrative-thread-event-confirm", []any{eventInput, project.Path(), expected})
	if status != 500 || !strings.Contains(raw, "NARRATIVE_THREAD_EVIDENCE_NOT_IN_SOURCE") {
		t.Fatal(status, raw)
	}
	input["title"] = "作者改题"
	business(call("db:narrative-thread-plan-update", id, input))
	plans = call("db:narrative-thread-list")["result"].([]any)
	if plans[0].(map[string]any)["title"] != "作者改题" {
		t.Fatal(plans)
	}
	business(call("db:consistency-exemption-save", " \ufeff stable:fact ", " 作者说明 "))
	exemptions := call("db:consistency-exemption-list")["result"].([]any)
	if len(exemptions) != 1 || exemptions[0].(map[string]any)["revoked"] != false || exemptions[0].(map[string]any)["stableFactKey"] != "stable:fact" {
		t.Fatal(exemptions)
	}
	business(call("db:consistency-exemption-revoke", "stable:fact"))
	if call("db:consistency-exemption-list")["result"].([]any)[0].(map[string]any)["revoked"] != true {
		t.Fatal("not revoked")
	}
	business(call("db:consistency-exemption-save", "stable:fact", "作者再次确认"))
	if call("db:consistency-exemption-list")["result"].([]any)[0].(map[string]any)["revoked"] != false {
		t.Fatal("save did not reactivate")
	}
	rejected := call("db:consistency-exemption-save", "\ufeff \n", "reason")["result"].(map[string]any)
	if rejected["error"] != "Error: CONSISTENCY_EXEMPTION_KEY_INVALID" {
		t.Fatal(rejected)
	}
	const count = 8
	var group sync.WaitGroup
	errors := make(chan string, count)
	for i := 0; i < count; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			status, result, raw := invoke(cookie, csrf, tab, "db:consistency-exemption-save", []any{"stable:fact", "concurrent author note", project.Path(), expected})
			if status != 200 {
				errors <- raw
				return
			}
			receipt, ok := result["result"].(map[string]any)
			if !ok || receipt["success"] != true {
				errors <- raw
			}
		}()
	}
	group.Wait()
	close(errors)
	for failure := range errors {
		t.Fatal(failure)
	}
	if len(call("db:consistency-exemption-list")["result"].([]any)) != 1 {
		t.Fatal("upsert duplicated stable fact")
	}
	business(call("db:narrative-thread-plan-delete", id))
	if len(call("db:narrative-thread-list")["result"].([]any)) != 0 {
		t.Fatal("delete retained plan")
	}
	otherCookie, otherCSRF := bootstrap()
	for _, other := range []struct {
		cookie    *http.Cookie
		csrf, tab string
	}{{cookie, csrf, "synthetic-narrative-tab-0002"}, {otherCookie, otherCSRF, tab}} {
		if status, _, _ := invoke(other.cookie, other.csrf, other.tab, "db:narrative-thread-list", []any{project.Path(), expected}); status == 200 {
			t.Fatal("cross-owner narrative read")
		}
	}
	if status, _, _ := invoke(cookie, "invalid", tab, "db:consistency-exemption-save", []any{"key", "reason", project.Path(), expected}); status != 403 {
		t.Fatal("CSRF missing", status)
	}
	repository, done, err := manager.BeginNarrative(context.Background(), owner, expected)
	if err != nil || repository == nil {
		t.Fatal(err)
	}
	if err := manager.Release(owner, expected); err == nil {
		t.Fatal("released active narrative hold")
	}
	done()
	if err := manager.Release(owner, expected); err != nil {
		t.Fatal(err)
	}
	if status, _, _ := invoke(cookie, csrf, tab, "db:narrative-thread-list", []any{project.Path(), expected}); status == 200 {
		t.Fatal("stale narrative context")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source mutated", err)
	}
}

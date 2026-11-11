package projectsession

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestRecoveryRealPrivateSessionHTTPAndSourceUnchanged(t *testing.T) {
	project, contract, source := sessionFixture(t)
	dsn, err := store.ExistingDSN(source)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatal(err)
	}
	blueprint := map[string]any{"chapterNumber": 1, "title": "合成恢复来源", "role": "铺垫", "purpose": "取得钥匙", "keyEvents": "打开旧门", "characters": []any{"林秋"}, "suspenseHook": "门后是谁", "userGuidance": "作者提示", "notes": "", "notesUpdatedAt": ""}
	if err := store.NewBlueprints(seed).Upsert(context.Background(), blueprint); err != nil {
		seed.Close()
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
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
	provider := transport.RecoveryProviderFunc(func(ctx context.Context, c transport.Caller, raw any) (*store.Recovery, func(), error) {
		expected, err := ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		return manager.BeginRecovery(ctx, Owner{SessionID: c.SessionID, ClientID: c.ClientID}, expected)
	})
	if err := server.BindRecovery(provider); err != nil {
		t.Fatal(err)
	}
	var caller transport.Caller
	if err := server.Bind("config:get", func(_ context.Context, c transport.Caller, _ []any) (any, error) { caller = c; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	bootstrap := func() (*http.Cookie, string) {
		r := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
		r.Header.Set("X-Novel-User", "synthetic-author")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		var result struct {
			Token string `json:"csrfToken"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Result().Cookies()[0], result.Token
	}
	cookie, csrf := bootstrap()
	invoke := func(cookie *http.Cookie, csrf, tab, op string, args []any) (int, map[string]any) {
		body, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/operations/"+op, bytes.NewReader(body))
		r.AddCookie(cookie)
		r.Header.Set("X-Novel-User", "synthetic-author")
		r.Header.Set("X-Client-Id", tab)
		r.Header.Set("X-CSRF-Token", csrf)
		r.Header.Set("Origin", "http://synthetic.test")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		server.ServeHTTP(w, r)
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return w.Code, result
	}
	const tab = "synthetic-recovery-tab-0001"
	if status, _ := invoke(cookie, csrf, tab, "config:get", []any{}); status != 200 {
		t.Fatal(status)
	}
	owner := Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	expected, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	call := func(op string, args ...any) map[string]any {
		args = append(args, project.Path(), expected)
		status, result := invoke(cookie, csrf, tab, op, args)
		if status != 200 {
			t.Fatal(op, status, result)
		}
		return result
	}
	business := func(wire map[string]any) map[string]any {
		result, ok := wire["result"].(map[string]any)
		if !ok || result["success"] != true {
			t.Fatal(wire)
		}
		return result
	}
	sourceInput := map[string]any{}
	for _, key := range []string{"chapterNumber", "title", "role", "purpose", "keyEvents", "characters", "suspenseHook", "userGuidance"} {
		sourceInput[key] = blueprint[key]
	}
	input := map[string]any{"runId": "synthetic-run", "stepId": "synthetic-step", "chapterNumber": 1, "chapterTitle": "合成候选", "source": sourceInput, "sourceDraft": map[string]any{"id": 1, "version": 1}, "visibleText": "<think>private hidden reasoning</think> 合成恢复候选🙂", "failureCode": "INTERRUPTED", "failureReason": "synthetic"}
	candidate := business(call("db:recovery-candidate-record", input))["candidate"].(map[string]any)
	if candidate["projectId"] != expected.ProjectID || candidate["sourceCurrent"] != true || candidate["visibleText"] != "合成恢复候选🙂" {
		t.Fatal(candidate)
	}
	id := candidate["candidateId"].(string)
	listed := call("db:recovery-candidate-list")["result"].([]any)
	if len(listed) != 1 {
		t.Fatal(listed)
	}
	edited := business(call("db:recovery-candidate-update", id, "<think>hidden</think> 人工编辑正文"))["candidate"].(map[string]any)
	if edited["visibleText"] != "人工编辑正文" || edited["contentHash"] == candidate["contentHash"] {
		t.Fatal(edited)
	}
	// Change only the copied blueprint: stale candidates remain readable but cannot edit/continue.
	blueprint["title"] = "changed copied blueprint"
	blueprints, done, err := manager.BeginBlueprints(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	err = blueprints.Upsert(context.Background(), blueprint)
	done()
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"db:recovery-candidate-update", "db:recovery-candidate-resolve"} {
		arg := "continued"
		if op == "db:recovery-candidate-update" {
			arg = "edit"
		}
		wire := call(op, id, arg)["result"].(map[string]any)
		if wire["success"] != false || wire["error"] != "Error: RECOVERY_CANDIDATE_SOURCE_CHANGED" {
			t.Fatal(wire)
		}
	}
	business(call("db:recovery-candidate-resolve", id, "discarded"))
	if len(call("db:recovery-candidate-list")["result"].([]any)) != 0 {
		t.Fatal("resolved candidate still pending")
	}
	otherCookie, otherCSRF := bootstrap()
	for _, other := range []struct {
		cookie    *http.Cookie
		csrf, tab string
	}{{cookie, csrf, "synthetic-recovery-tab-0002"}, {otherCookie, otherCSRF, tab}} {
		status, _ := invoke(other.cookie, other.csrf, other.tab, "db:recovery-candidate-list", []any{project.Path(), expected})
		if status == 200 {
			t.Fatal("cross owner candidate leak")
		}
	}
	if status, _ := invoke(cookie, "wrong-csrf", tab, "db:recovery-candidate-update", []any{id, "bad", project.Path(), expected}); status != 403 {
		t.Fatal(status)
	}
	repo, done, err := manager.BeginRecovery(context.Background(), owner, expected)
	if err != nil || repo == nil {
		t.Fatal(err)
	}
	if !errors.Is(manager.Release(owner, expected), novelruntime.ErrBusy) {
		t.Fatal("inflight release accepted")
	}
	done()
	done()
	if err := manager.Release(owner, expected); err != nil {
		t.Fatal(err)
	}
	if status, _ := invoke(cookie, csrf, tab, "db:recovery-candidate-list", []any{project.Path(), expected}); status == 200 {
		t.Fatal("revoked candidate lease reused")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("original fixture modified", err)
	}
}

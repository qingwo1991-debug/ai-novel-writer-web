package projectsession

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/policy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func TestReviewRevisionBrowserSourceCASAndReplayOnPrivateCopy(t *testing.T) {
	project, contract, source := sessionFixture(t)
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
	provider := transport.ReviewRevisionProviderFunc(func(ctx context.Context, c transport.Caller, raw any) (*store.ReviewRevisions, func(), error) {
		expected, err := ParseContext(raw)
		if err != nil {
			return nil, nil, err
		}
		return manager.BeginReviewRevisions(ctx, Owner{SessionID: c.SessionID, ClientID: c.ClientID}, expected)
	})
	if err := server.BindReviewRevisions(provider); err != nil {
		t.Fatal(err)
	}
	if err := server.BindReviewRevisions(provider); err == nil {
		t.Fatal("duplicate binding replaced handlers")
	}
	var caller transport.Caller
	if err := server.Bind("config:get", func(_ context.Context, c transport.Caller, _ []any) (any, error) { caller = c; return nil, nil }); err != nil {
		t.Fatal(err)
	}
	boot := httptest.NewRequest("GET", "/api/bootstrap", nil)
	boot.Header.Set("X-Novel-User", "synthetic-author")
	response := httptest.NewRecorder()
	server.ServeHTTP(response, boot)
	if response.Code != 200 {
		t.Fatal(response.Code)
	}
	cookie := response.Result().Cookies()[0]
	var bootstrap struct {
		CSRFToken string `json:"csrfToken"`
	}
	if json.Unmarshal(response.Body.Bytes(), &bootstrap) != nil {
		t.Fatal("bootstrap")
	}
	invoke := func(op, tab string, args []any) (int, map[string]any) {
		body, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Error(err)
			return 0, nil
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+op, bytes.NewReader(body))
		request.AddCookie(cookie)
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", bootstrap.CSRFToken)
		request.Header.Set("Origin", "http://synthetic.test")
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		var result map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
			t.Error(err, response.Body.String())
		}
		return response.Code, result
	}
	const tab = "synthetic-revision-tab-0001"
	if status, _ := invoke("config:get", tab, []any{}); status != 200 {
		t.Fatal(status)
	}
	owner := Owner{SessionID: caller.SessionID, ClientID: caller.ClientID}
	expected, err := manager.OpenScratchCopy(context.Background(), owner, project, contract, t.TempDir(), 32*1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	drafts, release, err := manager.Begin(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	full, err := drafts.GetFull(context.Background(), 1)
	release()
	if err != nil || full == nil {
		t.Fatal(full, err)
	}
	baseline := map[string]any{"id": full.ID, "chapterNumber": full.ChapterNumber, "version": full.Version, "status": full.Status, "content": full.Content}
	call := func(op string, input any) map[string]any {
		status, result := invoke(op, tab, []any{input, project.Path(), expected})
		if status != 200 {
			t.Fatal(op, status, result)
		}
		return result
	}
	success := func(result map[string]any) map[string]any {
		data, ok := result["result"].(map[string]any)
		if !ok || data["success"] != true {
			t.Fatal("business failure", result)
		}
		return data
	}
	missing := call("db:review-create", map[string]any{"baseDraftId": 1, "content": "must not allocate"})["result"].(map[string]any)
	if missing["errorCode"] != "SOURCE_DRAFT_CHANGED" || missing["error"] != "SourceDraftChangedError: SOURCE_DRAFT_CHANGED" {
		t.Fatal(missing)
	}
	reviewInput := map[string]any{"baseDraftId": 1, "reviewIndex": 999, "content": "public report 中文", "expectedSource": baseline}
	review := success(call("db:review-create", reviewInput))
	if review["reviewIndex"] != float64(1) {
		t.Fatal("caller allocated index", review)
	}
	report := call("db:review-get-full", review["id"])["result"].(map[string]any)
	if report["content"] != "public report 中文" || report["sourceDraft"].(map[string]any)["content"] != full.Content {
		t.Fatal(report)
	}
	input := func(body string) map[string]any {
		return map[string]any{"baseDraftId": 1, "revisionType": "refine", "content": body, "wordCount": 12, "expectedSource": baseline}
	}
	old := success(call("db:revision-create", input("old branch")))
	replacement := success(call("db:revision-replace-pending", input("new branch 😀")))
	if replacement["revisionIndex"] != float64(2) {
		t.Fatal(replacement)
	}
	oldFull := call("db:revision-get-full", old["id"])["result"].(map[string]any)
	if oldFull["status"] != "discarded" {
		t.Fatal("pending replacement did not atomically discard", oldFull)
	}
	pending := call("db:revision-get-pending", 1)["result"].([]any)
	if len(pending) != 1 {
		t.Fatal(pending)
	}
	mergedBody := "author confirmed merge 中文😀"
	merge := map[string]any{"revisionId": replacement["id"], "targetDraftId": 1, "expectedDraftContent": full.Content, "mergedContent": mergedBody, "wordCount": 18}
	first := success(call("db:revision-merge", merge))["receipt"].(map[string]any)
	if first["idempotent"] != false || first["status"] != "revised" {
		t.Fatal(first)
	}
	second := success(call("db:revision-merge", merge))["receipt"].(map[string]any)
	if second["idempotent"] != true {
		t.Fatal("exact merge replay lost identity", second)
	}
	// A replay cannot restore its old merged body after a later author edit.
	drafts, release, err = manager.Begin(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	err = drafts.UpdateContentIfCurrent(context.Background(), 1, "later author edit", 7, &mergedBody)
	release()
	if err != nil {
		t.Fatal(err)
	}
	replay := call("db:revision-merge", merge)["result"].(map[string]any)
	if replay["success"] != false || replay["error"] != "Error: REVISION_ALREADY_MERGED" {
		t.Fatal(replay)
	}
	stale := call("db:revision-create", input("stale source must not save"))["result"].(map[string]any)
	if stale["errorCode"] != "SOURCE_DRAFT_CHANGED" {
		t.Fatal(stale)
	}
	for _, key := range []string{"reviewCycleId", "generationRunHandle", "authorRecovery"} {
		request := input("unowned")
		request[key] = nil
		result := call("db:revision-create", request)["result"].(map[string]any)
		if result["success"] != false || result["error"] != "Error: REVIEW_CYCLE_OWNER_REQUIRED" {
			t.Fatal(key, result)
		}
	}
	if status, _ := invoke("db:review-get-full", "synthetic-revision-tab-0002", []any{review["id"], project.Path(), expected}); status == 200 {
		t.Fatal("cross-tab read leaked")
	}
	status, cross := invoke("db:revision-create", "synthetic-revision-tab-0002", []any{input("cross tab"), project.Path(), expected})
	if status == 200 {
		if cross["result"].(map[string]any)["success"] != false {
			t.Fatal("cross-tab mutation succeeded")
		}
	}
	// Concurrent author-confirmed merges of two branches over the SAME source may
	// commit only one; the loser cannot be rebased onto the newly edited target.
	drafts, release, err = manager.Begin(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := drafts.GetFull(context.Background(), 1)
	release()
	if err != nil {
		t.Fatal(err)
	}
	baseline = map[string]any{"id": fresh.ID, "chapterNumber": fresh.ChapterNumber, "version": fresh.Version, "status": fresh.Status, "content": fresh.Content}
	a := success(call("db:revision-create", input("candidate a")))
	b := success(call("db:revision-create", input("candidate b")))
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins, conflicts := 0, 0
	for i, id := range []any{a["id"], b["id"]} {
		wg.Go(func() {
			body := []string{"branch A", "branch B"}[i]
			request := map[string]any{"revisionId": id, "targetDraftId": 1, "expectedDraftContent": fresh.Content, "mergedContent": body, "wordCount": 8}
			status, result := invoke("db:revision-merge", tab, []any{request, project.Path(), expected})
			if status != 200 {
				t.Error(status, result)
				return
			}
			data := result["result"].(map[string]any)
			mu.Lock()
			defer mu.Unlock()
			if data["success"] == true {
				wins++
			} else if data["error"] == "Error: REVISION_TARGET_CHANGED" {
				conflicts++
			} else {
				t.Error(data)
			}
		})
	}
	wg.Wait()
	if wins != 1 || conflicts != 1 {
		t.Fatal("merge lost update", wins, conflicts)
	}
	repository, done, err := manager.BeginReviewRevisions(context.Background(), owner, expected)
	if err != nil || repository == nil {
		t.Fatal(err)
	}
	if err := manager.Release(owner, expected); err == nil {
		t.Fatal("released transaction capability")
	}
	done()
	done()
	if err := manager.Release(owner, expected); err != nil {
		t.Fatal(err)
	}
	if status, _ := invoke("db:review-list", tab, []any{1, project.Path(), expected}); status == 200 {
		t.Fatal("stale context read")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("source database changed", err)
	}
}

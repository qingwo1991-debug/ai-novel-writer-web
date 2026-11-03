//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestSyntheticEditorStageAndReviewReadsShareExactPrivateLease(t *testing.T) {
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original schema oracle required")
		}
		t.Skip("GitHub original schema oracle required")
	}
	file, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	contract, err := admission.ParseContract(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := os.ReadFile(filepath.Join(filepath.Dir(oracle), "baseline-v7.db"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	libraryDir := filepath.Join(root, "projects")
	projectPath := filepath.Join(libraryDir, "synthetic-stage-review")
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(storage, projectfs.DatabaseName)
	if err := os.WriteFile(source, baseline, 0600); err != nil {
		t.Fatal(err)
	}
	seed, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at, premise) VALUES('main', 'Stage review', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00', 'Read-only premise')`); err != nil {
		t.Fatal(err)
	}
	if err := store.NewBlueprints(seed).Upsert(context.Background(), map[string]any{"chapterNumber": int64(1), "title": "同租约蓝图🙂", "role": "转折", "purpose": "目的", "keyEvents": "事件", "characters": []any{"甲"}, "suspenseHook": "悬念", "userGuidance": "", "notes": "", "notesUpdatedAt": ""}); err != nil {
		t.Fatal(err)
	}
	content, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", "编辑原稿🙂")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := content.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	draft, err := seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(1,1,'draft',?)", contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := draft.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	revisions := store.NewReviewRevisions(seed)
	expectedSource := map[string]any{"id": draftID, "chapterNumber": int64(1), "version": int64(1), "status": "draft", "content": "编辑原稿🙂"}
	review, err := revisions.CreateReview(context.Background(), map[string]any{"baseDraftId": draftID, "content": "审稿意见", "expectedSource": expectedSource})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := revisions.CreateRevision(context.Background(), map[string]any{"baseDraftId": draftID, "content": "修稿建议", "revisionType": "refine", "wordCount": int64(4), "expectedSource": expectedSource})
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithModelsAndReadOpen(migrationRegistry(t), "127.0.0.1:16082", "", libraryDir, "", "", nil, &syntheticReadOpenConfig{Schema: contract})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
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
	rpc := func(tab, operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", tab)
		request.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	const tab = "client_1234567890"
	opened := rpc(tab, "project:open", []any{projectPath, "stage-review-open", nil})
	var openWire struct {
		Result struct {
			Success bool `json:"success"`
			Project struct {
				ID    string `json:"id"`
				Path  string `json:"path"`
				Lease string `json:"sessionLease"`
			} `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &openWire); err != nil || opened.Code != http.StatusOK || !openWire.Result.Success || openWire.Result.Project.ID == "" || openWire.Result.Project.Path != projectPath || openWire.Result.Project.Lease == "" {
		t.Fatal("open failed", opened.Code, opened.Body.String(), err)
	}
	expected := map[string]any{"projectId": openWire.Result.Project.ID, "leaseId": openWire.Result.Project.Lease, "projectPath": projectPath}
	overview := rpc(tab, "project:overview-current", []any{expected})
	var overviewWire struct {
		Result struct {
			State     string `json:"state"`
			ProjectID string `json:"projectId"`
			Name      string `json:"name"`
			Revision  string `json:"revision"`
			Excerpt   string `json:"excerpt"`
			Stages    []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
				Count  int    `json:"count"`
			} `json:"stages"`
		} `json:"result"`
	}
	if err := json.Unmarshal(overview.Body.Bytes(), &overviewWire); err != nil || overview.Code != http.StatusOK || overviewWire.Result.State != "ready" || overviewWire.Result.ProjectID != openWire.Result.Project.ID || overviewWire.Result.Name != "Stage review" || len(overviewWire.Result.Revision) != 64 || overviewWire.Result.Excerpt != "编辑原稿🙂" || len(overviewWire.Result.Stages) != 6 {
		t.Fatal("current overview mismatch", overview.Code, overview.Body.String(), err)
	}
	if foreign := rpc("client_0987654321", "project:overview-current", []any{expected}); foreign.Code == http.StatusOK {
		t.Fatal("cross-tab overview read accepted", foreign.Body.String())
	}
	for _, item := range []struct {
		operation, marker string
		args              []any
		wantID            int64
	}{
		{"db:project-core-get", "Read-only premise", []any{projectPath, expected}, 0},
		{"db:blueprint-get-all", "同租约蓝图🙂", []any{projectPath, expected}, 0},
		{"db:revision-get-pending", "", []any{draftID, projectPath, expected}, revision["id"].(int64)},
		{"db:revision-list", "", []any{draftID, projectPath, expected}, revision["id"].(int64)},
		{"db:review-list", "", []any{draftID, projectPath, expected}, review["id"].(int64)},
	} {
		response := rpc(tab, item.operation, item.args)
		if response.Code != http.StatusOK || !json.Valid(response.Body.Bytes()) || (item.marker != "" && !bytes.Contains(response.Body.Bytes(), []byte(item.marker))) {
			t.Fatalf("%s: HTTP %d %s", item.operation, response.Code, response.Body.String())
		}
		if item.wantID != 0 {
			var envelope struct {
				Result []map[string]any `json:"result"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result) != 1 || envelope.Result[0]["id"] != float64(item.wantID) || envelope.Result[0]["baseDraftId"] != float64(draftID) {
				t.Fatal("review/revision metadata mismatch", item.operation, response.Body.String(), err)
			}
		}
	}
	for _, item := range []struct {
		operation, content string
		id                 int64
	}{
		{"db:review-get-full", "审稿意见", review["id"].(int64)},
		{"db:revision-get-full", "修稿建议", revision["id"].(int64)},
	} {
		response := rpc(tab, item.operation, []any{item.id, projectPath, expected})
		var envelope struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusOK || envelope.Result["id"] != float64(item.id) || envelope.Result["baseDraftId"] != float64(draftID) || envelope.Result["content"] != item.content {
			t.Fatalf("%s full DTO: HTTP %d %s, %v", item.operation, response.Code, response.Body.String(), err)
		}
		sourceDraft, ok := envelope.Result["sourceDraft"].(map[string]any)
		if !ok || sourceDraft["id"] != float64(draftID) || sourceDraft["content"] != expectedSource["content"] {
			t.Fatalf("%s source draft: %#v", item.operation, envelope.Result["sourceDraft"])
		}
		missing := rpc(tab, item.operation, []any{int64(9007199254740991), projectPath, expected})
		var missingEnvelope struct {
			Result any `json:"result"`
		}
		if err := json.Unmarshal(missing.Body.Bytes(), &missingEnvelope); err != nil || missing.Code != http.StatusOK || missingEnvelope.Result != nil {
			t.Fatalf("%s missing row: HTTP %d %s, %v", item.operation, missing.Code, missing.Body.String(), err)
		}
		if foreign := rpc("client_0987654321", item.operation, []any{item.id, projectPath, expected}); foreign.Code == http.StatusOK {
			t.Fatalf("foreign tab read %s: %s", item.operation, foreign.Body.String())
		}
	}
	cycle := rpc(tab, "db:review-cycle-get", []any{review["id"], projectPath, expected})
	var absentCycle struct {
		Result any `json:"result"`
	}
	if err := json.Unmarshal(cycle.Body.Bytes(), &absentCycle); err != nil || cycle.Code != http.StatusOK || absentCycle.Result != nil {
		t.Fatalf("absent review cycle: HTTP %d %s, %v", cycle.Code, cycle.Body.String(), err)
	}
	if foreign := rpc("client_0987654321", "db:review-cycle-get", []any{review["id"], projectPath, expected}); foreign.Code == http.StatusOK {
		t.Fatal("foreign tab read review cycle", foreign.Body.String())
	}
	if response := rpc(tab, "db:project-core-get", []any{filepath.Join(root, "other"), expected}); response.Code != http.StatusConflict {
		t.Fatal("different path reached core", response.Code, response.Body.String())
	}
	if response := rpc("client_0987654321", "db:review-list", []any{draftID, projectPath, expected}); response.Code == http.StatusOK {
		t.Fatal("foreign tab read review", response.Body.String())
	}
	stale := map[string]any{"projectId": openWire.Result.Project.ID, "leaseId": "stale", "projectPath": projectPath}
	if response := rpc(tab, "db:blueprint-get-all", []any{projectPath, stale}); response.Code == http.StatusOK {
		t.Fatal("stale lease read blueprint", response.Body.String())
	}
	for _, operation := range []string{"db:project-core-update", "db:blueprint-upsert", "db:revision-merge", "db:review-create"} {
		if response := rpc(tab, operation, []any{}); response.Code != http.StatusNotImplemented {
			t.Fatal("editor write bound", operation, response.Code, response.Body.String())
		}
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("editor reads changed author source", err)
	}
}

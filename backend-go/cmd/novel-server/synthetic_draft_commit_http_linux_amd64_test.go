//go:build linux && amd64

package main

import (
	"bytes"
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
)

func TestSyntheticDraftSaveRotatesLeaseAndReopenUsesPromotedKey(t *testing.T) {
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
	contract, err := admission.ParseContract(contractFile)
	_ = contractFile.Close()
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
	projectPath := filepath.Join(libraryDir, "synthetic-author")
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	const projectID = "8b5929e6-cda4-43e2-a15c-f232571f0036"
	manifest := `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"` + projectID + `","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(storage, projectfs.DatabaseName)
	if err := os.WriteFile(source, baseline, 0600); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Synthetic novel', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	inserted, err := db.Exec("INSERT INTO contents(body) VALUES(?)", "old draft")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	inserted, err = db.Exec("INSERT INTO drafts(chapter_number,version,status,content_id,word_count) VALUES(1,1,'draft',?,9)", contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := inserted.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
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
	if boot.Code != http.StatusOK {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootstrap struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootstrap); err != nil || bootstrap.CSRFToken == "" {
		t.Fatal(err, boot.Body.String())
	}
	cookie := boot.Result().Cookies()[0]
	rpc := func(operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"args": args})
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", "client_1234567890")
		request.Header.Set("X-CSRF-Token", bootstrap.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	opened := rpc("project:open", []any{projectPath, "open-a", nil})
	var openWire struct {
		Result struct {
			Project struct {
				ID    string `json:"id"`
				Lease string `json:"sessionLease"`
			} `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &openWire); err != nil || opened.Code != http.StatusOK || openWire.Result.Project.Lease == "" {
		t.Fatal(opened.Code, opened.Body.String(), err)
	}
	old := map[string]any{"projectId": projectID, "projectPath": projectPath, "leaseId": openWire.Result.Project.Lease}
	stale := rpc("db:draft-update-content", []any{draftID, "should not publish", 3, projectPath, "wrong baseline", old})
	if stale.Code != http.StatusOK || !bytes.Contains(stale.Body.Bytes(), []byte(`"errorCode":"DRAFT_CONTENT_CONFLICT"`)) {
		t.Fatal("stale baseline did not return conflict", stale.Code, stale.Body.String())
	}
	const changed = "new private words"
	saved := rpc("db:draft-update-content", []any{draftID, changed, 3, projectPath, "old draft", old})
	var saveWire struct {
		Result struct {
			Success        bool           `json:"success"`
			ProjectSession map[string]any `json:"projectSession"`
		} `json:"result"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &saveWire); err != nil || saved.Code != http.StatusOK || !saveWire.Result.Success {
		t.Fatal("verified synthetic save failed", saved.Code, saved.Body.String(), err)
	}
	fresh := saveWire.Result.ProjectSession
	if fresh["projectId"] != projectID || fresh["projectPath"] != projectPath || fresh["leaseId"] == old["leaseId"] {
		t.Fatal("save did not issue fresh project lease", fresh)
	}
	oldRead := rpc("db:draft-get-full", []any{draftID, projectPath, old})
	if oldRead.Code == http.StatusOK {
		t.Fatal("old lease still reads after save", oldRead.Body.String())
	}
	full := rpc("db:draft-get-full", []any{draftID, projectPath, fresh})
	if full.Code != http.StatusOK || !bytes.Contains(full.Body.Bytes(), []byte(changed)) {
		t.Fatal("new lease cannot read saved body", full.Code, full.Body.String())
	}
	reopened := rpc("project:open", []any{projectPath, "open-b", nil})
	if reopened.Code != http.StatusOK || !bytes.Contains(reopened.Body.Bytes(), []byte(`"dbReady":true`)) {
		t.Fatal("promoted catalog key cannot reopen", reopened.Code, reopened.Body.String())
	}
}

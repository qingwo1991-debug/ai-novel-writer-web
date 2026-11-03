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
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/authorruntime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectgrant"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func TestSyntheticHTTPGrantOpenRequiresTrustedOneUseIssue(t *testing.T) {
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
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(libraryDir)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	bridge, err := newSyntheticReadOpen(library, libraryDir, syntheticReadOpenConfig{Schema: contract})
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://127.0.0.1:16082"), "go-grant-test")
	defer server.Close()
	if err := server.BindProjectReadOpenWithGrants(bridge, bridge); err != nil {
		t.Fatal(err)
	}
	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrapRequest.Header.Set("X-Novel-User", "synthetic-author")
	bootstrap := httptest.NewRecorder()
	server.ServeHTTP(bootstrap, bootstrapRequest)
	if bootstrap.Code != http.StatusOK {
		t.Fatal(bootstrap.Code, bootstrap.Body.String())
	}
	var boot struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(bootstrap.Body.Bytes(), &boot); err != nil || boot.CSRFToken == "" {
		t.Fatal(err, bootstrap.Body.String())
	}
	cookie := bootstrap.Result().Cookies()[0]
	const tab = "client_1234567890"
	caller := transport.Caller{SessionID: cookie.Value, ClientID: tab}
	initial, err := bridge.OpenReadOnlyProject(context.Background(), caller, projectPath, "initial")
	opened, ok := initial.(authorruntime.ProjectOpenResult)
	if err != nil || !ok || !opened.Success {
		t.Fatal("trusted path setup failed", err)
	}
	project, err := library.OpenProject(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	grant, issueErr := bridge.grants.Issue(context.Background(), projectsession.Owner{SessionID: cookie.Value, ClientID: tab}, projectgrant.PurposeProjectOpen, project, time.Minute)
	closeErr = project.Close()
	if issueErr != nil || closeErr != nil {
		t.Fatal(issueErr, closeErr)
	}
	rpc := func(clientID, grantID, requestToken string) *httptest.ResponseRecorder {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"args": []any{map[string]any{"grantId": grantID, "displayName": "Synthetic novel"}, requestToken, nil}})
		request := httptest.NewRequest(http.MethodPost, "/api/operations/project:open", bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", clientID)
		request.Header.Set("X-CSRF-Token", boot.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		server.ServeHTTP(response, request)
		return response
	}
	if response := rpc("client_0987654321", grant, "foreign"); response.Code == http.StatusOK {
		t.Fatal("foreign tab opened grant", response.Body.String())
	}
	if response := rpc(tab, "forged", "forged"); response.Code == http.StatusOK {
		t.Fatal("forged grant opened project", response.Body.String())
	}
	if response := rpc(tab, grant, "valid"); response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"dbReady":true`)) {
		t.Fatal("trusted grant did not open", response.Code, response.Body.String())
	}
	if response := rpc(tab, grant, "replay"); response.Code == http.StatusOK {
		t.Fatal("consumed grant opened again", response.Body.String())
	}
}

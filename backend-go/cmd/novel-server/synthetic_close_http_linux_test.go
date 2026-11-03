//go:build linux

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

func TestSyntheticHTTPProjectCloseReopensExactReadLease(t *testing.T) {
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
	projectPath := filepath.Join(libraryDir, "synthetic-close-http")
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
	db, err := sql.Open("sqlite3", source)
	if err != nil {
		t.Fatal(err)
	}
	_, execErr := db.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Close HTTP', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	closeErr := db.Close()
	if execErr != nil || closeErr != nil {
		t.Fatal(execErr, closeErr)
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
	open := func(token string) map[string]any {
		t.Helper()
		response := rpc(tab, "project:open", []any{projectPath, token, nil})
		var envelope struct {
			Result struct {
				Success bool `json:"success"`
				Project struct {
					ID    string `json:"id"`
					Path  string `json:"path"`
					Lease string `json:"sessionLease"`
				} `json:"project"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || response.Code != http.StatusOK || !envelope.Result.Success || envelope.Result.Project.ID == "" || envelope.Result.Project.Path != projectPath || envelope.Result.Project.Lease == "" {
			t.Fatal("open failed", response.Code, response.Body.String(), err)
		}
		return map[string]any{"projectId": envelope.Result.Project.ID, "leaseId": envelope.Result.Project.Lease, "projectPath": projectPath}
	}
	first := open("close-open-first")
	read := func(expected map[string]any) *httptest.ResponseRecorder {
		return rpc(tab, "db:draft-list-all", []any{projectPath, expected})
	}
	if response := read(first); response.Code != http.StatusOK {
		t.Fatal("read before close failed", response.Code, response.Body.String())
	}
	if response := rpc("client_0987654321", "db:close", []any{projectPath, first}); response.Code == http.StatusOK {
		t.Fatal("foreign tab closed owner lease", response.Body.String())
	}
	if response := rpc(tab, "db:close", []any{filepath.Join(root, "other"), first}); response.Code != http.StatusConflict {
		t.Fatal("mismatched path closed lease", response.Code, response.Body.String())
	}
	if response := read(first); response.Code != http.StatusOK {
		t.Fatal("failed close removed owner lease", response.Code, response.Body.String())
	}
	closed := rpc(tab, "db:close", []any{projectPath, first})
	if closed.Code != http.StatusOK || !bytes.Contains(closed.Body.Bytes(), []byte(`"success":true`)) {
		t.Fatal("close not confirmed", closed.Code, closed.Body.String())
	}
	if response := read(first); response.Code == http.StatusOK {
		t.Fatal("closed lease remained readable", response.Body.String())
	}
	if response := rpc(tab, "db:close", []any{projectPath, first}); response.Code == http.StatusOK {
		t.Fatal("replayed close succeeded", response.Body.String())
	}
	second := open("close-open-second")
	if second["leaseId"] == first["leaseId"] {
		t.Fatal("reopen retained closed lease")
	}
	if response := read(second); response.Code != http.StatusOK {
		t.Fatal("reopened lease unreadable", response.Code, response.Body.String())
	}
	if response := rpc(tab, "project:save", []any{}); response.Code != http.StatusNotImplemented {
		t.Fatal("author save bound", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("close or reopen changed author source", err)
	}
}

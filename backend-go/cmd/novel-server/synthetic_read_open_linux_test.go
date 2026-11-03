//go:build linux

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestSyntheticProjectOpenUsesListedPathAndKeepsAuthorSource(t *testing.T) {
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
	projectPath := filepath.Join(libraryDir, "synthetic-author")
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
	if _, err := db.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Synthetic novel', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(source)
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
	if boot.Code != http.StatusOK {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootPayload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootPayload); err != nil || bootPayload.CSRFToken == "" {
		t.Fatal(err, boot.Body.String())
	}
	cookie := boot.Result().Cookies()[0]
	libraryRequest := httptest.NewRequest(http.MethodGet, "/api/library", nil)
	libraryRequest.Header.Set("X-Novel-User", "synthetic-author")
	libraryRequest.Header.Set("X-Client-Id", "client_1234567890")
	libraryRequest.AddCookie(cookie)
	library := httptest.NewRecorder()
	handler.ServeHTTP(library, libraryRequest)
	if library.Code != http.StatusOK || !strings.Contains(library.Body.String(), projectPath) || strings.Contains(library.Body.String(), `"key"`) {
		t.Fatal(library.Code, library.Body.String())
	}
	rpc := func(operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, _ := json.Marshal(map[string]any{"args": args})
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", "client_1234567890")
		request.Header.Set("X-CSRF-Token", bootPayload.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	opened := rpc("project:open", []any{projectPath, "open-a", "/untrusted/renderer"})
	if opened.Code != http.StatusOK || !strings.Contains(opened.Body.String(), `"dbReady":true`) || !strings.Contains(opened.Body.String(), `"sessionLease"`) || !strings.Contains(opened.Body.String(), projectPath) {
		t.Fatal(opened.Code, opened.Body.String())
	}
	unknown := rpc("project:open", []any{filepath.Join(root, "other"), "open-b", nil})
	if unknown.Code != http.StatusNotFound {
		t.Fatal(unknown.Code, unknown.Body.String())
	}
	save := rpc("project:save", []any{})
	if save.Code != http.StatusNotImplemented {
		t.Fatal(save.Code, save.Body.String())
	}
	if unexpected, err := writerlock.AcquireLegacy(root); !errors.Is(err, writerlock.ErrLocked) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		t.Fatal("read lock not retained", err)
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("author database changed", err)
	}
	cleanup()
	lock, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal("cleanup did not release lock", err)
	}
	_ = lock.Close()
}

func TestSyntheticReadOpenRejectsPublicOrAuthorRoot(t *testing.T) {
	for _, path := range []string{"/root/AI/ai-novel-writer-data/projects", filepath.Join(t.TempDir(), "projects")} {
		if path != "/root/AI/ai-novel-writer-data/projects" {
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
				t.Fatal(err)
			}
		}
		if privateSyntheticGlobalRoot(filepath.Dir(path)) && underTempRoot(path) {
			t.Fatal("unsafe root accepted", path)
		}
	}
}

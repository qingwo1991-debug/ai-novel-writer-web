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

func TestSyntheticEditorNavigationUsesCatalogAndExactReadLease(t *testing.T) {
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
	projectPath := filepath.Join(libraryDir, "synthetic-navigation")
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
	_, execErr := db.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Navigation', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`)
	closeErr := db.Close()
	if execErr != nil || closeErr != nil {
		t.Fatal(execErr, closeErr)
	}
	if err := os.Mkdir(filepath.Join(projectPath, "chapters"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "chapters", "one.txt"), []byte("one"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "notes.txt"), []byte("notes"), 0600); err != nil {
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
	if response := rpc(tab, "project:recent-list", []any{}); response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"result":[]`)) {
		t.Fatal("nonempty initial recent catalog", response.Code, response.Body.String())
	}
	opened := rpc(tab, "project:open", []any{projectPath, "navigation-open", nil})
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
	recent := rpc(tab, "project:recent-list", []any{})
	var recentWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(recent.Body.Bytes(), &recentWire); err != nil || recent.Code != http.StatusOK || len(recentWire.Result) != 1 || recentWire.Result[0]["path"] != projectPath || recentWire.Result[0]["key"] != nil {
		t.Fatal("catalog DTO mismatch", recent.Code, recent.Body.String(), err)
	}
	tree := rpc(tab, "fs:list-dir", []any{projectPath, projectPath, expected})
	var treeWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(tree.Body.Bytes(), &treeWire); err != nil || tree.Code != http.StatusOK || len(treeWire.Result) != 2 {
		t.Fatal("tree DTO mismatch", tree.Code, tree.Body.String(), err)
	}
	var sawFile, sawDir bool
	for _, node := range treeWire.Result {
		if node["name"] == "notes.txt" && node["path"] == filepath.Join(projectPath, "notes.txt") && node["isDir"] == false {
			sawFile = true
		}
		if node["name"] == "chapters" && node["path"] == filepath.Join(projectPath, "chapters") && node["isDir"] == true {
			children, ok := node["children"].([]any)
			if ok && len(children) == 1 && children[0].(map[string]any)["name"] == "one.txt" {
				sawDir = true
			}
		}
	}
	if !sawFile || !sawDir {
		t.Fatal("visible tree mismatch or hidden storage exposed", tree.Body.String())
	}
	if response := rpc(tab, "fs:list-dir", []any{filepath.Join(root, "outside"), projectPath, expected}); response.Code != http.StatusBadRequest {
		t.Fatal("outside path accepted", response.Code, response.Body.String())
	}
	if response := rpc(tab, "fs:list-dir", []any{projectPath, filepath.Join(root, "other"), expected}); response.Code != http.StatusConflict {
		t.Fatal("different root accepted", response.Code, response.Body.String())
	}
	if response := rpc("client_0987654321", "fs:list-dir", []any{projectPath, projectPath, expected}); response.Code == http.StatusOK {
		t.Fatal("foreign tab tree read accepted", response.Body.String())
	}
	if response := rpc(tab, "fs:write-file", []any{}); response.Code != http.StatusNotImplemented {
		t.Fatal("file write bound", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("navigation changed author source", err)
	}
}

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

func TestSyntheticCharacterReadsUseOpenedPrivateLeaseAndOriginalWire(t *testing.T) {
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
	projectPath := filepath.Join(libraryDir, "synthetic-characters")
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
	if _, err := db.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Characters', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00'); INSERT INTO characters(character_id,name,role) VALUES('c1','甲','protagonist')`); err != nil {
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
	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrapRequest.Header.Set("X-Novel-User", "synthetic-author")
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, bootstrapRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootPayload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootPayload); err != nil || bootPayload.CSRFToken == "" {
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
		request.Header.Set("X-CSRF-Token", bootPayload.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	const tab = "client_1234567890"
	opened := rpc(tab, "project:open", []any{projectPath, "characters-open", nil})
	if opened.Code != http.StatusOK {
		t.Fatal(opened.Code, opened.Body.String())
	}
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
	if err := json.Unmarshal(opened.Body.Bytes(), &openWire); err != nil || !openWire.Result.Success || openWire.Result.Project.ID == "" || openWire.Result.Project.Path != projectPath || openWire.Result.Project.Lease == "" {
		t.Fatal(err, opened.Body.String())
	}
	expected := map[string]any{"projectId": openWire.Result.Project.ID, "leaseId": openWire.Result.Project.Lease, "projectPath": projectPath}
	for _, operation := range []string{"db:character-get-all", "db:character-roster-read", "character-identity:read"} {
		args := []any{expected}
		if operation != "character-identity:read" {
			args = []any{projectPath, expected}
		}
		response := rpc(tab, operation, args)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		switch operation {
		case "db:character-get-all":
			var rows []map[string]any
			if err := json.Unmarshal(wire["result"], &rows); err != nil || len(rows) != 1 || rows[0]["name"] != "甲" {
				t.Fatal(operation, err, response.Body.String())
			}
		case "db:character-roster-read":
			var roster map[string]any
			if err := json.Unmarshal(wire["result"], &roster); err != nil || roster["entries"] == nil || roster["currentDerivedFields"] == nil {
				t.Fatal(operation, err, response.Body.String())
			}
			entries, ok := roster["entries"].([]any)
			if !ok || len(entries) != 1 || entries[0].(map[string]any)["name"] != "甲" {
				t.Fatal("roster did not read admitted snapshot", response.Body.String())
			}
		case "character-identity:read":
			var identity map[string]any
			if err := json.Unmarshal(wire["result"], &identity); err != nil || identity["characters"] == nil || identity["aliases"] == nil || identity["relationships"] == nil {
				t.Fatal(operation, err, response.Body.String())
			}
			characters, ok := identity["characters"].([]any)
			if !ok || len(characters) != 1 || characters[0].(map[string]any)["fields"].(map[string]any)["name"] != "甲" {
				t.Fatal("identity did not read admitted snapshot", response.Body.String())
			}
		}
	}
	foreign := rpc("client_0987654321", "db:character-get-all", []any{projectPath, expected})
	assertNotOwned := func(label string, response *httptest.ResponseRecorder) {
		t.Helper()
		var body struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusInternalServerError || body.Error.Code != "PROJECT_SESSION_NOT_OWNED" {
			t.Fatal(label, response.Code, response.Body.String(), err)
		}
	}
	assertNotOwned("foreign tab accepted", foreign)
	stale := map[string]any{"projectId": openWire.Result.Project.ID, "leaseId": "stale", "projectPath": projectPath}
	assertNotOwned("stale lease accepted", rpc(tab, "db:character-get-all", []any{projectPath, stale}))
	if response := rpc(tab, "db:character-roster-commit", []any{}); response.Code != http.StatusNotImplemented {
		t.Fatal("roster write bound", response.Code, response.Body.String())
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, original) {
		t.Fatal("character reads changed author source", err)
	}
}

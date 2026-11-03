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

func TestSyntheticDraftReadsKeepOpenedLeaseAndOriginalWire(t *testing.T) {
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
	projectPath := filepath.Join(libraryDir, "synthetic-drafts")
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
	if _, err := seed.Exec(`INSERT INTO project_core(id, project_name, writing_language, total_chapters, words_per_chapter, character_states, created_at, updated_at) VALUES('main', 'Drafts', 'zh-CN', 120, 2000, '{}', '2026-01-01 00:00:00', '2026-01-01 00:00:00')`); err != nil {
		t.Fatal(err)
	}
	content, err := seed.Exec("INSERT INTO contents(body) VALUES(?)", "保留正文🙂")
	if err != nil {
		t.Fatal(err)
	}
	contentID, err := content.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	var chapter int64
	if err := seed.QueryRow("SELECT COALESCE(MAX(chapter_number),0)+1 FROM drafts").Scan(&chapter); err != nil {
		t.Fatal(err)
	}
	draft, err := seed.Exec("INSERT INTO drafts(chapter_number,version,status,content_id) VALUES(?,1,'draft',?)", chapter, contentID)
	if err != nil {
		t.Fatal(err)
	}
	draftID, err := draft.LastInsertId()
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
	bootstrapRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootstrapRequest.Header.Set("X-Novel-User", "synthetic-author")
	boot := httptest.NewRecorder()
	handler.ServeHTTP(boot, bootstrapRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatal(boot.Code, boot.Body.String())
	}
	var bootstrap struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootstrap); err != nil || bootstrap.CSRFToken == "" {
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
		request.Header.Set("X-CSRF-Token", bootstrap.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	const tab = "client_1234567890"
	opened := rpc(tab, "project:open", []any{projectPath, "draft-open", nil})
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
	for _, item := range []struct {
		operation string
		number    int64
		kind      string
	}{
		{"db:draft-list", chapter, "list"}, {"db:draft-list-all", 0, "list"},
		{"db:draft-get-meta", draftID, "object"}, {"db:draft-get-full", draftID, "full"},
		{"db:draft-get-latest", chapter, "object"}, {"db:draft-get-finalized", chapter, "null"},
		{"db:draft-get-max-finalized-chapter", 0, "zero"}, {"db:draft-next-version", chapter, "two"},
	} {
		args := []any{projectPath, expected}
		if item.number != 0 {
			args = []any{item.number, projectPath, expected}
		}
		response := rpc(tab, item.operation, args)
		if response.Code != http.StatusOK {
			t.Fatalf("%s: HTTP %d %s", item.operation, response.Code, response.Body.String())
		}
		var wire map[string]json.RawMessage
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		switch item.kind {
		case "list":
			var rows []map[string]any
			if err := json.Unmarshal(wire["result"], &rows); err != nil || len(rows) != 1 || rows[0]["id"] != float64(draftID) {
				t.Fatal(item.operation, response.Body.String(), err)
			}
		case "object", "full":
			var row map[string]any
			if err := json.Unmarshal(wire["result"], &row); err != nil || row["id"] != float64(draftID) || (item.kind == "full" && row["content"] != "保留正文🙂") {
				t.Fatal(item.operation, response.Body.String(), err)
			}
		case "null":
			if string(wire["result"]) != "null" {
				t.Fatal(item.operation, response.Body.String())
			}
		case "zero":
			if string(wire["result"]) != "0" {
				t.Fatal(item.operation, response.Body.String())
			}
		case "two":
			if string(wire["result"]) != "2" {
				t.Fatal(item.operation, response.Body.String())
			}
		}
	}
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
	assertNotOwned("foreign tab", rpc("client_0987654321", "db:draft-get-full", []any{draftID, projectPath, expected}))
	stale := map[string]any{"projectId": openWire.Result.Project.ID, "leaseId": "stale", "projectPath": projectPath}
	assertNotOwned("stale lease", rpc(tab, "db:draft-get-full", []any{draftID, projectPath, stale}))
	malformedSave := rpc(tab, "db:draft-update-content", []any{})
	if malformedSave.Code != http.StatusOK || !bytes.Contains(malformedSave.Body.Bytes(), []byte(`"success":false`)) {
		t.Fatal("malformed synthetic save was not rejected", malformedSave.Code, malformedSave.Body.String())
	}
	for _, operation := range []string{"db:draft-create", "finalization:commit", "generation:confirm-batch-finalization"} {
		if response := rpc(tab, operation, []any{}); response.Code != http.StatusNotImplemented {
			t.Fatal("draft write bound", operation, response.Code, response.Body.String())
		}
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(after, before) {
		t.Fatal("draft reads changed author source", err)
	}
}

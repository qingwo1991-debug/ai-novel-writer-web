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
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// TestSyntheticDurableHTTPOpenSaveCloseReopenPersists drives the real browser
// project lifecycle protocol over the durable Go author runtime. It opens the
// one real synthetic project, rejects a stale frozen context before any write,
// saves a new projectName plus novelConfig, reads the live frozen context, and
// confirms the committed value survives db:close and a fresh project:open.
func TestSyntheticDurableHTTPOpenSaveCloseReopenPersists(t *testing.T) {
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
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	durableDir := filepath.Join(root, "durable-projects")
	if err := os.Mkdir(durableDir, 0700); err != nil {
		t.Fatal(err)
	}
	handler, cleanup, err := migrationHandlerWithDurable(migrationRegistry(t), "127.0.0.1:16082", "", durableDir, syntheticDurableConfig{
		Schema: contract, Baseline: filepath.Join(filepath.Dir(oracle), syntheticBaselineDatabaseName),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	projectPath := filepath.Join(durableDir, syntheticDurableProjectName)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	if _, err := os.Stat(database); err != nil {
		t.Fatalf("real synthetic project.db missing: %v", err)
	}
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
	const tab = "client_1234567890"
	rpc := func(operation string, args []any) *httptest.ResponseRecorder {
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
	// project:open returns the durable Open DTO backed by the author's own DB.
	opened := rpc("project:open", []any{projectPath, "durable-open-1", nil})
	var openWire struct {
		Result struct {
			Success           bool           `json:"success"`
			Project           map[string]any `json:"project"`
			RequestToken      string         `json:"requestToken"`
			ActiveProjectPath *string        `json:"activeProjectPath"`
			DatabaseRestored  bool           `json:"databaseRestored"`
			DBReady           bool           `json:"dbReady"`
		} `json:"result"`
	}
	if err := json.Unmarshal(opened.Body.Bytes(), &openWire); err != nil || opened.Code != http.StatusOK {
		t.Fatal("open decode failed", opened.Code, opened.Body.String(), err)
	}
	if !openWire.Result.Success || openWire.Result.Project == nil || openWire.Result.RequestToken != "durable-open-1" || !openWire.Result.DBReady || !openWire.Result.DatabaseRestored || openWire.Result.ActiveProjectPath == nil || *openWire.Result.ActiveProjectPath != projectPath {
		t.Fatal("open DTO malformed", opened.Body.String())
	}
	id, _ := openWire.Result.Project["id"].(string)
	name, _ := openWire.Result.Project["name"].(string)
	gotPath, _ := openWire.Result.Project["path"].(string)
	lease, _ := openWire.Result.Project["sessionLease"].(string)
	if id != syntheticDurableProjectID || gotPath != projectPath || name != "Synthetic durable novel" || lease == "" {
		t.Fatal("open project identity malformed", opened.Body.String())
	}
	expected := map[string]any{"projectId": id, "leaseId": lease, "projectPath": projectPath}
	// Editor reads run over the same durable runtime: project-core reflects the
	// author's own database right after the open admission.
	seededCore := rpc("db:project-core-get", []any{projectPath, expected})
	if seededCore.Code != http.StatusOK || !json.Valid(seededCore.Body.Bytes()) || !bytes.Contains(seededCore.Body.Bytes(), []byte(`"projectName":"Synthetic durable novel"`)) {
		t.Fatal("durable project-core read failed", seededCore.Code, seededCore.Body.String())
	}
	// The live frozen context matches the Open DTO exactly.
	contextRec := rpc("project:get-runtime-context", []any{})
	var contextWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(contextRec.Body.Bytes(), &contextWire); err != nil || contextRec.Code != http.StatusOK {
		t.Fatal("runtime context decode failed", contextRec.Code, contextRec.Body.String(), err)
	}
	if contextWire.Result["projectId"] != id || contextWire.Result["leaseId"] != lease || contextWire.Result["projectPath"] != projectPath {
		t.Fatal("frozen context mismatch", contextRec.Body.String())
	}
	// A stale frozen context is rejected before any write reaches the database.
	staleExpected := map[string]any{"projectId": id, "leaseId": "forged-lease", "projectPath": projectPath}
	stale := rpc("project:save", []any{id, map[string]any{"id": id, "sessionLease": "forged-lease", "path": projectPath, "name": "骗写"}, projectPath, staleExpected})
	var staleWire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stale.Body.Bytes(), &staleWire); err != nil || stale.Code != http.StatusOK {
		t.Fatal("stale save decode failed", stale.Code, stale.Body.String(), err)
	}
	if staleWire.Result.Success || !strings.Contains(staleWire.Result.Error, "PROJECT_SESSION_NOT_OWNED") {
		t.Fatal("stale frozen context was not rejected before the write", stale.Body.String())
	}
	if got := durableProjectName(t, database); got != "Synthetic durable novel" {
		t.Fatalf("stale save changed the author database: %q", got)
	}
	// project:save commits a new projectName to the author's own project.db.
	const savedName = "持久化新书名"
	saved := rpc("project:save", []any{id, map[string]any{
		"id": id, "sessionLease": lease, "path": projectPath, "name": savedName,
		"novelConfig": map[string]any{"writingLanguage": "zh-CN"},
	}, projectPath, expected})
	var saveWire struct {
		Result struct {
			Success bool           `json:"success"`
			Project map[string]any `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &saveWire); err != nil || saved.Code != http.StatusOK {
		t.Fatal("save decode failed", saved.Code, saved.Body.String(), err)
	}
	if !saveWire.Result.Success {
		t.Fatal("save reported failure", saved.Body.String())
	}
	if value, _ := saveWire.Result.Project["projectName"].(string); value != savedName {
		t.Fatalf("save result missing committed name: %q", saveWire.Result.Project)
	}
	if got := durableProjectName(t, database); got != savedName {
		t.Fatalf("save did not persist to the author database: %q", got)
	}
	// project:update-config commits a novelConfig field through the same write
	// path and the change is reflected in the author database.
	updated := rpc("project:update-config", []any{id, map[string]any{"novelConfig": map[string]any{"genre": "urban"}}, projectPath, expected})
	if err := json.Unmarshal(updated.Body.Bytes(), &saveWire); err != nil || updated.Code != http.StatusOK || !saveWire.Result.Success {
		t.Fatal("update-config failed", updated.Code, updated.Body.String(), err)
	}
	if got := durableProjectGenre(t, database); got != "urban" {
		t.Fatalf("update-config did not persist: %q", got)
	}
	// The durable lane's shared read surface serves editor reads from the same
	// author DB: the overview and project-core reads reflect the committed save
	// and config update before db:close.
	overviewRec := rpc("project:overview-current", []any{expected})
	var overviewWire struct {
		Result struct {
			State     string `json:"state"`
			ProjectID string `json:"projectId"`
			Revision  string `json:"revision"`
			Name      string `json:"name"`
		} `json:"result"`
	}
	if err := json.Unmarshal(overviewRec.Body.Bytes(), &overviewWire); err != nil || overviewRec.Code != http.StatusOK {
		t.Fatal("overview decode failed", overviewRec.Code, overviewRec.Body.String(), err)
	}
	if overviewWire.Result.State != "ready" || overviewWire.Result.ProjectID != id || overviewWire.Result.Name != savedName || len(overviewWire.Result.Revision) != 64 {
		t.Fatal("durable overview mismatch", overviewRec.Body.String())
	}
	coreRec := rpc("db:project-core-get", []any{projectPath, expected})
	if coreRec.Code != http.StatusOK || !json.Valid(coreRec.Body.Bytes()) || !bytes.Contains(coreRec.Body.Bytes(), []byte(`"projectName":"`+savedName+`"`)) || !bytes.Contains(coreRec.Body.Bytes(), []byte(`"genre":"urban"`)) {
		t.Fatal("durable project-core read missing committed data", coreRec.Code, coreRec.Body.String())
	}
	// Recent navigation exposes only the one synthetic project, never a key.
	recentRec := rpc("project:recent-list", []any{})
	var recentWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(recentRec.Body.Bytes(), &recentWire); err != nil || recentRec.Code != http.StatusOK || len(recentWire.Result) != 1 || recentWire.Result[0]["path"] != projectPath || recentWire.Result[0]["key"] != nil {
		t.Fatal("durable recent navigation mismatch", recentRec.Code, recentRec.Body.String(), err)
	}
	// db:close closes the admitted database before acknowledging.
	closed := rpc("db:close", []any{projectPath, expected})
	if closed.Code != http.StatusOK {
		t.Fatal("db:close failed", closed.Code, closed.Body.String())
	}
	// A fresh open reads back the committed name from the same project.db.
	reopened := rpc("project:open", []any{projectPath, "durable-open-2", nil})
	var reopenWire struct {
		Result struct {
			Success bool           `json:"success"`
			Project map[string]any `json:"project"`
		} `json:"result"`
	}
	if err := json.Unmarshal(reopened.Body.Bytes(), &reopenWire); err != nil || reopened.Code != http.StatusOK {
		t.Fatal("reopen decode failed", reopened.Code, reopened.Body.String(), err)
	}
	if !reopenWire.Result.Success || reopenWire.Result.Project == nil {
		t.Fatal("reopen failed", reopened.Body.String())
	}
	if value, _ := reopenWire.Result.Project["name"].(string); value != savedName {
		t.Fatalf("reopen lost committed update: %q", value)
	}
	if value, _ := reopenWire.Result.Project["path"].(string); value != projectPath {
		t.Fatalf("reopen changed project path: %q", value)
	}
}

func durableProjectName(t *testing.T, database string) string {
	t.Helper()
	var value string
	durableQuery(t, database, "SELECT project_name FROM project_core WHERE id='main'", &value)
	return value
}

func durableProjectGenre(t *testing.T, database string) string {
	t.Helper()
	var value string
	durableQuery(t, database, "SELECT genre FROM project_core WHERE id='main'", &value)
	return value
}

func durableQuery(t *testing.T, database, query string, target any) {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+database+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	err = db.QueryRow(query).Scan(target)
	closeErr := db.Close()
	if err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
}
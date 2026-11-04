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
	"strconv"
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
	// Seed the real current-schema rows this lane must serve: one working draft
	// body (the CAS target) and two characters. This runs before project:open so
	// the durable write admission fingerprints exactly the seeded database.
	draftID, originalBody := seedSyntheticDurableRows(t, database)
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
	// Character reads run directly on the admitted author database under the
	// same frozen cookie/tab/lease fence: both the full characters query and the
	// roster read return real rows written into this durable project.
	characterAll := rpc("db:character-get-all", []any{projectPath, expected})
	if characterAll.Code != http.StatusOK || !bytes.Contains(characterAll.Body.Bytes(), []byte(`"name":"甲"`)) {
		t.Fatal("durable character-get-all failed", characterAll.Code, characterAll.Body.String())
	}
	characterRoster := rpc("db:character-roster-read", []any{projectPath, expected})
	if characterRoster.Code != http.StatusOK || !bytes.Contains(characterRoster.Body.Bytes(), []byte(`"name":"乙"`)) || !bytes.Contains(characterRoster.Body.Bytes(), []byte(`"currentDerivedFields"`)) {
		t.Fatal("durable character-roster-read failed", characterRoster.Code, characterRoster.Body.String())
	}
	// The five post-process receipt and LLM ledger reads are served from the
	// same admitted author database under the same frozen lease. Absence is the
	// absent shape (null run, false aggregate) or an empty list, never an error.
	statsRec := rpc("db:get-llm-stats", []any{projectPath, expected})
	var statsWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(statsRec.Body.Bytes(), &statsWire); err != nil || statsRec.Code != http.StatusOK {
		t.Fatal("durable llm stats decode failed", statsRec.Code, statsRec.Body.String(), err)
	}
	for _, key := range []string{"totalCalls", "successfulCalls", "failedCalls", "knownUsageCalls", "totalTokens", "totalPromptTokens", "totalCompletionTokens"} {
		if _, ok := statsWire.Result[key]; !ok {
			t.Fatalf("durable llm stats missing %q: %s", key, statsRec.Body.String())
		}
	}
	if _, ok := statsWire.Result["totalCalls"].(float64); !ok {
		t.Fatal("durable llm stats totalCalls is not a number", statsRec.Body.String())
	}
	// An absent run and a zero history limit both resolve to a real empty list.
	for _, emptyRead := range []struct {
		name string
		args []any
	}{
		{"db:get-llm-history", []any{0, projectPath, expected}},
		{"db:post-process-get-steps", []any{"synthetic-durable-absent-run", projectPath, expected}},
	} {
		emptyRec := rpc(emptyRead.name, emptyRead.args)
		var emptyWire struct {
			Result []any `json:"result"`
		}
		if err := json.Unmarshal(emptyRec.Body.Bytes(), &emptyWire); err != nil || emptyRec.Code != http.StatusOK || emptyWire.Result == nil || len(emptyWire.Result) != 0 {
			t.Fatalf("durable %s should be an empty list: %d %s", emptyRead.name, emptyRec.Code, emptyRec.Body.String())
		}
	}
	for _, absentRead := range []struct {
		name string
		args []any
		want string
	}{
		{"db:post-process-get-latest-run", []any{"arch_extract", "synthetic-durable-absent-source", projectPath, expected}, "null"},
		{"db:post-process-is-all-passed", []any{"arch_extract", "synthetic-durable-absent-source", projectPath, expected}, "false"},
	} {
		absentRec := rpc(absentRead.name, absentRead.args)
		var absentWire struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(absentRec.Body.Bytes(), &absentWire); err != nil || absentRec.Code != http.StatusOK || string(absentWire.Result) != absentRead.want {
			t.Fatalf("durable %s absent shape mismatch: %d %s", absentRead.name, absentRec.Code, absentRec.Body.String())
		}
	}
	// The four mutating names in the same family stay unbound on this lane: the
	// registry exposes them, so the 501 is the durable binding, not a 404.
	unboundRec := rpc("db:post-process-create-run", []any{map[string]any{}, projectPath, expected})
	if unboundRec.Code != http.StatusNotImplemented || !strings.Contains(unboundRec.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatal("mutating ledger name was bound on the durable lane", unboundRec.Code, unboundRec.Body.String())
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
	// The durable lane commits a draft body CAS against the author's own
	// database: the stale-baseline rejection keeps the stored content unchanged,
	// then the matched CAS persists and rotates the frozen lease in place.
	var resultWire struct {
		Result map[string]any `json:"result"`
	}
	staleCAS := rpc("db:draft-update-content", []any{draftID, "replayed", 2, projectPath, "stale draft", expected})
	if err := json.Unmarshal(staleCAS.Body.Bytes(), &resultWire); err != nil || staleCAS.Code != http.StatusOK {
		t.Fatal("stale CAS decode failed", staleCAS.Code, staleCAS.Body.String(), err)
	}
	conflict, _ := resultWire.Result["errorCode"].(string)
	if success, _ := resultWire.Result["success"].(bool); success || conflict != "DRAFT_CONTENT_CONFLICT" {
		t.Fatal("stale draft CAS was not rejected", staleCAS.Body.String())
	}
	if got := durableDraftBody(t, database, draftID); got != originalBody {
		t.Fatalf("stale draft CAS changed stored content: %q", got)
	}
	const changedBody = "durable committed draft"
	committedDraft := rpc("db:draft-update-content", []any{draftID, changedBody, 5, projectPath, originalBody, expected})
	if err := json.Unmarshal(committedDraft.Body.Bytes(), &resultWire); err != nil || committedDraft.Code != http.StatusOK {
		t.Fatal("draft CAS decode failed", committedDraft.Code, committedDraft.Body.String(), err)
	}
	if success, _ := resultWire.Result["success"].(bool); !success {
		t.Fatal("draft CAS reported failure", committedDraft.Body.String())
	}
	session, _ := resultWire.Result["projectSession"].(map[string]any)
	fresh, _ := session["leaseId"].(string)
	if fresh == "" || fresh == lease || session["projectId"] != id || session["projectPath"] != projectPath {
		t.Fatal("draft CAS did not rotate the lease in place", committedDraft.Body.String())
	}
	freshSession := map[string]any{"projectId": id, "leaseId": fresh, "projectPath": projectPath}
	// The committed body is readable through the rotated lease both over the
	// real transport and by reading the SQLite file directly.
	full := rpc("db:draft-get-full", []any{draftID, projectPath, freshSession})
	if full.Code != http.StatusOK || !bytes.Contains(full.Body.Bytes(), []byte(`"content":"`+changedBody+`"`)) {
		t.Fatal("committed draft not readable over HTTP", full.Code, full.Body.String())
	}
	if got := durableDraftBody(t, database, draftID); got != changedBody {
		t.Fatalf("committed draft not visible in direct readback: %q", got)
	}
	if got := durableDraftWordCount(t, database, draftID); got != 5 {
		t.Fatalf("committed word count not persisted: %d", got)
	}
	// The rotated lease is the only live one: the pre-commit frozen context is
	// fenced out of every further operation.
	if old := rpc("db:draft-get-full", []any{draftID, projectPath, expected}); old.Code == http.StatusOK {
		t.Fatal("pre-commit lease still readable after the CAS", old.Body.String())
	}
	// Replaying the pre-commit baseline through the rotated lease is a stale
	// revision and must be rejected without touching the stored content.
	replayed := rpc("db:draft-update-content", []any{draftID, "again", 6, projectPath, originalBody, freshSession})
	if err := json.Unmarshal(replayed.Body.Bytes(), &resultWire); err != nil || replayed.Code != http.StatusOK {
		t.Fatal("replayed CAS decode failed", replayed.Code, replayed.Body.String(), err)
	}
	conflict, _ = resultWire.Result["errorCode"].(string)
	if success, _ := resultWire.Result["success"].(bool); success || conflict != "DRAFT_CONTENT_CONFLICT" {
		t.Fatal("replayed draft CAS was not rejected", replayed.Body.String())
	}
	if got := durableDraftBody(t, database, draftID); got != changedBody {
		t.Fatalf("replayed draft CAS changed stored content: %q", got)
	}
	// project:save still succeeds AFTER the draft CAS with the rotated lease:
	// the durable session was not downgraded by the commit.
	const savedNameAfterCAS = "草稿后书名"
	afterCAS := rpc("project:save", []any{id, map[string]any{
		"id": id, "sessionLease": fresh, "path": projectPath, "name": savedNameAfterCAS,
	}, projectPath, freshSession})
	if err := json.Unmarshal(afterCAS.Body.Bytes(), &saveWire); err != nil || afterCAS.Code != http.StatusOK {
		t.Fatal("save after draft CAS decode failed", afterCAS.Code, afterCAS.Body.String(), err)
	}
	if !saveWire.Result.Success {
		t.Fatal("save after draft CAS reported failure", afterCAS.Body.String())
	}
	if value, _ := saveWire.Result.Project["projectName"].(string); value != savedNameAfterCAS {
		t.Fatalf("save after draft CAS missing committed name: %q", saveWire.Result.Project)
	}
	if got := durableProjectName(t, database); got != savedNameAfterCAS {
		t.Fatalf("save after draft CAS did not persist: %q", got)
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
	closed := rpc("db:close", []any{projectPath, freshSession})
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
	if value, _ := reopenWire.Result.Project["name"].(string); value != savedNameAfterCAS {
		t.Fatalf("reopen lost committed update: %q", value)
	}
	if value, _ := reopenWire.Result.Project["path"].(string); value != projectPath {
		t.Fatalf("reopen changed project path: %q", value)
	}
}

// seedSyntheticDurableRows writes the real current-schema rows the durable
// lane must serve into the created synthetic project: one working draft body
// (the CAS target) and two characters. It runs before project:open so the write
// admission fingerprints exactly the seeded database.
func seedSyntheticDurableRows(t *testing.T, database string) (int64, string) {
	t.Helper()
	db, err := sql.Open("sqlite3", database)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	originalBody := "old durable draft"
	inserted, err := db.Exec("INSERT INTO contents(body) VALUES(?)", originalBody)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	contentID, err := inserted.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	inserted, err = db.Exec("INSERT INTO drafts(chapter_number, version, status, content_id, word_count) VALUES(1, 1, 'draft', ?, 9)", contentID)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	draftID, err := inserted.LastInsertId()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO characters(character_id, name, role) VALUES('c-durable', '甲', 'protagonist')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO characters(character_id, name, role) VALUES('c-durable-2', '乙', 'supporting')"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return draftID, originalBody
}

// durableDraftBody reads a draft body straight from the online database file.
func durableDraftBody(t *testing.T, database string, draftID int64) string {
	t.Helper()
	var value string
	durableQuery(t, database, "SELECT c.body FROM drafts d JOIN contents c ON c.id=d.content_id WHERE d.id="+strconv.FormatInt(draftID, 10), &value)
	return value
}

// durableDraftWordCount reads a draft's committed word count straight from the
// online database file.
func durableDraftWordCount(t *testing.T, database string, draftID int64) int64 {
	t.Helper()
	var value int64
	durableQuery(t, database, "SELECT word_count FROM drafts WHERE id="+strconv.FormatInt(draftID, 10), &value)
	return value
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
//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

func transportCallerForTest() transport.Caller {
	return transport.Caller{Principal: "synthetic-author", SessionID: "session", ClientID: productionTestTab}
}

// The synthetic data directory below is a disposable t.TempDir(): no real author
// data, and no path outside the test's own temporary tree, is ever touched.
const (
	productionTestGeneration = "3f1c8a90-6b2d-4e57-9c14-2a7b5d8e0f31"
	productionTestProjectID  = "d1ea93f0-3b7a-4c2b-9e5d-a1f2b3c4d5e6"
	productionTestOlderID    = "8c2b7d41-5a6e-4f39-bb02-1d9e4c7a3f58"
	productionTestUnknownID  = "b41f2c60-9d3a-4a18-8e77-5c0b1a6d2e94"
	productionTestTab        = "client_1234567890"
	// The committed generation carries two saved model profiles and a routing
	// assignment. The three llm reads have to echo exactly these, so a 200 that
	// returned null or an empty stage map cannot pass as a migrated read.
	productionTestModelID          = "ci-model-1"
	productionTestEmbeddingModelID = "ci-embedding-model-1"
	productionTestModelSecret      = "synthetic-production-model-key"
)

const (
	productionTestParentsDDL  = "CREATE TABLE parents (id INTEGER PRIMARY KEY, label TEXT NOT NULL)"
	productionTestChildrenDDL = "CREATE TABLE children (id INTEGER PRIMARY KEY, parent_id INTEGER REFERENCES parents(id), body TEXT)"
)

// productionTestContract builds the accepted schema independently of the
// verifier's sqlite_master reader, the same oracle the admission tests use. A
// wrong independent construction fails the assertions instead of silently
// accepting a database the lane should reject.
func productionTestContract() admission.SchemaContract {
	objects := []struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Table string `json:"tbl_name"`
		SQL   string `json:"sql"`
	}{
		{"table", "children", "children", productionTestChildrenDDL},
		{"table", "parents", "parents", productionTestParentsDDL},
	}
	encoded, err := json.Marshal(objects)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return admission.SchemaContract{{Version: 7, Fingerprints: []string{hex.EncodeToString(digest[:])}}}
}

// syntheticProductionDataDir writes the layout the shipped web gateway owns:
// one committed global generation behind a migration receipt, plus a library of
// three author projects. Only "current-novel" is a current-schema database.
func syntheticProductionDataDir(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	target := filepath.Join(root, productionConfigDirectory, productionConfigTarget)
	generation := filepath.Join(target, "generations", productionTestGeneration)
	for _, directory := range []string{
		filepath.Join(target, ".migration"),
		generation,
		filepath.Join(root, productionConfigDirectory, productionMigrationTarget),
		filepath.Join(root, productionLegacyDirectory),
		filepath.Join(root, productionLibraryDirectory),
	} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	receipt := `{"version":1,"generation":"` + productionTestGeneration +
		`","completed":true,"preservedUnknownCount":2,"requiredObjects":["config.json"]}`
	if err := os.WriteFile(filepath.Join(target, ".migration", "receipt.json"), []byte(receipt), 0600); err != nil {
		t.Fatal(err)
	}
	// The deployed extension key must never reach the browser through config:get.
	// defaultModelId, defaultEmbeddingModelId and stageModelIds are the committed
	// routing truth the llm reads project, so the fixture commits real ids instead
	// of nulls: a read that echoed a null would prove nothing about admission.
	committed := `{"theme":"light","editorFontSize":17,"defaultModelId":"` + productionTestModelID +
		`","defaultEmbeddingModelId":"` + productionTestEmbeddingModelID +
		`","stageModelIds":{"drafting":"` + productionTestModelID +
		`"},"deploymentExtension":{"hostRoot":"/srv/private"}}`
	if err := os.WriteFile(filepath.Join(generation, "config.json"), []byte(committed), 0600); err != nil {
		t.Fatal(err)
	}
	// models.json is a committed document of the generation (an optional one in
	// the pinned contract), so the private store exists the way it does on a real
	// author machine: one story profile and one embedding profile, 0600, with a
	// credential the browser projection has to withhold.
	models := `[{"id":"` + productionTestModelID +
		`","provider":"custom","protocol":"openai","baseUrl":"https://synthetic.invalid/v1","apiKey":"` + productionTestModelSecret +
		`","name":"合成正文模型","modelName":"ci-story-model","temperature":0.7,"maxTokens":1024},` +
		`{"id":"` + productionTestEmbeddingModelID +
		`","provider":"custom","protocol":"openai","baseUrl":"https://synthetic.invalid/v1","apiKey":"synthetic-production-embedding-key"` +
		`,"name":"合成向量模型","modelName":"ci-embedding-model"}]`
	if err := os.WriteFile(filepath.Join(generation, "models.json"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, "current-novel", productionTestProjectID, 7,
		[]string{productionTestParentsDDL, productionTestChildrenDDL, "INSERT INTO parents VALUES (7, '合成')"})
	seedProductionProject(t, library, "older-novel", productionTestOlderID, 6,
		[]string{productionTestParentsDDL, productionTestChildrenDDL})
	seedProductionProject(t, library, "unknown-novel", productionTestUnknownID, 7,
		[]string{"CREATE TABLE something_else (id INTEGER PRIMARY KEY)"})
	return root
}

func seedProductionProject(t *testing.T, library, name, projectID string, version int, statements []string) {
	t.Helper()
	projectPath := filepath.Join(library, name)
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"` + projectID +
		`","createdAt":"2024-05-06T07:08:09.000Z","storageFormat":"ai-novel","storageVersion":1}`
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	database := filepath.Join(storage, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rwc"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA user_version = " + strconv.Itoa(version)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestProductionLaneAdmitsOnlyCurrentSchemaAuthorDatabase proves the lane really
// opens and reads the author databases instead of trusting the directory shape:
// the two projects whose schema cannot be served are dropped, and the admitted
// one carries evidence read out of the staged database.
func TestProductionLaneAdmitsOnlyCurrentSchemaAuthorDatabase(t *testing.T) {
	root := syntheticProductionDataDir(t)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer open.Close()
	if len(open.projects) != 1 {
		t.Fatalf("admitted %d projects; want only the current-schema one", len(open.projects))
	}
	admitted := open.projects[0]
	if admitted.listed.Name != "current-novel" || admitted.listed.ProjectID != productionTestProjectID {
		t.Fatalf("wrong project admitted: %+v", admitted.listed)
	}
	if admitted.evidence.UserVersion != admission.CurrentSchemaVersion {
		t.Fatalf("admitted schema user_version = %d; want %d", admitted.evidence.UserVersion, admission.CurrentSchemaVersion)
	}
	if admitted.evidence.SchemaFingerprint != productionTestContract()[0].Fingerprints[0] {
		t.Fatalf("admitted schema fingerprint %q is not the trusted contract", admitted.evidence.SchemaFingerprint)
	}
	if admitted.evidence.TableCounts["parents"] != 1 || admitted.evidence.TableCounts["children"] != 0 {
		t.Fatalf("row evidence was not read from the database: %v", admitted.evidence.TableCounts)
	}
	if admitted.evidence.ContentIdentity.Database == "" {
		t.Fatal("admitted database has no content identity")
	}
	listed, err := open.ListCanonicalProjects(context.Background(), productionLibraryEntries)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Path != filepath.Join(root, productionLibraryDirectory, "current-novel") {
		t.Fatalf("library inventory leaked unadmitted projects: %v", listed)
	}
	recent, err := open.RecentProjects(context.Background(), transportCallerForTest())
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Path != filepath.Join(root, productionLibraryDirectory, "current-novel") {
		t.Fatalf("recent shelf leaked unadmitted projects: %v", recent)
	}
}

// TestProductionLaneFailsClosedWithoutCommittedGeneration proves a data root
// that is not a completed author data directory is refused instead of seeded.
func TestProductionLaneFailsClosedWithoutCommittedGeneration(t *testing.T) {
	layout, err := productionLayoutFor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := openProduction(context.Background(), layout, productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	}); err == nil {
		t.Fatal("an empty data root was accepted as an author data directory")
	}
	if _, _, err := productionHandlerWithContract(migrationRegistry(t), "127.0.0.1:16082", "", "/tmp/not-author-data", productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	}); err == nil {
		t.Fatal("a missing data root produced a serving handler")
	}
}

// TestProductionLaneServesStartupConfigAndLibrary drives the real browser
// envelope over a real (synthetic) data directory.
func TestProductionLaneServesStartupConfigAndLibrary(t *testing.T) {
	root := syntheticProductionDataDir(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), "127.0.0.1:16082", "", root, productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	boot := httptest.NewRecorder()
	bootRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootRequest.Header.Set("X-Novel-User", "synthetic-author")
	handler.ServeHTTP(boot, bootRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatalf("bootstrap failed: %d %s", boot.Code, boot.Body.String())
	}
	var bootWire struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &bootWire); err != nil || bootWire.CSRFToken == "" {
		t.Fatalf("bootstrap csrf: %v %s", err, boot.Body.String())
	}
	cookie := boot.Result().Cookies()[0]
	rpc := func(operation string, args []any) *httptest.ResponseRecorder {
		t.Helper()
		payload, err := json.Marshal(map[string]any{"args": args})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
		request.Header.Set("X-Novel-User", "synthetic-author")
		request.Header.Set("X-Client-Id", productionTestTab)
		request.Header.Set("X-CSRF-Token", bootWire.CSRFToken)
		request.Header.Set("Origin", "http://127.0.0.1:16082")
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		return response
	}
	startupState := func() map[string]any {
		t.Helper()
		response := rpc("startup:get-state", []any{})
		var wire struct {
			Result map[string]any `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK || wire.Result == nil {
			t.Fatalf("startup:get-state: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	state := startupState()
	if state["state"] != "ready" {
		t.Fatalf("startup:get-state is not ready over a committed data root: %v", state)
	}
	if state["globalGeneration"] != productionTestGeneration {
		t.Fatalf("startup:get-state reported generation %v", state["globalGeneration"])
	}
	notice, ok := state["migrationNotice"].(map[string]any)
	if !ok || notice["legacySourceIgnored"] != true || notice["preservedUnknownCount"] != float64(2) {
		t.Fatalf("startup:get-state did not report the receipt's migration facts: %v", state["migrationNotice"])
	}
	acknowledge := func(generation string, revision int) bool {
		t.Helper()
		response := rpc("startup:appearance-ack", []any{map[string]any{
			"storageKey": "ai-novel-writer-appearance-v3", "profileRevision": revision,
			"globalGeneration": generation,
		}})
		var wire struct {
			Result bool `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || response.Code != http.StatusOK {
			t.Fatalf("startup:appearance-ack: %d %s %v", response.Code, response.Body.String(), err)
		}
		return wire.Result
	}
	if !acknowledge(productionTestGeneration, 3) {
		t.Fatal("startup:appearance-ack refused the committed generation")
	}
	if state := startupState(); state["appearanceRevisionFloor"] != float64(3) {
		t.Fatalf("startup:appearance-ack was not retained: %v", state)
	}
	if acknowledge("another-generation", 4) {
		t.Fatal("startup:appearance-ack accepted an uncommitted generation")
	}
	configResponse := rpc("config:get", []any{})
	var configWire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(configResponse.Body.Bytes(), &configWire); err != nil || configResponse.Code != http.StatusOK || configWire.Result == nil {
		t.Fatalf("config:get: %d %s %v", configResponse.Code, configResponse.Body.String(), err)
	}
	if configWire.Result["theme"] != "light" || configWire.Result["editorFontSize"] != float64(17) {
		t.Fatalf("config:get did not read the committed generation: %v", configWire.Result)
	}
	if strings.Contains(configResponse.Body.String(), "deploymentExtension") || strings.Contains(configResponse.Body.String(), "/srv/private") {
		t.Fatalf("config:get leaked a non-public config field: %s", configResponse.Body.String())
	}
	recentResponse := rpc("project:recent-list", []any{})
	var recentWire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(recentResponse.Body.Bytes(), &recentWire); err != nil || recentResponse.Code != http.StatusOK {
		t.Fatalf("project:recent-list: %d %s %v", recentResponse.Code, recentResponse.Body.String(), err)
	}
	if len(recentWire.Result) != 1 {
		t.Fatalf("project:recent-list returned %d projects; want only the admitted one: %s", len(recentWire.Result), recentResponse.Body.String())
	}
	if recentWire.Result[0]["name"] != "current-novel" || recentWire.Result[0]["projectId"] != productionTestProjectID {
		t.Fatalf("project:recent-list entry is not the admitted author database: %v", recentWire.Result[0])
	}
	libraryRequest := httptest.NewRequest(http.MethodGet, "/api/library", nil)
	libraryRequest.Header.Set("X-Novel-User", "synthetic-author")
	libraryRequest.Header.Set("X-Client-Id", productionTestTab)
	libraryRequest.AddCookie(cookie)
	library := httptest.NewRecorder()
	handler.ServeHTTP(library, libraryRequest)
	var libraryWire struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(library.Body.Bytes(), &libraryWire); err != nil || library.Code != http.StatusOK {
		t.Fatalf("/api/library: %d %s %v", library.Code, library.Body.String(), err)
	}
	if len(libraryWire.Projects) != 1 || libraryWire.Projects[0]["name"] != "current-novel" {
		t.Fatalf("/api/library did not serve exactly the admitted author database: %s", library.Body.String())
	}
	// What this lane still does not own stays fail-closed over real data.
	for _, operation := range []string{"project:create"} {
		response := rpc(operation, []any{})
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s did not fail closed: %d %s", operation, response.Code, response.Body.String())
		}
	}
	// The project lifecycle and the draft surface are bound here now, so they must
	// be answered instead of 501. This project has no project_core row, so a
	// successful open would be a false ready-editor claim; the lane must refuse it
	// through the same envelope the browser already understands.
	openResponse := rpc("project:open", []any{filepath.Join(root, productionLibraryDirectory, "current-novel"), "open-token", nil})
	if openResponse.Code != http.StatusOK {
		t.Fatalf("project:open is not served by the production lane: %d %s", openResponse.Code, openResponse.Body.String())
	}
	var openWire struct {
		Result struct {
			Success bool   `json:"success"`
			Error   string `json:"error"`
		} `json:"result"`
	}
	if err := json.Unmarshal(openResponse.Body.Bytes(), &openWire); err != nil {
		t.Fatalf("project:open envelope: %v %s", err, openResponse.Body.String())
	}
	if openWire.Result.Success || openWire.Result.Error == "" {
		t.Fatalf("project:open reported a usable editor over a database with no project_core row: %s", openResponse.Body.String())
	}
	if response := rpc("db:draft-list", []any{}); response.Code != http.StatusBadRequest {
		t.Fatalf("db:draft-list is not served by the production lane: %d %s", response.Code, response.Body.String())
	}
}

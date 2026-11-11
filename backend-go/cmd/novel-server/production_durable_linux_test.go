//go:build linux

package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// The durable lane below runs against a disposable t.TempDir() only. No real
// author data root (and nothing under the operator's own data mount) is read or
// written by this test: the writer lease, the project database and the CAS proof
// all live inside the test's temporary tree.
const (
	productionDurableProjectName = "durable-novel"
	productionDurableProjectID   = "7f3a1b52-9c4d-4e0a-8b61-2d5c8e9f0a13"
	productionDurableTab         = "client_1234567890"
	productionDurableOtherTab    = "client_0987654321"
	productionDurableAddress     = "127.0.0.1:16083"
	productionDurableSeed        = "第一章原稿：他推开门。"
	productionDurableNext        = "第一章改稿：她推开门。"
)

// productionDurableSchema is the smallest schema the admitted runtime actually
// uses: the pinned v7 core row, one draft and its body, and the finalization
// receipt table the CAS fence counts. Every statement is written exactly as it
// must appear in sqlite_master, because the contract hashes that text.
var productionDurableSchema = []string{
	"CREATE TABLE contents (id INTEGER PRIMARY KEY, body TEXT)",
	"CREATE TABLE drafts (id INTEGER PRIMARY KEY, chapter_number INTEGER, version INTEGER, status TEXT, source TEXT, content_id INTEGER, word_count INTEGER, source_dependencies TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE finalization_outbox (draft_id INTEGER, chapter_title TEXT)",
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, created_at TEXT, updated_at TEXT)",
}

// productionSchemaEntry mirrors the admission fingerprint object byte for byte.
type productionSchemaEntry struct {
	Kind  string `json:"type"`
	Name  string `json:"name"`
	Table string `json:"tbl_name"`
	SQL   string `json:"sql"`
}

// productionDurableContract is the trusted contract for productionDurableSchema.
// The verifier hashes sqlite_master in (type, name) order, so the entries are
// sorted the same way instead of in DDL order.
func productionDurableContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionDurableSchema))
	for _, statement := range productionDurableSchema {
		name, _, found := strings.Cut(strings.TrimPrefix(statement, prefix), " ")
		if !found || name == "" || !strings.HasPrefix(statement, prefix) {
			t.Fatalf("unparseable schema statement: %s", statement)
		}
		entries = append(entries, productionSchemaEntry{Kind: "table", Name: name, Table: name, SQL: statement})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: []string{sha256Hex(string(encoded))}}}
}

// productionDurableDataRoot seeds a disposable data root whose only admitted
// project is one this lane can really open, read and write.
func productionDurableDataRoot(t *testing.T) (string, string) {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionDurableProjectName, productionDurableProjectID,
		admission.CurrentSchemaVersion, productionDurableSchema)
	projectPath := filepath.Join(library, productionDurableProjectName)
	seedProductionDurableRows(t, projectPath)
	return root, projectPath
}

func seedProductionDurableRows(t *testing.T, projectPath string) {
	t.Helper()
	statements := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, created_at, updated_at)
 VALUES ('main', 'Durable Novel', 'zh-CN', 'fantasy', '', '', 12, 3000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO contents (id, body) VALUES (1, ?)`, []any{productionDurableSeed}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'draft', 'write', 1, 11, '[]', '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
	}
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %q: %v", statement.query, err)
		}
	}
}

// productionDurableBody reads the draft body straight out of the author database
// on disk, so a save is only "persisted" when the file itself says so.
func productionDurableBody(t *testing.T, projectPath string) string {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	db.SetMaxOpenConns(1)
	var body string
	if err := db.QueryRow(`SELECT body FROM contents WHERE id = 1`).Scan(&body); err != nil {
		t.Fatalf("read persisted draft body: %v", err)
	}
	return body
}

// productionDurableClient is one browser tab: its own cookie session, its own
// CSRF token and its own X-Client-Id. Those two halves are its TAB IDENTITY
// (file grants, upload staging, generation receipts); the principal it bootstraps
// as is the ACCOUNT the production lane keys the project lease by, so two
// clients with the same principal are two tabs of one author that share the
// single live lease.
type productionDurableClient struct {
	handler   http.Handler
	principal string
	tab       string
	csrf      string
	cookie    *http.Cookie
}

func newProductionDurableClient(t *testing.T, handler http.Handler, tab string) *productionDurableClient {
	t.Helper()
	return newProductionDurableClientAs(t, handler, "production-author", tab)
}

// newProductionDurableClientAs boots a client for an explicit account, so a test
// can prove that a sibling tab of the same account shares the account lease while
// a different account is still refused.
func newProductionDurableClientAs(t *testing.T, handler http.Handler, principal, tab string) *productionDurableClient {
	t.Helper()
	boot := httptest.NewRecorder()
	bootRequest := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	bootRequest.Header.Set("X-Novel-User", principal)
	handler.ServeHTTP(boot, bootRequest)
	if boot.Code != http.StatusOK || len(boot.Result().Cookies()) != 1 {
		t.Fatalf("bootstrap failed: %d %s", boot.Code, boot.Body.String())
	}
	var wire struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(boot.Body.Bytes(), &wire); err != nil || wire.CSRFToken == "" {
		t.Fatalf("bootstrap csrf: %v %s", err, boot.Body.String())
	}
	return &productionDurableClient{handler: handler, principal: principal, tab: tab, csrf: wire.CSRFToken, cookie: boot.Result().Cookies()[0]}
}

func (c *productionDurableClient) call(t *testing.T, operation string, args []any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"args": args})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/operations/"+operation, bytes.NewReader(payload))
	request.Header.Set("X-Novel-User", c.principal)
	request.Header.Set("X-Client-Id", c.tab)
	request.Header.Set("X-CSRF-Token", c.csrf)
	request.Header.Set("Origin", "http://"+productionDurableAddress)
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(c.cookie)
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, request)
	return response
}

// decode returns the operation's result payload, or fails the test.
func (c *productionDurableClient) decode(t *testing.T, operation string, args []any, into any) {
	t.Helper()
	response := c.call(t, operation, args)
	if response.Code != http.StatusOK {
		t.Fatalf("%s: %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if err := json.Unmarshal(wire.Result, into); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
}

type productionDurableOpenWire struct {
	Success      bool           `json:"success"`
	Error        string         `json:"error"`
	DBReady      bool           `json:"dbReady"`
	Project      map[string]any `json:"project"`
	RequestToken string         `json:"requestToken"`
}

type productionDurableSaveWire struct {
	Success        bool           `json:"success"`
	Error          string         `json:"error"`
	ErrorCode      string         `json:"errorCode"`
	ProjectSession map[string]any `json:"projectSession"`
}

type productionDurableDraftWire struct {
	ID        int64  `json:"id"`
	Status    string `json:"status"`
	WordCount int64  `json:"wordCount"`
	Content   string `json:"content"`
}

// TestProductionLaneOpensProjectAndCommitsDraftCAS is the whole point of this
// lane: a real project is opened over the real project.db, its drafts are read,
// a save is committed with full-body CAS, and the result is durable on disk.
func TestProductionLaneOpensProjectAndCommitsDraftCAS(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	// Opening the data root took the exclusive writer lease, before any project
	// database was touched: a second cooperating writer cannot get it.
	if held, err := writerlock.AcquireLegacy(root); err == nil {
		_ = held.Close()
		t.Fatal("the production lane did not hold the exclusive writer lease at startup")
	} else if !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("writer lease probe failed with %v; want %v", err, writerlock.ErrLocked)
	}

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "open-token-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	if opened.Project["id"] != productionDurableProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}

	// The four operations this lane has to answer are bound, not 501.
	for _, operation := range []string{"db:draft-list", "db:draft-get-full", "db:draft-update-content"} {
		if response := author.call(t, operation, nil); response.Code == http.StatusNotImplemented {
			t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
		}
	}

	var drafts []productionDurableDraftWire
	author.decode(t, "db:draft-list", []any{int64(1), projectPath, frozen}, &drafts)
	if len(drafts) != 1 || drafts[0].ID != 1 || drafts[0].Content != "" {
		t.Fatalf("db:draft-list did not read the seeded draft metadata: %+v", drafts)
	}
	var full productionDurableDraftWire
	author.decode(t, "db:draft-get-full", []any{int64(1), projectPath, frozen}, &full)
	if full.Content != productionDurableSeed {
		t.Fatalf("db:draft-get-full returned %q; want the stored body", full.Content)
	}

	var saved productionDurableSaveWire
	author.decode(t, "db:draft-update-content", []any{int64(1), productionDurableNext, int64(12), projectPath, productionDurableSeed, frozen}, &saved)
	if !saved.Success || saved.Error != "" {
		t.Fatalf("db:draft-update-content refused a CAS save over a fresh lease: %s", saved.Error)
	}
	freshLease, _ := saved.ProjectSession["leaseId"].(string)
	if freshLease == "" || freshLease == lease || saved.ProjectSession["projectPath"] != projectPath {
		t.Fatalf("db:draft-update-content did not rotate the lease for the same project: %v", saved.ProjectSession)
	}
	updated := map[string]any{
		"projectId":   saved.ProjectSession["projectId"],
		"projectPath": saved.ProjectSession["projectPath"],
		"leaseId":     saved.ProjectSession["leaseId"],
	}
	if body := productionDurableBody(t, projectPath); body != productionDurableNext {
		t.Fatalf("the save did not persist into the real project database: %q", body)
	}
	author.decode(t, "db:draft-get-full", []any{int64(1), projectPath, updated}, &full)
	if full.Content != productionDurableNext || full.WordCount != 12 {
		t.Fatalf("the saved body did not read back under the new lease: %+v", full)
	}

	// A save replaying the lease the successful save already rotated away is
	// refused, and the body it tried to write never reaches the database.
	var stale productionDurableSaveWire
	author.decode(t, "db:draft-update-content", []any{int64(1), "stale write", int64(2), projectPath, productionDurableNext, frozen}, &stale)
	if stale.Success {
		t.Fatal("db:draft-update-content accepted a save under a rotated (stale) lease")
	}
	if body := productionDurableBody(t, projectPath); body != productionDurableNext {
		t.Fatalf("a refused stale-lease save still changed the database: %q", body)
	}

	// A stale baseline under the live lease is a CAS conflict, not a silent
	// overwrite: this is the invariant that protects a full body the editor
	// never read.
	var conflict productionDurableSaveWire
	author.decode(t, "db:draft-update-content", []any{int64(1), "lost update", int64(2), projectPath, productionDurableSeed, updated}, &conflict)
	if conflict.Success || conflict.ErrorCode != "DRAFT_CONTENT_CONFLICT" {
		t.Fatalf("db:draft-update-content did not refuse a stale baseline: %+v", conflict)
	}
	if body := productionDurableBody(t, projectPath); body != productionDurableNext {
		t.Fatalf("a refused CAS save still changed the database: %q", body)
	}

	// Two tabs, one author, one project. The lease is keyed by the authenticated
	// ACCOUNT now, so the second tab ADOPTS the live lease and is served instead
	// of being refused; the write safety between them is the CAS proved above,
	// not the lease. A different ACCOUNT is still refused, which is the security
	// boundary that sharing must not cross.
	other := newProductionDurableClient(t, handler, productionDurableOtherTab)
	var second productionDurableOpenWire
	other.decode(t, "project:open", []any{projectPath, "open-token-2", projectPath}, &second)
	if !second.Success || !second.DBReady || second.Error != "" {
		t.Fatalf("a sibling tab of the same account was refused the account lease: %+v", second)
	}
	// Both tabs resolve the SAME live lease, which is what "one writer per
	// account" means: neither holds a private copy of the project write handle.
	if second.Project["sessionLease"] != saved.ProjectSession["leaseId"] {
		t.Fatalf("a sibling tab was handed a different lease for the shared project: %+v vs %+v",
			second.Project["sessionLease"], saved.ProjectSession["leaseId"])
	}

	stranger := newProductionDurableClientAs(t, handler, "other-author", productionDurableOtherTab)
	var cross productionDurableOpenWire
	stranger.decode(t, "project:open", []any{projectPath, "open-token-3", projectPath}, &cross)
	if cross.Success || cross.Error != "PROJECT_OPEN_IN_ANOTHER_TAB" {
		t.Fatalf("a different account was not refused the project lease: %+v", cross)
	}
}

// TestProductionLaneReleasesWriterLeaseOnEveryPath proves the lease is not merely
// taken: a refused startup and a clean shutdown both hand it back, so a stale
// lease is never the reason a restart fails closed.
func TestProductionLaneReleasesWriterLeaseOnEveryPath(t *testing.T) {
	unprepared := t.TempDir()
	if _, _, err := productionHandlerWithContract(migrationRegistry(t), "127.0.0.1:16084", "", unprepared,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes}); err == nil {
		t.Fatal("an unprepared data root was admitted")
	}
	held, err := writerlock.AcquireLegacy(unprepared)
	if err != nil {
		t.Fatalf("the failed startup kept the writer lease: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}

	root, _ := productionDurableDataRoot(t)
	_, cleanup, err := productionHandlerWithContract(migrationRegistry(t), "127.0.0.1:16085", "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	cleanup()
	held, err = writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatalf("shutdown kept the writer lease: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

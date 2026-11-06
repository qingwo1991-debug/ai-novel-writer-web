//go:build linux

package main

// The portability family's decisive evidence. One disposable data root, one
// admitted book with a really finalized chapter, and the four served names driven
// over the frozen HTTP session the browser uses. The tests below prove, in order:
//
//   - project:peek-overview answers a ready overview and leaves the author's
//     database byte for byte identical (the read-only, query-only handle is the
//     property, not a promise in a comment);
//   - project:archive-export publishes one archive whose bytes on disk are the
//     bytes the receipt names, and browser:download-ready carries a URL the
//     download route really serves;
//   - project:archive-restore installs a copy whose project.db digest equals the
//     original's, while the original is untouched, and refuses to replace an
//     existing project or to write into a directory that already holds data;
//   - fs:list-dir holds the caller's lease and answers a project-relative tree.
//
// Nothing here reaches outside the fixture: every path is created by the test.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/archiveformat"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/librarypath"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	productionPortabilityFixtureTab    = "client_portability_author1"
	productionPortabilityFixtureAuthor = "production-author"
	productionPortabilityFixtureBook   = "便携之书"
	productionPortabilityFixtureOutput = "exports"
	productionPortabilityFixtureTarget = "还原副本"
	productionPortabilityFixtureLabel  = "第一章"
	productionPortabilityFixtureBody   = "第一章：他推开门，风从走廊尽头灌进来，带着雨的气味。"
	productionPortabilityFixtureFinal  = "finalization-0001"
)

// productionPortabilitySchema is the smallest schema this family can describe: the
// six tables the overview reads, plus the two the transfer receipt is made of. It
// is deliberately NOT the lane's production baseline - what matters is that the
// export describes the schema it was handed and the restore proves the same schema
// on the other side.
var productionPortabilitySchema = []string{
	"CREATE TABLE blueprints (chapter_number INTEGER)",
	"CREATE TABLE characters (id INTEGER PRIMARY KEY, retired INTEGER)",
	"CREATE TABLE contents (id INTEGER PRIMARY KEY, body TEXT)",
	"CREATE TABLE drafts (id INTEGER PRIMARY KEY, chapter_number INTEGER, version INTEGER, status TEXT, source TEXT, content_id INTEGER, word_count INTEGER, source_dependencies TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE finalization_outbox (finalization_id TEXT PRIMARY KEY, draft_id INTEGER, chapter_number INTEGER, content_hash TEXT, content_snapshot TEXT)",
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, writing_language TEXT, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, writing_style TEXT, reference_works TEXT, global_guidance TEXT, golden_finger TEXT, core_outline TEXT, world_setting TEXT, protagonist_profile TEXT, premise TEXT, worldbuilding TEXT, synopsis TEXT, character_states TEXT, characters_arch TEXT)",
	"CREATE TABLE reviews (id INTEGER PRIMARY KEY, base_draft_id INTEGER)",
	"CREATE TABLE summary_snapshots (id INTEGER PRIMARY KEY, draft_id INTEGER, chapter_number INTEGER, source_finalization_id TEXT, source_content_hash TEXT, projection_generation INTEGER)",
}

// productionPortabilityContract is the trusted contract for the schema above. The
// verifier hashes sqlite_master in (type, name) order, so the entries are sorted
// the same way instead of in DDL order.
func productionPortabilityContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionPortabilitySchema))
	for _, statement := range productionPortabilitySchema {
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

// productionPortabilityFixture is one open production lane over a disposable data
// root, with this family mounted where the browser mounts it: the pickers mint the
// grants, the transport serves the operations, and /api/downloads/ is answered by
// the file's own handler instead of the transport's 501.
type productionPortabilityFixture struct {
	server      *transport.Server
	handler     http.Handler
	open        *productionOpen
	portability *productionPortability
	client      *productionDurableClient
	config      productionConfig
	library     string
	project     string
	projectID   string
}

func newProductionPortabilityFixture(t *testing.T) *productionPortabilityFixture {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	projectID := "6f1c2a34-8b57-4d19-9a06-3c7e5b21d8f4"
	seedProductionProject(t, library, productionPortabilityFixtureBook, projectID,
		admission.CurrentSchemaVersion, productionPortabilitySchema)
	projectPath := filepath.Join(library, productionPortabilityFixtureBook)
	seedProductionPortabilityRows(t, projectPath)
	// The export writes into an output directory the picker grants. The picker is
	// allowed to mint a name that does not exist yet; the export is not allowed to
	// write into a directory it cannot see, so the fixture creates it.
	for _, directory := range []string{
		filepath.Join(library, productionPortabilityFixtureOutput),
		filepath.Join(projectPath, productionPortabilityFixtureLabel),
		filepath.Join(library, "杂物"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create %v: %v", directory, err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectPath, productionPortabilityFixtureLabel, "第一章.md"),
		[]byte(productionPortabilityFixtureBody), 0o600); err != nil {
		t.Fatalf("seed a chapter file: %v", err)
	}
	if err := os.WriteFile(filepath.Join(library, "杂物", "占位.txt"), []byte("existing author data"), 0o600); err != nil {
		t.Fatalf("seed a non-project directory: %v", err)
	}
	layout, layoutErr := productionLayoutFor(root)
	if layoutErr != nil {
		t.Fatal(layoutErr)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	config := productionConfig{Schema: productionPortabilityContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes}
	open, openErr := openProduction(startup, layout, config)
	if openErr != nil {
		t.Fatal(openErr)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-project-portability")
	portability := newProductionPortability(open, config)
	mux := http.NewServeMux()
	mux.Handle(productionPortabilityDownloadPrefix, portability.DownloadHandler())
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	mux.Handle("/", server)
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatalf("bind the project lifecycle: %v", bindErr)
	}
	pickers := newProductionBrowserPickers(open)
	if bindErr := pickers.Bind(server); bindErr != nil {
		t.Fatalf("bind the picker names: %v", bindErr)
	}
	if bindErr := portability.Bind(server); bindErr != nil {
		t.Fatalf("bind the portability names: %v", bindErr)
	}
	fixture := &productionPortabilityFixture{
		server:      server,
		handler:     mux,
		open:        open,
		portability: portability,
		config:      config,
		library:     library,
		project:     projectPath,
		projectID:   projectID,
	}
	fixture.client = newProductionDurableClient(t, mux, productionPortabilityFixtureTab)
	var opened productionDurableOpenWire
	fixture.client.decode(t, "project:open", []any{projectPath, "portability-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted project: %+v", opened)
	}
	return fixture
}

// seedProductionPortabilityRows puts one finalized, receipted chapter into the
// seeded database: the export has nothing portable to describe otherwise.
func seedProductionPortabilityRows(t *testing.T, projectPath string) {
	t.Helper()
	digest := sha256Hex(productionPortabilityFixtureBody)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	handle, openErr := sql.Open("sqlite3", uri.String())
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer func() {
		if closeErr := handle.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	}()
	handle.SetMaxOpenConns(1)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO project_core (id, project_name, genre, sub_genre, target_audience,
 total_chapters, words_per_chapter, writing_language, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, writing_style,
 reference_works, global_guidance, golden_finger, core_outline, world_setting,
 protagonist_profile, premise, worldbuilding, synopsis, character_states, characters_arch)
 VALUES ('main', '便携之书', '悬疑', '', '成人', 12, 3000, 'zh-CN', 'auto', 3,
 'three_act', 'third_limited', '', '', '', '', '大纲', '世界', '', '前提', '世界观',
 '梗概', '{}', '群像')`, nil},
		{`INSERT INTO blueprints (chapter_number) VALUES (1)`, nil},
		{`INSERT INTO contents (id, body) VALUES (1, ?)`, []any{productionPortabilityFixtureBody}},
		{`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (1, 1, 1, 'finalized', 'write', 1, 24, '[]',
 '2024-05-06 07:08:09', '2024-05-06 07:08:09')`, nil},
		{`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number,
 content_hash, content_snapshot) VALUES (?, 1, 1, ?, ?)`,
			[]any{productionPortabilityFixtureFinal, digest, productionPortabilityFixtureBody}},
		{`INSERT INTO summary_snapshots (id, draft_id, chapter_number,
 source_finalization_id, source_content_hash, projection_generation)
 VALUES (1, 1, 1, ?, ?, 1)`, []any{productionPortabilityFixtureFinal, digest}},
		{`INSERT INTO reviews (id, base_draft_id) VALUES (1, 1)`, nil},
		{`INSERT INTO characters (id, retired) VALUES (1, 0), (2, 1)`, nil},
	} {
		if _, err := handle.Exec(statement.query, statement.args...); err != nil {
			t.Fatalf("seed %v: %v", statement.query, err)
		}
	}
}

// reply is one call's status and decoded result, so a test can assert a symbolic
// refusal and a business envelope alike.
func (f *productionPortabilityFixture) reply(t *testing.T, operation string, args []any) (int, json.RawMessage) {
	t.Helper()
	response := f.client.call(t, operation, args)
	var body struct {
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%v: %v %s", operation, err, response.Body.String())
	}
	return response.Code, body.Result
}

// decode requires an HTTP 200 and decodes the operation's result.
func (f *productionPortabilityFixture) decode(t *testing.T, operation string, args []any, into any) {
	t.Helper()
	status, result := f.reply(t, operation, args)
	if status != http.StatusOK {
		t.Fatalf("%v: status %d", operation, status)
	}
	if len(result) == 0 {
		t.Fatalf("%v answered no result", operation)
	}
	if err := json.Unmarshal(result, into); err != nil {
		t.Fatalf("%v: %v %s", operation, err, string(result))
	}
}

// session reads the frozen project session the browser passes to every operation
// that writes or exports: the same {projectId, leaseId, projectPath} shape
// project:get-runtime-context answers with.
func (f *productionPortabilityFixture) session(t *testing.T) map[string]any {
	t.Helper()
	var frozen map[string]any
	f.decode(t, "project:get-runtime-context", []any{}, &frozen)
	for _, key := range []string{"projectId", "leaseId", "projectPath"} {
		value, ok := frozen[key].(string)
		if !ok || value == "" {
			t.Fatalf("project:get-runtime-context answered no %v", key)
		}
	}
	return frozen
}

// pick mints one grant for a library-relative name.
func (f *productionPortabilityFixture) pick(t *testing.T, operation, relative string) string {
	t.Helper()
	var answer struct {
		Success      bool   `json:"success"`
		GrantID      string `json:"grantId"`
		RelativePath string `json:"relativePath"`
		Error        string `json:"error"`
	}
	query := map[string]any{"rootId": librarypath.LibraryRootID, "relativePath": relative}
	f.decode(t, operation, []any{query}, &answer)
	if !answer.Success || answer.GrantID == "" {
		t.Fatalf("%v refused to mint a grant for %v: %s", operation, relative, answer.Error)
	}
	return answer.GrantID
}

// exportRequest is the archive the browser asked for: its receipt, the URL the
// download event carried, and the name on disk.
func (f *productionPortabilityFixture) export(t *testing.T) (archiveformat.ExportReceipt, string, string) {
	t.Helper()
	grant := f.pick(t, "dialog:select-project-archive-export", productionPortabilityFixtureOutput)
	var answer struct {
		Success bool                        `json:"success"`
		Error   string                      `json:"error"`
		Receipt archiveformat.ExportReceipt `json:"receipt"`
	}
	f.decode(t, "project:archive-export", []any{map[string]any{
		"targetArchiveGrantId": grant,
		"projectSession":       f.session(t),
	}}, &answer)
	if !answer.Success || answer.Error != "" {
		t.Fatalf("project:archive-export refused: %s", answer.Error)
	}
	download, name := f.downloadReady(t)
	return answer.Receipt, download, name
}

// downloadReady reads the browser's own channel: the export publishes
// browser:download-ready with the one URL that serves the archive, and the event
// stream replays it to a tab that connects afterwards.
func (f *productionPortabilityFixture) downloadReady(t *testing.T) (string, string) {
	t.Helper()
	live := httptest.NewServer(f.handler)
	defer live.Close()
	request, requestErr := http.NewRequest(http.MethodGet, live.URL+"/api/events?client="+url.QueryEscape(f.client.tab), nil)
	if requestErr != nil {
		t.Fatal(requestErr)
	}
	request.Header.Set("X-Novel-User", productionPortabilityFixtureAuthor)
	request.AddCookie(f.client.cookie)
	client := &http.Client{Timeout: 20 * time.Second}
	response, streamErr := client.Do(request)
	if streamErr != nil {
		t.Fatalf("open the event stream: %v", streamErr)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
		t.Fatalf("the event stream was refused with %d %s", response.StatusCode, string(body))
	}
	reader := bufio.NewReader(response.Body)
	for line := 0; line < 128; line++ {
		text, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("the event stream ended before a download was published: %v", readErr)
		}
		trimmed := strings.TrimSpace(text)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		var payload any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))), &payload); err != nil {
			continue
		}
		if download, name, found := productionPortabilityFindDownload(payload, 0); found {
			return download, name
		}
	}
	t.Fatal("the export never published a download URL")
	return "", ""
}

// productionPortabilityFindDownload walks one event payload for the download
// object. The channel carries the same two strings the desktop's preview does, and
// the walk keeps this test independent of how deeply the hub nests them.
func productionPortabilityFindDownload(payload any, depth int) (string, string, bool) {
	if depth > 6 {
		return "", "", false
	}
	switch value := payload.(type) {
	case map[string]any:
		download, downloadOK := value["url"].(string)
		name, nameOK := value["name"].(string)
		if downloadOK && nameOK && strings.HasPrefix(download, productionPortabilityDownloadPrefix) && name != "" {
			return download, name, true
		}
		for _, item := range value {
			if download, name, found := productionPortabilityFindDownload(item, depth+1); found {
				return download, name, true
			}
		}
	case []any:
		for _, item := range value {
			if download, name, found := productionPortabilityFindDownload(item, depth+1); found {
				return download, name, true
			}
		}
	}
	return "", "", false
}

// fetch performs one plain browser navigation: a GET with the tab's cookie and no
// CSRF token, which is exactly how a download link arrives.
func (f *productionPortabilityFixture) fetch(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Header.Set("X-Novel-User", productionPortabilityFixtureAuthor)
	request.AddCookie(f.client.cookie)
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	return recorder
}

func productionPortabilityFileDigest(t *testing.T, path string) string {
	t.Helper()
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read one file: %v", readErr)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

// productionPortabilityStorageFiles is every file in the project's storage
// directory with its digest: the before and after of "reads without writing".
func productionPortabilityStorageFiles(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatalf("read the storage directory: %v", readErr)
	}
	files := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			continue
		}
		files[entry.Name()] = productionPortabilityFileDigest(t, filepath.Join(directory, entry.Name()))
	}
	return files
}

// TestProductionPortabilityPeekReadsWithoutLeaseOrWrite proves the glance is a
// read of the author's own bytes: the overview is ready, and the storage directory
// is byte for byte identical afterwards, with no new log, journal or sidecar.
func TestProductionPortabilityPeekReadsWithoutLeaseOrWrite(t *testing.T) {
	fixture := newProductionPortabilityFixture(t)
	storage := filepath.Join(fixture.project, projectfs.StorageDirectory)
	before := productionPortabilityStorageFiles(t, storage)

	var overview struct {
		State             string `json:"state"`
		ProjectID         string `json:"projectId"`
		Name              string `json:"name"`
		Revision          string `json:"revision"`
		FinalizedChapters int   `json:"finalizedChapters"`
		PlannedChapters   int64 `json:"plannedChapters"`
		Stages            []struct {
			ID    string `json:"id"`
			Count int    `json:"count"`
		} `json:"stages"`
	}
	fixture.decode(t, productionPortabilityPeekName, []any{fixture.projectID}, &overview)
	if overview.State != "ready" || overview.ProjectID != fixture.projectID {
		t.Fatalf("the glance answered %v for %v", overview.State, overview.ProjectID)
	}
	if overview.Name != productionPortabilityFixtureBook {
		t.Fatalf("the glance named %v", overview.Name)
	}
	if len(overview.Stages) != 6 {
		t.Fatalf("the glance carried %d stage facts", len(overview.Stages))
	}
	if overview.Stages[5].ID != "finalization" || overview.FinalizedChapters != 1 {
		t.Fatalf("the glance did not see the finalized chapter: %+v", overview.Stages)
	}
	if len(overview.Revision) != 64 {
		t.Fatalf("the glance carried no revision digest")
	}

	after := productionPortabilityStorageFiles(t, storage)
	if len(after) != len(before) {
		t.Fatalf("the glance changed the file set: %d files before, %d after", len(before), len(after))
	}
	for name, digest := range before {
		held, present := after[name]
		if !present {
			t.Fatalf("%v disappeared during the glance", name)
		}
		if held != digest {
			t.Fatalf("%v changed during the glance", name)
		}
	}

	// An identity this lane never admitted is unanswerable, and says only that.
	for _, identity := range []string{"未知", "", fixture.projectID + "x"} {
		var silent map[string]any
		fixture.decode(t, productionPortabilityPeekName, []any{identity}, &silent)
		if silent["state"] != "unavailable" || len(silent) != 1 {
			t.Fatalf("the glance answered %v for an unknown identity", silent)
		}
	}
	if held := productionPortabilityStorageFiles(t, storage); len(held) != len(before) {
		t.Fatal("a refused glance wrote to the author's database")
	}
}

// TestProductionPortabilityArchiveRoundTrip is the decisive test: one export of a
// seeded book, one restore of that export into a fresh destination, and two equal
// digests with the original untouched.
func TestProductionPortabilityArchiveRoundTrip(t *testing.T) {
	fixture := newProductionPortabilityFixture(t)
	source := filepath.Join(fixture.project, projectfs.StorageDirectory, projectfs.DatabaseName)
	sourceDigest := productionPortabilityFileDigest(t, source)

	receipt, download, name := fixture.export(t)
	if receipt.OriginProjectID != fixture.projectID {
		t.Fatalf("the receipt named %v as the origin", receipt.OriginProjectID)
	}
	if receipt.EntryCount != 3 || !receipt.RequiresRuntimeFreezeGuard {
		t.Fatalf("the receipt described %d entries (guard=%v)", receipt.EntryCount, receipt.RequiresRuntimeFreezeGuard)
	}
	if receipt.SnapshotGeneration != sourceDigest {
		t.Fatalf("the receipt generation is not the database digest")
	}
	if !strings.HasSuffix(name, ".anovel") {
		t.Fatalf("the published archive is named %v", name)
	}
	archivePath := filepath.Join(fixture.library, productionPortabilityFixtureOutput, name)

	// What was published is what the receipt promised.
	info, statErr := os.Stat(archivePath)
	if statErr != nil {
		t.Fatalf("the export published nothing: %v", statErr)
	}
	if uint64(info.Size()) != receipt.TargetByteSize {
		t.Fatalf("the published archive is %d bytes, the receipt named %d", info.Size(), receipt.TargetByteSize)
	}
	if held := productionPortabilityFileDigest(t, archivePath); held != receipt.TargetSHA256 {
		t.Fatalf("the published archive digest is not the receipt's")
	}

	// And it reads back as an archive of this family: three entries, a receipt id,
	// and the ledger of what a portable copy does not carry.
	opened, openErr := os.Open(archivePath)
	if openErr != nil {
		t.Fatal(openErr)
	}
	archive, inspectErr := archiveformat.Inspect(opened, info.Size(), archiveformat.DefaultLimits())
	closeErr := opened.Close()
	if inspectErr != nil || closeErr != nil {
		t.Fatalf("the published archive did not inspect: %v", inspectErr)
	}
	manifest := archive.Manifest()
	if len(manifest.Entries) != 3 || manifest.SourceSchemaVersion != 7 {
		t.Fatalf("the archive carried %d entries at schema %d", len(manifest.Entries), manifest.SourceSchemaVersion)
	}
	if manifest.OriginProjectID != fixture.projectID || manifest.SnapshotGeneration != sourceDigest {
		t.Fatalf("the archive manifest does not bind this project's bytes")
	}
	if len(manifest.TransferReceiptIDs) != 1 || manifest.TransferReceiptIDs[0] == "" {
		t.Fatal("the archive manifest named no transfer receipt")
	}
	if len(manifest.OmittedItems) == 0 {
		t.Fatal("the archive manifest claimed to omit nothing")
	}
	// Exactly the three entries a restorable copy has, each under the disposition
	// this family writes for it.
	dispositions := map[string]string{
		productionPortabilityDatabaseEntry:  productionPortabilityDatabaseDisposition,
		productionPortabilityFreezeEntry:    productionPortabilityFreezeDisposition,
		productionPortabilityAuthorityEntry: productionPortabilityAuthorityDisposition,
	}
	for _, entry := range manifest.Entries {
		want, known := dispositions[entry.Path]
		if !known || entry.Disposition != want {
			t.Fatalf("the archive carried an unexpected entry")
		}
		if uint64(len(entry.SHA256)) != 64 {
			t.Fatalf("an archive entry carried no digest")
		}
	}

	// The download route serves exactly those bytes, with the headers a browser
	// needs to save them under the same name.
	downloadResponse := fixture.fetch(t, download)
	if downloadResponse.Code != http.StatusOK {
		t.Fatalf("the download was refused with %d %s", downloadResponse.Code, downloadResponse.Body.String())
	}
	if held := sha256Hex(downloadResponse.Body.String()); held != receipt.TargetSHA256 {
		t.Fatal("the download route served different bytes than the receipt named")
	}
	if disposition := downloadResponse.Header().Get("Content-Disposition"); !strings.Contains(disposition, name) {
		t.Fatalf("the download did not name the archive: %v", disposition)
	}
	if downloadResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("the download was cacheable")
	}

	// The export never touched the author's own database.
	if productionPortabilityFileDigest(t, source) != sourceDigest {
		t.Fatal("the export changed the author's database")
	}

	// The restore: a fresh directory, the archive the browser holds, one receipt.
	targetGrant := fixture.pick(t, "dialog:select-project-restore-target", productionPortabilityFixtureTarget)
	archiveGrant := fixture.pick(t, "dialog:select-project-archive",
		productionPortabilityFixtureOutput+"/"+name)
	var restored struct {
		Success         bool           `json:"success"`
		Error           string         `json:"error"`
		ErrorCode       string         `json:"errorCode"`
		State           string         `json:"state"`
		RecentProjectOK bool           `json:"recentProjectUpdated"`
		Receipt         map[string]any `json:"receipt"`
	}
	fixture.decode(t, productionPortabilityRestoreName, []any{map[string]any{
		"archiveGrantId": archiveGrant,
		"targetGrantId":  targetGrant,
	}}, &restored)
	if !restored.Success || restored.Error != "" {
		t.Fatalf("the restore refused: %v %v", restored.Error, restored.ErrorCode)
	}
	if restored.State != "restore-complete" {
		t.Fatalf("the restore answered %v", restored.State)
	}

	restoredRoot := filepath.Join(fixture.library, productionPortabilityFixtureTarget)
	restoredDatabase := filepath.Join(restoredRoot, projectfs.StorageDirectory, projectfs.DatabaseName)
	restoredDigest := productionPortabilityFileDigest(t, restoredDatabase)
	if restoredDigest != sourceDigest {
		t.Fatalf("the restored database is not the original's bytes")
	}
	// The original is still the original, and the archive is still there.
	if productionPortabilityFileDigest(t, source) != sourceDigest {
		t.Fatal("the restore changed the original project")
	}
	if _, statErr := os.Stat(archivePath); statErr != nil {
		t.Fatal("the restore consumed the archive it was given")
	}
	// The copy is an admitted project in its own right, without the frozen
	// authority this lane deliberately does not install: a project that carried
	// installed authority would be unopenable here.
	evidence, verifyErr := admission.VerifySnapshot(context.Background(), restoredDatabase, fixture.config.Schema)
	if verifyErr != nil {
		t.Fatalf("the restored database did not verify: %v", verifyErr)
	}
	if evidence.UserVersion != admission.CurrentSchemaVersion || evidence.ContentIdentity.Database != sourceDigest {
		t.Fatal("the restored database verified as a different database")
	}
	if _, statErr := os.Stat(filepath.Join(restoredRoot, projectfs.StorageDirectory, "portable-transfer-authority.json")); statErr == nil {
		t.Fatal("the restore installed the portable authority it refuses to run")
	}
	if _, statErr := os.Stat(filepath.Join(restoredRoot, projectfs.StorageDirectory, "portable-runtime-freeze.json")); statErr == nil {
		t.Fatal("the restore installed the runtime freeze it cannot replay")
	}
	receiptProject, _ := restored.Receipt["targetProjectId"].(string)
	if receiptProject == "" || receiptProject == fixture.projectID {
		t.Fatalf("the restore receipt named %v as the target", receiptProject)
	}
	if digest, _ := restored.Receipt["portableDatabaseSha256"].(string); digest != sourceDigest {
		t.Fatal("the restore receipt does not bind the installed bytes")
	}
}

// TestProductionPortabilityRestoreRefusesAuthorData proves the restore never
// replaces what the author already has: an existing project is copied first and
// then refused, and a directory that is not a project is refused untouched.
func TestProductionPortabilityRestoreRefusesAuthorData(t *testing.T) {
	fixture := newProductionPortabilityFixture(t)
	_, _, name := fixture.export(t)
	archiveGrant := fixture.pick(t, "dialog:select-project-archive",
		productionPortabilityFixtureOutput+"/"+name)

	// restore asks the operation the browser asks and answers with its refusal.
	restore := func(t *testing.T, archiveGrantID, targetGrantID string) (bool, string) {
		t.Helper()
		var answer struct {
			Success   bool   `json:"success"`
			ErrorCode string `json:"errorCode"`
		}
		fixture.decode(t, productionPortabilityRestoreName, []any{map[string]any{
			"archiveGrantId": archiveGrantID,
			"targetGrantId":  targetGrantID,
		}}, &answer)
		return answer.Success, answer.ErrorCode
	}

	// 1. The book itself. The answer is a refusal, and the book is intact.
	source := filepath.Join(fixture.project, projectfs.StorageDirectory, projectfs.DatabaseName)
	sourceDigest := productionPortabilityFileDigest(t, source)
	existing := fixture.pick(t, "dialog:select-project-restore-target", productionPortabilityFixtureBook)
	if success, code := restore(t, archiveGrant, existing); success || code != productionPortabilityCodeRestoreExists {
		t.Fatalf("the restore answered %v for an existing project", code)
	}
	if productionPortabilityFileDigest(t, source) != sourceDigest {
		t.Fatal("a refused restore changed the existing project")
	}

	// 2. A directory that holds something else. Nothing in it is touched.
	occupied := filepath.Join(fixture.library, "杂物")
	placeholder := filepath.Join(occupied, "占位.txt")
	before := productionPortabilityFileDigest(t, placeholder)
	busy := fixture.pick(t, "dialog:select-project-restore-target", "杂物")
	if success, code := restore(t, archiveGrant, busy); success || code != productionPortabilityCodeRestoreUnsafe {
		t.Fatalf("the restore answered %v for an occupied directory", code)
	}
	if productionPortabilityFileDigest(t, placeholder) != before {
		t.Fatal("a refused restore wrote into an occupied directory")
	}
	if _, statErr := os.Stat(filepath.Join(occupied, projectfs.DatabaseName)); statErr == nil {
		t.Fatal("a refused restore left a database behind")
	}

	// 3. A grant this lane never minted is a refusal, not a path.
	fresh := fixture.pick(t, "dialog:select-project-restore-target", productionPortabilityFixtureTarget+"2")
	if success, code := restore(t, "grant-that-was-never-minted", fresh); success || code != productionPortabilityCodeGrant {
		t.Fatalf("the restore answered %v for an unknown grant", code)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.library, productionPortabilityFixtureTarget+"2", projectfs.StorageDirectory)); statErr == nil {
		t.Fatal("a refused restore created a project tree")
	}
}

// TestProductionPortabilityServesEditorNavigation proves the other half of the
// name set: the shelf and the project tree the editor needs before it can draw
// anything.
func TestProductionPortabilityServesEditorNavigation(t *testing.T) {
	fixture := newProductionPortabilityFixture(t)
	var recent []struct {
		Name      string `json:"name"`
		Path      string `json:"path"`
		ProjectID string `json:"projectId"`
	}
	fixture.decode(t, "project:recent-list", []any{}, &recent)
	found := false
	for _, item := range recent {
		if item.ProjectID == fixture.projectID || item.Path == fixture.project {
			found = true
		}
	}
	if !found {
		t.Fatalf("the shelf did not carry the admitted project")
	}
	var nodes []struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		IsDir bool   `json:"isDir"`
	}
	fixture.decode(t, productionPortabilityListName,
		[]any{fixture.project, fixture.project, fixture.session(t)}, &nodes)
	if len(nodes) == 0 {
		t.Fatal("the project tree answered nothing")
	}
	label := false
	for _, node := range nodes {
		if node.Name == productionPortabilityFixtureLabel {
			label = node.IsDir
		}
		if filepath.IsAbs(node.Path) && !strings.HasPrefix(node.Path, fixture.project) {
			t.Fatalf("the tree carried a path outside the project")
		}
	}
	if !label {
		t.Fatalf("the project tree did not carry the chapter directory")
	}
}

// TestProductionPortabilityUnboundNamesNameTheirMissingPort proves the two refused
// names are refused for a reason, and that the four served names are not on that
// list.
func TestProductionPortabilityUnboundNamesNameTheirMissingPort(t *testing.T) {
	unbound := productionProjectPortabilityUnbound()
	for _, name := range []string{productionPortabilityLegacyCopyName, productionPortabilityNovelFilesName} {
		port, held := unbound[name]
		if !held || strings.TrimSpace(port) == "" {
			t.Fatalf("%v is reported unbound without naming the port it lacks", name)
		}
	}
	if len(unbound) != 2 {
		t.Fatalf("the unbound list carries %d names", len(unbound))
	}
	for _, name := range []string{productionPortabilityPeekName, productionPortabilityExportName,
		productionPortabilityRestoreName, productionPortabilityListName} {
		if port, held := unbound[name]; held {
			t.Fatalf("%v is served but reported unbound (%v)", name, port)
		}
	}
}

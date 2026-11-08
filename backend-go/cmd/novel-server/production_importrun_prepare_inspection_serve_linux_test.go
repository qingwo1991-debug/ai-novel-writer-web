//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/importsource"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/uploadstore"
)

// db:import-run-prepare-inspection is served now, so this file walks the whole
// browser journey instead of arguing about a refusal:
//
//	POST /api/uploads                 the author's manuscript is staged
//	dialog:select-novel-files         the served picker freezes it and answers an id
//	db:import-run-author-preview      the served preview answers the two fingerprints
//	db:import-run-prepare-inspection  the author run is created
//	mode=ro connection                the run and its forty chapters are re-read
//
// # Why this test builds the lane itself
//
// productionHandlerWithContract (production_open_linux.go) binds the names its own
// `for _, bind := range []func() error{` slice names, and this change may not edit
// that file. The three names are therefore installed on a transport.Server this test
// constructs over the SAME openProduction result and the SAME durable bridge, in the
// order the wiring uses: the picker over one importsource.Store, the serve adapter
// over that same store, and the finalized-draft binder over the adapter. Nothing
// else is bound but the project lifecycle and the upload route, so an accidentally
// unbound name would fail the first assertion instead of passing on an empty
// envelope.
//
// # What each test proves
//
//   - one staged forty-chapter manuscript becomes an author-commit run whose forty
//     chapter rows, manifest fingerprint, authority fingerprint and inference ledger
//     row are read back over a SEPARATE mode=ro connection;
//   - a foreign project and a stale lease answer the same status, headers and bytes,
//     and neither spends the receipt (the live request after them still succeeds);
//   - a wrong-CAS consume is refused without consuming, and the served preview and
//     prepare that follow still resolve the same receipt;
//   - the production wiring the served name needs is present in
//     production_open_linux.go, exactly once, and it replaced the closure-local
//     construction instead of joining it.
//
// Word counts, digests and chapter numbers are never guessed: the preview's
// fingerprints are what the run must freeze, and every chapter row is compared with
// the staged body it came from.

const (
	productionPrepareInspectionProjectName = "prepare-inspection-novel"
	productionPrepareInspectionProjectID   = "5d2b8c41-7e63-4a19-9f04-8c6a1e7b3d99"
	productionPrepareInspectionNonce       = "production-prepare-inspection-open-1"
	productionPrepareInspectionStaleLease  = "stale-prepare-inspection-0001"
	productionPrepareInspectionRunID       = "production-prepare-inspection-run-1"
	productionPrepareInspectionChapters    = 40
	productionPrepareInspectionSourceName  = "全稿.txt"
)

// productionPrepareInspectionSchema is the smallest author schema these three names
// really touch: the pinned core row project:open reads, the drafts and their bodies
// the preview classifies, the outbox it reads a title from, and the import tables the
// author prepare writes or reads inside its transaction. Every statement is written
// exactly as it must appear in sqlite_master, because the contract hashes that text,
// and none of them contains a character the contract encoder escapes.
var productionPrepareInspectionSchema = []string{
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, premise TEXT, worldbuilding TEXT, synopsis TEXT, characters_arch TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE contents (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL DEFAULT '', created_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE drafts (id INTEGER PRIMARY KEY AUTOINCREMENT, chapter_number INTEGER NOT NULL, version INTEGER NOT NULL, status TEXT DEFAULT 'draft', source TEXT DEFAULT 'write', content_id INTEGER NOT NULL, word_count INTEGER DEFAULT 0, source_dependencies TEXT NOT NULL DEFAULT '[]', created_at TEXT DEFAULT (datetime('now')), updated_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE finalization_outbox (finalization_id TEXT PRIMARY KEY, draft_id INTEGER NOT NULL UNIQUE, chapter_number INTEGER NOT NULL, chapter_title TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL, content_revision INTEGER NOT NULL, content_snapshot TEXT NOT NULL DEFAULT '', target_file_name TEXT NOT NULL, knowledge_document_id TEXT NOT NULL DEFAULT '', publication_status TEXT NOT NULL DEFAULT 'pending', last_error TEXT NOT NULL DEFAULT '', published_at TEXT DEFAULT NULL, created_at TEXT DEFAULT (datetime('now')), updated_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE import_runs (id TEXT PRIMARY KEY, purpose TEXT NOT NULL DEFAULT '', root_run_id TEXT NOT NULL DEFAULT '', effect_namespace TEXT NOT NULL DEFAULT '', source_fingerprint TEXT NOT NULL DEFAULT '', manifest_fingerprint TEXT NOT NULL DEFAULT '', authority_fingerprint TEXT NOT NULL DEFAULT '', legacy_source_fingerprint TEXT NOT NULL DEFAULT '', source_display_json TEXT NOT NULL DEFAULT '[]', locale TEXT NOT NULL DEFAULT 'zh-CN', stage TEXT NOT NULL DEFAULT 'prepared', status TEXT NOT NULL DEFAULT 'ready', completed_batches_json TEXT NOT NULL DEFAULT '{}', last_error TEXT NOT NULL DEFAULT '', resumable INTEGER NOT NULL DEFAULT 1, cancel_requested INTEGER NOT NULL DEFAULT 0, execution_owner TEXT NOT NULL DEFAULT '', execution_epoch INTEGER NOT NULL DEFAULT 0, lease_expires_at INTEGER NOT NULL DEFAULT 0, total_chapters INTEGER NOT NULL DEFAULT 0, total_content_size INTEGER NOT NULL DEFAULT 0, manifest_chapter_count INTEGER NOT NULL DEFAULT 0, manifest_content_size INTEGER NOT NULL DEFAULT 0, manifest_word_count INTEGER NOT NULL DEFAULT 0, completed_chapters INTEGER NOT NULL DEFAULT 0, base_run_id TEXT, created_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:11', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:12', completed_at TEXT)",
	"CREATE TABLE import_run_sources (run_id TEXT NOT NULL, source_index INTEGER NOT NULL, source_id TEXT NOT NULL, source_fingerprint TEXT NOT NULL, display_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL, chapter_count INTEGER NOT NULL DEFAULT 0, content_size INTEGER NOT NULL DEFAULT 0, word_count INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:13', PRIMARY KEY (run_id, source_index))",
	"CREATE TABLE import_run_chapters (run_id TEXT NOT NULL, chapter_number INTEGER NOT NULL, source_id TEXT NOT NULL DEFAULT '', source_chapter_number INTEGER NOT NULL DEFAULT 0, title TEXT NOT NULL DEFAULT '', content_fingerprint TEXT NOT NULL DEFAULT '', content_size INTEGER NOT NULL DEFAULT 0, content_snapshot TEXT NOT NULL DEFAULT '', PRIMARY KEY (run_id, chapter_number))",
	"CREATE TABLE import_effect_ledger (run_id TEXT NOT NULL, stage TEXT NOT NULL, batch_id TEXT NOT NULL, effect_namespace TEXT NOT NULL DEFAULT '', effect_key TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, evidence_kind TEXT NOT NULL DEFAULT '', diagnostic_code TEXT NOT NULL DEFAULT '', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:14', PRIMARY KEY (run_id, stage, batch_id))",
	"CREATE TABLE import_run_receipts (run_id TEXT NOT NULL, schema_version INTEGER NOT NULL DEFAULT 1, effect_namespace TEXT NOT NULL DEFAULT '', effect_key TEXT NOT NULL DEFAULT '', stage TEXT NOT NULL, batch_id TEXT NOT NULL, kind TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', payload_hash TEXT NOT NULL DEFAULT '', state TEXT NOT NULL, effect_receipt_json TEXT, created_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:15', updated_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:16', PRIMARY KEY (run_id, stage, batch_id))",
	"CREATE TABLE import_source_aliases (alias_digest TEXT PRIMARY KEY, alias_kind TEXT NOT NULL, source_id TEXT NOT NULL, created_at TEXT NOT NULL DEFAULT '2024-05-06 09:10:17')",
}

// productionPrepareInspectionSeedCore is the one project_core row project:open
// requires, copied column for column from the import-run write fixture.
const productionPrepareInspectionSeedCore = `INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, premise, worldbuilding, synopsis, characters_arch,
 created_at, updated_at)
 VALUES ('main', '准备检查小说', 'zh-CN', 'fantasy', '', '', 40, 2000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '', '', '', '',
 '2024-05-06 09:10:11', '2024-05-06 09:10:12')`

// productionPrepareInspectionContract is the trusted contract for the schema above.
// The verifier hashes sqlite_master in (type, name) order, so the entries are sorted
// the same way rather than in DDL order.
func productionPrepareInspectionContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionPrepareInspectionSchema))
	for _, statement := range productionPrepareInspectionSchema {
		name, _, found := strings.Cut(strings.TrimPrefix(statement, prefix), " ")
		if !found || name == "" || !strings.HasPrefix(statement, prefix) {
			t.Fatalf("unparseable schema statement: %s", statement)
		}
		entries = append(entries, productionSchemaEntry{Kind: "table", Name: name, Table: name, SQL: statement})
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].Name < entries[right].Name })
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion,
		Fingerprints: []string{sha256Hex(string(encoded))}}}
}

// productionPrepareInspectionLane is one open production lane with the upload route
// mounted and the three names bound over one shared receipt store.
type productionPrepareInspectionLane struct {
	open      *productionOpen
	route     *productionUploadRoute
	mux       http.Handler
	server    *transport.Server
	selection *productionImportSelection
	adapter   *productionImportRunPrepareInspectionServe
	project   string
	database  string
	frozen    map[string]any
	author    *productionDurableClient
}

// newProductionPrepareInspectionLane seeds the disposable project, opens the real
// lane over it and installs exactly the three binders this name depends on.
func newProductionPrepareInspectionLane(t *testing.T) *productionPrepareInspectionLane {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionPrepareInspectionProjectName,
		productionPrepareInspectionProjectID, admission.CurrentSchemaVersion,
		productionPrepareInspectionSchema)
	projectPath := filepath.Join(library, productionPrepareInspectionProjectName)
	productionPrepareInspectionSeed(t, projectPath)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: productionPrepareInspectionContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-import-run-prepare-inspection")
	route, err := newProductionUploadRoute(open, server, productionDurableAddress)
	if err != nil {
		_ = open.Close()
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	if mountErr := route.Mount(mux); mountErr != nil {
		_ = route.Close()
		_ = open.Close()
		t.Fatal(mountErr)
	}
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	mux.Handle("/", server)
	t.Cleanup(func() {
		server.Close()
		if closeErr := route.Close(); closeErr != nil {
			t.Error(closeErr)
		}
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	selection, err := newProductionImportSelection(open, route)
	if err != nil {
		t.Fatalf("build the import selection: %v", err)
	}
	if bindErr := selection.Bind(server); bindErr != nil {
		t.Fatalf("bind dialog:select-novel-files: %v", bindErr)
	}
	adapter := newProductionImportRunPrepareInspectionServe(open.durable, selection)
	if bindErr := adapter.Bind(server); bindErr != nil {
		t.Fatalf("bind db:import-run-prepare-inspection: %v", bindErr)
	}
	if bindErr := newProductionFinalizedDraftImports(open.durable, adapter).Bind(server); bindErr != nil {
		t.Fatalf("bind db:import-run-author-preview: %v", bindErr)
	}
	author, frozen := productionExecuteSession(t, mux, projectPath, productionPrepareInspectionNonce)
	return &productionPrepareInspectionLane{open: open, route: route, mux: mux, server: server,
		selection: selection, adapter: adapter, project: projectPath,
		database: filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		frozen:   frozen, author: author}
}

// productionPrepareInspectionSeed writes the core row an imported project must have
// and folds the write-ahead log into the admitted file, so the bytes the lane admits
// are the bytes this test compares later.
func productionPrepareInspectionSeed(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(productionPrepareInspectionSeedCore); err != nil {
		_ = db.Close()
		t.Fatalf("seed the project core row: %v", err)
	}
	productionEditorCheckpoint(t, db, database)
}

// productionPrepareInspectionManuscript is one staged manuscript of the requested
// chapter count, in the heading shape importsource.Parse reads.
func productionPrepareInspectionManuscript(chapters int) string {
	var manuscript strings.Builder
	for number := 1; number <= chapters; number++ {
		fmt.Fprintf(&manuscript, "第%d章 标题%d\n\n%s\n\n", number, number,
			productionPrepareInspectionChapterBody(number))
	}
	return manuscript.String()
}

// productionPrepareInspectionChapterBody is the body the parser freezes for one
// chapter: the heading line is the title, the paragraph is the content.
func productionPrepareInspectionChapterBody(number int) string {
	return fmt.Sprintf("这是第%d章的正文内容。", number)
}

// request is the four-key display intent the renderer sends
// (src/components/dialogs/ImportNovelDialog.tsx:214-219). It cannot name a file.
func (l *productionPrepareInspectionLane) request() map[string]any {
	return map[string]any{
		"runId":               productionPrepareInspectionRunID,
		"purpose":             string(importsource.AuthorManuscript),
		"locale":              "zh-CN",
		"expectedProjectPath": l.frozen["projectPath"],
	}
}

// stage uploads one manuscript through the mounted route with the headers the
// renderer sets (src/components/BrowserDialogs.tsx:92-95) and the session cookie, so
// the staged id belongs to the cookie session the calls below run under.
func (l *productionPrepareInspectionLane) stage(t *testing.T, name, body string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, uploadstore.RouteFile, bytes.NewReader([]byte(body)))
	request.Header.Set("X-Novel-User", "production-author")
	request.Header.Set("X-Client-Id", l.author.tab)
	request.Header.Set("X-CSRF-Token", l.author.csrf)
	request.Header.Set("Origin", "http://"+productionDurableAddress)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Upload-Name", url.PathEscape(name))
	request.AddCookie(l.author.cookie)
	response := httptest.NewRecorder()
	l.mux.ServeHTTP(response, request)
	return productionUploadTestCreated(t, response, "uploadId")
}

// selectInspection runs the served picker and returns the inspection id it minted.
func (l *productionPrepareInspectionLane) selectInspection(t *testing.T) string {
	t.Helper()
	response := l.author.call(t, "dialog:select-novel-files", []any{l.request(), l.frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("the picker refused the staged manuscript: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "previewCAS") {
		t.Fatal("the picker put the preview CAS on the wire; it is a server-side handoff")
	}
	var wire productionImportWire
	if err := decodeProductionPrepareInspection(response.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if !wire.Success || wire.Inspection.InspectionID == "" {
		t.Fatalf("the picker did not mint an inspection: %s", response.Body.String())
	}
	if wire.Inspection.ChapterCount != productionPrepareInspectionChapters {
		t.Fatalf("the picker froze %d chapters, want %d", wire.Inspection.ChapterCount,
			productionPrepareInspectionChapters)
	}
	return wire.Inspection.InspectionID
}

// preview runs the served author preview's id form.
func (l *productionPrepareInspectionLane) preview(t *testing.T,
	inspectionID string) productionPrepareInspectionPreview {
	t.Helper()
	var preview productionPrepareInspectionPreview
	response := l.author.call(t, "db:import-run-author-preview",
		[]any{inspectionID, l.project, l.frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("db:import-run-author-preview refused a minted id: %d %s",
			response.Code, response.Body.String())
	}
	if err := decodeProductionPrepareInspection(response.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Classification != "ready" || preview.ChapterCount != productionPrepareInspectionChapters {
		t.Fatalf("the preview classified %v with %d chapters: %s",
			preview.Classification, preview.ChapterCount, response.Body.String())
	}
	if len(preview.AuthorityFingerprint) != 64 || len(preview.ManifestFingerprint) != 64 {
		t.Fatalf("the preview did not answer both fingerprints: %s", response.Body.String())
	}
	if len(preview.NewChapterNumbers) != productionPrepareInspectionChapters {
		t.Fatalf("the preview offered %d new chapters, want %d",
			len(preview.NewChapterNumbers), productionPrepareInspectionChapters)
	}
	return preview
}

// prepareRequest is ImportRunPrepareFromInspectionRequest's author arm
// (src/shared/import-run.ts:296-304).
func (l *productionPrepareInspectionLane) prepareRequest(inspectionID, runID string,
	preview productionPrepareInspectionPreview) map[string]any {
	return map[string]any{
		"inspectionId":         inspectionID,
		"runId":                runID,
		"purpose":              string(importsource.AuthorManuscript),
		"locale":               "zh-CN",
		"authorityFingerprint": preview.AuthorityFingerprint,
		"manifestFingerprint":  preview.ManifestFingerprint,
		"authorInference":      true,
	}
}

// prepare runs the served prepare and returns its decoded envelope.
func (l *productionPrepareInspectionLane) prepare(t *testing.T, inspectionID, runID string,
	preview productionPrepareInspectionPreview) productionPrepareInspectionPreparation {
	t.Helper()
	response := l.author.call(t, productionImportRunPrepareInspectionOperation,
		[]any{l.prepareRequest(inspectionID, runID, preview), l.project, l.frozen})
	if response.Code != http.StatusOK {
		t.Fatalf("db:import-run-prepare-inspection answered %d %s",
			response.Code, response.Body.String())
	}
	var preparation productionPrepareInspectionPreparation
	if err := decodeProductionPrepareInspection(response.Body.Bytes(), &preparation); err != nil {
		t.Fatalf("%v: %s", err, response.Body.String())
	}
	return preparation
}

// binding is the receipt binding the picker minted for this caller and generation
// (internal/transport/import_selection.go:104-105).
func (l *productionPrepareInspectionLane) binding() importsource.Binding {
	return importsource.Binding{
		Caller:     l.author.cookie.Value + ":" + l.author.tab,
		Generation: syntheticString(l.frozen["projectId"]) + ":" + syntheticString(l.frozen["leaseId"]),
		Purpose:    importsource.AuthorManuscript,
	}
}

// productionPrepareInspectionPreview is AuthorManuscriptImportPreview, limited to the
// fields this file asserts.
type productionPrepareInspectionPreview struct {
	Classification          string  `json:"classification"`
	AuthorityFingerprint    string  `json:"authorityFingerprint"`
	ManifestFingerprint     string  `json:"manifestFingerprint"`
	ChapterCount            int     `json:"chapterCount"`
	NewChapterNumbers       []int64 `json:"newChapterNumbers"`
	DuplicateChapterNumbers []int64 `json:"duplicateChapterNumbers"`
	ConflictChapterNumbers  []int64 `json:"conflictChapterNumbers"`
}

// productionPrepareInspectionPreparation is ImportRunPrepareFromInspectionResult.
type productionPrepareInspectionPreparation struct {
	Success     bool   `json:"success"`
	ErrorCode   string `json:"errorCode"`
	Preparation struct {
		Classification          string         `json:"classification"`
		Run                     map[string]any `json:"run"`
		NewChapterNumbers       []int64        `json:"newChapterNumbers"`
		ConflictChapterNumbers  []int64        `json:"conflictChapterNumbers"`
		DuplicateChapterNumbers []int64        `json:"duplicateChapterNumbers"`
	} `json:"preparation"`
}

// decodeProductionPrepareInspection unwraps the transport's {result} envelope.
func decodeProductionPrepareInspection(body []byte, into any) error {
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("the answer carries no result")
	}
	return json.Unmarshal(envelope.Result, into)
}

// productionPrepareInspectionReadOnly is a SEPARATE mode=ro connection, so nothing
// this file asserts about the committed rows is read back through the lane that
// wrote them.
func productionPrepareInspectionReadOnly(t *testing.T, database string) *sql.DB {
	t.Helper()
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	handle, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	handle.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if closeErr := handle.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return handle
}

// TestProductionImportRunPrepareInspectionCreatesTheAuthorRun is the whole success
// journey: stage, select, preview, prepare, and the run read back over mode=ro.
func TestProductionImportRunPrepareInspectionCreatesTheAuthorRun(t *testing.T) {
	lane := newProductionPrepareInspectionLane(t)
	lane.stage(t, productionPrepareInspectionSourceName,
		productionPrepareInspectionManuscript(productionPrepareInspectionChapters))
	inspectionID := lane.selectInspection(t)
	preview := lane.preview(t, inspectionID)
	prepared := lane.prepare(t, inspectionID, productionPrepareInspectionRunID, preview)
	if !prepared.Success {
		t.Fatalf("the author prepare refused a live receipt: %v", prepared.ErrorCode)
	}
	if prepared.Preparation.Classification != "new" {
		t.Fatalf("the author run was classified %v, want new", prepared.Preparation.Classification)
	}
	run := prepared.Preparation.Run
	if run == nil || syntheticString(run["id"]) != productionPrepareInspectionRunID {
		t.Fatalf("the preparation carries no run for %v", productionPrepareInspectionRunID)
	}
	if syntheticString(run["stage"]) != "author-commit" || syntheticString(run["status"]) != "ready" ||
		syntheticString(run["purpose"]) != "author-manuscript" {
		t.Fatalf("the run was created at %v/%v/%v, want author-commit/ready/author-manuscript",
			run["stage"], run["status"], run["purpose"])
	}
	if syntheticString(run["manifestFingerprint"]) != preview.ManifestFingerprint ||
		syntheticString(run["authorityFingerprint"]) != preview.AuthorityFingerprint {
		t.Fatalf("the run froze %v/%v, want the preview's %v/%v",
			run["manifestFingerprint"], run["authorityFingerprint"],
			preview.ManifestFingerprint, preview.AuthorityFingerprint)
	}
	if run["authorInference"] != true {
		t.Fatalf("the run did not freeze the authorInference option: %v", run["authorInference"])
	}
	if len(prepared.Preparation.NewChapterNumbers) != productionPrepareInspectionChapters ||
		len(prepared.Preparation.ConflictChapterNumbers) != 0 ||
		len(prepared.Preparation.DuplicateChapterNumbers) != 0 {
		t.Fatalf("the preparation answered new=%d conflict=%d duplicate=%d",
			len(prepared.Preparation.NewChapterNumbers),
			len(prepared.Preparation.ConflictChapterNumbers),
			len(prepared.Preparation.DuplicateChapterNumbers))
	}

	handle := productionPrepareInspectionReadOnly(t, lane.database)
	var stage, status, purpose, locale, manifest, authority, display string
	var resumable, totalChapters, manifestChapterCount, manifestContentSize int64
	if err := handle.QueryRow(`SELECT stage, status, purpose, locale, manifest_fingerprint,
 authority_fingerprint, source_display_json, resumable, total_chapters, manifest_chapter_count,
 manifest_content_size FROM import_runs WHERE id = ?`,
		productionPrepareInspectionRunID).Scan(&stage, &status, &purpose, &locale, &manifest,
		&authority, &display, &resumable, &totalChapters, &manifestChapterCount,
		&manifestContentSize); err != nil {
		t.Fatalf("read the committed run back over mode=ro: %v", err)
	}
	if stage != "author-commit" || status != "ready" || purpose != "author-manuscript" ||
		locale != "zh-CN" || resumable != 1 {
		t.Fatalf("the committed run is %v/%v/%v/%v resumable=%d",
			stage, status, purpose, locale, resumable)
	}
	if manifest != preview.ManifestFingerprint || authority != preview.AuthorityFingerprint {
		t.Fatalf("the committed run froze %v/%v, want the preview's fingerprints", manifest, authority)
	}
	if totalChapters != productionPrepareInspectionChapters ||
		manifestChapterCount != productionPrepareInspectionChapters || manifestContentSize <= 0 {
		t.Fatalf("the committed manifest is %d chapters of %d bytes (total %d)",
			manifestChapterCount, manifestContentSize, totalChapters)
	}
	if !strings.Contains(display, productionPrepareInspectionSourceName) {
		t.Fatalf("the committed display list does not name the staged source: %v", display)
	}

	// The forty chapter rows: the staged bodies, their own digests, and the one source
	// id the alias table minted.
	var aliasSourceID string
	if err := handle.QueryRow(`SELECT source_id FROM import_source_aliases
 WHERE alias_kind = 'location' LIMIT 1`).Scan(&aliasSourceID); err != nil {
		t.Fatalf("read the alias row back: %v", err)
	}
	var aliasRows, aliasSources int64
	if err := handle.QueryRow(`SELECT COUNT(*), COUNT(DISTINCT source_id) FROM import_source_aliases`).
		Scan(&aliasRows, &aliasSources); err != nil {
		t.Fatalf("count the alias rows: %v", err)
	}
	if aliasRows != 2 || aliasSources != 1 {
		t.Fatalf("the alias table holds %d rows over %d source ids, want 2 over 1",
			aliasRows, aliasSources)
	}
	rows, err := handle.Query(`SELECT chapter_number, title, content_snapshot,
 content_fingerprint, source_id FROM import_run_chapters WHERE run_id = ?
 ORDER BY chapter_number`, productionPrepareInspectionRunID)
	if err != nil {
		t.Fatalf("read the committed chapters back: %v", err)
	}
	seen := make(map[int64]bool, productionPrepareInspectionChapters)
	for rows.Next() {
		var number int64
		var title, content, fingerprint, sourceID string
		if err := rows.Scan(&number, &title, &content, &fingerprint, &sourceID); err != nil {
			_ = rows.Close()
			t.Fatalf("scan a committed chapter: %v", err)
		}
		if seen[number] {
			_ = rows.Close()
			t.Fatalf("chapter %d was written twice", number)
		}
		seen[number] = true
		if number < 1 || number > productionPrepareInspectionChapters {
			_ = rows.Close()
			t.Fatalf("chapter %d is outside the staged range", number)
		}
		if content != productionPrepareInspectionChapterBody(int(number)) {
			_ = rows.Close()
			t.Fatalf("chapter %d did not freeze the staged body", number)
		}
		if fingerprint != sha256Hex(content) {
			_ = rows.Close()
			t.Fatalf("chapter %d did not freeze its own digest", number)
		}
		if sourceID != aliasSourceID {
			_ = rows.Close()
			t.Fatalf("chapter %d names source %v, want the alias table's %v",
				number, sourceID, aliasSourceID)
		}
		if title == "" {
			_ = rows.Close()
			t.Fatalf("chapter %d froze no title", number)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate the committed chapters: %v", err)
	}
	_ = rows.Close()
	if len(seen) != productionPrepareInspectionChapters {
		t.Fatalf("the run holds %d chapters, want %d", len(seen), productionPrepareInspectionChapters)
	}

	var ledger int64
	if err := handle.QueryRow(`SELECT COUNT(*) FROM import_effect_ledger
 WHERE run_id = ? AND stage = 'prepared' AND batch_id = 'author-inference'
  AND state = 'complete' AND evidence_kind = 'legacy-checkpoint'`,
		productionPrepareInspectionRunID).Scan(&ledger); err != nil {
		t.Fatalf("read the inference ledger row back: %v", err)
	}
	if ledger != 1 {
		t.Fatalf("the inference option left %d ledger rows, want 1", ledger)
	}

	// The receipt is one-use: the same id cannot create a second run.
	replay := lane.author.call(t, productionImportRunPrepareInspectionOperation,
		[]any{lane.prepareRequest(inspectionID, productionPrepareInspectionRunID+"-replay", preview),
			lane.project, lane.frozen})
	if replay.Code == http.StatusOK ||
		!strings.Contains(replay.Body.String(), "IMPORT_INSPECTION_EXPIRED") {
		t.Fatalf("a spent receipt answered %d %s, want IMPORT_INSPECTION_EXPIRED",
			replay.Code, replay.Body.String())
	}
	var runs int64
	if err := handle.QueryRow(`SELECT COUNT(*) FROM import_runs`).Scan(&runs); err != nil {
		t.Fatalf("count the runs: %v", err)
	}
	if runs != 1 {
		t.Fatalf("the lane committed %d runs, want the one", runs)
	}
}

// TestProductionImportRunPrepareInspectionRefusesForeignProjectAndStaleLease pins the
// one authority refusal: a project this caller does not hold and a lease this process
// never issued answer the same status, headers and bytes, and neither spends the
// receipt.
func TestProductionImportRunPrepareInspectionRefusesForeignProjectAndStaleLease(t *testing.T) {
	lane := newProductionPrepareInspectionLane(t)
	lane.stage(t, productionPrepareInspectionSourceName,
		productionPrepareInspectionManuscript(productionPrepareInspectionChapters))
	inspectionID := lane.selectInspection(t)
	// Only the shapes matter here: the refusals below are decided before either
	// fingerprint is compared.
	preview := productionPrepareInspectionPreview{AuthorityFingerprint: strings.Repeat("a", 64),
		ManifestFingerprint: strings.Repeat("b", 64)}
	treeBefore := productionCloudBackupTreeDigest(t, lane.project)
	databaseBefore := productionCloudBackupDatabaseDigest(t, lane.project)

	foreignPath := lane.project + "-foreign"
	foreign := productionImportContext(lane.frozen, "projectPath", foreignPath)
	stale := productionImportContext(lane.frozen, "leaseId", productionPrepareInspectionStaleLease)
	refuse := func(expected map[string]any, path string) *httptest.ResponseRecorder {
		response := lane.author.call(t, productionImportRunPrepareInspectionOperation,
			[]any{lane.prepareRequest(inspectionID, productionPrepareInspectionRunID, preview),
				path, expected})
		if response.Code == http.StatusOK {
			t.Fatalf("a refused authority answered 200: %s", response.Body.String())
		}
		if !strings.Contains(response.Body.String(), "PROJECT_SESSION_STALE") {
			t.Fatalf("a refused authority answered %d %s, want PROJECT_SESSION_STALE",
				response.Code, response.Body.String())
		}
		return response
	}
	foreignAnswer := refuse(foreign, foreignPath)
	staleAnswer := refuse(stale, lane.project)
	if foreignAnswer.Code != staleAnswer.Code {
		t.Fatalf("the two refusals answer %d and %d", foreignAnswer.Code, staleAnswer.Code)
	}
	if foreignAnswer.Header().Get("Content-Type") != staleAnswer.Header().Get("Content-Type") {
		t.Fatalf("the two refusals answer content types %v and %v",
			foreignAnswer.Header().Get("Content-Type"), staleAnswer.Header().Get("Content-Type"))
	}
	if foreignAnswer.Body.String() != staleAnswer.Body.String() {
		t.Fatalf("the two refusals are not byte-identical:\n foreign: %s\n stale:   %s",
			foreignAnswer.Body.String(), staleAnswer.Body.String())
	}

	// Neither refusal spent the receipt: the same id still previews and prepares under
	// the live lease, and until this point nothing was written.
	productionImportUnchanged(t, lane.project, treeBefore, databaseBefore)
	livePreview := lane.preview(t, inspectionID)
	prepared := lane.prepare(t, inspectionID, productionPrepareInspectionRunID, livePreview)
	if !prepared.Success || prepared.Preparation.Classification != "new" {
		t.Fatalf("the live request after two refusals was refused: %v", prepared.ErrorCode)
	}
}

// TestProductionImportRunPrepareInspectionWrongCasConsumeRefuses proves the store's
// compare-and-swap still guards the receipt: a wrong CAS is refused without
// consuming, so the served preview and prepare that follow still resolve the id.
func TestProductionImportRunPrepareInspectionWrongCasConsumeRefuses(t *testing.T) {
	lane := newProductionPrepareInspectionLane(t)
	lane.stage(t, productionPrepareInspectionSourceName,
		productionPrepareInspectionManuscript(productionPrepareInspectionChapters))
	inspectionID := lane.selectInspection(t)
	binding := lane.binding()
	summary, err := lane.selection.store.Peek(inspectionID, binding)
	if err != nil {
		t.Fatalf("the picker's receipt is not resolvable by its own store: %v", err)
	}
	if len(summary.PreviewCAS) != 64 {
		t.Fatal("the receipt does not carry a preview CAS")
	}
	if _, err := lane.selection.store.Consume(inspectionID, strings.Repeat("0", 64), binding); err == nil ||
		err.Error() != "IMPORT_INSPECTION_PREVIEW_CHANGED" {
		t.Fatalf("a wrong CAS was not refused with IMPORT_INSPECTION_PREVIEW_CHANGED: %v", err)
	}
	if active := lane.selection.store.ActiveCount(); active != 1 {
		t.Fatalf("a wrong CAS left %d receipts, want the one", active)
	}
	// The served half of the same proof: the id the wrong CAS named is still the
	// receipt the preview and the prepare resolve.
	preview := lane.preview(t, inspectionID)
	prepared := lane.prepare(t, inspectionID, productionPrepareInspectionRunID, preview)
	if !prepared.Success {
		t.Fatalf("the served prepare refused a receipt a wrong CAS did not spend: %v",
			prepared.ErrorCode)
	}
	if active := lane.selection.store.ActiveCount(); active != 0 {
		t.Fatalf("the served request left %d receipts behind", active)
	}
}

// TestProductionImportRunPrepareInspectionWiringLanded pins the wiring this change
// cannot install itself: production_open_linux.go has to hoist the picker's store and
// hand the same adapter to the preview and to the name. The marker comparison ignores
// white space, so formatting cannot decide whether the wiring is there.
func TestProductionImportRunPrepareInspectionWiringLanded(t *testing.T) {
	raw, err := os.ReadFile("production_open_linux.go")
	if err != nil {
		t.Fatal(err)
	}
	source := string(raw)
	hoist := "importSelection, importSelectionErr := newProductionImportSelection(open, uploads)"
	for _, marker := range []string{
		hoist,
		"importPrepareInspection := newProductionImportRunPrepareInspectionServe(open.durable, importSelection)",
		"newProductionFinalizedDraftImports(open.durable, importPrepareInspection)",
		"func() error { return importPrepareInspection.Bind(server) },",
	} {
		if !productionSeamContains(source, marker) {
			t.Fatalf("production_open_linux.go does not wire the served name; it is missing %v", marker)
		}
	}
	// The hoist replaces the closure-local construction instead of joining it: two
	// stores would be two receipt tables, and the picker would mint ids no other binder
	// could resolve. Exactly one construction may remain in the file.
	if count := strings.Count(productionSeamCompact(source),
		productionSeamCompact("newProductionImportSelection(open, uploads)")); count != 1 {
		t.Fatalf("production_open_linux.go constructs the picker %d times, want one shared store",
			count)
	}
}

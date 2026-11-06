//go:build linux

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/draftalg"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// Both served names run against a disposable t.TempDir() only: the session lease,
// the admitted project database and every readback live inside the test's own
// temporary tree.
//
// # Why this test builds the lane itself
//
// productionHandlerWithContract (production_open_linux.go:458) binds the names its
// own `for _, bind := range []func() error{` slice names, and this batch is not
// allowed to edit that file. The two names are therefore installed on a
// transport.Server this test constructs over the SAME openProduction result and the
// SAME durable bridge, exactly the way
// production_importrun_residual_linux_test.go:322-350 mounts its own binder.
// Nothing else is bound but the project lifecycle, so an accidentally unbound name
// answers 501 GO_OPERATION_NOT_MIGRATED and every refusal assertion would catch it.
//
// # What is hand-seeded, and why
//
// The store has no "create an imported chapter" method other than the import
// itself, so the scenarios that need a pre-existing chapter (a working draft on a
// version the import must skip, an already-finalized chapter the import must
// refuse) write those rows as SQL, exactly the way the store's own tests do. Every
// other row this file asserts against is written by the store through the lane.
//
// continuity_projection_meta is seeded by an UPSERT and asserted unchanged after
// every import, because the store's header States that the reference invalidates no
// continuity projection here (internal/store/finalized_draft_import.go:70-74): a
// bare UPDATE would be a silent no-op on a database whose table starts empty, and
// INSERT OR IGNORE alone would keep the stale 0 that a later reader would read as
// "the projection is current".
//
// Word counts come from draftalg.CountDraftUnits, the shared visible-prose counter
// the payload is validated against (finalized_draft_import.go:358-362).

// productionFinalizedDraftImportSchema is the smallest set of tables these two
// names really touch: the pinned v7 core row project:open reads, the drafts and
// their bodies, the publication outbox the import freezes, the import operation
// ledger, and the continuity projection row the import must leave alone. Every
// statement is written exactly as it must appear in sqlite_master, because the
// contract hashes that text.
var productionFinalizedDraftImportSchema = []string{
	"CREATE TABLE project_core (id TEXT PRIMARY KEY, project_name TEXT, writing_language TEXT, genre TEXT, sub_genre TEXT, target_audience TEXT, total_chapters INTEGER, words_per_chapter INTEGER, creative_strategy TEXT, narrative_thread_dormant_threshold INTEGER, plot_structure TEXT, narrative_pov TEXT, core_outline TEXT, world_setting TEXT, golden_finger TEXT, protagonist_profile TEXT, global_guidance TEXT, writing_style TEXT, reference_works TEXT, character_states TEXT, premise TEXT, worldbuilding TEXT, synopsis TEXT, characters_arch TEXT, created_at TEXT, updated_at TEXT)",
	"CREATE TABLE contents (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL DEFAULT '', created_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE drafts (id INTEGER PRIMARY KEY AUTOINCREMENT, chapter_number INTEGER NOT NULL, version INTEGER NOT NULL, status TEXT DEFAULT 'draft', source TEXT DEFAULT 'write', content_id INTEGER NOT NULL, word_count INTEGER DEFAULT 0, source_dependencies TEXT NOT NULL DEFAULT '[]', created_at TEXT DEFAULT (datetime('now')), updated_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE finalization_outbox (finalization_id TEXT PRIMARY KEY, draft_id INTEGER NOT NULL UNIQUE, chapter_number INTEGER NOT NULL, chapter_title TEXT NOT NULL DEFAULT '', content_hash TEXT NOT NULL, content_revision INTEGER NOT NULL, content_snapshot TEXT NOT NULL DEFAULT '', target_file_name TEXT NOT NULL, knowledge_document_id TEXT NOT NULL DEFAULT '', publication_status TEXT NOT NULL DEFAULT 'pending', last_error TEXT NOT NULL DEFAULT '', published_at TEXT DEFAULT NULL, created_at TEXT DEFAULT (datetime('now')), updated_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE finalized_draft_import_operations (operation_id TEXT PRIMARY KEY, payload_hash TEXT NOT NULL, receipt_json TEXT NOT NULL, created_at TEXT DEFAULT (datetime('now')))",
	"CREATE TABLE continuity_projection_meta (id TEXT PRIMARY KEY, generation INTEGER NOT NULL DEFAULT 0, stale_from_chapter INTEGER DEFAULT NULL)",
}

// productionFinalizedDraftImportContract is the trusted contract for
// productionFinalizedDraftImportSchema. The verifier hashes sqlite_master in
// (type, name) order, so the entries are sorted the same way, not in DDL order.
func productionFinalizedDraftImportContract(t *testing.T) admission.SchemaContract {
	t.Helper()
	const prefix = "CREATE TABLE "
	entries := make([]productionSchemaEntry, 0, len(productionFinalizedDraftImportSchema))
	for _, statement := range productionFinalizedDraftImportSchema {
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
	return admission.SchemaContract{{Version: admission.CurrentSchemaVersion, Fingerprints: []string{sha256Hex(string(encoded))}}}
}

const (
	productionFinalizedDraftImportProjectName = "finalized-draft-import-novel"
	productionFinalizedDraftImportProjectID   = "5d2b8c41-7e63-4a19-9f04-8c6a1e7b3d52"
	productionFinalizedDraftImportTab         = "client_finalized_imp"
	productionFinalizedDraftImportStaleLease  = "stale-finalized-import-0001"
	productionFinalizedDraftImportOperationID = "draft-import:run-1"
	productionFinalizedDraftImportContinuity  = int64(7)
	productionFinalizedDraftImportTitleOne    = "山雨"
	productionFinalizedDraftImportTitleTwo    = "夜渡"
	productionFinalizedDraftImportTitleThree  = "归途"
	productionFinalizedDraftImportBodyOne     = "山雨欲来，风满楼。"
	productionFinalizedDraftImportBodyTwo     = "夜色沉沉，江上无人。"
	productionFinalizedDraftImportBodyThree   = "归途已远，灯火将尽。"
	productionFinalizedDraftImportWorkingBody = "作者正在写的草稿。"
	productionFinalizedDraftImportSeededBody  = "这是作者自己写的定稿。"
)

// productionFinalizedDraftImportSeedCore is the one project_core row project:open
// requires, copied column for column from
// production_importrun_residual_linux_test.go:153-164 so this project opens exactly
// like a real finalised one.
const productionFinalizedDraftImportSeedCore = `INSERT INTO project_core (id, project_name, writing_language, genre, sub_genre,
 target_audience, total_chapters, words_per_chapter, creative_strategy,
 narrative_thread_dormant_threshold, plot_structure, narrative_pov, core_outline,
 world_setting, golden_finger, protagonist_profile, global_guidance, writing_style,
 reference_works, character_states, premise, worldbuilding, synopsis, characters_arch,
 created_at, updated_at)
 VALUES ('main', '定稿导入小说', 'zh-CN', 'fantasy', '', '', 3, 2000, 'auto', 3,
 '', '', '', '', '', '', '', '', '', '{}', '', '', '', '',
 '2024-05-06 09:10:11', '2024-05-06 09:10:12')`

// productionFinalizedDraftImportSeedContinuity seats the continuity projection row
// a real project carries, as an upsert: the table starts empty, so the INSERT arm is
// the one that matters and the conflict arm is what a re-seed takes.
const productionFinalizedDraftImportSeedContinuity = `INSERT INTO continuity_projection_meta (id, generation, stale_from_chapter)
 VALUES ('main', 7, NULL)
 ON CONFLICT(id) DO UPDATE SET generation = excluded.generation,
  stale_from_chapter = excluded.stale_from_chapter`

// productionFinalizedDraftImportWorkingDraft is a non-finalized draft on chapter 2
// which already holds version 2, so the import must land on version 3 there and 1
// elsewhere (finalized_draft_import.go:442-446).
func productionFinalizedDraftImportWorkingDraft() []string {
	return []string{
		fmt.Sprintf(`INSERT INTO contents (id, body) VALUES (100, '%s')`,
			productionFinalizedDraftImportWorkingBody),
		fmt.Sprintf(`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (100, 2, 2, 'draft', 'write', 100, %d, '[]', '2024-05-06 09:10:13', '2024-05-06 09:10:14')`,
			int64(draftalg.CountDraftUnits(productionFinalizedDraftImportWorkingBody))),
	}
}

// productionFinalizedDraftImportFinalizedChapter is an already-finalized chapter
// with its frozen outbox row: the state the collision scenario imports into.
func productionFinalizedDraftImportFinalizedChapter(number int64, title, body string) []string {
	return []string{
		fmt.Sprintf(`INSERT INTO contents (id, body) VALUES (200, '%s')`, body),
		fmt.Sprintf(`INSERT INTO drafts (id, chapter_number, version, status, source, content_id,
 word_count, source_dependencies, created_at, updated_at)
 VALUES (200, %d, 1, 'finalized', 'write', 200, %d, '[]', '2024-05-06 09:10:15', '2024-05-06 09:10:16')`,
			number, int64(draftalg.CountDraftUnits(body))),
		fmt.Sprintf(`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number,
 chapter_title, content_hash, content_revision, content_snapshot, target_file_name,
 publication_status, last_error)
 VALUES ('import-seeded-%d', 200, %d, '%s', '%s', 1, '%s', '第%d章 %s.txt', 'published', '')`,
			number, number, title, sha256Hex(body), body, number, title),
	}
}

// productionFinalizedDraftImportSeedProject writes an admitted project whose
// database carries the two names' schema, a continuity row and the given rows, then
// folds the write-ahead log into the admitted file so the bytes the test reads are
// the bytes the lane admits.
func productionFinalizedDraftImportSeedProject(t *testing.T, rows ...string) string {
	t.Helper()
	root := syntheticProductionDataDir(t)
	library := filepath.Join(root, productionLibraryDirectory)
	seedProductionProject(t, library, productionFinalizedDraftImportProjectName,
		productionFinalizedDraftImportProjectID, admission.CurrentSchemaVersion,
		productionFinalizedDraftImportSchema)
	projectPath := filepath.Join(library, productionFinalizedDraftImportProjectName)
	statements := []string{productionFinalizedDraftImportSeedCore, productionFinalizedDraftImportSeedContinuity}
	statements = append(statements, rows...)
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %s: %v", statement, err)
		}
	}
	productionEditorCheckpoint(t, db, database)
	return projectPath
}

// productionFinalizedDraftImportLane opens the real production lane over the
// disposable data root and installs exactly two binders on it: the project
// lifecycle (so project:open can issue the session lease these calls name) and this
// batch's two names.
func productionFinalizedDraftImportLane(t *testing.T, root string) http.Handler {
	t.Helper()
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: productionFinalizedDraftImportContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-finalized-draft-imports")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	if bindErr := newProductionFinalizedDraftImports(open.durable).Bind(server); bindErr != nil {
		t.Fatal(bindErr)
	}
	return server
}

// productionFinalizedDraftImportFixture is one opened lane with one opened project.
type productionFinalizedDraftImportFixture struct {
	t           *testing.T
	projectPath string
	database    string
	author      *productionDurableClient
	frozen      map[string]any
}

// newProductionFinalizedDraftImportFixture seeds the given rows, opens the lane and
// opens the project, returning the frozen three-key context every call carries.
func newProductionFinalizedDraftImportFixture(t *testing.T, rows ...string) *productionFinalizedDraftImportFixture {
	t.Helper()
	projectPath := productionFinalizedDraftImportSeedProject(t, rows...)
	handler := productionFinalizedDraftImportLane(t, filepath.Dir(filepath.Dir(projectPath)))
	author := newProductionDurableClient(t, handler, productionFinalizedDraftImportTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "finalized-import-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the admitted import project: %+v", opened)
	}
	if opened.Project["id"] != productionFinalizedDraftImportProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned the wrong project identity: %v", opened.Project)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	return &productionFinalizedDraftImportFixture{
		t:           t,
		projectPath: projectPath,
		database:    filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName),
		author:      author,
		frozen: map[string]any{
			"projectId":   opened.Project["id"],
			"projectPath": opened.Project["path"],
			"leaseId":     lease,
		},
	}
}

// staleContext is the same frozen session with a lane lease this lane never issued.
func (f *productionFinalizedDraftImportFixture) staleContext() map[string]any {
	return map[string]any{
		"projectId":   f.frozen["projectId"],
		"projectPath": f.frozen["projectPath"],
		"leaseId":     productionFinalizedDraftImportStaleLease,
	}
}

// answer sends one call, decodes its result and proves the answer does not echo the
// host path. A non-200 is fatal: a bound name answers 200 only after its read or its
// transaction ran.
func (f *productionFinalizedDraftImportFixture) answer(operation string, args []any, into any) {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code != http.StatusOK {
		f.t.Fatalf("%s: %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || len(wire.Result) == 0 {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if err := json.Unmarshal(wire.Result, into); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s echoed the project path: %s", operation, response.Body.String())
	}
}

// refuse asserts one refused call carries the code the store or the binder itself
// raised. A 501 or a generic message here means the binder was never reached or the
// detail was swallowed.
func (f *productionFinalizedDraftImportFixture) refuse(operation string, args []any, code string) {
	f.t.Helper()
	response := f.author.call(f.t, operation, args)
	if response.Code == http.StatusOK {
		f.t.Fatalf("%s answered 200 for a refused call: %s", operation, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		f.t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Error.Code == "GO_OPERATION_NOT_MIGRATED" {
		f.t.Fatalf("%s is not bound at all: %s", operation, response.Body.String())
	}
	if wire.Error.Code != code {
		f.t.Fatalf("%s refused with %v; want %v: %s", operation, wire.Error.Code, code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), f.projectPath) {
		f.t.Fatalf("%s leaked the project path: %s", operation, response.Body.String())
	}
}

// productionFinalizedDraftImportChapterWire is one chapter of the wire payload
// (src/shared/finalized-draft-import.ts:1-6), with the count the store validates.
// JSON numbers arrive as float64 on both sides of the wire.
func productionFinalizedDraftImportChapterWire(number int64, title, content string) map[string]any {
	return map[string]any{"chapterNumber": float64(number), "title": title, "content": content,
		"wordCount": float64(draftalg.CountDraftUnits(content))}
}

// productionFinalizedDraftImportChaptersWire is the chapters array of one request.
func productionFinalizedDraftImportChaptersWire(chapters ...map[string]any) []any {
	items := make([]any, 0, len(chapters))
	for _, chapter := range chapters {
		items = append(items, chapter)
	}
	return items
}

// productionFinalizedDraftImportBatchRequest is FinalizedDraftImportRequest
// (src/shared/finalized-draft-import.ts:8-17): an operation id and a chapters array,
// plus whichever expectations the caller inspected.
func productionFinalizedDraftImportBatchRequest(operationID string, chapters ...map[string]any) map[string]any {
	return map[string]any{"operationId": operationID, "chapters": productionFinalizedDraftImportChaptersWire(chapters...)}
}

// productionFinalizedDraftImportPreviewPayload is the preview's payload: the frozen
// chapters the caller already holds, in the one key the binder reads.
func productionFinalizedDraftImportPreviewPayload(chapters ...map[string]any) map[string]any {
	return map[string]any{"chapters": productionFinalizedDraftImportChaptersWire(chapters...)}
}

// productionFinalizedDraftImportManifest recomputes manifestFingerprint
// (internal/store/finalized_draft_import.go:441-449) over the wire's chapters, so a
// preview's fingerprint is checked against the document it claims to describe and a
// stale expectation is a real digest of a different batch.
func productionFinalizedDraftImportManifest(t *testing.T, chapters ...map[string]any) string {
	t.Helper()
	type manifestChapter struct {
		ChapterNumber int64  `json:"chapterNumber"`
		Title         string `json:"title"`
		ContentHash   string `json:"contentHash"`
		WordCount     int64  `json:"wordCount"`
	}
	payload := make([]manifestChapter, 0, len(chapters))
	for _, chapter := range chapters {
		content, _ := chapter["content"].(string)
		title, _ := chapter["title"].(string)
		number, _ := chapter["chapterNumber"].(float64)
		wordCount, _ := chapter["wordCount"].(float64)
		payload = append(payload, manifestChapter{ChapterNumber: int64(number), Title: title,
			ContentHash: sha256Hex(content), WordCount: int64(wordCount)})
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return sha256Hex(string(encoded))
}

// productionFinalizedDraftImportPreviewWire is AuthorManuscriptImportPreview
// (src/shared/author-manuscript-import.ts:20-33) as it crosses the wire.
type productionFinalizedDraftImportPreviewWire struct {
	Classification          string `json:"classification"`
	AuthorityFingerprint    string `json:"authorityFingerprint"`
	ManifestFingerprint     string `json:"manifestFingerprint"`
	ChapterCount            int64  `json:"chapterCount"`
	TargetStatus            string `json:"targetStatus"`
	NextChapterNumber       *int64 `json:"nextChapterNumber"`
	Chapters                []struct {
		Number      int64  `json:"number"`
		Title       string `json:"title"`
		WordCount   int64  `json:"wordCount"`
		Disposition string `json:"disposition"`
	} `json:"chapters"`
	NewChapterNumbers       []int64 `json:"newChapterNumbers"`
	DuplicateChapterNumbers []int64 `json:"duplicateChapterNumbers"`
	ConflictChapterNumbers  []int64 `json:"conflictChapterNumbers"`
	FirstGapChapterNumber   *int64  `json:"firstGapChapterNumber"`
	AuthorityInvalid        bool    `json:"authorityInvalid"`
}

// productionFinalizedDraftImportBatchWire is the batch channel's answer
// (src/shared/ipc-channels.ts:1019-1022): the reference's envelope around
// FinalizedDraftImportReceipt.
type productionFinalizedDraftImportBatchWire struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Receipt *struct {
		OperationID    string  `json:"operationId"`
		PayloadHash    string  `json:"payloadHash"`
		ChapterNumbers []int64 `json:"chapterNumbers"`
		Drafts         []struct {
			ChapterNumber     int64  `json:"chapterNumber"`
			DraftID           int64  `json:"draftId"`
			FinalizationID    string `json:"finalizationId"`
			ContentHash       string `json:"contentHash"`
			TargetFileName    string `json:"targetFileName"`
			Status            string `json:"status"`
			PublicationStatus string `json:"publicationStatus"`
		} `json:"drafts"`
		Idempotent bool `json:"idempotent"`
	} `json:"receipt"`
}

// productionFinalizedDraftImportAssertContinuity proves the import neither bumped
// nor damaged the projection meta the fixture seated.
func productionFinalizedDraftImportAssertContinuity(t *testing.T, database string) {
	t.Helper()
	row := productionImportRunWriteRow(t, database,
		`SELECT generation, stale_from_chapter FROM continuity_projection_meta WHERE id = 'main'`)
	if generation := productionImportRunWriteNumber(row, "generation"); generation != productionFinalizedDraftImportContinuity {
		t.Fatalf("continuity generation is %d, want the seeded %d", generation, productionFinalizedDraftImportContinuity)
	}
	if row["stale_from_chapter"] != nil {
		t.Fatalf("continuity stale_from_chapter is %v, want NULL", row["stale_from_chapter"])
	}
}

// TestProductionFinalizedDraftImports is the whole served surface of the two names.
func TestProductionFinalizedDraftImports(t *testing.T) {
	chapterOne := productionFinalizedDraftImportChapterWire(1, productionFinalizedDraftImportTitleOne,
		productionFinalizedDraftImportBodyOne)
	chapterTwo := productionFinalizedDraftImportChapterWire(2, productionFinalizedDraftImportTitleTwo,
		productionFinalizedDraftImportBodyTwo)
	chapterThree := productionFinalizedDraftImportChapterWire(3, productionFinalizedDraftImportTitleThree,
		productionFinalizedDraftImportBodyThree)
	batch := productionFinalizedDraftImportBatchName
	previewName := productionFinalizedDraftImportPreviewName

	// The preview over a project with no chapters at all: everything appends, so the
	// batch is 'ready' from an 'empty' authority, and a preview writes nothing.
	t.Run("preview on a project with no chapters", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t)
		settled := productionImportRunResidualImage(t, f.database)
		var preview productionFinalizedDraftImportPreviewWire
		f.answer(previewName, []any{
			productionFinalizedDraftImportPreviewPayload(chapterOne, chapterTwo),
			f.projectPath, f.frozen,
		}, &preview)
		if preview.Classification != "ready" || preview.AuthorityInvalid || preview.TargetStatus != "finalized" {
			t.Fatalf("preview is %v/%v/%v, want ready/false/finalized", preview.Classification,
				preview.AuthorityInvalid, preview.TargetStatus)
		}
		if preview.ChapterCount != 2 || len(preview.Chapters) != 2 {
			t.Fatalf("preview reports %d chapters and %d rows, want 2", preview.ChapterCount, len(preview.Chapters))
		}
		// :167-175 - the empty authority is the fingerprint of the JSON array "[]".
		if preview.AuthorityFingerprint != sha256Hex("[]") {
			t.Fatalf("authority fingerprint is %v, want the empty array digest", preview.AuthorityFingerprint)
		}
		if want := productionFinalizedDraftImportManifest(t, chapterOne, chapterTwo); preview.ManifestFingerprint != want {
			t.Fatalf("manifest fingerprint is %v, want %v", preview.ManifestFingerprint, want)
		}
		if len(preview.NewChapterNumbers) != 2 || preview.NewChapterNumbers[0] != 1 || preview.NewChapterNumbers[1] != 2 {
			t.Fatalf("newChapterNumbers is %v, want [1 2]", preview.NewChapterNumbers)
		}
		if len(preview.DuplicateChapterNumbers) != 0 || len(preview.ConflictChapterNumbers) != 0 {
			t.Fatalf("duplicates/conflicts are %v/%v, want empty", preview.DuplicateChapterNumbers,
				preview.ConflictChapterNumbers)
		}
		if preview.Chapters[0].Disposition != "new" || preview.Chapters[1].Disposition != "new" ||
			preview.Chapters[0].Number != 1 || preview.Chapters[1].Number != 2 {
			t.Fatalf("preview chapters are %v, want two new ascending rows", preview.Chapters)
		}
		if want := int64(draftalg.CountDraftUnits(productionFinalizedDraftImportBodyTwo)); preview.Chapters[1].WordCount != want {
			t.Fatalf("chapter 2 word count is %d, want the shared counter's %d", preview.Chapters[1].WordCount, want)
		}
		// :370-372 - lastChapterNumber(0) + fresh(2) + 1.
		if preview.NextChapterNumber == nil || *preview.NextChapterNumber != 3 {
			t.Fatalf("nextChapterNumber is %v, want 3", preview.NextChapterNumber)
		}
		if preview.FirstGapChapterNumber != nil {
			t.Fatalf("firstGapChapterNumber is %v, want omitted", preview.FirstGapChapterNumber)
		}
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the author-manuscript preview")
		productionFinalizedDraftImportAssertContinuity(t, f.database)
	})

	// A real multi-chapter batch: the preview's own two fingerprints are handed back,
	// so this is the reference's whole flow, and every effect is read afterwards over
	// a SEPARATE mode=ro connection.
	t.Run("a real multi-chapter batch is committed", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t, productionFinalizedDraftImportWorkingDraft()...)
		var preview productionFinalizedDraftImportPreviewWire
		f.answer(previewName, []any{
			productionFinalizedDraftImportPreviewPayload(chapterOne, chapterTwo, chapterThree),
			f.projectPath, f.frozen,
		}, &preview)
		if preview.Classification != "ready" || preview.NextChapterNumber == nil || *preview.NextChapterNumber != 4 {
			t.Fatalf("preview is %v with next %v, want ready/4 over the empty authority",
				preview.Classification, preview.NextChapterNumber)
		}
		request := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne, chapterTwo, chapterThree)
		request["expectedAuthorityFingerprint"] = preview.AuthorityFingerprint
		request["expectedCommitManifestFingerprint"] = preview.ManifestFingerprint
		var answer productionFinalizedDraftImportBatchWire
		f.answer(batch, []any{request, f.projectPath, f.frozen}, &answer)
		if !answer.Success || answer.Error != "" || answer.Receipt == nil {
			t.Fatalf("the batch was not reported as a commit: %+v", answer)
		}
		receipt := answer.Receipt
		if receipt.Idempotent {
			t.Fatal("a first commit is not idempotent")
		}
		if receipt.OperationID != productionFinalizedDraftImportOperationID || len(receipt.PayloadHash) != 64 {
			t.Fatalf("receipt identity is %v/%v, want the operation id and a sha256 payload hash",
				receipt.OperationID, receipt.PayloadHash)
		}
		if len(receipt.ChapterNumbers) != 3 || receipt.ChapterNumbers[0] != 1 ||
			receipt.ChapterNumbers[1] != 2 || receipt.ChapterNumbers[2] != 3 {
			t.Fatalf("chapterNumbers is %v, want ascending [1 2 3]", receipt.ChapterNumbers)
		}
		if len(receipt.Drafts) != 3 {
			t.Fatalf("the receipt names %d drafts, want 3", len(receipt.Drafts))
		}
		wantFinalization := "import-" + sha256Hex(productionFinalizedDraftImportOperationID+":1")[:32]
		first := receipt.Drafts[0]
		if first.ChapterNumber != 1 || first.FinalizationID != wantFinalization ||
			first.ContentHash != sha256Hex(productionFinalizedDraftImportBodyOne) ||
			first.TargetFileName != "第1章 山雨.txt" || first.Status != "finalized" ||
			first.PublicationStatus != "pending" {
			t.Fatalf("first draft receipt is %v, want a frozen pending finalized chapter", first)
		}
		for index, draft := range receipt.Drafts {
			if draft.DraftID < 1 || draft.ChapterNumber != int64(index+1) {
				t.Fatalf("draft %d of the receipt is %v", index, draft)
			}
		}

		// Every effect, read by a connection that could not have written it.
		draftRows := productionImportRunWriteQuery(t, f.database, `SELECT chapter_number, version,
 status, source, word_count FROM drafts ORDER BY chapter_number ASC, version ASC`)
		if len(draftRows) != 4 {
			t.Fatalf("drafts holds %d rows, want the seeded draft plus three imported ones", len(draftRows))
		}
		wantDrafts := []struct {
			chapter int64
			version int64
			words   int64
			status  string
		}{
			{1, 1, int64(draftalg.CountDraftUnits(productionFinalizedDraftImportBodyOne)), "finalized"},
			{2, 2, int64(draftalg.CountDraftUnits(productionFinalizedDraftImportWorkingBody)), "draft"},
			{2, 3, int64(draftalg.CountDraftUnits(productionFinalizedDraftImportBodyTwo)), "finalized"},
			{3, 1, int64(draftalg.CountDraftUnits(productionFinalizedDraftImportBodyThree)), "finalized"},
		}
		for index, want := range wantDrafts {
			row := draftRows[index]
			if productionImportRunWriteNumber(row, "chapter_number") != want.chapter ||
				productionImportRunWriteNumber(row, "version") != want.version ||
				productionImportRunWriteNumber(row, "word_count") != want.words ||
				productionImportRunWriteText(row, "status") != want.status {
				t.Fatalf("draft %d is %v, want chapter %d version %d words %d %s", index, row,
					want.chapter, want.version, want.words, want.status)
			}
			if want.status == "finalized" && productionImportRunWriteText(row, "source") != "write" {
				t.Fatalf("imported draft %d has source %v, want write", index,
					productionImportRunWriteText(row, "source"))
			}
		}
		outbox := productionImportRunWriteQuery(t, f.database, `SELECT chapter_number, chapter_title,
 content_snapshot, target_file_name, publication_status, last_error, content_revision
 FROM finalization_outbox ORDER BY chapter_number ASC`)
		if len(outbox) != 3 {
			t.Fatalf("the outbox holds %d rows, want one frozen row per imported chapter", len(outbox))
		}
		wantTitles := []string{productionFinalizedDraftImportTitleOne, productionFinalizedDraftImportTitleTwo,
			productionFinalizedDraftImportTitleThree}
		wantBodies := []string{productionFinalizedDraftImportBodyOne, productionFinalizedDraftImportBodyTwo,
			productionFinalizedDraftImportBodyThree}
		for index := 0; index < 3; index++ {
			row := outbox[index]
			if productionImportRunWriteNumber(row, "chapter_number") != int64(index+1) ||
				productionImportRunWriteText(row, "chapter_title") != wantTitles[index] ||
				productionImportRunWriteText(row, "content_snapshot") != wantBodies[index] ||
				productionImportRunWriteText(row, "publication_status") != "pending" ||
				productionImportRunWriteText(row, "last_error") != "" ||
				productionImportRunWriteNumber(row, "content_revision") != 1 {
				t.Fatalf("outbox row %d is %v, want a pending unretried freeze of the imported body", index, row)
			}
			wantTarget := fmt.Sprintf("第%d章 %s.txt", index+1, wantTitles[index])
			if productionImportRunWriteText(row, "target_file_name") != wantTarget {
				t.Fatalf("outbox row %d target is %v, want %v", index,
					productionImportRunWriteText(row, "target_file_name"), wantTarget)
			}
		}
		operation := productionImportRunWriteRow(t, f.database, `SELECT operation_id, payload_hash,
 receipt_json FROM finalized_draft_import_operations`)
		if productionImportRunWriteText(operation, "operation_id") != productionFinalizedDraftImportOperationID {
			t.Fatalf("the operation row names %v", productionImportRunWriteText(operation, "operation_id"))
		}
		if productionImportRunWriteText(operation, "payload_hash") != receipt.PayloadHash {
			t.Fatalf("the stored payload hash is %v, want the receipt's %v",
				productionImportRunWriteText(operation, "payload_hash"), receipt.PayloadHash)
		}
		if !strings.Contains(productionImportRunWriteText(operation, "receipt_json"), wantFinalization) {
			t.Fatalf("the stored receipt does not freeze the finalization identity: %v",
				productionImportRunWriteText(operation, "receipt_json"))
		}
		productionFinalizedDraftImportAssertContinuity(t, f.database)
	})

	// The same operation again: the receipt answers idempotent and nothing is
	// written; a different payload under that id is the reference's conflict.
	t.Run("the same operation replays idempotently", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t)
		request := productionFinalizedDraftImportBatchRequest("  " + productionFinalizedDraftImportOperationID + "  ",
			chapterOne, chapterTwo, chapterThree)
		args := []any{request, f.projectPath, f.frozen}
		var first productionFinalizedDraftImportBatchWire
		f.answer(batch, args, &first)
		if first.Receipt == nil || first.Receipt.Idempotent {
			t.Fatalf("the first commit answered %+v, want a non-idempotent receipt", first.Receipt)
		}
		if first.Receipt.OperationID != productionFinalizedDraftImportOperationID {
			t.Fatalf("the trimmed operation id was not stored: %v", first.Receipt.OperationID)
		}
		settled := productionImportRunResidualImage(t, f.database)
		var replay productionFinalizedDraftImportBatchWire
		f.answer(batch, args, &replay)
		if replay.Receipt == nil || !replay.Receipt.Idempotent {
			t.Fatalf("the replay is not marked idempotent: %+v", replay.Receipt)
		}
		if replay.Receipt.PayloadHash != first.Receipt.PayloadHash ||
			len(replay.Receipt.Drafts) != 3 ||
			replay.Receipt.Drafts[0].DraftID != first.Receipt.Drafts[0].DraftID ||
			replay.Receipt.Drafts[2].DraftID != first.Receipt.Drafts[2].DraftID {
			t.Fatalf("replay receipt is %v, want the committed one", replay.Receipt)
		}
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the idempotent replay")
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT id FROM drafts ORDER BY id`); len(rows) != 3 {
			t.Fatalf("the replay wrote %d drafts, want the committed 3", len(rows))
		}
		// :407-409 - the same id bound to a different payload is a conflict.
		conflict := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			productionFinalizedDraftImportChapterWire(1, "另一个标题", productionFinalizedDraftImportBodyOne),
			chapterTwo, chapterThree)
		f.refuse(batch, []any{conflict, f.projectPath, f.frozen}, "FINALIZED_IMPORT_OPERATION_CONFLICT")
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the operation conflict")
	})

	// A chapter number the batch targets is already finalized: the reference refuses
	// before it inserts anything, so not one body is written.
	t.Run("a collision with an existing chapter is refused", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t,
			productionFinalizedDraftImportFinalizedChapter(2, productionFinalizedDraftImportTitleTwo,
				productionFinalizedDraftImportSeededBody)...)
		settled := productionImportRunResidualImage(t, f.database)
		request := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne, chapterTwo, chapterThree)
		f.refuse(batch, []any{request, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTER_ALREADY_FINALIZED")
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the already-finalized collision")
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT id FROM contents ORDER BY id`); len(rows) != 1 {
			t.Fatalf("the refused batch wrote %d content rows, want the seeded 1", len(rows))
		}
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT chapter_number FROM drafts`); len(rows) != 1 {
			t.Fatalf("the refused batch wrote %d drafts, want the seeded 1", len(rows))
		}
	})

	// The two expectation guards of :415-431: a manifest that describes another
	// batch, an authority that moved, and an authority that is current but would not
	// append.
	t.Run("a stale preview is refused", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t)
		settled := productionImportRunResidualImage(t, f.database)
		var twoChapterPreview, threeChapterPreview, gapPreview productionFinalizedDraftImportPreviewWire
		f.answer(previewName, []any{
			productionFinalizedDraftImportPreviewPayload(chapterOne, chapterTwo), f.projectPath, f.frozen,
		}, &twoChapterPreview)
		f.answer(previewName, []any{
			productionFinalizedDraftImportPreviewPayload(chapterOne, chapterTwo, chapterThree), f.projectPath, f.frozen,
		}, &threeChapterPreview)
		chapterFour := productionFinalizedDraftImportChapterWire(4, productionFinalizedDraftImportTitleThree,
			productionFinalizedDraftImportBodyThree)
		chapterFive := productionFinalizedDraftImportChapterWire(5, productionFinalizedDraftImportTitleOne,
			productionFinalizedDraftImportBodyOne)
		f.answer(previewName, []any{
			productionFinalizedDraftImportPreviewPayload(chapterThree, chapterFour, chapterFive), f.projectPath, f.frozen,
		}, &gapPreview)
		if gapPreview.Classification != "conflict" || gapPreview.FirstGapChapterNumber == nil ||
			*gapPreview.FirstGapChapterNumber != 1 {
			t.Fatalf("the non-appending batch previewed as %v with gap %v, want conflict/1",
				gapPreview.Classification, gapPreview.FirstGapChapterNumber)
		}

		// (a) the manifest of a batch of two, handed to a batch of three.
		staleManifest := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne, chapterTwo, chapterThree)
		staleManifest["expectedCommitManifestFingerprint"] = twoChapterPreview.ManifestFingerprint
		f.refuse(batch, []any{staleManifest, f.projectPath, f.frozen}, "FINALIZED_IMPORT_PREVIEW_STALE")
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the stale manifest")

		// (b) the right batch, inspected against an authority that has moved.
		staleAuthority := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne, chapterTwo, chapterThree)
		staleAuthority["expectedCommitManifestFingerprint"] = threeChapterPreview.ManifestFingerprint
		staleAuthority["expectedAuthorityFingerprint"] = strings.Repeat("0", 64)
		f.refuse(batch, []any{staleAuthority, f.projectPath, f.frozen}, "FINALIZED_IMPORT_PREVIEW_STALE")
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the stale authority")

		// (c) both fingerprints current, but the batch does not append: :427-430
		// re-runs the preview inside the transaction and refuses on its verdict.
		notAppending := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterThree, chapterFour, chapterFive)
		notAppending["expectedCommitManifestFingerprint"] = gapPreview.ManifestFingerprint
		notAppending["expectedAuthorityFingerprint"] = gapPreview.AuthorityFingerprint
		if gapPreview.AuthorityFingerprint != threeChapterPreview.AuthorityFingerprint {
			t.Fatalf("the two previews disagree on an unchanged authority: %v against %v",
				gapPreview.AuthorityFingerprint, threeChapterPreview.AuthorityFingerprint)
		}
		f.refuse(batch, []any{notAppending, f.projectPath, f.frozen}, "FINALIZED_IMPORT_AUTHORITY_CONFLICT")
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the non-appending batch")
	})

	// A frozen context naming a lease this lane never issued never reaches the store:
	// the fence is the admission, not the store's guards.
	t.Run("a batch without a live lease is refused", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t)
		settled := productionImportRunResidualImage(t, f.database)
		request := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne, chapterTwo, chapterThree)
		response := f.author.call(t, batch, []any{request, f.projectPath, f.staleContext()})
		if response.Code == http.StatusOK {
			t.Fatalf("the batch served a frozen context this lane never issued: %s", response.Body.String())
		}
		var wire struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
			t.Fatalf("%v %s", err, response.Body.String())
		}
		fenced := false
		for _, code := range []string{
			"PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE", "PROJECT_SESSION_REQUIRED",
			"PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_NOT_OPEN", "AUTHOR_WRITE_STALE",
			"PROJECT_HANDLE_CLOSED", "PRODUCTION_CLOSED",
		} {
			if wire.Error.Code == code {
				fenced = true
			}
		}
		if !fenced {
			t.Fatalf("the unleased batch refused with %v; want the admission fence's own lease code: %s",
				wire.Error.Code, response.Body.String())
		}
		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the unleased batch")
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT id FROM drafts`); len(rows) != 0 {
			t.Fatalf("the unleased batch wrote %d drafts", len(rows))
		}
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT operation_id FROM finalized_draft_import_operations`); len(rows) != 0 {
			t.Fatalf("the unleased batch wrote %d operation rows", len(rows))
		}
	})

	// Every payload the binder must refuse itself, each naming the code the
	// reference's own guard raises, and none of them writing a byte.
	t.Run("a malformed payload names its code", func(t *testing.T) {
		f := newProductionFinalizedDraftImportFixture(t)
		settled := productionImportRunResidualImage(t, f.database)
		full := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne, chapterTwo, chapterThree)

		// The arity, the frozen session and the path assertion come first.
		f.refuse(batch, []any{full, f.projectPath}, "INVALID_ARGUMENTS")
		f.refuse(batch, []any{full, 7, f.frozen}, "PROJECT_CONTEXT_REQUIRED")
		f.refuse(batch, []any{full, f.projectPath + "-other", f.frozen}, "PROJECT_CONTEXT_CHANGED")
		// The operation id: missing, then over the 200 code-unit bound (:52-58).
		missingID := map[string]any{"chapters": productionFinalizedDraftImportChaptersWire(chapterOne)}
		f.refuse(batch, []any{missingID, f.projectPath, f.frozen}, "FINALIZED_IMPORT_OPERATION_INVALID")
		longID := productionFinalizedDraftImportBatchRequest(strings.Repeat("a", 201), chapterOne)
		f.refuse(batch, []any{longID, f.projectPath, f.frozen}, "FINALIZED_IMPORT_OPERATION_INVALID")
		// The chapters array itself, then each field typeof/Number.isInteger guarded.
		notAnArray := map[string]any{"operationId": productionFinalizedDraftImportOperationID, "chapters": "one"}
		f.refuse(batch, []any{notAnArray, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTERS_INVALID")
		empty := map[string]any{"operationId": productionFinalizedDraftImportOperationID, "chapters": []any{}}
		f.refuse(batch, []any{empty, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTERS_INVALID")
		fractional := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			map[string]any{"chapterNumber": 1.5, "title": productionFinalizedDraftImportTitleOne,
				"content": productionFinalizedDraftImportBodyOne,
				"wordCount": float64(draftalg.CountDraftUnits(productionFinalizedDraftImportBodyOne))})
		f.refuse(batch, []any{fractional, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTERS_INVALID")
		numberTitle := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			map[string]any{"chapterNumber": float64(1), "title": float64(7),
				"content": productionFinalizedDraftImportBodyOne,
				"wordCount": float64(draftalg.CountDraftUnits(productionFinalizedDraftImportBodyOne))})
		f.refuse(batch, []any{numberTitle, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTERS_INVALID")
		stringCount := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			map[string]any{"chapterNumber": float64(1), "title": productionFinalizedDraftImportTitleOne,
				"content": productionFinalizedDraftImportBodyOne, "wordCount": "5"})
		f.refuse(batch, []any{stringCount, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTERS_INVALID")
		// A count that is an integer but not this body's own count is the store's
		// check (:79-81) and the same code.
		wrongCount := productionFinalizedDraftImportChapterWire(1, productionFinalizedDraftImportTitleOne,
			productionFinalizedDraftImportBodyOne)
		wrongCount["wordCount"] = wrongCount["wordCount"].(float64) + 1
		f.refuse(batch, []any{productionFinalizedDraftImportBatchRequest(
			productionFinalizedDraftImportOperationID, wrongCount), f.projectPath, f.frozen},
			"FINALIZED_IMPORT_CHAPTERS_INVALID")
		// An explicit null expectation is not the reference's undefined (:157-159).
		nullExpectation := productionFinalizedDraftImportBatchRequest(productionFinalizedDraftImportOperationID,
			chapterOne)
		nullExpectation["expectedAuthorityFingerprint"] = nil
		f.refuse(batch, []any{nullExpectation, f.projectPath, f.frozen}, "INVALID_ARGUMENTS")
		// The preview's own argument form, and a payload with no chapters at all.
		f.refuse(previewName, []any{"inspection-1", f.projectPath, f.frozen}, "IMPORT_INSPECTION_OWNER_UNAVAILABLE")
		f.refuse(previewName, []any{map[string]any{}, f.projectPath, f.frozen}, "FINALIZED_IMPORT_CHAPTERS_INVALID")

		productionImportRunResidualAssertUnchanged(t, f.database, settled, "the malformed payloads")
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT id FROM drafts`); len(rows) != 0 {
			t.Fatalf("the malformed payloads wrote %d drafts", len(rows))
		}
		if rows := productionImportRunWriteQuery(t, f.database, `SELECT id FROM contents`); len(rows) != 0 {
			t.Fatalf("the malformed payloads wrote %d content rows", len(rows))
		}
		productionFinalizedDraftImportAssertContinuity(t, f.database)
	})
}

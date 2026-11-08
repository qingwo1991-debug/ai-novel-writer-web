//go:build linux

package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The `db:import-run-*` family has 18 inventoried names. This test pins the exact
// split this lane ships: three reads served from the admitted author database and
// fifteen names refused. There is one subtest per operation - never a loop over
// the names, because a loop once hid a bound name behind an empty CI annotation -
// so a failure names the operation that moved. Every read is compared with the
// rows the fixture really committed, every refusal is asserted, and none of them
// may touch the admitted bytes.
//
// The fixture is the same original-v7 baseline the editor read tests copy: two
// import runs (one resumable author run, one completed reference run), their
// frozen chapters and their source rows, all written before the lane is opened so
// the admitted digest covers them.
const (
	productionImportRunResumableID     = "production-import-run-1"
	productionImportRunCompletedID     = "production-import-run-2"
	productionImportRunDoneSourceID    = "production-source-done"
	productionImportRunWaitSourceID    = "production-source-wait"
	productionImportRunDoneDisplayName = "门.txt"
	productionImportRunWaitDisplayName = "灯.md"
	productionImportRunChapterOneTitle = "第一章 门"
	productionImportRunChapterTwoTitle = "第二章 灯"
	productionImportRunChapterOneBody  = "第一章快照：他推开门。"
	productionImportRunChapterTwoBody  = "第二章快照：她关上门。"
	productionImportRunManifestSeed    = "production-import-run-manifest"
	productionImportRunAuthoritySeed   = "production-import-run-authority"
	productionImportRunSourceSeed      = "production-import-run-source"
	productionImportRunOwner           = "production-import-owner"
)

// productionImportRunDigest is the content identity the fixture writes into the
// database and every read must hand back unchanged.
func productionImportRunDigest(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

// productionImportRunLease is a well-formed execution lease. None of the refused
// names reaches it, but a request shaped the way the renderer really sends it
// makes the 501 meaningful instead of an arity accident.
func productionImportRunLease() map[string]any {
	return map[string]any{"owner": productionImportRunOwner, "epoch": float64(1)}
}

// productionImportRunSourceDisplayJSON is the run-level display list: both
// sources, so the read has to project two entries and the unfinished-source read
// only the pending one.
func productionImportRunSourceDisplayJSON() string {
	return fmt.Sprintf(`[{"displayName":"%s","mediaType":"text/plain","size":12},{"displayName":"%s","mediaType":"text/markdown","size":20}]`,
		productionImportRunDoneDisplayName, productionImportRunWaitDisplayName)
}

// seedProductionImportRunRows commits the two runs, their chapters and their
// sources through hand-written INSERTs (the shipped store owns no run-creation
// method) and then folds the write-ahead log into the admitted file, so the bytes
// the test reads are the bytes the lane admits.
func seedProductionImportRunRows(t *testing.T, projectPath string) {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []string{
		// Children first: every one of these tables carries a foreign key into
		// import_runs, so the parent row can only go last.
		"DELETE FROM import_run_knowledge_receipts",
		"DELETE FROM import_run_source_chapters",
		"DELETE FROM import_run_chapters",
		"DELETE FROM import_run_sources",
		"DELETE FROM import_effect_ledger",
		"DELETE FROM import_run_receipts",
		"DELETE FROM import_reference_documents",
		"DELETE FROM import_runs",
		// The resumable author run. Its stage and status keep it inside
		// db:import-run-list-resumable's own filter, and its manifest facts are
		// the ones an author run carries.
		fmt.Sprintf(`INSERT INTO import_runs (id, purpose, root_run_id, effect_namespace,
 source_fingerprint, manifest_fingerprint, authority_fingerprint, source_display_json, locale,
 stage, status, resumable, total_chapters, total_content_size, manifest_chapter_count,
 manifest_content_size, manifest_word_count, completed_chapters, created_at, updated_at)
 VALUES ('%s', 'author-manuscript', '%s', 'import:%s', '%s', '%s', '%s', '%s', 'zh-CN',
 'global', 'ready', 1, 4, 48, 4, 48, 24, 0, '2024-05-06 09:10:11', '2024-05-06 09:10:12')`,
			productionImportRunResumableID, productionImportRunResumableID, productionImportRunResumableID,
			productionImportRunDigest(productionImportRunSourceSeed),
			productionImportRunDigest(productionImportRunManifestSeed),
			productionImportRunDigest(productionImportRunAuthoritySeed),
			productionImportRunSourceDisplayJSON()),
		// The completed reference run. It is not resumable, so it must not appear
		// in the resumable list; it still has to be readable by identity, with its
		// base run and its completion time.
		fmt.Sprintf(`INSERT INTO import_runs (id, purpose, root_run_id, effect_namespace,
 source_fingerprint, manifest_fingerprint, source_display_json, locale, stage, status, resumable,
 total_chapters, total_content_size, manifest_chapter_count, manifest_content_size,
 manifest_word_count, completed_chapters, base_run_id, created_at, updated_at, completed_at)
 VALUES ('%s', 'reference', '%s', 'import:%s', '%s', '%s', '[]', 'zh-CN', 'completed', 'completed', 0,
 2, 24, 2, 24, 12, 2, '%s', '2024-05-06 09:10:13', '2024-05-06 09:10:14', '2024-05-06 09:10:15')`,
			productionImportRunCompletedID, productionImportRunCompletedID, productionImportRunCompletedID,
			productionImportRunDigest(productionImportRunSourceSeed + "-completed"),
			productionImportRunDigest(productionImportRunManifestSeed + "-completed"),
			productionImportRunResumableID),
		// One finished source and one still pending: the run snapshot reports both
		// and the unfinished projection only the pending display object.
		fmt.Sprintf(`INSERT INTO import_run_sources (run_id, source_index, source_id,
 source_fingerprint, display_json, status, chapter_count, content_size, word_count, updated_at)
 VALUES ('%s', 0, '%s', '%s', '{"displayName":"%s","mediaType":"text/plain","size":12}',
 'completed', 2, 24, 12, '2024-05-06 09:10:16')`,
			productionImportRunResumableID, productionImportRunDoneSourceID,
			productionImportRunDigest(productionImportRunSourceSeed), productionImportRunDoneDisplayName),
		fmt.Sprintf(`INSERT INTO import_run_sources (run_id, source_index, source_id,
 source_fingerprint, display_json, status, chapter_count, content_size, word_count, updated_at)
 VALUES ('%s', 1, '%s', '%s', '{"displayName":"%s","mediaType":"text/markdown","size":20}',
 'pending', 0, 0, 0, '2024-05-06 09:10:17')`,
			productionImportRunResumableID, productionImportRunWaitSourceID,
			productionImportRunDigest(productionImportRunSourceSeed), productionImportRunWaitDisplayName),
		// Two frozen chapter bodies with their real digests and byte sizes, so the
		// read's own re-hash succeeds instead of refusing SNAPSHOT_CORRUPT.
		fmt.Sprintf(`INSERT INTO import_run_chapters (run_id, chapter_number, source_id,
 source_chapter_number, title, content_fingerprint, content_size, content_snapshot)
 VALUES ('%s', 1, '%s', 1, '%s', '%s', %d, '%s')`,
			productionImportRunResumableID, productionImportRunDoneSourceID,
			productionImportRunChapterOneTitle,
			productionImportRunDigest(productionImportRunChapterOneBody),
			len(productionImportRunChapterOneBody), productionImportRunChapterOneBody),
		fmt.Sprintf(`INSERT INTO import_run_chapters (run_id, chapter_number, source_id,
 source_chapter_number, title, content_fingerprint, content_size, content_snapshot)
 VALUES ('%s', 2, '%s', 2, '%s', '%s', %d, '%s')`,
			productionImportRunResumableID, productionImportRunDoneSourceID,
			productionImportRunChapterTwoTitle,
			productionImportRunDigest(productionImportRunChapterTwoBody),
			len(productionImportRunChapterTwoBody), productionImportRunChapterTwoBody),
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatalf("seed %q: %v", statement, err)
		}
	}
	// The lane admits the file itself, so the seeded rows must be checkpointed
	// into it: no sidecar may carry rows the admitted digest does not cover.
	productionEditorCheckpoint(t, db, database)
}

// productionImportRunText reads one string field of a decoded wire row.
func productionImportRunText(t *testing.T, row map[string]any, key string) string {
	t.Helper()
	value, ok := row[key].(string)
	if !ok {
		t.Fatalf("%s is %T, not a string: %v", key, row[key], row)
	}
	return value
}

// productionImportRunCount reads one numeric field of a decoded wire row. JSON
// numbers decode to float64, so the safe-integer surface is compared exactly.
func productionImportRunCount(t *testing.T, row map[string]any, key string) float64 {
	t.Helper()
	value, ok := row[key].(float64)
	if !ok {
		t.Fatalf("%s is %T, not a number: %v", key, row[key], row)
	}
	return value
}

// productionImportRunList reads one array field of a decoded wire row.
func productionImportRunList(t *testing.T, row map[string]any, key string) []any {
	t.Helper()
	value, ok := row[key].([]any)
	if !ok {
		t.Fatalf("%s is %T, not an array: %v", key, row[key], row)
	}
	return value
}

// productionImportRunStaysUnbound asserts one name this file deliberately does
// not install. The registry answers 501 before any handler, lease or database is
// reached, so no argument list and no lease can turn it into a 200.
func productionImportRunStaysUnbound(t *testing.T, client *productionDurableClient, operation string, args []any) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("%s answered %d, but this file binds no handler for it: %s",
			operation, response.Code, response.Body.String())
	}
	var wire struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	if wire.Error.Code != "GO_OPERATION_NOT_MIGRATED" {
		t.Fatalf("%s failed closed with %q instead of GO_OPERATION_NOT_MIGRATED: %s",
			operation, wire.Error.Code, response.Body.String())
	}
}

// TestProductionImportRunReadsServeAdmittedAuthorDatabase is the import-run half
// of this lane. The three bound reads answer 200 with the seeded values, refuse a
// lease this server never issued, and leave the admitted bytes untouched; the
// fifteen names this file refuses keep failing closed at 501 rather than being
// published as handlers that could only refuse or, worse, that could write.
func TestProductionImportRunReadsServeAdmittedAuthorDatabase(t *testing.T) {
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	seedProductionImportRunRows(t, projectPath)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	expected, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	// unchanged re-reads the admitted database: a served read and a refused lease
	// must both leave the author's bytes exactly as they were seeded.
	unchanged := func(t *testing.T, label string) {
		t.Helper()
		after, err := os.ReadFile(database)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, expected) {
			t.Fatalf("%s changed the admitted author database", label)
		}
	}

	author := newProductionDurableClient(t, handler, productionEditorTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "import-run-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == "" || frozen["leaseId"] == nil {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	stale := map[string]any{"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease"}

	// db:import-run-get: the resumable author run with its manifest facts, both
	// source displays and no invented checkpoint; then the completed reference
	// run, an absent id (null, never a refusal), and a stale lease.
	t.Run("db:import-run-get", func(t *testing.T) {
		var run map[string]any
		productionEditorDecode(t, author, "db:import-run-get",
			[]any{productionImportRunResumableID, projectPath, frozen}, &run)
		if productionImportRunText(t, run, "id") != productionImportRunResumableID ||
			productionImportRunText(t, run, "purpose") != "author-manuscript" ||
			productionImportRunText(t, run, "stage") != "global" ||
			productionImportRunText(t, run, "status") != "ready" ||
			productionImportRunText(t, run, "locale") != "zh-CN" {
			t.Fatalf("db:import-run-get did not read the seeded run: %v", run)
		}
		if run["resumable"] != true || run["cancelRequested"] != false {
			t.Fatalf("db:import-run-get lost the seeded run flags: %v", run)
		}
		if productionImportRunCount(t, run, "totalChapters") != 4 ||
			productionImportRunCount(t, run, "manifestChapterCount") != 4 ||
			productionImportRunCount(t, run, "manifestContentSize") != 48 ||
			productionImportRunCount(t, run, "manifestWordCount") != 24 {
			t.Fatalf("db:import-run-get lost the seeded manifest facts: %v", run)
		}
		if productionImportRunText(t, run, "manifestFingerprint") != productionImportRunDigest(productionImportRunManifestSeed) ||
			productionImportRunText(t, run, "authorityFingerprint") != productionImportRunDigest(productionImportRunAuthoritySeed) {
			t.Fatalf("db:import-run-get lost the seeded identities: %v", run)
		}
		if display := productionImportRunList(t, run, "sourceDisplay"); len(display) != 2 {
			t.Fatalf("db:import-run-get returned %d source displays; want both seeded sources: %v", len(display), display)
		}
		unfinished := productionImportRunList(t, run, "unfinishedSourceDisplay")
		if len(unfinished) != 1 {
			t.Fatalf("db:import-run-get returned %d unfinished sources; want only the pending one: %v", len(unfinished), unfinished)
		}
		pending, ok := unfinished[0].(map[string]any)
		if !ok || pending["displayName"] != productionImportRunWaitDisplayName {
			t.Fatalf("db:import-run-get lost the pending source display: %v", unfinished[0])
		}
		// The ledger is present in this schema and holds no completed row for the
		// run, so the projection must be an empty JSON object.
		batches, ok := run["completedBatches"].(map[string]any)
		if !ok || len(batches) != 0 {
			t.Fatalf("db:import-run-get invented checkpoints: %v", run["completedBatches"])
		}

		var completed map[string]any
		productionEditorDecode(t, author, "db:import-run-get",
			[]any{productionImportRunCompletedID, projectPath, frozen}, &completed)
		if completed["resumable"] != false ||
			productionImportRunText(t, completed, "status") != "completed" ||
			productionImportRunText(t, completed, "baseRunId") != productionImportRunResumableID ||
			productionImportRunText(t, completed, "completedAt") != "2024-05-06 09:10:15" {
			t.Fatalf("db:import-run-get did not read the completed seeded run: %v", completed)
		}
		// A reference run carries no manifest fingerprint: the field is omitted,
		// not zeroed.
		if _, present := completed["manifestFingerprint"]; present {
			t.Fatalf("db:import-run-get invented a manifest fingerprint for a reference run: %v", completed)
		}

		response := author.call(t, "db:import-run-get",
			[]any{"production-import-run-absent", projectPath, frozen})
		if response.Code != http.StatusOK {
			t.Fatalf("db:import-run-get refused an unknown id with HTTP %d %s", response.Code, response.Body.String())
		}
		var absent struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &absent); err != nil || string(absent.Result) != "null" {
			t.Fatalf("db:import-run-get answered an unknown id with %v instead of null: %s", err, response.Body.String())
		}

		productionEditorRefused(t, author, "stale lease", "db:import-run-get",
			[]any{productionImportRunResumableID, projectPath, stale})
		unchanged(t, "db:import-run-get")
	})

	// db:import-run-list-resumable: exactly the resumable run, and the completed
	// one filtered out by the store's own predicate.
	t.Run("db:import-run-list-resumable", func(t *testing.T) {
		var runs []map[string]any
		productionEditorDecode(t, author, "db:import-run-list-resumable", []any{projectPath, frozen}, &runs)
		if len(runs) != 1 {
			t.Fatalf("db:import-run-list-resumable returned %d runs; want only the resumable one: %v", len(runs), runs)
		}
		if productionImportRunText(t, runs[0], "id") != productionImportRunResumableID {
			t.Fatalf("db:import-run-list-resumable returned the wrong run: %v", runs[0])
		}
		if productionImportRunCount(t, runs[0], "totalChapters") != 4 {
			t.Fatalf("db:import-run-list-resumable lost the seeded manifest facts: %v", runs[0])
		}
		productionEditorRefused(t, author, "stale lease", "db:import-run-list-resumable",
			[]any{projectPath, stale})
		unchanged(t, "db:import-run-list-resumable")
	})

	// db:import-run-list-chapters: both frozen chapters in order with their real
	// bodies, digests and byte sizes, a paged read after the first, an empty page
	// for the run that owns no chapters, and a stale lease.
	t.Run("db:import-run-list-chapters", func(t *testing.T) {
		var chapters []map[string]any
		productionEditorDecode(t, author, "db:import-run-list-chapters",
			[]any{productionImportRunResumableID, float64(0), float64(10), projectPath, frozen}, &chapters)
		if len(chapters) != 2 {
			t.Fatalf("db:import-run-list-chapters returned %d chapters; want the two seeded ones: %v", len(chapters), chapters)
		}
		first := chapters[0]
		if productionImportRunCount(t, first, "number") != 1 ||
			productionImportRunText(t, first, "title") != productionImportRunChapterOneTitle ||
			productionImportRunText(t, first, "content") != productionImportRunChapterOneBody ||
			productionImportRunCount(t, first, "contentSize") != float64(len(productionImportRunChapterOneBody)) ||
			productionImportRunText(t, first, "contentFingerprint") != productionImportRunDigest(productionImportRunChapterOneBody) {
			t.Fatalf("db:import-run-list-chapters did not read the first seeded chapter: %v", first)
		}
		second := chapters[1]
		if productionImportRunCount(t, second, "number") != 2 ||
			productionImportRunText(t, second, "title") != productionImportRunChapterTwoTitle ||
			productionImportRunText(t, second, "content") != productionImportRunChapterTwoBody ||
			productionImportRunCount(t, second, "contentSize") != float64(len(productionImportRunChapterTwoBody)) ||
			productionImportRunText(t, second, "contentFingerprint") != productionImportRunDigest(productionImportRunChapterTwoBody) {
			t.Fatalf("db:import-run-list-chapters did not read the second seeded chapter: %v", second)
		}

		var page []map[string]any
		productionEditorDecode(t, author, "db:import-run-list-chapters",
			[]any{productionImportRunResumableID, float64(1), float64(10), projectPath, frozen}, &page)
		if len(page) != 1 || productionImportRunCount(t, page[0], "number") != 2 {
			t.Fatalf("db:import-run-list-chapters did not page after chapter 1: %v", page)
		}

		var none []map[string]any
		productionEditorDecode(t, author, "db:import-run-list-chapters",
			[]any{productionImportRunCompletedID, float64(0), float64(10), projectPath, frozen}, &none)
		if len(none) != 0 {
			t.Fatalf("db:import-run-list-chapters returned %d chapters for a run that owns none: %v", len(none), none)
		}

		productionEditorRefused(t, author, "stale lease", "db:import-run-list-chapters",
			[]any{productionImportRunResumableID, float64(0), float64(10), projectPath, stale})
		unchanged(t, "db:import-run-list-chapters")
	})

	// db:import-run-prepare-inspection: refused. Run creation; it needs the
	// tab-bound in-memory inspection owner this lane does not own.
	t.Run("db:import-run-prepare-inspection", func(t *testing.T) {
		productionImportRunStaysUnbound(t, author, "db:import-run-prepare-inspection",
			[]any{map[string]any{}, projectPath, frozen})
		unchanged(t, "db:import-run-prepare-inspection")
	})

	// The two effect-receipt names are served now (their source proof reads the run
	// on the receipt's own connection), so this file no longer pins them unbound.
}

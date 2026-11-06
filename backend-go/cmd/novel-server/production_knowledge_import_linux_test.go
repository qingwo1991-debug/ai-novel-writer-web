//go:build linux

package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3" // Registers the database/sql driver "sqlite3".
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This file proves the two knowledge-base imports production_knowledge_import_linux.go
// binds, over the real HTTP handler and the real frozen project session:
//
//	kb:import-text            writes a document, its chunks and its copy
//	kb:import-finalized-text  links the frozen finalization and is idempotent
//
// Every assertion is on what the import left behind - the canonical snapshot
// decoded back through knowledgedomain, the reference-format copy, the
// finalization_outbox column read over a separate read-only connection, and a
// knowledgequery search over the written store - never on the receipt alone.
// Three refusals are proved too, each with the admitted database left
// byte-identical, because a refused import must not spend the author's store.
const (
	// productionKnowledgeOpenNonce opens the one project session these tests use.
	productionKnowledgeOpenNonce = "knowledge-open-1"
	// productionKnowledgeFinalizationID is the outbox identity chapter 1 is
	// finalized under, so the finalized arm has a real frozen receipt to cite.
	productionKnowledgeFinalizationID = "ci-knowledge-finalization-1"
	// productionKnowledgeChapterTitle is that row's chapter title, which the
	// imported chunk rows must carry.
	productionKnowledgeChapterTitle = "门"
	// productionKnowledgeChapterFile is the name the browser sends for a
	// finalized chapter (finalize-chapter.command.ts constructs `… .txt`), so the
	// chapter meta here comes from the outbox row rather than from the name.
	productionKnowledgeChapterFile = "第1章 门.txt"
	// productionKnowledgeNamedFile is a name parseChapterMetaFromFileName accepts
	// (第N章 <title> 正文.md), so the plain-text arm proves the name-derived meta.
	productionKnowledgeNamedFile = "第3章 灯 正文.md"
	// productionKnowledgeNamedChapter and productionKnowledgeNamedTitle are what
	// that name must produce.
	productionKnowledgeNamedChapter = int64(3)
	productionKnowledgeNamedTitle   = "灯"
)

// The prose is two paragraphs of BMP-only CJK. Each is shorter than the 500-unit
// chunk size, so neither takes the sentence-splitting path, and their sum is
// longer than it, so the second paragraph starts a new chunk that still carries
// the 50-unit overlap chunkText takes from the first
// (electron/embedding.ts:398-411). A naive splitter - by paragraph, by byte
// length, or without overlap - lands somewhere else, so the exact chunk texts
// below are what separates this port from an approximation.
var (
	productionKnowledgeProseOne = strings.Repeat("他推开门，门后是一条没有尽头的走廊。", 16)
	productionKnowledgeProseTwo = strings.Repeat("走廊尽头挂着一盏不灭的灯。", 20)
	productionKnowledgeProse    = productionKnowledgeProseOne + "\n\n" + productionKnowledgeProseTwo
)

// productionKnowledgeProseChunks is chunkText's expected output for that prose,
// derived from the algorithm rather than from the implementation under test: the
// first paragraph is emitted whole, and the second chunk is the overlap of the
// first plus the separator plus the second paragraph.
func productionKnowledgeProseChunks(t *testing.T) []string {
	t.Helper()
	units := []rune(productionKnowledgeProseOne)
	if len(units) <= productionKnowledgeChunkOverlap {
		t.Fatalf("the first paragraph is %d units, too short to overlap", len(units))
	}
	return []string{
		productionKnowledgeProseOne,
		string(units[len(units)-productionKnowledgeChunkOverlap:]) + "\n\n" + productionKnowledgeProseTwo,
	}
}

// productionKnowledgeSeed turns the editor-read fixture's chapter 1 into the
// finalized chapter both arms work on: the frozen outbox row the finalized arm
// re-reads, the continuity generation its summary joins, and the roster row the
// projection refresh keeps. The rows are checkpointed into the admitted file and
// its bytes are returned as the baseline a refusal has to leave untouched.
func productionKnowledgeSeed(t *testing.T, projectPath string) []byte {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=rw&_foreign_keys=on"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	statements := []struct {
		query string
		args  []any
	}{
		// The stored prose is the exact text both arms import, so
		// readFinalizedSource's snapshot.content === text clause is satisfied by
		// the row the API really reads.
		{"UPDATE contents SET body=? WHERE id=1", []any{productionKnowledgeProse}},
		{"UPDATE drafts SET status='finalized' WHERE id=1", nil},
		{`INSERT INTO continuity_projection_meta (id, generation) VALUES ('main', 1)
 ON CONFLICT(id) DO UPDATE SET generation=excluded.generation, stale_from_chapter=NULL`, nil},
		{`INSERT INTO character_roster_meta (id, schema_version, revision, migration_state,
 legacy_markdown, projection_hash, fact_hash) VALUES ('main', 1, 0, 'ready', '', '', '')
 ON CONFLICT(id) DO UPDATE SET schema_version=excluded.schema_version,
 revision=excluded.revision, migration_state=excluded.migration_state`, nil},
		{`INSERT INTO finalization_outbox (finalization_id, draft_id, chapter_number, chapter_title,
 content_hash, content_revision, content_snapshot, target_file_name)
 VALUES (?, 1, 1, ?, ?, 1, ?, ?)`,
			[]any{productionKnowledgeFinalizationID, productionKnowledgeChapterTitle,
				productionKnowledgeHash(productionKnowledgeProse), productionKnowledgeProse,
				productionKnowledgeChapterFile}},
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement.query, statement.args...); err != nil {
			_ = db.Close()
			t.Fatalf("seed %s: %v", statement.query, err)
		}
	}
	productionEditorCheckpoint(t, db, database)
	baseline, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	return baseline
}

// productionKnowledgeLane seeds that fixture into a fresh admitted project,
// starts the real production lane over it and opens the project through HTTP, so
// every call below travels the adapter the renderer talks to.
func productionKnowledgeLane(t *testing.T) (*productionDurableClient, string, map[string]any, []byte) {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	baseline := productionKnowledgeSeed(t, projectPath)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	client, frozen := productionExecuteSession(t, handler, projectPath, productionKnowledgeOpenNonce)
	return client, projectPath, frozen, baseline
}

// productionKnowledgeCall performs one operation and returns its result object. A
// 501 fails immediately: every name this test claims is bound.
func productionKnowledgeCall(t *testing.T, client *productionDurableClient, operation string, args []any) map[string]any {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil || wire.Result == nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return wire.Result
}

// productionKnowledgeRefusal asserts the envelope both imports keep for a
// business failure: HTTP 200, success=false, and the bare symbolic code in
// `error` - knowledgeFailureDetails (knowledge-base.ts:67-74) attaches errorCode
// only to the registered legacy-migration refusal.
func productionKnowledgeRefusal(t *testing.T, client *productionDurableClient, label, operation string, args []any, wantCode string) {
	t.Helper()
	result := productionKnowledgeCall(t, client, operation, args)
	if result["success"] != false {
		t.Fatalf("%s: %s answered success instead of %s: %v", label, operation, wantCode, result)
	}
	if result["error"] != wantCode {
		t.Fatalf("%s: %s refused with %v instead of %s", label, operation, result["error"], wantCode)
	}
	if _, present := result["errorCode"]; present && wantCode != productionKnowledgeCodeLegacyBlocked {
		t.Fatalf("%s: %s attached errorCode to %s", label, operation, wantCode)
	}
}

// productionKnowledgeCount reads a receipt count back as an integer.
func productionKnowledgeCount(t *testing.T, value any) int64 {
	t.Helper()
	count, ok := productionKnowledgeSafeIntegerOf(value)
	if !ok {
		t.Fatalf("receipt count %v is not an integer", value)
	}
	return count
}

// productionKnowledgeSnapshotOfProject decodes the canonical snapshot the import
// wrote, with the same JSON settings the lane's own reader uses, so a document a
// test cannot find here is a document nothing can find.
func productionKnowledgeSnapshotOfProject(t *testing.T, projectPath string) knowledgedomain.Snapshot {
	t.Helper()
	file := filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("the import wrote no canonical snapshot: %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var snapshot knowledgedomain.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		t.Fatalf("the canonical snapshot does not decode: %v", err)
	}
	return snapshot
}

// productionKnowledgeRows is one table's rows out of that snapshot.
func productionKnowledgeRows(t *testing.T, snapshot knowledgedomain.Snapshot, name string) []map[string]any {
	t.Helper()
	for _, table := range snapshot.Tables {
		if table.Name == name {
			return table.Records
		}
	}
	t.Fatalf("the canonical snapshot holds no %s table", name)
	return nil
}

// productionKnowledgeTexts orders one document's chunk texts by chunkIndex, so a
// reordered store fails here rather than passing on row count alone.
func productionKnowledgeTexts(t *testing.T, rows []map[string]any) []string {
	t.Helper()
	texts := make([]string, len(rows))
	for _, row := range rows {
		position, ok := productionKnowledgeSafeIntegerOf(row["chunkIndex"])
		if !ok || position < 0 || position >= int64(len(rows)) {
			t.Fatalf("chunk row carries no usable chunkIndex: %v", row["chunkIndex"])
		}
		text, ok := row["text"].(string)
		if !ok {
			t.Fatal("chunk row carries no text")
		}
		texts[position] = text
	}
	return texts
}

// productionKnowledgeCopyPath is where the reference's own copy of a document
// lives (vector-store.ts:135-142).
func productionKnowledgeCopyPath(projectPath, documentID string) string {
	return filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeCopyDirectory,
		productionKnowledgeCopyName(documentID))
}

// productionKnowledgeSearch runs the FTS lane a served kb:search runs, over the
// store the import wrote and the copies it wrote beside it. It is the proof that
// the write landed where the reads look.
func productionKnowledgeSearch(t *testing.T, projectPath, query string) []knowledgequery.SearchResult {
	t.Helper()
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	storage, err := os.OpenRoot(filepath.Join(projectPath, projectfs.StorageDirectory))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()
	capture, err := knowledgequery.NewSearchCapture(context.Background(), snapshot, storage,
		productionKnowledgeCopyBudget())
	if err != nil {
		t.Fatalf("the imported store does not admit a search capture: %v", err)
	}
	results, err := capture.SearchFTS(context.Background(), query, 5, nil, nil)
	if err != nil {
		t.Fatalf("a search over the imported store failed: %v", err)
	}
	return results
}

// productionKnowledgeReadback opens the admitted database read-only, so a column
// this test asserts was written by the lane's own connection, not by the test's.
func productionKnowledgeReadback(t *testing.T, projectPath string) *sql.DB {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestProductionKnowledgeImportTextWritesSearchableStore is the plain-text arm:
// one kb:import-text must leave exactly one document with the reference's chunk
// boundaries, its copy, the name-derived chapter meta - and it must be findable
// by the same FTS capture a served kb:search builds.
func TestProductionKnowledgeImportTextWritesSearchableStore(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	answer := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-text refused an importable chapter: %v", answer)
	}
	documentID, ok := answer["docId"].(string)
	if !ok || documentID == "" {
		t.Fatalf("kb:import-text returned no document identity receipt: %v", answer)
	}
	if count := productionKnowledgeCount(t, answer["chunkCount"]); count != 2 {
		t.Fatalf("kb:import-text chunked the chapter into %d chunks instead of 2", count)
	}
	if _, present := answer["idempotent"]; present {
		t.Fatal("kb:import-text claimed an idempotency field that belongs to kb:import-finalized-text")
	}
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	documents := productionKnowledgeRows(t, snapshot, productionKnowledgeTableDocuments)
	if len(documents) != 1 {
		t.Fatalf("the import left %d document rows instead of 1", len(documents))
	}
	document := documents[0]
	if document["id"] != documentID || document["fileName"] != productionKnowledgeNamedFile ||
		document["corpusKind"] != productionKnowledgeCorpusKind ||
		document["filePath"] != productionKnowledgeCopyMarker+documentID {
		t.Fatalf("the document row does not describe the import: %v", document)
	}
	if count, _ := productionKnowledgeSafeIntegerOf(document["chunkCount"]); count != 2 {
		t.Fatalf("the document row records %v chunks instead of 2", document["chunkCount"])
	}
	chunks := productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks)
	if len(chunks) != 2 {
		t.Fatalf("the import left %d chunk rows instead of 2", len(chunks))
	}
	want := productionKnowledgeProseChunks(t)
	for position, text := range productionKnowledgeTexts(t, chunks) {
		if text != want[position] {
			t.Fatalf("chunk %d does not match chunkText's boundary", position)
		}
	}
	// parseChapterMetaFromFileName (knowledge-base.ts:262-271) is what gives a
	// 正文.md import its chapter meta; every row must carry it.
	for _, chunk := range chunks {
		if number, _ := productionKnowledgeSafeIntegerOf(chunk["chapterNumber"]); number != productionKnowledgeNamedChapter {
			t.Fatalf("the chunk row records chapter %v instead of %d", chunk["chapterNumber"], productionKnowledgeNamedChapter)
		}
		if chunk["chapterTitle"] != productionKnowledgeNamedTitle {
			t.Fatalf("the chunk row records chapter title %v instead of %s", chunk["chapterTitle"], productionKnowledgeNamedTitle)
		}
		if chunk["docId"] != documentID || chunk["importedAt"] != document["importedAt"] {
			t.Fatal("the chunk rows do not share their document's identity and timestamp")
		}
	}
	// The reference-format copy is written too, and it is what the repair and
	// idempotency checks read (vector-store.ts:156-166).
	copied := struct {
		Content     string `json:"content"`
		IndexedHash string `json:"indexedHash"`
		Edited      bool   `json:"edited"`
		IndexDirty  bool   `json:"indexDirty"`
	}{}
	raw, err := os.ReadFile(productionKnowledgeCopyPath(projectPath, documentID))
	if err != nil {
		t.Fatalf("the import wrote no knowledge copy: %v", err)
	}
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatalf("the knowledge copy does not decode: %v", err)
	}
	if copied.Content != productionKnowledgeProse || copied.Edited || copied.IndexDirty ||
		copied.IndexedHash != productionKnowledgeHash(productionKnowledgeProse) {
		t.Fatal("the knowledge copy does not hold the imported text and its hash")
	}
	// A term from the second chunk and a term from the first must both be found:
	// the store is searchable across the whole chapter, not just its head.
	for _, query := range []string{"不灭的灯", "没有尽头"} {
		results := productionKnowledgeSearch(t, projectPath, query)
		if len(results) == 0 {
			t.Fatalf("a search for %s found nothing in the imported store", query)
		}
		found := false
		for _, result := range results {
			if result.FileName == productionKnowledgeNamedFile && strings.Contains(result.Text, query) {
				found = true
			}
		}
		if !found {
			t.Fatalf("a search for %s did not return the imported chunk", query)
		}
	}
}

// TestProductionKnowledgeImportTextSupersedesSameFileName reproduces the
// reference's prune-and-insert: kb:import-text mints a fresh identity per call, so
// a second import of the same fileName leaves ONE document - the new one - and
// the superseded document's copy is removed with it (vector-store.ts:1011-1049).
func TestProductionKnowledgeImportTextSupersedesSameFileName(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	first := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProseOne, productionKnowledgeNamedFile, projectPath, frozen})
	if first["success"] != true {
		t.Fatalf("kb:import-text refused the first import: %v", first)
	}
	superseded, _ := first["docId"].(string)
	if superseded == "" {
		t.Fatalf("the first import returned no document identity: %v", first)
	}
	second := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if second["success"] != true {
		t.Fatalf("kb:import-text refused the second import: %v", second)
	}
	current, _ := second["docId"].(string)
	if current == "" || current == superseded {
		t.Fatalf("the second import did not mint a fresh document identity: %v", second)
	}
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	documents := productionKnowledgeRows(t, snapshot, productionKnowledgeTableDocuments)
	if len(documents) != 1 || documents[0]["id"] != current {
		t.Fatalf("the superseded document survived the second import: %d rows", len(documents))
	}
	for _, chunk := range productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks) {
		if chunk["docId"] != current {
			t.Fatal("a chunk of the superseded document survived the second import")
		}
	}
	if _, err := os.Lstat(productionKnowledgeCopyPath(projectPath, superseded)); !os.IsNotExist(err) {
		t.Fatalf("the superseded knowledge copy was not removed: %v", err)
	}
	if _, err := os.Lstat(productionKnowledgeCopyPath(projectPath, current)); err != nil {
		t.Fatalf("the current knowledge copy is missing: %v", err)
	}
}

// TestProductionKnowledgeImportFinalizedTextLinksAndIsIdempotent is the arm 终稿
// actually calls: it writes the deterministic document, links the frozen
// finalization to it, and answers the same identity with idempotent:true the
// second time instead of importing the chapter again.
func TestProductionKnowledgeImportFinalizedTextLinksAndIsIdempotent(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	source := map[string]any{
		"draftId":        int64(1),
		"finalizationId": productionKnowledgeFinalizationID,
		"chapterNumber":  int64(1),
		"contentHash":    productionKnowledgeHash(productionKnowledgeProse),
	}
	args := []any{productionKnowledgeProse, productionKnowledgeChapterFile, source, frozen}
	answer := productionKnowledgeCall(t, client, productionKnowledgeImportFinalizedName, args)
	if answer["success"] != true {
		t.Fatalf("kb:import-finalized-text refused a finalized chapter: %v", answer)
	}
	documentID, _ := answer["docId"].(string)
	if !strings.HasPrefix(documentID, productionKnowledgeFinalizationPrefix) || len(documentID) !=
		len(productionKnowledgeFinalizationPrefix)+64 {
		t.Fatalf("the finalized import did not use the deterministic document identity: %v", answer)
	}
	if answer["idempotent"] != false {
		t.Fatalf("the first finalized import claimed to be idempotent: %v", answer)
	}
	if count := productionKnowledgeCount(t, answer["chunkCount"]); count != 2 {
		t.Fatalf("the finalized import chunked the chapter into %d chunks instead of 2", count)
	}
	// The link is a real column of the admitted database, read here through a
	// connection that could not have written it.
	var linked string
	if err := productionKnowledgeReadback(t, projectPath).QueryRow(
		"SELECT knowledge_document_id FROM finalization_outbox WHERE finalization_id=?",
		productionKnowledgeFinalizationID).Scan(&linked); err != nil {
		t.Fatalf("finalization_outbox readback: %v", err)
	}
	if linked != documentID {
		t.Fatalf("the finalized import linked %v instead of the document it wrote", linked)
	}
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	documents := productionKnowledgeRows(t, snapshot, productionKnowledgeTableDocuments)
	if len(documents) != 1 || documents[0]["id"] != documentID {
		t.Fatalf("the finalized import left %d document rows", len(documents))
	}
	chunks := productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks)
	want := productionKnowledgeProseChunks(t)
	for position, text := range productionKnowledgeTexts(t, chunks) {
		if text != want[position] {
			t.Fatalf("finalized chunk %d does not match chunkText's boundary", position)
		}
	}
	// The finalized arm passes the chapter pair as an override, so the rows carry
	// the frozen chapter rather than anything derived from the file name.
	for _, chunk := range chunks {
		if number, _ := productionKnowledgeSafeIntegerOf(chunk["chapterNumber"]); number != 1 {
			t.Fatalf("the finalized chunk row records chapter %v instead of 1", chunk["chapterNumber"])
		}
		if chunk["chapterTitle"] != productionKnowledgeChapterTitle {
			t.Fatalf("the finalized chunk row records chapter title %v", chunk["chapterTitle"])
		}
	}
	// The second call is the same frozen identity, so it must not import again.
	again := productionKnowledgeCall(t, client, productionKnowledgeImportFinalizedName, args)
	if again["success"] != true || again["idempotent"] != true {
		t.Fatalf("the second finalized import did not answer idempotent: %v", again)
	}
	if again["docId"] != documentID {
		t.Fatalf("the second finalized import returned a different document identity: %v", again)
	}
	if count := productionKnowledgeCount(t, again["chunkCount"]); count != 2 {
		t.Fatalf("the idempotent receipt counted %d chunks instead of 2", count)
	}
	if rows := productionKnowledgeRows(t, productionKnowledgeSnapshotOfProject(t, projectPath),
		productionKnowledgeTableDocuments); len(rows) != 1 {
		t.Fatalf("the idempotent call wrote a duplicate document: %d rows", len(rows))
	}
}

// TestProductionKnowledgeImportFinalizedTextRefusesChangedSource proves the
// authority gates: a body that disagrees with the frozen finalization - in its
// hash or in its prose - is refused, and neither the database nor the store is
// touched, so a stale renderer cannot overwrite a finalized chapter's knowledge.
func TestProductionKnowledgeImportFinalizedTextRefusesChangedSource(t *testing.T) {
	client, projectPath, frozen, baseline := productionKnowledgeLane(t)
	indexName := filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName)
	refusals := []struct {
		label string
		args  []any
	}{
		{
			// A hash the outbox row does not hold: copyHash(text) !== contentHash.
			label: "changed content hash",
			args: []any{productionKnowledgeProse, productionKnowledgeChapterFile, map[string]any{
				"draftId":        int64(1),
				"finalizationId": productionKnowledgeFinalizationID,
				"chapterNumber":  int64(1),
				"contentHash":    strings.Repeat("b", 64),
			}, frozen},
		},
		{
			// The right hash with different prose: the frozen snapshot no longer
			// equals the text the caller wants imported.
			label: "changed prose",
			args: []any{productionKnowledgeProse + "多写的一句。", productionKnowledgeChapterFile, map[string]any{
				"draftId":        int64(1),
				"finalizationId": productionKnowledgeFinalizationID,
				"chapterNumber":  int64(1),
				"contentHash":    productionKnowledgeHash(productionKnowledgeProse),
			}, frozen},
		},
		{
			// A finalization identity that is not the frozen one.
			label: "changed finalization identity",
			args: []any{productionKnowledgeProse, productionKnowledgeChapterFile, map[string]any{
				"draftId":        int64(1),
				"finalizationId": "ci-knowledge-finalization-other",
				"chapterNumber":  int64(1),
				"contentHash":    productionKnowledgeHash(productionKnowledgeProse),
			}, frozen},
		},
	}
	for _, refusal := range refusals {
		productionKnowledgeRefusal(t, client, refusal.label, productionKnowledgeImportFinalizedName, refusal.args,
			productionKnowledgeCodeSourceChanged)
		if _, err := os.Lstat(indexName); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused import wrote the knowledge store: %v", refusal.label, err)
		}
		after, err := os.ReadFile(filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(after, baseline) {
			t.Fatalf("%s: a refused import wrote the author database", refusal.label)
		}
		var linked string
		if err := productionKnowledgeReadback(t, projectPath).QueryRow(
			"SELECT knowledge_document_id FROM finalization_outbox WHERE finalization_id=?",
			productionKnowledgeFinalizationID).Scan(&linked); err != nil {
			t.Fatalf("%s: finalization_outbox readback: %v", refusal.label, err)
		}
		if linked != "" {
			t.Fatalf("%s: a refused import linked %v", refusal.label, linked)
		}
	}
}

// TestProductionKnowledgeImportTextRefusesEmptyText is the refusal the reference
// makes before anything else (:282): whitespace is not a knowledge document, and
// the refusal costs the store nothing.
func TestProductionKnowledgeImportTextRefusesEmptyText(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	indexName := filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName)
	productionKnowledgeRefusal(t, client, "whitespace only", productionKnowledgeImportTextName,
		[]any{" \n\t " + strings.Repeat("\n", 3), productionKnowledgeNamedFile, projectPath, frozen},
		productionKnowledgeCodeTextEmpty)
	if _, err := os.Lstat(indexName); !os.IsNotExist(err) {
		t.Fatalf("a refused import wrote the knowledge store: %v", err)
	}
	// The same project then accepts a real chapter, so the refusal above was the
	// text and not a lane that cannot write at all.
	answer := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-text refused the following valid import: %v", answer)
	}
	if len(productionKnowledgeRows(t, productionKnowledgeSnapshotOfProject(t, projectPath),
		productionKnowledgeTableDocuments)) != 1 {
		t.Fatal("the valid import did not leave exactly one document")
	}
}

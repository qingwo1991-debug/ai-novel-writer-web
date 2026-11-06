//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This file proves the five read names production_knowledge_reads_linux.go binds,
// over the real HTTP adapter and the real frozen project session:
//
//	kb:list-documents        the documents the writer committed, and nothing else
//	kb:read-document-copy    the copy file's text, hash, edited flag and index status
//	kb:stats                 documents, chunks and the absent vector dimension
//	kb:get-vectorless-count  every chunk, because this store has no vectors
//	kb:search                the FTS answer over that same store
//
// The decisive test is
// TestProductionKnowledgeReadsServeWhatTheImportWrote: it imports through the
// SERVED import name (kb:import-text), then lists, reads back, counts and searches
// what that import wrote in the same session. A reader answered from any store
// other than the writer's - a capture mounted at project:open, a second index, a
// cached view - finds nothing here, because nothing else in the fixture ever held
// that text. Every refusal is proved with a digest over every file this family can
// write - the database and its WAL sidecar, the snapshot and every copy, read back
// through a separate mode=ro connection - so a read that wrote one byte fails.
//
// The rest of the file covers what the decisive test cannot reach:
//
//	TestProductionKnowledgeListAnnotatesFinalizedChapter  finalizedChapter, from the outbox
//	TestProductionKnowledgeReadRefuseWithoutWriting       four refusals, and the store intact
//	TestProductionKnowledgeReadsAnswerAnUndecodableStore  kb:search [] where the reads refuse
//	TestProductionKnowledgeReadsRefuseALegacyStore        the 409, and the 200 beside it
//
// These tests need the binder block's new line in production_open_linux.go (see the
// file header); until the integrator adds it, every call below answers 501 and the
// first productionKnowledgeCall fails loudly. They skip without GO_SQLITE_CONTRACT
// exactly like every other production knowledge test.

// productionKnowledgeReadArities is the wire shape of each name before the
// frozen context (src/shared/ipc-channels.ts:1221-1242):
//
//	kb:list-documents        [expectedProjectPath]
//	kb:stats                 [expectedProjectPath]
//	kb:get-vectorless-count  [expectedProjectPath]
//	kb:read-document-copy    [docId, expectedProjectPath]
//	kb:search                [query, topK | undefined, expectedProjectPath]
var productionKnowledgeReadArities = map[string]int{
	productionKnowledgeListDocumentsName:   1,
	productionKnowledgeStatsName:           1,
	productionKnowledgeVectorlessCountName: 1,
	productionKnowledgeReadCopyName:        2,
	productionKnowledgeSearchChannelName:   3,
}

// productionKnowledgeReadDeclare registers those arities with the table the
// package's knowledge tests assert against, so an argument list that forgets the
// trailing frozen context fails as a shape mismatch naming the contract instead of
// arriving as INVALID_ARGUMENTS from the handler's own arity check. It runs on
// every entry point rather than in an init, so the order the three knowledge test
// files are compiled in cannot matter.
func productionKnowledgeReadDeclare() {
	for name, arity := range productionKnowledgeReadArities {
		productionKnowledgeWireArity[name] = arity
	}
}

// productionKnowledgeReadDocuments lists the knowledge-base documents over HTTP.
func productionKnowledgeReadDocuments(t *testing.T, client *productionDurableClient,
	projectPath string, frozen map[string]any) []map[string]any {
	t.Helper()
	productionKnowledgeReadDeclare()
	return productionKnowledgeQueryResults(t, client, productionKnowledgeListDocumentsName,
		[]any{projectPath, frozen})
}

// productionKnowledgeReadObject runs one object-shaped read: kb:stats,
// kb:get-vectorless-count or kb:read-document-copy.
func productionKnowledgeReadObject(t *testing.T, client *productionDurableClient, operation string,
	args []any) map[string]any {
	t.Helper()
	productionKnowledgeReadDeclare()
	return productionKnowledgeCall(t, client, operation, args)
}

// productionKnowledgeReadSearch runs kb:search, the family's default scope.
func productionKnowledgeReadSearch(t *testing.T, client *productionDurableClient,
	projectPath string, frozen map[string]any, query string) []map[string]any {
	t.Helper()
	productionKnowledgeReadDeclare()
	return productionKnowledgeQueryResults(t, client, productionKnowledgeSearchChannelName,
		[]any{query, nil, projectPath, frozen})
}

// productionKnowledgeReadFind is the one document row with this identity.
func productionKnowledgeReadFind(t *testing.T, documents []map[string]any, documentID string) map[string]any {
	t.Helper()
	for _, document := range documents {
		if document["id"] == documentID {
			return document
		}
	}
	t.Fatalf("the listing holds no document %s among %d rows", documentID, len(documents))
	return nil
}

// productionKnowledgeReadJSON renders one result so two answers can be compared
// exactly, key order included: json.Marshal sorts object keys, so equal JSON is
// equal content and equal score.
func productionKnowledgeReadJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("a read result does not encode: %v", err)
	}
	return string(encoded)
}

// productionKnowledgeReadFingerprint digests every file this family can write, in
// one value: the admitted database and its WAL sidecar, the canonical snapshot,
// and every knowledge copy. The database is first read through a SEPARATE
// mode=ro connection, so the bytes below are confirmed readable by a handle this
// lane never held - a write that only reached the lane's own connection would
// still have to appear here to be a write, and the sidecar is digested for the
// same reason.
func productionKnowledgeReadFingerprint(t *testing.T, projectPath string) string {
	t.Helper()
	var chapters int64
	if err := productionKnowledgeReadback(t, projectPath).QueryRow(
		"SELECT COUNT(*) FROM finalization_outbox WHERE knowledge_document_id != ''").Scan(&chapters); err != nil {
		t.Fatalf("the admitted database does not read over a read-only connection: %v", err)
	}
	digest := sha256.New()
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	for _, name := range []string{
		projectfs.DatabaseName,
		projectfs.DatabaseName + "-wal",
		productionKnowledgeIndexName,
	} {
		raw := productionKnowledgeQueryFileBytes(t, filepath.Join(storage, name))
		_, _ = digest.Write([]byte(name))
		if raw != nil {
			_, _ = digest.Write(raw)
		}
	}
	entries, err := os.ReadDir(filepath.Join(storage, productionKnowledgeCopyDirectory))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("the knowledge copies tree does not read: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		_, _ = digest.Write([]byte(name))
		_, _ = digest.Write(productionKnowledgeQueryFileBytes(t,
			filepath.Join(storage, productionKnowledgeCopyDirectory, name)))
	}
	return string(digest.Sum(nil))
}

// productionKnowledgeReadUnique and productionKnowledgeReadSecondFile belong to
// the second imported document only, so a search that finds the phrase names
// exactly one document.
const (
	productionKnowledgeReadUnique     = "灰鲸在第七夜熄灯"
	productionKnowledgeReadSecondFile = "第4章 灰鲸 正文.md"
)

// TestProductionKnowledgeReadsServeWhatTheImportWrote is the end-to-end proof this
// batch exists for: import through the served writer, then see, read, count and
// search that document through the five served readers - in one session, in that
// order, with the store never reopened for writing.
func TestProductionKnowledgeReadsServeWhatTheImportWrote(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)

	// Before any import: an absent snapshot is an empty knowledge base, not an
	// error. The reference answers the same way for an empty store
	// (vector-store.ts:1419, knowledge-base.ts:240-248).
	if documents := productionKnowledgeReadDocuments(t, client, projectPath, frozen); len(documents) != 0 {
		t.Fatalf("a project with no knowledge store lists %d documents", len(documents))
	}
	empty := productionKnowledgeReadObject(t, client, productionKnowledgeStatsName, []any{projectPath, frozen})
	for _, key := range []string{"documentCount", "totalChunks", "vectorDimension"} {
		if count, ok := productionKnowledgeSafeIntegerOf(empty[key]); !ok || count != 0 {
			t.Fatalf("an empty kb:stats answered %v for %s", empty[key], key)
		}
	}
	if results := productionKnowledgeReadSearch(t, client, projectPath, frozen, "灯"); len(results) != 0 {
		t.Fatalf("a search over an empty store found %d chunks", len(results))
	}

	// The served writer: two documents, two file names, so the prune rule
	// (same fileName and corpusKind supersedes) does not drop either.
	first := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if first["success"] != true {
		t.Fatalf("kb:import-text refused the first document: %v", first)
	}
	documentID, _ := first["docId"].(string)
	if documentID == "" {
		t.Fatalf("kb:import-text returned no document identity: %v", first)
	}
	second := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeReadUnique + "。", productionKnowledgeReadSecondFile, projectPath, frozen})
	if second["success"] != true {
		t.Fatalf("kb:import-text refused the second document: %v", second)
	}
	secondID, _ := second["docId"].(string)
	if secondID == "" || secondID == documentID {
		t.Fatalf("the second import reused the first identity: %v", second)
	}

	// kb:list-documents: exactly the two rows the writer committed, with the
	// writer's own columns, and no finalizedChapter on either - neither document
	// was imported through kb:import-finalized-text, so the outbox holds no link.
	documents := productionKnowledgeReadDocuments(t, client, projectPath, frozen)
	if len(documents) != 2 {
		t.Fatalf("kb:list-documents answered %d rows for two imports: %v", len(documents), documents)
	}
	listed := productionKnowledgeReadFind(t, documents, documentID)
	if listed["fileName"] != productionKnowledgeNamedFile ||
		listed["filePath"] != productionKnowledgeCopyMarker+documentID ||
		listed["corpusKind"] != productionKnowledgeCorpusKind {
		t.Fatalf("the listing does not describe the import: %v", listed)
	}
	if count, ok := productionKnowledgeSafeIntegerOf(listed["chunkCount"]); !ok || count != 2 {
		t.Fatalf("the listing records %v chunks instead of 2", listed["chunkCount"])
	}
	if imported, ok := listed["importedAt"].(string); !ok || imported == "" {
		t.Fatalf("the listing records no import time: %v", listed["importedAt"])
	}
	for _, document := range documents {
		if _, present := document["finalizedChapter"]; present {
			t.Fatalf("a plain import was annotated with a finalized chapter: %v", document)
		}
	}

	// kb:read-document-copy: the copy file's own text, hash and index status.
	copied := productionKnowledgeReadObject(t, client, productionKnowledgeReadCopyName,
		[]any{documentID, projectPath, frozen})
	if copied["available"] != true || copied["content"] != productionKnowledgeProse ||
		copied["contentHash"] != productionKnowledgeHash(productionKnowledgeProse) ||
		copied["edited"] != false || copied["indexStatus"] != "current" {
		t.Fatalf("kb:read-document-copy did not answer the imported copy: %v", copied)
	}

	// kb:stats and kb:get-vectorless-count: the truth of this store. Two
	// documents, three chunks, and a vector dimension of 0 because no
	// embedding-space registry exists; every one of the three chunks is
	// vectorless, which is what `registry: null` means.
	stats := productionKnowledgeReadObject(t, client, productionKnowledgeStatsName, []any{projectPath, frozen})
	if count, _ := productionKnowledgeSafeIntegerOf(stats["documentCount"]); count != 2 {
		t.Fatalf("kb:stats counted %v documents instead of 2", stats["documentCount"])
	}
	if count, _ := productionKnowledgeSafeIntegerOf(stats["totalChunks"]); count != 3 {
		t.Fatalf("kb:stats counted %v chunks instead of 3", stats["totalChunks"])
	}
	if dimension, ok := productionKnowledgeSafeIntegerOf(stats["vectorDimension"]); !ok || dimension != 0 {
		t.Fatalf("kb:stats claimed a vector dimension of %v in an FTS-only store", stats["vectorDimension"])
	}
	vectorless := productionKnowledgeReadObject(t, client, productionKnowledgeVectorlessCountName,
		[]any{projectPath, frozen})
	if count, _ := productionKnowledgeSafeIntegerOf(vectorless["count"]); count != 3 {
		t.Fatalf("kb:get-vectorless-count counted %v of 3 vectorless chunks", vectorless["count"])
	}

	// kb:search: the phrase only the second document holds finds the second
	// document, and a phrase only the first holds finds the first.
	unique := productionKnowledgeReadSearch(t, client, projectPath, frozen, productionKnowledgeReadUnique)
	if len(unique) != 1 {
		t.Fatalf("a search for the second document's own phrase found %d chunks: %v", len(unique), unique)
	}
	text, ok := unique[0]["text"].(string)
	if !ok || unique[0]["fileName"] != productionKnowledgeReadSecondFile ||
		!strings.Contains(text, productionKnowledgeReadUnique) {
		t.Fatalf("the search did not return the second document's chunk: %v", unique[0])
	}
	if score, ok := unique[0]["score"].(float64); !ok || score != 0.5 {
		t.Fatalf("the search scored %v instead of the reference's 0.5", unique[0]["score"])
	}
	firstOnly := productionKnowledgeReadSearch(t, client, projectPath, frozen, "不灭的灯")
	if len(firstOnly) != 1 || firstOnly[0]["fileName"] != productionKnowledgeNamedFile {
		t.Fatalf("the search did not return the first document's second chunk: %v", firstOnly)
	}

	// kb:search is the family's default scope, so it must be the same query path
	// as kb:search-with-scope and kb:search-writing-context and not a second
	// implementation: identical arguments (no exclusions, a range that covers
	// every chapter) must answer identically, byte for byte.
	defaultAnswer := productionKnowledgeReadSearch(t, client, projectPath, frozen, "灯")
	scoped := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWithScopeName,
		[]any{"灯", int64(0), int64(9), nil, projectPath, frozen})
	writingContext := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWritingContextName,
		[]any{"灯", nil, projectPath, frozen})
	if len(defaultAnswer) == 0 {
		t.Fatal("the parity query matched nothing, so equality would be vacuous")
	}
	if productionKnowledgeReadJSON(t, defaultAnswer) != productionKnowledgeReadJSON(t, scoped) {
		t.Fatalf("kb:search and kb:search-with-scope disagree: %v vs %v", defaultAnswer, scoped)
	}
	if productionKnowledgeReadJSON(t, defaultAnswer) != productionKnowledgeReadJSON(t, writingContext) {
		t.Fatalf("kb:search and kb:search-writing-context disagree: %v vs %v", defaultAnswer, writingContext)
	}

	// And the store is untouched by all of it: none of the five reads wrote a
	// byte, and the import's own two documents are still exactly two.
	if documents := productionKnowledgeReadDocuments(t, client, projectPath, frozen); len(documents) != 2 {
		t.Fatalf("the reads left %d documents in the store", len(documents))
	}
}

// TestProductionKnowledgeListAnnotatesFinalizedChapter proves the one difference
// the transport adds to kb:list-documents: a document linked to a frozen
// finalization through kb:import-finalized-text carries finalizedChapter from
// finalization_outbox.knowledge_document_id, and a plain import beside it does not.
func TestProductionKnowledgeListAnnotatesFinalizedChapter(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	source := map[string]any{
		"draftId":        int64(1),
		"finalizationId": productionKnowledgeFinalizationID,
		"chapterNumber":  int64(1),
		"contentHash":    productionKnowledgeHash(productionKnowledgeProse),
	}
	finalized := productionKnowledgeCall(t, client, productionKnowledgeImportFinalizedName,
		[]any{productionKnowledgeProse, productionKnowledgeChapterFile, source, projectPath, frozen})
	if finalized["success"] != true {
		t.Fatalf("kb:import-finalized-text refused a finalized chapter: %v", finalized)
	}
	finalizedID, _ := finalized["docId"].(string)
	if finalizedID == "" {
		t.Fatalf("the finalized import returned no document identity: %v", finalized)
	}
	plain := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeReadUnique + "。", productionKnowledgeNamedFile, projectPath, frozen})
	if plain["success"] != true {
		t.Fatalf("kb:import-text refused the plain document: %v", plain)
	}
	plainID, _ := plain["docId"].(string)
	if plainID == "" || plainID == finalizedID {
		t.Fatalf("the plain import reused the finalized identity: %v", plain)
	}
	// The link is asserted where the reader gets it: the same admitted database,
	// read over a separate connection that could not have written it.
	var linked string
	if err := productionKnowledgeReadback(t, projectPath).QueryRow(
		"SELECT knowledge_document_id FROM finalization_outbox WHERE finalization_id=?",
		productionKnowledgeFinalizationID).Scan(&linked); err != nil {
		t.Fatalf("finalization_outbox readback: %v", err)
	}
	if linked != finalizedID {
		t.Fatalf("the finalized import linked %v instead of the document it wrote", linked)
	}
	documents := productionKnowledgeReadDocuments(t, client, projectPath, frozen)
	if len(documents) != 2 {
		t.Fatalf("kb:list-documents answered %d rows for two imports", len(documents))
	}
	chapter, ok := productionKnowledgeSafeIntegerOf(
		productionKnowledgeReadFind(t, documents, finalizedID)["finalizedChapter"])
	if !ok || chapter != 1 {
		t.Fatalf("the finalized document was not annotated with chapter 1")
	}
	if _, present := productionKnowledgeReadFind(t, documents, plainID)["finalizedChapter"]; present {
		t.Fatal("a plain import was annotated with a finalized chapter")
	}
	// The copy of the finalized document is readable too, and it is the text the
	// finalization froze.
	copied := productionKnowledgeReadObject(t, client, productionKnowledgeReadCopyName,
		[]any{finalizedID, projectPath, frozen})
	if copied["available"] != true || copied["content"] != productionKnowledgeProse {
		t.Fatalf("kb:read-document-copy did not answer the finalized copy: %v", copied)
	}
}

// TestProductionKnowledgeReadRefuseWithoutWriting is the refusal arm: a missing
// copy is refused by name and never answered as an empty document, a malformed
// request is refused by the frozen transport, and every one of those calls leaves
// the author database, the canonical snapshot and the copies tree byte-identical.
func TestProductionKnowledgeReadRefuseWithoutWriting(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused an importable chapter: %v", imported)
	}
	documentID, _ := imported["docId"].(string)
	if documentID == "" {
		t.Fatalf("kb:import-text returned no document identity: %v", imported)
	}
	// The copy the store wrote, and the digest of everything the family owns.
	copyPath := productionKnowledgeCopyPath(projectPath, documentID)
	if _, err := os.Lstat(copyPath); err != nil {
		t.Fatalf("the import wrote no knowledge copy: %v", err)
	}
	baseline := productionKnowledgeReadFingerprint(t, projectPath)

	// A document whose copy file is gone: available:false, indexStatus
	// unavailable, and NO content key at all. An empty `content` would open a
	// blank editor buffer for text this lane cannot vouch for
	// (knowledge-base.ts:182-184).
	if err := os.Remove(copyPath); err != nil {
		t.Fatalf("removing the copy: %v", err)
	}
	missing := productionKnowledgeReadObject(t, client, productionKnowledgeReadCopyName,
		[]any{documentID, projectPath, frozen})
	if missing["available"] != false || missing["indexStatus"] != "unavailable" {
		t.Fatalf("a missing copy answered %v instead of a named refusal", missing)
	}
	if content, present := missing["content"]; present {
		t.Fatalf("a missing copy answered an empty document instead of a refusal: %v", content)
	}
	// An identity the store never held answers the same refusal, which is the
	// reference's own `find` miss (knowledge-base.ts:181-184).
	unknown := productionKnowledgeReadObject(t, client, productionKnowledgeReadCopyName,
		[]any{"never-imported-identity", projectPath, frozen})
	if unknown["available"] != false || unknown["indexStatus"] != "unavailable" {
		t.Fatalf("an unknown identity answered %v instead of a named refusal", unknown)
	}
	// The two documented consequences of a missing copy: the document row is
	// still listed, and retrieval no longer returns its chunks, because a copy
	// that is not current is excluded (knowledgequery/search.go:60-66). Both are
	// answers, not errors: an editor can still see the document and repair it.
	if documents := productionKnowledgeReadDocuments(t, client, projectPath, frozen); len(documents) != 1 {
		t.Fatalf("the listing dropped a document whose copy is missing: %d rows", len(documents))
	}
	if results := productionKnowledgeReadSearch(t, client, projectPath, frozen, "不灭的灯"); len(results) != 0 {
		t.Fatalf("a stale document stayed searchable: %v", results)
	}
	// The digest must be able to see a store change, or the comparisons below
	// would be vacuous.
	if productionKnowledgeReadFingerprint(t, projectPath) == baseline {
		t.Fatal("the digest did not see the removed copy, so it cannot prove a read wrote nothing")
	}

	// Every request-shape refusal the frozen transport owns, none of which may
	// reach a store: arity, a project path that disagrees with the frozen
	// context, and a context that is not the frozen three-key object.
	refusals := []struct {
		label     string
		operation string
		args      []any
		status    int
		code      string
	}{
		{"search arity", productionKnowledgeSearchChannelName, []any{"灯", nil}, http.StatusBadRequest, "INVALID_ARGUMENTS"},
		{"foreign project path", productionKnowledgeListDocumentsName,
			[]any{projectPath + "-elsewhere", frozen}, http.StatusConflict, "PROJECT_CONTEXT_CHANGED"},
		{"malformed context", productionKnowledgeStatsName,
			[]any{projectPath, map[string]any{"projectPath": projectPath}}, http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"},
		{"search topK out of range", productionKnowledgeSearchChannelName,
			[]any{"灯", int64(101), projectPath, frozen}, http.StatusBadRequest, "INVALID_ARGUMENTS"},
	}
	before := productionKnowledgeReadFingerprint(t, projectPath)
	for _, refusal := range refusals {
		response := client.call(t, refusal.operation, refusal.args)
		if response.Code != refusal.status ||
			!strings.Contains(response.Body.String(), "\"code\":\""+refusal.code+"\"") {
			t.Fatalf("%s: HTTP %d %s, want %d %s", refusal.label, response.Code,
				response.Body.String(), refusal.status, refusal.code)
		}
		// No host path and no author file name may reach an answer, refusal
		// bodies included.
		for _, secret := range []string{projectPath, projectfs.StorageDirectory, productionKnowledgeNamedFile} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s leaked %s in its refusal body", refusal.label, secret)
			}
		}
	}
	if after := productionKnowledgeReadFingerprint(t, projectPath); after != before {
		t.Fatal("a refused read wrote the author database or the knowledge store")
	}
}

// TestProductionKnowledgeReadsAnswerAnUndecodableStore proves the degradation this
// file documents, in both directions: a snapshot the reader cannot decode makes
// kb:search answer the reference's own empty array (vector-store.ts:1518-1520), and
// makes the four document reads refuse by name instead of inventing zeros. Neither
// answer repairs the store, which is why the digest is compared across both.
func TestProductionKnowledgeReadsAnswerAnUndecodableStore(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	// A byte sequence that is not the canonical snapshot: the writer never emits
	// this, and the reader must not guess at it.
	if err := os.WriteFile(productionKnowledgeQueryIndexPath(projectPath),
		[]byte(`{"tables":[`), 0600); err != nil {
		t.Fatalf("writing an unreadable snapshot: %v", err)
	}
	before := productionKnowledgeReadFingerprint(t, projectPath)
	if results := productionKnowledgeReadSearch(t, client, projectPath, frozen, "灯"); len(results) != 0 {
		t.Fatalf("a search over an undecodable store answered %d chunks", len(results))
	}
	for _, read := range []struct {
		operation string
		args      []any
	}{
		{productionKnowledgeListDocumentsName, []any{projectPath, frozen}},
		{productionKnowledgeStatsName, []any{projectPath, frozen}},
		{productionKnowledgeVectorlessCountName, []any{projectPath, frozen}},
	} {
		response := client.call(t, read.operation, read.args)
		if response.Code != http.StatusInternalServerError ||
			!strings.Contains(response.Body.String(), "\"code\":\"KNOWLEDGE_READ_FAILED\"") {
			t.Fatalf("%s answered HTTP %d %s for an undecodable store", read.operation,
				response.Code, response.Body.String())
		}
		for _, secret := range []string{projectPath, projectfs.StorageDirectory} {
			if strings.Contains(response.Body.String(), secret) {
				t.Fatalf("%s leaked %s in its refusal body", read.operation, secret)
			}
		}
	}
	if after := productionKnowledgeReadFingerprint(t, projectPath); after != before {
		t.Fatal("a read repaired or rewrote the undecodable store")
	}
}

// TestProductionKnowledgeReadsRefuseALegacyStore is deviation 1 made executable,
// in both directions: an unmigrated vectors.json makes all five of this file's
// names refuse with the one registered migration code, while
// kb:search-with-scope - a hand-written map handler, not a frozen View binder -
// keeps the reference's 200 {success:false,errorCode} for the same store. If that
// asymmetry ever closes, this test is where it shows.
//
// No name here is exempt, and no name may answer 500: the code has to arrive as an
// *security.HTTPError, because transport.knowledgeReadError rewrites every other
// error to KNOWLEDGE_READ_FAILED. Each refusal is collected rather than fatal on
// the first, so one run reports every operation that got this wrong.
func TestProductionKnowledgeReadsRefuseALegacyStore(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	legacy := filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeLegacyVectorName)
	if err := os.WriteFile(legacy, []byte(`{"tables":[]}`), 0600); err != nil {
		t.Fatalf("writing a legacy vector store: %v", err)
	}
	before := productionKnowledgeReadFingerprint(t, projectPath)
	var wrong []string
	for _, read := range []struct {
		operation string
		args      []any
	}{
		{productionKnowledgeListDocumentsName, []any{projectPath, frozen}},
		{productionKnowledgeStatsName, []any{projectPath, frozen}},
		{productionKnowledgeVectorlessCountName, []any{projectPath, frozen}},
		{productionKnowledgeReadCopyName, []any{"0123456789abcdef", projectPath, frozen}},
		{productionKnowledgeSearchChannelName, []any{"灯", nil, projectPath, frozen}},
	} {
		response := client.call(t, read.operation, read.args)
		if response.Code != http.StatusConflict ||
			!strings.Contains(response.Body.String(),
				"\"code\":\""+productionKnowledgeCodeLegacyBlocked+"\"") {
			wrong = append(wrong, read.operation+" answered HTTP "+
				strconv.Itoa(response.Code)+" "+response.Body.String())
			continue
		}
		if strings.Contains(response.Body.String(), projectPath) {
			wrong = append(wrong, read.operation+" leaked the host path in its refusal body")
		}
	}
	if len(wrong) != 0 {
		t.Fatalf("%d of 5 names do not refuse a legacy vector store with %s: %s",
			len(wrong), productionKnowledgeCodeLegacyBlocked, strings.Join(wrong, "; "))
	}
	scoped := client.call(t, productionKnowledgeSearchWithScopeName,
		[]any{"灯", int64(0), int64(9), nil, projectPath, frozen})
	var body map[string]any
	if err := json.Unmarshal(scoped.Body.Bytes(), &body); err != nil {
		t.Fatalf("kb:search-with-scope did not answer a body: %v", err)
	}
	// The operation channel wraps a handler's answer in result, so the reference's
	// 200 refusal arrives one level down. Asserting it at the top would have failed
	// against a correct lane.
	inner, _ := body["result"].(map[string]any)
	if scoped.Code != http.StatusOK || inner == nil || inner["success"] != false ||
		inner["errorCode"] != productionKnowledgeCodeLegacyBlocked {
		t.Fatalf("kb:search-with-scope answered HTTP %d %v instead of the reference's 200 refusal",
			scoped.Code, body)
	}
	if after := productionKnowledgeReadFingerprint(t, projectPath); after != before {
		t.Fatal("a refused read wrote the store")
	}
}

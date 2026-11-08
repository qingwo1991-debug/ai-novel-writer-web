//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This file proves the five document names
// production_knowledge_documents_linux.go binds, over the real HTTP adapter and
// the real frozen project session, and proves the three it refuses are still
// refused at GO_OPERATION_NOT_MIGRATED with the author database byte-identical:
//
//	kb:import-planning-text   writes the canonical store kb:import-text writes
//	kb:save-document-copy     the content-hash CAS, and the copy it leaves dirty
//	kb:reindex-document-copy  replaces the chunk rows from the copy's own text
//	kb:remove-document        drops the rows and the copy, and is idempotent
//	kb:clear-all              removes the snapshot, the registry and the copies
//	kb:import-document        refused: no knowledge file picker
//	kb:import-folder          refused: no knowledge folder picker
//	kb:import-reference-text  refused: no reference receipt writer
//
// Every assertion is a readback of what the call left behind - the canonical
// snapshot decoded through knowledgedomain, the reference-format copy file, and
// the search the store admits - never the receipt alone. The refusals are proved
// with the database and both knowledge files compared byte for byte before and
// after, because a refused mutation must not spend the author's store.
//
// These tests need the two binders installed in production_open_linux.go's binder
// block (see the file header for the exact lines); until the integrator adds them,
// every call below answers 501 and the first productionKnowledgeCall fails loudly.

func init() {
	// The wire shapes the browser declares (src/shared/ipc-channels.ts:1205-1238),
	// before the trailing frozen context. Registering them here means an argument
	// list that forgets the context fails as a shape mismatch naming the contract
	// instead of arriving as INVALID_ARGUMENTS from the handler's own arity check.
	for name, arity := range map[string]int{
		productionKnowledgePlanningTextName:         3,
		productionKnowledgeSaveCopyName:             4,
		productionKnowledgeReindexCopyName:          2,
		productionKnowledgeRemoveDocumentName:       2,
		productionKnowledgeClearAllName:             1,
		productionKnowledgeSearchWritingContextName: 3,
		productionKnowledgeSearchWithScopeName:      5,
		productionKnowledgeVectorStatusName:         1,
	} {
		productionKnowledgeWireArity[name] = arity
	}
}

// productionKnowledgeQueryIndexPath is the canonical snapshot's path.
func productionKnowledgeQueryIndexPath(projectPath string) string {
	return filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName)
}

// productionKnowledgeQueryFileBytes reads one knowledge file, answering nil when
// it does not exist, so a refusal can be compared exactly.
func productionKnowledgeQueryFileBytes(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return raw
}

// productionKnowledgeQueryCopy is the reference-format copy record.
type productionKnowledgeQueryCopy struct {
	Content     string `json:"content"`
	IndexedHash string `json:"indexedHash"`
	Edited      bool   `json:"edited"`
	IndexDirty  bool   `json:"indexDirty"`
}

// productionKnowledgeQueryReadCopy decodes the copy file of one document.
func productionKnowledgeQueryReadCopy(t *testing.T, projectPath, documentID string) productionKnowledgeQueryCopy {
	t.Helper()
	raw := productionKnowledgeQueryFileBytes(t, productionKnowledgeCopyPath(projectPath, documentID))
	if raw == nil {
		t.Fatalf("document %s has no knowledge copy", documentID)
	}
	var copied productionKnowledgeQueryCopy
	if err := json.Unmarshal(raw, &copied); err != nil {
		t.Fatalf("the knowledge copy of %s does not decode: %v", documentID, err)
	}
	return copied
}

// productionKnowledgeQueryChunkIDs lists one document's chunk identities in
// chunkIndex order, so a reindex that reused them fails.
func productionKnowledgeQueryChunkIDs(t *testing.T, projectPath, documentID string) []string {
	t.Helper()
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	rows := productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks)
	ordered := make([]string, 0, len(rows))
	for _, row := range rows {
		if row["docId"] != documentID {
			continue
		}
		position, ok := productionKnowledgeSafeIntegerOf(row["chunkIndex"])
		if !ok || position < 0 || position >= int64(len(rows)) {
			t.Fatalf("chunk row carries no usable chunkIndex: %v", row["chunkIndex"])
		}
		identity, ok := row["id"].(string)
		if !ok || identity == "" {
			t.Fatal("chunk row carries no identity")
		}
		for int64(len(ordered)) <= position {
			ordered = append(ordered, "")
		}
		ordered[position] = identity
	}
	return ordered
}

// TestProductionKnowledgePlanningTextWritesCanonicalStore is the planning arm:
// the reference's only difference from kb:import-text is `{mode:'fts-only'}`
// (kb-controller.ts:323-336), which is the only mode this lane has, so the call
// must leave exactly the store kb:import-text leaves.
func TestProductionKnowledgePlanningTextWritesCanonicalStore(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	answer := productionKnowledgeCall(t, client, productionKnowledgePlanningTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-planning-text refused an importable text: %v", answer)
	}
	if _, present := answer["errorCode"]; present {
		t.Fatalf("a successful planning import carried an error code: %v", answer)
	}
	documentID, _ := answer["docId"].(string)
	if documentID == "" {
		t.Fatalf("kb:import-planning-text returned no document identity receipt: %v", answer)
	}
	if count := productionKnowledgeCount(t, answer["chunkCount"]); count != 2 {
		t.Fatalf("kb:import-planning-text chunked the text into %d chunks instead of 2", count)
	}
	if _, present := answer["idempotent"]; present {
		t.Fatal("kb:import-planning-text claimed the finalized import's idempotency field")
	}
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	documents := productionKnowledgeRows(t, snapshot, productionKnowledgeTableDocuments)
	if len(documents) != 1 {
		t.Fatalf("the planning import left %d document rows instead of 1", len(documents))
	}
	document := documents[0]
	if document["id"] != documentID || document["fileName"] != productionKnowledgeNamedFile ||
		document["corpusKind"] != productionKnowledgeCorpusKind ||
		document["filePath"] != productionKnowledgeCopyMarker+documentID {
		t.Fatalf("the planning document row does not describe the import: %v", document)
	}
	chunks := productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks)
	want := productionKnowledgeProseChunks(t)
	for position, text := range productionKnowledgeTexts(t, chunks) {
		if text != want[position] {
			t.Fatalf("planning chunk %d does not match chunkText's boundary", position)
		}
	}
	for _, chunk := range chunks {
		if chunk["corpusKind"] != productionKnowledgeCorpusKind {
			t.Fatalf("the planning chunk row records corpus kind %v", chunk["corpusKind"])
		}
		if number, _ := productionKnowledgeSafeIntegerOf(chunk["chapterNumber"]); number != productionKnowledgeNamedChapter {
			t.Fatalf("the planning chunk row records chapter %v instead of %d",
				chunk["chapterNumber"], productionKnowledgeNamedChapter)
		}
	}
	copied := productionKnowledgeQueryReadCopy(t, projectPath, documentID)
	if copied.Content != productionKnowledgeProse || copied.Edited || copied.IndexDirty ||
		copied.IndexedHash != productionKnowledgeHash(productionKnowledgeProse) {
		t.Fatal("the planning knowledge copy does not hold the imported text and its hash")
	}
}

// TestProductionKnowledgeSaveThenReindexCopyReplacesChunks is the repair round
// trip: a saved copy is deliberately stale - the reference marks it edited and
// dirty and the store's search skips a document whose copy is not current - and a
// reindex is what makes the copy's text canonical again, with fresh chunk
// identities and the inherited document meta.
func TestProductionKnowledgeSaveThenReindexCopyReplacesChunks(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProseOne, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the import the copy edits: %v", imported)
	}
	documentID, _ := imported["docId"].(string)
	if documentID == "" {
		t.Fatalf("kb:import-text returned no document identity: %v", imported)
	}
	if count := productionKnowledgeCount(t, imported["chunkCount"]); count != 1 {
		t.Fatalf("the one-paragraph import counted %d chunks instead of 1", count)
	}
	before := productionKnowledgeSnapshotOfProject(t, projectPath)
	originalChunks := productionKnowledgeQueryChunkIDs(t, projectPath, documentID)
	if len(originalChunks) != 1 {
		t.Fatalf("the import left %d chunk identities instead of 1", len(originalChunks))
	}
	var importedAt string
	for _, document := range productionKnowledgeRows(t, before, productionKnowledgeTableDocuments) {
		if document["id"] == documentID {
			importedAt, _ = document["importedAt"].(string)
		}
	}
	if importedAt == "" {
		t.Fatal("the imported document row carries no timestamp")
	}

	// Save the two-paragraph text over the copy. expectedContentHash is the hash
	// of the content the copy holds now, which is the reference's CAS.
	saved := productionKnowledgeCall(t, client, productionKnowledgeSaveCopyName,
		[]any{documentID, productionKnowledgeProse, productionKnowledgeHash(productionKnowledgeProseOne),
			projectPath, frozen})
	if saved["success"] != true {
		t.Fatalf("kb:save-document-copy refused a matching content hash: %v", saved)
	}
	copied := productionKnowledgeQueryReadCopy(t, projectPath, documentID)
	if copied.Content != productionKnowledgeProse {
		t.Fatal("the saved copy does not hold the text it was given")
	}
	if !copied.Edited || !copied.IndexDirty {
		t.Fatalf("the saved copy is not marked edited and dirty: edited=%v indexDirty=%v",
			copied.Edited, copied.IndexDirty)
	}
	if copied.IndexedHash != productionKnowledgeHash(productionKnowledgeProseOne) {
		t.Fatal("saving a copy changed the hash of the text the index still holds")
	}
	// The index is untouched, and a dirty copy is invisible to the store's search
	// until a reindex, which is why the second paragraph is not found yet.
	afterSave := productionKnowledgeSnapshotOfProject(t, projectPath)
	if chunks := productionKnowledgeRows(t, afterSave, productionKnowledgeTableChunks); len(chunks) != 1 {
		t.Fatalf("saving a copy changed the chunk rows: %d rows", len(chunks))
	}
	if results := productionKnowledgeSearch(t, projectPath, "不灭的灯"); len(results) != 0 {
		t.Fatalf("a document with a dirty copy was searchable: %d results", len(results))
	}

	// Reindex: the copy's text becomes the canonical chunk set.
	reindexed := productionKnowledgeCall(t, client, productionKnowledgeReindexCopyName,
		[]any{documentID, projectPath, frozen})
	if reindexed["success"] != true || reindexed["docId"] != documentID {
		t.Fatalf("kb:reindex-document-copy refused the dirty copy: %v", reindexed)
	}
	if count := productionKnowledgeCount(t, reindexed["chunkCount"]); count != 2 {
		t.Fatalf("the reindex receipt counted %d chunks instead of 2", count)
	}
	after := productionKnowledgeSnapshotOfProject(t, projectPath)
	documents := productionKnowledgeRows(t, after, productionKnowledgeTableDocuments)
	if len(documents) != 1 || documents[0]["id"] != documentID {
		t.Fatalf("the reindex changed the document rows: %d rows", len(documents))
	}
	if count, _ := productionKnowledgeSafeIntegerOf(documents[0]["chunkCount"]); count != 2 {
		t.Fatalf("the reindexed document records %v chunks instead of 2", documents[0]["chunkCount"])
	}
	if documents[0]["fileName"] != productionKnowledgeNamedFile ||
		documents[0]["importedAt"] != importedAt ||
		documents[0]["corpusKind"] != productionKnowledgeCorpusKind {
		t.Fatalf("the reindex changed the document identity columns: %v", documents[0])
	}
	want := productionKnowledgeProseChunks(t)
	chunks := productionKnowledgeRows(t, after, productionKnowledgeTableChunks)
	for position, text := range productionKnowledgeTexts(t, chunks) {
		if text != want[position] {
			t.Fatalf("reindexed chunk %d does not hold the copy's own text", position)
		}
	}
	// replaceDocumentChunks mints a fresh identity per chunk and inherits the rest
	// of the row set from the rows it replaces (vector-store.ts:1226-1264).
	current := productionKnowledgeQueryChunkIDs(t, projectPath, documentID)
	if len(current) != 2 {
		t.Fatalf("the reindex left %d chunk identities instead of 2", len(current))
	}
	for _, identity := range current {
		if identity == originalChunks[0] {
			t.Fatal("the reindex reused the replaced chunk's identity")
		}
	}
	for _, chunk := range chunks {
		if chunk["docId"] != documentID || chunk["fileName"] != productionKnowledgeNamedFile ||
			chunk["importedAt"] != importedAt {
			t.Fatalf("a reindexed chunk row does not inherit the document meta: %v", chunk)
		}
		if number, _ := productionKnowledgeSafeIntegerOf(chunk["chapterNumber"]); number != productionKnowledgeNamedChapter {
			t.Fatalf("a reindexed chunk row records chapter %v instead of %d",
				chunk["chapterNumber"], productionKnowledgeNamedChapter)
		}
		if chunk["chapterTitle"] != productionKnowledgeNamedTitle {
			t.Fatalf("a reindexed chunk row records chapter title %v", chunk["chapterTitle"])
		}
	}
	// The copy is current again, and the text it now describes is searchable.
	repaired := productionKnowledgeQueryReadCopy(t, projectPath, documentID)
	if repaired.IndexDirty {
		t.Fatal("the reindex did not clear the copy's dirty flag")
	}
	if !repaired.Edited {
		t.Fatal("the reindex forgot that the copy was edited")
	}
	if repaired.IndexedHash != productionKnowledgeHash(productionKnowledgeProse) {
		t.Fatal("the reindex did not record the hash of the text it indexed")
	}
	if results := productionKnowledgeSearch(t, projectPath, "不灭的灯"); len(results) != 1 {
		t.Fatalf("the reindexed text is not searchable: %d results", len(results))
	}
}

// TestProductionKnowledgeSaveCopyRefusalsLeaveStoreUnchanged proves the three
// save refusals: a request whose arguments are not strings, an identity the store
// does not hold, and a stale content hash. All three answer business failures at
// HTTP 200 and none of them writes anything.
func TestProductionKnowledgeSaveCopyRefusalsLeaveStoreUnchanged(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProseOne, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the import the refusals need: %v", imported)
	}
	documentID, _ := imported["docId"].(string)
	if documentID == "" {
		t.Fatalf("kb:import-text returned no document identity: %v", imported)
	}
	indexPath := productionKnowledgeQueryIndexPath(projectPath)
	copyPath := productionKnowledgeCopyPath(projectPath, documentID)
	indexBefore := productionKnowledgeQueryFileBytes(t, indexPath)
	copyBefore := productionKnowledgeQueryFileBytes(t, copyPath)
	if indexBefore == nil || copyBefore == nil {
		t.Fatal("the import did not write both knowledge files")
	}
	refusals := []struct {
		label string
		args  []any
		code  string
	}{
		{
			// The reference answers before opening any store
			// (knowledge-base.ts:193).
			label: "non-string content hash",
			args:  []any{documentID, productionKnowledgeProse, int64(7), projectPath, frozen},
			code:  productionKnowledgeCodeCopyRequestInvalid,
		},
		{
			// A managed identity the store does not hold.
			label: "unknown document",
			args: []any{"knowledge-copy-unknown", productionKnowledgeProse,
				productionKnowledgeHash(productionKnowledgeProseOne), projectPath, frozen},
			code: productionKnowledgeCodeCopyUnavailable,
		},
		{
			// The CAS: the hash names content the copy does not hold.
			label: "stale content hash",
			args: []any{documentID, productionKnowledgeProse, strings.Repeat("c", 64),
				projectPath, frozen},
			code: productionKnowledgeCodeCopyChanged,
		},
	}
	for _, refusal := range refusals {
		productionKnowledgeRefusal(t, client, refusal.label, productionKnowledgeSaveCopyName, refusal.args,
			refusal.code)
		if after := productionKnowledgeQueryFileBytes(t, indexPath); !bytes.Equal(after, indexBefore) {
			t.Fatalf("%s: a refused save changed the canonical snapshot", refusal.label)
		}
		if after := productionKnowledgeQueryFileBytes(t, copyPath); !bytes.Equal(after, copyBefore) {
			t.Fatalf("%s: a refused save changed the knowledge copy", refusal.label)
		}
	}
}

// TestProductionKnowledgeRemoveDocumentDropsRowsAndCopy proves the removal drops
// the document, its chunks and its copy, and that removing an identity the store
// no longer holds answers success while writing nothing at all - the reference's
// two "no rows remain" checks (vector-store.ts:1265-1316).
func TestProductionKnowledgeRemoveDocumentDropsRowsAndCopy(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the import the removal needs: %v", imported)
	}
	documentID, _ := imported["docId"].(string)
	if documentID == "" {
		t.Fatalf("kb:import-text returned no document identity: %v", imported)
	}
	removed := productionKnowledgeCall(t, client, productionKnowledgeRemoveDocumentName,
		[]any{documentID, projectPath, frozen})
	if removed["success"] != true {
		t.Fatalf("kb:remove-document refused an imported document: %v", removed)
	}
	if _, present := removed["errorCode"]; present {
		t.Fatalf("a successful removal carried an error code: %v", removed)
	}
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	if documents := productionKnowledgeRows(t, snapshot, productionKnowledgeTableDocuments); len(documents) != 0 {
		t.Fatalf("the removal left %d document rows", len(documents))
	}
	if chunks := productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks); len(chunks) != 0 {
		t.Fatalf("the removal left %d chunk rows", len(chunks))
	}
	if _, err := os.Lstat(productionKnowledgeCopyPath(projectPath, documentID)); !os.IsNotExist(err) {
		t.Fatalf("the removal left the knowledge copy behind: %v", err)
	}
	// The store the removal left behind must still be a store this lane can read:
	// an empty index is valid, and a search over it answers nothing.
	if results := productionKnowledgeSearch(t, projectPath, "不灭的灯"); len(results) != 0 {
		t.Fatalf("a removed document is still searchable: %d results", len(results))
	}
	indexPath := productionKnowledgeQueryIndexPath(projectPath)
	before := productionKnowledgeQueryFileBytes(t, indexPath)
	again := productionKnowledgeCall(t, client, productionKnowledgeRemoveDocumentName,
		[]any{documentID, projectPath, frozen})
	if again["success"] != true {
		t.Fatalf("kb:remove-document refused an identity it had already removed: %v", again)
	}
	if after := productionKnowledgeQueryFileBytes(t, indexPath); !bytes.Equal(after, before) {
		t.Fatal("removing an absent identity rewrote the canonical snapshot")
	}
}

// TestProductionKnowledgeClearAllRemovesStoreAndCopies proves clear-all removes
// the snapshot and the whole copies tree, and that a second clear is a no-op
// rather than a failure.
func TestProductionKnowledgeClearAllRemovesStoreAndCopies(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	// Two documents under two different names, so the copies directory holds two
	// copies and a partial clear fails here.
	for _, imported := range []struct {
		text string
		file string
	}{
		{productionKnowledgeProseOne, productionKnowledgeNamedFile},
		{productionKnowledgeProseTwo, "第4章 影 正文.md"},
	} {
		answer := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
			[]any{imported.text, imported.file, projectPath, frozen})
		if answer["success"] != true {
			t.Fatalf("kb:import-text refused an import the clear needs: %v", answer)
		}
	}
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if _, err := os.Lstat(filepath.Join(storage, productionKnowledgeCopyDirectory)); err != nil {
		t.Fatalf("the imports wrote no copies directory: %v", err)
	}
	cleared := productionKnowledgeCall(t, client, productionKnowledgeClearAllName,
		[]any{projectPath, frozen})
	if cleared["success"] != true {
		t.Fatalf("kb:clear-all refused a store it could clear: %v", cleared)
	}
	if _, err := os.Lstat(productionKnowledgeQueryIndexPath(projectPath)); !os.IsNotExist(err) {
		t.Fatalf("kb:clear-all left the canonical snapshot behind: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(storage, productionKnowledgeCopyDirectory)); !os.IsNotExist(err) {
		t.Fatalf("kb:clear-all left the copies directory behind: %v", err)
	}
	again := productionKnowledgeCall(t, client, productionKnowledgeClearAllName,
		[]any{projectPath, frozen})
	if again["success"] != true {
		t.Fatalf("kb:clear-all refused an already empty store: %v", again)
	}
}

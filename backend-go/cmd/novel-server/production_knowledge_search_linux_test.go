//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// This file proves the four retrieval names
// production_knowledge_search_linux.go binds, over the real HTTP adapter and the
// real frozen project session:
//
//	kb:search-writing-context     FTS over the canonical store, reference corpus excluded
//	kb:search-with-scope          the same retrieval restricted to a chapter range
//	kb:get-vector-rebuild-status  a truthful "this store has no vector index" answer
//	kb:backfill-vectors           the honest zero-vector no-op, proved in
//	                              production_kb_backfill_linux_test.go
//
// The exclusion arm is the one that separates this port from a naive one: the
// store is seeded with a reference-corpus document that holds the same query term
// as the project document, so a handler that ignores `excludedCorpusKinds` returns
// two results where the reference returns one, and a handler that ignores the
// chapter scope returns the reference chunk for a range that cannot contain it.
//
// These tests need the two binders installed in production_open_linux.go's binder
// block; until the integrator adds them, every call answers 501 and the first
// productionKnowledgeCall fails loudly.

var (
	// productionKnowledgeQueryProjectText is one chunk that holds the query term
	// the reference document below also holds, so only the corpus exclusion and
	// the chapter scope can tell the two documents apart.
	productionKnowledgeQueryProjectText = strings.Repeat("他把青霜剑放在门槛上。", 20)
	// productionKnowledgeQueryReferenceText is the reference corpus' own chunk.
	productionKnowledgeQueryReferenceText = strings.Repeat("参考稿里也写了青霜剑。", 20)
	// productionKnowledgeQueryReferenceFile and ...Title are the reference
	// document's meta. Its chapter is 0 on purpose: kb:search-with-scope [0,0]
	// must return it and the imported chapter 3 must not appear.
	productionKnowledgeQueryReferenceFile  = "参考样章 第9章.txt"
	productionKnowledgeQueryReferenceTitle = "参考"
	productionKnowledgeQueryTerm           = "青霜剑"
)

// productionKnowledgeQueryResults performs one retrieval and returns the result
// array. The reference's search channels answer a bare array (AppResult,
// src/shared/ipc-channels.ts:1222-1223), so this reads `result` as one.
func productionKnowledgeQueryResults(t *testing.T, client *productionDurableClient, operation string,
	args []any) []map[string]any {
	t.Helper()
	if arity, declared := productionKnowledgeWireArity[operation]; declared && len(args) != arity+1 {
		t.Fatalf("%s: %d arguments sent, the wire shape is %d plus the frozen context",
			operation, len(args), arity)
	}
	response := client.call(t, operation, args)
	if response.Code == http.StatusNotImplemented || strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("%s is still GO_OPERATION_NOT_MIGRATED in the production lane: %s", operation, response.Body.String())
	}
	if response.Code != http.StatusOK {
		t.Fatalf("%s: HTTP %d %s", operation, response.Code, response.Body.String())
	}
	var wire struct {
		Result []map[string]any `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &wire); err != nil {
		t.Fatalf("%s: %v %s", operation, err, response.Body.String())
	}
	return wire.Result
}

// productionKnowledgeQueryFileNames is the ordered file names of one result set,
// so a refusal and an exclusion are asserted on identity rather than on count.
func productionKnowledgeQueryFileNames(t *testing.T, results []map[string]any) []string {
	t.Helper()
	names := make([]string, len(results))
	for position, result := range results {
		name, ok := result["fileName"].(string)
		if !ok || name == "" {
			t.Fatalf("result %d carries no file name: %v", position, result)
		}
		names[position] = name
	}
	return names
}

// productionKnowledgeQuerySeedReference appends a reference-corpus document and
// its copy to the canonical store an import just wrote, through the same typed
// index the served mutations use. It is the only fixture here that is not written
// by a served name: no served name in this lane can create a reference document,
// which is exactly why kb:import-reference-text is refused.
func productionKnowledgeQuerySeedReference(t *testing.T, projectPath string) string {
	t.Helper()
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		t.Fatalf("open the project storage: %v", err)
	}
	defer func() { _ = storage.Close() }()
	index, err := productionKnowledgeReadIndex(context.Background(), storage.storage)
	if err != nil {
		t.Fatalf("read the canonical index: %v", err)
	}
	if len(index.Documents) == 0 {
		t.Fatal("the project document must be imported before a reference document is seeded")
	}
	importedAt := index.Documents[0].ImportedAt
	chapter := int64(0)
	title := productionKnowledgeQueryReferenceTitle
	documentID := "reference-corpus-document-1"
	index.Documents = append(index.Documents, productionKnowledgeDocument{
		ID:         documentID,
		FileName:   productionKnowledgeQueryReferenceFile,
		ImportedAt: importedAt,
		ChunkCount: 1,
		FilePath:   productionKnowledgeCopyMarker + documentID,
		CorpusKind: productionKnowledgeReferenceCorpusKind,
	})
	index.Chunks = append(index.Chunks, productionKnowledgeChunk{
		ID:            "reference-corpus-chunk-1",
		DocID:         documentID,
		FileName:      productionKnowledgeQueryReferenceFile,
		Text:          productionKnowledgeQueryReferenceText,
		ChunkIndex:    0,
		TotalChunks:   1,
		ImportedAt:    importedAt,
		ChapterNumber: &chapter,
		ChapterTitle:  &title,
		CorpusKind:    productionKnowledgeReferenceCorpusKind,
	})
	if err := productionKnowledgeWriteIndex(context.Background(), storage.storage, index); err != nil {
		t.Fatalf("seed the reference document: %v", err)
	}
	if _, err := productionKnowledgeWriteCopy(storage.storage, documentID, productionKnowledgeQueryReferenceText,
		productionKnowledgeHash(productionKnowledgeQueryReferenceText)); err != nil {
		t.Fatalf("seed the reference copy: %v", err)
	}
	return documentID
}

// TestProductionKnowledgeSearchWritingContextFindsImportedChunk is the plain
// arm: the name a draft generation and the novel import both call returns the
// imported chunk, and an explicit topK of 0 answers nothing - which is the
// reference's `topK ?? 5`, where 0 is a real request for zero rows.
func TestProductionKnowledgeSearchWritingContextFindsImportedChunk(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeQueryProjectText, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the text the search needs: %v", imported)
	}
	results := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWritingContextName,
		[]any{productionKnowledgeQueryTerm, nil, projectPath, frozen})
	if len(results) != 1 {
		t.Fatalf("kb:search-writing-context returned %d results instead of 1", len(results))
	}
	result := results[0]
	if result["fileName"] != productionKnowledgeNamedFile {
		t.Fatalf("the writing context named %v instead of the imported chapter", result["fileName"])
	}
	if text, _ := result["text"].(string); !strings.Contains(text, productionKnowledgeQueryTerm) {
		t.Fatal("the writing context returned a chunk that does not hold the query term")
	}
	// vector-store.ts:1497 scores every FTS row 0.5.
	if score, _ := result["score"].(float64); score != 0.5 {
		t.Fatalf("the writing context scored the row %v instead of 0.5", result["score"])
	}
	if empty := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWritingContextName,
		[]any{productionKnowledgeQueryTerm, int64(0), projectPath, frozen}); len(empty) != 0 {
		t.Fatalf("topK 0 returned %d results instead of none", len(empty))
	}
}

// TestProductionKnowledgeSearchSeparatesReferenceCorpusAndScope is the arm a
// naive handler fails: the store holds a project chunk (chapter 3) and a
// reference chunk (chapter 0) that both match the query, so the two served names
// must answer differently.
func TestProductionKnowledgeSearchSeparatesReferenceCorpusAndScope(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeQueryProjectText, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the text the search needs: %v", imported)
	}
	productionKnowledgeQuerySeedReference(t, projectPath)
	// The plain lane proves the fixture is really searchable, so the exclusions
	// below are exclusions and not a query that never matched.
	seeded := productionKnowledgeSearch(t, projectPath, productionKnowledgeQueryTerm)
	if len(seeded) != 2 {
		t.Fatalf("the seeded store holds %d matching chunks instead of 2", len(seeded))
	}
	// kb:search-writing-context excludes the reference corpus
	// (kb-controller.ts:404, :407), so the reference chunk must not appear.
	writingContext := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWritingContextName,
		[]any{productionKnowledgeQueryTerm, nil, projectPath, frozen})
	names := productionKnowledgeQueryFileNames(t, writingContext)
	if len(names) != 1 || names[0] != productionKnowledgeNamedFile {
		t.Fatalf("the writing context returned %v instead of the project chapter alone", names)
	}
	// kb:search-with-scope excludes nothing and filters on the chapter range: the
	// reference document is chapter 0, so [0,0] returns it and the chapter 3
	// document cannot appear.
	referenceScope := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWithScopeName,
		[]any{productionKnowledgeQueryTerm, int64(0), int64(0), nil, projectPath, frozen})
	if names := productionKnowledgeQueryFileNames(t, referenceScope); len(names) != 1 ||
		names[0] != productionKnowledgeQueryReferenceFile {
		t.Fatalf("a scope of chapter 0 returned %v instead of the reference chunk alone", names)
	}
	projectScope := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWithScopeName,
		[]any{productionKnowledgeQueryTerm, int64(3), int64(3), nil, projectPath, frozen})
	if names := productionKnowledgeQueryFileNames(t, projectScope); len(names) != 1 ||
		names[0] != productionKnowledgeNamedFile {
		t.Fatalf("a scope of chapter 3 returned %v instead of the imported chapter alone", names)
	}
	// A range that holds neither chapter answers nothing rather than everything.
	outside := productionKnowledgeQueryResults(t, client, productionKnowledgeSearchWithScopeName,
		[]any{productionKnowledgeQueryTerm, int64(7), int64(9), nil, projectPath, frozen})
	if len(outside) != 0 {
		t.Fatalf("a scope outside every chapter returned %d results", len(outside))
	}
}

// TestProductionKnowledgeSearchRefusesArgsOutOfContract proves the three shapes
// this lane refuses before reading anything - an inverted range, a negative
// chapter, and a topK outside the reader's 0..100 bound - and that not one of them
// touches the store.
func TestProductionKnowledgeSearchRefusesArgsOutOfContract(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeQueryProjectText, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the text the search needs: %v", imported)
	}
	indexPath := productionKnowledgeQueryIndexPath(projectPath)
	before := productionKnowledgeQueryFileBytes(t, indexPath)
	if before == nil {
		t.Fatal("the import wrote no canonical snapshot")
	}
	refusals := []struct {
		label string
		name  string
		args  []any
	}{
		{
			label: "inverted chapter range",
			name:  productionKnowledgeSearchWithScopeName,
			args:  []any{productionKnowledgeQueryTerm, int64(4), int64(2), nil, projectPath, frozen},
		},
		{
			label: "negative chapter",
			name:  productionKnowledgeSearchWithScopeName,
			args:  []any{productionKnowledgeQueryTerm, int64(-1), int64(2), nil, projectPath, frozen},
		},
		{
			label: "topK above the reader's bound",
			name:  productionKnowledgeSearchWritingContextName,
			args:  []any{productionKnowledgeQueryTerm, int64(productionKnowledgeSearchMaxTopK + 1), projectPath, frozen},
		},
		{
			label: "topK that is not an integer",
			name:  productionKnowledgeSearchWritingContextName,
			args:  []any{productionKnowledgeQueryTerm, "5", projectPath, frozen},
		},
		{
			label: "chapter that is not an integer",
			name:  productionKnowledgeSearchWithScopeName,
			args:  []any{productionKnowledgeQueryTerm, "一", int64(2), nil, projectPath, frozen},
		},
	}
	for _, refused := range refusals {
		response := client.call(t, refused.name, refused.args)
		if response.Code != http.StatusBadRequest ||
			!strings.Contains(response.Body.String(), "INVALID_ARGUMENTS") {
			t.Fatalf("%s: %s answered HTTP %d %s instead of INVALID_ARGUMENTS",
				refused.label, refused.name, response.Code, response.Body.String())
		}
		if after := productionKnowledgeQueryFileBytes(t, indexPath); !bytes.Equal(after, before) {
			t.Fatalf("%s: a refused retrieval wrote the canonical snapshot", refused.label)
		}
	}
}

// TestProductionKnowledgeVectorStatusReportsFTSOnly proves the status answer is
// about this store rather than a fixed refusal: an empty project answers zeros, an
// imported store answers its real chunk count, and every chunk it holds is
// vectorless because no vector index exists. kb:backfill-vectors answers the
// honest zero-vector no-op beside it.
func TestProductionKnowledgeVectorStatusReportsFTSOnly(t *testing.T) {
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	assertStatus := func(label string, answer map[string]any, chunks int64) {
		t.Helper()
		if answer["embeddingConfigured"] != false || answer["canRebuild"] != false {
			t.Fatalf("%s: the status offered a rebuild this lane cannot run: %v", label, answer)
		}
		if count := productionKnowledgeCount(t, answer["totalChunks"]); count != chunks {
			t.Fatalf("%s: the status counted %d chunks instead of %d", label, count, chunks)
		}
		if count := productionKnowledgeCount(t, answer["vectorlessCount"]); count != chunks {
			t.Fatalf("%s: the status counted %d vectorless chunks instead of %d", label, count, chunks)
		}
		if dimension := productionKnowledgeCount(t, answer["activeVectorDimension"]); dimension != 0 {
			t.Fatalf("%s: the status claimed an active vector dimension of %d", label, dimension)
		}
	}
	empty := productionKnowledgeCall(t, client, productionKnowledgeVectorStatusName, []any{projectPath, frozen})
	assertStatus("empty project", empty, 0)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("kb:import-text refused the text the status needs: %v", imported)
	}
	if count := productionKnowledgeCount(t, imported["chunkCount"]); count != 2 {
		t.Fatalf("the import counted %d chunks instead of 2", count)
	}
	status := productionKnowledgeCall(t, client, productionKnowledgeVectorStatusName, []any{projectPath, frozen})
	assertStatus("imported store", status, 2)
	// The honest backfill is a zero-vector no-op, and it spends nothing.
	indexPath := productionKnowledgeQueryIndexPath(projectPath)
	before := productionKnowledgeQueryFileBytes(t, indexPath)
	backfilled := productionKnowledgeCall(t, client, productionKnowledgeBackfillVectorsName,
		[]any{projectPath, frozen})
	if backfilled["success"] != true {
		t.Fatalf("kb:backfill-vectors refused instead of answering the honest no-op: %v", backfilled)
	}
	if processed := productionKnowledgeCount(t, backfilled["processed"]); processed != 0 {
		t.Fatalf("kb:backfill-vectors claimed %d vectors backfilled in an FTS-only store", processed)
	}
	if after := productionKnowledgeQueryFileBytes(t, indexPath); !bytes.Equal(after, before) {
		t.Fatal("a backfill wrote the canonical snapshot")
	}
}

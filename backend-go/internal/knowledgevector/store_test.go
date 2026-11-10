//go:build linux

package knowledgevector

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
)

// Every fixture here is synthetic: a temp directory, invented chunk ids and
// hand-written float32 vectors. No author file, database or provider is touched.

func vectorTestChunk(id, document, text string, vector ...float32) Chunk {
	return Chunk{ID: id, DocumentID: document, FileName: "第1章 测试正文.md", Text: text,
		CorpusKind: "project-knowledge", Vector: vector}
}

func vectorTestNumber(value int64) *int64 { return &value }

func vectorTestStore(generation int64, fingerprint string, chunks []Chunk) *Store {
	dimension := 0
	if len(chunks) > 0 {
		dimension = len(chunks[0].Vector)
	}
	name := SpaceTableName(generation)
	active := generation
	return &Store{
		ActiveGeneration: &active,
		Spaces: []Space{{
			Generation: generation, TableName: name, Status: "active", VectorDimension: dimension,
			ModelFingerprint: fingerprint, DistanceMetric: CosineMetric, CreatedAt: "2026-01-01T00:00:00.000Z",
		}},
		Tables: []Table{{Name: name, Version: TableVersion, Dimension: dimension, Chunks: chunks}},
	}
}

func vectorTestRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func TestVectorStoreRoundTripAndRegistryProjection(t *testing.T) {
	ctx := context.Background()
	root := vectorTestRoot(t)
	active := int64(2)
	store := &Store{
		ActiveGeneration: &active,
		Spaces: []Space{
			{Generation: 1, TableName: SpaceTableName(1), Status: "inactive", VectorDimension: 2,
				ModelFingerprint: "model-a", DistanceMetric: CosineMetric, CreatedAt: "2026-01-01T00:00:00.000Z"},
			{Generation: 2, TableName: SpaceTableName(2), Status: "active", VectorDimension: 2,
				ModelFingerprint: "model-b", DistanceMetric: CosineMetric, CreatedAt: "2026-01-02T00:00:00.000Z"},
		},
		Tables: []Table{
			{Name: SpaceTableName(1), Version: TableVersion, Dimension: 2,
				Chunks: []Chunk{vectorTestChunk("c1", "d1", "one", 1, 0)}},
			{Name: SpaceTableName(2), Version: TableVersion, Dimension: 2,
				Chunks: []Chunk{vectorTestChunk("c1", "d1", "one", 0, 1)}},
		},
	}
	if err := Write(ctx, root, store); err != nil {
		t.Fatalf("write refused a valid store: %v", err)
	}
	read, err := Read(ctx, root)
	if err != nil || read == nil {
		t.Fatalf("read: %v %v", read, err)
	}
	first, firstOK := read.Space(1)
	second, secondOK := read.Space(2)
	if !firstOK || !secondOK || first.Status != "inactive" || second.Status != "active" ||
		read.ActiveGeneration == nil || *read.ActiveGeneration != 2 {
		t.Fatalf("round trip lost the registry: %+v", read.Spaces)
	}
	table, ok := read.Table(SpaceTableName(2))
	if !ok || len(table.Chunks) != 1 || table.Chunks[0].Vector[1] != 1 {
		t.Fatalf("round trip lost a vector row: %+v", table)
	}
	raw, err := os.ReadFile(filepath.Join(root.Name(), RegistryFileName))
	if err != nil {
		t.Fatalf("no registry projection: %v", err)
	}
	registry, err := knowledgequery.DecodeVectorSpaceRegistry(ctx, raw)
	if err != nil {
		t.Fatalf("the browser registry does not decode: %v", err)
	}
	space, ok := registry.ActiveSpace()
	if !ok || space.Generation != 2 || space.ModelFingerprint != "model-b" {
		t.Fatalf("registry active space is wrong: %+v", registry)
	}
	if len(registry.Spaces) != 2 || registry.Spaces[0].Status != "inactive" {
		t.Fatalf("the superseded space must stay registered as inactive: %+v", registry.Spaces)
	}
}

func TestVectorStoreStatusCountsMissingChunks(t *testing.T) {
	all := map[string]bool{"c1": true, "c2": true, "c3": true}
	partial := vectorTestStore(1, "model-a", []Chunk{
		vectorTestChunk("c1", "d1", "one", 1, 0),
		vectorTestChunk("c2", "d2", "two", 0, 1),
	})
	partial.Spaces[0].Status = "building"
	partial.ActiveGeneration = nil
	status := partial.Status(all)
	if status.Active || status.Vectorless != 1 || status.Dimension != 0 {
		t.Fatalf("an incomplete building space must report one vectorless chunk and no dimension: %+v", status)
	}
	complete := vectorTestStore(1, "model-a", []Chunk{
		vectorTestChunk("c1", "d1", "one", 1, 0),
		vectorTestChunk("c2", "d2", "two", 0, 1),
		vectorTestChunk("c3", "d3", "three", 1, 1),
	})
	status = complete.Status(all)
	if !status.Active || status.Vectorless != 0 || status.Dimension != 2 {
		t.Fatalf("a complete active space must report zero vectorless chunks: %+v", status)
	}
	if status := (&Store{}).Status(all); status.Vectorless != 3 || status.HasSpace {
		t.Fatalf("a store with no space must report every canonical chunk vectorless: %+v", status)
	}
	// A space that claims active but has lost rows to a later canonical commit is
	// incomplete, not an index.
	relabelled := vectorTestStore(1, "model-a", []Chunk{vectorTestChunk("c1", "d1", "one", 1, 0)})
	status = relabelled.Status(all)
	if status.Active || status.Vectorless != 2 || status.Dimension != 0 {
		t.Fatalf("a stale active pointer must not report an active dimension: %+v", status)
	}
}

func TestVectorStoreRefusesUnregisteredOrMismatchedTable(t *testing.T) {
	ctx := context.Background()
	root := vectorTestRoot(t)
	store := vectorTestStore(1, "model-a", []Chunk{vectorTestChunk("c1", "d1", "one", 1, 0)})
	store.Tables[0].Name = SpaceTableName(2)
	if err := Write(ctx, root, store); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("an unregistered space table was accepted: %v", err)
	}
	store = vectorTestStore(1, "model-a", []Chunk{vectorTestChunk("c1", "d1", "one", 1, 0)})
	store.Tables[0].Chunks[0].Vector = []float32{1, 0, 0}
	if err := Write(ctx, root, store); !errors.Is(err, ErrStoreInvalid) {
		t.Fatalf("a row whose width differs from the dimension was accepted: %v", err)
	}
}

func TestVectorStoreAbsentIsNotAnError(t *testing.T) {
	root := vectorTestRoot(t)
	store, err := Read(context.Background(), root)
	if err != nil || store != nil {
		t.Fatalf("an absent store must be (nil, nil): %v %v", store, err)
	}
	executor, err := Open(context.Background(), root)
	if err != nil || executor != nil {
		t.Fatalf("an absent store must open as (nil, nil): %v %v", executor, err)
	}
}

func TestVectorStoreChapterNumberRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := vectorTestRoot(t)
	chunk := vectorTestChunk("c1", "d1", "one", 1, 0)
	chunk.ChapterNumber = vectorTestNumber(7)
	store := vectorTestStore(1, "model-a", []Chunk{chunk})
	if err := Write(ctx, root, store); err != nil {
		t.Fatal(err)
	}
	read, err := Read(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	table, _ := read.Table(SpaceTableName(1))
	if len(table.Chunks) != 1 || table.Chunks[0].ChapterNumber == nil || *table.Chunks[0].ChapterNumber != 7 {
		t.Fatalf("chapterNumber did not survive the round trip: %+v", table.Chunks)
	}
}

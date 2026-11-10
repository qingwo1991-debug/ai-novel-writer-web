//go:build linux

package knowledgevector

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
)

func vectorTestPlan(generation int64, fingerprint string, query []float64, topK int) knowledgequery.VectorPlan {
	return knowledgequery.VectorPlan{
		TableName: SpaceTableName(generation), TableVersion: TableVersion, Generation: generation,
		Dimension: len(query), ModelFingerprint: fingerprint, DistanceMetric: CosineMetric,
		Query: query, TopK: topK,
	}
}

func vectorTestExecutor(t *testing.T, store *Store) *Executor {
	t.Helper()
	ctx := context.Background()
	root := vectorTestRoot(t)
	if err := Write(ctx, root, store); err != nil {
		t.Fatalf("write: %v", err)
	}
	executor, err := Open(ctx, root)
	if err != nil || executor == nil {
		t.Fatalf("open: %v %v", executor, err)
	}
	return executor
}

func TestVectorExecutorCosineTopK(t *testing.T) {
	ctx := context.Background()
	store := vectorTestStore(1, "model-a", []Chunk{
		vectorTestChunk("c1", "d1", "aligned", 1, 0),
		vectorTestChunk("c2", "d2", "orthogonal", 0, 1),
		vectorTestChunk("c3", "d3", "opposed", -1, 0),
	})
	executor := vectorTestExecutor(t, store)
	response, err := executor.QueryVector(ctx, vectorTestPlan(1, "model-a", []float64{1, 0}, 2))
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if response.TableName != SpaceTableName(1) || response.TableVersion != TableVersion ||
		response.Generation != 1 || response.Dimension != 2 || response.DistanceMetric != CosineMetric {
		t.Fatalf("response identity is wrong: %+v", response)
	}
	if len(response.Hits) != 2 || response.Hits[0].ID != "c1" || response.Hits[1].ID != "c2" {
		t.Fatalf("cosine top-k order is wrong: %+v", response.Hits)
	}
	if response.Hits[0].Distance != 0 || math.Abs(response.Hits[1].Distance-1) > 1e-12 {
		t.Fatalf("cosine distances are wrong: %+v", response.Hits)
	}
	if response.Hits[0].FileName != "第1章 测试正文.md" || response.Hits[0].Text != "aligned" ||
		response.Hits[0].DocumentID != "d1" {
		t.Fatalf("a hit lost its canonical identity: %+v", response.Hits[0])
	}
}

func TestVectorExecutorFiltersBeforeTopK(t *testing.T) {
	ctx := context.Background()
	aligned := vectorTestChunk("c1", "d1", "aligned", 1, 0)
	aligned.CorpusKind = "reference"
	aligned.ChapterNumber = vectorTestNumber(9)
	orthogonal := vectorTestChunk("c2", "d2", "orthogonal", 0, 1)
	orthogonal.ChapterNumber = vectorTestNumber(2)
	opposed := vectorTestChunk("c3", "d3", "opposed", -1, 0)
	opposed.ChapterNumber = vectorTestNumber(3)
	store := vectorTestStore(1, "model-a", []Chunk{aligned, orthogonal, opposed})
	executor := vectorTestExecutor(t, store)
	// The nearest row is excluded by the corpus filter, so topK=1 must return the
	// next nearest: a filter applied after top-k would return nothing.
	plan := vectorTestPlan(1, "model-a", []float64{1, 0}, 1)
	plan.ApplyCorpusFilter = true
	plan.ExcludedCorpusKinds = []string{"reference"}
	response, err := executor.QueryVector(ctx, plan)
	if err != nil || len(response.Hits) != 1 || response.Hits[0].ID != "c2" {
		t.Fatalf("corpus filter was not applied before top-k: %+v %v", response.Hits, err)
	}
	// A stale managed copy is excluded the same way.
	plan = vectorTestPlan(1, "model-a", []float64{1, 0}, 1)
	plan.ExcludedDocumentIDs = []string{"d1"}
	response, err = executor.QueryVector(ctx, plan)
	if err != nil || len(response.Hits) != 1 || response.Hits[0].ID != "c2" {
		t.Fatalf("stale-copy exclusion was not applied before top-k: %+v %v", response.Hits, err)
	}
	// A chapter scope keeps only rows inside the range.
	plan = vectorTestPlan(1, "model-a", []float64{1, 0}, 3)
	plan.ApplyChapterScope = true
	plan.Scope = &knowledgequery.ChapterScope{From: 1, To: 3}
	response, err = executor.QueryVector(ctx, plan)
	if err != nil || len(response.Hits) != 2 {
		t.Fatalf("chapter scope kept the wrong rows: %+v %v", response.Hits, err)
	}
	for _, hit := range response.Hits {
		if hit.ID == "c1" {
			t.Fatalf("chapter scope kept the out-of-range row: %+v", response.Hits)
		}
	}
}

func TestVectorExecutorRefusesSwappedStoreFile(t *testing.T) {
	ctx := context.Background()
	root := vectorTestRoot(t)
	first := vectorTestStore(1, "model-a", []Chunk{vectorTestChunk("c1", "d1", "one", 1, 0)})
	if err := Write(ctx, root, first); err != nil {
		t.Fatal(err)
	}
	executor, err := Open(ctx, root)
	if err != nil || executor == nil {
		t.Fatalf("open: %v %v", executor, err)
	}
	if _, err := executor.QueryVector(ctx, vectorTestPlan(1, "model-a", []float64{1, 0}, 1)); err != nil {
		t.Fatalf("the pinned store must serve its own query: %v", err)
	}
	// Replace the store with different bytes at a new inode. The pinned identity
	// no longer matches, so the query is refused rather than answered from a file
	// the caller never admitted.
	second := vectorTestStore(1, "model-a", []Chunk{
		vectorTestChunk("c1", "d1", "one", 1, 0),
		vectorTestChunk("c2", "d2", "two", 0, 1),
	})
	if err := Write(ctx, root, second); err != nil {
		t.Fatal(err)
	}
	if _, err := executor.QueryVector(ctx, vectorTestPlan(1, "model-a", []float64{1, 0}, 1)); !errors.Is(err, knowledgequery.ErrDomain) {
		t.Fatalf("a swapped store file was queried: %v", err)
	}
}

func TestVectorExecutorRefusesMismatchedPlan(t *testing.T) {
	ctx := context.Background()
	store := vectorTestStore(1, "model-a", []Chunk{vectorTestChunk("c1", "d1", "one", 1, 0)})
	executor := vectorTestExecutor(t, store)
	plans := []knowledgequery.VectorPlan{
		vectorTestPlan(1, "model-b", []float64{1, 0}, 1),
		vectorTestPlan(2, "model-a", []float64{1, 0}, 1),
		vectorTestPlan(1, "model-a", []float64{1, 0, 0}, 1),
		vectorTestPlan(1, "model-a", []float64{1, 0}, 0),
	}
	for _, plan := range plans {
		if _, err := executor.QueryVector(ctx, plan); err == nil {
			t.Fatalf("a mismatched plan was served: %+v", plan)
		}
	}
}

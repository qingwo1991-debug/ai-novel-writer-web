//go:build linux

package knowledgevector

import (
	"context"
	"math"
	"os"
	"sort"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
)

// Executor is one project's admitted, pinned vector query executor. It is built
// from the storage root the knowledge lane already proved and the identity of
// the store file read at that moment; QueryVector re-reads the file and proves
// the same inode before and after, so a store swapped under the query is
// refused rather than queried. It implements knowledgequery.PlanFilteringExecutor
// because it applies the plan's chapter scope, corpus exclusions and stale-copy
// exclusions inside the store, before top-k selection.
type Executor struct {
	root  *os.Root
	name  string
	info  os.FileInfo
	store *Store
}

var _ knowledgequery.PlanFilteringExecutor = (*Executor)(nil)

// Open reads and decodes the store and pins the exact inode it read. It returns
// (nil, nil) when this project has no store yet.
func Open(ctx context.Context, root *os.Root) (*Executor, error) {
	if root == nil || ctx == nil {
		return nil, ErrStoreUnavailable
	}
	raw, info, missing, err := readFile(ctx, root, StoreFileName, MaxStoreBytes)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, nil
	}
	store, err := decodeStore(ctx, raw)
	if err != nil {
		return nil, err
	}
	return &Executor{root: root, name: StoreFileName, info: info, store: store}, nil
}

// Store is the decoded store this executor pinned. It is a read-only view: the
// caller must not mutate it.
func (e *Executor) Store() *Store {
	if e == nil {
		return nil
	}
	return e.store
}

// Registry projects the pinned store's spaces into the registry form
// knowledgequery.PrepareVectorPlanOverSpaces admits.
func (e *Executor) Registry() knowledgequery.VectorSpaceRegistry {
	registry := knowledgequery.VectorSpaceRegistry{}
	if e == nil || e.store == nil {
		return registry
	}
	if e.store.ActiveGeneration != nil {
		generation := *e.store.ActiveGeneration
		registry.ActiveGeneration = &generation
	}
	registry.Spaces = make([]knowledgequery.VectorSpace, 0, len(e.store.Spaces))
	for _, space := range e.store.Spaces {
		registry.Spaces = append(registry.Spaces, knowledgequery.VectorSpace{
			Generation: space.Generation, TableName: space.TableName, Status: space.Status,
			VectorDimension: space.VectorDimension, ModelFingerprint: space.ModelFingerprint,
			DistanceMetric: space.DistanceMetric, CreatedAt: space.CreatedAt,
		})
	}
	return registry
}

// AppliesPlanFilters marks this executor as one that applies the plan's scope,
// corpus and stale-copy filters before top-k.
func (e *Executor) AppliesPlanFilters() {}

// QueryVector answers one admitted plan with the real top-k hits of the pinned
// store, ordered by the plan's metric. Every failure is symbolic.
func (e *Executor) QueryVector(ctx context.Context, plan knowledgequery.VectorPlan) (knowledgequery.VectorResponse, error) {
	if e == nil || e.root == nil || e.name == "" || e.info == nil ||
		plan.Generation <= 0 || plan.TableVersion == 0 || plan.Dimension <= 0 ||
		plan.Dimension > MaxDimension || len(plan.Query) != plan.Dimension || plan.TopK <= 0 {
		return knowledgequery.VectorResponse{}, knowledgequery.ErrVectorQueryUnavailable
	}
	if err := ctx.Err(); err != nil {
		return knowledgequery.VectorResponse{}, err
	}
	// Prove the pinned file, read exactly its bytes, prove it again.
	raw, info, missing, err := readFile(ctx, e.root, e.name, MaxStoreBytes)
	if err != nil {
		return knowledgequery.VectorResponse{}, err
	}
	if missing || info == nil || !os.SameFile(info, e.info) {
		return knowledgequery.VectorResponse{}, knowledgequery.ErrDomain
	}
	store, err := decodeStore(ctx, raw)
	if err != nil {
		return knowledgequery.VectorResponse{}, knowledgequery.ErrDomain
	}
	after, err := e.root.Lstat(e.name)
	if err != nil || after == nil || after.Mode()&os.ModeSymlink != 0 || !os.SameFile(after, e.info) {
		return knowledgequery.VectorResponse{}, knowledgequery.ErrDomain
	}
	space, ok := store.Space(plan.Generation)
	if !ok || space.TableName != plan.TableName || space.Status != "active" ||
		space.VectorDimension != plan.Dimension || space.ModelFingerprint != plan.ModelFingerprint ||
		space.DistanceMetric != plan.DistanceMetric {
		return knowledgequery.VectorResponse{}, knowledgequery.ErrDomain
	}
	table, ok := store.Table(plan.TableName)
	if !ok || table.Version != plan.TableVersion || table.Dimension != plan.Dimension {
		return knowledgequery.VectorResponse{}, knowledgequery.ErrDomain
	}
	excludedDocuments := make(map[string]bool, len(plan.ExcludedDocumentIDs))
	for _, id := range plan.ExcludedDocumentIDs {
		excludedDocuments[id] = true
	}
	excludedKinds := make(map[string]bool, len(plan.ExcludedCorpusKinds))
	for _, kind := range plan.ExcludedCorpusKinds {
		excludedKinds[kind] = true
	}
	type scored struct {
		chunk    Chunk
		distance float64
	}
	candidates := make([]scored, 0, len(table.Chunks))
	for _, chunk := range table.Chunks {
		if err := ctx.Err(); err != nil {
			return knowledgequery.VectorResponse{}, err
		}
		if excludedDocuments[chunk.DocumentID] {
			continue
		}
		if plan.ApplyCorpusFilter && excludedKinds[chunk.CorpusKind] {
			continue
		}
		if plan.ApplyChapterScope {
			scope := plan.Scope
			if scope == nil || chunk.ChapterNumber == nil ||
				*chunk.ChapterNumber < scope.From || *chunk.ChapterNumber > scope.To {
				continue
			}
		}
		distance, ok := vectorDistance(plan.DistanceMetric, plan.Query, chunk.Vector)
		if !ok {
			continue
		}
		candidates = append(candidates, scored{chunk: chunk, distance: distance})
	}
	// A stable order over the identity makes a tie deterministic: two providers
	// that agree on every distance still answer the same top-k in the same order.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].distance != candidates[j].distance {
			return candidates[i].distance < candidates[j].distance
		}
		return candidates[i].chunk.ID < candidates[j].chunk.ID
	})
	if len(candidates) > plan.TopK {
		candidates = candidates[:plan.TopK]
	}
	hits := make([]knowledgequery.VectorHit, 0, len(candidates))
	for _, candidate := range candidates {
		hits = append(hits, knowledgequery.VectorHit{
			ID: candidate.chunk.ID, DocumentID: candidate.chunk.DocumentID,
			FileName: candidate.chunk.FileName, Text: candidate.chunk.Text,
			Distance: candidate.distance,
		})
	}
	return knowledgequery.VectorResponse{
		TableName: plan.TableName, TableVersion: plan.TableVersion, Generation: plan.Generation,
		Dimension: plan.Dimension, ModelFingerprint: plan.ModelFingerprint,
		DistanceMetric: plan.DistanceMetric, Hits: hits,
	}, nil
}

// vectorDistance scores one stored row against the query under the plan's
// metric. A row that cannot be scored (a length mismatch, a zero-norm cosine
// pair) is skipped rather than scored as a maximum distance, because a row with
// no direction is not a weak hit but no hit at all.
func vectorDistance(metric string, query []float64, stored []float32) (float64, bool) {
	if len(stored) != len(query) || len(query) == 0 {
		return 0, false
	}
	switch metric {
	case "cosine":
		var dot, queryNorm, storedNorm float64
		for i, value := range query {
			sample := float64(stored[i])
			dot += value * sample
			queryNorm += value * value
			storedNorm += sample * sample
		}
		if queryNorm == 0 || storedNorm == 0 {
			return 0, false
		}
		similarity := dot / (math.Sqrt(queryNorm) * math.Sqrt(storedNorm))
		if math.IsNaN(similarity) {
			return 0, false
		}
		// Clamp only the rounding excursions of a finite dot product.
		if similarity > 1 {
			similarity = 1
		}
		if similarity < -1 {
			similarity = -1
		}
		return 1 - similarity, true
	case "l2":
		var sum float64
		for i, value := range query {
			delta := value - float64(stored[i])
			sum += delta * delta
		}
		return math.Sqrt(sum), true
	case "dot":
		var dot float64
		for i, value := range query {
			dot += value * float64(stored[i])
		}
		return -dot, true
	default:
		return 0, false
	}
}

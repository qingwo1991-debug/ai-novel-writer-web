//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
	"math"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/embedding"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgevector"
)

// This file is the embedding half of the knowledge lane. It resolves the one
// committed embedding model the author configured (the model profile named by
// config.json's defaultEmbeddingModelId, else the first saved profile whose
// purposes include "embedding"), reads that profile's plaintext apiKey from the
// same private store the generation executor reads, and dispatches through the
// SAME committed model-egress policy (productionOpen.ModelEgress). It never
// builds a bare http.Client, never formats an endpoint, key or vector into an
// error, and never returns a partial embedding batch as a vector.
//
// The vector space store is internal/knowledgevector: one
// knowledgedomain.Table-shaped table per generation in
// {projectPath}/.ai-novel/knowledge-vectors.json plus the browser-readable
// embedding-spaces.json projection. The canonical documents/chunks snapshot is
// not touched by this file at all, so a store with no embedding model keeps the
// byte-exact FTS-only behavior it had before.
const (
	// productionKnowledgeEmbeddingPurpose is the purpose value a saved profile
	// must carry to be selectable by purpose.
	productionKnowledgeEmbeddingPurpose = "embedding"
	// productionKnowledgeEmbeddingBatchSize bounds one dispatch. It is
	// DEFAULT_EMBEDDING_OPTIONS' own batchSize.
	productionKnowledgeEmbeddingBatchSize = 50
)

var (
	errProductionKnowledgeEmbeddingUnavailable  = errors.New("KNOWLEDGE_EMBEDDING_UNAVAILABLE")
	errProductionKnowledgeEmbeddingUnconfigured = errors.New("KNOWLEDGE_EMBEDDING_UNCONFIGURED")
	errProductionKnowledgeEmbeddingResponse     = errors.New("KNOWLEDGE_EMBEDDING_RESPONSE_INVALID")
)

// productionKnowledgeEmbedding is one resolved, dispatchable embedding model.
// Identity is the model fingerprint every stored vector is keyed by; it is a
// hash of the protocol, endpoint and model name, never of the key.
type productionKnowledgeEmbedding struct {
	Protocol embedding.Protocol
	Model    embedding.Model
	Identity string
}

// productionKnowledgeModel is the lane's embedding seam. A nil source means this
// lane has no embedding configuration at all, which is the FTS-only mode.
type productionKnowledgeModel interface {
	EmbeddingSettings() (productionKnowledgeEmbedding, bool, error)
	EgressClient() (*egress.Client, error)
}

// productionEmbeddingSource is the real source over the admitted lane.
type productionEmbeddingSource struct {
	open *productionOpen
}

func (s productionEmbeddingSource) EmbeddingSettings() (productionKnowledgeEmbedding, bool, error) {
	if s.open == nil {
		return productionKnowledgeEmbedding{}, false, errProductionKnowledgeEmbeddingUnavailable
	}
	s.open.mu.Lock()
	config, closed := s.open.config, s.open.closed
	s.open.mu.Unlock()
	if closed || config == nil {
		return productionKnowledgeEmbedding{}, false, errProductionKnowledgeEmbeddingUnavailable
	}
	return productionKnowledgeEmbeddingSettings(config)
}

func (s productionEmbeddingSource) EgressClient() (*egress.Client, error) {
	if s.open == nil {
		return nil, errProductionModelEgressUnavailable
	}
	return s.open.ModelEgress()
}

// productionKnowledgeEmbeddingSettings resolves the committed embedding model.
// An absent selection is (false, nil): not configured is not an error. A config
// the service cannot read is a symbolic refusal, because answering "no model"
// for a damaged document would silently disable retrieval.
func productionKnowledgeEmbeddingSettings(config *globalconfig.Service) (productionKnowledgeEmbedding, bool, error) {
	if config == nil {
		return productionKnowledgeEmbedding{}, false, errProductionKnowledgeEmbeddingUnavailable
	}
	records, err := config.ListPrivateModels()
	if err != nil {
		return productionKnowledgeEmbedding{}, false, errProductionKnowledgeEmbeddingUnavailable
	}
	defaultValue, err := config.DefaultEmbeddingModel()
	if err != nil {
		return productionKnowledgeEmbedding{}, false, errProductionKnowledgeEmbeddingUnavailable
	}
	targetID := ""
	if text, ok := defaultValue.(string); ok {
		targetID = strings.TrimSpace(text)
	}
	var selected map[string]any
	if targetID != "" {
		for _, record := range records {
			if id, _ := record["id"].(string); id == targetID {
				selected = record
				break
			}
		}
		// A default id that names no saved profile is not a usable model.
		if selected == nil {
			return productionKnowledgeEmbedding{}, false, nil
		}
	} else {
		for _, record := range records {
			if productionKnowledgeModelHasPurpose(record, productionKnowledgeEmbeddingPurpose) {
				selected = record
				break
			}
		}
		if selected == nil {
			return productionKnowledgeEmbedding{}, false, nil
		}
	}
	protocol, ok := productionKnowledgeEmbeddingProtocol(selected["protocol"])
	if !ok {
		return productionKnowledgeEmbedding{}, false, nil
	}
	baseURL, _ := selected["baseUrl"].(string)
	modelName, _ := selected["modelName"].(string)
	key, _ := selected["apiKey"].(string)
	if strings.TrimSpace(baseURL) == "" || !productionKnowledgeEmbeddingKeyUsable(baseURL, key) {
		return productionKnowledgeEmbedding{}, false, nil
	}
	identity := productionKnowledgeEmbeddingFingerprint(protocol, baseURL, modelName)
	return productionKnowledgeEmbedding{
		Protocol: protocol,
		Model:    embedding.Model{BaseURL: baseURL, APIKey: key, ModelName: modelName},
		Identity: identity,
	}, true, nil
}

func productionKnowledgeModelHasPurpose(record map[string]any, purpose string) bool {
	purposes, ok := record["purposes"].([]any)
	if !ok {
		return false
	}
	for _, value := range purposes {
		if text, ok := value.(string); ok && text == purpose {
			return true
		}
	}
	return false
}

func productionKnowledgeEmbeddingProtocol(value any) (embedding.Protocol, bool) {
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case string(embedding.OpenAI):
		return embedding.OpenAI, true
	case string(embedding.Gemini):
		return embedding.Gemini, true
	default:
		return "", false
	}
}

// productionKnowledgeEmbeddingKeyUsable is hasRequiredApiKey over the committed
// plaintext store: a key, or a loopback endpoint that needs none.
func productionKnowledgeEmbeddingKeyUsable(baseURL, key string) bool {
	if strings.TrimSpace(key) != "" {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed == nil {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

// productionKnowledgeEmbeddingFingerprint is the model identity stored in every
// registry entry. It covers exactly what changes the vector space: the protocol,
// the endpoint and the resolved model name. The key is never an input.
func productionKnowledgeEmbeddingFingerprint(protocol embedding.Protocol, baseURL, modelName string) string {
	name := strings.TrimSpace(modelName)
	if name == "" {
		if protocol == embedding.Gemini {
			name = "gemini-embedding-001"
		} else {
			name = "text-embedding-3-small"
		}
	}
	digest := sha256.Sum256([]byte("knowledge-embedding:v1\x00" + string(protocol) + "\x00" + baseURL + "\x00" + name))
	return hex.EncodeToString(digest[:])
}

// productionKnowledgeEmbeddingDispatch sends one bounded batch through the
// committed egress policy and returns no partial result on any failure.
func productionKnowledgeEmbeddingDispatch(ctx context.Context, client *egress.Client, target productionKnowledgeEmbedding, texts []string) ([][]float64, error) {
	if client == nil || len(texts) == 0 {
		return nil, errProductionKnowledgeEmbeddingUnavailable
	}
	batch := productionKnowledgeEmbeddingBatchSize
	vectors, err := embedding.New(client).Generate(ctx, target.Protocol, target.Model, texts, &batch, nil)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, errProductionKnowledgeEmbeddingResponse
	}
	return vectors, nil
}

func productionKnowledgeChunkIDs(index productionKnowledgeIndex) map[string]bool {
	ids := make(map[string]bool, len(index.Chunks))
	for _, chunk := range index.Chunks {
		ids[chunk.ID] = true
	}
	return ids
}

// updateVectors is the one vector writer. It is called after a canonical commit
// (import) and by kb:backfill-vectors. It never fails an import:
//
//   - no model configured returns errProductionKnowledgeEmbeddingUnconfigured
//     before it reads or writes anything, so the FTS-only lane is byte-identical;
//   - an embedding batch that fails is counted in failed and the chunks it could
//     not cover stay vectorless (the space is demoted to building, so a search
//     degrades to FTS instead of mixing a stale space);
//   - a store that cannot be read or written is a symbolic error and leaves the
//     last committed store exactly as it was.
func (k *productionKnowledgeImports) updateVectors(ctx context.Context, storage *productionKnowledgeStorage, index productionKnowledgeIndex) (int64, int64, error) {
	if k == nil || storage == nil || storage.storage == nil || k.model == nil {
		return 0, 0, errProductionKnowledgeEmbeddingUnconfigured
	}
	target, configured, err := k.model.EmbeddingSettings()
	if err != nil {
		return 0, 0, err
	}
	if !configured {
		return 0, 0, errProductionKnowledgeEmbeddingUnconfigured
	}
	client, err := k.model.EgressClient()
	if err != nil {
		return 0, 0, err
	}
	// An endpoint outside the committed model egress is never dialed: it degrades
	// to vectorless exactly like a dispatch failure.
	if err := client.CheckBaseURL(target.Model.BaseURL); err != nil {
		return 0, 0, errProductionKnowledgeEmbeddingUnavailable
	}
	existing, err := knowledgevector.Read(ctx, storage.storage)
	if err != nil {
		return 0, 0, err
	}
	generation, createdAt, dimension := productionKnowledgeVectorTarget(existing, target)
	reused := map[string]knowledgevector.Chunk{}
	if existing != nil && dimension > 0 {
		if table, ok := existing.Table(knowledgevector.SpaceTableName(generation)); ok && table.Dimension == dimension {
			for _, chunk := range table.Chunks {
				reused[chunk.ID] = chunk
			}
		}
	}
	rows := make([]knowledgevector.Chunk, 0, len(index.Chunks))
	pending := make([]productionKnowledgeChunk, 0, len(index.Chunks))
	pendingRows := make([]int, 0, len(index.Chunks))
	for _, chunk := range index.Chunks {
		if err := ctx.Err(); err != nil {
			return 0, 0, err
		}
		if vector, ok := reused[chunk.ID]; ok && productionKnowledgeVectorReusable(vector, chunk, dimension) {
			rows = append(rows, vector)
			continue
		}
		rows = append(rows, productionKnowledgeVectorBase(chunk))
		pending = append(pending, chunk)
		pendingRows = append(pendingRows, len(rows)-1)
	}
	var processed, failed int64
	var embedFailure error
	for start := 0; start < len(pending); start += productionKnowledgeEmbeddingBatchSize {
		if err := ctx.Err(); err != nil {
			return processed, failed, err
		}
		end := start + productionKnowledgeEmbeddingBatchSize
		if end > len(pending) {
			end = len(pending)
		}
		texts := make([]string, 0, end-start)
		for _, chunk := range pending[start:end] {
			texts = append(texts, chunk.Text)
		}
		vectors, dispatchErr := productionKnowledgeEmbeddingDispatch(ctx, client, target, texts)
		if dispatchErr == nil && len(vectors) != end-start {
			dispatchErr = errProductionKnowledgeEmbeddingResponse
		}
		if dispatchErr == nil {
			for _, vector := range vectors {
				if dimension == 0 {
					dimension = len(vector)
				}
				if len(vector) == 0 || len(vector) != dimension {
					dispatchErr = errProductionKnowledgeEmbeddingResponse
					break
				}
			}
		}
		if dispatchErr != nil {
			// This batch and every later one stay vectorless; the chunks already
			// embedded keep their vectors and the space stays building.
			embedFailure = dispatchErr
			failed += int64(len(pending) - start)
			break
		}
		narrowed := make([][]float32, len(vectors))
		finite := true
		for offset, vector := range vectors {
			values := make([]float32, len(vector))
			for i, value := range vector {
				if math.IsNaN(value) || math.IsInf(value, 0) {
					finite = false
					break
				}
				values[i] = float32(value)
			}
			if !finite {
				break
			}
			narrowed[offset] = values
		}
		if !finite {
			embedFailure = errProductionKnowledgeEmbeddingResponse
			failed += int64(len(pending) - start)
			break
		}
		for offset, values := range narrowed {
			rows[pendingRows[start+offset]].Vector = values
			processed++
		}
	}
	next := &knowledgevector.Store{}
	if existing != nil {
		next.Spaces = append(next.Spaces, existing.Spaces...)
		next.Tables = append(next.Tables, existing.Tables...)
	}
	if dimension > 0 {
		tableName := knowledgevector.SpaceTableName(generation)
		productionKnowledgeUpsertVectorTable(next, knowledgevector.Table{
			Name: tableName, Version: knowledgevector.TableVersion, Dimension: dimension, Chunks: rows,
		})
		productionKnowledgeUpsertVectorSpace(next, knowledgevector.Space{
			Generation: generation, TableName: tableName, Status: "building",
			VectorDimension: dimension, ModelFingerprint: target.Identity,
			DistanceMetric: knowledgevector.CosineMetric, CreatedAt: createdAt,
		})
	}
	productionKnowledgeRecomputeVectorSpaces(next, productionKnowledgeChunkIDs(index), target)
	productionKnowledgePruneVectorSpaces(next, generation)
	if len(next.Tables) == 0 {
		return processed, failed, embedFailure
	}
	if err := knowledgevector.Write(ctx, storage.storage, next); err != nil {
		return processed, failed, err
	}
	return processed, failed, embedFailure
}

// refreshVectorsBestEffort runs after a canonical commit. A canonical import is
// already successful at this point, so every vector failure is logged
// symbolically and never returned.
func (k *productionKnowledgeImports) refreshVectorsBestEffort(ctx context.Context, storage *productionKnowledgeStorage) {
	if k == nil || k.model == nil || storage == nil {
		return
	}
	// Resolve before reading anything: a lane with no embedding model must not
	// pay an extra snapshot read per import, and must create no vector file.
	if _, configured, err := k.model.EmbeddingSettings(); err != nil || !configured {
		return
	}
	index, err := productionKnowledgeReadIndex(ctx, storage.storage)
	if err != nil {
		return
	}
	if _, _, err := k.updateVectors(ctx, storage, index); err != nil && !errors.Is(err, errProductionKnowledgeEmbeddingUnconfigured) {
		log.Printf("novel-server: knowledge vectors not refreshed (%v)", err)
	}
}

func productionKnowledgeVectorTarget(existing *knowledgevector.Store, target productionKnowledgeEmbedding) (int64, string, int) {
	if existing != nil {
		found := false
		best := knowledgevector.Space{}
		for _, space := range existing.Spaces {
			if space.ModelFingerprint != target.Identity || space.DistanceMetric != knowledgevector.CosineMetric {
				continue
			}
			if !found || space.Generation > best.Generation {
				best = space
				found = true
			}
		}
		if found {
			return best.Generation, best.CreatedAt, best.VectorDimension
		}
	}
	if existing == nil {
		// Generations start at 1: the execution envelope refuses generation <= 0.
		return 1, productionKnowledgeTimestamp(time.Now()), 0
	}
	return existing.NextGeneration(), productionKnowledgeTimestamp(time.Now()), 0
}

func productionKnowledgeVectorBase(chunk productionKnowledgeChunk) knowledgevector.Chunk {
	return knowledgevector.Chunk{
		ID: chunk.ID, DocumentID: chunk.DocID, FileName: chunk.FileName, Text: chunk.Text,
		ChapterNumber: chunk.ChapterNumber, CorpusKind: chunk.CorpusKind,
	}
}

// productionKnowledgeVectorReusable accepts an existing row only when it is the
// same canonical chunk (id, document, file name and text all match) and carries
// a vector of the target dimension. A re-import mints fresh chunk ids, so a
// superseded row is never reused.
func productionKnowledgeVectorReusable(vector knowledgevector.Chunk, chunk productionKnowledgeChunk, dimension int) bool {
	if dimension <= 0 || len(vector.Vector) != dimension {
		return false
	}
	return vector.ID == chunk.ID && vector.DocumentID == chunk.DocID &&
		vector.FileName == chunk.FileName && vector.Text == chunk.Text
}

func productionKnowledgeUpsertVectorSpace(store *knowledgevector.Store, space knowledgevector.Space) {
	for i := range store.Spaces {
		if store.Spaces[i].Generation == space.Generation {
			store.Spaces[i] = space
			return
		}
	}
	store.Spaces = append(store.Spaces, space)
}

func productionKnowledgeUpsertVectorTable(store *knowledgevector.Store, table knowledgevector.Table) {
	for i := range store.Tables {
		if store.Tables[i].Name == table.Name {
			store.Tables[i] = table
			return
		}
	}
	store.Tables = append(store.Tables, table)
}

// productionKnowledgeRecomputeVectorSpaces applies the reindex gate: only a
// space whose model identity matches the current model and whose rows cover
// exactly the canonical chunks may be active. A space built by a different model
// is marked inactive, so distances from two models are never mixed.
func productionKnowledgeRecomputeVectorSpaces(store *knowledgevector.Store, chunkIDs map[string]bool, target productionKnowledgeEmbedding) {
	covered := make(map[string]bool, len(store.Tables))
	for _, table := range store.Tables {
		covered[table.Name] = productionKnowledgeVectorTableCovers(table, chunkIDs)
	}
	active := int64(-1)
	for _, space := range store.Spaces {
		if space.ModelFingerprint != target.Identity || space.DistanceMetric != knowledgevector.CosineMetric {
			continue
		}
		if !covered[space.TableName] {
			continue
		}
		if active < 0 || space.Generation > active {
			active = space.Generation
		}
	}
	for i := range store.Spaces {
		space := &store.Spaces[i]
		switch {
		case space.Generation == active:
			space.Status = "active"
		case space.ModelFingerprint == target.Identity && space.DistanceMetric == knowledgevector.CosineMetric:
			space.Status = "building"
		default:
			space.Status = "inactive"
		}
	}
	if active < 0 {
		store.ActiveGeneration = nil
		return
	}
	value := active
	store.ActiveGeneration = &value
}

func productionKnowledgeVectorTableCovers(table knowledgevector.Table, chunkIDs map[string]bool) bool {
	if len(table.Chunks) != len(chunkIDs) {
		return false
	}
	for _, chunk := range table.Chunks {
		if !chunkIDs[chunk.ID] {
			return false
		}
	}
	return true
}

// productionKnowledgePruneVectorSpaces bounds the store to three generations:
// the generation just written, the active one, and the newest other one (kept so
// one model change stays reversible). Every dropped space's table goes with it.
func productionKnowledgePruneVectorSpaces(store *knowledgevector.Store, generation int64) {
	keep := make(map[int64]bool, 3)
	keep[generation] = true
	if store.ActiveGeneration != nil {
		keep[*store.ActiveGeneration] = true
	}
	newestOther := int64(-1)
	for _, space := range store.Spaces {
		if keep[space.Generation] {
			continue
		}
		if space.Generation > newestOther {
			newestOther = space.Generation
		}
	}
	if newestOther >= 0 {
		keep[newestOther] = true
	}
	if len(keep) >= len(store.Spaces) {
		return
	}
	spaces := make([]knowledgevector.Space, 0, len(keep))
	names := make(map[string]bool, len(keep))
	for _, space := range store.Spaces {
		if keep[space.Generation] {
			spaces = append(spaces, space)
			names[space.TableName] = true
		}
	}
	tables := make([]knowledgevector.Table, 0, len(keep))
	for _, table := range store.Tables {
		if names[table.Name] {
			tables = append(tables, table)
		}
	}
	store.Spaces = spaces
	store.Tables = tables
}

// searchWithVectors answers one admitted query from the active vector space, or
// reports that the caller must use SearchFTS. It degrades to false on every
// embedding, egress, registry or store failure; a context error is returned
// because a cancelled request is not a fallback.
func (k *productionKnowledgeSearches) searchWithVectors(ctx context.Context, storage *productionKnowledgeStorage, index productionKnowledgeIndex, query string, topK int, scope *knowledgequery.ChapterScope, excludedCorpusKinds []string) ([]knowledgequery.SearchResult, bool, error) {
	if k == nil || k.imports == nil || k.imports.model == nil || storage == nil || storage.storage == nil {
		return nil, false, nil
	}
	if topK <= 0 || len(query) > 32*1024 || !utf8.ValidString(query) {
		return nil, false, nil
	}
	target, configured, err := k.imports.model.EmbeddingSettings()
	if err != nil || !configured {
		return nil, false, nil
	}
	client, err := k.imports.model.EgressClient()
	if err != nil {
		return nil, false, nil
	}
	if err := client.CheckBaseURL(target.Model.BaseURL); err != nil {
		return nil, false, nil
	}
	executor, err := knowledgevector.Open(ctx, storage.storage)
	if err != nil || executor == nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, false, contextError
		}
		return nil, false, nil
	}
	store := executor.Store()
	status := store.Status(productionKnowledgeChunkIDs(index))
	if !status.Active {
		return nil, false, nil
	}
	space, ok := store.Space(status.Generation)
	if !ok || space.ModelFingerprint != target.Identity || space.DistanceMetric != knowledgevector.CosineMetric {
		return nil, false, nil
	}
	table, ok := store.Table(space.TableName)
	if !ok || table.Version != knowledgevector.TableVersion || table.Dimension != space.VectorDimension {
		return nil, false, nil
	}
	vectors, err := productionKnowledgeEmbeddingDispatch(ctx, client, target, []string{query})
	if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 || len(vectors[0]) != space.VectorDimension {
		if contextError := ctx.Err(); contextError != nil {
			return nil, false, contextError
		}
		return nil, false, nil
	}
	request := knowledgequery.VectorRequest{
		Query: vectors[0], ModelFingerprint: target.Identity,
		DistanceMetric: knowledgevector.CosineMetric, TopK: topK,
		Scope: scope, ExcludedCorpusKinds: excludedCorpusKinds,
	}
	results, err := knowledgequery.SearchWithAdmittedVectorOverSpaces(ctx, productionKnowledgeSnapshotOf(index),
		storage.storage, productionKnowledgeCopyBudget(), request, query, executor.Registry(), table.Version,
		executor, func(context.Context) error { return storage.verify() })
	if err != nil {
		if contextError := ctx.Err(); contextError != nil {
			return nil, false, contextError
		}
		return nil, false, nil
	}
	return results, true, nil
}

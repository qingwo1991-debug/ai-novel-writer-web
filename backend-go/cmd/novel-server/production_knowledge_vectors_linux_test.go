//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/embedding"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgevector"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests exercise the embedding half of the knowledge lane against
// synthetic data only: a temp project seeded by the shared knowledge lane, a
// loopback httptest embedding server, and hand-written vectors. No real provider,
// author file or credential is involved. The embedding seam is the same
// productionKnowledgeModel the lane binds, filled with a fake resolver so the
// model identity and endpoint are exactly what each test needs.

// productionVectorAbsentQuery matches no indexed chunk, so the FTS answer for it
// is the empty result set. Any hit for it therefore proves the vector path ran.
const productionVectorAbsentQuery = "zzzunmatchedtermzzz"

type productionVectorModel struct {
	settings    productionKnowledgeEmbedding
	configured  bool
	settingsErr error
	client      *egress.Client
	clientErr   error
}

func (m productionVectorModel) EmbeddingSettings() (productionKnowledgeEmbedding, bool, error) {
	return m.settings, m.configured, m.settingsErr
}

func (m productionVectorModel) EgressClient() (*egress.Client, error) {
	if m.clientErr != nil {
		return nil, m.clientErr
	}
	if m.client == nil {
		return nil, errProductionModelEgressUnconfigured
	}
	return m.client, nil
}

func productionVectorPolicy(t *testing.T, origins ...string) *egress.Client {
	t.Helper()
	policy, err := egress.New(origins)
	if err != nil {
		t.Fatalf("egress policy: %v", err)
	}
	t.Cleanup(policy.Close)
	return policy
}

// productionVectorSyntheticVector is a deterministic 3-dimension unit-ish vector
// derived from the text, so two requests for the same text agree and different
// texts differ without any provider.
func productionVectorSyntheticVector(text string) []float64 {
	digest := sha256.Sum256([]byte(text))
	return []float64{
		float64(digest[0])/255 + 0.01,
		float64(digest[1])/255 + 0.01,
		float64(digest[2])/255 + 0.01,
	}
}

// productionVectorEmbeddingServer is a synthetic OpenAI-protocol embedding
// endpoint on loopback.
func productionVectorEmbeddingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/embeddings" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Input) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		type item struct {
			Index     int       `json:"index"`
			Embedding []float64 `json:"embedding"`
		}
		items := make([]item, 0, len(request.Input))
		for index, text := range request.Input {
			items = append(items, item{Index: index, Embedding: productionVectorSyntheticVector(text)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": items})
	}))
	t.Cleanup(server.Close)
	return server
}

// productionVectorFailingServer answers every dispatch with HTTP 500, which is
// internal/embedding's own EMBEDDING_REQUEST_FAILED branch.
func productionVectorFailingServer(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(server.Close)
	return server
}

func productionVectorSettings(origin, fingerprint string) productionKnowledgeEmbedding {
	return productionKnowledgeEmbedding{
		Protocol: embedding.OpenAI,
		Model:    embedding.Model{BaseURL: origin, ModelName: "synthetic-embedding"},
		Identity: fingerprint,
	}
}

func productionVectorSearches(model productionKnowledgeModel) *productionKnowledgeSearches {
	imports := newProductionKnowledgeImports(nil)
	imports.model = model
	return &productionKnowledgeSearches{imports: imports}
}

type productionVectorFixture struct {
	projectPath  string
	contextValue projectsession.Context
	storage      *productionKnowledgeStorage
	index        productionKnowledgeIndex
}

func productionVectorSeed(t *testing.T) productionVectorFixture {
	t.Helper()
	client, projectPath, frozen, _ := productionKnowledgeLane(t)
	imported := productionKnowledgeCall(t, client, productionKnowledgeImportTextName,
		[]any{productionKnowledgeProse, productionKnowledgeNamedFile, projectPath, frozen})
	if imported["success"] != true {
		t.Fatalf("the vector fixture import was refused: %v", imported)
	}
	contextValue, err := projectsession.ParseContext(frozen)
	if err != nil {
		t.Fatalf("frozen context: %v", err)
	}
	storage, err := productionKnowledgeOpenStorage(projectPath)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	t.Cleanup(func() { _ = storage.Close() })
	index, err := productionKnowledgeReadIndex(context.Background(), storage.storage)
	if err != nil {
		t.Fatalf("canonical index: %v", err)
	}
	if len(index.Chunks) != 2 {
		t.Fatalf("the fixture produced %d chunks, want 2", len(index.Chunks))
	}
	return productionVectorFixture{projectPath: projectPath, contextValue: contextValue, storage: storage, index: index}
}

func (f productionVectorFixture) storePath() string {
	return filepath.Join(f.projectPath, projectfs.StorageDirectory, knowledgevector.StoreFileName)
}

func (f productionVectorFixture) search(t *testing.T, searches *productionKnowledgeSearches, query string) []knowledgequery.SearchResult {
	t.Helper()
	value, err := searches.search(context.Background(), f.projectPath, query, 5, nil, nil)
	if err != nil {
		t.Fatalf("kb search: %v", err)
	}
	results, ok := value.([]knowledgequery.SearchResult)
	if !ok {
		t.Fatalf("kb search answered %T, want []knowledgequery.SearchResult", value)
	}
	return results
}

func TestProductionVectorSearchWithoutModelUsesFTS(t *testing.T) {
	fixture := productionVectorSeed(t)
	searches := productionVectorSearches(nil)
	searches.imports.model = nil
	results := fixture.search(t, searches, productionVectorAbsentQuery)
	if len(results) != 0 {
		t.Fatalf("an FTS-only lane answered %d hits for an unmatched query: %+v", len(results), results)
	}
	if _, err := os.Stat(fixture.storePath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a search with no embedding model created a vector store: %v", err)
	}
}

func TestProductionVectorSearchAnswersFromStoreWhenConfigured(t *testing.T) {
	fixture := productionVectorSeed(t)
	server := productionVectorEmbeddingServer(t)
	model := productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(server.URL, "fp-a"),
		client:     productionVectorPolicy(t, server.URL),
	}
	imports := newProductionKnowledgeImports(nil)
	imports.model = model
	processed, failed, err := imports.updateVectors(context.Background(), fixture.storage, fixture.index)
	if err != nil || processed != 2 || failed != 0 {
		t.Fatalf("vector write: processed=%d failed=%d err=%v", processed, failed, err)
	}
	searches := &productionKnowledgeSearches{imports: imports}
	results := fixture.search(t, searches, productionVectorAbsentQuery)
	if len(results) == 0 {
		t.Fatal("an active vector space answered nothing for a query no chunk contains")
	}
}

func TestProductionVectorSearchFallsBackWhenDispatchFails(t *testing.T) {
	fixture := productionVectorSeed(t)
	working := productionVectorEmbeddingServer(t)
	failing := productionVectorFailingServer(t)
	imports := newProductionKnowledgeImports(nil)
	imports.model = productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(working.URL, "fp-a"),
		client:     productionVectorPolicy(t, working.URL),
	}
	if _, _, err := imports.updateVectors(context.Background(), fixture.storage, fixture.index); err != nil {
		t.Fatalf("vector write: %v", err)
	}
	before, err := os.ReadFile(fixture.storePath())
	if err != nil {
		t.Fatal(err)
	}
	// The same model identity, but the dispatch now fails at the transport.
	imports.model = productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(failing.URL, "fp-a"),
		client:     productionVectorPolicy(t, failing.URL),
	}
	searches := &productionKnowledgeSearches{imports: imports}
	results := fixture.search(t, searches, productionVectorAbsentQuery)
	if len(results) != 0 {
		t.Fatalf("a failed embedding dispatch did not fall back to FTS: %+v", results)
	}
	after, err := os.ReadFile(fixture.storePath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a failed search rewrote the vector store")
	}
}

func TestProductionVectorSearchFallsBackWhenEndpointIsOutsideEgress(t *testing.T) {
	fixture := productionVectorSeed(t)
	server := productionVectorEmbeddingServer(t)
	// The model names a loopback endpoint the committed policy does not admit:
	// it must degrade without dispatching.
	model := productionVectorModel{
		configured: true,
		settings:   productionVectorSettings("http://127.0.0.1:1", "fp-a"),
		client:     productionVectorPolicy(t, server.URL),
	}
	searches := productionVectorSearches(model)
	results := fixture.search(t, searches, productionVectorAbsentQuery)
	if len(results) != 0 {
		t.Fatalf("a refused endpoint was dispatched to: %+v", results)
	}
}

func TestProductionVectorSearchFallsBackWhenSettingsFail(t *testing.T) {
	fixture := productionVectorSeed(t)
	searches := productionVectorSearches(productionVectorModel{
		settingsErr: errProductionKnowledgeEmbeddingUnavailable,
	})
	if results := fixture.search(t, searches, productionVectorAbsentQuery); len(results) != 0 {
		t.Fatalf("a settings failure did not fall back to FTS: %+v", results)
	}
}

func TestProductionVectorModelChangeInactivatesTheOldSpace(t *testing.T) {
	ctx := context.Background()
	fixture := productionVectorSeed(t)
	first := productionVectorEmbeddingServer(t)
	second := productionVectorEmbeddingServer(t)
	if first.URL == second.URL {
		t.Fatal("the fixture needs two distinct embedding origins")
	}
	imports := newProductionKnowledgeImports(nil)
	imports.model = productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(first.URL, "fp-a"),
		client:     productionVectorPolicy(t, first.URL),
	}
	if _, _, err := imports.updateVectors(ctx, fixture.storage, fixture.index); err != nil {
		t.Fatalf("first vector write: %v", err)
	}
	imports.model = productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(second.URL, "fp-b"),
		client:     productionVectorPolicy(t, second.URL),
	}
	if _, _, err := imports.updateVectors(ctx, fixture.storage, fixture.index); err != nil {
		t.Fatalf("second vector write: %v", err)
	}
	store, err := knowledgevector.Read(ctx, fixture.storage.storage)
	if err != nil || store == nil {
		t.Fatalf("read: %v %v", store, err)
	}
	if store.ActiveGeneration == nil {
		t.Fatalf("the new model's space should be active: %+v", store.Spaces)
	}
	newGeneration := *store.ActiveGeneration
	for _, space := range store.Spaces {
		if space.ModelFingerprint == "fp-a" && space.Status != "inactive" {
			t.Fatalf("the superseded model's space is still %q: %+v", space.Status, store.Spaces)
		}
		if space.Generation == newGeneration && (space.Status != "active" || space.ModelFingerprint != "fp-b") {
			t.Fatalf("the active space is not the new model's: %+v", space)
		}
	}
	// A query from the old model must not be served from the new model's space.
	imports.model = productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(first.URL, "fp-a"),
		client:     productionVectorPolicy(t, first.URL),
	}
	searches := &productionKnowledgeSearches{imports: imports}
	if results := fixture.search(t, searches, productionVectorAbsentQuery); len(results) != 0 {
		t.Fatalf("the old model was served from the new model's space: %+v", results)
	}
}

func TestProductionVectorRecomputeLeavesAnIncompleteNewSpaceBuilding(t *testing.T) {
	old := int64(1)
	store := &knowledgevector.Store{
		ActiveGeneration: &old,
		Spaces: []knowledgevector.Space{
			{Generation: 1, TableName: knowledgevector.SpaceTableName(1), Status: "active", VectorDimension: 2,
				ModelFingerprint: "fp-a", DistanceMetric: knowledgevector.CosineMetric, CreatedAt: "2026-01-01T00:00:00.000Z"},
			{Generation: 2, TableName: knowledgevector.SpaceTableName(2), Status: "building", VectorDimension: 2,
				ModelFingerprint: "fp-b", DistanceMetric: knowledgevector.CosineMetric, CreatedAt: "2026-01-02T00:00:00.000Z"},
		},
		Tables: []knowledgevector.Table{
			{Name: knowledgevector.SpaceTableName(1), Version: knowledgevector.TableVersion, Dimension: 2,
				Chunks: []knowledgevector.Chunk{{ID: "c1", DocumentID: "d1", Text: "one", Vector: []float32{1, 0}}}},
			{Name: knowledgevector.SpaceTableName(2), Version: knowledgevector.TableVersion, Dimension: 2},
		},
	}
	productionKnowledgeRecomputeVectorSpaces(store, map[string]bool{"c1": true, "c2": true},
		productionKnowledgeEmbedding{Identity: "fp-b"})
	if store.ActiveGeneration != nil {
		t.Fatalf("an incomplete new space was made active: %+v", store.ActiveGeneration)
	}
	for _, space := range store.Spaces {
		switch space.Generation {
		case 1:
			if space.Status != "inactive" {
				t.Fatalf("the other model's space must be inactive, got %q", space.Status)
			}
		case 2:
			if space.Status != "building" {
				t.Fatalf("the incomplete new space must be building, got %q", space.Status)
			}
		}
	}
}

// productionVectorPartialStore is a synthetic store whose one building space
// already carries `keep` of the canonical chunks, so a backfill has real work.
func productionVectorPartialStore(t *testing.T, index productionKnowledgeIndex, fingerprint string, keep int) *knowledgevector.Store {
	t.Helper()
	if keep > len(index.Chunks) {
		keep = len(index.Chunks)
	}
	chunks := make([]knowledgevector.Chunk, 0, keep)
	for position := 0; position < keep; position++ {
		chunk := index.Chunks[position]
		chunks = append(chunks, knowledgevector.Chunk{
			ID: chunk.ID, DocumentID: chunk.DocID, FileName: chunk.FileName, Text: chunk.Text,
			ChapterNumber: chunk.ChapterNumber, CorpusKind: chunk.CorpusKind, Vector: []float32{1, 0, 0},
		})
	}
	name := knowledgevector.SpaceTableName(1)
	return &knowledgevector.Store{
		Spaces: []knowledgevector.Space{{
			Generation: 1, TableName: name, Status: "building", VectorDimension: 3,
			ModelFingerprint: fingerprint, DistanceMetric: knowledgevector.CosineMetric,
			CreatedAt: productionKnowledgeTimestamp(time.Now()),
		}},
		Tables: []knowledgevector.Table{{
			Name: name, Version: knowledgevector.TableVersion, Dimension: 3, Chunks: chunks,
		}},
	}
}

func productionVectorStatus(t *testing.T, fixture productionVectorFixture, searches *productionKnowledgeSearches) map[string]any {
	t.Helper()
	value, err := searches.vectorRebuildStatus(context.Background(), transport.Caller{}, nil,
		fixture.contextValue, []any{fixture.projectPath})
	if err != nil {
		t.Fatalf("vector status: %v", err)
	}
	answer, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("vector status answered %T", value)
	}
	return answer
}

func TestProductionVectorStatusReportsRealCountsFromStore(t *testing.T) {
	ctx := context.Background()
	fixture := productionVectorSeed(t)
	server := productionVectorEmbeddingServer(t)
	model := productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(server.URL, "fp-a"),
		client:     productionVectorPolicy(t, server.URL),
	}
	if err := knowledgevector.Write(ctx, fixture.storage.storage,
		productionVectorPartialStore(t, fixture.index, "fp-a", 1)); err != nil {
		t.Fatalf("partial store: %v", err)
	}
	searches := productionVectorSearches(model)
	answer := productionVectorStatus(t, fixture, searches)
	if answer["embeddingConfigured"] != true || answer["canRebuild"] != true {
		t.Fatalf("a configured model must offer a rebuild: %v", answer)
	}
	if count := productionKnowledgeCount(t, answer["totalChunks"]); count != 2 {
		t.Fatalf("totalChunks=%d, want 2", count)
	}
	if count := productionKnowledgeCount(t, answer["vectorlessCount"]); count != 1 {
		t.Fatalf("vectorlessCount=%d, want 1", count)
	}
	if dimension := productionKnowledgeCount(t, answer["activeVectorDimension"]); dimension != 0 {
		t.Fatalf("an incomplete space must report dimension 0, got %d", dimension)
	}
	// Completing the space through backfill must move every count.
	processed, failed, err := searches.imports.updateVectors(ctx, fixture.storage, fixture.index)
	if err != nil || processed != 1 || failed != 0 {
		t.Fatalf("backfill: processed=%d failed=%d err=%v", processed, failed, err)
	}
	answer = productionVectorStatus(t, fixture, searches)
	if count := productionKnowledgeCount(t, answer["vectorlessCount"]); count != 0 {
		t.Fatalf("vectorlessCount=%d after backfill, want 0", count)
	}
	if dimension := productionKnowledgeCount(t, answer["activeVectorDimension"]); dimension != 3 {
		t.Fatalf("activeVectorDimension=%d after backfill, want 3", dimension)
	}
}

func TestProductionVectorBackfillFillsMissingChunks(t *testing.T) {
	ctx := context.Background()
	fixture := productionVectorSeed(t)
	server := productionVectorEmbeddingServer(t)
	model := productionVectorModel{
		configured: true,
		settings:   productionVectorSettings(server.URL, "fp-a"),
		client:     productionVectorPolicy(t, server.URL),
	}
	if err := knowledgevector.Write(ctx, fixture.storage.storage,
		productionVectorPartialStore(t, fixture.index, "fp-a", 1)); err != nil {
		t.Fatalf("partial store: %v", err)
	}
	searches := productionVectorSearches(model)
	value, err := searches.backfillVectors(ctx, transport.Caller{}, nil, fixture.contextValue, []any{fixture.projectPath})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	answer, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("backfill answered %T", value)
	}
	if answer["success"] != true {
		t.Fatalf("backfill failed: %v", answer)
	}
	if processed := productionKnowledgeCount(t, answer["processed"]); processed != 1 {
		t.Fatalf("backfill processed %d chunks, want 1", processed)
	}
	if failed := productionKnowledgeCount(t, answer["failed"]); failed != 0 {
		t.Fatalf("backfill failed %d chunks, want 0", failed)
	}
	store, err := knowledgevector.Read(ctx, fixture.storage.storage)
	if err != nil || store == nil || store.ActiveGeneration == nil {
		t.Fatalf("backfill left no active space: %+v %v", store, err)
	}
	if status := store.Status(map[string]bool{fixture.index.Chunks[0].ID: true, fixture.index.Chunks[1].ID: true}); !status.Active || status.Vectorless != 0 {
		t.Fatalf("backfill left the space incomplete: %+v", status)
	}
}

func TestProductionVectorBackfillWithoutModelIsAnHonestNoOp(t *testing.T) {
	ctx := context.Background()
	fixture := productionVectorSeed(t)
	searches := productionVectorSearches(nil)
	searches.imports.model = nil
	value, err := searches.backfillVectors(ctx, transport.Caller{}, nil, fixture.contextValue, []any{fixture.projectPath})
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	answer, ok := value.(map[string]any)
	if !ok || answer["success"] != true {
		t.Fatalf("the unconfigured backfill must stay an honest no-op: %v", value)
	}
	if processed := productionKnowledgeCount(t, answer["processed"]); processed != 0 {
		t.Fatalf("the unconfigured backfill claimed %d vectors", processed)
	}
	if _, err := os.Stat(fixture.storePath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the unconfigured backfill created a vector store: %v", err)
	}
}

// TestProductionVectorEmbeddingFingerprintIsModelIdentityOnly proves the model
// fingerprint changes with the model identity and never with the credential.
func TestProductionVectorEmbeddingFingerprintIsModelIdentityOnly(t *testing.T) {
	first := productionKnowledgeEmbeddingFingerprint(embedding.OpenAI, "https://api.example.com", "text-embedding-3-small")
	if first == "" || len(first) != 64 {
		t.Fatalf("the fingerprint is not a sha256 hex: %q", first)
	}
	if repeat := productionKnowledgeEmbeddingFingerprint(embedding.OpenAI, "https://api.example.com", "text-embedding-3-small"); repeat != first {
		t.Fatal("the same model identity produced two fingerprints")
	}
	if other := productionKnowledgeEmbeddingFingerprint(embedding.OpenAI, "https://api.example.com", "text-embedding-3-large"); other == first {
		t.Fatal("a different model name did not change the fingerprint")
	}
	if other := productionKnowledgeEmbeddingFingerprint(embedding.Gemini, "https://api.example.com", "text-embedding-3-small"); other == first {
		t.Fatal("a different protocol did not change the fingerprint")
	}
	if other := productionKnowledgeEmbeddingFingerprint(embedding.OpenAI, "https://other.example.com", "text-embedding-3-small"); other == first {
		t.Fatal("a different endpoint did not change the fingerprint")
	}
}

// TestProductionVectorEmbeddingSettingsResolveCommittedModel proves the profile
// resolution reads the committed models.json/config.json pair and returns the
// unsealed key, and that a profile without the embedding purpose is not selected.
func TestProductionVectorEmbeddingSettingsResolveCommittedModel(t *testing.T) {
	root := t.TempDir()
	models := `[{"id":"emb-1","provider":"openai","protocol":"openai","baseUrl":"https://api.example.com",` +
		`"modelName":"text-embedding-3-small","apiKey":"synthetic-key","purposes":["embedding"]},` +
		`{"id":"gen-1","provider":"openai","protocol":"openai","baseUrl":"https://api.example.com",` +
		`"modelName":"synthetic-chat","apiKey":"synthetic-key","purposes":["generation"]}]`
	if err := os.WriteFile(filepath.Join(root, "models.json"), []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(`{"defaultEmbeddingModelId":"emb-1"}`), 0600); err != nil {
		t.Fatal(err)
	}
	service, err := globalconfig.New(root)
	if err != nil {
		t.Fatalf("global config: %v", err)
	}
	t.Cleanup(func() { _ = service.Close() })
	settings, configured, err := productionKnowledgeEmbeddingSettings(service)
	if err != nil || !configured {
		t.Fatalf("the committed embedding model was not resolved: configured=%v err=%v", configured, err)
	}
	if settings.Protocol != embedding.OpenAI || settings.Model.ModelName != "text-embedding-3-small" ||
		settings.Model.APIKey != "synthetic-key" || settings.Identity == "" {
		t.Fatalf("the resolved model is wrong: %+v", settings)
	}

	// A config whose only profile is a generation profile selects nothing.
	generationOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(generationOnly, "models.json"), []byte(`[{"id":"gen-1","provider":"openai",`+
		`"protocol":"openai","baseUrl":"https://api.example.com","modelName":"synthetic-chat",`+
		`"apiKey":"synthetic-key","purposes":["generation"]}]`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generationOnly, "config.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	other, err := globalconfig.New(generationOnly)
	if err != nil {
		t.Fatalf("global config: %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })
	if _, configured, err := productionKnowledgeEmbeddingSettings(other); err != nil || configured {
		t.Fatalf("a generation-only profile was selected as an embedding model: configured=%v err=%v", configured, err)
	}
}

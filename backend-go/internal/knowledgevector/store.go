//go:build linux

// Package knowledgevector is the Go-native embedding-space store of the
// knowledge base. It keeps per-chunk float32 vectors beside the canonical
// documents/chunks snapshot instead of inside it, because a real space table of
// thousands of 1536-dimension rows cannot fit knowledgedomain's hard cell budget
// (200000 cells); the canonical snapshot therefore stays exactly what it was and
// this file is the only place vectors live.
//
// The store document is one knowledgedomain.Table-shaped table per generation
// plus the same registry shape the canonical snapshot uses
// (version/activeGeneration/spaces[] with generation, tableName, status,
// vectorDimension, modelFingerprint, distanceMetric and createdAt). Every write
// is a temporary file plus rename; every read is bounded and re-validated. A
// separate embedding-spaces.json projection gives the browser the registry
// without exposing vectors.
package knowledgevector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
	"strconv"
	"syscall"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

const (
	// StoreFileName is the authoritative vector store, written beside
	// knowledge-snapshot.json in {projectPath}/.ai-novel.
	StoreFileName = "knowledge-vectors.json"
	// RegistryFileName is the browser-readable registry projection.
	RegistryFileName = "embedding-spaces.json"
	// FormatVersion is the only store format this package reads or writes.
	FormatVersion = 1
	// TableVersion is the version of every space table this package writes.
	TableVersion uint64 = 1
	// MaxStoreBytes bounds one whole store document.
	MaxStoreBytes = 512 << 20
	// MaxChunks bounds the rows of one space table.
	MaxChunks = 10000
	// MaxDimension is the ceiling SearchWithAdmittedVector enforces.
	MaxDimension = 4096
	// MaxSpaces mirrors knowledgedomain.HardMaxTables.
	MaxSpaces = 32
	// CosineMetric is the distance metric this package writes. Cosine is
	// scale-invariant, so a provider that returns unnormalised vectors still
	// ranks by direction rather than by norm.
	CosineMetric = "cosine"
)

// Symbolic failures. They never carry a path, a vector, a fingerprint or any
// author text, so a caller may log them verbatim.
var (
	ErrStoreUnavailable = errors.New("KNOWLEDGE_VECTOR_STORE_UNAVAILABLE")
	ErrStoreInvalid     = errors.New("KNOWLEDGE_VECTOR_STORE_INVALID")
	ErrStoreTooLarge    = errors.New("KNOWLEDGE_VECTOR_STORE_TOO_LARGE")
)

// Space is one registered embedding space.
type Space struct {
	Generation       int64
	TableName        string
	Status           string
	VectorDimension  int
	ModelFingerprint string
	DistanceMetric   string
	CreatedAt        string
}

// Chunk is one vectorised chunk: the canonical chunk identity, the canonical
// text (so a hit can be re-proved against the snapshot) plus the vector.
type Chunk struct {
	ID            string
	DocumentID    string
	FileName      string
	Text          string
	ChapterNumber *int64
	CorpusKind    string
	Vector        []float32
}

// Table is one generation's space table.
type Table struct {
	Name      string
	Version   uint64
	Dimension int
	Chunks    []Chunk
}

// Store is the decoded store document.
type Store struct {
	ActiveGeneration *int64
	Spaces           []Space
	Tables           []Table
}

// Status is the truthful summary of the store against a canonical chunk-id set.
type Status struct {
	HasSpace   bool
	Active     bool
	Generation int64
	Dimension  int
	Vectorless int
}

type wireStore struct {
	Version          *json.Number `json:"version"`
	ActiveGeneration *json.Number `json:"activeGeneration"`
	Spaces           []wireSpace  `json:"spaces"`
	Tables           []wireTable  `json:"tables"`
}

type wireSpace struct {
	Generation       *json.Number `json:"generation"`
	TableName        string       `json:"tableName"`
	Status           string       `json:"status"`
	VectorDimension  *json.Number `json:"vectorDimension"`
	ModelFingerprint string       `json:"modelFingerprint"`
	DistanceMetric   string       `json:"distanceMetric"`
	CreatedAt        string       `json:"createdAt"`
}

type wireTable struct {
	Name    string                 `json:"name"`
	Version *json.Number           `json:"version"`
	Schema  []knowledgedomain.Field `json:"schema"`
	Records []wireChunk            `json:"records"`
}

type wireChunk struct {
	ID            string       `json:"id"`
	DocumentID    string       `json:"docId"`
	FileName      string       `json:"fileName"`
	Text          string       `json:"text"`
	ChapterNumber *json.Number `json:"chapterNumber"`
	CorpusKind    string       `json:"corpusKind"`
	Vector        []float32    `json:"vector"`
}

// TableSchema is the exact schema of every space table this package writes. It
// is exported so the writer's own assembly and the reader's check cannot drift.
func TableSchema(dimension int) []knowledgedomain.Field {
	return []knowledgedomain.Field{
		{Name: "id", Type: "utf8"},
		{Name: "docId", Type: "utf8"},
		{Name: "fileName", Type: "utf8"},
		{Name: "text", Type: "utf8"},
		{Name: "chapterNumber", Type: "int32", Nullable: true},
		{Name: "corpusKind", Type: "utf8"},
		{Name: "vector", Type: "fixed_size_list", ListSize: dimension,
			Children: []knowledgedomain.Field{{Name: "item", Type: "float32"}}},
	}
}

// SpaceTableName is the domain's chunks__space_<generation> rule.
func SpaceTableName(generation int64) string {
	return "chunks__space_" + formatGeneration(generation)
}

// Read decodes the store, or returns (nil, nil) when this project has none.
func Read(ctx context.Context, root *os.Root) (*Store, error) {
	if root == nil || ctx == nil {
		return nil, ErrStoreUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw, _, missing, err := readFile(ctx, root, StoreFileName, MaxStoreBytes)
	if err != nil {
		return nil, err
	}
	if missing {
		return nil, nil
	}
	return decodeStore(ctx, raw)
}

// Write commits the store and its registry projection. The store is validated
// by decoding it before it is written, so a row this reader could not admit is
// never written. The authoritative file goes first: a registry projection that
// failed to install can never make a stale vector table look current.
func Write(ctx context.Context, root *os.Root, store *Store) error {
	if root == nil || store == nil || ctx == nil {
		return ErrStoreUnavailable
	}
	if err := validateStore(ctx, store); err != nil {
		return err
	}
	body, err := json.Marshal(encodeStore(store))
	if err != nil {
		return ErrStoreInvalid
	}
	if _, err := decodeStore(ctx, body); err != nil {
		return err
	}
	if err := writeAtomic(root, StoreFileName, body); err != nil {
		return err
	}
	registry, err := json.Marshal(encodeRegistry(store))
	if err != nil {
		return ErrStoreInvalid
	}
	return writeAtomic(root, RegistryFileName, registry)
}

// Table returns one space table by name.
func (s *Store) Table(name string) (*Table, bool) {
	if s == nil {
		return nil, false
	}
	for i := range s.Tables {
		if s.Tables[i].Name == name {
			return &s.Tables[i], true
		}
	}
	return nil, false
}

// Space returns one registered space by generation.
func (s *Store) Space(generation int64) (Space, bool) {
	if s == nil {
		return Space{}, false
	}
	for _, space := range s.Spaces {
		if space.Generation == generation {
			return space, true
		}
	}
	return Space{}, false
}

// NextGeneration is the generation a newly created space must use. Generations
// start at 1: the execution envelope refuses generation <= 0, so a space numbered
// 0 could be written but never queried.
func (s *Store) NextGeneration() int64 {
	next := int64(1)
	if s == nil {
		return next
	}
	for _, space := range s.Spaces {
		if space.Generation >= next {
			next = space.Generation + 1
		}
	}
	return next
}

// Selected is the space a read would use: the declared active space when the
// pointer names one, else the greatest-generation building space. It mirrors
// knowledgequery.View's own selection.
func (s *Store) Selected() (Space, *Table, bool) {
	if s == nil {
		return Space{}, nil, false
	}
	if s.ActiveGeneration != nil {
		if space, ok := s.Space(*s.ActiveGeneration); ok && space.Status == "active" {
			if table, ok := s.Table(space.TableName); ok {
				return space, table, true
			}
		}
	}
	best := -1
	for i := range s.Spaces {
		if s.Spaces[i].Status != "building" {
			continue
		}
		if best < 0 || s.Spaces[i].Generation > s.Spaces[best].Generation {
			best = i
		}
	}
	if best < 0 {
		return Space{}, nil, false
	}
	space := s.Spaces[best]
	table, ok := s.Table(space.TableName)
	if !ok {
		return Space{}, nil, false
	}
	return space, table, true
}

// Status counts the canonical chunk ids the selected space does not carry and
// reports the active dimension only when the declared active space really
// covers every canonical chunk. A declared-active space that lost rows to a
// later canonical commit therefore reports as incomplete, never as an index.
func (s *Store) Status(chunkIDs map[string]bool) Status {
	space, table, ok := s.Selected()
	if !ok {
		return Status{Vectorless: len(chunkIDs)}
	}
	present := make(map[string]bool, len(table.Chunks))
	for _, chunk := range table.Chunks {
		present[chunk.ID] = true
	}
	missing := 0
	for id := range chunkIDs {
		if !present[id] {
			missing++
		}
	}
	status := Status{HasSpace: true, Generation: space.Generation, Vectorless: missing}
	if space.Status == "active" && missing == 0 {
		status.Active = true
		status.Dimension = space.VectorDimension
	}
	return status
}

// Missing returns the canonical chunk ids the selected space does not carry,
// sorted, so a backfill can embed exactly those.
func (s *Store) Missing(chunkIDs map[string]bool) []string {
	_, table, ok := s.Selected()
	if !ok {
		out := make([]string, 0, len(chunkIDs))
		for id := range chunkIDs {
			out = append(out, id)
		}
		sortStrings(out)
		return out
	}
	present := make(map[string]bool, len(table.Chunks))
	for _, chunk := range table.Chunks {
		present[chunk.ID] = true
	}
	out := make([]string, 0, len(chunkIDs))
	for id := range chunkIDs {
		if !present[id] {
			out = append(out, id)
		}
	}
	sortStrings(out)
	return out
}

func validateStore(ctx context.Context, store *Store) error {
	if store == nil {
		return ErrStoreInvalid
	}
	if len(store.Spaces) > MaxSpaces {
		return ErrStoreInvalid
	}
	seenGeneration := make(map[int64]bool, len(store.Spaces))
	seenTable := make(map[string]bool, len(store.Spaces))
	active := int64(0)
	activeSeen := false
	for _, space := range store.Spaces {
		if err := ctx.Err(); err != nil {
			return err
		}
		if space.Generation < 0 || seenGeneration[space.Generation] || seenTable[space.TableName] ||
			space.TableName != SpaceTableName(space.Generation) || !validStatus(space.Status) ||
			space.VectorDimension < 1 || space.VectorDimension > MaxDimension ||
			space.ModelFingerprint == "" || len(space.ModelFingerprint) > 1024 ||
			space.CreatedAt == "" || len(space.CreatedAt) > 128 || !validMetric(space.DistanceMetric) {
			return ErrStoreInvalid
		}
		seenGeneration[space.Generation] = true
		seenTable[space.TableName] = true
		if space.Status == "active" {
			if activeSeen {
				return ErrStoreInvalid
			}
			activeSeen = true
			active = space.Generation
		}
	}
	if (store.ActiveGeneration != nil) != activeSeen || (activeSeen && *store.ActiveGeneration != active) {
		return ErrStoreInvalid
	}
	seenTableRows := make(map[string]bool, len(store.Tables))
	for _, table := range store.Tables {
		if err := ctx.Err(); err != nil {
			return err
		}
		if table.Name == "" || seenTableRows[table.Name] {
			return ErrStoreInvalid
		}
		seenTableRows[table.Name] = true
		registered := false
		for _, candidate := range store.Spaces {
			if candidate.TableName != table.Name {
				continue
			}
			if candidate.VectorDimension != table.Dimension {
				return ErrStoreInvalid
			}
			registered = true
			break
		}
		if !registered || table.Version != TableVersion ||
			table.Dimension < 1 || table.Dimension > MaxDimension || len(table.Chunks) > MaxChunks {
			return ErrStoreInvalid
		}
		seen := make(map[string]bool, len(table.Chunks))
		for _, chunk := range table.Chunks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if chunk.ID == "" || chunk.DocumentID == "" || seen[chunk.ID] ||
				!utf8.ValidString(chunk.ID+chunk.DocumentID+chunk.FileName+chunk.Text) ||
				len(chunk.Vector) != table.Dimension {
				return ErrStoreInvalid
			}
			seen[chunk.ID] = true
		}
	}
	return nil
}

func decodeStore(ctx context.Context, raw []byte) (*Store, error) {
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, ErrStoreInvalid
	}
	var wire wireStore
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&wire); err != nil {
		return nil, ErrStoreInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, ErrStoreInvalid
	}
	if wire.Version == nil {
		return nil, ErrStoreInvalid
	}
	version, err := jsonInt(wire.Version)
	if err != nil || version != FormatVersion || len(wire.Spaces) > MaxSpaces {
		return nil, ErrStoreInvalid
	}
	store := &Store{Spaces: make([]Space, 0, len(wire.Spaces))}
	for _, space := range wire.Spaces {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		generation, generationErr := jsonInt(space.Generation)
		dimension, dimensionErr := jsonInt(space.VectorDimension)
		if generationErr != nil || dimensionErr != nil {
			return nil, ErrStoreInvalid
		}
		store.Spaces = append(store.Spaces, Space{
			Generation: generation, TableName: space.TableName, Status: space.Status,
			VectorDimension: int(dimension), ModelFingerprint: space.ModelFingerprint,
			DistanceMetric: space.DistanceMetric, CreatedAt: space.CreatedAt,
		})
	}
	if wire.ActiveGeneration != nil {
		generation, err := jsonInt(wire.ActiveGeneration)
		if err != nil {
			return nil, ErrStoreInvalid
		}
		store.ActiveGeneration = &generation
	}
	store.Tables = make([]Table, 0, len(wire.Tables))
	for _, table := range wire.Tables {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		version, err := jsonInt(table.Version)
		if err != nil {
			return nil, ErrStoreInvalid
		}
		dimension := tableVectorDimension(table.Schema)
		if dimension < 1 || dimension > MaxDimension || !schemaMatches(table.Schema, dimension) {
			return nil, ErrStoreInvalid
		}
		decoded := Table{Name: table.Name, Version: uint64(version), Dimension: dimension,
			Chunks: make([]Chunk, 0, len(table.Records))}
		for _, record := range table.Records {
			chunk := Chunk{ID: record.ID, DocumentID: record.DocumentID, FileName: record.FileName,
				Text: record.Text, CorpusKind: record.CorpusKind, Vector: record.Vector}
			if record.ChapterNumber != nil {
				number, err := jsonInt(record.ChapterNumber)
				if err != nil {
					return nil, ErrStoreInvalid
				}
				chunk.ChapterNumber = &number
			}
			decoded.Chunks = append(decoded.Chunks, chunk)
		}
		store.Tables = append(store.Tables, decoded)
	}
	if err := validateStore(ctx, store); err != nil {
		return nil, err
	}
	return store, nil
}

func encodeStore(store *Store) wireStore {
	wire := wireStore{Version: jsonNumber(FormatVersion), Spaces: make([]wireSpace, 0, len(store.Spaces)), Tables: make([]wireTable, 0, len(store.Tables))}
	if store.ActiveGeneration != nil {
		wire.ActiveGeneration = jsonNumber(*store.ActiveGeneration)
	}
	for _, space := range store.Spaces {
		wire.Spaces = append(wire.Spaces, wireSpace{
			Generation: jsonNumber(space.Generation), TableName: space.TableName, Status: space.Status,
			VectorDimension: jsonNumber(int64(space.VectorDimension)), ModelFingerprint: space.ModelFingerprint,
			DistanceMetric: space.DistanceMetric, CreatedAt: space.CreatedAt,
		})
	}
	for _, table := range store.Tables {
		encoded := wireTable{Name: table.Name, Version: jsonNumber(int64(table.Version)), Schema: TableSchema(table.Dimension), Records: make([]wireChunk, 0, len(table.Chunks))}
		for _, chunk := range table.Chunks {
			record := wireChunk{ID: chunk.ID, DocumentID: chunk.DocumentID, FileName: chunk.FileName,
				Text: chunk.Text, CorpusKind: chunk.CorpusKind, Vector: chunk.Vector}
			if chunk.ChapterNumber != nil {
				record.ChapterNumber = jsonNumber(*chunk.ChapterNumber)
			}
			encoded.Records = append(encoded.Records, record)
		}
		wire.Tables = append(wire.Tables, encoded)
	}
	return wire
}

func encodeRegistry(store *Store) any {
	type registrySpace struct {
		Generation       int64  `json:"generation"`
		TableName        string `json:"tableName"`
		Status           string `json:"status"`
		VectorDimension  int    `json:"vectorDimension"`
		ModelFingerprint string `json:"modelFingerprint"`
		DistanceMetric   string `json:"distanceMetric"`
		CreatedAt        string `json:"createdAt"`
	}
	spaces := make([]registrySpace, 0, len(store.Spaces))
	for _, space := range store.Spaces {
		spaces = append(spaces, registrySpace{
			Generation: space.Generation, TableName: space.TableName, Status: space.Status,
			VectorDimension: space.VectorDimension, ModelFingerprint: space.ModelFingerprint,
			DistanceMetric: space.DistanceMetric, CreatedAt: space.CreatedAt,
		})
	}
	return struct {
		Version          int             `json:"version"`
		ActiveGeneration *int64          `json:"activeGeneration"`
		Spaces           []registrySpace `json:"spaces"`
	}{Version: FormatVersion, ActiveGeneration: store.ActiveGeneration, Spaces: spaces}
}

// readFile reads one bounded regular file through the root, refusing a symlink
// or a special file, and returns the FileInfo of the exact inode it read.
func readFile(ctx context.Context, root *os.Root, name string, limit int64) ([]byte, os.FileInfo, bool, error) {
	listed, err := root.Lstat(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, true, nil
	}
	if err != nil || listed.Mode()&os.ModeSymlink != 0 || !listed.Mode().IsRegular() || listed.Size() > limit {
		return nil, nil, false, ErrStoreUnavailable
	}
	file, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, false, ErrStoreUnavailable
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !os.SameFile(listed, info) || info.Size() > limit {
		return nil, nil, false, ErrStoreUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, nil, false, ErrStoreUnavailable
	}
	if int64(len(raw)) > limit {
		return nil, nil, false, ErrStoreTooLarge
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, false, err
	}
	return raw, info, false, nil
}

func writeAtomic(root *os.Root, name string, body []byte) error {
	token := make([]byte, 16)
	if _, err := rand.Read(token); err != nil {
		return ErrStoreUnavailable
	}
	temporary := name + "." + hex.EncodeToString(token) + ".tmp"
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrStoreUnavailable
	}
	installed := false
	defer func() {
		if !installed {
			_ = root.Remove(temporary)
		}
	}()
	if _, err := file.Write(body); err != nil {
		_ = file.Close()
		return ErrStoreUnavailable
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return ErrStoreUnavailable
	}
	if err := file.Close(); err != nil {
		return ErrStoreUnavailable
	}
	if err := root.Rename(temporary, name); err != nil {
		return ErrStoreUnavailable
	}
	installed = true
	directory, err := root.OpenFile(".", os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return ErrStoreUnavailable
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func tableVectorDimension(schema []knowledgedomain.Field) int {
	for _, field := range schema {
		if field.Name == "vector" && field.Type == "fixed_size_list" {
			return field.ListSize
		}
	}
	return 0
}

func schemaMatches(schema []knowledgedomain.Field, dimension int) bool {
	expected := TableSchema(dimension)
	if len(schema) != len(expected) {
		return false
	}
	for i := range expected {
		actual := schema[i]
		if actual.Name != expected[i].Name || actual.Type != expected[i].Type ||
			actual.Nullable != expected[i].Nullable || actual.ListSize != expected[i].ListSize ||
			actual.Metadata || len(actual.Children) != len(expected[i].Children) {
			return false
		}
		for j := range expected[i].Children {
			child := actual.Children[j]
			want := expected[i].Children[j]
			if child.Name != want.Name || child.Type != want.Type || child.ListSize != want.ListSize ||
				child.Nullable != want.Nullable || child.Metadata || len(child.Children) != 0 {
				return false
			}
		}
	}
	return true
}

func validStatus(status string) bool {
	return status == "active" || status == "inactive" || status == "building"
}

func validMetric(metric string) bool {
	return metric == "l2" || metric == "cosine" || metric == "dot"
}

// jsonInt admits exactly one non-negative safe integer lexeme from a
// UseNumber-decoded document. A fractional, negative, empty or oversized value
// is refused; this is the same rule knowledgedomain applies to a space field.
func jsonInt(value *json.Number) (int64, error) {
	if value == nil {
		return 0, ErrStoreInvalid
	}
	text := string(*value)
	if len(text) == 0 || len(text) > 128 {
		return 0, ErrStoreInvalid
	}
	parsed, err := strconv.ParseInt(text, 10, 64)
	if err != nil || parsed < 0 {
		return 0, ErrStoreInvalid
	}
	return parsed, nil
}

func jsonNumber(value int64) *json.Number {
	number := json.Number(strconv.FormatInt(value, 10))
	return &number
}

func formatGeneration(generation int64) string {
	return strconv.FormatInt(generation, 10)
}

func sortStrings(values []string) {
	sort.Strings(values)
}

package knowledgedomain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

const prefix = "KNOWLEDGE_DOMAIN_"
const maxSafeInteger int64 = 9007199254740991

func failure(code string) error { return errors.New(prefix + code) }
func checkContext(ctx context.Context) error {
	if ctx == nil {
		return failure("CONTEXT_REQUIRED")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func normalizedBudget(b Budget) (Budget, error) {
	values := []*int{&b.MaxTables, &b.MaxRows, &b.MaxCells, &b.MaxTextBytes}
	hard := []int{HardMaxTables, HardMaxRows, HardMaxCells, HardMaxTextBytes}
	for i, v := range values {
		if *v <= 0 || *v > hard[i] {
			return Budget{}, failure("BUDGET_INVALID")
		}
	}
	return b, nil
}

type meter struct {
	budget            Budget
	rows, cells, text int
}

func (m *meter) textBytes(s string) error {
	if len(s) > m.budget.MaxTextBytes-m.text {
		return failure("TEXT_LIMIT")
	}
	m.text += len(s)
	if !utf8.ValidString(s) {
		return failure("TEXT_INVALID")
	}
	return nil
}
func (m *meter) cellCount(n int) error {
	if n > m.budget.MaxCells-m.cells {
		return failure("CELL_LIMIT")
	}
	m.cells += n
	return nil
}

func spaceTable(name string) bool {
	const stem = "chunks__space_"
	if !strings.HasPrefix(name, stem) || len(name) == len(stem) {
		return false
	}
	for _, c := range name[len(stem):] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
func allowedField(name string, documents bool) bool {
	if name == "id" || name == "fileName" || name == "importedAt" || name == "corpusKind" {
		return true
	}
	if documents {
		return name == "chunkCount" || name == "filePath"
	}
	switch name {
	case "docId", "chapterNumber", "chapterTitle", "text", "vector", "chunkIndex", "totalChunks":
		return true
	}
	return false
}
func numericField(name string) bool {
	return name == "chunkCount" || name == "chunkIndex" || name == "totalChunks" || name == "chapterNumber"
}

// The deliberate strict-schema lane supports only the original known scalar
// columns and fixed-size float32 vectors. Other Arrow shapes are rejected even
// when a loose JavaScript row-only check would have accepted their values.
func schemaShape(ctx context.Context, table Table, m *meter) error {
	if table.Metadata {
		return failure("SCHEMA_METADATA_UNSUPPORTED")
	}
	maxFields := 11
	if table.Name == "documents" {
		maxFields = 6
	}
	if len(table.Schema) > maxFields {
		return failure("SCHEMA_INVALID")
	}
	seen := make(map[string]bool, len(table.Schema))
	for _, field := range table.Schema {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if field.Metadata {
			return failure("SCHEMA_METADATA_UNSUPPORTED")
		}
		if err := m.textBytes(field.Name); err != nil {
			return err
		}
		if err := m.textBytes(field.Type); err != nil {
			return err
		}
		if seen[field.Name] {
			return failure("DUPLICATE_FIELD")
		}
		seen[field.Name] = true
		if !allowedField(field.Name, table.Name == "documents") {
			return failure("UNKNOWN_FIELD")
		}
		if field.Name == "vector" {
			if field.Type != "fixed_size_list" || field.ListSize < 1 || len(field.Children) != 1 {
				return failure("SCHEMA_TYPE_UNSUPPORTED")
			}
			child := field.Children[0]
			if child.Metadata {
				return failure("SCHEMA_METADATA_UNSUPPORTED")
			}
			if err := m.textBytes(child.Name); err != nil {
				return err
			}
			if err := m.textBytes(child.Type); err != nil {
				return err
			}
			if child.Type != "float32" || child.ListSize != 0 || len(child.Children) != 0 {
				return failure("SCHEMA_TYPE_UNSUPPORTED")
			}
		} else {
			if field.ListSize != 0 || len(field.Children) != 0 {
				return failure("SCHEMA_TYPE_UNSUPPORTED")
			}
			if numericField(field.Name) {
				if field.Type != "int32" && field.Type != "float64" {
					return failure("SCHEMA_TYPE_UNSUPPORTED")
				}
			} else if field.Type != "utf8" {
				return failure("SCHEMA_TYPE_UNSUPPORTED")
			}
		}
	}
	required := []string{"id", "docId", "fileName", "text", "chunkIndex", "totalChunks", "importedAt"}
	if table.Name == "documents" {
		required = []string{"id", "fileName", "importedAt", "chunkCount", "filePath"}
	}
	for _, name := range required {
		if !seen[name] {
			return failure("MISSING_FIELD")
		}
	}
	return nil
}

// Preflight resource accounting precedes row indexes, relationship maps, numeric
// parsing, and registry decoding. No caller-owned data is mutated or copied.
func preflight(ctx context.Context, snapshot Snapshot, b Budget) error {
	if len(snapshot.Tables) > b.MaxTables {
		return failure("TABLE_LIMIT")
	}
	m := meter{budget: b}
	if len(snapshot.Registry) > b.MaxTextBytes {
		return failure("TEXT_LIMIT")
	}
	if !utf8.Valid(snapshot.Registry) {
		return failure("TEXT_INVALID")
	}
	m.text = len(snapshot.Registry)
	names := make(map[string]bool, len(snapshot.Tables))
	for _, table := range snapshot.Tables {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if err := m.textBytes(table.Name); err != nil {
			return err
		}
		if (table.Name != "documents" && table.Name != "chunks" && !spaceTable(table.Name)) || names[table.Name] {
			return failure("UNKNOWN_TABLE")
		}
		names[table.Name] = true
		if len(table.Records) > b.MaxRows-m.rows {
			return failure("ROW_LIMIT")
		}
		m.rows += len(table.Records)
		if err := schemaShape(ctx, table, &m); err != nil {
			return err
		}
		for _, row := range table.Records {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if len(row) != len(table.Schema) {
				return failure("ROW_KEYS_INVALID")
			}
			if err := m.cellCount(len(row)); err != nil {
				return err
			}
			for key, value := range row {
				if err := m.textBytes(key); err != nil {
					return err
				}
				switch value := value.(type) {
				case string:
					if err := m.textBytes(value); err != nil {
						return err
					}
				case []any:
					if err := m.cellCount(len(value)); err != nil {
						return err
					}
					for _, element := range value {
						if err := checkContext(ctx); err != nil {
							return err
						}
						if text, ok := element.(string); ok {
							if err := m.textBytes(text); err != nil {
								return err
							}
						}
					}
				}
			}
		}
	}
	return nil
}

func finiteNumber(value any) (float64, bool) {
	var n float64
	switch value := value.(type) {
	case json.Number:
		text := string(value)
		if len(text) == 0 || len(text) > 128 || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) || !json.Valid([]byte(text)) {
			return 0, false
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return 0, false
		}
		n = parsed
	case float64:
		n = value
	case float32:
		n = float64(value)
	case int:
		n = float64(value)
	case int8:
		n = float64(value)
	case int16:
		n = float64(value)
	case int32:
		n = float64(value)
	case int64:
		n = float64(value)
	case uint:
		n = float64(value)
	case uint8:
		n = float64(value)
	case uint16:
		n = float64(value)
	case uint32:
		n = float64(value)
	case uint64:
		n = float64(value)
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}
func safeInteger(value any) (int64, bool) {
	n, ok := finiteNumber(value)
	if !ok || math.Trunc(n) != n || n < -float64(maxSafeInteger) || n > float64(maxSafeInteger) {
		return 0, false
	}
	// Use the same finite binary64 Number semantics as the original JavaScript
	// safe-integer checks, including decimal/exponent rounding on JSON decode.
	return int64(n), true
}
func validateCell(ctx context.Context, field Field, value any) error {
	if value == nil {
		if !field.Nullable {
			return failure("NULL_INVALID")
		}
		return nil
	}
	switch field.Type {
	case "utf8":
		if _, ok := value.(string); !ok {
			return failure("CELL_TYPE_INVALID")
		}
	case "int32":
		n, ok := safeInteger(value)
		if !ok || n < math.MinInt32 || n > math.MaxInt32 {
			return failure("CELL_TYPE_INVALID")
		}
	case "float64":
		if _, ok := finiteNumber(value); !ok {
			return failure("CELL_TYPE_INVALID")
		}
	case "fixed_size_list":
		values, ok := value.([]any)
		if !ok || len(values) != field.ListSize {
			return failure("VECTOR_INVALID")
		}
		for _, element := range values {
			if err := checkContext(ctx); err != nil {
				return err
			}
			n, ok := finiteNumber(element)
			// Null vector elements are never finite, even if Arrow child nullable.
			if !ok || math.IsInf(float64(float32(n)), 0) {
				return failure("VECTOR_INVALID")
			}
		}
	default:
		return failure("SCHEMA_TYPE_UNSUPPORTED")
	}
	return nil
}

type checkedTable struct {
	table           *Table
	rows            map[string]map[string]any
	vectorDimension int
}

// Validate implements the original migration snapshot's domain relationships,
// except table hashes, with bounded stricter schema/cell validation. Any error
// returns a zero Summary and a symbolic code without author text or row IDs.
func Validate(ctx context.Context, snapshot Snapshot, budget Budget) (Summary, error) {
	if err := checkContext(ctx); err != nil {
		return Summary{}, err
	}
	b, err := normalizedBudget(budget)
	if err != nil {
		return Summary{}, err
	}
	if err := preflight(ctx, snapshot, b); err != nil {
		return Summary{}, err
	}
	result, err := validateDomain(ctx, snapshot)
	if err != nil {
		return Summary{}, err
	}
	if err := checkContext(ctx); err != nil {
		return Summary{}, err
	}
	return result, nil
}
func validateDomain(ctx context.Context, snapshot Snapshot) (Summary, error) {
	tables := make(map[string]checkedTable, len(snapshot.Tables))
	for i := range snapshot.Tables {
		table := &snapshot.Tables[i]
		checked := checkedTable{table: table, rows: make(map[string]map[string]any, len(table.Records))}
		for _, field := range table.Schema {
			if field.Name == "vector" {
				checked.vectorDimension = field.ListSize
			}
		}
		for _, row := range table.Records {
			if err := checkContext(ctx); err != nil {
				return Summary{}, err
			}
			for _, field := range table.Schema {
				value, exists := row[field.Name]
				if !exists {
					return Summary{}, failure("ROW_KEYS_INVALID")
				}
				if err := validateCell(ctx, field, value); err != nil {
					return Summary{}, err
				}
			}
			id, ok := row["id"].(string)
			if !ok || id == "" || checked.rows[id] != nil {
				return Summary{}, failure("DUPLICATE_ID")
			}
			checked.rows[id] = row
			textFields := []string{"docId", "fileName", "text", "importedAt"}
			if table.Name == "documents" {
				textFields = []string{"fileName", "importedAt", "filePath"}
			}
			for _, name := range textFields {
				if _, ok := row[name].(string); !ok {
					return Summary{}, failure("INVALID_TEXT")
				}
			}
			if value, exists := row["corpusKind"]; exists {
				if value != "reference" && value != "project-knowledge" && value != "unknown" {
					return Summary{}, failure("CORPUS_UNSUPPORTED")
				}
			}
		}
		tables[table.Name] = checked
	}
	documents, chunks := tables["documents"].rows, tables["chunks"].rows
	counts := make(map[string]int, len(documents))
	indexes := make(map[string]map[int64]bool, len(documents))
	for _, chunk := range chunks {
		if err := checkContext(ctx); err != nil {
			return Summary{}, err
		}
		docID := chunk["docId"].(string)
		if documents[docID] == nil {
			return Summary{}, failure("ORPHAN_CHUNK")
		}
		counts[docID]++
	}
	for id, document := range documents {
		if err := checkContext(ctx); err != nil {
			return Summary{}, err
		}
		count, ok := safeInteger(document["chunkCount"])
		if !ok || count != int64(counts[id]) {
			return Summary{}, failure("DOCUMENT_COUNT_MISMATCH")
		}
	}
	for _, chunk := range chunks {
		if err := checkContext(ctx); err != nil {
			return Summary{}, err
		}
		docID := chunk["docId"].(string)
		index, ok := safeInteger(chunk["chunkIndex"])
		total, totalOK := safeInteger(chunk["totalChunks"])
		if !ok || index < 0 || index >= int64(counts[docID]) || !totalOK || total != int64(counts[docID]) {
			return Summary{}, failure("CHUNK_ORDER_INVALID")
		}
		if indexes[docID] == nil {
			indexes[docID] = make(map[int64]bool)
		}
		if indexes[docID][index] {
			return Summary{}, failure("CHUNK_ORDER_INVALID")
		}
		indexes[docID][index] = true
	}
	result := Summary{TableCount: len(tables), Documents: len(documents), Chunks: len(chunks)}
	if err := validateRegistry(ctx, snapshot.Registry, tables, chunks, &result); err != nil {
		return Summary{}, err
	}
	return result, nil
}

func decodeRegistry(raw json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var registry map[string]any
	if err := decoder.Decode(&registry); err != nil {
		return nil, failure("REGISTRY_INVALID")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return nil, failure("REGISTRY_INVALID")
	}
	return registry, nil
}
func validateRegistry(ctx context.Context, raw json.RawMessage, tables map[string]checkedTable, canonical map[string]map[string]any, result *Summary) error {
	registered := make(map[string]bool)
	if len(raw) > 0 {
		registry, err := decodeRegistry(raw)
		if err != nil {
			return err
		}
		if registry != nil {
			version, versionOK := safeInteger(registry["version"])
			spaces, spacesOK := registry["spaces"].([]any)
			if !versionOK || version != 1 || !spacesOK || len(spaces) > HardMaxTables {
				return failure("REGISTRY_INVALID")
			}
			generations := make(map[int64]bool, len(spaces))
			for _, value := range spaces {
				if err := checkContext(ctx); err != nil {
					return err
				}
				space, ok := value.(map[string]any)
				if !ok {
					return failure("SPACE_INVALID")
				}
				generation, generationOK := safeInteger(space["generation"])
				dimension, dimensionOK := safeInteger(space["vectorDimension"])
				name, nameOK := space["tableName"].(string)
				status, statusOK := space["status"].(string)
				fingerprint, fingerprintOK := space["modelFingerprint"].(string)
				_, createdOK := space["createdAt"].(string)
				metric, metricOK := space["distanceMetric"].(string)
				table, exists := tables[name]
				if !generationOK || generation < 0 || generations[generation] || !dimensionOK || dimension < 1 || !nameOK || registered[name] || !exists || !statusOK || (status != "active" && status != "inactive" && status != "building") || !fingerprintOK || fingerprint == "" || !createdOK || !metricOK || (metric != "l2" && metric != "cosine" && metric != "dot") {
					return failure("SPACE_INVALID")
				}
				generations[generation] = true
				registered[name] = true
				if int64(table.vectorDimension) != dimension {
					return failure("DIMENSION_MISMATCH")
				}
				for id, row := range table.rows {
					if err := checkContext(ctx); err != nil {
						return err
					}
					source := canonical[id]
					vector, vectorOK := row["vector"].([]any)
					if source == nil || source["text"] != row["text"] || source["docId"] != row["docId"] || !vectorOK || int64(len(vector)) != dimension {
						return failure("VECTOR_INVALID")
					}
				}
				if status == "active" {
					if result.ActiveGeneration != nil || len(table.rows) != len(canonical) {
						return failure("ACTIVE_INCOMPLETE")
					}
					active := generation
					result.ActiveGeneration = &active
				}
				result.Spaces++
				result.EmbeddingRows += len(table.rows)
			}
			pointer, present := registry["activeGeneration"]
			if !present {
				return failure("ACTIVE_POINTER_INVALID")
			}
			if result.ActiveGeneration == nil {
				if pointer != nil {
					return failure("ACTIVE_POINTER_INVALID")
				}
			} else {
				active, ok := safeInteger(pointer)
				if !ok || active != *result.ActiveGeneration {
					return failure("ACTIVE_POINTER_INVALID")
				}
			}
		}
	}
	for name := range tables {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if spaceTable(name) && !registered[name] {
			return failure("UNREGISTERED_SPACE")
		}
	}
	return nil
}

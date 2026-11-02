package knowledgedomain

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func domainBudget() Budget {
	return Budget{MaxTables: HardMaxTables, MaxRows: HardMaxRows, MaxCells: HardMaxCells, MaxTextBytes: HardMaxTextBytes}
}
func domainScalar(name, kind string) Field { return Field{Name: name, Type: kind} }
func domainChunkSchema() []Field {
	return []Field{
		domainScalar("id", "utf8"), domainScalar("docId", "utf8"),
		domainScalar("fileName", "utf8"), domainScalar("text", "utf8"),
		domainScalar("chunkIndex", "int32"), domainScalar("totalChunks", "int32"),
		domainScalar("importedAt", "utf8"), domainScalar("corpusKind", "utf8"),
		{Name: "chapterNumber", Type: "int32", Nullable: true},
		{Name: "chapterTitle", Type: "utf8", Nullable: true},
	}
}
func domainChunk(id string, index int) map[string]any {
	return map[string]any{
		"id": id, "docId": "synthetic-document", "fileName": "synthetic.pdf",
		"text": "合成正文 " + id, "chunkIndex": json.Number(string(rune('0' + index))),
		"totalChunks": json.Number("2"), "importedAt": "2026-01-01",
		"corpusKind": "reference", "chapterNumber": nil, "chapterTitle": nil,
	}
}
func domainSpace(generation int, table, status string, dimension int) map[string]any {
	return map[string]any{
		"generation": generation, "tableName": table, "status": status,
		"vectorDimension": dimension, "modelFingerprint": "synthetic-model",
		"createdAt": "not-required-to-be-a-date", "distanceMetric": "cosine",
	}
}
func domainSetRegistry(t *testing.T, snapshot *Snapshot, registry map[string]any) {
	t.Helper()
	raw, err := json.Marshal(registry)
	if err != nil { t.Fatal(err) }
	snapshot.Registry = raw
}
func domainMutateRegistry(t *testing.T, snapshot *Snapshot, mutate func(map[string]any)) {
	t.Helper()
	var registry map[string]any
	if err := json.Unmarshal(snapshot.Registry, &registry); err != nil { t.Fatal(err) }
	mutate(registry)
	domainSetRegistry(t, snapshot, registry)
}
func domainFixture(t *testing.T) Snapshot {
	t.Helper()
	documents := Table{Name: "documents", Version: 7, Schema: []Field{
		domainScalar("id", "utf8"), domainScalar("fileName", "utf8"),
		domainScalar("importedAt", "utf8"), domainScalar("chunkCount", "float64"),
		domainScalar("filePath", "utf8"), domainScalar("corpusKind", "utf8"),
	}, Records: []map[string]any{{
		"id": "synthetic-document", "fileName": "synthetic.pdf", "importedAt": "2026-01-01",
		"chunkCount": json.Number("2"), "filePath": "private-copy/synthetic.pdf", "corpusKind": "reference",
	}}}
	chunks := Table{Name: "chunks", Version: 2, Schema: domainChunkSchema(), Records: []map[string]any{domainChunk("synthetic-a", 0), domainChunk("synthetic-b", 1)}}
	embeddings := func(name string, count, dimension int) Table {
		table := Table{Name: name, Version: 3, Schema: domainChunkSchema()}
		table.Schema = append(table.Schema, Field{Name: "vector", Type: "fixed_size_list", ListSize: dimension, Children: []Field{{Name: "item", Type: "float32", Nullable: true}}})
		for i := 0; i < count; i++ {
			row := domainChunk([]string{"synthetic-a", "synthetic-b"}[i], i)
			vector := make([]any, dimension)
			for j := range vector { vector[j] = json.Number("0.25") }
			row["vector"] = vector
			table.Records = append(table.Records, row)
		}
		return table
	}
	snapshot := Snapshot{Tables: []Table{documents, chunks,
		embeddings("chunks__space_1", 2, 3), embeddings("chunks__space_2", 1, 2), embeddings("chunks__space_3", 0, 4),
	}}
	domainSetRegistry(t, &snapshot, map[string]any{"version": 1, "activeGeneration": 1,
		"spaces": []any{domainSpace(1, "chunks__space_1", "active", 3), domainSpace(2, "chunks__space_2", "inactive", 2), domainSpace(3, "chunks__space_3", "building", 4)},
	})
	return snapshot
}
func domainAssertRejected(t *testing.T, snapshot Snapshot, budget Budget) {
	t.Helper()
	summary, err := Validate(context.Background(), snapshot, budget)
	if err == nil { t.Fatal("accepted invalid snapshot") }
	if summary != (Summary{}) { t.Fatalf("nonzero summary on failure: %+v", summary) }
	if !strings.HasPrefix(err.Error(), "KNOWLEDGE_DOMAIN_") { t.Fatalf("not symbolic: %v", err) }
	if strings.Contains(err.Error(), "synthetic") || strings.Contains(err.Error(), "合成") { t.Fatalf("author data in error: %v", err) }
}

func TestValidateDomainSyntheticFixture(t *testing.T) {
	snapshot := domainFixture(t)
	summary, err := Validate(context.Background(), snapshot, domainBudget())
	if err != nil { t.Fatal(err) }
	if summary.TableCount != 5 || summary.Documents != 1 || summary.Chunks != 2 || summary.Spaces != 3 || summary.EmbeddingRows != 3 || summary.ActiveGeneration == nil || *summary.ActiveGeneration != 1 { t.Fatalf("summary: %+v", summary) }
	before := domainFixture(t)
	if !reflect.DeepEqual(snapshot, before) { t.Fatal("validator mutated input") }
	raw, err := json.Marshal(summary)
	if err != nil { t.Fatal(err) }
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil { t.Fatal(err) }
	if len(keys) != 6 || keys["tableCount"] != float64(5) || keys["activeGeneration"] != float64(1) { t.Fatalf("unexpected summary wire shape: %s", raw) }
	if _, exists := keys["logicalHash"]; exists { t.Fatal("validator must not claim a logical hash") }
}

func TestValidateDomainEmptyAndRegistryAbsence(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null"), json.RawMessage(" \n null \t")} {
		summary, err := Validate(context.Background(), Snapshot{Registry: raw}, domainBudget())
		if err != nil || summary != (Summary{}) { t.Fatalf("empty snapshot: %+v %v", summary, err) }
	}
	snapshot := domainFixture(t)
	snapshot.Tables = snapshot.Tables[:2]
	snapshot.Registry = nil
	if summary, err := Validate(context.Background(), snapshot, domainBudget()); err != nil || summary.Spaces != 0 || summary.ActiveGeneration != nil { t.Fatalf("canonical only: %+v %v", summary, err) }
	for i := range snapshot.Tables { snapshot.Tables[i].Records = nil }
	if summary, err := Validate(context.Background(), snapshot, domainBudget()); err != nil || summary.TableCount != 2 { t.Fatalf("schema-carrying empty tables: %+v %v", summary, err) }
	snapshot.Tables[0].Schema = nil
	domainAssertRejected(t, snapshot, domainBudget())
}

func TestValidateDomainSchemaMutations(t *testing.T) {
	cases := []struct{name string; mutate func(*Snapshot)}{
		{"unknown table", func(s *Snapshot) { s.Tables[0].Name = "author-secrets" }},
		{"unsafe table suffix", func(s *Snapshot) { s.Tables[2].Name = "chunks__space_1/other" }},
		{"empty space suffix", func(s *Snapshot) { s.Tables[2].Name = "chunks__space_" }},
		{"duplicate table", func(s *Snapshot) { s.Tables[1].Name = "documents" }},
		{"table metadata", func(s *Snapshot) { s.Tables[0].Metadata = true }},
		{"field metadata", func(s *Snapshot) { s.Tables[0].Schema[0].Metadata = true }},
		{"vector child metadata", func(s *Snapshot) { s.Tables[2].Schema[10].Children[0].Metadata = true }},
		{"unknown field", func(s *Snapshot) { s.Tables[0].Schema[0].Name = "secret" }},
		{"duplicate field", func(s *Snapshot) { s.Tables[0].Schema[1].Name = "id" }},
		{"missing field", func(s *Snapshot) { s.Tables[0].Schema = s.Tables[0].Schema[1:] }},
		{"wrong string type", func(s *Snapshot) { s.Tables[0].Schema[0].Type = "float64" }},
		{"wrong count type", func(s *Snapshot) { s.Tables[0].Schema[3].Type = "utf8" }},
		{"unsupported integer", func(s *Snapshot) { s.Tables[1].Schema[4].Type = "int64" }},
		{"scalar child", func(s *Snapshot) { s.Tables[0].Schema[0].Children = []Field{{Type: "utf8"}} }},
		{"scalar list size", func(s *Snapshot) { s.Tables[0].Schema[0].ListSize = 1 }},
		{"vector wrong type", func(s *Snapshot) { s.Tables[2].Schema[10].Type = "list" }},
		{"vector zero size", func(s *Snapshot) { s.Tables[2].Schema[10].ListSize = 0 }},
		{"vector negative size", func(s *Snapshot) { s.Tables[2].Schema[10].ListSize = -3 }},
		{"vector no child", func(s *Snapshot) { s.Tables[2].Schema[10].Children = nil }},
		{"vector two children", func(s *Snapshot) { f := &s.Tables[2].Schema[10]; f.Children = append(f.Children, f.Children[0]) }},
		{"vector wrong child type", func(s *Snapshot) { s.Tables[2].Schema[10].Children[0].Type = "float64" }},
		{"vector nested child", func(s *Snapshot) { s.Tables[2].Schema[10].Children[0].Children = []Field{{Type: "float32"}} }},
		{"empty table still checks schema", func(s *Snapshot) { s.Tables[4].Schema[0].Type = "binary" }},
		{"invalid utf8 schema", func(s *Snapshot) { s.Tables[0].Schema[0].Name = string([]byte{255}) }},
	}
	for _, tc := range cases { t.Run(tc.name, func(t *testing.T) { snapshot := domainFixture(t); tc.mutate(&snapshot); domainAssertRejected(t, snapshot, domainBudget()) }) }
}

func TestValidateDomainRowAndRelationshipMutations(t *testing.T) {
	cases := []struct{name string; mutate func(*Snapshot)}{
		{"extra key", func(s *Snapshot) { s.Tables[0].Records[0]["unexpected"] = "value" }},
		{"missing key", func(s *Snapshot) { delete(s.Tables[0].Records[0], "filePath") }},
		{"substituted key", func(s *Snapshot) { delete(s.Tables[0].Records[0], "filePath"); s.Tables[0].Records[0]["unexpected"] = "value" }},
		{"empty id", func(s *Snapshot) { s.Tables[0].Records[0]["id"] = "" }},
		{"duplicate id", func(s *Snapshot) { s.Tables[1].Records[1]["id"] = "synthetic-a" }},
		{"null required string", func(s *Snapshot) { s.Tables[0].Schema[1].Nullable = true; s.Tables[0].Records[0]["fileName"] = nil }},
		{"nonnull violation", func(s *Snapshot) { s.Tables[0].Records[0]["fileName"] = nil }},
		{"boolean string", func(s *Snapshot) { s.Tables[0].Records[0]["fileName"] = true }},
		{"map string", func(s *Snapshot) { s.Tables[0].Records[0]["fileName"] = map[string]any{"x": "y"} }},
		{"invalid utf8 row", func(s *Snapshot) { s.Tables[1].Records[0]["text"] = string([]byte{255}) }},
		{"unknown corpus", func(s *Snapshot) { s.Tables[1].Records[0]["corpusKind"] = "unsupported" }},
		{"null corpus", func(s *Snapshot) { s.Tables[1].Schema[7].Nullable = true; s.Tables[1].Records[0]["corpusKind"] = nil }},
		{"orphan", func(s *Snapshot) { s.Tables[1].Records[0]["docId"] = "missing-synthetic" }},
		{"count mismatch", func(s *Snapshot) { s.Tables[0].Records[0]["chunkCount"] = 3 }},
		{"fractional count", func(s *Snapshot) { s.Tables[0].Records[0]["chunkCount"] = 2.5 }},
		{"negative count", func(s *Snapshot) { s.Tables[0].Records[0]["chunkCount"] = -1 }},
		{"unsafe count", func(s *Snapshot) { s.Tables[0].Records[0]["chunkCount"] = json.Number("9007199254740992") }},
		{"duplicate index", func(s *Snapshot) { s.Tables[1].Records[1]["chunkIndex"] = 0 }},
		{"negative index", func(s *Snapshot) { s.Tables[1].Records[0]["chunkIndex"] = -1 }},
		{"out of bounds index", func(s *Snapshot) { s.Tables[1].Records[1]["chunkIndex"] = 2 }},
		{"fractional index", func(s *Snapshot) { s.Tables[1].Records[0]["chunkIndex"] = 0.5 }},
		{"int32 overflow", func(s *Snapshot) { s.Tables[1].Records[0]["chunkIndex"] = int64(2147483648) }},
		{"wrong total", func(s *Snapshot) { s.Tables[1].Records[0]["totalChunks"] = 1 }},
		{"null optional nonnullable", func(s *Snapshot) { s.Tables[1].Schema[8].Nullable = false }},
		{"wrong optional numeric", func(s *Snapshot) { s.Tables[1].Records[0]["chapterNumber"] = "1" }},
		{"wrong optional title", func(s *Snapshot) { s.Tables[1].Records[0]["chapterTitle"] = 1 }},
		{"nan number", func(s *Snapshot) { s.Tables[0].Records[0]["chunkCount"] = math.NaN() }},
		{"infinite number", func(s *Snapshot) { s.Tables[0].Records[0]["chunkCount"] = math.Inf(1) }},
	}
	for _, tc := range cases { t.Run(tc.name, func(t *testing.T) { snapshot := domainFixture(t); tc.mutate(&snapshot); domainAssertRejected(t, snapshot, domainBudget()) }) }
}

func TestValidateDomainVectorMutations(t *testing.T) {
	cases := []struct{name string; value any}{
		{"short", []any{0, 1}}, {"long", []any{0, 1, 2, 3}}, {"nil", nil},
		{"typed slice not JSON array", []float32{0, 1, 2}}, {"number", 1},
		{"nan", []any{0, math.NaN(), 1}}, {"positive infinity", []any{0, math.Inf(1), 1}},
		{"negative infinity", []any{0, math.Inf(-1), 1}}, {"null child", []any{0, nil, 1}},
		{"string child", []any{0, "1", 1}}, {"bool child", []any{0, true, 1}},
		{"nested array", []any{0, []any{1}, 1}}, {"float32 overflow", []any{0, math.MaxFloat64, 1}},
		{"bad json number", []any{0, json.Number("NaN"), 1}},
	}
	for _, tc := range cases { t.Run(tc.name, func(t *testing.T) { snapshot := domainFixture(t); snapshot.Tables[2].Records[0]["vector"] = tc.value; domainAssertRejected(t, snapshot, domainBudget()) }) }
	for _, key := range []string{"id", "docId", "text"} {
		t.Run("canonical mismatch "+key, func(t *testing.T) { snapshot := domainFixture(t); snapshot.Tables[2].Records[0][key] = "missing-synthetic"; domainAssertRejected(t, snapshot, domainBudget()) })
	}
}

func TestValidateDomainRegistryMutations(t *testing.T) {
	cases := []struct{name string; mutate func(map[string]any)}{
		{"wrong version", func(r map[string]any) { r["version"] = 2 }},
		{"version string", func(r map[string]any) { r["version"] = "1" }},
		{"missing spaces", func(r map[string]any) { delete(r, "spaces") }},
		{"nonarray spaces", func(r map[string]any) { r["spaces"] = map[string]any{} }},
		{"null space", func(r map[string]any) { r["spaces"].([]any)[0] = nil }},
		{"duplicate generation", func(r map[string]any) { r["spaces"].([]any)[1].(map[string]any)["generation"] = 1 }},
		{"negative generation", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["generation"] = -1 }},
		{"fractional generation", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["generation"] = 1.5 }},
		{"unsafe generation", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["generation"] = json.Number("9007199254740992") }},
		{"duplicate table", func(r map[string]any) { r["spaces"].([]any)[1].(map[string]any)["tableName"] = "chunks__space_1" }},
		{"absent table", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["tableName"] = "chunks__space_99" }},
		{"wrong status", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["status"] = "ready" }},
		{"empty fingerprint", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["modelFingerprint"] = "" }},
		{"wrong fingerprint type", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["modelFingerprint"] = 1 }},
		{"wrong createdAt type", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["createdAt"] = nil }},
		{"wrong metric", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["distanceMetric"] = "euclidean" }},
		{"zero dimension", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["vectorDimension"] = 0 }},
		{"fractional dimension", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["vectorDimension"] = 3.5 }},
		{"dimension mismatch", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["vectorDimension"] = 4 }},
		{"missing pointer", func(r map[string]any) { delete(r, "activeGeneration") }},
		{"null active pointer", func(r map[string]any) { r["activeGeneration"] = nil }},
		{"wrong pointer", func(r map[string]any) { r["activeGeneration"] = 2 }},
		{"string pointer", func(r map[string]any) { r["activeGeneration"] = "1" }},
		{"second active incomplete", func(r map[string]any) { r["spaces"].([]any)[1].(map[string]any)["status"] = "active" }},
		{"no active but pointer", func(r map[string]any) { r["spaces"].([]any)[0].(map[string]any)["status"] = "inactive" }},
		{"unregistered table", func(r map[string]any) { r["spaces"] = r["spaces"].([]any)[:2] }},
	}
	for _, tc := range cases { t.Run(tc.name, func(t *testing.T) { snapshot := domainFixture(t); domainMutateRegistry(t, &snapshot, tc.mutate); domainAssertRejected(t, snapshot, domainBudget()) }) }
	for _, raw := range []string{"", " ", "{", "[]", "1", "true", `{"version":1,"spaces":[],"activeGeneration":null} {}`, `{"version":1,"spaces":[],"activeGeneration":null} false`} {
		if raw == "" { continue } // Missing is a supported absence representation.
		t.Run("malformed "+raw, func(t *testing.T) { snapshot := domainFixture(t); snapshot.Registry = json.RawMessage(raw); domainAssertRejected(t, snapshot, domainBudget()) })
	}
	for _, raw := range []json.RawMessage{nil, json.RawMessage("null")} {
		snapshot := domainFixture(t); snapshot.Registry = raw; domainAssertRejected(t, snapshot, domainBudget())
	}
}

func TestValidateDomainRegistryPositiveVariants(t *testing.T) {
	for _, metric := range []string{"l2", "cosine", "dot"} {
		t.Run(metric, func(t *testing.T) {
			snapshot := domainFixture(t)
			domainMutateRegistry(t, &snapshot, func(r map[string]any) {
				r["unknownFutureField"] = map[string]any{"kept": true}
				r["spaces"].([]any)[0].(map[string]any)["distanceMetric"] = metric
				r["spaces"].([]any)[0].(map[string]any)["unknownField"] = []any{true, nil}
			})
			if _, err := Validate(context.Background(), snapshot, domainBudget()); err != nil { t.Fatal(err) }
		})
	}
	snapshot := domainFixture(t)
	domainMutateRegistry(t, &snapshot, func(r map[string]any) { r["activeGeneration"] = nil; r["spaces"].([]any)[0].(map[string]any)["status"] = "inactive" })
	if summary, err := Validate(context.Background(), snapshot, domainBudget()); err != nil || summary.ActiveGeneration != nil { t.Fatalf("all inactive: %+v %v", summary, err) }
	for i := range snapshot.Tables { snapshot.Tables[i].Records = nil }
	domainMutateRegistry(t, &snapshot, func(r map[string]any) { r["activeGeneration"] = 1; r["spaces"].([]any)[0].(map[string]any)["status"] = "active" })
	if summary, err := Validate(context.Background(), snapshot, domainBudget()); err != nil || summary.ActiveGeneration == nil { t.Fatalf("empty active: %+v %v", summary, err) }
}

func TestValidateDomainNumericRepresentations(t *testing.T) {
	for _, value := range []any{int(2), int8(2), int16(2), int32(2), int64(2), uint(2), uint8(2), uint16(2), uint32(2), uint64(2), float32(2), float64(2), json.Number("2"), json.Number("2.0"), json.Number("2e0"), json.Number("2.0000000000000001")} {
		snapshot := domainFixture(t)
		snapshot.Tables[0].Records[0]["chunkCount"] = value
		if _, err := Validate(context.Background(), snapshot, domainBudget()); err != nil { t.Fatalf("numeric %T %v: %v", value, value, err) }
	}
	for _, value := range []any{json.Number(""), json.Number("NaN"), json.Number("Infinity"), json.Number("01"), json.Number("+2"), json.Number("true"), json.Number("2 3"), json.Number("1e9999"), json.Number(strings.Repeat("0", 129)), "2", true, nil, uint64(math.MaxUint64)} {
		snapshot := domainFixture(t); snapshot.Tables[0].Records[0]["chunkCount"] = value; domainAssertRejected(t, snapshot, domainBudget())
	}
	snapshot := domainFixture(t)
	for i := range snapshot.Tables[1].Schema { if numericField(snapshot.Tables[1].Schema[i].Name) { snapshot.Tables[1].Schema[i].Type = "float64" } }
	snapshot.Tables[1].Records[0]["chunkIndex"] = json.Number("0e-99999999")
	if _, err := Validate(context.Background(), snapshot, domainBudget()); err != nil { t.Fatal(err) }
	snapshot.Tables[2].Records[0]["vector"] = []any{float32(0.1), float64(float32(0.2)), int(1)}
	if _, err := Validate(context.Background(), snapshot, domainBudget()); err != nil { t.Fatal(err) }
}

func TestValidateDomainBudgets(t *testing.T) {
	for _, budget := range []Budget{
		{}, {MaxTables: -1, MaxRows: 1, MaxCells: 1, MaxTextBytes: 1},
		{MaxTables: 33, MaxRows: 1, MaxCells: 1, MaxTextBytes: 1},
		{MaxTables: 1, MaxRows: 10001, MaxCells: 1, MaxTextBytes: 1},
		{MaxTables: 1, MaxRows: 1, MaxCells: 200001, MaxTextBytes: 1},
		{MaxTables: 1, MaxRows: 1, MaxCells: 1, MaxTextBytes: (32 << 20) + 1},
	} { domainAssertRejected(t, Snapshot{}, budget) }
	snapshot := domainFixture(t)
	for _, modify := range []func(*Budget){
		func(b *Budget) { b.MaxTables = 4 },
		func(b *Budget) { b.MaxRows = 5 }, // 1 doc + 2 canonical + 3 embeddings.
		func(b *Budget) { b.MaxCells = 61 }, // 59 top-level values plus 8 vector elements = 67.
		func(b *Budget) { b.MaxTextBytes = len(snapshot.Registry) },
	} { budget := domainBudget(); modify(&budget); domainAssertRejected(t, snapshot, budget) }
	budget := domainBudget(); budget.MaxTables = 5; budget.MaxRows = 6; budget.MaxCells = 67
	if _, err := Validate(context.Background(), snapshot, budget); err != nil { t.Fatalf("exact resource limits: %v", err) }
	budget.MaxCells = 66
	domainAssertRejected(t, snapshot, budget)
	// All raw registry bytes, including ignored keys and whitespace, count.
	budget = domainBudget(); budget.MaxTextBytes = 128
	domainAssertRejected(t, Snapshot{Registry: json.RawMessage(strings.Repeat(" ", 129) + "null")}, budget)
	// The aggregate text boundary is discoverable exactly, not per-table.
	low, high := 1, 10000
	for low < high {
		mid := (low + high) / 2; budget = domainBudget(); budget.MaxTextBytes = mid
		if _, err := Validate(context.Background(), snapshot, budget); err == nil { high = mid } else { low = mid + 1 }
	}
	budget = domainBudget(); budget.MaxTextBytes = low
	if _, err := Validate(context.Background(), snapshot, budget); err != nil { t.Fatal(err) }
	budget.MaxTextBytes--
	domainAssertRejected(t, snapshot, budget)
}

func TestValidateDomainLargeVectorCountPreflight(t *testing.T) {
	snapshot := domainFixture(t)
	snapshot.Tables[2].Records[0]["vector"] = make([]any, HardMaxCells)
	summary, err := Validate(context.Background(), snapshot, domainBudget())
	if err == nil || err.Error() != "KNOWLEDGE_DOMAIN_CELL_LIMIT" || summary != (Summary{}) { t.Fatalf("vector elements must consume cells before validation: %+v %v", summary, err) }
}

func TestValidateDomainContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background()); cancel()
	if summary, err := Validate(ctx, domainFixture(t), domainBudget()); !errors.Is(err, context.Canceled) || summary != (Summary{}) { t.Fatalf("cancellation: %+v %v", summary, err) }
	ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0)); defer cancel()
	if summary, err := Validate(ctx, Snapshot{}, domainBudget()); !errors.Is(err, context.DeadlineExceeded) || summary != (Summary{}) { t.Fatalf("deadline: %+v %v", summary, err) }
	if summary, err := Validate(nil, Snapshot{}, domainBudget()); err == nil || summary != (Summary{}) { t.Fatalf("nil context: %+v %v", summary, err) }
}

package knowledgelogical

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"sort"
	"strings"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

// Summary describes the ORIGINAL vector-migration logical hash protocol, only
// in the explicit bounded generated-ID/en-US compatibility lane. Physical copy
// hashes and portable document/copy hashes are different protocols. Versions are
// intentionally excluded, as in the original. No input is mutated.
type Summary struct {
	Documents int `json:"documents"`
	Chunks int `json:"chunks"`
	TableCount int `json:"tableCount"`
	LogicalHash string `json:"logicalHash"`
	Tables []TableHash `json:"tables"`
}
type TableHash struct {
	Name string `json:"name"`
	LogicalHash string `json:"logicalHash"`
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func hexLower(s string) bool {
	for _, c := range s { if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') { return false } }
	return true
}
func generatedID(s string) bool {
	if len(s) == 64 && hexLower(s) { return true }
	for _, prefix := range []string{"finalization-", "portable:"} {
		if strings.HasPrefix(s,prefix) && len(s) == len(prefix)+64 && hexLower(s[len(prefix):]) { return true }
	}
	if len(s) != 36 { return false }
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 { if c != '-' { return false }; continue }
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') { return false }
	}
	return true
}
func ordering(ids []string, options Options) error {
	if options.Locale != "en-US" { return failure("LOCALE_UNSUPPORTED") }
	if len(ids) < 2 { return nil }
	for _, id := range ids { if !generatedID(id) { return failure("ID_COLLATION_UNSUPPORTED") } }
	return nil
}

// arrowType is an explicit mapping from the admitted native typed schema, not
// typeof values, inferred row shapes, SQL types, or a fabricated Arrow string.
// The actual Apache Arrow runtime in the contract is the spelling authority.
func arrowType(field knowledgedomain.Field) (string, error) {
	switch field.Type {
	case "utf8": return "Utf8", nil
	case "int32": return "Int32", nil
	case "float32": return "Float32", nil
	case "float64": return "Float64", nil
	case "fixed_size_list":
		if len(field.Children) != 1 { return "", failure("SCHEMA_UNSUPPORTED") }
		child, err := arrowType(field.Children[0]); if err != nil { return "", err }
		size, err := numberString(float64(field.ListSize)); if err != nil { return "", err }
		return "FixedSizeList["+size+"]<"+child+">", nil
	default: return "", failure("SCHEMA_UNSUPPORTED")
	}
}
func schemaValue(field knowledgedomain.Field) ([]any, error) {
	if field.Metadata { return nil, failure("SCHEMA_METADATA_UNSUPPORTED") }
	typ, err := arrowType(field); if err != nil { return nil, err }
	children := make([]any, 0, len(field.Children))
	for _, child := range field.Children { value, err := schemaValue(child); if err != nil { return nil, err }; children = append(children,value) }
	return []any{field.Name,typ,field.Nullable,children}, nil
}
func cellValue(field knowledgedomain.Field, value any) (any,error) {
	if value == nil { return nil,nil }
	switch field.Type {
	case "utf8": return value,nil
	case "int32", "float64": return number(value)
	case "fixed_size_list":
		values, ok := value.([]any); if !ok { return nil,failure("CELL_UNSUPPORTED") }
		out := make([]any,len(values))
		for i, v := range values {
			n, err := number(v); if err != nil { return nil,err }
			// Actual Arrow Float32Array elements are binary32 promoted to JS
			// Number. Native JSON sidecars may spell more decimal digits.
			out[i] = float64(float32(n))
		}
		return out,nil
	default: return nil,failure("CELL_UNSUPPORTED")
	}
}

// HashSnapshot first applies the existing strict typed/domain validator, then
// hashes schema-ordered records. Registry must contain ORIGINAL exact asset
// bytes, not a parsed/remarshaled object: even whitespace changes its hash.
// nil Registry means absent; present JSON null has a real raw-byte hash.
// The caller must supply an immutable snapshot and establish exclusion, native
// Arrow authority, private-copy provenance, and locale profile independently.
// UTF-8 malformed/surrogate strings and arbitrary multi-ID collation fail closed.
func HashSnapshot(ctx context.Context, snapshot knowledgedomain.Snapshot, budget knowledgedomain.Budget, options Options) (Summary,error) {
	if err := contextOK(ctx); err != nil { return Summary{},err }
	if options.Locale != "en-US" { return Summary{},failure("LOCALE_UNSUPPORTED") }
	domain, err := knowledgedomain.Validate(ctx,snapshot,budget)
	if err != nil { return Summary{},err }
	out := Summary{Documents:domain.Documents,Chunks:domain.Chunks,TableCount:domain.TableCount,Tables:make([]TableHash,0,len(snapshot.Tables))}
	// Escaping at most six bytes per input byte; numeric cells at most 32.
	limit := budget.MaxTextBytes*6 + budget.MaxCells*32 + 65536
	pairs := make([]any,0,len(snapshot.Tables))
	for _, table := range snapshot.Tables {
		if err := contextOK(ctx); err != nil { return Summary{},err }
		schema := make([]any,0,len(table.Schema))
		for _, field := range table.Schema { v, err := schemaValue(field); if err != nil { return Summary{},err }; schema = append(schema,v) }
		ids := make([]string,0,len(table.Records))
		for _, row := range table.Records { ids = append(ids,row["id"].(string)) }
		if err := ordering(ids,options); err != nil { return Summary{},err }
		indexes := make([]int,len(ids)); for i := range indexes { indexes[i] = i }
		// This comparator is intentionally restricted by ordering, not offered
		// as a general substitute for ICU localeCompare. Generated alphabets
		// and fixed separator positions have en-US lexicographic order.
		sort.SliceStable(indexes,func(i,j int) bool { return ids[indexes[i]] < ids[indexes[j]] })
		records := make([]any,0,len(indexes))
		for _, index := range indexes {
			row := table.Records[index]
			obj := make(object,0,len(table.Schema))
			for _, field := range table.Schema { v, err := cellValue(field,row[field.Name]); if err != nil { return Summary{},err }; obj = append(obj,member{field.Name,v}) }
			records = append(records,obj)
		}
		raw, err := encode(ctx,[]any{table.Name,schema,records},limit); if err != nil { return Summary{},err }
		out.Tables = append(out.Tables,TableHash{Name:table.Name,LogicalHash:digest(raw)})
	}
	// Original Array.sort converts [name,hash] pairs to 'name,hash' and
	// compares UTF-16 code units. Names are unique in the admitted domain.
	sort.Slice(out.Tables,func(i,j int) bool { return utf16Less(out.Tables[i].Name+","+out.Tables[i].LogicalHash,out.Tables[j].Name+","+out.Tables[j].LogicalHash) })
	for _, table := range out.Tables { pairs = append(pairs,[]any{table.Name,table.LogicalHash}) }
	var registry any
	if snapshot.Registry != nil { registry = digest(snapshot.Registry) }
	raw, err := encode(ctx,[]any{pairs,registry},limit); if err != nil { return Summary{},err }
	out.LogicalHash = digest(raw)
	if err := contextOK(ctx); err != nil { return Summary{},err }
	return out,nil
}

// DecodeSnapshot decodes bounded private native domain JSON with UseNumber.
// Registry bytes in this envelope are not necessarily the original asset: the
// caller must replace Registry with its independently captured exact raw bytes
// before hashing if native serialized a registry object rather than raw bytes.
func DecodeSnapshot(ctx context.Context, raw []byte, maxBytes int) (knowledgedomain.Snapshot,error) {
	if err := contextOK(ctx); err != nil { return knowledgedomain.Snapshot{},err }
	if maxBytes < 1 || maxBytes > 64<<20 || len(raw) > maxBytes { return knowledgedomain.Snapshot{},failure("INPUT_LIMIT") }
	if !bytes.Equal(bytes.TrimSpace(raw),[]byte("null")) && len(bytes.TrimSpace(raw)) > 0 && bytes.TrimSpace(raw)[0] == '{' {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var out knowledgedomain.Snapshot
		if err := decoder.Decode(&out); err != nil { return knowledgedomain.Snapshot{},failure("INPUT_INVALID") }
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF { return knowledgedomain.Snapshot{},failure("INPUT_INVALID") }
		if err := contextOK(ctx); err != nil { return knowledgedomain.Snapshot{},err }
		return out,nil
	}
	return knowledgedomain.Snapshot{},failure("INPUT_INVALID")
}

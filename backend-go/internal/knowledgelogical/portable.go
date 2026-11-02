package knowledgelogical

import (
	"context"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
)

// PortableChunk/Document/Snapshot are the original strict portable projection.
// No file paths, vectors or embedding identities are serialized.
type PortableChunk struct { ChunkIndex int `json:"chunkIndex"`; Text string `json:"text"` }
type PortableDocument struct {
	DocID string `json:"docId"`
	FileName string `json:"fileName"`
	CorpusKind string `json:"corpusKind"`
	Chunks []PortableChunk `json:"chunks"`
	Copy *knowledgecopy.Copy `json:"-"`
}
type PortableSnapshot struct { Version int; Documents []PortableDocument }

func portableText(s string) bool { return s != "" && utf8.ValidString(s) && !strings.ContainsRune(s,0) }
func portableFileName(s string) bool {
	if !portableText(s) || utf16Length(s) > 512 { return false }
	n := norm.NFKC.String(s)
	if strings.ContainsAny(n,"/\\<>:\"|?*") || strings.HasSuffix(n,".") || strings.HasSuffix(n," ") { return false }
	for _, c := range n { if c <= 31 { return false } }
	stem := n
	if at := strings.IndexByte(stem,'.'); at >= 0 { stem = stem[:at] }
	stem = strings.ToLower(stem)
	if stem == "con" || stem == "prn" || stem == "aux" || stem == "nul" { return false }
	if len(stem) == 4 && (strings.HasPrefix(stem,"com") || strings.HasPrefix(stem,"lpt")) && stem[3] >= '1' && stem[3] <= '9' { return false }
	return true
}
func corpus(s string) bool { return s == "reference" || s == "project-knowledge" || s == "unknown" }

// SerializePortable matches serializePortableKnowledgeSnapshot's bytes, including
// its final newline, in the explicit ordering lane. NFKC is used ONLY for filename
// rejection; original filenames and all content retain their exact Unicode form.
// x/text's Unicode version is pinned by go.mod; newly assigned characters beyond
// that version are not an arbitrary future-ICU compatibility claim.
func SerializePortable(ctx context.Context, snapshot PortableSnapshot, budget knowledgedomain.Budget, options Options) ([]byte,error) {
	if err := contextOK(ctx); err != nil { return nil,err }
	if budget.MaxTables < 1 || budget.MaxTables > knowledgedomain.HardMaxTables || budget.MaxRows < 1 || budget.MaxRows > knowledgedomain.HardMaxRows || budget.MaxCells < 1 || budget.MaxCells > knowledgedomain.HardMaxCells || budget.MaxTextBytes < 1 || budget.MaxTextBytes > knowledgedomain.HardMaxTextBytes { return nil,failure("BUDGET_INVALID") }
	if snapshot.Version != 1 { return nil,failure("PORTABLE_INVALID") }
	if len(snapshot.Documents) > budget.MaxRows { return nil,failure("ROW_LIMIT") }
	ids := make([]string,0,len(snapshot.Documents))
	seen := make(map[string]bool,len(snapshot.Documents))
	textBytes, rows, cells := 0,len(snapshot.Documents),0
	charge := func(s string) error {
		if len(s) > budget.MaxTextBytes-textBytes { return failure("TEXT_LIMIT") }
		textBytes += len(s)
		if !utf8.ValidString(s) { return failure("TEXT_INVALID") }
		return nil
	}
	// Account all untrusted content before allocating canonical projection.
	for _, doc := range snapshot.Documents {
		if err := contextOK(ctx); err != nil { return nil,err }
		for _, s := range []string{doc.DocID,doc.FileName,doc.CorpusKind} { if err := charge(s); err != nil { return nil,err } }
		if len(doc.Chunks) > budget.MaxRows-rows { return nil,failure("ROW_LIMIT") }; rows += len(doc.Chunks)
		if len(doc.Chunks) > (budget.MaxCells-cells)/2 { return nil,failure("CELL_LIMIT") }; cells += len(doc.Chunks)*2
		for _, chunk := range doc.Chunks { if err := charge(chunk.Text); err != nil { return nil,err } }
		if doc.Copy != nil { if err := charge(doc.Copy.Content); err != nil { return nil,err }; if err := charge(doc.Copy.IndexedHash); err != nil { return nil,err } }
		if !portableText(doc.DocID) || seen[doc.DocID] || !portableFileName(doc.FileName) || !corpus(doc.CorpusKind) || len(doc.Chunks) == 0 { return nil,failure("PORTABLE_INVALID") }
		seen[doc.DocID] = true; ids = append(ids,doc.DocID)
		for i, chunk := range doc.Chunks { if chunk.ChunkIndex != i || !portableText(chunk.Text) { return nil,failure("PORTABLE_INVALID") } }
		if copy := doc.Copy; copy != nil {
			if len(copy.IndexedHash) != 64 || !hexLower(copy.IndexedHash) || (!copy.IndexDirty && digest([]byte(copy.Content)) != copy.IndexedHash) { return nil,failure("PORTABLE_INVALID") }
		}
	}
	if err := ordering(ids,options); err != nil { return nil,err }
	indexes := make([]int,len(ids)); for i := range indexes { indexes[i] = i }
	sort.SliceStable(indexes,func(i,j int) bool { return ids[indexes[i]] < ids[indexes[j]] })
	docs := make([]any,0,len(indexes))
	for _, index := range indexes {
		doc := snapshot.Documents[index]
		chunks := make([]any,0,len(doc.Chunks))
		for _, chunk := range doc.Chunks { chunks = append(chunks,object{{"chunkIndex",chunk.ChunkIndex},{"text",chunk.Text}}) }
		obj := object{{"docId",doc.DocID},{"fileName",doc.FileName},{"corpusKind",doc.CorpusKind},{"chunks",chunks}}
		if copy := doc.Copy; copy != nil { obj = append(obj,member{"copy",object{{"content",copy.Content},{"indexedHash",copy.IndexedHash},{"edited",copy.Edited},{"indexDirty",copy.IndexDirty}}}) }
		docs = append(docs,obj)
	}
	raw, err := encode(ctx,object{{"version",1},{"documents",docs}},budget.MaxTextBytes*6+budget.MaxCells*32+65536)
	if err != nil { return nil,err }
	return append(raw,'\n'),nil
}

// PortableFromSnapshot implements the original exported readPortable... projection
// over an already-captured native typed snapshot and captured managed copies.
// It does not open author files or silently repair missing/dirty copies. Domain
// validation is required even though embeddings are omitted from the projection.
func PortableFromSnapshot(ctx context.Context, snapshot knowledgedomain.Snapshot, copies map[string]knowledgecopy.Copy, budget knowledgedomain.Budget, options Options) ([]byte,error) {
	if _, err := knowledgedomain.Validate(ctx,snapshot,budget); err != nil { return nil,err }
	var documents, chunks *knowledgedomain.Table
	for i := range snapshot.Tables { switch snapshot.Tables[i].Name { case "documents": documents = &snapshot.Tables[i]; case "chunks": chunks = &snapshot.Tables[i] } }
	portable := PortableSnapshot{Version:1}
	if documents == nil && chunks == nil { return SerializePortable(ctx,portable,budget,options) }
	if documents == nil || chunks == nil { return nil,failure("PORTABLE_INVALID") }
	grouped := make(map[string][]map[string]any)
	for _, row := range chunks.Records { id := row["docId"].(string); grouped[id] = append(grouped[id],row) }
	for _, row := range documents.Records {
		if err := contextOK(ctx); err != nil { return nil,err }
		id := row["id"].(string)
		kind, ok := row["corpusKind"].(string); if !ok { return nil,failure("PORTABLE_INVALID") }
		doc := PortableDocument{DocID:id,FileName:row["fileName"].(string),CorpusKind:kind}
		values := grouped[id]
		// Domain guarantees unique contiguous indexes; do not mutate caller rows.
		doc.Chunks = make([]PortableChunk,len(values))
		for _, chunk := range values {
			if chunk["fileName"] != doc.FileName || chunk["corpusKind"] != kind { return nil,failure("PORTABLE_INVALID") }
			n, err := number(chunk["chunkIndex"]); if err != nil { return nil,err }
			doc.Chunks[int(n)] = PortableChunk{ChunkIndex:int(n),Text:chunk["text"].(string)}
		}
		if strings.HasPrefix(row["filePath"].(string),"knowledge-copy:") {
			copy, exists := copies[id]; if !exists { return nil,failure("PORTABLE_INVALID") }; doc.Copy = &copy
		}
		portable.Documents = append(portable.Documents,doc)
	}
	return SerializePortable(ctx,portable,budget,options)
}

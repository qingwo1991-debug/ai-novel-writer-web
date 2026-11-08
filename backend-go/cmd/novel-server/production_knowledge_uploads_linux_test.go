//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// These tests travel the same real HTTP surface the served knowledge tests use -
// the frozen three-key session over an admitted v7 project - and assert on what
// the import left in the canonical snapshot, never on the receipt alone. The
// point of the slice is that a browser upload lands in exactly the store the
// served readers already decode, so every positive case re-reads that store with
// knowledgedomain and searches it with the same knowledgequery capture a served
// kb:search builds.
//
// The three names under test:
//
//	kb:import-document        bound, arity 2
//	kb:import-folder          bound, arity 2
//	kb:import-reference-text  bound, arity 2 (the browser's shape)

const (
	// productionKnowledgeUploadNonce opens the one project session these tests use.
	productionKnowledgeUploadNonce = "knowledge-upload-1"
	// productionKnowledgeUploadReferenceKey is the author's stable import key; the
	// document identity must be derived from it, not from a fresh UUID.
	productionKnowledgeUploadReferenceKey = "novel-alpha"
	// productionKnowledgeUploadFolderFile is one text member's name inside a
	// staged folder, chosen so the name-derived chapter meta is asserted too.
	productionKnowledgeUploadFolderFile = "第1章 门.txt"
)

// productionKnowledgeUploadBinary is bytes no text reader may accept: a lone
// continuation byte after a BOM lead byte is not valid UTF-8.
var productionKnowledgeUploadBinary = []byte{0xff, 0xfe, 0x00, 0x41, 0xc3}

// productionKnowledgeUploadTestLane is one open lane with the knowledge writers,
// the three upload names, and two callers: the tab that opened the project and one
// that never did.
type productionKnowledgeUploadTestLane struct {
	server      *transport.Server
	open        *productionOpen
	uploads     *productionPickerFixtureUploads
	imports     *productionKnowledgeImports
	author      *productionDurableClient
	other       *productionDurableClient
	projectPath string
	frozen      map[string]any
}

// newProductionKnowledgeUploadTestLane builds the lane itself, because the
// production handler's binder block is what installs a name: this slice's binder
// is under test, so the test installs it. The plain import binder is installed
// beside it so both doors provably share one writer.
func newProductionKnowledgeUploadTestLane(t *testing.T,
	uploads productionPickerUploads) *productionKnowledgeUploadTestLane {
	t.Helper()
	root, projectPath, contract, _, _ := productionEditorReadsRoot(t)
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	startup, cancel := context.WithTimeout(context.Background(), productionStartupTimeout)
	defer cancel()
	open, err := openProduction(startup, layout, productionConfig{
		Schema: contract, MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress),
		"go-production-knowledge-uploads")
	t.Cleanup(func() {
		server.Close()
		if closeErr := open.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	if bindErr := server.BindProjects(open.durable); bindErr != nil {
		t.Fatal(bindErr)
	}
	imports := newProductionKnowledgeImports(open.durable)
	if bindErr := imports.Bind(server); bindErr != nil {
		t.Fatalf("bind the served knowledge imports: %v", bindErr)
	}
	if bindErr := newProductionKnowledgeUploads(imports, uploads).Bind(server); bindErr != nil {
		t.Fatalf("bind the knowledge upload names: %v", bindErr)
	}
	author, frozen := productionExecuteSession(t, server, projectPath, productionKnowledgeUploadNonce)
	lane := &productionKnowledgeUploadTestLane{
		server:      server,
		open:        open,
		imports:     imports,
		author:      author,
		other:       newProductionDurableClient(t, server, "client_knowledge_other"),
		projectPath: projectPath,
		frozen:      frozen,
	}
	if staged, ok := uploads.(*productionPickerFixtureUploads); ok {
		lane.uploads = staged
	}
	return lane
}

// stage stages one file upload for the tab that opened the project.
func (l *productionKnowledgeUploadTestLane) stage(id, name string, data []byte) {
	l.uploads.stage(l.author.cookie.Value, l.author.tab, id, name, data)
}

// stageFolder stages one folder upload for the tab that opened the project.
func (l *productionKnowledgeUploadTestLane) stageFolder(id, name string, files []productionPickerUploaded) {
	l.uploads.stageFolder(l.author.cookie.Value, l.author.tab, id, name, files)
}

// productionKnowledgeUploadTestTree digests every file the project holds, so "a
// refusal wrote nothing" is one comparison instead of a list of files a test hoped
// were the only ones. Directories carry no bytes and are skipped; SQLite's
// write-ahead sidecars are excluded because they are the driver's transport, not
// the project's data; every remaining entry must be a regular file.
func productionKnowledgeUploadTestTree(t *testing.T, projectPath string) []byte {
	t.Helper()
	relative := make([]string, 0, 16)
	walkErr := filepath.WalkDir(projectPath, func(path string, entry fs.DirEntry, failure error) error {
		if failure != nil {
			return failure
		}
		if path == projectPath || entry.IsDir() {
			return nil
		}
		name := filepath.Base(path)
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if strings.HasSuffix(name, suffix) {
				return nil
			}
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", path)
		}
		rel, err := filepath.Rel(projectPath, path)
		if err != nil {
			return err
		}
		relative = append(relative, rel)
		return nil
	})
	if walkErr != nil {
		t.Fatalf("the project tree digest cannot walk %s: %v", projectPath, walkErr)
	}
	sort.Strings(relative)
	digest := sha256.New()
	for _, rel := range relative {
		body, err := os.ReadFile(filepath.Join(projectPath, rel))
		if err != nil {
			t.Fatalf("the project tree digest cannot read %s: %v", rel, err)
		}
		digest.Write([]byte(rel))
		digest.Write([]byte{0})
		digest.Write(body)
		digest.Write([]byte{0})
	}
	return digest.Sum(nil)
}

// productionKnowledgeUploadSnapshotAbsent requires that no import ever wrote the
// canonical snapshot, which is the only file an import creates.
func productionKnowledgeUploadSnapshotAbsent(t *testing.T, projectPath string) {
	t.Helper()
	file := filepath.Join(projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName)
	if _, err := os.Lstat(file); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a refusal wrote a canonical snapshot: %v", err)
	}
}

// productionKnowledgeUploadDocuments is the snapshot's documents of one corpus,
// so a test that names a corpus is not satisfied by a document of another.
func productionKnowledgeUploadDocuments(t *testing.T, projectPath, corpusKind string) []map[string]any {
	t.Helper()
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	documents := make([]map[string]any, 0, 2)
	for _, row := range productionKnowledgeRows(t, snapshot, productionKnowledgeTableDocuments) {
		if row["corpusKind"] == corpusKind {
			documents = append(documents, row)
		}
	}
	return documents
}

// productionKnowledgeUploadChunks is every chunk row of one document, ordered by
// chunkIndex, so a duplicated chunk cannot hide behind a document count.
func productionKnowledgeUploadChunks(t *testing.T, projectPath, documentID string) []map[string]any {
	t.Helper()
	snapshot := productionKnowledgeSnapshotOfProject(t, projectPath)
	rows := make([]map[string]any, 0, 4)
	for _, row := range productionKnowledgeRows(t, snapshot, productionKnowledgeTableChunks) {
		if row["docId"] == documentID {
			rows = append(rows, row)
		}
	}
	return rows
}

// TestProductionKnowledgeImportDocumentWritesTheServedSnapshot is the plain file
// arm: one staged upload becomes one canonical document, its copy, the
// name-derived chapter meta, and an FTS hit in the same capture a served search
// builds.
func TestProductionKnowledgeImportDocumentWritesTheServedSnapshot(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-kb-1", productionKnowledgeNamedFile, []byte(productionKnowledgeProse))
	answer := productionKnowledgeCall(t, lane.author, productionKnowledgeImportDocumentName,
		[]any{"upload-kb-1", lane.projectPath, lane.frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-document refused an importable file: %v", answer)
	}
	documentID, ok := answer["docId"].(string)
	if !ok || documentID == "" {
		t.Fatalf("kb:import-document returned no document identity: %v", answer)
	}
	if count := productionKnowledgeCount(t, answer["chunkCount"]); count != 2 {
		t.Fatalf("kb:import-document reported %d chunks, expected 2", count)
	}
	documents := productionKnowledgeUploadDocuments(t, lane.projectPath, productionKnowledgeCorpusKind)
	if len(documents) != 1 || documents[0]["id"] != documentID ||
		documents[0]["fileName"] != productionKnowledgeNamedFile ||
		productionKnowledgeCount(t, documents[0]["chunkCount"]) != 2 {
		t.Fatalf("the store holds %v", documents)
	}
	texts := productionKnowledgeTexts(t, productionKnowledgeUploadChunks(t, lane.projectPath, documentID))
	expected := productionKnowledgeProseChunks(t)
	if len(texts) != len(expected) || texts[0] != expected[0] || texts[1] != expected[1] {
		t.Fatalf("the stored chunk texts are not the reference's boundaries: %d chunks", len(texts))
	}
	if _, err := os.Stat(productionKnowledgeCopyPath(lane.projectPath, documentID)); err != nil {
		t.Fatalf("the import wrote no knowledge copy: %v", err)
	}
	if results := productionKnowledgeSearch(t, lane.projectPath, "走廊"); len(results) == 0 {
		t.Fatal("the imported document is not findable by the served FTS capture")
	}
}

// TestProductionKnowledgeImportDocumentReplayDoesNotDuplicate proves the prune
// rule survives the upload door: importing the same file twice leaves one
// document with its own chunk count, not two documents or four chunks.
func TestProductionKnowledgeImportDocumentReplayDoesNotDuplicate(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-kb-2", productionKnowledgeNamedFile, []byte(productionKnowledgeProse))
	first := productionKnowledgeCall(t, lane.author, productionKnowledgeImportDocumentName,
		[]any{"upload-kb-2", lane.projectPath, lane.frozen})
	second := productionKnowledgeCall(t, lane.author, productionKnowledgeImportDocumentName,
		[]any{"upload-kb-2", lane.projectPath, lane.frozen})
	if first["success"] != true || second["success"] != true {
		t.Fatalf("a replay was refused: %v %v", first, second)
	}
	documents := productionKnowledgeUploadDocuments(t, lane.projectPath, productionKnowledgeCorpusKind)
	if len(documents) != 1 {
		t.Fatalf("a replay left %d documents, expected the pruned one", len(documents))
	}
	documentID, _ := documents[0]["id"].(string)
	if chunks := productionKnowledgeUploadChunks(t, lane.projectPath, documentID); len(chunks) != 2 {
		t.Fatalf("a replay left %d chunk rows, expected 2", len(chunks))
	}
}

// TestProductionKnowledgeImportFolderImportsEveryTextMember proves the folder arm:
// each text member becomes its own document, a non-text member is skipped rather
// than read, and a replay of the same folder does not duplicate either document.
func TestProductionKnowledgeImportFolderImportsEveryTextMember(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	members := []productionPickerUploaded{
		{Relative: productionKnowledgeUploadFolderFile, Data: []byte(productionKnowledgeProseOne)},
		{Relative: "notes/second.md", Data: []byte(productionKnowledgeProseTwo)},
		{Relative: "cover.png", Data: productionKnowledgeUploadBinary},
	}
	lane.stageFolder("upload-folder-1", "references", members)
	answer := productionKnowledgeCall(t, lane.author, productionKnowledgeImportFolderName,
		[]any{"upload-folder-1", lane.projectPath, lane.frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-folder refused a text folder: %v", answer)
	}
	if imported := productionKnowledgeCount(t, answer["importedCount"]); imported != 2 {
		t.Fatalf("kb:import-folder imported %d files, expected the two text members", imported)
	}
	if failed, ok := answer["failedFiles"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("kb:import-folder reported failures: %v", answer["failedFiles"])
	}
	if documents := productionKnowledgeUploadDocuments(t, lane.projectPath,
		productionKnowledgeCorpusKind); len(documents) != 2 {
		t.Fatalf("the folder import left %d documents, expected 2", len(documents))
	}
	productionKnowledgeCall(t, lane.author, productionKnowledgeImportFolderName,
		[]any{"upload-folder-1", lane.projectPath, lane.frozen})
	if documents := productionKnowledgeUploadDocuments(t, lane.projectPath,
		productionKnowledgeCorpusKind); len(documents) != 2 {
		t.Fatalf("a folder replay left %d documents, expected 2", len(documents))
	}
}

// TestProductionKnowledgeImportReferenceTextUsesTheStableId proves the reference
// arm's identity rule: the document id is sha256("reference-import:"+stableKey),
// its corpus kind is the one the writing-context search excludes, and a second
// import of the same work is an idempotent receipt that writes nothing.
func TestProductionKnowledgeImportReferenceTextUsesTheStableId(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-ref-1", "第2章 剑.md", []byte(productionKnowledgeProse))
	answer := productionKnowledgeCall(t, lane.author, productionKnowledgeUploadReferenceName,
		[]any{"upload-ref-1", productionKnowledgeUploadReferenceKey, lane.frozen})
	if answer["success"] != true {
		t.Fatalf("kb:import-reference-text refused an importable work: %v", answer)
	}
	expectedID := productionKnowledgeReferenceDocumentID(productionKnowledgeUploadReferenceKey)
	if answer["docId"] != expectedID {
		t.Fatalf("the reference import used %v, expected the stable id %s", answer["docId"], expectedID)
	}
	if answer["idempotent"] != false {
		t.Fatalf("a first reference import was reported as a replay: %v", answer["idempotent"])
	}
	documents := productionKnowledgeUploadDocuments(t, lane.projectPath, productionKnowledgeReferenceCorpusKind)
	if len(documents) != 1 || documents[0]["id"] != expectedID ||
		documents[0]["corpusKind"] != productionKnowledgeReferenceCorpusKind {
		t.Fatalf("the store holds %v", documents)
	}
	if texts := productionKnowledgeTexts(t, productionKnowledgeUploadChunks(t, lane.projectPath, expectedID)); len(texts) != 2 {
		t.Fatalf("the reference document holds %d chunk rows, expected 2", len(texts))
	}
	snapshotFile := filepath.Join(lane.projectPath, projectfs.StorageDirectory, productionKnowledgeIndexName)
	before, err := os.ReadFile(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	replay := productionKnowledgeCall(t, lane.author, productionKnowledgeUploadReferenceName,
		[]any{"upload-ref-1", productionKnowledgeUploadReferenceKey, lane.frozen})
	if replay["success"] != true || replay["idempotent"] != true || replay["docId"] != expectedID {
		t.Fatalf("a replay was not an idempotent receipt: %v", replay)
	}
	after, err := os.ReadFile(snapshotFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("an idempotent replay rewrote the canonical snapshot")
	}
	if chunks := productionKnowledgeUploadChunks(t, lane.projectPath, expectedID); len(chunks) != 2 {
		t.Fatalf("a replay left %d chunk rows, expected 2", len(chunks))
	}
}

// TestProductionKnowledgeImportReferenceTextRepairsUnderTheSameId proves the other
// half of 'stable-id': different text under the same key replaces that document's
// own rows and prunes nothing, so the id never forks into two documents.
func TestProductionKnowledgeImportReferenceTextRepairsUnderTheSameId(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-ref-2", "第2章 剑.md", []byte(productionKnowledgeProseOne))
	productionKnowledgeCall(t, lane.author, productionKnowledgeUploadReferenceName,
		[]any{"upload-ref-2", productionKnowledgeUploadReferenceKey, lane.frozen})
	lane.stage("upload-ref-3", "第2章 剑.md", []byte(productionKnowledgeProseTwo))
	repaired := productionKnowledgeCall(t, lane.author, productionKnowledgeUploadReferenceName,
		[]any{"upload-ref-3", productionKnowledgeUploadReferenceKey, lane.frozen})
	if repaired["success"] != true || repaired["idempotent"] != false {
		t.Fatalf("a changed reference work was not re-imported: %v", repaired)
	}
	expectedID := productionKnowledgeReferenceDocumentID(productionKnowledgeUploadReferenceKey)
	if repaired["docId"] != expectedID {
		t.Fatalf("the repair minted %v instead of reusing %s", repaired["docId"], expectedID)
	}
	documents := productionKnowledgeUploadDocuments(t, lane.projectPath, productionKnowledgeReferenceCorpusKind)
	if len(documents) != 1 {
		t.Fatalf("the repair left %d reference documents, expected 1", len(documents))
	}
	texts := productionKnowledgeTexts(t, productionKnowledgeUploadChunks(t, lane.projectPath, expectedID))
	if len(texts) != 1 || texts[0] != productionKnowledgeProseTwo {
		t.Fatalf("the repaired document holds %d chunks", len(texts))
	}
}

// TestProductionKnowledgeImportUploadRefusalsWriteNothing is the refusal battery:
// an unknown upload, a foreign caller's upload, binary bytes, an oversized member,
// an unusable stable key and a crossed project path. Each must answer its own
// symbolic code and leave the project tree byte-identical.
func TestProductionKnowledgeImportUploadRefusalsWriteNothing(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	tree := productionKnowledgeUploadTestTree(t, lane.projectPath)
	productionKnowledgeRefusal(t, lane.author, "unowned upload", productionKnowledgeImportDocumentName,
		[]any{"upload-missing", lane.projectPath, lane.frozen},
		productionKnowledgeCodeUploadUnknown)
	lane.uploads.stage(lane.other.cookie.Value, lane.other.tab, "upload-foreign-kb", "chapter.md",
		[]byte(productionKnowledgeProse))
	productionKnowledgeRefusal(t, lane.author, "foreign upload", productionKnowledgeImportDocumentName,
		[]any{"upload-foreign-kb", lane.projectPath, lane.frozen},
		productionKnowledgeCodeUploadUnknown)
	lane.stage("upload-binary", "chapter.md", productionKnowledgeUploadBinary)
	productionKnowledgeRefusal(t, lane.author, "binary upload", productionKnowledgeImportDocumentName,
		[]any{"upload-binary", lane.projectPath, lane.frozen},
		productionKnowledgeCodeUploadNotText)
	lane.stage("upload-huge", "chapter.md", bytes.Repeat([]byte("a"), productionKnowledgeTextMaxBytes+1))
	productionKnowledgeRefusal(t, lane.author, "oversized upload", productionKnowledgeImportDocumentName,
		[]any{"upload-huge", lane.projectPath, lane.frozen},
		productionKnowledgeCodeUploadTooLarge)
	lane.stage("upload-key", "chapter.md", []byte(productionKnowledgeProse))
	productionKnowledgeRefusal(t, lane.author, "unusable stable key", productionKnowledgeUploadReferenceName,
		[]any{"upload-key", "not a key", lane.frozen},
		productionKnowledgeCodeReferenceKey)
	lane.stageFolder("upload-folder-binary-md", "references", []productionPickerUploaded{
		{Relative: "chapter.md", Data: productionKnowledgeUploadBinary},
	})
	productionKnowledgeRefusal(t, lane.author, "binary folder member", productionKnowledgeImportFolderName,
		[]any{"upload-folder-binary-md", lane.projectPath, lane.frozen},
		productionKnowledgeCodeUploadNotText)
	// A folder holding no text member is not a failure: the reference imports the
	// text files it finds, and there are none, so the honest answer is zero.
	lane.stageFolder("upload-folder-no-text", "images", []productionPickerUploaded{
		{Relative: "cover.png", Data: productionKnowledgeUploadBinary},
	})
	empty := productionKnowledgeCall(t, lane.author, productionKnowledgeImportFolderName,
		[]any{"upload-folder-no-text", lane.projectPath, lane.frozen})
	if empty["success"] != true || productionKnowledgeCount(t, empty["importedCount"]) != 0 {
		t.Fatalf("a folder with no text member answered %v", empty)
	}
	if fromTree := productionKnowledgeUploadTestTree(t, lane.projectPath); string(fromTree) != string(tree) {
		t.Fatal("a refused upload changed the project tree")
	}
	productionKnowledgeUploadSnapshotAbsent(t, lane.projectPath)
	// A crossed project path is the lease's refusal, not a business answer: it
	// never reaches the writer.
	response := lane.author.call(t, productionKnowledgeImportDocumentName,
		[]any{"upload-key", lane.projectPath + "-other", lane.frozen})
	if response.Code != http.StatusConflict ||
		!strings.Contains(response.Body.String(), "PROJECT_CONTEXT_CHANGED") {
		t.Fatalf("a crossed project path answered %d %s", response.Code, response.Body.String())
	}
}

// TestProductionKnowledgeUploadNamesWithoutAnOwner is the honest refusal this
// process gives while the lane installs no upload route: all three names are
// served, and each names the port it is missing instead of answering a fake
// success.
func TestProductionKnowledgeUploadNamesWithoutAnOwner(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, nil)
	tree := productionKnowledgeUploadTestTree(t, lane.projectPath)
	for _, binding := range []struct {
		name string
		args []any
	}{
		{productionKnowledgeImportDocumentName, []any{"upload-none", lane.projectPath, lane.frozen}},
		{productionKnowledgeImportFolderName, []any{"upload-none", lane.projectPath, lane.frozen}},
		{productionKnowledgeUploadReferenceName, []any{"upload-none", productionKnowledgeUploadReferenceKey, lane.frozen}},
	} {
		productionKnowledgeRefusal(t, lane.author, "unowned seam", binding.name, binding.args,
			productionKnowledgeCodeUploadUnavailable)
	}
	if after := productionKnowledgeUploadTestTree(t, lane.projectPath); string(after) != string(tree) {
		t.Fatal("an unowned seam changed the project tree")
	}
}

// TestProductionKnowledgeImportUploadRefusesAForeignTab proves the lease, not the
// operation name, is the authority: a tab that never opened the project cannot
// import even with a well-formed request.
func TestProductionKnowledgeImportUploadRefusesAForeignTab(t *testing.T) {
	lane := newProductionKnowledgeUploadTestLane(t, newProductionPickerFixtureUploads())
	lane.stage("upload-kb-foreign", productionKnowledgeNamedFile, []byte(productionKnowledgeProse))
	if code := lane.other.call(t, productionKnowledgeImportDocumentName,
		[]any{"upload-kb-foreign", lane.projectPath, lane.frozen}).Code; code == http.StatusOK {
		t.Fatal("a tab without the project lease imported a document")
	}
	productionKnowledgeUploadSnapshotAbsent(t, lane.projectPath)
}

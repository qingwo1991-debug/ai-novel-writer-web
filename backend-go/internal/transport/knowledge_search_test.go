package transport

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgedomain"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgequery"
	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
)

func searchTransportCapture(t *testing.T) *knowledgequery.SearchCapture {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	field := func(name, kind string) knowledgedomain.Field { return knowledgedomain.Field{Name: name, Type: kind} }
	snapshot := knowledgedomain.Snapshot{Tables: []knowledgedomain.Table{
		{Name: "documents", Version: 1, Schema: []knowledgedomain.Field{field("id", "utf8"), field("fileName", "utf8"), field("importedAt", "utf8"), field("chunkCount", "int32"), field("filePath", "utf8"), field("corpusKind", "utf8")}, Records: []map[string]any{{"id": "doc", "fileName": "source.txt", "importedAt": "2026-01-01", "chunkCount": json.Number("2"), "filePath": "/unmanaged/source.txt", "corpusKind": "reference"}}},
		{Name: "chunks", Version: 1, Schema: []knowledgedomain.Field{field("id", "utf8"), field("docId", "utf8"), field("fileName", "utf8"), field("text", "utf8"), field("chunkIndex", "int32"), field("totalChunks", "int32"), field("importedAt", "utf8"), field("corpusKind", "utf8")}, Records: []map[string]any{{"id": "a", "docId": "doc", "fileName": "source.txt", "text": "alpha beta", "chunkIndex": 0, "totalChunks": 2, "importedAt": "2026-01-01", "corpusKind": "reference"}, {"id": "b", "docId": "doc", "fileName": "source.txt", "text": "beta", "chunkIndex": 1, "totalChunks": 2, "importedAt": "2026-01-01", "corpusKind": "reference"}}},
	}}
	capture, err := knowledgequery.NewSearchCapture(context.Background(), snapshot, root, knowledgecopy.Budget{MaxMetadataBytes: 1024, MaxCopies: 10})
	if err != nil {
		t.Fatal(err)
	}
	return capture
}

func TestKnowledgeSearchOriginalDTOAndFrozenOwner(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	tab := "synthetic-kb-search-tab-0001"
	owner := draftOwner(cookie, tab)
	expected := draftContext("project", "lease", "/synthetic/book")
	capture := searchTransportCapture(t)
	var begins, releases atomic.Int64
	provider := KnowledgeSearchProviderFunc(func(ctx context.Context, caller Caller, raw any) (*knowledgequery.SearchCapture, func(), error) {
		begins.Add(1)
		if caller != owner || !reflect.DeepEqual(raw, expected) {
			return nil, nil, novelruntime.ErrNotOwned
		}
		return capture, func() { releases.Add(1) }, nil
	})
	if err := server.BindKnowledgeSearch(provider); err != nil {
		t.Fatal(err)
	}
	got := draftResult(t, draftRPC(t, server, cookie, csrf, tab, "kb:search", "alpha beta", nil, expected["projectPath"], expected))
	want := []any{map[string]any{"text": "alpha beta", "score": float64(0.5), "fileName": "source.txt"}, map[string]any{"text": "beta", "score": float64(0.5), "fileName": "source.txt"}}
	if !reflect.DeepEqual(got, want) || begins.Load() != 1 || releases.Load() != 1 {
		t.Fatal(got, begins.Load(), releases.Load())
	}
	if got := draftResult(t, draftRPC(t, server, cookie, csrf, tab, "kb:search", "alpha", 0, expected["projectPath"], expected)); !reflect.DeepEqual(got, []any{}) {
		t.Fatal(got)
	}
	knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, "synthetic-kb-search-tab-0002", "kb:search", "alpha", 5, expected["projectPath"], expected), 500, "PROJECT_SESSION_NOT_OWNED")
	bad := draftContext("project", "stale", "/synthetic/book")
	knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, tab, "kb:search", "alpha", 5, expected["projectPath"], bad), 500, "PROJECT_SESSION_NOT_OWNED")
	if releases.Load() != 2 {
		t.Fatal("successful searches did not release exactly once")
	}
}

func TestKnowledgeSearchArgumentsFailBeforeBorrowAndProviderErrorIsRedacted(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	tab := "synthetic-kb-search-tab-0001"
	expected := draftContext("project", "lease", "/synthetic/book")
	var begins atomic.Int64
	provider := KnowledgeSearchProviderFunc(func(context.Context, Caller, any) (*knowledgequery.SearchCapture, func(), error) {
		begins.Add(1)
		return nil, nil, errors.New("private /author/secret and API token")
	})
	if err := server.BindKnowledgeSearch(provider); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args   []any
		code   string
		status int
	}{
		{[]any{"alpha", 5, expected["projectPath"]}, "INVALID_ARGUMENTS", 400},
		{[]any{42, 5, expected["projectPath"], expected}, "INVALID_ARGUMENTS", 400},
		{[]any{"alpha", -1, expected["projectPath"], expected}, "INVALID_ARGUMENTS", 400},
		{[]any{"alpha", 101, expected["projectPath"], expected}, "INVALID_ARGUMENTS", 400},
		{[]any{"alpha", 1.5, expected["projectPath"], expected}, "INVALID_ARGUMENTS", 400},
		{[]any{"alpha", 5, "/synthetic/other", expected}, "PROJECT_CONTEXT_CHANGED", 409},
		{[]any{"alpha", 5, expected["projectPath"], nil}, "PROJECT_SESSION_REQUIRED", 400},
	} {
		knowledgeRequireResponse(t, draftRPC(t, server, cookie, csrf, tab, "kb:search", test.args...), test.status, test.code)
	}
	if begins.Load() != 0 {
		t.Fatal("invalid search borrowed capture")
	}
	response := draftRPC(t, server, cookie, csrf, tab, "kb:search", "alpha", 5, expected["projectPath"], expected)
	knowledgeRequireResponse(t, response, 500, "KNOWLEDGE_READ_FAILED")
	if begins.Load() != 1 {
		t.Fatal("provider not called once")
	}
}

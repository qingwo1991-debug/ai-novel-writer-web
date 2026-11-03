//go:build linux && lance_native && cgo

package authorruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/knowledgecopy"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
)

func TestNativeKnowledgeReadBorrowsProjectOpenLease(t *testing.T) {
	fixture := os.Getenv("GO_LANCE_CONTRACTROOT")
	if fixture == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("native copied fixture required")
		}
		t.Skip("GitHub synthetic Lance fixture required")
	}
	catalog, key, config, source, original := readFixture(t)
	if err := os.CopyFS(filepath.Join(config.ProjectPath, projectfs.StorageDirectory), os.DirFS(filepath.Join(fixture, "copy"))); err != nil {
		t.Fatal(err)
	}
	config.Knowledge = &projectaccess.KnowledgeConfig{
		Copy:     projectfs.KnowledgeBudget{MaxBytes: 128 << 20, MaxEntries: 10000, MaxDepth: 32},
		Metadata: knowledgecopy.Budget{MaxMetadataBytes: 8 << 20, MaxCopies: 100}, Native: true,
	}
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "cookie-native", ClientID: "tab-native"}
	expected, err := lifecycle.Open(context.Background(), owner, key, "open-native")
	if err != nil {
		t.Fatal(err)
	}
	borrow, err := lifecycle.BeginKnowledgeRead(context.Background(), owner, expected)
	if err != nil || borrow.View == nil || borrow.Search == nil || borrow.Release == nil {
		t.Fatal(err)
	}
	documents, err := borrow.View.Documents(context.Background())
	if err != nil || len(documents) != 3 {
		t.Fatal(documents, err)
	}
	results, err := borrow.Search.SearchFTS(context.Background(), "", 5, nil, nil)
	if err != nil || len(results) == 0 {
		t.Fatal(results, err)
	}
	borrow.Finalized["injected"] = 999
	borrow.Release()
	again, err := lifecycle.BeginKnowledgeRead(context.Background(), owner, expected)
	if err != nil {
		t.Fatal(err)
	}
	if again.View != borrow.View || again.Search != borrow.Search {
		t.Fatal("same project lease recaptured knowledge")
	}
	if _, exists := again.Finalized["injected"]; exists {
		t.Fatal("mutable finalized annotation escaped its borrow")
	}
	again.Release()
	foreign := projectsession.Owner{SessionID: "cookie-native", ClientID: "other-tab"}
	if other, err := lifecycle.BeginKnowledgeRead(context.Background(), foreign, expected); err == nil {
		if other.Release != nil {
			other.Release()
		}
		t.Fatal("foreign tab borrowed knowledge")
	}
	stale := expected
	stale.LeaseID = "stale"
	if other, err := lifecycle.BeginKnowledgeRead(context.Background(), owner, stale); err == nil {
		if other.Release != nil {
			other.Release()
		}
		t.Fatal("stale lease borrowed knowledge")
	}
	if err := lifecycle.Close(context.Background(), owner, expected); err != nil {
		t.Fatal(err)
	}
	if other, err := lifecycle.BeginKnowledgeRead(context.Background(), owner, expected); err == nil {
		if other.Release != nil {
			other.Release()
		}
		t.Fatal("closed project borrowed knowledge")
	}
	after, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("source database changed", err)
	}
}

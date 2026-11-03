package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

type navigationStub struct {
	recentCalls int
	treeCalls   int
	path        string
	caller      Caller
}

func (stub *navigationStub) RecentProjects(_ context.Context, caller Caller) ([]RecentNavigationProject, error) {
	stub.recentCalls++
	stub.caller = caller
	return []RecentNavigationProject{{Name: "Book", Path: "/library/Book", UpdatedAt: "2026-11-03T00:00:00Z", ProjectID: "book-id"}}, nil
}

func (stub *navigationStub) ListDirectory(_ context.Context, caller Caller, _ any, relative string) ([]projectfs.VisibleNode, error) {
	stub.treeCalls++
	stub.caller = caller
	stub.path = relative
	children := []projectfs.VisibleNode{}
	return []projectfs.VisibleNode{{Name: "empty", Path: "/library/Book/empty", IsDir: true, Children: &children}}, nil
}

func TestEditorNavigationBindingShapeAndPathBoundary(t *testing.T) {
	server := fixture(t)
	stub := &navigationStub{}
	if err := server.BindEditorNavigation(stub); err != nil {
		t.Fatal(err)
	}
	if err := server.BindEditorNavigation(stub); err == nil {
		t.Fatal("duplicate binding accepted")
	}
	if server.handlers["project:save"] != nil || server.handlers["fs:write-file"] != nil {
		t.Fatal("navigation exposed author writes")
	}
	caller := Caller{SessionID: "cookie", ClientID: "tab"}
	listed, err := server.handlers["project:recent-list"](context.Background(), caller, []any{})
	if err != nil || stub.recentCalls != 1 || stub.caller != caller {
		t.Fatalf("recent-list: %#v %v", listed, err)
	}
	wired, err := json.Marshal(listed)
	if err != nil || strings.Contains(string(wired), "key") || strings.Contains(string(wired), "previewCapabilityId") || !strings.Contains(string(wired), `"updatedAt"`) {
		t.Fatalf("recent-list DTO: %s %v", wired, err)
	}
	expected := map[string]any{"projectId": "book-id", "leaseId": "lease", "projectPath": "/library/Book"}
	tree, err := server.handlers["fs:list-dir"](context.Background(), caller, []any{"/library/Book/chapters", "/library/Book", expected})
	if err != nil || stub.treeCalls != 1 || stub.path != "chapters" {
		t.Fatalf("tree: %#v %v, relative=%q", tree, err, stub.path)
	}
	wired, err = json.Marshal(tree)
	if err != nil || !strings.Contains(string(wired), `"children":[]`) {
		t.Fatalf("tree DTO: %s %v", wired, err)
	}
	for _, args := range [][]any{
		{"/library/Other", "/library/Book", expected},
		{"/library/Book/../Other", "/library/Book", expected},
		{"/library/Book", "/library/Other", expected},
		{"/library/Book", "/library/Book", map[string]any{"projectId": "", "leaseId": "lease", "projectPath": "/library/Book"}},
		{"/library/Book", "/library/Book"},
	} {
		_, err := server.handlers["fs:list-dir"](context.Background(), caller, args)
		if err == nil {
			t.Fatalf("unsafe navigation accepted: %#v", args)
		}
	}
	if stub.treeCalls != 1 {
		t.Fatalf("unsafe path reached provider: %d", stub.treeCalls)
	}
	if _, err := server.handlers["project:recent-list"](context.Background(), caller, []any{"unexpected"}); err == nil {
		t.Fatal("recent-list accepted arguments")
	}
}

func TestEditorNavigationRequiresProviderAndCanonicalPath(t *testing.T) {
	server := fixture(t)
	var missing *navigationStub
	if err := server.BindEditorNavigation(missing); err == nil {
		t.Fatal("typed nil provider accepted")
	}
	if !editorCanonicalPath(filepath.Join("/", "library", "Book")) || editorCanonicalPath("relative") || editorCanonicalPath("/library/../escape") {
		t.Fatal("canonical path check failed")
	}
	public := editorNavigationError(security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE"), "FS_LIST_DIR_FAILED")
	if value, ok := public.(*security.HTTPError); !ok || value.Status != http.StatusConflict {
		t.Fatalf("public error not preserved: %v", public)
	}
}

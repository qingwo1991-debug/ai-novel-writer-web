//go:build linux

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The fixture below is the same disposable data root the file surface uses, with
// one difference that matters here: a second admitted project, so "removed one
// shelf entry" and "removed every project" cannot be confused, and a manuscript in
// the project whose shelf entry is removed, so the live lease can be read after.
const (
	productionRecentChapterPath = "manuscript/chapter-1.md"
	productionRecentChapterBody = "第一章：他推开门，门后有人。"
	productionRecentSecondName  = "second-novel"
	productionRecentSecondID    = "4d2e9a71-3c58-4f6b-8a10-7e5d2b9c4f63"
	productionRecentOpenToken   = "open-token-one"
	productionRecentReopenToken = "open-token-two"
	productionRecentOutsideBody = "sealed outside bytes no removal may reach"
)

// productionRecentWire is the shelf shape project:recent-list answers
// (internal/transport/editor_nav_reads.go:19-24).
type productionRecentWire struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	ProjectID string `json:"projectId"`
}

// productionRecentRemoveWire is the family's envelope for project:recent-remove:
// `{success:true}` and, on a refusal, `{success:false, error:"Error: CODE"}`
// (electron/controllers/project-controller.ts:896-904,
// internal/transport/projects.go:319-321).
type productionRecentRemoveWire struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// TestProductionRecentRemoveServesShelfRemoval is the whole write: one shelf entry
// removed through the frozen session, observed through the served list on a
// second connection and through the chooser, with every listed refusal answered by
// name and every project tree byte-identical before and after.
func TestProductionRecentRemoveServesShelfRemoval(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	library := filepath.Dir(projectPath)
	secondPath := filepath.Join(library, productionRecentSecondName)
	seedProductionProject(t, library, productionRecentSecondName, productionRecentSecondID,
		admission.CurrentSchemaVersion, productionDurableSchema)
	seedProductionDurableRows(t, secondPath)
	// A manuscript the live lease can still read once the shelf entry is gone:
	// removing navigation metadata must not revoke the project the author has open.
	if err := os.MkdirAll(filepath.Join(projectPath, filepath.Dir(productionRecentChapterPath)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, productionRecentChapterPath), []byte(productionRecentChapterBody), 0600); err != nil {
		t.Fatal(err)
	}
	// A tree outside the lane, one link that resolves to an admitted project and
	// one that leaves the library. None of them may be accepted as a shelf entry.
	outside := t.TempDir()
	outsideProject := filepath.Join(outside, "foreign-novel")
	if err := os.MkdirAll(outsideProject, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideProject, "manuscript.md"), []byte(productionRecentOutsideBody), 0600); err != nil {
		t.Fatal(err)
	}
	admittedLink := filepath.Join(root, "admitted-link")
	escapeLink := filepath.Join(root, "escape-link")
	if err := os.Symlink(projectPath, admittedLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideProject, escapeLink); err != nil {
		t.Fatal(err)
	}

	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	stopped := false
	defer func() {
		if !stopped {
			cleanup()
		}
	}()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, productionRecentOpenToken, projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %s", opened.Error)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == nil || frozen["leaseId"] == "" {
		t.Fatal("project:open returned no session lease")
	}
	beforeFirst := productionFSSnapshot(t, projectPath)
	beforeSecond := productionFSSnapshot(t, secondPath)
	beforeOutside := productionFSSnapshot(t, outside)

	// shelf reads the served list through the given client, sorted so neither the
	// assertion nor its failure message depends on the library's newest-first order.
	shelf := func(t *testing.T, client *productionDurableClient) []string {
		t.Helper()
		var recent []productionRecentWire
		productionEditorDecode(t, client, "project:recent-list", []any{}, &recent)
		paths := make([]string, 0, len(recent))
		for _, item := range recent {
			paths = append(paths, item.Path)
		}
		sort.Strings(paths)
		return paths
	}
	initial := shelf(t, author)
	if !reflect.DeepEqual(initial, []string{projectPath, secondPath}) {
		t.Fatalf("the shelf before any removal is %v; want both admitted projects", initial)
	}

	// refused asserts the family's business refusal: HTTP 200, success false, and
	// the binder's own symbolic code. An unknown id must never be answered as a
	// success, and no provider message or path may be reflected back.
	refused := func(t *testing.T, label string, args []any) {
		t.Helper()
		raw := productionEditorCall(t, author, productionRecentRemoveName, args)
		var wire productionRecentRemoveWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("%s: %v %s", label, err, raw)
		}
		if wire.Success {
			t.Fatalf("%s was answered as a success: %s", label, raw)
		}
		if wire.Error != "Error: PROJECT_RECENT_REMOVE_FAILED" {
			t.Fatalf("%s was refused as %s; want the family's named failure", label, wire.Error)
		}
	}
	// wrongArity asserts the transport refusal the binder owns: the family takes
	// exactly one argument (internal/transport/projects.go:311-313).
	wrongArity := func(t *testing.T, label string, args []any) {
		t.Helper()
		response := author.call(t, productionRecentRemoveName, args)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_ARGUMENTS") {
			t.Fatalf("%s answered %d %s; want 400 INVALID_ARGUMENTS", label, response.Code, response.Body.String())
		}
	}
	// A ".." spelling of an admitted path, built without cleaning it away: a
	// normalising matcher would accept this string, exact membership must not.
	traversal := library + string(filepath.Separator) + ".." + string(filepath.Separator) +
		filepath.Base(library) + string(filepath.Separator) + productionDurableProjectName

	t.Run("refusals", func(t *testing.T) {
		t.Run("unknown-id", func(t *testing.T) {
			refused(t, "a shelf entry this lane never admitted", []any{filepath.Join(library, "never-admitted")})
		})
		t.Run("foreign-project", func(t *testing.T) {
			refused(t, "a project from another tree", []any{outsideProject})
		})
		t.Run("absolute-path", func(t *testing.T) {
			refused(t, "an absolute path outside the library", []any{"/etc/passwd"})
		})
		t.Run("traversal", func(t *testing.T) {
			refused(t, "a traversal spelling of an admitted path", []any{traversal})
		})
		t.Run("trailing-slash", func(t *testing.T) {
			refused(t, "an admitted path with a trailing separator", []any{projectPath + string(filepath.Separator)})
		})
		t.Run("symlink-to-admitted", func(t *testing.T) {
			refused(t, "a symlink that resolves to an admitted project", []any{admittedLink})
		})
		t.Run("symlink-escape", func(t *testing.T) {
			refused(t, "a symlink that leaves the library", []any{escapeLink})
		})
		t.Run("wrong-arity", func(t *testing.T) {
			wrongArity(t, "no arguments", []any{})
			wrongArity(t, "two arguments", []any{projectPath, secondPath})
			wrongArity(t, "a non-string target", []any{float64(1)})
		})
	})

	t.Run("refusals-change-nothing", func(t *testing.T) {
		if after := shelf(t, author); !reflect.DeepEqual(initial, after) {
			t.Fatalf("a refused removal changed the served shelf: %v", after)
		}
		if after := productionFSSnapshot(t, projectPath); !reflect.DeepEqual(beforeFirst, after) {
			t.Fatal("a refused removal changed the project tree")
		}
		if after := productionFSSnapshot(t, secondPath); !reflect.DeepEqual(beforeSecond, after) {
			t.Fatal("a refused removal changed the other project's tree")
		}
		if after := productionFSSnapshot(t, outside); !reflect.DeepEqual(beforeOutside, after) {
			t.Fatal("a refused removal reached the tree outside the library")
		}
	})

	var removed productionRecentRemoveWire
	productionEditorDecode(t, author, productionRecentRemoveName, []any{projectPath}, &removed)
	if !removed.Success || removed.Error != "" {
		t.Fatalf("project:recent-remove refused an admitted shelf entry: %s", removed.Error)
	}

	t.Run("served-shelf-drops-the-entry", func(t *testing.T) {
		// A second connection: its own cookie session and its own tab, so this
		// shelf cannot be the first client's cached answer.
		reader := newProductionDurableClient(t, handler, productionDurableOtherTab)
		if after := shelf(t, reader); !reflect.DeepEqual(after, []string{secondPath}) {
			t.Fatalf("the served shelf after the removal is %v; want only the remaining project", after)
		}
		// The chooser is NOT the shelf: it lists what this lane admits, and removing
		// an entry from the recent list must never hide a book that is still on disk
		// (that is the difference between navigation metadata and a delete). Both
		// projects stay listed here.
		request := httptest.NewRequest(http.MethodGet, "/api/library", nil)
		request.Header.Set("X-Novel-User", "production-author")
		request.Header.Set("X-Client-Id", productionDurableOtherTab)
		request.AddCookie(reader.cookie)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("the chooser answered %d %s", response.Code, response.Body.String())
		}
		var chooser struct {
			Projects []struct {
				Path string `json:"path"`
			} `json:"projects"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &chooser); err != nil {
			t.Fatalf("the chooser answered %v", err)
		}
		seen := make([]string, 0, len(chooser.Projects))
		for _, project := range chooser.Projects {
			seen = append(seen, project.Path)
		}
		sort.Strings(seen)
		want := []string{secondPath, projectPath}
		sort.Strings(want)
		if !reflect.DeepEqual(seen, want) {
			t.Fatalf("the chooser after the removal is %v; want both admitted projects", seen)
		}
	})

	t.Run("project-and-lease-survive", func(t *testing.T) {
		// Not a deletion: both trees and the outside tree are byte-identical.
		if after := productionFSSnapshot(t, projectPath); !reflect.DeepEqual(beforeFirst, after) {
			t.Fatal("the removal changed the project tree it must leave alone")
		}
		if after := productionFSSnapshot(t, secondPath); !reflect.DeepEqual(beforeSecond, after) {
			t.Fatal("the removal changed the other project's tree")
		}
		if after := productionFSSnapshot(t, outside); !reflect.DeepEqual(beforeOutside, after) {
			t.Fatal("the removal reached the tree outside the library")
		}
		// The live lease was not revoked by a navigation write: the file read still
		// answers under the same frozen context.
		var readWire productionFSReadFileWire
		productionEditorDecode(t, author, productionFSReadFileName,
			[]any{productionRecentChapterPath, projectPath, frozen}, &readWire)
		if !readWire.Success || readWire.Content != productionRecentChapterBody {
			t.Fatalf("the removal revoked a live lease: %s", readWire.Error)
		}
	})

	t.Run("repeated-removal-is-idempotent", func(t *testing.T) {
		// The reference rewrites a navigation document without checking membership, so a
		// second removal of the same entry succeeds and changes nothing.
		var again productionRecentRemoveWire
		productionEditorDecode(t, author, productionRecentRemoveName, []any{projectPath}, &again)
		if !again.Success || again.Error != "" {
			t.Fatalf("a repeated removal was refused: %s", again.Error)
		}
		if entries := shelf(t, author); len(entries) != 1 || entries[0] != secondPath {
			t.Fatalf("the repeated removal changed the shelf to %v", entries)
		}
	})

	// The removal is navigation state for this process and nothing more. A restart
	// re-derives the shelf from the library, which no removal touched, so the
	// project is listed and openable again - which is what "not a delete" means
	// for a lane whose shelf is the admitted library itself.
	cleanup()
	stopped = true
	restarted, restartCleanup, restartErr := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if restartErr != nil {
		t.Fatalf("the lane did not reopen over the same data root: %v", restartErr)
	}
	defer restartCleanup()
	reader := newProductionDurableClient(t, restarted, productionDurableOtherTab)
	if after := shelf(t, reader); !reflect.DeepEqual(after, []string{projectPath, secondPath}) {
		t.Fatalf("a restart did not re-derive the shelf from the library: %v", after)
	}
	var reopened productionDurableOpenWire
	reader.decode(t, "project:open", []any{projectPath, productionRecentReopenToken, projectPath}, &reopened)
	if !reopened.Success || !reopened.DBReady || reopened.Error != "" {
		t.Fatalf("the removed project is no longer openable: %s", reopened.Error)
	}
}

// TestProductionRecentRemoveShelveNamesUnknownTarget pins the two properties the
// helper itself must keep. The removal is a string match against the admitted
// inventory, so it needs no library, no database and no handle - a zero-value lane
// performs it, which is why it cannot delete a project - and a target that is not
// in that inventory is refused BY NAME rather than answered as a success.
func TestProductionRecentRemoveShelveNamesUnknownTarget(t *testing.T) {
	open := &productionOpen{projects: []productionProject{
		{listed: projectfs.ListedProject{Name: "a", Path: "/shelf/a", ProjectID: "id-a"}},
		{listed: projectfs.ListedProject{Name: "b", Path: "/shelf/b", ProjectID: "id-b"}},
	}}
	provider := &productionRecentRemove{open: open}
	caller := transport.Caller{Principal: "production-author", SessionID: "session-1", ClientID: "client_1234567890"}

	// An unnamed caller never reaches the inventory: the shelf is global, so the
	// caller identity is the only authority this name can require.
	if _, err := provider.RemoveRecentNavigation(context.Background(), transport.Caller{}, "/shelf/a"); err == nil || err.Error() != "PROJECT_SESSION_REQUIRED" {
		t.Fatalf("an unnamed caller was not refused by name: %v", err)
	}
	closedLane := &productionRecentRemove{open: &productionOpen{closed: true}}
	if _, err := closedLane.RemoveRecentNavigation(context.Background(), caller, "/shelf/a"); err == nil || err.Error() != productionFSClosedCode {
		t.Fatalf("a closed lane was not refused by name: %v", err)
	}
	// The target must be one admitted string, byte for byte.
	for _, target := range []string{"", "/shelf/c", "/shelf/a/", "/shelf/../shelf/a", "/shelf/a" + string(filepath.Separator)} {
		if _, err := productionRecentRemoveShelve(open, target); err == nil || err.Error() != productionRecentTargetUnknown {
			t.Fatalf("the target %s was not refused by name: %v", target, err)
		}
	}
	result, err := provider.RemoveRecentNavigation(context.Background(), caller, "/shelf/a")
	if err != nil {
		t.Fatalf("removing an admitted shelf entry failed: %v", err)
	}
	wire, ok := result.(map[string]any)
	if !ok || wire["success"] != true {
		t.Fatalf("the removal did not answer the family's success envelope: %v", result)
	}
	// The lane still admits both projects: a shelf entry is navigation, so dropping it
	// may not withdraw the book from the library or from any of the family's proofs.
	if len(open.projects) != 2 {
		t.Fatalf("the removal changed the admitted inventory to %d entries; want both projects kept", len(open.projects))
	}
	// What the author sees shrinks: the served shelf, and only the served shelf.
	listed, err := open.RecentProjects(context.Background(), caller)
	if err != nil {
		t.Fatalf("read the shelf after the removal: %v", err)
	}
	if len(listed) != 1 || listed[0].Path != "/shelf/b" {
		t.Fatalf("the shelf after the removal is %v; want only the other project", listed)
	}
	// Removing it again is idempotent, the way the reference's navigation rewrite is:
	// the author's intent is already satisfied, so it is not an error.
	if _, err := productionRecentRemoveShelve(open, "/shelf/a"); err != nil {
		t.Fatalf("a repeated removal was refused: %v", err)
	}
	if _, err := productionRecentRemoveShelve(nil, "/shelf/b"); err == nil || err.Error() != productionFSClosedCode {
		t.Fatalf("a nil lane was not refused by name: %v", err)
	}
}

//go:build linux

package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The two subtests below run against a disposable t.TempDir() only: the writer
// lease, the author database and every removed byte live inside the test's own
// temporary tree, and no path outside it is read or written.
const (
	productionProjectDeleteUnknownID = "5c7d0f21-8a44-4b93-b6e2-9f0134ca77d5"
	productionProjectCreateName      = "新建之书"
)

// productionProjectDeleteWire is the browser's view of project:delete, spelled
// out here instead of reusing productionProjectDeleteResult so a renamed or
// dropped JSON field fails this test instead of silently agreeing with itself
// (src/shared/ipc-channels.ts:432 declares these five names).
type productionProjectDeleteWire struct {
	Success          bool   `json:"success"`
	DirectoryDeleted bool   `json:"directoryDeleted"`
	DatabaseRestored bool   `json:"databaseRestored"`
	Error            string `json:"error"`
	Warning          string `json:"warning"`
}

// productionProjectDeleteFixture opens the lane on a disposable root and returns
// the one admitted project with its live frozen session, exactly as the browser
// holds it after project:open.
func productionProjectDeleteFixture(t *testing.T) (*productionDurableClient, http.Handler, string, string, map[string]any) {
	t.Helper()
	root, projectPath := productionDurableDataRoot(t)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "open-token-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused the seeded project: %+v", opened)
	}
	lease, _ := opened.Project["sessionLease"].(string)
	if lease == "" || opened.Project["id"] != productionDurableProjectID || opened.Project["path"] != projectPath {
		t.Fatalf("project:open returned no usable identity: %v", opened.Project)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	return author, handler, root, projectPath, frozen
}

// TestProductionProjectLifecycleDeletesAdmittedProject is the delete half of the
// lifecycle: a project the lane really admitted is removed from disk, every
// refusal before the removal leaves the data root byte-identical, and the live
// lease - not the request - decides whether a removal may start.
func TestProductionProjectLifecycleDeletesAdmittedProject(t *testing.T) {
	author, handler, root, projectPath, frozen := productionProjectDeleteFixture(t)
	lease, _ := frozen["leaseId"].(string)
	library := filepath.Join(root, productionLibraryDirectory)

	// The baseline is the data root at rest: the lane is already open and the
	// project is already admitted, so anything a refusal changes shows up here.
	atRest := productionFSSnapshot(t, root)

	// A lease this server never issued, an id it does not hold, and a path the
	// startup inventory never admitted are three different refusals, and none of
	// them may touch a byte.
	refusals := []struct {
		label string
		args  []any
		code  string
	}{
		{
			label: "lease never issued",
			args: []any{projectPath, productionDurableProjectID, "00000000-0000-4000-8000-000000000000", map[string]any{
				"projectId": productionDurableProjectID, "projectPath": projectPath,
				"leaseId": "00000000-0000-4000-8000-000000000000",
			}},
			code: "PROJECT_SESSION_STALE",
		},
		{
			label: "project never admitted",
			args: []any{filepath.Join(library, "ghost-novel"), productionProjectDeleteUnknownID, lease, map[string]any{
				"projectId": productionProjectDeleteUnknownID, "projectPath": filepath.Join(library, "ghost-novel"),
				"leaseId": lease,
			}},
			code: "PROJECT_NOT_FOUND",
		},
		{
			label: "frozen session disagrees with its own scalars",
			args: []any{projectPath, productionProjectDeleteUnknownID, lease, frozen},
			code: "PROJECT_CONTEXT_CHANGED",
		},
	}
	for _, refusal := range refusals {
		t.Run(refusal.label, func(t *testing.T) {
			var refused productionProjectDeleteWire
			author.decode(t, "project:delete", refusal.args, &refused)
			if refused.Success || refused.DirectoryDeleted {
				t.Fatalf("project:delete removed a project on a %s: %+v", refusal.label, refused)
			}
			if refused.Error != refusal.code {
				t.Fatalf("project:delete answered %q; want %s", refused.Error, refusal.code)
			}
			if !refused.DatabaseRestored {
				t.Fatalf("a refusal taken before the close reported databaseRestored=false: %+v", refused)
			}
			if refused.Warning != "" {
				t.Fatalf("a refusal carried a warning: %+v", refused)
			}
		})
	}
	// Nothing above moved: the refused attempts deleted nothing, closed nothing
	// and wrote nothing, byte for byte.
	if after := productionFSSnapshot(t, root); !reflect.DeepEqual(atRest, after) {
		t.Fatalf("a refused project:delete changed the data root:\nbefore=%v\nafter=%v", atRest, after)
	}
	// The caller's session really survived those refusals, so the refusals were
	// taken before the fence and not by poisoning it.
	var full productionDurableDraftWire
	author.decode(t, "db:draft-get-full", []any{int64(1), projectPath, frozen}, &full)
	if full.Content != productionDurableSeed {
		t.Fatalf("the refused deletes disturbed the live author database: %q", full.Content)
	}
	// One project, one writer: a second tab cannot hold this project, which is why
	// the delete only has to account for the caller's own lease.
	other := newProductionDurableClient(t, handler, productionDurableOtherTab)
	var second productionDurableOpenWire
	other.decode(t, "project:open", []any{projectPath, "open-token-other", projectPath}, &second)
	if second.Success || second.Error != "PROJECT_OPEN_IN_ANOTHER_TAB" {
		t.Fatalf("a second tab was not refused the single-writer lease: %+v", second)
	}

	var deleted productionProjectDeleteWire
	author.decode(t, "project:delete", []any{projectPath, productionDurableProjectID, lease, frozen}, &deleted)
	if !deleted.Success || !deleted.DirectoryDeleted || deleted.Error != "" || deleted.Warning != "" {
		t.Fatalf("project:delete refused an admitted project: %+v", deleted)
	}
	// The database is not "restored": the handle is closed and this lane never
	// issues the caller a second lease behind the browser's back.
	if deleted.DatabaseRestored {
		t.Fatalf("project:delete claimed a restored database after closing it: %+v", deleted)
	}

	// The project is gone from disk, manifest and database included, and the rest
	// of the library is untouched.
	for _, relative := range []string{"", projectfs.StorageDirectory, filepath.Join(projectfs.StorageDirectory, projectfs.ManifestName)} {
		if _, statErr := os.Lstat(filepath.Join(projectPath, relative)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("project:delete left %q behind: %v", filepath.Join(projectPath, relative), statErr)
		}
	}
	if _, statErr := os.Stat(filepath.Join(library, "current-novel", projectfs.StorageDirectory, projectfs.DatabaseName)); statErr != nil {
		t.Fatalf("project:delete removed a project it was not asked about: %v", statErr)
	}
	// The transient tombstone is released too, so a delete leaves no residue.
	entries, readErr := os.ReadDir(root)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), productionProjectTombstonePrefix) {
			t.Fatalf("project:delete left %q behind in the data root", entry.Name())
		}
	}

	// project:open can no longer resolve it, and the shelf no longer names it.
	if response := author.call(t, "project:open", []any{projectPath, "open-token-2", projectPath}); response.Code != http.StatusNotFound ||
		!strings.Contains(response.Body.String(), "PROJECT_NOT_FOUND") {
		t.Fatalf("project:open still resolved the deleted project: %d %s", response.Code, response.Body.String())
	}
	if response := author.call(t, "project:recent-list", []any{}); strings.Contains(response.Body.String(), projectPath) {
		t.Fatalf("the recent shelf still names the deleted project: %s", response.Body.String())
	}
	// A second delete of the same project is a refusal, not a second removal.
	var again productionProjectDeleteWire
	author.decode(t, "project:delete", []any{projectPath, productionDurableProjectID, lease, frozen}, &again)
	if again.Success || again.DirectoryDeleted || again.Error != "PROJECT_NOT_FOUND" {
		t.Fatalf("project:delete answered a repeat call with %+v", again)
	}
}

// TestProductionProjectCreateStaysUnbound pins the other half of the lifecycle
// as an honest refusal instead of a fake success: create cannot mint a v7
// database and cannot register a project with the live catalog (the header of
// production_project_lifecycle_linux.go names the missing pieces), so the name
// keeps answering GO_OPERATION_NOT_MIGRATED and creates nothing at all.
func TestProductionProjectCreateStaysUnbound(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	library := filepath.Join(root, productionLibraryDirectory)
	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	author := newProductionDurableClient(t, handler, productionDurableTab)
	atRest := productionFSSnapshot(t, root)

	// The wire shape the browser sends for project:create
	// (src/shared/ipc-channels.ts:342): the create config, a request token and the
	// renderer's parent path.
	response := author.call(t, "project:create", []any{
		map[string]any{
			"name": productionProjectCreateName, "parentGrantId": "synthetic-parent-grant",
			"genre": "fantasy", "targetAudience": "adult",
		},
		"create-token-1",
		library,
	})
	if response.Code != http.StatusNotImplemented {
		t.Fatalf("project:create answered %d instead of staying unimplemented: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("project:create refused with a different code: %s", response.Body.String())
	}
	// A refusal that created a directory, a manifest or a database would be worse
	// than no implementation: the data root is byte-identical.
	if _, statErr := os.Stat(filepath.Join(library, productionProjectCreateName)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("project:create left a directory behind: %v", statErr)
	}
	if after := productionFSSnapshot(t, root); !reflect.DeepEqual(atRest, after) {
		t.Fatalf("the unimplemented project:create changed the data root:\nbefore=%v\nafter=%v", atRest, after)
	}
	// The lane still serves the project it admitted, so the refusals above are
	// the operation's own answer and not a broken lane.
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "open-token-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady {
		t.Fatalf("the lane could not open its admitted project: %+v", opened)
	}
}

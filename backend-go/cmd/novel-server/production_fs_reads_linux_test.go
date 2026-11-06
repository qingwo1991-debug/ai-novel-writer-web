//go:build linux

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// The fixture below lives entirely inside one disposable t.TempDir(): a synthetic
// production data root (productionDurableDataRoot), one browser-visible
// manuscript file, the storage marker fs:read-json is allowed to serve, and a
// second temporary tree outside the project that only a planted symlink can reach.
const (
	productionFSChapterPath = "manuscript/chapter-1.md"
	productionFSChapterBody = "第一章：他推开门，门后有人。"
	productionFSMissingPath = "manuscript/no-such-chapter.md"
	productionFSOutsideBody = "sealed outside bytes that no read may serve"
	productionFSMarkerJSON  = `{"version":1,"arcs":[{"name":"开篇","chapters":[1,2]}]}`
)

// productionFSReadFileWire is the family's business envelope for fs:read-file.
type productionFSReadFileWire struct {
	Success bool   `json:"success"`
	Content string `json:"content"`
	Error   string `json:"error"`
}

// TestProductionFSReadsServeConfinedProjectFiles is the read-only file surface of
// this lane: the three bound names answer 200 with the bytes the project really
// holds, every escape is refused without returning an outside byte and without
// writing anything, a stale lease is refused, and the seven names this file must
// not publish stay fail-closed at 501. Each bound operation has its own subtest,
// so a failure names the operation that broke.
func TestProductionFSReadsServeConfinedProjectFiles(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	// The manuscript and the storage marker exist before the lane is opened, so
	// the bytes the reads must return are the fixture's and not a write of theirs.
	if err := os.MkdirAll(filepath.Join(projectPath, filepath.Dir(productionFSChapterPath)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, productionFSChapterPath), []byte(productionFSChapterBody), 0600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(projectPath, projectfs.StorageDirectory, productionFSStorageMarkerName)
	if err := os.WriteFile(marker, []byte(productionFSMarkerJSON), 0600); err != nil {
		t.Fatal(err)
	}
	// A real outside tree plus two planted links inside the project: a file link
	// and a directory link. Both are the escape every confinement rule has to
	// refuse; neither may ever yield the outside bytes.
	outside := t.TempDir()
	outsideSecret := filepath.Join(outside, "outside-secret.txt")
	if err := os.WriteFile(outsideSecret, []byte(productionFSOutsideBody), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideSecret, filepath.Join(projectPath, "escape.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(projectPath, "escape-dir")); err != nil {
		t.Fatal(err)
	}

	handler, cleanup, err := productionHandlerWithContract(migrationRegistry(t), productionDurableAddress, "", root,
		productionConfig{Schema: productionDurableContract(t), MaxDatabaseBytes: productionMaxDatabaseBytes})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	author := newProductionDurableClient(t, handler, productionDurableTab)
	var opened productionDurableOpenWire
	author.decode(t, "project:open", []any{projectPath, "fs-open-1", projectPath}, &opened)
	if !opened.Success || !opened.DBReady || opened.Error != "" {
		t.Fatalf("project:open refused an admitted project: %+v", opened)
	}
	frozen := map[string]any{
		"projectId":   opened.Project["id"],
		"projectPath": opened.Project["path"],
		"leaseId":     opened.Project["sessionLease"],
	}
	if frozen["leaseId"] == nil || frozen["leaseId"] == "" {
		t.Fatalf("project:open returned no session lease: %v", opened.Project)
	}
	// The disk state is captured now, after the lease pinned the admitted bytes,
	// so every refusal below is measured against the same tree.
	before := productionFSSnapshot(t, projectPath)
	outsideBefore := productionFSSnapshot(t, outside)

	// fs:read-file: the exact seeded body, under the caller's live lease.
	t.Run("fs:read-file", func(t *testing.T) {
		var wire productionFSReadFileWire
		productionEditorDecode(t, author, productionFSReadFileName,
			[]any{productionFSChapterPath, projectPath, frozen}, &wire)
		if !wire.Success || wire.Error != "" {
			t.Fatalf("fs:read-file refused a confined file: %+v", wire)
		}
		if wire.Content != productionFSChapterBody {
			t.Fatalf("fs:read-file returned %q; want the seeded body %q", wire.Content, productionFSChapterBody)
		}
	})

	// fs:check-exists: a bare boolean, true for the seeded file and the seeded
	// directory, false for a path inside the project that does not exist yet -
	// including one whose parent directory does not exist either.
	t.Run("fs:check-exists", func(t *testing.T) {
		var file bool
		productionEditorDecode(t, author, productionFSCheckExistsName,
			[]any{productionFSChapterPath, projectPath, frozen}, &file)
		if !file {
			t.Fatalf("fs:check-exists reported the seeded file %q absent", productionFSChapterPath)
		}
		var directory bool
		productionEditorDecode(t, author, productionFSCheckExistsName,
			[]any{filepath.Dir(productionFSChapterPath), projectPath, frozen}, &directory)
		if !directory {
			t.Fatal("fs:check-exists reported the seeded directory absent")
		}
		var missing bool
		productionEditorDecode(t, author, productionFSCheckExistsName,
			[]any{productionFSMissingPath, projectPath, frozen}, &missing)
		if missing {
			t.Fatalf("fs:check-exists reported the unseeded path %q present", productionFSMissingPath)
		}
		var unwritten bool
		productionEditorDecode(t, author, productionFSCheckExistsName,
			[]any{"unwritten/deep/chapter-2.md", projectPath, frozen}, &unwritten)
		if unwritten {
			t.Fatal("fs:check-exists reported a path under a missing parent present")
		}
	})

	// fs:read-json: the storage marker this lane derives itself, with the marker's
	// real content - not a null and not the manifest.
	t.Run("fs:read-json", func(t *testing.T) {
		var wire struct {
			Success bool            `json:"success"`
			Data    json.RawMessage `json:"data"`
			Error   string          `json:"error"`
		}
		productionEditorDecode(t, author, productionFSReadJSONName, []any{marker, projectPath, frozen}, &wire)
		if !wire.Success || wire.Error != "" {
			t.Fatalf("fs:read-json refused the storage marker: %+v", wire)
		}
		var served, expected any
		if err := json.Unmarshal(wire.Data, &served); err != nil {
			t.Fatalf("fs:read-json returned %s: %v", wire.Data, err)
		}
		if err := json.Unmarshal([]byte(productionFSMarkerJSON), &expected); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(served, expected) {
			t.Fatalf("fs:read-json returned %s; want the marker's own content %s", wire.Data, productionFSMarkerJSON)
		}
	})

	// The one confinement refusal: an absolute path, a ".." traversal, a planted
	// file symlink and a planted directory symlink are all refused, the storage
	// name is out of reach, and none of it writes anything.
	t.Run("confinement", func(t *testing.T) {
		absolute := filepath.Join(outside, "outside-secret.txt")
		// fs:read-file answers the family's business envelope for a path it will
		// not open, exactly as the desktop did.
		for _, attempt := range []string{
			absolute,
			"../outside-secret.txt",
			"manuscript/../../outside-secret.txt",
			".ai-novel/project.json",
		} {
			raw := productionEditorCall(t, author, productionFSReadFileName, []any{attempt, projectPath, frozen})
			var wire productionFSReadFileWire
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("fs:read-file %q: %v %s", attempt, err, raw)
			}
			if wire.Success || wire.Content != "" {
				t.Fatalf("fs:read-file read %q: %s", attempt, raw)
			}
			if wire.Error != "PROJECT_FILE_PATH_REFUSED" {
				t.Fatalf("fs:read-file refused %q with %q; want PROJECT_FILE_PATH_REFUSED", attempt, wire.Error)
			}
			if strings.Contains(string(raw), productionFSOutsideBody) {
				t.Fatalf("fs:read-file served outside bytes for %q: %s", attempt, raw)
			}
		}
		// A symlink inside the project is an unsafe entry, not a followed path.
		for _, attempt := range []string{"escape.md", "escape-dir/outside-secret.txt"} {
			raw := productionEditorCall(t, author, productionFSReadFileName, []any{attempt, projectPath, frozen})
			var wire productionFSReadFileWire
			if err := json.Unmarshal(raw, &wire); err != nil {
				t.Fatalf("fs:read-file %q: %v %s", attempt, err, raw)
			}
			if wire.Success || wire.Content != "" {
				t.Fatalf("fs:read-file followed the planted link %q: %s", attempt, raw)
			}
			if wire.Error != "PROJECT_FILE_UNSAFE" {
				t.Fatalf("fs:read-file refused %q with %q; want PROJECT_FILE_UNSAFE", attempt, wire.Error)
			}
			if strings.Contains(string(raw), productionFSOutsideBody) {
				t.Fatalf("fs:read-file served outside bytes through %q: %s", attempt, raw)
			}
		}
		// fs:check-exists answers the transport envelope for the same attempts.
		productionFSRefused(t, author, productionFSCheckExistsName, "absolute path",
			[]any{absolute, projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		productionFSRefused(t, author, productionFSCheckExistsName, "traversal",
			[]any{"../outside-secret.txt", projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		productionFSRefused(t, author, productionFSCheckExistsName, "hidden storage path",
			[]any{filepath.Join(projectfs.StorageDirectory, projectfs.ManifestName), projectPath, frozen},
			"PROJECT_FILE_PATH_REFUSED")
		productionFSRefused(t, author, productionFSCheckExistsName, "storage-reserved name",
			[]any{projectfs.ManifestName, projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		productionFSRefused(t, author, productionFSCheckExistsName, "file symlink",
			[]any{"escape.md", projectPath, frozen}, "PROJECT_FILE_UNSAFE")
		productionFSRefused(t, author, productionFSCheckExistsName, "directory symlink",
			[]any{"escape-dir/outside-secret.txt", projectPath, frozen}, "PROJECT_FILE_UNSAFE")
		// fs:read-json serves ONE derived storage path: any other file - an
		// outside path, the manifest beside the marker, a traversal out of the
		// storage directory - is refused before the runtime is asked for anything.
		productionFSRefused(t, author, productionFSReadJSONName, "absolute path",
			[]any{absolute, projectPath, frozen}, "PROJECT_CONTEXT_REQUIRED")
		productionFSRefused(t, author, productionFSReadJSONName, "manifest",
			[]any{filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.ManifestName), projectPath, frozen},
			"PROJECT_CONTEXT_REQUIRED")
		productionFSRefused(t, author, productionFSReadJSONName, "traversal",
			[]any{filepath.Join(projectPath, projectfs.StorageDirectory, "..", projectfs.ManifestName), projectPath, frozen},
			"PROJECT_CONTEXT_REQUIRED")

		if after := productionFSSnapshot(t, projectPath); !reflect.DeepEqual(before, after) {
			t.Fatal("a confinement refusal changed the admitted project tree")
		}
		if after := productionFSSnapshot(t, outside); !reflect.DeepEqual(outsideBefore, after) {
			t.Fatal("a confinement refusal wrote into the tree outside the project")
		}
	})

	// One stale-lease refusal: a lease this server never issued reads nothing,
	// under the same envelope each name uses.
	t.Run("stale-lease", func(t *testing.T) {
		stale := map[string]any{
			"projectId": frozen["projectId"], "projectPath": projectPath, "leaseId": "stale-lease",
		}
		raw := productionEditorCall(t, author, productionFSReadFileName,
			[]any{productionFSChapterPath, projectPath, stale})
		var wire productionFSReadFileWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("fs:read-file: %v %s", err, raw)
		}
		if wire.Success || wire.Content != "" || wire.Error != "PROJECT_SESSION_STALE" {
			t.Fatalf("fs:read-file read a file under a stale lease: %s", raw)
		}
		productionFSRefused(t, author, productionFSCheckExistsName, "stale lease",
			[]any{productionFSChapterPath, projectPath, stale}, "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED")
		productionFSRefused(t, author, productionFSReadJSONName, "stale lease",
			[]any{marker, projectPath, stale}, "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED")
	})

	// The two names this file must not publish, one subtest each: fs:write-json and
	// the absolute-path listing. fs:write-file and fs:mkdir are bound by the write
	// batch, which proves their confinement against the same adapter this file
	// exercises, and the three fs:grant-* names are bound by the browser grant
	// batch (production_fs_grants_linux.go), whose own file proves the allowlist
	// that replaces the desktop's native picker.
	// fs:write-json left this list the same way fs:list-dir did: the lane now binds
	// it with the same confinement the reads here prove, and its own file asserts the
	// refusals (production_fs_write_json_linux_test.go). What this file still proves
	// is that the two names cannot be answered by an empty call: an unbound name is a
	// 501 and a bound one is an argument error, and neither is allowed to look like a
	// successful write.
	for _, operation := range []string{
		"fs:write-json",
	} {
		operation := operation
		t.Run("not-answerable-empty/"+operation, func(t *testing.T) {
			response := author.call(t, operation, []any{})
			if response.Code == http.StatusOK {
				t.Fatalf("%s answered an empty call: %s", operation, response.Body.String())
			}
			if response.Code == http.StatusNotImplemented &&
				strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
				t.Fatalf("%s is unbound, which the served lane must not answer", operation)
			}
		})
	}

	// Nothing above wrote a byte: the project tree and the outside tree are
	// exactly as they were captured under the live lease.
	t.Run("nothing-written", func(t *testing.T) {
		after := productionFSSnapshot(t, projectPath)
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("the file reads changed the project tree: %v then %v", before, after)
		}
		if outsideAfter := productionFSSnapshot(t, outside); !reflect.DeepEqual(outsideBefore, outsideAfter) {
			t.Fatalf("the file reads changed the outside tree: %v then %v", outsideBefore, outsideAfter)
		}
	})
}

// productionFSRefused asserts one transport-envelope refusal: not 200, one of the
// symbolic codes this lane promises, and no byte from outside the project.
func productionFSRefused(t *testing.T, client *productionDurableClient, operation, label string, args []any, want ...string) {
	t.Helper()
	response := client.call(t, operation, args)
	if response.Code == http.StatusOK {
		t.Fatalf("%s answered 200 for a %s: %s", operation, label, response.Body.String())
	}
	if strings.Contains(response.Body.String(), productionFSOutsideBody) {
		t.Fatalf("%s leaked bytes from outside the project: %s", operation, response.Body.String())
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %s: %v %s", operation, label, err, response.Body.String())
	}
	for _, code := range want {
		if body.Error.Code == code {
			return
		}
	}
	t.Fatalf("%s refused a %s with %q; want one of %v: %s", operation, label, body.Error.Code, want, response.Body.String())
}

// productionFSSnapshot records every entry under root by relative path: a regular
// file by content digest, a symlink by its target text. SQLite's sidecars are
// skipped because a read-write open legitimately creates or truncates them; no
// other entry is ignored, so a read that created, removed or replaced anything
// visible fails the comparison.
func productionFSSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if relative == "." || entry.IsDir() {
			return nil
		}
		for _, suffix := range []string{"-wal", "-shm", "-journal"} {
			if strings.HasSuffix(relative, suffix) {
				return nil
			}
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			snapshot[relative] = "symlink->" + target
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(data)
		snapshot[relative] = hex.EncodeToString(digest[:])
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

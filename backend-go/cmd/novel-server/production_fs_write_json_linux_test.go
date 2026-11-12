//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// The fixture below lives inside one disposable t.TempDir(): a synthetic
// production data root with two admitted projects, one manuscript file, and a
// directory outside the project that only a planted symlink can reach. Every
// refusal is measured against digests of that tree, not against the database.
const (
	productionJSONWritePath     = "structured/plot-outline.json"
	productionJSONChapterPath   = "manuscript/chapter-1.md"
	productionJSONChapterBody   = "第一章：他推开门，门后有人。"
	productionJSONOutsideBody   = "sealed outside bytes that no write may reach"
	productionJSONSecondName    = "second-novel"
	productionJSONSecondID      = "0b6f2c48-1d7e-4a53-9c02-6f8e1b3d5a70"
	productionJSONWriteOpenTok  = "json-open-1"
	productionJSONMarkerName    = "partial_arch.json"
	productionJSONOutsideSecret = "outside-secret.txt"
)

// productionJSONWriteWire is the family's business envelope for fs:write-json:
// `{success, error}` and no commitState (electron/controllers/fs-controller.ts:
// 117-125 covers only fs:write-file with one).
type productionJSONWriteWire struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// TestProductionFSWriteJSONServesConfinedDocument is the write half of the file
// surface: one document written through the frozen session, read back through the
// served read and from disk, and every listed refusal answered with the family's
// own envelope while the project tree stays byte-identical. Each refusal has its
// own subtest, so a failure names the escape that was not closed.
func TestProductionFSWriteJSONServesConfinedDocument(t *testing.T) {
	root, projectPath := productionDurableDataRoot(t)
	// A second admitted project, so "aim this lease at another tree" is answered
	// about a project this lane really knows.
	library := filepath.Dir(projectPath)
	secondPath := filepath.Join(library, productionJSONSecondName)
	seedProductionProject(t, library, productionJSONSecondName, productionJSONSecondID,
		admission.CurrentSchemaVersion, productionDurableSchema)
	seedProductionDurableRows(t, secondPath)
	if err := os.MkdirAll(filepath.Join(projectPath, filepath.Dir(productionJSONChapterPath)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, productionJSONChapterPath), []byte(productionJSONChapterBody), 0600); err != nil {
		t.Fatal(err)
	}
	// A real outside tree plus a planted directory link: the escape no write may
	// reach, and the tree whose digests must not move.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, productionJSONOutsideSecret), []byte(productionJSONOutsideBody), 0600); err != nil {
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
	author.decode(t, "project:open", []any{projectPath, productionJSONWriteOpenTok, projectPath}, &opened)
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
	// A lease this server never issued, under the same project and path.
	stale := map[string]any{
		"projectId": frozen["projectId"], "projectPath": frozen["projectPath"], "leaseId": "stale-lease",
	}
	// The disk state is captured under the live lease, so every refusal below is
	// measured against the same tree.
	before := productionFSSnapshot(t, projectPath)
	outsideBefore := productionFSSnapshot(t, outside)

	document := map[string]any{
		"version": float64(1),
		"arcs": []any{
			map[string]any{"name": "开篇", "chapters": []any{float64(1), float64(2)}},
		},
	}

	// refused asserts one business-envelope refusal: HTTP 200, success false, one
	// symbolic code. A transport error here would mean the request never reached
	// the handler, which is exactly what this name must not do for a bad path.
	refused := func(t *testing.T, label string, args []any, want string) {
		t.Helper()
		raw := productionEditorCall(t, author, productionFSWriteJSONName, args)
		var wire productionJSONWriteWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("%s: %v %s", label, err, raw)
		}
		if wire.Success || wire.Error != want {
			t.Fatalf("%s: success is %v and the code is %s; want %s", label, wire.Success, wire.Error, want)
		}
	}

	t.Run("refusals", func(t *testing.T) {
		t.Run("foreign-project", func(t *testing.T) {
			refused(t, "another project's tree", []any{productionJSONWritePath, document, secondPath, frozen}, "PROJECT_CONTEXT_CHANGED")
		})
		t.Run("absolute-path", func(t *testing.T) {
			refused(t, "an absolute path", []any{"/etc/passwd", document, projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		})
		t.Run("traversal", func(t *testing.T) {
			refused(t, "a traversal", []any{"../escape.json", document, projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		})

		t.Run("symlink-escape", func(t *testing.T) {
			refused(t, "a symlink out of the project", []any{"escape-dir/owned.json", document, projectPath, frozen}, "PROJECT_FILE_UNSAFE")
			if _, statErr := os.Stat(filepath.Join(outside, "owned.json")); statErr == nil {
				t.Fatal("a refused write created a file outside the project")
			}
		})
		t.Run("wrong-arity", func(t *testing.T) {
			refused(t, "a missing context", []any{productionJSONWritePath, document, projectPath}, "INVALID_ARGUMENTS")
			refused(t, "one argument too many", []any{productionJSONWritePath, document, projectPath, frozen, frozen}, "INVALID_ARGUMENTS")
			refused(t, "a non-string path", []any{float64(1), document, projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		})
		t.Run("stale-lease", func(t *testing.T) {
			refused(t, "a lease this server never issued", []any{productionJSONWritePath, document, projectPath, stale}, "PROJECT_SESSION_STALE")
		})
		t.Run("oversize", func(t *testing.T) {
			huge := strings.Repeat("a", int(productionJSONMaxBytes)+1)
			refused(t, "a document over the ceiling", []any{productionJSONWritePath, huge, projectPath, frozen}, "PROJECT_FILE_TOO_LARGE")
		})
	})

	t.Run("refusals-change-nothing", func(t *testing.T) {
		added, changed, removed := productionJSONSnapshotDiff(before, productionFSSnapshot(t, projectPath))
		if len(added)+len(changed)+len(removed) != 0 {
			t.Fatalf("a refused write changed the project tree: added %v, changed %v, removed %v", added, changed, removed)
		}
		if after := productionFSSnapshot(t, outside); !reflect.DeepEqual(outsideBefore, after) {
			t.Fatal("a refused write changed the tree outside the project")
		}
	})

	t.Run("write-and-read-back", func(t *testing.T) {
		var wire productionJSONWriteWire
		productionEditorDecode(t, author, productionFSWriteJSONName,
			[]any{productionJSONWritePath, document, projectPath, frozen}, &wire)
		if !wire.Success || wire.Error != "" {
			t.Fatalf("fs:write-json refused a confined document: %s", wire.Error)
		}
		onDisk, readErr := os.ReadFile(filepath.Join(projectPath, productionJSONWritePath))
		if readErr != nil {
			t.Fatalf("the document was not committed: %v", readErr)
		}
		if !json.Valid(onDisk) {
			t.Fatal("the committed bytes are not one JSON document")
		}
		var decoded any
		if err := json.Unmarshal(onDisk, &decoded); err != nil {
			t.Fatalf("the committed document does not parse: %v", err)
		}
		if !reflect.DeepEqual(decoded, document) {
			t.Fatalf("the committed document is not the one that was sent: %s", string(onDisk))
		}
		// JSON.stringify(data, null, 2): two-space indent, no trailing newline.
		if !strings.Contains(string(onDisk), "\n  \"arcs\": [") {
			t.Fatalf("the document is not two-space indented: %s", string(onDisk))
		}
		if strings.HasSuffix(string(onDisk), "\n") {
			t.Fatal("the document was committed with a trailing newline the reference does not write")
		}
		// The served read, over a separate request under the same frozen lease,
		// returns exactly the bytes on disk.
		var readWire productionFSReadFileWire
		productionEditorDecode(t, author, productionFSReadFileName,
			[]any{productionJSONWritePath, projectPath, frozen}, &readWire)
		if !readWire.Success || readWire.Content != string(onDisk) {
			t.Fatalf("the served read did not return the committed document: %s", readWire.Error)
		}
		// Exactly one new entry: the document. A leftover temporary file or a
		// created parent file would show up here.
		added, changed, removed := productionJSONSnapshotDiff(before, productionFSSnapshot(t, projectPath))
		if !reflect.DeepEqual(added, []string{productionJSONWritePath}) || len(changed)+len(removed) != 0 {
			t.Fatalf("the write changed more than the one document: added %v, changed %v, removed %v", added, changed, removed)
		}
	})

	t.Run("storage-marker", func(t *testing.T) {
		// The architecture checkpoint is served through its own fixed-name
		// writer, like fs:read-json serves it read-only. It must never go
		// through the general browser-file allowlist.
		marker := projectfs.StorageDirectory + "/" + productionJSONMarkerName
		raw := productionEditorCall(t, author, productionFSWriteJSONName, []any{marker, document, projectPath, frozen})
		var wire productionJSONWriteWire
		if err := json.Unmarshal(raw, &wire); err != nil {
			t.Fatalf("the storage marker: %v %s", err, raw)
		}
		if !wire.Success || wire.Error != "" {
			t.Fatalf("the storage marker: success is %v and the code is %s; want success", wire.Success, wire.Error)
		}
		if _, err := os.Stat(filepath.Join(projectPath, marker)); err != nil {
			t.Fatalf("the storage marker was not written: %v", err)
		}
	})

	t.Run("refused-overwrite-keeps-bytes", func(t *testing.T) {
		digest := productionFSSnapshot(t, projectPath)[productionJSONWritePath]
		if digest == "" {
			t.Fatal("the committed document is missing from the snapshot")
		}
		overwrite := map[string]any{"version": float64(2)}
		refused(t, "a stale lease replacing a committed document", []any{productionJSONWritePath, overwrite, projectPath, stale}, "PROJECT_SESSION_STALE")
		refused(t, "an absolute path replacing a committed document", []any{"/tmp/plot-outline.json", overwrite, projectPath, frozen}, "PROJECT_FILE_PATH_REFUSED")
		refused(t, "an oversize replacement", []any{productionJSONWritePath, strings.Repeat("b", int(productionJSONMaxBytes)+1), projectPath, frozen}, "PROJECT_FILE_TOO_LARGE")
		if after := productionFSSnapshot(t, projectPath)[productionJSONWritePath]; after != digest {
			t.Fatal("a refused write replaced an existing document")
		}
	})
}

// TestProductionJSONDocumentRefusesUnencodableValue pins the encode-and-validate
// step this name depends on: the bytes are one JSON document with no trailing
// newline, a value that is not representable as JSON is refused as
// INVALID_ARGUMENTS before any tree is touched, and the document ceiling stays
// strictly inside the transport's request envelope so that the size refusal is
// this name's own rather than the transport's.
func TestProductionJSONDocumentRefusesUnencodableValue(t *testing.T) {
	document, err := productionJSONDocument(map[string]any{"version": float64(1)})
	if err != nil {
		t.Fatalf("a plain object did not encode: %v", err)
	}
	if !json.Valid(document) {
		t.Fatal("the encoded bytes are not one JSON document")
	}
	if strings.HasSuffix(string(document), "\n") {
		t.Fatal("the encoded document kept the encoder's trailing newline")
	}
	if _, err := productionJSONDocument(make(chan int)); err == nil {
		t.Fatal("a value that is not representable as JSON was encoded")
	} else if code := productionFSCode(err); code != "INVALID_ARGUMENTS" {
		t.Fatalf("an unencodable document was refused as %s; want INVALID_ARGUMENTS", code)
	}
	if productionJSONMaxBytes <= 0 || productionJSONMaxBytes >= security.DefaultBodyLimit {
		t.Fatalf("the document ceiling %d is not inside the transport envelope %d",
			productionJSONMaxBytes, security.DefaultBodyLimit)
	}
}

// productionJSONSnapshotDiff names the entries that differ between two snapshots,
// so a failure says which file changed instead of printing two maps of digests.
func productionJSONSnapshotDiff(before, after map[string]string) ([]string, []string, []string) {
	var added, changed, removed []string
	for path, digest := range after {
		previous, present := before[path]
		switch {
		case !present:
			added = append(added, path)
		case previous != digest:
			changed = append(changed, path)
		}
	}
	for path := range before {
		if _, present := after[path]; !present {
			removed = append(removed, path)
		}
	}
	sort.Strings(added)
	sort.Strings(changed)
	sort.Strings(removed)
	return added, changed, removed
}

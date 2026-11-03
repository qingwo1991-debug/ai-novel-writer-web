//go:build linux

package projectcatalog

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

const manifest = `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"12345678-1234-4234-8234-123456789abc","createdAt":"2026-01-02T03:04:05.123Z","storageFormat":"ai-novel","storageVersion":1}`

func fixture(t *testing.T) (*projectfs.Library, *projectfs.Project, string, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "First Novel")
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("synthetic fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { library.Close() })
	project, err := library.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { project.Close() })
	metadata := t.TempDir()
	if err := os.Chmod(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	return library, project, path, metadata
}

func TestCatalogKeyRequiresAdmissionAndInvalidatesReplacement(t *testing.T) {
	library, project, path, metadata := fixture(t)
	store, err := Open(library, metadata)
	if err != nil {
		t.Fatal(err)
	}
	registered, err := store.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	if registered.Key == "" || registered.Name != "First Novel" || registered.Path != path {
		t.Fatalf("unexpected record: %#v", registered)
	}
	if _, err := store.Resolve(path); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("request path granted authority: %v", err)
	}
	resolved, err := store.Resolve(registered.Key)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ProjectID() != project.ProjectID() {
		t.Fatal("wrong project")
	}
	resolved.Close()
	// Same UUID and bytes under a new inode must revoke the old key.
	old := path + "-old"
	if err := os.Rename(path, old); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("synthetic fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if current, err := store.Resolve(registered.Key); err == nil {
		current.Close()
		t.Fatal("replaced project retained key")
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Key == "" || listed[0].Key == registered.Key {
		t.Fatalf("replacement did not get fresh key: %#v", listed)
	}
	if _, err := store.Register(project); err == nil {
		t.Fatal("stale capability re-registered")
	}
}

func TestCatalogPersistenceAndBoundedNavigation(t *testing.T) {
	library, project, _, metadata := fixture(t)
	store, err := Open(library, metadata)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(library, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Resolve(first.Key); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("process key persisted: %v", err)
	}
	listed, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Key == "" || listed[0].ProjectID != project.ProjectID() {
		t.Fatalf("missing navigation after restart: %#v", listed)
	}
	if err := reopened.Remove(first.Key); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("stale key removed record: %v", err)
	}
	activeKey := listed[0].Key
	if err := reopened.Remove(activeKey); err != nil {
		t.Fatal(err)
	}
	listed, err = reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("remove persisted incorrectly: %#v", listed)
	}
	if _, err := reopened.Resolve(activeKey); !errors.Is(err, ErrCatalogKey) {
		t.Fatalf("removed key resolved: %v", err)
	}
	info, err := os.Lstat(filepath.Join(metadata, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("metadata mode: %v", info.Mode())
	}
}

func TestCatalogRefusesCorruptOrLinkedMetadata(t *testing.T) {
	library, _, _, metadata := fixture(t)
	path := filepath.Join(metadata, fileName)
	for _, raw := range []string{`{`, `{"version":2,"recent":[]}`, `{"version":1,"recent":[{"name":"N","path":"/etc","projectId":"x","updatedAt":"2026-01-01T00:00:00Z"},{"name":"N","path":"/etc","projectId":"x","updatedAt":"2026-01-01T00:00:00Z"}]}`} {
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(library, metadata); err == nil {
			t.Fatalf("accepted corrupt metadata %q", raw)
		}
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte(`{"version":1,"recent":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(library, metadata); !errors.Is(err, ErrCatalogUnsafe) {
		t.Fatalf("symlink accepted: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(library, metadata); !errors.Is(err, ErrCatalogUnsafe) {
		t.Fatalf("hardlink accepted: %v", err)
	}
}

func TestMissingRecentCanBeRemovedWithoutOpeningPath(t *testing.T) {
	library, project, path, metadata := fixture(t)
	store, err := Open(library, metadata)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+"-moved"); err != nil {
		t.Fatal(err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Key == "" || listed[0].Key == initial.Key {
		t.Fatalf("missing entry did not receive removal key: %#v", listed)
	}
	if current, err := store.Resolve(listed[0].Key); err == nil {
		current.Close()
		t.Fatal("removal key opened missing project")
	}
	if err := store.Remove(listed[0].Key); err != nil {
		t.Fatal(err)
	}
	listed, err = store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 0 {
		t.Fatalf("missing entry remained: %#v", listed)
	}
}

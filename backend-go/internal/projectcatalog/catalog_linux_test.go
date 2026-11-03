//go:build linux

package projectcatalog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func TestRecoveryFailureRetainsOriginalCause(t *testing.T) {
	library, project, path, metadata := fixture(t)
	cause := errors.New("synthetic readback cleanup ownership")
	blocked := false
	store, err := OpenWithRecoveryCheck(library, metadata, func(gotPath, gotID string) error {
		if gotPath != path || gotID != project.ProjectID() {
			return ErrCatalogInvalid
		}
		if blocked {
			return cause
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Register(project); err != nil {
		t.Fatal(err)
	}
	blocked = true
	if _, err := store.List(); !errors.Is(err, ErrCatalogRecoveryPending) || !errors.Is(err, cause) {
		t.Fatalf("list hid recovery cause: %v", err)
	}
	if err := store.HoldRecovery(path, project.ProjectID()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PromoteVerifiedRecovery(context.Background(), project); !errors.Is(err, ErrCatalogRecoveryPending) || !errors.Is(err, cause) {
		t.Fatalf("promotion hid recovery cause: %v", err)
	}
}

func TestPromoteVerifiedRecoveryWithBindingPublishesAfterBinder(t *testing.T) {
	library, project, path, metadata := fixture(t)
	store, err := OpenWithRecoveryCheck(library, metadata, func(gotPath, gotID string) error {
		if gotPath != path || gotID != project.ProjectID() {
			return ErrCatalogRecoveryPending
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HoldRecovery(path, project.ProjectID()); err != nil {
		t.Fatal(err)
	}
	binderFailure := errors.New("synthetic binding failure")
	var rejectedKey string
	if got, err := store.PromoteVerifiedRecoveryWithBinding(context.Background(), project, func(key string, verified *projectfs.Project) (func(), error) {
		rejectedKey = key
		if store.holds[path] != project.ProjectID() || store.byPath[path] != initial.Key {
			t.Fatal("catalog changed before failed binder")
		}
		if err := verified.VerifyNoPortableAuthority(context.Background()); err != nil {
			t.Fatalf("binder did not receive a live verified project: %v", err)
		}
		return nil, binderFailure
	}); got.Key != "" || !errors.Is(err, binderFailure) {
		t.Fatalf("failed binder published key: %#v, %v", got, err)
	}
	if _, ok := store.keys[rejectedKey]; ok || store.holds[path] != project.ProjectID() || store.byPath[path] != initial.Key {
		t.Fatal("failed binder changed catalog state")
	}
	if _, err := store.List(); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("failed binder released hold: %v", err)
	}
	if got, err := store.PromoteVerifiedRecoveryWithBinding(context.Background(), project, func(string, *projectfs.Project) (func(), error) {
		return nil, nil
	}); got.Key != "" || !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("missing binder unlock published key: %#v, %v", got, err)
	}
	var boundKey string
	unlocked := false
	promoted, err := store.PromoteVerifiedRecoveryWithBinding(context.Background(), project, func(key string, verified *projectfs.Project) (func(), error) {
		boundKey = key
		if store.holds[path] != project.ProjectID() || store.byPath[path] != initial.Key {
			t.Fatal("catalog published before reader binding")
		}
		if !verified.Identity().Same(project.Identity()) {
			t.Fatal("binder received the wrong project identity")
		}
		return func() {
			if store.holds[path] != "" || store.byPath[path] != key {
				t.Fatal("reader unlocked before catalog publication")
			}
			unlocked = true
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !unlocked || promoted.Key != boundKey || promoted.Key == initial.Key || promoted.Key == rejectedKey {
		t.Fatalf("binding and promotion diverged: %#v, unlocked=%v", promoted, unlocked)
	}
	if opened, err := store.Resolve(initial.Key); opened != nil || !errors.Is(err, ErrCatalogKey) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("old key survived binding: %v", err)
	}
}

func TestPromoteVerifiedRecoveryKeepsHoldUntilNewKeyIsReady(t *testing.T) {
	library, oldProject, path, metadata := fixture(t)
	pending := true
	store, err := OpenWithRecoveryCheck(library, metadata, func(gotPath, gotID string) error {
		if gotPath != path || gotID != oldProject.ProjectID() {
			return ErrCatalogInvalid
		}
		if pending {
			return ErrCatalogRecoveryPending
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Initial registration predates the exchange and persists the navigation
	// record. The held promotion must not rewrite that record.
	pending = false
	initial, err := store.Register(oldProject)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(metadata, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HoldRecovery(path, oldProject.ProjectID()); err != nil {
		t.Fatal(err)
	}
	pending = true
	if got, err := store.PromoteVerifiedRecovery(context.Background(), oldProject); got.Key != "" || !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("pending recovery promoted a key: %#v, %v", got, err)
	}
	if got, err := store.PromoteVerifiedRecovery(nil, oldProject); got.Key != "" || !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("invalid context promoted a key: %#v, %v", got, err)
	}
	if err := os.Rename(path, path+"-retained-old"); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("confirmed synthetic fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement, err := library.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	pending = false
	if got, err := store.PromoteVerifiedRecovery(context.Background(), oldProject); got.Key != "" || !errors.Is(err, projectfs.ErrChanged) {
		t.Fatalf("stale project promoted a key: %#v, %v", got, err)
	}
	if _, err := store.List(); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("failed promotion removed hold: %v", err)
	}
	promoted, err := store.PromoteVerifiedRecovery(context.Background(), replacement)
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Key == "" || promoted.Key == initial.Key || promoted.Path != path || promoted.ProjectID != replacement.ProjectID() {
		t.Fatalf("replacement key or metadata invalid: %#v", promoted)
	}
	after, err := os.ReadFile(filepath.Join(metadata, fileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("promotion rewrote durable recent metadata")
	}
	if resolved, err := store.Resolve(initial.Key); resolved != nil || !errors.Is(err, ErrCatalogKey) {
		if resolved != nil {
			resolved.Close()
		}
		t.Fatalf("old key survived promotion: %v", err)
	}
	resolved, err := store.Resolve(promoted.Key)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Close()
}

func TestRecoveryCheckBlocksRekeyResolutionAndRemovalAcrossRestart(t *testing.T) {
	library, project, path, metadata := fixture(t)
	var mu sync.Mutex
	pending := false
	check := func(gotPath, gotID string) error {
		if gotPath != path || gotID != project.ProjectID() {
			return ErrCatalogInvalid
		}
		mu.Lock()
		defer mu.Unlock()
		if pending {
			return errors.New("unresolved synthetic exchange")
		}
		return nil
	}
	store, err := OpenWithRecoveryCheck(library, metadata, check)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := store.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.HoldRecovery(path, project.ProjectID()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	pending = true
	mu.Unlock()
	if err := store.ReleaseAfterVerifiedRecovery(project); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("released hold before journal confirmation: %v", err)
	}
	if _, err := store.List(); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("list issued key during unresolved exchange: %v", err)
	}
	if opened, err := store.Resolve(initial.Key); !errors.Is(err, ErrCatalogRecoveryPending) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("old key resolved during unresolved exchange: %v", err)
	}
	if _, err := store.Register(project); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("register bypassed recovery gate: %v", err)
	}
	if err := store.Remove(initial.Key); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("remove erased pending recovery metadata: %v", err)
	}
	reopened, err := OpenWithRecoveryCheck(library, metadata, check)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.List(); !errors.Is(err, ErrCatalogRecoveryPending) {
		t.Fatalf("restart repinned unresolved project: %v", err)
	}
	// Simulate a confirmed swap: the project path has the same ID but fresh
	// storage inodes. The external journal checker releases only after its own
	// durable SWITCHED/readback checks, represented by clearing pending here.
	if err := os.Rename(path, path+"-retained-old"); err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("confirmed synthetic fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	pending = false
	mu.Unlock()
	newProject, err := library.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	defer newProject.Close()
	if err := store.ReleaseAfterVerifiedRecovery(project); err == nil {
		t.Fatal("stale source capability released recovery hold")
	}
	if err := store.ReleaseAfterVerifiedRecovery(newProject); err != nil {
		t.Fatal(err)
	}
	if opened, err := store.Resolve(initial.Key); !errors.Is(err, ErrCatalogKey) {
		if opened != nil {
			opened.Close()
		}
		t.Fatalf("old source key survived confirmed replacement: %v", err)
	}
	if listed, err := store.List(); err != nil || len(listed) != 1 || listed[0].Key == initial.Key {
		t.Fatalf("released store did not repin replacement: %#v, %v", listed, err)
	}
	listed, err := reopened.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(listed) != 1 || listed[0].Key == "" || listed[0].Key == initial.Key {
		t.Fatalf("confirmed replacement did not receive new key: %#v", listed)
	}
	opened, err := reopened.Resolve(listed[0].Key)
	if err != nil {
		t.Fatal(err)
	}
	if opened.ProjectID() != project.ProjectID() {
		t.Fatal("wrong project re-admitted")
	}
	opened.Close()
}

func TestGuardedCatalogConstructorRequiresRecoveryCheck(t *testing.T) {
	library, project, _, metadata := fixture(t)
	if store, err := OpenWithRecoveryCheck(library, metadata, nil); store != nil || !errors.Is(err, ErrCatalogInvalid) {
		t.Fatalf("guarded constructor accepted nil checker: %#v, %v", store, err)
	}
	legacy, err := Open(library, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Register(project); err != nil {
		t.Fatalf("legacy synthetic constructor stopped working: %v", err)
	}
}

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

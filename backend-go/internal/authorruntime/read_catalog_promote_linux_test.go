//go:build linux

package authorruntime

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func pinnedRecoveryReader(t *testing.T, catalog *projectcatalog.Store, key string, config projectaccess.Config) *Reader {
	t.Helper()
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	project, err := catalog.Resolve(key)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	reader, err := openReadOnlyPinnedProjectUnderLock(context.Background(), held, project, config)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close recovery reader: %v", err)
			return
		}
		if err := held.Close(); err != nil {
			t.Errorf("close legacy lock: %v", err)
		}
	})
	return reader
}

func TestPromoteRecoveryReaderToCatalogKey(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader := pinnedRecoveryReader(t, catalog, key, config)
	if err := reader.promoteCatalogKey(context.Background(), catalog, key); err != nil {
		t.Fatal(err)
	}
	if err := reader.Verify(context.Background()); err != nil {
		t.Fatalf("promoted reader verification: %v", err)
	}
	if err := reader.promoteCatalogKey(context.Background(), catalog, key); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second promotion must be rejected: %v", err)
	}
	if err := reader.Verify(context.Background()); err != nil {
		t.Fatalf("second promotion changed established read authority: %v", err)
	}
}

func TestPromoteRecoveryReaderRejectsDifferentPinnedProject(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader := pinnedRecoveryReader(t, catalog, key, config)
	otherCatalog, otherKey, _, _, _ := readFixture(t)
	if err := reader.promoteCatalogKey(context.Background(), otherCatalog, otherKey); !errors.Is(err, ErrChanged) {
		t.Fatalf("different project key must fail closed: %v", err)
	}
	if err := reader.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("failed promotion retained catalog-free read authority: %v", err)
	}
}

func TestCatalogPromotionBindsRecoveryReaderBeforePublishingKey(t *testing.T) {
	_, _, config, _, _ := readFixture(t)
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = library.Close() })
	project, err := library.OpenProject(config.ProjectPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = project.Close() })
	metadata := t.TempDir()
	if err := os.Chmod(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := projectcatalog.OpenWithRecoveryCheck(library, metadata, func(path, id string) error {
		if path != project.Path() || id != project.ProjectID() {
			return projectcatalog.ErrCatalogInvalid
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	initial, err := catalog.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	reader := pinnedRecoveryReader(t, catalog, initial.Key, config)
	if err := catalog.HoldRecovery(project.Path(), project.ProjectID()); err != nil {
		t.Fatal(err)
	}
	promoted, err := catalog.PromoteVerifiedRecoveryWithBinding(context.Background(), project, func(key string, verified *projectfs.Project) (func(), error) {
		return reader.bindPromotedCatalogKey(context.Background(), catalog, key, verified)
	})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Key == "" || promoted.Key == initial.Key {
		t.Fatalf("catalog did not publish a fresh key: %#v", promoted)
	}
	if err := reader.Verify(context.Background()); err != nil {
		t.Fatalf("reader was not catalog-backed after publication: %v", err)
	}
}

func TestRecoveryReaderBindingRejectsForeignVerifiedProject(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader := pinnedRecoveryReader(t, catalog, key, config)
	otherCatalog, otherKey, _, _, _ := readFixture(t)
	foreign, err := otherCatalog.Resolve(otherKey)
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	unlock, err := reader.bindPromotedCatalogKey(context.Background(), catalog, "new-key", foreign)
	if unlock != nil || !errors.Is(err, ErrChanged) {
		t.Fatalf("foreign verified project bound reader: unlock=%t, err=%v", unlock != nil, err)
	}
	if err := reader.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("failed binder retained catalog-free authority: %v", err)
	}
}

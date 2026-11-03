//go:build linux

package authorruntime

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func secondProjectKey(t *testing.T, catalog *projectcatalog.Store, config projectaccess.Config, database []byte) string {
	t.Helper()
	path := filepath.Join(config.LibraryRoot, "synthetic-author-b")
	storage := filepath.Join(path, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	manifest := strings.Replace(fixtureManifest, "8b5929e6-cda4-43e2-a15c-f232571f0036", "54c6e0fa-a835-4a63-a09a-6b8a0e01f821", 1)
	for name, data := range map[string][]byte{projectfs.ManifestName: []byte(manifest), projectfs.DatabaseName: database} {
		if err := os.WriteFile(filepath.Join(storage, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	library, err := projectfs.OpenLibrary(config.LibraryRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	project, err := library.OpenProject(path)
	if err != nil {
		t.Fatal(err)
	}
	defer project.Close()
	recent, err := catalog.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	return recent.Key
}

func TestBorrowedReadOnlyAdmissionDoesNotReleaseCoordinatorLock(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	reader, err := OpenReadOnlyUnderLock(context.Background(), held, catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		t.Fatal("reader released borrowed lock", err)
	}
	if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("competing writer entered after read close: %v", err)
	}
	if _, err := OpenReadOnlyUnderLock(context.Background(), held, catalog, "unknown", config); !errors.Is(err, projectcatalog.ErrCatalogKey) {
		t.Fatalf("unknown key admitted: %v", err)
	}
	if err := held.VerifyLegacy(config.DataRoot); err != nil {
		t.Fatal("failed admission released borrowed lock", err)
	}
}

func TestReadLifecycleSwitchKeepsOwnerAndSourceAuthority(t *testing.T) {
	catalog, keyA, config, source, original := readFixture(t)
	keyB := secondProjectKey(t, catalog, config, original)
	config.ProjectPath = ""
	lifecycle, err := NewReadLifecycle(catalog, config)
	if err != nil {
		t.Fatal(err)
	}
	defer lifecycle.Shutdown()
	owner := projectsession.Owner{SessionID: "cookie-a", ClientID: "tab-a"}
	other := projectsession.Owner{SessionID: "cookie-b", ClientID: "tab-b"}
	first, err := lifecycle.Open(context.Background(), owner, keyA, "open-a")
	if err != nil {
		t.Fatal(err)
	}
	if first.ProjectPath != filepath.Dir(filepath.Dir(source)) || first.ProjectID == "" || first.LeaseID == "" {
		t.Fatalf("untrusted first context: %+v", first)
	}
	refreshed, err := lifecycle.Open(context.Background(), owner, keyA, "refresh-a")
	if err != nil || refreshed != first {
		t.Fatalf("same-project refresh lost lease: %+v %v", refreshed, err)
	}
	if _, err := lifecycle.Open(context.Background(), owner, "unknown-key", "bad-b"); !errors.Is(err, projectcatalog.ErrCatalogKey) {
		t.Fatalf("invalid candidate admitted: %v", err)
	}
	current, ok, err := lifecycle.Current(context.Background(), owner)
	if err != nil || !ok || current != first {
		t.Fatalf("failed switch lost A: %+v %v %v", current, ok, err)
	}
	if err := lifecycle.Read(context.Background(), owner, first, func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version != 7 {
			t.Fatalf("wrong admitted version: %d", version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := lifecycle.Read(context.Background(), other, first, func(*sql.Tx) error { t.Fatal("cross-owner read"); return nil }); err == nil {
		t.Fatal("cross-owner lease accepted")
	}
	second, err := lifecycle.Open(context.Background(), owner, keyB, "open-b")
	if err != nil {
		t.Fatal(err)
	}
	if second.ProjectPath == first.ProjectPath || second.ProjectID == first.ProjectID || second.LeaseID == first.LeaseID {
		t.Fatalf("switch reused old authority: %+v %+v", first, second)
	}
	if err := lifecycle.Read(context.Background(), owner, first, func(*sql.Tx) error { t.Fatal("stale lease read"); return nil }); err == nil {
		t.Fatal("stale A lease accepted")
	}
	if err := lifecycle.Close(context.Background(), owner, first); err == nil {
		t.Fatal("stale close removed B")
	}
	current, ok, err = lifecycle.Current(context.Background(), owner)
	if err != nil || !ok || current != second {
		t.Fatalf("stale close changed B: %+v %v %v", current, ok, err)
	}
	if err := lifecycle.Close(context.Background(), owner, second); err != nil {
		t.Fatal(err)
	}
	_, ok, err = lifecycle.Current(context.Background(), owner)
	if err != nil || ok {
		t.Fatalf("closed project still current: %v %v", ok, err)
	}
	if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("coordinator lock released before shutdown: %v", err)
	}
	if err := lifecycle.Shutdown(); err != nil {
		t.Fatal(err)
	}
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal("shutdown retained lock", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

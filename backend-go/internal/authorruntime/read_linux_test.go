//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/admission"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectaccess"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

const fixtureManifest = `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"8b5929e6-cda4-43e2-a15c-f232571f0036","createdAt":"2026-01-01T00:00:00.000Z","storageFormat":"ai-novel","storageVersion":1}`

func readFixture(t *testing.T) (*projectcatalog.Store, string, projectaccess.Config, string, []byte) {
	t.Helper()
	oracle := os.Getenv("GO_SQLITE_CONTRACT")
	if oracle == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original schema oracle required")
		}
		t.Skip("GitHub original schema oracle required")
	}
	file, err := os.Open(oracle)
	if err != nil {
		t.Fatal(err)
	}
	schema, parseErr := admission.ParseContract(file)
	if closeErr := file.Close(); parseErr == nil {
		parseErr = closeErr
	}
	if parseErr != nil {
		t.Fatal(parseErr)
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(oracle), "baseline-v7.db"))
	if err != nil {
		t.Fatal(err)
	}
	dataRoot := t.TempDir()
	if err := os.Chmod(dataRoot, 0700); err != nil {
		t.Fatal(err)
	}
	libraryPath := filepath.Join(dataRoot, "projects")
	projectPath := filepath.Join(libraryPath, "synthetic-author")
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{projectfs.ManifestName: []byte(fixtureManifest), projectfs.DatabaseName: data} {
		if err := os.WriteFile(filepath.Join(storage, name), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	library, err := projectfs.OpenLibrary(libraryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = library.Close() })
	project, err := library.OpenProject(projectPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = project.Close() })
	metadata := t.TempDir()
	if err := os.Chmod(metadata, 0700); err != nil {
		t.Fatal(err)
	}
	catalog, err := projectcatalog.Open(library, metadata)
	if err != nil {
		t.Fatal(err)
	}
	recent, err := catalog.Register(project)
	if err != nil {
		t.Fatal(err)
	}
	scratch := t.TempDir()
	if err := os.Chmod(scratch, 0700); err != nil {
		t.Fatal(err)
	}
	config := projectaccess.Config{DataRoot: dataRoot, LibraryRoot: libraryPath, ProjectPath: projectPath, ScratchParent: scratch, MaxDatabaseBytes: 64 << 20, Schema: schema}
	return catalog, recent.Key, config, filepath.Join(storage, projectfs.DatabaseName), data
}

func TestReadOnlyAdmissionKeepsLockAndLeavesSourceBytes(t *testing.T) {
	catalog, key, config, source, original := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("read lifetime released gateway lock: %v", err)
	}
	report, err := reader.Report()
	if err != nil || report.ProjectID == "" || report.SQLite.UserVersion != 7 {
		t.Fatalf("admission report: %+v %v", report, err)
	}
	if err := reader.Read(context.Background(), func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version != 7 {
			t.Fatalf("private schema version %d", version)
		}
		if _, err := tx.Exec("CREATE TABLE unauthorized_author_write(x INTEGER)"); err == nil {
			t.Fatal("read-only transaction admitted DDL")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(source)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatal("author database bytes changed", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed read accepted: %v", err)
	}
	entries, err := os.ReadDir(config.ScratchParent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("private snapshot leaked: %v %v", entries, err)
	}
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadOnlyAdmissionRejectsStaleKeyAndSourceDrift(t *testing.T) {
	catalog, key, config, source, _ := readFixture(t)
	if _, err := OpenReadOnly(context.Background(), catalog, config.ProjectPath, config); !errors.Is(err, projectcatalog.ErrCatalogKey) {
		t.Fatalf("client path admitted: %v", err)
	}
	held, err := writerlock.AcquireLegacy(config.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	file, err := os.OpenFile(source, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("noncooperating synthetic writer")); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := reader.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("source drift retained authority: %v", err)
	}
	called := false
	if err := reader.Read(context.Background(), func(*sql.Tx) error { called = true; return nil }); !errors.Is(err, ErrChanged) || called {
		t.Fatalf("stale source read: %v called=%v", err, called)
	}
}

func TestCloseWaitsForReadBeforeReleasingLegacyLock(t *testing.T) {
	catalog, key, config, _, _ := readFixture(t)
	reader, err := OpenReadOnly(context.Background(), catalog, key, config)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	readDone := make(chan error, 1)
	go func() {
		readDone <- reader.Read(context.Background(), func(*sql.Tx) error {
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("read never entered")
	}
	closed := make(chan error, 1)
	go func() { closed <- reader.Close() }()
	select {
	case err := <-closed:
		close(release)
		t.Fatalf("close passed in-flight read: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	if _, err := writerlock.AcquireLegacy(config.DataRoot); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("legacy lock released during read: %v", err)
	}
	close(release)
	select {
	case err := <-readDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("read did not finish")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("close did not finish")
	}
}

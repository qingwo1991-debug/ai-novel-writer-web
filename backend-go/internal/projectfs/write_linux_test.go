//go:build linux

package projectfs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func writeCandidateFixture(t *testing.T) (string, string, *Project) {
	t.Helper()
	dataRoot := t.TempDir()
	libraryPath := filepath.Join(dataRoot, "library")
	storagePath := filepath.Join(libraryPath, "Synthetic project", StorageDirectory)
	if err := os.MkdirAll(storagePath, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(storagePath, ManifestName), []byte(fixtureManifest))
	put(t, filepath.Join(storagePath, DatabaseName), []byte("synthetic source bytes; no SQLite transaction"))
	library, err := OpenLibrary(libraryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = library.Close() })
	project, err := library.OpenProject(filepath.Dir(storagePath))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = project.Close() })
	return dataRoot, storagePath, project
}

func TestSQLiteWriteCandidateOnlyPinsUnchangedSource(t *testing.T) {
	dataRoot, storage, project := writeCandidateFixture(t)
	lock, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	path := filepath.Join(storage, DatabaseName)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), lock, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := candidate.Verify(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Close(); err != nil {
		t.Fatal(err)
	}
	if err := candidate.Verify(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed candidate still admitted: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("candidate changed source: %v", err)
	}
	if err := lock.VerifyLegacy(dataRoot); err != nil {
		t.Fatalf("candidate closed caller-owned lock: %v", err)
	}
}

func TestSQLiteWriteCandidateRequiresExactLegacyLockAndOwnedTree(t *testing.T) {
	dataRoot, storage, project := writeCandidateFixture(t)
	private, err := writerlock.Acquire(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer private.Close()
	if candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), private, dataRoot); err == nil {
		candidate.Close()
		t.Fatal("different lock policy admitted")
	}
	legacy, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), legacy, filepath.Dir(dataRoot)); err == nil {
		candidate.Close()
		t.Fatal("wrong data root admitted")
	}
	if err := os.Chmod(filepath.Join(storage, DatabaseName), 0666); err != nil {
		t.Fatal(err)
	}
	if candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), legacy, dataRoot); err == nil {
		candidate.Close()
		t.Fatal("publicly writable database admitted")
	}
	if err := os.Chmod(filepath.Join(storage, DatabaseName), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(storage, 0777); err != nil {
		t.Fatal(err)
	}
	if candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), legacy, dataRoot); err == nil {
		candidate.Close()
		t.Fatal("publicly writable storage admitted")
	}
}

func TestSQLiteWriteCandidateRejectsLinkAndReplacement(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dataRoot, storage, project := writeCandidateFixture(t)
			lock, err := writerlock.AcquireLegacy(dataRoot)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			path := filepath.Join(storage, DatabaseName)
			saved := filepath.Join(storage, "saved")
			if err := os.Rename(path, saved); err != nil {
				t.Fatal(err)
			}
			if kind == "symlink" {
				err = os.Symlink("saved", path)
			} else {
				err = os.Link(saved, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), lock, dataRoot); err == nil {
				candidate.Close()
				t.Fatal("linked source admitted")
			}
		})
	}
	dataRoot, storage, project := writeCandidateFixture(t)
	lock, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	candidate, err := project.OpenSQLiteWriteCandidate(context.Background(), lock, dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	path := filepath.Join(storage, DatabaseName)
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	put(t, path, []byte("different synthetic file"))
	if err := candidate.Verify(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("replaced source retained admission: %v", err)
	}
}

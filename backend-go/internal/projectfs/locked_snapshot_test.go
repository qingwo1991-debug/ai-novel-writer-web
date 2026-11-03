package projectfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func lockedSnapshotFixture(t *testing.T) (string, string, string, *Project, *os.Root) {
	t.Helper()
	dataRoot := t.TempDir()
	library := filepath.Join(dataRoot, "projects")
	storage := filepath.Join(library, "Synthetic project", StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(storage, ManifestName), []byte(fixtureManifest))
	put(t, filepath.Join(storage, DatabaseName), []byte("synthetic author database"))
	_, project := openFixture(t, library, filepath.Dir(storage))
	destinationPath := t.TempDir()
	if err := os.Chmod(destinationPath, 0700); err != nil {
		t.Fatal(err)
	}
	destination, err := os.OpenRoot(destinationPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = destination.Close() })
	return dataRoot, library, storage, project, destination
}

func TestStageDatabaseUnderLegacyLockBindsLiveLockAndSource(t *testing.T) {
	dataRoot, _, storage, project, destination := lockedSnapshotFixture(t)
	held, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	ctx := context.Background()
	snapshot, err := project.StageDatabaseUnderLegacyLock(ctx, held, dataRoot, destination, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	staged, err := destination.ReadFile("snapshot.db")
	if err != nil || string(staged) != "synthetic author database" {
		t.Fatalf("private copy mismatch: %q, %v", staged, err)
	}
	if err := snapshot.VerifyUnchanged(ctx); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(storage, DatabaseName), []byte("changed synthetic bytes"))
	if err := snapshot.VerifyUnchanged(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestStageDatabaseUnderLegacyLockRejectsWrongOrClosedLock(t *testing.T) {
	dataRoot, _, _, project, destination := lockedSnapshotFixture(t)
	held, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	ctx := context.Background()
	if _, err := project.StageDatabaseUnderLegacyLock(ctx, held, filepath.Dir(dataRoot), destination, 1<<20); err == nil {
		t.Fatal("wrong root accepted")
	}
	if _, err := destination.Lstat("snapshot.db"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("wrong root staged data: %v", err)
	}
	snapshot, err := project.StageDatabaseUnderLegacyLock(ctx, held, dataRoot, destination, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.VerifyUnchanged(ctx); !errors.Is(err, writerlock.ErrUnsafe) {
		t.Fatalf("closed lock accepted: %v", err)
	}
}

func TestStageDatabaseUnderLegacyLockRequiresPortableAuthorityClear(t *testing.T) {
	dataRoot, _, _, project, destination := lockedSnapshotFixture(t)
	held, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	if err := os.WriteFile(filepath.Join(project.Path(), ".vela"), []byte("legacy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := project.StageDatabaseUnderLegacyLock(context.Background(), held, dataRoot, destination, 1<<20); err == nil {
		t.Fatal("portable authority marker accepted")
	}
	if _, err := destination.Lstat("snapshot.db"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("marker staged data: %v", err)
	}
}

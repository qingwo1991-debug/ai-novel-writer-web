//go:build linux

package projectpublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

const syntheticManifest = `{"schemaVersion":1,"kind":"ai-novel-project","projectId":"12345678-1234-4234-8234-123456789abc","createdAt":"2026-01-02T03:04:05.123Z","storageFormat":"ai-novel","storageVersion":1}`

type fixture struct {
	dataRoot    string
	projectPath string
	storage     string
	stageParent string
	library     *projectfs.Library
	lock        *writerlock.Lock
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dataRoot := t.TempDir()
	libraryPath := filepath.Join(dataRoot, "projects")
	projectPath := filepath.Join(libraryPath, "Synthetic book")
	storage := filepath.Join(projectPath, projectfs.StorageDirectory)
	if err := os.MkdirAll(storage, 0700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		projectfs.ManifestName:          syntheticManifest,
		projectfs.DatabaseName:          "synthetic db",
		projectfs.DatabaseName + "-wal": "synthetic WAL",
		"unknown-author-file.bin":       "must survive intact",
	} {
		if err := os.WriteFile(filepath.Join(storage, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, body := range map[string]string{
		"lancedb/data/fragment.bin":  "lance chunk",
		"knowledge-copies/copy.json": "copy text",
		"avatars/cover.png":          "avatar bytes",
	} {
		path := filepath.Join(storage, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	stageParent := filepath.Join(dataRoot, "private-publish")
	if err := os.Mkdir(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	library, err := projectfs.OpenLibrary(libraryPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = library.Close() })
	lock, err := writerlock.AcquireLegacy(dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return fixture{dataRoot, projectPath, storage, stageParent, library, lock}
}

func testLimits() Limits { return Limits{MaxFiles: 100, MaxBytes: 1 << 20, MaxDepth: 8} }

func TestStageCopiesCompleteStorageAndDeniesPublication(t *testing.T) {
	f := newFixture(t)
	if err := os.Chmod(filepath.Join(f.storage, "unknown-author-file.bin"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(f.storage, "avatars"), 0755); err != nil {
		t.Fatal(err)
	}
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		projectfs.DatabaseName:          "synthetic db",
		projectfs.DatabaseName + "-wal": "synthetic WAL",
		"unknown-author-file.bin":       "must survive intact",
		"lancedb/data/fragment.bin":     "lance chunk",
		"knowledge-copies/copy.json":    "copy text",
		"avatars/cover.png":             "avatar bytes",
	} {
		got, err := os.ReadFile(filepath.Join(stage, path))
		if err != nil || string(got) != want {
			t.Fatalf("%s: %q %v", path, got, err)
		}
	}
	for path, want := range map[string]os.FileMode{
		"unknown-author-file.bin": 0644,
		"avatars":                 0755,
		".":                       0700,
	} {
		mode, err := os.Stat(filepath.Join(stage, path))
		if err != nil || mode.Mode().Perm() != want {
			t.Fatalf("permission mismatch for %s: %v, %v", path, mode, err)
		}
	}
	parentMode, err := os.Stat(f.stageParent)
	if err != nil || parentMode.Mode().Perm() != 0700 {
		t.Fatalf("staging parent became public: %v, %v", parentMode, err)
	}
	if err := snapshot.VerifySource(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.VerifyPrivateCopy(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.PublicationAuthority(context.Background()); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("source publication granted: %v", err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private stage remained: %v", err)
	}
	if err := snapshot.VerifySource(context.Background()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closed snapshot accepted: %v", err)
	}
}

func TestStageDetectsSourceAndCopyChange(t *testing.T) {
	f := newFixture(t)
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "unknown-author-file.bin"), []byte("changed staged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.VerifyPrivateCopy(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed stage accepted: %v", err)
	}
	if err := os.WriteFile(filepath.Join(f.storage, "knowledge-copies", "copy.json"), []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.VerifySource(context.Background()); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed source accepted: %v", err)
	}
}

func TestStageRejectsLinkSpecialBudgetAndUntrustedParent(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "special", "setuid", "budget", "file-count", "public-stage", "source-stage", "ancestor-alias"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			limits := testLimits()
			parent := f.stageParent
			switch kind {
			case "symlink":
				if err := os.Symlink("unknown-author-file.bin", filepath.Join(f.storage, "alias")); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(filepath.Join(f.storage, "unknown-author-file.bin"), filepath.Join(f.storage, "alias")); err != nil {
					t.Fatal(err)
				}
			case "special":
				if err := syscall.Mkfifo(filepath.Join(f.storage, "pipe"), 0600); err != nil {
					t.Fatal(err)
				}
			case "setuid":
				if err := os.Chmod(filepath.Join(f.storage, "unknown-author-file.bin"), os.ModeSetuid|0600); err != nil {
					t.Fatal(err)
				}
			case "budget":
				limits.MaxBytes = 3
			case "file-count":
				limits.MaxFiles = 2
			case "public-stage":
				if err := os.Chmod(parent, 0755); err != nil {
					t.Fatal(err)
				}
			case "source-stage":
				parent = f.storage
			case "ancestor-alias":
				inside := filepath.Join(f.storage, "private-stage")
				if err := os.Mkdir(inside, 0700); err != nil {
					t.Fatal(err)
				}
				alias := filepath.Join(f.dataRoot, "storage-alias")
				if err := os.Symlink(f.storage, alias); err != nil {
					t.Fatal(err)
				}
				parent = filepath.Join(alias, "private-stage")
			}
			if snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, parent, limits); err == nil {
				_ = snapshot.Close()
				t.Fatal("unsafe stage accepted")
			}
			if kind == "ancestor-alias" {
				items, err := os.ReadDir(filepath.Join(f.storage, "private-stage"))
				if err != nil || len(items) != 0 {
					t.Fatalf("wrote through storage alias: %v, %d entries", err, len(items))
				}
			}
		})
	}
}

func TestCloseRetriesAfterStageNameReplacement(t *testing.T) {
	f := newFixture(t)
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	retained := stage + "-retained"
	if err := os.Rename(stage, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stage, 0700); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); !errors.Is(err, ErrChanged) {
		t.Fatalf("replacement cleanup accepted: %v", err)
	}
	if _, err := snapshot.PrivatePath(); !errors.Is(err, ErrChanged) {
		t.Fatalf("replaced stage path exposed: %v", err)
	}
	if err := os.Remove(stage); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(retained, stage); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatalf("retry cleanup failed: %v", err)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stage remained after retry: %v", err)
	}
}

func TestStageRequiresLiveExactLegacyLock(t *testing.T) {
	f := newFixture(t)
	if snapshot, err := Stage(context.Background(), f.lock, filepath.Dir(f.dataRoot), f.library, f.projectPath, f.stageParent, testLimits()); err == nil {
		_ = snapshot.Close()
		t.Fatal("wrong lock root accepted")
	}
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	if err := f.lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.VerifySource(context.Background()); err == nil {
		t.Fatal("closed lock accepted")
	}
}

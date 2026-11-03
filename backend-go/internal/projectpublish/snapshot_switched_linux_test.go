//go:build linux

package projectpublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

// This three-rename fixture models the final inode positions only. Production
// publication still requires a separate atomic exchange and journal.
func swapFixtureTrees(t *testing.T, source, stage string) {
	t.Helper()
	moving := stage + "-moving"
	for _, pair := range [][2]string{{source, moving}, {stage, source}, {moving, stage}} {
		if err := os.Rename(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func editedStageHash(t *testing.T, stage string) string {
	t.Helper()
	root, err := os.OpenRoot(stage)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, err := scan(context.Background(), root, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	return hashTreeEntries(entries)
}

func TestRetainSwitchedPreservesOldSourceAndIdentifiesNewSource(t *testing.T) {
	f := newFixture(t)
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, projectfs.DatabaseName), []byte("edited db"), 0600); err != nil {
		t.Fatal(err)
	}
	editedHash := editedStageHash(t, stage)
	swapFixtureTrees(t, f.storage, stage)
	if err := snapshot.Close(); !errors.Is(err, ErrChanged) {
		t.Fatalf("ordinary cleanup after exchange: %v", err)
	}
	if _, err := snapshot.PrivatePath(); !errors.Is(err, ErrChanged) {
		t.Fatalf("old source exposed as private copy: %v", err)
	}
	observation, err := snapshot.RetainSwitched(context.Background(), editedHash)
	if err != nil {
		t.Fatal(err)
	}
	if observation.NewSourcePath != f.storage || observation.OldSourcePath != stage || observation.NewSourceTreeHash != editedHash || observation.NewSourceInode == 0 {
		t.Fatalf("incorrect post-swap observation: %+v", observation)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{filepath.Join(f.storage, projectfs.DatabaseName): "edited db", filepath.Join(stage, projectfs.DatabaseName): "synthetic db"} {
		body, err := os.ReadFile(path)
		if err != nil || string(body) != want {
			t.Fatalf("tree lost after Close at %s: %q %v", path, body, err)
		}
	}
	if _, err := snapshot.RetainSwitched(context.Background(), editedHash); !errors.Is(err, ErrInvalid) {
		t.Fatalf("closed capability observed exchange: %v", err)
	}
}

func TestRetainSwitchedRejectsWrongDigestAndChangedOldSource(t *testing.T) {
	for _, kind := range []string{"digest", "old-source"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			stage, err := snapshot.PrivatePath()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(stage, projectfs.DatabaseName), []byte("edited db"), 0600); err != nil {
				t.Fatal(err)
			}
			editedHash := editedStageHash(t, stage)
			swapFixtureTrees(t, f.storage, stage)
			if kind == "digest" {
				editedHash = hashTreeEntries(snapshot.entries)
			} else if err := os.WriteFile(filepath.Join(stage, "unknown-author-file.bin"), []byte("tampered old source"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshot.RetainSwitched(context.Background(), editedHash); !errors.Is(err, ErrChanged) {
				t.Fatalf("invalid swapped trees accepted: %v", err)
			}
			if err := snapshot.Close(); !errors.Is(err, ErrChanged) {
				t.Fatalf("ordinary Close removed old source: %v", err)
			}
			if err := snapshot.Abandon(); err != nil {
				t.Fatalf("abandon uncertain exchange: %v", err)
			}
			if _, err := os.Stat(filepath.Join(stage, projectfs.DatabaseName)); err != nil {
				t.Fatalf("old source missing after failed observation: %v", err)
			}
		})
	}
}

func TestRetainSwitchedRejectsSameIDProjectRootReplacement(t *testing.T) {
	f := newFixture(t)
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, projectfs.DatabaseName), []byte("edited db"), 0600); err != nil {
		t.Fatal(err)
	}
	editedHash := editedStageHash(t, stage)
	swapFixtureTrees(t, f.storage, stage)
	retainedProject := f.projectPath + "-retained"
	if err := os.Rename(f.projectPath, retainedProject); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.projectPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(retainedProject, projectfs.StorageDirectory), f.storage); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.RetainSwitched(context.Background(), editedHash); !errors.Is(err, ErrChanged) {
		t.Fatalf("same-ID replacement project root accepted: %v", err)
	}
	if err := snapshot.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, projectfs.DatabaseName)); err != nil {
		t.Fatalf("old source lost: %v", err)
	}
}

func TestRetainSwitchedRejectsStageParentPathReplacement(t *testing.T) {
	f := newFixture(t)
	snapshot, err := Stage(context.Background(), f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, projectfs.DatabaseName), []byte("edited db"), 0600); err != nil {
		t.Fatal(err)
	}
	editedHash := editedStageHash(t, stage)
	swapFixtureTrees(t, f.storage, stage)
	retainedParent := f.stageParent + "-retained"
	if err := os.Rename(f.stageParent, retainedParent); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(retainedParent, filepath.Base(stage)), stage); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot.RetainSwitched(context.Background(), editedHash); !errors.Is(err, ErrChanged) {
		t.Fatalf("replaced stage-parent path accepted: %v", err)
	}
	if err := snapshot.Abandon(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stage, projectfs.DatabaseName)); err != nil {
		t.Fatalf("old source lost: %v", err)
	}
}

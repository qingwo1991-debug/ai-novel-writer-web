//go:build linux

package authorruntime

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectpublish"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func catalogRecoveryFixture(t *testing.T) (string, string, string, *writerlock.Lock) {
	t.Helper()
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	stageParent := filepath.Join(root, "publication")
	projectPath := filepath.Join(root, "projects", "story")
	if err := os.MkdirAll(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectPath, 0700); err != nil {
		t.Fatal(err)
	}
	held, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	return root, stageParent, projectPath, held
}

func TestCatalogJournalRecoveryCheckOnlyClearsDedicatedEmptyParent(t *testing.T) {
	root, stageParent, projectPath, held := catalogRecoveryFixture(t)
	check, err := CatalogJournalRecoveryCheck(context.Background(), held, root, stageParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(stageParent, "novel-private-publish-0123456789abcdef0123456789abcdef"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); err != nil {
		t.Fatalf("an unprepared private copy changed catalog authority: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageParent, "novel-exchange-invalid.prepared.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("malformed journal did not block catalog: %v", err)
	}
}

func TestCatalogJournalRecoveryCheckRejectsOrphanAndUnverifiedPrepared(t *testing.T) {
	root, stageParent, projectPath, held := catalogRecoveryFixture(t)
	check, err := CatalogJournalRecoveryCheck(context.Background(), held, root, stageParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	name := "novel-exchange-0123456789abcdef0123456789abcdef.prepared.json"
	marker := filepath.Join(stageParent, name+".switched")
	if err := os.WriteFile(marker, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("orphan switched marker did not block catalog: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stageParent, name), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("unverified prepared marker did not block catalog: %v", err)
	}
}

func TestCatalogJournalRecoveryCheckRejectsReplacementAndReleasedLock(t *testing.T) {
	root, stageParent, projectPath, held := catalogRecoveryFixture(t)
	check, err := CatalogJournalRecoveryCheck(context.Background(), held, root, stageParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stageParent, stageParent+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("replaced stage parent did not block catalog: %v", err)
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, "synthetic-project"); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("released legacy lock did not block catalog: %v", err)
	}
}

func TestCatalogJournalRecoveryCheckRequiresTrustedArguments(t *testing.T) {
	root, stageParent, projectPath, held := catalogRecoveryFixture(t)
	if check, err := CatalogJournalRecoveryCheck(nil, held, root, stageParent, nil); check != nil || !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("nil context accepted: %v", err)
	}
	if check, err := CatalogJournalRecoveryCheck(context.Background(), nil, root, stageParent, nil); check != nil || !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("nil lock accepted: %v", err)
	}
	check, err := CatalogJournalRecoveryCheck(context.Background(), held, root, stageParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(projectPath, ""); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("empty project ID accepted: %v", err)
	}
	if err := check(root, "synthetic-project"); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("data root accepted as project: %v", err)
	}
	var catalogCheck projectcatalog.RecoveryCheck = check
	if catalogCheck == nil {
		t.Fatal("bridge did not expose catalog callback")
	}
}

func TestCatalogRecoveryRequiresSwitchedMarkerAndDraftReadback(t *testing.T) {
	ctx := context.Background()
	projectPath, projectID := "/trusted/story", "synthetic-project"
	recovery := projectpublish.JournalRecovery{Class: projectpublish.JournalPrepared, JournalID: "journal", ProjectPath: projectPath}
	called := false
	readback := func(context.Context, string, string, projectpublish.JournalRecovery) error {
		called = true
		return nil
	}
	if err := catalogRecoveryAcceptSwitched(ctx, projectPath, projectID, recovery, readback); !errors.Is(err, ErrCatalogRecoveryUnverified) || called {
		t.Fatalf("prepared marker cleared catalog: %v", err)
	}
	recovery.Class = projectpublish.JournalSwitched
	if err := catalogRecoveryAcceptSwitched(ctx, projectPath, projectID, recovery, nil); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("switched marker alone cleared catalog: %v", err)
	}
	if err := catalogRecoveryAcceptSwitched(ctx, projectPath, projectID, recovery, readback); err != nil || !called {
		t.Fatalf("verified switched readback was rejected: %v", err)
	}
}

func TestCatalogRecoveryJournalBlocksOnlyItsValidatedProject(t *testing.T) {
	root, stageParent, storyPath, held := catalogRecoveryFixture(t)
	ctx := context.Background()
	otherPath := filepath.Join(filepath.Dir(storyPath), "other")
	const storyID = "8b5929e6-cda4-43e2-a15c-f232571f0036"
	const otherID = "12345678-1234-4234-8234-123456789abc"
	for path, id := range map[string]string{storyPath: storyID, otherPath: otherID} {
		storage := filepath.Join(path, projectfs.StorageDirectory)
		if err := os.MkdirAll(storage, 0700); err != nil {
			t.Fatal(err)
		}
		manifest := strings.Replace(fixtureManifest, storyID, id, 1)
		if err := os.WriteFile(filepath.Join(storage, projectfs.ManifestName), []byte(manifest), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("synthetic private db"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	library, err := projectfs.OpenLibrary(filepath.Dir(storyPath))
	if err != nil {
		t.Fatal(err)
	}
	defer library.Close()
	snapshot, err := projectpublish.Stage(ctx, held, root, library, storyPath, stageParent,
		projectpublish.Limits{MaxFiles: 100, MaxBytes: 1 << 20, MaxDepth: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	plan, err := projectpublish.PrepareExchangePlan(ctx, snapshot,
		projectpublish.HandleDrainFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	journal, err := projectpublish.PrepareExchangeJournal(ctx, plan)
	if err != nil {
		t.Fatal(err)
	}
	check, err := CatalogJournalRecoveryCheck(ctx, held, root, stageParent, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := check(storyPath, storyID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("PREPARED owner project was allowed: %v", err)
	}
	if err := check(otherPath, otherID); err != nil {
		t.Fatalf("unrelated project blocked by validated PREPARED journal: %v", err)
	}
	markerPath := filepath.Join(stageParent, journal.Name())
	originalMarker, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	relabelled := bytes.Replace(originalMarker, []byte(storyID), []byte(otherID), 1)
	if bytes.Equal(relabelled, originalMarker) {
		t.Fatal("journal did not contain the original owner ID")
	}
	if err := os.WriteFile(markerPath, relabelled, 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(storyPath, storyID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("relabelled journal bypassed its actual owner: %v", err)
	}
	if err := os.WriteFile(markerPath, originalMarker, 0600); err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	storage := filepath.Join(storyPath, projectfs.StorageDirectory)
	temporary := stage + "-moving"
	for _, pair := range [][2]string{{storage, temporary}, {stage, storage}, {temporary, stage}} {
		if err := os.Rename(pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := journal.MarkSwitched(ctx); err != nil {
		t.Fatal(err)
	}
	if err := check(otherPath, otherID); err != nil {
		t.Fatalf("unrelated project blocked by validated SWITCHED journal: %v", err)
	}
	if err := check(storyPath, storyID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("owner project cleared without readback: %v", err)
	}
	lateArtifact := filepath.Join(stageParent, "unknown-quarantine")
	withReadback, err := CatalogJournalRecoveryCheck(ctx, held, root, stageParent,
		func(context.Context, string, string, projectpublish.JournalRecovery) error {
			return os.WriteFile(lateArtifact, []byte("late"), 0600)
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := withReadback(storyPath, storyID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("artifact added during readback was missed: %v", err)
	}
	if err := os.Remove(lateArtifact); err != nil {
		t.Fatal(err)
	}
	withMarkerRewrite, err := CatalogJournalRecoveryCheck(ctx, held, root, stageParent,
		func(context.Context, string, string, projectpublish.JournalRecovery) error {
			return os.WriteFile(markerPath, []byte("{}"), 0600)
		})
	if err != nil {
		t.Fatal(err)
	}
	if err := withMarkerRewrite(storyPath, storyID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("same-name marker rewrite during readback was missed: %v", err)
	}
	if err := os.WriteFile(markerPath, originalMarker, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(storage, projectfs.DatabaseName), []byte("changed new source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(otherPath, otherID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("unknown switched journal did not block unrelated project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(stageParent, journal.Name()), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := check(otherPath, otherID); !errors.Is(err, ErrCatalogRecoveryUnverified) {
		t.Fatalf("malformed journal did not block unrelated project: %v", err)
	}
}

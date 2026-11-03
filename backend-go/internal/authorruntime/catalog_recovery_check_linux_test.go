//go:build linux

package authorruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectcatalog"
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

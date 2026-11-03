//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func preparedJournalFixture(t *testing.T) (fixture, *Snapshot, *ExchangeJournal) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PrepareExchangePlan(ctx, snapshot, HandleDrainFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		_ = snapshot.Close()
		t.Fatal(err)
	}
	journal, err := PrepareExchangeJournal(ctx, plan)
	if err != nil {
		_ = snapshot.Close()
		t.Fatal(err)
	}
	return f, snapshot, journal
}

func TestExchangeJournalPreparedAndSwitchedAreOnlyClassifications(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	preparedPath := filepath.Join(f.stageParent, journal.Name())
	info, err := os.Lstat(preparedPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		t.Fatalf("prepared marker not private: %v", err)
	}
	recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalPrepared || !recovery.PreparedMarked || recovery.SwitchedMarked {
		t.Fatalf("unexpected PREPARED observation: %+v %v", recovery, err)
	}
	if err := journal.MarkSwitched(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("SWITCHED marker accepted before any exchange: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("journal changed source bytes: %v", err)
	}
	// Simulate only the *visible result* of a future atomic namespace exchange
	// using disposable directories. The journal itself never renames anything.
	stagePath, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	spare := filepath.Join(f.dataRoot, "synthetic-old-storage")
	if err := os.Rename(f.storage, spare); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(stagePath, f.storage); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(spare, stagePath); err != nil {
		t.Fatal(err)
	}
	recovery, err = RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalSwitchedUnconfirmed || recovery.SwitchedMarked {
		t.Fatalf("switched but unmarked state misclassified: %+v %v", recovery, err)
	}
	if err := journal.MarkSwitched(ctx); err != nil {
		t.Fatal(err)
	}
	recovery, err = RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalSwitched || !recovery.SwitchedMarked {
		t.Fatalf("SWITCHED state misclassified: %+v %v", recovery, err)
	}
	if err := snapshot.parent.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExchangeJournalChangedOrTamperedStateNeverRestores(t *testing.T) {
	for _, change := range []string{"source-changed", "source-missing", "stage-missing", "journal-tampered", "marker-tampered", "foreign-name", "foreign-project"} {
		t.Run(change, func(t *testing.T) {
			f, snapshot, journal := preparedJournalFixture(t)
			defer snapshot.Close()
			ctx := context.Background()
			switch change {
			case "source-changed":
				if err := os.WriteFile(filepath.Join(f.storage, "unknown-author-file.bin"), []byte("changed after prepare"), 0600); err != nil {
					t.Fatal(err)
				}
			case "source-missing":
				if err := os.Rename(f.storage, filepath.Join(f.dataRoot, "detached-source")); err != nil {
					t.Fatal(err)
				}
			case "stage-missing":
				stagePath, err := snapshot.PrivatePath()
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(stagePath, filepath.Join(f.stageParent, "detached-stage")); err != nil {
					t.Fatal(err)
				}
			case "journal-tampered":
				if err := os.WriteFile(filepath.Join(f.stageParent, journal.Name()), []byte(`{"format":1}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "marker-tampered":
				if err := os.WriteFile(filepath.Join(f.stageParent, journal.Name()+".switched"), []byte(`{"format":1,"preparedHash":"wrong"}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "foreign-name":
				if _, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, "../foreign"); !errors.Is(err, ErrJournalInvalid) {
					t.Fatalf("foreign marker name accepted: %v", err)
				}
				return
			case "foreign-project":
				if _, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, "other-project", f.projectPath, f.stageParent, journal.Name()); !errors.Is(err, ErrJournalInvalid) {
					t.Fatalf("foreign project accepted: %v", err)
				}
				return
			}
			recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
			if change == "journal-tampered" || change == "marker-tampered" {
				if !errors.Is(err, ErrJournalInvalid) {
					t.Fatalf("tampered marker accepted: %+v %v", recovery, err)
				}
				return
			}
			if err != nil || recovery.Class != JournalUnknown {
				t.Fatalf("uncertain state misclassified: %+v %v", recovery, err)
			}
		})
	}
}

func TestExchangeJournalRejectsAliasedProjectParentBeforeSwitchedMarker(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	stagePath, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	spare := filepath.Join(f.dataRoot, "synthetic-old-storage")
	for _, move := range [][2]string{{f.storage, spare}, {stagePath, f.storage}, {spare, stagePath}} {
		if err := os.Rename(move[0], move[1]); err != nil {
			t.Fatal(err)
		}
	}
	detached := f.projectPath + "-detached"
	if err := os.Rename(f.projectPath, detached); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(detached, f.projectPath); err != nil {
		t.Fatal(err)
	}
	if err := journal.MarkSwitched(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("aliased project parent accepted: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.stageParent, journal.Name()+".switched")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SWITCHED marker created despite parent alias: %v", err)
	}
	_ = snapshot.parent.Close()
}

func TestExchangeJournalNormalizesRestrictiveProcessUmask(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	plan, err := PrepareExchangePlan(ctx, snapshot, HandleDrainFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	oldUmask := syscall.Umask(0777)
	defer syscall.Umask(oldUmask)
	journal, prepareErr := PrepareExchangeJournal(ctx, plan)
	syscall.Umask(oldUmask)
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	info, err := os.Lstat(filepath.Join(f.stageParent, journal.Name()))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal mode depends on process umask: %v %+v", err, info)
	}
	recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalPrepared {
		t.Fatalf("strict recovery cannot read normalized marker: %+v %v", recovery, err)
	}
}

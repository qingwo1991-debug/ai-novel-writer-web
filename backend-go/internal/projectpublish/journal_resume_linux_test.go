//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func restartedEditedJournalFixture(t *testing.T) (fixture, *writerlock.Lock, string, string, string) {
	t.Helper()
	f := newFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, projectfs.DatabaseName), []byte("edited private DB"), 0600); err != nil {
		t.Fatal(err)
	}
	journal, err := PrepareEditedExchangeJournal(ctx, snapshot,
		HandleDrainFunc(func(context.Context, string) error { return nil }),
		EditedDraftCASVerifierFunc(func(context.Context, EditedStageFacts) (EditedDraftCASProof, error) {
			return editedProof(), nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	name := journal.Name()
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.lock.Close(); err != nil {
		t.Fatal(err)
	}
	held, err := writerlock.AcquireLegacy(f.dataRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	return f, held, stage, name, snapshot.projectID
}

func moveEditedFixtureToSwitched(t *testing.T, f fixture, stage string) {
	t.Helper()
	spare := filepath.Join(f.dataRoot, "restart-confirm-spare")
	for _, move := range [][2]string{{f.storage, spare}, {stage, f.storage}, {spare, stage}} {
		if err := os.Rename(move[0], move[1]); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConfirmSwitchedAfterRestartPersistsOnlyExistingEditedExchange(t *testing.T) {
	f, held, stage, name, projectID := restartedEditedJournalFixture(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ConfirmSwitchedAfterRestart(ctx, held, f.dataRoot, projectID, f.projectPath, f.stageParent, name); !errors.Is(err, ErrChanged) {
		t.Fatalf("confirmed PREPARED without exchange: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.stageParent, name+".switched")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("premature switched marker: %v", err)
	}
	moveEditedFixtureToSwitched(t, f, stage)
	unconfirmed, err := RecoverExchangeJournal(ctx, held, f.dataRoot, projectID, f.projectPath, f.stageParent, name)
	if err != nil || unconfirmed.Class != JournalSwitchedUnconfirmed {
		t.Fatalf("expected swapped but unconfirmed state: %+v %v", unconfirmed, err)
	}
	confirmed, err := ConfirmSwitchedAfterRestart(ctx, held, f.dataRoot, projectID, f.projectPath, f.stageParent, name)
	if err != nil || confirmed.Class != JournalSwitched || !confirmed.SwitchedMarked ||
		confirmed.EditedDraftID != editedProof().DraftID || confirmed.VerifiedNewSourceTreeHash == "" ||
		confirmed.NewSourceDevice == 0 || confirmed.NewSourceInode == 0 {
		t.Fatalf("restart confirmation failed: %+v %v", confirmed, err)
	}
	after, err := os.ReadFile(filepath.Join(stage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(after, before) {
		t.Fatalf("old source changed during confirmation: %v", err)
	}
	if _, err := ConfirmSwitchedAfterRestart(ctx, held, f.dataRoot, projectID, f.projectPath, f.stageParent, name); !errors.Is(err, ErrChanged) {
		t.Fatalf("second confirmation was treated as new unconfirmed exchange: %v", err)
	}
}

func TestConfirmSwitchedAfterRestartRefusesChangedTreeAndWrongProject(t *testing.T) {
	f, held, stage, name, projectID := restartedEditedJournalFixture(t)
	moveEditedFixtureToSwitched(t, f, stage)
	if _, err := ConfirmSwitchedAfterRestart(context.Background(), held, f.dataRoot, "other-project", f.projectPath, f.stageParent, name); err == nil {
		t.Fatal("foreign project confirmed")
	}
	if err := os.WriteFile(filepath.Join(f.storage, "changed-after-swap.bin"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfirmSwitchedAfterRestart(context.Background(), held, f.dataRoot, projectID, f.projectPath, f.stageParent, name); !errors.Is(err, ErrChanged) {
		t.Fatalf("changed swapped tree confirmed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.stageParent, name+".switched")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("refused confirmation wrote marker: %v", err)
	}
}

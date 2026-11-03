//go:build linux

package projectpublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func TestAbortPreparedJournalRemovesOnlyCandidateAndMarker(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	before, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	stagePath := snapshot.stage
	markerPath := filepath.Join(f.stageParent, journal.Name())
	if err := AbortPreparedJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private stage survived successful abort: %v", err)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PREPARED marker survived successful abort: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || string(after) != string(before) {
		t.Fatalf("abort changed author source: %v", err)
	}
	entries, err := os.ReadDir(f.stageParent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "novel-abort-") {
			t.Fatalf("successful abort retained quarantine %q", entry.Name())
		}
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAbortPreparedJournalRejectsChangedSourceWithoutCleanup(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	defer snapshot.Close()
	markerPath := filepath.Join(f.stageParent, journal.Name())
	if err := os.WriteFile(filepath.Join(f.storage, projectfs.DatabaseName), []byte("changed source"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AbortPreparedJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name()); err == nil {
		t.Fatal("changed source allowed PREPARED abort")
	}
	if _, err := os.Stat(snapshot.stage); err != nil {
		t.Fatalf("changed-source abort removed stage: %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("changed-source abort removed marker: %v", err)
	}
}

func TestAbortPreparedJournalRejectsChangedStageWithoutCleanup(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	defer snapshot.Close()
	markerPath := filepath.Join(f.stageParent, journal.Name())
	if err := os.WriteFile(filepath.Join(snapshot.stage, projectfs.DatabaseName), []byte("changed private stage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := AbortPreparedJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name()); err == nil {
		t.Fatal("changed stage allowed PREPARED abort")
	}
	if _, err := os.Stat(snapshot.stage); err != nil {
		t.Fatalf("changed-stage abort removed stage: %v", err)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("changed-stage abort removed marker: %v", err)
	}
}

func TestAbortPreparedJournalRejectsInterruptedStageCleanup(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	defer snapshot.Close()
	markerPath := filepath.Join(f.stageParent, journal.Name())
	quarantine := filepath.Join(f.stageParent, "novel-abort-00000000000000000000000000000000")
	if err := os.Mkdir(quarantine, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(snapshot.stage, filepath.Join(quarantine, "stage")); err != nil {
		t.Fatal(err)
	}
	if err := AbortPreparedJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name()); err == nil {
		t.Fatal("interrupted stage cleanup allowed a second abort")
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("interrupted abort removed marker: %v", err)
	}
	if _, err := os.Stat(filepath.Join(quarantine, "stage")); err != nil {
		t.Fatalf("interrupted abort removed quarantined stage: %v", err)
	}
	recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalUnknown {
		t.Fatalf("interrupted abort was not unknown: %+v %v", recovery, err)
	}
}

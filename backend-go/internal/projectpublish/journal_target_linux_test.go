//go:build linux

package projectpublish

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInspectPreparedJournalTargetRequiresExactPinnedMarker(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	ctx := context.Background()
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	target, err := InspectPreparedJournalTarget(ctx, f.lock, f.dataRoot, f.stageParent, journal.Name())
	if err != nil || target.ProjectID != snapshot.projectID || target.ProjectPath != f.projectPath ||
		target.StagePath != stage || !validJournalHash(target.PreparedHash) {
		t.Fatalf("pinned target: %+v %v", target, err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := InspectPreparedJournalTarget(ctx, f.lock, f.dataRoot, f.stageParent, journal.Name())
	if err != nil || again != target {
		t.Fatalf("durable target changed after close: %+v %v", again, err)
	}
	if _, err := InspectPreparedJournalTarget(ctx, f.lock, f.dataRoot, f.stageParent,
		"novel-exchange-0123456789abcdef0123456789abcdef.prepared.json"); !errors.Is(err, ErrJournalInvalid) {
		t.Fatalf("unowned marker name accepted: %v", err)
	}
}

func TestInspectPreparedJournalTargetRejectsCorruptOrReplacedParent(t *testing.T) {
	f, snapshot, journal := preparedJournalFixture(t)
	defer snapshot.Close()
	ctx := context.Background()
	marker := filepath.Join(f.stageParent, journal.Name())
	if err := os.WriteFile(marker, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPreparedJournalTarget(ctx, f.lock, f.dataRoot, f.stageParent, journal.Name()); !errors.Is(err, ErrJournalInvalid) {
		t.Fatalf("malformed marker supplied owner: %v", err)
	}
	if err := os.Rename(f.stageParent, f.stageParent+"-replaced"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.stageParent, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPreparedJournalTarget(ctx, f.lock, f.dataRoot, f.stageParent, journal.Name()); err == nil {
		t.Fatal("missing marker in replacement parent supplied owner")
	}
}

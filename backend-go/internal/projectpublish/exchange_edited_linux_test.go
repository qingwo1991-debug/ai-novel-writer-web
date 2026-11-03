//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func preparedEditedFixture(t *testing.T) (fixture, *Snapshot, DraftEditEvidence, *ExchangeJournal) {
	t.Helper()
	f := privateDraftFixture(t)
	ctx := context.Background()
	snapshot, err := Stage(ctx, f.lock, f.dataRoot, f.library, f.projectPath, f.stageParent, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = snapshot.Close() })
	baseline := "old draft"
	evidence, err := snapshot.EditDraftCASPrivate(ctx, 1, "edited private body", 19, &baseline)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := PrepareEditedExchangeJournal(ctx, snapshot,
		HandleDrainFunc(func(context.Context, string) error { return nil }), evidence)
	if err != nil {
		t.Fatal(err)
	}
	return f, snapshot, evidence, journal
}

func hexEvidenceHash(value [32]byte) string {
	return hex.EncodeToString(value[:])
}

func TestEditedExchangePlanBindsPrivateCASJournalAndCurrentAuthority(t *testing.T) {
	f, snapshot, evidence, journal := preparedEditedFixture(t)
	ctx := context.Background()
	sourceBefore, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	checks := 0
	current := CurrentDraftAuthorityFunc(func(_ context.Context, facts CurrentDraftFacts) error {
		checks++
		if facts.ProjectID != evidence.projectID || facts.ProjectPath != f.projectPath || facts.DraftID != 1 ||
			facts.OperationHash != hexEvidenceHash(evidence.operationHash) ||
			facts.SourceTreeHash != hexEvidenceHash(evidence.SourceTreeSHA256) ||
			facts.EditedTreeHash != hexEvidenceHash(evidence.AfterStageTreeSHA256) {
			return ErrChanged
		}
		return nil
	})
	drains := 0
	plan, err := PrepareEditedExchangePlan(ctx, snapshot, evidence, journal, current,
		HandleDrainFunc(func(_ context.Context, projectPath string) error {
			drains++
			if projectPath != f.projectPath {
				return ErrChanged
			}
			return nil
		}))
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if checks != 4 || drains != 2 {
		t.Fatalf("currentness or handle drain not repeated: %d/%d", checks, drains)
	}
	if err := plan.Exchange(ctx); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("edited plan published source: %v", err)
	}
	sourceAfter, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(sourceBefore, sourceAfter) {
		t.Fatalf("edited plan changed author DB: %v", err)
	}
}

func TestEditedExchangePlanRejectsStaleAuthorityAndTamperedStage(t *testing.T) {
	_, snapshot, evidence, journal := preparedEditedFixture(t)
	ctx := context.Background()
	stale := false
	current := CurrentDraftAuthorityFunc(func(context.Context, CurrentDraftFacts) error {
		if stale {
			return ErrChanged
		}
		return nil
	})
	drain := HandleDrainFunc(func(context.Context, string) error { return nil })
	plan, err := PrepareEditedExchangePlan(ctx, snapshot, evidence, journal, current, drain)
	if err != nil {
		t.Fatal(err)
	}
	stale = true
	if err := plan.Verify(ctx); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale project lease accepted: %v", err)
	}
	stale = false
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "unknown-author-file.bin"), []byte("tampered outside SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := plan.Verify(ctx); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("non-SQLite stage mutation accepted: %v", err)
	}
	if _, err := PrepareEditedExchangePlan(ctx, snapshot, evidence, journal, current, drain); err == nil {
		t.Fatal("tampered stage accepted as a new plan")
	}
}

func TestEditedExchangePlanRejectsForeignEvidenceAndJournal(t *testing.T) {
	_, snapshot, evidence, journal := preparedEditedFixture(t)
	ctx := context.Background()
	current := CurrentDraftAuthorityFunc(func(context.Context, CurrentDraftFacts) error { return nil })
	drain := HandleDrainFunc(func(context.Context, string) error { return nil })
	foreign := evidence
	foreign.AfterStageTreeSHA256[0] ^= 1
	if plan, err := PrepareEditedExchangePlan(ctx, snapshot, foreign, journal, current, drain); plan != nil || err == nil {
		t.Fatalf("foreign edited digest accepted: %v", err)
	}
	marker := filepath.Join(snapshot.parentPath, journal.name)
	if err := os.WriteFile(marker, []byte(`{"format":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if plan, err := PrepareEditedExchangePlan(ctx, snapshot, evidence, journal, current, drain); plan != nil || err == nil {
		t.Fatalf("tampered PREPARED marker accepted: %v", err)
	}
}

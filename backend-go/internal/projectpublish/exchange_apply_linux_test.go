//go:build linux

package projectpublish

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
)

func TestApplyEditedExchangeKeepsPreparedStageAndAuthorSource(t *testing.T) {
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
	stage, err := snapshot.PrivatePath()
	if err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(f.storage, projectfs.DatabaseName)
	stagePath := filepath.Join(stage, projectfs.DatabaseName)
	sourceBefore, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	edited := []byte("synthetic private draft edit")
	if err := os.WriteFile(stagePath, edited, 0600); err != nil {
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
	if err := ApplyEditedExchange(ctx, plan, journal); !errors.Is(err, ErrPublishUnavailable) {
		t.Fatalf("unproven exchange was not denied: %v", err)
	}
	sourceAfter, err := os.ReadFile(sourcePath)
	if err != nil || !bytes.Equal(sourceAfter, sourceBefore) {
		t.Fatalf("author source changed: %v", err)
	}
	stageAfter, err := os.ReadFile(stagePath)
	if err != nil || !bytes.Equal(stageAfter, edited) {
		t.Fatalf("private edited stage changed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(f.stageParent, journal.Name()+".switched")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SWITCHED marker appeared without exchange: %v", err)
	}
	recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalPrepared {
		t.Fatalf("journal no longer PREPARED: %+v %v", recovery, err)
	}
}

func TestApplyEditedExchangeRejectsMissingEvidence(t *testing.T) {
	if err := ApplyEditedExchange(context.Background(), nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil plan accepted: %v", err)
	}
	if err := ApplyEditedExchange(nil, &ExchangePlan{snapshot: &Snapshot{}}, &ExchangeJournal{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil context accepted: %v", err)
	}
	if err := ApplyEditedExchange(context.Background(), &ExchangePlan{snapshot: &Snapshot{}}, &ExchangeJournal{}); !errors.Is(err, ErrJournalInvalid) {
		t.Fatalf("non-edited marker accepted: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := ApplyEditedExchange(ctx, &ExchangePlan{snapshot: &Snapshot{}}, &ExchangeJournal{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context ignored: %v", err)
	}
}

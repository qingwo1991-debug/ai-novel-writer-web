//go:build linux && amd64

package projectpublish

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

type exchangeTestAdmitted struct {
	identity projectsession.Context
	closed   bool
}

func (a *exchangeTestAdmitted) Context() projectsession.Context { return a.identity }
func (a *exchangeTestAdmitted) Verify(context.Context) error {
	if a.closed {
		return ErrChanged
	}
	return nil
}
func (a *exchangeTestAdmitted) Close() error {
	a.closed = true
	return nil
}

func TestEditedExchangeRequiresLiveCommitFenceAndCannotRetry(t *testing.T) {
	f, snapshot, evidence, journal := preparedEditedFixture(t)
	ctx := context.Background()
	plan, err := PrepareEditedExchangePlan(ctx, snapshot, evidence, journal,
		CurrentDraftAuthorityFunc(func(context.Context, CurrentDraftFacts) error { return nil }),
		HandleDrainFunc(func(context.Context, string) error { return nil }))
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.ExchangeWithFence(ctx, EditedExchangeClaim{Held: f.lock}); !errors.Is(err, ErrInvalid) || plan.attempted {
		t.Fatalf("missing fence started exchange: %v", err)
	}
	stillOld, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(stillOld, oldBytes) {
		t.Fatalf("invalid claim changed author source: %v", err)
	}
	active, err := projectsession.NewActiveCoordinator(f.dataRoot, f.lock)
	if err != nil {
		t.Fatal(err)
	}
	owner := projectsession.Owner{SessionID: "synthetic-cookie", ClientID: "synthetic-tab"}
	before := &exchangeTestAdmitted{identity: projectsession.Context{
		ProjectID: snapshot.projectID, ProjectPath: f.projectPath, LeaseID: "old-lease"}}
	if _, err := active.Open(ctx, owner, "old-key", "open-token", func(context.Context, *writerlock.Lock) (projectsession.Admitted, error) {
		return before, nil
	}); err != nil {
		t.Fatal(err)
	}
	var claim EditedExchangeClaim
	actionFailure := errors.New("synthetic stop before readmission")
	_, err = active.CommitDrained(ctx, owner, before.identity, "old-key",
		func(ctx context.Context, held *writerlock.Lock, fence *projectsession.PublishFence) (projectsession.Admitted, projectsession.CommitClearance, error) {
			if !before.closed {
				return nil, nil, ErrChanged
			}
			claim = EditedExchangeClaim{Held: held, Fence: fence, Owner: owner, Expected: before.identity, OldKey: "old-key"}
			if err := plan.ExchangeWithFence(ctx, claim); err != nil {
				return nil, nil, err
			}
			if err := plan.ExchangeWithFence(ctx, claim); !errors.Is(err, ErrExchangeOutcomeUnknown) {
				return nil, nil, errors.New("second exchange attempt was not rejected")
			}
			if _, err := snapshot.RetainSwitched(ctx, plan.stageHash); err != nil {
				return nil, nil, err
			}
			if err := journal.MarkSwitched(ctx); err != nil {
				return nil, nil, err
			}
			return nil, nil, actionFailure
		})
	if !errors.Is(err, projectsession.ErrCommitOutcomeUnknown) || !errors.Is(err, actionFailure) {
		t.Fatalf("unacknowledged exchange was reported as a save: %v", err)
	}
	newBytes, err := os.ReadFile(filepath.Join(f.storage, projectfs.DatabaseName))
	if err != nil || bytes.Equal(newBytes, oldBytes) {
		t.Fatalf("physical exchange did not install edited stage: %v", err)
	}
	retained, err := os.ReadFile(filepath.Join(snapshot.stage, projectfs.DatabaseName))
	if err != nil || !bytes.Equal(retained, oldBytes) {
		t.Fatalf("old source was not retained after exchange: %v", err)
	}
	recovery, err := RecoverExchangeJournal(ctx, f.lock, f.dataRoot, snapshot.projectID, f.projectPath, f.stageParent, journal.Name())
	if err != nil || recovery.Class != JournalSwitched {
		t.Fatalf("journal did not classify swapped trees: %+v %v", recovery, err)
	}
	if err := plan.ExchangeWithFence(ctx, claim); !errors.Is(err, ErrExchangeOutcomeUnknown) {
		t.Fatalf("expired fence retried exchange: %v", err)
	}
}

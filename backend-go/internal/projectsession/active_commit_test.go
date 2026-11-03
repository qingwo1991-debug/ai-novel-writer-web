//go:build linux

package projectsession

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

func TestCommitDrainedInstallsFreshLeaseAndRejectsOpenDuringAction(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	old := admitted("project", "/trusted/project")
	if _, err := active.Open(context.Background(), owner, "old-key", "open", stageAdmitted(old)); err != nil {
		t.Fatal(err)
	}
	wrong := old.identity
	wrong.LeaseID = "wrong"
	called := false
	if _, err := active.CommitDrained(context.Background(), owner, wrong, "old-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
		called = true
		return nil, nil, nil
	}); !errors.Is(err, novelruntime.ErrNotOwned) || called || old.closed.Load() != 0 {
		t.Fatalf("wrong lease entered commit: %v", err)
	}
	if _, err := active.CommitDrained(context.Background(), owner, old.identity, "wrong-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
		called = true
		return nil, nil, nil
	}); !errors.Is(err, novelruntime.ErrNotOwned) || called || old.closed.Load() != 0 {
		t.Fatalf("wrong key entered commit: %v", err)
	}
	newHandle := admitted("project", "/trusted/project")
	newHandle.identity.LeaseID = "fresh-lease"
	got, err := active.CommitDrained(context.Background(), owner, old.identity, "old-key", func(ctx context.Context, held *writerlock.Lock) (Admitted, CommitClearance, error) {
		called = true
		if err := held.VerifyLegacy(active.root); err != nil || old.closed.Load() != 1 {
			return nil, nil, errors.Join(errors.New("old handle not drained"), err)
		}
		active.mu.Lock()
		before := active.slots[owner.key()].latest
		active.mu.Unlock()
		if _, err := active.Open(ctx, owner, "queued-key", "during-commit", stageAdmitted(admitted("project", "/trusted/project"))); !errors.Is(err, ErrCommitInProgress) {
			return nil, nil, errors.New("Open registered during irreversible action")
		}
		active.mu.Lock()
		after := active.slots[owner.key()].latest
		active.mu.Unlock()
		if after != before {
			return nil, nil, errors.New("Open changed generation during irreversible action")
		}
		return newHandle, func(context.Context, Admitted) (string, error) {
			active.mu.Lock()
			installedAndFenced := active.slots[owner.key()].handle == newHandle && active.slots[owner.key()].switching && active.slots[owner.key()].committing
			active.mu.Unlock()
			if !installedAndFenced {
				return "", errors.New("candidate was not installed and fenced before clearance")
			}
			return "new-key", nil
		}, nil
	})
	if err != nil || !called || !sameContext(got, newHandle.identity) {
		t.Fatalf("commit did not install fresh lease: %+v %v", got, err)
	}
	if _, done, err := active.Begin(context.Background(), owner, old.identity); !errors.Is(err, novelruntime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatalf("old lease retained: %v", err)
	}
	if _, done, err := active.Begin(context.Background(), owner, newHandle.identity); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
}

func TestCommitDrainedQueuedOpenAbortsBeforeAction(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	old := admitted("project", "/trusted/project")
	if _, err := active.Open(context.Background(), owner, "old-key", "open", stageAdmitted(old)); err != nil {
		t.Fatal(err)
	}
	_, done, err := active.Begin(context.Background(), owner, old.identity)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	commitResult := make(chan error, 1)
	go func() {
		_, err := active.CommitDrained(context.Background(), owner, old.identity, "old-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
			called = true
			return nil, nil, nil
		})
		commitResult <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active.mu.Lock()
		draining := active.slots[owner.key()].switching
		active.mu.Unlock()
		if draining {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("commit did not start draining")
		}
		runtime.Gosched()
	}
	queued := admitted("project", "/trusted/project")
	queued.identity.LeaseID = "queued-lease"
	openResult := make(chan error, 1)
	go func() {
		_, err := active.Open(context.Background(), owner, "queued-key", "queued-open", stageAdmitted(queued))
		openResult <- err
	}()
	for {
		active.mu.Lock()
		state := active.slots[owner.key()]
		registered := state.latest != state.admittedGeneration
		active.mu.Unlock()
		if registered {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("queued Open did not register")
		}
		runtime.Gosched()
	}
	done()
	if err := <-commitResult; !errors.Is(err, ErrStaleOpen) || called {
		t.Fatalf("stale commit entered action: %v", err)
	}
	if err := <-openResult; err != nil {
		t.Fatalf("queued Open could not re-admit after precommit abort: %v", err)
	}
}

func TestCommitDrainedActionErrorPoisonsWithoutReleasingLock(t *testing.T) {
	root := t.TempDir()
	held, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	active, err := NewActiveCoordinator(root, held)
	if err != nil {
		t.Fatal(err)
	}
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	old := admitted("project", "/trusted/project")
	if _, err := active.Open(context.Background(), owner, "old-key", "open", stageAdmitted(old)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("marker write uncertain")
	_, err = active.CommitDrained(context.Background(), owner, old.identity, "old-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
		return nil, nil, failure
	})
	if !errors.Is(err, ErrCommitOutcomeUnknown) || !errors.Is(err, failure) || old.closed.Load() != 1 {
		t.Fatalf("post-action error was not quarantined: %v", err)
	}
	if err := held.VerifyLegacy(root); err != nil {
		t.Fatalf("uncertain result released writer lock: %v", err)
	}
	if _, err := active.Open(context.Background(), owner, "any", "retry", stageAdmitted(admitted("project", "/trusted/project"))); !errors.Is(err, ErrActiveClosed) {
		t.Fatalf("uncertain result allowed retry: %v", err)
	}
	if err := active.Shutdown(); !errors.Is(err, ErrActiveCleanup) {
		t.Fatalf("uncertain result released coordinator: %v", err)
	}
}

func TestCommitDrainedRejectsReusedLeaseAfterAction(t *testing.T) {
	root := t.TempDir()
	held, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	active, err := NewActiveCoordinator(root, held)
	if err != nil {
		t.Fatal(err)
	}
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	old := admitted("project", "/trusted/project")
	if _, err := active.Open(context.Background(), owner, "old-key", "open", stageAdmitted(old)); err != nil {
		t.Fatal(err)
	}
	reused := admitted("project", "/trusted/project")
	_, err = active.CommitDrained(context.Background(), owner, old.identity, "old-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
		return reused, func(context.Context, Admitted) (string, error) { return "new-key", nil }, nil
	})
	if !errors.Is(err, ErrCommitOutcomeUnknown) || reused.closed.Load() != 1 {
		t.Fatalf("reused lease installed after action: %v", err)
	}
	if err := held.VerifyLegacy(root); err != nil {
		t.Fatalf("invalid candidate released writer lock: %v", err)
	}
	if err := active.Shutdown(); !errors.Is(err, ErrActiveCleanup) {
		t.Fatalf("invalid candidate left coordinator usable: %v", err)
	}
}

func TestCommitDrainedClearanceFailureRemovesInstalledCandidate(t *testing.T) {
	root := t.TempDir()
	held, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	active, err := NewActiveCoordinator(root, held)
	if err != nil {
		t.Fatal(err)
	}
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	old := admitted("project", "/trusted/project")
	if _, err := active.Open(context.Background(), owner, "old-key", "open", stageAdmitted(old)); err != nil {
		t.Fatal(err)
	}
	candidate := admitted("project", "/trusted/project")
	candidate.identity.LeaseID = "fresh-lease"
	failure := errors.New("catalog clearance uncertain")
	_, err = active.CommitDrained(context.Background(), owner, old.identity, "old-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
		return candidate, func(ctx context.Context, handle Admitted) (string, error) {
			active.mu.Lock()
			state := active.slots[owner.key()]
			installedAndFenced := state.handle == handle && state.switching && state.committing
			active.mu.Unlock()
			if !installedAndFenced {
				return "", errors.New("candidate was not fenced before clearance")
			}
			if _, done, err := active.Begin(ctx, owner, candidate.identity); !errors.Is(err, novelruntime.ErrBusy) {
				if done != nil {
					done()
				}
				return "", errors.New("candidate served before clearance")
			}
			return "", failure
		}, nil
	})
	if !errors.Is(err, ErrCommitOutcomeUnknown) || !errors.Is(err, failure) || candidate.closed.Load() != 1 {
		t.Fatalf("clearance failure did not quarantine candidate: %v", err)
	}
	active.mu.Lock()
	retained := active.slots[owner.key()].handle
	active.mu.Unlock()
	if retained != nil {
		t.Fatal("failed clearance retained candidate")
	}
	if err := held.VerifyLegacy(root); err != nil {
		t.Fatalf("failed clearance released writer lock: %v", err)
	}
}

func TestCommitDrainedCancellationAfterClearanceIsCommitted(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	old := admitted("project", "/trusted/project")
	if _, err := active.Open(context.Background(), owner, "old-key", "open", stageAdmitted(old)); err != nil {
		t.Fatal(err)
	}
	candidate := admitted("project", "/trusted/project")
	candidate.identity.LeaseID = "fresh-lease"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got, err := active.CommitDrained(ctx, owner, old.identity, "old-key", func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error) {
		return candidate, func(context.Context, Admitted) (string, error) {
			cancel()
			return "new-key", nil
		}, nil
	})
	if err != nil || !sameContext(got, candidate.identity) || candidate.closed.Load() != 0 {
		t.Fatalf("cancellation after clearance did not commit: %+v %v", got, err)
	}
	active.mu.Lock()
	state := active.slots[owner.key()]
	retained, key, committing := state.handle, state.key, state.committing
	active.mu.Unlock()
	if retained != candidate || key != "new-key" || committing {
		t.Fatal("successful clearance did not install candidate and key")
	}
}

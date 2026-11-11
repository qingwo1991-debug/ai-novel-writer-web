//go:build linux

package projectsession

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

type admittedFixture struct {
	identity  Context
	closed    atomic.Int32
	verifyErr error
	closeErr  error
}

func (a *admittedFixture) Context() Context             { return a.identity }
func (a *admittedFixture) Verify(context.Context) error { return a.verifyErr }
func (a *admittedFixture) Close() error                 { a.closed.Add(1); return a.closeErr }

func activeFixture(t *testing.T) *ActiveCoordinator {
	t.Helper()
	root := t.TempDir()
	held, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	active, err := NewActiveCoordinator(root, held)
	if err != nil {
		held.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := active.Shutdown(); err != nil {
			t.Error(err)
		}
	})
	return active
}
func admitted(id, path string) *admittedFixture {
	return &admittedFixture{identity: Context{ProjectID: id, ProjectPath: path, LeaseID: "lease-" + id}}
}
func stageAdmitted(a *admittedFixture) StageActive {
	return func(context.Context, *writerlock.Lock) (Admitted, error) { return a, nil }
}

func TestActiveFailedAndStaleSwitchPreservePreviousHandle(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "browser", ClientID: "tab"}
	previous := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), owner, "catalog-A", "first", stageAdmitted(previous)); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("synthetic candidate rejected")
	if _, err := active.Open(context.Background(), owner, "catalog-B", "failed", func(context.Context, *writerlock.Lock) (Admitted, error) { return nil, failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if previous.closed.Load() != 0 {
		t.Fatal("failed B closed A")
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	stale := admitted("B", "/trusted/B")
	oldResult := make(chan error, 1)
	go func() {
		_, err := active.Open(context.Background(), owner, "catalog-B", "older", func(context.Context, *writerlock.Lock) (Admitted, error) {
			close(entered)
			<-release
			return stale, nil
		})
		oldResult <- err
	}()
	<-entered
	latest := admitted("C", "/trusted/C")
	newResult := make(chan error, 1)
	go func() {
		_, err := active.Open(context.Background(), owner, "catalog-C", "newer", stageAdmitted(latest))
		newResult <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active.mu.Lock()
		generation := active.slots[owner.key()].latest
		active.mu.Unlock()
		if generation >= 4 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("newer request not registered while old stage held")
		}
		runtime.Gosched()
	}
	close(release)
	if err := <-oldResult; !errors.Is(err, ErrStaleOpen) {
		t.Fatal("old request committed", err)
	}
	if err := <-newResult; err != nil {
		t.Fatal(err)
	}
	if stale.closed.Load() != 1 || previous.closed.Load() != 1 {
		t.Fatal("old/stale candidate cleanup", stale.closed.Load(), previous.closed.Load())
	}
	if _, done, err := active.Begin(context.Background(), owner, latest.identity); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
	if _, done, err := active.Begin(context.Background(), owner, previous.identity); err == nil {
		done()
		t.Fatal("A retained after committed C")
	}
}

// TestActiveAccountLeaseServesOnlyTheTabsThatOpenedIt is the multi-tab fence at
// its narrowest. One account-level slot (SessionID+ClientID name the account, the
// way the production lane derives them) is opened by one tab. That tab keeps the
// handle; a sibling tab of the SAME account that never opened the project is
// refused exactly as it was when the lease was keyed per tab - and becomes
// admissible, on the same shared handle, the moment it opens the project itself.
func TestActiveAccountLeaseServesOnlyTheTabsThatOpenedIt(t *testing.T) {
	active := activeFixture(t)
	account := func(tab string) Owner {
		return Owner{SessionID: "author", ClientID: "account", Tab: tab}
	}
	handle := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), account("tab-a"), "catalog-A", "first", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	// The sibling tab of the same account never opened this project.
	sibling := account("tab-b")
	if _, done, err := active.Begin(context.Background(), sibling, handle.identity); !errors.Is(err, novelruntime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatal("a tab that never opened the project borrowed the shared handle", err)
	}
	if _, present, err := active.Current(context.Background(), sibling); err != nil || present {
		t.Fatal("a tab that never opened the project reported the live context", err, present)
	}
	if err := active.CloseProject(context.Background(), sibling, handle.identity); !errors.Is(err, novelruntime.ErrNotOwned) {
		t.Fatal("a tab that never opened the project closed the shared handle", err)
	}
	// The tab that did open it is still served.
	_, done, err := active.Begin(context.Background(), account("tab-a"), handle.identity)
	if err != nil {
		t.Fatal("the tab that opened the project was refused", err)
	}
	done()

	// The sibling opens the same project itself: it adopts the live lease instead
	// of staging a second handle, and is served from its own tab.
	refreshed, err := active.Open(context.Background(), sibling, "catalog-A", "second", func(context.Context, *writerlock.Lock) (Admitted, error) {
		t.Fatal("a sibling tab of the same account staged a second handle")
		return nil, nil
	})
	if err != nil || refreshed != handle.identity {
		t.Fatal("the sibling tab did not adopt the live lease", refreshed, err)
	}
	_, siblingDone, err := active.Begin(context.Background(), sibling, handle.identity)
	if err != nil {
		t.Fatal("the sibling tab was refused after opening the project", err)
	}
	siblingDone()
	if _, firstDone, err := active.Begin(context.Background(), account("tab-a"), handle.identity); err != nil {
		t.Fatal("the first tab was refused after its sibling opened the project", err)
	} else {
		firstDone()
	}
}

// TestActiveSiblingLeaseRotationKeepsBeginAndCloseAdmitted is the regression
// this fix exists for: after one tab acknowledges a draft save, the shared
// account lease rotates in place, but a sibling tab that opened the project
// still owns the same project and must be able to write and close with the
// older lease it was issued.
func TestActiveSiblingLeaseRotationKeepsBeginAndCloseAdmitted(t *testing.T) {
	active := activeFixture(t)
	account := func(tab string) Owner {
		return Owner{SessionID: "author", ClientID: "account", Tab: tab}
	}
	tabA, tabB := account("tab-a"), account("tab-b")
	handle := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), tabA, "catalog-A", "open-a", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	if _, err := active.Open(context.Background(), tabB, "catalog-A", "open-b", func(context.Context, *writerlock.Lock) (Admitted, error) {
		t.Fatal("a sibling tab staged a second handle")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := Context{ProjectID: "A", ProjectPath: "/trusted/A", LeaseID: "lease-A"}
	rotated := old
	rotated.LeaseID = "lease-A-rotated"
	// One tab's acknowledged draft save rotates the shared handle in place.
	handle.identity.LeaseID = rotated.LeaseID
	if err := active.AdoptCommittedLease(tabA, old, rotated); err != nil {
		t.Fatal(err)
	}
	// Current reports each tab's own view: the saver sees the fresh lease, the
	// sibling keeps the old one it actually received.
	if current, present, err := active.Current(context.Background(), tabA); err != nil || !present || current != rotated {
		t.Fatalf("saver's current view = %+v %v %v", current, present, err)
	}
	if current, present, err := active.Current(context.Background(), tabB); err != nil || !present || current != old {
		t.Fatalf("sibling's current view = %+v %v %v", current, present, err)
	}
	// The sibling's next write does not need a reopen.
	if _, done, err := active.Begin(context.Background(), tabB, old); err != nil {
		t.Fatalf("sibling write refused after a peer rotated the lease: %v", err)
	} else {
		done()
	}
	// Closing from the sibling with its older lease is still its own project.
	if err := active.CloseProject(context.Background(), tabB, old); err != nil {
		t.Fatalf("sibling close refused after a peer rotated the lease: %v", err)
	}
	if _, present, err := active.Current(context.Background(), tabB); err != nil || present {
		t.Fatalf("closed project still reports current: %v %v", present, err)
	}
}

// TestActiveSiblingLeaseRotationStillRefusesUnopenedAndForeign proves the
// relaxation is limited to tabs that opened this project under this account:
// a never-opened tab and a different account remain refused even when the
// shared lease has rotated to a value they do not hold.
func TestActiveSiblingLeaseRotationStillRefusesUnopenedAndForeign(t *testing.T) {
	active := activeFixture(t)
	account := func(tab string) Owner {
		return Owner{SessionID: "author", ClientID: "account", Tab: tab}
	}
	handle := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), account("tab-a"), "catalog-A", "open-a", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	old := Context{ProjectID: "A", ProjectPath: "/trusted/A", LeaseID: "lease-A"}
	rotated := old
	rotated.LeaseID = "lease-A-rotated"
	handle.identity.LeaseID = rotated.LeaseID
	if err := active.AdoptCommittedLease(account("tab-a"), old, rotated); err != nil {
		t.Fatal(err)
	}
	// A tab that did open may use its own older lease, but not a lease it was
	// never shown: the per-tab record is not a blanket project-only pass.
	forged := old
	forged.LeaseID = "forged-never-issued"
	if _, done, err := active.Begin(context.Background(), account("tab-a"), forged); !errors.Is(err, novelruntime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatal("a lease this tab never held borrowed the project", err)
	}
	unopened := account("tab-never-opened")
	if _, done, err := active.Begin(context.Background(), unopened, old); !errors.Is(err, novelruntime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatal("a tab that never opened the project borrowed it after a lease rotation", err)
	}
	if _, present, err := active.Current(context.Background(), unopened); err != nil || present {
		t.Fatal("a tab that never opened the project reported current", err, present)
	}
	if err := active.CloseProject(context.Background(), unopened, old); !errors.Is(err, novelruntime.ErrNotOwned) {
		t.Fatal("a tab that never opened the project closed it", err)
	}
	foreign := Owner{SessionID: "intruder", ClientID: "account", Tab: "tab-foreign"}
	if _, done, err := active.Begin(context.Background(), foreign, rotated); !errors.Is(err, novelruntime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatal("a different account borrowed the project after a lease rotation", err)
	}
	if err := active.CloseProject(context.Background(), foreign, rotated); !errors.Is(err, novelruntime.ErrNotOwned) {
		t.Fatal("a different account closed the project after a lease rotation", err)
	}
	candidate := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), foreign, "catalog-A", "foreign-open", stageAdmitted(candidate)); !errors.Is(err, novelruntime.ErrOwned) {
		t.Fatal("a different account opened the project after a lease rotation", err)
	}
	if candidate.closed.Load() != 1 {
		t.Fatal("refused foreign candidate was not closed")
	}
}

// TestActiveClosedProjectRequiresReopenAfterSiblingRotation proves a real close
// still ends the session: the old sibling lease cannot write into the handle a
// later Open installs, even though the project path and id are unchanged.
func TestActiveClosedProjectRequiresReopenAfterSiblingRotation(t *testing.T) {
	active := activeFixture(t)
	account := func(tab string) Owner {
		return Owner{SessionID: "author", ClientID: "account", Tab: tab}
	}
	tabA, tabB := account("tab-a"), account("tab-b")
	handle := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), tabA, "catalog-A", "open-a", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	if _, err := active.Open(context.Background(), tabB, "catalog-A", "open-b", func(context.Context, *writerlock.Lock) (Admitted, error) {
		t.Fatal("a sibling tab staged a second handle")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	old := Context{ProjectID: "A", ProjectPath: "/trusted/A", LeaseID: "lease-A"}
	rotated := old
	rotated.LeaseID = "lease-A-rotated"
	handle.identity.LeaseID = rotated.LeaseID
	if err := active.AdoptCommittedLease(tabA, old, rotated); err != nil {
		t.Fatal(err)
	}
	if err := active.CloseProject(context.Background(), tabB, old); err != nil {
		t.Fatal(err)
	}
	reopened := admitted("A", "/trusted/A")
	reopened.identity.LeaseID = "lease-A-reopened"
	if _, err := active.Open(context.Background(), tabA, "catalog-A", "reopen", stageAdmitted(reopened)); err != nil {
		t.Fatal(err)
	}
	if _, done, err := active.Begin(context.Background(), tabB, old); !errors.Is(err, novelruntime.ErrNotOwned) {
		if done != nil {
			done()
		}
		t.Fatal("the old sibling lease survived a real close", err)
	}
	if _, done, err := active.Begin(context.Background(), tabA, reopened.identity); err != nil {
		t.Fatal(err)
	} else {
		done()
	}
}

func TestActiveRefreshAndExactCloseWaitForInFlight(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "browser", ClientID: "tab"}
	handle := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), owner, "catalog-A", "first", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	if _, err := active.Open(context.Background(), owner, "catalog-A", "refresh", func(context.Context, *writerlock.Lock) (Admitted, error) {
		t.Fatal("refresh staged again")
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, done, err := active.Begin(context.Background(), owner, Context{ProjectID: "A", ProjectPath: "/trusted/A", LeaseID: "wrong"}); err == nil {
		done()
		t.Fatal("wrong lease accepted")
	}
	_, done, err := active.Begin(context.Background(), owner, handle.identity)
	if err != nil {
		t.Fatal(err)
	}
	closing := make(chan error, 1)
	go func() { closing <- active.CloseProject(context.Background(), owner, handle.identity) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active.mu.Lock()
		switching := active.slots[owner.key()].switching
		active.mu.Unlock()
		if switching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close did not fence new calls")
		}
		runtime.Gosched()
	}
	if _, next, err := active.Begin(context.Background(), owner, handle.identity); !errors.Is(err, novelruntime.ErrBusy) {
		if next != nil {
			next()
		}
		t.Fatal("new call admitted during close", err)
	}
	if handle.closed.Load() != 0 {
		t.Fatal("handle closed under in-flight operation")
	}
	done()
	done()
	if err := <-closing; err != nil {
		t.Fatal(err)
	}
	if handle.closed.Load() != 1 {
		t.Fatal("close count", handle.closed.Load())
	}
	if _, next, err := active.Begin(context.Background(), owner, handle.identity); err == nil {
		next()
		t.Fatal("closed context resumed")
	}
}

func TestActiveCrossOwnerSameProjectDeniedAndCancelledCloseRetainsHandle(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "one", ClientID: "tab"}
	other := Owner{SessionID: "two", ClientID: "tab"}
	handle := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), owner, "catalog-A", "first", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	candidate := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), other, "catalog-A", "other", stageAdmitted(candidate)); !errors.Is(err, novelruntime.ErrOwned) {
		t.Fatal("cross owner admitted", err)
	}
	if candidate.closed.Load() != 1 {
		t.Fatal("rejected candidate not closed")
	}
	_, done, err := active.Begin(context.Background(), owner, handle.identity)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	closing := make(chan error, 1)
	go func() { closing <- active.CloseProject(ctx, owner, handle.identity) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active.mu.Lock()
		switching := active.slots[owner.key()].switching
		active.mu.Unlock()
		if switching {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close did not enter wait")
		}
		runtime.Gosched()
	}
	cancel()
	if err := <-closing; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done()
	if _, next, err := active.Begin(context.Background(), owner, handle.identity); err != nil {
		t.Fatal("cancelled close revoked A", err)
	} else {
		next()
	}
}

func TestActiveFailedDatabaseCloseRetainsWriterExclusion(t *testing.T) {
	root := t.TempDir()
	held, err := writerlock.AcquireLegacy(root)
	if err != nil {
		t.Fatal(err)
	}
	active, err := NewActiveCoordinator(root, held)
	if err != nil {
		_ = held.Close()
		t.Fatal(err)
	}
	defer held.Close() // Test-only cleanup after proving fail-closed behavior.
	owner := Owner{SessionID: "browser", ClientID: "tab"}
	handle := admitted("A", "/trusted/A")
	handle.closeErr = errors.New("synthetic database close unconfirmed")
	if _, err := active.Open(context.Background(), owner, "catalog-A", "first", stageAdmitted(handle)); err != nil {
		t.Fatal(err)
	}
	if err := active.CloseProject(context.Background(), owner, handle.identity); !errors.Is(err, handle.closeErr) {
		t.Fatal("close failure hidden", err)
	}
	if err := active.Shutdown(); !errors.Is(err, ErrActiveCleanup) {
		t.Fatal("shutdown released unconfirmed database", err)
	}
	if err := held.VerifyLegacy(root); err != nil {
		t.Fatal("gateway exclusion released after close failure", err)
	}
	if _, err := writerlock.AcquireLegacy(root); !errors.Is(err, writerlock.ErrLocked) {
		t.Fatalf("competing writer entered after failed close: %v", err)
	}
}

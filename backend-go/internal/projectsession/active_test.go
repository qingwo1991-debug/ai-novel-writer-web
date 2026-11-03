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

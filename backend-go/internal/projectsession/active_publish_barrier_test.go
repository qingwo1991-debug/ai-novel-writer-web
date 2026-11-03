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

func TestPublishBarrierDrainsTargetAndRequiresNewAdmission(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie-a", ClientID: "tab-a"}
	other := Owner{SessionID: "cookie-b", ClientID: "tab-b"}
	target := admitted("A", "/trusted/A")
	untouched := admitted("B", "/trusted/B")
	for _, item := range []struct {
		owner Owner
		key   string
		h     *admittedFixture
	}{{owner, "key-a", target}, {other, "key-b", untouched}} {
		if _, err := active.Open(context.Background(), item.owner, item.key, "first", stageAdmitted(item.h)); err != nil {
			t.Fatal(err)
		}
	}
	called := false
	if err := active.WithProjectDrained(context.Background(), other, target.identity, func(context.Context, *writerlock.Lock) error {
		called = true
		return nil
	}); !errors.Is(err, novelruntime.ErrNotOwned) || called {
		t.Fatalf("wrong owner entered barrier: %v", err)
	}
	wrongLease := target.identity
	wrongLease.LeaseID = "wrong"
	if err := active.WithProjectDrained(context.Background(), owner, wrongLease, func(context.Context, *writerlock.Lock) error {
		called = true
		return nil
	}); !errors.Is(err, novelruntime.ErrNotOwned) || called {
		t.Fatalf("wrong lease entered barrier: %v", err)
	}
	_, done, err := active.Begin(context.Background(), owner, target.identity)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- active.WithProjectDrained(context.Background(), owner, target.identity, func(_ context.Context, held *writerlock.Lock) error {
			if err := held.VerifyLegacy(active.root); err != nil {
				return err
			}
			if target.closed.Load() != 1 || untouched.closed.Load() != 0 {
				return errors.New("callback entered before exact target handle closed")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active.mu.Lock()
		fenced := active.slots[owner.key()].switching
		active.mu.Unlock()
		if fenced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target was not fenced")
		}
		runtime.Gosched()
	}
	if target.closed.Load() != 0 {
		t.Fatal("reader closed before active borrow ended")
	}
	if _, next, err := active.Begin(context.Background(), owner, target.identity); !errors.Is(err, novelruntime.ErrBusy) {
		if next != nil {
			next()
		}
		t.Fatalf("new target borrow entered during drain: %v", err)
	}
	if _, otherDone, err := active.Begin(context.Background(), other, untouched.identity); err != nil {
		t.Fatalf("other project read blocked: %v", err)
	} else {
		otherDone()
	}
	done()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("barrier callback did not start")
	}
	if _, next, err := active.Begin(context.Background(), owner, target.identity); !errors.Is(err, novelruntime.ErrNotOwned) {
		if next != nil {
			next()
		}
		t.Fatalf("old lease read during callback: %v", err)
	}
	readmitted := admitted("A", "/trusted/A")
	readmitted.identity.LeaseID = "new-lease"
	openResult := make(chan error, 1)
	go func() {
		_, err := active.Open(context.Background(), owner, "key-a", "after-publish", stageAdmitted(readmitted))
		openResult <- err
	}()
	select {
	case err := <-openResult:
		t.Fatalf("target opened inside callback: %v", err)
	default:
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if err := <-openResult; err != nil {
		t.Fatal(err)
	}
	if _, next, err := active.Begin(context.Background(), owner, target.identity); !errors.Is(err, novelruntime.ErrNotOwned) {
		if next != nil {
			next()
		}
		t.Fatalf("old lease survived re-admission: %v", err)
	}
	if _, next, err := active.Begin(context.Background(), owner, readmitted.identity); err != nil {
		t.Fatal(err)
	} else {
		next()
	}
}

func TestPublishBarrierCancellationRestoresTargetRead(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	target := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), owner, "key-a", "first", stageAdmitted(target)); err != nil {
		t.Fatal(err)
	}
	_, done, err := active.Begin(context.Background(), owner, target.identity)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- active.WithProjectDrained(ctx, owner, target.identity, func(context.Context, *writerlock.Lock) error {
			t.Error("cancelled barrier ran callback")
			return nil
		})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		active.mu.Lock()
		fenced := active.slots[owner.key()].switching
		active.mu.Unlock()
		if fenced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("target was not fenced")
		}
		runtime.Gosched()
	}
	cancel()
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	done()
	if target.closed.Load() != 0 {
		t.Fatal("cancelled barrier closed target")
	}
	if _, next, err := active.Begin(context.Background(), owner, target.identity); err != nil {
		t.Fatal(err)
	} else {
		next()
	}
}

func TestTypedPublishFenceBindsOwnerLeaseKeyAndLifetime(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	other := Owner{SessionID: "cookie", ClientID: "other-tab"}
	target := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), owner, "key-a", "first", stageAdmitted(target)); err != nil {
		t.Fatal(err)
	}
	var retained *PublishFence
	err := active.WithProjectDrainedFence(context.Background(), owner, target.identity, func(ctx context.Context, fence *PublishFence) error {
		retained = fence
		if err := fence.Verify(ctx, owner, target.identity, "key-a"); err != nil {
			return err
		}
		if err := fence.Verify(ctx, other, target.identity, "key-a"); !errors.Is(err, novelruntime.ErrNotOwned) {
			return errors.New("other owner accepted")
		}
		wrongLease := target.identity
		wrongLease.LeaseID = "wrong"
		if err := fence.Verify(ctx, owner, wrongLease, "key-a"); !errors.Is(err, novelruntime.ErrNotOwned) {
			return errors.New("wrong lease accepted")
		}
		if err := fence.Verify(ctx, owner, target.identity, "other-key"); !errors.Is(err, novelruntime.ErrNotOwned) {
			return errors.New("wrong catalog key accepted")
		}
		return fence.Verify(ctx, owner, target.identity, "key-a")
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := retained.Verify(context.Background(), owner, target.identity, "key-a"); !errors.Is(err, novelruntime.ErrNotOwned) {
		t.Fatalf("retained fence remained valid: %v", err)
	}
}

func TestTypedPublishFenceQueuedOpenRevokesFinalAuthority(t *testing.T) {
	active := activeFixture(t)
	owner := Owner{SessionID: "cookie", ClientID: "tab"}
	target := admitted("A", "/trusted/A")
	if _, err := active.Open(context.Background(), owner, "key-a", "first", stageAdmitted(target)); err != nil {
		t.Fatal(err)
	}
	readmitted := admitted("A", "/trusted/A")
	readmitted.identity.LeaseID = "new-lease"
	openResult := make(chan error, 1)
	err := active.WithProjectDrainedFence(context.Background(), owner, target.identity, func(ctx context.Context, fence *PublishFence) error {
		go func() {
			_, openErr := active.Open(context.Background(), owner, "key-a", "queued", stageAdmitted(readmitted))
			openResult <- openErr
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			active.mu.Lock()
			advanced := active.slots[owner.key()].latest != fence.generation
			active.mu.Unlock()
			if advanced {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("queued Open did not advance request generation")
			}
			runtime.Gosched()
		}
		if err := fence.Verify(ctx, owner, target.identity, "key-a"); !errors.Is(err, ErrStaleOpen) {
			return errors.New("queued Open did not revoke fence")
		}
		return nil
	})
	if !errors.Is(err, ErrStaleOpen) {
		t.Fatalf("stale publisher received ACK authority: %v", err)
	}
	if err := <-openResult; err != nil {
		t.Fatal(err)
	}
}

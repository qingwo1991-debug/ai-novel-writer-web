package projectsession

import (
	"context"
	"errors"
	"sync/atomic"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

// PublishFence is minted only after the target project's admitted handles
// have drained and closed. It is bound to one owner, lease, catalog key and
// Open generation. A retained fence expires with its callback. The fence is
// read-only until exchange, re-admission and ACK are one irrevocable protocol.
type PublishFence struct {
	coordinator *ActiveCoordinator
	owner       Owner
	expected    Context
	key         string
	generation  uint64
	active      atomic.Bool
}

// Verify rejects a stale or forged fence and checks that the target remains
// unadmitted under the same continuously held writer lock.
func (f *PublishFence) Verify(ctx context.Context, owner Owner, expected Context, key string) error {
	if f == nil || f.coordinator == nil || ctx == nil || !f.active.Load() || !owner.valid() ||
		owner != f.owner || !sameContext(expected, f.expected) || key == "" || key != f.key {
		return novelruntime.ErrNotOwned
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c := f.coordinator
	c.mu.Lock()
	defer c.mu.Unlock()
	if !f.active.Load() || c.closed {
		return ErrActiveClosed
	}
	state := c.slots[owner.key()]
	if state == nil || state.latest != f.generation || state.admittedGeneration != f.generation {
		return ErrStaleOpen
	}
	if state.handle != nil || state.key != "" {
		return novelruntime.ErrBusy
	}
	for _, other := range c.slots {
		if other.handle != nil && other.handle.Context().ProjectPath == expected.ProjectPath {
			return novelruntime.ErrBusy
		}
	}
	return c.lock.VerifyLegacy(c.root)
}

// WithProjectDrainedFence provides a typed read-only fence. It deliberately
// does not expose the held writer lock or authorize a source exchange.
// Its callback runs under the transition lock, and the fence expires before
// that lock is released. A queued Open invalidates the final result.
func (c *ActiveCoordinator) WithProjectDrainedFence(ctx context.Context, owner Owner, expected Context, publish func(context.Context, *PublishFence) error) error {
	if publish == nil {
		return ErrContext
	}
	return c.withProjectDrained(ctx, owner, expected, true, func(ctx context.Context, held *writerlock.Lock, fence *PublishFence) error {
		return publish(ctx, fence)
	})
}

// WithProjectDrained fences the project identified by an exact live owner
// lease, waits for its in-flight operations, and closes every admitted handle
// for that project before calling publish. The transition lock stays held
// through publish, so a queued Open cannot re-admit the project until this
// method returns. The caller must not call coordinator transition methods from
// publish. Other projects' already admitted reads may continue.
//
// A failed publish does not restore old handles: readers must Open again and
// receive new leases. The continuously held legacy writer lock is never
// released by this method.
func (c *ActiveCoordinator) WithProjectDrained(ctx context.Context, owner Owner, expected Context, publish func(context.Context, *writerlock.Lock) error) error {
	if publish == nil {
		return ErrContext
	}
	return c.withProjectDrained(ctx, owner, expected, false, func(ctx context.Context, held *writerlock.Lock, _ *PublishFence) error {
		return publish(ctx, held)
	})
}

func (c *ActiveCoordinator) withProjectDrained(ctx context.Context, owner Owner, expected Context, validateFinal bool, publish func(context.Context, *writerlock.Lock, *PublishFence) error) error {
	if c == nil || ctx == nil || !owner.valid() || !validActiveContext(expected) || publish == nil {
		return ErrContext
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.transition.Lock()
	defer c.transition.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrActiveClosed
	}
	anchor := c.slots[owner.key()]
	if anchor == nil || anchor.handle == nil || !sameContext(anchor.handle.Context(), expected) {
		c.mu.Unlock()
		return novelruntime.ErrNotOwned
	}
	if validateFinal && (anchor.admittedGeneration == 0 || anchor.latest != anchor.admittedGeneration) {
		c.mu.Unlock()
		return ErrStaleOpen
	}
	fence := &PublishFence{coordinator: c, owner: owner, expected: expected, key: anchor.key, generation: anchor.latest}
	path := expected.ProjectPath
	if validateFinal {
		for _, state := range c.slots {
			if state.handle != nil && state.handle.Context().ProjectPath == path &&
				(state.admittedGeneration == 0 || state.latest != state.admittedGeneration) {
				c.mu.Unlock()
				return ErrStaleOpen
			}
		}
	}
	// Keep every target slot fenced while waiting. Open and CloseProject are
	// serialized by transition; Begin checks switching under mu.
	targets := make(map[string]*activeSlot)
	for key, state := range c.slots {
		if state.handle != nil && state.handle.Context().ProjectPath == path {
			state.switching = true
			targets[key] = state
		}
	}
	restore := func() {
		for _, state := range targets {
			state.switching = false
		}
	}
	for _, state := range targets {
		for state.active > 0 {
			idle := state.idle
			c.mu.Unlock()
			select {
			case <-idle:
			case <-ctx.Done():
				c.mu.Lock()
				restore()
				c.mu.Unlock()
				return ctx.Err()
			}
			c.mu.Lock()
		}
	}
	if err := ctx.Err(); err != nil {
		restore()
		c.mu.Unlock()
		return err
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		restore()
		c.mu.Unlock()
		return err
	}
	// Close each handle while the target remains fenced. On an unconfirmed
	// close, poison the coordinator and retain the legacy lock.
	for _, state := range targets {
		handle := state.handle
		c.mu.Unlock()
		err := handle.Close()
		c.mu.Lock()
		if err != nil {
			c.closed = true
			c.poisoned = true
			c.mu.Unlock()
			return errors.Join(ErrActiveCleanup, err)
		}
		state.handle = nil
		state.key = ""
		state.switching = false
	}
	c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return err
	}
	fence.active.Store(true)
	defer fence.active.Store(false)
	if validateFinal {
		if err := fence.Verify(ctx, owner, expected, fence.key); err != nil {
			return err
		}
	}
	publishErr := publish(ctx, c.lock, fence)
	// Open records its request generation before waiting for transition. A
	// queued request therefore revokes this publisher's ACK authority.
	if validateFinal {
		return errors.Join(publishErr, fence.Verify(ctx, owner, expected, fence.key))
	}
	return errors.Join(publishErr, c.lock.VerifyLegacy(c.root))
}

package projectsession

import (
	"context"
	"errors"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

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
	path := expected.ProjectPath
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
	return errors.Join(publish(ctx, c.lock), c.lock.VerifyLegacy(c.root))
}

package projectsession

import (
	"context"
	"errors"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/writerlock"
)

var ErrCommitInProgress = errors.New("PROJECT_COMMIT_IN_PROGRESS")
var ErrCommitOutcomeUnknown = errors.New("PROJECT_COMMIT_OUTCOME_UNKNOWN")

// CommitClearance is trusted authorruntime-only finalization. It may release
// the catalog recovery hold and issue a real key only after the candidate is
// installed but still fenced. Its implementation must be atomic: on error the
// catalog hold remains, and on success it returns a nonempty, fresh key with
// no further fallible work. The coordinator cannot prove this callback's
// behavior. A returned key alone is not proof of catalog-backed Reader
// promotion; this preparatory API must not be used as a public save ACK.
type CommitClearance func(context.Context, Admitted) (string, error)

// CommitAction is a trusted authorruntime-only action. It must finish the
// durable exchange, recovery marker and private-copy readback before returning
// a candidate and its deferred clearance. It must clean any partial candidate
// that it does not return. These callbacks are not compile-time sealed
// publication authority; transport must never receive the held lock or call
// CommitDrained.
type CommitAction func(context.Context, *writerlock.Lock) (Admitted, CommitClearance, error)

// CommitDrained is an unbound coordinator primitive, not a browser save or an
// exchange implementation. It closes every admitted handle for the target
// project, then freezes the owner's Open generation while a trusted action
// performs its irreversible work. Once the action begins, any failure is
// outcome-unknown: the coordinator is poisoned and retains the writer lock.
// Only a verified, fresh-lease handle for the same project can be installed.
// This is preparatory and unbound: authorruntime must supply atomic catalog
// clearance, and separately prove the candidate's post-clearance read mode
// before treating the result as a public save acknowledgement.
func (c *ActiveCoordinator) CommitDrained(ctx context.Context, owner Owner, expected Context, oldKey string, action CommitAction) (result Context, retErr error) {
	if c == nil || ctx == nil || !owner.valid() || !validActiveContext(expected) || oldKey == "" || action == nil {
		return Context{}, ErrContext
	}
	if err := ctx.Err(); err != nil {
		return Context{}, err
	}
	c.transition.Lock()
	defer c.transition.Unlock()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Context{}, ErrActiveClosed
	}
	anchor := c.slots[owner.key()]
	if anchor == nil || anchor.handle == nil || anchor.key != oldKey || !sameContext(anchor.handle.Context(), expected) {
		c.mu.Unlock()
		return Context{}, novelruntime.ErrNotOwned
	}
	generation := anchor.admittedGeneration
	if generation == 0 || anchor.latest != generation {
		c.mu.Unlock()
		return Context{}, ErrStaleOpen
	}
	targets := make(map[*activeSlot]struct{})
	oldLeases := make(map[string]struct{})
	for _, state := range c.slots {
		if state.handle != nil && state.handle.Context().ProjectPath == expected.ProjectPath {
			if state.handle.Context().ProjectID != expected.ProjectID {
				c.mu.Unlock()
				return Context{}, ErrContext
			}
			if state.admittedGeneration == 0 || state.latest != state.admittedGeneration || state.committing {
				c.mu.Unlock()
				return Context{}, ErrStaleOpen
			}
			targets[state] = struct{}{}
			oldLeases[state.handle.Context().LeaseID] = struct{}{}
		}
	}
	for state := range targets {
		state.switching = true
	}
	restore := func() {
		for state := range targets {
			state.switching = false
		}
	}
	for state := range targets {
		for state.active > 0 {
			idle := state.idle
			c.mu.Unlock()
			select {
			case <-idle:
			case <-ctx.Done():
				c.mu.Lock()
				restore()
				c.mu.Unlock()
				return Context{}, ctx.Err()
			}
			c.mu.Lock()
		}
	}
	if err := ctx.Err(); err != nil {
		restore()
		c.mu.Unlock()
		return Context{}, err
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		restore()
		c.mu.Unlock()
		return Context{}, err
	}
	for state := range targets {
		handle := state.handle
		c.mu.Unlock()
		err := handle.Close()
		c.mu.Lock()
		if err != nil {
			c.closed = true
			c.poisoned = true
			c.mu.Unlock()
			return Context{}, errors.Join(ErrActiveCleanup, err)
		}
		state.handle = nil
		state.key = ""
	}
	// Open can register while the transition lock is held. The final check and
	// committing flag are atomic under mu, before the action can exchange data.
	if err := ctx.Err(); err != nil {
		restore()
		c.mu.Unlock()
		return Context{}, err
	}
	if anchor.latest != generation || anchor.admittedGeneration != generation {
		restore()
		c.mu.Unlock()
		return Context{}, ErrStaleOpen
	}
	for state := range targets {
		if state.latest != state.admittedGeneration {
			restore()
			c.mu.Unlock()
			return Context{}, ErrStaleOpen
		}
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		restore()
		c.mu.Unlock()
		return Context{}, err
	}
	for state := range targets {
		state.committing = true
	}
	c.mu.Unlock()
	var candidate Admitted
	complete := false
	defer func() {
		if complete {
			return
		}
		c.mu.Lock()
		anchor.handle = nil
		anchor.key = ""
		c.closed = true
		c.poisoned = true
		c.mu.Unlock()
		if candidate != nil {
			retErr = errors.Join(retErr, candidate.Close())
		}
	}()

	var clearance CommitClearance
	var actionErr error
	candidate, clearance, actionErr = action(ctx, c.lock)
	if actionErr != nil {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, actionErr)
	}
	if candidate == nil || clearance == nil {
		return Context{}, ErrCommitOutcomeUnknown
	}
	identity := candidate.Context()
	valid := validActiveContext(identity) && identity.ProjectID == expected.ProjectID &&
		identity.ProjectPath == expected.ProjectPath && identity.LeaseID != expected.LeaseID
	if _, reused := oldLeases[identity.LeaseID]; reused {
		valid = false
	}
	if !valid {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, ErrContext)
	}
	if err := candidate.Verify(ctx); err != nil {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, err)
	}
	if err := c.lock.VerifyLegacy(c.root); err != nil {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, err)
	}
	if !sameContext(candidate.Context(), identity) {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, ErrContext)
	}
	if err := ctx.Err(); err != nil {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, err)
	}
	c.mu.Lock()
	if c.closed || anchor.handle != nil || anchor.key != "" || !anchor.committing || anchor.latest != generation {
		c.mu.Unlock()
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, ErrStaleOpen)
	}
	for state := range targets {
		if !state.committing || state.handle != nil || state.key != "" || state.latest != state.admittedGeneration {
			c.mu.Unlock()
			return Context{}, errors.Join(ErrCommitOutcomeUnknown, ErrStaleOpen)
		}
	}
	// Begin observes switching; Current holds transition. The candidate is
	// installed before catalog clearance but cannot yet serve a repository call.
	anchor.handle = candidate
	c.mu.Unlock()

	// The clearance callback is the last fallible step. Its implementation
	// must leave the catalog hold in place on error and atomically publish the
	// fresh key on success. A success does not allow another ctx/Verify call:
	// either could fail after the catalog becomes externally visible.
	newKey, err := clearance(ctx, candidate)
	if err != nil {
		return Context{}, errors.Join(ErrCommitOutcomeUnknown, err)
	}
	c.mu.Lock()
	anchor.key = newKey
	anchor.latest++
	anchor.admittedGeneration = anchor.latest
	for state := range targets {
		state.switching = false
		state.committing = false
	}
	c.mu.Unlock()
	complete = true
	return identity, nil
}

//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// errProductionGenerationRunSettled is the symbolic refusal for a stop that has
// nothing left to stop. The shipped ledger answers a stop of an already cancelled
// root as a successful no-op: internal/store/generation_ledger.go:806 returns nil
// before it writes a single row. A lane that forwarded that nil would answer 200
// for a cancel that changed nothing, so this binder refuses the case with its own
// code instead, for cancel and pause alike. "sealed" is the other root status that
// guard names, but nothing in this codebase writes it, so cancelled is the only
// settled stop this lane can observe.
var errProductionGenerationRunSettled = errors.New("GENERATION_RUN_ALREADY_SETTLED")

// productionGenerationSettledStatus is the run status a settled stop leaves
// behind. This lane admits exactly one run per root (generation:begin), and the
// ledger's stop path writes the same terminal status to the root and to every run
// of it, so the run's own status identifies the silent no-op above without a
// second ledger read.
const productionGenerationSettledStatus = "cancelled"

// Cancel answers generation:cancel for the frozen browser session. It settles one
// admitted run - the root and the run both become cancelled and every reserved
// attempt becomes cancelled-before-dispatch - and returns the post-stop view, so a
// caller that cancelled a run reads back exactly the rows the ledger now holds.
//
// Nothing here dials a provider. The stop path writes the ledger control row
// (method "pause" with the root-cancelling flag, then "budget") and aborts the
// in-memory execution of this call's own service instance, which this lane builds
// per call and never hands a prepared dispatch. A second cancel of the same run is
// refused with GENERATION_RUN_ALREADY_SETTLED rather than reported as a fresh
// success over a row set it did not write.
func (g *productionGeneration) Cancel(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	return g.stopAdmittedRun(ctx, caller, expected, arg, true)
}

// Pause answers generation:pause for the frozen browser session: the same stop as
// cancel without the root-cancelling flag, so the root and the run are parked
// paused and generation:resume can continue from the frozen binding. The ledger
// records the transition, so generation:read reports the parked run instead of the
// running one this call replaced.
//
// The settled guard is shared with cancel: parking a run that is already cancelled
// is refused with GENERATION_RUN_ALREADY_SETTLED instead of answering success for
// a stop the ledger silently dropped. Pausing a run that is already paused is a
// repeated stop, not a settled one: the ledger rewrites the same paused rows and
// cancels any attempt that is still reserved, so it is answered with the paused
// view.
func (g *productionGeneration) Pause(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	return g.stopAdmittedRun(ctx, caller, expected, arg, false)
}

// stopAdmittedRun is the shared stop for cancel and pause.
func (g *productionGeneration) stopAdmittedRun(ctx context.Context, caller transport.Caller, expected map[string]any, arg any, cancelRoot bool) (any, error) {
	if g == nil || g.open == nil || g.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	// The two gates generation:begin and generation:execute read, in the same
	// order, and for a harder reason than dispatch: the owner service every stop
	// path needs is built over the executor, and the executor exists only when the
	// committed config named a usable origin. A lane with no committed policy
	// therefore cannot park or settle a run either, and says so with the policy's
	// own symbolic code.
	if _, err := g.open.ModelEgress(); err != nil {
		return nil, err
	}
	if g.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	// Read the run through the same admission the stop uses, before touching it.
	// That read is the live-lease fence: a stale, foreign or absent lease is refused
	// there, before any ledger row, exactly like generation:execute. It is also the
	// only way to tell a real transition from the ledger's silent no-op on a
	// settled root, because the stop path returns the same nil for both.
	view, err := g.inner.Read(ctx, caller, expected, arg)
	if err != nil {
		return nil, err
	}
	if syntheticString(syntheticMap(view)["status"]) == productionGenerationSettledStatus {
		return nil, errProductionGenerationRunSettled
	}
	if cancelRoot {
		return g.inner.Cancel(ctx, caller, expected, arg)
	}
	return g.inner.Pause(ctx, caller, expected, arg)
}

// Resume answers generation:resume for the frozen browser session. It advances a
// parked run back to running by storing the frozen binding this lane already holds:
// the reused admission marshals the run's own binding as `next`, and the reused
// service's ValidateRecovery accepts exactly an unchanged binding, so resume
// authorizes nothing new and preserves the begin-time fingerprint, context
// snapshot and source manifest. No model profile is resolved and no provider is
// dialed - the run becomes dispatchable again only when generation:execute is
// called - and the ledger records the transition (root back to active, run back to
// running) so generation:read reflects it.
//
// Resume needs no settled guard of its own: the reused path refuses with
// GENERATION_RESUME_UNAVAILABLE unless the run is paused, and the ledger refuses
// independently with GENERATION_RESUME_REFUSED unless the root is paused, no
// attempt is dispatch-marked and the binding still names this project.
func (g *productionGeneration) Resume(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if g == nil || g.open == nil || g.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := g.open.ModelEgress(); err != nil {
		return nil, err
	}
	if g.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	return g.inner.Resume(ctx, caller, expected, arg)
}

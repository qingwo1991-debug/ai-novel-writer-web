//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// This file is the production lane's WRITE slice of the two families whose
// binders are atomic across names this lane already serves: the generation
// ledger mutations and the finalization outbox. It publishes exactly nine
// names and nothing else:
//
//	generation:bind-material-decision          generation.go:48
//	generation:discard-candidate               generation.go:120
//	generation:restart                         generation.go:114
//	generation:retry-draft-short-outline       production_retry_draft_outline_linux.go
//	graph-generation:cancel                    generation.go:81
//	finalization-generation:cancel             generation.go:95
//	finalization:commit                        finalization.go:112 (BindFinalization)
//	finalization:retry                         do.
//	db:finalization-link-knowledge-document    do.
//
// # Why one server.Bind per generation name
//
// BindGeneration (internal/transport/generation.go:37) installs thirteen names
// atomically, so taking it would collide with generation:begin, generation:execute,
// generation:cancel, generation:pause and generation:resume, which
// production_open_linux.go already binds through BindGenerationBegin,
// BindGenerationExecute and BindGenerationLifecycle. The same binder would also
// install generation:retry-draft-short-outline, which this lane serves through a
// faithful re-dispatch rather than through the atomic binder. Each name is
// therefore installed with server.Bind, the exported single-name binder
// (internal/transport/server.go:73-77), exactly the way
// production_review_write_linux.go:51-56 installs its own slice. Bind refuses an
// uninventoried name (OPERATION_NOT_EXPOSED) and an already bound one
// (OPERATION_ALREADY_BOUND), so a name this file should not publish - or a
// sibling binder that took one of these names first - is a startup error instead
// of silent dead code or a silent collision.
//
// # The generation admission, and why nothing here dials
//
// Every generation name below reads the same two gates generation:begin and
// generation:execute read, in the same order (production_generation_begin_linux.go:
// 54-56, :77-82, production_generation_lifecycle_linux.go:62-76): the committed
// model-egress policy, then the executor it builds. Only then does it delegate to
// the reused owner, which re-admits the frozen three-key session and the live
// writer lease (synthetic_generation_linux.go:318-336). A stale, foreign or absent
// lease is refused there, before a single ledger row is read or written.
//
// The stop pair exists because every one of these names needs an owner service
// built over the executor: discard-candidate and restart call
// syntheticGeneration.newService (synthetic_generation_linux.go:1631, :1479), and
// so does either family cancel. bind-material-decision is a ledger-only transition
// and carries the same two gates with the rest of the family so a lane that never
// had a usable policy never serves any of these names - a run can only exist here
// if this lane admitted it, and admission refuses without that policy.
//
// None of the five non-retry names dials: bind-material-decision writes one
// ledger row (synthetic_generation_linux.go:1605 "replaceUnstartedBinding"),
// discard-candidate writes one artifact status (:1635 -> generation/service.go:795),
// the two family cancels are the ordinary stop (generation/service.go stop ->
// ledger "pause"), and restart opens a replacement run and cancels the old root
// (:1475 -> :1483), writing ledger rows only. The retry name is the one exception:
// it re-dispatches the proven frozen task through the executor (see its own
// section below). The executor's Egress client is never reached by the other five,
// which the tests assert with a loopback listener that receives no connection.
//
// # generation:restart, judged rather than assumed
//
// A previous triage refused restart as "a second run-admission path". Reading the
// code, that is not what it is: restart calls syntheticGeneration.begin
// (synthetic_generation_linux.go:1475), the SAME function body generation:begin's
// own admission performs - registry.Begin (:1491), the frozen source build, BindSources,
// the canonical plan, the fingerprint and the single ledger "open" (:1543) - so it
// opens the replacement run through exactly the admission this lane ALREADY serves,
// under exactly the same committed gates, and it adds no admission machinery of its
// own. It dispatches nothing: the only other call it makes is the stop of the old
// root (:1483). The replacement run can become a provider call only through
// generation:execute, which is gated by the committed model-egress policy. Restart
// is therefore bound here; the tests prove it opens no second path by driving a
// live run to a restarted run and asserting the ledger rows, with the loopback
// listener untouched.
//
// # generation:retry-draft-short-outline, re-dispatched rather than authored
//
// It is a provider call, so it needs the owner this file already wraps. It does
// not author a prompt: OrdinaryService.Execute records the frozen task on the
// attempt's usage receipt as replayTask (internal/generationowner/ordinary.go),
// and productionRetryDraftShortOutline reads that task back, proves it against
// the material decision's shortOutlinePromptHash and the stored request hash,
// then re-dispatches it (production_retry_draft_outline_linux.go). A run with no
// failed short-outline attempt, or a tampered replayTask/request hash, is refused
// by name without dispatching.
type productionGenerationWrites struct {
	owner *productionGeneration
}

// newProductionGenerationWrites wraps the one generation owner this lane built,
// so these names share generation:begin's registry, model leases and executor
// instead of constructing a second owner. It must be called with the same owner
// newProductionGeneration returned; a nil owner installs nothing.
func newProductionGenerationWrites(owner *productionGeneration) *productionGenerationWrites {
	return &productionGenerationWrites{owner: owner}
}

// Bind installs the six generation names. A nil owner installs nothing and is
// not an error (the names keep failing closed), exactly like
// transport.BindGenerationReads with a nil provider; the caller only calls this
// in the branch that built an owner.
func (w *productionGenerationWrites) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if w == nil || w.owner == nil {
		return nil
	}
	// The arities and request shapes are the family's own: the request plus the
	// frozen expected context at args[arity], with the same three-key fence and
	// the same mutating business DTO as BindGeneration.
	for _, bind := range []struct {
		name   string
		arity  int
		handle func(context.Context, transport.Caller, map[string]any, []any) (any, error)
	}{
		{"generation:bind-material-decision", 1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return w.owner.BindMaterialDecision(ctx, caller, expected, args[0])
		}},
		{"generation:discard-candidate", 2, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return w.owner.DiscardCandidate(ctx, caller, expected, args[0], args[1])
		}},
		{"generation:restart", 2, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return w.owner.Restart(ctx, caller, expected, args[0], args[1])
		}},
		{"generation:retry-draft-short-outline", 1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			return w.owner.RetryDraftShortOutline(ctx, caller, expected, args[0])
		}},
		// The graph and finalization channels carry `{ handle }`, not the handle
		// itself (src/services/graph-generation.ts:40,
		// src/services/finalization-generation.ts:27). The unwrap and its refusal
		// code mirror generation.go:81-104 exactly: the Node owner rejects any
		// request carrying a key other than the documented one before it looks at
		// the run, so the envelope is refused here too.
		{"graph-generation:cancel", 1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			handle, err := productionFamilyRequestHandle(args[0], "GENERATION_GRAPH_REQUEST_INVALID")
			if err != nil {
				return nil, err
			}
			return w.owner.Cancel(ctx, caller, expected, handle)
		}},
		{"finalization-generation:cancel", 1, func(ctx context.Context, caller transport.Caller, expected map[string]any, args []any) (any, error) {
			handle, err := productionFamilyRequestHandle(args[0], "GENERATION_FINALIZATION_REQUEST_INVALID")
			if err != nil {
				return nil, err
			}
			return w.owner.Cancel(ctx, caller, expected, handle)
		}},
	} {
		name, arity, call := bind.name, bind.arity, bind.handle
		if err := server.Bind(name, productionGenerationMutation(arity, call)); err != nil {
			return err
		}
	}
	return nil
}

// productionGenerationMutation is this file's copy of the family adapter
// (internal/transport/generation.go:260-304): wire arity is the request arity plus
// the frozen expected context, the context must be exactly the three-key map, and
// every failure leaves as the mutating business DTO. The graph binder's
// mutating=true rows answer identically, so the envelopes match.
func productionGenerationMutation(arity int, call func(context.Context, transport.Caller, map[string]any, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		fail := func(err error) (any, error) { return productionGenerationFailure(err), nil }
		if len(args) != arity+1 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[arity].(map[string]any)
		if !ok || len(raw) != 3 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected[key] = text
		}
		if ctx == nil {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := call(ctx, caller, expected, args)
		if err != nil {
			return fail(err)
		}
		return value, nil
	}
}

// productionGenerationFailure is the mutating envelope of this family
// (internal/transport/drafts.go:219-240): the symbolic code, or REQUEST_FAILED for
// anything that is not an all-caps symbolic code, never SQLite diagnostics, a path
// or a provider message. The draft-CAS conflict branch of that helper cannot occur
// here: no name in this file touches a draft content CAS.
func productionGenerationFailure(err error) map[string]any {
	if err == nil {
		return map[string]any{"success": false, "error": "Error: REQUEST_FAILED"}
	}
	code := err.Error()
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	}
	if len(code) < 2 {
		code = "REQUEST_FAILED"
	}
	for i, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(i > 0 && (c >= '0' && c <= '9' || c == '_')) {
			code = "REQUEST_FAILED"
			break
		}
	}
	return map[string]any{"success": false, "error": "Error: " + code}
}

// productionGenerationRead is the read half of the family adapter
// (internal/transport/generation.go:265-294 with mutating=false): same wire
// arity and three-key fence, but failures surface as security.HTTPError so the
// browser transport can read the symbolic code instead of a business DTO.
func productionGenerationRead(arity int, call func(context.Context, transport.Caller, map[string]any, []any) (any, error)) transport.Operation {
	return func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		fail := func(err error) (any, error) {
			var public *security.HTTPError
			if errors.As(err, &public) && public != nil {
				return nil, security.NewHTTPError(public.Status, public.Code)
			}
			return nil, security.NewHTTPError(http.StatusInternalServerError, productionGenerationSymbolicCode(err))
		}
		if len(args) != arity+1 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[arity].(map[string]any)
		if !ok || len(raw) != 3 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected[key] = text
		}
		if ctx == nil {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := call(ctx, caller, expected, args)
		if err != nil {
			return fail(err)
		}
		return value, nil
	}
}

// productionGenerationSymbolicCode mirrors generationSymbolicCode
// (internal/transport/generation.go:323-341): an all-caps symbolic code passes
// through, anything else collapses to REQUEST_FAILED.
func productionGenerationSymbolicCode(err error) string {
	if err == nil {
		return "REQUEST_FAILED"
	}
	code := err.Error()
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	}
	if len(code) < 2 {
		return "REQUEST_FAILED"
	}
	for i, c := range code {
		if !(c >= 'A' && c <= 'Z') && !(i > 0 && (c >= '0' && c <= '9' || c == '_')) {
			return "REQUEST_FAILED"
		}
	}
	return code
}

// productionFamilyRequestHandle is the `{ handle }` unwrap of
// internal/transport/generation.go:371-373 with the same single-key rule.
func productionFamilyRequestHandle(value any, invalidCode string) (any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New(invalidCode)
	}
	for key := range object {
		if key != "handle" {
			return nil, errors.New(invalidCode)
		}
	}
	inner, present := object["handle"]
	if !present {
		return nil, errors.New(invalidCode)
	}
	return inner, nil
}

// mutation is the one admission this file shares with generation:begin and
// generation:execute: the lane must still be open with an owner, the committed
// model-egress policy must be usable, and the executor that policy builds must
// exist. Nothing is written and nothing is dialed by the gate itself.
func (g *productionGeneration) mutation() error {
	if g == nil || g.open == nil || g.inner == nil {
		return errProductionGenerationUnavailable
	}
	if _, err := g.open.ModelEgress(); err != nil {
		return err
	}
	if g.inner.executor == nil {
		return errProductionGenerationExecuteUnavailable
	}
	return nil
}

// BindMaterialDecision freezes the renderer's closed material decision into an
// unstarted review/refine run's manifest. It is a ledger transition: the reused
// body re-reads the run, refuses an operation that cannot carry a decision and a
// different already-bound decision, then writes the run's binding once
// (synthetic_generation_linux.go:1600-1605 "replaceUnstartedBinding"). No model is
// resolved and no provider is dialed.
func (g *productionGeneration) BindMaterialDecision(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if err := g.mutation(); err != nil {
		return nil, err
	}
	return g.inner.BindMaterialDecision(ctx, caller, expected, arg)
}

// DiscardCandidate acknowledges one admitted candidate artifact and returns the
// updated view. The only write is the artifact's status (generation/service.go:
// 795-809 -> ledger "discardCandidate"), so the attempt rows and their settled
// usage are untouched: a discarded candidate is not a settled attempt.
func (g *productionGeneration) DiscardCandidate(ctx context.Context, caller transport.Caller, expected map[string]any, arg any, artifactIDArg any) (any, error) {
	if err := g.mutation(); err != nil {
		return nil, err
	}
	return g.inner.DiscardCandidate(ctx, caller, expected, arg, artifactIDArg)
}

// Restart opens the replacement run from a new selection and cancels the old root,
// exactly like the original: the new run is admitted first so a rejected source
// leaves the old run resumable (synthetic_generation_linux.go:1442-1487). It
// reuses generation:begin's admission function, so it is not a second admission
// path, and its own effect is a new run row plus the ordinary stop of the old root;
// dispatch of the replacement remains generation:execute's gated job.
func (g *productionGeneration) Restart(ctx context.Context, caller transport.Caller, expected map[string]any, arg any, selectionArg any) (any, error) {
	if err := g.mutation(); err != nil {
		return nil, err
	}
	return g.inner.Restart(ctx, caller, expected, arg, selectionArg)
}

// productionFinalizationWrites is the finalization-outbox half: the three names
// BindFinalization installs, served on the author's own admitted database.
//
// It owns no manuscript publisher and no knowledge-base document store, so it
// mints no publication receipt and no document identity: a commit reports
// committed:true with publicationStatus pending and the lane's own symbolic
// FINALIZATION_PUBLISHER_UNAVAILABLE (internal/transport/finalization.go:64-72), and
// a knowledge link records only the identity the request carried after the store
// re-verifies the frozen snapshot hash. This is the same refusal the durable lane
// already ships (synthetic_durable_open_linux.go:473-496), rebuilt over this lane's
// own borrow instead of that lane's repository helper.
type productionFinalizationWrites struct {
	durable *productionDurableOpen
}

// newProductionFinalizationWrites wraps the lane's durable bridge, the only object
// that can name the author database.
func newProductionFinalizationWrites(durable *productionDurableOpen) *productionFinalizationWrites {
	return &productionFinalizationWrites{durable: durable}
}

// Bind installs exactly the three finalization names. BindFinalization publishes
// all three atomically and refuses a name that is already bound, so a collision
// with another binder is a startup error.
func (f *productionFinalizationWrites) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if f == nil || f.durable == nil {
		return errors.New("PRODUCTION_FINALIZATION_WRITES_UNAVAILABLE")
	}
	return server.BindFinalization(transport.FinalizationProviderFunc(f.BeginFinalization))
}

// BeginFinalization borrows the admitted author database through
// productionDurableOpen.borrowAuthorDatabase (production_durable_linux.go:308-357) -
// the same live-lease admission db:save-summary-snapshot, the draft CAS and every
// other production write already uses - and hands back one outbox repository whose
// scope is the frozen project and lease. The owner proof verifier is the durable
// lane's own (synthetic_durable_open_linux.go:534-579): it accepts only the opaque
// transport proof for this exact project, lease, action and frozen values, and the
// store re-reads and re-hashes the outbox row inside the same IMMEDIATE transaction
// before asking it.
func (f *productionFinalizationWrites) BeginFinalization(ctx context.Context, caller transport.Caller, raw any) (*store.FinalizationOutbox, func(), error) {
	if f == nil || f.durable == nil {
		return nil, nil, errors.New("PRODUCTION_FINALIZATION_WRITES_UNAVAILABLE")
	}
	expected, err := projectsession.ParseContext(raw)
	if err != nil {
		return nil, nil, err
	}
	db, release, err := f.durable.borrowAuthorDatabase(ctx, caller, raw)
	if err != nil {
		return nil, nil, err
	}
	scope := store.FinalizationScope{ProjectID: expected.ProjectID, Epoch: expected.LeaseID}
	return store.NewFinalizationOutbox(db, scope, durableFinalizationOwner{expected: expected}), release, nil
}

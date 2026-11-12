//go:build linux

package main

import (
	"context"
	"errors"
	"log"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generation"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// errProductionGenerationUnavailable is the symbolic refusal for a lane whose
// admitted runtime or committed model configuration is missing. It is a fixed
// code: no committed value, path or credential is formatted into it.
var errProductionGenerationUnavailable = errors.New("PRODUCTION_GENERATION_UNAVAILABLE")

// errProductionGenerationExecuteUnavailable is the symbolic refusal for a lane
// that admitted no executor, which happens only when the committed config named
// no origin. It is a second fence behind ModelEgress, never a default-allow path.
var errProductionGenerationExecuteUnavailable = errors.New("PRODUCTION_GENERATION_EXECUTE_UNAVAILABLE")

// productionGeneration is the production lane's generation owner: the reduced S1
// run admission (generation:begin) and the one dispatch name this lane now binds
// (generation:execute), both lent from the verified synthetic lane behind the same
// committed gates.
//
// It reuses syntheticGeneration verbatim, so the ledger rows begin writes (the root
// budget row, then the run row), the frozen source manifest, the nine binding
// hashes, the canonical budget and the ordinary dispatch service are exactly what
// the lane that proved them uses. The executor is the generationowner.Executor
// built here - registry, plaintext credential seam and the lane's committed
// model-egress policy - and it is the only executor this lane has. DispatchFunc
// stays nil unless the lane was opened with the synthetic test seam, so a real
// deployment can only leave through egress.Client.Do.
//
// Both gates refuse before the reused admission is attempted whenever the
// committed config the model profile is read from, or the committed model-egress
// policy, is unavailable.
type productionGeneration struct {
	open  *productionOpen
	inner *syntheticGeneration
}

// Begin answers generation:begin for the frozen browser session. A malformed
// argument is refused by the reused admission before it touches the database, so
// it changes nothing.
func (g *productionGeneration) Begin(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if g == nil || g.open == nil || g.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := g.open.ModelEgress(); err != nil {
		return nil, err
	}
	return g.inner.Begin(ctx, caller, expected, arg)
}

// BeginGraph answers graph-generation:begin for the frozen browser session: the
// same reduced run admission as Begin, for the one graph family whose context
// this lane can capture (currently the plan kind). The egress gate comes first,
// exactly as it does for Begin, so a lane with no committed allowlist refuses
// before a root row exists rather than freezing a run it could never dispatch.
func (g *productionGeneration) BeginGraph(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if g == nil || g.open == nil || g.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := g.open.ModelEgress(); err != nil {
		return nil, err
	}
	return g.inner.BeginGraph(ctx, caller, expected, arg)
}

// Execute answers generation:execute for the frozen browser session: the dispatch
// half of the same admitted run. The egress gate comes first, so a lane with no
// committed allowlist refuses symbolically and never dispatches without a
// boundary. It then reuses syntheticGeneration.Execute verbatim, which re-admits
// the live lease, resolves the begin-time model lease, rebuilds and
// re-fingerprints the frozen sources and hands the prepared Frozen to the shared
// generationowner.OrdinaryService.
//
// That service owns the ledger ordering this lane inherits: it writes the attempt
// row (reserve), marks it dispatched and only then calls the executor, so a run
// that never reserved an attempt can never dispatch. A dispatch failure - a
// credential refusal, an origin outside the allowlist, a provider error - settles
// that same attempt as failed, and the receipt reports the failure instead of a
// completed candidate.
func (g *productionGeneration) Execute(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if g == nil || g.open == nil || g.inner == nil {
		return nil, errProductionGenerationUnavailable
	}
	if _, err := g.open.ModelEgress(); err != nil {
		return nil, err
	}
	if g.inner.executor == nil {
		return nil, errProductionGenerationExecuteUnavailable
	}
	return g.inner.Execute(ctx, caller, expected, arg)
}

// newProductionGeneration builds the run admission and the executor over the
// already admitted author runtime and the committed global config this lane pinned
// at startup. The model profiles it reads are the same private store llm reads
// project, and the live lease it revalidates is the durable project session this
// process holds. A lane with nothing to lend returns an error instead of a
// provider, so generation:begin and generation:execute keep failing closed rather
// than becoming handlers that could only refuse.
//
// dispatch is the executor's injectable dispatch seam and is synthetic-only:
// production main.go passes nil, so the sole network client is the committed
// egress policy. A lane opened without a committed origin still binds both names,
// and Execute refuses with the policy's own symbolic code instead of widening.
func newProductionGeneration(open *productionOpen, server *transport.Server, dispatch generation.Dispatch) (*productionGeneration, error) {
	if open == nil {
		return nil, errProductionGenerationUnavailable
	}
	open.mu.Lock()
	durable := open.durable
	config := open.config
	closed := open.closed
	open.mu.Unlock()
	if closed || durable == nil || config == nil {
		return nil, errProductionGenerationUnavailable
	}
	durable.mu.Lock()
	lifecycle := durable.lifecycle
	durableClosed := durable.closed
	durable.mu.Unlock()
	if durableClosed || lifecycle == nil {
		return nil, errProductionGenerationUnavailable
	}
	loadModel := func(_ context.Context, modelID string) (generationowner.Profile, bool, error) {
		records, err := config.ListPrivateModels()
		if err != nil {
			return generationowner.Profile{}, false, err
		}
		for _, record := range records {
			if id, _ := record["id"].(string); id != modelID {
				continue
			}
			profile, err := syntheticModelProfile(record)
			if err != nil {
				return generationowner.Profile{}, false, err
			}
			return profile, true, nil
		}
		return generationowner.Profile{}, false, nil
	}
	// The live lease is the durable project session this process still owns for
	// this caller's ACCOUNT, and the call reaches it through the TAB that captured
	// the scope: the account key names the shared lease, while the tab half is what
	// proves this tab opened the project itself. A stale, foreign or unopened one
	// refuses here, before any ledger row is written, which is the same fence the
	// generation reads already use.
	current := func(ctx context.Context, scope generationowner.Scope) error {
		value, found, err := lifecycle.CurrentProjectDurable(ctx, projectsession.Owner{SessionID: scope.LeaseSession, ClientID: scope.LeaseClient, Tab: scope.Tab})
		if err != nil {
			// The durable error itself is symbolic; no path, owner or lease enters
			// the log. It is usually a cancelled/expired provider context or a lost
			// writer lock, and GENERATION_EPOCH_STALE would hide which one.
			log.Printf("novel-server: generation current: durable error (%v)", err)
			return err
		}
		if !found {
			// No durable slot for this account+tab: the tab never opened the
			// project itself, or the slot was released between begin and execute.
			log.Printf("novel-server: generation current: no durable slot (tab tracked=%v)", scope.Tab != "")
			return errors.New("GENERATION_EPOCH_STALE")
		}
		if value.ProjectID != scope.Project || value.LeaseID != scope.Epoch {
			// Project id or lease mismatch: a sibling's save may have rotated the
			// tab lease, or the request carries a stale context. Values are not
			// logged; the two booleans are enough to tell which half failed.
			log.Printf("novel-server: generation current: stale context (projectMismatch=%v leaseMismatch=%v)", value.ProjectID != scope.Project, value.LeaseID != scope.Epoch)
			return errors.New("GENERATION_EPOCH_STALE")
		}
		return nil
	}
	registry, err := generationowner.NewRegistry(generationowner.RegistryOptions{
		LoadModel: loadModel,
		Current:   current,
		AuthorityLock: func(ctx context.Context, scope generationowner.Scope) (func(), error) {
			if err := current(ctx, scope); err != nil {
				// The symbolic reason only; the scope itself never enters the log.
				log.Printf("novel-server: generation authority lock refused (%v)", err)
				return nil, err
			}
			return func() {}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	inner := &syntheticGeneration{
		lifecycle:  lifecycle,
		server:     server,
		registry:   registry,
		loadModel:  loadModel,
		leaseOwner: productionLeaseOwner,
	}
	// The one executor, built the way the synthetic lane builds it: the same
	// registry the admission uses, the documented plaintext credential seam and the
	// committed model-egress policy productionOpen derived at startup. No policy
	// means no executor: Execute refuses with the policy's own code rather than
	// running with a nil Egress.
	if policy, egressErr := open.ModelEgress(); egressErr == nil {
		inner.executor = &generationowner.Executor{
			Registry:      registry,
			SecretStorage: generationowner.PlaintextCredential{},
			Egress:        policy,
			DispatchFunc:  dispatch,
		}
	}
	return &productionGeneration{open: open, inner: inner}, nil
}

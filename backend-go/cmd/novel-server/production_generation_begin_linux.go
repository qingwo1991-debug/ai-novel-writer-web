//go:build linux

package main

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectsession"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// errProductionGenerationBeginUnavailable is the symbolic refusal for a lane
// whose admitted runtime or committed model configuration is missing. It is a
// fixed code: no committed value, path or credential is formatted into it.
var errProductionGenerationBeginUnavailable = errors.New("PRODUCTION_GENERATION_BEGIN_UNAVAILABLE")

// productionGenerationBegin is generation:begin in the production lane: the
// reduced S1 run admission the verified synthetic lane already implements, lent
// to this lane behind one extra gate.
//
// It reuses syntheticGeneration.Begin verbatim, so the ledger rows it writes (the
// root budget row, then the run row), the frozen source manifest, the nine
// binding hashes and the canonical budget are exactly what the lane that proved
// them writes. Opening a run reserves no tokens, marks no attempt dispatched and
// constructs no provider client: this binder installs no executor, so
// generation:execute and the other eleven execution names still fail closed at
// GO_OPERATION_NOT_MIGRATED.
//
// The gate refuses before the admission is attempted whenever the committed
// config the model profile is read from, or the committed model-egress policy a
// future executor would have to dial through, is unavailable. Holding that policy
// is still not dispatch.
type productionGenerationBegin struct {
	open  *productionOpen
	inner *syntheticGeneration
}

// Begin answers generation:begin for the frozen browser session. A malformed
// argument is refused by the reused admission before it touches the database, so
// it changes nothing.
func (b *productionGenerationBegin) Begin(ctx context.Context, caller transport.Caller, expected map[string]any, arg any) (any, error) {
	if b == nil || b.open == nil || b.inner == nil {
		return nil, errProductionGenerationBeginUnavailable
	}
	if _, err := b.open.ModelEgress(); err != nil {
		return nil, err
	}
	return b.inner.Begin(ctx, caller, expected, arg)
}

// newProductionGenerationBegin builds the run admission over the already admitted
// author runtime and the committed global config this lane pinned at startup. The
// model profiles it reads are the same private store llm reads project, and the
// live lease it revalidates is the durable project session this process holds. A
// lane with nothing to lend returns an error instead of a provider, so
// generation:begin keeps failing closed rather than becoming a handler that can
// only refuse.
func newProductionGenerationBegin(open *productionOpen) (*productionGenerationBegin, error) {
	if open == nil {
		return nil, errProductionGenerationBeginUnavailable
	}
	open.mu.Lock()
	durable := open.durable
	config := open.config
	closed := open.closed
	open.mu.Unlock()
	if closed || durable == nil || config == nil {
		return nil, errProductionGenerationBeginUnavailable
	}
	durable.mu.Lock()
	lifecycle := durable.lifecycle
	durableClosed := durable.closed
	durable.mu.Unlock()
	if durableClosed || lifecycle == nil {
		return nil, errProductionGenerationBeginUnavailable
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
	// this tab. A stale, foreign or closed one refuses here, before any ledger row
	// is written, which is the same fence the generation reads already use.
	current := func(ctx context.Context, scope generationowner.Scope) error {
		value, found, err := lifecycle.CurrentProjectDurable(ctx, projectsession.Owner{SessionID: scope.Session, ClientID: scope.Tab})
		if err != nil {
			return err
		}
		if !found || value.ProjectID != scope.Project || value.LeaseID != scope.Epoch {
			return errors.New("GENERATION_EPOCH_STALE")
		}
		return nil
	}
	registry, err := generationowner.NewRegistry(generationowner.RegistryOptions{
		LoadModel: loadModel,
		Current:   current,
		AuthorityLock: func(ctx context.Context, scope generationowner.Scope) (func(), error) {
			if err := current(ctx, scope); err != nil {
				return nil, err
			}
			return func() {}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	inner := &syntheticGeneration{
		lifecycle: lifecycle,
		registry:  registry,
		loadModel: loadModel,
	}
	return &productionGenerationBegin{open: open, inner: inner}, nil
}

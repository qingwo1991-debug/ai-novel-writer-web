//go:build linux

package main

import "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"

// newProductionGenerationReads lends the production lane exactly the two
// committed-state generation reads and nothing else.
//
// Both are SELECT-only projections of the author's own ledger rows in the
// admitted project database: the run list, and one run view including its
// attempt/budget diagnostics. Neither one reserves an attempt, touches a model
// profile, opens an egress endpoint or dispatches to a provider, so this lane can
// answer them while the execution family stays mostly unbound. The lane does
// derive a real model-egress policy from the committed config
// (production_model_egress_linux.go) and builds its one executor from it, and it
// owns generation:begin and generation:execute over that executor
// (production_generation_begin_linux.go), but every other execution name
// (resume/restart/pause/cancel and the candidate mutations) stays unbound.
//
// The projection is reused verbatim from the durable S1 provider instead of being
// rewritten, so the browser receives the same MainGenerationRunView the verified
// synthetic lane returns. A nil runtime lends nothing: the caller still binds,
// but every field is nil, BindGenerationReads installs no name, and both
// operations keep failing closed at 501 GO_OPERATION_NOT_MIGRATED.
func newProductionGenerationReads(durable *productionDurableOpen) *transport.GenerationProvider {
	provider := &transport.GenerationProvider{}
	if durable == nil {
		return provider
	}
	durable.mu.Lock()
	lifecycle := durable.lifecycle
	closed := durable.closed
	durable.mu.Unlock()
	if closed || lifecycle == nil {
		return provider
	}
	// The reads resolve the caller's frozen session context against the live
	// account lease, so they need the same LEASE OWNER derivation the lifecycle
	// uses; the per-tab half of that owner is what keeps a tab which never opened
	// the project refused.
	reads := &syntheticGeneration{lifecycle: lifecycle, leaseOwner: productionLeaseOwner}
	provider.Read = reads.ReadCommitted
	provider.List = reads.List
	return provider
}

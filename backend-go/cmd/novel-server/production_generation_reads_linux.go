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
// answer them while its execution half stays unbound: NOVEL_DATA_ROOT never
// configures model origins, and binding begin/execute here would mean inventing
// a dispatch seam the production lane does not have.
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
	reads := &syntheticGeneration{lifecycle: lifecycle}
	provider.Read = reads.ReadCommitted
	provider.List = reads.List
	return provider
}

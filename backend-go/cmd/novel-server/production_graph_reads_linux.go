//go:build linux

package main

import "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"

// newProductionGraphReads lends the production lane the graph generation
// recovery projection and the finalization slot read, and nothing else of that
// family.
//
// Both names are served by the durable implementation the committed generation
// reads already use (newProductionGenerationReads), constructed the same way: of
// syntheticGeneration's fields only lifecycle is set, because GraphRead and
// FinalizationRead do nothing with the rest. Each one re-admits the exact frozen
// cookie/tab/lease fence through BeginGenerationLedgerDB and then projects the
// author's own ledger rows, so neither reserves an attempt, resolves a
// credential, opens an egress endpoint or dispatches to a provider. The graph
// family's begin, execute, confirm and cancel therefore stay unbound here.
//
// The provider type is transport.GraphGenerationProvider, and BindGraphGeneration
// reads exactly two of its fields: Read, which GraphRead satisfies, and
// FinalizationRead, whose field name, arity and argument type GraphRead's
// sibling already matches. The binder unwraps the single-key family envelope
// first, so the methods receive the frozen argument itself - a `{ handle }`
// object for the graph read and a `{ slot }` object for the finalization read -
// not the envelope around it.
//
// A nil runtime, a closed runtime and a runtime without a lifecycle lend
// nothing: both fields stay nil, so the binder installs no finalization read
// (that name keeps failing closed at 501 GO_OPERATION_NOT_MIGRATED) and the
// graph read, which the binder always lists, answers
// GENERATION_OPERATION_UNSUPPORTED instead of naming a view. That branch is
// defensive: productionOpen.durable is nil only while startup is still admitting
// projects, or after Close. Once bound, the two names refuse rather than guess
// (synthetic_graph_linux.go): a handle that does not name a run frozen under this
// very lease, a run whose manifest froze no graph context, and a finalization
// slot another project owns are each answered with their documented refusal
// code, never with a fabricated view.
func newProductionGraphReads(durable *productionDurableOpen) *transport.GraphGenerationProvider {
	provider := &transport.GraphGenerationProvider{}
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
	// Only the finalization read is lent. generation:begin in this lane freezes
	// no graph context (it is the reduced begin, not the batch begin), so
	// GraphRead could only ever answer GENERATION_GRAPH_CONTEXT_INVALID - a 200
	// is unreachable. Binding a name that can only refuse is exactly what this
	// lane refuses to do, so graph-generation:read stays at 501 until a begin
	// that freezes the graph context is itself bound.
	provider.FinalizationRead = reads.FinalizationRead
	return provider
}

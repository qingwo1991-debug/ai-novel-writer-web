//go:build linux

package main

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionGraphReads lends the production lane exactly one name of the graph
// family: the finalization slot read. It binds that one name itself instead of
// calling transport.BindGraphGeneration.
//
// BindGraphGeneration always installs graph-generation:read - that row is in its
// list unconditionally - and, when no read is lent, installs a handler that
// answers GENERATION_OPERATION_UNSUPPORTED. That is a bound name that can only
// refuse, and this lane does not ship those: a browser user would get a 500 for
// a feature that does not exist instead of the fail-closed 501 that says the
// server cannot do it yet. The begin that freezes a graph context is now served
// (graph-generation:begin, production_open_linux.go), so the original reason for
// leaving this read unbound - a projection with nothing to project - no longer
// holds. graph-generation:read is bound by the sibling graph slice
// (production_graph_generation_linux.go), which answers the reload path begin's
// replay never sees, so this binder still lends exactly one name.
//
// The finalization slot read is a real projection: it answers the recovery of
// the run that the slot currently owns - the row whose committed slot
// re-derives into the requested key - JSON null when it owns none, or its
// documented refusal (GENERATION_FINALIZATION_CONTEXT_INVALID for a row whose
// committed slot cannot be re-proved, GENERATION_FINALIZATION_SLOT_CONFLICT for
// a row this project does not own, GENERATION_FINALIZATION_SLOT_INVALID for a
// slot that is not a slot). Of syntheticGeneration's fields only lifecycle is
// set, because FinalizationRead touches nothing else, and the read reserves no
// attempt, resolves no credential, opens no egress endpoint and dispatches to no
// provider.
type productionGraphReads struct {
	read func(context.Context, transport.Caller, map[string]any, any) (any, error)
}

// newProductionGraphReads builds the reader from the lane's durable open. A nil
// or closed runtime lends nothing, and Bind then installs nothing, so the name
// keeps failing closed.
func newProductionGraphReads(durable *productionDurableOpen) *productionGraphReads {
	if durable == nil {
		return &productionGraphReads{}
	}
	durable.mu.Lock()
	lifecycle := durable.lifecycle
	closed := durable.closed
	durable.mu.Unlock()
	if closed || lifecycle == nil {
		return &productionGraphReads{}
	}
	reads := &syntheticGeneration{lifecycle: lifecycle}
	return &productionGraphReads{read: reads.FinalizationRead}
}

// Bind installs finalization-generation:read and nothing else.
func (g *productionGraphReads) Bind(server *transport.Server) error {
	if server == nil {
		return errors.New("TRANSPORT_SERVER_REQUIRED")
	}
	if g == nil || g.read == nil {
		return nil
	}
	read := g.read
	return server.Bind("finalization-generation:read", func(ctx context.Context, caller transport.Caller, args []any) (any, error) {
		// Wire arity is the request arity plus the frozen expected context.
		if len(args) != 2 {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[1].(map[string]any)
		if !ok || len(raw) != 3 {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected[key] = text
		}
		// The request is `{ slot }` and nothing else: any other key is refused
		// before the ledger is read, which is what the family binder does when it
		// unwraps this channel.
		slot, err := productionFinalizationSlotRequest(args[0])
		if err != nil {
			return productionGraphReadFailure(err)
		}
		if ctx == nil {
			return productionGraphReadFailure(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		value, err := read(ctx, caller, expected, slot)
		if err != nil {
			return productionGraphReadFailure(err)
		}
		return value, nil
	})
}

// productionFinalizationSlotRequest is the `{ slot }` unwrap of
// internal/transport/graph_generation.go with the same single-key rule and the
// same refusal code.
func productionFinalizationSlotRequest(value any) (any, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("GENERATION_FINALIZATION_SLOT_INVALID")
	}
	slot, present := object["slot"]
	if !present || len(object) != 1 {
		return nil, errors.New("GENERATION_FINALIZATION_SLOT_INVALID")
	}
	return slot, nil
}

// productionGraphReadFailure is the read half of the family adapter
// (internal/transport/generation.go generationFailure): a read op surfaces its
// HTTP error, and anything that is not one becomes a symbolic 500 rather than a
// raw Go error carrying a path or a driver message.
func productionGraphReadFailure(err error) (any, error) {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return nil, security.NewHTTPError(public.Status, public.Code)
	}
	return nil, security.NewHTTPError(http.StatusInternalServerError, productionGraphReadCode(err))
}

// productionGraphReadCode keeps an all-caps symbolic code and answers
// REQUEST_FAILED for anything else.
func productionGraphReadCode(err error) string {
	if err == nil {
		return "REQUEST_FAILED"
	}
	code := err.Error()
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

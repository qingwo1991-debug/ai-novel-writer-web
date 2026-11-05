package transport

import (
	"context"
	"errors"
)

// GraphGenerationProvider is the graph and finalization family lane. Its
// operations carry `{ handle }` or `{ slot }` rather than the bare value, so
// they cannot share BindGeneration's rows even though they share its envelopes.
type GraphGenerationProvider struct {
	Read func(context.Context, Caller, map[string]any, any) (any, error)
	// FinalizationRead serves finalization-generation:read, whose request is
	// `{ slot }` and whose answer is the recovery of the run that slot currently
	// owns, or JSON null when it owns none. A nil field leaves the channel
	// unbound, so it keeps answering 501 GO_OPERATION_NOT_MIGRATED: a lane may
	// serve the slot read without claiming begin, execute or commit, which need
	// the run admission and the model dispatch this binder does not have.
	FinalizationRead func(context.Context, Caller, map[string]any, any) (any, error)
}

// BindGraphGeneration binds the graph generation recovery projection and, when
// the provider implements it, the finalization slot read.
func (s *Server) BindGraphGeneration(provider *GraphGenerationProvider) error {
	if provider == nil {
		return errors.New("GENERATION_PROVIDER_REQUIRED")
	}
	operations := []generationOperation{
		{"graph-generation:read", 1, false, func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			if provider.Read == nil {
				return nil, errors.New("GENERATION_OPERATION_UNSUPPORTED")
			}
			handle, err := familyRequestHandle(args[0], "GENERATION_GRAPH_REQUEST_INVALID")
			if err != nil {
				return nil, err
			}
			return provider.Read(ctx, caller, expected, handle)
		}},
	}
	if provider.FinalizationRead != nil {
		read := provider.FinalizationRead
		handler := func(ctx context.Context, caller Caller, expected map[string]any, args []any) (any, error) {
			slot, err := familyRequestField(args[0], "slot", "GENERATION_FINALIZATION_SLOT_INVALID")
			if err != nil {
				return nil, err
			}
			return read(ctx, caller, expected, slot)
		}
		operations = append(operations, generationOperation{"finalization-generation:read", 1, false, handler})
	}
	return s.bindGenerationOperations(operations)
}

package transport

import (
	"context"
	"errors"
)

// GraphGenerationProvider is the graph and finalization family lane. Its
// operations carry `{ handle }` rather than the bare handle, so they cannot
// share BindGeneration's rows even though they share its envelopes.
type GraphGenerationProvider struct {
	Read func(context.Context, Caller, map[string]any, any) (any, error)
}

// BindGraphGeneration binds the graph generation recovery projection.
func (s *Server) BindGraphGeneration(provider *GraphGenerationProvider) error {
	if provider == nil {
		return errors.New("GENERATION_PROVIDER_REQUIRED")
	}
	return s.bindGenerationOperations([]generationOperation{
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
	})
}

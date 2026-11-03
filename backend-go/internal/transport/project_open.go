package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// ProjectReadOpenProvider accepts only a server-issued catalog key. A grant
// target needs a separate grant resolver and is deliberately not admitted by
// this binding. The provider must return observed read-only lifecycle state.
type ProjectReadOpenProvider interface {
	OpenReadOnlyProject(context.Context, Caller, string, string) (any, error)
}

type ProjectReadOpenProviderFunc func(context.Context, Caller, string, string) (any, error)

func (f ProjectReadOpenProviderFunc) OpenReadOnlyProject(ctx context.Context, caller Caller, key, token string) (any, error) {
	if f == nil {
		return nil, errors.New("PROJECT_READ_OPEN_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, key, token)
}

// BindProjectReadOpen binds just project:open. It cannot accidentally expose
// project:save, close, or runtime readiness from the all-or-nothing binder.
func (s *Server) BindProjectReadOpen(provider ProjectReadOpenProvider) error {
	if provider == nil {
		return errors.New("PROJECT_READ_OPEN_PROVIDER_REQUIRED")
	}
	return s.Bind("project:open", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 3 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		key, keyOK := args[0].(string)
		token, tokenOK := args[1].(string)
		if !keyOK || key == "" || !tokenOK || token == "" {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if args[2] != nil {
			if path, ok := args[2].(string); !ok || path == "" {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		// The renderer path is never forwarded to the catalog-backed provider.
		return provider.OpenReadOnlyProject(ctx, caller, key, token)
	})
}

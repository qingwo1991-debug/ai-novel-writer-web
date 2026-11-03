package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// ProjectReadOpenProvider accepts only a server-issued catalog key. The
// provider must return observed read-only lifecycle state.
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

// ProjectGrantReadOpenProvider consumes a caller-owned one-use grant. Its
// implementation must resolve the grant against trusted server state before
// entering project admission; a grant ID is never a filesystem path.
type ProjectGrantReadOpenProvider interface {
	OpenGrantedReadOnlyProject(context.Context, Caller, string, string) (any, error)
}

type ProjectGrantReadOpenProviderFunc func(context.Context, Caller, string, string) (any, error)

func (f ProjectGrantReadOpenProviderFunc) OpenGrantedReadOnlyProject(ctx context.Context, caller Caller, grantID, token string) (any, error) {
	if f == nil {
		return nil, errors.New("PROJECT_GRANT_READ_OPEN_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, grantID, token)
}

// BindProjectReadOpen binds just project:open. It cannot accidentally expose
// project:save, close, or runtime readiness from the all-or-nothing binder.
func (s *Server) BindProjectReadOpen(provider ProjectReadOpenProvider) error {
	return s.BindProjectReadOpenWithGrants(provider, nil)
}

// BindProjectReadOpenWithGrants also accepts an exact grant target when a
// trusted grant resolver is supplied. A path-bearing map is never accepted.
// Passing nil keeps the catalog-only binding's original behavior.
func (s *Server) BindProjectReadOpenWithGrants(provider ProjectReadOpenProvider, grants ProjectGrantReadOpenProvider) error {
	if provider == nil {
		return errors.New("PROJECT_READ_OPEN_PROVIDER_REQUIRED")
	}
	return s.Bind("project:open", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 3 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		token, tokenOK := args[1].(string)
		if !tokenOK || token == "" || !utf8.ValidString(token) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if args[2] != nil {
			if path, ok := args[2].(string); !ok || path == "" {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
		}
		switch target := args[0].(type) {
		case string:
			if target == "" || !utf8.ValidString(target) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			// The renderer path is never forwarded to the catalog-backed provider.
			return provider.OpenReadOnlyProject(ctx, caller, target, token)
		case map[string]any:
			if grants == nil || len(target) < 1 || len(target) > 2 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			grantID, ok := target["grantId"].(string)
			if !ok || grantID == "" || !utf8.ValidString(grantID) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if name, present := target["displayName"]; present {
				label, ok := name.(string)
				if !ok || !utf8.ValidString(label) {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			for field := range target {
				if field != "grantId" && field != "displayName" {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			return grants.OpenGrantedReadOnlyProject(ctx, caller, grantID, token)
		default:
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
	})
}

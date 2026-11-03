package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// ProjectReadCloseProvider closes only the caller's currently admitted read
// lease. It must not report success until the admitted database is closed.
type ProjectReadCloseProvider interface {
	CloseReadOnlyProject(context.Context, Caller, string, any) (any, error)
}

type ProjectReadCloseProviderFunc func(context.Context, Caller, string, any) (any, error)

func (f ProjectReadCloseProviderFunc) CloseReadOnlyProject(ctx context.Context, caller Caller, path string, expected any) (any, error) {
	if f == nil {
		return nil, errors.New("PROJECT_READ_CLOSE_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, path, expected)
}

// BindProjectReadClose binds only db:close. The normal project binder also
// includes author writes, so the synthetic read-only runtime cannot use it.
func (s *Server) BindProjectReadClose(provider ProjectReadCloseProvider) error {
	if provider == nil {
		return errors.New("PROJECT_READ_CLOSE_PROVIDER_REQUIRED")
	}
	return s.Bind("db:close", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 2 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := draftExpectedContext(args, 1)
		if err != nil {
			return nil, err
		}
		path, ok := args[0].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected["projectPath"] {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		return provider.CloseReadOnlyProject(ctx, caller, path, expected)
	})
}

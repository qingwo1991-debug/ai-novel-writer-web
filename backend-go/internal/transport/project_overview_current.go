package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// CurrentOverviewProvider returns only facts from the caller's already-open
// private project snapshot, serialized under its exact live lease.
type CurrentOverviewProvider interface {
	ReadCurrentOverviewJSON(context.Context, Caller, any) (json.RawMessage, error)
}

type CurrentOverviewProviderFunc func(context.Context, Caller, any) (json.RawMessage, error)

func (f CurrentOverviewProviderFunc) ReadCurrentOverviewJSON(ctx context.Context, caller Caller, expected any) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("PROJECT_OVERVIEW_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

func (s *Server) BindCurrentOverview(provider CurrentOverviewProvider) error {
	if provider == nil {
		return errors.New("PROJECT_OVERVIEW_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("PROJECT_OVERVIEW_PROVIDER_REQUIRED")
		}
	}
	const name = "project:overview-current"
	handler := func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 1 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := draftExpectedContext(args, 0)
		if err != nil {
			return nil, err
		}
		wire, err := provider.ReadCurrentOverviewJSON(ctx, caller, expected)
		if err != nil {
			return nil, editorNavigationError(err, "PROJECT_OVERVIEW_FAILED")
		}
		if !json.Valid(wire) {
			return nil, errors.New("PROJECT_OVERVIEW_FAILED")
		}
		return json.RawMessage(append([]byte(nil), wire...)), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registry.Lookup(name); !ok {
		return errors.New("OPERATION_NOT_EXPOSED")
	}
	if _, ok := s.handlers[name]; ok {
		return errors.New("OPERATION_ALREADY_BOUND")
	}
	s.handlers[name] = handler
	return nil
}

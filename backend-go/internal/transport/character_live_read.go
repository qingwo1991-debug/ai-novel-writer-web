package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// CharacterLiveReadProvider returns a fully serialized read from the caller's
// current project lease. It must keep that lease and its source proof through
// serialization; a database or repository must never escape to transport.
type CharacterLiveReadProvider interface {
	ReadCharactersJSON(context.Context, Caller, any, string) (json.RawMessage, error)
}

type CharacterLiveReadProviderFunc func(context.Context, Caller, any, string) (json.RawMessage, error)

func (f CharacterLiveReadProviderFunc) ReadCharactersJSON(ctx context.Context, caller Caller, expected any, operation string) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("CHARACTER_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected, operation)
}

// BindCharacterLiveReads binds only the three original character reads.
// db:character-roster-commit is deliberately NOT among them: it is the author
// lane's write and is bound on its own by BindCharacterRosterCommit, so binding
// the reads can never grant a write.
func (s *Server) BindCharacterLiveReads(provider CharacterLiveReadProvider) error {
	if provider == nil {
		return errors.New("CHARACTER_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("CHARACTER_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:character-get-all", 2, 0},
		{"db:character-roster-read", 2, 0},
		{"character-identity:read", 1, -1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) {
				code := charactersCode(err)
				var public *security.HTTPError
				if errors.As(err, &public) && public != nil && public.Code == code {
					return nil, security.NewHTTPError(public.Status, code)
				}
				return nil, errors.New(code)
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			raw, ok := args[arity-1].(map[string]any)
			if !ok || len(raw) != 3 {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected := make(map[string]any, 3)
			for _, key := range []string{"projectId", "leaseId", "projectPath"} {
				text, ok := raw[key].(string)
				if !ok || text == "" || !utf8.ValidString(text) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
				}
				expected[key] = text
			}
			if pathIndex >= 0 {
				path, ok := args[pathIndex].(string)
				if !ok || path == "" || !utf8.ValidString(path) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
				}
				if path != expected["projectPath"] {
					return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
				}
			}
			wire, err := provider.ReadCharactersJSON(ctx, caller, expected, name)
			if err != nil {
				return fail(err)
			}
			if !json.Valid(wire) {
				return fail(errors.New("CHARACTER_PROVIDER_UNAVAILABLE"))
			}
			return json.RawMessage(append([]byte(nil), wire...)), nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, operation := range operations {
		if _, ok := s.registry.Lookup(operation.name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[operation.name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

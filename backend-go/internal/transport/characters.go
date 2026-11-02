package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// CharactersProvider borrows an already admitted caller/session/tab-owned project.
// All three frozen context fields must match before exposing the repository. A
// renderer path is never permission to open, migrate, or switch a database. The
// release holds project authority through the read, serialization, or denial.
type CharactersProvider interface {
	BeginCharacters(context.Context, Caller, any) (*store.Characters, func(), error)
}

type CharactersProviderFunc func(context.Context, Caller, any) (*store.Characters, func(), error)

func (f CharactersProviderFunc) BeginCharacters(ctx context.Context, caller Caller, expected any) (*store.Characters, func(), error) {
	if f == nil {
		return nil, nil, errors.New("CHARACTER_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// BindCharacters implements the original two database reads and the generation
// controller's identity read. The commit name preserves ONLY an unsupported
// author/identity-lane denial, not an implemented write or generation capability.
// register('character-identity:read', 0, ...) consumes [context], unlike the two
// database reads' [expectedPath, context]. Do not add a path-only fallback.
func (s *Server) BindCharacters(provider CharactersProvider) error {
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
		name string
		arity, pathIndex int
	}{
		{"db:character-get-all", 2, 0},
		{"db:character-roster-read", 2, 0},
		{"character-identity:read", 1, -1},
		{"db:character-roster-commit", 3, 1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			denial := name == "db:character-roster-commit"
			fail := func(err error) (any, error) {
				code := charactersCode(err)
				if denial {
					return charactersSerialized(map[string]any{"success": false, "error": "Error: " + code})
				}
				var public *security.HTTPError
				if errors.As(err, &public) && public != nil && public.Code == code {
					return nil, security.NewHTTPError(public.Status, code)
				}
				return nil, errors.New(code)
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			// Reject extras on the ORIGINAL object, before any helper clones it.
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
			// Only consume the captured project ID AFTER the provider has admitted
			// the entire identity against the caller; never take a request's ID
			// as authorization. Capture before provider code can mutate its clone.
			projectID := expected["projectId"].(string)
			manual := false
			if denial {
				if request, ok := args[0].(map[string]any); ok {
					manual = request["intent"] == "manual_edit"
				}
			}
			repository, release, err := provider.BeginCharacters(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("CHARACTER_PROVIDER_UNAVAILABLE"))
			}
			if denial {
				// db-controller's manual_edit branch requires author scope; all
				// other identity-schema roster commits require the identity lane.
				// Neither lane is supplied by a read borrow or a renderer handle.
				if manual {
					return fail(errors.New("CHARACTER_AUTHOR_SCOPE_REQUIRED"))
				}
				return fail(errors.New("CHARACTER_ID_WRITE_REQUIRED"))
			}
			var result any
			switch name {
			case "db:character-get-all":
				var rows []map[string]any
				rows, err = repository.All(ctx)
				if rows == nil && err == nil {
					rows = []map[string]any{}
				}
				result = rows
			case "db:character-roster-read":
				// Roster, hashes, source proof and currentDerivedFields MUST be
				// read in one repository snapshot, not separate transactions.
				result, err = repository.ReadForProject(ctx, projectID)
			case "character-identity:read":
				// Public proposal-service view, NOT raw identity registry rows.
				result, err = repository.IdentityView(ctx)
			}
			if err != nil {
				return fail(err)
			}
			wire, err := charactersSerialized(result)
			if err != nil {
				return fail(err)
			}
			return wire, nil
		}
	}
	// Preflight all four names before publishing any handler.
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

// Freeze public bytes under the project hold; Server only wraps the immutable
// JSON in {result:...} after release. Marshal failures retain generic redaction.
func charactersSerialized(value any) (any, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, errors.New("CHARACTER_OPERATION_FAILED")
	}
	return json.RawMessage(encoded), nil
}

func charactersCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "REQUEST_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "REQUEST_DEADLINE_EXCEEDED"
	}
	code := ""
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return "CHARACTER_OPERATION_FAILED"
		}
		code = public.Code
	} else if err != nil {
		code = err.Error()
	}
	switch code {
	case "INVALID_ARGUMENTS", "CHARACTER_INPUT_UNSUPPORTED", "CHARACTER_ID_SCHEMA_REQUIRED", "CHARACTER_ID_META_MISSING",
		"CHARACTER_ROSTER_SCHEMA_UNSUPPORTED", "CHARACTER_ROSTER_META_MISSING",
		"CHARACTER_ROSTER_INPUT_UNSUPPORTED", "CHARACTER_PROVIDER_UNAVAILABLE",
		"CHARACTER_AUTHOR_SCOPE_REQUIRED", "CHARACTER_ID_WRITE_REQUIRED",
		"PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED",
		"PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_MANAGER_CLOSED",
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_SESSION_NOT_OWNED",
		"PROJECT_OPEN_IN_ANOTHER_TAB", "PROJECT_REQUEST_IN_FLIGHT", "PROJECT_NOT_OPEN",
		"PROJECT_ROOT_CHANGED", "PROJECT_SOURCE_CHANGED", "PROJECT_HANDLE_CLOSED",
		"PORTABLE_AUTHORITY_UNSUPPORTED", "PORTABLE_RUNTIME_FROZEN":
		return code
	}
	return "CHARACTER_OPERATION_FAILED"
}

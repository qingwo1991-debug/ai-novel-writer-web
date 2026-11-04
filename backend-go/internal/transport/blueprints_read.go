package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindBlueprintReads binds only the three blueprint and character-sync reads
// that the durable lane serves from its admitted author database. All three
// carry a project path and the frozen three-key session context as the LAST
// argument: db:blueprint-get takes (chapter key, path, context),
// db:blueprint-character-sync-get takes (sync operation id, path, context), and
// db:blueprint-character-sync-list-pending takes (path, context).
//
// db:blueprint-get-all is deliberately absent: the editor stage read binder
// already serves it, and binding it here would fail OPERATION_ALREADY_BOUND.
// The five mutating names in the same family (commit-range, delete,
// update-notes, upsert, upsert-many) stay unbound, so they keep failing closed.
//
// The provider is the same seam BindBlueprints already uses: it lends one
// admitted repository per request and never a database handle. Arity, path
// index, argument validation and wire normalization mirror BindBlueprints
// exactly, so a lane that takes this binder answers these reads with the same
// shapes and the same absence behaviour (null object, empty list) instead of an
// error.
func (s *Server) BindBlueprintReads(provider BlueprintProvider) error {
	if provider == nil {
		return errors.New("BLUEPRINT_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("BLUEPRINT_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:blueprint-get", 3, 1},
		{"db:blueprint-character-sync-get", 3, 1},
		{"db:blueprint-character-sync-list-pending", 2, 0},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return nil, err
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			// db:blueprint-character-sync-get is the only read here with a typed
			// leading argument: the transport binder requires a string operation
			// id. db:blueprint-get hands its leading chapter key to the store
			// unvalidated, exactly like BindBlueprints, so a key the store cannot
			// bind becomes the symbolic BLUEPRINT_OPERATION_FAILED instead of a
			// reflection error.
			var syncID string
			if name == "db:blueprint-character-sync-get" {
				if syncID, ok = args[0].(string); !ok {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			if ctx == nil {
				return nil, errors.New("BLUEPRINT_READ_FAILED")
			}
			repository, release, err := provider.BeginBlueprints(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, blueprintReadError(err)
			}
			if repository == nil || release == nil {
				return nil, errors.New("BLUEPRINT_PROVIDER_UNAVAILABLE")
			}
			var result any
			switch name {
			case "db:blueprint-get":
				result, err = repository.Get(ctx, args[0])
			case "db:blueprint-character-sync-get":
				result, err = repository.GetSync(ctx, syncID)
			case "db:blueprint-character-sync-list-pending":
				result, err = repository.PendingSync(ctx)
			}
			if err != nil {
				return nil, blueprintReadError(err)
			}
			return reviewRevisionWire(result), nil
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

// blueprintReadError keeps these read failures symbolic. A borrow error may
// wrap a filesystem path or raw SQLite text, so only the finite vocabulary
// below is ever reflected; nothing else reaches the browser verbatim.
// blueprintCode is the original transport binder's own finite mapping, so a
// store error keeps exactly the code BindBlueprints would have returned for
// the same read.
func blueprintReadError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return errors.New("BLUEPRINT_READ_FAILED")
		}
		// Rebuild so a provider's own message never becomes public text.
		switch public.Code {
		case "INVALID_ARGUMENTS", "PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED":
			return security.NewHTTPError(public.Status, public.Code)
		}
		return errors.New("BLUEPRINT_READ_FAILED")
	}
	return errors.New(blueprintCode(err))
}

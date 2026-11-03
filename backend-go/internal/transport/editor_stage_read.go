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

// EditorStageReadProvider serializes one read while the caller's exact project
// lease and private SQLite transaction are held. It does not lend a database.
type EditorStageReadProvider interface {
	ReadEditorStageJSON(context.Context, Caller, any, string) (json.RawMessage, error)
}

type EditorStageReadProviderFunc func(context.Context, Caller, any, string) (json.RawMessage, error)

func (f EditorStageReadProviderFunc) ReadEditorStageJSON(ctx context.Context, caller Caller, expected any, operation string) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("EDITOR_STAGE_READ_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected, operation)
}

// BindEditorStageReads binds only the two reads needed to populate the stage.
// Project-core updates and blueprint mutations remain unported 501 responses.
func (s *Server) BindEditorStageReads(provider EditorStageReadProvider) error {
	if provider == nil {
		return errors.New("EDITOR_STAGE_READ_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("EDITOR_STAGE_READ_PROVIDER_REQUIRED")
		}
	}
	names := []string{"db:project-core-get", "db:blueprint-get-all"}
	handlers := make(map[string]Operation, len(names))
	for _, operation := range names {
		name := operation
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
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
			if ctx == nil {
				return nil, errors.New("EDITOR_STAGE_READ_FAILED")
			}
			wire, err := provider.ReadEditorStageJSON(ctx, caller, expected, name)
			if err != nil {
				return nil, editorStageReadError(err, name)
			}
			if !json.Valid(wire) {
				return nil, errors.New("EDITOR_STAGE_READ_FAILED")
			}
			return json.RawMessage(append([]byte(nil), wire...)), nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, name := range names {
		if _, ok := s.registry.Lookup(name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

func editorStageReadError(err error, operation string) error {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	if err != nil {
		switch err.Error() {
		case "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_MANAGER_CLOSED",
			"PROJECT_SESSION_STALE", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_NOT_OPEN":
			return errors.New(err.Error())
		}
	}
	if operation == "db:project-core-get" {
		return errors.New("PROJECT_CORE_READ_FAILED")
	}
	return errors.New("BLUEPRINT_OPERATION_FAILED")
}

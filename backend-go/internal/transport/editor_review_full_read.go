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

type EditorReviewFullReadProvider interface {
	ReadReviewRevisionFullJSON(context.Context, Caller, any, string, int64) (json.RawMessage, error)
}

type EditorReviewFullReadProviderFunc func(context.Context, Caller, any, string, int64) (json.RawMessage, error)

func (f EditorReviewFullReadProviderFunc) ReadReviewRevisionFullJSON(ctx context.Context, caller Caller, expected any, operation string, id int64) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("REVIEW_REVISION_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected, operation, id)
}

// BindEditorReviewFullReads admits only the two nullable full-row reads used
// by the editor. All review and revision mutations remain unbound.
func (s *Server) BindEditorReviewFullReads(provider EditorReviewFullReadProvider) error {
	if provider == nil {
		return errors.New("REVIEW_REVISION_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("REVIEW_REVISION_PROVIDER_REQUIRED")
		}
	}
	names := []string{"db:review-get-full", "db:revision-get-full"}
	handlers := make(map[string]Operation, len(names))
	for _, operation := range names {
		name := operation
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != 3 {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, 2)
			if err != nil {
				return nil, err
			}
			path, ok := args[1].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			id, ok := draftSafeInteger(args[0], 1)
			if !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if ctx == nil {
				return nil, errors.New("REVIEW_REVISION_OPERATION_FAILED")
			}
			wire, err := provider.ReadReviewRevisionFullJSON(ctx, caller, expected, name, id)
			if err != nil {
				return nil, editorReviewReadError(err)
			}
			if !json.Valid(wire) || !(len(wire) == 4 && string(wire) == "null" || len(wire) > 0 && wire[0] == '{') {
				return nil, errors.New("REVIEW_REVISION_OPERATION_FAILED")
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

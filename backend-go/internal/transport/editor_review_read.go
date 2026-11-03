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

// EditorReviewReadProvider serializes metadata while the exact private
// project lease and read transaction are still held. It cannot return a DB.
type EditorReviewReadProvider interface {
	ReadReviewRevisionJSON(context.Context, Caller, any, string, int64) (json.RawMessage, error)
}

type EditorReviewReadProviderFunc func(context.Context, Caller, any, string, int64) (json.RawMessage, error)

func (f EditorReviewReadProviderFunc) ReadReviewRevisionJSON(ctx context.Context, caller Caller, expected any, operation string, baseDraftID int64) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("REVIEW_REVISION_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected, operation, baseDraftID)
}

// BindEditorReviewReads binds the two original metadata lists used when a
// draft tab loads. All review/revision mutations stay unported and return 501.
func (s *Server) BindEditorReviewReads(provider EditorReviewReadProvider) error {
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
	names := []string{"db:revision-get-pending", "db:review-list"}
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
			baseDraftID, ok := draftSafeInteger(args[0], 1)
			if !ok {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			if ctx == nil {
				return nil, errors.New("REVIEW_REVISION_OPERATION_FAILED")
			}
			wire, err := provider.ReadReviewRevisionJSON(ctx, caller, expected, name, baseDraftID)
			if err != nil {
				return nil, editorReviewReadError(err)
			}
			if !json.Valid(wire) || len(wire) == 0 || wire[0] != '[' {
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

func editorReviewReadError(err error) error {
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
	return errors.New("REVIEW_REVISION_OPERATION_FAILED")
}

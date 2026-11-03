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

type EditorReviewCycleReadProvider interface {
	ReadAbsentReviewCycleJSON(context.Context, Caller, any, int64) (json.RawMessage, error)
}

type EditorReviewCycleReadProviderFunc func(context.Context, Caller, any, int64) (json.RawMessage, error)

func (f EditorReviewCycleReadProviderFunc) ReadAbsentReviewCycleJSON(ctx context.Context, caller Caller, expected any, reviewID int64) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("REVIEW_CYCLE_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected, reviewID)
}

// BindEditorReviewCycleAbsentRead supports the exact no-cycle result used by
// the first editor read. An existing cycle fails explicitly until the full
// original M03 finding and recheck projection is implemented.
func (s *Server) BindEditorReviewCycleAbsentRead(provider EditorReviewCycleReadProvider) error {
	if provider == nil {
		return errors.New("REVIEW_CYCLE_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("REVIEW_CYCLE_PROVIDER_REQUIRED")
		}
	}
	const name = "db:review-cycle-get"
	handler := func(ctx context.Context, caller Caller, args []any) (any, error) {
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
		reviewID, ok := draftSafeInteger(args[0], 1)
		if !ok {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if ctx == nil {
			return nil, errors.New("REVIEW_CYCLE_OPERATION_FAILED")
		}
		wire, err := provider.ReadAbsentReviewCycleJSON(ctx, caller, expected, reviewID)
		if errors.Is(err, store.ErrReviewCycleProjectionUnavailable) {
			return nil, security.NewHTTPError(http.StatusNotImplemented, "GO_OPERATION_NOT_MIGRATED")
		}
		if err != nil {
			return nil, editorReviewReadError(err)
		}
		if string(wire) != "null" {
			return nil, errors.New("REVIEW_CYCLE_OPERATION_FAILED")
		}
		return json.RawMessage("null"), nil
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

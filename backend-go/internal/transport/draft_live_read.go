package transport

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// DraftLiveReadProvider returns serialized DTO bytes while the exact caller's
// project lease and read transaction are still held. It must not return a DB.
type DraftLiveReadProvider interface {
	ReadDraftJSON(context.Context, Caller, any, string, int64) (json.RawMessage, error)
}

type DraftLiveReadProviderFunc func(context.Context, Caller, any, string, int64) (json.RawMessage, error)

func (f DraftLiveReadProviderFunc) ReadDraftJSON(ctx context.Context, caller Caller, expected any, operation string, number int64) (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("DRAFT_READ_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected, operation, number)
}

// BindDraftLiveReads binds only read operations. Every draft mutation and
// generation/finalization operation retains the ordinary unported 501 result.
func (s *Server) BindDraftLiveReads(provider DraftLiveReadProvider) error {
	if provider == nil {
		return errors.New("DRAFT_READ_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("DRAFT_READ_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name  string
		arity int
	}{
		{"db:draft-list", 3},
		{"db:draft-list-all", 2},
		{"db:draft-get-meta", 3},
		{"db:draft-get-full", 3},
		{"db:draft-get-latest", 3},
		{"db:draft-get-finalized", 3},
		{"db:draft-get-max-finalized-chapter", 2},
		{"db:draft-next-version", 3},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, spec := range operations {
		name, arity := spec.name, spec.arity
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			if len(args) != arity {
				return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return nil, err
			}
			path, ok := args[arity-2].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
			}
			if path != expected["projectPath"] {
				return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
			}
			var number int64
			if arity == 3 {
				if number, ok = draftSafeInteger(args[0], 1); !ok {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			if ctx == nil {
				return nil, errors.New("DRAFT_READ_FAILED")
			}
			wire, err := provider.ReadDraftJSON(ctx, caller, expected, name, number)
			if err != nil {
				return nil, draftLiveReadError(err)
			}
			if !json.Valid(wire) {
				return nil, errors.New("DRAFT_READ_FAILED")
			}
			return json.RawMessage(append([]byte(nil), wire...)), nil
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, spec := range operations {
		if _, ok := s.registry.Lookup(spec.name); !ok {
			return errors.New("OPERATION_NOT_EXPOSED")
		}
		if _, ok := s.handlers[spec.name]; ok {
			return errors.New("OPERATION_ALREADY_BOUND")
		}
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

func draftLiveReadError(err error) error {
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public != nil {
			return public
		}
		return errors.New("DRAFT_READ_FAILED")
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
		log.Printf("novel-server: draft read failed: %v", err)
	}
	return errors.New("DRAFT_READ_FAILED")
}

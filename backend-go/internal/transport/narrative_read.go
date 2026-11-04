package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindNarrativeReads binds only the three narrative thread and consistency
// exemption reads that the durable lane serves from its admitted author
// database. The six mutating names in the same family (plan-create, -update,
// -delete, event-confirm, exemption-save, exemption-revoke) stay unbound here;
// the five plan and exemption writes are bound by BindNarrativeWrites and
// db:narrative-thread-event-confirm by BindNarrativeEventConfirm, which is why
// this binder leaves them out instead of failing them closed.
//
// The provider is the same seam BindNarrative already uses: it lends one
// admitted repository per request and never a database handle. Arity, path
// index and wire normalization mirror BindNarrative exactly, so a lane that
// takes this binder answers these reads with the same shapes and the same
// absence behaviour (an empty list) instead of an error.
func (s *Server) BindNarrativeReads(provider NarrativeProvider) error {
	if provider == nil {
		return errors.New("NARRATIVE_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("NARRATIVE_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:narrative-thread-list", 2, 0},
		{"db:narrative-thread-list-relevant", 3, 1},
		{"db:consistency-exemption-list", 2, 0},
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
			// db:narrative-thread-list-relevant is the only read here with a
			// non-path argument: the relevance filter object at index 0. An
			// absent (null) filter is the original default behaviour and stays
			// valid, but a present non-object is rejected before the borrow.
			// The two arity-2 names carry the path at index 0 and no leading
			// argument, so there is nothing else to validate.
			var input map[string]any
			if name == "db:narrative-thread-list-relevant" && args[0] != nil {
				if input, ok = args[0].(map[string]any); !ok || input == nil {
					return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
				}
			}
			if ctx == nil {
				return nil, errors.New("NARRATIVE_READ_FAILED")
			}
			repository, release, err := provider.BeginNarrative(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, narrativeReadError(err)
			}
			if repository == nil || release == nil {
				return nil, errors.New("NARRATIVE_PROVIDER_UNAVAILABLE")
			}
			var result any
			switch name {
			case "db:narrative-thread-list":
				result, err = repository.List(ctx)
			case "db:narrative-thread-list-relevant":
				result, err = repository.Relevant(ctx, input)
			case "db:consistency-exemption-list":
				result, err = repository.Exemptions(ctx)
			}
			if err != nil {
				return nil, narrativeReadError(err)
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

// narrativeReadError keeps these read failures symbolic. A borrow error may
// wrap a filesystem path or raw SQLite text, so only the finite vocabulary
// below is ever reflected; nothing else reaches the browser verbatim.
func narrativeReadError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return errors.New("NARRATIVE_READ_FAILED")
		}
		// Rebuild so a provider's own message never becomes public text.
		switch public.Code {
		case "INVALID_ARGUMENTS", "PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED":
			return security.NewHTTPError(public.Status, public.Code)
		}
		return errors.New("NARRATIVE_READ_FAILED")
	}
	if err != nil {
		switch err.Error() {
		case "PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE",
			"PROJECT_SESSION_MANAGER_CLOSED", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_NOT_OPEN",
			"PROJECT_REQUEST_IN_FLIGHT", "PROJECT_OPEN_STALE", "PROJECT_ACTIVE_COORDINATOR_CLOSED",
			"PROJECT_ACTIVE_CLEANUP_UNCONFIRMED", "NARRATIVE_PROVIDER_UNAVAILABLE":
			return errors.New(err.Error())
		}
	}
	return errors.New("NARRATIVE_READ_FAILED")
}

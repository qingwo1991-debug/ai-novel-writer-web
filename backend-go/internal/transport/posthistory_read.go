package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindPostHistoryReads binds only the five post-process receipt and LLM ledger
// reads that the durable lane serves from its admitted author database. The
// four mutating names in the same family (create-run, mark-step-ok,
// mark-step-failed, log-llm-call) are absent here on purpose: they belong to
// BindPostHistoryWrites, which a lane installs only when it owns the author
// write lease. A lane that does not bind them keeps answering
// GO_OPERATION_NOT_MIGRATED rather than a handler that could only refuse.
//
// The provider is the same seam BindPostHistory already uses: it lends one
// admitted repository per request and never a database handle. Arity, path
// index, argument validation and wire normalization mirror BindPostHistory
// exactly, so a lane that takes this binder answers these reads with the same
// shapes and the same absence behaviour (null run, empty step/history lists,
// false aggregate) instead of an error.
func (s *Server) BindPostHistoryReads(provider PostHistoryProvider) error {
	if provider == nil {
		return errors.New("POST_HISTORY_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("POST_HISTORY_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:post-process-get-latest-run", 4, 2},
		{"db:post-process-get-steps", 3, 1},
		{"db:post-process-is-all-passed", 4, 2},
		{"db:get-llm-stats", 2, 0},
		{"db:get-llm-history", 3, 1},
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
			// db:get-llm-history is the only read with a non-identity
			// argument: the transport binder defaults it to 50 and rejects
			// values outside JavaScript's safe integer range.
			limit := int64(50)
			if name == "db:get-llm-history" {
				if args[0] != nil {
					if limit, ok = draftSafeInteger(args[0], -9007199254740991); !ok {
						return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
					}
				}
			} else {
				for index := 0; index < pathIndex; index++ {
					if _, ok := args[index].(string); !ok {
						return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
					}
				}
			}
			if ctx == nil {
				return nil, errors.New("POST_HISTORY_READ_FAILED")
			}
			repository, release, err := provider.BeginPostHistory(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return nil, postHistoryReadError(err)
			}
			if repository == nil || release == nil {
				return nil, errors.New("POST_HISTORY_PROVIDER_UNAVAILABLE")
			}
			var result any
			switch name {
			case "db:post-process-get-latest-run":
				result, err = repository.LatestRun(ctx, args[0].(string), args[1].(string))
			case "db:post-process-get-steps":
				result, err = repository.Steps(ctx, args[0].(string))
			case "db:post-process-is-all-passed":
				result, err = repository.AllPassed(ctx, args[0].(string), args[1].(string))
			case "db:get-llm-stats":
				result, err = repository.Stats(ctx)
			case "db:get-llm-history":
				result, err = repository.History(ctx, limit)
			}
			if err != nil {
				return nil, postHistoryReadError(err)
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

// postHistoryReadError keeps these read failures symbolic. A borrow error may
// wrap a filesystem path or raw SQLite text, so only the finite vocabulary
// below is ever reflected; nothing else reaches the browser verbatim.
func postHistoryReadError(err error) error {
	if errors.Is(err, context.Canceled) {
		return errors.New("REQUEST_CANCELED")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errors.New("REQUEST_DEADLINE_EXCEEDED")
	}
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return errors.New("POST_HISTORY_READ_FAILED")
		}
		// Rebuild so a provider's own message never becomes public text.
		switch public.Code {
		case "INVALID_ARGUMENTS", "PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED":
			return security.NewHTTPError(public.Status, public.Code)
		}
		return errors.New("POST_HISTORY_READ_FAILED")
	}
	if err != nil {
		switch err.Error() {
		case "PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_NOT_OWNED", "PROJECT_SESSION_STALE",
			"PROJECT_SESSION_MANAGER_CLOSED", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_NOT_OPEN",
			"PROJECT_REQUEST_IN_FLIGHT", "PROJECT_OPEN_STALE", "PROJECT_ACTIVE_COORDINATOR_CLOSED",
			"PROJECT_ACTIVE_CLEANUP_UNCONFIRMED", "POST_HISTORY_INPUT_INVALID",
			"POST_HISTORY_PROVIDER_UNAVAILABLE":
			return errors.New(err.Error())
		}
	}
	return errors.New("POST_HISTORY_READ_FAILED")
}

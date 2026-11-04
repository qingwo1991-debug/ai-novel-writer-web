package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindPostHistoryWrites binds only the four mutating post-process receipt and
// LLM ledger operations of that family: create-run, mark-step-ok,
// mark-step-failed and log-llm-call. The five reads in the same family stay
// with BindPostHistoryReads, so the two binders together serve the family and
// neither is bound twice.
//
// The provider is the same seam BindPostHistory already uses: it lends one
// admitted repository per request and never a database handle, so the write
// permit is composed and released exactly like the durable draft write's.
// Arity, path index and argument validation mirror BindPostHistory exactly, so
// a lane that takes this binder answers these writes with the same shapes:
// {"success":true} plus id for create-run on commit, and
// {"success":false,"error":"Error: CODE"} on refusal. db:log-llm-call keeps its
// own unusual refusal shape, a bare {"success":false} with no error code.
//
// A refusal is always reported as a business result and never as success, so a
// write that did not commit can never be read as committed.
func (s *Server) BindPostHistoryWrites(provider PostHistoryProvider) error {
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
	// Arity and path index are exactly BindPostHistory's: create-run and
	// log-llm-call carry (input, path, context), mark-step-ok carries
	// (runId, stepKey, path, context) and mark-step-failed carries
	// (runId, stepKey, message, path, context). The frozen context is always the
	// last argument.
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:post-process-create-run", 3, 1},
		{"db:post-process-mark-step-ok", 4, 2},
		{"db:post-process-mark-step-failed", 5, 3},
		{"db:log-llm-call", 3, 1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) { return postHistoryWriteFailure(name, err), nil }
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return fail(err)
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
			// Only create-run and log-llm-call carry an input object at index 0;
			// the two step names carry identity strings up to the path index and
			// nothing else, so those are checked as strings before the borrow.
			var input map[string]any
			if name == "db:post-process-create-run" || name == "db:log-llm-call" {
				if input, ok = args[0].(map[string]any); !ok || input == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			} else {
				for index := 0; index < pathIndex; index++ {
					if _, ok := args[index].(string); !ok {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
				}
			}
			if ctx == nil {
				return fail(errors.New("POST_HISTORY_WRITE_FAILED"))
			}
			repository, release, err := provider.BeginPostHistory(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("POST_HISTORY_PROVIDER_UNAVAILABLE"))
			}
			var result any
			switch name {
			case "db:post-process-create-run":
				result, err = repository.CreateRun(ctx, input)
			case "db:post-process-mark-step-ok":
				err = repository.MarkStepOK(ctx, args[0].(string), args[1].(string))
			case "db:post-process-mark-step-failed":
				err = repository.MarkStepFailed(ctx, args[0].(string), args[1].(string), args[2].(string))
			case "db:log-llm-call":
				err = repository.LogCall(ctx, input)
			}
			if err != nil {
				return fail(err)
			}
			response := map[string]any{"success": true}
			if name == "db:post-process-create-run" {
				response["id"] = result
			}
			return response, nil
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

// postHistoryWriteFailure keeps the mutation refusal envelope identical to
// BindPostHistory. db:log-llm-call is deliberately different: the original
// answers a failed ledger append with a bare {"success":false} and no error
// code at all, and that shape must not gain one. Only postHistoryCode's finite
// vocabulary is ever reflected, so a wrapped filesystem path or raw SQLite text
// never reaches the browser.
func postHistoryWriteFailure(name string, err error) map[string]any {
	if name == "db:log-llm-call" {
		return map[string]any{"success": false}
	}
	return map[string]any{"success": false, "error": "Error: " + postHistoryCode(err)}
}

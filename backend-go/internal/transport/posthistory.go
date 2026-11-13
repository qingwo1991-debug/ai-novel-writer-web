package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// PostHistoryProvider lends only an admitted project for the complete operation.
// Run IDs and finalization identities are project facts, not path capabilities.
type PostHistoryProvider interface {
	BeginPostHistory(context.Context, Caller, any) (*store.PostHistory, func(), error)
}
type PostHistoryProviderFunc func(context.Context, Caller, any) (*store.PostHistory, func(), error)

func (f PostHistoryProviderFunc) BeginPostHistory(ctx context.Context, c Caller, expected any) (*store.PostHistory, func(), error) {
	if f == nil {
		return nil, nil, errors.New("POST_HISTORY_PROVIDER_REQUIRED")
	}
	return f(ctx, c, expected)
}

// This binds ledger/history operations only, not workflow execution, finalization
// publication, knowledge documents or model dispatch/accounting authority.
func (s *Server) BindPostHistory(provider PostHistoryProvider) error {
	if provider == nil {
		return errors.New("POST_HISTORY_PROVIDER_REQUIRED")
	}
	names := []string{"db:post-process-create-run", "db:post-process-get-latest-run", "db:post-process-get-steps", "db:post-process-mark-step-ok", "db:post-process-mark-step-failed", "db:post-process-is-all-passed", "db:log-llm-call", "db:get-llm-stats", "db:get-llm-history"}
	handlers := make(map[string]Operation, len(names))
	for _, name := range names {
		handlers[name] = func(ctx context.Context, c Caller, args []any) (any, error) {
			mutate := name == "db:post-process-create-run" || name == "db:post-process-mark-step-ok" || name == "db:post-process-mark-step-failed" || name == "db:log-llm-call"
			fail := func(err error) (any, error) {
				if name == "db:log-llm-call" {
					return map[string]any{"success": false}, nil
				}
				code := postHistoryCode(err)
				if mutate {
					return map[string]any{"success": false, "error": "Error: " + code}, nil
				}
				var public *security.HTTPError
				if errors.As(err, &public) {
					return nil, public
				}
				return nil, errors.New(code)
			}
			arity, pathIndex := 3, 1
			switch name {
			case "db:post-process-get-latest-run", "db:post-process-is-all-passed", "db:post-process-mark-step-ok":
				arity, pathIndex = 4, 2
			case "db:post-process-mark-step-failed":
				arity, pathIndex = 5, 3
			case "db:get-llm-stats":
				arity, pathIndex = 2, 0
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			expected, err := draftExpectedContext(args, len(args)-1)
			if err != nil {
				return fail(err)
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
			var input map[string]any
			limit := int64(50)
			if name == "db:post-process-create-run" || name == "db:log-llm-call" {
				input, ok = args[0].(map[string]any)
				if !ok || input == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			}
			if name == "db:get-llm-history" {
				if args[0] != nil {
					limit, ok = draftSafeInteger(args[0], -9007199254740991)
					if !ok {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
				}
			} else if input == nil {
				for i := 0; i < pathIndex; i++ {
					if _, ok := args[i].(string); !ok {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
				}
			}
			repository, release, err := provider.BeginPostHistory(ctx, c, expected)
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
			case "db:post-process-get-latest-run":
				result, err = repository.LatestRun(ctx, args[0].(string), args[1].(string))
			case "db:post-process-get-steps":
				result, err = repository.Steps(ctx, args[0].(string))
			case "db:post-process-mark-step-ok":
				err = repository.MarkStepOK(ctx, args[0].(string), args[1].(string))
			case "db:post-process-mark-step-failed":
				err = repository.MarkStepFailed(ctx, args[0].(string), args[1].(string), args[2].(string))
			case "db:post-process-is-all-passed":
				result, err = repository.AllPassed(ctx, args[0].(string), args[1].(string))
			case "db:log-llm-call":
				err = repository.LogCall(ctx, input)
			case "db:get-llm-stats":
				result, err = repository.Stats(ctx)
			case "db:get-llm-history":
				result, err = repository.History(ctx, limit)
			}
			if err != nil {
				return fail(err)
			}
			if !mutate {
				return reviewRevisionWire(result), nil
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
func postHistoryCode(err error) string {
	var public *security.HTTPError
	if errors.As(err, &public) {
		return public.Code
	}
	if err == nil {
		return "POST_HISTORY_OPERATION_FAILED"
	}
	switch err.Error() {
	case "PROJECT_NOT_OPEN", "POST_HISTORY_INPUT_INVALID", "POST_HISTORY_PROVIDER_UNAVAILABLE", "POST_PROCESS_STEP_NOT_FOUND", "POST_PROCESS_FINALIZATION_SOURCE_REQUIRED", "POST_PROCESS_FINALIZATION_SOURCE_CHANGED":
		return err.Error()
	}
	return "POST_HISTORY_OPERATION_FAILED"
}

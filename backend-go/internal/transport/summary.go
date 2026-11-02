package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// SummaryProvider borrows only an already admitted caller-owned project. The
// provider must check session, tab and all three frozen identity fields before
// exposing a repository, never open a renderer-supplied path. Release holds the
// admitted project stable through the complete call, including formal denials.
type SummaryProvider interface {
	BeginSummary(context.Context, Caller, any) (*store.Summary, func(), error)
}

type SummaryProviderFunc func(context.Context, Caller, any) (*store.Summary, func(), error)

func (f SummaryProviderFunc) BeginSummary(ctx context.Context, caller Caller, expected any) (*store.Summary, func(), error) {
	if f == nil {
		return nil, nil, errors.New("SUMMARY_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// BindSummary binds four original repository operations and preserves two
// original admission denials. Those denials are NOT implemented formal writes;
// no generation/finalization authority is granted. Production main stays unbound.
func (s *Server) BindSummary(provider SummaryProvider) error {
	if provider == nil {
		return errors.New("SUMMARY_PROVIDER_REQUIRED")
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:save-summary-snapshot", 4, 2},
		{"db:get-latest-summary", 2, 0},
		{"db:continuity-list-before", 3, 1},
		{"db:continuity-read-source", 3, 1},
		{"db:continuity-save-finalized", 3, 1},
		{"db:continuity-save-character-state-candidates", 3, 1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			denial := name == "db:continuity-save-finalized" || name == "db:continuity-save-character-state-candidates"
			fail := func(err error) (any, error) {
				code := summaryCode(err)
				if denial {
					return map[string]any{"success": false, "error": "Error: " + code}, nil
				}
				var public *security.HTTPError
				if errors.As(err, &public) && public != nil && code == public.Code {
					return nil, security.NewHTTPError(public.Status, code)
				}
				return nil, errors.New(code)
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			// Preserve evidence of extras before the common parser clones fields.
			raw, ok := args[arity-1].(map[string]any)
			if !ok || len(raw) != 3 {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return fail(err)
			}
			for _, value := range expected {
				text, ok := value.(string)
				if !ok || text == "" || !utf8.ValidString(text) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
				}
			}
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
			var number int64
			var states string
			switch name {
			case "db:save-summary-snapshot", "db:continuity-list-before", "db:continuity-read-source":
				minimum := int64(1)
				if name == "db:save-summary-snapshot" {
					// Legacy snapshots accept zero and negative safe integers.
					minimum = -9007199254740991
				}
				number, ok = draftSafeInteger(args[0], minimum)
				if !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				if name == "db:save-summary-snapshot" {
					states, ok = args[1].(string)
					if !ok || !utf8.ValidString(states) {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
					if len(states) > 32<<20 {
						return fail(errors.New("SUMMARY_INPUT_UNSUPPORTED"))
					}
				}
			}
			// Scalar inputs have been captured and the frozen context cloned before
			// entering provider code. The ignored denial request grants no authority.
			repository, release, err := provider.BeginSummary(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("SUMMARY_PROVIDER_UNAVAILABLE"))
			}
			if denial {
				return fail(errors.New("GENERATION_FINALIZATION_ADMISSION_REQUIRED"))
			}
			switch name {
			case "db:save-summary-snapshot":
				err = repository.SaveSnapshot(ctx, number, states)
				if err == nil {
					return map[string]any{"success": true}, nil
				}
			case "db:get-latest-summary":
				result, readErr := repository.LatestSnapshot(ctx)
				if readErr == nil {
					if result == nil {
						return nil, nil
					}
					return result, nil
				}
				err = readErr
			case "db:continuity-list-before":
				result, readErr := repository.ListBefore(ctx, number)
				if readErr == nil {
					if result == nil {
						result = []map[string]any{}
					}
					return result, nil
				}
				err = readErr
			case "db:continuity-read-source":
				result, readErr := repository.ReadFinalizedSource(ctx, number)
				if readErr == nil {
					return result, nil
				}
				err = readErr
			}
			return fail(err)
		}
	}
	// No partial publication: all six names must be inventoried and unbound.
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

// Only this finite public vocabulary may cross the transport boundary. Neither
// symbolic-looking unknown codes nor a provider's HTTPError.Message are public.
func summaryCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "REQUEST_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "REQUEST_DEADLINE_EXCEEDED"
	}
	code := ""
	var public *security.HTTPError
	if errors.As(err, &public) {
		if public == nil {
			return "SUMMARY_OPERATION_FAILED"
		}
		code = public.Code
	} else if err != nil {
		code = err.Error()
	}
	switch code {
	case "SUMMARY_INPUT_UNSUPPORTED", "SUMMARY_PROVIDER_UNAVAILABLE", "INVALID_ARGUMENTS",
		"CONTINUITY_TARGET_CHAPTER_INVALID", "CONTINUITY_SOURCE_DRAFT_INVALID",
		"GENERATION_FINALIZATION_ADMISSION_REQUIRED",
		"PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED",
		"PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_MANAGER_CLOSED",
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_SESSION_NOT_OWNED",
		"PROJECT_OPEN_IN_ANOTHER_TAB", "PROJECT_REQUEST_IN_FLIGHT", "PROJECT_NOT_OPEN",
		"PROJECT_ROOT_CHANGED", "PROJECT_SOURCE_CHANGED", "PROJECT_HANDLE_CLOSED",
		"PORTABLE_AUTHORITY_UNSUPPORTED", "PORTABLE_RUNTIME_FROZEN":
		return code
	}
	return "SUMMARY_OPERATION_FAILED"
}

package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// RecoveryProvider borrows an already admitted repository for this exact session,
// tab and frozen context. It must not open a renderer-supplied path. Its release
// keeps the admitted project stable for the entire repository call.
type RecoveryProvider interface {
	BeginRecovery(context.Context, Caller, any) (*store.Recovery, func(), error)
}

type RecoveryProviderFunc func(context.Context, Caller, any) (*store.Recovery, func(), error)

func (f RecoveryProviderFunc) BeginRecovery(ctx context.Context, caller Caller, expected any) (*store.Recovery, func(), error) {
	if f == nil {
		return nil, nil, errors.New("RECOVERY_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, expected)
}

// BindRecovery explicitly binds only the four inventoried candidate operations.
// It grants no generation, finalization, project-open or recovery-run authority.
// Production main deliberately does not opt in to this group yet.
func (s *Server) BindRecovery(provider RecoveryProvider) error {
	if provider == nil {
		return errors.New("RECOVERY_PROVIDER_REQUIRED")
	}
	operations := []struct {
		name string
		arity, pathIndex int
	}{
		{"db:recovery-candidate-record", 3, 1},
		{"db:recovery-candidate-list", 2, 0},
		{"db:recovery-candidate-update", 4, 2},
		{"db:recovery-candidate-resolve", 4, 2},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) {
				code := recoveryCode(err)
				if name != "db:recovery-candidate-list" {
					return map[string]any{"success": false, "error": "Error: " + code}, nil
				}
				var public *security.HTTPError
				if errors.As(err, &public) && code == public.Code {
					// Never forward a provider's arbitrary public message.
					return nil, security.NewHTTPError(public.Status, code)
				}
				return nil, errors.New(code)
			}
			if len(args) != arity {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			expected, err := draftExpectedContext(args, arity-1)
			if err != nil {
				return fail(err)
			}
			for _, value := range expected {
				if !recoveryString(value) {
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
			var input map[string]any
			var id, text string
			switch name {
			case "db:recovery-candidate-record":
				input, err = recoveryRecordInput(args[0])
				if err != nil {
					return fail(err)
				}
				// Renderer projectId is rejected, not trusted or looked up. Inject
				// only the identity captured by the existing frozen-context parser.
				input["projectId"] = expected["projectId"]
			case "db:recovery-candidate-update", "db:recovery-candidate-resolve":
				if !recoveryString(args[0]) || !recoveryString(args[1]) {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				id, text = args[0].(string), args[1].(string)
				if name == "db:recovery-candidate-resolve" && text != "continued" && text != "discarded" {
					return fail(errors.New("RECOVERY_CANDIDATE_ACTION_INVALID"))
				}
			}
			repository, release, err := provider.BeginRecovery(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("RECOVERY_CANDIDATE_PROVIDER_UNAVAILABLE"))
			}
			var candidate map[string]any
			switch name {
			case "db:recovery-candidate-record":
				candidate, err = repository.Record(ctx, input)
			case "db:recovery-candidate-list":
				candidates, listErr := repository.ListPending(ctx)
				if listErr != nil {
					return fail(listErr)
				}
				if candidates == nil {
					candidates = []map[string]any{}
				}
				return candidates, nil
			case "db:recovery-candidate-update":
				candidate, err = repository.UpdatePending(ctx, id, text)
			case "db:recovery-candidate-resolve":
				err = repository.Resolve(ctx, id, text)
			}
			if err != nil {
				return fail(err)
			}
			if name == "db:recovery-candidate-resolve" {
				return map[string]any{"success": true}, nil
			}
			return map[string]any{"success": true, "candidate": candidate}, nil
		}
	}
	// Validate the whole group while locked before publishing even one handler.
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

func recoveryString(value any) bool {
	text, ok := value.(string)
	return ok && utf8.ValidString(text)
}

func recoveryRecordInput(raw any) (map[string]any, error) {
	invalid := security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	request, ok := raw.(map[string]any)
	if !ok || request == nil {
		return nil, invalid
	}
	out := make(map[string]any, len(request)+1)
	for key, value := range request {
		switch key {
		case "runId", "stepId", "chapterTitle", "visibleText", "failureCode", "failureReason", "replacesCandidateId":
			if !recoveryString(value) {
				return nil, invalid
			}
		case "chapterNumber", "source", "sourceDraft":
		default:
			return nil, invalid
		}
		out[key] = value
	}
	for _, key := range []string{"runId", "stepId", "chapterTitle", "visibleText", "failureCode", "failureReason"} {
		if !recoveryString(out[key]) {
			return nil, invalid
		}
	}
	if _, ok := draftSafeInteger(out["chapterNumber"], 1); !ok {
		return nil, errors.New("RECOVERY_CANDIDATE_CHAPTER_INVALID")
	}
	source, ok := out["source"].(map[string]any)
	if !ok || source == nil {
		return nil, invalid
	}
	copySource := make(map[string]any, len(source))
	for key, value := range source {
		switch key {
		case "chapterNumber":
			if _, ok := draftSafeInteger(value, 1); !ok {
				return nil, invalid
			}
		case "title", "role", "purpose", "keyEvents", "suspenseHook", "userGuidance":
			if !recoveryString(value) {
				return nil, invalid
			}
		case "characters":
			items, ok := value.([]any)
			if !ok {
				return nil, invalid
			}
			cloned := make([]any, len(items))
			for i, item := range items {
				if !recoveryString(item) {
					return nil, invalid
				}
				cloned[i] = item
			}
			value = cloned
		default:
			return nil, invalid
		}
		copySource[key] = value
	}
	for _, key := range []string{"title", "role", "purpose", "keyEvents"} {
		if !recoveryString(copySource[key]) {
			return nil, invalid
		}
	}
	if _, ok := draftSafeInteger(copySource["chapterNumber"], 1); !ok {
		return nil, invalid
	}
	if _, ok := copySource["characters"]; !ok {
		return nil, invalid
	}
	out["source"] = copySource
	identity, exists := out["sourceDraft"]
	if !exists {
		return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
	}
	if identity != nil {
		draft, ok := identity.(map[string]any)
		if !ok || len(draft) != 2 {
			return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
		}
		if _, ok := draftSafeInteger(draft["id"], 1); !ok {
			return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
		}
		if _, ok := draftSafeInteger(draft["version"], 1); !ok {
			return nil, errors.New("RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID")
		}
		out["sourceDraft"] = map[string]any{"id": draft["id"], "version": draft["version"]}
	}
	return out, nil
}

// A finite public vocabulary, not a prefix/uppercase heuristic. Even HTTPError
// codes pass this allowlist; arbitrary provider diagnostics are never reflected.
func recoveryCode(err error) string {
	if errors.Is(err, context.Canceled) {
		return "REQUEST_CANCELED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "REQUEST_DEADLINE_EXCEEDED"
	}
	code := ""
	var public *security.HTTPError
	if errors.As(err, &public) {
		code = public.Code
	} else if err != nil {
		code = err.Error()
	}
	switch code {
	case "RECOVERY_CANDIDATE_IDENTITY_INVALID", "RECOVERY_CANDIDATE_INTEGRITY_FAILED",
		"RECOVERY_CANDIDATE_CHAPTER_INVALID", "RECOVERY_CANDIDATE_SOURCE_MISMATCH",
		"RECOVERY_CANDIDATE_SOURCE_DRAFT_INVALID", "RECOVERY_CANDIDATE_TEXT_EMPTY",
		"RECOVERY_CANDIDATE_REPLACEMENT_INVALID", "RECOVERY_CANDIDATE_NOT_PENDING",
		"RECOVERY_CANDIDATE_SOURCE_CHANGED", "RECOVERY_CANDIDATE_ACTION_INVALID",
		"RECOVERY_CANDIDATE_PROVIDER_UNAVAILABLE", "RECOVERY_CANDIDATE_INPUT_UNSUPPORTED", "INVALID_ARGUMENTS",
		"PROJECT_CONTEXT_REQUIRED", "PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_REQUIRED",
		"PROJECT_SESSION_CONTEXT_REQUIRED", "PROJECT_SESSION_MANAGER_CLOSED",
		"PROJECT_SESSION_STALE", "PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_SESSION_NOT_OWNED",
		"PROJECT_OPEN_IN_ANOTHER_TAB", "PROJECT_REQUEST_IN_FLIGHT", "PROJECT_NOT_OPEN",
		"PROJECT_ROOT_CHANGED", "PROJECT_SOURCE_CHANGED", "PROJECT_HANDLE_CLOSED",
		"PORTABLE_AUTHORITY_UNSUPPORTED", "PORTABLE_RUNTIME_FROZEN":
		return code
	}
	return "RECOVERY_CANDIDATE_OPERATION_FAILED"
}

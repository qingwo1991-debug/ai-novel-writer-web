package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindNarrativeWrites binds only the five mutating narrative thread plan and
// consistency exemption names: plan-create, plan-update, plan-delete,
// exemption-save and exemption-revoke. db:narrative-thread-event-confirm is
// deliberately NOT here either: thread confirmation is a separate write with
// its own evidence rule, so it is bound by BindNarrativeEventConfirm instead of
// being mixed into this list. Together with BindNarrativeReads and that binder
// the family is then served completely, and no name is bound twice.
//
// The provider is the same seam BindNarrative already uses: it lends one
// admitted repository per request and never a database handle, so the write
// permit is composed and released exactly like the durable draft write's.
// Arity, path index and argument validation mirror BindNarrative exactly. The
// arities in this family are NOT uniform and the path sits at a different index
// per name, so each row states both explicitly.
//
// A refusal is always reported as a business result, never as success, so a
// write that did not commit can never be read as committed; and a commit is
// always reported as success, so a write that did commit can never be read as a
// refusal.
func (s *Server) BindNarrativeWrites(provider NarrativeProvider) error {
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
	// Exactly BindNarrative's rows for these five names: plan-create is
	// (input, path, context), plan-update is (id, input, path, context),
	// plan-delete is (id, path, context), exemption-save is
	// (key, reason, path, context) and exemption-revoke is (key, path,
	// context). The frozen three-key context is always the last argument.
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:narrative-thread-plan-create", 3, 1},
		{"db:narrative-thread-plan-update", 4, 2},
		{"db:narrative-thread-plan-delete", 3, 1},
		{"db:consistency-exemption-save", 4, 2},
		{"db:consistency-exemption-revoke", 3, 1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) { return narrativeWriteFailure(err), nil }
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
			// The leading arguments are not uniform either. plan-create carries
			// the plan object at index 0; plan-update carries a safe-integer id
			// at 0 and the plan object at 1; plan-delete carries a
			// safe-integer id at 0; the two exemption names carry identity
			// strings up to the path index. Every one is checked before the
			// borrow, so a malformed request never opens a repository.
			var input map[string]any
			var id int64
			switch name {
			case "db:narrative-thread-plan-create":
				if input, ok = args[0].(map[string]any); !ok || input == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			case "db:narrative-thread-plan-update":
				if id, ok = draftSafeInteger(args[0], -9007199254740991); !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				if input, ok = args[1].(map[string]any); !ok || input == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			case "db:narrative-thread-plan-delete":
				if id, ok = draftSafeInteger(args[0], -9007199254740991); !ok {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			default:
				for index := 0; index < pathIndex; index++ {
					if _, ok := args[index].(string); !ok {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
				}
			}
			if ctx == nil {
				return fail(errors.New("NARRATIVE_OPERATION_FAILED"))
			}
			repository, release, err := provider.BeginNarrative(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("NARRATIVE_PROVIDER_UNAVAILABLE"))
			}
			var result any
			switch name {
			case "db:narrative-thread-plan-create":
				result, err = repository.CreatePlan(ctx, input)
			case "db:narrative-thread-plan-update":
				result, err = repository.UpdatePlan(ctx, id, input)
			case "db:narrative-thread-plan-delete":
				err = repository.DeletePlan(ctx, id)
			case "db:consistency-exemption-save":
				err = repository.SaveExemption(ctx, args[0].(string), args[1].(string))
			case "db:consistency-exemption-revoke":
				err = repository.RevokeExemption(ctx, args[0].(string))
			}
			if err != nil {
				return fail(err)
			}
			// The committed shapes are exactly BindNarrative's: the two plan
			// writes return the persisted plan row, and the delete and both
			// exemption writes return a bare success receipt.
			if name == "db:narrative-thread-plan-create" || name == "db:narrative-thread-plan-update" {
				return map[string]any{"success": true, "plan": reviewRevisionWire(result)}, nil
			}
			return map[string]any{"success": true}, nil
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

// narrativeWriteFailure keeps the mutation refusal envelope identical to
// BindPostHistoryWrites: a business result carrying success=false and a
// symbolic code, never a bare transport error. Only narrativeCode's finite
// vocabulary is ever reflected, so a wrapped filesystem path or raw SQLite
// text never reaches the browser.
func narrativeWriteFailure(err error) map[string]any {
	return map[string]any{"success": false, "error": "Error: " + narrativeCode(err)}
}

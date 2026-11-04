package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindProjectCoreWrites binds the two mutating project core names that the
// durable lane did not serve yet: db:project-core-update and
// db:project-core-synopsis-commit. db:project-core-get belongs to the plain
// read binders (BindProjectCore and the editor stage read binder), so it is
// deliberately NOT among them and a lane that also binds a read cannot collide.
//
// The provider is the same seam BindProjectCore already uses: it lends one
// admitted project repository per request and never a database handle, so the
// write permit is composed and released exactly like the durable draft write's.
// Arity, path index and argument validation mirror BindProjectCore exactly
// (backend-go/internal/transport/projectcore.go:22-33). Both names are
// (input object, path, context) with the path at index 1, the input map at 0
// and the frozen three-key session context last:
//
//	db:project-core-update          input object at 0, path 1, context 2
//	db:project-core-synopsis-commit input object at 0, path 1, context 2
//
// db:character-roster-commit is NOT bound here. Its original binder only ever
// denies it (backend-go/internal/transport/characters.go:61,103-126), and no
// store method expresses a roster commit at all — every method on
// store.Characters is a read — so binding it would mean inventing SQL in a
// transport file. It therefore keeps failing closed.
//
// A refusal is always reported as a business result, never as success, so a
// write that did not commit can never be read as committed; and a commit is
// always reported as success, so a write that did commit can never be read as a
// refusal. Two shapes intentionally follow the durable write lane instead of
// BindProjectCore: a synopsis-commit refusal keeps the lane's "Error: " prefix
// (the original returned the bare PROJECT_CORE_SYNOPSIS_CHANGED code) and a
// synopsis-commit failure that BindProjectCore reported as a bare transport
// error is reported here as a success=false business result. Neither change can
// turn a refusal into a success or a success into a refusal.
func (s *Server) BindProjectCoreWrites(provider ProjectCoreProvider) error {
	if provider == nil {
		return errors.New("PROJECT_CORE_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("PROJECT_CORE_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:project-core-update", 3, 1},
		{"db:project-core-synopsis-commit", 3, 1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) { return projectCoreWriteFailure(err), nil }
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
			// The leading argument is checked before the borrow, so a malformed
			// request never opens a repository. Both names carry the input
			// object at index 0, and synopsis-commit additionally keeps the
			// original's full request validation: no generation or recovery
			// owner key (even null) and every one of the eleven CAS baselines
			// present with the right JSON type.
			data, ok := args[0].(map[string]any)
			if !ok || data == nil {
				return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
			}
			if name == "db:project-core-synopsis-commit" {
				if err := validateSynopsisRequest(data); err != nil {
					return fail(err)
				}
			}
			if ctx == nil {
				return fail(errors.New("PROJECT_CORE_OPERATION_FAILED"))
			}
			repository, release, err := provider.BeginProjectCore(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("PROJECT_CORE_PROVIDER_UNAVAILABLE"))
			}
			switch name {
			case "db:project-core-update":
				// Update reports only an error: the store does not compare
				// bookkeeping and cannot say whether any row changed, exactly
				// like the original repository's update. Its all-or-nothing
				// guard rejects a charactersArch key before any SQL runs.
				if err := repository.Update(ctx, data); err != nil {
					return fail(err)
				}
			case "db:project-core-synopsis-commit":
				// CommitSynopsis is one atomic compare-and-set over the target
				// body and all eleven source fields; changed=false is the lost
				// CAS and is reported as a refusal, never as success.
				changed, err := repository.CommitSynopsis(ctx, data)
				if err != nil {
					return fail(err)
				}
				if !changed {
					return projectCoreWriteFailure(errors.New("PROJECT_CORE_SYNOPSIS_CHANGED")), nil
				}
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

// projectCoreWriteFailure keeps the mutation refusal envelope identical to the
// other durable write binders: a business result carrying success=false and a
// symbolic code, never a bare transport error, and never a success for a write
// that did not commit. projectCoreErrorCode is the original binder's own finite
// mapping, so a store error keeps exactly the code BindProjectCore would have
// returned for the same write and a wrapped filesystem path or raw SQLite text
// never reaches the browser.
func projectCoreWriteFailure(err error) map[string]any {
	return map[string]any{"success": false, "error": "Error: " + projectCoreErrorCode(err)}
}

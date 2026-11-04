package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindBlueprintWrites binds only the mutating blueprint names that a single
// existing store method already expresses: upsert, upsert-many, delete and
// commit-range. Together with BindBlueprintReads the family is then served
// except db:blueprint-get-all (the editor stage read binder owns it) and
// db:blueprint-update-notes, which stays unbound on purpose: the original
// refuses it unconditionally (electron/controllers/db-controller.ts:636) and
// the Go transport mirrors that refusal, so it must keep failing closed.
//
// The provider is the same seam BindBlueprints already uses: it lends one
// admitted repository per request and never a database handle, so the write
// permit is composed and released exactly like the durable draft write's.
// Arity, path index and argument validation mirror BindBlueprints exactly
// (backend-go/internal/transport/blueprints.go:22-24). All four names here are
// (leading argument, path, context) with the path at index 1 and the frozen
// three-key session context last, but the leading argument differs:
//
//	db:blueprint-upsert        input object at 0, path 1, context 2
//	db:blueprint-upsert-many   input array of objects at 0, path 1, context 2
//	db:blueprint-delete        chapter key at 0 (unvalidated), path 1, context 2
//	db:blueprint-commit-range  input object at 0, path 1, context 2
//
// A refusal is always reported as a business result, never as success, so a
// write that did not commit can never be read as committed; and a commit is
// always reported as success, so a write that did commit can never be read as a
// refusal.
func (s *Server) BindBlueprintWrites(provider BlueprintProvider) error {
	if provider == nil {
		return errors.New("BLUEPRINT_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("BLUEPRINT_PROVIDER_REQUIRED")
		}
	}
	operations := []struct {
		name             string
		arity, pathIndex int
	}{
		{"db:blueprint-upsert", 3, 1},
		{"db:blueprint-upsert-many", 3, 1},
		{"db:blueprint-delete", 3, 1},
		{"db:blueprint-commit-range", 3, 1},
	}
	handlers := make(map[string]Operation, len(operations))
	for _, operation := range operations {
		name, arity, pathIndex := operation.name, operation.arity, operation.pathIndex
		handlers[name] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			fail := func(err error) (any, error) { return blueprintWriteFailure(err), nil }
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
			// request never opens a repository: upsert and commit-range carry the
			// input object at index 0, upsert-many carries an array whose every
			// entry must be a present object, and delete hands its chapter key to
			// the store unvalidated exactly like BindBlueprints, so a key the
			// store cannot bind becomes the symbolic BLUEPRINT_OPERATION_FAILED
			// instead of a reflection error.
			var input map[string]any
			var batch []map[string]any
			switch name {
			case "db:blueprint-upsert", "db:blueprint-commit-range":
				if input, ok = args[0].(map[string]any); !ok || input == nil {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
			case "db:blueprint-upsert-many":
				items, itemsOK := args[0].([]any)
				if !itemsOK {
					return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
				}
				batch = make([]map[string]any, len(items))
				for index, item := range items {
					entry, entryOK := item.(map[string]any)
					if !entryOK || entry == nil {
						return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
					}
					batch[index] = entry
				}
			}
			if ctx == nil {
				return fail(errors.New("BLUEPRINT_OPERATION_FAILED"))
			}
			repository, release, err := provider.BeginBlueprints(ctx, caller, expected)
			if release != nil {
				defer release()
			}
			if err != nil {
				return fail(err)
			}
			if repository == nil || release == nil {
				return fail(errors.New("BLUEPRINT_PROVIDER_UNAVAILABLE"))
			}
			var result any
			switch name {
			case "db:blueprint-upsert":
				err = repository.Upsert(ctx, input)
			case "db:blueprint-upsert-many":
				err = repository.UpsertMany(ctx, batch)
			case "db:blueprint-delete":
				err = repository.Delete(ctx, args[0])
			case "db:blueprint-commit-range":
				// CommitRange is one store call that owns the whole range commit
				// (blueprint rows, commit operation row, character-sync operation
				// row and the read-back proof) inside its own immediate
				// transaction, so no orchestration is composed here.
				result, err = repository.CommitRange(ctx, input)
			}
			if err != nil {
				return fail(err)
			}
			// The committed shapes are exactly BindBlueprints': commit-range
			// returns its receipt and the three row writes a bare success receipt.
			if name == "db:blueprint-commit-range" {
				return map[string]any{"success": true, "receipt": reviewRevisionWire(result)}, nil
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

// blueprintWriteFailure keeps the mutation refusal envelope identical to
// BindBlueprints: a business result carrying success=false and a symbolic code,
// never a bare transport error, and never a success for a write that did not
// commit. blueprintCode is the original binder's own finite mapping, so a store
// error keeps exactly the code BindBlueprints would have returned for the same
// write and a wrapped filesystem path or raw SQLite text never reaches the
// browser.
func blueprintWriteFailure(err error) map[string]any {
	return map[string]any{"success": false, "error": "Error: " + blueprintCode(err)}
}

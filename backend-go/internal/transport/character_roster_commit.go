package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// CharacterRosterCommitter is the author lane's db:character-roster-commit
// capability: one admitted project database, one frozen session, one roster
// commit. It returns the original CharacterRosterCommitReceipt and never a
// database handle. A provider that cannot commit simply does not implement it, so
// BindCharacters keeps its refusal for a read-only borrow.
type CharacterRosterCommitter interface {
	CommitCharacterRoster(context.Context, Caller, map[string]any, any) (map[string]any, error)
}

// BindCharacterRosterCommit binds ONLY db:character-roster-commit, the one name
// the durable lane could not serve before: the roster commit's author pipeline
// (store.RosterAuthor) is the same SQL the original ran, so this binder is the
// author lane's own write and not an invention of the transport.
//
// The shape is fixed by src/shared/ipc-channels.ts:1014 and the durable lane's
// existing binders: (input object, project path, frozen three-key session
// context), so arity 3 with the path at index 1 and the context last. The
// original controller (electron/controllers/db-controller.ts:689-711) answers
// every outcome as a business result — {success:true,receipt} for a commit and
// {success:false,error:"Error: CODE"} for every refusal — so a refusal can never
// be read as a commit and a commit can never be read as a refusal.
//
// This binder deliberately does not also bind the three character reads
// (BindCharacterLiveReads owns them) and does not rebind anything else, so a lane
// that binds both never hits OPERATION_ALREADY_BOUND.
func (s *Server) BindCharacterRosterCommit(provider CharacterRosterCommitter) error {
	if provider == nil {
		return errors.New("CHARACTER_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("CHARACTER_PROVIDER_REQUIRED")
		}
	}
	handlers := map[string]Operation{
		"db:character-roster-commit": characterRosterCommitOperation(provider, 3, 1),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.registry.Lookup("db:character-roster-commit"); !ok {
		return errors.New("OPERATION_NOT_EXPOSED")
	}
	if _, ok := s.handlers["db:character-roster-commit"]; ok {
		return errors.New("OPERATION_ALREADY_BOUND")
	}
	for name, handler := range handlers {
		s.handlers[name] = handler
	}
	return nil
}

// characterRosterCommitOperation is the one db:character-roster-commit handler
// shared by BindCharacterRosterCommit and BindCharacters. The provider is asserted
// for the commit capability: when it has none, the request is still admitted
// through the read borrow and released again, and only then refused with the
// lane's own code, so a denial never turns into an admission and an unadmitted
// caller never reaches a write.
func characterRosterCommitOperation(provider any, arity, pathIndex int) Operation {
	committer, _ := provider.(CharacterRosterCommitter)
	return func(ctx context.Context, caller Caller, args []any) (any, error) {
		fail := func(err error) (any, error) {
			return charactersSerialized(map[string]any{"success": false, "error": "Error: " + charactersCode(err)})
		}
		if len(args) != arity {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		raw, ok := args[arity-1].(map[string]any)
		if !ok || len(raw) != 3 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
		}
		expected := make(map[string]any, 3)
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := raw[key].(string)
			if !ok || text == "" || !utf8.ValidString(text) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_SESSION_REQUIRED"))
			}
			expected[key] = text
		}
		if pathIndex >= 0 {
			path, ok := args[pathIndex].(string)
			if !ok || path == "" || !utf8.ValidString(path) {
				return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
			}
			if path != expected["projectPath"] {
				return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
			}
		}
		request, _ := args[0].(map[string]any)
		if committer == nil {
			// The original controller sends a manual_edit author request to the
			// author service and every other intent to the identity lane. A
			// provider without either capability admits the caller first and then
			// refuses with the code that lane would have used.
			if begin, ok := provider.(CharactersProvider); ok {
				repository, release, err := begin.BeginCharacters(ctx, caller, expected)
				if release != nil {
					defer release()
				}
				if err != nil {
					return fail(err)
				}
				if repository == nil || release == nil {
					return fail(errors.New("CHARACTER_PROVIDER_UNAVAILABLE"))
				}
			}
			if request["intent"] == "manual_edit" {
				return fail(errors.New("CHARACTER_AUTHOR_SCOPE_REQUIRED"))
			}
			return fail(errors.New("CHARACTER_ID_WRITE_REQUIRED"))
		}
		if request == nil {
			// The author service refuses a request that is not an object before it
			// reads any field, so a malformed payload never opens a transaction.
			return fail(errors.New("CHARACTER_AUTHOR_REQUEST_INVALID"))
		}
		receipt, err := committer.CommitCharacterRoster(ctx, caller, request, expected)
		if err != nil {
			return fail(err)
		}
		if receipt == nil {
			return fail(errors.New("CHARACTER_PROVIDER_UNAVAILABLE"))
		}
		return charactersSerialized(map[string]any{"success": true, "receipt": receipt})
	}
}

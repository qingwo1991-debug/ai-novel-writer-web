package transport

import (
	"context"
	"errors"
	"net/http"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// BindNarrativeEventConfirm binds exactly db:narrative-thread-event-confirm, the
// one name BindNarrativeWrites deliberately leaves out. Confirmation is an
// ordinary author write on the admitted author database: store.ConfirmEvent
// re-reads the frozen finalization outbox itself and binds the evidence to that
// outbox prose, so no generation graph admission is composed here (unlike
// db:plot-tree-save, which BindPlotTree refuses unconditionally).
//
// Arity, path index, argument validation and the committed envelope mirror
// BindNarrative's row for this name exactly (narrative.go:25 states
// `arity,pathIndex:=3,1` for it, narrative.go:26 reads the plan object at index
// 0, and narrative.go:38 calls ConfirmEvent): the request is
// (plan object, project path, frozen three-key context), and the result is
// {"success":true,"event":...}.
//
// The refusal envelope is NOT the family's `Error: CODE` business result:
// BindNarrative only wraps the two consistency-exemption names into a caught
// result (narrative.go:20), so a refused confirmation stays a transport error
// carrying the symbolic code, which the boundary renders as HTTP 500 with
// {"error":{"code":CODE}} and no `Error: ` prefix. That shape is preserved.
func (s *Server) BindNarrativeEventConfirm(provider NarrativeProvider) error {
	if provider == nil {
		return errors.New("NARRATIVE_PROVIDER_REQUIRED")
	}
	return s.Bind("db:narrative-thread-event-confirm", func(ctx context.Context, caller Caller, args []any) (any, error) {
		if len(args) != 3 {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		expected, err := draftExpectedContext(args, 2)
		if err != nil {
			return nil, err
		}
		path, ok := args[1].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return nil, security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED")
		}
		if path != expected["projectPath"] {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
		}
		// The leading argument is the confirmation object at index 0. It is
		// validated before the borrow, exactly like BindNarrative, so a
		// malformed request never opens a repository.
		input, inputOK := args[0].(map[string]any)
		if !inputOK || input == nil {
			return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
		}
		if ctx == nil {
			return nil, errors.New("NARRATIVE_OPERATION_FAILED")
		}
		repository, release, err := provider.BeginNarrative(ctx, caller, expected)
		if release != nil {
			defer release()
		}
		if err != nil {
			return nil, narrativeConfirmFailure(err)
		}
		if repository == nil || release == nil {
			return nil, errors.New("NARRATIVE_PROVIDER_UNAVAILABLE")
		}
		result, err := repository.ConfirmEvent(ctx, input)
		if err != nil {
			return nil, narrativeConfirmFailure(err)
		}
		return map[string]any{"success": true, "event": reviewRevisionWire(result)}, nil
	})
}

// narrativeConfirmFailure mirrors BindNarrative's `fail` for this name: a public
// HTTP error passes through as itself (a caller-visible 400/409), and every
// other failure becomes a bare symbolic error through narrativeCode's finite
// vocabulary, never the wrapped store text, which may carry SQL or a path.
func narrativeConfirmFailure(err error) error {
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		return public
	}
	return errors.New(narrativeCode(err))
}

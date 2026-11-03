package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

// DraftLiveWriteRequest contains the caller's frozen lease and an exact body
// baseline. The provider must authenticate Caller and fence the complete
// source switch; the path and lease are claims, never authority by themselves.
type DraftLiveWriteRequest struct {
	Expected        map[string]any
	DraftID         int64
	Content         string
	WordCount       int64
	ExpectedContent string
}

// DraftLiveWriteAck is issued only after a verified commit and catalog
// publication. ProjectSession must be the new lease for the same project.
type DraftLiveWriteAck struct {
	ProjectSession map[string]any
}

type DraftLiveWriteProvider interface {
	CommitDraftContent(context.Context, Caller, DraftLiveWriteRequest) (DraftLiveWriteAck, error)
}

type DraftLiveWriteProviderFunc func(context.Context, Caller, DraftLiveWriteRequest) (DraftLiveWriteAck, error)

func (f DraftLiveWriteProviderFunc) CommitDraftContent(ctx context.Context, caller Caller, request DraftLiveWriteRequest) (DraftLiveWriteAck, error) {
	if f == nil {
		return DraftLiveWriteAck{}, errors.New("DRAFT_WRITE_PROVIDER_REQUIRED")
	}
	return f(ctx, caller, request)
}

// BindDraftLiveWrites claims only the author body CAS operation. The live read
// handlers remain separately bound and this function is not a project opener.
func (s *Server) BindDraftLiveWrites(provider DraftLiveWriteProvider) error {
	if provider == nil {
		return errors.New("DRAFT_WRITE_PROVIDER_REQUIRED")
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		if value.IsNil() {
			return errors.New("DRAFT_WRITE_PROVIDER_REQUIRED")
		}
	}
	return s.Bind("db:draft-update-content", func(ctx context.Context, caller Caller, args []any) (any, error) {
		fail := func(err error) (any, error) { return draftLiveWriteFailure(err), nil }
		if len(args) != 6 {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		expected, err := draftExpectedContext(args, 5)
		if err != nil {
			return fail(err)
		}
		path, ok := args[3].(string)
		if !ok || path == "" || !utf8.ValidString(path) {
			return fail(security.NewHTTPError(http.StatusBadRequest, "PROJECT_CONTEXT_REQUIRED"))
		}
		if path != expected["projectPath"] {
			return fail(security.NewHTTPError(http.StatusConflict, "PROJECT_CONTEXT_CHANGED"))
		}
		id, ok := draftSafeInteger(args[0], 1)
		if !ok {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		content, ok := args[1].(string)
		if !ok || !utf8.ValidString(content) {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		wordCount, ok := draftSafeInteger(args[2], 0)
		if !ok {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		baseline, ok := args[4].(string)
		if !ok || !utf8.ValidString(baseline) {
			return fail(security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS"))
		}
		if ctx == nil {
			return fail(errors.New("DRAFT_WRITE_FAILED"))
		}
		ack, err := provider.CommitDraftContent(ctx, caller, DraftLiveWriteRequest{
			Expected: expected, DraftID: id, Content: content,
			WordCount: wordCount, ExpectedContent: baseline,
		})
		if err != nil {
			return fail(err)
		}
		fresh, err := draftExpectedContext([]any{ack.ProjectSession}, 0)
		if err != nil || fresh["projectId"] != expected["projectId"] || fresh["projectPath"] != path || fresh["leaseId"] == expected["leaseId"] {
			return fail(errors.New("DRAFT_WRITE_ACK_INVALID"))
		}
		return map[string]any{"success": true, "projectSession": fresh}, nil
	})
}

func draftLiveWriteFailure(err error) map[string]any {
	if errors.Is(err, store.ErrDraftContentConflict) {
		return draftSaveFailure(err)
	}
	var public *security.HTTPError
	if errors.As(err, &public) && public != nil {
		switch public.Code {
		case "INVALID_ARGUMENTS", "PROJECT_SESSION_REQUIRED", "PROJECT_CONTEXT_REQUIRED",
			"PROJECT_CONTEXT_CHANGED", "PROJECT_SESSION_STALE", "PROJECT_SESSION_NOT_OWNED",
			"PROJECT_SESSION_ROOT_MISMATCH", "PROJECT_NOT_OPEN":
			return draftSaveFailure(public)
		}
	}
	return draftSaveFailure(errors.New("REQUEST_FAILED"))
}

package transport

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestDraftLiveWriteBindsOnlyExactCASAndRotatesLease(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	fresh := draftContext("project-a", "lease-b", path)
	called := 0
	provider := DraftLiveWriteProviderFunc(func(_ context.Context, caller Caller, request DraftLiveWriteRequest) (DraftLiveWriteAck, error) {
		called++
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" ||
			!reflect.DeepEqual(request.Expected, expected) || request.DraftID != 7 ||
			request.Content != "新正文" || request.WordCount != 3 || request.ExpectedContent != "原正文" {
			t.Fatalf("unexpected CAS request: caller=%+v request=%+v", caller, request)
		}
		return DraftLiveWriteAck{ProjectSession: fresh}, nil
	})
	if err := server.BindDraftLiveWrites(provider); err != nil {
		t.Fatal(err)
	}
	got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 7, "新正文", 3, path, "原正文", expected))
	result, ok := got.(map[string]any)
	if !ok || result["success"] != true || !reflect.DeepEqual(result["projectSession"], fresh) || called != 1 {
		t.Fatalf("write ACK = %#v, provider called %d times", got, called)
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list-all", path, expected), 501, "GO_OPERATION_NOT_MIGRATED")
	if err := server.BindDraftLiveWrites(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate write binder accepted: %v", err)
	}
}

func TestDraftLiveWriteRejectsMalformedRequestBeforeProvider(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	called := 0
	if err := server.BindDraftLiveWrites(DraftLiveWriteProviderFunc(func(context.Context, Caller, DraftLiveWriteRequest) (DraftLiveWriteAck, error) {
		called++
		return DraftLiveWriteAck{}, nil
	})); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		args []any
		code string
	}{
		{"missing baseline", []any{7, "新正文", 3, path, expected}, "INVALID_ARGUMENTS"},
		{"null baseline", []any{7, "新正文", 3, path, nil, expected}, "INVALID_ARGUMENTS"},
		{"missing lease", []any{7, "新正文", 3, path, "原正文"}, "INVALID_ARGUMENTS"},
		{"path mismatch", []any{7, "新正文", 3, "/other", "原正文", expected}, "PROJECT_CONTEXT_CHANGED"},
		{"fractional id", []any{7.5, "新正文", 3, path, "原正文", expected}, "INVALID_ARGUMENTS"},
		{"fractional word count", []any{7, "新正文", 3.5, path, "原正文", expected}, "INVALID_ARGUMENTS"},
		{"nontext body", []any{7, nil, 3, path, "原正文", expected}, "INVALID_ARGUMENTS"},
		{"nontext baseline", []any{7, "新正文", 3, path, 10, expected}, "INVALID_ARGUMENTS"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requireDraftFailure(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", test.args...), test.code)
		})
	}
	if called != 0 {
		t.Fatalf("malformed request reached provider %d times", called)
	}
}

func TestDraftLiveWriteRejectsInvalidAckAndRedactsProviderError(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	returnAck := DraftLiveWriteAck{ProjectSession: expected}
	providerError := error(nil)
	if err := server.BindDraftLiveWrites(DraftLiveWriteProviderFunc(func(context.Context, Caller, DraftLiveWriteRequest) (DraftLiveWriteAck, error) {
		return returnAck, providerError
	})); err != nil {
		t.Fatal(err)
	}
	call := func() string {
		response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 7, "新正文", 3, path, "原正文", expected)
		return response.Body.String()
	}
	if body := call(); !strings.Contains(body, "REQUEST_FAILED") || strings.Contains(body, "success\":true") {
		t.Fatalf("stale ACK accepted: %s", body)
	}
	returnAck.ProjectSession = draftContext("other", "lease-b", path)
	if body := call(); !strings.Contains(body, "REQUEST_FAILED") {
		t.Fatalf("foreign ACK accepted: %s", body)
	}
	providerError = errors.New("SECRET_TOKEN")
	if body := call(); !strings.Contains(body, "REQUEST_FAILED") || strings.Contains(body, "SECRET_TOKEN") {
		t.Fatalf("provider error leaked: %s", body)
	}
	providerError = store.ErrDraftContentConflict
	if body := call(); !strings.Contains(body, "DRAFT_CONTENT_CONFLICT") {
		t.Fatalf("CAS conflict lost: %s", body)
	}
}

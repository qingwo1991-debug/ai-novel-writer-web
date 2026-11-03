package transport

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func TestProjectReadCloseBindsOnlyExactContextAndObservedProviderResult(t *testing.T) {
	server := fixture(t)
	var calls int
	var gotCaller Caller
	var gotPath string
	var gotExpected any
	provider := ProjectReadCloseProviderFunc(func(_ context.Context, caller Caller, path string, expected any) (any, error) {
		calls++
		gotCaller, gotPath, gotExpected = caller, path, expected
		return map[string]any{"success": true}, nil
	})
	if err := server.BindProjectReadClose(provider); err != nil {
		t.Fatal(err)
	}
	if server.handlers["project:save"] != nil || server.handlers["project:update-config"] != nil {
		t.Fatal("read-only close bound author writes")
	}
	cookie, csrf := bootstrap(t, server)
	const tab = "synthetic-close-tab-0001"
	const path = "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	bad := draftRPC(t, server, cookie, csrf, tab, "db:close", "/synthetic/project-b", expected)
	requireDraftHTTPError(t, bad, http.StatusConflict, "PROJECT_CONTEXT_CHANGED")
	bad = draftRPC(t, server, cookie, csrf, tab, "db:close", path, map[string]any{"projectId": "project-a"})
	requireDraftHTTPError(t, bad, http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	bad = draftRPC(t, server, cookie, csrf, tab, "db:close", expected)
	requireDraftHTTPError(t, bad, http.StatusBadRequest, "INVALID_ARGUMENTS")
	if calls != 0 {
		t.Fatal("invalid close reached provider")
	}
	closed := draftRPC(t, server, cookie, csrf, tab, "db:close", path, expected)
	if closed.Code != http.StatusOK || !reflect.DeepEqual(draftResult(t, closed), map[string]any{"success": true}) {
		t.Fatal(closed.Code, closed.Body.String())
	}
	if calls != 1 || gotCaller.ClientID != tab || gotCaller.SessionID == "" || gotPath != path || !reflect.DeepEqual(gotExpected, expected) {
		t.Fatal("close owner or frozen context changed", calls, gotCaller, gotPath, gotExpected)
	}
	if err := server.BindProjectReadClose(provider); err == nil {
		t.Fatal("duplicate close binder replaced provider")
	}
}

func TestProjectReadCloseNeverInventsSuccessOnUnknownOutcome(t *testing.T) {
	server := fixture(t)
	if err := server.BindProjectReadClose(ProjectReadCloseProviderFunc(func(context.Context, Caller, string, any) (any, error) {
		return nil, errors.New("PROJECT_CLOSE_UNCONFIRMED")
	})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	response := draftRPC(t, server, cookie, csrf, "synthetic-close-tab-0001", "db:close", "/synthetic/project-a", draftContext("project-a", "lease-a", "/synthetic/project-a"))
	requireDraftHTTPError(t, response, http.StatusInternalServerError, "PROJECT_CLOSE_UNCONFIRMED")
	if got := draftRPC(t, server, cookie, csrf, "synthetic-close-tab-0001", "project:save"); got.Code != http.StatusNotImplemented {
		t.Fatal("write operation bound", got.Code, got.Body.String())
	}
}

package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

func TestCurrentOverviewRequiresExactProjectSession(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-a", "lease-a", "/synthetic/project-a")
	provider := CurrentOverviewProviderFunc(func(_ context.Context, caller Caller, raw any) (json.RawMessage, error) {
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" || !reflect.DeepEqual(raw, expected) {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
		}
		return json.RawMessage(`{"state":"ready","projectId":"project-a","revision":"opaque","name":"Book","totalWords":5,"characters":0,"finalizedChapters":0,"stages":[],"excerpt":"tail"}`), nil
	})
	if err := server.BindCurrentOverview(provider); err != nil {
		t.Fatal(err)
	}
	ready := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "project:overview-current", expected))
	if row, ok := ready.(map[string]any); !ok || row["state"] != "ready" || row["excerpt"] != "tail" {
		t.Fatalf("overview = %#v", ready)
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "project:overview-current", expected), http.StatusConflict, "PROJECT_SESSION_STALE")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "project:overview-current", map[string]any{"projectId": "", "leaseId": "lease-a", "projectPath": "/synthetic/project-a"}), http.StatusBadRequest, "PROJECT_SESSION_REQUIRED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "project:save", map[string]any{}, expected), http.StatusNotImplemented, "GO_OPERATION_NOT_MIGRATED")
	if err := server.BindCurrentOverview(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate provider accepted: %v", err)
	}
}

func TestCurrentOverviewRedactsProviderFailure(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	if err := server.BindCurrentOverview(CurrentOverviewProviderFunc(func(context.Context, Caller, any) (json.RawMessage, error) {
		return nil, errors.New("secret /author/project.db")
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "project:overview-current", draftContext("project-a", "lease-a", "/synthetic/project-a"))
	if response.Code != http.StatusInternalServerError || !containsDraftReadCode(response.Body.Bytes(), "PROJECT_OVERVIEW_FAILED") {
		t.Fatalf("provider error leaked: %d %s", response.Code, response.Body.String())
	}
}

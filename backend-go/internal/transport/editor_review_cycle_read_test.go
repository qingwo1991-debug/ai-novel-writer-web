package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestEditorReviewCycleAbsentReadIsNullableAndFenced(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	provider := EditorReviewCycleReadProviderFunc(func(_ context.Context, caller Caller, raw any, reviewID int64) (json.RawMessage, error) {
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" || !reflect.DeepEqual(raw, expected) {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
		}
		if reviewID == 8 {
			return nil, store.ErrReviewCycleProjectionUnavailable
		}
		return json.RawMessage("null"), nil
	})
	if err := server.BindEditorReviewCycleAbsentRead(provider); err != nil {
		t.Fatal(err)
	}
	if got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-cycle-get", 7, path, expected)); got != nil {
		t.Fatalf("absent cycle = %#v", got)
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-cycle-get", 8, path, expected), 501, "GO_OPERATION_NOT_MIGRATED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "db:review-cycle-get", 7, path, expected), 409, "PROJECT_SESSION_STALE")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-cycle-get", 7, "/foreign", expected), 409, "PROJECT_CONTEXT_CHANGED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-cycle-get", 0, path, expected), 400, "INVALID_ARGUMENTS")
	if err := server.BindEditorReviewCycleAbsentRead(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate binder: %v", err)
	}
}

func TestEditorReviewCycleAbsentReadRedactsProviderFailure(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	if err := server.BindEditorReviewCycleAbsentRead(EditorReviewCycleReadProviderFunc(func(context.Context, Caller, any, int64) (json.RawMessage, error) {
		return nil, errors.New("private /author/source.db SECRET_TOKEN")
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-cycle-get", 7, "/synthetic/project-a", draftContext("project-a", "lease-a", "/synthetic/project-a"))
	if response.Code != http.StatusInternalServerError || !containsDraftReadCode(response.Body.Bytes(), "REVIEW_REVISION_OPERATION_FAILED") {
		t.Fatalf("provider failure leaked: %d %s", response.Code, response.Body.String())
	}
}

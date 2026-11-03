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

func TestEditorReviewFullReadsBindOnlyNullableFullRows(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	provider := EditorReviewFullReadProviderFunc(func(_ context.Context, caller Caller, raw any, operation string, id int64) (json.RawMessage, error) {
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" || !reflect.DeepEqual(raw, expected) {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
		}
		if id == 99 {
			return json.RawMessage("null"), nil
		}
		if id != 7 {
			return nil, errors.New("unexpected id")
		}
		return json.Marshal(map[string]any{"id": 7, "baseDraftId": 3, "content": operation, "sourceDraft": map[string]any{"content": "source 🙂"}})
	})
	if err := server.BindEditorReviewFullReads(provider); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"db:review-get-full", "db:revision-get-full"} {
		got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", operation, 7, path, expected))
		row, ok := got.(map[string]any)
		if !ok || row["content"] != operation || row["sourceDraft"].(map[string]any)["content"] != "source 🙂" {
			t.Fatalf("%s = %#v", operation, got)
		}
		if missing := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", operation, 99, path, expected)); missing != nil {
			t.Fatalf("%s missing = %#v", operation, missing)
		}
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-get-full", 7, "/foreign", expected), 409, "PROJECT_CONTEXT_CHANGED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "db:review-get-full", 7, path, expected), 409, "PROJECT_SESSION_STALE")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:revision-get-full", 0, path, expected), 400, "INVALID_ARGUMENTS")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-create", map[string]any{}, path, expected), 501, "GO_OPERATION_NOT_MIGRATED")
	if err := server.BindEditorReviewFullReads(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate binder accepted: %v", err)
	}
}

func TestEditorReviewFullReadsRedactProviderFailure(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	if err := server.BindEditorReviewFullReads(EditorReviewFullReadProviderFunc(func(context.Context, Caller, any, string, int64) (json.RawMessage, error) {
		return nil, errors.New("private /author/source.db SECRET_TOKEN")
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-get-full", 7, "/synthetic/project-a", draftContext("project-a", "lease-a", "/synthetic/project-a"))
	if response.Code != http.StatusInternalServerError || !containsDraftReadCode(response.Body.Bytes(), "REVIEW_REVISION_OPERATION_FAILED") {
		t.Fatalf("provider failure leaked: %d %s", response.Code, response.Body.String())
	}
}

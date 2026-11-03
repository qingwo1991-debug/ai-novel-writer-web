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

func TestEditorReviewReadsBindOnlyMetadataLists(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	provider := EditorReviewReadProviderFunc(func(_ context.Context, caller Caller, raw any, operation string, baseDraftID int64) (json.RawMessage, error) {
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" || !reflect.DeepEqual(raw, expected) || baseDraftID != 7 {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
		}
		switch operation {
		case "db:review-list":
			return json.Marshal([]map[string]any{{"id": 11, "baseDraftId": 7, "reviewIndex": 1, "contentId": 99, "createdAt": "first"}})
		case "db:revision-get-pending":
			return json.Marshal([]map[string]any{{"id": 12, "baseDraftId": 7, "revisionIndex": 2, "status": "pending", "mergedToDraftId": nil}})
		case "db:revision-list":
			return json.Marshal([]map[string]any{{"id": 13, "baseDraftId": 7, "revisionIndex": 1, "status": "merged", "mergedToDraftId": 8}})
		default:
			return nil, errors.New("unexpected operation")
		}
	})
	if err := server.BindEditorReviewReads(provider); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		operation string
		wantID    float64
	}{
		{"db:review-list", 11},
		{"db:revision-get-pending", 12},
		{"db:revision-list", 13},
	} {
		got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", tc.operation, 7, path, expected))
		rows, ok := got.([]any)
		if !ok || len(rows) != 1 || rows[0].(map[string]any)["id"] != tc.wantID {
			t.Fatalf("%s = %#v", tc.operation, got)
		}
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-list", 7, "/foreign", expected), 409, "PROJECT_CONTEXT_CHANGED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "db:review-list", 7, path, expected), 409, "PROJECT_SESSION_STALE")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:revision-get-pending", 0, path, expected), 400, "INVALID_ARGUMENTS")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-create", map[string]any{}, path, expected), 501, "GO_OPERATION_NOT_MIGRATED")
	if err := server.BindEditorReviewReads(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate binder accepted: %v", err)
	}
}

func TestEditorReviewReadsRedactProviderFailures(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	if err := server.BindEditorReviewReads(EditorReviewReadProviderFunc(func(context.Context, Caller, any, string, int64) (json.RawMessage, error) {
		return nil, errors.New("private /author/source.db SECRET_TOKEN")
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:review-list", 7, "/synthetic/project-a", draftContext("project-a", "lease-a", "/synthetic/project-a"))
	if response.Code != http.StatusInternalServerError || !containsDraftReadCode(response.Body.Bytes(), "REVIEW_REVISION_OPERATION_FAILED") {
		t.Fatalf("provider failure leaked: %d %s", response.Code, response.Body.String())
	}
}

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

func TestEditorStageReadsBindOnlyCoreAndBlueprintReads(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	provider := EditorStageReadProviderFunc(func(_ context.Context, caller Caller, raw any, operation string) (json.RawMessage, error) {
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" || !reflect.DeepEqual(raw, expected) {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
		}
		switch operation {
		case "db:project-core-get":
			return json.RawMessage(`{"premise":"保持事实","writingLanguage":"zh-CN"}`), nil
		case "db:blueprint-get-all":
			return json.RawMessage(`[{"chapterNumber":1,"title":"第一章"}]`), nil
		default:
			return nil, errors.New("unexpected operation")
		}
	})
	if err := server.BindEditorStageReads(provider); err != nil {
		t.Fatal(err)
	}
	core := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:project-core-get", path, expected))
	if row, ok := core.(map[string]any); !ok || row["premise"] != "保持事实" {
		t.Fatalf("project core = %#v", core)
	}
	blueprints := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:blueprint-get-all", path, expected))
	if rows, ok := blueprints.([]any); !ok || len(rows) != 1 {
		t.Fatalf("blueprints = %#v", blueprints)
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:project-core-get", "/wrong", expected), 409, "PROJECT_CONTEXT_CHANGED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "db:blueprint-get-all", path, expected), 409, "PROJECT_SESSION_STALE")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:project-core-update", map[string]any{"premise": "forbidden"}, path, expected), 501, "GO_OPERATION_NOT_MIGRATED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:blueprint-upsert", map[string]any{"chapterNumber": 2}, path, expected), 501, "GO_OPERATION_NOT_MIGRATED")
	if err := server.BindEditorStageReads(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate binder accepted: %v", err)
	}
}

func TestEditorStageReadErrorsAreRedacted(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	path := "/synthetic/project-a"
	if err := server.BindEditorStageReads(EditorStageReadProviderFunc(func(context.Context, Caller, any, string) (json.RawMessage, error) {
		return nil, errors.New("secret /author/project.db")
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:blueprint-get-all", path, draftContext("project-a", "lease-a", path))
	if response.Code != http.StatusInternalServerError || !containsDraftReadCode(response.Body.Bytes(), "BLUEPRINT_OPERATION_FAILED") {
		t.Fatalf("provider error leaked: %d %s", response.Code, response.Body.String())
	}
}

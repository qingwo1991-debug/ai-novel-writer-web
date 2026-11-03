package transport

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestDraftLiveReadsBindOnlySnapshotReads(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	db, _ := transportDraftDB(t)
	draftSeed(t, db, 1, 1, 1, "draft", "保留正文🙂")
	path := "/synthetic/project-a"
	expected := draftContext("project-a", "lease-a", path)
	provider := DraftLiveReadProviderFunc(func(ctx context.Context, caller Caller, raw any, operation string, number int64) (json.RawMessage, error) {
		if caller.SessionID != cookie.Value || caller.ClientID != "synthetic-tab-0001" || !reflect.DeepEqual(raw, expected) {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_STALE")
		}
		tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		value, err := store.ReadDraftOperation(ctx, tx, operation, number)
		if err != nil {
			return nil, err
		}
		return json.Marshal(value)
	})
	if err := server.BindDraftLiveReads(provider); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		operation string
		args      []any
		wantKind  string
	}{
		{"db:draft-list", []any{1, path, expected}, "list"},
		{"db:draft-list-all", []any{path, expected}, "list"},
		{"db:draft-get-meta", []any{1, path, expected}, "object"},
		{"db:draft-get-full", []any{1, path, expected}, "full"},
		{"db:draft-get-latest", []any{1, path, expected}, "object"},
		{"db:draft-get-finalized", []any{1, path, expected}, "null"},
		{"db:draft-get-max-finalized-chapter", []any{path, expected}, "zero"},
		{"db:draft-next-version", []any{1, path, expected}, "two"},
	} {
		t.Run(item.operation, func(t *testing.T) {
			got := draftResult(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", item.operation, item.args...))
			switch item.wantKind {
			case "list":
				if rows, ok := got.([]any); !ok || len(rows) != 1 {
					t.Fatalf("list = %#v", got)
				}
			case "object", "full":
				row, ok := got.(map[string]any)
				if !ok || row["id"] != float64(1) || (item.wantKind == "full" && row["content"] != "保留正文🙂") {
					t.Fatalf("draft = %#v", got)
				}
			case "null":
				if got != nil {
					t.Fatalf("finalized = %#v", got)
				}
			case "zero":
				if got != float64(0) {
					t.Fatalf("max finalized = %#v", got)
				}
			case "two":
				if got != float64(2) {
					t.Fatalf("next version = %#v", got)
				}
			}
		})
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list", 1, "/different", expected), 409, "PROJECT_CONTEXT_CHANGED")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0002", "db:draft-get-full", 1, path, expected), 409, "PROJECT_SESSION_STALE")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-get-full", 1, path), 400, "INVALID_ARGUMENTS")
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-update-content", 1, "changed", 1, path, "保留正文🙂", expected), 501, "GO_OPERATION_NOT_MIGRATED")
	if err := server.BindDraftLiveReads(provider); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate binding accepted: %v", err)
	}
}

func TestDraftLiveReadProviderErrorsAreRedacted(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	if err := server.BindDraftLiveReads(DraftLiveReadProviderFunc(func(context.Context, Caller, any, string, int64) (json.RawMessage, error) {
		return nil, errors.New("private /author/source.db SECRET_TOKEN")
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:draft-list-all", "/synthetic/project-a", draftContext("project-a", "lease-a", "/synthetic/project-a"))
	if response.Code != http.StatusInternalServerError || !json.Valid(response.Body.Bytes()) || !containsDraftReadCode(response.Body.Bytes(), "DRAFT_READ_FAILED") {
		t.Fatalf("provider error leaked: %d %s", response.Code, response.Body.String())
	}
}

func containsDraftReadCode(body []byte, code string) bool {
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	return json.Unmarshal(body, &envelope) == nil && envelope.Error.Code == code
}

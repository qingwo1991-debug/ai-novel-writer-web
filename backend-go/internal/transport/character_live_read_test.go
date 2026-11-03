package transport

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	novelruntime "github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/runtime"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

func TestCharacterLiveReadsUseExactContextAndLeaveCommitUnbound(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	owner := draftOwner(cookie, "synthetic-tab-0001")
	expected := draftContext("project-1", "lease-1", "/synthetic/project-1")
	want := map[string]json.RawMessage{
		"db:character-get-all":     json.RawMessage(`[{"characterId":"a","name":"甲"}]`),
		"db:character-roster-read": json.RawMessage(`{"status":"empty","entries":[]}`),
		"character-identity:read":  json.RawMessage(`{"revision":0,"characters":[],"aliases":[],"relationships":[]}`),
	}
	calls := 0
	if err := server.BindCharacterLiveReads(CharacterLiveReadProviderFunc(func(_ context.Context, caller Caller, raw any, operation string) (json.RawMessage, error) {
		calls++
		if caller != owner || !reflect.DeepEqual(raw, expected) {
			return nil, security.NewHTTPError(http.StatusConflict, "PROJECT_SESSION_NOT_OWNED")
		}
		return want[operation], nil
	})); err != nil {
		t.Fatal(err)
	}
	for operation, wire := range want {
		got := draftResult(t, draftRPC(t, server, cookie, csrf, owner.ClientID, operation, charactersTransportArgs(operation, expected)...))
		var desired any
		if err := json.Unmarshal(wire, &desired); err != nil || !reflect.DeepEqual(got, desired) {
			t.Fatalf("%s: %#v != %#v: %v", operation, got, desired, err)
		}
	}
	if calls != 3 {
		t.Fatalf("read provider calls = %d", calls)
	}
	commit := draftRPC(t, server, cookie, csrf, owner.ClientID, "db:character-roster-commit", map[string]any{"intent": "manual_edit"}, expected["projectPath"], expected)
	if commit.Code != http.StatusNotImplemented || calls != 3 {
		t.Fatalf("commit bound or borrowed read authority: %d %s", commit.Code, commit.Body.String())
	}
	for _, bad := range []struct {
		name string
		args []any
		code string
	}{
		{"missing-context", []any{expected["projectPath"]}, "INVALID_ARGUMENTS"},
		{"extra-context", []any{expected["projectPath"], map[string]any{"projectId": "project-1", "leaseId": "lease-1", "projectPath": expected["projectPath"], "extra": true}}, "PROJECT_SESSION_REQUIRED"},
		{"wrong-path", []any{"/other", expected}, "PROJECT_CONTEXT_CHANGED"},
	} {
		t.Run(bad.name, func(t *testing.T) {
			response := draftRPC(t, server, cookie, csrf, owner.ClientID, "db:character-get-all", bad.args...)
			requireDraftHTTPError(t, response, map[string]int{"INVALID_ARGUMENTS": http.StatusBadRequest, "PROJECT_SESSION_REQUIRED": http.StatusBadRequest, "PROJECT_CONTEXT_CHANGED": http.StatusConflict}[bad.code], bad.code)
		})
	}
	if calls != 3 {
		t.Fatalf("invalid requests reached provider: %d", calls)
	}
	requireDraftHTTPError(t, draftRPC(t, server, cookie, csrf, "synthetic-other-tab", "db:character-get-all", charactersTransportArgs("db:character-get-all", expected)...), http.StatusConflict, "PROJECT_SESSION_NOT_OWNED")
}

func TestCharacterLiveReadBindingRejectsTypedNilAndInvalidWire(t *testing.T) {
	server := fixture(t)
	var nilProvider CharacterLiveReadProviderFunc
	if err := server.BindCharacterLiveReads(nilProvider); err == nil {
		t.Fatal("typed nil provider accepted")
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-1", "lease-1", "/synthetic/project-1")
	if err := server.BindCharacterLiveReads(CharacterLiveReadProviderFunc(func(context.Context, Caller, any, string) (json.RawMessage, error) {
		return json.RawMessage(`{"private":"unterminated"`), nil
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:character-get-all", charactersTransportArgs("db:character-get-all", expected)...)
	var public map[string]map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil || response.Code != http.StatusInternalServerError || public["error"]["code"] != "CHARACTER_PROVIDER_UNAVAILABLE" {
		t.Fatalf("invalid provider wire exposed or accepted: %d %s: %v", response.Code, response.Body.String(), err)
	}
	if err := server.BindCharacterLiveReads(CharacterLiveReadProviderFunc(func(context.Context, Caller, any, string) (json.RawMessage, error) {
		return nil, nil
	})); err == nil || err.Error() != "OPERATION_ALREADY_BOUND" {
		t.Fatalf("duplicate binding replaced read handlers: %v", err)
	}
}

func TestCharacterLiveReadRuntimeNotOwnedKeepsOriginalGenericStatus(t *testing.T) {
	server := fixture(t)
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("project-1", "lease-1", "/synthetic/project-1")
	if err := server.BindCharacterLiveReads(CharacterLiveReadProviderFunc(func(context.Context, Caller, any, string) (json.RawMessage, error) {
		return nil, novelruntime.ErrNotOwned
	})); err != nil {
		t.Fatal(err)
	}
	response := draftRPC(t, server, cookie, csrf, "synthetic-tab-0001", "db:character-get-all", charactersTransportArgs("db:character-get-all", expected)...)
	var public map[string]map[string]string
	if err := json.Unmarshal(response.Body.Bytes(), &public); err != nil || response.Code != http.StatusInternalServerError || public["error"]["code"] != "PROJECT_SESSION_NOT_OWNED" || public["error"]["message"] == "PROJECT_SESSION_NOT_OWNED" {
		t.Fatalf("unexpected generic session denial: %d %s: %v", response.Code, response.Body.String(), err)
	}
}

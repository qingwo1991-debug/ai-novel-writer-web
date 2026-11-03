package transport

import (
	"context"
	"net/http"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

func TestReadOnlyProjectOpenBindingIsIndependentAndCatalogOnly(t *testing.T) {
	server := fixture(t)
	called := 0
	provider := ProjectReadOpenProviderFunc(func(_ context.Context, caller Caller, key, token string) (any, error) {
		called++
		if caller.SessionID != "cookie" || caller.ClientID != "tab" || key != "catalog-key" || token != "request" {
			t.Fatalf("untrusted arguments forwarded: %+v %q %q", caller, key, token)
		}
		return map[string]any{"success": true, "requestToken": token}, nil
	})
	if err := server.BindProjectReadOpen(provider); err != nil {
		t.Fatal(err)
	}
	if server.handlers["project:save"] != nil || server.handlers["db:close"] != nil || server.handlers["project:get-runtime-context"] != nil {
		t.Fatal("read-only binder exposed lifecycle or save")
	}
	caller := Caller{SessionID: "cookie", ClientID: "tab"}
	result, err := server.handlers["project:open"](context.Background(), caller, []any{"catalog-key", "request", "/untrusted/renderer/path"})
	if err != nil || result.(map[string]any)["success"] != true || called != 1 {
		t.Fatalf("valid catalog key not forwarded: %#v %v", result, err)
	}
	for _, args := range [][]any{{map[string]any{"grantId": "unresolved"}, "request", nil},
		{"", "request", nil}, {"catalog-key", "", nil}, {"catalog-key", "request", 7},
		{"catalog-key", "request"}} {
		_, err := server.handlers["project:open"](context.Background(), caller, args)
		public, ok := err.(*security.HTTPError)
		if !ok || public.Status != http.StatusBadRequest {
			t.Fatalf("accepted invalid target %#v: %v", args, err)
		}
	}
	if called != 1 {
		t.Fatalf("invalid target reached provider %d times", called)
	}
}

func TestReadOnlyProjectOpenGrantBindingAcceptsOnlyOpaqueTarget(t *testing.T) {
	server := fixture(t)
	catalogCalls, grantCalls := 0, 0
	catalog := ProjectReadOpenProviderFunc(func(_ context.Context, _ Caller, key, token string) (any, error) {
		catalogCalls++
		if key != "catalog-key" || token != "catalog-request" {
			t.Fatalf("catalog arguments changed: %q %q", key, token)
		}
		return map[string]any{"success": true}, nil
	})
	grants := ProjectGrantReadOpenProviderFunc(func(_ context.Context, caller Caller, grantID, token string) (any, error) {
		grantCalls++
		if caller.SessionID != "cookie" || caller.ClientID != "tab" || grantID != "opaque-grant" || token != "grant-request" {
			t.Fatalf("untrusted grant arguments forwarded: %+v %q %q", caller, grantID, token)
		}
		return map[string]any{"success": true}, nil
	})
	if err := server.BindProjectReadOpenWithGrants(catalog, grants); err != nil {
		t.Fatal(err)
	}
	if server.handlers["project:save"] != nil || server.handlers["db:close"] != nil {
		t.Fatal("grant binder exposed other project mutations")
	}
	caller := Caller{SessionID: "cookie", ClientID: "tab"}
	for _, request := range []struct {
		target any
		token  string
	}{
		{map[string]any{"grantId": "opaque-grant"}, "grant-request"},
		{map[string]any{"grantId": "opaque-grant", "displayName": "Book"}, "grant-request"},
		{"catalog-key", "catalog-request"},
	} {
		result, err := server.handlers["project:open"](context.Background(), caller, []any{request.target, request.token, "/untrusted/renderer/path"})
		if err != nil || result.(map[string]any)["success"] != true {
			t.Fatalf("valid target refused: %#v %v", request.target, err)
		}
	}
	if catalogCalls != 1 || grantCalls != 2 {
		t.Fatalf("unexpected routing: catalog=%d grants=%d", catalogCalls, grantCalls)
	}
	for _, target := range []any{
		map[string]any{"grantId": "opaque-grant", "path": "/author/project"},
		map[string]any{"grantId": "opaque-grant", "displayName": "Book", "extra": true},
		map[string]any{"grantId": ""}, map[string]any{"grantId": 17},
		map[string]any{"grantId": "opaque-grant", "displayName": 17},
		map[string]any{"path": "/author/project"},
	} {
		_, err := server.handlers["project:open"](context.Background(), caller, []any{target, "grant-request", nil})
		public, ok := err.(*security.HTTPError)
		if !ok || public.Status != http.StatusBadRequest {
			t.Fatalf("accepted invalid grant target %#v: %v", target, err)
		}
	}
	if catalogCalls != 1 || grantCalls != 2 {
		t.Fatalf("invalid target reached provider: catalog=%d grants=%d", catalogCalls, grantCalls)
	}
}

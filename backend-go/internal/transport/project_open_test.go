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

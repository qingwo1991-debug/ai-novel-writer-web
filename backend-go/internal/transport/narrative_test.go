package transport

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/store"
)

func TestNarrativeAtomicBindingsAndPrivateErrors(t *testing.T) {
	server := fixture(t)
	providerCalls := 0
	provider := NarrativeProviderFunc(func(context.Context, Caller, any) (*store.Narrative, func(), error) {
		providerCalls++
		return nil, nil, errors.New("private provider prompt /author/path")
	})
	if err := server.Bind("db:consistency-exemption-list", func(context.Context, Caller, []any) (any, error) { return "existing", nil }); err != nil {
		t.Fatal(err)
	}
	if err := server.BindNarrative(provider); err == nil {
		t.Fatal("partial bind allowed")
	}
	if _, ok := server.handlers["db:narrative-thread-plan-create"]; ok {
		t.Fatal("binding failure partially registered")
	}
	server = fixture(t)
	if err := server.BindNarrative(provider); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	expected := draftContext("synthetic-project", "synthetic-lease", "/synthetic/book")
	response := draftRPC(t, server, cookie, csrf, "synthetic-narrative-tab-001", "db:narrative-thread-plan-update", "bad id", map[string]any{}, "/synthetic/book", expected)
	if response.Code != 400 || providerCalls != 0 {
		t.Fatal(response.Code, response.Body.String(), providerCalls)
	}
	response = draftRPC(t, server, cookie, csrf, "synthetic-narrative-tab-001", "db:consistency-exemption-save", "key", "reason", "/synthetic/book", expected)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "NARRATIVE_OPERATION_FAILED") || strings.Contains(response.Body.String(), "/author/") {
		t.Fatal(response.Code, response.Body.String())
	}
	response = draftRPC(t, server, cookie, csrf, "synthetic-narrative-tab-001", "db:narrative-thread-list", "/synthetic/book", expected)
	if response.Code != 500 || !strings.Contains(response.Body.String(), "NARRATIVE_OPERATION_FAILED") || strings.Contains(response.Body.String(), "prompt") {
		t.Fatal(response.Code, response.Body.String())
	}
}

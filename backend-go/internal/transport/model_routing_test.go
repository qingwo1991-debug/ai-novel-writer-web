package transport

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

// TestModelRoutingReadsServeOnlyTheCommittedReadHalf pins the read-only split of
// the routing family. The production lane serves the author's committed global
// generation, so it may answer the three names that only read config.json, but it
// must not install the three names that rewrite that file: an installed setter
// would answer 200 and move bytes the lane has promised not to touch, while an
// uninstalled one keeps failing closed at 501 GO_OPERATION_NOT_MIGRATED.
func TestModelRoutingReadsServeOnlyTheCommittedReadHalf(t *testing.T) {
	server := fixture(t)
	models, root := trustedConfigFixture(t)
	raw := `{"defaultModelId":"saved-model","defaultEmbeddingModelId":"saved-embedding","stageModelIds":{"drafting":"saved-model"}}`
	if err := os.WriteFile(filepath.Join(root, "config.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "admitted-test-generation"})
	if err := server.BindModelRoutingReads(models, startup); err != nil {
		t.Fatal(err)
	}
	if bound := strings.Join(server.BoundOperations(), ","); bound !=
		"llm:get-default-embedding-model,llm:get-default-model,llm:get-stage-models" {
		t.Fatalf("the routing read binder installed %q", bound)
	}
	cookie, csrf := bootstrap(t, server)
	for _, want := range []struct{ operation, body string }{
		{"llm:get-stage-models", `"drafting":"saved-model"`},
		{"llm:get-default-model", `"result":"saved-model"`},
		{"llm:get-default-embedding-model", `"result":"saved-embedding"`},
	} {
		response := rpc(t, server, cookie, csrf, want.operation, `{"args":[]}`, nil)
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), want.body) {
			t.Fatalf("%s: %d %s", want.operation, response.Code, response.Body.String())
		}
	}
	for _, operation := range []string{"llm:set-stage-model", "llm:set-default-model", "llm:set-default-embedding-model"} {
		response := rpc(t, server, cookie, csrf, operation, `{"args":[null]}`, nil)
		if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
			t.Fatalf("%s is bound on the read-only lane: %d %s", operation, response.Code, response.Body.String())
		}
	}
	// Nothing above may have rewritten the committed document.
	after, err := os.ReadFile(filepath.Join(root, "config.json"))
	if err != nil || string(after) != raw {
		t.Fatalf("a routing read changed the committed config: %v %s", err, after)
	}
	// The full family keeps installing all six names, so the split is additive.
	full := fixture(t)
	if err := full.BindModelRouting(models, startup); err != nil {
		t.Fatal(err)
	}
	if got := len(full.BoundOperations()); got != 6 {
		t.Fatalf("BindModelRouting installed %d names", got)
	}
	if err := fixture(t).BindModelRoutingReads(nil, startup); err == nil {
		t.Fatal("the routing read binder accepted a nil model store")
	}
}

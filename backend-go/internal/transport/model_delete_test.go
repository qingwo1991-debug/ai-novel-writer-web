package transport

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

func TestModelDeleteHTTPClearsReferencesAndPreservesOtherCredential(t *testing.T) {
	server := fixture(t)
	models, root := trustedConfigFixture(t)
	for _, model := range []globalconfig.ModelRecord{
		{"id": "writer", "provider": "openai", "protocol": "openai", "baseUrl": "https://synthetic.invalid/v1", "apiKey": "private-writer-key"},
		{"id": "other", "provider": "openai", "protocol": "openai", "baseUrl": "https://synthetic.invalid/v1", "apiKey": "private-other-key"},
	} {
		if err := models.SaveModel(model); err != nil {
			t.Fatal(err)
		}
	}
	config := []byte(`{"defaultModelId":"writer","defaultEmbeddingModelId":"writer","stageModelIds":{"planning":"writer","review":"other"},"untouched":{"keep":true}}`)
	if err := os.WriteFile(filepath.Join(root, "config.json"), config, 0600); err != nil {
		t.Fatal(err)
	}
	if err := server.BindModelDelete(models, globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "admitted"})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	response := rpc(t, server, cookie, csrf, "llm:delete-model", `{"args":["writer"]}`, nil)
	body := response.Body.String()
	if response.Code != 200 || !strings.Contains(body, `"success":true`) ||
		!strings.Contains(body, `"defaultModelId":null`) || !strings.Contains(body, `"defaultEmbeddingModelId":null`) ||
		!strings.Contains(body, `"review":"other"`) || strings.Contains(body, `"planning"`) ||
		strings.Contains(body, "private-writer-key") || strings.Contains(body, "private-other-key") {
		t.Fatal(response.Code, body)
	}
	private, err := models.ListPrivateModels()
	if err != nil || len(private) != 1 || private[0]["id"] != "other" || private[0]["apiKey"] != "private-other-key" {
		t.Fatal("model deletion changed another credential", err, private)
	}
	current, err := models.Get()
	if err != nil {
		t.Fatal(err)
	}
	settings := current.(map[string]any)
	if settings["defaultModelId"] != nil || settings["defaultEmbeddingModelId"] != nil || settings["untouched"] == nil {
		t.Fatal("global settings changed outside model references", settings)
	}
}

func TestModelDeleteHTTPFailsClosedOnJournalAndStartup(t *testing.T) {
	models, root := trustedConfigFixture(t)
	if err := models.SaveModel(globalconfig.ModelRecord{
		"id": "writer", "provider": "openai", "protocol": "openai",
		"baseUrl": "https://synthetic.invalid/v1", "apiKey": "private-writer-key",
	}); err != nil {
		t.Fatal(err)
	}
	pending := fixture(t)
	if err := pending.BindModelDelete(models, globalconfig.NewStartup(globalconfig.StartupCapture{})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, pending)
	blocked := rpc(t, pending, cookie, csrf, "llm:delete-model", `{"args":["writer"]}`, nil)
	if blocked.Code != 409 || !strings.Contains(blocked.Body.String(), "GLOBAL_DATA_NOT_READY") {
		t.Fatal(blocked.Code, blocked.Body.String())
	}
	if err := os.WriteFile(filepath.Join(root, ".model-delete.journal"), []byte(`{"secret":"private-writer-key","path":"/author/data"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ready := fixture(t)
	if err := ready.BindModelDelete(models, globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "admitted"})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf = bootstrap(t, ready)
	failure := rpc(t, ready, cookie, csrf, "llm:delete-model", `{"args":["writer"]}`, nil)
	if failure.Code != 200 || !strings.Contains(failure.Body.String(), `"success":false`) ||
		!strings.Contains(failure.Body.String(), "LLM_MODEL_DELETE_JOURNAL_INVALID") ||
		strings.Contains(failure.Body.String(), "private-writer-key") || strings.Contains(failure.Body.String(), "/author/data") {
		t.Fatal(failure.Code, failure.Body.String())
	}
	wrong := rpc(t, ready, cookie, csrf, "llm:delete-model", `{"args":[{"id":"writer"}]}`, nil)
	if wrong.Code != 200 || !strings.Contains(wrong.Body.String(), "LLM_MODEL_INVALID") {
		t.Fatal(wrong.Code, wrong.Body.String())
	}
	invalid := rpc(t, ready, cookie, csrf, "llm:delete-model", `{"args":[]}`, nil)
	if invalid.Code != 400 || !strings.Contains(invalid.Body.String(), "INVALID_ARGUMENTS") {
		t.Fatal(invalid.Code, invalid.Body.String())
	}
}

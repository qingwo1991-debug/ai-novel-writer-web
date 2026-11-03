package transport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

func TestModelListHTTPProjectsPrivateProfiles(t *testing.T) {
	server := fixture(t)
	models, _ := trustedConfigFixture(t)
	if err := models.SaveModel(globalconfig.ModelRecord{
		"id": "one", "provider": "openai", "protocol": "openai",
		"baseUrl": "https://synthetic.invalid/v1", "apiKey": "synthetic-key-one",
		"name": "First model", "modelName": "synthetic-model",
		"temperature": 0.7, "maxTokens": 1024,
	}); err != nil {
		t.Fatal(err)
	}
	if err := models.SaveModel(globalconfig.ModelRecord{
		"id": "two", "provider": "gemini", "protocol": "gemini",
		"baseUrl": "https://synthetic.invalid/v1", "apiKey": "synthetic-key-two",
		"name": "contains synthetic-key-one", "modelName": "synthetic-second",
	}); err != nil {
		t.Fatal(err)
	}
	startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "admitted-model-generation"})
	if err := server.BindModelReads(models, startup); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	response := rpc(t, server, cookie, csrf, "llm:list-models", `{"args":[]}`, nil)
	body := response.Body.String()
	if response.Code != 200 || !strings.Contains(body, `"hasCredential":true`) ||
		!strings.Contains(body, `"apiKey":""`) || !strings.Contains(body, `"modelName":"synthetic-model"`) ||
		strings.Contains(body, "synthetic-key-one") || strings.Contains(body, "synthetic-key-two") {
		t.Fatal(response.Code, body)
	}
	private, err := models.ListPrivateModels()
	if err != nil || len(private) != 2 || private[0]["apiKey"] != "synthetic-key-one" {
		t.Fatal("public listing changed private credentials", err)
	}
	invalid := rpc(t, server, cookie, csrf, "llm:list-models", `{"args":[null]}`, nil)
	if invalid.Code != 400 || !strings.Contains(invalid.Body.String(), "INVALID_ARGUMENTS") {
		t.Fatal(invalid.Code, invalid.Body.String())
	}
}

func TestModelListFailClosedOnCorruptPrivateFileAndPendingStartup(t *testing.T) {
	models, root := trustedConfigFixture(t)
	server := fixture(t)
	if err := server.BindModelReads(models, globalconfig.NewStartup(globalconfig.StartupCapture{})); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	pending := rpc(t, server, cookie, csrf, "llm:list-models", `{"args":[]}`, nil)
	if pending.Code != 409 || !strings.Contains(pending.Body.String(), "GLOBAL_DATA_NOT_READY") {
		t.Fatal(pending.Code, pending.Body.String())
	}
	ready := fixture(t)
	if err := ready.BindModelReads(models, globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "ready"})); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "models.json"), []byte(`{"apiKey":"PRIVATE-KEY","path":"/private/author"}`), 0600); err != nil {
		t.Fatal(err)
	}
	cookie, csrf = bootstrap(t, ready)
	response := rpc(t, ready, cookie, csrf, "llm:list-models", `{"args":[]}`, nil)
	if response.Code != 500 || !strings.Contains(response.Body.String(), "MODEL_CONFIG_UNAVAILABLE") ||
		strings.Contains(response.Body.String(), "PRIVATE-KEY") || strings.Contains(response.Body.String(), "/private/author") {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestModelListBindingRejectsMissingProviderAndCollision(t *testing.T) {
	server := fixture(t)
	models, _ := trustedConfigFixture(t)
	startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "ready"})
	if err := server.BindModelReads(nil, startup); err == nil {
		t.Fatal("nil model store accepted")
	}
	if err := server.Bind("llm:list-models", func(context.Context, Caller, []any) (any, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if err := server.BindModelReads(models, startup); err == nil {
		t.Fatal("existing model handler replaced")
	}
}

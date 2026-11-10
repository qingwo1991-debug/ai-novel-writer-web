package transport

import (
	"strings"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
)

func TestModelSaveUsesTrustedEndpointAndPreservesOnlySameEndpointCredential(t *testing.T) {
	server := fixture(t)
	models, _ := trustedConfigFixture(t)
	policy, err := egress.New([]string{"https://allowed.example:8443"})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	if err := server.BindModelSave(models, globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "trusted"}), policy, nil); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	newProfile := `{"args":[{"id":"writer","name":"Writer","provider":"openai","protocol":"openai","modelName":"model","baseUrl":"https://allowed.example:8443/v1","apiKey":"secret-for-writer","temperature":0.7,"maxTokens":1024,"purposes":["generation"],"reasoningMapping":{"adapter":"openai-thinking-budget","supportedEfforts":["off","high"],"providerValues":{"off":0,"high":1024}}}]}`
	saved := rpc(t, server, cookie, csrf, "llm:save-model", newProfile, nil)
	if saved.Code != 200 || !strings.Contains(saved.Body.String(), `"success":true`) || strings.Contains(saved.Body.String(), "secret-for-writer") {
		t.Fatal(saved.Code, saved.Body.String())
	}
	blankKey := `{"args":[{"id":"writer","name":"Renamed","provider":"openai","protocol":"openai","modelName":"model","baseUrl":"https://allowed.example:8443/v1","apiKey":"","hasCredential":true,"temperature":0.7,"maxTokens":1024,"purposes":["generation"]}]}`
	restored := rpc(t, server, cookie, csrf, "llm:save-model", blankKey, nil)
	if restored.Code != 200 || !strings.Contains(restored.Body.String(), `"success":true`) {
		t.Fatal(restored.Code, restored.Body.String())
	}
	private, err := models.ListPrivateModels()
	if err != nil || len(private) != 1 || private[0]["apiKey"] != "secret-for-writer" || private[0]["name"] != "Renamed" {
		t.Fatal("same-endpoint save lost private credential", err, private)
	}
	changed := `{"args":[{"id":"writer","name":"Denied","provider":"openai","protocol":"openai","modelName":"model","baseUrl":"https://allowed.example:8443/v2","apiKey":"","temperature":0.7,"maxTokens":1024,"purposes":["generation"]}]}`
	// A path change on the same origin is still the same endpoint by the
	// original gateway rule only when trailing slashes differ; /v1 to /v2
	// must refuse blank-key credential reuse.
	rejected := rpc(t, server, cookie, csrf, "llm:save-model", changed, nil)
	if rejected.Code != 400 || !strings.Contains(rejected.Body.String(), "CREDENTIAL_ENDPOINT_CHANGED") {
		t.Fatal(rejected.Code, rejected.Body.String())
	}
	private, err = models.ListPrivateModels()
	if err != nil || private[0]["baseUrl"] != "https://allowed.example:8443/v1" {
		t.Fatal("rejected update changed private profile", err, private)
	}
	// A base URL outside the current policy is the author declaring a new
	// endpoint: the save commits it and the lane rebuilds the dispatch policy
	// from that committed config. It must succeed and never echo the credential.
	otherOrigin := strings.Replace(newProfile, "https://allowed.example:8443/v1", "https://other.example/v1", 1)
	added := rpc(t, server, cookie, csrf, "llm:save-model", otherOrigin, nil)
	if added.Code != 200 || !strings.Contains(added.Body.String(), `"success":true`) || strings.Contains(added.Body.String(), "secret-for-writer") {
		t.Fatal(added.Code, added.Body.String())
	}
	// The same profile id is an update, not a second model: it persists the new
	// endpoint in place and never echoes the credential.
	private, err = models.ListPrivateModels()
	if err != nil || len(private) != 1 || private[0]["baseUrl"] != "https://other.example/v1" {
		t.Fatal("a committed new endpoint did not persist in place", err, private)
	}
}

// TestModelSaveRefreshRunsAfterTheCommit proves the lane can adopt the newly
// committed endpoint without a restart: refresh is invoked once, after the
// profile is durable.
func TestModelSaveRefreshRunsAfterTheCommit(t *testing.T) {
	server := fixture(t)
	models, _ := trustedConfigFixture(t)
	policy, err := egress.New([]string{"https://allowed.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	refreshes := 0
	var profilesAtRefresh int
	err = server.BindModelSave(models, globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "trusted"}), policy, func() error {
		refreshes++
		private, listErr := models.ListPrivateModels()
		if listErr == nil {
			profilesAtRefresh = len(private)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	profile := `{"args":[{"id":"writer","name":"Writer","provider":"openai","protocol":"openai","modelName":"model","baseUrl":"https://new.example/v1","apiKey":"credential","purposes":["generation"]}]}`
	if response := rpc(t, server, cookie, csrf, "llm:save-model", profile, nil); response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	if refreshes != 1 || profilesAtRefresh != 1 {
		t.Fatalf("refresh ran %d times with %d committed profiles; want 1/1", refreshes, profilesAtRefresh)
	}
}

func TestModelSaveRejectsReasoningMappingMismatchWithoutWriting(t *testing.T) {
	server := fixture(t)
	models, _ := trustedConfigFixture(t)
	policy, err := egress.New([]string{"https://allowed.example"})
	if err != nil {
		t.Fatal(err)
	}
	defer policy.Close()
	if err := server.BindModelSave(models, globalconfig.NewStartup(globalconfig.StartupCapture{Generation: "trusted"}), policy, nil); err != nil {
		t.Fatal(err)
	}
	cookie, csrf := bootstrap(t, server)
	body := `{"args":[{"id":"writer","name":"Writer","provider":"openai","protocol":"openai","modelName":"model","baseUrl":"https://allowed.example/v1","apiKey":"credential","reasoningMapping":{"adapter":"gemini-thinking-level","supportedEfforts":["off"],"providerValues":{"off":"minimal"}}}]}`
	response := rpc(t, server, cookie, csrf, "llm:save-model", body, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "INVALID_REASONING_MAPPING") || strings.Contains(response.Body.String(), "credential") {
		t.Fatal(response.Code, response.Body.String())
	}
	private, err := models.ListPrivateModels()
	if err != nil || len(private) != 0 {
		t.Fatal("rejected mapping persisted profile", err, private)
	}
}

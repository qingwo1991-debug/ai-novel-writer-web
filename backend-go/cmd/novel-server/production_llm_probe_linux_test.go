//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// productionProbeRecorder records what one fake provider was actually asked, so a
// test can assert the request shape, the dispatch count and the credential the
// probe sent without reaching into the transport.
type productionProbeRecorder struct {
	mu      sync.Mutex
	paths   []string
	auths   []string
	bodies  []string
}

func (r *productionProbeRecorder) record(request *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, request.Method+" "+request.URL.Path)
	r.auths = append(r.auths, request.Header.Get("Authorization"))
	body, _ := io.ReadAll(request.Body)
	r.bodies = append(r.bodies, string(body))
}

func (r *productionProbeRecorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.paths)
}

func (r *productionProbeRecorder) lastPath() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.paths) == 0 {
		return ""
	}
	return r.paths[len(r.paths)-1]
}

func (r *productionProbeRecorder) lastAuth() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.auths) == 0 {
		return ""
	}
	return r.auths[len(r.auths)-1]
}

func (r *productionProbeRecorder) lastBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.bodies) == 0 {
		return ""
	}
	return r.bodies[len(r.bodies)-1]
}

// productionProbeFakeProvider is the only provider these tests ever talk to: a
// loopback httptest server that answers one fixed status and body. Nothing here
// opens a real network connection.
func productionProbeFakeProvider(t *testing.T, status int, body string) (*httptest.Server, *productionProbeRecorder) {
	t.Helper()
	recorder := &productionProbeRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			return
		}
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

// productionProbeSilentProvider accepts the connection and answers only when the
// caller gives up, which is how a hung endpoint behaves.
func productionProbeSilentProvider(t *testing.T) (*httptest.Server, *productionProbeRecorder) {
	t.Helper()
	recorder := &productionProbeRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		recorder.record(request)
		select {
		case <-request.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

// productionProbeFixture is one admitted lane over one committed global root: the
// same config service the production lane pins, the same policy derived from it by
// productionModelEgressPlan, and the two bound names answered through the real
// transport with a real bootstrap, CSRF token and frozen tab.
type productionProbeFixture struct {
	config *globalconfig.Service
	policy *egress.Client
	probe  *productionLLMProbe
	client *productionDurableClient
}

func newProductionProbeFixture(t *testing.T, origins []string, models string) *productionProbeFixture {
	t.Helper()
	declared, err := json.Marshal(origins)
	if err != nil {
		t.Fatal(err)
	}
	service := productionCommittedConfigRoot(t, `{"theme":"dark","defaultModelId":null,"modelOrigins":`+string(declared)+`}`, models)
	plan, err := productionModelEgressPlan(service)
	if err != nil {
		t.Fatalf("the committed allowlist did not produce a policy: %v", err)
	}
	t.Cleanup(plan.policy.Close)
	startup := globalconfig.NewStartup(globalconfig.StartupCapture{Generation: productionTestGeneration})
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-admitted")
	probe := newProductionLLMProbeOver(service, startup, func() (*egress.Client, error) {
		return plan.policy, nil
	})
	if err := probe.Bind(server); err != nil {
		t.Fatalf("binding the two llm names failed: %v", err)
	}
	return &productionProbeFixture{
		config: service,
		policy: plan.policy,
		probe:  probe,
		client: newProductionDurableClient(t, server, productionTestTab),
	}
}

// productionProbeTypedProfile is the settings form as the browser sends it for a
// key the author typed in but has not saved yet.
func productionProbeTypedProfile(baseURL, key string) map[string]any {
	return map[string]any{
		"id":            productionTestModelID,
		"name":          "Story model",
		"provider":      "custom",
		"protocol":      "openai",
		"modelName":     "ci-story-model",
		"baseUrl":       baseURL,
		"apiKey":        key,
		"hasCredential": false,
		"temperature":   0.7,
		"maxTokens":     4096,
		"purposes":      []any{"generation"},
	}
}

// productionProbeSavedProfile is the settings form as the browser sends it for an
// already saved model: the public Web lane redacts apiKey
// (src/shared/ipc-channels.ts:833), so the profile carries only hasCredential.
func productionProbeSavedProfile(baseURL string) map[string]any {
	profile := productionProbeTypedProfile(baseURL, "")
	profile["apiKey"] = ""
	profile["hasCredential"] = true
	return profile
}

// productionProbeModelsJSON is the private models.json the browser never sees,
// holding the plaintext credential the committed lane restores.
func productionProbeModelsJSON(t *testing.T, baseURL, key string) string {
	t.Helper()
	records, err := json.Marshal([]any{map[string]any{
		"id":          productionTestModelID,
		"name":        "Saved story model",
		"provider":    "custom",
		"protocol":    "openai",
		"modelName":   "ci-story-model",
		"baseUrl":     baseURL,
		"apiKey":      key,
		"temperature": 0.7,
		"maxTokens":   4096,
		"purposes":    []any{"generation"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return string(records)
}

type productionProbeTestWire struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Status  int    `json:"status"`
}

type productionProbeDiscoveredWire struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Value string `json:"value"`
	// Capabilities is decoded loosely because every field is optional.
	Capabilities map[string]any `json:"capabilities"`
}

type productionProbeDiscoveryWire struct {
	Success   bool                            `json:"success"`
	ErrorCode string                          `json:"errorCode"`
	Models    []productionProbeDiscoveredWire `json:"models"`
}

// TestProductionLLMProbeTestsAWorkingConnection is the settings screen's happy
// path: one chat request to the configured endpoint, bearing the credential, and
// the renderer's own success shape back.
func TestProductionLLMProbeTestsAWorkingConnection(t *testing.T) {
	provider, recorder := productionProbeFakeProvider(t, http.StatusOK,
		`{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL}, "")
	var wire productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{productionProbeTypedProfile(provider.URL+"/v1", productionTestModelSecret)}, &wire)
	if !wire.Success {
		t.Fatalf("a working endpoint was reported as a failure: %v", wire.Error)
	}
	if recorder.count() != 1 {
		t.Fatalf("the probe dispatched %v requests, want exactly one", recorder.count())
	}
	if path := recorder.lastPath(); path != "POST /v1/chat/completions" {
		t.Fatalf("the probe dialed %v", path)
	}
	if auth := recorder.lastAuth(); auth != "Bearer "+productionTestModelSecret {
		t.Fatal("the probe did not send the credential as a bearer token")
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(recorder.lastBody()), &body); err != nil {
		t.Fatalf("the probe sent a body that is not JSON: %v", err)
	}
	if body["model"] != "ci-story-model" {
		t.Fatalf("the probe named the model %v", body["model"])
	}
	if body["max_tokens"] != float64(productionProbeMaxTokens) {
		t.Fatalf("the probe asked for %v tokens, want %v", body["max_tokens"], productionProbeMaxTokens)
	}
	if _, present := body["stream"]; present && body["stream"] != false {
		t.Fatal("the probe asked for a stream, which this lane cannot read")
	}
}

// TestProductionLLMProbeReportsTheProviderStatusWhenTheCredentialIsRefused keeps
// "the provider answered and said no" apart from "we never reached the provider".
// The status is the provider's own; the body it sent is not repeated, because a
// provider that echoes the submitted key back would then be published.
func TestProductionLLMProbeReportsTheProviderStatusWhenTheCredentialIsRefused(t *testing.T) {
	provider, recorder := productionProbeFakeProvider(t, http.StatusUnauthorized,
		`{"error":{"message":"Incorrect API key provided: `+productionTestModelSecret+`","type":"invalid_request_error"}}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL}, "")
	var wire productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{productionProbeTypedProfile(provider.URL+"/v1", productionTestModelSecret)}, &wire)
	if recorder.count() != 1 {
		t.Fatalf("the probe dispatched %v requests, want exactly one", recorder.count())
	}
	if wire.Success {
		t.Fatal("a refused credential was reported as a working connection")
	}
	if wire.Error != productionProbeRefusedMessage(http.StatusUnauthorized) {
		t.Fatalf("the refusal read %v", wire.Error)
	}
	if wire.Status != http.StatusUnauthorized {
		t.Fatalf("the provider status was reported as %v, want %v", wire.Status, http.StatusUnauthorized)
	}
	if !strings.Contains(wire.Error, "401") {
		t.Fatal("the refusal does not name the provider's status")
	}
	if wire.Error == productionProbeNotReachedMessage {
		t.Fatal("a real provider answer was reported as an unreachable host")
	}
	if strings.Contains(wire.Error, productionTestModelSecret) || strings.Contains(wire.Error, "Incorrect API key provided") {
		t.Fatal("the refusal repeated the provider body or the credential")
	}
}

// TestProductionLLMProbeReportsAnUnreachableProvider is the other half of that
// distinction: nothing answered, so no status exists to report.
func TestProductionLLMProbeReportsAnUnreachableProvider(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	fixture := newProductionProbeFixture(t, []string{origin}, "")
	var wire productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{productionProbeTypedProfile(origin+"/v1", productionTestModelSecret)}, &wire)
	if wire.Success {
		t.Fatal("an unreachable provider was reported as a working connection")
	}
	if wire.Error != productionProbeNotReachedMessage {
		t.Fatalf("the failure read %v", wire.Error)
	}
	if wire.Status != 0 {
		t.Fatalf("the failure reported HTTP status %v although no response arrived", wire.Status)
	}
	if strings.Contains(wire.Error, productionTestModelSecret) {
		t.Fatal("the failure message carried the credential")
	}
}

// TestProductionLLMProbeRefusesAnOriginOutsideTheCommittedAllowlist proves both
// names sit behind the same wall the generation executor dispatches through: the
// probe refuses, the foreign endpoint sees nothing, and the policy refuses the very
// same URL when it is dispatched directly.
func TestProductionLLMProbeRefusesAnOriginOutsideTheCommittedAllowlist(t *testing.T) {
	declared, declaredRecorder := productionProbeFakeProvider(t, http.StatusOK, `{"choices":[]}`)
	outside, outsideRecorder := productionProbeFakeProvider(t, http.StatusOK,
		`{"object":"list","data":[{"id":"smuggled-model"}]}`)
	fixture := newProductionProbeFixture(t, []string{declared.URL}, "")
	var testWire productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{productionProbeTypedProfile(outside.URL+"/v1", productionTestModelSecret)}, &testWire)
	if testWire.Success {
		t.Fatal("an origin outside the committed allowlist was probed")
	}
	if testWire.Error != productionProbeOriginRefusedMessage(egress.ErrNotAllowed) {
		t.Fatalf("the refusal read %v", testWire.Error)
	}
	var discoveryWire productionProbeDiscoveryWire
	fixture.client.decode(t, "llm:discover-models", []any{productionProbeTypedProfile(outside.URL+"/v1", productionTestModelSecret)}, &discoveryWire)
	if discoveryWire.Success || discoveryWire.ErrorCode != "unsupported" {
		t.Fatalf("discovery from an outside origin answered %v / %v", discoveryWire.Success, discoveryWire.ErrorCode)
	}
	if outsideRecorder.count() != 0 {
		t.Fatalf("the foreign endpoint received %v requests", outsideRecorder.count())
	}
	if declaredRecorder.count() != 0 {
		t.Fatalf("the declared endpoint received %v requests", declaredRecorder.count())
	}
	request, err := http.NewRequest(http.MethodGet, outside.URL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.policy.Do(request); !errors.Is(err, egress.ErrNotAllowed) {
		t.Fatalf("the policy the executor dispatches through answered %v", err)
	}
	if outsideRecorder.count() != 0 {
		t.Fatal("the policy reached the foreign endpoint")
	}
}

// TestProductionLLMProbeDiscoversTheProvidersModelList is the discover happy path:
// three published models arrive in the renderer's DiscoveredModel shape.
func TestProductionLLMProbeDiscoversTheProvidersModelList(t *testing.T) {
	provider, recorder := productionProbeFakeProvider(t, http.StatusOK,
		`{"object":"list","data":[`+
			`{"id":"novel-pro","name":"Novel Pro","context_window":65536,"max_output_tokens":8192,"capabilities":{"reasoning":true}},`+
			`{"id":"novel-lite"},`+
			`{"id":"novel-embed","name":"Novel Embed"}]}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL}, "")
	var wire productionProbeDiscoveryWire
	fixture.client.decode(t, "llm:discover-models", []any{productionProbeTypedProfile(provider.URL+"/v1", productionTestModelSecret)}, &wire)
	if !wire.Success {
		t.Fatalf("a published model list was refused: %v", wire.ErrorCode)
	}
	if len(wire.Models) != 3 {
		t.Fatalf("the probe returned %v models, want three", len(wire.Models))
	}
	if wire.Models[0].ID != "novel-pro" || wire.Models[0].Value != "novel-pro" || wire.Models[0].Name != "Novel Pro" {
		t.Fatalf("the first model arrived as %v", wire.Models[0])
	}
	if wire.Models[0].Capabilities["contextWindowTokens"] != float64(65536) {
		t.Fatalf("the declared context window arrived as %v", wire.Models[0].Capabilities["contextWindowTokens"])
	}
	if wire.Models[0].Capabilities["maxOutputTokens"] != float64(8192) {
		t.Fatalf("the declared output limit arrived as %v", wire.Models[0].Capabilities["maxOutputTokens"])
	}
	if wire.Models[0].Capabilities["reasoning"] != true {
		t.Fatal("the declared reasoning flag was dropped")
	}
	if wire.Models[1].Name != "novel-lite" {
		t.Fatalf("a model without a display name arrived as %v", wire.Models[1].Name)
	}
	if wire.Models[2].ID != "novel-embed" {
		t.Fatalf("the third model arrived as %v", wire.Models[2])
	}
	if path := recorder.lastPath(); path != "GET /v1/models" {
		t.Fatalf("discovery dialed %v", path)
	}
	if auth := recorder.lastAuth(); auth != "Bearer "+productionTestModelSecret {
		t.Fatal("discovery did not send the credential as a bearer token")
	}
}

// TestProductionLLMProbeRefusesToInventAModelForAnEmptyList: an endpoint that
// publishes nothing is no_models, and the answer carries no list at all.
func TestProductionLLMProbeRefusesToInventAModelForAnEmptyList(t *testing.T) {
	provider, recorder := productionProbeFakeProvider(t, http.StatusOK, `{"object":"list","data":[]}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL}, "")
	response := fixture.client.call(t, "llm:discover-models", []any{productionProbeTypedProfile(provider.URL+"/v1", productionTestModelSecret)})
	if response.Code != http.StatusOK {
		t.Fatalf("an empty model list answered HTTP %v", response.Code)
	}
	if recorder.count() != 1 {
		t.Fatalf("discovery dispatched %v requests, want exactly one", recorder.count())
	}
	var envelope struct {
		Result productionProbeDiscoveryWire `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("the answer is not JSON: %v", err)
	}
	if envelope.Result.Success {
		t.Fatal("an empty model list was reported as success")
	}
	if envelope.Result.ErrorCode != "no_models" {
		t.Fatalf("an empty model list answered %v", envelope.Result.ErrorCode)
	}
	if strings.Contains(response.Body.String(), `"models"`) {
		t.Fatalf("an empty model list still carried a models key: %v", envelope.Result.ErrorCode)
	}
}

// TestProductionLLMProbeAnswersWithoutAGenerationLease is the lease statement as a
// test: both names answer for an admitted tab that holds no lease, while the name
// that does require one stays unbound on the very same server.
func TestProductionLLMProbeAnswersWithoutAGenerationLease(t *testing.T) {
	provider, recorder := productionProbeFakeProvider(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	discovery, _ := productionProbeFakeProvider(t, http.StatusOK, `{"object":"list","data":[{"id":"novel-pro"}]}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL, discovery.URL}, "")
	// The settings form never sends a lease, a project session or a run epoch: the
	// probe acts on configuration, not on a generation run
	// (internal/generationowner/lease.go:145-146).
	profile := productionProbeTypedProfile(provider.URL+"/v1", productionTestModelSecret)
	if _, present := profile["modelExecutionLeaseId"]; present {
		t.Fatal("the fixture invented a lease")
	}
	var testWire productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{profile}, &testWire)
	if !testWire.Success {
		t.Fatalf("llm:test-connection refused a caller without a lease: %v", testWire.Error)
	}
	var discoveryWire productionProbeDiscoveryWire
	fixture.client.decode(t, "llm:discover-models", []any{productionProbeTypedProfile(discovery.URL+"/v1", productionTestModelSecret)}, &discoveryWire)
	if !discoveryWire.Success || len(discoveryWire.Models) != 1 {
		t.Fatalf("llm:discover-models refused a caller without a lease: %v", discoveryWire.ErrorCode)
	}
	if recorder.count() != 1 {
		t.Fatalf("the chat endpoint received %v requests", recorder.count())
	}
	// The sibling name that does require an in-flight lease is still unbound, so a
	// lease-free caller cannot reach a generation by accident.
	refused := fixture.client.call(t, "generation:execute", []any{})
	if refused.Code != http.StatusNotImplemented {
		t.Fatalf("generation:execute answered HTTP %v although it is unbound", refused.Code)
	}
	if !strings.Contains(refused.Body.String(), "GO_OPERATION_NOT_MIGRATED") {
		t.Fatalf("generation:execute did not report its unbound code: %v", refused.Body.String())
	}
}

// TestProductionLLMProbeRestoresAStoredCredentialOnlyForItsOwnEndpoint is the
// browser's saved-model path: the key is restored for the endpoint it was saved
// with, and refused the moment the profile's endpoint moves.
func TestProductionLLMProbeRestoresAStoredCredentialOnlyForItsOwnEndpoint(t *testing.T) {
	provider, recorder := productionProbeFakeProvider(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL, "https://api.example.invalid"}, productionProbeModelsJSON(t, provider.URL+"/v1", productionTestModelSecret))
	var restored productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{productionProbeSavedProfile(provider.URL+"/v1")}, &restored)
	if !restored.Success {
		t.Fatalf("the saved credential was not restored for its own endpoint: %v", restored.Error)
	}
	if auth := recorder.lastAuth(); auth != "Bearer "+productionTestModelSecret {
		t.Fatal("the restored credential was not sent as a bearer token")
	}
	if recorder.count() != 1 {
		t.Fatalf("the provider received %v requests", recorder.count())
	}
	// A profile that denies it has a credential must not get the stored one
	// silently, even for the same endpoint. The endpoint here is a remote name, so
	// no local-endpoint exemption applies: an endpoint that needs a key and has none
	// is refused before anything is dialed.
	denied := productionProbeSavedProfile("https://api.example.invalid/v1")
	denied["hasCredential"] = false
	var missing productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{denied}, &missing)
	if missing.Success || missing.Error != productionProbeCredentialRequiredMessage {
		t.Fatalf("a profile with no credential and no stored key answered %v / %v", missing.Success, missing.Error)
	}
	// The same saved model, pointed at another endpoint on the same allowed origin:
	// the stored key belongs to the old endpoint and is not reused.
	moved := productionProbeSavedProfile(provider.URL + "/v2")
	var changed productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{moved}, &changed)
	if changed.Success || changed.Error != productionProbeCredentialChangedMessage {
		t.Fatalf("a moved endpoint reused the stored key: %v / %v", changed.Success, changed.Error)
	}
	// A moved endpoint that supplies its own key is served, which is the rule the
	// settings form states (src/components/settings/ModelForm.tsx:246-268).
	var supplied productionProbeTestWire
	fixture.client.decode(t, "llm:test-connection", []any{productionProbeTypedProfile(provider.URL+"/v2", productionTestModelSecret)}, &supplied)
	if !supplied.Success {
		t.Fatalf("a moved endpoint with its own key was refused: %v", supplied.Error)
	}
}

// TestProductionLLMProbeDiscoveryNamesAnUncommittedEndpoint holds the settings
// form's ability to tell "save this model first" apart from "this endpoint
// publishes no list". An endpoint outside the committed egress cannot be dialed
// yet, and answering "unsupported" told the author the endpoint had no model
// list at all - which is exactly what happened when adding a vector model.
func TestProductionLLMProbeDiscoveryNamesAnUncommittedEndpoint(t *testing.T) {
	for _, purpose := range []string{"generation", "embedding"} {
		provider, _ := productionProbeFakeProvider(t, http.StatusOK,
			`{"object":"list","data":[{"id":"synthetic-model","object":"model"}]}`)
		fixture := newProductionProbeFixture(t, []string{provider.URL}, "")
		profile := productionProbeTypedProfile("https://uncommitted.example/v1", productionTestModelSecret)
		profile["purposes"] = []any{purpose}
		var wire productionProbeDiscoveryWire
		fixture.client.decode(t, "llm:discover-models", []any{profile}, &wire)
		if wire.Success || wire.ErrorCode != "endpoint_not_allowed" {
			t.Fatalf("%s: an uncommitted endpoint answered success=%v errorCode=%q", purpose, wire.Success, wire.ErrorCode)
		}
	}
}

// TestProductionLLMProbeDiscoversWithoutAModelName holds the browser contract: a
// ModelDiscoveryRequest is provider/protocol/baseUrl/apiKey/purposes and carries
// no model name (src/shared/ipc-channels.ts). Demanding one refused every real
// discovery with an unreadable-list answer, whatever the endpoint.
func TestProductionLLMProbeDiscoversWithoutAModelName(t *testing.T) {
	provider, _ := productionProbeFakeProvider(t, http.StatusOK,
		`{"object":"list","data":[{"id":"synthetic-model","object":"model"}]}`)
	fixture := newProductionProbeFixture(t, []string{provider.URL}, "")
	// Exactly the fields the settings form sends: no modelName.
	request := map[string]any{
		"provider": "openai",
		"protocol": "openai",
		"baseUrl":  provider.URL + "/v1",
		"apiKey":   productionTestModelSecret,
		"purposes": []any{"generation"},
	}
	var wire productionProbeDiscoveryWire
	fixture.client.decode(t, "llm:discover-models", []any{request}, &wire)
	if !wire.Success {
		t.Fatalf("a discovery without a model name was refused: errorCode=%q", wire.ErrorCode)
	}
	if len(wire.Models) != 1 || wire.Models[0].ID != "synthetic-model" {
		t.Fatalf("a discovery without a model name returned %+v", wire.Models)
	}
}

// TestProductionLLMProbeReportsAnUnreachableEndpointAsNetwork holds the error-code
// line the settings form depends on: a refused connection is a network failure,
// never an unreadable model list. Reporting a refused connection as
// invalid_response blamed the provider's list and sent the author looking in the
// wrong place.
func TestProductionLLMProbeReportsAnUnreachableEndpointAsNetwork(t *testing.T) {
	// Port 1 is closed on every host. The origin is committed, so the policy
	// admits it and the only possible failure is the transport itself.
	const unreachable = "http://127.0.0.1:1"
	fixture := newProductionProbeFixture(t, []string{unreachable}, "")
	var wire productionProbeDiscoveryWire
	fixture.client.decode(t, "llm:discover-models",
		[]any{productionProbeTypedProfile(unreachable+"/v1", productionTestModelSecret)}, &wire)
	if wire.Success {
		t.Fatal("an unreachable endpoint reported a successful model list")
	}
	if wire.ErrorCode != "network" {
		t.Fatalf("an unreachable endpoint answered %q; want network", wire.ErrorCode)
	}
}

// TestProductionLLMProbeKeepsTheCredentialOutOfEveryRefusal holds the credential
// containment line: a provider that echoes the submitted key, a provider whose
// model list echoes it, and a discovery refusal all stay clean.
func TestProductionLLMProbeKeepsTheCredentialOutOfEveryRefusal(t *testing.T) {
	echoing, _ := productionProbeFakeProvider(t, http.StatusUnauthorized,
		`{"error":{"message":"bad key `+productionTestModelSecret+`"}}`)
	tainted, _ := productionProbeFakeProvider(t, http.StatusOK,
		`{"object":"list","data":[{"id":"`+productionTestModelSecret+`"}]}`)
	fixture := newProductionProbeFixture(t, []string{echoing.URL, tainted.URL}, "")
	rejected := fixture.client.call(t, "llm:test-connection", []any{productionProbeTypedProfile(echoing.URL+"/v1", productionTestModelSecret)})
	if rejected.Code != http.StatusOK {
		t.Fatalf("a refused credential answered HTTP %v", rejected.Code)
	}
	if strings.Contains(rejected.Body.String(), productionTestModelSecret) {
		t.Fatal("the connection-test answer carried the credential the provider echoed")
	}
	var taintedWire productionProbeDiscoveryWire
	fixture.client.decode(t, "llm:discover-models", []any{productionProbeTypedProfile(tainted.URL+"/v1", productionTestModelSecret)}, &taintedWire)
	if taintedWire.Success {
		t.Fatal("a model list that echoed the credential was accepted")
	}
	if taintedWire.ErrorCode != "invalid_response" {
		t.Fatalf("a credential-tainted model list answered %v", taintedWire.ErrorCode)
	}
	answer := fixture.client.call(t, "llm:discover-models", []any{productionProbeTypedProfile(tainted.URL+"/v1", productionTestModelSecret)})
	if strings.Contains(answer.Body.String(), productionTestModelSecret) {
		t.Fatal("the discovery answer carried the credential")
	}
	// The stored lane is the other place a key could escape: a saved model with no
	// form key must still keep its restored credential off the wire shape.
	stored := newProductionProbeFixture(t, []string{tainted.URL}, productionProbeModelsJSON(t, tainted.URL+"/v1", productionTestModelSecret))
	restored := stored.client.call(t, "llm:discover-models", []any{productionProbeSavedProfile(tainted.URL + "/v1")})
	if restored.Code != http.StatusOK {
		t.Fatalf("a stored-credential discovery answered HTTP %v", restored.Code)
	}
	if strings.Contains(restored.Body.String(), productionTestModelSecret) {
		t.Fatal("the stored-credential discovery answer carried the credential")
	}
}

// TestProductionLLMProbeStopsAtItsDeadline proves the probe owns a timeout: a
// provider that never answers is abandoned at the budget the renderer uses for a
// local endpoint, and the failure names the deadline rather than an endpoint.
func TestProductionLLMProbeStopsAtItsDeadline(t *testing.T) {
	silent, recorder := productionProbeSilentProvider(t)
	fixture := newProductionProbeFixture(t, []string{silent.URL}, "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	value, err := fixture.probe.testConnection(ctx, transportCallerForTest(), []any{productionProbeTypedProfile(silent.URL+"/v1", productionTestModelSecret)})
	if err != nil {
		t.Fatalf("a silent provider was reported as a request error: %v", err)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	var wire productionProbeTestWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Success {
		t.Fatal("a silent provider was reported as a working connection")
	}
	// 127.0.0.1 is a local endpoint, so the local budget applies
	// (electron/controllers/llm-controller.ts:51-58).
	if wire.Error != productionProbeTimeoutMessage(productionProbeLocalTimeoutSec) {
		t.Fatalf("the deadline was reported as %v", wire.Error)
	}
	if strings.Contains(wire.Error, productionTestModelSecret) {
		t.Fatal("the deadline message carried the credential")
	}
	if recorder.count() != 1 {
		t.Fatalf("the provider received %v requests", recorder.count())
	}
}

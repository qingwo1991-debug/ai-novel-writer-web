//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/embedding"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

// The two llm names whose absence leaves the settings screen unusable in a
// browser are served here:
//
//   - llm:test-connection sends one tiny request to prove that a key and a base
//     URL work - a chat completion for a generation profile, one embedding of a
//     single fixed word for an embedding profile, exactly as the desktop chooses
//     between the two (electron/controllers/llm-controller.ts:495-521);
//   - llm:discover-models lists the models the endpoint publishes
//     (electron/services/model-discovery-service.ts:183-300).
//
// internal/transport/model_routing.go:19-24 states exactly why they were
// unbound, and the condition it names is a real provider round trip: "llm:
// generate, llm:generate-stream and llm:test-connection only take effect through
// a live provider round trip, llm:discover-models only through an outbound
// /models request ... Binding them would mean inventing a provider." Nothing is
// invented here. The requests are built by llm.BuildOpenAIRequest and
// llm.BuildGeminiRequest, the responses are read with llm's own response types,
// the network client is the committed egress policy the generation executor
// dispatches through, and the stored credential is unsealed through the same
// documented seam the executor uses.
//
// Neither name needs a generation lease. A lease binds a model profile to one
// admitted project run, which this file has no part in: internal/
// generationowner/lease.go:145-146 refuses any Begin whose guard is
// `!scope.valid()||purpose!="generation"`, and a Scope is a project writer,
// session, tab and epoch pair (lease.go:24-26). A probe acts on the author's
// configuration, not on a run, so it carries neither; it refuses nothing a lease
// would have proved, and it grants nothing a lease would have fenced.
//
// Every outbound call passes the same admission the executor uses
// (production_open_linux.go ModelEgress, derived once at startup by
// production_model_egress_linux.go): the policy is fetched per request, the base
// URL is checked against its immutable origin set before anything is built, and
// the dispatch itself goes through egress.Client.Do, which revalidates the
// resolved URL at send time (internal/egress/client.go:33-35 and :116-119). A
// probe can therefore never be a hole in the wall the executor sits behind.

const (
	// productionProbeMaxTokens mirrors CONNECTION_TEST_MAX_TOKENS
	// (electron/controllers/llm-controller.ts:50): reasoning models may spend
	// tokens before they answer, so a tiny budget would report a working endpoint
	// as truncated.
	productionProbeMaxTokens int64 = 1024
	// productionProbeTimeoutSec mirrors CLOUD_CONNECTION_TEST_TIMEOUT_MS and
	// productionProbeLocalTimeoutSec LOCAL_CONNECTION_TEST_TIMEOUT_MS
	// (electron/controllers/llm-controller.ts:52-58): a local model has to load
	// its weights before it answers, a stuck cloud call should be reported early.
	productionProbeTimeoutSec      = 30
	productionProbeLocalTimeoutSec = 120
	// productionProbeDiscoveryTimeoutSec mirrors
	// DEFAULT_MODEL_DISCOVERY_TIMEOUT_MS (model-discovery-service.ts:19).
	productionProbeDiscoveryTimeoutSec = 15
	// productionProbeMaxModels and productionProbeMaxModelText bound one list the
	// way model-discovery-service.ts:20-21 bounds it.
	productionProbeMaxModels    = 500
	productionProbeMaxModelText = 512
	// productionProbeMaxBodyBytes bounds one provider body before it is decoded.
	// llm's own limiter (internal/llm/openai.go:59 and :97) reads one byte past its
	// bound and refuses the oversized reply; a model list is far smaller than a
	// completion, so the same idiom is used with a tighter ceiling here.
	productionProbeMaxBodyBytes int64 = 4 << 20
)

// The refusal vocabulary is fixed text. No committed value, endpoint, path or
// credential is ever formatted into one, and none of them quotes its input.
const (
	productionProbeCredentialRequiredMessage = "No API key is available for this endpoint. Enter the key in the model form, or reopen the saved model without editing its provider, protocol, or Base URL."
	productionProbeCredentialChangedMessage  = "The saved API key belongs to a different provider, protocol, or Base URL, so it was not reused. Enter the key for this endpoint."
	productionProbeConfigMessage             = "The saved model configuration could not be read, so no credential could be restored."
	productionProbeProtocolMessage           = "This deployment can only probe the openai and gemini protocols."
	productionProbeUnavailableMessage        = "The connection probe is unavailable in this deployment."
	productionProbeNotReachedMessage         = "The provider was not reached: the request failed before any HTTP response came back. Check the Base URL, the network, and this deployment's allowed model origins."
	productionProbeCancelledMessage          = "The probe was cancelled before the endpoint answered."
)

var (
	errProductionProbeUnavailable         = errors.New("PRODUCTION_LLM_PROBE_UNAVAILABLE")
	errProductionProbeRequestInvalid      = errors.New("LLM_PROBE_REQUEST_INVALID")
	errProductionProbeCredentialRequired  = errors.New("LLM_CREDENTIAL_REQUIRED")
	errProductionProbeCredentialChanged   = errors.New("CREDENTIAL_ENDPOINT_CHANGED")
	errProductionProbeConfigUnavailable   = errors.New("MODEL_CONFIG_UNAVAILABLE")
	errProductionProbeProtocolUnsupported = errors.New("LLM_PROTOCOL_UNSUPPORTED")
	errProductionProbeResponseTooLarge    = errors.New("LLM_RESPONSE_TOO_LARGE")
	errProductionProbeNotReached          = errors.New("LLM_PROVIDER_UNREACHABLE")
)

// productionLLMProbe serves llm:test-connection and llm:discover-models over the
// committed global config the lane pinned and the committed model-egress policy
// it derived at startup. It owns no lease, no project database and no author
// content: both names read one configuration, dial one endpoint and return a
// reduced answer.
type productionLLMProbe struct {
	config  *globalconfig.Service
	startup *globalconfig.Startup
	// policy is productionOpen.ModelEgress in a real deployment: the committed
	// policy, or a symbolic refusal when the committed config named no origin.
	policy func() (*egress.Client, error)
	// credentials is the credential seam the generation executor resolves through
	// (internal/generationowner/dispatch.go:43). models.json keeps apiKey
	// plaintext, which generationowner.PlaintextCredential documents as the
	// legacy lane's seam (dispatch.go:21-31); *secrets.Storage satisfies the same
	// interface for the AES-GCM lane, so a future sealed store swaps nothing else.
	credentials generationowner.CredentialStorage
	// register is the server's public response sanitizer, captured at Bind time.
	// internal/transport/public.go:10-12 requires a secret to be registered before
	// it is admitted or used, so no diagnostic this lane emits can carry it.
	register func(string) (bool, error)
}

// newProductionLLMProbe builds the two probes over an admitted lane. A nil lane
// yields a nil probe, which Bind refuses, so both names keep failing closed at
// GO_OPERATION_NOT_MIGRATED instead of becoming handlers that could only refuse.
func newProductionLLMProbe(open *productionOpen) *productionLLMProbe {
	if open == nil {
		return nil
	}
	return newProductionLLMProbeOver(open.config, open.startup, open.ModelEgress)
}

// newProductionLLMProbeOver is the same assembly from explicit parts, so a test
// can pin one committed config root and the policy derived from it without
// opening an author database.
func newProductionLLMProbeOver(
	config *globalconfig.Service,
	startup *globalconfig.Startup,
	policy func() (*egress.Client, error),
) *productionLLMProbe {
	return &productionLLMProbe{
		config:      config,
		startup:     startup,
		policy:      policy,
		credentials: generationowner.PlaintextCredential{},
	}
}

// Bind installs the two names. It is called by the lane before serving, exactly
// like the other production binders, so the captured sanitizer registrar is the
// server's own and cannot be replaced by a request.
func (p *productionLLMProbe) Bind(server *transport.Server) error {
	if p == nil || server == nil || p.config == nil || p.startup == nil || p.policy == nil || p.credentials == nil {
		return errProductionProbeUnavailable
	}
	p.register = server.RegisterSecret
	for _, binding := range []struct {
		name    string
		handler transport.Operation
	}{
		{"llm:test-connection", p.testConnection},
		{"llm:discover-models", p.discoverModels},
	} {
		if err := server.Bind(binding.name, binding.handler); err != nil {
			return err
		}
	}
	return nil
}

// productionProbeForm is the endpoint part of one probe request. It is exactly
// the endpoint the settings form currently shows, which the browser may not have
// saved yet (internal/transport/model_routing.go:19-24 is why an unsaved form can
// only be served by a real round trip).
type productionProbeForm struct {
	provider  string
	protocol  string
	baseURL   string
	modelName string
	apiKey    string
	modelID   string
	saved     bool
	purposes  []string
}

// testConnection answers llm:test-connection for the frozen browser session. The
// three outcomes the author must be able to tell apart are kept apart:
//
//   - the provider answered and refused the credential: a 200 business result
//     carrying the provider's own HTTP status;
//   - the provider was never reached: a 200 business result with the transport
//     sentence, never a status the provider never sent;
//   - the endpoint is outside the committed allowlist: the policy's own symbolic
//     refusal, which is what generation:execute would also report.
func (p *productionLLMProbe) testConnection(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) < 1 || len(args) > 2 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	// The original passes the project's creative strategy as the second argument
	// (electron/controllers/llm-controller.ts:486-490). A probe needs no
	// generation parameter, so it is read as arity only and never as input.
	request, ok := args[0].(map[string]any)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	form, key, err := p.admit(ctx, caller, request)
	if err != nil {
		return productionProbeTestFailure(err)
	}
	if strings.TrimSpace(form.modelName) == "" {
		// The connection test builds a chat body, so it is the one caller that
		// cannot proceed without a model name.
		return productionProbeTestFailure(errProductionProbeRequestInvalid)
	}
	if productionProbePurpose(form.purposes) == "embedding" {
		// An embedding profile is probed with the lane's own embedding dispatch,
		// over the same committed egress client and the same deadline, exactly as
		// the desktop switches between the two paths
		// (electron/controllers/llm-controller.ts:495-499). The chat body below is
		// never built for it.
		return p.testEmbeddingConnection(ctx, form, key)
	}
	client, err := p.egress()
	if err != nil {
		return productionProbeTestFailure(err)
	}
	if err := client.CheckBaseURL(form.baseURL); err != nil {
		return productionProbeTestFailure(productionProbeRefusedOrigin(err))
	}
	budget := productionProbeDeadline(form)
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	// A dispatch failure is captured as well as returned: llm.Generate collapses
	// every transport error into LLM_REQUEST_FAILED, and the difference between a
	// refused origin, a refused redirect and an unreachable host is exactly what
	// the author needs to see.
	var dispatchErr error
	dispatch := func(request *http.Request) (*http.Response, error) {
		response, err := client.Do(request)
		if err != nil {
			dispatchErr = err
		}
		return response, err
	}
	model := llm.Model{
		Provider:  form.provider,
		ModelName: form.modelName,
		BaseURL:   form.baseURL,
		APIKey:    key,
		MaxTokens: productionProbeMaxTokens,
	}
	messages := []llm.Message{{Role: "user", Content: `Say "hello" and nothing else.`}}
	maxTokens := productionProbeMaxTokens
	options := llm.Options{MaxTokens: &maxTokens}
	var result llm.Response
	switch form.protocol {
	case "openai":
		result = (&llm.OpenAI{Dispatch: dispatch}).Generate(deadline, model, messages, options)
	case "gemini":
		result = (&llm.Gemini{Dispatch: dispatch}).Generate(deadline, model, messages, options)
	default:
		// admit already refused every other protocol; this arm keeps the switch
		// total so a future protocol cannot dispatch by accident.
		return productionProbeTestFailure(errProductionProbeProtocolUnsupported)
	}
	// A reply that arrived is a working connection even if the deadline fired
	// while it was being read, so success is settled before the deadline is.
	if result.Success {
		return map[string]any{"success": true}, nil
	}
	if deadline.Err() != nil {
		return productionProbeTestFailure(productionProbeDeadlineError(deadline, budget))
	}
	if dispatchErr != nil {
		return productionProbeTestFailure(productionProbeDispatchFailure(dispatchErr))
	}
	if result.HTTPFailure != nil {
		// A real provider answer: report its status, never its body. The body may
		// quote the credential back (some providers echo the submitted key), so it
		// stays inside this function.
		return productionProbeTestFailure(productionProbeRefusalError(result.HTTPFailure.Status))
	}
	return productionProbeTestFailure(productionProbeUnusableError(result))
}

// testEmbeddingConnection probes one embedding-purpose profile. The desktop
// answers the very same name by calling generateEmbeddings(['hello'], ...) for a
// profile whose purposes include 'embedding'
// (electron/controllers/llm-controller.ts:495-499), so this is the same round
// trip through the lane's committed embedding dispatch: the same egress client
// the chat path dispatches through, the same deadline, and the same outcome
// sentences. Only the request differs - one fixed word instead of a chat body.
func (p *productionLLMProbe) testEmbeddingConnection(ctx context.Context, form productionProbeForm, key string) (any, error) {
	protocol, ok := productionKnowledgeEmbeddingProtocol(form.protocol)
	if !ok {
		// admit already refused every other protocol; this arm keeps the branch
		// total so a future protocol cannot dispatch by accident.
		return productionProbeTestFailure(errProductionProbeProtocolUnsupported)
	}
	client, err := p.egress()
	if err != nil {
		return productionProbeTestFailure(err)
	}
	if err := client.CheckBaseURL(form.baseURL); err != nil {
		return productionProbeTestFailure(productionProbeRefusedOrigin(err))
	}
	budget := productionProbeDeadline(form)
	deadline, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	target := productionKnowledgeEmbedding{
		Protocol: protocol,
		Model: embedding.Model{
			BaseURL:   form.baseURL,
			APIKey:    key,
			ModelName: form.modelName,
		},
	}
	// The vector lane's own dispatch: it embeds the fixed probe word through the
	// passed-in egress client and returns no partial result on any failure.
	if _, err := productionKnowledgeEmbeddingDispatch(deadline, client, target, []string{"hello"}); err != nil {
		return productionProbeTestFailure(productionProbeEmbeddingFailure(deadline, budget, err))
	}
	return map[string]any{"success": true}, nil
}

// productionProbeEmbeddingFailure classifies one embedding round trip with the
// same vocabulary the chat path answers with, so the settings screen reads one
// story whichever kind of profile it probes. The lane's embedding client keeps
// the provider's own status on a non-2xx reply
// (internal/embedding/provider.go HTTPError) while collapsing every transport
// failure into a statusless fixed symbol, which is exactly what keeps "the
// provider answered and refused the key" apart from "the provider was never
// reached".
func productionProbeEmbeddingFailure(deadline context.Context, budget time.Duration, err error) error {
	// A deadline that fired is a timeout, never a cancellation - whichever of the
	// two bounds fired. The embedding client owns a per-request bound
	// (internal/embedding requestTimeout) tighter than a local endpoint's probe
	// budget, so its own deadline can expire while this probe's context is still
	// live; productionProbeDeadlineError would then read that live context and
	// answer "cancelled", which is the one thing that did not happen.
	if errors.Is(err, context.DeadlineExceeded) {
		return &productionProbeText{text: productionProbeTimeoutMessage(int(budget / time.Second))}
	}
	if errors.Is(err, context.Canceled) || deadline.Err() != nil {
		return productionProbeDeadlineError(deadline, budget)
	}
	if productionProbeEgressRefusal(err) {
		return productionProbeDispatchFailure(err)
	}
	if status, ok := embedding.HTTPStatus(err); ok {
		// A real provider answer: the status is published, the body is not. Some
		// providers echo the submitted key in an error body.
		return productionProbeRefusalError(status)
	}
	if errors.Is(err, embedding.ErrRequestFailed) || errors.Is(err, egress.ErrRequestFailed) {
		// No HTTP response ever arrived, so no provider status exists to report.
		return errProductionProbeNotReached
	}
	for _, known := range []error{
		embedding.ErrResponseInvalid,
		embedding.ErrTooLarge,
		embedding.ErrRequestInvalid,
		embedding.ErrOllamaNativeURL,
	} {
		if errors.Is(err, known) {
			// The provider answered, but the reply or the resolved endpoint was
			// unusable. The symbolic code is the only detail published.
			return &productionProbeText{text: productionProbeUnusableMessage(known.Error())}
		}
	}
	return errProductionProbeUnavailable
}

// discoverModels answers llm:discover-models. It returns the provider's list in
// the DiscoveredModel shape the renderer reads
// (src/shared/ipc-channels.ts:510-531) and refuses to invent an entry when the
// provider lists nothing.
func (p *productionLLMProbe) discoverModels(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 1 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	request, ok := args[0].(map[string]any)
	if !ok {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	form, key, err := p.admit(ctx, caller, request)
	if err != nil {
		return productionProbeDiscoveryFailure(err)
	}
	if form.protocol != "openai" {
		// The OpenAI-compatible branch is the only one internal/llm can resolve a
		// list URL for: ResolveOpenAIURL is the single endpoint resolver the
		// package exposes. Gemini builds its chat URL inline in
		// BuildGeminiRequest and publishes no list resolver, so a Gemini list here
		// would be an invented provider route rather than assembly.
		return productionProbeDiscoveryFailure(errProductionProbeProtocolUnsupported)
	}
	client, err := p.egress()
	if err != nil {
		return productionProbeDiscoveryFailure(err)
	}
	if err := client.CheckBaseURL(form.baseURL); err != nil {
		// The committed policy refuses an origin it was never told to admit, a Base
		// URL it cannot parse, a policy it cannot build and a redirect it will not
		// follow. None of those says the endpoint has no model list: saving this
		// model commits the endpoint and rebuilds the allowed origins, after which
		// the same list request is served. productionProbeRefusedOrigin keeps each
		// fixed egress code and productionProbeDiscoveryFailure answers the whole
		// family with endpoint_not_allowed. Answering "unsupported" here told the
		// author the endpoint had no model list at all - which is exactly what
		// happened when adding a vector model at a new endpoint.
		return productionProbeDiscoveryFailure(productionProbeRefusedOrigin(err))
	}
	endpoint, ok := productionModelsEndpoint(form.baseURL, form.provider)
	if !ok {
		return productionProbeDiscoveryFailure(errProductionProbeRequestInvalid)
	}
	deadline, cancel := context.WithTimeout(ctx, productionProbeDiscoveryTimeoutSec*time.Second)
	defer cancel()
	listRequest, err := http.NewRequestWithContext(deadline, http.MethodGet, endpoint, nil)
	if err != nil {
		return productionProbeDiscoveryFailure(errProductionProbeRequestInvalid)
	}
	listRequest.Header.Set("Accept", "application/json")
	listRequest.Header.Set("Authorization", "Bearer "+key)
	response, err := client.Do(listRequest)
	if err != nil {
		if deadline.Err() != nil {
			return map[string]any{"success": false, "errorCode": "network"}, nil
		}
		// The dispatcher's own refusals are policy answers. Every other failure to
		// reach the endpoint is a transport failure, not an unreadable body: a
		// refused connection reported as "invalid_response" blames the provider's
		// model list and sends the author looking in the wrong place.
		if failure := productionProbeDispatchFailure(err); productionProbeEgressRefusal(failure) {
			return productionProbeDiscoveryFailure(failure)
		}
		return map[string]any{"success": false, "errorCode": "network"}, nil
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return map[string]any{"success": false, "errorCode": productionProbeDiscoveryStatus(response.StatusCode)}, nil
	}
	body, err := productionProbeBody(response.Body)
	if err != nil {
		if deadline.Err() != nil {
			return map[string]any{"success": false, "errorCode": "network"}, nil
		}
		return map[string]any{"success": false, "errorCode": "invalid_response"}, nil
	}
	var payload any
	if json.Unmarshal(body, &payload) != nil {
		return map[string]any{"success": false, "errorCode": "invalid_response"}, nil
	}
	models, ok := productionDiscoveredModels(payload, key)
	if !ok {
		return map[string]any{"success": false, "errorCode": "invalid_response"}, nil
	}
	if len(models) == 0 {
		// The endpoint answered with a list of nothing. Reporting success with an
		// empty array would invite the renderer to fall back to a guessed name, so
		// the answer is no_models and carries no models key at all.
		return map[string]any{"success": false, "errorCode": "no_models"}, nil
	}
	if productionProbePurpose(form.purposes) == "embedding" {
		models = productionEmbeddingFirst(models)
	}
	return map[string]any{"success": true, "models": models}, nil
}

// admit performs the gates both names share: arity-independent request parsing,
// the global-root readiness gate the sibling llm reads use, credential
// resolution and the sanitizer registration that must precede it.
func (p *productionLLMProbe) admit(ctx context.Context, caller transport.Caller, request map[string]any) (productionProbeForm, string, error) {
	if p == nil || p.config == nil || p.startup == nil || p.policy == nil || p.credentials == nil || p.register == nil {
		return productionProbeForm{}, "", errProductionProbeUnavailable
	}
	if key := productionGlobalCaller(caller); key == "" || p.startup.State(key, true).State != "ready" {
		return productionProbeForm{}, "", security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
	}
	if ctx == nil || ctx.Err() != nil {
		return productionProbeForm{}, "", security.NewHTTPError(http.StatusConflict, "MODEL_CONFIG_CANCELLED")
	}
	form, err := productionProbeFormFrom(request)
	if err != nil {
		return productionProbeForm{}, "", err
	}
	key, err := p.credential(form)
	if err != nil {
		return productionProbeForm{}, "", err
	}
	// internal/transport/public.go:10-12: register before admitting or using a
	// saved or newly submitted credential, including before the first provider
	// request. Capacity failure denies the call instead of evicting a live secret.
	if _, err := p.register(key); err != nil {
		return productionProbeForm{}, "", security.NewHTTPError(http.StatusInternalServerError, "PUBLIC_RESPONSE_SECRET_LIMIT")
	}
	return form, key, nil
}

// productionProbeFormFrom reads the endpoint fields of one profile. json.Number
// never appears here: every field is a string, a boolean or a string list, so a
// numeric field cannot be mistaken for authority.
func productionProbeFormFrom(request map[string]any) (productionProbeForm, error) {
	form := productionProbeForm{}
	var ok bool
	if form.provider, ok = request["provider"].(string); !ok || form.provider == "" {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	if form.protocol, ok = request["protocol"].(string); !ok || form.protocol == "" {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	if form.protocol != "openai" && form.protocol != "gemini" {
		return productionProbeForm{}, errProductionProbeProtocolUnsupported
	}
	if form.baseURL, ok = request["baseUrl"].(string); !ok || strings.TrimSpace(form.baseURL) == "" {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	// modelName is what the tiny chat body names and what a Gemini endpoint puts
	// in its path, so test-connection requires it. llm:discover-models does not:
	// the browser discovery request is provider/protocol/baseUrl/apiKey/purposes
	// with no model name (src/shared/ipc-channels.ts ModelDiscoveryRequest), so
	// demanding one here refused every discovery with an unreadable-list answer.
	if form.modelName, ok = productionProbeOptionalString(request, "modelName"); !ok {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	// A public Web profile redacts apiKey (src/shared/ipc-channels.ts:833), so
	// absent and empty mean the same thing: the form carries no key of its own.
	if form.apiKey, ok = productionProbeOptionalString(request, "apiKey"); !ok {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	if form.modelID, ok = productionProbeOptionalString(request, "id"); !ok {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	if form.saved, ok = productionProbeOptionalBool(request, "hasCredential"); !ok {
		return productionProbeForm{}, errProductionProbeRequestInvalid
	}
	if raw, present := request["purposes"]; present && raw != nil {
		items, ok := raw.([]any)
		if !ok {
			return productionProbeForm{}, errProductionProbeRequestInvalid
		}
		for _, item := range items {
			text, ok := item.(string)
			if !ok {
				return productionProbeForm{}, errProductionProbeRequestInvalid
			}
			form.purposes = append(form.purposes, text)
		}
	}
	return form, nil
}

func productionProbeOptionalString(request map[string]any, key string) (string, bool) {
	raw, present := request[key]
	if !present || raw == nil {
		return "", true
	}
	text, ok := raw.(string)
	return text, ok
}

func productionProbeOptionalBool(request map[string]any, key string) (bool, bool) {
	raw, present := request[key]
	if !present || raw == nil {
		return false, true
	}
	value, ok := raw.(bool)
	return value, ok
}

// credential resolves the one credential this probe may use, in the order the
// shipped settings form prepares it (src/components/settings/ModelForm.tsx:64-78,
// 219-226):
//
//  1. the key the author typed into the form, when the form carries one;
//  2. otherwise the saved profile's key, restored only for the same provider,
//     protocol and endpoint - the same binding globalconfig itself enforces when
//     a save arrives with an empty key (internal/globalconfig/models_linux.go:
//     169-179). A changed endpoint must supply its own key instead;
//  3. otherwise a local endpoint, which needs no key at all
//     (src/shared/provider-presets.ts:484-496);
//  4. otherwise it refuses rather than calling a provider unauthenticated.
//
// The returned string is the transient llm.Model.APIKey: it is registered with the
// public sanitizer by the caller, it is never returned, logged, formatted into a
// message or written to a test failure.
func (p *productionLLMProbe) credential(form productionProbeForm) (string, error) {
	if form.apiKey != "" {
		return form.apiKey, nil
	}
	if form.saved && form.modelID != "" && p.config != nil {
		records, err := p.config.ListPrivateModels()
		if err != nil {
			return "", errProductionProbeConfigUnavailable
		}
		for _, record := range records {
			if id, _ := record["id"].(string); id != form.modelID {
				continue
			}
			if !productionProbeSameEndpoint(record, form) {
				return "", errProductionProbeCredentialChanged
			}
			raw, ok := record["apiKey"].(string)
			if !ok {
				return "", errProductionProbeConfigUnavailable
			}
			key, err := p.credentials.DecryptString([]byte(raw))
			if err != nil {
				return "", errProductionProbeConfigUnavailable
			}
			return key, nil
		}
	}
	if productionProbeLocalEndpoint(form) {
		return "", nil
	}
	return "", errProductionProbeCredentialRequired
}

// productionProbeSameEndpoint is globalconfig's own restore rule
// (internal/globalconfig/models_linux.go:169-179): provider, protocol and the
// base URL with only its trailing slashes removed must all be unchanged.
func productionProbeSameEndpoint(record map[string]any, form productionProbeForm) bool {
	provider, ok := record["provider"].(string)
	if !ok || provider != form.provider {
		return false
	}
	protocol, ok := record["protocol"].(string)
	if !ok || protocol != form.protocol {
		return false
	}
	baseURL, ok := record["baseUrl"].(string)
	if !ok {
		return false
	}
	return strings.TrimRight(baseURL, "/") == strings.TrimRight(form.baseURL, "/")
}

// productionProbeLocalEndpoint mirrors isLocalModelEndpoint
// (src/shared/provider-presets.ts:474-487): Ollama, localhost, 127.0.0.1 and ::1
// need no API key, so an empty credential is a legitimate probe there.
func productionProbeLocalEndpoint(form productionProbeForm) bool {
	if form.provider == "ollama" {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(form.baseURL))
	if err != nil || parsed == nil {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// productionProbeDeadline mirrors connectionTestTimeoutMs
// (electron/controllers/llm-controller.ts:52-58).
func productionProbeDeadline(form productionProbeForm) time.Duration {
	if productionProbeLocalEndpoint(form) {
		return productionProbeLocalTimeoutSec * time.Second
	}
	return productionProbeTimeoutSec * time.Second
}

// egress returns the committed policy, or the lane's symbolic refusal. A lane
// that derived no policy from its committed config refuses here instead of
// dialing, which is the same fence generation:execute reads
// (production_generation_begin_linux.go Execute).
func (p *productionLLMProbe) egress() (*egress.Client, error) {
	if p == nil || p.policy == nil {
		return nil, errProductionProbeUnavailable
	}
	client, err := p.policy()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errProductionModelEgressUnconfigured
	}
	return client, nil
}

// productionProbeRefusedOrigin turns one policy refusal into a symbolic error
// whose text is a fixed egress code, so the caller can format the code without a
// committed URL ever entering the message. All four of egress's refusal codes are
// named, so a redirect refusal keeps its own identity instead of being reported
// under the allowlist's code.
func productionProbeRefusedOrigin(err error) error {
	for _, known := range []error{
		egress.ErrNotAllowed,
		egress.ErrBaseURLInvalid,
		egress.ErrPolicyInvalid,
		egress.ErrRedirectForbidden,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	return egress.ErrNotAllowed
}

// productionProbeEndpointNotAdmitted reports whether err is one of the lane's
// "this endpoint is not admitted yet" answers rather than a statement about the
// provider. The committed policy refuses an origin it was never told to admit, a
// Base URL it cannot parse, a policy it cannot build and a redirect it will not
// follow; a lane that cannot obtain a policy at all admits nothing. Saving this
// model commits the endpoint and rebuilds the allowed origins
// (production_open_linux.go:418), after which the very same request is served, so
// the whole family is the state the settings form calls endpoint_not_allowed.
func productionProbeEndpointNotAdmitted(err error) bool {
	for _, known := range []error{
		egress.ErrNotAllowed,
		egress.ErrBaseURLInvalid,
		egress.ErrPolicyInvalid,
		egress.ErrRedirectForbidden,
		errProductionModelEgressUnconfigured,
		errProductionModelEgressInvalid,
		errProductionModelEgressUnavailable,
		errProductionProbeUnavailable,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

// productionProbeDispatchFailure classifies a dispatch error that came back from
// the policy itself. Every code is fixed: egress.Do never exposes a native
// *url.Error, because that error carries the full URL.
// productionProbeEgressRefusal reports whether err is one of the dispatcher's own
// policy refusals - an answer from this lane - rather than a provider or
// transport fault.
func productionProbeEgressRefusal(err error) bool {
	for _, known := range []error{
		egress.ErrNotAllowed,
		egress.ErrBaseURLInvalid,
		egress.ErrPolicyInvalid,
		egress.ErrRedirectForbidden,
	} {
		if errors.Is(err, known) {
			return true
		}
	}
	return false
}

func productionProbeDispatchFailure(err error) error {
	for _, known := range []error{
		egress.ErrNotAllowed,
		egress.ErrBaseURLInvalid,
		egress.ErrPolicyInvalid,
		egress.ErrRedirectForbidden,
	} {
		if errors.Is(err, known) {
			return known
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return errProductionProbeNotReached
}

// productionModelsEndpoint derives the OpenAI-compatible model list URL from the
// configured chat base URL by reusing llm.ResolveOpenAIURL's own trailing-path
// rules and replacing only the known chat suffix, which is what
// resolveOpenAIModelsUrl does in the renderer's service
// (electron/services/model-discovery-service.ts:38-53). Any other shape is
// refused instead of guessed.
func productionModelsEndpoint(baseURL, provider string) (string, bool) {
	resolved, err := llm.ResolveOpenAIURL(baseURL, provider)
	if err != nil {
		return "", false
	}
	const chatSuffix = "/chat/completions"
	if !strings.HasSuffix(resolved, chatSuffix) {
		return "", false
	}
	return strings.TrimSuffix(resolved, chatSuffix) + "/models", true
}

// productionProbeBody caps the provider body before it is decoded, using the same
// read-one-byte-past-the-limit idiom as llm's limiter
// (internal/llm/openai.go:97). The connection test does not need this: it calls
// llm.Generate, which applies llm's own cap to the reply it decodes.
func productionProbeBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, productionProbeMaxBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > productionProbeMaxBodyBytes {
		return nil, errProductionProbeResponseTooLarge
	}
	return data, nil
}

// productionProbePurpose is the renderer's purpose split
// (model-discovery-service.ts:186): a profile is asking for embeddings when its
// purposes say so.
func productionProbePurpose(purposes []string) string {
	for _, purpose := range purposes {
		if purpose == "embedding" {
			return "embedding"
		}
	}
	return "generation"
}

// productionProbeDiscoveryStatus mirrors classifyHttpFailure
// (model-discovery-service.ts:88-96): the renderer has exactly three codes for a
// provider status, and an unlisted status is unsupported rather than invalid.
func productionProbeDiscoveryStatus(status int) string {
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return "auth"
	case status == http.StatusRequestTimeout || status == http.StatusTooEarly ||
		status == http.StatusTooManyRequests || status >= 500:
		return "network"
	default:
		return "unsupported"
	}
}

// productionDiscoveredModels projects one provider list into the renderer's
// DiscoveredModel shape (src/shared/ipc-channels.ts:510-524): id is the identifier
// the provider returned, name falls back to it, and value is what can be saved as
// modelName. A field that is not a short, credential-free, control-character-free
// string makes the whole answer invalid instead of being dropped silently, which
// is what safeProviderText does in the renderer's service.
func productionDiscoveredModels(payload any, credential string) ([]map[string]any, bool) {
	document, ok := payload.(map[string]any)
	if !ok {
		return nil, false
	}
	entries, ok := document["data"].([]any)
	if !ok || len(entries) > productionProbeMaxModels {
		return nil, false
	}
	models := make([]map[string]any, 0, len(entries))
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		item, ok := entry.(map[string]any)
		if !ok {
			return nil, false
		}
		id, ok := productionProbeModelText(item["id"], credential)
		if !ok {
			return nil, false
		}
		name := id
		if raw, present := item["name"]; present {
			if name, ok = productionProbeModelText(raw, credential); !ok {
				return nil, false
			}
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		model := map[string]any{"id": id, "name": name, "value": id}
		if capabilities := productionDiscoveredCapabilities(item); len(capabilities) > 0 {
			model["capabilities"] = capabilities
		}
		models = append(models, model)
	}
	return models, true
}

// productionProbeModelText mirrors safeProviderText
// (model-discovery-service.ts:74-86). A value the provider echoes back that
// contains the submitted credential is refused, so the model list can never be
// the path that puts a key in front of the browser.
func productionProbeModelText(value any, credential string) (string, bool) {
	text, ok := value.(string)
	if !ok || len(text) > productionProbeMaxModelText {
		return "", false
	}
	for _, character := range text {
		if character <= 0x1f || (character >= 0x7f && character <= 0x9f) {
			return "", false
		}
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	if credential != "" {
		if strings.Contains(text, credential) || strings.Contains(text, url.QueryEscape(credential)) {
			return "", false
		}
	}
	return text, true
}

// productionDiscoveredCapabilities keeps only the explicit declarations the
// renderer accepts (model-discovery-service.ts:118-127): a positive safe integer
// window or output limit and three boolean feature flags. Nothing is inferred.
func productionDiscoveredCapabilities(item map[string]any) map[string]any {
	capabilities := map[string]any{}
	for _, key := range []string{"context_window", "context_length"} {
		if value, ok := productionProbePositiveInteger(item[key]); ok {
			capabilities["contextWindowTokens"] = value
			break
		}
	}
	if value, ok := productionProbePositiveInteger(item["max_output_tokens"]); ok {
		capabilities["maxOutputTokens"] = value
	}
	if flags, ok := item["capabilities"].(map[string]any); ok {
		for _, key := range []string{"reasoning", "structuredOutput", "usage"} {
			if value, ok := flags[key].(bool); ok {
				capabilities[key] = value
			}
		}
	}
	return capabilities
}

func productionProbePositiveInteger(value any) (int64, bool) {
	number, ok := value.(float64)
	if !ok || number <= 0 || number != math.Trunc(number) || number > 9007199254740991 {
		return 0, false
	}
	return int64(number), true
}

// productionEmbeddingFirst puts the models a compatible endpoint names like an
// embedding model first, which is the only ordering hint such an endpoint gives
// (model-discovery-service.ts:294-299). The list itself is never filtered: an
// author still sees every model the provider published.
func productionEmbeddingFirst(models []map[string]any) []map[string]any {
	ordered := make([]map[string]any, 0, len(models))
	for _, model := range models {
		if productionProbeEmbeddingName(model) {
			ordered = append(ordered, model)
		}
	}
	for _, model := range models {
		if !productionProbeEmbeddingName(model) {
			ordered = append(ordered, model)
		}
	}
	return ordered
}

func productionProbeEmbeddingName(model map[string]any) bool {
	value, _ := model["value"].(string)
	lowered := strings.ToLower(value)
	return strings.Contains(lowered, "embed") || strings.Contains(lowered, "bge")
}

// productionProbeTestFailure shapes one llm:test-connection refusal as the
// renderer's { success: false, error } business result
// (src/shared/ipc-channels.ts:615-618). A malformed call, an unready global root
// or a full secret registry stays an HTTP refusal; every provider, policy and
// credential condition becomes a 200 result so the settings screen can show the
// sentence instead of a generic "request failed".
func productionProbeTestFailure(err error) (any, error) {
	var httpError *security.HTTPError
	if errors.As(err, &httpError) {
		return nil, err
	}
	var sentence *productionProbeText
	if errors.As(err, &sentence) && sentence.status != 0 {
		// The provider's own status travels twice: inside the sentence the author
		// reads, and as a field a future settings panel can style separately.
		return map[string]any{"success": false, "error": sentence.text, "status": sentence.status}, nil
	}
	return map[string]any{"success": false, "error": productionProbeMessage(err)}, nil
}

// productionProbeDiscoveryFailure shapes one llm:discover-models refusal as the
// renderer's { success: false, errorCode } business result
// (src/shared/ipc-channels.ts:529-531), whose union has no free text. "unsupported"
// is reserved for a genuine protocol or feature lack - the settings form reads it
// as "this endpoint does not support the standard model-list API". An endpoint the
// committed policy does not admit yet is not a feature lack: the same list request
// succeeds once the model is saved, so that whole family is endpoint_not_allowed.
func productionProbeDiscoveryFailure(err error) (any, error) {
	var httpError *security.HTTPError
	if errors.As(err, &httpError) {
		return nil, err
	}
	switch {
	case errors.Is(err, errProductionProbeCredentialRequired), errors.Is(err, errProductionProbeCredentialChanged),
		errors.Is(err, errProductionProbeConfigUnavailable):
		// A credential could not be resolved or restored, which the settings form
		// reads as "check the API key".
		return map[string]any{"success": false, "errorCode": "auth"}, nil
	case errors.Is(err, errProductionProbeProtocolUnsupported):
		// The only genuine feature lack: a protocol internal/llm has no list URL
		// resolver for.
		return map[string]any{"success": false, "errorCode": "unsupported"}, nil
	case productionProbeEndpointNotAdmitted(err):
		// Every way the committed policy can refuse an endpoint, including a policy
		// that could not be obtained at all: not admitted *yet*, never an endpoint
		// without a model list.
		return map[string]any{"success": false, "errorCode": "endpoint_not_allowed"}, nil
	case errors.Is(err, errProductionProbeNotReached), errors.Is(err, context.DeadlineExceeded):
		return map[string]any{"success": false, "errorCode": "network"}, nil
	}
	return map[string]any{"success": false, "errorCode": "invalid_response"}, nil
}

// productionProbeMessage is the author-facing sentence for one refusal. It
// formats symbolic codes only; paths, endpoints and credentials never enter it,
// and the rejected value is never echoed back.
func productionProbeMessage(err error) string {
	var sentence *productionProbeText
	if errors.As(err, &sentence) {
		return sentence.text
	}
	switch {
	case errors.Is(err, errProductionProbeCredentialRequired):
		return productionProbeCredentialRequiredMessage
	case errors.Is(err, errProductionProbeCredentialChanged):
		return productionProbeCredentialChangedMessage
	case errors.Is(err, errProductionProbeConfigUnavailable):
		return productionProbeConfigMessage
	case errors.Is(err, errProductionProbeProtocolUnsupported):
		return productionProbeProtocolMessage
	case errors.Is(err, errProductionProbeNotReached):
		return productionProbeNotReachedMessage
	case errors.Is(err, context.DeadlineExceeded):
		return productionProbeTimeoutMessage(productionProbeTimeoutSec)
	case errors.Is(err, egress.ErrNotAllowed), errors.Is(err, egress.ErrBaseURLInvalid), errors.Is(err, egress.ErrPolicyInvalid):
		return productionProbeOriginRefusedMessage(err)
	case errors.Is(err, egress.ErrRedirectForbidden):
		return fmt.Sprintf("The endpoint redirected, and this deployment refuses model redirects (%v).", err)
	case errors.Is(err, errProductionModelEgressUnconfigured),
		errors.Is(err, errProductionModelEgressInvalid),
		errors.Is(err, errProductionModelEgressUnavailable):
		return fmt.Sprintf("This deployment has no usable model-egress policy (%v). Check the committed model endpoints, then save this model: a save rebuilds the allowed origins.", err)
	}
	return productionProbeUnavailableMessage
}

// productionProbeTimeoutMessage names the budget the probe gave up after, never
// the endpoint it was dialing.
func productionProbeTimeoutMessage(seconds int) string {
	return fmt.Sprintf("The endpoint did not answer before the %v second deadline.", seconds)
}

// productionProbeRefusedMessage reports the provider's own status, which is the
// whole point of keeping this case apart from an unreachable host.
func productionProbeRefusedMessage(status int) string {
	return fmt.Sprintf("The provider answered and refused the request (HTTP %v). Check the Base URL, API key, and model name.", status)
}

// productionProbeOriginRefusedMessage reports the policy's fixed code. An origin
// the committed allowlist does not hold yet and a policy that could not be built
// are the same state llm:discover-models calls endpoint_not_allowed, so both name
// the one action that admits the endpoint - a model save commits it and rebuilds
// the allowed origins (production_open_linux.go:418) - and neither surface tells
// the author a different story. A Base URL the policy cannot parse is the author's
// own input and names that instead of an allowlist.
func productionProbeOriginRefusedMessage(err error) string {
	if errors.Is(err, egress.ErrBaseURLInvalid) {
		return fmt.Sprintf("This deployment refuses the endpoint's Base URL (%v). Check the Base URL.", err)
	}
	return fmt.Sprintf("This deployment has not admitted that endpoint yet (%v). Save this model, then retry: a save commits the endpoint and rebuilds the allowed origins.", err)
}

// productionProbeUnusableMessage reports a reply the adapter could not decode,
// keeping llm's own symbolic code so the diagnosis stays specific.
func productionProbeUnusableMessage(code string) string {
	return fmt.Sprintf("The provider answered, but its reply was unusable (%v).", code)
}

// The three tiny constructors below turn one condition into an error the two
// refusal shapers above can classify. They exist so no caller has to format a
// provider status or a symbolic code twice.
func productionProbeRefusalError(status int) error {
	return &productionProbeText{text: productionProbeRefusedMessage(status), status: status}
}

func productionProbeUnusableError(result llm.Response) error {
	code := result.Error
	if code == "" {
		code = "LLM_RESPONSE_INVALID"
	}
	return &productionProbeText{text: productionProbeUnusableMessage(code)}
}

func productionProbeDeadlineError(ctx context.Context, budget time.Duration) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return &productionProbeText{text: productionProbeTimeoutMessage(int(budget / time.Second))}
	}
	return &productionProbeText{text: productionProbeCancelledMessage}
}

// productionProbeText is one finished refusal sentence. It carries the provider's
// status when there was one, and never a body, a URL or a credential.
type productionProbeText struct {
	text   string
	status int
}

func (e *productionProbeText) Error() string { return e.text }

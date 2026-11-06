//go:build linux

package main

// This file binds the five unbound llm:* names the browser renderer uses to
// talk to a model: llm:begin-execution-lease, llm:generate, llm:generate-stream,
// llm:cancel and llm:close-execution-lease. All five are served here; nothing in
// this family is left unbound and no name answers a fake 200.
//
// HARD SECURITY RULES this file implements, in the order a request meets them:
//
//  1. A credential is read from the author's own configuration at request time
//     (models.json through internal/globalconfig, at begin and again at resolve)
//     and never from the request body, an environment variable or a public DTO.
//     The key lives only inside the transient llm.Model.APIKey of one call. It is
//     registered with the server's public sanitizer before it is used, so it
//     cannot reach an answer, an error, a log line, a database row or a lease
//     receipt; llm.HTTPFailure.Body, which can quote the key back, is stripped
//     from every answer. TestLLMGenerateNeverAnswersWithTheCredential scans the
//     whole HTTP answer for the secret.
//  2. All egress goes through internal/egress against the committed policy this
//     lane derived at startup. This file never builds a URL, follows a redirect
//     or opens a proxy: it hands the request to egress.Client.Do and maps the
//     policy's own symbolic refusal. There is no injectable dispatch seam here.
//  3. A lease is the only thing that grants provider authority. It is bound to
//     the caller tab, the project and the run it names, it expires, and it is
//     refused by name (MODEL_EXECUTION_LEASE_INVALID / ..._EXPIRED) when it is
//     absent, foreign, expired or stale. llm:generate without a lease is refused
//     rather than falling back to a profile lookup: the shipped renderer always
//     begins a lease first (src/services/generation/generation-runtime.ts:
//     500-540), and a second, unleased path to a provider is authority this
//     server does not grant.
//  4. llm:cancel stops the in-flight provider call and settles the run with
//     exactly one terminal event; a cancelled run publishes nothing afterwards
//     and writes no artifact. This lane never opens the project database, so a
//     refused or cancelled request cannot leave a byte behind.
//  5. Every call has a deadline, every provider body is capped by internal/llm
//     (64 MiB collected, 8 MiB per SSE event, 8 MiB per stream), and nothing
//     buffers an unbounded stream: each chunk is published as it arrives.
//
// What this file deliberately does not do, because the Go port of that policy
// does not exist: it does not resolve a reasoning policy, an output-token
// parameter override or a reasoning-usage evidence shape
// (src/shared/generation-parameter-policy.ts, src/shared/reasoning-policy.ts).
// llm.Options.Reasoning therefore stays nil, which sends no reasoning field at
// all rather than inventing one. Input budgeting, capability verification and
// receipt validation remain the renderer's own concerns
// (src/services/generation/generation-runtime.ts).

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/generationowner"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llm"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llmlease"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionLLMRequestTimeout bounds one non-streaming answer on a cloud
	// endpoint, and productionLLMLocalRequestTimeout the same on a loopback
	// endpoint, where a small local model legitimately takes minutes.
	productionLLMRequestTimeout      = 10 * time.Minute
	productionLLMLocalRequestTimeout = 20 * time.Minute
	// productionLLMLeaseTTL is the renderer's own default
	// (electron/services/model-execution-lease.ts:13).
	productionLLMLeaseTTL = 4 * time.Hour
	// productionLLMMaxLeases bounds the lease registry.
	productionLLMMaxLeases = 256
	// productionLLMMaxMessages and productionLLMMaxMessageBytes bound one request
	// before a provider body is built. The operation body itself is already capped
	// at security.DefaultBodyLimit (8 MiB).
	productionLLMMaxMessages     = 4096
	productionLLMMaxMessageBytes = 1 << 20
	// productionLLMMaxRequestIDBytes bounds the stream identity the browser picks.
	productionLLMMaxRequestIDBytes = 128
)

var (
	errProductionLLMUnavailable    = errors.New("PRODUCTION_LLM_EXECUTION_UNAVAILABLE")
	errProductionLLMRequestInvalid = errors.New("LLM_REQUEST_INVALID")
	errProductionLLMProtocol       = errors.New("LLM_PROTOCOL_UNSUPPORTED")
	errProductionLLMCredential     = errors.New("MODEL_CREDENTIAL_UNAVAILABLE")
	errProductionLLMSecretLimit    = errors.New("PUBLIC_RESPONSE_SECRET_LIMIT")
	// errProductionLLMKimiTemperature is the renderer's own refusal for a fixed
	// temperature model whose saved profile already carries a custom one
	// (src/shared/generation-parameter-policy.ts:47-64).
	errProductionLLMKimiTemperature = errors.New("GENERATION_KIMI_TEMPERATURE_INVALID")
)

// productionLLMExecution serves the five llm:* names over the committed global
// config the lane pinned, the committed model-egress policy it derived at
// startup, and the lease registry this file owns. It holds no project database
// handle and no author content.
type productionLLMExecution struct {
	config *globalconfig.Service
	// policy is productionOpen.ModelEgress in a real deployment.
	policy func() (*egress.Client, error)
	// credentials is the credential seam the generation executor resolves through
	// (internal/generationowner/dispatch.go:18-31). models.json keeps its apiKey
	// plaintext; *secrets.Storage satisfies the same interface for the sealed lane.
	credentials generationowner.CredentialStorage
	leases      *llmlease.Registry
	server      *transport.Server
	// register is the server's public sanitizer, captured at Bind time.
	register func(string) (bool, error)
	// mu guards streams. An entry exists for exactly the life of one provider
	// call, so llm:cancel can find and stop it.
	mu      sync.Mutex
	streams map[string]*productionLLMStream
}

// productionLLMStream is one in-flight streaming call. cancel is the only thing
// llm:cancel touches; the run itself is a single goroutine whose callbacks fire
// in order, so no further lock is needed to keep one terminal event terminal.
type productionLLMStream struct {
	cancel  context.CancelFunc
	leaseID string
	settled bool
}

// newProductionLLMExecution builds the family over an admitted lane. A lane that
// is closed or carries no config yields a refusal, so all five names keep failing
// closed at GO_OPERATION_NOT_MIGRATED instead of becoming handlers that could
// only refuse.
func newProductionLLMExecution(open *productionOpen) (*productionLLMExecution, error) {
	if open == nil {
		return nil, errProductionLLMUnavailable
	}
	open.mu.Lock()
	config, policy, closed := open.config, open.ModelEgress, open.closed
	open.mu.Unlock()
	if closed || config == nil || policy == nil {
		return nil, errProductionLLMUnavailable
	}
	return newProductionLLMExecutionOver(config, policy)
}

// newProductionLLMExecutionOver is the same assembly from explicit parts, so a
// test can pin one committed config root and the policy derived from it without
// opening an author database.
func newProductionLLMExecutionOver(config *globalconfig.Service, policy func() (*egress.Client, error)) (*productionLLMExecution, error) {
	if config == nil || policy == nil {
		return nil, errProductionLLMUnavailable
	}
	execution := &productionLLMExecution{
		config:      config,
		policy:      policy,
		credentials: generationowner.PlaintextCredential{},
		streams:     make(map[string]*productionLLMStream),
	}
	registry, err := llmlease.New(llmlease.Options{
		LoadModel: execution.loadModel,
		TTL:       productionLLMLeaseTTL,
		MaxLeases: productionLLMMaxLeases,
	})
	if err != nil {
		return nil, errProductionLLMUnavailable
	}
	execution.leases = registry
	return execution, nil
}

// Bind installs the five names. It is called by the lane before serving, exactly
// like the other production binders, so the captured sanitizer registrar is the
// server's own and cannot be replaced by a request.
func (x *productionLLMExecution) Bind(server *transport.Server) error {
	if x == nil || server == nil || x.config == nil || x.policy == nil || x.credentials == nil || x.leases == nil {
		return errProductionLLMUnavailable
	}
	x.server = server
	x.register = server.RegisterSecret
	for _, binding := range []struct {
		name    string
		handler transport.Operation
	}{
		{"llm:begin-execution-lease", x.begin},
		{"llm:generate", x.generate},
		{"llm:generate-stream", x.generateStream},
		{"llm:cancel", x.cancel},
		{"llm:close-execution-lease", x.close},
	} {
		if err := server.Bind(binding.name, binding.handler); err != nil {
			return err
		}
	}
	return nil
}

// loadModel reads one saved profile from the author's own configuration. It is
// the only place a credential enters this lane, and it is called from a request
// rather than from startup, so a rotated key is picked up without a restart.
func (x *productionLLMExecution) loadModel(ctx context.Context, modelID string) (generationowner.Profile, bool, error) {
	if x == nil || x.config == nil || modelID == "" {
		return generationowner.Profile{}, false, errProductionLLMUnavailable
	}
	if err := ctx.Err(); err != nil {
		return generationowner.Profile{}, false, err
	}
	records, err := x.config.ListPrivateModels()
	if err != nil {
		return generationowner.Profile{}, false, err
	}
	for _, record := range records {
		if id, _ := record["id"].(string); id != modelID {
			continue
		}
		profile, err := syntheticModelProfile(record)
		if err != nil {
			return generationowner.Profile{}, false, err
		}
		return profile, true, nil
	}
	return generationowner.Profile{}, false, nil
}

// egressClient returns the committed policy, or the lane's symbolic refusal.
func (x *productionLLMExecution) egressClient() (*egress.Client, error) {
	if x == nil || x.policy == nil {
		return nil, errProductionLLMUnavailable
	}
	client, err := x.policy()
	if err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errProductionModelEgressUnconfigured
	}
	return client, nil
}

// begin answers llm:begin-execution-lease. The renderer's shape is
// BeginModelExecutionLeaseResult (src/shared/ipc-channels.ts:770-778): success
// with a receipt, or success:false with an errorCode it maps to LEASE_BEGIN_FAILED.
func (x *productionLLMExecution) begin(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	modelID, err := productionLLMSingleString(args)
	if err != nil {
		return nil, err
	}
	receipt, err := x.leases.Begin(ctx, productionLLMOwner(caller), modelID)
	if err != nil {
		if errors.Is(err, llmlease.ErrModelNotFound) {
			return map[string]any{"success": false, "errorCode": "MODEL_NOT_FOUND"}, nil
		}
		return map[string]any{"success": false, "errorCode": "LEASE_BEGIN_FAILED"}, nil
	}
	return map[string]any{"success": true, "lease": receipt}, nil
}

// close answers llm:close-execution-lease. An unknown, expired or foreign lease
// is the renderer's already-closed case (electron/controllers/llm-controller.ts:
// 213-225), which it treats as success; anything this tab still holds is released
// and any call still running under it loses authority.
func (x *productionLLMExecution) close(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	leaseID, err := productionLLMSingleString(args)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return map[string]any{"success": false, "error": "MODEL_EXECUTION_LEASE_INVALID"}, nil
	}
	if !x.leases.Close(productionLLMOwner(caller), leaseID) {
		return map[string]any{"success": false, "error": "MODEL_EXECUTION_LEASE_INVALID"}, nil
	}
	x.stopLease(productionLLMOwner(caller), leaseID)
	return map[string]any{"success": true}, nil
}

// cancel answers llm:cancel. It stops the provider call the tab is running; the
// run's own goroutine then publishes exactly one terminal event. A request id
// with no active stream is not an error (electron/controllers/llm-controller.ts:
// 339-355 answers success:false to the same state).
func (x *productionLLMExecution) cancel(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	requestID, err := productionLLMSingleString(args)
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil {
		return map[string]any{"success": false}, nil
	}
	stream := x.takeStream(productionLLMStreamKey(caller, requestID))
	if stream == nil {
		return map[string]any{"success": false}, nil
	}
	stream.cancel()
	return map[string]any{"success": true}, nil
}

// generate answers llm:generate with the renderer's LLMResponse
// (src/shared/ipc-channels.ts:792-806). Every failure is a symbolic string in its
// error field, because that is the only field the renderer shows and the only
// field the main process records.
func (x *productionLLMExecution) generate(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 1 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	if ctx.Err() != nil {
		return productionLLMFailure("LLM_CANCELLED"), nil
	}
	invocation, failure := x.prepare(ctx, caller, args[0])
	if failure != "" {
		return productionLLMFailure(failure), nil
	}
	ctx, release := context.WithTimeout(ctx, invocation.budget)
	defer release()
	response, dispatchErr := invocation.generateOnce(ctx)
	return productionLLMAnswer(response, dispatchErr, ctx.Err()), nil
}

// generateStream answers llm:generate-stream. It admits the request exactly like
// llm:generate, then publishes the provider's frames on the caller's own tab:
// llm:stream-chunk, then exactly one of llm:stream-done or llm:stream-error
// (src/stores/llm-store.ts:180-340 consumes only that grammar).
func (x *productionLLMExecution) generateStream(ctx context.Context, caller transport.Caller, args []any) (any, error) {
	if len(args) != 2 {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	requestID, ok := args[0].(string)
	if !ok || requestID == "" || len(requestID) > productionLLMMaxRequestIDBytes || !utf8Printable(requestID) {
		return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	key := productionLLMStreamKey(caller, requestID)
	refuse := func(code string) (any, error) {
		return map[string]any{"requestId": requestID, "started": false, "error": code}, nil
	}
	if ctx.Err() != nil {
		return refuse("LLM_CANCELLED")
	}
	if x.activeStream(key) != nil {
		return refuse("LLM_STREAM_ALREADY_ACTIVE")
	}
	invocation, failure := x.prepare(ctx, caller, args[1])
	if failure != "" {
		return refuse(failure)
	}
	// The operation context dies with its HTTP response, so the stream runs on a
	// detached context that keeps only the values (nothing here reads one) and
	// carries its own deadline. The renderer's cancel arrives on llm:cancel.
	streamCtx, release := context.WithTimeout(context.WithoutCancel(ctx), invocation.budget)
	stream := &productionLLMStream{cancel: release, leaseID: invocation.leaseID}
	x.mu.Lock()
	if x.streams == nil {
		x.streams = make(map[string]*productionLLMStream)
	}
	existing := x.streams[key]
	if existing == nil {
		x.streams[key] = stream
	}
	x.mu.Unlock()
	if existing != nil {
		release()
		return refuse("LLM_STREAM_ALREADY_ACTIVE")
	}
	go x.runStream(streamCtx, key, caller, requestID, invocation, stream)
	return map[string]any{"requestId": requestID, "started": true}, nil
}

// runStream dials the provider and republishes its frames until the run settles.
// It owns exactly one terminal event: every publish after that returns early, and
// the caller's tab disappearing stops the provider call rather than buffering it.
func (x *productionLLMExecution) runStream(ctx context.Context, key string, caller transport.Caller, requestID string, invocation *productionLLMInvocation, stream *productionLLMStream) {
	defer func() {
		stream.cancel()
		x.mu.Lock()
		if current := x.streams[key]; current == stream {
			delete(x.streams, key)
		}
		x.mu.Unlock()
	}()
	publish := func(channel string, payload map[string]any) bool {
		if stream.settled {
			return false
		}
		if err := x.server.Publish(caller, channel, []any{payload}); err != nil {
			// A tab that is gone cannot receive the rest of this run; stop the
			// provider call instead of streaming into a closed hub.
			stream.settled = true
			stream.cancel()
			return false
		}
		return true
	}
	settle := func(channel string, payload map[string]any) {
		if stream.settled {
			return
		}
		stream.settled = true
		_ = x.server.Publish(caller, channel, []any{payload})
	}
	done := func(fullText string, usage *llm.Usage, finish string) {
		payload := map[string]any{"requestId": requestID, "fullText": fullText, "finishReason": finish}
		if usage != nil {
			payload["usage"] = usage
		}
		settle("llm:stream-done", payload)
	}
	callbacks := llm.Callbacks{
		OnChunk: func(text string) {
			publish("llm:stream-chunk", map[string]any{"requestId": requestID, "chunk": text})
		},
		OnDone: done,
		OnError: func(code, partial string, usage *llm.Usage) {
			switch {
			case errors.Is(ctx.Err(), context.DeadlineExceeded):
				// A call that ran out of its own budget failed; it is not a cancel.
				// The renderer has no separate timeout code, so this is the network
				// failure the shipped provider also reports for one
				// (electron/llm/openai-provider.ts:416-422).
				if partial != "" {
					done(partial, usage, "error")
					return
				}
				settle("llm:stream-error", map[string]any{"requestId": requestID, "error": "LLM_STREAM_NETWORK_FAILED"})
			case ctx.Err() != nil:
				// A cancelled run settles as cancelled: it is a member of the
				// renderer's frozen LLMFinishReason union (src/shared/ipc-channels.ts:
				// 805) and the ledger records it as 'cancelled'
				// (internal/store/posthistory.go:351), so the run is settled rather
				// than reported as a provider failure. Publishing nothing after this is
				// what keeps a cancelled run from writing an artifact.
				done(partial, usage, "cancelled")
			case partial != "":
				// The renderer keeps a partial answer it can show and marks the run
				// failed (electron/controllers/llm-controller.ts:307-320).
				done(partial, usage, "error")
			default:
				settle("llm:stream-error", map[string]any{"requestId": requestID, "error": code})
			}
		},
	}
	if dispatchErr := invocation.stream(ctx, callbacks); dispatchErr != nil {
		callbacks.OnError(productionProbeDispatchFailure(dispatchErr).Error(), "", nil)
	}
	// One terminal event, always: an adapter that neither settled nor failed (an
	// unsupported protocol reaching this far) still ends the caller's stream.
	if !stream.settled {
		callbacks.OnError("LLM_REQUEST_FAILED", "", nil)
	}
}

// productionLLMInvocation is one admitted request: the frozen profile the lease
// resolved, the bounds the lease's own evidence attests, the credential resolved
// for this call, and the committed policy client it may dial through.
type productionLLMInvocation struct {
	profile  generationowner.Profile
	leaseID  string
	client   *egress.Client
	model    llm.Model
	messages []llm.Message
	options  llm.Options
	budget   time.Duration
}

// generate runs the admitted request against the protocol's adapter and reports
// the dispatch error separately, because only the caller knows how to classify it.
func (i *productionLLMInvocation) generateOnce(ctx context.Context) (llm.Response, error) {
	var dispatchErr error
	dispatch := i.dispatch(&dispatchErr)
	switch i.profile.Protocol {
	case "openai":
		return (&llm.OpenAI{Dispatch: dispatch}).Generate(ctx, i.model, i.messages, i.options), dispatchErr
	case "gemini":
		return (&llm.Gemini{Dispatch: dispatch}).Generate(ctx, i.model, i.messages, i.options), dispatchErr
	default:
		return llm.Response{}, errProductionLLMProtocol
	}
}

// stream runs the same admitted request as a provider stream, reporting frames
// through the callbacks until the adapter settles them.
func (i *productionLLMInvocation) stream(ctx context.Context, callbacks llm.Callbacks) error {
	var dispatchErr error
	dispatch := i.dispatch(&dispatchErr)
	switch i.profile.Protocol {
	case "openai":
		(&llm.OpenAI{Dispatch: dispatch}).Stream(ctx, i.model, i.messages, i.options, callbacks)
	case "gemini":
		(&llm.Gemini{Dispatch: dispatch}).Stream(ctx, i.model, i.messages, i.options, callbacks)
	default:
		return errProductionLLMProtocol
	}
	return dispatchErr
}

// dispatch is the only path to a provider: every request leaves through the
// committed egress policy, which validates the origin and refuses a redirect.
func (i *productionLLMInvocation) dispatch(failure *error) llm.Dispatch {
	return func(request *http.Request) (*http.Response, error) {
		response, err := i.client.Do(request)
		if err != nil && failure != nil {
			*failure = err
		}
		return response, err
	}
}

// prepare applies every admission rule in order and returns either one admitted
// invocation or one symbolic refusal code. It never formats a path, an endpoint,
// a provider body or a credential into that code.
func (x *productionLLMExecution) prepare(ctx context.Context, caller transport.Caller, value any) (*productionLLMInvocation, string) {
	request, err := productionLLMRequestFrom(value)
	if err != nil {
		return nil, errProductionLLMRequestInvalid.Error()
	}
	if request.leaseID == "" {
		// Rule 3: no lease, no authority. The name is the renderer's own lease code.
		return nil, llmlease.ErrInvalid.Error()
	}
	snapshot, err := x.leases.Resolve(ctx, productionLLMOwner(caller), request.leaseID, request.projectID)
	if err != nil {
		for _, known := range []error{llmlease.ErrInvalid, llmlease.ErrExpired, llmlease.ErrChanged} {
			if errors.Is(err, known) {
				return nil, known.Error()
			}
		}
		return nil, llmlease.ErrInvalid.Error()
	}
	// The credential is read from the author's own profile for this request only.
	key, err := x.credentials.DecryptString(snapshot.Profile.EncryptedCredential)
	if err != nil {
		return nil, errProductionLLMCredential.Error()
	}
	if key != "" {
		// Registered before the call so no diagnostic this lane can emit carries it.
		if _, err := x.register(key); err != nil {
			return nil, errProductionLLMSecretLimit.Error()
		}
	}
	client, err := x.egressClient()
	if err != nil {
		return nil, productionLLMSymbolic(err)
	}
	if err := client.CheckBaseURL(snapshot.Profile.BaseURL); err != nil {
		return nil, productionProbeRefusedOrigin(err).Error()
	}
	switch snapshot.Profile.Protocol {
	case "openai", "gemini":
	default:
		return nil, errProductionLLMProtocol.Error()
	}
	temperature, ok := productionLLMTemperature(snapshot.Profile)
	if !ok {
		return nil, errProductionLLMKimiTemperature.Error()
	}
	// The lease's own evidence is the frozen output bound, so a request can never
	// ask for more than the receipt the browser already holds promises.
	tokens := snapshot.Receipt.CapabilityEvidence.Output
	if tokens <= 0 {
		return nil, llmlease.ErrOutputLimit.Error()
	}
	if request.maxTokens != nil && *request.maxTokens > 0 && *request.maxTokens < tokens {
		tokens = *request.maxTokens
	}
	options := llm.Options{
		MaxTokens:      &tokens,
		ResponseFormat: request.responseFormat,
		// The lease is the run's identity for gateway session affinity
		// (electron/controllers/llm-controller.ts:265-270).
		ConversationID: snapshot.Receipt.LeaseID,
	}
	if temperature != nil {
		value := *temperature
		options.Temperature = &value
	}
	profile := snapshot.Profile
	return &productionLLMInvocation{
		profile: profile,
		leaseID: snapshot.Receipt.LeaseID,
		client:  client,
		model: llm.Model{
			Provider:  profile.Provider,
			ModelName: profile.ModelName,
			BaseURL:   profile.BaseURL,
			APIKey:    key,
			MaxTokens: tokens,
		},
		messages: request.messages,
		options:  options,
		budget:   productionLLMBudget(profile),
	}, ""
}

// productionLLMBudget is the one deadline a call gets. A loopback endpoint gets
// the local budget, exactly like the probe's own rule
// (src/shared/provider-presets.ts:474-487).
func productionLLMBudget(profile generationowner.Profile) time.Duration {
	if productionProbeLocalEndpoint(productionProbeForm{provider: profile.Provider, baseURL: profile.BaseURL}) {
		return productionLLMLocalRequestTimeout
	}
	return productionLLMRequestTimeout
}

// productionLLMTemperature returns the temperature to send, nil to send none.
// Kimi's official hosts fix sampling for their K-series models and reject a
// custom value, and refuse an out-of-range saved temperature for the rest - the
// two exceptions the shipped parameter policy makes
// (electron/llm/generation-parameter-policy.ts:37-56, 90-107).
func productionLLMTemperature(profile generationowner.Profile) (*float64, bool) {
	official := productionLLMKimiHost(profile.BaseURL)
	fixed := official && productionLLMKimiFixedTemperature(profile.ModelName)
	if official && !fixed && !(profile.Temperature >= 0 && profile.Temperature <= 1) {
		return nil, false
	}
	if fixed {
		return nil, true
	}
	value := profile.Temperature
	return &value, true
}

// productionLLMKimiHost is isOfficialKimiHost: https and one of the two official
// hosts. It deliberately ignores the provider label, exactly like the original.
func productionLLMKimiHost(baseURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed == nil || parsed.Scheme != "https" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "api.moonshot.cn" || host == "api.moonshot.ai"
}

// productionLLMKimiFixedTemperature is hasModelFamilyPrefix over the pinned K
// prefixes: the family name itself or a dash-suffixed variant of it.
func productionLLMKimiFixedTemperature(modelName string) bool {
	name := strings.ToLower(strings.TrimSpace(modelName))
	for _, prefix := range []string{"kimi-k3", "kimi-k2.7", "kimi-k2.6", "kimi-k2.5"} {
		if name == prefix || strings.HasPrefix(name, prefix+"-") {
			return true
		}
	}
	return false
}

// productionLLMAnswer reduces one non-streaming adapter response to the
// renderer's LLMResponse. The provider body is always dropped: it can quote the
// credential back, and the renderer only ever shows the symbolic code.
func productionLLMAnswer(response llm.Response, dispatchErr error, deadline error) llm.Response {
	if response.HTTPFailure != nil {
		response.HTTPFailure = &llm.HTTPFailure{Status: response.HTTPFailure.Status}
	}
	if response.Success {
		response.FinishReason = productionLLMFinishReason(response.FinishReason)
		return response
	}
	if deadline != nil && errors.Is(deadline, context.DeadlineExceeded) {
		// The renderer has no separate timeout code; a provider that never answered
		// is the request failure its matrix already understands.
		response.Error = "LLM_REQUEST_FAILED"
		response.FinishReason = "error"
		return response
	}
	if dispatchErr != nil {
		switch {
		case errors.Is(dispatchErr, context.Canceled):
			response.Error = "LLM_CANCELLED"
		case errors.Is(dispatchErr, context.DeadlineExceeded):
			response.Error = "LLM_REQUEST_FAILED"
		default:
			// productionProbeDispatchFailure turns every native error into an
			// egress code: a *url.Error would carry the whole endpoint.
			response.Error = productionProbeDispatchFailure(dispatchErr).Error()
		}
		response.FinishReason = "error"
		return response
	}
	if response.Error == "" {
		response.Error = "LLM_REQUEST_FAILED"
	}
	response.FinishReason = productionLLMFinishReason(response.FinishReason)
	if response.FinishReason == "stop" {
		response.FinishReason = "error"
	}
	return response
}

// productionLLMFinishReason keeps the answer inside the renderer's frozen
// LLMFinishReason union (src/shared/ipc-channels.ts:805).
func productionLLMFinishReason(value string) string {
	switch value {
	case "stop", "length", "content_filter", "cancelled", "error":
		return value
	default:
		return "unknown"
	}
}

func productionLLMFailure(code string) llm.Response {
	return llm.Response{Content: "", FinishReason: "error", Error: code}
}

// productionLLMSymbolic keeps an internal refusal symbolic. A policy error is
// already a fixed code; anything else becomes the lane's unavailability rather
// than a formatted message.
func productionLLMSymbolic(err error) string {
	for _, known := range []error{
		errProductionLLMUnavailable,
		errProductionModelEgressUnconfigured,
		errProductionModelEgressInvalid,
		errProductionModelEgressUnavailable,
		egress.ErrPolicyInvalid,
	} {
		if errors.Is(err, known) {
			return known.Error()
		}
	}
	return errProductionLLMUnavailable.Error()
}

// productionLLMOwner is the lease identity of one browser tab.
func productionLLMOwner(caller transport.Caller) llmlease.Owner {
	return llmlease.Owner{Session: caller.SessionID, Tab: caller.ClientID}
}

// productionLLMStreamKey mirrors the renderer's own stream key
// (electron/controllers/llm-controller.ts:96: `${client.id}:${requestId}`).
func productionLLMStreamKey(caller transport.Caller, requestID string) string {
	return caller.SessionID + "\x00" + caller.ClientID + "\x00" + requestID
}

func (x *productionLLMExecution) activeStream(key string) *productionLLMStream {
	if x == nil {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.streams[key]
}

func (x *productionLLMExecution) takeStream(key string) *productionLLMStream {
	if x == nil {
		return nil
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	stream := x.streams[key]
	if stream != nil {
		delete(x.streams, key)
	}
	return stream
}

// stopLease cancels every call still running under one closed lease: closing a
// lease revokes the authority it granted, so no run may continue under it.
func (x *productionLLMExecution) stopLease(owner llmlease.Owner, leaseID string) {
	if x == nil || leaseID == "" {
		return
	}
	prefix := owner.Session + "\x00" + owner.Tab + "\x00"
	x.mu.Lock()
	stops := make([]context.CancelFunc, 0, len(x.streams))
	for key, stream := range x.streams {
		if stream.leaseID != leaseID || !strings.HasPrefix(key, prefix) {
			continue
		}
		stops = append(stops, stream.cancel)
		delete(x.streams, key)
	}
	x.mu.Unlock()
	for _, stop := range stops {
		stop()
	}
}

// productionLLMRequest is one decoded LLMRequest. Unknown fields are ignored,
// exactly like the renderer's own object spread, but every field this lane reads
// is type-checked and bounded.
type productionLLMRequest struct {
	leaseID        string
	modelID        string
	projectID      string
	messages       []llm.Message
	maxTokens      *int64
	responseFormat map[string]any
}

func productionLLMRequestFrom(value any) (productionLLMRequest, error) {
	request := productionLLMRequest{}
	object, ok := value.(map[string]any)
	if !ok || object == nil {
		return request, errProductionLLMRequestInvalid
	}
	if request.modelID, ok = productionProbeOptionalString(object, "modelId"); !ok {
		return request, errProductionLLMRequestInvalid
	}
	if request.leaseID, ok = productionProbeOptionalString(object, "modelExecutionLeaseId"); !ok {
		return request, errProductionLLMRequestInvalid
	}
	raw, present := object["messages"]
	items, ok := raw.([]any)
	if !present || !ok || len(items) == 0 || len(items) > productionLLMMaxMessages {
		return request, errProductionLLMRequestInvalid
	}
	request.messages = make([]llm.Message, 0, len(items))
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		role, ok := entry["role"].(string)
		if !ok {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		switch role {
		case "system", "user", "assistant":
		default:
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		content, ok := entry["content"].(string)
		if !ok || len(content) > productionLLMMaxMessageBytes {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		request.messages = append(request.messages, llm.Message{Role: role, Content: content})
	}
	if raw, present := object["maxTokens"]; present && raw != nil {
		number, ok := raw.(float64)
		if !ok || number <= 0 || number != math.Trunc(number) || number > 9007199254740991 {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		tokens := int64(number)
		request.maxTokens = &tokens
	}
	if raw, present := object["responseFormat"]; present && raw != nil {
		format, ok := raw.(map[string]any)
		if !ok {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		kind, ok := format["type"].(string)
		if !ok || (kind != "json_object" && kind != "text") {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		request.responseFormat = map[string]any{"type": kind}
	}
	// The frozen project context arrives inside the request for this family:
	// isProjectScopedChannel (src/services/ipc-client.ts:39-70) appends no trailing
	// context to an llm:* call. The lease binds the first project that names it.
	if raw, present := object["projectSession"]; present && raw != nil {
		session, ok := raw.(map[string]any)
		if !ok {
			return productionLLMRequest{}, errProductionLLMRequestInvalid
		}
		for _, key := range []string{"projectId", "leaseId", "projectPath"} {
			text, ok := session[key].(string)
			if !ok || text == "" {
				return productionLLMRequest{}, errProductionLLMRequestInvalid
			}
		}
		request.projectID, _ = session["projectId"].(string)
	}
	return request, nil
}

// productionLLMSingleString decodes a one-string-argument operation.
func productionLLMSingleString(args []any) (string, error) {
	if len(args) != 1 {
		return "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	value, ok := args[0].(string)
	if !ok || value == "" || len(value) > productionLLMMaxRequestIDBytes || !utf8Printable(value) {
		return "", security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
	}
	return value, nil
}

// utf8Printable refuses an identity that cannot be a JSON object key safely.
func utf8Printable(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

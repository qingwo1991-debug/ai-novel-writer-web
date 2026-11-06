//go:build linux

package main

// This file proves the five llm:* names the previous file binds, over real HTTP:
// a frozen browser session, the committed model-egress policy, and a synthetic
// provider that is a real httptest listener dialed through that policy. Nothing
// here fakes a dispatch: the only way a request reaches the stub is
// egress.Client.Do against the origin derived from the committed models.json.
//
// The provider answers as a hostile one would: it echoes the credential back in
// an error body, and one case answers a body that is not JSON at all. The answers
// are then scanned for the secret, so the credential boundary is proven by the
// bytes that leave the server rather than by the intent of the code.

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/llmlease"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionLLMTestStreamRequest is the identity the renderer would generate
	// for one stream; the frame assertions below quote it verbatim.
	productionLLMTestStreamRequest = "prod-llm-stream-1"
	productionLLMTestCancelRequest = "prod-llm-cancel-1"
	// productionLLMTestFrameTimeout bounds one expected frame. A frame that needs
	// longer than this is a hung run, not a slow one.
	productionLLMTestFrameTimeout = 5 * time.Second
)

// productionLLMStub is the synthetic provider. It records what it was asked and
// answers through the test's own handler.
type productionLLMStub struct {
	mu       sync.Mutex
	requests int
	bodies   []string
	// cancelled records that the provider itself saw the request context end,
	// which is the only proof that llm:cancel stopped the in-flight call rather
	// than merely settling the renderer's stream.
	cancelled int
	serve     func(stub *productionLLMStub, w http.ResponseWriter, r *http.Request)
}

func (s *productionLLMStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	s.mu.Lock()
	s.requests++
	s.bodies = append(s.bodies, string(body))
	serve := s.serve
	s.mu.Unlock()
	if serve == nil {
		w.WriteHeader(http.StatusTeapot)
		return
	}
	serve(s, w, r)
}

func (s *productionLLMStub) recordCancelled() {
	s.mu.Lock()
	s.cancelled++
	s.mu.Unlock()
}

func (s *productionLLMStub) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requests
}

func (s *productionLLMStub) cancellations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cancelled
}

// productionLLMStubCompletion answers a non-streaming chat completion.
func productionLLMStubCompletion(content string) func(*productionLLMStub, http.ResponseWriter, *http.Request) {
	return func(_ *productionLLMStub, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-synthetic","object":"chat.completion","choices":[{"index":0,`+
			`"message":{"role":"assistant","content":"`+content+`"},"finish_reason":"stop"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":10}}`)
	}
}

// productionLLMStubStream answers an OpenAI-compatible SSE stream: two content
// deltas, a terminal finish_reason, an accounting-only event and [DONE], which is
// the exact grammar internal/llm consumes.
func productionLLMStubStream(parts []string, hold bool) func(*productionLLMStub, http.ResponseWriter, *http.Request) {
	return func(stub *productionLLMStub, w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		for _, part := range parts {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":"+
				productionLLMJSONString(part)+"}}]}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}
		if hold {
			// A provider that keeps the stream open is the only way to prove a
			// cancel reached the socket instead of the buffer.
			<-r.Context().Done()
			stub.recordCancelled()
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":7,\"total_tokens\":10}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// productionLLMJSONString encodes one text as a JSON string literal.
func productionLLMJSONString(text string) string {
	encoded, err := json.Marshal(text)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

// productionLLMFixture is one served lane: the admitted real data directory, the
// committed policy derived from it, the five bound names and a synthetic provider
// behind the policy.
type productionLLMFixture struct {
	root      string
	open      *productionOpen
	execution *productionLLMExecution
	gateway   *httptest.Server
	provider  *productionLLMStub
}

func newProductionLLMFixture(t *testing.T) *productionLLMFixture {
	t.Helper()
	stub := &productionLLMStub{}
	provider := httptest.NewServer(stub)
	t.Cleanup(provider.Close)
	root := syntheticProductionDataDir(t)
	// The committed models.json is rewritten so the story profile points at this
	// provider, keeping the same id, model name, key, temperature and bound, and
	// keeping the embedding profile untouched, so every other fixture fact holds.
	models := `[{"id":"` + productionTestModelID +
		`","provider":"custom","protocol":"openai","baseUrl":"` + provider.URL + `/v1","apiKey":"` + productionTestModelSecret +
		`","name":"合成正文模型","modelName":"ci-story-model","temperature":0.7,"maxTokens":1024},` +
		`{"id":"` + productionTestEmbeddingModelID +
		`","provider":"custom","protocol":"openai","baseUrl":"https://synthetic.invalid/v1","apiKey":"synthetic-production-embedding-key"` +
		`,"name":"合成向量模型","modelName":"ci-embedding-model"}]`
	modelsPath := filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration, "models.json")
	if err := os.WriteFile(modelsPath, []byte(models), 0600); err != nil {
		t.Fatal(err)
	}
	layout, err := productionLayoutFor(root)
	if err != nil {
		t.Fatal(err)
	}
	open, err := openProduction(context.Background(), layout, productionConfig{
		Schema: productionTestContract(), MaxDatabaseBytes: productionMaxDatabaseBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = open.Close() })
	execution, err := newProductionLLMExecution(open)
	if err != nil {
		t.Fatal(err)
	}
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-admitted")
	if err := execution.Bind(server); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	gateway := httptest.NewServer(mux)
	t.Cleanup(gateway.Close)
	return &productionLLMFixture{
		root:      root,
		open:      open,
		execution: execution,
		gateway:   gateway,
		provider:  stub,
	}
}

// client bootstraps one browser tab against the served lane.
func (f *productionLLMFixture) client(t *testing.T, tab string) *productionDurableClient {
	t.Helper()
	return newProductionDurableClient(t, f.gateway.Config.Handler, tab)
}

// lease begins one execution lease and returns its receipt.
func (f *productionLLMFixture) lease(t *testing.T, client *productionDurableClient) productionLLMReceiptWire {
	t.Helper()
	return f.leaseFor(t, client, productionTestModelID)
}

func (f *productionLLMFixture) leaseFor(t *testing.T, client *productionDurableClient, modelID string) productionLLMReceiptWire {
	t.Helper()
	var wire struct {
		Success   bool                     `json:"success"`
		ErrorCode string                   `json:"errorCode"`
		Lease     productionLLMReceiptWire `json:"lease"`
	}
	client.decode(t, "llm:begin-execution-lease", []any{modelID}, &wire)
	if !wire.Success {
		t.Fatalf("llm:begin-execution-lease refused with %v", wire.ErrorCode)
	}
	if wire.Lease.LeaseID == "" {
		t.Fatal("llm:begin-execution-lease returned no receipt")
	}
	return wire.Lease
}

func (f *productionLLMFixture) generate(t *testing.T, client *productionDurableClient, body map[string]any) productionLLMAnswerWire {
	t.Helper()
	return productionLLMDecodeAnswer(t, client.call(t, "llm:generate", []any{body}))
}

// productionLLMDecodeAnswer decodes the operation envelope of one llm:generate answer.
func productionLLMDecodeAnswer(t *testing.T, response *httptest.ResponseRecorder) productionLLMAnswerWire {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("llm:generate answered %d", response.Code)
	}
	var envelope struct {
		Result productionLLMAnswerWire `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("the answer is not the operation envelope: %v", err)
	}
	return envelope.Result
}

// productionLLMCapabilityWire is the renderer's ModelExecutionCapabilityEvidence.
type productionLLMCapabilityWire struct {
	SubjectFingerprint string `json:"subjectFingerprint"`
	MaxOutputTokens    int64  `json:"maxOutputTokens"`
	ContextWindow      *int64 `json:"contextWindowTokens"`
}

// productionLLMReceiptWire is the renderer's ModelExecutionLeaseReceipt, decoded
// only to the fields these tests assert.
type productionLLMReceiptWire struct {
	LeaseID             string                      `json:"leaseId"`
	ModelID             string                      `json:"modelId"`
	Provider            string                      `json:"provider"`
	Protocol            string                      `json:"protocol"`
	ModelName           string                      `json:"modelName"`
	ModelRevision       string                      `json:"modelRevision"`
	EndpointFingerprint string                      `json:"endpointFingerprint"`
	CreatedAt           int64                       `json:"createdAt"`
	ExpiresAt           int64                       `json:"expiresAt"`
	CapabilityEvidence  productionLLMCapabilityWire `json:"capabilityEvidence"`
}

// productionLLMUsageWire is the renderer's TokenUsage.
type productionLLMUsageWire struct {
	PromptTokens     *float64 `json:"promptTokens"`
	CompletionTokens *float64 `json:"completionTokens"`
	TotalTokens      *float64 `json:"totalTokens"`
}

// productionLLMFailureWire is the renderer's provider-failure projection.
type productionLLMFailureWire struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

// productionLLMAnswerWire is the renderer's LLMResponse.
type productionLLMAnswerWire struct {
	Success      bool                      `json:"success"`
	Content      string                    `json:"content"`
	FinishReason string                    `json:"finishReason"`
	Error        string                    `json:"error"`
	Usage        *productionLLMUsageWire   `json:"usage"`
	HTTPFailure  *productionLLMFailureWire `json:"httpFailure"`
}

func productionLLMGenerateRequest(leaseID string) map[string]any {
	return map[string]any{
		"modelId":               productionTestModelID,
		"modelExecutionLeaseId": leaseID,
		"messages":              []any{map[string]any{"role": "user", "content": "写一段海边的风"}},
	}
}

// productionLLMSubscription is one open /api/events stream, read as raw frame
// blocks so a frame boundary can be asserted byte for byte.
type productionLLMSubscription struct {
	body   io.ReadCloser
	cancel context.CancelFunc
	blocks chan string
}

func (f *productionLLMFixture) subscribe(t *testing.T, client *productionDurableClient) *productionLLMSubscription {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.gateway.URL+"/api/events?client="+url.QueryEscape(client.tab), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	request.Header.Set("X-Novel-User", productionLLMTestPrincipal)
	request.Header.Set("Origin", "http://"+productionDurableAddress)
	request.AddCookie(client.cookie)
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		_ = response.Body.Close()
		t.Fatalf("event stream refused the session; status %d", response.StatusCode)
	}
	subscription := &productionLLMSubscription{body: response.Body, cancel: cancel, blocks: make(chan string, 64)}
	go subscription.read()
	t.Cleanup(func() { subscription.close() })
	return subscription
}

func (s *productionLLMSubscription) read() {
	reader := bufio.NewReader(s.body)
	var block strings.Builder
	for {
		line, err := reader.ReadString('\n')
		block.WriteString(line)
		if line == "\n" {
			select {
			case s.blocks <- block.String():
			default:
			}
			block.Reset()
		}
		if err != nil {
			close(s.blocks)
			return
		}
	}
}

func (s *productionLLMSubscription) close() {
	s.cancel()
	_ = s.body.Close()
}

// next returns the next event frame (never the ": connected" comment), or fails.
func (s *productionLLMSubscription) next(t *testing.T) string {
	t.Helper()
	deadline := time.After(productionLLMTestFrameTimeout)
	for {
		select {
		case block, ok := <-s.blocks:
			if !ok {
				t.Fatal("the event stream closed before the expected frame")
			}
			if strings.HasPrefix(block, ":") {
				continue
			}
			return block
		case <-deadline:
			t.Fatal("timed out waiting for an event frame")
		}
	}
}

// quiet proves nothing else arrives on this stream.
func (s *productionLLMSubscription) quiet(t *testing.T, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		select {
		case block, ok := <-s.blocks:
			if !ok {
				return
			}
			if strings.HasPrefix(block, ":") {
				continue
			}
			t.Fatalf("unexpected extra event frame after the run settled: %v", block)
		case <-deadline:
			return
		}
	}
}

// productionLLMFrame decodes one wire frame into its channel and single argument.
func productionLLMFrame(t *testing.T, block string) (string, map[string]any) {
	t.Helper()
	trimmed := strings.TrimSuffix(block, "\n\n")
	lines := strings.Split(trimmed, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "id: ") || !strings.HasPrefix(lines[1], "data: ") {
		t.Fatalf("frame is not one id/data block: %v", block)
	}
	if lines[0] == "id: " {
		t.Fatal("frame carried no event id")
	}
	var wire struct {
		Channel string           `json:"channel"`
		Args    []map[string]any `json:"args"`
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "data: ")), &wire); err != nil {
		t.Fatalf("frame data is not JSON: %v", err)
	}
	if wire.Channel == "" || len(wire.Args) != 1 {
		t.Fatalf("frame does not carry one channel and one argument: %v", block)
	}
	return wire.Channel, wire.Args[0]
}

// productionLLMTestPrincipal is the identity the shared durable fixture uses.
const productionLLMTestPrincipal = "production-author"

// productionLLMWaitFor polls until the condition holds or the window expires. It
// is used only for facts that settle on another goroutine (the provider handler
// observing its own cancellation, the run's deferred cleanup), never to paper over
// a missing event.
func productionLLMWaitFor(window time.Duration, condition func() bool) bool {
	deadline := time.Now().Add(window)
	for {
		if condition() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// productionLLMStreamCount reports how many streaming runs this lane still holds.
func productionLLMStreamCount(execution *productionLLMExecution) int {
	execution.mu.Lock()
	defer execution.mu.Unlock()
	return len(execution.streams)
}

// TestLLMExecutionBindsTheFiveNames proves the family is installed and that the
// served surface is exactly the five names the renderer sends.
func TestLLMExecutionBindsTheFiveNames(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	// A fresh server, so the assertion covers the family's own bind loop rather
	// than the fixture. Nothing streams on the rebound server.
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-admitted")
	if err := fixture.execution.Bind(server); err != nil {
		t.Fatalf("binding the family a second time failed: %v", err)
	}
	bound := make(map[string]bool, 8)
	for _, name := range server.BoundOperations() {
		bound[name] = true
	}
	for _, name := range []string{
		"llm:begin-execution-lease",
		"llm:generate",
		"llm:generate-stream",
		"llm:cancel",
		"llm:close-execution-lease",
	} {
		if !bound[name] {
			t.Fatalf("%s is not bound", name)
		}
	}
}

// TestLLMGenerateReturnsModelTextAndUsage proves the non-streaming answer is the
// provider's text plus the usage receipt the llm-call ledger records.
func TestLLMGenerateReturnsModelTextAndUsage(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubCompletion("海风把盐吹进衣领")
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)

	answer := fixture.generate(t, client, productionLLMGenerateRequest(receipt.LeaseID))
	if !answer.Success || answer.Error != "" {
		t.Fatalf("generate refused: success=%v error=%v", answer.Success, answer.Error)
	}
	if answer.Content != "海风把盐吹进衣领" {
		t.Fatalf("answer text was not the model's text: %v", answer.Content)
	}
	if answer.FinishReason != "stop" {
		t.Fatalf("finish reason is not stop but %v", answer.FinishReason)
	}
	if answer.Usage == nil || answer.Usage.PromptTokens == nil || answer.Usage.CompletionTokens == nil || answer.Usage.TotalTokens == nil {
		t.Fatal("the answer carries no complete token usage")
	}
	if *answer.Usage.PromptTokens != 3 || *answer.Usage.CompletionTokens != 7 || *answer.Usage.TotalTokens != 10 {
		t.Fatalf("token usage was not the provider's: %v/%v/%v",
			*answer.Usage.PromptTokens, *answer.Usage.CompletionTokens, *answer.Usage.TotalTokens)
	}
	if answer.HTTPFailure != nil {
		t.Fatalf("a successful answer carried a provider failure: %d", answer.HTTPFailure.Status)
	}
	if fixture.provider.count() != 1 {
		t.Fatalf("the provider was dialed %d times; want one", fixture.provider.count())
	}
	// The receipt is the frozen profile the answer was produced under.
	if receipt.ModelID != productionTestModelID || receipt.Provider != "custom" || receipt.Protocol != "openai" {
		t.Fatalf("receipt does not describe the committed profile: %v %v %v", receipt.ModelID, receipt.Provider, receipt.Protocol)
	}
	if len(receipt.ModelRevision) != 64 || len(receipt.EndpointFingerprint) != 64 || len(receipt.CapabilityEvidence.SubjectFingerprint) != 64 {
		t.Fatal("receipt hashes are not the renderer's 64-hex fingerprints")
	}
	if receipt.CapabilityEvidence.MaxOutputTokens != 1024 || receipt.ExpiresAt <= receipt.CreatedAt {
		t.Fatalf("receipt bounds are wrong: output=%d expiry=%d created=%d",
			receipt.CapabilityEvidence.MaxOutputTokens, receipt.ExpiresAt, receipt.CreatedAt)
	}
}

// TestLLMGenerateStreamPublishesTheRendererFrames asserts one whole frame byte for
// byte, because the renderer parses its own SSE grammar and a frame that is only
// "equivalent" is a broken frame.
func TestLLMGenerateStreamPublishesTheRendererFrames(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubStream([]string{"Hel", "lo"}, false)
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)
	subscription := fixture.subscribe(t, client)

	var wire struct {
		RequestID string `json:"requestId"`
		Started   bool   `json:"started"`
		Error     string `json:"error"`
	}
	client.decode(t, "llm:generate-stream", []any{productionLLMTestStreamRequest,
		productionLLMGenerateRequest(receipt.LeaseID)}, &wire)
	if !wire.Started || wire.RequestID != productionLLMTestStreamRequest {
		t.Fatalf("stream was not started: %v %v", wire.Started, wire.Error)
	}

	first := subscription.next(t)
	want := "id: 1\ndata: {\"channel\":\"llm:stream-chunk\",\"args\":[{\"chunk\":\"Hel\",\"requestId\":\"" +
		productionLLMTestStreamRequest + "\"}]}\n\n"
	if first != want {
		t.Fatalf("first frame is not the renderer's frame boundary:\n got %v\nwant %v", first, want)
	}
	channel, second := productionLLMFrame(t, subscription.next(t))
	if channel != "llm:stream-chunk" || second["chunk"] != "lo" {
		t.Fatalf("second frame is not the second delta: %v %v", channel, second["chunk"])
	}

	channel, done := productionLLMFrame(t, subscription.next(t))
	if channel != "llm:stream-done" {
		t.Fatalf("the run did not settle with stream-done but with %v", channel)
	}
	if done["fullText"] != "Hello" || done["finishReason"] != "stop" {
		t.Fatalf("settled frame is wrong: %v %v", done["fullText"], done["finishReason"])
	}
	usage, ok := done["usage"].(map[string]any)
	if !ok || usage["totalTokens"] != float64(10) {
		t.Fatalf("settled frame carries no usage: %v", done["usage"])
	}
	subscription.quiet(t, 250*time.Millisecond)
}

// TestLLMCancelSettlesTheRunAndWritesNoArtifact proves llm:cancel stops the
// in-flight provider call (the provider itself observes the cancellation), settles
// the run with exactly one terminal frame, and leaves the author database alone.
func TestLLMCancelSettlesTheRunAndWritesNoArtifact(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubStream([]string{"Hel"}, true)
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)
	before := productionLLMDatabaseDigest(t, fixture.root)
	subscription := fixture.subscribe(t, client)

	var started struct {
		Started bool `json:"started"`
	}
	client.decode(t, "llm:generate-stream", []any{productionLLMTestCancelRequest,
		productionLLMGenerateRequest(receipt.LeaseID)}, &started)
	if !started.Started {
		t.Fatal("the stream did not start")
	}
	channel, chunk := productionLLMFrame(t, subscription.next(t))
	if channel != "llm:stream-chunk" || chunk["chunk"] != "Hel" {
		t.Fatalf("first frame is not the partial chunk: %v %v", channel, chunk["chunk"])
	}

	var cancelled struct {
		Success bool `json:"success"`
	}
	client.decode(t, "llm:cancel", []any{productionLLMTestCancelRequest}, &cancelled)
	if !cancelled.Success {
		t.Fatal("llm:cancel answered success:false for a running stream")
	}

	channel, settled := productionLLMFrame(t, subscription.next(t))
	if channel != "llm:stream-done" {
		t.Fatalf("a cancelled run settled with %v instead of stream-done", channel)
	}
	if settled["finishReason"] != "cancelled" {
		t.Fatalf("a cancelled run reported %v instead of cancelled", settled["finishReason"])
	}
	if settled["fullText"] != "Hel" {
		t.Fatalf("a cancelled run lost its partial text: %v", settled["fullText"])
	}
	subscription.quiet(t, 250*time.Millisecond)
	if !productionLLMWaitFor(2*time.Second, func() bool { return fixture.provider.cancellations() == 1 }) {
		t.Fatalf("the provider saw %d cancellations; want the call to stop", fixture.provider.cancellations())
	}
	if !productionLLMWaitFor(2*time.Second, func() bool { return productionLLMStreamCount(fixture.execution) == 0 }) {
		t.Fatalf("%d streaming runs are still registered after the cancel", productionLLMStreamCount(fixture.execution))
	}
	if after := productionLLMDatabaseDigest(t, fixture.root); after != before {
		t.Fatal("a cancelled run changed the author database")
	}
}

// TestLLMExpiredLeaseRefuses proves expiry is enforced by the lease, not by the
// provider: the request never leaves the server.
func TestLLMExpiredLeaseRefuses(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubCompletion("不该出现")
	client := fixture.client(t, productionTestTab)
	// A one-nanosecond lease expires in the same millisecond it was created, so the
	// next request is already past its expiry with no sleep and no clock seam.
	registry, err := llmlease.New(llmlease.Options{
		LoadModel: fixture.execution.loadModel,
		TTL:       time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.execution.leases = registry
	receipt := fixture.lease(t, client)

	answer := fixture.generate(t, client, productionLLMGenerateRequest(receipt.LeaseID))
	if answer.Success || answer.Error != "MODEL_EXECUTION_LEASE_EXPIRED" {
		t.Fatalf("an expired lease did not refuse by name: success=%v error=%v", answer.Success, answer.Error)
	}
	if answer.Content != "" {
		t.Fatal("a refused request still carried model text")
	}
	if fixture.provider.count() != 0 {
		t.Fatalf("an expired lease still reached the provider %d times", fixture.provider.count())
	}
}

// TestLLMForeignLeaseRefuses proves the lease is bound to the tab that opened it:
// a second tab of the same browser session cannot spend another tab's lease.
func TestLLMForeignLeaseRefuses(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	fixture.provider.serve = productionLLMStubCompletion("不该出现")
	owner := fixture.client(t, productionTestTab)
	other := fixture.client(t, "client_0987654321")
	receipt := fixture.lease(t, owner)

	answer := fixture.generate(t, other, productionLLMGenerateRequest(receipt.LeaseID))
	if answer.Success || answer.Error != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("another tab's lease was accepted: success=%v error=%v", answer.Success, answer.Error)
	}
	if fixture.provider.count() != 0 {
		t.Fatalf("a foreign lease still reached the provider %d times", fixture.provider.count())
	}
	// The owner still holds a usable lease: a refusal must not consume it.
	if answer := fixture.generate(t, owner, productionLLMGenerateRequest(receipt.LeaseID)); !answer.Success {
		t.Fatalf("the owner's lease stopped working after a foreign attempt: %v", answer.Error)
	}
}

// TestLLMGenerateNeverAnswersWithTheCredential scans the whole HTTP answer for the
// committed key, including a provider failure whose own body echoes it back.
func TestLLMGenerateNeverAnswersWithTheCredential(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)

	fixture.provider.serve = productionLLMStubCompletion("没问题")
	success := client.call(t, "llm:generate", []any{productionLLMGenerateRequest(receipt.LeaseID)})
	if success.Code != http.StatusOK {
		t.Fatalf("a successful generate answered %d", success.Code)
	}
	if strings.Contains(success.Body.String(), productionTestModelSecret) {
		t.Fatal("the credential appears in a successful answer body")
	}

	// The hostile case: the provider rejects the key and quotes it back.
	fixture.provider.serve = func(_ *productionLLMStub, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided: `+productionTestModelSecret+`"}}`)
	}
	failure := client.call(t, "llm:generate", []any{productionLLMGenerateRequest(receipt.LeaseID)})
	if failure.Code != http.StatusOK {
		t.Fatalf("a provider failure answered %d instead of a business result", failure.Code)
	}
	if strings.Contains(failure.Body.String(), productionTestModelSecret) {
		t.Fatal("the credential appears in a provider-failure answer body")
	}
	answer := productionLLMDecodeAnswer(t, failure)
	if answer.Success {
		t.Fatal("a provider 500 was reported as success")
	}
	if answer.Error != "LLM_REQUEST_FAILED" {
		t.Fatalf("a provider 500 did not map to one symbolic code but to %v", answer.Error)
	}
	if answer.HTTPFailure == nil {
		t.Fatal("a provider 500 answer lost its failure projection")
	}
	if answer.HTTPFailure.Status != http.StatusInternalServerError {
		t.Fatalf("the answer reported provider status %d", answer.HTTPFailure.Status)
	}
	if answer.HTTPFailure.Body != "" {
		t.Fatal("the answer carried the provider body, which can quote the credential")
	}
	if answer.FinishReason != "error" {
		t.Fatalf("a failed answer reported finish reason %v", answer.FinishReason)
	}
}

// TestLLMInvalidProviderBodyMapsToSymbolicCode proves an unusable reply becomes one
// symbolic code, never the provider's own text.
func TestLLMInvalidProviderBodyMapsToSymbolicCode(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	client := fixture.client(t, productionTestTab)
	receipt := fixture.lease(t, client)
	const driverText = "provider-said-something-unusable"
	fixture.provider.serve = func(_ *productionLLMStub, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, driverText)
	}
	response := client.call(t, "llm:generate", []any{productionLLMGenerateRequest(receipt.LeaseID)})
	if response.Code != http.StatusOK {
		t.Fatalf("an unusable reply answered %d instead of a business result", response.Code)
	}
	if strings.Contains(response.Body.String(), driverText) {
		t.Fatal("the provider's own text reached the answer")
	}
	answer := productionLLMDecodeAnswer(t, response)
	if answer.Error != "LLM_RESPONSE_INVALID" || answer.FinishReason != "error" {
		t.Fatalf("an unusable reply mapped to %v/%v", answer.Error, answer.FinishReason)
	}
}

// TestLLMRefusedRequestLeavesTheDatabaseByteIdentical proves a refused request is
// inert: the author database is read over its own read-only connection before and
// after, and the file digest does not move.
func TestLLMRefusedRequestLeavesTheDatabaseByteIdentical(t *testing.T) {
	fixture := newProductionLLMFixture(t)
	client := fixture.client(t, productionTestTab)
	before := productionLLMDatabaseDigest(t, fixture.root)

	// No lease at all: the family refuses by name.
	answer := fixture.generate(t, client, productionLLMGenerateRequest(""))
	// An unleased request is refused with the renderer's own lease code.
	if answer.Success || answer.Error != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("an unleased request was not refused by name: success=%v error=%v", answer.Success, answer.Error)
	}
	// And a lease that does not exist is refused the same way.
	answer = fixture.generate(t, client, productionLLMGenerateRequest("00000000-0000-4000-8000-000000000000"))
	if answer.Success || answer.Error != "MODEL_EXECUTION_LEASE_INVALID" {
		t.Fatalf("an unknown lease was not refused by name: success=%v error=%v", answer.Success, answer.Error)
	}
	if fixture.provider.count() != 0 {
		t.Fatalf("a refused request reached the provider %d times", fixture.provider.count())
	}
	if after := productionLLMDatabaseDigest(t, fixture.root); after != before {
		t.Fatal("a refused request changed the author database")
	}
}

// productionLLMDatabaseDigest reads the admitted project database over a separate
// read-only connection and hashes the file, so "nothing was written" is proven by
// bytes rather than by intent.
func productionLLMDatabaseDigest(t *testing.T, root string) string {
	t.Helper()
	database := filepath.Join(root, productionLibraryDirectory, "current-novel", projectfs.StorageDirectory, projectfs.DatabaseName)
	raw, err := os.ReadFile(database)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	db, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	var parents int
	if err := db.QueryRow("SELECT COUNT(*) FROM parents").Scan(&parents); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(sum[:]) + "|user_version=" + strconv.Itoa(version) +
		",parents=" + strconv.Itoa(parents)
}

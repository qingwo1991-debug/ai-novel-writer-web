// Package llm implements native provider protocols. It never commits author
// content; semantic stop evidence is separate from transport termination.
package llm

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Mapping struct {
	Adapter string `json:"adapter"`
}
type Model struct {
	Provider         string `json:"provider"`
	ModelName        string `json:"modelName"`
	BaseURL          string `json:"baseUrl"`
	APIKey           string `json:"apiKey"`
	MaxTokens        int64  `json:"maxTokens"`
	MaxTokensMissing bool   `json:"-"`
	// Mapping must be resolved by the future model-policy owner, not raw RPC.
	SyntheticMapping *Mapping `json:"syntheticMapping,omitempty"`
}
type Reasoning struct {
	Adapter               string `json:"adapter"`
	ReasoningEffort       string `json:"reasoningEffort,omitempty"`
	ThinkingBudget        int64  `json:"thinkingBudget,omitempty"`
	ThinkingBudgetMissing bool   `json:"-"`
	ThinkingLevel         string `json:"thinkingLevel,omitempty"`
	Thinking              string `json:"thinking,omitempty"`
}
type Options struct {
	Temperature          *float64       `json:"temperature,omitempty"`
	TemperatureNull      bool           `json:"-"`
	MaxTokens            *int64         `json:"maxTokens,omitempty"`
	OutputTokenParameter string         `json:"outputTokenParameter,omitempty"`
	ResponseFormat       map[string]any `json:"responseFormat,omitempty"`
	Reasoning            *Reasoning     `json:"reasoning,omitempty"`
	ConversationID       string         `json:"conversationId,omitempty"`
	VisibleOnly          bool           `json:"visibleOnly,omitempty"`
}
type Usage struct {
	PromptTokens     *float64 `json:"promptTokens"`
	CompletionTokens *float64 `json:"completionTokens"`
	TotalTokens      *float64 `json:"totalTokens"`
}

// UsageEvidence is untrusted protocol evidence, NOT normalized billing authority.
// Gemini historically forwards raw malformed fields, unlike normalized Usage.
type UsageEvidence struct {
	Usage                  any    `json:"usage"`
	ReasoningTokens        any    `json:"reasoningTokens"`
	Accounting             string `json:"accounting"`
	TotalIncludesReasoning bool   `json:"totalIncludesReasoning"`
	Protocol               string `json:"protocol"`
}
type HTTPFailure struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}
type Response struct {
	Success      bool         `json:"success"`
	Content      string       `json:"content"`
	FinishReason string       `json:"finishReason"`
	Error        string       `json:"error,omitempty"`
	Usage        *Usage       `json:"usage,omitempty"`
	HTTPFailure  *HTTPFailure `json:"httpFailure,omitempty"`
}
type Callbacks struct {
	OnChunk         func(string)
	OnReasoning     func(string)
	OnUsageEvidence func(UsageEvidence)
	OnDone          func(string, *Usage, string)
	OnError         func(string, string, *Usage)
}

// Dispatch is mandatory. Its owner must validate policy and resolved addresses
// at every dispatch. Passing http.DefaultClient for untrusted URLs is unsafe.
// This API is deliberately NOT wired to RPC until that owner is implemented.
type Dispatch func(*http.Request) (*http.Response, error)
type OpenAI struct{ Dispatch Dispatch }

const maxProviderBytes = 64 * 1024 * 1024

func ResolveOpenAIURL(base, provider string) (string, error) {
	endpoint, err := url.Parse(strings.TrimSpace(base))
	if err != nil || endpoint == nil || endpoint.Host == "" || (endpoint.Scheme != "https" && endpoint.Scheme != "http") || endpoint.User != nil {
		return "", errors.New("LLM_ENDPOINT_INVALID")
	}
	path := strings.TrimRight(endpoint.Path, "/")
	switch {
	case strings.HasSuffix(path, "/chat/completions"):
	case strings.HasSuffix(path, "/chat"):
		path += "/completions"
	case path == "" || provider == "novelai":
		path += "/v1/chat/completions"
	default:
		path += "/chat/completions"
	}
	endpoint.Path = path
	endpoint.RawPath = ""
	return endpoint.String(), nil
}
func FinishReason(reason string) string {
	switch reason {
	case "stop":
		return "stop"
	case "length", "model_context_window_exceeded":
		return "length"
	case "content_filter", "sensitive":
		return "content_filter"
	case "network_error":
		return "error"
	default:
		return "unknown"
	}
}
func BuildOpenAIRequest(ctx context.Context, model Model, messages []Message, opts Options, stream bool) (*http.Request, error) {
	endpoint, err := ResolveOpenAIURL(model.BaseURL, model.Provider)
	if err != nil {
		return nil, err
	}
	maxTokens := model.MaxTokens
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}
	parameter := "max_tokens"
	if opts.OutputTokenParameter != "" {
		if opts.OutputTokenParameter != "max_completion_tokens" {
			return nil, errors.New("LLM_PARAMETER_INVALID")
		}
		parameter = opts.OutputTokenParameter
	}
	body := map[string]any{"model": model.ModelName, "messages": messages, parameter: maxTokens, "stream": stream}
	if opts.Temperature != nil {
		body["temperature"] = *opts.Temperature
	} else if opts.TemperatureNull {
		body["temperature"] = nil
	}
	novel := model.Provider == "novelai"
	if r := opts.Reasoning; r != nil {
		mapping := ""
		if model.SyntheticMapping != nil {
			mapping = model.SyntheticMapping.Adapter
		}
		switch r.Adapter {
		case "openai-reasoning-effort":
			if !novel {
				body["reasoning_effort"] = r.ReasoningEffort
			}
		case "openai-thinking-budget":
			if !novel && mapping == r.Adapter {
				body["enable_thinking"] = r.ThinkingBudget > 0
				if r.ThinkingBudget > 0 {
					body["thinking_budget"] = r.ThinkingBudget
				}
			}
		case "siliconflow-v4-thinking":
			if mapping == r.Adapter {
				body["enable_thinking"] = true
				body["reasoning_effort"] = r.ReasoningEffort
			}
		case "deepseek-v4-thinking":
			if mapping == r.Adapter {
				body["thinking"] = map[string]string{"type": r.Thinking}
				if r.Thinking == "enabled" && r.ReasoningEffort != "" {
					body["reasoning_effort"] = r.ReasoningEffort
				}
			}
		}
	}
	if !novel {
		if opts.ResponseFormat != nil {
			body["response_format"] = opts.ResponseFormat
		}
		if stream {
			body["stream_options"] = map[string]bool{"include_usage": true}
		}
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, errors.New("LLM_REQUEST_INVALID")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(encoded)))
	if err != nil {
		return nil, errors.New("LLM_REQUEST_INVALID")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+model.APIKey)
	base, _ := url.Parse(strings.TrimSpace(model.BaseURL))
	path := strings.TrimRight(base.Path, "/")
	if base.Scheme == "https" && base.Hostname() == "opencode.ai" && (path == "/zen/go" || strings.HasPrefix(path, "/zen/go/")) {
		conversation := opts.ConversationID
		if conversation == "" {
			var bytes [16]byte
			if _, err := rand.Read(bytes[:]); err != nil {
				return nil, errors.New("LLM_RANDOM_FAILED")
			}
			bytes[6] = (bytes[6] & 15) | 64
			bytes[8] = (bytes[8] & 63) | 128
			raw := hex.EncodeToString(bytes[:])
			conversation = raw[:8] + "-" + raw[8:12] + "-" + raw[12:16] + "-" + raw[16:20] + "-" + raw[20:]
		}
		request.Header.Set("x-opencode-session", conversation)
		request.Header.Set("User-Agent", "ai-novel-writer/1.4.0") // Must track appVersion; oracle asserts repository version.
	}
	return request, nil
}
func (p *OpenAI) do(ctx context.Context, model Model, messages []Message, opts Options, stream bool) (*http.Response, error) {
	if p.Dispatch == nil {
		return nil, errors.New("LLM_DISPATCH_REQUIRED")
	}
	request, err := BuildOpenAIRequest(ctx, model, messages, opts, stream)
	if err != nil {
		return nil, err
	}
	response, err := p.Dispatch(request)
	if err != nil {
		if response != nil && response.Body != nil {
			response.Body.Close()
		}
		return nil, err
	}
	if response == nil || response.Body == nil {
		return nil, errors.New("LLM_STREAM_UNREADABLE")
	}
	return response, nil
}
func limitedBody(body io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxProviderBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxProviderBytes {
		return nil, errors.New("LLM_RESPONSE_TOO_LARGE")
	}
	return data, nil
}

var thinkBlock = regexp.MustCompile(`(?is)<think>.*?(?:</think>|$)`)
var thinkPrefix = regexp.MustCompile(`(?is)^.*?</think>\s*`)
var thinkTag = regexp.MustCompile(`(?i)</?think>`)

func jsTrim(content string) string {
	return strings.TrimFunc(content, func(r rune) bool {
		return r == '\u0009' || r == '\u000a' || r == '\u000b' || r == '\u000c' || r == '\u000d' || r == '\u0020' || r == '\u00a0' || r == '\u1680' || (r >= '\u2000' && r <= '\u200a') || r == '\u2028' || r == '\u2029' || r == '\u202f' || r == '\u205f' || r == '\u3000' || r == '\ufeff'
	})
}
func stripThinking(content string) string {
	return jsTrim(thinkTag.ReplaceAllString(thinkPrefix.ReplaceAllString(thinkBlock.ReplaceAllString(content, ""), ""), ""))
}
func number(value any) *float64 {
	v, ok := value.(float64)
	if !ok {
		return nil
	}
	return &v
}
func usageFrom(raw map[string]any) *Usage {
	if raw == nil {
		return nil
	}
	u := Usage{number(raw["prompt_tokens"]), number(raw["completion_tokens"]), number(raw["total_tokens"])}
	if u.PromptTokens == nil || u.CompletionTokens == nil || u.TotalTokens == nil {
		return nil
	}
	return &u
}
func (p *OpenAI) Generate(ctx context.Context, model Model, messages []Message, opts Options) Response {
	fail := func(code string) Response { return Response{Content: "", FinishReason: "error", Error: code} }
	response, err := p.do(ctx, model, messages, opts, false)
	if err != nil {
		if ctx.Err() != nil {
			return fail("LLM_CANCELLED")
		}
		return fail("LLM_REQUEST_FAILED")
	}
	defer response.Body.Close()
	data, err := limitedBody(response.Body)
	if ctx.Err() != nil {
		return fail("LLM_CANCELLED")
	}
	if err != nil {
		return fail("LLM_RESPONSE_INVALID")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		r := fail("LLM_REQUEST_FAILED")
		r.HTTPFailure = &HTTPFailure{response.StatusCode, string(data)}
		return r
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil {
		return fail("LLM_RESPONSE_INVALID")
	}
	content, finish := "", "unknown"
	if choices, ok := payload["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			reason, _ := choice["finish_reason"].(string)
			finish = FinishReason(reason)
			if message, ok := choice["message"].(map[string]any); ok {
				content, _ = message["content"].(string)
			}
		}
	}
	raw, _ := payload["usage"].(map[string]any)
	r := Response{Success: finish == "stop", Content: stripThinking(content), FinishReason: finish, Usage: usageFrom(raw)}
	if !r.Success {
		r.Error = "LLM_RESPONSE_INCOMPLETE"
	}
	return r
}

var contextLength = regexp.MustCompile(`(?i)maximum context length is [0-9]+ tokens|input tokens \([0-9]+\) has exceeded max_seq_len|max_tokens \([0-9]+\) have exceeded max_seq_len|input token count \([0-9]+\) exceeds the maximum number of tokens`)

func (p *OpenAI) Stream(ctx context.Context, model Model, messages []Message, opts Options, callbacks Callbacks) {
	visible := NewVisibleFilter()
	var full strings.Builder
	var usage *Usage
	isThinking := false
	emit := func(text string) {
		if text != "" && callbacks.OnChunk != nil {
			callbacks.OnChunk(text)
		}
	}
	reasoning := func(text string) {
		if callbacks.OnReasoning != nil {
			callbacks.OnReasoning(text)
		}
	}
	inBand := NewInBandReasoning(reasoning)
	fail := func(code string) {
		if ctx.Err() != nil {
			code = "LLM_CANCELLED"
		}
		candidate := stripThinking(full.String())
		if opts.VisibleOnly {
			candidate = visible.Text()
		}
		if callbacks.OnError != nil {
			callbacks.OnError(code, candidate, usage)
		}
	}
	response, err := p.do(ctx, model, messages, opts, true)
	if err != nil {
		fail("LLM_STREAM_NETWORK_FAILED")
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		code := "LLM_REQUEST_FAILED"
		if response.StatusCode == 400 {
			body, err := limitedBody(response.Body)
			if err == nil && contextLength.Match(body) {
				code = "LLM_CONTEXT_LENGTH_EXCEEDED"
			}
		}
		fail(code)
		return
	}
	sawDone := false
	finish := "unknown"
	fatal := ""
	total := 0
	event := func(data string) bool {
		text := jsTrim(data)
		if text == "" {
			return true
		}
		if text == "[DONE]" {
			sawDone = true
			return false
		}
		var payload map[string]any
		if json.Unmarshal([]byte(text), &payload) != nil || payload == nil {
			fatal = "LLM_STREAM_INVALID"
			return false
		}
		if _, exists := payload["error"]; exists {
			fatal = "LLM_STREAM_PROVIDER_ERROR"
			return false
		}
		if raw, ok := payload["usage"].(map[string]any); ok {
			var tokens *float64
			if details, ok := raw["completion_tokens_details"].(map[string]any); ok {
				tokens = number(details["reasoning_tokens"])
			}
			if callbacks.OnUsageEvidence != nil {
				callbacks.OnUsageEvidence(UsageEvidence{Usage{number(raw["prompt_tokens"]), number(raw["completion_tokens"]), number(raw["total_tokens"])}, tokens, "included-in-completion", true, "openai"})
			}
			if u := usageFrom(raw); u != nil {
				usage = u
			}
		}
		raw, exists := payload["choices"]
		if !exists {
			return true
		}
		choices, ok := raw.([]any)
		if !ok {
			fatal = "LLM_STREAM_INVALID"
			return false
		}
		if len(choices) == 0 {
			return true
		}
		choice, ok := choices[0].(map[string]any)
		if !ok {
			fatal = "LLM_STREAM_INVALID"
			return false
		}
		if reason, exists := choice["finish_reason"]; exists && reason != nil {
			value, ok := reason.(string)
			if !ok {
				fatal = "LLM_STREAM_INVALID"
				return false
			}
			finish = FinishReason(value)
		}
		raw, exists = choice["delta"]
		if !exists {
			return true
		}
		delta, ok := raw.(map[string]any)
		if !ok {
			fatal = "LLM_STREAM_INVALID"
			return false
		}
		content, hasContent := "", false
		thought := ""
		for _, key := range []string{"reasoning_content", "content"} {
			value, exists := delta[key]
			if !exists || value == nil {
				continue
			}
			text, ok := value.(string)
			if !ok {
				fatal = "LLM_STREAM_INVALID"
				return false
			}
			if key == "content" {
				content = text
				hasContent = true
			} else {
				thought = text
			}
		}
		total += len(content) + len(thought)
		if total > maxProviderBytes {
			fatal = "LLM_STREAM_TOO_LARGE"
			return false
		}
		if thought != "" {
			reasoning(thought)
		}
		if hasContent {
			inBand.Push(content)
		}
		if opts.VisibleOnly {
			if hasContent {
				emit(visible.Push(content))
			}
			return true
		}
		chunk := ""
		if thought != "" {
			if !isThinking {
				isThinking = true
				chunk += "<think>\n"
			}
			chunk += thought
		}
		if hasContent {
			if isThinking {
				isThinking = false
				chunk += "\n</think>\n\n"
			}
			chunk += content
		}
		if chunk != "" {
			full.WriteString(chunk)
			emit(chunk)
		}
		return true
	}
	err = readProviderSSE(ctx, response.Body, event)
	if fatal != "" {
		fail(fatal)
		return
	}
	if err != nil {
		if errors.Is(err, errSSELimit) {
			fail("LLM_STREAM_TOO_LARGE")
		} else {
			fail("LLM_STREAM_NETWORK_FAILED")
		}
		return
	}
	if !sawDone {
		fail("LLM_STREAM_INCOMPLETE")
		return
	}
	if ctx.Err() != nil {
		fail("LLM_CANCELLED")
		return
	}
	if isThinking {
		closeTag := "\n</think>\n\n"
		full.WriteString(closeTag)
		emit(closeTag)
	}
	if ctx.Err() != nil {
		fail("LLM_CANCELLED")
		return
	}
	content := stripThinking(full.String())
	if opts.VisibleOnly {
		content = visible.Text()
	}
	if callbacks.OnDone != nil {
		callbacks.OnDone(content, usage, finish)
	}
}

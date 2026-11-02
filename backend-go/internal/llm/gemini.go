package llm

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
)

// Gemini uses the same policy-enforcing dispatch boundary as OpenAI. It is not
// an implicit network client and must not be wired to unvalidated raw URLs.
type Gemini struct{ Dispatch Dispatch }

func GeminiFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "BLOCKLIST", "PROHIBITED_CONTENT":
		return "content_filter"
	default:
		return "unknown"
	}
}

func BuildGeminiRequest(ctx context.Context, model Model, messages []Message, opts Options, stream bool) (*http.Request, error) {
	// The original removes exactly one trailing slash and does not add a model
	// field: the model name belongs in the endpoint path.
	endpoint := strings.TrimSuffix(model.BaseURL, "/") + "/v1beta/models/" + model.ModelName
	if stream {
		endpoint += ":streamGenerateContent?alt=sse"
	} else {
		endpoint += ":generateContent"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return nil, errors.New("LLM_ENDPOINT_INVALID")
	}
	maxTokens := model.MaxTokens
	if opts.MaxTokens != nil {
		maxTokens = *opts.MaxTokens
	}
	config := map[string]any{}
	if opts.MaxTokens != nil || !model.MaxTokensMissing {
		config["maxOutputTokens"] = maxTokens
	}
	if opts.Temperature != nil {
		config["temperature"] = *opts.Temperature
	} else if opts.TemperatureNull {
		config["temperature"] = nil
	}
	if opts.ResponseFormat["type"] == "json_object" {
		config["responseMimeType"] = "application/json"
	}
	if r := opts.Reasoning; r != nil {
		switch r.Adapter {
		case "gemini-thinking-budget":
			thinking := map[string]any{}
			if !r.ThinkingBudgetMissing {
				thinking["thinkingBudget"] = r.ThinkingBudget
			}
			config["thinkingConfig"] = thinking
		case "gemini-thinking-level":
			config["thinkingConfig"] = map[string]any{"thinkingLevel": r.ThinkingLevel}
		}
	}
	contents := []any{}
	system := ""
	for _, message := range messages {
		if message.Role == "system" {
			system = message.Content
			continue
		}
		role := "user"
		if message.Role == "assistant" {
			role = "model"
		}
		contents = append(contents, map[string]any{"role": role, "parts": []any{map[string]string{"text": message.Content}}})
	}
	body := map[string]any{"contents": contents, "generationConfig": config}
	if system != "" {
		body["systemInstruction"] = map[string]any{"parts": []any{map[string]string{"text": system}}}
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
	request.Header.Set("x-goog-api-key", model.APIKey)
	return request, nil
}

func (p *Gemini) do(ctx context.Context, model Model, messages []Message, opts Options, stream bool) (*http.Response, error) {
	if p.Dispatch == nil {
		return nil, errors.New("LLM_DISPATCH_REQUIRED")
	}
	request, err := BuildGeminiRequest(ctx, model, messages, opts, stream)
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

func geminiSafeCount(value any) *float64 {
	count := number(value)
	if count == nil || *count < 0 || *count > 9007199254740991 || math.Trunc(*count) != *count {
		return nil
	}
	return count
}

// Missing completion components count as zero, but explicit null and invalid
// components invalidate the sum. The original does NOT revalidate the sum as a
// safe integer after adding two individually safe counts.
func geminiUsage(raw map[string]any) *Usage {
	candidates, candidatesPresent := raw["candidatesTokenCount"]
	thoughts, thoughtsPresent := raw["thoughtsTokenCount"]
	candidateCount, thoughtCount := geminiSafeCount(candidates), geminiSafeCount(thoughts)
	zero := float64(0)
	if !candidatesPresent {
		candidateCount = &zero
	}
	if !thoughtsPresent {
		thoughtCount = &zero
	}
	var completion *float64
	if (candidatesPresent || thoughtsPresent) && candidateCount != nil && thoughtCount != nil {
		sum := *candidateCount + *thoughtCount
		completion = &sum
	}
	return &Usage{PromptTokens: geminiSafeCount(raw["promptTokenCount"]), CompletionTokens: completion, TotalTokens: geminiSafeCount(raw["totalTokenCount"])}
}

// JSON truthiness matters here: even [] is truthy in JavaScript, whereas null,
// false, zero and the empty string do not report usage. Non-record truthy usage
// metadata has no named token properties, yielding an all-null normalized usage.
func geminiMetadata(payload map[string]any) (map[string]any, bool) {
	value := payload["usageMetadata"]
	if value == nil || value == false || value == "" || value == float64(0) {
		return nil, false
	}
	raw, _ := value.(map[string]any)
	return raw, true
}

func geminiCandidate(payload map[string]any) map[string]any {
	candidates, _ := payload["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	return candidate
}

func (p *Gemini) Generate(ctx context.Context, model Model, messages []Message, opts Options) Response {
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
		result := fail("LLM_REQUEST_FAILED")
		result.HTTPFailure = &HTTPFailure{response.StatusCode, string(data)}
		return result
	}
	var payload map[string]any
	if json.Unmarshal(data, &payload) != nil || payload == nil {
		return fail("LLM_RESPONSE_INVALID")
	}
	candidate := geminiCandidate(payload)
	reason, _ := candidate["finishReason"].(string)
	finish := GeminiFinishReason(reason)
	content := ""
	if body, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := body["parts"].([]any); ok && len(parts) > 0 {
			if part, ok := parts[0].(map[string]any); ok {
				content, _ = part["text"].(string)
			}
		}
	}
	var usage *Usage
	if raw, ok := geminiMetadata(payload); ok {
		usage = geminiUsage(raw)
	}
	if ctx.Err() != nil {
		return fail("LLM_CANCELLED")
	}
	result := Response{Success: finish == "stop", Content: content, FinishReason: finish, Usage: usage}
	if !result.Success {
		result.Error = "LLM_RESPONSE_INCOMPLETE"
	}
	return result
}

// Validate all candidates, including unused alternatives, exactly as the
// original visible-only owner does. Missing properties differ from JSON null.
func validateGeminiVisible(payload map[string]any) string {
	if payload == nil {
		return "LLM_STREAM_INVALID"
	}
	if _, exists := payload["error"]; exists {
		return "LLM_STREAM_PROVIDER_ERROR"
	}
	raw, exists := payload["candidates"]
	if !exists {
		return ""
	}
	candidates, ok := raw.([]any)
	if !ok {
		return "LLM_STREAM_INVALID"
	}
	for _, rawCandidate := range candidates {
		candidate, ok := rawCandidate.(map[string]any)
		if !ok {
			return "LLM_STREAM_INVALID"
		}
		if value, exists := candidate["finishReason"]; exists && value != nil {
			if _, ok := value.(string); !ok {
				return "LLM_STREAM_INVALID"
			}
		}
		rawContent, exists := candidate["content"]
		if !exists {
			continue
		}
		content, ok := rawContent.(map[string]any)
		if !ok {
			return "LLM_STREAM_INVALID"
		}
		rawParts, exists := content["parts"]
		if !exists {
			continue
		}
		parts, ok := rawParts.([]any)
		if !ok {
			return "LLM_STREAM_INVALID"
		}
		for _, rawPart := range parts {
			part, ok := rawPart.(map[string]any)
			if !ok {
				return "LLM_STREAM_INVALID"
			}
			if value, exists := part["text"]; exists {
				if _, ok := value.(string); !ok {
					return "LLM_STREAM_INVALID"
				}
			}
			if value, exists := part["thought"]; exists {
				if _, ok := value.(bool); !ok {
					return "LLM_STREAM_INVALID"
				}
			}
		}
	}
	return ""
}

// Gemini's original is not an SSE event parser: it processes each LF-delimited
// line starting with exactly "data: ", including the last unterminated line.
// Do not replace this with readProviderSSE: multiline joining, optional spaces,
// and CR-only delimiters would change both legacy and visible-only contracts.
func readGeminiLines(ctx context.Context, source io.Reader, process func(string) bool) error {
	reader := bufio.NewReaderSize(source, 32*1024)
	line := []byte{}
	wire := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fragment, err := reader.ReadSlice('\n')
		wire += len(fragment)
		if wire > maxProviderBytes || len(line)+len(fragment) > maxSSEEventBytes {
			return errSSELimit
		}
		line = append(line, fragment...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		if len(line) > 0 {
			text := strings.ToValidUTF8(strings.TrimSuffix(string(line), "\n"), "\ufffd")
			if !process(text) {
				return nil
			}
			line = line[:0]
		}
		if err == io.EOF {
			return nil
		}
	}
}

func (p *Gemini) Stream(ctx context.Context, model Model, messages []Message, opts Options, callbacks Callbacks) {
	visible := NewVisibleFilter()
	var full strings.Builder
	var usage *Usage
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
		candidate := "" // Legacy errors deliberately do not expose partial text.
		if opts.VisibleOnly {
			candidate = visible.Text()
		}
		if callbacks.OnError != nil {
			callbacks.OnError(code, candidate, usage)
		}
	}
	response, err := p.do(ctx, model, messages, opts, true)
	if err != nil {
		if err.Error() == "LLM_STREAM_UNREADABLE" {
			fail("LLM_STREAM_UNREADABLE")
		} else {
			fail("LLM_STREAM_NETWORK_FAILED")
		}
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
	finish, fatal := "unknown", ""
	candidateBytes := 0
	process := func(line string) bool {
		if !strings.HasPrefix(line, "data: ") {
			return true
		}
		text := jsTrim(line[6:])
		if text == "" {
			return true
		}
		var payload map[string]any
		if json.Unmarshal([]byte(text), &payload) != nil || payload == nil {
			if opts.VisibleOnly {
				fatal = "Error: LLM_STREAM_INVALID"
				return false
			}
			return true
		}
		if opts.VisibleOnly {
			if code := validateGeminiVisible(payload); code != "" {
				fatal = "Error: " + code
				return false
			}
		}
		candidate := geminiCandidate(payload)
		if raw, exists := candidate["finishReason"]; exists {
			reason, _ := raw.(string)
			finish = GeminiFinishReason(reason)
		}
		content, _ := candidate["content"].(map[string]any)
		rawParts := content["parts"]
		parts, partsAreArray := rawParts.([]any)
		if rawParts != nil && !partsAreArray {
			// Legacy for-of/filter on a non-array fails before usage emission.
			return true
		}
		var chunk strings.Builder
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			if !ok {
				// null throws on property access; other JSON primitives and
				// arrays have no named thought/text properties and are inert.
				if raw == nil {
					return true
				}
				continue
			}
			text, _ := part["text"].(string)
			candidateBytes += len(text)
			if candidateBytes > maxProviderBytes {
				fatal = "LLM_STREAM_TOO_LARGE"
				return false
			}
			if thought, _ := part["thought"].(bool); thought {
				if text != "" {
					reasoning(text)
				}
			} else {
				if _, ok := part["text"].(string); ok {
					inBand.Push(text)
				}
				chunk.WriteString(text)
			}
		}
		emitted := chunk.String()
		if opts.VisibleOnly {
			emitted = visible.Push(emitted)
		}
		if emitted != "" {
			if !opts.VisibleOnly {
				full.WriteString(emitted)
			}
			if callbacks.OnChunk != nil {
				callbacks.OnChunk(emitted)
			}
		}
		if raw, ok := geminiMetadata(payload); ok {
			usage = geminiUsage(raw)
			if callbacks.OnUsageEvidence != nil {
				callbacks.OnUsageEvidence(UsageEvidence{
					Usage: map[string]any{"promptTokens": raw["promptTokenCount"], "completionTokens": raw["candidatesTokenCount"], "totalTokens": raw["totalTokenCount"]},
					ReasoningTokens: raw["thoughtsTokenCount"], Accounting: "separately-billed", TotalIncludesReasoning: true, Protocol: "gemini",
				})
			}
		}
		return true
	}
	err = readGeminiLines(ctx, response.Body, process)
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
	if ctx.Err() != nil {
		fail("LLM_CANCELLED")
		return
	}
	text := full.String()
	if opts.VisibleOnly {
		text = visible.Text()
	}
	if ctx.Err() != nil {
		fail("LLM_CANCELLED")
		return
	}
	// EOF is Gemini's transport completion, NOT evidence of semantic STOP.
	// Keep unknown/length/filter finish reasons for the completion owner.
	if callbacks.OnDone != nil {
		callbacks.OnDone(text, usage, finish)
	}
}

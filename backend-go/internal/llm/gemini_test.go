package llm

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func geminiTestModel() Model {
	return Model{Provider: "gemini", BaseURL: "https://example.invalid/public/", ModelName: "synthetic", APIKey: "public-synthetic", MaxTokens: 128}
}

func geminiTestFrame(payload any) string {
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return "data: " + string(data) + "\n\n"
}

func geminiTestText(text, finish string) map[string]any {
	candidate := map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": text}}}}
	if finish != "" {
		candidate["finishReason"] = finish
	}
	return map[string]any{"candidates": []any{candidate}}
}

type geminiTestBody struct {
	reader io.Reader
	closed bool
}

func (b *geminiTestBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *geminiTestBody) Close() error              { b.closed = true; return nil }

func geminiTestProvider(body *geminiTestBody, status int) *Gemini {
	return &Gemini{Dispatch: func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: body}, nil
	}}
}

func TestGeminiRequestMappingAndExplicitZero(t *testing.T) {
	zero, tokens := float64(0), int64(0)
	model := geminiTestModel()
	opts := Options{Temperature: &zero, MaxTokens: &tokens, ResponseFormat: map[string]any{"type": "json_object"}, Reasoning: &Reasoning{Adapter: "gemini-thinking-level", ThinkingLevel: "high"}}
	messages := []Message{{"system", "first"}, {"assistant", " prior "}, {"tool", "tool"}, {"system", " last "}}
	request, err := BuildGeminiRequest(context.Background(), model, messages, opts, true)
	if err != nil {
		t.Fatal(err)
	}
	if request.URL.String() != "https://example.invalid/public/v1beta/models/synthetic:streamGenerateContent?alt=sse" || request.Method != "POST" || request.Header.Get("x-goog-api-key") != "public-synthetic" || request.Header.Get("Authorization") != "" {
		t.Fatal("request routing", request.URL, request.Header)
	}
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	config := body["generationConfig"].(map[string]any)
	if config["maxOutputTokens"] != float64(0) || config["temperature"] != float64(0) || config["responseMimeType"] != "application/json" || !reflect.DeepEqual(config["thinkingConfig"], map[string]any{"thinkingLevel": "high"}) {
		t.Fatal(config)
	}
	contents := body["contents"].([]any)
	if len(contents) != 2 || contents[0].(map[string]any)["role"] != "model" || contents[1].(map[string]any)["role"] != "user" {
		t.Fatal(contents)
	}
	if !reflect.DeepEqual(body["systemInstruction"], map[string]any{"parts": []any{map[string]any{"text": " last "}}}) {
		t.Fatal(body["systemInstruction"])
	}
	for _, base := range []string{"file:///private", "https://user:secret@example.invalid", "not-url"} {
		model.BaseURL = base
		if _, err := BuildGeminiRequest(context.Background(), model, nil, Options{}, false); err == nil {
			t.Fatal("unsafe endpoint accepted", base)
		}
	}
	if result := (&Gemini{}).Generate(context.Background(), geminiTestModel(), nil, Options{}); result.Success || result.Error != "LLM_REQUEST_FAILED" {
		t.Fatal("implicit dispatcher", result)
	}
}

func TestGeminiJSONPresenceFlags(t *testing.T) {
	var model Model
	if err := json.Unmarshal([]byte(`{"baseUrl":"https://example.invalid","modelName":"synthetic"}`), &model); err != nil {
		t.Fatal(err)
	}
	var opts Options
	if err := json.Unmarshal([]byte(`{"temperature":null,"reasoning":{"adapter":"gemini-thinking-budget"}}`), &opts); err != nil {
		t.Fatal(err)
	}
	request, err := BuildGeminiRequest(context.Background(), model, nil, opts, false)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	config := body["generationConfig"].(map[string]any)
	if _, present := config["maxOutputTokens"]; present {
		t.Fatal("missing model default serialized as zero", config)
	}
	if value, present := config["temperature"]; !present || value != nil {
		t.Fatal("explicit temperature null lost", config)
	}
	if !reflect.DeepEqual(config["thinkingConfig"], map[string]any{}) {
		t.Fatal("absent thinking budget serialized as zero", config)
	}
}

func TestGeminiUsageSafeIntegersMissingNullAndRawEvidence(t *testing.T) {
	cases := []struct {
		name       string
		raw        map[string]any
		completion *float64
	}{
		{"missing", map[string]any{}, nil},
		{"candidate-only", map[string]any{"candidatesTokenCount": float64(7)}, geminiTestNumber(7)},
		{"thought-only", map[string]any{"thoughtsTokenCount": float64(3)}, geminiTestNumber(3)},
		{"addition", map[string]any{"candidatesTokenCount": float64(7), "thoughtsTokenCount": float64(3)}, geminiTestNumber(10)},
		{"null", map[string]any{"candidatesTokenCount": nil, "thoughtsTokenCount": float64(3)}, nil},
		{"fraction", map[string]any{"candidatesTokenCount": float64(1.5)}, nil},
		{"negative", map[string]any{"thoughtsTokenCount": float64(-1)}, nil},
		{"string", map[string]any{"candidatesTokenCount": "7"}, nil},
		{"unsafe", map[string]any{"candidatesTokenCount": float64(9007199254740992)}, nil},
		{"safe-sum-not-revalidated", map[string]any{"candidatesTokenCount": float64(9007199254740991), "thoughtsTokenCount": float64(1)}, geminiTestNumber(9007199254740992)},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			usage := geminiUsage(test.raw)
			if !reflect.DeepEqual(usage.CompletionTokens, test.completion) || usage.PromptTokens != nil || usage.TotalTokens != nil {
				t.Fatal(usage)
			}
		})
	}
	raw := map[string]any{"promptTokenCount": "bad", "candidatesTokenCount": false, "thoughtsTokenCount": []any{}, "totalTokenCount": float64(-4)}
	body := &geminiTestBody{reader: strings.NewReader(geminiTestFrame(map[string]any{"usageMetadata": raw}))}
	count := 0
	geminiTestProvider(body, 200).Stream(context.Background(), geminiTestModel(), nil, Options{}, Callbacks{OnUsageEvidence: func(e UsageEvidence) {
		count++
		if !reflect.DeepEqual(e.Usage, map[string]any{"promptTokens": "bad", "completionTokens": false, "totalTokens": float64(-4)}) || !reflect.DeepEqual(e.ReasoningTokens, []any{}) || e.Protocol != "gemini" || e.Accounting != "separately-billed" || !e.TotalIncludesReasoning {
			t.Fatal(e)
		}
	}})
	if count != 1 || !body.closed {
		t.Fatal(count, body.closed)
	}
}

func geminiTestNumber(value float64) *float64 { return &value }

func TestGeminiVisibleLegacyReasoningAndCallbackOrder(t *testing.T) {
	first := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{
		map[string]any{"text": "flag", "thought": true}, map[string]any{"text": " A <thi"},
	}}}}}
	wire := geminiTestFrame(first) + geminiTestFrame(geminiTestText("nk>secret</think> B ", "STOP")) + geminiTestFrame(map[string]any{"usageMetadata": map[string]any{"candidatesTokenCount": 2, "thoughtsTokenCount": 3}})
	for _, visible := range []bool{true, false} {
		t.Run(map[bool]string{true: "visible", false: "legacy"}[visible], func(t *testing.T) {
			body := &geminiTestBody{reader: strings.NewReader(wire)}
			var events []string
			geminiTestProvider(body, 200).Stream(context.Background(), geminiTestModel(), nil, Options{VisibleOnly: visible}, Callbacks{
				OnChunk:         func(s string) { events = append(events, "chunk:"+s) },
				OnReasoning:     func(s string) { events = append(events, "reasoning:"+s) },
				OnUsageEvidence: func(UsageEvidence) { events = append(events, "usage") },
				OnDone: func(s string, u *Usage, finish string) {
					if finish != "stop" || u == nil || u.CompletionTokens == nil || *u.CompletionTokens != 5 {
						t.Error(finish, u)
					}
					events = append(events, "done:"+s)
				},
				OnError: func(code, text string, u *Usage) { t.Error(code, text) },
			})
			want := []string{"reasoning:flag", "chunk: A ", "reasoning:secret", "chunk: B ", "usage", "done: A  B "}
			if !visible {
				want = []string{"reasoning:flag", "chunk: A <thi", "reasoning:secret", "chunk:nk>secret</think> B ", "usage", "done: A <think>secret</think> B "}
			}
			if !reflect.DeepEqual(events, want) || !body.closed {
				t.Fatal(events, want, body.closed)
			}
		})
	}
}

func TestGeminiFramingEOFAndFinishAreIndependent(t *testing.T) {
	for _, test := range []struct {
		name, wire, content, finish, code string
		visible                          bool
	}{
		{"empty-eof", "", "", "unknown", "", true},
		{"no-finish-eof", geminiTestFrame(geminiTestText("partial", "")), "partial", "unknown", "", true},
		{"finish-not-terminal", geminiTestFrame(geminiTestText("A", "STOP")) + geminiTestFrame(geminiTestText("B", "MAX_TOKENS")), "AB", "length", "", true},
		{"null-overwrites-stop", geminiTestFrame(geminiTestText("A", "STOP")) + "data: {\"candidates\":[{\"finishReason\":null}]}", "A", "unknown", "", true},
		{"raw-json-array-ignored", `[{"candidates":[{"finishReason":"STOP"}]}]`, "", "unknown", "", true},
		{"missing-space-ignored", "data:{\"candidates\":[{\"finishReason\":\"STOP\"}]}\n", "", "unknown", "", true},
		{"adjacent-lines-not-events", strings.ReplaceAll(geminiTestFrame(geminiTestText("A", "")), "\n\n", "\n") + strings.TrimSpace(geminiTestFrame(geminiTestText("B", "STOP"))), "AB", "stop", "", true},
		{"done-is-invalid", geminiTestFrame(geminiTestText("A", "STOP")) + "data: [DONE]\n\n", "A", "", "Error: LLM_STREAM_INVALID", true},
		{"legacy-done-ignored", geminiTestFrame(geminiTestText("A", "STOP")) + "data: [DONE]\n\n" + geminiTestFrame(geminiTestText("B", "MAX_TOKENS")), "AB", "length", "", false},
		{"legacy-malformed-ignored", "data: {bad}\n\n" + geminiTestFrame(geminiTestText("A", "STOP")), "A", "stop", "", false},
		{"cr-not-delimiter", "data: {}\rdata: {}\r", "", "", "Error: LLM_STREAM_INVALID", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &geminiTestBody{reader: strings.NewReader(test.wire)}
			terminals := 0
			geminiTestProvider(body, 200).Stream(context.Background(), geminiTestModel(), nil, Options{VisibleOnly: test.visible}, Callbacks{
				OnDone: func(s string, u *Usage, finish string) {
					terminals++
					if test.code != "" || s != test.content || finish != test.finish {
						t.Error(s, finish, test.code)
					}
				},
				OnError: func(code, s string, u *Usage) {
					terminals++
					if code != test.code || s != test.content {
						t.Error(code, s)
					}
				},
			})
			if terminals != 1 || !body.closed {
				t.Fatal(terminals, body.closed)
			}
		})
	}
}

func TestGeminiValidatesAllCandidatesBeforeEmittingAndKeepsUsage(t *testing.T) {
	valid := geminiTestText("must not emit", "STOP")["candidates"].([]any)[0]
	for _, invalid := range []any{
		map[string]any{"candidates": []any{valid, nil}},
		map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"parts": []any{map[string]any{"text": "must not emit"}, map[string]any{"text": nil}}}}}},
		map[string]any{"candidates": nil},
		map[string]any{"candidates": []any{map[string]any{"content": nil}}},
	} {
		wire := geminiTestFrame(geminiTestText("safe <thi", "")) + geminiTestFrame(map[string]any{"usageMetadata": map[string]any{"thoughtsTokenCount": 3}}) + geminiTestFrame(invalid)
		body := &geminiTestBody{reader: strings.NewReader(wire)}
		chunks, failures := "", 0
		geminiTestProvider(body, 200).Stream(context.Background(), geminiTestModel(), nil, Options{VisibleOnly: true}, Callbacks{
			OnChunk: func(s string) { chunks += s },
			OnDone:  func(string, *Usage, string) { t.Error("invalid stream completed") },
			OnError: func(code, text string, u *Usage) {
				failures++
				if code != "Error: LLM_STREAM_INVALID" || text != "safe " || u == nil || u.CompletionTokens == nil || *u.CompletionTokens != 3 {
					t.Error(code, text, u)
				}
			},
		})
		if chunks != "safe " || failures != 1 || !body.closed {
			t.Fatal(chunks, failures, body.closed)
		}
	}
}

func TestGeminiGenerateUsesOnlyFirstPartWithoutFiltering(t *testing.T) {
	payload := `{"candidates":[{"content":{"parts":[{"text":"  <think>private</think>  ","thought":true},{"text":"ignored"}]},"finishReason":"STOP"}],"usageMetadata":{"candidatesTokenCount":2,"thoughtsTokenCount":3}}`
	for _, finish := range []string{"STOP", "MAX_TOKENS", "SAFETY", "OTHER"} {
		body := &geminiTestBody{reader: strings.NewReader(strings.Replace(payload, "STOP", finish, 1))}
		result := geminiTestProvider(body, 200).Generate(context.Background(), geminiTestModel(), nil, Options{VisibleOnly: true})
		if result.Content != "  <think>private</think>  " || result.Success != (finish == "STOP") || result.FinishReason != GeminiFinishReason(finish) || result.Usage == nil || result.Usage.CompletionTokens == nil || *result.Usage.CompletionTokens != 5 || !body.closed {
			t.Fatal(result, body.closed)
		}
		if !result.Success && result.Error != "LLM_RESPONSE_INCOMPLETE" {
			t.Fatal(result)
		}
	}
}

type geminiTestErrorReader struct {
	data string
	sent bool
}

func (r *geminiTestErrorReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, errors.New("private transport error and URL")
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if r.data == "" {
		r.sent = true
	}
	return n, nil
}

type geminiTestFinalCancel struct {
	data   string
	cancel context.CancelFunc
}

func (r *geminiTestFinalCancel) Read(p []byte) (int, error) {
	if r.data == "" {
		r.cancel()
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

func TestGeminiReadErrorPartialScopeAndCancellationFence(t *testing.T) {
	wire := geminiTestFrame(geminiTestText("safe <think>private</think> tail <thi", "STOP")) + geminiTestFrame(map[string]any{"usageMetadata": map[string]any{"candidatesTokenCount": 2}})
	for _, visible := range []bool{true, false} {
		for _, cancelled := range []bool{true, false} {
			ctx, cancel := context.WithCancel(context.Background())
			var reader io.Reader = &geminiTestErrorReader{data: wire}
			if cancelled {
				reader = &geminiTestFinalCancel{data: wire, cancel: cancel}
			}
			body := &geminiTestBody{reader: reader}
			failures := 0
			geminiTestProvider(body, 200).Stream(ctx, geminiTestModel(), nil, Options{VisibleOnly: visible}, Callbacks{
				OnDone: func(string, *Usage, string) { t.Error("read error/cancellation completed") },
				OnError: func(code, text string, u *Usage) {
					failures++
					wantCode, wantText := "LLM_STREAM_NETWORK_FAILED", ""
					if cancelled {
						wantCode = "LLM_CANCELLED"
					}
					if visible {
						wantText = "safe  tail "
					}
					if code != wantCode || text != wantText || u == nil || u.CompletionTokens == nil || *u.CompletionTokens != 2 {
						t.Error(code, text, u)
					}
				},
			})
			cancel()
			if failures != 1 || !body.closed {
				t.Fatal(failures, body.closed)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := &geminiTestBody{reader: &geminiTestFinalCancel{data: `{"candidates":[{"finishReason":"STOP"}]}`, cancel: cancel}}
	result := geminiTestProvider(body, 200).Generate(ctx, geminiTestModel(), nil, Options{})
	if result.Success || result.Error != "LLM_CANCELLED" || !body.closed {
		t.Fatal(result, body.closed)
	}
	body = &geminiTestBody{reader: strings.NewReader("")}
	provider := &Gemini{Dispatch: func(*http.Request) (*http.Response, error) {
		return &http.Response{Body: body}, errors.New("private dispatch URL")
	}}
	result = provider.Generate(context.Background(), geminiTestModel(), nil, Options{})
	if !body.closed || result.Error != "LLM_REQUEST_FAILED" {
		t.Fatal(result, body.closed)
	}
}

func TestGeminiHTTPContextAndProviderErrors(t *testing.T) {
	for _, status := range []int{400, 429} {
		body := &geminiTestBody{reader: strings.NewReader("Input token count (999) exceeds the maximum number of tokens")}
		failed := 0
		geminiTestProvider(body, status).Stream(context.Background(), geminiTestModel(), nil, Options{}, Callbacks{OnError: func(code, text string, usage *Usage) {
			failed++
			want := "LLM_REQUEST_FAILED"
			if status == 400 {
				want = "LLM_CONTEXT_LENGTH_EXCEEDED"
			}
			if code != want || text != "" || usage != nil {
				t.Error(code, text, usage)
			}
		}})
		if failed != 1 || !body.closed {
			t.Fatal(failed, body.closed)
		}
	}
	body := &geminiTestBody{reader: strings.NewReader(geminiTestFrame(geminiTestText("partial", "")) + "data: {\"error\":null}\n")}
	failed := 0
	geminiTestProvider(body, 200).Stream(context.Background(), geminiTestModel(), nil, Options{VisibleOnly: true}, Callbacks{OnError: func(code, text string, usage *Usage) {
		failed++
		if code != "Error: LLM_STREAM_PROVIDER_ERROR" || text != "partial" {
			t.Error(code, text)
		}
	}})
	if failed != 1 || !body.closed {
		t.Fatal(failed, body.closed)
	}
}

func TestGeminiWireAndLineLimits(t *testing.T) {
	if err := readGeminiLines(context.Background(), strings.NewReader(strings.Repeat("x", maxSSEEventBytes+1)), func(string) bool {
		t.Error("oversize line emitted")
		return true
	}); !errors.Is(err, errSSELimit) {
		t.Fatal(err)
	}
	// Small ignored lines must still count against the aggregate wire bound.
	reader := io.LimitReader(geminiTestEndlessHeartbeat{}, maxProviderBytes+1024)
	if err := readGeminiLines(context.Background(), reader, func(string) bool { return true }); !errors.Is(err, errSSELimit) {
		t.Fatal(err)
	}
}

type geminiTestEndlessHeartbeat struct{}

func (geminiTestEndlessHeartbeat) Read(p []byte) (int, error) {
	for i := range p {
		if i%2 == 0 {
			p[i] = ':'
		} else {
			p[i] = '\n'
		}
	}
	return len(p), nil
}

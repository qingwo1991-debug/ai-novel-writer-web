package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOriginalGeminiProviderSyntheticContracts(t *testing.T) {
	path := os.Getenv("GO_GEMINI_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original Gemini oracle mandatory")
		}
		t.Skip("GitHub-only original Gemini oracle")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var oracle struct {
		FormatVersion int `json:"formatVersion"`
		Cases         []struct {
			Name     string    `json:"name"`
			Mode     string    `json:"mode"`
			Model    Model     `json:"model"`
			Opts     Options   `json:"opts"`
			Messages []Message `json:"messages"`
			Response struct {
				Status  int               `json:"status"`
				Body    string            `json:"body"`
				Chunks  [][]byte          `json:"chunks"`
				Headers map[string]string `json:"headers"`
			} `json:"response"`
			Request map[string]any   `json:"request"`
			Events  []map[string]any `json:"events"`
			Result  map[string]any   `json:"result"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(data, &oracle); err != nil {
		t.Fatal(err)
	}
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 50 {
		t.Fatal("incomplete Gemini oracle", len(oracle.Cases))
	}
	normalize := func(value any) any {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded any
		if err := json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	for _, fixture := range oracle.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			called := 0
			provider := Gemini{Dispatch: func(request *http.Request) (*http.Response, error) {
				called++
				body, err := io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				var parsed any
				if err := json.Unmarshal(body, &parsed); err != nil {
					t.Fatal(err)
				}
				headers := map[string]string{}
				for key := range request.Header {
					headers[strings.ToLower(key)] = request.Header.Get(key)
				}
				actual := map[string]any{"url": request.URL.String(), "method": request.Method, "headers": headers, "body": parsed}
				if !reflect.DeepEqual(normalize(actual), normalize(fixture.Request)) {
					t.Fatalf("request Go=%#v TS=%#v", normalize(actual), normalize(fixture.Request))
				}
				header := http.Header{}
				for key, value := range fixture.Response.Headers {
					header.Set(key, value)
				}
				chunks := fixture.Response.Chunks
				if len(chunks) == 0 {
					chunks = [][]byte{[]byte(fixture.Response.Body)}
				}
				return &http.Response{StatusCode: fixture.Response.Status, Header: header, Body: io.NopCloser(&byteChunks{chunks: chunks})}, nil
			}}
			if fixture.Mode == "generate" {
				result := provider.Generate(context.Background(), fixture.Model, fixture.Messages, fixture.Opts)
				if !reflect.DeepEqual(normalize(result), normalize(fixture.Result)) {
					t.Fatalf("result Go=%#v TS=%#v", normalize(result), normalize(fixture.Result))
				}
			} else {
				events := []map[string]any{}
				callbacks := Callbacks{OnChunk: func(text string) { events = append(events, map[string]any{"type": "chunk", "text": text}) }, OnReasoning: func(text string) { events = append(events, map[string]any{"type": "reasoning", "text": text}) }, OnUsageEvidence: func(e UsageEvidence) { events = append(events, map[string]any{"type": "usage", "evidence": e}) }, OnDone: func(text string, u *Usage, finish string) {
					e := map[string]any{"type": "done", "content": text, "finishReason": finish}
					if u != nil {
						e["usage"] = u
					}
					events = append(events, e)
				}, OnError: func(code, text string, u *Usage) {
					e := map[string]any{"type": "error", "error": code}
					if text != "" || (fixture.Opts.VisibleOnly && strings.HasPrefix(code, "Error:")) {
						e["content"] = text
					}
					if u != nil {
						e["usage"] = u
					}
					events = append(events, e)
				}}
				provider.Stream(context.Background(), fixture.Model, fixture.Messages, fixture.Opts, callbacks)
				// Explicit compatibility exception: never expose engine-dependent JSON.parse
				// SyntaxError snippets (which can contain provider text) through Go errors.
				// Every other field/order/error is compared verbatim, no broad normalization.
				expected := make([]map[string]any, len(fixture.Events))
				for i, e := range fixture.Events {
					copy := map[string]any{}
					for key, value := range e {
						copy[key] = value
					}
					if code, ok := copy["error"].(string); ok && strings.HasPrefix(code, "SyntaxError:") {
						copy["error"] = "Error: LLM_STREAM_INVALID"
					}
					expected[i] = copy
				}
				if !reflect.DeepEqual(normalize(events), normalize(expected)) {
					t.Fatalf("events Go=%#v TS=%#v", normalize(events), normalize(expected))
				}
			}
			if called != 1 {
				t.Fatal("unexpected dispatch count", called)
			}
		})
	}
}

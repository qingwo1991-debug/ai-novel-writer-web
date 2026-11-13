package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestOriginalOpenAIProviderSyntheticContracts(t *testing.T) {
	path := os.Getenv("GO_OPENAI_CONTRACT")
	if path == "" {
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal("original provider oracle mandatory")
		}
		t.Skip("GitHub-only original provider oracle")
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
	if oracle.FormatVersion != 1 || len(oracle.Cases) < 20 {
		t.Fatal("incomplete oracle", len(oracle.Cases))
	}
	normalize := func(value any) any {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var result any
		if err := json.Unmarshal(encoded, &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, fixture := range oracle.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			called := 0
			provider := OpenAI{Dispatch: func(request *http.Request) (*http.Response, error) {
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
					t.Fatalf("request\nGo=%#v\nTS=%#v", normalize(actual), normalize(fixture.Request))
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
					t.Fatalf("result\nGo=%#v\nTS=%#v", normalize(result), normalize(fixture.Result))
				}
			} else {
				events := []map[string]any{}
				callbacks := Callbacks{
					OnChunk:         func(text string) { events = append(events, map[string]any{"type": "chunk", "text": text}) },
					OnReasoning:     func(text string) { events = append(events, map[string]any{"type": "reasoning", "text": text}) },
					OnUsageEvidence: func(e UsageEvidence) { events = append(events, map[string]any{"type": "usage", "evidence": e}) },
					OnDone: func(text string, u *Usage, finish string) {
						e := map[string]any{"type": "done", "content": text, "finishReason": finish}
						if u != nil {
							e["usage"] = u
						}
						events = append(events, e)
					},
					OnError: func(code, text string, u *Usage) {
						e := map[string]any{"type": "error", "error": code}
						if text != "" {
							e["content"] = text
						}
						if u != nil {
							e["usage"] = u
						}
						events = append(events, e)
					},
				}
				provider.Stream(context.Background(), fixture.Model, fixture.Messages, fixture.Opts, callbacks)
				if !reflect.DeepEqual(normalize(events), normalize(fixture.Events)) {
					t.Fatalf("events\nGo=%#v\nTS=%#v", normalize(events), normalize(fixture.Events))
				}
			}
			if called != 1 {
				t.Fatal("unexpected dispatch count", called)
			}
		})
	}
}

type byteChunks struct{ chunks [][]byte }

func (r *byteChunks) Read(p []byte) (int, error) {
	for len(r.chunks) > 0 {
		if len(r.chunks[0]) == 0 {
			r.chunks = r.chunks[1:]
			continue
		}
		n := copy(p, r.chunks[0])
		r.chunks[0] = r.chunks[0][n:]
		return n, nil
	}
	return 0, io.EOF
}
func TestOpenAIRequestNoImplicitTemperatureOrProviderFallback(t *testing.T) {
	model := Model{Provider: "openai", ModelName: "synthetic", BaseURL: "https://example.invalid/v1", MaxTokens: 500, APIKey: "public-synthetic"}
	request, err := BuildOpenAIRequest(context.Background(), model, []Message{{"user", "合成"}}, Options{}, true)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(request.Body)
	if bytes.Contains(data, []byte("temperature")) {
		t.Fatal("implicit temperature")
	}
	if response := (&OpenAI{}).Generate(context.Background(), model, nil, Options{}); response.Success || response.Error == "" {
		t.Fatal("missing dispatcher silently succeeded")
	}
	for _, base := range []string{"file:///etc/passwd", "http://user:secret@example.invalid", "not-url"} {
		model.BaseURL = base
		if _, err := BuildOpenAIRequest(context.Background(), model, nil, Options{}, false); err == nil {
			t.Fatal("unsafe endpoint accepted")
		}
	}
}

package embedding

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
)

func testClient(t *testing.T, origins ...string) *Client {
	t.Helper()
	boundary, err := egress.New(origins)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(boundary.Close)
	return New(boundary)
}

func assertNilError(t *testing.T, result [][]float64, err, want error) {
	t.Helper()
	if result != nil || !errors.Is(err, want) {
		t.Fatalf("result=%v err=%v, want nil and %v", result, err, want)
	}
}

func TestResolvedEndpoints(t *testing.T) {
	for _, test := range []struct {
		protocol                   Protocol
		base, name, want, wantName string
	}{
		{OpenAI, "https://api.openai.com///", "", "https://api.openai.com/v1/embeddings", "text-embedding-3-small"},
		{OpenAI, "https://api.deepseek.com", "", "https://api.deepseek.com/v1/embeddings", "text-embedding-3-small"},
		{OpenAI, "http://localhost:11434/", "m", "http://localhost:11434/v1/embeddings", "m"},
		{OpenAI, "http://127.0.0.1:11434", "m", "http://127.0.0.1:11434/v1/embeddings", "m"},
		{OpenAI, "https://custom.test", "m", "https://custom.test/embeddings", "m"},
		{OpenAI, "https://custom.test/api/", "m", "https://custom.test/api/embeddings", "m"},
		{OpenAI, "https://custom.test/v1beta///", "m", "https://custom.test/v1beta/embeddings", "m"},
		{OpenAI, "https://custom.test/embeddings///", "m", "https://custom.test/embeddings", "m"},
		{Gemini, "https://custom.test///", "", "https://custom.test///v1beta/models/gemini-embedding-001:batchEmbedContents", "gemini-embedding-001"},
		{Gemini, "https://custom.test/v1beta", "embedding-custom", "https://custom.test/v1beta/v1beta/models/embedding-custom:batchEmbedContents", "embedding-custom"},
	} {
		t.Run(string(test.protocol)+test.base, func(t *testing.T) {
			got, name, err := endpoint(test.protocol, Model{BaseURL: test.base, ModelName: test.name})
			if err != nil || got != test.want || name != test.wantName {
				t.Fatalf("endpoint=%q name=%q err=%v", got, name, err)
			}
		})
	}
	for _, base := range []string{"http://localhost:11434/api", "http://LOCALHOST:11434/api///", "http://127.0.0.1:11434/api/"} {
		if _, err := ResolveURL(OpenAI, Model{BaseURL: base}); err != ErrOllamaNativeURL {
			t.Fatalf("native URL: %v", err)
		}
	}
	for _, base := range []string{"http://localhost:11435/api", "https://localhost:11434/api", "http://other.test:11434/api"} {
		if _, err := ResolveURL(OpenAI, Model{BaseURL: base}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRealWireHeadersPayloadAndDefaults(t *testing.T) {
	for _, protocol := range []Protocol{OpenAI, Gemini} {
		for _, custom := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom=%t", protocol, custom), func(t *testing.T) {
				texts := []string{"quote\" newline\n中文", "<second>&"}
				name := ""
				if custom {
					name = "custom-model"
				}
				wantName := name
				if wantName == "" {
					wantName = "text-embedding-3-small"
					if protocol == Gemini {
						wantName = "gemini-embedding-001"
					}
				}
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" || r.URL.RawQuery != "" {
						t.Errorf("incorrect method, content-type, or query")
					}
					var actual any
					if err := json.NewDecoder(r.Body).Decode(&actual); err != nil {
						t.Error(err)
						return
					}
					if protocol == OpenAI {
						if r.URL.Path != "/base/embeddings" || r.Header.Get("Authorization") != "Bearer test-key" || r.Header.Get("x-goog-api-key") != "" {
							t.Error("OpenAI path or credentials")
						}
						want := map[string]any{"model": wantName, "input": []any{texts[0], texts[1]}}
						if !reflect.DeepEqual(actual, want) {
							t.Errorf("payload=%v", actual)
						}
						_, _ = io.WriteString(w, `{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}`)
					} else {
						if r.URL.Path != "/base/v1beta/models/"+wantName+":batchEmbedContents" || r.Header.Get("x-goog-api-key") != "test-key" || r.Header.Get("Authorization") != "" {
							t.Error("Gemini path or credentials")
						}
						requests := make([]any, len(texts))
						for i, text := range texts {
							requests[i] = map[string]any{"model": "models/" + wantName, "content": map[string]any{"parts": []any{map[string]any{"text": text}}}, "taskType": "RETRIEVAL_DOCUMENT"}
						}
						if !reflect.DeepEqual(actual, map[string]any{"requests": requests}) {
							t.Errorf("payload=%v", actual)
						}
						_, _ = io.WriteString(w, `{"embeddings":[{"values":[1,2]},{"values":[3,4]}]}`)
					}
				}))
				defer server.Close()
				client := testClient(t, server.URL)
				got, err := client.EmbedBatch(context.Background(), protocol, Model{BaseURL: server.URL + "/base/", APIKey: "test-key", ModelName: name}, texts)
				if err != nil || !reflect.DeepEqual(got, [][]float64{{1, 2}, {3, 4}}) || calls.Load() != 1 {
					t.Fatalf("result=%v err=%v calls=%d", got, err, calls.Load())
				}
				// Mutating one vector must not alias another result.
				got[0][0] = 99
				if got[1][0] != 3 {
					t.Fatal("aliased vectors")
				}
			})
		}
	}
}

func TestMalformedResponsesNeverReturnPartialResults(t *testing.T) {
	cases := []struct {
		protocol Protocol
		body     string
	}{
		{OpenAI, ``}, {OpenAI, `null`}, {OpenAI, `[]`}, {OpenAI, `{}`},
		{OpenAI, `{"data":null}`}, {OpenAI, `{"data":[]}`},
		{OpenAI, `{"data":[null]}`}, {OpenAI, `{"data":[{"embedding":[1]}]}`},
		{OpenAI, `{"data":[{"index":true,"embedding":[1]}]}`},
		{OpenAI, `{"data":[{"index":"0","embedding":[1]}]}`},
		{OpenAI, `{"data":[{"index":0.5,"embedding":[1]}]}`},
		{OpenAI, `{"data":[{"index":-1,"embedding":[1]}]}`},
		{OpenAI, `{"data":[{"index":1,"embedding":[1]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[true]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[null]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":["NaN"]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[NaN]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[1e999]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[1]}]} {}`},
		{OpenAI, "{\"data\":[{\"index\":0,\"embedding\":[1]}],\"ignored\":\"\xff\"}"},
		{Gemini, `{}`}, {Gemini, `{"embeddings":null}`}, {Gemini, `{"embeddings":[]}`},
		{Gemini, `{"embeddings":[null]}`}, {Gemini, `{"embeddings":[{"values":[]}]}`},
		{Gemini, `{"embeddings":[{"values":[false]}]}`},
		{Gemini, `{"embeddings":[{"values":["1"]}]}`},
		{Gemini, `{"embeddings":[{"values":[1e999]}]}`},
	}
	for i, test := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, test.body) }))
			defer server.Close()
			got, err := testClient(t, server.URL).EmbedBatch(context.Background(), test.protocol, Model{BaseURL: server.URL}, []string{"text"})
			assertNilError(t, got, err, ErrResponseInvalid)
		})
	}
	for _, test := range []struct {
		protocol Protocol
		body     string
	}{
		{OpenAI, `{"data":[{"index":0,"embedding":[1]},{"index":0,"embedding":[2]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[2,3]}]}`},
		{OpenAI, `{"data":[{"index":0,"embedding":[1]}]}`},
		{Gemini, `{"embeddings":[{"values":[1]},{"values":[2,3]}]}`},
		{Gemini, `{"embeddings":[{"values":[1]},null]}`},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, test.body) }))
		got, err := testClient(t, server.URL).EmbedBatch(context.Background(), test.protocol, Model{BaseURL: server.URL}, []string{"a", "b"})
		server.Close()
		assertNilError(t, got, err, ErrResponseInvalid)
	}
}

func TestBatchDefaultsAndConfiguredClamps(t *testing.T) {
	zero, negative, one, maximum, over := 0, -10, 1, 50, 999
	for _, test := range []struct {
		name     string
		protocol Protocol
		count    int
		size     *int
		want     []int
	}{
		{"openai-default", OpenAI, 51, nil, []int{50, 1}},
		{"gemini-default", Gemini, 101, nil, []int{100, 1}},
		{"zero", OpenAI, 3, &zero, []int{1, 1, 1}},
		{"negative", Gemini, 3, &negative, []int{1, 1, 1}},
		{"one", OpenAI, 2, &one, []int{1, 1}},
		{"maximum", Gemini, 51, &maximum, []int{50, 1}},
		{"over", Gemini, 51, &over, []int{50, 1}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var batches []int
			var batchMu sync.Mutex
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Input    []string          `json:"input"`
					Requests []json.RawMessage `json:"requests"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				n := len(body.Input)
				if test.protocol == Gemini {
					n = len(body.Requests)
				}
				batchMu.Lock()
				batches = append(batches, n)
				batchMu.Unlock()
				writeVectors(w, test.protocol, n, 2)
			}))
			defer server.Close()
			checks := 0
			got, err := testClient(t, server.URL).Generate(context.Background(), test.protocol, Model{BaseURL: server.URL}, make([]string, test.count), test.size, func() error { checks++; return nil })
			batchMu.Lock()
			defer batchMu.Unlock()
			if err != nil || len(got) != test.count || !reflect.DeepEqual(batches, test.want) || checks != len(test.want)*2 {
				t.Fatalf("count=%d err=%v batches=%v checks=%d", len(got), err, batches, checks)
			}
		})
	}
}

func writeVectors(w http.ResponseWriter, protocol Protocol, count, dimension int) {
	items := make([]any, count)
	for i := range items {
		vector := make([]float64, dimension)
		for j := range vector {
			vector[j] = float64(i + j + 1)
		}
		if protocol == OpenAI {
			items[i] = map[string]any{"index": i, "embedding": vector}
		} else {
			items[i] = map[string]any{"values": vector}
		}
	}
	field := "data"
	if protocol == Gemini {
		field = "embeddings"
	}
	_ = json.NewEncoder(w).Encode(map[string]any{field: items})
}

func TestGenerateCrossBatchDimensionAndLateFailures(t *testing.T) {
	for _, protocol := range []Protocol{OpenAI, Gemini} {
		for _, failure := range []string{"dimension", "http", "json"} {
			t.Run(string(protocol)+failure, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if calls.Add(1) == 1 {
						writeVectors(w, protocol, 1, 2)
						return
					}
					switch failure {
					case "dimension":
						writeVectors(w, protocol, 1, 3)
					case "http":
						w.WriteHeader(500)
						_, _ = io.WriteString(w, "private-url secret-key")
					case "json":
						_, _ = io.WriteString(w, "bad")
					}
				}))
				defer server.Close()
				size := 1
				got, err := testClient(t, server.URL).Generate(context.Background(), protocol, Model{BaseURL: server.URL}, []string{"a", "b", "c"}, &size, nil)
				want := ErrResponseInvalid
				if failure == "http" {
					want = ErrRequestFailed
				}
				assertNilError(t, got, err, want)
				if calls.Load() != 2 {
					t.Fatalf("calls=%d", calls.Load())
				}
			})
		}
	}
}

func TestAuthorityRevocationAndCallbackCancellation(t *testing.T) {
	for _, mode := range []string{"before", "after", "cancel-before", "cancel-after"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); writeVectors(w, OpenAI, 1, 1) }))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			revoked := errors.New("AUTHORITY_REVOKED")
			checks, target := 0, 1
			if strings.HasSuffix(mode, "after") {
				target = 2
			}
			size := 1
			got, err := testClient(t, server.URL).Generate(ctx, OpenAI, Model{BaseURL: server.URL}, []string{"a", "b"}, &size, func() error {
				checks++
				if checks == target {
					if strings.HasPrefix(mode, "cancel") {
						cancel()
						return nil
					}
					return revoked
				}
				return nil
			})
			want := revoked
			if strings.HasPrefix(mode, "cancel") {
				want = context.Canceled
			}
			assertNilError(t, got, err, want)
			if calls.Load() != int32(target-1) {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestInvalidInputsFailClosedWithoutDispatch(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); writeVectors(w, OpenAI, 1, 1) }))
	defer server.Close()
	client := testClient(t, server.URL)
	for _, base := range []string{"", "relative", "ftp://host", server.URL + "?", server.URL + "?key=secret", server.URL + "#", server.URL + "#secret", "http://user:secret@host", "http://@host", server.URL + "/\nsecret", "http://host\\other"} {
		got, err := client.EmbedBatch(context.Background(), OpenAI, Model{BaseURL: base, APIKey: "secret"}, []string{"text"})
		assertNilError(t, got, err, ErrRequestInvalid)
	}
	for _, model := range []Model{
		{BaseURL: server.URL, APIKey: "secret\nheader"},
		{BaseURL: server.URL, ModelName: "model?key=secret"},
		{BaseURL: server.URL, ModelName: "model#fragment"},
	} {
		got, err := client.EmbedBatch(context.Background(), Gemini, model, []string{"text"})
		assertNilError(t, got, err, ErrRequestInvalid)
	}
	for _, invalid := range []*Client{nil, {}, New(nil)} {
		got, err := invalid.EmbedBatch(context.Background(), OpenAI, Model{BaseURL: server.URL}, []string{"a"})
		assertNilError(t, got, err, ErrRequestInvalid)
	}
	got, err := client.Generate(nil, OpenAI, Model{}, nil, nil, nil)
	assertNilError(t, got, err, ErrRequestInvalid)
	got, err = client.EmbedBatch(context.Background(), Protocol("unknown"), Model{BaseURL: server.URL}, []string{"a"})
	assertNilError(t, got, err, ErrRequestInvalid)
	got, err = client.EmbedBatch(context.Background(), OpenAI, Model{BaseURL: server.URL}, []string{"\xff"})
	assertNilError(t, got, err, ErrRequestInvalid)
	got, err = client.Generate(context.Background(), OpenAI, Model{}, nil, nil, func() error { t.Error("empty input checked authority"); return nil })
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("empty=%v err=%v", got, err)
	}
	got, err = New(&egress.Client{}).EmbedBatch(context.Background(), OpenAI, Model{BaseURL: server.URL}, []string{"a"})
	assertNilError(t, got, err, egress.ErrPolicyInvalid)
	if calls.Load() != 0 {
		t.Fatalf("invalid input dispatched %d times", calls.Load())
	}
}

func TestSizeDepthAndVectorCaps(t *testing.T) {
	client := testClient(t, "http://allowed.test")
	for _, texts := range [][]string{
		make([]string, MaxTexts+1),
		{strings.Repeat("x", MaxTextBytes+1)},
		func() []string {
			values := make([]string, 33)
			for i := range values {
				values[i] = strings.Repeat("x", MaxTextBytes)
			}
			return values
		}(),
	} {
		got, err := client.Generate(context.Background(), OpenAI, Model{BaseURL: "http://allowed.test"}, texts, nil, nil)
		assertNilError(t, got, err, ErrTooLarge)
	}
	if _, err := decodeResponse([]byte(strings.Repeat("[", MaxJSONDepth+1) + strings.Repeat("]", MaxJSONDepth+1))); err != ErrResponseInvalid {
		t.Fatalf("deep response: %v", err)
	}
	if value, err := decodeResponse([]byte(`{"ignored":"[[[[\\\"", "data":[]}`)); err != nil || value == nil {
		t.Fatalf("quoted brackets: %v", err)
	}
	vector := make([]any, MaxDimension+1)
	for i := range vector {
		vector[i] = json.Number("1")
	}
	got, err := ValidateResponse(Gemini, map[string]any{"embeddings": []any{map[string]any{"values": vector}}}, 1)
	assertNilError(t, got, err, ErrTooLarge)
	vector = vector[:MaxDimension]
	items := make([]any, 16)
	for i := range items {
		items[i] = map[string]any{"values": vector}
	}
	got, err = ValidateResponse(Gemini, map[string]any{"embeddings": items}, len(items))
	assertNilError(t, got, err, ErrTooLarge)
	for _, declared := range []bool{false, true} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if declared {
				w.Header().Set("Content-Length", fmt.Sprint(MaxResponseBytes+1))
			} else {
				w.(http.Flusher).Flush()
			}
			chunk := strings.Repeat(" ", 64<<10)
			for n := 0; n <= MaxResponseBytes; n += len(chunk) {
				if _, err := io.WriteString(w, chunk); err != nil {
					return
				}
			}
		}))
		got, err := testClient(t, server.URL).EmbedBatch(context.Background(), OpenAI, Model{BaseURL: server.URL}, []string{"a"})
		server.Close()
		assertNilError(t, got, err, ErrTooLarge)
	}
}

func TestStatusRedirectPolicyAndNoCredentialLeak(t *testing.T) {
	var leaked atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { leaked.Add(1); writeVectors(w, OpenAI, 1, 1) }))
	defer other.Close()
	for _, status := range []int{204, 301, 302, 307, 308, 400, 401, 500} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", other.URL+"/private?key=secret")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, "secret-key private-body private-url")
		}))
		client := testClient(t, server.URL, other.URL)
		got, err := client.EmbedBatch(context.Background(), OpenAI, Model{BaseURL: server.URL + "/private", APIKey: "secret-key"}, []string{"a"})
		want := ErrRequestFailed
		if status == 204 {
			want = ErrResponseInvalid
		}
		if status >= 300 && status < 400 {
			want = egress.ErrRedirectForbidden
		}
		assertNilError(t, got, err, want)
		// A non-2xx provider reply keeps its own status readable while still
		// answering EMBEDDING_REQUEST_FAILED; a redirect, an empty 204 and a
		// policy refusal carry none.
		if status == 400 || status == 401 || status == 500 {
			if reported, ok := HTTPStatus(err); !ok || reported != status {
				t.Fatalf("status %d was reported as %d/%v", status, reported, ok)
			}
		} else if reported, ok := HTTPStatus(err); ok {
			t.Fatalf("status %d was reported as an HTTP status %d", status, reported)
		}
		if err != nil && (strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), server.URL) || strings.Contains(err.Error(), "private")) {
			t.Fatal("error reflected private data")
		}
		server.Close()
	}
	got, err := testClient(t, "http://allowed.test").EmbedBatch(context.Background(), Gemini, Model{BaseURL: other.URL, APIKey: "secret-key"}, []string{"a"})
	assertNilError(t, got, err, egress.ErrNotAllowed)
	if leaked.Load() != 0 {
		t.Fatal("credentials were dispatched to redirect/denied origin")
	}
}

func TestNativeClientIgnoresAmbientProxy(t *testing.T) {
	var proxyCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { proxyCalls.Add(1); w.WriteHeader(500) }))
	defer proxy.Close()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(name, proxy.URL)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeVectors(w, OpenAI, 1, 1) }))
	defer server.Close()
	got, err := testClient(t, server.URL).EmbedBatch(context.Background(), OpenAI, Model{BaseURL: server.URL}, []string{"a"})
	if err != nil || len(got) != 1 || proxyCalls.Load() != 0 {
		t.Fatalf("result=%v err=%v proxycalls=%d", got, err, proxyCalls.Load())
	}
}

func TestCancellationUnblocksNativeServerAndReturnsNoResults(t *testing.T) {
	for _, flushed := range []bool{false, true} {
		t.Run(fmt.Sprint(flushed), func(t *testing.T) {
			entered, stopped := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Drain request before observing disconnect; HTTP/1 may otherwise
				// defer its read-side disconnect detection until the body is read.
				_, _ = io.Copy(io.Discard, r.Body)
				if flushed {
					_, _ = io.WriteString(w, `{"data":[`)
					w.(http.Flusher).Flush()
				}
				close(entered)
				<-r.Context().Done()
				close(stopped)
			}))
			defer server.Close()
			client := testClient(t, server.URL)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type outcome struct {
				result [][]float64
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				got, err := client.EmbedBatch(ctx, OpenAI, Model{BaseURL: server.URL}, []string{"a"})
				done <- outcome{got, err}
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				cancel()
				t.Fatal("server did not receive request")
			}
			cancel()
			select {
			case got := <-done:
				assertNilError(t, got.result, got.err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("client did not cancel")
			}
			select {
			case <-stopped:
			case <-time.After(5 * time.Second):
				t.Fatal("server did not observe cancellation")
			}
		})
	}
}

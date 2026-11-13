package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeConfig(t *testing.T, root string, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "mcp_config.json")
	if err = os.WriteFile(p, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func configured(t *testing.T, entry map[string]any) *Manager {
	t.Helper()
	root := t.TempDir()
	p := writeConfig(t, root, map[string]any{"mcpServers": map[string]any{"fixture": entry}})
	m := New(Options{ConfigPath: p, RuntimeDir: filepath.Join(root, "runtime"), TrustedAdminGrant: true, RequestTimeout: 2 * time.Second})
	t.Cleanup(m.Close)
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if r := m.LoadConfig(); !r.Success || r.Status != "loaded" {
		t.Fatalf("load: %+v", r)
	}
	return m
}
func TestConfigValidationAndDisabledDefault(t *testing.T) {
	m := New(Options{TrustedAdminGrant: false})
	if m.Enabled() {
		t.Fatal("default enabled")
	}
	if err := m.SetEnabled(true); err == nil {
		t.Fatal("missing admin grant accepted")
	}
	if r := m.LoadConfig(); r.Error != "MCP_DISABLED" {
		t.Fatalf("disabled load: %+v", r)
	}
	if err := m.Connect(context.Background(), "fixture"); err == nil || err.Error() != "MCP_DISABLED" {
		t.Fatal("disabled connect accepted")
	}
	invalid := []string{`null`, `[]`, `{}`, `{"mcpServers":null}`, `{"mcpServers":[]}`, `{"mcpServers":{"a":null}}`, `{"mcpServers":{"a":{}}}`, `{"mcpServers":{"a":{"command":"x","url":"https://example.invalid/sse"}}}`, `{"mcpServers":{"a":{"command":"x","args":[1]}}}`, `{"mcpServers":{"a":{"command":"x","env":{"k":1}}}}`, `{"mcpServers":{"a":{"command":null}}}`}
	for _, raw := range invalid {
		if _, err := ParseConfig([]byte(raw)); err == nil {
			t.Fatalf("invalid accepted: %s", raw)
		}
	}
	got, err := ParseConfig([]byte(`{"mcpServers":{"a":{"command":"external-tool","env":{"SECRET":"fixture"}},"b":{"url":"https://example.invalid/sse"}}}`))
	if err != nil || len(got) != 2 || got[0].Transport != "stdio" || got[1].Transport != "sse" {
		t.Fatalf("parse: %+v %v", got, err)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "external-tool") {
		t.Fatal("config details leaked")
	}
}
func TestEnvironmentSanitization(t *testing.T) {
	t.Setenv("HOME", "SECRET_AUTHOR_HOME")
	t.Setenv("OPENAI_API_KEY", "SECRET_KEY")
	t.Setenv("NODE_OPTIONS", "SECRET_INJECTION")
	t.Setenv("LD_PRELOAD", "SECRET_LOADER")
	t.Setenv("HTTPS_PROXY", "SECRET_PROXY")
	t.Setenv("AI_NOVEL_RUNTIME", "SECRET_RUNTIME")
	env := strings.Join(childEnvironment("/synthetic/runtime", map[string]string{"EXPLICIT_TOKEN": "admin-token"}), "\n")
	if strings.Contains(env, "SECRET_") {
		t.Fatal("ambient environment leaked")
	}
	if !strings.Contains(env, "HOME=/synthetic/runtime") || !strings.Contains(env, "EXPLICIT_TOKEN=admin-token") {
		t.Fatal("explicit environment lost")
	}
	explicit := strings.Join(childEnvironment("/synthetic/runtime", map[string]string{"HOME": "/admin-home", "NODE_OPTIONS": "--no-warnings"}), "\n")
	if !strings.Contains(explicit, "HOME=/admin-home") || !strings.Contains(explicit, "NODE_OPTIONS=--no-warnings") {
		t.Fatal("admin overrides lost")
	}
}

type syntheticSSE struct {
	server      *httptest.Server
	messages    chan string
	posted      chan map[string]any
	closeStream chan struct{}
	once        sync.Once
	tools       []map[string]any
	hold        bool
}

func newSyntheticSSE(t *testing.T, tools []map[string]any, hold bool) *syntheticSSE {
	t.Helper()
	f := &syntheticSSE{messages: make(chan string, 128), posted: make(chan map[string]any, 128), closeStream: make(chan struct{}), tools: tools, hold: hold}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: endpoint\ndata: /messages?sessionId=public-synthetic\n\n")
		w.(http.Flusher).Flush()
		for {
			select {
			case data := <-f.messages:
				_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
				w.(http.Flusher).Flush()
			case <-r.Context().Done():
				return
			case <-f.closeStream:
				return
			}
		}
	})
	mux.HandleFunc("/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Query().Get("sessionId") != "public-synthetic" {
			http.Error(w, "invalid", 400)
			return
		}
		var request map[string]any
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			http.Error(w, "invalid", 400)
			return
		}
		f.posted <- request
		w.WriteHeader(202)
		if request["id"] == nil {
			return
		}
		if request["method"] == "tools/call" && f.hold {
			return
		}
		var result any = map[string]any{}
		switch request["method"] {
		case "tools/list":
			result = map[string]any{"tools": f.tools}
		case "resources/list":
			result = map[string]any{"resources": []map[string]any{{"uri": "fixture://public", "name": "fixture-resource", "mimeType": "text/plain", "SERVER_SECRET": "must-not-leak"}}}
		case "tools/call":
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "你好，fixture"}, {"type": "image", "data": "ignored"}, {"type": "text", "text": "second"}}}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": result})
		f.messages <- string(raw)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(func() { f.once.Do(func() { close(f.closeStream) }); f.server.Close() })
	return f
}
func fixtureTool() map[string]any {
	return map[string]any{"name": "fixture", "description": "public synthetic", "inputSchema": map[string]any{"type": "object"}, "env": map[string]any{"SECRET": "must-not-leak"}}
}
func TestLegacySSELifecycleToolProjectionAndDisable(t *testing.T) {
	f := newSyntheticSSE(t, []map[string]any{fixtureTool()}, false)
	m := configured(t, map[string]any{"url": f.server.URL + "/sse"})
	if err := m.Connect(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	tools := m.ListTools()
	if len(tools) != 1 || len(tools[0]) != 4 || tools[0]["serverId"] != "fixture" {
		t.Fatalf("tools: %+v", tools)
	}
	tools[0]["name"] = "mutated"
	if m.ListTools()[0]["name"] != "fixture" {
		t.Fatal("catalog aliases mutable state")
	}
	resources := m.ListResources()
	raw, _ := json.Marshal(resources)
	if strings.Contains(string(raw), "SECRET") {
		t.Fatal("resource spread leaked")
	}
	if r := m.CallTool(context.Background(), "fixture", "unknown", nil); r.Error != "MCP_TOOL_NOT_DISCOVERED" {
		t.Fatalf("undiscovered: %+v", r)
	}
	r := m.CallTool(context.Background(), "fixture", "fixture", nil)
	if !r.Success || r.Content != "你好，fixture\nsecond" {
		t.Fatalf("call: %+v", r)
	}
	statuses := m.ServersStatus()
	if len(statuses) != 1 || statuses[0].Status != "connected" || statuses[0].ToolCount != 1 {
		t.Fatalf("statuses: %+v", statuses)
	}
	if err := m.SetEnabled(false); err != nil {
		t.Fatal(err)
	}
	if len(m.ListTools()) != 0 || len(m.ServersStatus()) != 0 {
		t.Fatal("disabled stale catalog")
	}
	if err := m.SetEnabled(true); err != nil {
		t.Fatal(err)
	}
	if err := m.Connect(context.Background(), "fixture"); err == nil || err.Error() != "MCP_SERVER_NOT_CONFIGURED" {
		t.Fatal("enable silently retained old trust")
	}
}
func TestConfigReloadRevokesRemovedChangedAndInvalid(t *testing.T) {
	f := newSyntheticSSE(t, []map[string]any{fixtureTool()}, false)
	m := configured(t, map[string]any{"url": f.server.URL + "/sse"})
	if err := m.Connect(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	if r := m.LoadConfig(); !r.Success || len(m.ListTools()) != 1 {
		t.Fatal("unchanged reload disconnected")
	}
	if err := os.WriteFile(m.ConfigPath(), []byte(`{"mcpServers":{"fixture":{"url":"http://127.0.0.1:1/changed"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := m.LoadConfig(); !r.Success || len(m.ListTools()) != 0 || len(m.ServersStatus()) != 0 {
		t.Fatalf("changed entry retained: %+v", r)
	}
	if err := os.WriteFile(m.ConfigPath(), []byte(`broken`), 0600); err != nil {
		t.Fatal(err)
	}
	if r := m.LoadConfig(); r.Success || r.Error != "MCP_CONFIG_INVALID" {
		t.Fatalf("invalid config: %+v", r)
	}
	if err := os.Remove(m.ConfigPath()); err != nil {
		t.Fatal(err)
	}
	if r := m.LoadConfig(); !r.Success || r.Status != "missing" {
		t.Fatalf("missing: %+v", r)
	}
}
func TestSSERejectsCrossOriginEndpointAndRedirect(t *testing.T) {
	for _, redirect := range []bool{false, true} {
		t.Run(fmt.Sprint(redirect), func(t *testing.T) {
			var hits int
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(200) }))
			defer other.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if redirect {
					http.Redirect(w, r, other.URL, 302)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "event: endpoint\ndata: %s/messages\n\n", other.URL)
			}))
			defer server.Close()
			m := configured(t, map[string]any{"url": server.URL})
			if err := m.Connect(context.Background(), "fixture"); err == nil {
				t.Fatal("unsafe endpoint accepted")
			}
			if hits != 0 {
				t.Fatal("cross-origin dispatch")
			}
		})
	}
}
func TestPendingLimitConcurrentCancelAndExit(t *testing.T) {
	f := newSyntheticSSE(t, []map[string]any{fixtureTool()}, true)
	m := configured(t, map[string]any{"url": f.server.URL + "/sse"})
	if err := m.Connect(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	results := make(chan CallResult, MaxPending)
	for i := 0; i < MaxPending; i++ {
		go func() { results <- m.CallTool(ctx, "fixture", "fixture", map[string]any{}) }()
	}
	// Synchronize on actual synthetic POST dispatch instead of sleeps/poll loops.
	calls := 0
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for calls < MaxPending {
		select {
		case req := <-f.posted:
			if req["method"] == "tools/call" {
				calls++
			}
		case <-deadline.C:
			t.Fatal("pending dispatch stalled")
		}
	}
	if r := m.CallTool(context.Background(), "fixture", "fixture", nil); r.Error != "MCP_PENDING_LIMIT_EXCEEDED" {
		t.Fatalf("pending limit: %+v", r)
	}
	cancel()
	for i := 0; i < MaxPending; i++ {
		if r := <-results; r.Error != "MCP_REQUEST_CANCELED" {
			t.Fatalf("cancel: %+v", r)
		}
	}
	done := make(chan CallResult, 1)
	go func() { done <- m.CallTool(context.Background(), "fixture", "fixture", nil) }()
	select {
	case req := <-f.posted:
		if req["method"] != "tools/call" {
			t.Fatal("unexpected POST")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("call not dispatched")
	}
	f.once.Do(func() { close(f.closeStream) })
	select {
	case r := <-done:
		if r.Success || r.Error == "" {
			t.Fatal("exit did not fail pending")
		}
	case <-time.After(time.Second):
		t.Fatal("pending not completed on exit")
	}
	if len(m.ListTools()) != 0 {
		t.Fatal("exited catalog retained")
	}
}
func TestFrameCatalogAndRequestBounds(t *testing.T) {
	m := configured(t, map[string]any{"command": "not-invoked"})
	c := newConnection(serverConfig{ID: "fixture"}, Options{RequestTimeout: time.Second})
	defer c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
	for _, raw := range []string{"[]", `{"jsonrpc":"1.0"}`, string([]byte{255})} {
		if err := c.receive([]byte(raw)); err == nil {
			t.Fatalf("invalid frame accepted: %q", raw)
		}
	}
	if err := c.receive([]byte(strings.Repeat("x", MaxFrameBytes+1))); err == nil || err.Error() != "MCP_STDOUT_LIMIT_EXCEEDED" {
		t.Fatal("frame limit")
	}
	c.status = "connected"
	c.tools = []map[string]any{{"name": "fixture"}}
	m.mu.Lock()
	m.servers["fixture"] = c
	m.mu.Unlock()
	if r := m.CallTool(context.Background(), "fixture", "fixture", map[string]any{"data": strings.Repeat("中", MaxRequestBytes)}); r.Error != "MCP_REQUEST_TOO_LARGE" {
		t.Fatalf("request limit: %+v", r)
	}
	cyclic := map[string]any{}
	cyclic["self"] = cyclic
	if r := m.CallTool(context.Background(), "fixture", "fixture", cyclic); r.Error != "MCP_REQUEST_INVALID" {
		t.Fatalf("cyclic: %+v", r)
	}
	many := make([]map[string]any, MaxCatalogEntries+1)
	for i := range many {
		many[i] = fixtureTool()
	}
	f := newSyntheticSSE(t, many, false)
	catalog := configured(t, map[string]any{"url": f.server.URL + "/sse"})
	if err := catalog.Connect(context.Background(), "fixture"); err == nil {
		t.Fatal("unbounded catalog accepted")
	}
	if got := catalog.ServersStatus(); len(got) != 1 || got[0].ToolCount != 0 || got[0].Status != "error" {
		t.Fatalf("bad catalog status: %+v", got)
	}
}
func TestQueuedCancellationAndRevocationNeverDispatch(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprint(disable), func(t *testing.T) {
			c := newConnection(serverConfig{ID: "fixture"}, Options{RequestTimeout: time.Second})
			defer c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
			ctx, cancel := context.WithCancel(context.Background())
			ch := make(chan response, 1)
			c.mu.Lock()
			c.pending[1] = ch
			if err := c.enqueueLocked(queuedMessage{raw: []byte("must-not-send"), id: 1, ctx: ctx}); err != nil {
				t.Fatal(err)
			}
			c.mu.Unlock()
			if disable {
				c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
			} else {
				cancel()
			}
			defer cancel()
			// A sentinel active notification lets the test synchronize after a dropped
			// canceled request without sleeps. Stop path must not call write at all.
			wrote := make(chan string, 2)
			done := make(chan struct{})
			go func() {
				c.writer(func(_ context.Context, raw []byte) error {
					wrote <- string(raw)
					c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
					return nil
				})
				close(done)
			}()
			if !disable {
				if err := c.notify("sentinel", nil); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("writer stuck")
			}
			close(wrote)
			for value := range wrote {
				if value == "must-not-send" {
					t.Fatal("canceled queued request sent")
				}
			}
		})
	}
}
func TestRequestIDsRejectStringAndIgnoreLateResponses(t *testing.T) {
	c := newConnection(serverConfig{ID: "fixture"}, Options{RequestTimeout: time.Second})
	defer c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
	ch := make(chan response, 1)
	c.pending[1] = ch
	if err := c.receive([]byte(`{"jsonrpc":"2.0","id":"1","result":"wrong"}`)); err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatal("string ID settled numeric request")
	}
	if err := c.receive([]byte(`{"jsonrpc":"2.0","id":1,"result":"correct"}`)); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; string(got.result) != `"correct"` {
		t.Fatal("wrong response")
	}
	if err := c.receive([]byte(`{"jsonrpc":"2.0","id":1,"result":"late"}`)); err != nil {
		t.Fatal(err)
	}
	if len(ch) != 0 {
		t.Fatal("late response settled twice")
	}
}

// A Go test binary is the only synthetic stdio executable; CI must explicitly
// opt in. No Node/Python/helper is spawned during local development or tests.
func TestStdioLifecycleSyntheticGoHelper(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("synthetic subprocess allowed only in GitHub Actions")
	}
	m := configured(t, map[string]any{"command": os.Args[0], "args": []string{"-test.run=^TestMCPGoHelper$"}, "env": map[string]string{"GO_MCP_SYNTHETIC_HELPER": "1", "GITHUB_ACTIONS": "true"}})
	if err := m.Connect(context.Background(), "fixture"); err != nil {
		t.Fatal(err)
	}
	if got := m.CallTool(context.Background(), "fixture", "fixture", nil); !got.Success || got.Content != "native-go-helper" {
		t.Fatalf("stdio: %+v", got)
	}
	if got := m.ListTools(); len(got) != 1 || got[0]["name"] != "fixture" {
		t.Fatalf("stdio catalog: %+v", got)
	}
	m.Disconnect("fixture")
}
func TestMCPGoHelper(t *testing.T) {
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("GO_MCP_SYNTHETIC_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	lastID := float64(0)
	initialized := false
	for scanner.Scan() {
		var req map[string]any
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		if req["id"] == nil {
			if req["method"] == "notifications/initialized" {
				initialized = true
			}
			continue
		}
		id, ok := req["id"].(float64)
		if !ok || id <= lastID {
			os.Exit(3)
		}
		lastID = id
		if req["method"] == "initialize" {
			params, ok := req["params"].(map[string]any)
			if !ok || params["protocolVersion"] != "2024-11-05" {
				os.Exit(4)
			}
		} else if !initialized {
			os.Exit(5)
		}
		var result any = map[string]any{}
		switch req["method"] {
		case "tools/list":
			result = map[string]any{"tools": []map[string]any{fixtureTool()}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "tools/call":
			result = map[string]any{"content": []map[string]string{{"type": "text", "text": "native-go-helper"}}}
		}
		raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req["id"], "result": result})
		_, _ = fmt.Fprintln(os.Stdout, string(raw))
	}
	os.Exit(0)
}
func TestProjectionWhitelist(t *testing.T) {
	got, err := projectEntry(fixtureTool(), "fixture", false)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"name": "fixture", "description": "public synthetic", "inputSchema": map[string]any{"type": "object"}, "serverId": "fixture"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projection: %+v", got)
	}
}

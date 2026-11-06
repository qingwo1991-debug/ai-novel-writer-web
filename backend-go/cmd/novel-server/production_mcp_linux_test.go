//go:build linux

package main

// This file proves the eight mcp:* names over real HTTP through the frozen browser
// session: the pinned author data directory, the committed egress policy, and a
// synthetic MCP server that is a real httptest listener dialed through that policy.
// Nothing here fakes a dispatch: the only way a request reaches a stub is
// egress.Client.Do against an origin the committed config.json admits.
//
// The stub answers as a hostile one would: it echoes the credential the author
// stored in the configuration back in a tool result, so the answer scan below is
// evidence about the bytes that leave the server rather than about the intent of
// the code. A second stub is deliberately NOT admitted by the committed policy and
// proves, by counting its own requests, that a non-admitted origin is refused
// instead of dialed.

import (
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
	"strings"
	"sync"
	"testing"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/projectfs"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionMCPTestSecret is the credential the configuration carries. It is
	// long enough for the public sanitizer to register (MinSecretLength) and
	// distinctive enough that a scan of a whole answer body cannot match it by
	// accident.
	productionMCPTestSecret = "synthetic-mcp-credential-value"
	// productionMCPTestCommand and productionMCPTestArgument are the command entry
	// this lane must refuse by name. Neither may appear in any answer.
	productionMCPTestCommand  = "synthetic-mcp-stdio-binary"
	productionMCPTestArgument = "--synthetic-argument"
	// productionMCPTestEndpointPath is the MCP endpoint under each stub origin.
	productionMCPTestEndpointPath = "/mcp"
)

// productionMCPStub is one synthetic MCP server speaking streamable HTTP: one
// JSON-RPC message per POST, a JSON body or one event-stream frame back.
type productionMCPStub struct {
	mu      sync.Mutex
	methods []string
	// secret is echoed back inside a tool result, which is what makes the
	// credential scan meaningful.
	secret string
	// stream answers with text/event-stream instead of a JSON body.
	stream bool
}

func (s *productionMCPStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.Unmarshal(raw, &message) != nil || message.JSONRPC != "2.0" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.methods = append(s.methods, message.Method)
	stream := s.stream
	s.mu.Unlock()
	if message.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, ok := s.result(message.Method, message.Params)
	if message.Method == "initialize" {
		w.Header().Set("Mcp-Session-Id", "synthetic-mcp-session")
	}
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"jsonrpc":"2.0","id":`+string(message.ID)+`,"error":{"code":-32601,"message":"synthetic"}}`)
		return
	}
	answer := `{"jsonrpc":"2.0","id":` + string(message.ID) + `,"result":` + result + `}`
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, answer)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = io.WriteString(w, "event: message\ndata: "+answer+"\n\n")
}

// result answers one method, or reports that the stub does not implement it.
func (s *productionMCPStub) result(method string, params json.RawMessage) (string, bool) {
	switch method {
	case "initialize":
		return `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"synthetic","version":"1"}}`, true
	case "tools/list":
		return `{"tools":[{"name":"echo","description":"echo the text","inputSchema":{"type":"object"}}]}`, true
	case "resources/list":
		return `{"resources":[{"uri":"stub://one","name":"one","description":"one resource","mimeType":"text/plain"}]}`, true
	case "tools/call":
		var call struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(params, &call) != nil || call.Name != "echo" {
			return "", false
		}
		s.mu.Lock()
		secret := s.secret
		s.mu.Unlock()
		encoded, err := json.Marshal("echo:" + secret)
		if err != nil {
			return "", false
		}
		return `{"content":[{"type":"text","text":` + string(encoded) + `}]}`, true
	}
	return "", false
}

// total reports how many requests of any method reached this stub.
func (s *productionMCPStub) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.methods)
}

// count reports how many requests of one method reached this stub.
func (s *productionMCPStub) count(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	total := 0
	for _, candidate := range s.methods {
		if candidate == method {
			total++
		}
	}
	return total
}

// productionMCPFixture is one served lane: the admitted real data directory, the
// committed policy derived from it, the eight bound names, one admitted stub and
// one listener the policy deliberately does not admit.
type productionMCPFixture struct {
	root        string
	gateway     *httptest.Server
	admitted    *productionMCPStub
	admittedURL string
	outside     *productionMCPStub
	outsideURL  string
	// dead is an admitted origin with nothing listening on it: the honest
	// "configured, reachable in principle, but unreachable now" case.
	dead    *productionMCPStub
	deadURL string
}

func newProductionMCPFixture(t *testing.T) *productionMCPFixture {
	t.Helper()
	admittedStub := &productionMCPStub{secret: productionMCPTestSecret}
	admittedServer := httptest.NewServer(admittedStub)
	t.Cleanup(admittedServer.Close)
	outsideStub := &productionMCPStub{}
	outsideServer := httptest.NewServer(outsideStub)
	t.Cleanup(outsideServer.Close)
	deadStub := &productionMCPStub{}
	deadServer := httptest.NewServer(deadStub)
	deadOrigin := deadServer.URL
	deadURL := deadOrigin + productionMCPTestEndpointPath
	deadServer.Close()
	root := syntheticProductionDataDir(t)
	// config.json is rewritten so the committed allowlist names exactly the
	// admitted stub origin, the way an author commits modelOrigins. The outside
	// listener is deliberately absent from it, so connecting to it must be
	// refused rather than dialed.
	committed := `{"theme":"light","editorFontSize":17,"defaultModelId":"` + productionTestModelID +
		`","defaultEmbeddingModelId":"` + productionTestEmbeddingModelID +
		`","stageModelIds":{"drafting":"` + productionTestModelID +
		`"},"modelOrigins":["` + admittedServer.URL + `","` + deadOrigin + `"]}`
	generation := filepath.Join(root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration)
	if err := os.WriteFile(filepath.Join(generation, "config.json"), []byte(committed), 0600); err != nil {
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
	server := transport.New(migrationRegistry(t), security.NewOrigins("http://"+productionDurableAddress), "go-production-admitted")
	if err := newProductionMCP(open).Bind(server); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	mux := http.NewServeMux()
	mux.Handle("/api/", server)
	mux.Handle("/api", server)
	mux.Handle("/healthz", server)
	gateway := httptest.NewServer(mux)
	t.Cleanup(gateway.Close)
	return &productionMCPFixture{
		root:        root,
		gateway:     gateway,
		admitted:    admittedStub,
		admittedURL: admittedServer.URL + productionMCPTestEndpointPath,
		outside:     outsideStub,
		outsideURL:  outsideServer.URL + productionMCPTestEndpointPath,
		dead:        deadStub,
		deadURL:     deadURL,
	}
}

// client bootstraps one browser tab against the served lane.
func (f *productionMCPFixture) client(t *testing.T, tab string) *productionDurableClient {
	t.Helper()
	return newProductionDurableClient(t, f.gateway.Config.Handler, tab)
}

// configPath is the file this lane must read, inside the pinned generation.
func (f *productionMCPFixture) configPath() string {
	return filepath.Join(f.root, productionConfigDirectory, productionConfigTarget,
		"generations", productionTestGeneration, productionMCPConfigName)
}

// writeConfig commits one configuration document for the pinned generation.
func (f *productionMCPFixture) writeConfig(t *testing.T, servers map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		t.Fatal(err)
	}
	path := f.configPath()
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// productionMCPConfigWire is MCPConfigLoadResponse, decoded to what is asserted.
type productionMCPConfigWire struct {
	Success bool                       `json:"success"`
	Status  string                     `json:"status"`
	Error   string                     `json:"error"`
	Servers []productionMCPSummaryWire `json:"servers"`
}

type productionMCPSummaryWire struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
}

type productionMCPStatusWire struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ToolCount int    `json:"toolCount"`
	Error     string `json:"error"`
}

type productionMCPToolWire struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	ServerID    string         `json:"serverId"`
}

type productionMCPResourceWire struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
	ServerID    string `json:"serverId"`
}

type productionMCPCallWire struct {
	Success bool   `json:"success"`
	Content string `json:"content"`
	Error   string `json:"error"`
}

type productionMCPRefusedWire struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
}

// loadConfig asks the lane to load the author's configuration and returns both the
// decoded answer and the exact bytes, because the credential tests scan the bytes.
func (f *productionMCPFixture) loadConfig(t *testing.T, client *productionDurableClient) (productionMCPConfigWire, string) {
	t.Helper()
	response := client.call(t, "mcp:load-config", []any{})
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("mcp:load-config answered %d: %s", response.Code, body)
	}
	var envelope struct {
		Result productionMCPConfigWire `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("mcp:load-config is not the operation envelope: %v", err)
	}
	return envelope.Result, body
}

// statuses reads mcp:get-servers-status and returns the answer with its bytes.
func (f *productionMCPFixture) statuses(t *testing.T, client *productionDurableClient) ([]productionMCPStatusWire, string) {
	t.Helper()
	response := client.call(t, "mcp:get-servers-status", []any{})
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("mcp:get-servers-status answered %d: %s", response.Code, body)
	}
	var envelope struct {
		Result []productionMCPStatusWire `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("mcp:get-servers-status is not the operation envelope: %v", err)
	}
	return envelope.Result, body
}

// connect asks the lane to open one server and returns the refusal, if any.
func (f *productionMCPFixture) connect(t *testing.T, client *productionDurableClient, serverID string) (productionMCPRefusedWire, string) {
	t.Helper()
	response := client.call(t, "mcp:connect", []any{serverID})
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("mcp:connect answered %d: %s", response.Code, body)
	}
	var envelope struct {
		Result productionMCPRefusedWire `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("mcp:connect is not the operation envelope: %v", err)
	}
	return envelope.Result, body
}

// callTool asks the lane to invoke one tool and returns the answer with its bytes.
func (f *productionMCPFixture) callTool(t *testing.T, client *productionDurableClient, serverID, tool string, arguments map[string]any) (productionMCPCallWire, string) {
	t.Helper()
	response := client.call(t, "mcp:call-tool", []any{serverID, tool, arguments})
	body := response.Body.String()
	if response.Code != http.StatusOK {
		t.Fatalf("mcp:call-tool answered %d: %s", response.Code, body)
	}
	var envelope struct {
		Result productionMCPCallWire `json:"result"`
	}
	if err := json.Unmarshal([]byte(body), &envelope); err != nil {
		t.Fatalf("mcp:call-tool is not the operation envelope: %v", err)
	}
	return envelope.Result, body
}

// productionMCPStatusFor finds one server in a status answer.
func productionMCPStatusFor(t *testing.T, statuses []productionMCPStatusWire, serverID string) productionMCPStatusWire {
	t.Helper()
	for _, status := range statuses {
		if status.ID == serverID {
			return status
		}
	}
	t.Fatalf("mcp:get-servers-status did not report %s", serverID)
	return productionMCPStatusWire{}
}

// TestProductionMCPLoadConfigReadsTheAuthorDataDirectory proves the load half: the
// file comes from the pinned generation, both transports are reported with the
// renderer's own labels, and the summary is sorted so a page can rely on it.
func TestProductionMCPLoadConfigReadsTheAuthorDataDirectory(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	fixture.writeConfig(t, map[string]any{
		"echo": map[string]any{"url": fixture.admittedURL},
		"stdio-one": map[string]any{
			"command": productionMCPTestCommand,
			"args":    []any{productionMCPTestArgument},
			"env":     map[string]any{"MCP_TOKEN": productionMCPTestSecret},
		},
	})
	answer, body := fixture.loadConfig(t, client)
	if !answer.Success || answer.Status != "loaded" {
		t.Fatalf("mcp:load-config answered success=%v status=%s error=%s", answer.Success, answer.Status, answer.Error)
	}
	if len(answer.Servers) != 2 {
		t.Fatalf("mcp:load-config listed %d servers, want 2: %s", len(answer.Servers), body)
	}
	if answer.Servers[0].ID != "echo" || answer.Servers[1].ID != "stdio-one" {
		t.Fatalf("mcp:load-config did not sort by id: %s", body)
	}
	if answer.Servers[0].Transport != "sse" || answer.Servers[1].Transport != "stdio" {
		t.Fatalf("mcp:load-config reported the wrong transports: %s", body)
	}
	// The refused entry's command and credential never appear in the answer.
	for _, forbidden := range []string{productionMCPTestCommand, productionMCPTestArgument, productionMCPTestSecret} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("mcp:load-config published a configuration value: %s", body)
		}
	}
	// An excluded name stays unreachable: mcp:get-config-path is not served here.
	excluded := client.call(t, "mcp:get-config-path", []any{})
	if excluded.Code == http.StatusOK {
		t.Fatalf("mcp:get-config-path answered 200; it is excluded from the browser inventory")
	}
}

// TestProductionMCPStatusSeparatesUnconfiguredFromUnreachable is the question a page
// has to answer before it shows anything: is there no configuration at all, or is
// there one that cannot be used? An empty status list is the first; an entry in
// "error" carrying a symbolic code is the second.
func TestProductionMCPStatusSeparatesUnconfiguredFromUnreachable(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)

	// Phase one: the pinned generation holds no configuration at all.
	answer, _ := fixture.loadConfig(t, client)
	// "missing" is a successful load that found nothing: the renderer's union says
	// success:true for it (src/shared/ipc-channels.ts:1331), and a page must be
	// able to tell "no file" from "bad file".
	if !answer.Success || answer.Status != "missing" {
		t.Fatalf("mcp:load-config on a data directory with no file answered success=%v status=%s error=%s", answer.Success, answer.Status, answer.Error)
	}
	if len(answer.Servers) != 0 {
		t.Fatalf("mcp:load-config listed %d servers for a missing file", len(answer.Servers))
	}
	unconfigured, _ := fixture.statuses(t, client)
	if len(unconfigured) != 0 {
		t.Fatalf("mcp:get-servers-status reported %d servers while nothing is configured", len(unconfigured))
	}

	// Phase two: one admitted origin with nothing listening, one live server and
	// one command entry. The dead origin is inside the committed allowlist, so it
	// is dialed and fails; that is what makes it different from privileged.
	fixture.writeConfig(t, map[string]any{
		"dead":  map[string]any{"url": fixture.deadURL},
		"echo":  map[string]any{"url": fixture.admittedURL},
		"stdio": map[string]any{"command": productionMCPTestCommand},
	})
	loaded, _ := fixture.loadConfig(t, client)
	if !loaded.Success || loaded.Status != "loaded" {
		t.Fatalf("mcp:load-config answered success=%v status=%s error=%s", loaded.Success, loaded.Status, loaded.Error)
	}
	statuses, _ := fixture.statuses(t, client)
	if len(statuses) != 3 {
		t.Fatalf("mcp:get-servers-status reported %d servers, want 3", len(statuses))
	}
	for _, status := range statuses {
		if status.ID == "stdio" {
			// Refused by name before any connect: checked on its own below.
			continue
		}
		if status.Status != "disconnected" {
			t.Fatalf("%s reported status %s before any connect, want disconnected",
				status.ID, status.Status)
		}
	}
	if refused := productionMCPStatusFor(t, statuses, "stdio"); refused.Status != "error" || refused.Error != "MCP_STDIO_TRANSPORT_UNAVAILABLE" {
		t.Fatalf("the command entry answered status=%s error=%s", refused.Status, refused.Error)
	}

	// The reachable server connects and reports its real tool count.
	if connected, body := fixture.connect(t, client, "echo"); !connected.Success {
		t.Fatalf("mcp:connect refused a live configured server: %s", body)
	}
	// The dead one is configured and dialed, so its failure is a connection
	// failure rather than a policy refusal.
	if dead, body := fixture.connect(t, client, "dead"); dead.Success || dead.Error != "MCP_CONNECT_FAILED" {
		t.Fatalf("mcp:connect answered success=%v error=%s for a closed listener: %s", dead.Success, dead.Error, body)
	}
	statuses, _ = fixture.statuses(t, client)
	if live := productionMCPStatusFor(t, statuses, "echo"); live.Status != "connected" || live.ToolCount != 1 {
		t.Fatalf("the connected server answered status=%s toolCount=%d", live.Status, live.ToolCount)
	}
	if dead := productionMCPStatusFor(t, statuses, "dead"); dead.Status != "error" || dead.Error != "MCP_CONNECT_FAILED" {
		t.Fatalf("the unreachable server answered status=%s error=%s", dead.Status, dead.Error)
	}
	// ... which is exactly the distinction the reference could not draw: a page
	// here sees three configured servers, not zero.
	if len(statuses) == len(unconfigured) {
		t.Fatalf("configured-but-unreachable looks identical to unconfigured")
	}
}

// TestProductionMCPRefusesNonAdmittedOriginWithoutDialing proves the committed
// policy boundary: a configured URL outside the committed allowlist is refused with
// its own code, and the listening server behind it records no request at all.
func TestProductionMCPRefusesNonAdmittedOriginWithoutDialing(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	// Both stubs are listening and configured; the only difference between them is
	// that one origin is in the committed policy and the other is not.
	fixture.writeConfig(t, map[string]any{
		"echo":    map[string]any{"url": fixture.admittedURL},
		"outside": map[string]any{"url": fixture.outsideURL},
	})
	if answer, _ := fixture.loadConfig(t, client); !answer.Success {
		t.Fatalf("mcp:load-config refused a well-formed configuration: %s", answer.Error)
	}
	statuses, _ := fixture.statuses(t, client)
	if outside := productionMCPStatusFor(t, statuses, "outside"); outside.Status != "error" || outside.Error != "MCP_ORIGIN_NOT_ADMITTED" {
		t.Fatalf("a non-admitted origin answered status=%s error=%s", outside.Status, outside.Error)
	}
	// The control, in this same test: the admitted stub is dialed and connects.
	// Without it a zero on the other stub could mean a polite refusal or a counter
	// nothing ever reaches, and those are opposite conclusions. Its answer is
	// carried into both refusals below so a broken control reports itself instead
	// of the symptom it would produce here.
	control, controlBody := fixture.connect(t, client, "echo")
	if !control.Success {
		t.Fatalf("mcp:connect refused the ADMITTED server (success=%v error=%s): %s", control.Success, control.Error, controlBody)
	}
	refused, body := fixture.connect(t, client, "outside")
	if refused.Success || refused.Error != "MCP_ORIGIN_NOT_ADMITTED" {
		t.Fatalf("mcp:connect answered success=%v error=%s for the non-admitted origin (the admitted control answered success=%v error=%s): %s",
			refused.Success, refused.Error, control.Success, control.Error, body)
	}
	// Same measurement on both stubs, read after both connects: one handshake on
	// the admitted side, none at all on the refused side.
	if reached := fixture.admitted.count("initialize"); reached == 0 {
		t.Fatalf("the admitted stub answered mcp:connect (success=%v error=%s) but recorded %d handshakes, so these counters measure nothing: %s",
			control.Success, control.Error, reached, controlBody)
	}
	if reached := fixture.outside.count("initialize"); reached != 0 {
		t.Fatalf("a non-admitted origin was dialed %d times", reached)
	}
	if reached := fixture.outside.total(); reached != 0 {
		t.Fatalf("a non-admitted origin received %d requests of any method", reached)
	}
	statuses, _ = fixture.statuses(t, client)
	if outside := productionMCPStatusFor(t, statuses, "outside"); outside.Status != "error" || outside.Error != "MCP_ORIGIN_NOT_ADMITTED" {
		t.Fatalf("the refusal did not survive into status: %s/%s", outside.Status, outside.Error)
	}
}

// TestProductionMCPRefusesUnknownServerAndUnadvertisedTool proves a caller can name
// only what the author's own configuration and the server's own catalog contain.
func TestProductionMCPRefusesUnknownServerAndUnadvertisedTool(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	fixture.writeConfig(t, map[string]any{
		"echo": map[string]any{"url": fixture.admittedURL},
	})
	if answer, _ := fixture.loadConfig(t, client); !answer.Success {
		t.Fatalf("mcp:load-config refused a well-formed configuration: %s", answer.Error)
	}
	// An invented server id is refused as not configured, on every name that takes
	// one.
	if refused, body := fixture.connect(t, client, "invented"); refused.Success || refused.Error != "MCP_SERVER_NOT_CONFIGURED" {
		t.Fatalf("mcp:connect answered success=%v error=%s for an invented id: %s", refused.Success, refused.Error, body)
	}
	if call, body := fixture.callTool(t, client, "invented", "echo", map[string]any{}); call.Success || call.Error != "MCP_SERVER_NOT_CONFIGURED" {
		t.Fatalf("mcp:call-tool answered success=%v error=%s for an invented id: %s", call.Success, call.Error, body)
	}
	// A configured server that is not connected answers the disconnected code, and
	// contributes nothing to the catalogs.
	if call, _ := fixture.callTool(t, client, "echo", "echo", map[string]any{}); call.Success || call.Error != "MCP_SERVER_NOT_CONNECTED" {
		t.Fatalf("mcp:call-tool answered success=%v error=%s before any connect", call.Success, call.Error)
	}
	var tools []productionMCPToolWire
	client.decode(t, "mcp:list-tools", []any{}, &tools)
	if len(tools) != 0 {
		t.Fatalf("mcp:list-tools listed %d tools while nothing is connected", len(tools))
	}
	// Connected, but a tool the server never advertised is still refused before it
	// leaves the process.
	if connected, _ := fixture.connect(t, client, "echo"); !connected.Success {
		t.Fatal("mcp:connect refused the live configured server")
	}
	callsBefore := fixture.admitted.count("tools/call")
	if call, _ := fixture.callTool(t, client, "echo", "not-advertised", map[string]any{}); call.Success || call.Error != "MCP_TOOL_NOT_DISCOVERED" {
		t.Fatalf("mcp:call-tool answered success=%v error=%s for an unadvertised tool", call.Success, call.Error)
	}
	if after := fixture.admitted.count("tools/call"); after != callsBefore {
		t.Fatalf("an unadvertised tool reached the server %d times", after-callsBefore)
	}
	// The advertised one is served, with both catalogs from one connection.
	client.decode(t, "mcp:list-tools", []any{}, &tools)
	if len(tools) != 1 || tools[0].Name != "echo" || tools[0].ServerID != "echo" || tools[0].Description == "" {
		t.Fatalf("mcp:list-tools answered %+v", tools)
	}
	if _, ok := tools[0].InputSchema["type"]; !ok {
		t.Fatalf("mcp:list-tools dropped the advertised input schema: %+v", tools[0])
	}
	var resources []productionMCPResourceWire
	client.decode(t, "mcp:list-resources", []any{}, &resources)
	if len(resources) != 1 || resources[0].URI != "stub://one" || resources[0].ServerID != "echo" || resources[0].MIMEType != "text/plain" {
		t.Fatalf("mcp:list-resources answered %+v", resources)
	}
}

// TestProductionMCPCredentialNeverAppearsInAnAnswer is the credential proof: the
// configuration holds a token, the server echoes it back inside a tool result, and
// every answer body the lane produced is scanned for the exact string. The call
// itself must have succeeded, so the scan is over an answer that would have carried
// the credential had the redaction not been in place.
func TestProductionMCPCredentialNeverAppearsInAnAnswer(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	fixture.writeConfig(t, map[string]any{
		"echo": map[string]any{
			"url": fixture.admittedURL,
			"env": map[string]any{"MCP_TOKEN": productionMCPTestSecret},
		},
	})
	bodies := make([]string, 0, 8)
	answer, body := fixture.loadConfig(t, client)
	bodies = append(bodies, body)
	if !answer.Success {
		t.Fatalf("mcp:load-config refused a well-formed configuration: %s", answer.Error)
	}
	statuses, body := fixture.statuses(t, client)
	bodies = append(bodies, body)
	if len(statuses) != 1 {
		t.Fatalf("mcp:get-servers-status reported %d servers, want 1", len(statuses))
	}
	connected, body := fixture.connect(t, client, "echo")
	bodies = append(bodies, body)
	if !connected.Success {
		t.Fatalf("mcp:connect refused the live configured server: %s", connected.Error)
	}
	response := client.call(t, "mcp:list-tools", []any{})
	bodies = append(bodies, response.Body.String())
	if response.Code != http.StatusOK {
		t.Fatalf("mcp:list-tools answered %d", response.Code)
	}
	var envelope struct {
		Result []productionMCPToolWire `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Result) != 1 {
		t.Fatalf("mcp:list-tools did not answer the one advertised tool: %v", err)
	}
	call, body := fixture.callTool(t, client, "echo", "echo", map[string]any{"text": "hello"})
	bodies = append(bodies, body)
	if !call.Success || call.Content == "" {
		t.Fatalf("mcp:call-tool answered success=%v error=%s", call.Success, call.Error)
	}
	if fixture.admitted.count("tools/call") != 1 {
		t.Fatalf("the tool call did not reach the admitted server")
	}
	for index, candidate := range bodies {
		if strings.Contains(candidate, productionMCPTestSecret) {
			t.Fatalf("answer %d published the configured credential: %s", index, candidate)
		}
	}
	// The echo proves the answer really was built from a server that sent the
	// credential back, so "no secret" is not the same as "no answer".
	if !strings.Contains(call.Content, "echo:") {
		t.Fatalf("the tool answer was not relayed: %s", call.Content)
	}
}

// TestProductionMCPRefusesStdioEntryByName proves a command entry is refused rather
// than attempted, and that the neighbouring URL entry keeps working.
func TestProductionMCPRefusesStdioEntryByName(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	path := fixture.writeConfig(t, map[string]any{
		"stdio": map[string]any{
			"command": productionMCPTestCommand,
			"args":    []any{productionMCPTestArgument},
			"env":     map[string]any{"MCP_TOKEN": productionMCPTestSecret},
		},
		"echo": map[string]any{"url": fixture.admittedURL},
	})
	if answer, _ := fixture.loadConfig(t, client); !answer.Success {
		t.Fatalf("one command entry refused the whole file: %s", answer.Error)
	}
	refused, body := fixture.connect(t, client, "stdio")
	if refused.Success || refused.Error != "MCP_STDIO_TRANSPORT_UNAVAILABLE" {
		t.Fatalf("mcp:connect answered success=%v error=%s for a command entry: %s", refused.Success, refused.Error, body)
	}
	for _, forbidden := range []string{productionMCPTestCommand, productionMCPTestArgument, productionMCPTestSecret} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("the refusal published a configuration value: %s", body)
		}
	}
	if connected, _ := fixture.connect(t, client, "echo"); !connected.Success {
		t.Fatal("the command entry's refusal broke the neighbouring URL entry")
	}
	statuses, _ := fixture.statuses(t, client)
	if stdio := productionMCPStatusFor(t, statuses, "stdio"); stdio.Status != "error" || stdio.Error != "MCP_STDIO_TRANSPORT_UNAVAILABLE" {
		t.Fatalf("the command entry answered status=%s error=%s", stdio.Status, stdio.Error)
	}
	if live := productionMCPStatusFor(t, statuses, "echo"); live.Status != "connected" {
		t.Fatalf("the URL entry answered status=%s", live.Status)
	}
	// The configuration on disk is the author's; loading it wrote nothing.
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), productionMCPTestCommand) {
		t.Fatalf("the configuration was rewritten: %s", string(raw))
	}
}

// TestProductionMCPRefusalsLeaveTheDatabaseByteIdentical proves every refusal writes
// nothing: the author database is digested through a separate mode=ro connection
// before and after a battery of refusals, and the configuration file is digested the
// same way.
func TestProductionMCPRefusalsLeaveTheDatabaseByteIdentical(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	projectPath := filepath.Join(fixture.root, productionLibraryDirectory, "current-novel")
	path := fixture.writeConfig(t, map[string]any{
		"echo":    map[string]any{"url": fixture.admittedURL},
		"stdio":   map[string]any{"command": productionMCPTestCommand},
		"outside": map[string]any{"url": fixture.outsideURL},
		"broken":  map[string]any{"url": "stdio://not-a-server"},
	})
	before := productionMCPDatabaseDigest(t, projectPath)
	configBefore := productionMCPFileDigest(t, path)

	// Every refusal this family can answer, in one run.
	fixture.loadConfig(t, client)
	for _, serverID := range []string{"stdio", "outside", "broken", "invented"} {
		if refused, _ := fixture.connect(t, client, serverID); refused.Success {
			t.Fatalf("mcp:connect accepted the server %s", serverID)
		}
		if call, _ := fixture.callTool(t, client, serverID, "echo", map[string]any{}); call.Success {
			t.Fatalf("mcp:call-tool accepted the server %s", serverID)
		}
	}
	fixture.connect(t, client, "echo")
	fixture.callTool(t, client, "echo", "not-advertised", map[string]any{})

	// The two disconnect verbs are answered here, and each answer is checked against
	// the state it claims to have produced: a server that was connected is gone from
	// the status answer, out of the catalog, and refused on a tool call instead of
	// being served from the session that was just dropped.
	for _, step := range []struct {
		operation string
		args      []any
	}{
		{"mcp:disconnect", []any{"echo"}},
		{"mcp:disconnect-all", []any{}},
	} {
		if connected, _ := fixture.connect(t, client, "echo"); !connected.Success {
			t.Fatalf("mcp:connect refused to reconnect the live server before %s: %s", step.operation, connected.Error)
		}
		if statuses, _ := fixture.statuses(t, client); productionMCPStatusFor(t, statuses, "echo").Status != "connected" {
			t.Fatalf("the reconnected server is not reported connected before %s", step.operation)
		}
		var answer productionMCPRefusedWire
		client.decode(t, step.operation, step.args, &answer)
		if !answer.Success || answer.Error != "" {
			t.Fatalf("%s answered success=%v error=%s", step.operation, answer.Success, answer.Error)
		}
		after, _ := fixture.statuses(t, client)
		if gone := productionMCPStatusFor(t, after, "echo"); gone.Status != "disconnected" || gone.ToolCount != 0 || gone.Error != "" {
			t.Fatalf("after %s the server is still reported status=%s tools=%d error=%s", step.operation, gone.Status, gone.ToolCount, gone.Error)
		}
		var tools []productionMCPToolWire
		client.decode(t, "mcp:list-tools", []any{}, &tools)
		if len(tools) != 0 {
			t.Fatalf("after %s mcp:list-tools still advertises %d tools", step.operation, len(tools))
		}
		dispatched := fixture.admitted.count("tools/call")
		call, body := fixture.callTool(t, client, "echo", "echo", map[string]any{"text": "after-disconnect"})
		if call.Success || call.Error != "MCP_SERVER_NOT_CONNECTED" || call.Content != "" {
			t.Fatalf("after %s a tool call was answered from a stale session: success=%v error=%s %s", step.operation, call.Success, call.Error, body)
		}
		if reached := fixture.admitted.count("tools/call"); reached != dispatched {
			t.Fatalf("after %s the refused tool call still reached the server %d times", step.operation, reached-dispatched)
		}
	}
	client.call(t, "mcp:get-servers-status", []any{})
	client.call(t, "mcp:list-resources", []any{})

	if after := productionMCPDatabaseDigest(t, projectPath); after != before {
		t.Fatalf("a refusal changed the author database: %s -> %s", before, after)
	}
	if after := productionMCPFileDigest(t, path); after != configBefore {
		t.Fatalf("a refusal changed the configuration file: %s -> %s", configBefore, after)
	}
}

// TestProductionMCPRefusesWrongArity pins the wire arities against the renderer's
// declarations (src/shared/ipc-channels.ts:1336-1343): anything else is the
// transport's INVALID_ARGUMENTS, never a guessed call.
func TestProductionMCPRefusesWrongArity(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	for _, operation := range []struct {
		name string
		args []any
	}{
		{"mcp:load-config", []any{"extra"}},
		{"mcp:connect", []any{}},
		{"mcp:connect", []any{"echo", "extra"}},
		{"mcp:disconnect", []any{}},
		{"mcp:disconnect-all", []any{"extra"}},
		{"mcp:list-tools", []any{"extra"}},
		{"mcp:list-resources", []any{"extra"}},
		{"mcp:get-servers-status", []any{"extra"}},
		{"mcp:call-tool", []any{"echo", "echo"}},
		{"mcp:call-tool", []any{"echo", "echo", map[string]any{}, "extra"}},
		{"mcp:call-tool", []any{"echo", "echo", "not-an-object"}},
		{"mcp:call-tool", []any{"", "echo", map[string]any{}}},
	} {
		response := client.call(t, operation.name, operation.args)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s answered %d for %d arguments", operation.name, response.Code, len(operation.args))
		}
		if !strings.Contains(response.Body.String(), "INVALID_ARGUMENTS") {
			t.Fatalf("%s did not answer INVALID_ARGUMENTS: %s", operation.name, response.Body.String())
		}
	}
}

// TestProductionMCPRefusesANonPrivateConfigFile proves the file admission: a
// credential-bearing configuration another principal could read is not admitted at
// all, and the lane says so instead of parsing it.
func TestProductionMCPRefusesANonPrivateConfigFile(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	path := fixture.writeConfig(t, map[string]any{
		"echo": map[string]any{"url": fixture.admittedURL},
	})
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	answer, body := fixture.loadConfig(t, client)
	if answer.Success || answer.Status != "error" || answer.Error != "MCP_CONFIG_UNREADABLE" {
		t.Fatalf("a world-readable configuration answered success=%v status=%s error=%s", answer.Success, answer.Status, answer.Error)
	}
	statuses, _ := fixture.statuses(t, client)
	if len(statuses) != 0 {
		t.Fatalf("an unreadable configuration still reported %d servers: %s", len(statuses), body)
	}
}

// TestProductionMCPRefusesAnInvalidConfigShape proves a configuration this lane
// cannot interpret is refused as a whole - no entry is half-admitted - while the
// pinned generation still admits the file.
func TestProductionMCPRefusesAnInvalidConfigShape(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	// An id the committed generation's own validator does not bound, but this
	// lane's parser does.
	fixture.writeConfig(t, map[string]any{
		strings.Repeat("long-id-", 40): map[string]any{"url": fixture.admittedURL},
	})
	answer, body := fixture.loadConfig(t, client)
	if answer.Success || answer.Status != "error" || answer.Error != "MCP_CONFIG_INVALID" {
		t.Fatalf("an uninterpretable configuration answered success=%v status=%s error=%s: %s", answer.Success, answer.Status, answer.Error, body)
	}
	if len(answer.Servers) != 0 {
		t.Fatalf("a refused configuration listed %d servers", len(answer.Servers))
	}
	// Nothing survived the refusal: no entry, no session, no catalog, and no
	// server id of the refused document is callable.
	var tools []productionMCPToolWire
	client.decode(t, "mcp:list-tools", []any{}, &tools)
	if len(tools) != 0 {
		t.Fatalf("a refused configuration still advertised %d tools", len(tools))
	}
	if call, _ := fixture.callTool(t, client, "echo", "echo", map[string]any{}); call.Success {
		t.Fatal("a refused configuration left a callable server")
	}
}

// TestProductionMCPReadsAnEventStreamAnswer proves the second half of the streamable
// HTTP grammar: a server that answers with one event-stream frame is read, not
// rejected, because real servers answer tools/call that way when they stream
// progress.
func TestProductionMCPReadsAnEventStreamAnswer(t *testing.T) {
	fixture := newProductionMCPFixture(t)
	client := fixture.client(t, productionTestTab)
	fixture.admitted.mu.Lock()
	fixture.admitted.stream = true
	fixture.admitted.mu.Unlock()
	fixture.writeConfig(t, map[string]any{
		"echo": map[string]any{"url": fixture.admittedURL},
	})
	if answer, _ := fixture.loadConfig(t, client); !answer.Success {
		t.Fatalf("mcp:load-config refused a well-formed configuration: %s", answer.Error)
	}
	if connected, body := fixture.connect(t, client, "echo"); !connected.Success {
		t.Fatalf("mcp:connect refused an event-stream server: %s", body)
	}
	call, body := fixture.callTool(t, client, "echo", "echo", map[string]any{"text": "hello"})
	if !call.Success || call.Content == "" {
		t.Fatalf("mcp:call-tool answered success=%v error=%s: %s", call.Success, call.Error, body)
	}
	if strings.Contains(body, productionMCPTestSecret) {
		t.Fatalf("the event-stream answer published the configured credential: %s", body)
	}
}

// productionMCPDatabaseDigest proves the author database's bytes through a SEPARATE
// read-only connection: SQLite opens the file in mode=ro first, so a stale sidecar
// cannot hide behind the digest, and only then are the bytes hashed.
func productionMCPDatabaseDigest(t *testing.T, projectPath string) string {
	t.Helper()
	database := filepath.Join(projectPath, projectfs.StorageDirectory, projectfs.DatabaseName)
	uri := url.URL{Scheme: "file", Path: filepath.ToSlash(database)}
	uri.RawQuery = "mode=ro"
	handle, err := sql.Open("sqlite3", uri.String())
	if err != nil {
		t.Fatalf("open the author database read-only: %v", err)
	}
	handle.SetMaxOpenConns(1)
	var tables int
	if err := handle.QueryRow(`SELECT count(*) FROM sqlite_master`).Scan(&tables); err != nil {
		_ = handle.Close()
		t.Fatalf("read the author schema read-only: %v", err)
	}
	if err := handle.Close(); err != nil {
		t.Fatalf("close the read-only connection: %v", err)
	}
	if tables <= 0 {
		t.Fatalf("the author database has no schema")
	}
	return productionMCPFileDigest(t, database)
}

// productionMCPFileDigest returns one file's SHA-256.
func productionMCPFileDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the file under test: %v", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

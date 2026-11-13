// Package mcp implements administrator-configured MCP clients in native Go.
// External tools are trusted programs, NOT sandboxed programs: they run as the
// server identity and may independently require Node/Python. No core business
// operation uses a Node subprocess. A browser can select only a loaded server ID.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	MaxRequestBytes   = 1024 * 1024
	MaxFrameBytes     = 4 * 1024 * 1024
	MaxQueuedBytes    = 4 * 1024 * 1024
	MaxPending        = 32
	MaxCatalogEntries = 512
	MaxConfigBytes    = 1024 * 1024
)

type Options struct {
	ConfigPath string
	RuntimeDir string
	// TrustedAdminGrant is host configuration, never an operation argument.
	TrustedAdminGrant bool
	// HTTPClient is retained for source compatibility but intentionally ignored:
	// SSE always uses a private exact-origin egress client, never ambient dispatch.
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	// RegisterSecret is a host-only redactor registry; never a browser callback.
	// It must be non-reentrant: configuration admission holds the manager lock.
	RegisterSecret func(string) (bool, error)
}
type ServerSummary struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
}
type ConfigResult struct {
	Success bool            `json:"success"`
	Status  string          `json:"status"`
	Servers []ServerSummary `json:"servers"`
	Error   string          `json:"error,omitempty"`
}
type ServerStatus struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	ToolCount int    `json:"toolCount"`
	Error     string `json:"error,omitempty"`
}
type CallResult struct {
	Success bool   `json:"success"`
	Content string `json:"content"`
	Error   string `json:"error,omitempty"`
}
type serverConfig struct {
	ID      string
	Command string
	Args    []string
	Env     map[string]string
	URL     string
}

func (c serverConfig) transport() string {
	if c.URL != "" {
		return "sse"
	}
	return "stdio"
}

type Manager struct {
	mu      sync.Mutex
	options Options
	enabled bool
	configs map[string]serverConfig
	servers map[string]*connection
}

func New(options Options) *Manager {
	if options.RequestTimeout <= 0 {
		options.RequestTimeout = 10 * time.Second
	}
	if options.RuntimeDir == "" && options.ConfigPath != "" {
		options.RuntimeDir = filepath.Join(filepath.Dir(options.ConfigPath), "mcp-runtime")
	}
	return &Manager{options: options, configs: make(map[string]serverConfig), servers: make(map[string]*connection)}
}
func (m *Manager) ConfigPath() string { return m.options.ConfigPath }
func (m *Manager) HasSecretRegistrar() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.options.RegisterSecret != nil
}

// SetSecretRegistrar must be called by the host before enabling this manager.
// Browser bindings must require it, because tool catalogs may echo credentials.
func (m *Manager) SetSecretRegistrar(register func(string) (bool, error)) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enabled || len(m.configs) > 0 || len(m.servers) > 0 {
		return errors.New("MCP_SECRET_REGISTRAR_LOCKED")
	}
	if register == nil {
		return errors.New("MCP_SECRET_REGISTRAR_REQUIRED")
	}
	m.options.RegisterSecret = register
	return nil
}
func (m *Manager) Enabled() bool { m.mu.Lock(); defer m.mu.Unlock(); return m.enabled }

// SetEnabled is exclusively for the host administrator integration. Enabling
// never loads configuration or starts a process; explicit Connect is required.
func (m *Manager) SetEnabled(enabled bool) error {
	m.mu.Lock()
	if enabled && !m.options.TrustedAdminGrant {
		m.mu.Unlock()
		return errors.New("MCP_TRUSTED_ADMIN_GRANT_REQUIRED")
	}
	m.enabled = enabled
	if !enabled {
		for id, c := range m.servers {
			c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
			delete(m.servers, id)
		}
		m.configs = make(map[string]serverConfig)
	}
	m.mu.Unlock()
	return nil
}
func (m *Manager) Close() { _ = m.SetEnabled(false) }

// ParseConfig is pure: it never reads files, executes commands or connects URLs.
// Its public result contains only original summary fields, not credentials.
func ParseConfig(raw []byte) ([]ServerSummary, error) {
	configs, err := parseConfig(raw)
	if err != nil {
		return nil, err
	}
	return summaries(configs), nil
}
func summaries(configs map[string]serverConfig) []ServerSummary {
	out := make([]ServerSummary, 0, len(configs))
	for id, c := range configs {
		out = append(out, ServerSummary{ID: id, Name: id, Transport: c.transport()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func parseConfig(raw []byte) (map[string]serverConfig, error) {
	invalid := errors.New("MCP_CONFIG_INVALID")
	if len(raw) > MaxConfigBytes {
		return nil, invalid
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(raw, &root) != nil || root == nil {
		return nil, invalid
	}
	var entries map[string]json.RawMessage
	if json.Unmarshal(root["mcpServers"], &entries) != nil || entries == nil || len(entries) > MaxCatalogEntries {
		return nil, invalid
	}
	out := make(map[string]serverConfig, len(entries))
	for id, value := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(value, &fields) != nil || fields == nil {
			return nil, invalid
		}
		c := serverConfig{ID: id}
		for key, target := range map[string]any{"command": &c.Command, "url": &c.URL, "args": &c.Args, "env": &c.Env} {
			if v, ok := fields[key]; ok {
				if string(v) == "null" || json.Unmarshal(v, target) != nil {
					return nil, invalid
				}
			}
		}
		// encoding/json accepts null inside []string/map[string]string; the
		// original contract rejects every non-string entry explicitly.
		if v, ok := fields["args"]; ok {
			var items []any
			if json.Unmarshal(v, &items) != nil {
				return nil, invalid
			}
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return nil, invalid
				}
			}
		}
		if v, ok := fields["env"]; ok {
			var items map[string]any
			if json.Unmarshal(v, &items) != nil {
				return nil, invalid
			}
			for _, item := range items {
				if _, ok := item.(string); !ok {
					return nil, invalid
				}
			}
		}
		hasCommand := strings.TrimSpace(c.Command) != ""
		hasURL := strings.TrimSpace(c.URL) != ""
		if hasCommand == hasURL || strings.ContainsRune(c.Command, 0) {
			return nil, invalid
		}
		for _, arg := range c.Args {
			if strings.ContainsRune(arg, 0) {
				return nil, invalid
			}
		}
		for key, val := range c.Env {
			if key == "" || strings.ContainsAny(key, "=\x00") || strings.ContainsRune(val, 0) {
				return nil, invalid
			}
		}
		if hasURL {
			c.URL = strings.TrimSpace(c.URL)
		}
		out[id] = c
	}
	return out, nil
}

// LoadConfig revokes connections on invalid/missing config and disconnects any
// removed or changed entry. This holds the short host config lock throughout the
// bounded read so disable/config reload cannot resurrect stale connections.
func (m *Manager) LoadConfig() ConfigResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := ConfigResult{Status: "error", Servers: []ServerSummary{}, Error: "MCP_CONFIG_INVALID"}
	if !m.enabled {
		result.Error = "MCP_DISABLED"
		return result
	}
	raw, err := readConfigFile(m.options.ConfigPath)
	var configs map[string]serverConfig
	if err == nil {
		configs, err = parseConfig(raw)
	}
	if err == nil && m.options.RegisterSecret != nil {
		// Register before any process can echo explicit configuration credentials.
		// A false result is permitted (e.g. registry minimum secret length); only
		// registration errors revoke admission. Short values have the host registry's
		// documented redaction limitation, not a promise of universal secret removal.
		for _, cfg := range configs {
			values := make([]string, 0, len(cfg.Env)+1)
			for _, value := range cfg.Env {
				values = append(values, value)
			}
			if cfg.URL != "" {
				values = append(values, cfg.URL)
				if parsed, parseErr := url.Parse(cfg.URL); parseErr == nil {
					for _, items := range parsed.Query() {
						values = append(values, items...)
					}
				}
			}
			for _, value := range values {
				if _, registerErr := m.options.RegisterSecret(value); registerErr != nil {
					err = errors.New("MCP_CONFIG_INVALID")
					break
				}
			}
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		for id, c := range m.servers {
			c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
			delete(m.servers, id)
		}
		m.configs = make(map[string]serverConfig)
		if errors.Is(err, os.ErrNotExist) {
			result.Success = true
			result.Status = "missing"
			result.Error = ""
		}
		return result
	}
	for id, c := range m.servers {
		next, ok := configs[id]
		if !ok || !reflect.DeepEqual(c.config, next) {
			c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
			delete(m.servers, id)
		}
	}
	m.configs = configs
	return ConfigResult{Success: true, Status: "loaded", Servers: summaries(configs)}
}
func (m *Manager) Connect(ctx context.Context, id string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return errors.New("MCP_REQUEST_CANCELED")
	}
	m.mu.Lock()
	if !m.enabled {
		m.mu.Unlock()
		return errors.New("MCP_DISABLED")
	}
	cfg, ok := m.configs[id]
	if !ok {
		m.mu.Unlock()
		return errors.New("MCP_SERVER_NOT_CONFIGURED")
	}
	if old := m.servers[id]; old != nil {
		old.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
	}
	c := newConnection(cfg, m.options)
	m.servers[id] = c
	m.mu.Unlock()
	err := c.connect(ctx)
	if err != nil {
		c.stop("error", errorCode(err, "MCP_CONNECT_FAILED"))
		return err
	}
	// A simultaneous disable/reload/reconnect may have replaced this generation.
	m.mu.Lock()
	current := m.enabled && m.servers[id] == c
	m.mu.Unlock()
	if !current {
		c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
		return errors.New("MCP_SERVER_NOT_CONNECTED")
	}
	return nil
}
func (m *Manager) Disconnect(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.servers[id]; c != nil {
		c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
		delete(m.servers, id)
	}
}
func (m *Manager) DisconnectAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, c := range m.servers {
		c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
		delete(m.servers, id)
	}
}
func (m *Manager) ServersStatus() []ServerStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ServerStatus, 0, len(m.servers))
	for id, c := range m.servers {
		c.mu.Lock()
		out = append(out, ServerStatus{ID: id, Name: id, Status: c.status, ToolCount: len(c.tools), Error: c.failure})
		c.mu.Unlock()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (m *Manager) catalog(resources bool) []map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]map[string]any, 0)
	ids := make([]string, 0, len(m.servers))
	for id := range m.servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := m.servers[id]
		c.mu.Lock()
		if c.status == "connected" {
			entries := c.tools
			if resources {
				entries = c.resources
			}
			for _, entry := range entries {
				out = append(out, cloneObject(entry))
			}
		}
		c.mu.Unlock()
	}
	return out
}
func (m *Manager) ListTools() []map[string]any     { return m.catalog(false) }
func (m *Manager) ListResources() []map[string]any { return m.catalog(true) }
func (m *Manager) CallTool(ctx context.Context, id, name string, args map[string]any) CallResult {
	fail := func(code string) CallResult { return CallResult{Content: "", Error: code} }
	m.mu.Lock()
	c := m.servers[id]
	enabled := m.enabled
	m.mu.Unlock()
	if !enabled || c == nil {
		return fail("MCP_SERVER_NOT_CONNECTED")
	}
	c.mu.Lock()
	connected := c.status == "connected"
	found := false
	for _, tool := range c.tools {
		if tool["name"] == name {
			found = true
			break
		}
	}
	c.mu.Unlock()
	if !connected {
		return fail("MCP_SERVER_NOT_CONNECTED")
	}
	if !found {
		return fail("MCP_TOOL_NOT_DISCOVERED")
	}
	if args == nil {
		args = map[string]any{}
	}
	result, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return fail(errorCode(err, "MCP_REQUEST_FAILED"))
	}
	var body struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(result, &body) != nil {
		return fail("MCP_REQUEST_FAILED")
	}
	text := make([]string, 0, len(body.Content))
	for _, part := range body.Content {
		if part.Type == "text" {
			text = append(text, part.Text)
		}
	}
	return CallResult{Success: true, Content: strings.Join(text, "\n")}
}
func cloneObject(in map[string]any) map[string]any {
	raw, _ := json.Marshal(in)
	out := make(map[string]any)
	_ = json.Unmarshal(raw, &out)
	return out
}
func errorCode(err error, fallback string) string {
	if err == nil {
		return fallback
	}
	switch err.Error() {
	case "MCP_DISABLED", "MCP_SERVER_NOT_CONFIGURED", "MCP_CONNECT_FAILED", "MCP_SERVER_NOT_CONNECTED", "MCP_REQUEST_FAILED", "MCP_REQUEST_TIMEOUT", "MCP_REQUEST_CANCELED", "MCP_REQUEST_TOO_LARGE", "MCP_REQUEST_INVALID", "MCP_PENDING_LIMIT_EXCEEDED", "MCP_INPUT_LIMIT_EXCEEDED", "MCP_STDOUT_LIMIT_EXCEEDED", "MCP_STDOUT_INVALID", "MCP_STDIO_WRITE_FAILED", "MCP_SSE_INVALID", "MCP_SSE_ENDPOINT_INVALID", "MCP_SSE_POST_FAILED":
		return err.Error()
	}
	return fallback
}

package mcphost

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// This file is the client half of the protocol: one JSON-RPC message per HTTP
// POST, against one endpoint the committed policy admitted.
//
// It speaks MCP streamable HTTP (2025-06-18): a POST whose body is a single
// JSON-RPC message, answered either with a JSON body or with an event stream
// carrying the same message. Both shapes are read here, and both are bounded by
// MaxFrameBytes, so a server cannot answer with an unbounded body. Server-to-
// client requests are never dispatched: this client advertises no sampling, no
// roots and no elicitation capability, and an unexpected request frame simply
// does not match the identifier this client is waiting for.

// Tool is one advertised tool, projected for the browser. InputSchema is a plain
// object so the answer needs no second encode/decode round trip and the public
// sanitizer can walk it like any other value.
type Tool struct {
	// Name is the tool name the server advertised.
	Name string
	// Description is the advertised description, empty when absent.
	Description string
	// InputSchema is the advertised JSON schema, empty when absent.
	InputSchema map[string]any
	// ServerID is the configured server that advertised this tool.
	ServerID string
}

// Resource is one advertised resource.
type Resource struct {
	// URI is the resource identifier the server advertised.
	URI string
	// Name is the advertised name.
	Name string
	// Description is the advertised description, empty when absent.
	Description string
	// MIMEType is the advertised media type, empty when absent.
	MIMEType string
	// ServerID is the configured server that advertised this resource.
	ServerID string
}

// toolDescriptor and resourceDescriptor are the wire shapes of one catalog entry.
type toolDescriptor struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

type resourceDescriptor struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description"`
	MIMEType    string `json:"mimeType"`
}

// transportClient is one live conversation with one configured server. It holds
// no request body and no catalog: the manager owns those, so a disconnect drops
// them by dropping this value.
type transportClient struct {
	policy         *Policy
	endpoint       *url.URL
	serverID       string
	requestTimeout time.Duration
	callTimeout    time.Duration
	mu             sync.Mutex
	nextID         uint64
	protocol       string
	sessionID      string
}

func newTransportClient(policy *Policy, endpoint *url.URL, serverID string, requestTimeout, callTimeout time.Duration) *transportClient {
	if requestTimeout <= 0 {
		requestTimeout = defaultRequestTimeout
	}
	if callTimeout <= 0 {
		callTimeout = defaultCallTimeout
	}
	return &transportClient{
		policy:         policy,
		endpoint:       endpoint,
		serverID:       serverID,
		requestTimeout: requestTimeout,
		callTimeout:    callTimeout,
		nextID:         1,
		protocol:       ProtocolVersion,
	}
}

// initialize performs the one handshake MCP requires, then tells the server the
// client is ready. The negotiated revision is what every later request declares.
func (t *transportClient) initialize(ctx context.Context) error {
	result, err := t.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": clientName, "version": clientVersion},
	}, t.requestTimeout)
	if err != nil {
		return err
	}
	var answer struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(result, &answer) != nil {
		return ErrProtocolInvalid
	}
	// A server that answers a revision this client cannot verify is refused here
	// rather than spoken to in an unknown dialect.
	if !validHeaderToken(answer.ProtocolVersion) {
		return ErrProtocolInvalid
	}
	t.recordProtocol(answer.ProtocolVersion)
	return t.notify(ctx, "notifications/initialized")
}

// notify sends one JSON-RPC notification. A server answers 202 with no body; a
// body is tolerated and ignored, because it cannot be the answer to anything this
// client asked.
func (t *transportClient) notify(ctx context.Context, method string) error {
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	if err != nil {
		return ErrRequestInvalid
	}
	if len(payload) > MaxRequestBytes {
		return ErrRequestTooLarge
	}
	_, err = t.post(ctx, payload, 0, t.requestTimeout)
	return err
}

// call sends one request and returns its result.
func (t *transportClient) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id, err := t.identifier()
	if err != nil {
		return nil, err
	}
	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, ErrRequestInvalid
	}
	if len(payload) > MaxRequestBytes {
		return nil, ErrRequestTooLarge
	}
	body, err := t.post(ctx, payload, id, timeout)
	if err != nil {
		return nil, err
	}
	return decodeResult(body, id)
}

// post is the only place this package writes to a socket. Every request passes
// through the committed policy; nothing here builds a URL, follows a redirect or
// chooses a proxy.
func (t *transportClient) post(ctx context.Context, payload []byte, wantID uint64, timeout time.Duration) ([]byte, error) {
	if t == nil || t.policy == nil || t.endpoint == nil {
		return nil, ErrServerURLInvalid
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	bound, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request, err := http.NewRequestWithContext(bound, http.MethodPost, t.endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return nil, ErrRequestInvalid
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	protocol, sessionID := t.handshake()
	if protocol != "" {
		request.Header.Set("MCP-Protocol-Version", protocol)
	}
	if sessionID != "" {
		request.Header.Set("Mcp-Session-Id", sessionID)
	}
	response, err := t.policy.Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	// A session identifier is the server's to mint, but it becomes a request
	// header, so only a visible ASCII token is retained.
	if value := response.Header.Get("Mcp-Session-Id"); validHeaderToken(value) {
		t.recordSession(value)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, ErrServerRejected
	}
	if response.StatusCode == http.StatusAccepted {
		return nil, nil
	}
	body, err := readBounded(response.Body, MaxFrameBytes)
	if err != nil {
		return nil, err
	}
	media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if media == "text/event-stream" {
		if wantID == 0 {
			return nil, ErrProtocolInvalid
		}
		return readEventFrame(body, wantID)
	}
	return body, nil
}

// readBounded reads at most limit bytes and refuses a longer body instead of
// truncating one.
func readBounded(reader io.Reader, limit int) ([]byte, error) {
	if reader == nil || limit <= 0 {
		return nil, ErrProtocolInvalid
	}
	raw, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil {
		return nil, ErrConnectFailed
	}
	if len(raw) > limit {
		return nil, ErrResponseTooLarge
	}
	return raw, nil
}

// readEventFrame picks the one event-stream frame whose identifier is the request
// this client is waiting for. Anything else in the stream - notifications, a
// server-to-client request, an event of another kind - is skipped, never
// dispatched.
func readEventFrame(body []byte, wantID uint64) ([]byte, error) {
	pending := make([]byte, 0, 4096)
	total := 0
	for _, line := range bytes.Split(body, []byte{'\n'}) {
		line = bytes.TrimSuffix(line, []byte{'\r'})
		total += len(line) + 1
		if total > MaxFrameBytes {
			return nil, ErrResponseTooLarge
		}
		if len(line) == 0 {
			if frame := matchEventFrame(pending, wantID); frame != nil {
				return frame, nil
			}
			pending = pending[:0]
			continue
		}
		if line[0] == ':' {
			continue
		}
		key, value, found := bytes.Cut(line, []byte(":"))
		if !found || string(key) != "data" {
			continue
		}
		pending = append(pending, bytes.TrimPrefix(value, []byte(" "))...)
		pending = append(pending, '\n')
	}
	if frame := matchEventFrame(pending, wantID); frame != nil {
		return frame, nil
	}
	return nil, ErrProtocolInvalid
}

// matchEventFrame reports the frame when it is the answer to wantID.
func matchEventFrame(pending []byte, wantID uint64) []byte {
	frame := bytes.TrimSpace(pending)
	if len(frame) == 0 {
		return nil
	}
	var probe struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(frame, &probe) != nil || probe.ID == nil {
		return nil
	}
	var id uint64
	if json.Unmarshal(probe.ID, &id) != nil || id != wantID {
		return nil
	}
	return append([]byte(nil), frame...)
}

// decodeResult validates one JSON-RPC answer and returns its result. The error
// object's own message is deliberately dropped: an external server's text is not
// this lane's to publish, and it may quote the credential it was given.
func decodeResult(body []byte, wantID uint64) (json.RawMessage, error) {
	if len(body) == 0 {
		return nil, ErrProtocolInvalid
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
		Error   json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &message) != nil || message.JSONRPC != "2.0" {
		return nil, ErrProtocolInvalid
	}
	var id uint64
	if json.Unmarshal(message.ID, &id) != nil || id != wantID {
		return nil, ErrProtocolInvalid
	}
	if len(message.Error) > 0 && string(message.Error) != "null" {
		return nil, ErrRequestFailed
	}
	if len(message.Result) == 0 || string(message.Result) == "null" {
		return nil, ErrProtocolInvalid
	}
	return message.Result, nil
}

// listTools discovers the advertised tools, page by page, within the catalog
// bound.
func (t *transportClient) listTools(ctx context.Context) ([]Tool, error) {
	tools := make([]Tool, 0)
	cursor := ""
	seen := make(map[string]bool)
	catalogBytes := 0
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := t.call(ctx, "tools/list", params, t.requestTimeout)
		if err != nil {
			return nil, err
		}
		catalogBytes += len(result)
		if catalogBytes > MaxCatalogBytes {
			return nil, ErrCatalogTooLarge
		}
		var page struct {
			Tools      []toolDescriptor `json:"tools"`
			NextCursor string           `json:"nextCursor"`
		}
		if json.Unmarshal(result, &page) != nil {
			return nil, ErrProtocolInvalid
		}
		for _, descriptor := range page.Tools {
			if len(tools) >= MaxCatalogEntries {
				return nil, ErrCatalogTooLarge
			}
			tool, err := projectTool(t.serverID, descriptor)
			if err != nil {
				return nil, err
			}
			tools = append(tools, tool)
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		if seen[page.NextCursor] {
			return nil, ErrProtocolInvalid
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

// listResources discovers the advertised resources. A server that does not
// implement resources answers a JSON-RPC error, which the caller reads as "this
// server has none" rather than as a failed connection.
func (t *transportClient) listResources(ctx context.Context) ([]Resource, error) {
	resources := make([]Resource, 0)
	cursor := ""
	seen := make(map[string]bool)
	catalogBytes := 0
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := t.call(ctx, "resources/list", params, t.requestTimeout)
		if err != nil {
			return nil, err
		}
		catalogBytes += len(result)
		if catalogBytes > MaxCatalogBytes {
			return nil, ErrCatalogTooLarge
		}
		var page struct {
			Resources  []resourceDescriptor `json:"resources"`
			NextCursor string               `json:"nextCursor"`
		}
		if json.Unmarshal(result, &page) != nil {
			return nil, ErrProtocolInvalid
		}
		for _, descriptor := range page.Resources {
			if len(resources) >= MaxCatalogEntries {
				return nil, ErrCatalogTooLarge
			}
			resource, err := projectResource(t.serverID, descriptor)
			if err != nil {
				return nil, err
			}
			resources = append(resources, resource)
		}
		if page.NextCursor == "" {
			return resources, nil
		}
		if seen[page.NextCursor] {
			return nil, ErrProtocolInvalid
		}
		seen[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

// callTool invokes one advertised tool. It is bounded three ways: the caller's
// context and Options.CallTimeout, MaxRequestBytes on the message that leaves, and
// MaxToolResultBytes on the text that comes back. The tool must be one this
// server advertised, which the manager checked before it got here.
func (t *transportClient) callTool(ctx context.Context, name string, arguments map[string]any) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", ErrRequestInvalid
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	result, err := t.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, t.callTimeout)
	if err != nil {
		return "", err
	}
	var answer struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if json.Unmarshal(result, &answer) != nil || answer.Content == nil {
		return "", ErrProtocolInvalid
	}
	if answer.IsError {
		// The tool ran and reported its own failure. Its text is not relayed: it
		// may quote the credential the server was given.
		return "", ErrRequestFailed
	}
	text := make([]string, 0, len(answer.Content))
	size := 0
	for _, part := range answer.Content {
		if part.Type != "text" {
			continue
		}
		size += len(part.Text)
		if size > MaxToolResultBytes {
			return "", ErrToolResultTooLarge
		}
		text = append(text, part.Text)
	}
	return strings.Join(text, "\n"), nil
}

// projectTool validates one advertised tool and projects it.
func projectTool(serverID string, descriptor toolDescriptor) (Tool, error) {
	if strings.TrimSpace(descriptor.Name) == "" || len(descriptor.Name) > 256 {
		return Tool{}, ErrProtocolInvalid
	}
	tool := Tool{
		Name:        descriptor.Name,
		Description: descriptor.Description,
		InputSchema: map[string]any{},
		ServerID:    serverID,
	}
	if len(descriptor.InputSchema) > 0 && string(descriptor.InputSchema) != "null" {
		var schema map[string]any
		if json.Unmarshal(descriptor.InputSchema, &schema) != nil || schema == nil {
			return Tool{}, ErrProtocolInvalid
		}
		tool.InputSchema = schema
	}
	return tool, nil
}

// projectResource validates one advertised resource and projects it.
func projectResource(serverID string, descriptor resourceDescriptor) (Resource, error) {
	if descriptor.URI == "" || strings.TrimSpace(descriptor.Name) == "" {
		return Resource{}, ErrProtocolInvalid
	}
	return Resource{
		URI:         descriptor.URI,
		Name:        descriptor.Name,
		Description: descriptor.Description,
		MIMEType:    descriptor.MIMEType,
		ServerID:    serverID,
	}, nil
}

// identifier mints one connection-local monotonic request identifier.
func (t *transportClient) identifier() (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.nextID == 0 || t.nextID > maxID {
		return 0, ErrRequestInvalid
	}
	id := t.nextID
	t.nextID++
	return id, nil
}

// handshake snapshots the revision and session to declare on the next request.
func (t *transportClient) handshake() (string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.protocol, t.sessionID
}

func (t *transportClient) recordProtocol(value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if validHeaderToken(value) {
		t.protocol = value
	}
}

func (t *transportClient) recordSession(value string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if validHeaderToken(value) {
		t.sessionID = value
	}
}

// validHeaderToken accepts only the visible ASCII a header value may safely carry
// once a server has chosen it.
func validHeaderToken(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}

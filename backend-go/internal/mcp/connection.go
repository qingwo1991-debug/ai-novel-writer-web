package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"unicode/utf8"
)

type response struct {
	result json.RawMessage
	err    error
}
type queuedMessage struct {
	raw []byte
	id  uint64
	ctx context.Context
}
type connection struct {
	mu             sync.Mutex
	config         serverConfig
	options        Options
	ctx            context.Context
	cancel         context.CancelFunc
	status         string
	failure        string
	tools          []map[string]any
	resources      []map[string]any
	nextID         uint64
	pending        map[uint64]chan response
	queue          chan queuedMessage
	queuedBytes    int
	closeTransport func()
}

func newConnection(config serverConfig, options Options) *connection {
	ctx, cancel := context.WithCancel(context.Background())
	return &connection{config: config, options: options, ctx: ctx, cancel: cancel, status: "connecting", nextID: 1, pending: make(map[uint64]chan response), queue: make(chan queuedMessage, MaxPending+8)}
}
func (c *connection) stop(status, code string) {
	c.mu.Lock()
	if c.status == "error" || c.status == "disconnected" {
		c.mu.Unlock()
		return
	}
	c.status = status
	if status == "error" {
		c.failure = code
	}
	c.tools = nil
	c.resources = nil
	for id, ch := range c.pending {
		ch <- response{err: errors.New(code)}
		delete(c.pending, id)
	}
	c.cancel()
	// Release queued request bodies (up to the byte budget) on every terminal
	// transition, rather than retaining them with an error-status generation.
	for {
		select {
		case message := <-c.queue:
			c.queuedBytes -= len(message.raw)
		default:
			goto drained
		}
	}
drained:
	closeTransport := c.closeTransport
	c.closeTransport = nil
	c.mu.Unlock()
	if closeTransport != nil {
		closeTransport()
	}
}
func (c *connection) alive() bool { return c.status == "connecting" || c.status == "connected" }
func (c *connection) connect(ctx context.Context) error {
	var err error
	if c.config.URL != "" {
		err = c.startSSE(ctx)
	} else {
		err = c.startStdio()
	}
	if err != nil {
		return err
	}
	_, err = c.request(ctx, "initialize", map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "vela", "version": "1.0.0"}})
	if err != nil {
		return err
	}
	if err = c.notify("notifications/initialized", nil); err != nil {
		return err
	}
	tools, err := c.discover(ctx, false)
	if err != nil {
		return err
	}
	resources, err := c.discover(ctx, true)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.alive() {
		return errors.New(errorCode(errors.New(c.failure), "MCP_SERVER_NOT_CONNECTED"))
	}
	c.tools = tools
	c.resources = resources
	c.status = "connected"
	return nil
}
func (c *connection) enqueueLocked(message queuedMessage) error {
	raw := message.raw
	if !c.alive() {
		return errors.New("MCP_SERVER_NOT_CONNECTED")
	}
	if len(raw) > MaxRequestBytes {
		return errors.New("MCP_REQUEST_TOO_LARGE")
	}
	if c.queuedBytes+len(raw) > MaxQueuedBytes {
		return errors.New("MCP_INPUT_LIMIT_EXCEEDED")
	}
	select {
	case c.queue <- message:
		c.queuedBytes += len(raw)
		return nil
	default:
		return errors.New("MCP_INPUT_LIMIT_EXCEEDED")
	}
}
func (c *connection) notify(method string, params any) error {
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	raw, err := json.Marshal(msg)
	if err != nil {
		return errors.New("MCP_REQUEST_INVALID")
	}
	raw = append(raw, '\n')
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.enqueueLocked(queuedMessage{raw: raw, ctx: c.ctx})
}
func (c *connection) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, errors.New("MCP_REQUEST_CANCELED")
	default:
	}
	requestCtx, cancel := context.WithTimeout(ctx, c.options.RequestTimeout)
	defer cancel()
	c.mu.Lock()
	if !c.alive() {
		c.mu.Unlock()
		return nil, errors.New("MCP_SERVER_NOT_CONNECTED")
	}
	if len(c.pending) >= MaxPending {
		c.mu.Unlock()
		return nil, errors.New("MCP_PENDING_LIMIT_EXCEEDED")
	}
	// IDs are connection-local monotonic integers, not timestamps or caller IDs.
	// Avoid wraparound and JavaScript's integer precision boundary.
	if c.nextID > 9007199254740991 {
		c.mu.Unlock()
		return nil, errors.New("MCP_REQUEST_INVALID")
	}
	id := c.nextID
	c.nextID++
	if params == nil {
		params = map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		c.mu.Unlock()
		return nil, errors.New("MCP_REQUEST_INVALID")
	}
	ch := make(chan response, 1)
	c.pending[id] = ch
	err = c.enqueueLocked(queuedMessage{raw: append(raw, '\n'), id: id, ctx: requestCtx})
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	select {
	case reply := <-ch:
		return reply.result, reply.err
	case <-ctx.Done():
		return nil, errors.New("MCP_REQUEST_CANCELED")
	case <-requestCtx.Done():
		if ctx.Err() != nil {
			return nil, errors.New("MCP_REQUEST_CANCELED")
		}
		return nil, errors.New("MCP_REQUEST_TIMEOUT")
	case <-c.ctx.Done():
		// stop sends the specific safe error before canceling the connection.
		select {
		case reply := <-ch:
			return reply.result, reply.err
		default:
			return nil, errors.New("MCP_SERVER_NOT_CONNECTED")
		}
	}
}

// writer drops canceled/timed-out requests while still queued. Cancellation
// after the dispatch linearization point cannot undo an external side effect.
func (c *connection) writer(write func(context.Context, []byte) error) {
	for {
		select {
		case <-c.ctx.Done():
			return
		case message := <-c.queue:
			c.mu.Lock()
			valid := c.alive() && c.ctx.Err() == nil && message.ctx.Err() == nil
			if message.id != 0 && c.pending[message.id] == nil {
				valid = false
			}
			if !valid {
				c.queuedBytes -= len(message.raw)
				c.mu.Unlock()
				continue
			}
			// Selecting an active pending request here is the dispatch linearization
			// point relative to stop/cancel; no blocking I/O holds the lifecycle lock.
			c.mu.Unlock()
			err := write(message.ctx, message.raw)
			c.mu.Lock()
			c.queuedBytes -= len(message.raw)
			c.mu.Unlock()
			if err != nil {
				if message.ctx.Err() != nil {
					continue
				}
				c.stop("error", errorCode(err, "MCP_STDIO_WRITE_FAILED"))
				return
			}
		}
	}
}
func (c *connection) receive(raw []byte) error {
	if len(raw) > MaxFrameBytes {
		return errors.New("MCP_STDOUT_LIMIT_EXCEEDED")
	}
	if !utf8.Valid(raw) {
		return errors.New("MCP_STDOUT_INVALID")
	}
	var msg map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil || msg == nil || string(msg["jsonrpc"]) != `"2.0"` {
		return errors.New("MCP_STDOUT_INVALID")
	}
	// Server-initiated requests are never executed. In particular there is no
	// sampling, browser-use, arbitrary command or client-side RPC dispatch here.
	if methodRaw, ok := msg["method"]; ok {
		var method string
		if json.Unmarshal(methodRaw, &method) != nil || method == "" {
			return errors.New("MCP_STDOUT_INVALID")
		}
		if id, hasID := msg["id"]; hasID && string(id) != "null" {
			raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "Method not found"}})
			c.mu.Lock()
			err := c.enqueueLocked(queuedMessage{raw: append(raw, '\n'), ctx: c.ctx})
			c.mu.Unlock()
			return err
		}
		return nil
	}
	var id uint64
	if json.Unmarshal(msg["id"], &id) != nil {
		return nil
	} // unknown/string IDs cannot settle numeric requests
	result, hasResult := msg["result"]
	_, hasError := msg["error"]
	if hasResult == hasError {
		return errors.New("MCP_STDOUT_INVALID")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch := c.pending[id]; ch != nil {
		delete(c.pending, id)
		reply := response{result: result}
		if hasError {
			reply.err = errors.New("MCP_REQUEST_FAILED")
		}
		ch <- reply
	}
	return nil
}
func (c *connection) readStdio(reader io.Reader) {
	scan := bufio.NewScanner(reader)
	scan.Buffer(make([]byte, 4096), MaxFrameBytes+1)
	truncated := false
	scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) > 0 && !bytes.Contains(data, []byte{'\n'}) {
			truncated = true
			return 0, nil, errors.New("MCP_STDOUT_INVALID")
		}
		return bufio.ScanLines(data, atEOF)
	})
	for scan.Scan() {
		raw := bytes.TrimSpace(scan.Bytes())
		if len(raw) == 0 {
			continue
		}
		if err := c.receive(raw); err != nil {
			c.stop("error", errorCode(err, "MCP_STDOUT_INVALID"))
			return
		}
	}
	if truncated {
		c.stop("error", "MCP_STDOUT_INVALID")
	} else if scan.Err() != nil {
		c.stop("error", "MCP_STDOUT_LIMIT_EXCEEDED")
	} else {
		c.stop("disconnected", "MCP_SERVER_NOT_CONNECTED")
	}
}
func (c *connection) discover(ctx context.Context, resources bool) ([]map[string]any, error) {
	field, method := "tools", "tools/list"
	if resources {
		field, method = "resources", "resources/list"
	}
	out := make([]map[string]any, 0)
	cursor := ""
	seen := make(map[string]bool)
	catalogBytes := 0
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.request(ctx, method, params)
		if err != nil {
			if err.Error() == "MCP_REQUEST_FAILED" {
				return out, nil
			}
			return nil, err
		}
		catalogBytes += len(raw)
		if catalogBytes > MaxFrameBytes {
			return nil, c.invalidCatalog()
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			return nil, c.invalidCatalog()
		}
		if entriesRaw, ok := object[field]; ok {
			var entries []map[string]any
			if json.Unmarshal(entriesRaw, &entries) != nil || entries == nil || len(out)+len(entries) > MaxCatalogEntries {
				return nil, c.invalidCatalog()
			}
			for _, entry := range entries {
				projected, err := projectEntry(entry, c.config.ID, resources)
				if err != nil {
					return nil, c.invalidCatalog()
				}
				out = append(out, projected)
			}
		}
		nextRaw, ok := object["nextCursor"]
		if !ok || string(nextRaw) == "null" {
			return out, nil
		}
		if json.Unmarshal(nextRaw, &cursor) != nil {
			return nil, c.invalidCatalog()
		}
		if cursor == "" {
			return out, nil
		}
		if seen[cursor] || len(seen) >= MaxCatalogEntries {
			return nil, c.invalidCatalog()
		}
		seen[cursor] = true
	}
}
func (c *connection) invalidCatalog() error {
	c.stop("error", "MCP_STDOUT_INVALID")
	return errors.New("MCP_STDOUT_INVALID")
}
func projectEntry(entry map[string]any, id string, resources bool) (map[string]any, error) {
	invalid := errors.New("MCP_STDOUT_INVALID")
	name, ok := entry["name"].(string)
	if !ok || len(bytes.TrimSpace([]byte(name))) == 0 {
		return nil, invalid
	}
	out := map[string]any{"name": name, "serverId": id}
	optional := []string{"description"}
	if resources {
		uri, ok := entry["uri"].(string)
		if !ok || uri == "" {
			return nil, invalid
		}
		out["uri"] = uri
		optional = append(optional, "mimeType")
	} else {
		if schema, ok := entry["inputSchema"]; ok {
			object, ok := schema.(map[string]any)
			if !ok {
				return nil, invalid
			}
			out["inputSchema"] = cloneObject(object)
		} else {
			out["inputSchema"] = map[string]any{}
		}
		out["description"] = ""
	}
	for _, key := range optional {
		if value, exists := entry[key]; exists {
			str, ok := value.(string)
			if !ok {
				return nil, invalid
			}
			out[key] = str
		}
	}
	return out, nil
}

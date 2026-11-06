package mcphost

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Connection states. They are exactly MCPConnectionStatus
// (src/shared/ipc-channels.ts:1294), so a status answer needs no translation on
// the page and no page has to learn a second vocabulary.
const (
	// StatusDisconnected is a configured server with no live conversation.
	StatusDisconnected = "disconnected"
	// StatusConnecting is a conversation this lane is opening right now.
	StatusConnecting = "connecting"
	// StatusConnected is a conversation whose catalogs have been read.
	StatusConnected = "connected"
	// StatusError is a server that is configured but cannot be used; Code says
	// whether the configuration said so (a command entry, a non-admitted origin)
	// or the connection failed.
	StatusError = "error"
)

// Options is the trusted host configuration of one manager. Every field is
// captured by the lane at startup; no field is ever filled from a request.
type Options struct {
	// ConfigPath resolves the author's own MCP configuration file. It is called
	// at load time and is the only way a path can enter this package, so a request
	// can never point a load at another file. The production lane answers it from
	// the pinned, re-verified author data directory.
	ConfigPath func(context.Context) (string, error)
	// Policy is the committed-origin boundary. A nil policy loads and reports
	// configurations but connects nothing.
	Policy *Policy
	// RegisterSecret is the host's public redactor - the server's own sanitizer.
	// It is required: without it this lane could not promise that a configuration
	// credential stays out of an answer, so the manager refuses to exist.
	RegisterSecret func(string) (bool, error)
	// RequestTimeout bounds one handshake or one catalog page.
	RequestTimeout time.Duration
	// CallTimeout bounds one tools/call end to end.
	CallTimeout time.Duration
}

// Result is the answer to one configuration load, in the renderer's three states
// (src/shared/ipc-channels.ts:1325-1333): "loaded", "missing" or "error".
type Result struct {
	// Status is one of those three states.
	Status string
	// Servers is every configured entry, sorted by id, empty for "missing" and
	// "error".
	Servers []Summary
	// Code is the symbolic refusal of an "error" load, empty otherwise.
	Code string
}

// Status is one MCPServerStatus (src/shared/ipc-channels.ts:1302-1308) plus the
// symbolic code the page needs to explain it.
type Status struct {
	// ID is the configured server id.
	ID string
	// Name is the configured display name.
	Name string
	// Status is one of the four connection states.
	Status string
	// ToolCount is how many tools the server advertised, 0 when not connected.
	ToolCount int
	// Code is the symbolic reason a configured server cannot be used, empty when
	// there is none.
	Code string
}

// Manager owns the loaded configuration and the live conversations. Its lock only
// guards that state: it is never held across a network call, and a session never
// reaches back into it, so disconnect cannot deadlock against an in-flight call.
type Manager struct {
	mu       sync.Mutex
	options  Options
	entries  map[string]Entry
	sessions map[string]*session
}

// New builds a manager. Both the path resolver and the redactor are required; the
// policy is not, because a lane whose committed configuration admits no origin
// must still be able to show its author that the configuration exists.
func New(options Options) (*Manager, error) {
	if options.ConfigPath == nil || options.RegisterSecret == nil {
		return nil, ErrConfigUnreadable
	}
	return &Manager{
		options:  options,
		entries:  make(map[string]Entry),
		sessions: make(map[string]*session),
	}, nil
}

// Load re-reads the author's configuration and reconciles the live conversations
// with it: an entry that was removed or changed is disconnected, an entry that is
// unchanged keeps its conversation. A configuration that cannot be read revokes
// every conversation, because a server this lane can no longer describe is not one
// it may keep talking to.
func (m *Manager) Load(ctx context.Context) Result {
	if m == nil {
		return Result{Status: "error", Servers: []Summary{}, Code: Code(ErrConfigUnreadable)}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	path, err := m.options.ConfigPath(ctx)
	if err != nil {
		return m.failLocked(ErrConfigUnreadable)
	}
	entries, err := loadEntries(path, m.options.RegisterSecret)
	if err != nil {
		if errors.Is(err, ErrConfigMissing) {
			m.clearLocked()
			return Result{Status: "missing", Servers: []Summary{}}
		}
		return m.failLocked(err)
	}
	for id, live := range m.sessions {
		next, present := entries[id]
		if !present || next.URL != live.entry.URL || next.Transport != live.entry.Transport || next.Refusal != live.entry.Refusal {
			live.close()
			delete(m.sessions, id)
		}
	}
	m.entries = entries
	return Result{Status: "loaded", Servers: summariesOf(entries)}
}

// failLocked revokes everything and answers the symbolic refusal. The caller holds
// m.mu.
func (m *Manager) failLocked(err error) Result {
	m.clearLocked()
	return Result{Status: "error", Servers: []Summary{}, Code: Code(err)}
}

// clearLocked drops every conversation and every entry. The caller holds m.mu.
func (m *Manager) clearLocked() {
	for id, live := range m.sessions {
		live.close()
		delete(m.sessions, id)
	}
	m.entries = make(map[string]Entry)
}

// Statuses answers every configured server. This is the half a page needs: a
// server that is configured but unreachable is listed with the state and the code
// that explain it, which is what makes "nothing is configured" (an empty list)
// distinguishable from "configured and not reachable" (an entry in StatusError or
// StatusDisconnected).
//
// The admission question is answered without dialing: a URL the committed policy
// would refuse is already reported as StatusError/MCP_ORIGIN_NOT_ADMITTED, so a
// page does not have to attempt a connection to find out.
func (m *Manager) Statuses() []Status {
	if m == nil {
		return []Status{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ids := make([]string, 0, len(m.entries))
	for id := range m.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Status, 0, len(ids))
	for _, id := range ids {
		entry := m.entries[id]
		status := Status{ID: entry.ID, Name: entry.Name}
		switch {
		case m.sessions[id] != nil:
			snapshot := m.sessions[id].snapshot()
			status.Status = snapshot.Status
			status.ToolCount = snapshot.ToolCount
			status.Code = snapshot.Code
		case entry.Refused() != nil:
			status.Status = StatusError
			status.Code = Code(entry.Refusal)
		default:
			if code := m.admissionCodeLocked(entry); code != "" {
				status.Status = StatusError
				status.Code = code
			} else {
				status.Status = StatusDisconnected
			}
		}
		out = append(out, status)
	}
	return out
}

// admissionCodeLocked answers whether the committed policy would admit this entry,
// as a code and without dialing. The caller holds m.mu.
func (m *Manager) admissionCodeLocked(entry Entry) string {
	if entry.URL == "" {
		return ""
	}
	if m.options.Policy == nil {
		return Code(ErrPolicyUnavailable)
	}
	if _, err := m.options.Policy.Admit(entry.URL); err != nil {
		return Code(err)
	}
	return ""
}

// Tools answers every tool the connected servers advertised, grouped by server id.
func (m *Manager) Tools() []Tool {
	out := make([]Tool, 0)
	for _, live := range m.orderedSessions() {
		out = append(out, live.toolsSnapshot()...)
	}
	return out
}

// Resources answers every resource the connected servers advertised.
func (m *Manager) Resources() []Resource {
	out := make([]Resource, 0)
	for _, live := range m.orderedSessions() {
		out = append(out, live.resourcesSnapshot()...)
	}
	return out
}

// orderedSessions snapshots the live conversations in a stable order - server id,
// then the order each server itself advertised - without holding the manager lock
// while a catalog is copied.
func (m *Manager) orderedSessions() []*session {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.sessions))
	for id := range m.sessions {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]*session, 0, len(ids))
	for _, id := range ids {
		if live := m.sessions[id]; live != nil {
			out = append(out, live)
		}
	}
	m.mu.Unlock()
	return out
}

// Connect opens one conversation with a server the author's configuration names.
// A server id that is not in that configuration is refused rather than invented,
// a command entry is refused by name, and an endpoint the committed policy does
// not admit is refused before any socket exists.
func (m *Manager) Connect(ctx context.Context, serverID string) error {
	if m == nil {
		return ErrServerNotConnected
	}
	if ctx == nil {
		ctx = context.Background()
	}
	m.mu.Lock()
	entry, configured := m.entries[serverID]
	policy := m.options.Policy
	requestTimeout, callTimeout := m.options.RequestTimeout, m.options.CallTimeout
	m.mu.Unlock()
	if !configured {
		return ErrServerNotConfigured
	}
	if entry.Refused() != nil {
		return entry.Refusal
	}
	if err := ctx.Err(); err != nil {
		return ErrRequestCanceled
	}
	endpoint, err := policy.Admit(entry.URL)
	if err != nil {
		return err
	}
	created := &session{
		entry:  entry,
		client: newTransportClient(policy, endpoint, entry.ID, requestTimeout, callTimeout),
		status: StatusConnecting,
	}
	m.mu.Lock()
	if old := m.sessions[serverID]; old != nil {
		old.close()
	}
	m.sessions[serverID] = created
	m.mu.Unlock()
	if err := created.connect(ctx); err != nil {
		// The failed conversation stays in the map: it is how its status answer
		// carries the symbolic reason without this lane inventing a second state.
		return err
	}
	// A concurrent disconnect or reload may have replaced this generation.
	m.mu.Lock()
	current := m.sessions[serverID] == created
	m.mu.Unlock()
	if !current {
		created.close()
		return ErrServerNotConnected
	}
	return nil
}

// Disconnect drops one conversation. A server that was never connected is not an
// error: the state the caller asked for is the state it gets.
func (m *Manager) Disconnect(serverID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if live := m.sessions[serverID]; live != nil {
		live.close()
		delete(m.sessions, serverID)
	}
}

// DisconnectAll drops every conversation.
func (m *Manager) DisconnectAll() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, live := range m.sessions {
		live.close()
		delete(m.sessions, id)
	}
}

// CallTool invokes one tool on one connected server. The tool must be one that
// server advertised during this conversation: an invented name is refused before
// anything leaves the process, which is what keeps a tool call from becoming a
// guess at another server's surface.
func (m *Manager) CallTool(ctx context.Context, serverID, tool string, arguments map[string]any) (string, error) {
	if m == nil {
		return "", ErrServerNotConnected
	}
	m.mu.Lock()
	_, configured := m.entries[serverID]
	live := m.sessions[serverID]
	m.mu.Unlock()
	if !configured {
		return "", ErrServerNotConfigured
	}
	if live == nil {
		return "", ErrServerNotConnected
	}
	return live.callTool(ctx, tool, arguments)
}

// summariesOf answers the configuration as the renderer's summary list, sorted by
// id so neither the answer nor a count depends on map order.
func summariesOf(entries map[string]Entry) []Summary {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]Summary, 0, len(ids))
	for _, id := range ids {
		out = append(out, entries[id].Summary())
	}
	return out
}

// session is one conversation. It keeps its own state lock and never takes the
// manager's, so a disconnect can close it while a call is reading.
type session struct {
	entry  Entry
	client *transportClient
	mu     sync.Mutex
	status string
	code   string
	tools  []Tool
	res    []Resource
}

// sessionSnapshot is the state one status answer reports.
type sessionSnapshot struct {
	Status    string
	ToolCount int
	Code      string
}

func (s *session) snapshot() sessionSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return sessionSnapshot{Status: s.status, ToolCount: len(s.tools), Code: s.code}
}

func (s *session) toolsSnapshot() []Tool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusConnected {
		return nil
	}
	return append([]Tool(nil), s.tools...)
}

func (s *session) resourcesSnapshot() []Resource {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status != StatusConnected {
		return nil
	}
	return append([]Resource(nil), s.res...)
}

// connect runs the handshake and reads both catalogs. A server that has no
// resources answers a JSON-RPC error to resources/list, which is "no resources",
// not a failed connection.
func (s *session) connect(ctx context.Context) error {
	if err := s.client.initialize(ctx); err != nil {
		s.fail(err)
		return err
	}
	tools, err := s.client.listTools(ctx)
	if err != nil {
		s.fail(err)
		return err
	}
	resources, err := s.client.listResources(ctx)
	if err != nil {
		if !errors.Is(err, ErrRequestFailed) {
			s.fail(err)
			return err
		}
		resources = []Resource{}
	}
	s.mu.Lock()
	s.status = StatusConnected
	s.code = ""
	s.tools = tools
	s.res = resources
	s.mu.Unlock()
	return nil
}

// fail records the symbolic reason and drops both catalogs.
func (s *session) fail(err error) {
	s.mu.Lock()
	s.status = StatusError
	s.code = Code(err)
	s.tools = nil
	s.res = nil
	s.mu.Unlock()
}

// close drops this conversation's state. The transport holds no background
// goroutine and no idle socket, so there is nothing else to release.
func (s *session) close() {
	s.mu.Lock()
	s.status = StatusDisconnected
	s.code = ""
	s.tools = nil
	s.res = nil
	s.mu.Unlock()
}

// callTool enforces the two properties a tool call needs: a live conversation, and
// a tool that conversation advertised.
func (s *session) callTool(ctx context.Context, tool string, arguments map[string]any) (string, error) {
	s.mu.Lock()
	connected := s.status == StatusConnected
	advertised := false
	if connected {
		for _, candidate := range s.tools {
			if candidate.Name == tool {
				advertised = true
				break
			}
		}
	}
	s.mu.Unlock()
	if !connected {
		return "", ErrServerNotConnected
	}
	if !advertised {
		return "", ErrToolNotDiscovered
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", ErrRequestCanceled
	}
	return s.client.callTool(ctx, tool, arguments)
}

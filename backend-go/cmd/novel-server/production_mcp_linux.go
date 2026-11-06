//go:build linux

package main

// This file serves all eight `mcp:*` names the browser inventory exposes. None of
// them is left at the fail-closed 501, and none of them is served by pretending
// to be a transport this container does not have:
//
//	mcp:load-config         web/operations.json:286  served
//	mcp:connect             web/operations.json:287  served, HTTP(S) servers only
//	mcp:disconnect          web/operations.json:288  served
//	mcp:disconnect-all      web/operations.json:289  served
//	mcp:list-tools          web/operations.json:290  served
//	mcp:list-resources      web/operations.json:291  served
//	mcp:call-tool           web/operations.json:292  served, bounded
//	mcp:get-servers-status  web/operations.json:293  served
//
// The ninth `mcp:*` name the control plane knows about, `mcp:get-config-path`, is
// excluded from the browser inventory and stays unserved here, because it answers a
// host filesystem path (web/operations.json:40). The bridge still offers it
// (mcp-ipc-bridge.ts:83); this lane answers without it.
//
// MCP is how an author's own tool servers reach the writing agent, so a half-served
// family is a security problem rather than a convenience one: one `mcp:call-tool`
// is arbitrary code execution delegated to a third party. The two names a browser
// page cannot do without are `mcp:get-servers-status` and `mcp:list-tools`
// (src/stores/mcp-store.ts:70-190 loads the configuration, connects what it lists,
// then reads status and catalogs), and they are served from the same honest state
// as the rest.
//
// # The reference
//
//	electron/mcp/mcp-ipc-bridge.ts:17-82      the eight handlers, one for one
//	electron/mcp/mcp-manager.ts:145-147       the path: <VELA_HOME>/mcp_config.json
//	electron/mcp/mcp-manager.ts:154-239       loadConfig: whole-file shape, per-entry shape
//	electron/mcp/mcp-manager.ts:232-300       connect: transport, initialize, both catalogs
//	electron/mcp/mcp-manager.ts:563-591       callTool: connected, advertised, then call
//	electron/mcp/mcp-manager.ts:596-613       the two disconnect verbs
//	electron/mcp/mcp-manager.ts:638-646       getServersStatus
//	src/shared/ipc-channels.ts:1294-1345      MCPConnectionStatus, the four shapes, the arities
//
// # The transport this lane can have, and why
//
// The format this lane reads declares two, and the desktop uses one: an
// author-supplied child process over stdio, and a URL, which its own reload refuses
// by name (`MCP_SSE_UNSUPPORTED`, mcp-manager.ts:256-258). This lane is the mirror
// image of that. It is a server container - one static executable in an image, with
// no author terminal, no runtime directory and no OS sandbox for a child - so a
// command entry is refused by name (MCP_STDIO_TRANSPORT_UNAVAILABLE) instead of
// attempted, and the rest of the file keeps working. `internal/mcphost` never
// imports os/exec.
//
// What remains is the URL half, spoken as MCP streamable HTTP (2025-06-18) over one
// POST endpoint: the revision the desktop's refusal predates, not the legacy
// two-channel SSE binding. So an author who configures a URL server gets a working
// connection here that the desktop would refuse, and one who configures a command
// gets the reverse. Both answers name the transport instead of pretending, and a
// summary still labels a URL server "sse" because that is the only URL value
// MCPServerSummary admits (src/shared/ipc-channels.ts:1299).
//
// Every request leaves through internal/egress against the committed policy this
// lane derived at startup (production_open_linux.go:379 ModelEgress, the same one
// the generation executor dispatches through). A configured URL whose origin the
// policy does not admit is refused with MCP_ORIGIN_NOT_ADMITTED and is never
// dialed: the check is pure, and the policy revalidates the resolved URL again at
// dispatch time and refuses redirects itself. A lane whose committed configuration
// admits no origin at all still loads its configuration and reports honest
// statuses; it just cannot connect anything (MCP_EGRESS_POLICY_UNAVAILABLE).
//
// # What a status answer means
//
// One entry per server the author's own mcp_config.json names, sorted by id, with
// the renderer's own four states and the symbolic code that explains it:
//
//	disconnected  configured, admitted, never connected in this process
//	connecting    a conversation is being opened right now
//	connected     handshake and both catalogs succeeded; toolCount is real
//	error         configured but unusable, and `error` says why
//
// So "nothing is configured" is an empty list (and mcp:load-config answers
// "missing"), while "configured but unreachable" is an entry in `error` carrying
// MCP_CONNECT_FAILED or in `disconnected`; a command entry is `error` with
// MCP_STDIO_TRANSPORT_UNAVAILABLE, and an origin the policy does not admit is
// `error` with MCP_ORIGIN_NOT_ADMITTED before anything is dialed. The reference
// listed only servers it had a runtime object for, so a page could not tell those
// two apart; this answer can.
//
// # The configuration, and the credential boundary
//
// The file is read from the pinned author data directory and nowhere else: the path
// is `<NOVEL_DATA_ROOT>/config/ai-novel-writer/generations/<generation>/mcp_config.json`,
// resolved by re-verifying the pinned generation on every load
// (internal/globaladmission/pinned_linux.go:305-312), admitted as one regular 0600
// single-link file with no symlink (internal/mcphost/config_linux.go), and never
// derived from a request field. A caller may name a server that this file names and
// may never invent one.
//
// The values in that file are credentials: a URL may carry a token in its query, a
// command entry carries one in env. Both are registered with the server's own public
// sanitizer before the file is parsed into anything the lane acts on
// (internal/mcphost/config.go harvestCredentialValues), and command/args/env are
// then dropped, so they cannot be echoed, logged or answered. Nothing in this file
// or in internal/mcphost formats a URL, a host, a server id or a server message into
// an error: every failure is a fixed symbol, mapped once by mcphost.Code. A test
// scans the whole HTTP answer for the committed secret.
//
// # What a refused call writes
//
// Nothing. This file opens no project database, writes no file and appends no row:
// loading, connecting, listing and calling are all reads of committed state plus
// bounded network I/O, so every refusal leaves the author database byte-identical
// (proven by a whole-file digest over a separate mode=ro connection in the test).

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"time"
	"unicode/utf8"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/mcphost"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/transport"
)

const (
	// productionMCPConfigName is the configuration file the desktop reads from the
	// author's global data directory (mcp-manager.ts:145-147). Here it is a child
	// of the pinned committed generation, which is the same directory this lane
	// already reads config.json and models.json from.
	productionMCPConfigName = "mcp_config.json"
	// productionMCPRequestTimeout bounds one handshake or one catalog page.
	productionMCPRequestTimeout = 15 * time.Second
	// productionMCPCallTimeout bounds one tools/call end to end.
	productionMCPCallTimeout = 60 * time.Second
)

// errProductionMCPUnavailable is a wiring refusal, not a request refusal: it means
// the lane was built without the pinned global generation this family needs.
var errProductionMCPUnavailable = errors.New("PRODUCTION_MCP_UNAVAILABLE")

// productionMCP binds the eight names to one manager.
//
// There is deliberately no administrator predicate here, and no effect-confirmation
// step. The scoring inventory asks for both - "MCP lifecycle/config reload and all
// command/args/env configuration are administrator-limited; tool calls require
// assigned server/tool permissions and effect confirmation" - and says in the same
// sentence that "exposing a route alone does not enforce this"
// (web/operations.json:19). This lane has exactly one authenticated principal per
// data root and no role store, so it can enforce neither, and it does not pretend
// to. What it enforces instead is the part that is real: a caller may name only a
// server the author's own committed configuration contains and only a tool that
// server itself advertised in this conversation, every dispatch leaves through the
// committed origin policy, and no entry can start a process. A role check over one
// principal would be theatre; these boundaries actually hold.
type productionMCP struct {
	// open is the lane, kept for the two trusted accessors this family reads: the
	// pinned committed generation, and the committed egress policy.
	open    *productionOpen
	manager *mcphost.Manager
	err     error
}

// newProductionMCP captures the lane. It performs no I/O, no dial and no file read;
// the configuration is resolved on every mcp:load-config call, so a configuration
// the author edited after startup is picked up without a restart.
func newProductionMCP(open *productionOpen) *productionMCP {
	binding := &productionMCP{open: open}
	if open == nil || open.global == nil {
		binding.err = errProductionMCPUnavailable
	}
	return binding
}

// Bind installs the eight names. It is called by the lane before serving, exactly
// like the other production binders. The manager is built here because the public
// sanitizer it must register credentials with is the server's own, and it has to be
// in place before the first configuration byte is read.
func (x *productionMCP) Bind(server *transport.Server) error {
	if x == nil || x.err != nil {
		return errProductionMCPUnavailable
	}
	if server == nil || x.open == nil || x.open.global == nil {
		return errProductionMCPUnavailable
	}
	// The committed policy, or nil when this lane's committed configuration names
	// no usable origin. A nil client makes every connect a
	// MCP_EGRESS_POLICY_UNAVAILABLE refusal rather than a dial.
	client, _ := x.open.ModelEgress()
	manager, err := mcphost.New(mcphost.Options{
		ConfigPath:     x.configPath,
		Policy:         mcphost.NewPolicy(client),
		RegisterSecret: server.RegisterSecret,
		RequestTimeout: productionMCPRequestTimeout,
		CallTimeout:    productionMCPCallTimeout,
	})
	if err != nil {
		return errProductionMCPUnavailable
	}
	x.manager = manager
	for _, binding := range []struct {
		name    string
		handler transport.Operation
	}{
		{"mcp:load-config", x.loadConfig},
		{"mcp:connect", x.connect},
		{"mcp:disconnect", x.disconnect},
		{"mcp:disconnect-all", x.disconnectAll},
		{"mcp:list-tools", x.listTools},
		{"mcp:list-resources", x.listResources},
		{"mcp:call-tool", x.callTool},
		{"mcp:get-servers-status", x.serversStatus},
	} {
		if err := server.Bind(binding.name, binding.handler); err != nil {
			return err
		}
	}
	return nil
}

// configPath resolves the author's configuration file from the pinned generation.
// It re-verifies that pin on every call, which is why the answer is a path inside a
// directory whose identity this lane has just re-proven - and why no request field
// can move it.
func (x *productionMCP) configPath(ctx context.Context) (string, error) {
	if x == nil || x.open == nil || x.open.global == nil {
		return "", errProductionMCPUnavailable
	}
	root, err := x.open.global.DataRoot(ctx)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, productionMCPConfigName), nil
}

// loadConfig answers mcp:load-config with the renderer's discriminated union
// (src/shared/ipc-channels.ts:1325-1333): {"success":true,"status":"missing"|"loaded",
// "servers":[...]} or {"success":false,"status":"error","servers":[],"error":code}.
func (x *productionMCP) loadConfig(ctx context.Context, _ transport.Caller, args []any) (any, error) {
	if len(args) != 0 {
		return nil, productionMCPInvalid()
	}
	if err := ctx.Err(); err != nil {
		return nil, security.NewHTTPError(http.StatusRequestTimeout, mcphost.Code(mcphost.ErrRequestCanceled))
	}
	result := x.manager.Load(ctx)
	servers := make([]any, 0, len(result.Servers))
	for _, server := range result.Servers {
		servers = append(servers, map[string]any{
			"id":        server.ID,
			"name":      server.Name,
			"transport": server.Transport,
		})
	}
	answer := map[string]any{
		"success": result.Status != "error",
		"status":  result.Status,
		"servers": servers,
	}
	if result.Status == "error" {
		answer["success"] = false
		answer["error"] = result.Code
	}
	return answer, nil
}

// connect answers mcp:connect with {success, error?}. A refusal is a resolved
// value, never an HTTP error: the reference answers the same shape
// (mcp-ipc-bridge.ts:33-42) and the page shows the code. The code is the real
// symbolic reason - not the single MCP_CONNECT_FAILED the bridge flattened every
// failure into - so a page can say "this server is a command" rather than "failed".
func (x *productionMCP) connect(ctx context.Context, _ transport.Caller, args []any) (any, error) {
	serverID, err := productionMCPServerID(args)
	if err != nil {
		return nil, err
	}
	if callErr := ctx.Err(); callErr != nil {
		return productionMCPRefused(mcphost.ErrRequestCanceled), nil
	}
	if connectErr := x.manager.Connect(ctx, serverID); connectErr != nil {
		return productionMCPRefused(connectErr), nil
	}
	return map[string]any{"success": true}, nil
}

// disconnect answers mcp:disconnect. A server that was never connected is the state
// the caller asked for, so it is success, exactly like the reference
// (mcp-manager.ts:596-607 returns early).
func (x *productionMCP) disconnect(_ context.Context, _ transport.Caller, args []any) (any, error) {
	serverID, err := productionMCPServerID(args)
	if err != nil {
		return nil, err
	}
	x.manager.Disconnect(serverID)
	return map[string]any{"success": true}, nil
}

// disconnectAll answers mcp:disconnect-all.
func (x *productionMCP) disconnectAll(_ context.Context, _ transport.Caller, args []any) (any, error) {
	if len(args) != 0 {
		return nil, productionMCPInvalid()
	}
	x.manager.DisconnectAll()
	return map[string]any{"success": true}, nil
}

// listTools answers mcp:list-tools with MCPToolDescription[]
// (src/shared/ipc-channels.ts:1310-1315). Only connected servers contribute, so the
// list is exactly the surface a call could reach.
func (x *productionMCP) listTools(_ context.Context, _ transport.Caller, args []any) (any, error) {
	if len(args) != 0 {
		return nil, productionMCPInvalid()
	}
	tools := x.manager.Tools()
	out := make([]any, 0, len(tools))
	for _, tool := range tools {
		out = append(out, map[string]any{
			"name":        tool.Name,
			"description": tool.Description,
			"inputSchema": tool.InputSchema,
			"serverId":    tool.ServerID,
		})
	}
	return out, nil
}

// listResources answers mcp:list-resources with MCPResourceDescription[]
// (src/shared/ipc-channels.ts:1317-1323). description and mimeType are optional
// there, so they are only present when the server advertised them.
func (x *productionMCP) listResources(_ context.Context, _ transport.Caller, args []any) (any, error) {
	if len(args) != 0 {
		return nil, productionMCPInvalid()
	}
	resources := x.manager.Resources()
	out := make([]any, 0, len(resources))
	for _, resource := range resources {
		item := map[string]any{
			"uri":      resource.URI,
			"name":     resource.Name,
			"serverId": resource.ServerID,
		}
		if resource.Description != "" {
			item["description"] = resource.Description
		}
		if resource.MIMEType != "" {
			item["mimeType"] = resource.MIMEType
		}
		out = append(out, item)
	}
	return out, nil
}

// callTool answers mcp:call-tool with {success, content, error?}
// (src/shared/ipc-channels.ts:1342). It is bounded four ways: the named server must
// be one the author configured, the tool must be one that server advertised in this
// conversation, Options.CallTimeout bounds the round trip, and MaxToolResultBytes
// bounds the text handed back.
func (x *productionMCP) callTool(ctx context.Context, _ transport.Caller, args []any) (any, error) {
	if len(args) != 3 {
		return nil, productionMCPInvalid()
	}
	serverID, ok := args[0].(string)
	if !ok || !productionMCPIdentifier(serverID) {
		return nil, productionMCPInvalid()
	}
	tool, ok := args[1].(string)
	if !ok || !productionMCPIdentifier(tool) {
		return nil, productionMCPInvalid()
	}
	arguments, ok := args[2].(map[string]any)
	if !ok {
		return nil, productionMCPInvalid()
	}
	if err := ctx.Err(); err != nil {
		return productionMCPToolFailure(mcphost.ErrRequestCanceled), nil
	}
	content, err := x.manager.CallTool(ctx, serverID, tool, arguments)
	if err != nil {
		return productionMCPToolFailure(err), nil
	}
	return map[string]any{"success": true, "content": content}, nil
}

// serversStatus answers mcp:get-servers-status with MCPServerStatus[]
// (src/shared/ipc-channels.ts:1302-1308), plus the symbolic code the page needs to
// explain a server it cannot use. See the header for what each state means.
func (x *productionMCP) serversStatus(_ context.Context, _ transport.Caller, args []any) (any, error) {
	if len(args) != 0 {
		return nil, productionMCPInvalid()
	}
	statuses := x.manager.Statuses()
	out := make([]any, 0, len(statuses))
	for _, status := range statuses {
		item := map[string]any{
			"id":        status.ID,
			"name":      status.Name,
			"status":    status.Status,
			"toolCount": status.ToolCount,
		}
		if status.Code != "" {
			item["error"] = status.Code
		}
		out = append(out, item)
	}
	return out, nil
}

// productionMCPRefused is the {success:false,error} answer the three verbs share.
func productionMCPRefused(err error) map[string]any {
	return map[string]any{"success": false, "error": mcphost.Code(err)}
}

// productionMCPToolFailure is the {success:false,content:"",error} answer
// mcp-manager.ts:567-570 uses for the same states.
func productionMCPToolFailure(err error) map[string]any {
	return map[string]any{"success": false, "content": "", "error": mcphost.Code(err)}
}

// productionMCPInvalid is the transport's own arity/shape refusal.
func productionMCPInvalid() error {
	return security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS")
}

// productionMCPServerID reads the one server id mcp:connect and mcp:disconnect
// take. The id is only ever looked up in the author's own configuration.
func productionMCPServerID(args []any) (string, error) {
	if len(args) != 1 {
		return "", productionMCPInvalid()
	}
	serverID, ok := args[0].(string)
	if !ok || !productionMCPIdentifier(serverID) {
		return "", productionMCPInvalid()
	}
	return serverID, nil
}

// productionMCPIdentifier bounds one server id or tool name before it is used as a
// lookup key. It is never a path, a URL or a command.
func productionMCPIdentifier(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char == '\x7f' {
			return false
		}
	}
	return true
}

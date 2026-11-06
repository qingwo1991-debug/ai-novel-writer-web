// Package mcphost is the MCP (model context protocol) client this container can
// actually have, and the policy around it.
//
// # What a server is, and what this lane may do with one
//
// An MCP server is an author-owned tool server: the renderer turns its advertised
// tools into agent tools (src/stores/mcp-store.ts:147-190), and one tool call is
// arbitrary code execution delegated to a third party. This package therefore
// serves the halves a browser page needs in order to tell its author the truth
// about their own configuration (status and catalogs) plus the lifecycle verbs,
// under three hard rules.
//
//  1. No child processes. The desktop transport starts an author-supplied command
//     (electron/mcp/mcp-manager.ts:283-460, internal/mcp/transports.go:34-62).
//     This lane is a server container: it has no author terminal, no runtime
//     directory and no OS sandbox for such a child, so an entry that names a
//     command is refused by name - ErrStdioUnavailable, wire code
//     MCP_STDIO_TRANSPORT_UNAVAILABLE - while every other entry keeps working.
//     That entry's command, args and env are dropped in the parser and are never
//     stored, so the bytes that would start a process cannot be echoed either.
//
//  2. Every outbound connection passes the committed policy. The one transport is
//     MCP streamable HTTP over a single POST endpoint, dispatched through
//     internal/egress (*Policy, which is productionOpen.ModelEgress in the
//     production lane). A configured URL whose origin the committed policy does
//     not admit is refused with ErrOriginNotAdmitted and is never dialed; a
//     redirect is refused by the policy itself; a nil policy refuses every URL
//     rather than falling back to an ambient client. A URL that carries a query,
//     a fragment, credentials or another scheme is refused as
//     ErrServerURLInvalid before any policy question is asked.
//
//  3. The configuration is the author's, never the caller's. A caller may name a
//     server that exists in the author's own mcp_config.json and may never invent
//     one: no field of any request becomes a path, a URL, a command, an
//     environment value or a header.
//
// # Bounds
//
//	config       MaxConfigBytes, one regular 0600 single-link file, no symlink
//	request      MaxRequestBytes per JSON-RPC message
//	response     MaxFrameBytes per JSON-RPC message (JSON body or one SSE frame)
//	tool result  MaxToolResultBytes of text handed back to the browser
//	catalog      MaxCatalogEntries and MaxCatalogBytes per discovery
//	time         Options.RequestTimeout per handshake/catalog page,
//	             Options.CallTimeout per tools/call
//
// # Credentials
//
// The configuration is credential-bearing: a URL may carry a token in its query
// string and a command entry carries one in env. Every such value is registered
// with the host's redactor (Options.RegisterSecret, the server's own public
// sanitizer) before the file becomes anything this package acts on, and command/
// args/env are dropped immediately afterwards. No error in this package formats a
// URL, a host, a header, a command or a configuration value: each one is a fixed
// symbol, and Code is the one function that turns any of them - including a raw
// internal/egress refusal - into the code that reaches the wire.
package mcphost

import (
	"errors"
	"time"
)

const (
	// MaxConfigBytes bounds one mcp_config.json before it is parsed.
	MaxConfigBytes = 1 << 20
	// MaxRequestBytes bounds one outgoing JSON-RPC message.
	MaxRequestBytes = 1 << 20
	// MaxFrameBytes bounds one incoming JSON-RPC message, whether it arrives as
	// a JSON body or as one event-stream frame.
	MaxFrameBytes = 4 << 20
	// MaxToolResultBytes bounds the text one tools/call hands back to the browser.
	MaxToolResultBytes = 1 << 20
	// MaxCatalogEntries bounds one advertised catalog across all of its pages.
	MaxCatalogEntries = 512
	// MaxCatalogBytes bounds the bytes of every catalog page of one discovery.
	MaxCatalogBytes = 4 << 20
	// MaxServerURLLength bounds one configured endpoint before it is parsed.
	MaxServerURLLength = 2048
	// ProtocolVersion is the revision this client implements.
	ProtocolVersion = "2025-06-18"
	// clientName is the clientInfo name reported to a server. It names this
	// lane, not a desktop build.
	clientName = "ai-novel-writer-web"
	// clientVersion is the clientInfo version reported to a server.
	clientVersion = "1.0.0"
	// defaultRequestTimeout bounds one handshake or catalog page.
	defaultRequestTimeout = 15 * time.Second
	// defaultCallTimeout bounds one tools/call end to end.
	defaultCallTimeout = 60 * time.Second
	// maxID is the largest request identifier this client mints: JavaScript
	// cannot carry a larger integer exactly, and the renderer's tool bridge is
	// a JavaScript caller.
	maxID = 9007199254740991
)

// Every failure this package reports is one of these fixed symbols. None of them
// carries a URL, a host, a header, a command, a configuration value or a server
// message, because a malformed committed endpoint may itself be a credential and
// an external server's error text is not this lane's to publish.
var (
	// ErrConfigMissing is a data directory with no mcp_config.json: the honest
	// "unconfigured" state, not a failure.
	ErrConfigMissing = errors.New("MCP_CONFIG_MISSING")
	// ErrConfigUnreadable is a configuration the pinned data directory would not
	// hand over (wrong owner, wrong mode, a link, a swap, too large).
	ErrConfigUnreadable = errors.New("MCP_CONFIG_UNREADABLE")
	// ErrConfigInvalid is a configuration that is not the documented shape.
	ErrConfigInvalid = errors.New("MCP_CONFIG_INVALID")

	// ErrServerNotConfigured is a server id that the author's own configuration
	// does not name. A caller can never invent one.
	ErrServerNotConfigured = errors.New("MCP_SERVER_NOT_CONFIGURED")
	// ErrServerNotConnected is a catalog or tool call with no live connection.
	ErrServerNotConnected = errors.New("MCP_SERVER_NOT_CONNECTED")
	// ErrStdioUnavailable is the refusal of a command entry. This container has
	// no way to start an author-supplied process, so it says so by name instead
	// of attempting one.
	ErrStdioUnavailable = errors.New("MCP_STDIO_TRANSPORT_UNAVAILABLE")

	// ErrServerURLInvalid is a configured endpoint that is not one absolute
	// HTTP(S) URL without credentials, query or fragment.
	ErrServerURLInvalid = errors.New("MCP_SERVER_URL_INVALID")
	// ErrPolicyUnavailable is a lane with no committed egress policy. Nothing is
	// dialed in that state.
	ErrPolicyUnavailable = errors.New("MCP_EGRESS_POLICY_UNAVAILABLE")
	// ErrOriginNotAdmitted is an endpoint whose origin the committed policy does
	// not admit. It is refused, never dialed.
	ErrOriginNotAdmitted = errors.New("MCP_ORIGIN_NOT_ADMITTED")

	// ErrConnectFailed is a transport that did not answer at all.
	ErrConnectFailed = errors.New("MCP_CONNECT_FAILED")
	// ErrServerRejected is a server that answered outside 2xx.
	ErrServerRejected = errors.New("MCP_SERVER_REJECTED")
	// ErrProtocolInvalid is a body that is not the JSON-RPC this client speaks.
	ErrProtocolInvalid = errors.New("MCP_PROTOCOL_INVALID")
	// ErrRequestFailed is a JSON-RPC error answer, or a tool that reported its
	// own failure. The server's message text is deliberately not published.
	ErrRequestFailed = errors.New("MCP_REQUEST_FAILED")
	// ErrRequestTimeout is a call that outlived its deadline.
	ErrRequestTimeout = errors.New("MCP_REQUEST_TIMEOUT")
	// ErrRequestCanceled is a call whose caller went away.
	ErrRequestCanceled = errors.New("MCP_REQUEST_CANCELED")
	// ErrRequestTooLarge is a JSON-RPC message over MaxRequestBytes.
	ErrRequestTooLarge = errors.New("MCP_REQUEST_TOO_LARGE")
	// ErrResponseTooLarge is a JSON-RPC message over MaxFrameBytes.
	ErrResponseTooLarge = errors.New("MCP_RESPONSE_LIMIT_EXCEEDED")
	// ErrCatalogTooLarge is a catalog over MaxCatalogEntries or MaxCatalogBytes.
	ErrCatalogTooLarge = errors.New("MCP_CATALOG_LIMIT_EXCEEDED")
	// ErrToolResultTooLarge is a tool answer over MaxToolResultBytes.
	ErrToolResultTooLarge = errors.New("MCP_TOOL_RESULT_TOO_LARGE")
	// ErrToolNotDiscovered is a tool the named server never advertised.
	ErrToolNotDiscovered = errors.New("MCP_TOOL_NOT_DISCOVERED")
	// ErrRequestInvalid is an argument shape this package refuses before any
	// work (an empty server id, a missing tool name).
	ErrRequestInvalid = errors.New("MCP_REQUEST_INVALID")
)

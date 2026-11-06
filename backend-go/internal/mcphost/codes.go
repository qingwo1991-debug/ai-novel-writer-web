package mcphost

import (
	"context"
	"errors"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/egress"
)

// codeTable is the ONE mapping layer of this package. Every usable failure -
// this package's own symbols and the committed policy's, which are the two error
// vocabularies any code path here can produce - appears exactly once, in the
// order the checks run. Nothing else in this package or in the binder above it
// invents a code, and nothing formats an error into one: a rejected endpoint, a
// server's answer and a configuration value all leave as a fixed string.
//
// A code that is not in this table is not published at all: Code answers
// MCP_REQUEST_FAILED for it, which is the same conservative code the desktop
// control plane uses when it has no better symbol
// (electron/mcp/mcp-manager.ts:41-46).
var codeTable = [...]struct {
	err  error
	code string
}{
	{ErrConfigMissing, "MCP_CONFIG_MISSING"},
	{ErrConfigUnreadable, "MCP_CONFIG_UNREADABLE"},
	{ErrConfigInvalid, "MCP_CONFIG_INVALID"},

	{ErrServerNotConfigured, "MCP_SERVER_NOT_CONFIGURED"},
	{ErrServerNotConnected, "MCP_SERVER_NOT_CONNECTED"},
	{ErrStdioUnavailable, "MCP_STDIO_TRANSPORT_UNAVAILABLE"},

	{ErrServerURLInvalid, "MCP_SERVER_URL_INVALID"},
	{ErrPolicyUnavailable, "MCP_EGRESS_POLICY_UNAVAILABLE"},
	{ErrOriginNotAdmitted, "MCP_ORIGIN_NOT_ADMITTED"},

	{ErrConnectFailed, "MCP_CONNECT_FAILED"},
	{ErrServerRejected, "MCP_SERVER_REJECTED"},
	{ErrProtocolInvalid, "MCP_PROTOCOL_INVALID"},
	{ErrRequestFailed, "MCP_REQUEST_FAILED"},
	{ErrRequestTimeout, "MCP_REQUEST_TIMEOUT"},
	{ErrRequestCanceled, "MCP_REQUEST_CANCELED"},
	{ErrRequestInvalid, "MCP_REQUEST_INVALID"},
	{ErrRequestTooLarge, "MCP_REQUEST_TOO_LARGE"},
	{ErrResponseTooLarge, "MCP_RESPONSE_LIMIT_EXCEEDED"},
	{ErrCatalogTooLarge, "MCP_CATALOG_LIMIT_EXCEEDED"},
	{ErrToolResultTooLarge, "MCP_TOOL_RESULT_TOO_LARGE"},
	{ErrToolNotDiscovered, "MCP_TOOL_NOT_DISCOVERED"},

	// The committed policy's own vocabulary, inherited rather than restated:
	// a non-admitted origin and a redirect it refuses are the same refusal for
	// this lane, and an unreadable policy is never widened into a default.
	{egress.ErrNotAllowed, "MCP_ORIGIN_NOT_ADMITTED"},
	{egress.ErrRedirectForbidden, "MCP_ORIGIN_NOT_ADMITTED"},
	{egress.ErrPolicyInvalid, "MCP_EGRESS_POLICY_UNAVAILABLE"},
	{egress.ErrBaseURLInvalid, "MCP_SERVER_URL_INVALID"},
	{egress.ErrRequestFailed, "MCP_CONNECT_FAILED"},

	{context.Canceled, "MCP_REQUEST_CANCELED"},
	{context.DeadlineExceeded, "MCP_REQUEST_TIMEOUT"},
}

// Code maps one error to the symbolic wire code that describes it. It is total:
// every error gets a code, and an error nobody registered gets the conservative
// one instead of its text.
func Code(err error) string {
	if err == nil {
		return ""
	}
	for _, entry := range codeTable {
		if errors.Is(err, entry.err) {
			return entry.code
		}
	}
	return "MCP_REQUEST_FAILED"
}

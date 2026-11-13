package transport

import (
	"context"
	"errors"
	"net/http"

	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/globalconfig"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/mcp"
	"github.com/qingwo1991-debug/ai-novel-writer-web/backend-go/internal/security"
)

// MCPAdmin is a captured server-owned role policy, never a flag in RPC arguments.
// External tools have the server OS identity; enabling is a separate host action.
// BindMCP requires a disabled, unconfigured manager so secret protection can be
// installed BEFORE any configuration/environment values reach an external tool.
func (s *Server) BindMCP(manager *mcp.Manager, startup *globalconfig.Startup, admin func(Caller) bool) error {
	if manager == nil || startup == nil || admin == nil {
		return errors.New("MCP_PROVIDER_REQUIRED")
	}
	if err := manager.SetSecretRegistrar(s.RegisterSecret); err != nil {
		return errors.New("MCP_PROVIDER_NOT_PRISTINE")
	}
	handlers := map[string]Operation{}
	for _, name := range []string{"mcp:load-config", "mcp:connect", "mcp:disconnect", "mcp:disconnect-all", "mcp:list-tools", "mcp:list-resources", "mcp:call-tool", "mcp:get-servers-status"} {
		operation := name
		handlers[operation] = func(ctx context.Context, caller Caller, args []any) (any, error) {
			// Preserve the original gateway's conservative administration policy: only
			// tool summaries/status are ordinary-author readable, not resources/calls.
			if operation != "mcp:list-tools" && operation != "mcp:get-servers-status" && !admin(caller) {
				return nil, security.NewHTTPError(http.StatusForbidden, "MCP_SERVER_ADMIN_ONLY")
			}
			if startup.State(startupCaller(caller), true).State != "ready" {
				return nil, security.NewHTTPError(http.StatusConflict, "GLOBAL_DATA_NOT_READY")
			}
			if err := ctx.Err(); err != nil {
				return nil, errors.New("MCP_REQUEST_CANCELED")
			}
			invalid := func() (any, error) { return nil, security.NewHTTPError(http.StatusBadRequest, "INVALID_ARGUMENTS") }
			switch operation {
			case "mcp:load-config":
				if len(args) != 0 {
					return invalid()
				}
				return manager.LoadConfig(), nil
			case "mcp:connect":
				if len(args) != 1 {
					return invalid()
				}
				id, ok := args[0].(string)
				if !ok || id == "" {
					return invalid()
				}
				if err := manager.Connect(ctx, id); err != nil {
					return map[string]any{"success": false, "error": "MCP_CONNECT_FAILED"}, nil
				}
				return map[string]any{"success": true}, nil
			case "mcp:disconnect":
				if len(args) != 1 {
					return invalid()
				}
				id, ok := args[0].(string)
				if !ok || id == "" {
					return invalid()
				}
				manager.Disconnect(id)
				return map[string]any{"success": true}, nil
			case "mcp:disconnect-all":
				if len(args) != 0 {
					return invalid()
				}
				manager.DisconnectAll()
				return map[string]any{"success": true}, nil
			case "mcp:list-tools":
				if len(args) != 0 {
					return invalid()
				}
				return manager.ListTools(), nil
			case "mcp:list-resources":
				if len(args) != 0 {
					return invalid()
				}
				return manager.ListResources(), nil
			case "mcp:get-servers-status":
				if len(args) != 0 {
					return invalid()
				}
				return manager.ServersStatus(), nil
			case "mcp:call-tool":
				if len(args) != 3 {
					return invalid()
				}
				id, iok := args[0].(string)
				tool, tok := args[1].(string)
				input, aok := args[2].(map[string]any)
				if !iok || !tok || !aok || id == "" || tool == "" {
					return invalid()
				}
				return manager.CallTool(ctx, id, tool, input), nil
			}
			return nil, errors.New("OPERATION_NOT_EXPOSED")
		}
	}
	return s.bindOperations(handlers)
}

package agent

import (
	"context"
	"fmt"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/tools"
)

// McpAuth tool: starts the OAuth flow for a configured MCP HTTP server so its
// tools become authorized. The flow itself (browser + callback + token
// persistence) is wired by the harness via Config.McpAuth.

const mcpAuthDescription = `Start the OAuth authorization flow for an MCP server (HTTP transport). A browser window opens for consent; the resulting token is stored encrypted and used automatically for that server's tools from the next connection. Use when a server reports 401/403 Unauthorized.`

type mcpAuthArgs struct {
	Server string `json:"server"`
}

// mcpAuthTool builds the McpAuth tool when a flow is wired.
func (a *Agent) mcpAuthTool() []coretools.Tool {
	if a.mcpAuth == nil {
		return nil
	}
	t := tools.MustTool(coretools.FromFunc("McpAuth", mcpAuthDescription,
		func(ctx context.Context, args mcpAuthArgs) (coretools.Result, error) {
			if args.Server == "" {
				return coretools.Result{Content: "Error: server name is required"}, nil
			}
			if err := a.mcpAuth(ctx, args.Server); err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			return coretools.Result{Content: fmt.Sprintf("Authorized MCP server %s. Its tools authenticate on the next connection (restart woca if it was already connected).", args.Server)}, nil
		}))
	return []coretools.Tool{t}
}

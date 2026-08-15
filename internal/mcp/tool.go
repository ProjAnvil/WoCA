package mcp

import (
	"context"
	"fmt"
	"strings"

	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// AsTool converts an MCP tool into a langchain Tool, namespaced
// mcp__<server>__<tool>.
func (c *Client) AsTool(serverName string, t Tool) coretools.Tool {
	name := fmt.Sprintf("mcp__%s__%s", serverName, t.Name)
	s := normalizeSchema(t.InputSchema)
	tool, err := coretools.NewFunc(name, t.Description, s,
		func(ctx context.Context, input map[string]any) (coretools.Result, error) {
			out, err := c.CallTool(ctx, t.Name, input)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: out}, nil
		})
	if err != nil {
		// Fall back to an empty object schema on validation failure.
		tool, _ = coretools.NewFunc(name, t.Description, schema.Object(map[string]schema.Schema{}),
			func(ctx context.Context, input map[string]any) (coretools.Result, error) {
				out, err := c.CallTool(ctx, t.Name, input)
				if err != nil {
					return coretools.Result{}, err
				}
				return coretools.Result{Content: out}, nil
			})
	}
	return tool
}

// AsTools converts all server tools.
func (c *Client) AsTools(serverName string, tools []Tool) []coretools.Tool {
	out := make([]coretools.Tool, 0, len(tools))
	for _, t := range tools {
		out = append(out, c.AsTool(serverName, t))
	}
	return out
}

// normalizeSchema coerces an MCP inputSchema into a valid langchain
// schema.Schema (which is itself a map[string]any).
func normalizeSchema(input map[string]any) schema.Schema {
	s := schema.Schema(input)
	if typ, _ := s["type"].(string); typ != "object" {
		return schema.Object(map[string]schema.Schema{})
	}
	if _, ok := s["properties"]; !ok {
		s["properties"] = map[string]any{}
	}
	return s
}

// AsResourceTools returns list/read resource tools for a server.
func (c *Client) AsResourceTools(serverName string) []coretools.Tool {
	list, err := coretools.NewFunc(
		"mcp__"+serverName+"__resources",
		"List the resources exposed by MCP server "+serverName,
		schema.Object(map[string]schema.Schema{}),
		func(ctx context.Context, input map[string]any) (coretools.Result, error) {
			resources, err := c.ListResources(ctx)
			if err != nil {
				return coretools.Result{}, err
			}
			if len(resources) == 0 {
				return coretools.Result{Content: "(no resources)"}, nil
			}
			var b strings.Builder
			for _, r := range resources {
				fmt.Fprintf(&b, "- %s (%s)\n", r.URI, r.Name)
			}
			return coretools.Result{Content: b.String()}, nil
		})
	if err != nil {
		panic(err)
	}
	read, err := coretools.NewFunc(
		"mcp__"+serverName+"__resource",
		"Read an MCP resource by URI from server "+serverName,
		schema.Object(map[string]schema.Schema{"uri": schema.String("resource URI")}, "uri"),
		func(ctx context.Context, input map[string]any) (coretools.Result, error) {
			uri, _ := input["uri"].(string)
			text, err := c.ReadResource(ctx, uri)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: text}, nil
		})
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{list, read}
}

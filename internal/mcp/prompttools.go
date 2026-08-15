package mcp

import (
	"context"
	"strings"

	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// AsPromptTools returns list/get prompt tools for a server, named
// mcp__<server>__prompts and mcp__<server>__prompt.
func (c *Client) AsPromptTools(serverName string) []coretools.Tool {
	list, err := coretools.NewFunc(
		"mcp__"+serverName+"__prompts",
		"List the prompts exposed by MCP server "+serverName,
		schema.Object(map[string]schema.Schema{}),
		func(ctx context.Context, input map[string]any) (coretools.Result, error) {
			prompts, err := c.ListPrompts(ctx)
			if err != nil {
				return coretools.Result{}, err
			}
			if len(prompts) == 0 {
				return coretools.Result{Content: "(no prompts)"}, nil
			}
			var b strings.Builder
			for _, p := range prompts {
				b.WriteString(formatPrompt(p))
				b.WriteByte('\n')
			}
			return coretools.Result{Content: b.String()}, nil
		})
	if err != nil {
		panic(err)
	}

	get, err := coretools.NewFunc(
		"mcp__"+serverName+"__prompt",
		"Get an MCP prompt by name from server "+serverName,
		schema.Object(map[string]schema.Schema{
			"name":      schema.String("prompt name"),
			"arguments": schema.Schema{"type": "object", "description": "prompt arguments"},
		}, "name"),
		func(ctx context.Context, input map[string]any) (coretools.Result, error) {
			name, _ := input["name"].(string)
			var args map[string]any
			if v, ok := input["arguments"]; ok && v != nil {
				if m, ok := v.(map[string]any); ok {
					args = m
				}
			}
			text, err := c.GetPrompt(ctx, name, args)
			if err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: text}, nil
		})
	if err != nil {
		panic(err)
	}

	return []coretools.Tool{list, get}
}

// formatPrompt renders a single prompt as "name: description (arguments: a, b)".
func formatPrompt(p Prompt) string {
	var b strings.Builder
	b.WriteString("- ")
	b.WriteString(p.Name)
	if p.Description != "" {
		b.WriteString(": ")
		b.WriteString(p.Description)
	}
	if len(p.Arguments) > 0 {
		b.WriteString(" (arguments: ")
		for i, a := range p.Arguments {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(a.Name)
			if a.Required {
				b.WriteString(" (required)")
			}
		}
		b.WriteByte(')')
	}
	return b.String()
}

package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Prompt is a prompt template exposed by an MCP server.
type Prompt struct {
	Name        string           `json:"name"`
	Description string           `json:"description,omitempty"`
	Arguments   []PromptArgument `json:"arguments,omitempty"`
}

// PromptArgument describes one argument accepted by a Prompt.
type PromptArgument struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
}

// ListPrompts returns the server's prompts (prompts/list).
func (c *Client) ListPrompts(ctx context.Context) ([]Prompt, error) {
	raw, err := c.request(ctx, "prompts/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Prompts []Prompt `json:"prompts"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: decode prompts/list: %w", err)
	}
	return out.Prompts, nil
}

// GetPrompt resolves a prompt (prompts/get) and returns its text content
// (concatenated from the response's messages[].content[].text).
func (c *Client) GetPrompt(ctx context.Context, name string, args map[string]any) (string, error) {
	raw, err := c.request(ctx, "prompts/get", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mcp: decode prompts/get: %w", err)
	}
	var b strings.Builder
	for _, m := range out.Messages {
		b.WriteString(promptContentText(m.Content))
	}
	return b.String(), nil
}

// promptContentText extracts the concatenated text from a prompt message's
// content, which may be a single content block or an array of blocks.
func promptContentText(raw json.RawMessage) string {
	var single struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &single); err == nil && single.Type == "text" {
		return single.Text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var b strings.Builder
	for _, block := range blocks {
		if block.Type == "text" {
			b.WriteString(block.Text)
		}
	}
	return b.String()
}

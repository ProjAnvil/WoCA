package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// Subscribe registers interest in a resource (resources/subscribe).
func (c *Client) Subscribe(ctx context.Context, uri string) error {
	_, err := c.request(ctx, "resources/subscribe", map[string]any{"uri": uri})
	return err
}

// Unsubscribe removes interest in a resource (resources/unsubscribe).
func (c *Client) Unsubscribe(ctx context.Context, uri string) error {
	_, err := c.request(ctx, "resources/unsubscribe", map[string]any{"uri": uri})
	return err
}

// SamplingMessage is one message in a sampling request.
type SamplingMessage struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

// CreateMessageParams is a sampling/createMessage request.
type CreateMessageParams struct {
	Messages     []SamplingMessage `json:"messages"`
	MaxTokens    int               `json:"maxTokens"`
	SystemPrompt string            `json:"systemPrompt,omitempty"`
}

// CreateMessage requests a model sampling from the server and returns the
// assistant's text (concatenated from result.content[].text).
func (c *Client) CreateMessage(ctx context.Context, params CreateMessageParams) (string, error) {
	raw, err := c.request(ctx, "sampling/createMessage", params)
	if err != nil {
		return "", err
	}
	var out struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mcp: decode sampling/createMessage: %w", err)
	}
	return promptContentText(out.Content), nil
}

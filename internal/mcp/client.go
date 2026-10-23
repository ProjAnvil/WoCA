// Package mcp implements a minimal Model Context Protocol client over the
// stdio and streamable-HTTP transports: initialize, tools/list, tools/call,
// resources/list, resources/read, and server→client notifications over
// JSON-RPC.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/projanvil/woca/internal/platform/executor"
)

const protocolVersion = "2024-11-05"

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int64           `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("mcp: error %d: %s", e.Code, e.Message) }

// Client is an MCP client over a transport.
type Client struct {
	name string
	t    transport

	// OnNotification, when set, receives server→client notifications (JSON-RPC
	// messages with a method but no id).
	OnNotification func(method string, params json.RawMessage)

	// mu guards nextID only. The round-trip itself is not serialized so that
	// transports may run concurrent requests.
	mu     sync.Mutex
	nextID int64
}

func newClient(name string, t transport) *Client {
	c := &Client{name: name, t: t}
	t.setNotifier(func(method string, params json.RawMessage) {
		if c.OnNotification != nil {
			c.OnNotification(method, params)
		}
	})
	return c
}

// NewClient builds a stdio client over an arbitrary writer/reader pair
// (testable).
func NewClient(name string, w io.Writer, r io.Reader, closer func() error) *Client {
	t := newStdioTransportFromIO(w, r)
	t.closer = closer
	return newClient(name, t)
}

// NewStdio builds a stdio client that spawns a process on the local machine.
func NewStdio(name, command string, args ...string) *Client {
	return NewStdioWithExecutor(nil, name, command, args...)
}

// NewStdioWithExecutor is NewStdio with an explicit execution backend for the
// server process; a nil executor falls back to the local machine.
func NewStdioWithExecutor(x executor.Executor, name, command string, args ...string) *Client {
	if x == nil {
		x = executor.NewLocal()
	}
	t := newStdioTransport(x, name, command, args...)
	if t == nil {
		return nil
	}
	return newClient(name, t)
}

// Initialize performs the MCP handshake.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": c.name, "version": "0.1.0"},
	}
	if _, err := c.request(ctx, "initialize", params); err != nil {
		return err
	}
	note := rpcRequest{JSONRPC: "2.0", Method: "notifications/initialized"}
	return c.notify(note)
}

// Tool is a tool exposed by an MCP server.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Name returns the server name this client was configured with.
func (c *Client) Name() string { return c.name }

// ListTools fetches the server's tool list.
func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	raw, err := c.request(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Tools []Tool `json:"tools"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: decode tools/list: %w", err)
	}
	return out.Tools, nil
}

// CallTool calls a tool and returns the concatenated text content.
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	raw, err := c.request(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mcp: decode tools/call: %w", err)
	}
	if out.IsError {
		return "", fmt.Errorf("mcp: tool %s returned an error", name)
	}
	var result strings.Builder
	for _, block := range out.Content {
		if block.Type == "text" {
			result.WriteString(block.Text)
		}
	}
	return result.String(), nil
}

// Close releases the underlying transport.
func (c *Client) Close() error {
	if c.t == nil {
		return nil
	}
	return c.t.close()
}

// Listen opens a long-lived GET SSE connection (streamable HTTP transport only)
// and dispatches server notifications through OnNotification until ctx is done.
// For the stdio transport it is a no-op returning nil (notifications already
// arrive inline on stdin).
func (c *Client) Listen(ctx context.Context) error {
	if l, ok := c.t.(interface{ listen(context.Context) error }); ok {
		return l.listen(ctx)
	}
	return nil
}

func (c *Client) notify(req rpcRequest) error {
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return c.t.send(data)
}

func (c *Client) request(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}
	data, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := c.t.roundTrip(ctx, id, data)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

// Resource is a resource exposed by an MCP server.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ListResources fetches the server's resources.
func (c *Client) ListResources(ctx context.Context) ([]Resource, error) {
	raw, err := c.request(ctx, "resources/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var out struct {
		Resources []Resource `json:"resources"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("mcp: decode resources/list: %w", err)
	}
	return out.Resources, nil
}

// ReadResource reads a resource by URI, returning its text content.
func (c *Client) ReadResource(ctx context.Context, uri string) (string, error) {
	raw, err := c.request(ctx, "resources/read", map[string]any{"uri": uri})
	if err != nil {
		return "", err
	}
	var out struct {
		Contents []struct {
			URI  string `json:"uri"`
			Text string `json:"text"`
		} `json:"contents"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("mcp: decode resources/read: %w", err)
	}
	var b []string
	for _, c := range out.Contents {
		b = append(b, c.Text)
	}
	return strings.Join(b, "\n"), nil
}

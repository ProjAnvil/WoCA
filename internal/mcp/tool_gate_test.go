package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
	"github.com/projanvil/woca/internal/runexec"
	"github.com/projanvil/langchain-golang/core/schema"
)

// errTransport fails every round trip, so the un-gated local branch errors
// on the client without needing a live server.
type errTransport struct{}

func (errTransport) roundTrip(context.Context, int64, []byte) (rpcResponse, error) {
	return rpcResponse{}, errors.New("no connection")
}
func (errTransport) send([]byte) error                  { return errors.New("no connection") }
func (errTransport) close() error                       { return nil }
func (errTransport) setNotifier(func(string, json.RawMessage)) {}

// MCP stdio servers run as host processes spawned at build time — they
// cannot follow a per-run sandbox (design §4.5 / §10.2 M-8), so the tool
// Invoke refuses up front with an explicit error instead of silently
// talking to a host process during a sandboxed run.
func TestAsToolSandboxGate(t *testing.T) {
	c := newClient("srv", errTransport{})
	tool := c.AsTool("srv", Tool{Name: "echo", Description: "echo", InputSchema: schema.Object(map[string]schema.Schema{})})

	sbx, err := executor.NewSandbox(executor.SandboxConfig{Endpoint: fakesandbox.Addr(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sbx.Close() }()

	ctx := runexec.WithRunExecutor(context.Background(), sbx)
	_, err = tool.Invoke(ctx, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "MCP stdio servers are not available during sandboxed runs") {
		t.Fatalf("sandboxed invoke err = %v", err)
	}

	// Local ctx (no injection): the gate stays open; the call proceeds to
	// the client (and fails on the unconnected client, not on the gate).
	if _, err := tool.Invoke(context.Background(), map[string]any{}); err == nil || strings.Contains(err.Error(), "sandboxed runs") {
		t.Fatalf("local invoke err = %v (gate must not fire)", err)
	}
}

package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/projanvil/woca/internal/platform/executor"
)

// transport sends and receives JSON-RPC messages for a Client.
type transport interface {
	// roundTrip sends a request (already serialized) and returns the response
	// whose id matches. Notifications encountered along the way are dispatched
	// through the notifier.
	roundTrip(ctx context.Context, id int64, req []byte) (rpcResponse, error)
	// send delivers a one-way notification (no response expected).
	send(req []byte) error
	close() error
	setNotifier(fn func(method string, params json.RawMessage))
}

// stdioTransport implements transport over a process's stdin/stdout using
// newline-delimited JSON-RPC.
type stdioTransport struct {
	w        io.Writer
	r        *bufio.Reader
	closer   func() error
	notifier func(method string, params json.RawMessage)

	// mu serializes writes and reads over the single stdio pipe: a request's
	// write must be followed by reads for that same request before another
	// write can occur.
	mu sync.Mutex
}

func newStdioTransportFromIO(w io.Writer, r io.Reader) *stdioTransport {
	return &stdioTransport{w: w, r: bufio.NewReader(r)}
}

func newStdioTransport(x executor.Executor, name, command string, args ...string) *stdioTransport {
	// The MCP server outlives the request that connected it (clients are
	// kept for between-turn tool refreshes), so it starts under the
	// background context; close terminates it explicitly. Its stderr passes
	// through to the parent's so server logs stay visible in the console.
	proc, err := x.Start(context.Background(),
		executor.ExecRequest{Name: command, Args: args},
		executor.StartOptions{Stderr: os.Stderr})
	if err != nil {
		return nil
	}
	t := &stdioTransport{w: proc.Stdin(), r: bufio.NewReader(proc.Stdout())}
	t.closer = func() error {
		_ = proc.Stdin().Close()
		_ = proc.Kill()
		_ = proc.Wait()
		return nil
	}
	return t
}

func (t *stdioTransport) setNotifier(fn func(string, json.RawMessage)) { t.notifier = fn }

func (t *stdioTransport) roundTrip(ctx context.Context, id int64, req []byte) (rpcResponse, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return rpcResponse{}, err
	}
	if _, err := t.w.Write(append(req, '\n')); err != nil {
		return rpcResponse{}, fmt.Errorf("mcp: write request: %w", err)
	}
	for {
		line, err := t.r.ReadBytes('\n')
		if err != nil {
			return rpcResponse{}, fmt.Errorf("mcp: read response: %w", err)
		}
		var raw struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int64          `json:"id"`
			Method  string          `json:"method"`
			Result  json.RawMessage `json:"result"`
			Error   *rpcError       `json:"error"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			continue
		}
		if raw.Method != "" && raw.ID == nil {
			if t.notifier != nil {
				t.notifier(raw.Method, raw.Params)
			}
			continue
		}
		if raw.ID == nil || *raw.ID != id {
			continue
		}
		return rpcResponse{ID: id, Result: raw.Result, Error: raw.Error}, nil
	}
}

func (t *stdioTransport) send(req []byte) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, err := t.w.Write(append(req, '\n'))
	return err
}

func (t *stdioTransport) close() error {
	if t.closer != nil {
		return t.closer()
	}
	return nil
}

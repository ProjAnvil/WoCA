package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
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

func newStdioTransport(name, command string, args ...string) *stdioTransport {
	cmd := exec.Command(command, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil
	}
	t := &stdioTransport{w: stdin, r: bufio.NewReader(stdout)}
	t.closer = func() error {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
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

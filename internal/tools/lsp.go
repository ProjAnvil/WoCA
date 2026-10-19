package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/runexec"
)

// LSP tool: pulls diagnostics for one file from a language server speaking
// LSP over stdio. The server
// command comes from the WOCA_LSP_SERVER environment variable (e.g.
// "gopls", "typescript-language-server --stdio"); the pool initializes a
// server once at spawn and each call speaks enough JSON-RPC for didOpen →
// textDocument/diagnostic → didClose.

// lspTransport is the JSON-RPC transport the client runs over (stdio in
// production; fakes in tests).
type lspTransport interface {
	Call(ctx context.Context, method string, params, result any) error
	Notify(ctx context.Context, method string, params any) error
	Close() error
}

// stdioTransport runs a language server subprocess and frames LSP JSON-RPC
// messages over its stdin/stdout.
type stdioTransport struct {
	proc   executor.Process
	stdin  *jsonWriter
	stdout *bufio.Reader

	mu      sync.Mutex
	next    atomic.Int64
	pending map[int64]chan lspResponse
	closed  bool
}

type lspResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// jsonWriter writes Content-Length framed messages to a pipe.
type jsonWriter struct {
	w  io.WriteCloser
	mu sync.Mutex
}

func (j *jsonWriter) write(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if _, err := fmt.Fprintf(j.w, "Content-Length: %d\r\n\r\n", len(data)); err != nil {
		return err
	}
	_, err = j.w.Write(data)
	return err
}

// close shuts the write side so the server's read loop sees EOF.
func (j *jsonWriter) close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.w.Close()
}

// startLSP spawns the language server command on the local machine.
func startLSP(command []string) (*stdioTransport, error) {
	return startLSPWith(executor.NewLocal(), command)
}

// startLSPWith spawns the language server command through x.
func startLSPWith(x executor.Executor, command []string) (*stdioTransport, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("no LSP server configured (set WOCA_LSP_SERVER)")
	}
	// The language server outlives individual tool calls, so it starts under
	// the background context; Close terminates it explicitly.
	proc, err := x.Start(context.Background(),
		executor.ExecRequest{Name: command[0], Args: command[1:]},
		executor.StartOptions{})
	if err != nil {
		return nil, err
	}
	t := &stdioTransport{
		proc:    proc,
		stdin:   &jsonWriter{w: proc.Stdin()},
		stdout:  bufio.NewReader(proc.Stdout()),
		pending: map[int64]chan lspResponse{},
	}
	go t.readLoop()
	return t, nil
}

// readLoop routes server responses to pending calls and drops notifications.
func (t *stdioTransport) readLoop() {
	var contentLength int
	for {
		line, err := t.stdout.ReadString('\n')
		if err != nil {
			t.failAll(err)
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			if contentLength > 0 {
				body := make([]byte, contentLength)
				if _, err := ioReadFull(t.stdout, body); err != nil {
					t.failAll(err)
					return
				}
				var msg struct {
					ID     *int64          `json:"id"`
					Result json.RawMessage `json:"result"`
					Error  *struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if err := json.Unmarshal(body, &msg); err != nil {
					continue // malformed frame; keep going
				}
				if msg.ID == nil {
					continue // server notification/request: ignored
				}
				t.mu.Lock()
				ch := t.pending[*msg.ID]
				delete(t.pending, *msg.ID)
				t.mu.Unlock()
				if ch != nil {
					ch <- lspResponse{Result: msg.Result, Error: msg.Error}
				}
			}
			continue
		}
		if strings.HasPrefix(strings.ToLower(line), "content-length:") {
			_, _ = fmt.Sscanf(line, "Content-Length: %d", &contentLength)
		}
	}
}

func ioReadFull(r *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := r.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (t *stdioTransport) failAll(err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, ch := range t.pending {
		select {
		case ch <- lspResponse{Error: &struct {
			Message string `json:"message"`
		}{Message: err.Error()}}:
		default:
		}
		delete(t.pending, id)
	}
}

func (t *stdioTransport) Call(ctx context.Context, method string, params, result any) error {
	id := t.next.Add(1)
	req := map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
	ch := make(chan lspResponse, 1)
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return fmt.Errorf("lsp: transport closed")
	}
	t.pending[id] = ch
	t.mu.Unlock()
	if err := t.stdin.write(req); err != nil {
		return err
	}
	select {
	case resp := <-ch:
		if resp.Error != nil {
			return fmt.Errorf("lsp: %s: %s", method, resp.Error.Message)
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (t *stdioTransport) Notify(ctx context.Context, method string, params any) error {
	notice := map[string]any{"jsonrpc": "2.0", "method": method, "params": params}
	return t.stdin.write(notice)
}

func (t *stdioTransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()
	// Best-effort LSP shutdown; closing stdin makes well-behaved and stub
	// servers exit alike (a bare EOF terminates their read loops).
	_ = t.Notify(context.Background(), "shutdown", nil)
	_ = t.Notify(context.Background(), "exit", nil)
	_ = t.stdin.close()
	return t.proc.Wait()
}

// lspInitialize performs the one-time LSP handshake on a fresh transport:
// the initialize request plus the initialized notification. The pool runs it
// exactly once per spawned server; per-file diagnostics must never
// re-initialize (a second initialize against a live server is a protocol
// error — e.g. gopls answers "initialize called while server in initialized
// state" — which used to make every pooled reuse fail and restart).
func lspInitialize(t lspTransport, workspace string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var initResult json.RawMessage
	if err := t.Call(ctx, "initialize", map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      "file://" + workspace,
		"capabilities": map[string]any{},
	}, &initResult); err != nil {
		return err
	}
	return t.Notify(ctx, "initialized", map[string]any{})
}

// lspDiagnostics pulls diagnostics for one file over an already-initialized
// transport (see lspInitialize; the pool owns the handshake). Only the
// per-file didOpen/diagnostic/didClose sequence runs here, so a pooled server
// can serve many files.
func lspDiagnostics(ctx context.Context, t lspTransport, path string) (string, error) {
	uri := "file://" + path
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	content, _ := os.ReadFile(path)
	if err := t.Notify(ctx, "textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{
			"uri": uri, "languageId": lspLanguageID(path), "version": 1, "text": string(content),
		},
	}); err != nil {
		return "", err
	}

	var report struct {
		Kind  string `json:"kind"`
		Items []struct {
			Severity int      `json:"severity"`
			Range    lspRange `json:"range"`
			Message  string   `json:"message"`
			Source   string   `json:"source"`
		} `json:"items"`
	}
	if err := t.Call(ctx, "textDocument/diagnostic", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	}, &report); err != nil {
		return "", err
	}
	// didClose so the same pooled server can didOpen the same file again.
	_ = t.Notify(ctx, "textDocument/didClose", map[string]any{
		"textDocument": map[string]any{"uri": uri},
	})
	if len(report.Items) == 0 {
		return "(no diagnostics)", nil
	}
	var severity = map[int]string{1: "Error", 2: "Warning", 3: "Info", 4: "Hint"}
	var b bytes.Buffer
	for _, it := range report.Items {
		sev := severity[it.Severity]
		if sev == "" {
			sev = "Issue"
		}
		fmt.Fprintf(&b, "%s %d:%d — %s", sev, it.Range.Start.Line+1, it.Range.Start.Character+1, it.Message)
		if it.Source != "" {
			fmt.Fprintf(&b, " (%s)", it.Source)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// lspRange is an LSP range (zero-based positions).
type lspRange struct {
	Start struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"start"`
	End struct {
		Line      int `json:"line"`
		Character int `json:"character"`
	} `json:"end"`
}

func lspLanguageID(path string) string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return "go"
	case ".ts":
		return "typescript"
	case ".tsx":
		return "typescriptreact"
	case ".js", ".jsx", ".mjs":
		return "javascript"
	case ".py":
		return "python"
	case ".rs":
		return "rust"
	default:
		return "plaintext"
	}
}

// lspTool builds the LSP diagnostics tool. The pool reuses language-server
// processes per (workspace, command) instead of spawning one per call
// (WOCA_LSP_SERVER still resolves the command).
func lspTool(d *Dir, pool *lspPool) coretools.Tool {
	desc := `Get language-server diagnostics (errors, warnings) for one file via LSP. Configure the server with the WOCA_LSP_SERVER environment variable (e.g. "gopls" or "typescript-language-server --stdio"). Use before claiming code compiles or when hunting a compile error.`
	t := MustTool(coretools.FromFunc("LSP", desc,
		func(ctx context.Context, a struct {
			Path string `json:"path"`
		}) (coretools.Result, error) {
			// LSP servers are host processes (pooled, spawned at build
			// time) — unreachable from a sandboxed run (design §10.2
			// M-8), so refuse instead of diagnosing host files.
			if x, ok := runexec.RunExecutorFrom(ctx); ok && executor.IsSandbox(x) {
				return coretools.Result{Content: "Error: LSP is not available during sandboxed runs"}, nil
			}
			command := lspServerCommand()
			if len(command) == 0 {
				return coretools.Result{Content: "Error: no LSP server configured (set WOCA_LSP_SERVER, e.g. WOCA_LSP_SERVER=gopls)"}, nil
			}
			dir := d.For(ctx)
			path := resolvePath(dir, a.Path)
			var out string
			err := pool.call(ctx, dir, command, func(tr lspTransport) error {
				var derr error
				out, derr = lspDiagnostics(ctx, tr, path)
				return derr
			})
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			return coretools.Result{Content: out}, nil
		}))
	return t
}

// lspServerCommand parses WOCA_LSP_SERVER into an argv (simple shell-style
// splitting; no quoting complexity expected for server commands).
func lspServerCommand() []string {
	raw := strings.TrimSpace(os.Getenv("WOCA_LSP_SERVER"))
	if raw == "" {
		return nil
	}
	return strings.Fields(raw)
}

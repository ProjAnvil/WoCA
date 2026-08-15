package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// LSP tool (a lightweight port of Claude Code's LSPTool): pulls diagnostics
// for one file from a language server speaking LSP over stdio. The server
// command comes from the WOCA_LSP_SERVER environment variable (e.g.
// "gopls", "typescript-language-server --stdio"); the client speaks enough
// JSON-RPC for initialize → didOpen → textDocument/diagnostic per call.

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
	cmd    *exec.Cmd
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
	w  *os.File
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

// startLSP spawns the language server command.
func startLSP(command []string) (*stdioTransport, error) {
	if len(command) == 0 {
		return nil, fmt.Errorf("no LSP server configured (set WOCA_LSP_SERVER)")
	}
	cmd := exec.Command(command[0], command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	fstdin, ok := stdin.(*os.File)
	if !ok {
		return nil, fmt.Errorf("lsp: unexpected stdin pipe type")
	}
	fstdout, ok := stdout.(*os.File)
	if !ok {
		return nil, fmt.Errorf("lsp: unexpected stdout pipe type")
	}
	t := &stdioTransport{
		cmd:     cmd,
		stdin:   &jsonWriter{w: fstdin},
		stdout:  bufio.NewReader(fstdout),
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
			fmt.Sscanf(line, "Content-Length: %d", &contentLength)
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
	return t.cmd.Wait()
}

// lspDiagnostics pulls diagnostics for one file over the transport.
func lspDiagnostics(ctx context.Context, t lspTransport, root, path string) (string, error) {
	uri := "file://" + path
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var initResult json.RawMessage
	if err := t.Call(ctx, "initialize", map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      "file://" + root,
		"capabilities": map[string]any{},
	}, &initResult); err != nil {
		return "", err
	}
	if err := t.Notify(ctx, "initialized", map[string]any{}); err != nil {
		return "", err
	}

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

// lspTool builds the LSP diagnostics tool. serverCommand resolves the
// language server to spawn (WOCA_LSP_SERVER).
func lspTool(d *Dir) coretools.Tool {
	desc := `Get language-server diagnostics (errors, warnings) for one file via LSP. Configure the server with the WOCA_LSP_SERVER environment variable (e.g. "gopls" or "typescript-language-server --stdio"). Use before claiming code compiles or when hunting a compile error.`
	t, err := coretools.FromFunc("LSP", desc,
		func(ctx context.Context, a struct {
			Path string `json:"path"`
		}) (coretools.Result, error) {
			command := lspServerCommand()
			if len(command) == 0 {
				return coretools.Result{Content: "Error: no LSP server configured (set WOCA_LSP_SERVER, e.g. WOCA_LSP_SERVER=gopls)"}, nil
			}
			path := resolvePath(d.Current(), a.Path)
			tr, err := startLSP(command)
			if err != nil {
				return coretools.Result{Content: "Error: start LSP server: " + err.Error()}, nil
			}
			out, err := lspDiagnostics(ctx, tr, d.Current(), path)
			_ = tr.Close()
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			return coretools.Result{Content: out}, nil
		})
	if err != nil {
		panic(err)
	}
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

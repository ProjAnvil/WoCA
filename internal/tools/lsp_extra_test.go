package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
	"github.com/projanvil/woca/internal/runexec"
)

// lspTestServerSrc is a mode-aware LSP server used to drive the stdio
// transport's edge cases: "ok" (normal, plus an initial notification), "err"
// (JSON-RPC error responses), "silent" (reads but never answers), "garbage"
// (malformed frame before each valid response), "short" (truncated frame,
// then exit, after reading one request), and "countinit" (answers every
// request idempotently but reports how many initialize requests it has seen
// via the diagnostic message — used to assert the pool handshakes once).
const lspTestServerSrc = `package main

import (
	"bufio"; "encoding/json"; "fmt"; "io"; "os"; "strings"
)

type msg struct {
	ID     *int64  ` + "`json:\"id\"`" + `
	Method string ` + "`json:\"method\"`" + `
}

func write(v any) {
	b, _ := json.Marshal(v)
	fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(b), b)
}

func main() {
	mode := ""
	if len(os.Args) > 1 {
		mode = os.Args[1]
	}
	if mode == "ok" {
		write(map[string]any{"jsonrpc": "2.0", "method": "window/logMessage", "params": map[string]any{}})
	}
	if mode == "die" {
		os.Exit(0)
	}
	r := bufio.NewReader(os.Stdin)
	var n int
	inits := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		if strings.HasPrefix(line, "Content-Length:") {
			fmt.Sscanf(line, "Content-Length: %d", &n)
		} else if line == "\r\n" || line == "\n" {
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil {
				return
			}
			var m msg
			if json.Unmarshal(body, &m) != nil || m.ID == nil {
				continue
			}
			switch mode {
			case "nullresult":
				write(map[string]any{"jsonrpc": "2.0", "id": *m.ID})
			case "countinit":
				var result any
				switch m.Method {
				case "initialize":
					inits++
					result = map[string]any{"capabilities": map[string]any{}}
				case "textDocument/diagnostic":
					result = map[string]any{"kind": "full", "items": []map[string]any{{
						"severity": 1,
						"range": map[string]any{
							"start": map[string]any{"line": 0, "character": 0},
							"end":   map[string]any{"line": 0, "character": 1}},
						"message": fmt.Sprintf("server saw %d initialize calls", inits), "source": "fake",
					}}}
				default:
					result = map[string]any{}
				}
				write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": result})
			case "err":
				write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "error": map[string]any{"message": "boom"}})
			case "silent":
				// swallow the request: never respond
			case "garbage":
				g := "not-json{{{"
				fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(g), g)
				write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": map[string]any{}})
			case "short":
				fmt.Fprintf(os.Stdout, "Content-Length: 1000\r\n\r\ntiny")
				os.Exit(0)
			default:
				var result any
				switch m.Method {
				case "initialize":
					result = map[string]any{"capabilities": map[string]any{}}
				case "textDocument/diagnostic":
					result = map[string]any{"kind": "full", "items": []map[string]any{{
						"severity": 1,
						"range": map[string]any{
							"start": map[string]any{"line": 0, "character": 1},
							"end":   map[string]any{"line": 0, "character": 2}},
						"message": "stub error", "source": "fake",
					}}}
				default:
					result = map[string]any{}
				}
				write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": result})
			}
		}
	}
}
`

// buildLSPTestServer compiles the mode-aware fake server and returns its path.
func buildLSPTestServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "fakeserver.go")
	if err := os.WriteFile(src, []byte(lspTestServerSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "fakeserver")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build fake lsp server: %v\n%s", err, out)
	}
	return bin
}

func TestLSPLanguageID(t *testing.T) {
	cases := map[string]string{
		"main.go":    "go",
		"MAIN.GO":    "go",
		"a.ts":       "typescript",
		"b.tsx":      "typescriptreact",
		"c.js":       "javascript",
		"d.jsx":      "javascript",
		"e.mjs":      "javascript",
		"f.py":       "python",
		"g.rs":       "rust",
		"notes.txt":  "plaintext",
		"Makefile":   "plaintext",
		"dir/x.yaml": "plaintext",
	}
	for path, want := range cases {
		if got := lspLanguageID(path); got != want {
			t.Errorf("lspLanguageID(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestStartLPPEmptyCommand(t *testing.T) {
	if _, err := startLSP(nil); err == nil || !strings.Contains(err.Error(), "no LSP server configured") {
		t.Fatalf("startLSP(nil) = %v, want configuration error", err)
	}
}

func TestStartLSPMissingBinary(t *testing.T) {
	if _, err := startLSP([]string{"/nonexistent/lsp-server-binary"}); err == nil {
		t.Fatal("startLSP(missing binary) = nil error, want start failure")
	}
}

func TestLSPToolConfiguredServer(t *testing.T) {
	bin := buildLSPTestServer(t)
	t.Setenv("WOCA_LSP_SERVER", bin+" ok")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	res := reg.Execute(t.Context(), "LSP", map[string]any{"path": "x.go"})
	if strings.Contains(res.Content, "Error:") {
		t.Fatalf("configured LSP run = %q", res.Content)
	}
	if !strings.Contains(res.Content, "Error 1:2 — stub error (fake)") {
		t.Fatalf("LSP diagnostics = %q", res.Content)
	}
}

// Two consecutive LSP calls on the same registry must reuse the same server
// process and both succeed: with pooling, initialize may happen only once
// (the pool does it at spawn). The old implementation sent initialize on
// every call, and real gopls errors on a second initialize ("initialize
// called while server in initialized state"), so reuse always failed and
// triggered a restart — zeroing out the pooling benefit. The countinit-mode
// server reports how many initialize calls it received via its diagnostic
// message.
func TestLSPToolPooledServerInitializedOnce(t *testing.T) {
	bin := buildLSPTestServer(t)
	t.Setenv("WOCA_LSP_SERVER", bin+" countinit")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte("package x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := NewRegistry(dir)
	defer reg.Close()
	for i := 1; i <= 2; i++ {
		res := reg.Execute(t.Context(), "LSP", map[string]any{"path": "x.go"})
		if strings.Contains(res.Content, "Error:") {
			t.Fatalf("pooled LSP call %d = %q", i, res.Content)
		}
		if !strings.Contains(res.Content, "server saw 1 initialize calls") {
			t.Fatalf("LSP call %d = %q, want server initialized exactly once", i, res.Content)
		}
	}
	if active, _ := reg.LSPStats(); active != 1 {
		t.Fatalf("pooled servers = %d, want 1 (both calls share one process)", active)
	}
}

func TestLSPToolStartFailure(t *testing.T) {
	t.Setenv("WOCA_LSP_SERVER", "/nonexistent/lsp-server-binary")
	reg := NewRegistry(t.TempDir())
	res := reg.Execute(t.Context(), "LSP", map[string]any{"path": "x.go"})
	// The pool surfaces the raw spawn error (no "start LSP server" prefix).
	if !strings.Contains(res.Content, "Error:") || !strings.Contains(res.Content, "/nonexistent/lsp-server-binary") {
		t.Fatalf("start failure = %q", res.Content)
	}
}

func TestLSPToolServerError(t *testing.T) {
	bin := buildLSPTestServer(t)
	t.Setenv("WOCA_LSP_SERVER", bin+" err")
	reg := NewRegistry(t.TempDir())
	res := reg.Execute(t.Context(), "LSP", map[string]any{"path": "x.go"})
	if !strings.Contains(res.Content, "Error: lsp: initialize: boom") {
		t.Fatalf("server error = %q", res.Content)
	}
}

func TestLSPTransportCallAfterClose(t *testing.T) {
	bin := buildLSPTestServer(t)
	tr, err := startLSP([]string{bin, "ok"})
	if err != nil {
		t.Fatal(err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := tr.Call(t.Context(), "initialize", map[string]any{}, nil); err == nil ||
		!strings.Contains(err.Error(), "transport closed") {
		t.Fatalf("Call after Close = %v, want transport closed", err)
	}
	if err := tr.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

func TestLSPTransportCallContextCancel(t *testing.T) {
	bin := buildLSPTestServer(t)
	tr, err := startLSP([]string{bin, "silent"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	var result json.RawMessage
	if err := tr.Call(ctx, "initialize", map[string]any{}, &result); err == nil {
		t.Fatal("Call against silent server = nil error, want ctx deadline")
	}
}

func TestLSPTransportSkipsMalformedFrames(t *testing.T) {
	bin := buildLSPTestServer(t)
	tr, err := startLSP([]string{bin, "garbage"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	var result json.RawMessage
	if err := tr.Call(t.Context(), "initialize", map[string]any{}, &result); err != nil {
		t.Fatalf("Call after malformed frame = %v, want success", err)
	}
}

func TestLSPTransportServerDeathFailsPendingCall(t *testing.T) {
	bin := buildLSPTestServer(t)
	tr, err := startLSP([]string{bin, "short"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var result json.RawMessage
	if err := tr.Call(ctx, "initialize", map[string]any{}, &result); err == nil {
		t.Fatal("Call against dying server = nil error, want failure")
	}
}

func TestLSPDiagnosticsTransportErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	// Handshake failures surface from lspInitialize (the pool's one-time
	// initialize per spawned server).
	if err := lspInitialize(&fakeTransport{responses: map[string]any{}}, dir); err == nil {
		t.Fatal("unscripted initialize = nil error")
	}
	tr := &fakeTransport{responses: map[string]any{"initialize": map[string]any{}}}
	if err := lspInitialize(&notifyFailingTransport{fake: tr, fail: "initialized"}, dir); err == nil {
		t.Fatal("initialized notify failure not surfaced")
	}
	// Per-file sequence: didOpen notify failure, then unscripted diagnostic.
	if _, err := lspDiagnostics(ctx, &notifyFailingTransport{fake: tr, fail: "textDocument/didOpen"}, path); err == nil {
		t.Fatal("didOpen notify failure not surfaced")
	}
	if _, err := lspDiagnostics(ctx, tr, path); err == nil {
		t.Fatal("unscripted diagnostic = nil error")
	}
}

func TestLSPDiagnosticsSeverityFallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.py")
	if err := os.WriteFile(path, []byte("x = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := &fakeTransport{responses: map[string]any{
		"textDocument/diagnostic": map[string]any{
			"kind": "full",
			"items": []map[string]any{
				{"severity": 9, "range": map[string]any{
					"start": map[string]any{"line": 0, "character": 0},
					"end":   map[string]any{"line": 0, "character": 1}},
					"message": "weird severity"},
			},
		},
	}}
	out, err := lspDiagnostics(t.Context(), tr, path)
	if err != nil {
		t.Fatal(err)
	}
	// Unknown severities render as "Issue"; items without a source have no suffix.
	if !strings.Contains(out, "Issue 1:1 — weird severity") {
		t.Fatalf("severity fallback = %q", out)
	}
	if strings.Contains(out, " (") {
		t.Fatalf("source suffix should be omitted: %q", out)
	}
}

func TestJSONWriterErrorPaths(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "jw")
	if err != nil {
		t.Fatal(err)
	}
	jw := &jsonWriter{w: f}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := jw.write(make(chan int)); err == nil {
		t.Fatal("write of unmarshalable value = nil error")
	}
	if err := jw.write(map[string]any{"ok": true}); err == nil {
		t.Fatal("write to closed file = nil error")
	}
}

func TestLSPTransportEmptyResultCall(t *testing.T) {
	bin := buildLSPTestServer(t)
	tr, err := startLSP([]string{bin, "nullresult"})
	if err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	var result json.RawMessage
	if err := tr.Call(t.Context(), "initialize", map[string]any{}, &result); err != nil {
		t.Fatalf("empty-result Call = %v, want nil", err)
	}
	if len(result) != 0 {
		t.Fatalf("result should stay empty, got %s", result)
	}
}

func TestLSPTransportWriteAfterServerDeath(t *testing.T) {
	bin := buildLSPTestServer(t)
	tr, err := startLSP([]string{bin, "die"})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the exited server so the stdin write deterministically EPIPEs.
	_ = tr.proc.Wait()
	if err := tr.Call(t.Context(), "initialize", map[string]any{}, nil); err == nil {
		t.Fatal("Call after server death = nil error, want write failure")
	}
	_ = tr.Close() // second Wait errors; the transport is already dead
}

// notifyFailingTransport wraps fakeTransport and fails Notify for one method.
type notifyFailingTransport struct {
	fake *fakeTransport
	fail string
}

func (n *notifyFailingTransport) Call(ctx context.Context, method string, params, result any) error {
	return n.fake.Call(ctx, method, params, result)
}

func (n *notifyFailingTransport) Notify(ctx context.Context, method string, params any) error {
	if method == n.fail {
		return errNoScript
	}
	return n.fake.Notify(ctx, method, params)
}

func (n *notifyFailingTransport) Close() error { return n.fake.Close() }

// LSP servers are host processes (pooled, spawned at build time); a
// sandboxed run cannot reach them (design §10.2 M-8), so the tool refuses
// up front instead of silently diagnosing host files.
func TestLSPToolSandboxGate(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	tool, ok := reg.Get("LSP")
	if !ok {
		t.Fatal("LSP tool missing")
	}

	sbx, err := executor.NewSandbox(executor.SandboxConfig{Endpoint: fakesandbox.Addr(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sbx.Close() }()

	t.Setenv("WOCA_LSP_SERVER", "gopls")
	ctx := runexec.WithRunExecutor(nilCtx(), sbx)
	res, err := tool.Invoke(ctx, map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "LSP is not available during sandboxed runs") {
		t.Fatalf("sandboxed LSP = %q", res.Content)
	}

	// Local ctx: the gate stays open (the call proceeds to the pool).
	res, err = tool.Invoke(nilCtx(), map[string]any{"path": "a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "sandboxed runs") {
		t.Fatalf("local LSP wrongly gated: %q", res.Content)
	}
}

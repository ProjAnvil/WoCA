package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeTransport scripts responses per method and records notifications.
type fakeTransport struct {
	responses map[string]any
	notified  []string
	nextID    atomic.Int64
}

func (f *fakeTransport) Call(ctx context.Context, method string, params, result any) error {
	resp, ok := f.responses[method]
	if !ok {
		return errNoScript
	}
	data, _ := json.Marshal(resp)
	return json.Unmarshal(data, result)
}

func (f *fakeTransport) Notify(ctx context.Context, method string, params any) error {
	f.notified = append(f.notified, method)
	return nil
}

func (f *fakeTransport) Close() error { return nil }

var errNoScript = &scriptError{}

type scriptError struct{}

func (*scriptError) Error() string { return "no scripted response" }

func TestLSPDiagnosticsViaFakeTransport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	os.WriteFile(path, []byte("package main\nfunc main() {}\n"), 0o644)

	tr := &fakeTransport{responses: map[string]any{
		"initialize": map[string]any{"capabilities": map[string]any{}},
		"textDocument/diagnostic": map[string]any{
			"kind": "full",
			"items": []map[string]any{
				{
					"severity": 1,
					"range": map[string]any{
						"start": map[string]any{"line": 2, "character": 5},
						"end":   map[string]any{"line": 2, "character": 9},
					},
					"message": "undefined: foo",
					"source":  "gopls",
				},
			},
		},
	}}
	out, err := lspDiagnostics(context.Background(), tr, dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Error 3:6 — undefined: foo (gopls)") {
		t.Fatalf("diagnostics = %q", out)
	}
	if len(tr.notified) < 2 || tr.notified[0] != "initialized" || tr.notified[1] != "textDocument/didOpen" {
		t.Fatalf("notifications = %v", tr.notified)
	}
}

func TestLSPDiagnosticsEmpty(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	os.WriteFile(path, []byte("package a\n"), 0o644)
	tr := &fakeTransport{responses: map[string]any{
		"initialize":              map[string]any{},
		"textDocument/diagnostic": map[string]any{"kind": "full", "items": []map[string]any{}},
	}}
	out, err := lspDiagnostics(context.Background(), tr, dir, path)
	if err != nil {
		t.Fatal(err)
	}
	if out != "(no diagnostics)" {
		t.Fatalf("empty = %q", out)
	}
}

// fakeServerOverPipe serves LSP over a real framed stdio stream (a net.Pipe
// wrapped as files would be overkill; instead we drive startLSP with a real
// subprocess: a tiny LSP server implemented as a shell of Go run inline).
func TestLSPStdioTransportEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain unavailable")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "x.go")
	os.WriteFile(path, []byte("package x\n"), 0o644)

	// The fake server: reads framed requests, answers initialize and
	// diagnostics with one canned item.
	serverSrc := `package main
import (
	"bufio"; "encoding/json"; "fmt"; "io"; "os"; "strings"
)
type msg struct{ ID *int64 ` + "`json:\"id\"`" + `; Method string ` + "`json:\"method\"`" + ` }
func write(v any) { b, _ := json.Marshal(v); fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(b), b) }
func main() {
	r := bufio.NewReader(os.Stdin)
	var n int
	for {
		line, err := r.ReadString('\n')
		if err != nil { return }
		if strings.HasPrefix(line, "Content-Length:") {
			fmt.Sscanf(line, "Content-Length: %d", &n)
		} else if line == "\r\n" || line == "\n" {
			body := make([]byte, n)
			if _, err := io.ReadFull(r, body); err != nil { return }
			var m msg
			if json.Unmarshal(body, &m) != nil || m.ID == nil { continue }
			var result any
			switch m.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{}}
			case "textDocument/diagnostic":
				result = map[string]any{"kind": "full", "items": []map[string]any{{
					"severity": 2, "range": map[string]any{
						"start": map[string]any{"line": 0, "character": 0},
						"end":   map[string]any{"line": 0, "character": 1}},
					"message": "stub warning", "source": "fake",
				}}}
			default:
				result = map[string]any{}
			}
			write(map[string]any{"jsonrpc": "2.0", "id": *m.ID, "result": result})
		}
	}
}
`
	serverDir := t.TempDir()
	serverPath := filepath.Join(serverDir, "fakeserver.go")
	os.WriteFile(serverPath, []byte(serverSrc), 0o644)
	bin := filepath.Join(serverDir, "fakeserver")
	if out, err := exec.Command("go", "build", "-o", bin, serverPath).CombinedOutput(); err != nil {
		t.Fatalf("build fake server: %v\n%s", err, out)
	}

	tr, err := startLSP([]string{bin})
	if err != nil {
		t.Fatal(err)
	}
	out, err := lspDiagnostics(context.Background(), tr, dir, path)
	_ = tr.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Warning 1:1 — stub warning (fake)") {
		t.Fatalf("end-to-end diagnostics = %q", out)
	}
}

func TestLSPToolUnconfigured(t *testing.T) {
	t.Setenv("WOCA_LSP_SERVER", "")
	reg := NewRegistry(t.TempDir())
	tool, ok := reg.Get("LSP")
	if !ok {
		t.Fatal("LSP tool not registered")
	}
	res, err := tool.Invoke(context.Background(), map[string]any{"path": "x.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "no LSP server configured") {
		t.Fatalf("unconfigured = %q", res.Content)
	}
}

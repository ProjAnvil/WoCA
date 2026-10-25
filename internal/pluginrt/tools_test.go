package pluginrt

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	coretools "github.com/projanvil/langchain-golang/core/tools"
	"github.com/rs/zerolog"
)

func toolByName(t *testing.T, tools []coretools.Tool, name string) coretools.Tool {
	t.Helper()
	for _, tl := range tools {
		if tl.Name() == name {
			return tl
		}
	}
	t.Fatalf("tool %s missing", name)
	return nil
}

// fakePinch records requests and answers with canned payloads; failPaths make
// individual endpoints return HTTP 500.
type fakePinch struct {
	mu     sync.Mutex
	paths  []string
	quests []string
	fail   map[string]bool
}

func (f *fakePinch) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.paths = append(f.paths, r.URL.Path)
	f.quests = append(f.quests, r.URL.RawQuery)
	fail := f.fail[r.URL.Path]
	f.mu.Unlock()
	if fail {
		http.Error(w, `{"code":"err","error":"endpoint exploding"}`, http.StatusInternalServerError)
		return
	}
	switch r.URL.Path {
	case "/health":
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	case "/navigate":
		_, _ = w.Write([]byte(`{"tabId":"t9","title":"Example","url":"https://example.com"}`))
	case "/text":
		_, _ = w.Write([]byte(`{"url":"https://example.com","title":"Example","text":"page body","textLength":9,"truncated":true}`))
	case "/snapshot":
		_, _ = w.Write([]byte("[e5] link \"More\"\n[e6] button \"Go\"\n"))
	case "/action":
		_, _ = w.Write([]byte(`{"ok":true}`))
	default:
		http.Error(w, `{"code":"not_found","error":"nope"}`, http.StatusNotFound)
	}
}

func (f *fakePinch) lastQuery(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range slices.Backward(f.paths) {
		if v == path {
			return f.quests[i]
		}
	}
	return ""
}

// adoptedBrowser builds a BrowserRuntime whose client adopts a fake pinchtab
// server (config port -> test server), so tools run against canned responses.
func adoptedBrowser(t *testing.T, fake *fakePinch) *BrowserRuntime {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	root := t.TempDir()
	b := NewBrowserRuntime(root, zerolog.Nop())
	t.Cleanup(b.Stop)
	port := strconv.Itoa(srv.Listener.Addr().(*net.TCPAddr).Port)
	writeBrowserConfig(t, root, port, "tooltok")
	if err := os.MkdirAll(filepath.Dir(b.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	return b
}

func invoke(t *testing.T, tl coretools.Tool, args map[string]any) (string, error) {
	t.Helper()
	res, err := tl.Invoke(t.Context(), args)
	return res.Content, err
}

func TestBrowserNavigateTool(t *testing.T) {
	fake := &fakePinch{}
	b := adoptedBrowser(t, fake)
	tools := browserTools(b)
	nav := toolByName(t, tools, "BrowserNavigate")
	ctx := t.Context()

	if _, err := nav.Invoke(ctx, map[string]any{"url": ""}); err == nil ||
		!strings.Contains(err.Error(), "url is required") {
		t.Fatalf("missing url: %v", err)
	}

	out, err := invoke(t, nav, map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("navigate: %v", err)
	}
	for _, want := range []string{"Navigated: Example", "URL: https://example.com", "page text (truncated)", "page body", "interactive elements", "[e5]"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if q := fake.lastQuery("/text"); !strings.Contains(q, "maxChars=6000") {
		t.Fatalf("default max_chars not applied: %q", q)
	}

	// An explicit max_chars is forwarded.
	if _, err := invoke(t, nav, map[string]any{"url": "https://example.com", "max_chars": 42}); err != nil {
		t.Fatal(err)
	}
	if q := fake.lastQuery("/text"); !strings.Contains(q, "maxChars=42") {
		t.Fatalf("explicit max_chars not forwarded: %q", q)
	}

	// Degraded fetches: text and snapshot failing must not fail the tool.
	fake.fail = map[string]bool{"/text": true, "/snapshot": true}
	out, err = invoke(t, nav, map[string]any{"url": "https://example.com"})
	if err != nil {
		t.Fatalf("navigate with degraded extras: %v", err)
	}
	if strings.Contains(out, "page text") || strings.Contains(out, "interactive") {
		t.Fatalf("degraded sections leaked: %s", out)
	}
}

func TestBrowserNavigateToolAPIError(t *testing.T) {
	fake := &fakePinch{fail: map[string]bool{"/navigate": true}}
	b := adoptedBrowser(t, fake)
	nav := toolByName(t, browserTools(b), "BrowserNavigate")
	_, err := nav.Invoke(t.Context(), map[string]any{"url": "https://example.com"})
	if err == nil || !strings.Contains(err.Error(), "endpoint exploding") {
		t.Fatalf("navigate api error: %v", err)
	}
}

func TestBrowserSnapshotTool(t *testing.T) {
	fake := &fakePinch{}
	b := adoptedBrowser(t, fake)
	snap := toolByName(t, browserTools(b), "BrowserSnapshot")

	out, err := invoke(t, snap, map[string]any{})
	if err != nil || out != "[e5] link \"More\"\n[e6] button \"Go\"" {
		t.Fatalf("snapshot: %q %v", out, err)
	}

	fake.fail = map[string]bool{"/snapshot": true}
	if _, err := invoke(t, snap, map[string]any{}); err == nil {
		t.Fatal("snapshot failure must surface")
	}
}

func TestBrowserTextTool(t *testing.T) {
	fake := &fakePinch{}
	b := adoptedBrowser(t, fake)
	text := toolByName(t, browserTools(b), "BrowserText")

	out, err := invoke(t, text, map[string]any{})
	if err != nil {
		t.Fatalf("text: %v", err)
	}
	for _, want := range []string{"Example", "https://example.com", "page body", "(text truncated)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	if q := fake.lastQuery("/text"); !strings.Contains(q, "maxChars=12000") {
		t.Fatalf("default max_chars: %q", q)
	}

	if _, err := invoke(t, text, map[string]any{"max_chars": 7, "raw": true}); err != nil {
		t.Fatal(err)
	}
	if q := fake.lastQuery("/text"); !strings.Contains(q, "maxChars=7") || !strings.Contains(q, "mode=raw") {
		t.Fatalf("raw text params: %q", q)
	}

	fake.fail = map[string]bool{"/text": true}
	if _, err := invoke(t, text, map[string]any{}); err == nil {
		t.Fatal("text failure must surface")
	}
}

func TestBrowserActionTool(t *testing.T) {
	fake := &fakePinch{}
	b := adoptedBrowser(t, fake)
	action := toolByName(t, browserTools(b), "BrowserAction")
	ctx := t.Context()

	for _, bad := range []map[string]any{
		{"kind": "click"},
		{"kind": "fill", "ref": "e5"},
		{"kind": "fill", "text": "hi"},
		{"kind": "press"},
		{"kind": "teleport", "ref": "e5"},
	} {
		if _, err := action.Invoke(ctx, bad); err == nil {
			t.Fatalf("invalid action %v must fail", bad)
		}
	}

	out, err := invoke(t, action, map[string]any{"kind": "click", "ref": "e5"})
	if err != nil || !strings.HasPrefix(out, "click ok") || !strings.Contains(out, "[e5]") {
		t.Fatalf("click: %q %v", out, err)
	}
	if _, err := invoke(t, action, map[string]any{"kind": "fill", "ref": "e5", "text": "hello"}); err != nil {
		t.Fatalf("fill: %v", err)
	}
	if _, err := invoke(t, action, map[string]any{"kind": "press", "key": "Enter"}); err != nil {
		t.Fatalf("press: %v", err)
	}

	// Post-action snapshot failing degrades to the bare confirmation.
	fake.fail = map[string]bool{"/snapshot": true}
	out, err = invoke(t, action, map[string]any{"kind": "click", "ref": "e5"})
	if err != nil || out != "click ok" {
		t.Fatalf("click without snapshot: %q %v", out, err)
	}
}

func TestBrowserToolsRequireInstall(t *testing.T) {
	// Without a binary the tools surface the not-installed sentinel.
	b := NewBrowserRuntime(t.TempDir(), zerolog.Nop())
	nav := toolByName(t, browserTools(b), "BrowserNavigate")
	_, err := nav.Invoke(t.Context(), map[string]any{"url": "https://example.com"})
	if err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("err = %v, want not-installed guidance", err)
	}
}

// fakePDFEnv provisions a ready pdf environment driven by fakeVenvPython.
func fakePDFEnv(t *testing.T) *PythonRuntime {
	t.Helper()
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	stampPythonEnv(t, py, "pdf", nil)
	return py
}

func TestPdfReadTool(t *testing.T) {
	py := fakePDFEnv(t)
	read := toolByName(t, pdfTools(py, t.TempDir()), "PdfRead")
	ctx := t.Context()

	if _, err := read.Invoke(ctx, map[string]any{"file": ""}); err == nil ||
		!strings.Contains(err.Error(), "file is required") {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := read.Invoke(ctx, map[string]any{"file": "a.pdf", "op": "shred"}); err == nil ||
		!strings.Contains(err.Error(), "op must be text or metadata") {
		t.Fatalf("bad op: %v", err)
	}

	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	t.Setenv("FAKE_PYTHON_MODE", "ok")
	out, err := invoke(t, read, map[string]any{
		"file": "doc.pdf", "pages": "1-3", "max_chars": 500, "password": "pw",
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"doc.pdf — 3 pages (2 selected)", "hello pdf", "(text truncated)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	raw, _ := os.ReadFile(argsFile)
	got := strings.Fields(string(raw))
	want := []string{"text", "doc.pdf", "--password", "pw", "--pages", "1-3", "--max-chars", "500"}
	if len(got) < len(want) {
		t.Fatalf("interpreter args = %q, want at least %q", got, want)
	}
	tail := got[len(got)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("interpreter args = %q, want tail %q", got, want)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "meta")
	out, err = invoke(t, read, map[string]any{"file": "doc.pdf", "op": "metadata"})
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	for _, want := range []string{"doc.pdf — 9 pages (encrypted)", "  Author: Me", "  Title: Doc"} {
		if !strings.Contains(out, want) {
			t.Fatalf("metadata output missing %q:\n%s", want, out)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "errjson")
	if _, err := invoke(t, read, map[string]any{"file": "doc.pdf"}); err == nil ||
		!strings.Contains(err.Error(), "boom said pdf.py") {
		t.Fatalf("error payload: %v", err)
	}

	t.Setenv("FAKE_PYTHON_MODE", "invalid")
	_, err = invoke(t, read, map[string]any{"file": "doc.pdf"})
	if err == nil || !strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("invalid JSON: %v", err)
	}

	t.Setenv("FAKE_PYTHON_MODE", "fail")
	if _, err := invoke(t, read, map[string]any{"file": "doc.pdf"}); err == nil ||
		!strings.Contains(err.Error(), "pdf.py: fatal") {
		t.Fatalf("script failure: %v", err)
	}
}

func TestPdfWriteTool(t *testing.T) {
	py := fakePDFEnv(t)
	write := toolByName(t, pdfTools(py, t.TempDir()), "PdfWrite")
	ctx := t.Context()

	for _, bad := range []map[string]any{
		{"op": "merge", "files": []any{"a.pdf"}},
		{"op": "merge", "output": "o.pdf"},
		{"op": "split", "files": []any{}},
		{"op": "split", "files": []any{"a.pdf", "b.pdf"}, "out_dir": "d"},
		{"op": "rotate", "files": []any{"a.pdf"}, "output": "o.pdf"},
		{"op": "encrypt", "files": []any{"a.pdf"}, "output": "o.pdf"},
		{"op": "shred"},
	} {
		if _, err := write.Invoke(ctx, bad); err == nil {
			t.Fatalf("invalid write args %v must fail", bad)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "write")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	out, err := invoke(t, write, map[string]any{
		"op": "merge", "files": []any{"a.pdf", "b.pdf"}, "output": "merged.pdf", "password": "pw",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if out != "merge ok: merged.pdf (5 pages)\n  a.pdf\n  b.pdf" {
		t.Fatalf("merge output = %q", out)
	}
	raw, _ := os.ReadFile(argsFile)
	got := strings.Fields(string(raw))
	want := []string{"merge", "merged.pdf", "a.pdf", "b.pdf", "--password", "pw"}
	if len(got) < len(want)+1 {
		t.Fatalf("merge interpreter args = %q, want tail %q", got, want)
	}
	tail := got[len(got)-len(want):]
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("merge interpreter args = %q, want tail %q", got, want)
		}
	}

	for _, op := range []string{"split", "rotate", "encrypt", "decrypt"} {
		args := map[string]any{"op": op, "files": []any{"a.pdf"}, "output": "o.pdf", "out_dir": "d", "angle": 90, "password": "pw"}
		if _, err := write.Invoke(ctx, args); err != nil {
			t.Fatalf("%s: %v", op, err)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "errjson")
	if _, err := invoke(t, write, map[string]any{"op": "merge", "files": []any{"a.pdf", "b.pdf"}, "output": "o.pdf"}); err == nil ||
		!strings.Contains(err.Error(), "boom said pdf.py") {
		t.Fatalf("error payload: %v", err)
	}
}

func TestPdfToolsSchemaDriftDegrades(t *testing.T) {
	// ok=true with no text/pages fields must render empty, not panic.
	py := fakePDFEnv(t)
	t.Setenv("FAKE_PYTHON_MODE", "ok")

	// Swap the interpreter for one emitting a bare {"ok":true}.
	bare := "#!/bin/sh\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(py.venvPython("pdf"), []byte(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, pdfTools(py, t.TempDir()), "PdfRead")
	out, err := invoke(t, read, map[string]any{"file": "doc.pdf"})
	if err != nil {
		t.Fatalf("schema drift: %v", err)
	}
	if !strings.Contains(out, "doc.pdf — ") || strings.Contains(out, "panic") {
		t.Fatalf("schema drift output = %q", out)
	}
}

func TestPythonToolsDispatch(t *testing.T) {
	py := fakePDFEnv(t)
	if got := pythonTools(Builtin{Name: "unimplemented", Kind: "python"}, py, ""); got != nil {
		t.Fatalf("unknown python plugin tools = %v", got)
	}
	tools := pythonTools(mustBuiltin(t, "pdf"), py, "")
	if len(tools) != 2 || tools[0].Name() != "PdfRead" || tools[1].Name() != "PdfWrite" {
		t.Fatalf("pdf tools = %v", tools)
	}
	office := pythonTools(mustBuiltin(t, "office"), fakeOfficeEnv(t), "")
	if len(office) != 4 || office[0].Name() != "OfficeRead" || office[3].Name() != "PptxWrite" {
		t.Fatalf("office tools = %v", office)
	}
}

func TestBrowserActionToolAPIError(t *testing.T) {
	fake := &fakePinch{fail: map[string]bool{"/action": true}}
	b := adoptedBrowser(t, fake)
	action := toolByName(t, browserTools(b), "BrowserAction")
	_, err := action.Invoke(t.Context(), map[string]any{"kind": "click", "ref": "e5"})
	if err == nil || !strings.Contains(err.Error(), "endpoint exploding") {
		t.Fatalf("action api error: %v", err)
	}
}

func TestPdfToolsRunFailureWithoutDiagnostics(t *testing.T) {
	// A silent exit (no stdout, no stderr) falls back to the exec error.
	py := fakePDFEnv(t)
	t.Setenv("FAKE_PYTHON_MODE", "silent")
	tools := pdfTools(py, t.TempDir())
	read := toolByName(t, tools, "PdfRead")
	if _, err := read.Invoke(t.Context(), map[string]any{"file": "doc.pdf"}); err == nil ||
		!strings.Contains(err.Error(), "run pdf.py:") {
		t.Fatalf("silent failure: %v", err)
	}
	write := toolByName(t, tools, "PdfWrite")
	args := map[string]any{"op": "merge", "files": []any{"a.pdf", "b.pdf"}, "output": "o.pdf"}
	if _, err := write.Invoke(t.Context(), args); err == nil ||
		!strings.Contains(err.Error(), "run pdf.py:") {
		t.Fatalf("silent write failure: %v", err)
	}
}

// fakeOfficeEnv provisions a ready office environment driven by fakeVenvPython.
func fakeOfficeEnv(t *testing.T) *PythonRuntime {
	t.Helper()
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	stampPythonEnv(t, py, "office", nil)
	return py
}

// lastInterpreterArgs returns the trailing n argv lines recorded by
// FAKE_PYTHON_ARGS_FILE (one line per argument, so spec JSON stays intact).
func lastInterpreterArgs(t *testing.T, path string, n int) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("interpreter args not recorded: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < n {
		t.Fatalf("interpreter args = %q, want %d trailing lines", lines, n)
	}
	return lines[len(lines)-n:]
}

func TestOfficeReadTool(t *testing.T) {
	py := fakeOfficeEnv(t)
	read := toolByName(t, officeTools(py, t.TempDir()), "OfficeRead")
	ctx := t.Context()

	if _, err := read.Invoke(ctx, map[string]any{"file": ""}); err == nil ||
		!strings.Contains(err.Error(), "file is required") {
		t.Fatalf("missing file: %v", err)
	}

	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	t.Setenv("FAKE_PYTHON_MODE", "officeread")
	out, err := invoke(t, read, map[string]any{"file": "notes.docx", "max_chars": 800})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{"notes.docx (docx)", "# Hello", "- alpha", "(text truncated)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
	got := lastInterpreterArgs(t, argsFile, 4)
	want := []string{"read", "notes.docx", "--max-chars", "800"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("interpreter args = %q, want tail %q", got, want)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "errjson")
	if _, err := invoke(t, read, map[string]any{"file": "notes.docx"}); err == nil ||
		!strings.Contains(err.Error(), "boom") {
		t.Fatalf("error payload: %v", err)
	}
	t.Setenv("FAKE_PYTHON_MODE", "invalid")
	if _, err := invoke(t, read, map[string]any{"file": "notes.docx"}); err == nil ||
		!strings.Contains(err.Error(), "invalid JSON") {
		t.Fatalf("invalid JSON: %v", err)
	}
}

func TestOfficeReadSchemaDriftDegrades(t *testing.T) {
	// ok=true with no format/markdown fields must render an empty body.
	py := fakeOfficeEnv(t)
	bare := "#!/bin/sh\necho '{\"ok\":true}'\n"
	if err := os.WriteFile(py.venvPython("office"), []byte(bare), 0o755); err != nil {
		t.Fatal(err)
	}
	read := toolByName(t, officeTools(py, t.TempDir()), "OfficeRead")
	out, err := invoke(t, read, map[string]any{"file": "notes.docx"})
	if err != nil {
		t.Fatalf("schema drift: %v", err)
	}
	if !strings.Contains(out, "notes.docx") || strings.Contains(out, "panic") {
		t.Fatalf("schema drift output = %q", out)
	}
}

func TestDocxWriteTool(t *testing.T) {
	py := fakeOfficeEnv(t)
	write := toolByName(t, officeTools(py, t.TempDir()), "DocxWrite")
	ctx := t.Context()

	for _, bad := range []map[string]any{
		{},
		{"path": "r.docx"},
		{"path": "r.docx", "blocks": []any{map[string]any{"type": "heading"}}},
		{"path": "r.docx", "blocks": []any{map[string]any{"type": "paragraph"}}},
		{"path": "r.docx", "blocks": []any{map[string]any{"type": "bullets"}}},
		{"path": "r.docx", "blocks": []any{map[string]any{"type": "table", "rows": []any{[]any{"1"}}}}},
		{"path": "r.docx", "blocks": []any{map[string]any{"type": "teleport"}}},
	} {
		if _, err := write.Invoke(ctx, bad); err == nil {
			t.Fatalf("invalid docx args %v must fail", bad)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "officedocx")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	out, err := invoke(t, write, map[string]any{
		"path":  "out/report.docx",
		"title": "Quarterly report",
		"blocks": []any{
			map[string]any{"type": "heading", "text": "Chapter 1", "level": 1},
			map[string]any{"type": "bullets", "items": []any{"a", "b"}},
			map[string]any{"type": "table", "header": []any{"A", "B"}, "rows": []any{[]any{"1", "2"}}},
		},
	})
	if err != nil {
		t.Fatalf("docx write: %v", err)
	}
	if out != "docx written: out/report.docx (3 blocks, 4321 bytes)" {
		t.Fatalf("docx write output = %q", out)
	}
	got := lastInterpreterArgs(t, argsFile, 5)
	if got[0] != "docx-write" || got[1] != "--output" || got[2] != "out/report.docx" || got[3] != "--spec" {
		t.Fatalf("docx-write args = %q", got)
	}
	spec := got[4]
	for _, want := range []string{`"title":"Quarterly report"`, `"text":"Chapter 1"`, `"type":"bullets"`, `"header":["A","B"]`} {
		if !strings.Contains(spec, want) {
			t.Fatalf("spec missing %s: %s", want, spec)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "errjson")
	if _, err := invoke(t, write, map[string]any{
		"path": "r.docx", "title": "t",
	}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error payload: %v", err)
	}
}

func TestXlsxWriteTool(t *testing.T) {
	py := fakeOfficeEnv(t)
	write := toolByName(t, officeTools(py, t.TempDir()), "XlsxWrite")
	ctx := t.Context()

	for _, bad := range []map[string]any{
		{"path": "x.xlsx"},
		{"sheets": []any{map[string]any{"rows": []any{}}}},
		{"sheets": []any{map[string]any{"rows": []any{[]any{"a"}}}}},
	} {
		if _, err := write.Invoke(ctx, bad); err == nil {
			t.Fatalf("invalid xlsx args %v must fail", bad)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "officexlsx")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	out, err := invoke(t, write, map[string]any{
		"path":   "data.xlsx",
		"sheets": []any{map[string]any{"name": "Data", "rows": []any{[]any{"Name", "Count"}, []any{"apple", "3"}}, "header_row": true}},
	})
	if err != nil {
		t.Fatalf("xlsx write: %v", err)
	}
	if out != "xlsx written: data.xlsx (2 sheets, 9 rows, 4321 bytes)" {
		t.Fatalf("xlsx write output = %q", out)
	}
	got := lastInterpreterArgs(t, argsFile, 5)
	if got[0] != "xlsx-write" || got[1] != "--output" || got[2] != "data.xlsx" || got[3] != "--spec" {
		t.Fatalf("xlsx-write args = %q", got)
	}
	for _, want := range []string{`"name":"Data"`, `"header_row":true`, `"3"`} {
		if !strings.Contains(got[4], want) {
			t.Fatalf("spec missing %s: %s", want, got[4])
		}
	}
}

func TestPptxWriteTool(t *testing.T) {
	py := fakeOfficeEnv(t)
	write := toolByName(t, officeTools(py, t.TempDir()), "PptxWrite")
	ctx := t.Context()

	for _, bad := range []map[string]any{
		{},
		{"path": "p.pptx"},
		{"path": "p.pptx", "slides": []any{map[string]any{}}},
	} {
		if _, err := write.Invoke(ctx, bad); err == nil {
			t.Fatalf("invalid pptx args %v must fail", bad)
		}
	}

	t.Setenv("FAKE_PYTHON_MODE", "officepptx")
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	out, err := invoke(t, write, map[string]any{
		"path":  "deck.pptx",
		"title": "Q3 Review",
		"slides": []any{
			map[string]any{"title": "A", "bullets": []any{map[string]any{"text": "one"}, map[string]any{"text": "two", "level": 1}}, "notes": "remember"},
		},
	})
	if err != nil {
		t.Fatalf("pptx write: %v", err)
	}
	if out != "pptx written: deck.pptx (4 slides, 4321 bytes)" {
		t.Fatalf("pptx write output = %q", out)
	}
	got := lastInterpreterArgs(t, argsFile, 5)
	if got[0] != "pptx-write" || got[1] != "--output" || got[2] != "deck.pptx" || got[3] != "--spec" {
		t.Fatalf("pptx-write args = %q", got)
	}
	for _, want := range []string{`"title":"Q3 Review"`, `"level":1`, `"notes":"remember"`} {
		if !strings.Contains(got[4], want) {
			t.Fatalf("spec missing %s: %s", want, got[4])
		}
	}
}

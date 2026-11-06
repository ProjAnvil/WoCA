package mcp

import (
	"encoding/json"
	"testing"
)

// mustParse decodes raw as one mcpServers entry, failing the test on error.
func mustParse(t *testing.T, name, raw string) ServerEntry {
	t.Helper()
	e, err := ParseServerEntry(name, json.RawMessage(raw))
	if err != nil {
		t.Fatalf("ParseServerEntry(%s): %v", name, err)
	}
	return e
}

func TestParseServerEntryHTTP(t *testing.T) {
	e := mustParse(t, "deepwiki", `{"type":"http","url":"https://mcp.deepwiki.com/mcp"}`)
	if e.Type != "http" || e.URL != "https://mcp.deepwiki.com/mcp" {
		t.Fatalf("http entry = %+v", e)
	}
	if got := e.Spec(); got != "deepwiki=https://mcp.deepwiki.com/mcp" {
		t.Fatalf("http spec = %q", got)
	}
	sse := mustParse(t, "stream", `{"type":"sse","url":"https://example.com/sse"}`)
	if sse.Type != "sse" || sse.URL == "" {
		t.Fatalf("sse entry = %+v", sse)
	}
}

func TestParseServerEntryStdio(t *testing.T) {
	// type omitted + command → stdio; the display command quotes args and
	// env is only flagged via HasEnv.
	e := mustParse(t, "local", `{"command":"uvx","args":["mcp-server","--flag with space"],"env":{"KEY":"v"}}`)
	if e.Type != "stdio" || !e.HasEnv {
		t.Fatalf("stdio entry = %+v", e)
	}
	if got := e.Command; got != "uvx mcp-server '--flag with space'" {
		t.Fatalf("display command = %q", got)
	}
	if e2 := mustParse(t, "x", `{"type":"stdio","command":"run"}`); e2.Type != "stdio" {
		t.Fatalf("explicit stdio entry = %+v", e2)
	}
}

func TestSpecRendering(t *testing.T) {
	if got := (ServerEntry{Name: "w", URL: "https://x/y"}).Spec(); got != "w=https://x/y" {
		t.Fatalf("url spec = %q", got)
	}
	s := ServerEntry{
		Name:   "l",
		Inline: "run",
		Args:   []string{"--x"},
		Env:    map[string]string{"A": "1", "B": "two words"},
	}
	if got := s.Spec(); got != "l=A=1 B='two words' run --x" {
		t.Fatalf("stdio spec = %q", got)
	}
	// A stored shell line with no args runs as-is.
	s2 := ServerEntry{Name: "l2", Inline: "echo hi && exit"}
	if got := s2.Spec(); got != "l2=echo hi && exit" {
		t.Fatalf("inline spec = %q", got)
	}
	// A parsed entry renders env as a sorted K=V prefix before the command.
	p := mustParse(t, "local", `{"command":"uvx","args":["srv"],"env":{"K":"v"}}`)
	if got := p.Spec(); got != "local=K=v uvx srv" {
		t.Fatalf("parsed stdio spec = %q", got)
	}
}

func TestParseServerEntryErrors(t *testing.T) {
	if _, err := ParseServerEntry("badhttp", json.RawMessage(`{"type":"http"}`)); err == nil || err.Error() != "server badhttp: missing url" {
		t.Fatalf("missing url err = %v", err)
	}
	if _, err := ParseServerEntry("weird", json.RawMessage(`{"type":"carrier-pigeon"}`)); err == nil || err.Error() != "server weird: unsupported shape" {
		t.Fatalf("unsupported shape err = %v", err)
	}
	if _, err := ParseServerEntry("garbage", json.RawMessage(`[1,2]`)); err == nil {
		t.Fatal("expected decode error")
	}
}

func TestShellWordQuoting(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"plain", "plain"},
		{"", "''"},
		{"has space", "'has space'"},
		{"it's", `'it'\''s'`},
		{"~home", "'~home'"},
	} {
		if got := shellWord(c.in); got != c.want {
			t.Errorf("shellWord(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := shellQuote("cmd", []string{"a b", "c"}); got != "cmd 'a b' c" {
		t.Errorf("shellQuote = %q", got)
	}
}

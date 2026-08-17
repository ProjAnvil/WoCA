package claudeenv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeClaudeJSONRaw(t *testing.T, home, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestServersShapes(t *testing.T) {
	home := fakeHome(t)
	// Missing file: empty.
	if got, err := Servers(); err != nil || len(got) != 0 {
		t.Fatalf("missing = %+v %v", got, err)
	}
	writeClaudeJSONRaw(t, home, "not json")
	if _, err := Servers(); err == nil {
		t.Fatal("expected decode error")
	}
	writeClaudeJSONRaw(t, home, `{"mcpServers":{
		"web":{"type":"http","url":"https://example.com/mcp"},
		"stream":{"type":"sse","url":"https://example.com/sse"},
		"local":{"command":"uvx","args":["mcp-server","--flag with space"],"env":{"KEY":"v"}},
		"badhttp":{"type":"http"},
		"weird":{"type":"carrier-pigeon"},
		"garbage":[1,2]
	}}`)
	got, err := Servers()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]MCPServer{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("servers = %+v", got)
	}
	if byName["web"].URL == "" || byName["web"].Type != "http" {
		t.Fatalf("web = %+v", byName["web"])
	}
	if byName["local"].Type != "stdio" || !byName["local"].HasEnv {
		t.Fatalf("local = %+v", byName["local"])
	}
	if !strings.Contains(byName["local"].Command, "'--flag with space'") {
		t.Fatalf("command quoting = %q", byName["local"].Command)
	}
}

func TestSpecRendering(t *testing.T) {
	if got := (MCPServer{Name: "w", URL: "https://x/y"}).Spec(); got != "w=https://x/y" {
		t.Fatalf("url spec = %q", got)
	}
	s := MCPServer{
		Name:   "l",
		inline: "run",
		args:   []string{"--x"},
		env:    map[string]string{"A": "1", "B": "two words"},
	}
	if got := s.Spec(); got != "l=A=1 B='two words' run --x" {
		t.Fatalf("stdio spec = %q", got)
	}
	// A stored shell line with no args runs as-is.
	s2 := MCPServer{Name: "l2", inline: "echo hi && exit"}
	if got := s2.Spec(); got != "l2=echo hi && exit" {
		t.Fatalf("inline spec = %q", got)
	}
}

func TestMCPConfigRoundTripShapes(t *testing.T) {
	home := fakeHome(t)
	root := filepath.Join(home, ".woca")
	servers := []MCPServer{
		{Name: "web", Type: "http", URL: "https://example.com/mcp"},
		{Name: "local", Type: "stdio", inline: "uvx", args: []string{"srv"}, env: map[string]string{"K": "v"}},
	}
	if err := WriteMCPConfig(root, servers); err != nil {
		t.Fatal(err)
	}
	specs, err := LoadMCPConfig(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("specs = %v", specs)
	}
	if specs[0] != "local=K=v uvx srv" || specs[1] != "web=https://example.com/mcp" {
		t.Fatalf("specs = %v", specs)
	}

	// Missing config: empty.
	if got, err := LoadMCPConfig(filepath.Join(home, "empty")); err != nil || len(got) != 0 {
		t.Fatalf("missing config = %v %v", got, err)
	}
	// Malformed config: decode error.
	badRoot := filepath.Join(home, "bad")
	if err := os.MkdirAll(badRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRoot, "mcp.json"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMCPConfig(badRoot); err == nil {
		t.Fatal("expected decode error")
	}
	// An unwritable root fails the write.
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteMCPConfig(filepath.Join(blocker, "w"), servers); err == nil {
		t.Fatal("expected write failure")
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
	// mustJSON marshals without error.
	if got := string(mustJSON(map[string]int{"a": 1})); got != `{"a":1}` {
		t.Errorf("mustJSON = %s", got)
	}
}

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// execCmd runs a root command with args, returning the combined failure (nil
// on success).
func execCmd(t *testing.T, args ...string) error {
	t.Helper()
	root := newRootCmd()
	root.SetOut(os.Stderr)
	root.SetErr(os.Stderr)
	root.SetArgs(args)
	return root.Execute()
}

func TestRootVersionCmd(t *testing.T) {
	if err := execCmd(t, "version"); err != nil {
		t.Fatal(err)
	}
}

func TestRootBadLogLevel(t *testing.T) {
	// The tui command validates the log level before doing anything else.
	if err := execCmd(t, "tui", "--log-level", "bogus"); err == nil {
		t.Fatal("expected log-level error")
	}
	// Same for the default run path via the root command's RunE → run().
	if err := execCmd(t, "--log-level", "nope", "--prompt", "x"); err == nil {
		t.Fatal("expected log-level error from run()")
	}
}

func TestRootHelp(t *testing.T) {
	if err := execCmd(t, "--help"); err != nil {
		t.Fatal(err)
	}
}

func TestLoginMissingEnv(t *testing.T) {
	isolateEnv(t)
	if err := login(t.Context()); err == nil {
		t.Fatal("login without env should fail")
	}
}

func TestLoginDeviceFlow(t *testing.T) {
	home := isolateEnv(t)
	var tokenCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/device", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code":      "dc",
			"user_code":        "ABCD-EFGH",
			"verification_uri": "https://example.com/device",
			"interval":         0,
			"expires_in":       60,
		})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if tokenCalls.Add(1) < 2 {
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "at", "refresh_token": "rt"})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("WOCA_OAUTH_CLIENT_ID", "client")
	t.Setenv("WOCA_OAUTH_DEVICE_ENDPOINT", srv.URL+"/device")
	t.Setenv("WOCA_OAUTH_TOKEN_ENDPOINT", srv.URL+"/token")

	if err := login(t.Context()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".woca", "credentials.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken != "at" {
		t.Fatalf("credentials = %s (%v)", data, err)
	}
}

func TestConfigCmds(t *testing.T) {
	home := isolateEnv(t)
	db := filepath.Join(home, "woca.db")

	if err := execCmd(t, "--db", db, "config", "list"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "--db", db, "config", "get", "provider"); err != nil {
		t.Fatal(err)
	}
	// Secret keys print a masked placeholder.
	if err := execCmd(t, "--db", db, "config", "get", "anthropic-api-key"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "--db", db, "config", "set", "provider", "openai"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "--db", db, "config", "set", "anthropic-api-key", "sk-test"); err != nil {
		t.Fatal(err)
	}
	// Invalid key errors.
	if err := execCmd(t, "--db", db, "config", "set", "no-such-key", "x"); err == nil {
		t.Fatal("expected set error for unknown key")
	}
	// Arg count validation.
	if err := execCmd(t, "--db", db, "config", "get"); err == nil {
		t.Fatal("expected ExactArgs error")
	}
}

func TestConfigCmdsDBNone(t *testing.T) {
	isolateEnv(t)
	for _, args := range [][]string{
		{"--db", "none", "config", "list"},
		{"--db", "none", "config", "get", "provider"},
		{"--db", "none", "config", "set", "provider", "openai"},
	} {
		if err := execCmd(t, args...); err == nil {
			t.Fatalf("expected disabled-db error for %v", args)
		}
	}
}

func TestPluginCmds(t *testing.T) {
	home := isolateEnv(t)

	// Empty plugin root: list prints the hint.
	if err := execCmd(t, "plugin", "list"); err != nil {
		t.Fatal(err)
	}

	// Install from a local directory: a minimal valid plugin has a manifest.
	src := t.TempDir()
	manifest := map[string]any{"name": "demo", "version": "1.0.0", "description": "demo plugin"}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(src, "plugin.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "plugin", "install", src); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "plugin", "list"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "plugin", "remove", "demo"); err != nil {
		t.Fatal(err)
	}
	// An invalid plugin name errors.
	if err := execCmd(t, "plugin", "remove", "../escape"); err == nil {
		t.Fatal("expected invalid-name error")
	}
	// Installing a missing path fails.
	if err := execCmd(t, "plugin", "install", filepath.Join(home, "missing")); err == nil {
		t.Fatal("expected install error")
	}
}

func TestWebCmdsIdle(t *testing.T) {
	isolateEnv(t)
	if err := execCmd(t, "web", "status"); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "web", "stop"); err != nil {
		t.Fatal(err)
	}
}

func TestWebStartSpawnFailure(t *testing.T) {
	// HOME points at a regular file so the daemon data dir cannot be created;
	// start/restart fail before spawning anything.
	home := t.TempDir()
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", blocker)
	if err := execCmd(t, "web", "start"); err == nil {
		t.Fatal("expected start failure with unusable home")
	}
	if err := execCmd(t, "web", "restart"); err == nil {
		t.Fatal("expected restart failure with unusable home")
	}
}

func TestMcpAuthCmd(t *testing.T) {
	home := isolateEnv(t)
	db := filepath.Join(home, "woca.db")
	// Wrong arg count.
	if err := execCmd(t, "mcp", "auth", "only-one"); err == nil {
		t.Fatal("expected ExactArgs error")
	}
	// Disabled database.
	if err := execCmd(t, "--db", "none", "mcp", "auth", "srv", "https://example.invalid"); err == nil {
		t.Fatal("expected disabled-db error")
	}
	// Unreachable endpoint: the OAuth flow fails.
	if err := execCmd(t, "--db", db, "mcp", "auth", "srv", "http://127.0.0.1:1/mcp"); err == nil {
		t.Fatal("expected oauth failure")
	}
}

func TestNewRootCmdPersistentFlags(t *testing.T) {
	root := newRootCmd()
	if root.Name() != "woca" {
		t.Fatalf("root name = %q", root.Name())
	}
	var found int
	for _, name := range []string{"prompt", "model", "provider", "api-key", "base-url", "db", "max-turns", "debug-log"} {
		if f := root.PersistentFlags().Lookup(name); f != nil {
			found++
		}
	}
	if found != 8 {
		t.Fatalf("persistent flags found = %d", found)
	}
}

func TestTuiCmdBuildAgentError(t *testing.T) {
	isolateEnv(t)
	// Valid log level but an invalid Provider: the command wires the
	// console, fails building the agent, and returns before the TUI runs.
	if err := execCmd(t, "tui", "--provider", "nope"); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

func TestPluginListWithContent(t *testing.T) {
	home := isolateEnv(t)
	root := filepath.Join(home, ".woca", "plugins")

	// One broken plugin (invalid manifest) and one with skills and agents.
	broken := filepath.Join(root, "broken")
	if err := os.MkdirAll(broken, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(broken, "plugin.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	good := filepath.Join(root, "good")
	for _, d := range []string{filepath.Join(good, "skills", "s1"), filepath.Join(good, "agents")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(good, "plugin.json"), []byte(`{"name":"good","version":"2.0.0","description":"has things"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "skills", "s1", "SKILL.md"), []byte("---\nname: s1\ndescription: skill\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(good, "agents", "a1.md"), []byte("---\nname: a1\ndescription: agent\n---\nbody"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := execCmd(t, "plugin", "list"); err != nil {
		t.Fatal(err)
	}
}

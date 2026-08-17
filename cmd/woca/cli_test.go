package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
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
	var tokenCalls int32
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
		if atomic.AddInt32(&tokenCalls, 1) < 2 {
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

func TestServeFunc(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := serve(ctx, opts, "127.0.0.1:0", true); err != nil {
		t.Fatalf("serve = %v", err)
	}
}

func TestServeBadLogLevel(t *testing.T) {
	opts := options{logLevel: "bogus"}
	if err := serve(t.Context(), opts, "127.0.0.1:0", true); err == nil {
		t.Fatal("expected log-level error")
	}
}

func TestServeCmdNoBrowser(t *testing.T) {
	// Cover the serveCmd RunE wiring (flag → serve) with an immediate cancel.
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")

	root := newRootCmd()
	root.SetArgs([]string{"serve", "--addr", "127.0.0.1:0", "--no-browser",
		"--provider", "openai", "--model", "m", "--api-key", "k", "--base-url", url,
		"--workdir", t.TempDir(), "--db", filepath.Join(home, "woca.db")})
	// Cancel shortly after start via a timeout baked into the command's
	// context through ExecuteContext.
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	root.SetContext(ctx)
	if err := root.ExecuteContext(ctx); err != nil {
		t.Fatalf("serve cmd = %v", err)
	}
}

func TestOpenBrowserUnsupported(t *testing.T) {
	// On darwin/linux the command starts; use a bad URL scheme-free value —
	// cmd.Start itself rarely fails, so only assert no panic.
	_ = openBrowser("http://127.0.0.1:1/x")
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
	// Valid log level but an invalid provider: the command wires the
	// console, fails building the agent, and returns before the TUI runs.
	if err := execCmd(t, "tui", "--provider", "nope"); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

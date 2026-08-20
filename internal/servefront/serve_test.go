package servefront

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/authn"
)

func TestServeAPIDrivesClosures(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	// An empty claude.json keeps the env apply path clean.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pick a free port, then hand it to Serve().
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- Serve(ctx, opts, addr, true, "") }()

	base := "http://" + addr
	// Serve() answers /api/health from the moment the port is bound; the
	// real handler (and the closures under test) only swaps in once the
	// agent build finishes, so wait for an API route instead.
	healthOK := false
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/api/quota")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthOK = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthOK {
		t.Fatal("server did not come up")
	}

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	put := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Closures wired in Serve()/newConsole: quota, thinking, claude-env,
	// settings rebuilds, plugins.
	if resp := get("/api/quota"); resp.StatusCode != http.StatusOK {
		t.Fatalf("quota = %d", resp.StatusCode)
	}
	if resp := post("/api/agent/thinking", `{"budget":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("thinking = %d", resp.StatusCode)
	}
	if resp := put("/api/claudeenv", `{"skills":[],"mcp":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("claude-env = %d", resp.StatusCode)
	}
	if resp := put("/api/settings", `{"provider":"openai","memory_engine":"none"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", resp.StatusCode)
	}
	if resp := get("/api/plugins"); resp.StatusCode != http.StatusOK {
		t.Fatalf("plugins = %d", resp.StatusCode)
	}
	if resp := put("/api/plugins", `{"enabled":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("plugins put = %d", resp.StatusCode)
	}
	// Installs of a real builtin plugin pass validation; the (offline)
	// install fails inside the plugin manager, executing both closure
	// branches.
	_ = post("/api/plugins/browser/install", "{}")
	_ = post("/api/plugins/browser/reinstall", "{}")

	// A run in a different workspace exercises the per-workspace hooks
	// closure wired in buildAgent.
	if resp := post("/api/runs", `{"prompt":"hi","workspace":`+quoteJSONString(t.TempDir())+`}`); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
}

func TestServeBuildAgentError(t *testing.T) {
	isolateEnv(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.Provider = "nope"
	if err := Serve(t.Context(), opts, "127.0.0.1:0", true, ""); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

func TestServeOpensBrowserWithFakeOpen(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	fakeOpenBinary(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := Serve(ctx, opts, "127.0.0.1:0", false, ""); err != nil {
		t.Fatal(err)
	}
}

func TestNewAPIKeyProviderRequiresDB(t *testing.T) {
	if _, err := newAPIKeyProvider(nil); err == nil {
		t.Fatal("nil settings store accepted")
	}
	if _, err := newNativeAuth(nil, authn.NewLocal()); err == nil {
		t.Fatal("nil settings store accepted for native auth")
	}
}

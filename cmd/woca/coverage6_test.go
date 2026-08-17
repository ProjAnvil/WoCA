package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/webgui"
)

// TestMain lets the test binary double as the `woca serve` daemon when the
// webd manager spawns it (argv[1] == "serve"): a minimal health server stands
// in for the real web GUI so the web start/stop lifecycle can run for real.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		addr := webgui.DefaultAddr
		for i := 1; i < len(os.Args)-1; i++ {
			if os.Args[i] == "--addr" {
				addr = os.Args[i+1]
			}
		}
		mux := http.NewServeMux()
		mux.HandleFunc("/api/health", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `{"ok":true}`)
		})
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			os.Exit(1)
		}
		_ = (&http.Server{Handler: mux}).Serve(ln)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeOpenBinary installs a fake `open` command on PATH that fetches the URL
// with curl (following redirects), standing in for a browser visit. The
// WOCA_NO_BROWSER guard is cleared so the guarded openBrowser calls run.
func fakeOpenBinary(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexec /usr/bin/curl -s -o /dev/null -L \"$1\"\n"
	if err := os.WriteFile(filepath.Join(dir, "open"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("WOCA_NO_BROWSER", "")
}

// fakeAuthServerMux serves the MCP OAuth discovery/register/authorize/token
// endpoints; the authorize step redirects straight to the callback.
func fakeAuthServerMux(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/oauth-protected-resource", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"/register"}`)
	})
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"client_id":"dyn-client-1"}`)
	})
	mux.HandleFunc("/authorize", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac-123&state="+r.URL.Query().Get("state"), http.StatusFound)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("code_verifier") == "" || r.PostForm.Get("code") != "ac-123" {
			http.Error(w, "bad code", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"access_token":"tok-xyz","token_type":"bearer"}`)
	})
	srv := &http.Server{Handler: mux}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func TestMcpAuthCmdFullFlow(t *testing.T) {
	home := isolateEnv(t)
	db := filepath.Join(home, "woca.db")
	fakeOpenBinary(t)
	auth := fakeAuthServerMux(t)

	if err := execCmd(t, "--db", db, "mcp", "auth", "srv", auth+"/mcp"); err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !st.HasSecret("mcp_srv_token") {
		t.Fatal("token secret not stored")
	}
}

func TestNewMcpAuthRunnerFullFlow(t *testing.T) {
	home := isolateEnv(t)
	fakeOpenBinary(t)
	auth := fakeAuthServerMux(t)
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	oldArgs := os.Args
	os.Args = []string{"woca", "--mcp", "srv=" + auth + "/mcp"}
	defer func() { os.Args = oldArgs }()
	runner := newMcpAuthRunner(st)
	if err := runner(t.Context(), "srv"); err != nil {
		t.Fatal(err)
	}
	if !st.HasSecret("mcp_srv_token") {
		t.Fatal("token secret not stored")
	}
}

func freeLocalAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestWebDaemonLifecycle(t *testing.T) {
	isolateEnv(t)
	addr := freeLocalAddr(t)

	if err := execCmd(t, "web", "start", "--addr", addr); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "web", "status"); err != nil {
		t.Fatal(err)
	}
	// A second start reports the already-running daemon.
	if err := execCmd(t, "web", "start", "--addr", addr); err != nil {
		t.Fatal(err)
	}
	addr2 := freeLocalAddr(t)
	if err := execCmd(t, "web", "restart", "--addr", addr2); err != nil {
		t.Fatal(err)
	}
	if err := execCmd(t, "web", "stop"); err != nil {
		t.Fatal(err)
	}
}

func TestConfigOpenError(t *testing.T) {
	home := t.TempDir()
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := execCmd(t, "--db", filepath.Join(blocker, "woca.db"), "config", "list"); err == nil {
		t.Fatal("expected settings open error")
	}
}

func TestRunWebSearchBackendSuccess(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	// A working search endpoint: the WebSearch backend formats results.
	search := startHTTPServer(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"title":"Answer","url":"https://example.com/a","snippet":"the answer"}]}`)
	})
	url := fakeOpenAICustomToolFlow(t, "WebSearch", `{"query":"meaning"}`, "searched fine")
	opts := baseOpts(t, url)
	opts.prompt = "search the web"
	opts.searchAPI = search + "/search?q={q}"

	var out, errBuf bytes.Buffer
	if err := run(t.Context(), opts, strings.NewReader(""), &out, &errBuf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "searched fine") {
		t.Fatalf("output = %q", out.String())
	}
}

func TestServeOpensBrowserWithFakeOpen(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	fakeOpenBinary(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := serve(ctx, opts, "127.0.0.1:0", false); err != nil {
		t.Fatal(err)
	}
}

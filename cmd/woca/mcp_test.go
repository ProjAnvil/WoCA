package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/settings"
)

func TestMcpAuthFlowCancelled(t *testing.T) {
	home := isolateEnv(t)
	db := filepath.Join(home, "woca.db")
	// The authorization server discovery works, but the no-op opener from
	// TestMain means no browser visit ever happens, so the callback wait hits
	// the context deadline.
	mux := http_Mux(t,
		"/.well-known/oauth-authorization-server", `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"/register"}`,
		"/register", `{"client_id":"dyn-1"}`,
	)
	root := newRootCmd()
	root.SetArgs([]string{"--db", db, "mcp", "auth", "srv", mux + "/mcp"})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := root.ExecuteContext(ctx); err == nil {
		t.Fatal("expected cancelled flow error")
	}
}

func TestMcpAuthSettingsOpenError(t *testing.T) {
	home := t.TempDir()
	blocker := filepath.Join(home, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	if err := execCmd(t, "--db", filepath.Join(blocker, "woca.db"), "mcp", "auth", "srv", "http://127.0.0.1:1/mcp"); err == nil {
		t.Fatal("expected settings open error")
	}
}

func TestMcpAuthCmdFullFlow(t *testing.T) {
	home := isolateEnv(t)
	db := filepath.Join(home, "woca.db")
	stubBrowserOpen(t, true)
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

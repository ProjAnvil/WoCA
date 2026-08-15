package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAuthServer implements the MCP OAuth discovery + authorization-code
// endpoints; the "user" is simulated by OpenBrowser visiting the authorize
// URL, which redirects straight to the localhost callback.
func fakeAuthServer(t *testing.T) *httptest.Server {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-protected-resource":
			// No authorization_servers: discovery falls back to the origin.
		case "/.well-known/oauth-authorization-server":
			w.Write([]byte(`{
				"authorization_endpoint":"/authorize",
				"token_endpoint":"/token",
				"registration_endpoint":"/register"
			}`))
		case "/register":
			w.Write([]byte(`{"client_id":"dyn-client-1"}`))
		case "/authorize":
			if r.URL.Query().Get("code_challenge_method") != "S256" {
				http.Error(w, "expected S256", http.StatusBadRequest)
				return
			}
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac-123&state="+r.URL.Query().Get("state"), http.StatusFound)
		case "/token":
			if err := r.ParseForm(); err != nil {
				http.Error(w, "bad form", http.StatusBadRequest)
				return
			}
			mu.Lock()
			okVerifier := r.PostForm.Get("code_verifier") != "" && r.PostForm.Get("code") == "ac-123"
			mu.Unlock()
			if !okVerifier {
				http.Error(w, "missing code_verifier or bad code", http.StatusBadRequest)
				return
			}
			w.Write([]byte(`{"access_token":"tok-xyz","token_type":"bearer"}`))
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
	}))
	return srv
}

func TestRunOAuthFullFlow(t *testing.T) {
	auth := fakeAuthServer(t)
	defer auth.Close()

	var saved string
	flow := OAuthFlow{
		ServerName:  "acme",
		Endpoint:    auth.URL + "/mcp",
		Client:      auth.Client(),
		OpenBrowser: func(authURL string) error { _, err := auth.Client().Get(authURL); return err },
		SaveToken:   func(tok string) error { saved = tok; return nil },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := RunOAuth(ctx, flow); err != nil {
		t.Fatal(err)
	}
	if saved != "tok-xyz" {
		t.Fatalf("saved token = %q", saved)
	}
}

func TestRunOAuthDenied(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			w.Write([]byte(`{"authorization_endpoint":"/deny","token_endpoint":"/token"}`))
		case "/deny":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?error=access_denied&state="+r.URL.Query().Get("state"), http.StatusFound)
		}
	}))
	defer srv.Close()

	flow := OAuthFlow{
		ServerName:  "acme",
		Endpoint:    srv.URL + "/mcp",
		Client:      srv.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv.Client().Get(authURL); return err },
	}
	err := RunOAuth(context.Background(), flow)
	if err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("expected denial error, got %v", err)
	}
}

func TestPkcePair(t *testing.T) {
	v, c, err := pkcePair()
	if err != nil {
		t.Fatal(err)
	}
	if v == c || len(v) < 40 {
		t.Fatalf("verifier/challenge = %q/%q", v, c)
	}
	// The challenge must be the S256 of the verifier.
	if got := strings.TrimRight(base64RawURLEncode(sha256Sum(v)), "="); got != c {
		t.Fatalf("challenge mismatch: %q vs %q", got, c)
	}
}

func base64RawURLEncode(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	// minimal implementation sufficient for the test comparison
	var out []byte
	for i := 0; i < len(b); i += 3 {
		var chunk [3]byte
		n := copy(chunk[:], b[i:])
		v := uint32(chunk[0])<<16 | uint32(chunk[1])<<8 | uint32(chunk[2])
		out = append(out, alphabet[(v>>18)&63])
		out = append(out, alphabet[(v>>12)&63])
		if n > 1 {
			out = append(out, alphabet[(v>>6)&63])
		}
		if n > 2 {
			out = append(out, alphabet[v&63])
		}
	}
	return string(out)
}

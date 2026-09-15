package mcp

import (
	"context"
	"fmt"
	"net"
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
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
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
	err := RunOAuth(t.Context(), flow)
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

func TestResolveRef(t *testing.T) {
	if got := resolveRef("http://a/b/", "https://x/y"); got != "https://x/y" {
		t.Errorf("absolute = %q", got)
	}
	if got := resolveRef("http://a/b/", "rel"); got != "http://a/b/rel" {
		t.Errorf("relative = %q", got)
	}
	if got := resolveRef("http://a/b", "/abs"); got != "http://a/b/abs" {
		t.Errorf("rooted = %q", got)
	}
}

func TestAuthHelpersErrors(t *testing.T) {
	ctx := t.Context()
	client := &http.Client{Timeout: 2 * time.Second}

	// fetchJSON: non-200 and undecodable responses.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "badstatus") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, "not json")
	}))
	defer srv.Close()
	if err := fetchJSON(client, srv.URL+"/badstatus", &struct{}{}); err == nil {
		t.Error("expected status error")
	}
	if err := fetchJSON(client, srv.URL+"/garbage", &struct{}{}); err == nil {
		t.Error("expected decode error")
	}
	if err := fetchJSON(client, "http://127.0.0.1:1/x", &struct{}{}); err == nil {
		t.Error("expected transport error")
	}

	// discover helpers propagate failures.
	if _, err := discoverProtectedResource(client, srv.URL); err == nil {
		t.Error("expected protected-resource error")
	}
	if _, err := discoverAuthorizationServer(client, srv.URL); err == nil {
		t.Error("expected authorization-server error")
	}

	// dynamicRegister: empty endpoint uses the public client id.
	id, err := dynamicRegister(ctx, client, "", 0)
	if err != nil || id != "woca-mcp-public-client" {
		t.Fatalf("public client = %q %v", id, err)
	}
	// Registration failures: bad status, missing client_id, transport.
	reg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "nostatus") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.HasSuffix(r.URL.Path, "noid") {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"client_id":"c1"}`)
	}))
	defer reg.Close()
	if _, err := dynamicRegister(ctx, client, reg.URL+"/nostatus", 0); err == nil {
		t.Error("expected register status error")
	}
	if _, err := dynamicRegister(ctx, client, reg.URL+"/noid", 0); err == nil {
		t.Error("expected missing client_id error")
	}
	if _, err := dynamicRegister(ctx, client, "http://127.0.0.1:1/reg", 0); err == nil {
		t.Error("expected register transport error")
	}
	if id, err := dynamicRegister(ctx, client, reg.URL+"/ok", 0); err != nil || id != "c1" {
		t.Fatalf("register = %q %v", id, err)
	}

	// exchangeCode: non-200, missing token, transport.
	tok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "bad") {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, "nope")
			return
		}
		if strings.HasSuffix(r.URL.Path, "empty") {
			fmt.Fprint(w, `{}`)
			return
		}
		fmt.Fprint(w, `{"access_token":"at"}`)
	}))
	defer tok.Close()
	if _, err := exchangeCode(client, tok.URL+"/bad", "c", "code", "http://127.0.0.1/", "v"); err == nil {
		t.Error("expected exchange status error")
	}
	if _, err := exchangeCode(client, tok.URL+"/empty", "c", "code", "http://127.0.0.1/", "v"); err == nil {
		t.Error("expected missing token error")
	}
	if _, err := exchangeCode(client, "http://127.0.0.1:1/t", "c", "code", "http://127.0.0.1/", "v"); err == nil {
		t.Error("expected exchange transport error")
	}
	if v, err := exchangeCode(client, tok.URL+"/ok", "c", "code", "http://127.0.0.1/", "v"); err != nil || v != "at" {
		t.Fatalf("exchange = %q %v", v, err)
	}
}

func TestRunOAuthErrorPaths(t *testing.T) {
	// An unparsable endpoint fails immediately.
	if err := RunOAuth(t.Context(), OAuthFlow{ServerName: "x", Endpoint: "http://[::1]:namedport"}); err == nil {
		t.Fatal("expected parse error")
	}
	// Unreachable discovery fails.
	if err := RunOAuth(t.Context(), OAuthFlow{ServerName: "x", Endpoint: "http://127.0.0.1:1/mcp"}); err == nil {
		t.Fatal("expected discovery error")
	}
	// A cancelled context aborts.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := RunOAuth(ctx, OAuthFlow{ServerName: "x", Endpoint: "http://127.0.0.1:1/mcp"}); err == nil {
		t.Fatal("expected ctx error")
	}
}

func TestRunOAuthAuthServerSelectionAndSaveError(t *testing.T) {
	// The protected-resource metadata points at a separate authorization
	// server origin, which the flow must use.
	auth := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"/register"}`)
		case "/register":
			fmt.Fprint(w, `{"client_id":"dyn-1"}`)
		case "/authorize":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac-ok&state="+r.URL.Query().Get("state"), http.StatusFound)
		case "/token":
			fmt.Fprint(w, `{"access_token":"tok-ok"}`)
		}
	}))
	defer auth.Close()

	mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/oauth-protected-resource" {
			fmt.Fprintf(w, `{"authorization_servers":["%s"]}`, auth.URL)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer mcp.Close()

	var saved string
	flow := OAuthFlow{
		ServerName:  "sel",
		Endpoint:    mcp.URL + "/mcp",
		Client:      mcp.Client(),
		OpenBrowser: func(authURL string) error { _, err := mcp.Client().Get(authURL); return err },
		SaveToken:   func(tok string) error { saved = tok; return nil },
	}
	if err := RunOAuth(t.Context(), flow); err != nil {
		t.Fatal(err)
	}
	if saved != "tok-ok" {
		t.Fatalf("saved = %q", saved)
	}

	// A failing SaveToken surfaces as an error.
	flow2 := flow
	flow2.SaveToken = func(tok string) error { return fmt.Errorf("disk full") }
	if err := RunOAuth(t.Context(), flow2); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("save error = %v", err)
	}

	// Incomplete metadata (missing token endpoint).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize"}`)
	}))
	defer bad.Close()
	flow3 := OAuthFlow{
		ServerName: "bad",
		Endpoint:   bad.URL + "/mcp",
		Client:     bad.Client(),
	}
	if err := RunOAuth(t.Context(), flow3); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("incomplete metadata = %v", err)
	}
}

func TestRunOAuthStateMismatchAndExchangeFailure(t *testing.T) {
	// The "browser" tampers with the state parameter: the callback rejects
	// it and the flow errors.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
		case "/authorize":
			bad := r.URL.Query().Get("state") + "tampered"
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac&state="+bad, http.StatusFound)
		case "/token":
			fmt.Fprint(w, `{"access_token":"t"}`)
		}
	}))
	defer srv.Close()
	flow := OAuthFlow{
		ServerName:  "mismatch",
		Endpoint:    srv.URL + "/mcp",
		Client:      srv.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv.Client().Get(authURL); return err },
	}
	if err := RunOAuth(t.Context(), flow); err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("state mismatch = %v", err)
	}

	// The redirect carries no code at all.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
		case "/authorize":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?state="+r.URL.Query().Get("state"), http.StatusFound)
		}
	}))
	defer srv2.Close()
	flow2 := OAuthFlow{
		ServerName:  "nocode",
		Endpoint:    srv2.URL + "/mcp",
		Client:      srv2.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv2.Client().Get(authURL); return err },
	}
	if err := RunOAuth(t.Context(), flow2); err == nil {
		t.Fatal("expected missing-code error")
	}

	// A working authorization whose token exchange fails.
	srv3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/oauth-authorization-server":
			fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
		case "/authorize":
			http.Redirect(w, r, r.URL.Query().Get("redirect_uri")+"?code=ac&state="+r.URL.Query().Get("state"), http.StatusFound)
		case "/token":
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	}))
	defer srv3.Close()
	flow3 := OAuthFlow{
		ServerName:  "xchg",
		Endpoint:    srv3.URL + "/mcp",
		Client:      srv3.Client(),
		OpenBrowser: func(authURL string) error { _, err := srv3.Client().Get(authURL); return err },
	}
	if err := RunOAuth(t.Context(), flow3); err == nil || !strings.Contains(err.Error(), "token exchange") {
		t.Fatalf("exchange failure = %v", err)
	}
}

func TestRunOAuthRegistrationAndPortFailures(t *testing.T) {
	// The registration endpoint is unreachable: dynamic registration fails.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token","registration_endpoint":"http://127.0.0.1:1/reg"}`)
	}))
	defer srv.Close()
	flow := OAuthFlow{ServerName: "regfail", Endpoint: srv.URL + "/mcp", Client: srv.Client()}
	if err := RunOAuth(t.Context(), flow); err == nil || !strings.Contains(err.Error(), "registration") {
		t.Fatalf("registration failure = %v", err)
	}

	// A busy callback port cannot be listened on.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"authorization_endpoint":"/authorize","token_endpoint":"/token"}`)
	}))
	defer srv2.Close()
	flow2 := OAuthFlow{ServerName: "busy", Endpoint: srv2.URL + "/mcp", Client: srv2.Client(), CallbackPort: port}
	if err := RunOAuth(t.Context(), flow2); err == nil || !strings.Contains(err.Error(), "callback listener") {
		t.Fatalf("busy port = %v", err)
	}

	// No OpenBrowser: the flow waits and the context times out.
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	flow3 := OAuthFlow{ServerName: "nobrowser", Endpoint: srv2.URL + "/mcp", Client: srv2.Client()}
	if err := RunOAuth(ctx, flow3); err == nil {
		t.Fatal("expected timeout without a browser visit")
	}

	// A failing OpenBrowser aborts immediately.
	flow4 := OAuthFlow{
		ServerName:  "openfail",
		Endpoint:    srv2.URL + "/mcp",
		Client:      srv2.Client(),
		OpenBrowser: func(u string) error { return fmt.Errorf("no display") },
	}
	if err := RunOAuth(t.Context(), flow4); err == nil || !strings.Contains(err.Error(), "open browser") {
		t.Fatalf("open failure = %v", err)
	}
}

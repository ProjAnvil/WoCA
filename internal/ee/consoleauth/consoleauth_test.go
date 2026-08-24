package consoleauth

// Tests cover the console credential gate: open mode (no env) injects the
// administrator context, configured mode requires the session cookie, the
// login exchange mints it with the enumerated failure codes, and logout
// drops it.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/platform/tenancy"
)

func echoTenants(w http.ResponseWriter, r *http.Request) {
	tc, _ := tenancy.FromContext(r.Context())
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(tc.UserID))
}

func TestOpenConsoleInjectsAdminContext(t *testing.T) {
	g := New()
	if g.Configured() {
		t.Fatal("gate configured without env")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/echo", echoTenants)
	srv := httptest.NewServer(g.Wrap(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/echo")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 32)
	n, _ := resp.Body.Read(buf)
	if string(buf[:n]) != "console-admin" {
		t.Fatalf("open mode actor = %q", string(buf[:n]))
	}
}

func TestConfiguredGateRequiresSession(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	t.Setenv("WOCA_CONSOLE_PASSWORD", "s3cret")
	g := New()
	if !g.Configured() {
		t.Fatal("gate not configured with env")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/echo", echoTenants)
	srv := httptest.NewServer(g.Wrap(mux))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/echo")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session = %d want 401", resp.StatusCode)
	}
}

func TestLoginLogoutFlow(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	t.Setenv("WOCA_CONSOLE_PASSWORD", "s3cret")
	g := New()
	login := httptest.NewServer(http.HandlerFunc(g.Login))
	defer login.Close()
	logout := httptest.NewServer(http.HandlerFunc(g.Logout))
	defer logout.Close()
	gatedMux := http.NewServeMux()
	gatedMux.HandleFunc("GET /api/echo", echoTenants)
	gated := httptest.NewServer(g.Wrap(gatedMux))
	defer gated.Close()

	post := func(url, body string) *http.Response {
		resp, err := http.Post(url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Wrong credentials: enumerated code.
	resp := post(login.URL, `{"username":"admin","password":"wrong"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password = %d", resp.StatusCode)
	}

	// Right credentials: cookie opens the gate.
	resp = post(login.URL, `{"username":"admin","password":"s3cret"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login = %d", resp.StatusCode)
	}
	var session string
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			session = c.Value
		}
	}
	if session == "" {
		t.Fatal("no session cookie")
	}
	req, _ := http.NewRequest(http.MethodGet, gated.URL+"/api/echo", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	gr, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	gr.Body.Close()
	if gr.StatusCode != http.StatusOK {
		t.Fatalf("with session = %d want 200", gr.StatusCode)
	}

	// Logout invalidates: the same cookie now bounces.
	lr, err := http.NewRequest(http.MethodPost, logout.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	lr.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	lo, err := http.DefaultClient.Do(lr)
	if err != nil {
		t.Fatal(err)
	}
	lo.Body.Close()
	if lo.StatusCode != http.StatusOK {
		t.Fatalf("logout = %d", lo.StatusCode)
	}
	req2, _ := http.NewRequest(http.MethodGet, gated.URL+"/api/echo", nil)
	req2.AddCookie(&http.Cookie{Name: SessionCookie, Value: session})
	gr2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	gr2.Body.Close()
	if gr2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout = %d want 401", gr2.StatusCode)
	}
}

// TestAnonymousSurfaceStaysReachable: in configured mode the SPA shell
// and the health probe must stay anonymous — otherwise the login form
// itself is gated away and healthchecks break (R2's allowlist).
func TestAnonymousSurfaceStaysReachable(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	t.Setenv("WOCA_CONSOLE_PASSWORD", "s3cret")
	g := New()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("<html>login form</html>"))
	})
	mux.HandleFunc("GET /api/tenant/users", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(g.Wrap(mux))
	defer srv.Close()

	for _, path := range []string{"/", "/index.html", "/api/health"} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s in configured mode = %d want 200", path, resp.StatusCode)
		}
	}
	// The data plane stays gated.
	resp, err := http.Get(srv.URL + "/api/tenant/users")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/tenant/users without session = %d want 401", resp.StatusCode)
	}
}

// TestHalfConfiguredFailsClosed: exactly one credential env set must not
// degrade to an open console (and the empty half would be matchable).
func TestHalfConfiguredFailsClosed(t *testing.T) {
	t.Setenv("WOCA_CONSOLE_USER", "admin")
	g := New()
	if g.Configured() {
		t.Fatal("half-configured reports configured")
	}
	srv := httptest.NewServer(g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/api/settings")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("half-configured data plane = %d want 503", resp.StatusCode)
	}
	login := httptest.NewServer(http.HandlerFunc(g.Login))
	defer login.Close()
	lr, err := http.Post(login.URL, "application/json", strings.NewReader(`{"username":"admin","password":""}`))
	if err != nil {
		t.Fatal(err)
	}
	lr.Body.Close()
	if lr.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("half-configured login = %d want 503", lr.StatusCode)
	}
}

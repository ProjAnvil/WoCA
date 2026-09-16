package relay

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func testSPA() fstest.MapFS {
	return fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>woca</title>app")},
		"assets/app.js": {Data: []byte("console.log(1)")},
	}
}

func TestCheckBindFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name          string
		addr          string
		tls, dev, prx bool
		wantErr       bool
	}{
		{"public bind without TLS refuses", ":443", false, false, false, true},
		{"public bind with TLS passes", ":443", true, false, false, false},
		{"loopback plain HTTP passes", "127.0.0.1:8443", false, false, false, false},
		{"localhost plain HTTP passes", "localhost:8443", false, false, false, false},
		{"public insecure dev opt-in passes", "0.0.0.0:8080", false, true, false, false},
		{"public behind-proxy warns but passes", ":8080", false, false, true, false},
		{"wildcard IPv6 without TLS refuses", "[::]:443", false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckBind(tc.addr, tc.tls, tc.dev, tc.prx, testLogger())
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckBind(%q, tls=%v, dev=%v, proxy=%v) = %v, wantErr %v",
					tc.addr, tc.tls, tc.dev, tc.prx, err, tc.wantErr)
			}
		})
	}
}

func TestRelaySkeletonSurface(t *testing.T) {
	ks := openTestKeyStore(t)
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	rl := NewRelay(ks, testSPA(), false, testLogger())
	ts := httptest.NewServer(rl.Handler())
	t.Cleanup(ts.Close)

	get := func(path string) (int, http.Header, string) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, resp.Header, string(body)
	}

	// Anonymous whitelist (design §7.2): health + SPA + login only.
	if code, _, body := get("/api/health"); code != http.StatusOK ||
		!strings.Contains(body, `"ok":true`) {
		t.Fatalf("health: %d %s", code, body)
	}
	if code, hdr, body := get("/"); code != http.StatusOK ||
		!strings.Contains(hdr.Get("Content-Type"), "text/html") ||
		!strings.Contains(body, "app") {
		t.Fatalf("spa root: %d %s", code, body)
	}
	// Client-side routes fall back to the app shell.
	if code, _, body := get("/chat/session/abc"); code != http.StatusOK ||
		!strings.Contains(body, "app") {
		t.Fatalf("spa fallback: %d %s", code, body)
	}
	if code, _, body := get("/assets/app.js"); code != http.StatusOK ||
		!strings.Contains(body, "console.log") {
		t.Fatalf("spa asset: %d %s", code, body)
	}
	// The tunnel plane gates every /api/ path behind a session (design
	// §7.2 — the whitelist is exactly SPA assets + /api/health +
	// /api/auth/login).
	if code, _, body := get("/api/models"); code != http.StatusUnauthorized ||
		!strings.Contains(body, `"code":"unauthorized"`) {
		t.Fatalf("anonymous /api/models: %d %s, want 401 with the session gate attached", code, body)
	}
	// Login is reachable anonymously; me/logout demand a session.
	resp, err := http.Post(ts.URL+"/api/auth/login", "application/json",
		strings.NewReader(`{"api_key":"`+raw+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	if code, _, body := get("/api/auth/me"); code != http.StatusUnauthorized ||
		!strings.Contains(body, `"code":"unauthorized"`) {
		t.Fatalf("anonymous me: %d %s", code, body)
	}
}

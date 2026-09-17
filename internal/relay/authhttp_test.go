package relay

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func testLogger() zerolog.Logger { return zerolog.Nop() }

func newTestAuth(t *testing.T) (*AuthService, string) {
	t.Helper()
	ks := openTestKeyStore(t)
	_, raw, err := ks.CreateKey(t.Context(), "home-mac")
	if err != nil {
		t.Fatal(err)
	}
	return NewAuthService(ks, false, testLogger()), raw
}

func loginFor(t *testing.T, a *AuthService, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"api_key":"`+apiKey+`"}`))
	rec := httptest.NewRecorder()
	a.HandleLogin(rec, req)
	return rec
}

func TestLoginCookieAndBodyToken(t *testing.T) {
	a, raw := newTestAuth(t)
	rec := loginFor(t, a, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == SessionCookieName {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatalf("no %s cookie", SessionCookieName)
	}
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie flags: %+v (want HttpOnly + SameSite=Lax)", cookie)
	}
	// The body token is the native client's Bearer credential (design
	// §3.3 — cookie AND body token, the edgegw precedent).
	var body struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
		UserID    string `json:"user_id"`
		Remark    string `json:"remark"`
		Role      string `json:"role"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Token == "" || body.ExpiresAt == 0 || body.UserID == "" ||
		body.Remark != "home-mac" || body.Role != "admin" {
		t.Fatalf("login body: %+v", body)
	}

	// Bearer channel (native).
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer "+body.Token)
	meRec := httptest.NewRecorder()
	a.HandleMe(meRec, req)
	if meRec.Code != http.StatusOK ||
		!strings.Contains(meRec.Body.String(), `"remark":"home-mac"`) ||
		!strings.Contains(meRec.Body.String(), `"role":"admin"`) {
		t.Fatalf("me via bearer: %d %s", meRec.Code, meRec.Body)
	}

	// Cookie channel (browser EventSource cannot set headers).
	req = httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.AddCookie(cookie)
	meRec = httptest.NewRecorder()
	a.HandleMe(meRec, req)
	if meRec.Code != http.StatusOK {
		t.Fatalf("me via cookie: %d %s", meRec.Code, meRec.Body)
	}
}

func TestLoginFailureShapes(t *testing.T) {
	a, raw := newTestAuth(t)
	if rec := loginFor(t, a, "wcr_"+strings.Repeat("A", 43)); rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), `"code":"unauthorized"`) {
		t.Fatalf("unknown key: %d %s", rec.Code, rec.Body)
	}
	// A revoked key says so with the frontend contract code — the SPA's
	// revoked notice keys off exactly "revoked" (design §2).
	if _, ok, err := a.keys.RevokeKey(t.Context(), "home-mac"); err != nil || !ok {
		t.Fatal("revoke setup failed")
	}
	rec := loginFor(t, a, raw)
	if rec.Code != http.StatusUnauthorized ||
		!strings.Contains(rec.Body.String(), `"code":"revoked"`) {
		t.Fatalf("revoked login: %d %s (must carry code revoked)", rec.Code, rec.Body)
	}
}

func TestLogoutDualChannel(t *testing.T) {
	a, raw := newTestAuth(t)
	rec := loginFor(t, a, raw)
	var body struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	// Logout accepts Bearer (native) — and cookie (browser) — design §3.3.
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req.Header.Set("Authorization", "Bearer "+body.Token)
	outRec := httptest.NewRecorder()
	a.HandleLogout(outRec, req)
	if outRec.Code != http.StatusNoContent {
		t.Fatalf("logout: %d", outRec.Code)
	}
	if _, err := a.SessionFromRequest(req); err == nil {
		t.Fatal("session survived logout (server-side delete required)")
	}
}

func TestSecureCookieProxyModel(t *testing.T) {
	a, _ := newTestAuth(t)
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", nil)
	// Plain HTTP without --behind-proxy (dev loopback): not Secure.
	if a.SecureCookie(r) {
		t.Fatal("direct plain HTTP must not mark the cookie Secure")
	}
	// A client-forged X-Forwarded-Proto is NOT trusted by default
	// (design §7.3 — the cookie-downgrade vector).
	r.Header.Set("X-Forwarded-Proto", "https")
	if a.SecureCookie(r) {
		t.Fatal("X-Forwarded-Proto trusted without --behind-proxy")
	}
	// Only an explicit --behind-proxy trusts the front's statement.
	a.behindProxy = true
	if !a.SecureCookie(r) {
		t.Fatal("--behind-proxy must trust X-Forwarded-Proto: https")
	}
}

func TestLoginRateLimit(t *testing.T) {
	ks := openTestKeyStore(t)
	a := NewAuthService(ks, false, testLogger())
	// All attempts from one source IP (httptest.NewRequest default).
	for i := 0; i < 10; i++ {
		if rec := loginFor(t, a, "bad"); rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d rate-limited inside the allowance", i+1)
		}
	}
	if rec := loginFor(t, a, "bad"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: %d, want 429", rec.Code)
	}
}

func TestLoginMalformedBody(t *testing.T) {
	a, _ := newTestAuth(t)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader("{not json"))
	rec := httptest.NewRecorder()
	a.HandleLogin(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("malformed login body: %d, want 400", rec.Code)
	}
}

func TestLoginSessionStoreFailure(t *testing.T) {
	a, raw := newTestAuth(t)
	// Fail session inserts at the storage layer: the key resolves, the
	// session mint does not → 500.
	if _, err := a.keys.db.Exec(`CREATE TRIGGER fail_sessions BEFORE INSERT ON sessions
		BEGIN SELECT RAISE(FAIL, 'test: no sessions'); END`); err != nil {
		t.Fatal(err)
	}
	if rec := loginFor(t, a, raw); rec.Code != http.StatusInternalServerError {
		t.Fatalf("login with a failing session store: %d %s, want 500", rec.Code, rec.Body)
	}
}

func TestClientIP(t *testing.T) {
	a, _ := newTestAuth(t)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "198.51.100.7:4433"
	if got := a.ClientIP(r); got != "198.51.100.7" {
		t.Fatalf("peer ip: %q", got)
	}
	// No port on the peer address: the raw value is the fallback.
	r.RemoteAddr = "198.51.100.9"
	if got := a.ClientIP(r); got != "198.51.100.9" {
		t.Fatalf("unparseable peer: %q", got)
	}

	// X-Forwarded-For is ignored without --behind-proxy (design §7.3).
	r.Header.Set("X-Forwarded-For", "203.0.113.1, 10.0.0.1")
	if got := a.ClientIP(r); got != "198.51.100.9" {
		t.Fatalf("XFF trusted without --behind-proxy: %q", got)
	}

	a.behindProxy = true
	if got := a.ClientIP(r); got != "203.0.113.1" {
		t.Fatalf("XFF first hop: %q", got)
	}
	// A blank first hop falls back to the peer address.
	r.Header.Set("X-Forwarded-For", " , 10.0.0.1")
	if got := a.ClientIP(r); got != "198.51.100.9" {
		t.Fatalf("blank XFF first hop: %q", got)
	}
}

func TestSecureCookieDirectTLS(t *testing.T) {
	a, _ := newTestAuth(t)
	r := httptest.NewRequest(http.MethodPost, "https://relay.example.com/api/auth/login", nil)
	if r.TLS == nil {
		t.Fatal("https request should carry TLS state")
	}
	if !a.SecureCookie(r) {
		t.Fatal("direct TLS must mark the cookie Secure")
	}
}

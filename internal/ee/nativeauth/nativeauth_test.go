package nativeauth

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// newNativeAuthServer builds a server with the native credential surface
// configured, plus the fixtures needed to drive it.
type nativeAuthFixture struct {
	handler    http.Handler
	store      *tenant.Store
	key        string // full API key for the member user
	otherKey   string // full API key for a second tenant
	dpopKey    *ecdsa.PrivateKey
	thumbprint string
	t          *testing.T
}

func newNativeAuthFixture(t *testing.T) *nativeAuthFixture {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "nativeauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	st, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	tn, err := st.CreateTenant(ctx, "Acme")
	if err != nil {
		t.Fatal(err)
	}
	u, err := st.CreateUser(ctx, tn.ID, "alice", authz.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := st.IssueKey(ctx, tn.ID, u.ID, "native", authz.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	tn2, err := st.CreateTenant(ctx, "Other")
	if err != nil {
		t.Fatal(err)
	}
	u2, err := st.CreateUser(ctx, tn2.ID, "bob", authz.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey, err := st.IssueKey(ctx, tn2.ID, u2.ID, "native", authz.RoleMember)
	if err != nil {
		t.Fatal(err)
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := dpop.Thumbprint(priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	na := New(st, tenant.NewAPIKeyProvider(st))
	srv := server.New(server.Config{
		Logger:     zerolog.Nop(),
		NativeAuth: na,
		// The native surface's declared exemptions (mirrors the
		// woca-scheduler entry point): activation is anonymous, the whole
		// prefix is the client's session lifecycle.
		AnonymousAPIPaths:      []string{"/api/native/activate"},
		HostGateExemptPrefixes: []string{"/api/native/"},
	})
	return &nativeAuthFixture{
		handler: srv.Handler(), store: st, key: key, otherKey: otherKey,
		dpopKey: priv, thumbprint: tp, t: t,
	}
}

func (f *nativeAuthFixture) publicJWK() []byte {
	f.t.Helper()
	raw, err := f.dpopKey.PublicKey.Bytes() // uncompressed point: 0x04 || X || Y
	if err != nil {
		f.t.Fatal(err)
	}
	return []byte(fmt.Sprintf(`{"kty":"EC","crv":"P-256","x":"%s","y":"%s"}`,
		base64.RawURLEncoding.EncodeToString(raw[1:33]),
		base64.RawURLEncoding.EncodeToString(raw[33:65])))
}

func (f *nativeAuthFixture) activate(apiKey string, jwk []byte) *httptest.ResponseRecorder {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"api_key":   apiKey,
		"device_id": "device-test",
		"dpop_jwk":  json.RawMessage(jwk),
	})
	r := httptest.NewRequest(http.MethodPost, "/api/native/activate", bytes.NewReader(body))
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func (f *nativeAuthFixture) sign(method, uri, token string) string {
	f.t.Helper()
	proof, err := dpop.Sign(f.dpopKey, dpop.ProofInput{
		HTM: method, HTU: uri, AccessToken: token,
		IAT: time.Now(), JTI: fmt.Sprintf("jti-%d", time.Now().UnixNano()),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return proof
}

func (f *nativeAuthFixture) sessionCall(method, path, token string, proofOK bool) *httptest.ResponseRecorder {
	f.t.Helper()
	uri := "https://bff.example.com" + path
	r := httptest.NewRequest(method, uri, nil)
	r.Host = "bff.example.com"
	r.Header.Set("Authorization", "DPoP "+token)
	if proofOK {
		r.Header.Set("DPoP", f.sign(method, uri, token))
	}
	w := httptest.NewRecorder()
	f.handler.ServeHTTP(w, r)
	return w
}

func TestActivateIssuesSession(t *testing.T) {
	f := newNativeAuthFixture(t)

	w := f.activate(f.key, f.publicJWK())
	if w.Code != http.StatusOK {
		t.Fatalf("activate = %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		SessionID string `json:"session_id"`
		ExpiresAt int64  `json:"expires_at"`
		DeviceID  string `json:"device_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.SessionID == "" || resp.DeviceID != "device-test" {
		t.Fatalf("activate response = %+v", resp)
	}
	if resp.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("expires_at = %d", resp.ExpiresAt)
	}
	// The API key must never appear in the response body.
	if bytes.Contains(w.Body.Bytes(), []byte(f.key)) {
		t.Fatal("activate response leaks the API key")
	}
}

func TestActivateRejectsBadInputs(t *testing.T) {
	f := newNativeAuthFixture(t)

	cases := []struct {
		name   string
		key    string
		jwk    []byte
		status int
	}{
		{"wrong key", "woca_000000000000_" + strings.Repeat("0", 48), f.publicJWK(), http.StatusUnauthorized},
		{"malformed key", "not-a-key", f.publicJWK(), http.StatusUnauthorized},
		{"private jwk rejected", f.key,
			[]byte(`{"kty":"EC","crv":"P-256","x":"AAA","y":"AAA","d":"AAA"}`), http.StatusBadRequest},
		{"non-ec jwk", f.key, []byte(`{"kty":"RSA","n":"AAA","e":"AQAB"}`), http.StatusBadRequest},
		{"empty key", "", f.publicJWK(), http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := f.activate(tc.key, tc.jwk)
			if w.Code != tc.status {
				t.Fatalf("activate = %d %s, want %d", w.Code, w.Body.String(), tc.status)
			}
			if bytes.Contains(w.Body.Bytes(), []byte("woca_")) {
				t.Fatalf("response echoes key material: %s", w.Body.String())
			}
		})
	}
}

func TestActivateRateLimited(t *testing.T) {
	f := newNativeAuthFixture(t)
	for i := range 10 {
		if w := f.activate("woca_000000000000_"+strings.Repeat("0", 48), f.publicJWK()); w.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d", i, w.Code)
		}
	}
	if w := f.activate(f.key, f.publicJWK()); w.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt = %d, want 429", w.Code)
	}
}

func TestSessionLifecycleValidateAndLogout(t *testing.T) {
	f := newNativeAuthFixture(t)
	w := f.activate(f.key, f.publicJWK())
	if w.Code != http.StatusOK {
		t.Fatalf("activate = %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	// Validate: DPoP-authenticated GET returns session facts.
	info := f.sessionCall(http.MethodGet, "/api/native/session", resp.SessionID, true)
	if info.Code != http.StatusOK {
		t.Fatalf("session info = %d %s", info.Code, info.Body.String())
	}
	if !bytes.Contains(info.Body.Bytes(), []byte(`"device_id":"device-test"`)) {
		t.Fatalf("session info body = %s", info.Body.String())
	}

	// A proof-less request must not authenticate.
	if w := f.sessionCall(http.MethodGet, "/api/native/session", resp.SessionID, false); w.Code != http.StatusUnauthorized {
		t.Fatalf("proof-less session info = %d, want 401", w.Code)
	}

	// Logout revokes; subsequent validation fails.
	out := f.sessionCall(http.MethodDelete, "/api/native/session", resp.SessionID, true)
	if out.Code != http.StatusNoContent {
		t.Fatalf("logout = %d %s", out.Code, out.Body.String())
	}
	if w := f.sessionCall(http.MethodGet, "/api/native/session", resp.SessionID, true); w.Code != http.StatusUnauthorized {
		t.Fatalf("session still valid after logout: %d", w.Code)
	}
}

package tenant

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/ee/dpop"
	"github.com/projanvil/woca/internal/platform/authn"
	"github.com/projanvil/woca/internal/platform/authz"
)

func TestSessionProviderAuthenticatesValidProof(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	tenantID, userID := seedSessionTenant(t, st)

	key := newDPoPKey(t)
	sess, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", key.thumbprint, authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	_ = sess

	p := NewSessionProvider(st, dpop.NewVerifier())
	r := httptest.NewRequest(http.MethodGet, "https://bff.example.com/api/models", nil)
	r.Header.Set("DPoP", signDPoP(t, key, http.MethodGet, "https://bff.example.com/api/models", full))
	tc, err := p.AuthenticateRequest(r, full)
	if err != nil {
		t.Fatal(err)
	}
	if tc.TenantID != tenantID || tc.UserID != userID {
		t.Fatalf("tenancy = %+v", tc)
	}
	if len(authz.ScopesForRole(authz.RoleMember)) != 0 && len(tc.Scopes) == 0 {
		t.Fatalf("scopes missing for role %s", authz.RoleMember)
	}
}

func TestSessionProviderRejectsFailures(t *testing.T) {
	st := openTestStore(t)
	ctx := t.Context()
	tenantID, userID := seedSessionTenant(t, st)

	key := newDPoPKey(t)
	other := newDPoPKey(t)
	_, full, err := st.IssueSession(ctx, tenantID, userID, "device-1", key.thumbprint, authz.RoleMember, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	p := NewSessionProvider(st, dpop.NewVerifier())
	const uri = "https://bff.example.com/api/models"

	cases := []struct {
		name   string
		method string
		proof  func() string
		token  string
	}{
		{"missing proof header", http.MethodGet, func() string { return "" }, full},
		{"missing access token", http.MethodGet, func() string {
			return signDPoP(t, key, http.MethodGet, uri, "")
		}, ""},
		{"wrong method", http.MethodPost, func() string {
			return signDPoP(t, key, http.MethodGet, uri, full)
		}, full},
		{"wrong key", http.MethodGet, func() string {
			return signDPoP(t, other, http.MethodGet, uri, full)
		}, full},
		{"unknown token", http.MethodGet, func() string {
			return signDPoP(t, key, http.MethodGet, uri, full)
		}, "wsess_" + "000000000000" + "_" + "000000000000000000000000000000000000000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, uri, nil)
			if proof := tc.proof(); proof != "" {
				r.Header.Set("DPoP", proof)
			}
			if _, err := p.AuthenticateRequest(r, tc.token); err != authn.ErrUnauthenticated {
				t.Fatalf("AuthenticateRequest = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

type dpopKey struct {
	privEC     *ecdsa.PrivateKey
	thumbprint string
}

func newDPoPKey(t *testing.T) dpopKey {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tp, err := dpop.Thumbprint(priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return dpopKey{privEC: priv, thumbprint: tp}
}

func signDPoP(t *testing.T, k dpopKey, htm, htu, token string) string {
	t.Helper()
	proof, err := dpop.Sign(k.privEC, dpop.ProofInput{HTM: htm, HTU: htu, AccessToken: token, IAT: time.Now(), JTI: "jti-" + htm})
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

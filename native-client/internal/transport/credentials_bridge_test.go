package transport

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestBridgeCredentialsLoginFlow(t *testing.T) {
	token := "wocs_session_token"
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			APIKey string `json:"api_key"`
		}
		_ = jsonDecode(r, &req)
		if req.APIKey != "woca_good" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"` + token + `","role":"member"}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// A request the provider must serve (same origin).
	target, _ := url.Parse(ts.URL + "/api/sessions")

	b := &BridgeCredentials{BaseURL: ts.URL, APIKey: func() (string, error) { return "woca_good", nil }}
	creds, err := b.Credentials(context.Background(), "", target)
	if err != nil || creds.Scheme != "Bearer" || creds.Session != token {
		t.Fatalf("credentials = %+v %v", creds, err)
	}
	// Cached: no second login (the token would still work, but a counter
	// proves laziness).
	b.Reset()
	b.APIKey = func() (string, error) { return "", ErrAuthorizationRequired }
	if _, err := b.Credentials(context.Background(), "", target); err != ErrAuthorizationRequired {
		t.Fatalf("missing key = %v", err)
	}

	// Wrong key → authorization required (re-ask).
	b2 := &BridgeCredentials{BaseURL: ts.URL, APIKey: func() (string, error) { return "woca_bad", nil }}
	if _, err := b2.Credentials(context.Background(), "", target); err != ErrAuthorizationRequired {
		t.Fatalf("bad key = %v", err)
	}
}

func TestBridgeCredentialsFallsBackToDPoP(t *testing.T) {
	// No /api/auth/login route → 404 → the DPoP fallback serves.
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)
	defer ts.Close()
	target, _ := url.Parse(ts.URL + "/api/anything")
	fallback := &staticProvider{creds: Credentials{Scheme: "DPoP", Session: "x"}}
	b := &BridgeCredentials{BaseURL: ts.URL, APIKey: func() (string, error) { return "k", nil }, DPoP: fallback}
	creds, err := b.Credentials(context.Background(), "", target)
	if err != nil || creds.Session != "x" {
		t.Fatalf("fallback = %+v %v", creds, err)
	}
}

func TestPinnedTLSConfigAcceptsPinnedCertOnly(t *testing.T) {
	cert1 := selfSignedCert(t)
	pin := SPKIFingerprint(cert1.Leaf)
	ts := httptest.NewUnstartedServer(http.NotFoundHandler())
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{cert1}}
	ts.StartTLS()
	defer ts.Close()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: PinnedTLSConfig(pin)}, Timeout: 5 * time.Second}
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("pinned connect: %v", err)
	}
	resp.Body.Close()

	// A different key under the same hostname: hard reject.
	cert2 := selfSignedCert(t)
	ts2 := httptest.NewUnstartedServer(http.NotFoundHandler())
	ts2.TLS = &tls.Config{Certificates: []tls.Certificate{cert2}}
	ts2.StartTLS()
	defer ts2.Close()
	client2 := &http.Client{Transport: &http.Transport{TLSClientConfig: PinnedTLSConfig(pin)}, Timeout: 5 * time.Second}
	if _, err := client2.Get(ts2.URL); err == nil {
		t.Fatal("mismatched pin accepted")
	}
}

func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: mustLeaf(der)}
}

func jsonDecode(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

type staticProvider struct{ creds Credentials }

func (s *staticProvider) Credentials(context.Context, string, *url.URL) (Credentials, error) {
	return s.creds, nil
}

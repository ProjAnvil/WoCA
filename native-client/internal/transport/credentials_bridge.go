package transport

// BridgeCredentials authenticates against a woca-bridge login surface
// (product design §2.2 channel 1): POST /api/auth/login with the API key
// exchanges for a Bearer session token. Endpoints without the bridge
// login route (enterprise DPoP deployments) fall through to the DPoP
// provider, so one client serves both flavors.
//
// The API key is sourced lazily (OS keychain upstream) and never logged;
// the session token lives in memory only.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrAuthorizationRequired signals the UI to ask for the key.
var ErrAuthorizationRequired = fmt.Errorf("authorization required")

// BridgeCredentials implements CredentialProvider over the bridge login.
type BridgeCredentials struct {
	// BaseURL is the bridge origin this provider serves.
	BaseURL string
	// APIKey supplies the login key lazily (nil → authorization required).
	APIKey func() (string, error)
	// DPoP is the fallback provider for enterprise endpoints (optional).
	DPoP CredentialProvider
	// Client is the HTTP client (TOFU-pinned TLS possible).
	Client *http.Client

	mu    sync.Mutex
	token string
}

// Credentials returns a Bearer session token, logging in when needed.
func (b *BridgeCredentials) Credentials(ctx context.Context, _ string, target *url.URL) (Credentials, error) {
	if target == nil || !b.serves(target) {
		if b.DPoP != nil {
			return b.DPoP.Credentials(ctx, "", target)
		}
		return Credentials{}, ErrAuthorizationRequired
	}
	b.mu.Lock()
	tok := b.token
	b.mu.Unlock()
	if tok != "" {
		return Credentials{Scheme: "Bearer", Session: tok}, nil
	}
	if b.APIKey == nil {
		return Credentials{}, ErrAuthorizationRequired
	}
	key, err := b.APIKey()
	if err != nil || strings.TrimSpace(key) == "" {
		return Credentials{}, ErrAuthorizationRequired
	}
	client := b.Client
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	body, _ := json.Marshal(map[string]string{"api_key": key})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.BaseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return Credentials{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
		var out struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<10)).Decode(&out); err != nil || out.Token == "" {
			return Credentials{}, ErrAuthorizationRequired
		}
		b.mu.Lock()
		b.token = out.Token
		b.mu.Unlock()
		return Credentials{Scheme: "Bearer", Session: out.Token}, nil
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed:
		// Not a bridge login surface — enterprise DPoP flow.
		if b.DPoP != nil {
			return b.DPoP.Credentials(ctx, "", target)
		}
		return Credentials{}, ErrAuthorizationRequired
	case resp.StatusCode == http.StatusUnauthorized:
		// The key is wrong or revoked — re-ask (keys are single-source
		// identity; no silent retry).
		return Credentials{}, ErrAuthorizationRequired
	default:
		return Credentials{}, fmt.Errorf("bridge login: gateway returned %d", resp.StatusCode)
	}
}

func (b *BridgeCredentials) serves(target *url.URL) bool {
	base, err := url.Parse(b.BaseURL)
	if err != nil {
		return false
	}
	return strings.EqualFold(target.Scheme, base.Scheme) && strings.EqualFold(target.Host, base.Host)
}

// Reset drops the cached session (logout).
func (b *BridgeCredentials) Reset() {
	b.mu.Lock()
	b.token = ""
	b.mu.Unlock()
}

// TOFUPinError reports an untrusted self-signed endpoint: the fingerprint
// is what the user confirms against the bridge startup log (product
// design §2.2).
type TOFUPinError struct {
	Host        string
	Fingerprint string
	Stored      string // the previous pin when a pin CHANGED (hard reject)
}

func (e *TOFUPinError) Error() string {
	if e.Stored != "" {
		return fmt.Sprintf("certificate fingerprint changed for %s (was %s, now %s) — possible interception", e.Host, e.Stored[:12]+"…", e.Fingerprint[:12]+"…")
	}
	return fmt.Sprintf("untrusted certificate for %s — fingerprint %s", e.Host, e.Fingerprint)
}

// PinnedTLSConfig builds a client config that verifies the peer against
// the pinned fingerprint only (self-signed/private-CA bridges).
func PinnedTLSConfig(fingerprint string) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: true, // the pin IS the verification
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			return verifyPinnedPeer(rawCerts, fingerprint)
		},
		MinVersion: tls.VersionTLS12,
	}
}

// verifyPinnedPeer checks the leaf SPKI pin against fingerprint.
func verifyPinnedPeer(rawCerts [][]byte, fingerprint string) error {
	if len(rawCerts) == 0 {
		return fmt.Errorf("no peer certificate")
	}
	cert, err := x509.ParseCertificate(rawCerts[0])
	if err != nil {
		return err
	}
	got := fmt.Sprintf("%x", sha256.Sum256(cert.RawSubjectPublicKeyInfo))
	if !strings.EqualFold(got, fingerprint) {
		return fmt.Errorf("pinned fingerprint mismatch")
	}
	return nil
}

// SPKIFingerprint returns the hex SPKI pin of a certificate — the value
// TOFU dialogs display and stores.
func SPKIFingerprint(cert *x509.Certificate) string {
	return fmt.Sprintf("%x", sha256.Sum256(cert.RawSubjectPublicKeyInfo))
}

// mustLeaf parses the leaf certificate (test helper home).
func mustLeaf(der []byte) *x509.Certificate {
	c, _ := x509.ParseCertificate(der)
	return c
}

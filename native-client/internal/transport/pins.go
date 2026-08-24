// pins.go is the native client's bridge-certificate pin store (product
// design §2.2 TOFU): the first successful TLS connection to a remote
// bridge records its SPKI digest; every later connection verifies it and
// a change is a hard reject ("possible interception"). Auto-TOFU keeps
// the zero-dialog experience for public-CA bridges — normal chain
// verification still runs first, the pin adds host continuity on top.
// Self-signed first connections still fail chain verification; the
// confirm-dialog onboarding for those is the M2 native-auth plan.
package transport

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// PinStore persists per-host SPKI pins (pins.json under the native data
// dir — pins are public material, not secrets).
type PinStore struct {
	mu   sync.Mutex
	path string
	pins map[string]string
}

// OpenPinStore loads (or creates) the pin store rooted at dataDir.
func OpenPinStore(dataDir string) (*PinStore, error) {
	p := &PinStore{
		path: filepath.Join(dataDir, "pins.json"),
		pins: map[string]string{},
	}
	if data, err := os.ReadFile(p.path); err == nil {
		_ = json.Unmarshal(data, &p.pins)
	}
	return p, nil
}

// Pin returns the stored fingerprint for a host ("" when unpinned).
func (p *PinStore) Pin(host string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pins[host]
}

// SetPin records a fingerprint.
func (p *PinStore) SetPin(host, fingerprint string) error {
	p.mu.Lock()
	p.pins[host] = fingerprint
	data, _ := json.MarshalIndent(p.pins, "", "  ")
	err := os.WriteFile(p.path, data, 0o600)
	p.mu.Unlock()
	return err
}

// transport wraps base (nil clones the default) with TOFU enforcement:
// record on first handshake, hard-reject on any later change.
func (p *PinStore) transport(base *http.Transport) *http.Transport {
	var t *http.Transport
	if base == nil {
		t = http.DefaultTransport.(*http.Transport).Clone()
	} else {
		t = base.Clone()
	}
	tlsCfg := t.TLSClientConfig
	if tlsCfg == nil {
		tlsCfg = &tls.Config{}
	}
	// Wrap any existing verification-in-connection chain: pins compose
	// with (do not replace) normal certificate verification.
	prev := tlsCfg.VerifyConnection
	tlsCfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if prev != nil {
			if err := prev(cs); err != nil {
				return err
			}
		}
		if len(cs.PeerCertificates) == 0 {
			return nil
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
		observed := hex.EncodeToString(sum[:])
		host := pinKeyFor(cs)

		if stored := p.Pin(host); stored != "" {
			if stored != observed {
				return &TOFUPinError{Host: host, Fingerprint: observed, Stored: stored}
			}
			return nil
		}
		_ = p.SetPin(host, observed)
		return nil
	}
	t.TLSClientConfig = tlsCfg
	return t
}

// Client builds an HTTP client whose TLS layer pins through the store.
func (p *PinStore) Client() *http.Client {
	return &http.Client{Transport: p.transport(nil)}
}

// pinKeyFor derives the pin-store key for a connection: SNI name when
// the client sent one (DNS-dialed bridges — the production shape), else
// the certificate's first SAN, else the CN (IP-dialed servers send no
// SNI). Caveat: two distinct TLS servers sharing one SAN string collide
// — acceptable because remote bridges are reached by DNS name.
func pinKeyFor(cs tls.ConnectionState) string {
	if cs.ServerName != "" {
		return cs.ServerName
	}
	leaf := cs.PeerCertificates[0]
	if n := len(leaf.DNSNames); n > 0 {
		return leaf.DNSNames[0]
	}
	if n := len(leaf.IPAddresses); n > 0 {
		return leaf.IPAddresses[n-1].String()
	}
	return leaf.Subject.CommonName
}

// spkiOf is the shared digest helper (tests use it for cross-checks).
func spkiOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return hex.EncodeToString(sum[:])
}

var _ = fmt.Sprintf // keep fmt for future diagnostics without churn

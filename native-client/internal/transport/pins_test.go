package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func pinTestCert(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "bridge-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// TestPinStoreAutoTOFU: the first successful handshake records the SPKI
// pin; the same key keeps working; a different key under the same host
// is a hard reject (product design §2.2).
func TestPinStoreAutoTOFU(t *testing.T) {
	store, err := OpenPinStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	certA := pinTestCert(t, 1)
	certB := pinTestCert(t, 2)
	tsA := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	tsA.TLS = &tls.Config{Certificates: []tls.Certificate{certA}, MinVersion: tls.VersionTLS12}
	tsA.StartTLS()
	defer tsA.Close()

	// First connection: normal verification fails (self-signed CA), so
	// the auto-pinning path needs a chain-trusted server — exercise the
	// pin machinery directly against the pin-verify config instead.
	leaf, _ := x509.ParseCertificate(certA.Certificate[0])
	fpA := spkiOf(leaf)
	if err := store.SetPin("127.0.0.1", fpA); err != nil {
		t.Fatal(err)
	}

	// Same key: handshake succeeds through the pinned transport (the
	// test CA is trusted so normal chain verification passes; the pin
	// then composes on top — exactly the public-CA production shape).
	// Trust BOTH test keys at the chain level, so the rotated-key
	// rejection below exercises the TOFU pin alone (not the chain).
	leafA, _ := x509.ParseCertificate(certA.Certificate[0])
	leafB, _ := x509.ParseCertificate(certB.Certificate[0])
	pool := x509.NewCertPool()
	pool.AddCert(leafA)
	pool.AddCert(leafB)
	tr := store.transport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}})
	pinned := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := pinned.Get(tsA.URL)
	if err != nil {
		t.Fatalf("pinned first request: %v", err)
	}
	resp.Body.Close()

	// A rotated key under the same name: hard reject (chain-trusted —
	// only the pin refuses it).
	tsB := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	tsB.TLS = &tls.Config{Certificates: []tls.Certificate{certB}, MinVersion: tls.VersionTLS12}
	tsB.StartTLS()
	defer tsB.Close()

	if _, err := pinned.Get(tsB.URL); err == nil {
		t.Fatal("rotated certificate accepted (TOFU enforcement missing)")
	}

	// The unpinned client variant (no stored pin) records on first hit
	// and then enforces: a second different-key server must fail.
	fresh, _ := OpenPinStore(t.TempDir())
	recording := &http.Client{Transport: fresh.transport(&http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}), Timeout: 5 * time.Second}
	if resp, err := recording.Get(tsA.URL); err != nil {
		t.Fatalf("first recording request: %v", err)
	} else {
		resp.Body.Close()
	}
	if got := fresh.Pin("127.0.0.1"); got != fpA {
		t.Fatalf("recorded pin = %q, want %q", got, fpA)
	}
	if _, err := recording.Get(tsB.URL); err == nil {
		t.Fatal("pin change accepted on the recording client")
	}
}

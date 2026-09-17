package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// freeAddr reserves and releases a loopback port for the serve test.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestServeRunsAndShutsDown(t *testing.T) {
	db := tempDB(t)
	addr := freeAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- execRelayCmdCtx(t, ctx, "--db", db, "serve", "--addr", addr)
	}()

	// Wait for the listener, then probe the public surface end to end.
	base := "http://" + addr
	var health *http.Response
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(base + "/api/health")
		if err == nil {
			health = resp
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("serve never came up: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	body, _ := io.ReadAll(health.Body)
	_ = health.Body.Close()
	if health.StatusCode != http.StatusOK || !strings.Contains(string(body), `"service":"woca-relay"`) {
		t.Fatalf("health: %d %s", health.StatusCode, body)
	}
	// The SPA and the login surface answer through the full stack.
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("spa root: %d", resp.StatusCode)
	}
	// The audit log rides alongside the database (revocation plane).
	if _, err := os.Stat(db + ".audit.log"); err != nil {
		t.Fatalf("audit log missing: %v", err)
	}

	// Context cancellation is the graceful shutdown path (SIGTERM in
	// production).
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve shutdown: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("serve did not shut down on context cancel")
	}
}

func TestServeValidationFailures(t *testing.T) {
	t.Run("bad log level", func(t *testing.T) {
		if err := execRelayCmd(t, "--db", tempDB(t), "serve", "--log-level", "bogus"); err == nil {
			t.Fatal("bogus log level must fail")
		}
	})
	t.Run("public bind without TLS refuses", func(t *testing.T) {
		// The fail-closed red line (design §7.2) stops startup before any
		// listen happens.
		err := execRelayCmd(t, "--db", tempDB(t), "serve", "--addr", "0.0.0.0:18443")
		if err == nil || !strings.Contains(err.Error(), "refusing to serve plain HTTP") {
			t.Fatalf("public plain-HTTP bind: %v", err)
		}
	})
	t.Run("unopenable database", func(t *testing.T) {
		blocker := filepath.Join(t.TempDir(), "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := execRelayCmd(t, "--db", filepath.Join(blocker, "relay.db"),
			"serve", "--addr", "127.0.0.1:0"); err == nil {
			t.Fatal("unopenable db must fail")
		}
	})
	t.Run("unwritable audit log", func(t *testing.T) {
		db := tempDB(t)
		// The audit target exists as a directory: append-open fails.
		if err := os.MkdirAll(db+".audit.log", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := execRelayCmd(t, "--db", db, "serve", "--addr", "127.0.0.1:0"); err == nil {
			t.Fatal("unwritable audit log must fail")
		}
	})
	t.Run("listen failure", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		err = execRelayCmd(t, "--db", tempDB(t), "serve", "--addr", ln.Addr().String())
		if err == nil || !strings.Contains(err.Error(), "listen") {
			t.Fatalf("occupied port: %v", err)
		}
	})
	t.Run("TLS keypair failure", func(t *testing.T) {
		err := execRelayCmd(t, "--db", tempDB(t), "serve", "--addr", "127.0.0.1:0",
			"--tls-cert", filepath.Join(t.TempDir(), "no.pem"),
			"--tls-key", filepath.Join(t.TempDir(), "no-key.pem"))
		if err == nil || !strings.Contains(err.Error(), "tls") {
			t.Fatalf("missing keypair: %v", err)
		}
	})
}

func TestServeEnvFlagDefaults(t *testing.T) {
	// Container deployments configure via env (parsed at command build).
	t.Setenv("WOCA_RELAY_ADDR", "127.0.0.1:9443")
	t.Setenv("WOCA_TLS_CERT", "/cert.pem")
	t.Setenv("WOCA_TLS_KEY", "/key.pem")
	db := tempDB(t)
	cmd := newServeCmd(&db)
	for flag, want := range map[string]string{
		"addr":     "127.0.0.1:9443",
		"tls-cert": "/cert.pem",
		"tls-key":  "/key.pem",
	} {
		if got, _ := cmd.Flags().GetString(flag); got != want {
			t.Fatalf("env default for --%s: %q, want %q", flag, got, want)
		}
	}
}

func TestServeArgValidation(t *testing.T) {
	if err := execRelayCmd(t, "--db", tempDB(t), "serve", "extra"); err == nil {
		t.Fatal("serve takes no positional args")
	}
}

// writeTestCert generates a throwaway self-signed ECDSA keypair (the
// internal/servefront test pattern) for the TLS serve path.
func writeTestCert(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	var certOut bytes.Buffer
	if err := pem.Encode(&certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certPath, certOut.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var keyOut bytes.Buffer
	if err := pem.Encode(&keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyOut.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestServeTLS(t *testing.T) {
	db := tempDB(t)
	addr := freeAddr(t)
	certPath, keyPath := writeTestCert(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- execRelayCmdCtx(t, ctx, "--db", db, "serve", "--addr", addr,
			"--tls-cert", certPath, "--tls-key", keyPath)
	}()

	// Direct TLS serve marks the session cookie Secure end to end.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, // test cert
	}}
	base := "https://" + addr
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(base + "/api/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("TLS serve never came up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TLS serve shutdown: %v", err)
		}
	case <-time.After(45 * time.Second):
		t.Fatal("TLS serve did not shut down on context cancel")
	}
}

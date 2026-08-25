package servefront

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/agentbuild"
)

func TestServeAPIDrivesClosures(t *testing.T) {
	home := isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	url := fakeOpenAI(t, "hi")
	// An empty claude.json keeps the env apply path clean.
	if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Pick a free port, then hand it to Serve().
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	opts := baseOpts(t, url)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	serveDone := make(chan error, 1)
	go func() { serveDone <- Serve(ctx, opts, addr, true, "") }()

	base := "http://" + addr
	// Serve() answers /api/health from the moment the port is bound; the
	// real handler (and the closures under test) only swaps in once the
	// agent build finishes, so wait for an API route instead.
	healthOK := false
	for i := 0; i < 100; i++ {
		resp, err := http.Get(base + "/api/quota")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				healthOK = true
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !healthOK {
		t.Fatal("server did not come up")
	}

	post := func(path, body string) *http.Response {
		t.Helper()
		resp, err := http.Post(base+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	put := func(path, body string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPut, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	get := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}

	// Closures wired in Serve()/newConsole: quota, thinking, claude-env,
	// settings rebuilds, plugins.
	if resp := get("/api/quota"); resp.StatusCode != http.StatusOK {
		t.Fatalf("quota = %d", resp.StatusCode)
	}
	if resp := post("/api/agent/thinking", `{"budget":0}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("thinking = %d", resp.StatusCode)
	}
	if resp := put("/api/claudeenv", `{"skills":[],"mcp":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("claude-env = %d", resp.StatusCode)
	}
	// The GET side is the scan surface the bridge's .claude import rides
	// on (the bridge serves the same surface via its edition of Serve).
	if resp := get("/api/claudeenv"); resp.StatusCode != http.StatusOK {
		t.Fatalf("claude-env get = %d", resp.StatusCode)
	}
	if resp := put("/api/settings", `{"provider":"openai","memory_engine":"none"}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("settings = %d", resp.StatusCode)
	}
	if resp := get("/api/plugins"); resp.StatusCode != http.StatusOK {
		t.Fatalf("plugins = %d", resp.StatusCode)
	}
	if resp := put("/api/plugins", `{"enabled":[]}`); resp.StatusCode != http.StatusOK {
		t.Fatalf("plugins put = %d", resp.StatusCode)
	}
	// Installs of a real builtin plugin pass validation; the (offline)
	// install fails inside the plugin manager, executing both closure
	// branches.
	_ = post("/api/plugins/browser/install", "{}")
	_ = post("/api/plugins/browser/reinstall", "{}")

	// A run in a different workspace exercises the per-workspace hooks
	// closure wired in buildAgent.
	if resp := post("/api/runs", `{"prompt":"hi","workspace":`+quoteJSONString(t.TempDir())+`}`); resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		t.Fatalf("run = %d", resp.StatusCode)
	}

	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after cancel")
	}
}

func TestServeBuildAgentError(t *testing.T) {
	isolateEnv(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	opts.Provider = "nope"
	if err := Serve(t.Context(), opts, "127.0.0.1:0", true, ""); err == nil {
		t.Fatal("expected buildAgent error")
	}
}

func TestServeOpensBrowserWithFakeOpen(t *testing.T) {
	isolateEnv(t)
	seedSettings(t, [2]string{"memory-engine", "none"})
	fakeOpenBinary(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	if err := Serve(ctx, opts, "127.0.0.1:0", false, ""); err != nil {
		t.Fatal(err)
	}
}

// The serve path rejects apikey auth when no enterprise edition assembly
// is attached (the personal binary): multi-tenant auth is an enterprise
// capability, wired in by woca-server.
func TestServeAPIKeyRequiresEdition(t *testing.T) {
	isolateEnv(t)
	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	if err := Serve(t.Context(), opts, "127.0.0.1:0", true, "apikey"); err == nil {
		t.Fatal("apikey auth accepted without an edition Authn factory")
	}
}

// The web command's subcommands (stop, status, start failure) execute
// against an isolated home.
func TestWebCmdSubcommands(t *testing.T) {
	isolateEnv(t)

	// Status on a clean environment (nothing running).
	status := NewWebCmd()
	status.SetArgs([]string{"status"})
	if err := status.Execute(); err != nil {
		t.Fatalf("status: %v", err)
	}

	// Stop on a clean environment (no-op).
	stop := NewWebCmd()
	stop.SetArgs([]string{"stop"})
	if err := stop.Execute(); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

// The serve command's error path: an invalid auth mode is rejected before
// the agent builds.
func TestServeCmdInvalidAuthMode(t *testing.T) {
	isolateEnv(t)
	cmd := NewServeCmdWithEdition(&agentbuild.Options{}, Edition{})
	cmd.SetArgs([]string{"--auth", "bogus"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("bogus auth mode accepted")
	}
}

// TestServeDirectTLS: WOCA_TLS_CERT/KEY terminate TLS on the listener
// (product design path C — a public bridge without a reverse proxy).
func TestServeDirectTLS(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	if err := generateTestCert(certPath, keyPath); err != nil {
		t.Skipf("cert gen: %v", err)
	}
	isolateEnv(t)
	t.Setenv("WOCA_TLS_CERT", certPath)
	t.Setenv("WOCA_TLS_KEY", keyPath)
	seedSettings(t, [2]string{"memory-engine", "none"})

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	opts := baseOpts(t, fakeOpenAI(t, "hi"))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ready := make(chan string, 1)
	go func() {
		err := Serve(ctx, opts, addr, true, "")
		_ = err
		ready <- "done"
	}()
	// The TLS listener must answer HTTPS once the server is up.
	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}}
	var ok bool
	for i := 0; i < 200; i++ {
		resp, err := client.Get("https://" + addr + "/api/health")
		if err == nil {
			resp.Body.Close()
			ok = resp.StatusCode == http.StatusOK
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-ready
	if !ok {
		t.Fatal("TLS health check never succeeded")
	}
}

// generateTestCert writes a throwaway self-signed ECDSA pair.
func generateTestCert(certPath, keyPath string) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certOut, err := os.Create(certPath)
	if err != nil {
		return err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		return err
	}
	if err := certOut.Close(); err != nil {
		return err
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyOut, err := os.Create(keyPath)
	if err != nil {
		return err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		return err
	}
	return keyOut.Close()
}

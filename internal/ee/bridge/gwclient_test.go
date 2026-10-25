package bridge

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// newGWPair spins an entry gateway plus a settings store for the client.
func newGWPair(t *testing.T, version string, cfg map[string]string) (*entrygw.Store, string, *settings.Store, string) {
	t.Helper()
	gwdb, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gwdb.Close() })
	store, err := entrygw.NewStore(gwdb)
	if err != nil {
		t.Fatal(err)
	}
	// Both planes on one mux — the union the edge presents to bridges.
	mux := http.NewServeMux()
	entrygw.NewRegistryHandler(store).RegisterRoutes(mux)
	entrygw.NewConfigHandler(store, func(context.Context, string) (string, map[string]string, error) {
		return version, cfg, nil
	}).RegisterRoutes(mux)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	home := t.TempDir()
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return store, ts.URL, st, home
}

func TestGatewayClientEnrollPullApply(t *testing.T) {
	store, url, st, home := newGWPair(t, "gen-1", map[string]string{
		"provider":          "openai",
		"model":             "gpt-test",
		"model-whitelist":   "gpt-test,gpt-mini",
		"anthropic-api-key": "sk-ant-managed",
	})
	enroll, err := store.IssueEnrollment(t.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}

	g := NewGatewayClient(GatewayConfig{
		URL: url, EnrollmentToken: enroll,
		StatePath: filepath.Join(home, "bridge-gateway.json"),
	}, zerolog.Nop())

	if err := g.Enroll(t.Context(), enroll); err != nil {
		t.Fatal(err)
	}
	cfg, err := g.Pull(t.Context())
	if err != nil || cfg == nil {
		t.Fatalf("pull = %v %v", cfg, err)
	}
	if err := applyManaged(st, cfg.Settings); err != nil {
		t.Fatal(err)
	}

	// The managed overlay wins over DB defaults and locks the fields.
	if got := st.Load(); got.Provider != "openai" || got.ModelWhitelist != "gpt-test,gpt-mini" {
		t.Fatalf("managed load = %+v", got)
	}
	// Secrets land in the encrypted store, never in the overlay struct.
	if st.GetSecret(settings.SecretAnthropicAPIKey) != "sk-ant-managed" {
		t.Fatal("managed secret not stored")
	}
	// Writes to managed fields are rejected by the overlay lock.
	bad := st.Load()
	bad.Provider = "anthropic"
	if err := st.Save(bad); err == nil {
		t.Fatal("managed field write accepted")
	}

	// Unchanged version: 304 → (nil, nil), not a cache clear.
	g2status := g.Status()
	if g2status.Stale {
		t.Fatal("fresh pull marked stale")
	}
}

// TestGatewayClientStorageMemoryDelivery: the private-cloud storage domain
// rides the package — split postgres coordinates and the remote ladyM
// endpoint land in the overlay, their passwords in the encrypted store,
// and the enterprise plugin set locks builtin-plugins. Without this wire
// a "backend: postgres / engine: remote" delivery leaves the bridge with
// nowhere to connect.
func TestGatewayClientStorageMemoryDelivery(t *testing.T) {
	store, url, st, home := newGWPair(t, "gen-1", map[string]string{
		"checkpoint-backend":     "postgres",
		"memory-backend":         "postgres",
		"memory-engine":          "remote",
		"checkpoint-pg-host":     "pg.corp.example",
		"checkpoint-pg-port":     "5432",
		"checkpoint-pg-db":       "woca_checkpoints",
		"checkpoint-pg-user":     "woca",
		"checkpoint-pg-password": "cp-secret",
		"memory-pg-host":         "pg.corp.example",
		"memory-pg-port":         "5433",
		"memory-pg-db":           "woca_memory",
		"memory-pg-user":         "woca",
		"memory-pg-password":     "mem-secret",
		"ladym-endpoint":         "https://ladym.corp.example",
		"ladym-api-username":     "bridge",
		"ladym-api-password":     "lm-secret",
		"builtin-plugins":        `["browser","pdf"]`,
		"langfuse-enabled":       "true",
		"langfuse-base-url":      "https://lf.corp.example",
		"langfuse-public-key":    "pk-lf",
		"langfuse-secret-key":    "lf-secret",
	})
	enroll, err := store.IssueEnrollment(t.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	g := NewGatewayClient(GatewayConfig{
		URL: url, EnrollmentToken: enroll,
		StatePath: filepath.Join(home, "bridge-gateway.json"),
	}, zerolog.Nop())
	if err := g.Enroll(t.Context(), enroll); err != nil {
		t.Fatal(err)
	}
	cfg, err := g.Pull(t.Context())
	if err != nil || cfg == nil {
		t.Fatalf("pull = %v %v", cfg, err)
	}
	if err := applyManaged(st, cfg.Settings); err != nil {
		t.Fatal(err)
	}

	got := st.Load()
	if got.CheckpointPGHost != "pg.corp.example" || got.CheckpointPGDB != "woca_checkpoints" {
		t.Fatalf("checkpoint pg overlay = %+v", got)
	}
	if got.MemoryPGHost != "pg.corp.example" || got.MemoryPGPort != "5433" {
		t.Fatalf("memory pg overlay = %+v", got)
	}
	if got.LadyMEndpoint != "https://ladym.corp.example" || got.LadyMAPIUsername != "bridge" {
		t.Fatalf("ladym overlay = %+v", got)
	}
	if len(got.BuiltinPlugins) != 2 || got.BuiltinPlugins[0] != "browser" {
		t.Fatalf("builtin plugins overlay = %v", got.BuiltinPlugins)
	}
	if !got.LangfuseEnabled || got.LangfuseBaseURL != "https://lf.corp.example" || got.LangfusePublicKey != "pk-lf" {
		t.Fatalf("langfuse overlay = %+v", got)
	}
	for secret, want := range map[string]string{
		settings.SecretCheckpointPGPassword: "cp-secret",
		settings.SecretMemoryPGPassword:     "mem-secret",
		settings.SecretLadyMAPIPassword:     "lm-secret",
		settings.SecretLangfuseSecretKey:    "lf-secret",
	} {
		if st.GetSecret(secret) != want {
			t.Errorf("secret %s not delivered", secret)
		}
	}
	// The overlay locks what it manages; the generation fingerprint moves
	// so the settings watcher rebuilds the memory engine even when no
	// secrets row changed between pulls.
	if fields := st.ManagedFields(); !slices.Contains(fields, "checkpoint-pg-host") ||
		!slices.Contains(fields, "ladym-endpoint") || !slices.Contains(fields, "builtin-plugins") ||
		!slices.Contains(fields, "langfuse-enabled") {
		t.Fatalf("managed fields = %v", fields)
	}
	gen := st.Generation()
	st.SetManaged(settings.Settings{CheckpointBackend: "postgres", CheckpointPGHost: "pg2.corp.example"})
	if st.Generation() == gen {
		t.Fatal("overlay-only change did not move the generation fingerprint")
	}
}

func TestGatewayClientStaleKeepsCache(t *testing.T) {
	store, url, st, home := newGWPair(t, "gen-1", map[string]string{"provider": "openai"})
	state := filepath.Join(home, "bridge-gateway.json")

	// Enroll against the live gateway, then resume the same state against
	// a dead endpoint: pulls fail, STALE is flagged, and the managed
	// overlay (the cache) survives untouched.
	enroll, err := store.IssueEnrollment(t.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	live := NewGatewayClient(GatewayConfig{URL: url, EnrollmentToken: enroll, StatePath: state}, zerolog.Nop())
	if err := live.Enroll(t.Context(), enroll); err != nil {
		t.Fatal(err)
	}
	cfg, err := live.Pull(t.Context())
	if err != nil || cfg == nil {
		t.Fatalf("live pull = %v %v", cfg, err)
	}
	if err := applyManaged(st, cfg.Settings); err != nil {
		t.Fatal(err)
	}

	dead := NewGatewayClient(GatewayConfig{URL: "http://127.0.0.1:1", StatePath: state}, zerolog.Nop())
	if _, err := dead.Pull(t.Context()); err == nil {
		t.Fatal("dead gateway pull succeeded")
	}
	if !dead.Status().Stale {
		t.Fatal("pull failure not marked stale")
	}
	// The cache stays authoritative: the overlay still serves the pulled
	// values and keeps rejecting writes to them.
	if got := st.Load(); got.Provider != "openai" {
		t.Fatalf("overlay cleared on failure: %+v", got)
	}
	bad := st.Load()
	bad.Provider = "anthropic"
	if err := st.Save(bad); err == nil {
		t.Fatal("managed lock lost after failure")
	}
}

// TestGatewayClientInsecureSelfSigned: with the development toggle the
// channel enrolls against a self-signed HTTPS gateway (integration posture);
// without it the same handshake fails.
func TestGatewayClientInsecureSelfSigned(t *testing.T) {
	gwdb, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "gw.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gwdb.Close() })
	store, err := entrygw.NewStore(gwdb)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	entrygw.NewRegistryHandler(store).RegisterRoutes(mux)
	entrygw.NewConfigHandler(store, func(context.Context, string) (string, map[string]string, error) {
		return "v1", map[string]string{"provider": "openai"}, nil
	}).RegisterRoutes(mux)
	ts := httptest.NewUnstartedServer(mux)
	ts.TLS = &tls.Config{Certificates: []tls.Certificate{gwSelfSignedCert(t)}}
	ts.StartTLS()
	defer ts.Close()

	enroll, err := store.IssueEnrollment(t.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	cfg := GatewayConfig{
		URL: ts.URL, EnrollmentToken: enroll,
		StatePath: filepath.Join(home, "bridge-gateway.json"),
	}

	// Verified client: the self-signed handshake fails.
	if err := NewGatewayClient(cfg, zerolog.Nop()).Enroll(t.Context(), enroll); err == nil {
		t.Fatal("self-signed gateway accepted with verification on")
	}

	// Integration posture: a fresh token and the insecure toggle enroll fine.
	enroll2, err := store.IssueEnrollment(t.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.EnrollmentToken = enroll2
	g := NewGatewayClient(cfg, zerolog.Nop())
	g.cfg.Insecure = true
	g.hc = insecureClient()
	if err := g.Enroll(t.Context(), enroll); err != nil {
		t.Fatalf("insecure enroll: %v", err)
	}
	if pulled, err := g.Pull(t.Context()); err != nil || pulled == nil {
		t.Fatalf("insecure pull = %v %v", pulled, err)
	}
}

func insecureClient() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec
	return &http.Client{Timeout: 20 * time.Second, Transport: t}
}

func gwSelfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
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
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

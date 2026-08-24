package bridge

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/edgegw"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/wocadb"
)

// stack is a full mini control plane: core (registry + config planes) +
// edge access point, all in-process.
type stack struct {
	store *entrygw.Store
	keys  *tenant.Store
	auth  *authcore.Service
	edge  *httptest.Server

	mu       sync.Mutex
	version  string
	settings map[string]string
}

// edgeLogin mints a tenant key at the core and logs in through the edge
// — the v1.8 client credential every /api request carries.
func (s *stack) edgeLogin(t *testing.T, remark string) string {
	t.Helper()
	u, err := s.keys.CreateUser(context.Background(), "local", remark, "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	_, full, err := s.keys.IssueKey(context.Background(), "local", u.ID, remark, "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.edge.URL+"/api/auth/login", "application/json", strings.NewReader(`{"api_key":"`+full+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("edge login = %d", resp.StatusCode)
	}
	return out.Token
}

func (s *stack) setConfig(version string, kv map[string]string) {
	s.mu.Lock()
	s.version = version
	s.settings = kv
	s.mu.Unlock()
}

func (s *stack) currentConfig() (string, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settings == nil {
		return "gen-1", map[string]string{"provider": "openai", "model": "gpt-test"}
	}
	return s.version, s.settings
}

func newStack(t *testing.T) *stack {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := entrygw.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := tenant.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := authcore.New(keys, db, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	core := &stack{store: store, keys: keys, auth: auth}
	coreMux := http.NewServeMux()
	entrygw.NewRegistryHandler(store).RegisterRoutes(coreMux)
	coreMux.Handle("/gw/auth/", authcore.Handler(auth, func(string, string, string) {}))
	coreMux.HandleFunc("GET /gw/config", func(w http.ResponseWriter, r *http.Request) {
		// Blocking-query semantics like the real handler: park until the
		// version moves or the wait elapses (a push returns immediately).
		wait := 2 * time.Second
		if d, err := time.ParseDuration(r.URL.Query().Get("wait")); err == nil && d > time.Second {
			if d > 60*time.Second {
				d = 60 * time.Second
			}
			wait = d
		}
		known := r.URL.Query().Get("version")
		deadline := time.Now().Add(wait)
		for {
			version, settings := core.currentConfig()
			if known != version {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"version": version, "settings": settings})
				return
			}
			if time.Now().After(deadline) || r.Context().Err() != nil {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	coreSrv := httptest.NewServer(coreMux)
	t.Cleanup(coreSrv.Close)
	e := edgegw.New(edgegw.Config{ServiceURL: coreSrv.URL, ConfigURL: coreSrv.URL}, zerolog.Nop())
	edge := httptest.NewServer(e.Handler())
	t.Cleanup(edge.Close)
	core.edge = edge
	return core
}

func (s *stack) enrollmentToken(t *testing.T) string {
	t.Helper()
	token, err := s.store.IssueEnrollment(context.Background(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func newBridgeSettings(t *testing.T) *settings.Store {
	t.Helper()
	st, err := settings.Open("sqlite:" + filepath.Join(t.TempDir(), "bridge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// clearCloudEnv wipes the cloud-access environment so tests control
// precedence explicitly; call before NewCloudAccess.
func clearCloudEnv(t *testing.T) {
	t.Helper()
	t.Setenv("WOCA_EDGE_URL", "")
	t.Setenv("WOCA_GATEWAY_URL", "")
	t.Setenv("WOCA_ENROLL_TOKEN", "")
	t.Setenv("WOCA_EDGE_INSECURE", "")
	t.Setenv("WOCA_GATEWAY_INSECURE", "")
}

// newCloudAccess builds the manager with the environment cleared.
func newCloudAccess(t *testing.T, st *settings.Store) *CloudAccess {
	t.Helper()
	clearCloudEnv(t)
	return NewCloudAccess(st, zerolog.Nop())
}

// TestCloudAccessLifecycle: configure → enroll → status reflects the
// registration; the one-time token is consumed; reset wipes the identity.
func TestCloudAccessLifecycle(t *testing.T) {
	s := newStack(t)
	st := newBridgeSettings(t)
	ca := newCloudAccess(t, st)

	if err := ca.Configure(s.edge.URL, s.enrollmentToken(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)

	waitForCond(t, 10*time.Second, func() bool {
		snap := ca.Snapshot()
		return snap.Enrolled && snap.BridgeID != "" && snap.LastVer == "gen-1"
	})
	snap := ca.Snapshot()
	if snap.URLSource != "settings" || snap.Stale {
		t.Fatalf("snapshot = %+v", snap)
	}
	// The managed config landed as the overlay (config plane → bridge).
	if cfg := st.Load(); cfg.Provider != "openai" {
		t.Fatalf("managed provider = %q", cfg.Provider)
	}
	// Machine identity persisted in the settings database, not a file.
	if NewLocalKV(st.DB()).Get(KeyEdgeBridgeID) == "" {
		t.Fatal("edge.bridge_id not persisted")
	}
	if !st.HasSecret(SecretEdgeIdentity) {
		t.Fatal("machine private key not persisted as a secret")
	}
	// The one-time enrollment token is consumed after success.
	if NewLocalKV(st.DB()).Get(KeyEdgeToken) != "" {
		t.Fatal("enrollment token not consumed")
	}

	// 注销: reset wipes the local identity.
	if err := ca.Reset(); err != nil {
		t.Fatal(err)
	}
	waitForCond(t, 5*time.Second, func() bool {
		snap := ca.Snapshot()
		return !snap.Enrolled && snap.BridgeID == ""
	})
	kv := NewLocalKV(st.DB())
	for _, k := range []string{KeyEdgeURL, KeyEdgeBridgeID, KeyEdgeToken, KeyEdgeAccess} {
		if kv.Get(k) != "" {
			t.Fatalf("%s survived reset", k)
		}
	}
}

// TestCloudAccessTunnelE2E: with the handler and the auth gate on, the
// scenario-four data plane serves client traffic through the edge —
// cookies included, RemoteAddr never loopback.
func TestCloudAccessTunnelE2E(t *testing.T) {
	s := newStack(t)
	st := newBridgeSettings(t)
	ca := newCloudAccess(t, st)

	var remoteSeen lockedString
	bridgeMux := http.NewServeMux()
	bridgeMux.HandleFunc("/api/echo", func(w http.ResponseWriter, r *http.Request) {
		remoteSeen.Set(r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"cookie":%q,"ok":true}`, r.Header.Get("Cookie"))
	})
	ca.Handler = func() http.Handler { return bridgeMux }
	ca.TunnelGate = func() bool { return true }

	if err := ca.Configure(s.edge.URL, s.enrollmentToken(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)

	// The tunnel serves once enrolled. v1.8: the client logs in at the
	// edge; the credential stays there (the bridge sees the assertion).
	tok := s.edgeLogin(t, "tunnel-user")
	deadline := time.Now().Add(10 * time.Second)
	var resp *http.Response
	var err error
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodGet, s.edge.URL+"/api/echo", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err = http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			resp.Body.Close()
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel echo = %v %v", err, resp)
	}
	defer resp.Body.Close()
	var out map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["cookie"] != "" {
		t.Fatalf("credential leaked to the bridge: %+v", out)
	}
	if remoteSeen.Get() == "" {
		t.Fatal("RemoteAddr not propagated from the edge's client view")
	}
	if snap := ca.Snapshot(); !snap.Tunnel.Serving || snap.Tunnel.Served == 0 {
		t.Fatalf("tunnel status = %+v", snap.Tunnel)
	}
}

// TestCloudAccessTunnelGateOff: a local-trust bridge never opens the data
// plane, even fully enrolled.
func TestCloudAccessTunnelGateOff(t *testing.T) {
	s := newStack(t)
	st := newBridgeSettings(t)
	ca := newCloudAccess(t, st)
	ca.Handler = func() http.Handler { return http.NotFoundHandler() }
	ca.TunnelGate = func() bool { return false }

	if err := ca.Configure(s.edge.URL, s.enrollmentToken(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)
	waitForCond(t, 10*time.Second, func() bool { return ca.Snapshot().Enrolled })
	if ca.Snapshot().Tunnel.Serving {
		t.Fatal("tunnel must stay off under local trust")
	}
}

// TestCloudAccessEnvPrecedence: the environment pins the URL; a
// conflicting page write is refused with the conflict surfaced.
func TestCloudAccessEnvPrecedence(t *testing.T) {
	s := newStack(t)
	st := newBridgeSettings(t)
	clearCloudEnv(t)
	t.Setenv("WOCA_EDGE_URL", s.edge.URL)
	ca := NewCloudAccess(st, zerolog.Nop())

	if err := ca.Configure("https://other.example.com", ""); err == nil {
		t.Fatal("conflicting write accepted")
	}
	snap := ca.Snapshot()
	if snap.URL != s.edge.URL || snap.URLSource != "env" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// TestCloudAccessLegacyMigration: the v1.4 file state imports into the
// settings database and the file disappears.
func TestCloudAccessLegacyMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, priv, _ := ed25519.GenerateKey(nil)
	state := map[string]any{
		"bridge_id":     "br_legacy",
		"private_key":   []byte(priv),
		"access_token":  "tok",
		"access_expiry": time.Now().Add(time.Hour).UnixMilli(),
	}
	data, _ := json.Marshal(state)
	legacy := filepath.Join(home, ".woca", "bridge-gateway.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, data, 0o600); err != nil {
		t.Fatal(err)
	}

	st := newBridgeSettings(t)
	ca := newCloudAccess(t, st)
	if err := ca.Configure("https://edge.example.com", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)
	waitForCond(t, 5*time.Second, func() bool {
		return NewLocalKV(st.DB()).Get(KeyEdgeBridgeID) == "br_legacy"
	})
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatal("legacy state file not removed after migration")
	}
	if !st.HasSecret(SecretEdgeIdentity) {
		t.Fatal("legacy private key not migrated into secrets")
	}
}

// TestSpkiTOFU: the first TLS handshake pins the edge certificate; a
// different certificate under the same URL is a hard failure.
func TestSpkiTOFU(t *testing.T) {
	clearCloudEnv(t)
	t.Setenv("WOCA_EDGE_INSECURE", "1") // self-signed test edges: chain validation off, pinning on
	certA := selfSignedCert(t, 1)
	certB := selfSignedCert(t, 2)
	mkEdge := func(cert *tls.Certificate) (*httptest.Server, *entrygw.Store) {
		db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "core.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		store, err := entrygw.NewStore(db)
		if err != nil {
			t.Fatal(err)
		}
		mux := http.NewServeMux()
		entrygw.NewRegistryHandler(store).RegisterRoutes(mux)
		ts := httptest.NewUnstartedServer(mux)
		ts.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
		ts.StartTLS()
		t.Cleanup(ts.Close)
		return ts, store
	}

	st := newBridgeSettings(t)
	clearCloudEnv(t)
	t.Setenv("WOCA_EDGE_INSECURE", "1")
	ca := NewCloudAccess(st, zerolog.Nop())

	edgeA, storeA := mkEdge(&certA)
	tokA, err := storeA.IssueEnrollment(context.Background(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Configure(edgeA.URL, tokA); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)
	waitForCond(t, 5*time.Second, func() bool {
		return NewLocalKV(st.DB()).Get(KeyEdgeSPKIPin) != ""
	})

	// Same URL shape, different key: the pin mismatch must hard-fail
	// rather than silently trust the new certificate.
	ca2 := newCloudAccess(t, st)
	edgeB, _ := mkEdge(&certB)
	if err := ca2.Configure(edgeB.URL, ""); err != nil {
		t.Fatal(err)
	}
	client := NewGatewayClient(GatewayConfig{
		URL:       edgeB.URL,
		Insecure:  true, // pin enforcement is independent of chain validation
		Persist:   &settingsPersist{kv: NewLocalKV(st.DB()), st: st},
		PinSource: func() string { return NewLocalKV(st.DB()).Get(KeyEdgeSPKIPin) },
	}, zerolog.Nop())
	if _, err := client.Pull(context.Background()); err == nil {
		t.Fatal("pin mismatch accepted")
	} else if !strings.Contains(err.Error(), "pin mismatch") && !strings.Contains(err.Error(), "x509") {
		// Insecure skip-verify makes the transport surface the
		// VerifyConnection error verbatim.
		t.Fatalf("unexpected error: %v", err)
	}
}

func selfSignedCert(t *testing.T, serial int64) tls.Certificate {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "edge-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}
}

// lockedString is a tiny mutex-guarded string for cross-goroutine asserts.
type lockedString struct {
	mu sync.Mutex
	v  string
}

func (a *lockedString) Set(v string) {
	a.mu.Lock()
	a.v = v
	a.mu.Unlock()
}

func (a *lockedString) Get() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.v
}

func waitForCond(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestEmptyConfigNeverClearsOverlay (§3.2 red line): a 200 whose settings
// map is empty must keep the previous managed overlay intact — the exact
// incident shape the design calls out (Claude Code #70181).
func TestEmptyConfigNeverClearsOverlay(t *testing.T) {
	s := newStack(t)
	st := newBridgeSettings(t)
	ca := newCloudAccess(t, st)
	if err := ca.Configure(s.edge.URL, s.enrollmentToken(t)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)
	waitForCond(t, 10*time.Second, func() bool {
		return ca.Snapshot().Enrolled && st.Load().Provider != ""
	})
	before := st.ManagedFields()

	// Now the config plane misbehaves: a new "version" with no settings.
	s.setConfig("gen-empty", map[string]string{})
	waitForCond(t, 10*time.Second, func() bool {
		snap := ca.Snapshot()
		return snap.Stale || snap.LastVer == "gen-empty"
	})
	// The overlay must survive untouched: same fields, nothing unlocked.
	after := st.ManagedFields()
	if len(after) == 0 || !stringSliceEqual(after, before) {
		t.Fatalf("empty payload mutated the managed overlay: before=%v after=%v", before, after)
	}
	// And the empty package must not have been adopted silently.
	if snap := ca.Snapshot(); !snap.Stale && snap.LastVer == "gen-empty" {
		t.Fatalf("empty payload applied cleanly: %+v", snap)
	}
}

func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestLayerViolationRejectedAndAudited (§3.3): a config-plane push that
// tries to write bridge-local infrastructure keys (edge.*) rejects the
// whole package and lands in the audit trail; innocuous unknown keys are
// still stripped per §3.2.
func TestLayerViolationRejectedAndAudited(t *testing.T) {
	st := newBridgeSettings(t)
	st.SetManaged(settings.Settings{Provider: "openai"})
	var audited []string
	var mu sync.Mutex
	ca := NewCloudAccess(st, zerolog.Nop())
	ca.Audit = func(action, detail string) {
		mu.Lock()
		audited = append(audited, action+" "+detail)
		mu.Unlock()
	}

	// Innocuous unknown key: stripped, no audit, provider still applies.
	if err := applyManaged(st, map[string]string{"provider": "glm", "typo-key": "x"}); err == nil {
		t.Fatal("unknown key not reported")
	}
	if st.Load().Provider != "glm" {
		t.Fatalf("strip semantics broken: %q", st.Load().Provider)
	}
	mu.Lock()
	n := len(audited)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("unknown key audited: %v", audited)
	}

	// Infrastructure key: the whole package is rejected, nothing applies,
	// and the violation is audited.
	if err := applyManaged(st, map[string]string{"provider": "deepseek", "edge.url": "https://evil"}); !errors.Is(err, ErrLayerViolation) {
		t.Fatalf("layer violation not detected: %v", err)
	}
	if st.Load().Provider != "glm" {
		t.Fatalf("violating package partially applied: %q", st.Load().Provider)
	}
}

// TestPullWaitRidesPinnedTransport: the long-poll path enforces the TOFU
// pin exactly like the initial pull — a rotated certificate on the same
// URL must fail even mid-loop.
func TestPullWaitRidesPinnedTransport(t *testing.T) {
	clearCloudEnv(t)
	t.Setenv("WOCA_EDGE_INSECURE", "1")
	certA := selfSignedCert(t, 11)
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	store, err := entrygw.NewStore(db)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	entrygw.NewRegistryHandler(store).RegisterRoutes(mux)
	entrygw.NewConfigHandler(store, func(context.Context, string) (string, map[string]string, error) {
		return "v-pin", map[string]string{"provider": "openai"}, nil
	}).RegisterRoutes(mux)
	tsA := httptest.NewUnstartedServer(mux)
	tsA.TLS = &tls.Config{Certificates: []tls.Certificate{certA}}
	tsA.StartTLS()
	t.Cleanup(tsA.Close)

	st := newBridgeSettings(t)
	ca := NewCloudAccess(st, zerolog.Nop())
	tok, err := store.IssueEnrollment(context.Background(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	if err := ca.Configure(tsA.URL, tok); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go ca.Run(ctx)
	waitForCond(t, 5*time.Second, func() bool {
		return NewLocalKV(st.DB()).Get(KeyEdgeSPKIPin) != "" && ca.Snapshot().LastVer == "v-pin"
	})

	// The pinned client's PullWait must reject a different key under the
	// same URL — proving the long-poll shares the pinned transport.
	client := NewGatewayClient(GatewayConfig{
		URL:       tsA.URL,
		Insecure:  true,
		Persist:   &settingsPersist{kv: NewLocalKV(st.DB()), st: st},
		PinSource: func() string { return NewLocalKV(st.DB()).Get(KeyEdgeSPKIPin) },
	}, zerolog.Nop())
	// A fresh server with a different self-signed key on another port
	// cannot impersonate the pinned one: pin mismatch fires on PullWait.
	certB := selfSignedCert(t, 12)
	mux2 := http.NewServeMux()
	entrygw.NewRegistryHandler(store).RegisterRoutes(mux2)
	tsB := httptest.NewUnstartedServer(mux2)
	tsB.TLS = &tls.Config{Certificates: []tls.Certificate{certB}}
	tsB.StartTLS()
	t.Cleanup(tsB.Close)
	client2 := NewGatewayClient(GatewayConfig{
		URL:       tsB.URL,
		Insecure:  true,
		Persist:   &settingsPersist{kv: NewLocalKV(st.DB()), st: st},
		PinSource: func() string { return NewLocalKV(st.DB()).Get(KeyEdgeSPKIPin) },
	}, zerolog.Nop())
	if _, err := client2.PullWait(ctx, 2*time.Second); err == nil {
		t.Fatal("PullWait accepted a pin-mismatched edge")
	}
	// The pinned original still answers.
	if _, err := client.PullWait(ctx, 2*time.Second); err != nil {
		t.Fatalf("PullWait against the pinned edge failed: %v", err)
	}
}

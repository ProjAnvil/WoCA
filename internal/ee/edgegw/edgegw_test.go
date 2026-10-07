package edgegw

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/authcore"
	"github.com/projanvil/woca/internal/ee/bridge"
	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/ee/nativeauth"
	"github.com/projanvil/woca/internal/ee/tenant"
	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/internal/wocadb"
)

// fakeCore mounts the real registry handler (service plane) plus a stub
// config plane over the shared sqlite store — the private core the edge
// calls home to.
type fakeCore struct {
	store    *entrygw.Store
	auth     *authcore.Service
	keys     *tenant.Store
	svc      *httptest.Server
	cfg      *httptest.Server
	mu       sync.Mutex
	pulls    int
	authDown bool
}

func newFakeCore(t *testing.T) *fakeCore {
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
	svcMux := http.NewServeMux()
	entrygw.NewRegistryHandler(store).RegisterRoutes(svcMux)
	// The native credential machine channel (what woca-server mounts
	// beside authcore on its gateway listener).
	svcMux.Handle("/gw/native/", nativeauth.MachineHandler(nativeauth.New(keys, tenant.NewAPIKeyProvider(keys))))
	svc := httptest.NewServer(svcMux)
	t.Cleanup(svc.Close)
	core := &fakeCore{store: store, auth: auth, keys: keys, svc: svc}
	// The auth routes can be failed independently (outage-degradation
	// tests keep the machine registry alive — the design's degradation
	// model concerns the identity service, not the whole core).
	svcMux.Handle("/gw/auth/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		core.mu.Lock()
		down := core.authDown
		core.mu.Unlock()
		if down {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "core auth down"})
			return
		}
		authcore.Handler(auth, func(string, string, string) {}).ServeHTTP(w, r)
	}))
	cfgMux := http.NewServeMux()
	cfgMux.HandleFunc("GET /gw/config", func(w http.ResponseWriter, _ *http.Request) {
		core.mu.Lock()
		core.pulls++
		core.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"version": "gen-1", "settings": map[string]string{"provider": "glm"}})
	})
	cfg := httptest.NewServer(cfgMux)
	t.Cleanup(cfg.Close)
	core.cfg = cfg
	return core
}

func newEdge(t *testing.T, core *fakeCore) *httptest.Server {
	t.Helper()
	e := New(Config{ServiceURL: core.svc.URL, ConfigURL: core.cfg.URL}, zerolog.Nop())
	ts := httptest.NewServer(e.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// enroll registers one bridge through the real registry flow.
func enroll(t *testing.T, core *fakeCore) (access string) {
	t.Helper()
	token, err := core.store.IssueEnrollment(t.Context(), entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	// The enrollment is normally consumed over HTTP; the store path is
	// equivalent for wiring purposes (the public-key checks live in the
	// handler tests).
	pub := []byte("test-public-key")
	bridgeID, err := core.store.ConsumeEnrollment(t.Context(), machineid.HashToken(token), pub, entrygw.EnrollmentTTL)
	if err != nil {
		t.Fatal(err)
	}
	access, _, err = core.store.IssueAccess(t.Context(), bridgeID, entrygw.AccessTTL)
	if err != nil {
		t.Fatal(err)
	}
	return access
}

// beatOnce marks a bridge live in the inventory (selection/routing needs
// a fresh heartbeat).
func beatOnce(t *testing.T, core *fakeCore, access string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, core.svc.URL+"/gw/heartbeat", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

// userKey issues a woca_ key through the core's tenant tables and logs
// in through the edge — returning the core session token (the client
// credential for subsequent requests).
func userKey(t *testing.T, edge *httptest.Server, core *fakeCore, remark string) string {
	t.Helper()
	u, err := core.keys.CreateUser(t.Context(), "local", remark, "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	_, full, err := core.keys.IssueKey(t.Context(), "local", u.ID, remark, "tenant-admin")
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(edge.URL+"/api/auth/login", "application/json", strings.NewReader(`{"api_key":"`+full+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusOK || out.Token == "" {
		t.Fatalf("edge login = %d %v", resp.StatusCode, out)
	}
	return out.Token
}

// echoBridge is a bridge-side handler that answers with its identity and
// echoes interesting request properties.
func echoBridge(self string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/who", func(w http.ResponseWriter, r *http.Request) {
		actor := ""
		if a, ok := bridge.ActorFromContext(r.Context()); ok {
			actor = a.Remark + "/" + a.Role
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"bridge": self,
			"method": r.Method,
			"cookie": r.Header.Get("Cookie"),
			"bearer": r.Header.Get("Authorization"),
			"remote": r.RemoteAddr,
			"actor":  actor,
			"body":   func() string { b, _ := io.ReadAll(r.Body); return string(b) }(),
		})
	})
	mux.HandleFunc("/api/cookie", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "s1", Path: "/", HttpOnly: true})
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		rc := http.NewResponseController(w)
		for i := range 3 {
			fmt.Fprintf(w, "event: tick\ndata: %d-%s\n\n", i, self)
			_ = rc.Flush()
			time.Sleep(120 * time.Millisecond)
		}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html>spa of %s</html>", self)
	})
	return mux
}

func runTunnel(ctx context.Context, t *testing.T, core *fakeCore, edgeURL, access string, h http.Handler) *bridge.TunnelClient {
	t.Helper()
	// Routing counts live bridges only — mark this bridge live the way a
	// real bridge's 30s heartbeat does.
	hb, _ := http.NewRequest(http.MethodPost, core.svc.URL+"/gw/heartbeat", strings.NewReader("{}"))
	hb.Header.Set("Authorization", "Bearer "+access)
	if resp, err := http.DefaultClient.Do(hb); err == nil {
		resp.Body.Close()
	}
	tc := bridge.NewTunnelClient(bridge.TunnelConfig{
		EdgeURL: edgeURL,
		Token:   func() string { return access },
		Handler: h,
		Workers: 4,
	}, zerolog.Nop())
	go tc.Run(ctx)
	waitFor(t, 5*time.Second, func() bool { return tc.Status().Serving })
	return tc
}

func waitFor(t *testing.T, d time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// TestTunnelRoundTrip: client traffic parks, rides the tunnel, and comes
// back — method, body, cookies, and bearer intact; Set-Cookie survives
// the return direction; the SPA root serves through too.
func TestTunnelRoundTrip(t *testing.T) {
	parkPickupTimeout = 5 * time.Second
	defer func() { parkPickupTimeout = 30 * time.Second }()
	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge("br_main"))

	// v1.8: the edge is the auth enforcement point — the client logs in
	// HERE (the core validates through the edge's call-home) and every
	// /api request carries the core-issued session.
	tok := userKey(t, edge, core, "alice")

	// GET with the session token: the bridge sees the ASSERTION, never
	// the credential (both stripped at the edge).
	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&who)
	if who["bridge"] != "br_main" || who["cookie"] != "" ||
		who["bearer"] != "" || who["remote"] == "" {
		t.Fatalf("who = %+v", who)
	}
	if who["actor"] != "alice/tenant-admin" {
		t.Fatalf("actor assertion missing: %+v", who)
	}
	if who["remote"] == "127.0.0.1:1" {
		t.Fatal("remote addr must not look loopback-trusted")
	}

	// POST with body.
	req2, _ := http.NewRequest(http.MethodPost, edge.URL+"/api/who", strings.NewReader(`{"hello":"world"}`))
	req2.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	var who2 map[string]string
	_ = json.NewDecoder(resp2.Body).Decode(&who2)
	resp2.Body.Close()
	if who2["method"] != "POST" || !strings.Contains(who2["body"], "world") {
		t.Fatalf("post = %+v", who2)
	}

	// Set-Cookie rides back (bridge-issued cookies, host-only at the
	// edge origin).
	req3, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/cookie", nil)
	req3.Header.Set("Authorization", "Bearer "+tok)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatal(err)
	}
	var cookieOut map[string]string
	_ = json.NewDecoder(resp3.Body).Decode(&cookieOut)
	resp3.Body.Close()
	var found bool
	for _, c := range resp3.Cookies() {
		if c.Name == "session" && c.Value == "s1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("set-cookie lost: %+v", resp3.Cookies())
	}

	// SPA root.
	resp4, err := http.Get(edge.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != 200 || !strings.Contains(string(body4), "spa of br_main") {
		t.Fatalf("spa root = %d %s", resp4.StatusCode, body4)
	}
}

// TestTunnelSSE: a streamed event flow arrives progressively — flushes
// survive the tunnel (the whole point of the respond stream).
func TestTunnelSSE(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge("br_sse"))
	tok := userKey(t, edge, core, "sse-user")

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/stream", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q", ct)
	}
	// The edge is the public entry (design §6.3, same contract the
	// personal relay enforces in internal/relay/forward.go): SSE answers
	// must carry X-Accel-Buffering: no or a buffering proxy in front of
	// the edge stalls the stream.
	if ab := resp.Header.Get("X-Accel-Buffering"); ab != "no" {
		t.Fatalf("x-accel-buffering = %q (want \"no\")", ab)
	}
	buf := make([]byte, 4096)
	var reads []time.Duration
	start := time.Now()
	total := 0
	for total < 3 && time.Since(start) < 5*time.Second {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			reads = append(reads, time.Since(start))
			total += strings.Count(string(buf[:n]), "event: tick")
		}
		if err != nil {
			break
		}
	}
	if total < 3 {
		t.Fatalf("sse events = %d (reads=%v)", total, reads)
	}
	// Progressive delivery: not all three events in the first read.
	if len(reads) < 2 {
		t.Fatalf("sse arrived in one burst: %v", reads)
	}
}

// TestBridgeSelection: multiple bridges → the root renders the picker;
// /edge/select pins the cookie; the header routes without a cookie.
func TestBridgeSelection(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	a1 := enroll(t, core)
	a2 := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	beatOnce(t, core, a1)
	runTunnel(ctx, t, core, edge.URL, a1, echoBridge("br_one"))
	runTunnel(ctx, t, core, edge.URL, a2, echoBridge("br_two"))
	tok := userKey(t, edge, core, "picker")

	// No cookie, two bridges → selection page.
	resp, err := http.Get(edge.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), "选择目标 bridge") {
		t.Fatalf("selection page missing: %s", body[:min(200, len(body))])
	}

	// The pre-auth inventory API lists both.
	list, err := http.Get(edge.URL + "/api/edge/bridges")
	if err != nil {
		t.Fatal(err)
	}
	var bridges []entrygw.BridgeInfo
	_ = json.NewDecoder(list.Body).Decode(&bridges)
	list.Body.Close()
	if len(bridges) != 2 {
		t.Fatalf("bridges = %+v", bridges)
	}

	// Pick via the header (native-client posture): no bridge cookie
	// needed; the session credential still applies.
	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
	first := bridges[0].BridgeID
	req.Header.Set(BridgeHeader, first)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var who map[string]string
	_ = json.NewDecoder(resp2.Body).Decode(&who)
	resp2.Body.Close()
	if who["bridge"] == "" {
		t.Fatal("header routing failed (empty bridge in response)")
	}

	// /edge/select?bridge=<id> sets the cookie and bounces to /.
	client := &http.Client{CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp3, err := client.Get(edge.URL + "/edge/select?bridge=" + first)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != http.StatusSeeOther {
		t.Fatalf("select = %d", resp3.StatusCode)
	}
	var cookieSet bool
	for _, c := range resp3.Cookies() {
		if c.Name == BridgeCookie && c.Value == first {
			cookieSet = true
		}
	}
	if !cookieSet {
		t.Fatalf("select cookie missing: %+v", resp3.Cookies())
	}
}

// TestSingleBridgeAutoRoute: exactly one registered bridge routes without
// any cookie (the zero-friction login flow at the edge).
func TestSingleBridgeAutoRoute(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge("br_solo"))
	tok := userKey(t, edge, core, "solo-user")

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var who map[string]string
	_ = json.NewDecoder(resp.Body).Decode(&who)
	if who["bridge"] != "br_solo" {
		t.Fatalf("auto-route = %+v", who)
	}
}

// TestOfflineBridgeImmediate: a registered-but-silent bridge (no
// heartbeat, no tunnel) is not routable — the edge answers 503
// immediately instead of parking for the pickup timeout (auto-route
// counts live bridges only).
func TestOfflineBridgeImmediate(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	enroll(t, core) // registered but never heartbeated

	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
	req.Header.Set("Authorization", "Bearer "+userKey(t, edge, core, "off-user"))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("offline = %d, want 503", resp.StatusCode)
	}
}

// TestTunnelAuth: the tunnel endpoints only accept live machine tokens —
// resolved at the service plane every time (R7).
func TestTunnelAuth(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	req, _ := http.NewRequest(http.MethodGet, edge.URL+"/tunnel/poll?wait=1s", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous poll = %d", resp.StatusCode)
	}
	req2, _ := http.NewRequest(http.MethodGet, edge.URL+"/tunnel/poll?wait=1s", nil)
	req2.Header.Set("Authorization", "Bearer woca_bogus_token")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus poll = %d", resp2.StatusCode)
	}
}

// TestGwForwarding: machine-channel routes forward into the right plane —
// config pulls to the config plane, everything else to the service plane.
func TestGwForwarding(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)

	resp, err := http.Get(edge.URL + "/gw/config")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var cfg struct {
		Version  string            `json:"version"`
		Settings map[string]string `json:"settings"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&cfg)
	if cfg.Version != "gen-1" || cfg.Settings["provider"] != "glm" {
		t.Fatalf("config forward = %+v", cfg)
	}

	hello, err := http.Get(edge.URL + "/gw/hello")
	if err != nil {
		t.Fatal(err)
	}
	defer hello.Body.Close()
	if hello.StatusCode != http.StatusOK {
		t.Fatalf("hello forward = %d", hello.StatusCode)
	}

	// Core-internal routes never answer on the public listener: resolve
	// is the edge's call-home hook, bridges is its selection inventory —
	// forwarding either would leak the registry and (worse) turn
	// /gw/enroll-status into an anonymous token dispensary.
	for _, path := range []string{"/gw/resolve", "/gw/bridges"} {
		resp, err := http.Get(edge.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("public %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestSweepSparesPickedRequests: a bridge that takes longer than the
// pickup window to produce its first byte (a thinking LLM, a slow disk)
// must not have its parked request murdered by the janitor — the relay
// completes when the answer finally streams.
func TestSweepSparesPickedRequests(t *testing.T) {
	parkPickupTimeout = 200 * time.Millisecond
	firstByteTimeout = 5 * time.Second
	defer func() {
		parkPickupTimeout = 30 * time.Second
		firstByteTimeout = 120 * time.Second
	}()
	core := newFakeCore(t)
	e := New(Config{ServiceURL: core.svc.URL, ConfigURL: core.cfg.URL}, zerolog.Nop())
	edge := httptest.NewServer(e.Handler())
	t.Cleanup(edge.Close)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	slow := http.NewServeMux()
	slow.HandleFunc("/api/slow", func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(700 * time.Millisecond) // > parkPickupTimeout, < firstByteTimeout
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprint(w, "finally")
	})
	runTunnel(ctx, t, core, edge.URL, access, slow)

	// Run the janitor on a fast cadence so the sweep definitely fires
	// inside the slow-TTFB window (the production cadence is 15s).
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				e.sweep()
			}
		}
	}()

	reqSlow, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/slow", nil)
	reqSlow.Header.Set("Authorization", "Bearer "+userKey(t, edge, core, "slow-user"))
	resp, err := http.DefaultClient.Do(reqSlow)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "finally" {
		t.Fatalf("slow relay = %d %q (sweep killed a picked request)", resp.StatusCode, body)
	}
}

// TestRespondIsBridgeScoped: bridge A cannot answer a request parked for
// bridge B — the queue binding comes from the responder's own resolved
// identity, and an unknown request id is 410 Gone.
func TestRespondIsBridgeScoped(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	a1 := enroll(t, core)
	a2 := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	// Routing counts live bridges only and /api/* client traffic needs a
	// session: beat bridge 1 in (bridge 2 stays silent so auto-route picks
	// bridge 1) and park an authenticated client request on its queue.
	beatOnce(t, core, a1)
	tok := userKey(t, edge, core, "scoped-user")
	parked := make(chan *http.Response, 1)
	go func() {
		req, _ := http.NewRequest(http.MethodGet, edge.URL+"/api/who", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			parked <- resp
		}
	}()
	// Poll until the parked request reaches bridge 1's poller — a single
	// fixed-window poll that skips on a miss silently drops every assertion
	// below; a deadline loop fails loudly instead. (The edge rejects waits
	// under a second, so each attempt long-polls for 1s.)
	var payload struct {
		ID string `json:"id"`
	}
	waitFor(t, 2*time.Second, func() bool {
		preq, _ := http.NewRequestWithContext(ctx, http.MethodGet, edge.URL+"/tunnel/poll?wait=1s", nil)
		preq.Header.Set("Authorization", "Bearer "+a1)
		presp, err := http.DefaultClient.Do(preq)
		if err != nil {
			return false
		}
		defer presp.Body.Close()
		if presp.StatusCode != http.StatusOK {
			return false
		}
		_ = json.NewDecoder(presp.Body).Decode(&payload)
		return payload.ID != ""
	})

	// Bridge 2 tries to respond to bridge 1's request: 410, never a relay.
	body := strings.NewReader("hijack")
	rreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, edge.URL+"/tunnel/respond/"+payload.ID, body)
	rreq.Header.Set("Authorization", "Bearer "+a2)
	rreq.Header.Set("X-Woca-Status", "200")
	hijack, err := http.DefaultClient.Do(rreq)
	if err != nil {
		t.Fatal(err)
	}
	hijack.Body.Close()
	if hijack.StatusCode != http.StatusGone {
		t.Fatalf("cross-bridge respond = %d, want 410", hijack.StatusCode)
	}
	// The legitimate bridge still completes the relay — directly: the
	// manual poll above already consumed the pickup, so no tunnel client
	// could receive this request again; the edge only cares that the
	// respond carries bridge 1's token and the parked request's id.
	lreq, _ := http.NewRequestWithContext(ctx, http.MethodPost, edge.URL+"/tunnel/respond/"+payload.ID,
		strings.NewReader(`{"bridge":"br_ok"}`))
	lreq.Header.Set("Authorization", "Bearer "+a1)
	lreq.Header.Set("X-Woca-Status", "200")
	relay, err := http.DefaultClient.Do(lreq)
	if err != nil {
		t.Fatal(err)
	}
	relay.Body.Close()
	if relay.StatusCode != http.StatusOK {
		t.Fatalf("legitimate respond = %d, want 200", relay.StatusCode)
	}
	select {
	case resp := <-parked:
		var who map[string]string
		_ = json.NewDecoder(resp.Body).Decode(&who)
		resp.Body.Close()
		if who["bridge"] != "br_ok" {
			t.Fatalf("relay went to %+v", who)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("legitimate respond never arrived")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestClientBodyLimit: a request too large for the tunnel is shed with
// 413 before any memory is parked beyond the cap.
func TestClientBodyLimit(t *testing.T) {
	prev := clientBodyLimit
	clientBodyLimit = 128
	defer func() { clientBodyLimit = prev }()
	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	runTunnel(ctx, t, core, edge.URL, access, echoBridge("br_cap"))

	reqBig, _ := http.NewRequest(http.MethodPost, edge.URL+"/api/who", strings.NewReader(strings.Repeat("x", 512)))
	reqBig.Header.Set("Content-Type", "application/json")
	reqBig.Header.Set("Authorization", "Bearer "+userKey(t, edge, core, "cap-user"))
	resp, err := http.DefaultClient.Do(reqBig)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body = %d, want 413", resp.StatusCode)
	}
}

// TestSelectCookieAttributes: the selection cookie is host-only,
// HttpOnly, SameSite=Lax, and Secure follows the connection (or the
// operator override).
func TestSelectCookieAttributes(t *testing.T) {
	core := newFakeCore(t)
	edge := newEdge(t, core)
	access := enroll(t, core)
	beatOnce(t, core, access)

	client := &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(edge.URL + "/edge/select?bridge=br_x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var c *http.Cookie
	for _, kv := range resp.Cookies() {
		if kv.Name == BridgeCookie {
			c = kv
		}
	}
	if c == nil {
		t.Fatal("selection cookie missing")
	}
	if c.Path != "/" || c.Value != "br_x" {
		t.Fatalf("cookie = %+v", c)
	}
	// jsdom/httptest: HttpOnly and SameSite live on the cookie object.
	if !c.HttpOnly || c.SameSite != http.SameSiteLaxMode {
		t.Fatalf("cookie attributes = HttpOnly:%v SameSite:%v", c.HttpOnly, c.SameSite)
	}
	if c.Secure {
		t.Fatal("plain-HTTP test edge must not set Secure")
	}
}

func TestGWObsForwarding(t *testing.T) {
	core := newFakeCore(t)
	var gotPath, gotAuth, gotBody string
	obs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(obs.Close)
	e := New(Config{ServiceURL: core.svc.URL, ConfigURL: core.cfg.URL, ObsURL: obs.URL}, zerolog.Nop())
	ts := httptest.NewServer(e.Handler())
	t.Cleanup(ts.Close)
	access := enroll(t, core)

	// No token: the public surface admits nothing but enrolled bridges.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/gw/obs/v1/traces", strings.NewReader("{}"))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}

	// Enrolled bridge token: forwarded with the /gw/obs prefix stripped —
	// the collector serves the standard /v1/* OTLP paths.
	req, _ = http.NewRequest(http.MethodPost, ts.URL+"/gw/obs/v1/traces", strings.NewReader(`{"trace":1}`))
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("forwarded: status = %d, want 202", resp.StatusCode)
	}
	if gotPath != "/v1/traces" {
		t.Fatalf("collector path = %q, want /v1/traces", gotPath)
	}
	if gotBody != `{"trace":1}` {
		t.Fatalf("body = %q", gotBody)
	}
	_ = gotAuth
}

func TestGWObsMisconfigured(t *testing.T) {
	core := newFakeCore(t)
	e := New(Config{ServiceURL: core.svc.URL, ConfigURL: core.cfg.URL}, zerolog.Nop())
	ts := httptest.NewServer(e.Handler())
	t.Cleanup(ts.Close)
	access := enroll(t, core)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/gw/obs/v1/traces", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("no obs plane: status = %d, want 500 (edge misconfigured)", resp.StatusCode)
	}
}

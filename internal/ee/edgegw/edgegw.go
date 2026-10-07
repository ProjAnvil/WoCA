// Package edgegw implements the edge access point (product design v1.5
// 拓扑拆分): the control plane's only public component. It is stateless
// by construction (R7) — no database, no files, no registry copy, no
// secrets. Every machine-token check calls home to the service plane;
// config pulls forward to the config plane; client traffic (the bridge's
// SPA, /api/*, SSE) parks here until the target bridge's outbound tunnel
// picks it up and streams the answer back — scenario four: any-place web
// commanding a NAT-hidden bridge.
package edgegw

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/ee/entrygw"
	"github.com/projanvil/woca/internal/platform/httpx"
	"github.com/projanvil/woca/internal/platform/machineid"
	"github.com/projanvil/woca/internal/webfx"
)

// BridgeCookie carries the selected target bridge (browser flow, v1.6
// 入口②: the direct-link form /edge/select?bridge=<id> sets it).
const BridgeCookie = "woca_edge_bridge"

// BridgeHeader routes one request to a target bridge (native clients,
// which hold no cookies by design).
const BridgeHeader = "X-Woca-Bridge"

// Tunnel cadences and bounds (vars so tests can tighten them).
var (
	pollWaitDefault = 25 * time.Second
	// parkPickupTimeout bounds how long a parked client waits for a
	// tunnel pickup before the edge reports the bridge offline.
	parkPickupTimeout = 30 * time.Second
	// firstByteTimeout bounds a picked-up request whose bridge died
	// before answering. Long streams are safe: only the wait until the
	// first response byte is bounded.
	firstByteTimeout = 120 * time.Second
)

const (
	pollWaitMax = 60 * time.Second
)

// clientBodyLimit bounds forwarded request bodies (base64 inflates
// ~4/3 on the poll payload); a var so tests can tighten it.
var clientBodyLimit = 48 << 20

// Config pins the two private-core planes the edge forwards to.
type Config struct {
	// ServiceURL is the service plane base (registry, heartbeats).
	ServiceURL string
	// ConfigURL is the config plane base (config pulls).
	ConfigURL string
	// ObsURL is the observability plane base — the trace bridge
	// (woca-collector) the /gw/obs/* machine channel forwards into.
	ObsURL string
}

// Edge is the stateless access point.
type Edge struct {
	cfg    Config
	log    zerolog.Logger
	client *http.Client

	// auth is the in-process validation cache (§2.5 R7: ≤60s,
	// non-authoritative, never persisted).
	auth authCache
	// nativeAuth caches the last core introspection per native DPoP
	// token; the proof itself is verified on every request.
	nativeAuth    nativeAuthCache
	nativeLimiter nativeActivateLimiter

	mu      sync.Mutex
	queues  map[string]*bridgeQueue
	parkedN atomic.Int64
}

// New builds the edge over its plane addresses.
func New(cfg Config, log zerolog.Logger) *Edge {
	return &Edge{
		cfg: cfg,
		log: log,
		client: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        64,
				IdleConnTimeout:     90 * time.Second,
				TLSHandshakeTimeout: 10 * time.Second,
			},
		},
		queues: map[string]*bridgeQueue{},
		nativeLimiter: nativeActivateLimiter{
			window:  time.Minute,
			max:     10,
			buckets: map[string]int{},
		},
	}
}

// bridgeQueue is one bridge's parking lot: requests waiting for a tunnel
// pickup plus the in-flight responds being streamed back.
type bridgeQueue struct {
	mu       sync.Mutex
	parking  chan *parked
	parked   map[string]*parked
	lastPoll time.Time
}

func (q *bridgeQueue) put(p *parked) {
	q.mu.Lock()
	q.parked[p.id] = p
	q.mu.Unlock()
}

func (q *bridgeQueue) take(id string) *parked {
	q.mu.Lock()
	defer q.mu.Unlock()
	p := q.parked[id]
	if p != nil {
		delete(q.parked, id)
	}
	return p
}

// parked is one client request parked until the bridge answers.
type parked struct {
	id        string
	w         http.ResponseWriter
	method    string
	path      string
	query     string
	client    string
	headers   map[string][]string
	body      []byte
	actor     *ActorAssertion
	picked    chan struct{} // closed on tunnel pickup
	firstByte chan struct{} // closed on the first relayed response byte
	done      chan struct{} // closed when the relay finished
	pickState atomic.Bool   // true once a poller took the request
	abandon   atomic.Bool
	started   time.Time
}

// picked reports whether a tunnel poller already received the request.
func (p *parked) wasPicked() bool { return p.pickState.Load() }

func (e *Edge) queue(bridgeID string) *bridgeQueue {
	e.mu.Lock()
	defer e.mu.Unlock()
	q := e.queues[bridgeID]
	if q == nil {
		// Unbuffered by design: a send completes only when a tunnel
		// poller actually receives — that rendezvous is the pickup
		// signal, so "parked" means "waiting for a bridge", never
		// "queued and assumed served".
		q = &bridgeQueue{parking: make(chan *parked), parked: map[string]*parked{}}
		e.queues[bridgeID] = q
	}
	return q
}

// Handler builds the public listener surface on the shared webfx router.
func (e *Edge) Handler() http.Handler {
	r := webfx.New(webfx.Options{})
	r.HandleFunc("GET /api/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "woca-edge"})
	})
	// Bridge selection (pre-auth): the inventory and the cookie setter.
	r.HandleFunc("GET /api/edge/bridges", e.handleBridgeList)
	r.HandleFunc("GET /edge/select", e.handleSelect)
	// Machine channel: the bridge-facing /gw/* routes forward into the
	// private core — config pulls to the config plane, the registry
	// routes to the service plane. Explicitly enumerated: the
	// core-internal /gw/resolve and /gw/bridges are this edge's
	// call-home hooks and must never answer on the public listener
	// (entrygw's own contract), and /gw/enroll-status must not become an
	// anonymous token dispensary.
	for _, route := range []string{"/gw/enroll", "/gw/enroll-request", "/gw/enroll-status", "/gw/hello", "/gw/renew", "/gw/heartbeat"} {
		r.Handle(route, e.forward(e.cfg.ServiceURL))
	}
	// Identity forwarding (v1.8 §2.7): direct-connect bridges reach the
	// core's auth service through the machine channel — gated by the
	// same machine-token resolution as the tunnel surface. The public
	// listener must never expose the core's identity authority to
	// anonymous callers (login-guessing, blind logout DoS).
	r.Handle("/gw/auth/", e.authTunnel(func(w http.ResponseWriter, r *http.Request) {
		e.forward(e.cfg.ServiceURL).ServeHTTP(w, r)
	}))
	r.Handle("/gw/config", e.forward(e.cfg.ConfigURL))
	// Observability plane (machine channel): enrolled bridges push their
	// OTLP traces/metrics here (WOCA_OTEL_EXPORTER_OTLP_ENDPOINT=
	// <edge>/gw/obs; the token rides the standard Authorization Bearer
	// header via OTEL_EXPORTER_OTLP_HEADERS on the bridge side). The same
	// resolveToken call-home as every other bridge route — an enrolled
	// bridge identity is the only thing this public surface accepts. The
	// /gw/obs prefix is stripped before forwarding: the collector serves
	// the standard /v1/* OTLP paths.
	r.Handle("/gw/obs/", e.authTunnel(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.Clone(r.Context())
		r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/gw/obs")
		if r2.URL.Path == "" {
			r2.URL.Path = "/"
		}
		e.forward(e.cfg.ObsURL).ServeHTTP(w, r2)
	}))
	// The model catalog bridges pull on demand (§16) rides the same
	// machine channel into the config plane (subtree: list + verify).
	r.Handle("/gw/models", e.forward(e.cfg.ConfigURL))
	r.Handle("/gw/models/", e.forward(e.cfg.ConfigURL))
	// Everything else under /gw/ — including the core-internal resolve
	// and bridges — is not a public machine-channel route.
	r.HandleFunc("/gw/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "0004", "unknown gateway route")
	})
	r.HandleFunc("GET /tunnel/poll", e.authTunnel(e.handlePoll))
	r.HandleFunc("POST /tunnel/respond/{id}", e.authTunnel(e.handleRespond))
	// Client auth enforcement (v1.8 §2.2): login and logout are answered
	// AT the edge — the core validates and issues; the cookie is
	// edge-origin. Everything else /api/* is validated per request before
	// tunneling (see serveClient).
	r.HandleFunc("POST /api/auth/login", e.handleClientLogin)
	r.HandleFunc("POST /api/auth/logout", e.handleClientLogout)
	// Native-client credential surface (DPoP devices): activation proxies
	// to the core's machine channel, session reads and revocation verify
	// the RFC 9449 proof at this origin first.
	r.HandleFunc("POST /api/native/activate", e.handleNativeActivate)
	r.HandleFunc("GET /api/native/session", e.handleNativeSession)
	r.HandleFunc("DELETE /api/native/session", e.handleNativeLogout)
	// Everything else is client traffic routed to the selected bridge.
	r.HandleFunc("/", e.serveClient)
	return r.Handler()
}

// forward proxies a machine-channel route into one core plane. Flushing
// stays live so the blocking config query (§2.3 long poll) passes
// through unbuffered.
func (e *Edge) forward(target string) http.Handler {
	if target == "" {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusInternalServerError, "0500", "edge misconfigured")
		})
	}
	base, err := url.Parse(target)
	if err != nil {
		e.log.Error().Str("target", target).Msg("edge: bad plane URL")
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusInternalServerError, "0500", "edge misconfigured")
		})
	}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = base.Host
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			e.log.Warn().Err(err).Str("plane", target).Msg("edge: plane unreachable")
			writeErr(w, http.StatusBadGateway, "0502", "core plane unreachable")
		},
	}
	return proxy
}

// handleBridgeList serves the pre-auth bridge inventory (selection UI).
// Only servable bridges are listed: a row the user cannot successfully
// pick (offline zombies, local-trust bridges without a tunnel) is noise
// that dead-ends the picker.
func (e *Edge) handleBridgeList(w http.ResponseWriter, r *http.Request) {
	bridges, err := e.coreBridges(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "0502", "core plane unreachable")
		return
	}
	writeJSON(w, http.StatusOK, servableBridges(bridges))
}

// servableBridges keeps the bridges a client can actually be routed to:
// live, and not local-trust (a closed tunnel parks every request into a
// 30s dead end — R1). Tunnel capability is explicit in the heartbeat;
// unknown (never reported) stays servable for pre-upgrade bridges.
func servableBridges(bridges []entrygw.BridgeInfo) []entrygw.BridgeInfo {
	out := make([]entrygw.BridgeInfo, 0, len(bridges))
	for _, b := range bridges {
		if !b.Live || (b.Tunnel != nil && !*b.Tunnel) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// coreBridges fetches the registration inventory from the service plane —
// the stateless way: ask home, never cache a copy (R7).
func (e *Edge) coreBridges(ctx context.Context) ([]entrygw.BridgeInfo, error) {
	if e.cfg.ServiceURL == "" {
		return nil, errors.New("edge: no service plane configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.cfg.ServiceURL+"/gw/bridges", nil)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("edge: bridges: service plane returned %d", resp.StatusCode)
	}
	var bridges []entrygw.BridgeInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&bridges); err != nil {
		return nil, err
	}
	return bridges, nil
}

// handleSelect sets the target-bridge cookie (?bridge=<id>) and returns
// to the SPA root; without an id it renders the selection page.
func (e *Edge) handleSelect(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("bridge"))
	if id == "" {
		e.writeSelectionPage(w, r)
		return
	}
	// A local-trust bridge must not be selected: its tunnel is closed by
	// design and every client request would park into a 30s dead end.
	if bridges, err := e.coreBridges(r.Context()); err == nil {
		for _, b := range bridges {
			if b.BridgeID == id && b.Tunnel != nil && !*b.Tunnel {
				writeErr(w, http.StatusForbidden, "0003", "this bridge does not accept remote client traffic (local trust)")
				return
			}
		}
	}
	// Secure follows the connection unless the operator forces it on:
	// TLS terminates at a reverse proxy in the outsourced-edge shapes
	// (CF Tunnel / Tailscale Funnel, design §5.4), where r.TLS is nil.
	forceSecure := os.Getenv("WOCA_EDGE_SECURE_COOKIE") == "1"
	http.SetCookie(w, &http.Cookie{
		Name:     BridgeCookie,
		Value:    id,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || forceSecure,
	})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// writeSelectionPage renders the minimal built-in bridge picker (no
// secrets, no assets — a stateless page that asks the service plane for
// the live list on every load). Only servable bridges are offered: an
// unpickable row (offline, local-trust) is a dead end, not a choice.
func (e *Edge) writeSelectionPage(w http.ResponseWriter, r *http.Request) {
	bridges, err := e.coreBridges(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "0502", "core plane unreachable")
		return
	}
	var rows strings.Builder
	for _, b := range servableBridges(bridges) {
		fmt.Fprintf(&rows,
			`<li><a href="/edge/select?bridge=%s"><code>%s</code> <span style="color:#2fbf71">● online</span></a></li>`,
			url.QueryEscape(b.BridgeID), b.BridgeID)
	}
	if rows.Len() == 0 {
		rows.WriteString(`<li style="color:#8b93a1">No bridge is currently online.</li>`)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset="utf-8"><title>woca · 选择 bridge / Select bridge</title>
<style>body{font:14px/1.5 system-ui,sans-serif;background:#14161a;color:#e6e8ec;display:flex;justify-content:center;padding-top:12vh}
.card{background:#1b1e24;border:1px solid #2a2e37;border-radius:14px;padding:28px;width:min(420px,90vw)}
h1{font-size:18px;margin:0 0 6px}p{color:#9aa3b2;margin:0 0 18px;font-size:13px}
ul{list-style:none;margin:0;padding:0;display:flex;flex-direction:column;gap:8px}
a{color:#7fa5ff;text-decoration:none;display:flex;justify-content:space-between;padding:10px 12px;border:1px solid #2a2e37;border-radius:8px}
a:hover{border-color:#4f7cff}code{font-size:13px}</style></head><body><div class="card">
<h1>woca edge · 选择目标 bridge</h1><p>Select the bridge to command, or open a direct link carrying the bridge id.</p><ul>%s</ul></div></body></html>`,
		rows.String())
}

// serveClient parks one client request for the selected bridge's tunnel.
func (e *Edge) serveClient(w http.ResponseWriter, r *http.Request) {
	bridgeID := e.targetBridge(w, r)
	if bridgeID == "" {
		return // selection page written or error already sent
	}
	// Auth enforcement (v1.8 §2.2): /api/* client traffic (other than
	// the intercepted login/logout above) validates against the core
	// before tunneling — the bridge never sees the credential, only the
	// assertion. Non-API paths (the SPA) tunnel anonymously; the bridge's
	// own R2 anonymous list applies there. Native DPoP clients carry a
	// per-request proof and validate on their own path.
	var actor *ActorAssertion
	if strings.HasPrefix(r.URL.Path, "/api/") {
		assert, err := e.validateClientAPI(r)
		if err != nil {
			switch {
			case errors.Is(err, errCoreAuthDown):
				writeErr(w, http.StatusServiceUnavailable, "0502", "private core unreachable")
			case errors.Is(err, errRevokedCredential):
				writeErr(w, http.StatusUnauthorized, "0002", "credential revoked")
			default:
				writeErr(w, http.StatusUnauthorized, "0001", "unauthorized")
			}
			return
		}
		actor = &assert
	}
	// Bound and capture the body before parking (it travels base64 in
	// the poll payload).
	var body []byte
	if r.Body != nil {
		b, err := io.ReadAll(io.LimitReader(r.Body, int64(clientBodyLimit)+1))
		if err != nil {
			writeErr(w, http.StatusBadRequest, "0100", "bad request body")
			return
		}
		if len(b) > clientBodyLimit {
			writeErr(w, http.StatusRequestEntityTooLarge, "0100", "request too large for the tunnel")
			return
		}
		body = b
	}
	headers := tunnelableHeaders(r.Header)
	// Credentials stay at the edge — the bridge sees the assertion, never
	// the session token (§2.2); anonymous traffic forwards no edge-origin
	// cookies either. The DPoP proof is bound to this origin and dies
	// with the request.
	delete(headers, "Cookie")
	delete(headers, "Authorization")
	delete(headers, "DPoP")
	p := &parked{
		id:        requestID(),
		w:         w,
		method:    r.Method,
		path:      r.URL.Path,
		query:     r.URL.RawQuery,
		client:    r.RemoteAddr,
		headers:   headers,
		body:      body,
		actor:     actor,
		picked:    make(chan struct{}),
		firstByte: make(chan struct{}),
		done:      make(chan struct{}),
		started:   time.Now(),
	}
	if e.parkedN.Load() >= maxParked {
		writeErr(w, http.StatusServiceUnavailable, "0503", "edge busy")
		return
	}
	e.parkedN.Add(1)
	defer e.parkedN.Add(-1)
	q := e.queue(bridgeID)
	q.put(p)
	ctx := r.Context()
	select {
	case q.parking <- p:
	case <-time.After(parkPickupTimeout):
		if q.take(p.id) == p {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "bridge offline (no tunnel pickup)"})
			return
		}
		waitDoneOrGrace(p)
		return
	case <-ctx.Done():
		if q.take(p.id) == p {
			return
		}
		// A respond claimed the request mid-flight: never return while
		// the relay may still hold the writer — net/http closes it on
		// handler return and the concurrent write is a data race.
		p.abandon.Store(true)
		waitDoneOrGrace(p)
		return
	}
	close(p.picked)
	p.pickState.Store(true)
	// Picked: the respond stream owns the writer now. Guard the window
	// until the first byte — a bridge that died right after pickup must
	// not hang the client.
	timer := time.NewTimer(firstByteTimeout)
	defer timer.Stop()
	select {
	case <-p.firstByte:
	case <-p.done:
		return
	case <-timer.C:
		if q.take(p.id) == p {
			p.abandon.Store(true)
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "bridge tunnel stalled"})
			return
		}
		p.abandon.Store(true)
		waitDoneOrGrace(p)
		return
	case <-ctx.Done():
		p.abandon.Store(true)
	}
	// First byte seen (or the client is gone): the handler must not
	// return until the relay finished — returning early hands the writer
	// back to net/http while the respond goroutine may still write.
	<-p.done
}

// waitDoneOrGrace bounds the handoff window: every respond path closes
// done (its defer), but a bridge that died mid-request never answers —
// the grace keeps the handler from hanging forever on that.
func waitDoneOrGrace(p *parked) {
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	select {
	case <-p.done:
	case <-timer.C:
	}
}

// targetBridge resolves the request's target: explicit header (native) or
// cookie (browser), else the single registered bridge, else the selection
// page. Empty return means the response is already written.
func (e *Edge) targetBridge(w http.ResponseWriter, r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(BridgeHeader)); id != "" {
		return id
	}
	if c, err := r.Cookie(BridgeCookie); err == nil && strings.TrimSpace(c.Value) != "" {
		return strings.TrimSpace(c.Value)
	}
	bridges, err := e.coreBridges(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "0502", "core plane unreachable")
		return ""
	}
	// Auto-route counts SERVABLE bridges only: stale registrations
	// (offline zombies from old container generations) cannot serve
	// traffic, and neither can a local-trust bridge whose tunnel is
	// closed — a single servable bridge goes straight through even when
	// the registry holds several rows. The selection page appears only
	// when two or more bridges could actually answer.
	live := servableBridges(bridges)
	switch len(live) {
	case 0:
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no bridge online"})
		return ""
	case 1:
		return live[0].BridgeID
	default:
		e.writeSelectionPage(w, r)
		return ""
	}
}

// pollPayload is one parked client request handed to a bridge poller.
type pollPayload struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query,omitempty"`
	Client  string              `json:"client,omitempty"`
	Headers map[string][]string `json:"headers,omitempty"`
	Body    string              `json:"body,omitempty"` // base64
	// Actor is the edge's validated identity assertion (absent for
	// anonymous tunneled traffic). Client requests cannot forge it: the
	// payload is built here, after the X-Woca-* strip.
	Actor *bridgeActor `json:"actor,omitempty"`
}

// handlePoll is the bridge's pickup: long-poll the next parked request.
func (e *Edge) handlePoll(w http.ResponseWriter, r *http.Request) {
	bridgeID := bridgeIDFromCtx(r.Context())
	wait := pollWaitDefault
	if d := parseWait(r.URL.Query().Get("wait")); d > 0 {
		wait = d
	}
	q := e.queue(bridgeID)
	q.mu.Lock()
	q.lastPoll = time.Now()
	q.mu.Unlock()

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		select {
		case p := <-q.parking:
			if p.abandon.Load() {
				continue // client gone; take the next
			}
			body := ""
			if len(p.body) > 0 {
				body = base64.StdEncoding.EncodeToString(p.body)
			}
			writeJSON(w, http.StatusOK, pollPayload{
				ID:      p.id,
				Method:  p.method,
				Path:    p.path,
				Query:   p.query,
				Client:  p.client,
				Headers: p.headers,
				Body:    body,
				Actor:   actorFromBridge(p.actor),
			})
			return
		case <-deadline.C:
			w.WriteHeader(http.StatusNoContent)
			return
		case <-r.Context().Done():
			return
		}
	}
}

// handleRespond relays the bridge's streamed answer to the parked client:
// X-Woca-Status carries the status, X-Woca-Headers the header map
// (base64 JSON), and the request body is the raw response stream.
func (e *Edge) handleRespond(w http.ResponseWriter, r *http.Request) {
	bridgeID := bridgeIDFromCtx(r.Context())
	q := e.queue(bridgeID)
	p := q.take(r.PathValue("id"))
	if p == nil || p.abandon.Load() {
		writeJSON(w, http.StatusGone, map[string]string{"error": "unknown or abandoned request"})
		return
	}
	status := http.StatusOK
	if v := r.Header.Get("X-Woca-Status"); v != "" {
		if code, err := strconv.Atoi(v); err == nil && code >= 100 && code <= 999 {
			status = code
		}
	}
	header := http.Header{}
	if v := r.Header.Get("X-Woca-Headers"); v != "" {
		if raw, err := base64.StdEncoding.DecodeString(v); err == nil {
			_ = json.Unmarshal(raw, &header)
		}
	}
	// Defense in depth: the relayed headers are machine-authenticated
	// bridge output, but the edge still refuses to forward framing
	// headers (the relay frames the response itself).
	for k := range header {
		if isHopHeader(k) || strings.EqualFold(k, "Content-Length") {
			header.Del(k)
		}
	}
	defer close(p.done)
	first := sync.OnceFunc(func() { close(p.firstByte) })
	for k, vs := range header {
		for _, v := range vs {
			p.w.Header().Add(k, v)
		}
	}
	// The edge is the public entry behind which a buffering proxy may
	// sit, so SSE answers get the anti-buffering marker here — the same
	// contract the personal relay enforces (design §6.3,
	// internal/relay/forward.go).
	if strings.HasPrefix(p.w.Header().Get("Content-Type"), "text/event-stream") {
		p.w.Header().Set("X-Accel-Buffering", "no")
	}
	p.w.WriteHeader(status)
	first()
	rc := http.NewResponseController(p.w)
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Body.Read(buf)
		if n > 0 {
			if _, werr := p.w.Write(buf[:n]); werr != nil {
				p.abandon.Store(true)
				return
			}
			first()
			_ = rc.Flush()
		}
		if err != nil {
			return
		}
	}
}

// authTunnel wraps a tunnel endpoint with the machine-token check —
// resolved at the service plane, every call (R7: no local registry copy).
func (e *Edge) authTunnel(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearer(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "0001", "unauthorized")
			return
		}
		bridgeID, err := e.resolveToken(r.Context(), token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "0001", "unauthorized")
			return
		}
		next(w, r.WithContext(withBridgeID(r.Context(), bridgeID)))
	}
}

// resolveToken calls the service plane with the token hash (hashes only —
// no token material crosses the private network).
func (e *Edge) resolveToken(ctx context.Context, token string) (string, error) {
	if e.cfg.ServiceURL == "" {
		return "", errors.New("edge: no service plane configured")
	}
	body, _ := json.Marshal(map[string]string{"token_hash": machineid.HashToken(token)})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.cfg.ServiceURL+"/gw/resolve", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("edge: resolve: service plane returned %d", resp.StatusCode)
	}
	var out struct {
		BridgeID string `json:"bridge_id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&out); err != nil || out.BridgeID == "" {
		return "", errors.New("edge: resolve: bad answer")
	}
	return out.BridgeID, nil
}

// Run sweeps stale parking entries so nothing accumulates.
func (e *Edge) Run(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			e.sweep()
		}
	}
}

func (e *Edge) sweep() {
	now := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, q := range e.queues {
		q.mu.Lock()
		for pid, pr := range q.parked {
			age := now.Sub(pr.started)
			if !pr.wasPicked() {
				// Never picked: serveClient's own pickup timeout owns
				// the answer; this is map hygiene only.
				if age > parkPickupTimeout {
					delete(q.parked, pid)
				}
				continue
			}
			// Picked: the entry must survive until the respond claims it
			// or serveClient's first-byte timeout resolves the client —
			// a slow-TTFB bridge (long LLM thinking before the first
			// byte) is exactly the case the pickup sweep used to murder.
			// Beyond that window the handler is long gone; hygiene only.
			if age > firstByteTimeout+10*time.Second {
				delete(q.parked, pid)
			}
		}
		if len(q.parked) == 0 && now.Sub(q.lastPoll) > 4*time.Minute {
			delete(e.queues, id)
		}
		q.mu.Unlock()
	}
}

func bridgeIDFromCtx(ctx context.Context) string {
	id, _ := ctx.Value(bridgeIDKey{}).(string)
	return id
}

type bridgeIDKey struct{}

func withBridgeID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, bridgeIDKey{}, id)
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// tunnelableHeaders additionally strips the X-Woca-* signalling family:
// those headers are edge vocabulary (routing, native request ids) and are
// consumed here — a client must not be able to forge internal signalling
// at the bridge.
func tunnelableHeaders(h http.Header) map[string][]string {
	out := map[string][]string{}
	for k, vs := range h {
		if isHopHeader(k) || strings.HasPrefix(k, "X-Woca-") {
			continue
		}
		out[k] = vs
	}
	return out
}

func isHopHeader(k string) bool {
	switch strings.ToLower(k) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate",
		"proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// maxParked bounds the in-flight parking lot (unauthenticated memory DoS
// surface): beyond it the edge sheds load with 503 instead of buffering.
const maxParked = 512

func parseWait(v string) time.Duration {
	if v == "" {
		return 0
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < time.Second {
		return 0
	}
	if d > pollWaitMax {
		return pollWaitMax
	}
	return d
}

func requestID() string {
	var b [8]byte
	if _, err := cryptorand.Read(b[:]); err != nil {
		return fmt.Sprintf("rq_%x", time.Now().UnixNano())
	}
	return "rq_" + hex.EncodeToString(b[:])
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	httpx.WriteJSON(w, code, v)
}

// writeErr emits the standard S-E error envelope (api-standard.md §1).
func writeErr(w http.ResponseWriter, status int, code, msg string) {
	httpx.WriteCoded(w, httpx.CodedError{Status: status, Segment: httpx.SegEdge, Code: code, Message: msg})
}

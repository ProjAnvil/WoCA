// tunnel.go is the bridge side of the scenario-four data plane (product
// design v1.5 §5.4): client traffic that parks at the edge access point
// is picked up here over the same outbound-only channel and executed
// against the bridge's own handler stack — so a browser behind
// https://edge.example.com drives a NAT-hidden bridge with zero inbound
// ports. The protocol is the design's v1 long-poll form (Syncthing/Consul
// blocking-query posture): GET /tunnel/poll hands over one parked request,
// POST /tunnel/respond streams the answer back chunk by chunk (SSE rides
// it with flushes intact).
package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Tunnel cadences.
const (
	tunnelPollWait       = 25 * time.Second
	tunnelBackoff        = 2 * time.Second
	tunnelAuthPause      = 5 * time.Second
	tunnelDefaultWorkers = 6
)

// TunnelConfig wires the tunnel client.
type TunnelConfig struct {
	// EdgeURL is the edge access point base (the only control-plane
	// address the bridge holds, D5).
	EdgeURL string
	// Token supplies the current machine access token (renewal-aware).
	Token func() string
	// Handler executes picked-up requests — the bridge's full server
	// stack (auth middleware included: remote clients authenticate at
	// the bridge exactly as they would locally).
	Handler http.Handler
	// Workers is the concurrent poller count (default 6): each parked
	// client request occupies one worker until its answer completes, so
	// streams (SSE) must not starve plain requests.
	Workers int
	// Insecure accepts self-signed edges — development 联调 only.
	Insecure bool
	// PinSource supplies the pinned edge SPKI (TOFU); the tunnel holds
	// the same trust in the edge as the gateway channel.
	PinSource func() string
	// OnPin records the observed SPKI on the first verified handshake.
	OnPin func(pin string)
}

// TunnelStatus is the status-page view of the data plane.
type TunnelStatus struct {
	Serving bool  `json:"serving"`
	Served  int64 `json:"served"`
}

// TunnelClient drives the outbound data-plane channel.
type TunnelClient struct {
	cfg TunnelConfig
	log zerolog.Logger
	hc  *http.Client

	served atomic.Int64
	live   atomic.Int64
}

// NewTunnelClient builds the data-plane channel.
func NewTunnelClient(cfg TunnelConfig, log zerolog.Logger) *TunnelClient {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 0 // streams answer late by design
	if cfg.Insecure || cfg.PinSource != nil {
		base := PinnedTransport(cfg.Insecure, cfg.PinSource, cfg.OnPin).(*http.Transport)
		transport = base
		transport.ResponseHeaderTimeout = 0
	}
	return &TunnelClient{
		cfg: cfg,
		log: log,
		hc: &http.Client{
			Timeout:   0, // a respond stream may run for the whole SSE session
			Transport: transport,
		},
	}
}

// Run drives the worker pool until ctx ends.
func (t *TunnelClient) Run(ctx context.Context) {
	if t.cfg.Handler == nil || t.cfg.EdgeURL == "" {
		return
	}
	workers := t.cfg.Workers
	if workers <= 0 {
		workers = tunnelDefaultWorkers
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { t.worker(ctx) })
	}
	wg.Wait()
}

// Status snapshots the data-plane channel.
func (t *TunnelClient) Status() TunnelStatus {
	return TunnelStatus{Serving: t.live.Load() > 0, Served: t.served.Load()}
}

func (t *TunnelClient) worker(ctx context.Context) {
	t.live.Add(1)
	defer t.live.Add(-1)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		token := ""
		if t.cfg.Token != nil {
			token = t.cfg.Token()
		}
		if token == "" {
			// Not enrolled yet: the gateway channel will fix that; idle.
			if !sleepCtx(ctx, tunnelAuthPause) {
				return
			}
			continue
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			t.cfg.EdgeURL+"/tunnel/poll?wait="+tunnelPollWait.String(), nil)
		if err != nil {
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := t.hc.Do(req)
		if err != nil {
			if !sleepCtx(ctx, tunnelBackoff) {
				return
			}
			continue
		}
		switch resp.StatusCode {
		case http.StatusOK:
			var payload tunnelPayload
			err = json.NewDecoder(io.LimitReader(resp.Body, (64<<20)+1)).Decode(&payload)
			_ = resp.Body.Close()
			if err != nil {
				continue
			}
			t.served.Add(1)
			go t.execute(ctx, payload)
		case http.StatusNoContent:
			_ = resp.Body.Close()
		case http.StatusUnauthorized:
			_ = resp.Body.Close()
			// Token expired mid-cycle: the gateway channel renews; pause.
			if !sleepCtx(ctx, tunnelAuthPause) {
				return
			}
		default:
			_ = resp.Body.Close()
			if !sleepCtx(ctx, tunnelBackoff) {
				return
			}
		}
	}
}

// tunnelPayload mirrors the edge's parked-request wire shape.
type tunnelPayload struct {
	ID      string              `json:"id"`
	Method  string              `json:"method"`
	Path    string              `json:"path"`
	Query   string              `json:"query"`
	Client  string              `json:"client"`
	Headers map[string][]string `json:"headers"`
	Body    string              `json:"body"`
	// Actor is the edge's validated identity statement for this request
	// (v1.8 §2.2): present when the edge authenticated the caller by
	// calling home; absent for anonymous tunneled traffic (SPA assets,
	// health). Client-supplied headers cannot forge it — the payload is
	// built by the edge after stripping the X-Woca-* family.
	Actor *ActorAssertion `json:"actor,omitempty"`
}

// execute runs one picked-up client request against the local handler and
// streams the answer back. RemoteAddr carries the original client address:
// tunneled traffic must never look loopback-local to the auth middleware.
func (t *TunnelClient) execute(ctx context.Context, payload tunnelPayload) {
	defer func() {
		if r := recover(); r != nil {
			t.log.Error().Any("panic", r).Str("path", payload.Path).Msg("bridge: tunnel request panicked")
		}
	}()
	var body []byte
	if payload.Body != "" {
		var err error
		if body, err = base64.StdEncoding.DecodeString(payload.Body); err != nil {
			return
		}
	}
	target := payload.Path
	if payload.Query != "" {
		target += "?" + payload.Query
	}
	req, err := http.NewRequestWithContext(ctx, payload.Method, "http://tunnel.local"+target, bytes.NewReader(body))
	if err != nil {
		return
	}
	for k, vs := range payload.Headers {
		if strings.HasPrefix(k, "X-Woca-") {
			continue // edge signalling vocabulary never reaches the handler
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	if payload.Client != "" {
		req.RemoteAddr = payload.Client
	} else {
		req.RemoteAddr = "edge:0"
	}
	if payload.Actor != nil {
		req = req.WithContext(WithActor(req.Context(), *payload.Actor))
	}

	pr, pw := io.Pipe()
	rec := newStreamRecorder(pw)
	go func() {
		defer rec.finish()
		t.cfg.Handler.ServeHTTP(rec, req)
		_ = pw.Close()
	}()
	select {
	case <-rec.ready:
	case <-ctx.Done():
		_ = pr.CloseWithError(context.Canceled)
		return
	}

	rreq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		t.cfg.EdgeURL+"/tunnel/respond/"+payload.ID, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	token := ""
	if t.cfg.Token != nil {
		token = t.cfg.Token()
	}
	rreq.Header.Set("Authorization", "Bearer "+token)
	rreq.Header.Set("X-Woca-Status", fmt.Sprintf("%d", rec.committedStatus()))
	rreq.Header.Set("X-Woca-Headers", encodeHeaders(rec.snapshot()))
	resp, err := t.hc.Do(rreq)
	if err != nil {
		// Unstick the handler goroutine: further pipe writes fail fast.
		_ = pr.CloseWithError(err)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain so the connection reuses cleanly
	_ = resp.Body.Close()
	_ = pr.CloseWithError(errors.New("bridge: tunnel respond complete"))
}

// streamRecorder captures a handler's response as a stream: status and
// headers freeze at the first byte (exactly when net/http would commit
// them), the body flows through the pipe so flush-sized chunks survive.
type streamRecorder struct {
	pw     *io.PipeWriter
	header http.Header
	status int

	mu            sync.Mutex
	ready         chan struct{}
	markReadyOnce func()
}

func newStreamRecorder(pw *io.PipeWriter) *streamRecorder {
	r := &streamRecorder{pw: pw, header: http.Header{}, ready: make(chan struct{})}
	r.markReadyOnce = sync.OnceFunc(func() { close(r.ready) })
	return r
}

func (r *streamRecorder) Header() http.Header { return r.header }

func (r *streamRecorder) WriteHeader(code int) {
	r.mu.Lock()
	if r.status == 0 {
		r.status = code
	}
	r.mu.Unlock()
	r.markReady()
}

func (r *streamRecorder) Write(b []byte) (int, error) {
	r.markReady()
	return r.pw.Write(b)
}

// Flush is a no-op: every pipe write already becomes one transport chunk.
func (r *streamRecorder) Flush() {}

func (r *streamRecorder) finish() { r.markReady() }

func (r *streamRecorder) markReady() { r.markReadyOnce() }

func (r *streamRecorder) snapshot() map[string][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]string{}
	for k, vs := range r.header {
		if strings.EqualFold(k, "Content-Length") {
			continue // streamed answers have no fixed length
		}
		out[k] = vs
	}
	return out
}

// committedStatus returns the status code (implicit 200).
func (r *streamRecorder) committedStatus() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}

func encodeHeaders(h map[string][]string) string {
	raw, err := json.Marshal(h)
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

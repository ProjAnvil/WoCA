package relayclient

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"
)

// Tunnel cadences (design §3.2/§6.5).
const (
	pollWait       = 25 * time.Second
	backoffMin     = time.Second
	backoffMax     = 60 * time.Second
	defaultWorkers = 4
)

// Config wires the tunnel client.
type Config struct {
	// URL is the relay base URL (https://relay.example.com).
	URL string
	// Key is the wcr_ rendezvous key (the same key clients log in with).
	Key string
	// Handler executes parked requests in process — server.Handler().
	Handler http.Handler
	// Workers is the concurrent poller count (default 4, design §3.2;
	// each parked request occupies one worker until its answer completes,
	// so SSE streams must not starve plain requests).
	Workers int
	// HTTPClient overrides the transport (tests); nil uses a
	// streaming-safe default.
	HTTPClient *http.Client
	// HeartbeatInterval overrides the 20s SSE heartbeat (tests).
	HeartbeatInterval time.Duration
}

// Status is the /api/relay/status snapshot (design §3.2).
type Status struct {
	Configured bool   `json:"configured"`
	Connected  bool   `json:"connected"`
	KeyPrefix  string `json:"key_prefix,omitempty"`
	Generation string `json:"generation,omitempty"`
	Revoked    bool   `json:"revoked"`
	LastError  string `json:"last_error,omitempty"`
	Served     int64  `json:"served"`
}

// Client is the outbound tunnel: worker goroutines long-poll the relay,
// execute parked requests against Handler, and stream responses back.
type Client struct {
	cfg Config
	log zerolog.Logger
	hc  *http.Client

	genMu sync.Mutex
	gen   string

	live   atomic.Int64
	served atomic.Int64

	stMu      sync.Mutex
	lastError string
	revoked   bool

	term       context.Context
	termCancel context.CancelFunc
}

// New builds the tunnel client. The relay URL is normalized by trimming
// trailing slashes: endpoints are joined as URL+"/tunnel/poll", and a
// stored "https://relay.example.com/" would otherwise produce
// "//tunnel/poll" — which net/http ServeMux answers with a 301 redirect
// that breaks POST /tunnel/respond (a redirected POST re-issues as GET).
func New(cfg Config, log zerolog.Logger) *Client {
	cfg.URL = strings.TrimRight(cfg.URL, "/")
	hc := cfg.HTTPClient
	if hc == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = 0 // streams answer late by design
		hc = &http.Client{Timeout: 0, Transport: transport}
	}
	c := &Client{cfg: cfg, log: log, hc: hc}
	// Mint the first generation at construction (design §6.2/§6.5: one
	// generation per connect cycle, a fresh one per transport-level
	// reconnect). Synchronous minting keeps the token observable and
	// stable from the moment New returns.
	c.regenerate()
	return c
}

// Run drives the worker pool until ctx ends or a terminal state (key
// revoked/unknown) stops the tunnel (design §6.5). The generation is NOT
// re-minted here: New already minted it synchronously, so a concurrent
// Status()/generation() reader always observes a stable token — re-minting
// inside Run would make the token change under a reader that snapped it
// the moment Run started (and an answered-5xx retry loop must observe NO
// rotation, design §6.5: rotation is transport-error-only).
func (c *Client) Run(ctx context.Context) {
	if c.cfg.Handler == nil || c.cfg.URL == "" || c.cfg.Key == "" {
		return
	}
	c.term, c.termCancel = context.WithCancel(ctx)
	defer c.termCancel()
	workers := c.cfg.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { c.worker(c.term) })
	}
	wg.Wait()
}

// Status snapshots the tunnel state.
func (c *Client) Status() Status {
	c.stMu.Lock()
	defer c.stMu.Unlock()
	prefix := c.cfg.Key
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return Status{
		Configured: c.cfg.URL != "" && c.cfg.Key != "",
		Connected:  c.live.Load() > 0 && !c.revoked,
		KeyPrefix:  prefix,
		Generation: c.generation(),
		Revoked:    c.revoked,
		LastError:  c.lastError,
		Served:     c.served.Load(),
	}
}

// generation is the current generation token.
func (c *Client) generation() string {
	c.genMu.Lock()
	defer c.genMu.Unlock()
	return c.gen
}

// regenerate rotates the generation — every reconnect cycle (design
// §6.2/§6.5: the relay's 45s lease lets the old generation yield).
func (c *Client) regenerate() {
	var raw [16]byte
	if _, err := crand.Read(raw[:]); err != nil {
		panic(err) // crypto/rand failure is unrecoverable
	}
	c.genMu.Lock()
	c.gen = base64.RawURLEncoding.EncodeToString(raw[:])
	c.genMu.Unlock()
}

// setTerminal records a terminal failure and stops every worker —
// terminal, never a backoff retry (design §5.1/§6.5).
func (c *Client) setTerminal(revoked bool, msg string) {
	c.stMu.Lock()
	c.revoked = revoked
	c.lastError = msg
	c.stMu.Unlock()
	c.termCancel()
}

func (c *Client) setError(msg string) {
	c.stMu.Lock()
	c.lastError = msg
	c.stMu.Unlock()
}

// pollOutcome classifies one poll cycle.
type pollOutcome int

const (
	outcomeOK       pollOutcome = iota // picked up work
	outcomeTimeout                     // 204 — immediate re-poll
	outcomeRetry                       // network/5xx — backoff
	outcomeConflict                    // 409 — another generation lives
	outcomeTerminal                    // 401/410 — stop reconnecting
)

func (c *Client) worker(ctx context.Context) {
	c.live.Add(1)
	defer c.live.Add(-1)
	attempt := 0
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		switch c.pollOnce(ctx) {
		case outcomeOK, outcomeTimeout:
			attempt = 0
		case outcomeRetry:
			if !sleepCtx(ctx, backoff(attempt)) {
				return
			}
			attempt++
		case outcomeConflict:
			// Our generation loses while the previous one is still live:
			// back off; the relay's 45s lease evicts the old one
			// (design §6.2).
			if !sleepCtx(ctx, backoff(attempt)) {
				return
			}
			attempt++
		case outcomeTerminal:
			return
		}
	}
}

// backoff is 1s doubling to 60s with ±20% jitter (design §6.5).
func backoff(attempt int) time.Duration {
	d := backoffMin << min(attempt, 6)
	if d > backoffMax {
		d = backoffMax
	}
	return time.Duration(float64(d) * (0.8 + 0.4*mrand.Float64()))
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

// pollOnce performs one long poll; a picked-up request runs on its own
// goroutine so SSE streams never starve the worker pool.
func (c *Client) pollOnce(ctx context.Context) pollOutcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.URL+"/tunnel/poll?wait="+pollWait.String(), nil)
	if err != nil {
		c.setTerminal(false, "invalid relay URL")
		return outcomeTerminal
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set(generationHeader, c.generation())
	resp, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return outcomeTerminal // shutdown, not a failure
		}
		c.setError("poll: " + err.Error())
		c.regenerate() // a fresh generation per reconnect (design §6.5)
		return outcomeRetry
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		var payload pollPayload
		if err := json.NewDecoder(io.LimitReader(resp.Body, (8<<20)+1)).Decode(&payload); err != nil {
			if errors.Is(err, io.EOF) {
				// Bare 200 with an empty body: the relay's poll handler
				// returns without writing when the take is released
				// with no parked request (internal/relay HandlePoll's
				// "nothing to write" path, e.g. a mid-poll hangup wake).
				// Treat it exactly like 204: no work, re-poll at once —
				// a decode error here would backoff-throttle a healthy
				// tunnel.
				return outcomeTimeout
			}
			c.setError("poll decode: " + err.Error())
			return outcomeRetry
		}
		c.served.Add(1)
		go c.execute(ctx, payload)
		return outcomeOK
	case http.StatusNoContent:
		return outcomeTimeout
	case http.StatusConflict:
		return outcomeConflict
	case http.StatusGone, http.StatusUnauthorized:
		// 410 {"code":"revoked"} or 401: terminal — never re-poll
		// (design §5.1/§6.5).
		c.setTerminal(resp.StatusCode == http.StatusGone, terminalMessage(resp))
		return outcomeTerminal
	default:
		c.setError(fmt.Sprintf("poll: unexpected status %d", resp.StatusCode))
		return outcomeRetry
	}
}

// terminalMessage extracts the relay's error code for the status surface.
func terminalMessage(resp *http.Response) string {
	var body struct {
		Code string `json:"code"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<10)).Decode(&body)
	if body.Code == "revoked" {
		return "key revoked"
	}
	return fmt.Sprintf("tunnel authentication failed (HTTP %d)", resp.StatusCode)
}

// requestHopByHop collects the connection-specific header names that may
// arrive inside a parked payload: the relay parks r.Header verbatim
// (minus credentials), so a browser's Connection/Keep-Alive/
// Transfer-Encoding/TE/Trailer/Upgrade — plus anything the Connection
// token nominates — can ride the tunnel. They must NOT be re-injected
// into the in-process request: net/http manages hop state per connection
// and a forwarded Connection: keep-alive would leak client-side
// connection semantics into local handlers (the request-side mirror of
// internal/relay's StripHopByHop — semantics copied, code rewritten).
func requestHopByHop(headers map[string][]string) map[string]bool {
	hop := map[string]bool{
		"connection":          true,
		"keep-alive":          true,
		"proxy-authenticate":  true,
		"proxy-authorization": true,
		"te":                  true,
		"trailer":             true,
		"transfer-encoding":   true,
		"upgrade":             true,
	}
	for k, vs := range headers {
		if !strings.EqualFold(k, "Connection") {
			continue
		}
		for _, v := range vs {
			for name := range strings.SplitSeq(v, ",") {
				if n := strings.ToLower(strings.TrimSpace(name)); n != "" {
					hop[n] = true
				}
			}
		}
	}
	return hop
}

// execute runs one parked request against the in-process handler and
// streams the answer back (design §3.2/§6.3/§6.4).
func (c *Client) execute(ctx context.Context, payload pollPayload) {
	defer func() {
		if r := recover(); r != nil {
			c.log.Error().Any("panic", r).Str("path", payload.Path).Msg("relayclient: tunneled request panicked")
		}
	}()
	var body []byte
	if payload.Body != "" {
		var err error
		if body, err = base64.StdEncoding.DecodeString(payload.Body); err != nil {
			c.setError("payload body: " + err.Error())
			return
		}
	}
	target := payload.Path
	if payload.Query != "" {
		target += "?" + payload.Query
	}
	req, err := http.NewRequestWithContext(ctx, payload.Method, "http://relay.local"+target, bytes.NewReader(body))
	if err != nil {
		c.setError("payload request: " + err.Error())
		return
	}
	hop := requestHopByHop(payload.Headers)
	for k, vs := range payload.Headers {
		// Relay signalling vocabulary never reaches the handler (design
		// §6.4 — a client-forged X-Woca-* header dies here; the superset
		// covers X-Woca-Relay-*).
		if strings.HasPrefix(k, "X-Woca-") {
			continue
		}
		if hop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// Injected AFTER the strip: neither relay nor client can forge the
	// remote marker (design §6.4 — local audit marks remote origin).
	req.Header.Set(remoteHeader, "1")
	if payload.Client != "" {
		req.RemoteAddr = payload.Client
	} else {
		req.RemoteAddr = "relay:0"
	}

	ivl := c.cfg.HeartbeatInterval
	if ivl <= 0 {
		ivl = heartbeatInterval
	}
	pr, pw := io.Pipe()
	hb := newHeartbeatWriter(pw, ivl)
	rec := newStreamRecorder(hb)
	go func() {
		// The handler runs on its own goroutine, so execute's recover
		// cannot catch its panic — without this guard a single panicking
		// tunneled handler would crash the whole bridge process.
		defer func() {
			if r := recover(); r != nil {
				c.log.Error().Any("panic", r).Str("path", payload.Path).Msg("relayclient: tunneled request panicked")
				_ = pw.CloseWithError(fmt.Errorf("relayclient: handler panicked: %v", r))
			} else {
				_ = pw.Close()
			}
			rec.finish()
		}()
		c.cfg.Handler.ServeHTTP(rec, req)
	}()
	select {
	case <-rec.ready:
	case <-ctx.Done():
		_ = pr.CloseWithError(context.Canceled)
		hb.stop()
		return
	}
	if rec.isEventStream() {
		hb.arm()
	}
	c.respond(ctx, payload, rec, pr, hb)
}

// respond uploads the recorded response: frozen status + headers first,
// then the body chunk by chunk as the handler produces it (design §6.3 —
// the Go transport chunks the request body automatically).
func (c *Client) respond(ctx context.Context, payload pollPayload, rec *streamRecorder, pr *io.PipeReader, hb *heartbeatWriter) {
	defer hb.stop()
	rawHeaders, err := json.Marshal(rec.snapshot())
	if err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.cfg.URL+"/tunnel/respond/"+payload.ID, pr)
	if err != nil {
		_ = pr.CloseWithError(err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Key)
	req.Header.Set(generationHeader, c.generation())
	req.Header.Set(statusHeader, fmt.Sprintf("%d", rec.committedStatus()))
	req.Header.Set(headersHeader, base64.StdEncoding.EncodeToString(rawHeaders))
	resp, err := c.hc.Do(req)
	if err != nil {
		// Unstick the handler goroutine: further pipe writes fail fast
		// (design §6.5 — relay restart mid-respond).
		_ = pr.CloseWithError(err)
		c.setError("respond: " + err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body) // drain for connection reuse
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusGone || resp.StatusCode == http.StatusNotFound {
		// Client gone or a superseded generation: stop the handler
		// cleanly (the relay's 410/404 semantics, design §6.2).
		_ = pr.CloseWithError(fmt.Errorf("relayclient: respond rejected (HTTP %d)", resp.StatusCode))
		return
	}
	_ = pr.CloseWithError(errors.New("relayclient: respond complete"))
}

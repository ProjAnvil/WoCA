// Package msgplane is the run dispatch message plane (design §5.4): the
// shared PG queue (runs table, SKIP-LOCKED claim, leases) is always the
// source of truth — NATS is a progressive notification overlay that wakes
// idle workers instantly instead of leaving them polling. The overlay is
// additive by construction: if NATS is unavailable or misses a
// notification, the workers' poll fallback picks the run up anyway, so
// switching the plane on or off never interrupts the PG queue.
//
// Notifications are plain core-NATS pub/sub on one subject — deliberately
// not JetStream consumers: a notification is advisory (the run row in PG
// is the work item), so at-most-once delivery plus a poll safety net is
// the right cost/semantics trade. The compose-provisioned WOCA_RUNS
// stream stays reserved for a future full JetStream queue.
//
// The plane resolves its configuration live from the settings store on
// every operation (mode: "pg" | "pg+nats", NATS URL, subject), so console
// changes take effect without a restart. The client is lazily connected
// and reconnected when the URL changes.
package msgplane

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
)

// Config is one resolved message-plane configuration.
type Config struct {
	// Mode selects the plane: "pg" (queue only, the default) or "pg+nats"
	// (PG queue + NATS dispatch notifications).
	Mode string
	// NATSURL is the NATS server URL (nats://host:port). Empty in pg mode.
	NATSURL string
	// Subject is the dispatch notification subject (default runs.dispatch).
	Subject string
}

// Dispatch notification subjects. runs.dispatch wakes idle workers for a
// queued run (Phase 3); config.dispatch and tenant.dispatch wake roles for
// management-plane writes (design §16.7) — the payload is the new
// generation fingerprint, advisory only.
const (
	DefaultSubject = "runs.dispatch"
	ConfigSubject  = "config.dispatch"
	TenantSubject  = "tenant.dispatch"
)

// The accepted plane modes and the worker-side cadences.
const (
	pgMode     = "pg"
	pgNATSMode = "pg+nats"
	// pollEvery is the pg-mode worker poll cadence (and the degraded
	// pg+nats cadence while NATS is unreachable).
	pollEvery = 200 * time.Millisecond
	// pollTimeout is the pg+nats safety net: runs re-queued by the lease
	// reconciler produce no notification, so even a healthy NATS waiter
	// re-polls at this interval.
	pollTimeout = 15 * time.Second
)

// ValidMode reports whether mode is an accepted plane mode.
func ValidMode(mode string) bool {
	return mode == "" || mode == pgMode || mode == pgNATSMode
}

// NATSEnabled reports whether cfg routes dispatch notifications to NATS.
func (c Config) NATSEnabled() bool {
	return c.Mode == pgNATSMode && c.NATSURL != ""
}

// Resolver reads the live message-plane configuration (typically closing
// over the settings store, so console edits surface immediately).
type Resolver func() Config

// Plane owns the NATS connection and publishes dispatch notifications for
// the active configuration. A nil resolver keeps pg-mode semantics.
type Plane struct {
	resolve Resolver
	log     zerolog.Logger

	mu  sync.Mutex
	url string     // URL the current connection belongs to
	nc  *nats.Conn // nil until the first pg+nats operation
}

// New creates a plane. A nil resolver keeps pg-mode semantics.
func New(resolve Resolver, log zerolog.Logger) *Plane {
	return &Plane{resolve: resolve, log: log}
}

func (c Config) subj() string {
	if c.Subject == "" {
		return DefaultSubject
	}
	return c.Subject
}

// Notify publishes one run dispatch notification (run id payload).
func (p *Plane) Notify(runID string) {
	p.notify(DefaultSubject, runID)
}

// NotifyConfig publishes a config-plane change notification carrying the
// new settings generation (design §16.7). Best-effort, same as Notify.
func (p *Plane) NotifyConfig(generation string) {
	p.notify(ConfigSubject, generation)
}

// NotifyTenant publishes a tenant-plane change notification carrying the
// new tenant generation. Best-effort, same as Notify.
func (p *Plane) NotifyTenant(generation string) {
	p.notify(TenantSubject, generation)
}

// notify publishes one advisory notification. It is best-effort by design:
// the consumers' generation poll is the correctness net, so a failed
// publish logs and returns — it never fails the write that already landed
// in the shared database.
func (p *Plane) notify(subject, payload string) {
	if p == nil || p.resolve == nil {
		return
	}
	cfg := p.resolve()
	if !cfg.NATSEnabled() {
		return
	}
	nc, degraded := p.conn(cfg)
	if degraded {
		return
	}
	if err := nc.Publish(subject, []byte(payload)); err != nil {
		p.log.Warn().Str("subject", subject).Err(err).Msg("msgplane: notification failed (poll fallback covers it)")
	}
}

// conn returns a live connection for cfg, connecting or reconnecting as
// needed. degraded=true means NATS is configured but unreachable — callers
// fall back to pg-mode behavior and retry on the next operation.
func (p *Plane) conn(cfg Config) (*nats.Conn, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nc != nil && (p.url != cfg.NATSURL || !p.nc.IsConnected()) {
		_ = p.nc.Drain()
		p.nc = nil
	}
	if p.nc == nil {
		nc, err := nats.Connect(cfg.NATSURL,
			nats.Timeout(5*time.Second),
			nats.RetryOnFailedConnect(false),
			nats.ReconnectWait(2*time.Second),
		)
		if err != nil {
			p.log.Warn().Str("url", cfg.NATSURL).Err(err).Msg("msgplane: NATS connect failed (staying on the PG queue)")
			return nil, true
		}
		p.nc, p.url = nc, cfg.NATSURL
		p.log.Info().Str("url", cfg.NATSURL).Msg("msgplane: NATS dispatch overlay connected")
	}
	return p.nc, false
}

// Close releases the NATS connection (the PG queue is unaffected).
func (p *Plane) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.nc != nil {
		_ = p.nc.Drain()
		p.nc = nil
	}
}

// Waiter blocks a worker between claim attempts and exposes a wake channel
// for other plane consumers: in pg mode it sleeps one poll interval; in
// pg+nats mode it waits for any dispatch notification (run, config, or
// tenant — design §16.7) with the poll-timeout safety net. It shares the
// plane's connection and re-subscribes when the live configuration (URL or
// subject) changes.
type Waiter struct {
	plane  *Plane
	mu     sync.Mutex
	subs   []*nats.Subscription
	ch     chan *nats.Msg
	wake   chan struct{}
	pumped bool
	url    string
	subj   string
}

// NewWaiter creates the waiter over the same plane.
func NewWaiter(p *Plane) *Waiter {
	return &Waiter{plane: p, wake: make(chan struct{}, 1)}
}

// Wait blocks until a wake-up is warranted.
func (w *Waiter) Wait(ctx context.Context) {
	w.start()
	var cfg Config
	if w.plane != nil && w.plane.resolve != nil {
		cfg = w.plane.resolve()
	}
	if !cfg.NATSEnabled() {
		w.dropSubs()
		sleepCtx(ctx, pollEvery)
		return
	}
	if _, err := w.ensureSubs(cfg); err != nil {
		// No subscription: degrade to the pg poll cadence rather than spin.
		w.plane.log.Warn().Err(err).Msg("msgplane: subscribe failed; polling")
		sleepCtx(ctx, pollEvery)
		return
	}
	select {
	case <-ctx.Done():
	case <-w.wake:
	case <-time.After(pollTimeout):
	}
}

// Wake returns a channel that receives one signal per notification of any
// subject (run dispatch, config change, tenant change). The first call arms
// the background machinery, so consumers that never idle-loop (servers)
// still receive pushes; subsequent calls return the same channel.
func (w *Waiter) Wake() <-chan struct{} {
	w.start()
	return w.wake
}

// start launches the waiter's background machinery once: a subscriber loop
// that maintains the subscriptions against the live plane configuration
// (re-subscribing on URL/subject changes, backing off while NATS is
// unreachable), and a pump that forwards every notification into the wake
// channel.
func (w *Waiter) start() {
	w.mu.Lock()
	if w.pumped {
		w.mu.Unlock()
		return
	}
	w.pumped = true
	w.mu.Unlock()

	// Pump: one goroutine for the waiter's whole life. It re-reads the
	// live subscription channel under the lock each iteration, so
	// resubscribes swap channels without leaking goroutines.
	go func() {
		for {
			w.mu.Lock()
			ch := w.ch
			w.mu.Unlock()
			if ch == nil {
				sleepCtx(context.Background(), pollEvery)
				continue
			}
			if _, ok := <-ch; !ok {
				continue
			}
			select {
			case w.wake <- struct{}{}:
			default: // a burst coalesces: consumers re-read the fingerprint
			}
		}
	}()

	// Subscriber maintenance: arm immediately, then re-check every poll
	// interval (also the reconnect path after a NATS outage).
	arm := func() {
		var cfg Config
		if w.plane != nil && w.plane.resolve != nil {
			cfg = w.plane.resolve()
		}
		if !cfg.NATSEnabled() {
			w.dropSubs()
			return
		}
		if _, err := w.ensureSubs(cfg); err != nil {
			return // the poll fallback covers the miss
		}
	}
	go func() {
		arm()
		tick := time.NewTicker(pollEvery)
		defer tick.Stop()
		for range tick.C {
			arm()
		}
	}()
}

// subjects lists the notification subjects for cfg.
func (w *Waiter) subjects(cfg Config) []string {
	return []string{cfg.subj(), ConfigSubject, TenantSubject}
}

// ensureSubs returns a live notification channel subscribed to every
// subject, (re)subscribing when the configuration changed.
func (w *Waiter) ensureSubs(cfg Config) (<-chan *nats.Msg, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.subs) > 0 && w.url == cfg.NATSURL && w.subj == cfg.subj() {
		return w.ch, nil
	}
	w.dropSubsLocked()
	nc, degraded := w.plane.conn(cfg)
	if degraded {
		return nil, fmt.Errorf("NATS unreachable at %s", cfg.NATSURL)
	}
	ch := make(chan *nats.Msg, 64)
	for _, subj := range w.subjects(cfg) {
		sub, err := nc.ChanSubscribe(subj, ch)
		if err != nil {
			for _, s := range w.subs {
				_ = s.Unsubscribe()
			}
			w.subs = nil
			return nil, err
		}
		w.subs = append(w.subs, sub)
	}
	w.ch, w.url, w.subj = ch, cfg.NATSURL, cfg.subj()
	return ch, nil
}

// dropSubs releases the subscriptions (plane switched back to pg mode).
func (w *Waiter) dropSubs() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropSubsLocked()
}

func (w *Waiter) dropSubsLocked() {
	for _, s := range w.subs {
		_ = s.Unsubscribe()
	}
	w.subs, w.ch = nil, nil
}

// Close releases the waiter's subscriptions.
func (w *Waiter) Close() { w.dropSubs() }

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

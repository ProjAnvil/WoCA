package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Pool tuning (vars so tests can shrink them).
var (
	lspIdleTimeout      = 5 * time.Minute  // 空闲回收
	lspBrokenCooldown   = 10 * time.Minute // 熔断冷却
	lspMaxSpawnFailures = 2                // 连续 spawn 失败熔断阈值
	lspReapInterval     = time.Minute
)

// lspPool pools language-server processes by (workspace, command): a ready
// transport is reused directly, crashed ones are restarted once, and a key
// whose spawns keep failing is marked broken for a cooldown so a crashing
// server can't spin (opencode broken-set lesson). The LSP handshake
// (initialize/initialized) runs ONCE here, right after spawn — callers' fn
// must not re-initialize, since a second initialize against a live server
// is a protocol error that would make every pooled reuse fail.
type lspPool struct {
	spawn func([]string) (lspTransport, error)
	// init runs the one-time LSP handshake on a freshly spawned transport
	// for the workspace the key is pooled under. A handshake failure is
	// treated as a spawn failure (it counts toward the breaker).
	init func(tr lspTransport, workspace string) error

	// Tuning snapshots taken at construction (on the caller's goroutine):
	// the reaper goroutine must not read the package vars, which tests
	// mutate while other pools are still alive.
	idleTimeout time.Duration

	mu      sync.Mutex // map lookup/insert only; Call runs outside the lock
	entries map[string]*lspEntry

	stopCh  chan struct{}
	stopped bool
	wg      sync.WaitGroup
}

type lspEntry struct {
	tr          lspTransport
	lastUsed    time.Time
	failures    int
	brokenUntil time.Time

	// mu serializes fn execution on this entry's transport: concurrent
	// callers must never interleave their didOpen/diagnostic/didClose
	// sequences (same file: A's didClose could otherwise precede B's
	// diagnostic). Lock order is strictly p.mu → entry.mu, never reversed;
	// fn runs with neither lock held.
	mu sync.Mutex
}

func newLSPPool(spawn func([]string) (lspTransport, error), init func(lspTransport, string) error) *lspPool {
	p := &lspPool{
		spawn:       spawn,
		init:        init,
		entries:     map[string]*lspEntry{},
		stopCh:      make(chan struct{}),
		idleTimeout: lspIdleTimeout,
	}
	p.wg.Add(1)
	go p.reapLoop(lspReapInterval)
	return p
}

func lspPoolKey(workspace string, command []string) string {
	return workspace + "\x00" + strings.Join(command, " ")
}

// call runs fn with a ready, already-initialized transport. On fn error the
// entry is evicted and fn retried once on a freshly spawned server; the
// second error is returned. Calls sharing one server are serialized by the
// entry's mutex (see run).
func (p *lspPool) call(ctx context.Context, workspace string, command []string, fn func(lspTransport) error) error {
	key := lspPoolKey(workspace, command)
	e, tr, err := p.acquire(key, workspace, command)
	if err != nil {
		return err
	}
	err = p.run(e, tr, fn)
	p.touch(key, tr)
	if err == nil {
		return nil
	}
	p.evict(key, tr)
	e, tr, err = p.acquire(key, workspace, command)
	if err != nil {
		return err
	}
	err = p.run(e, tr, fn)
	p.touch(key, tr)
	if err != nil {
		p.evict(key, tr)
	}
	return err
}

// run executes fn under the entry's mutex. The transport is captured at
// acquire time so a concurrent evict (which nils e.tr under p.mu) can never
// make fn observe a nil transport; a concurrent Close is surfaced as the
// fn error and retried on a fresh server.
func (p *lspPool) run(e *lspEntry, tr lspTransport, fn func(lspTransport) error) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return fn(tr)
}

func (p *lspPool) acquire(key, workspace string, command []string) (*lspEntry, lspTransport, error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, nil, fmt.Errorf("lsp pool closed")
	}
	if e, ok := p.entries[key]; ok {
		if time.Now().Before(e.brokenUntil) {
			p.mu.Unlock()
			return nil, nil, fmt.Errorf("LSP server unavailable (cooling down after repeated spawn failures)")
		}
		if e.tr != nil {
			// Refresh under the lock: the entry is in use again, and the idle
			// reaper must not claim it mid-call based on a stale lastUsed.
			e.lastUsed = time.Now()
			p.mu.Unlock()
			return e, e.tr, nil
		}
	}
	p.mu.Unlock()

	tr, err := p.spawnAndInit(workspace, command) // spawn + handshake outside the lock

	p.mu.Lock()
	defer p.mu.Unlock()
	e := p.entries[key]
	if e == nil {
		e = &lspEntry{}
		p.entries[key] = e
	}
	if err != nil {
		e.failures++
		if e.failures >= lspMaxSpawnFailures {
			e.brokenUntil = time.Now().Add(lspBrokenCooldown)
			e.failures = 0
		}
		return nil, nil, err
	}
	if e.tr != nil { // 并发 spawn 竞速：保留先入池者
		_ = tr.Close()
		return e, e.tr, nil
	}
	e.tr = tr
	e.failures = 0
	e.lastUsed = time.Now()
	return e, tr, nil
}

// spawnAndInit spawns a server and runs the one-time LSP handshake. A
// handshake failure closes the transport and is reported as a spawn failure
// so it feeds the breaker.
func (p *lspPool) spawnAndInit(workspace string, command []string) (lspTransport, error) {
	tr, err := p.spawn(command)
	if err != nil {
		return nil, err
	}
	if p.init == nil {
		return tr, nil
	}
	if err := p.init(tr, workspace); err != nil {
		_ = tr.Close()
		return nil, err
	}
	return tr, nil
}

func (p *lspPool) touch(key string, tr lspTransport) {
	p.mu.Lock()
	if e, ok := p.entries[key]; ok && e.tr == tr {
		e.lastUsed = time.Now()
	}
	p.mu.Unlock()
}

func (p *lspPool) evict(key string, tr lspTransport) {
	p.mu.Lock()
	if e, ok := p.entries[key]; ok && e.tr == tr {
		e.tr = nil
	}
	p.mu.Unlock()
	_ = tr.Close()
}

func (p *lspPool) reapLoop(interval time.Duration) {
	defer p.wg.Done()
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-p.stopCh:
			return
		case <-tick.C:
			p.reapIdle()
		}
	}
}

func (p *lspPool) reapIdle() {
	cutoff := time.Now().Add(-p.idleTimeout)
	var stale []lspTransport
	p.mu.Lock()
	for key, e := range p.entries {
		if e.tr != nil && e.lastUsed.Before(cutoff) {
			stale = append(stale, e.tr)
			delete(p.entries, key)
		}
	}
	p.mu.Unlock()
	for _, tr := range stale {
		_ = tr.Close()
	}
}

// Close stops the reaper and shuts down every pooled transport.
func (p *lspPool) Close() {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return
	}
	p.stopped = true
	close(p.stopCh)
	var all []lspTransport
	for key, e := range p.entries {
		if e.tr != nil {
			all = append(all, e.tr)
		}
		delete(p.entries, key)
	}
	p.mu.Unlock()
	p.wg.Wait()
	for _, tr := range all {
		_ = tr.Close()
	}
}

// stats reports pooled (active) and broken-cooldown entry counts.
func (p *lspPool) stats() (active, broken int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for _, e := range p.entries {
		if e.tr != nil {
			active++
		}
		if now.Before(e.brokenUntil) {
			broken++
		}
	}
	return active, broken
}

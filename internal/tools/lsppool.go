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
// server can't spin (opencode broken-set lesson).
type lspPool struct {
	spawn func([]string) (lspTransport, error)

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
}

func newLSPPool(spawn func([]string) (lspTransport, error)) *lspPool {
	p := &lspPool{
		spawn:       spawn,
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

// call runs fn with a ready transport. On fn error the entry is evicted and
// fn retried once on a freshly spawned server; the second error is returned.
func (p *lspPool) call(ctx context.Context, workspace string, command []string, fn func(lspTransport) error) error {
	key := lspPoolKey(workspace, command)
	tr, err := p.acquire(key, command)
	if err != nil {
		return err
	}
	err = fn(tr)
	p.touch(key, tr)
	if err == nil {
		return nil
	}
	p.evict(key, tr)
	tr, err = p.acquire(key, command)
	if err != nil {
		return err
	}
	err = fn(tr)
	p.touch(key, tr)
	if err != nil {
		p.evict(key, tr)
	}
	return err
}

func (p *lspPool) acquire(key string, command []string) (lspTransport, error) {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil, fmt.Errorf("lsp pool closed")
	}
	if e, ok := p.entries[key]; ok {
		if time.Now().Before(e.brokenUntil) {
			p.mu.Unlock()
			return nil, fmt.Errorf("LSP server unavailable (cooling down after repeated spawn failures)")
		}
		if e.tr != nil {
			p.mu.Unlock()
			return e.tr, nil
		}
	}
	p.mu.Unlock()

	tr, err := p.spawn(command) // spawn outside the lock

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
		return nil, err
	}
	if e.tr != nil { // 并发 spawn 竞速：保留先入池者
		_ = tr.Close()
		return e.tr, nil
	}
	e.tr = tr
	e.failures = 0
	e.lastUsed = time.Now()
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

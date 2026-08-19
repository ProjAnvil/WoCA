// Package cache defines the cache interface used by business code and
// provides a local in-process implementation.
package cache

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrClosed is returned by operations on a closed cache.
var ErrClosed = errors.New("cache: closed")

// Cache is a key-value byte cache. Implementations must be safe for
// concurrent use.
type Cache interface {
	// Get returns the value stored for key. The second return value
	// reports whether the key was present and unexpired. The returned
	// slice is a copy; mutating it does not affect the cache.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Set stores value under key. A ttl <= 0 means the entry never
	// expires. The value is copied; later mutations of the caller's
	// slice do not affect the cache.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Delete removes key. Deleting a missing key is not an error.
	Delete(ctx context.Context, key string) error
	// Close releases resources held by the cache. After Close, all
	// other methods return ErrClosed. Close is idempotent.
	Close() error
}

// defaultJanitorInterval is the cleanup interval used by NewLocal.
const defaultJanitorInterval = time.Minute

// item is a single cache entry. expiresAt is zero for entries that
// never expire.
type item struct {
	value     []byte
	expiresAt time.Time
}

// Local is an in-process Cache backed by a memory map. Entries expire
// lazily on Get and are also swept by a background janitor goroutine.
type Local struct {
	mu       sync.Mutex
	items    map[string]item
	stop     chan struct{}
	stopped  chan struct{}
	stopOnce sync.Once
	closed   bool
}

// NewLocal returns a Local cache with a janitor interval of one minute.
func NewLocal() *Local {
	return NewLocalWithJanitor(defaultJanitorInterval)
}

// NewLocalWithJanitor returns a Local cache whose janitor sweeps expired
// entries every interval.
func NewLocalWithJanitor(interval time.Duration) *Local {
	c := &Local{
		items:   make(map[string]item),
		stop:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go c.janitor(interval)
	return c
}

// Get implements Cache.
func (c *Local) Get(_ context.Context, key string) ([]byte, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, false, ErrClosed
	}
	it, ok := c.items[key]
	if !ok {
		return nil, false, nil
	}
	if !it.expiresAt.IsZero() && !time.Now().Before(it.expiresAt) {
		delete(c.items, key)
		return nil, false, nil
	}
	return append([]byte(nil), it.value...), true, nil
}

// Set implements Cache.
func (c *Local) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	it := item{value: append([]byte(nil), value...)}
	if ttl > 0 {
		it.expiresAt = time.Now().Add(ttl)
	}
	c.items[key] = it
	return nil
}

// Delete implements Cache.
func (c *Local) Delete(_ context.Context, key string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	delete(c.items, key)
	return nil
}

// Close stops the janitor goroutine. Subsequent calls to other methods
// return ErrClosed.
func (c *Local) Close() error {
	c.stopOnce.Do(func() {
		close(c.stop)
		<-c.stopped
		c.mu.Lock()
		c.closed = true
		c.mu.Unlock()
	})
	return nil
}

// janitor periodically removes expired entries until stopped.
func (c *Local) janitor(interval time.Duration) {
	defer close(c.stopped)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			c.sweep()
		case <-c.stop:
			return
		}
	}
}

// sweep removes all expired entries.
func (c *Local) sweep() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, it := range c.items {
		if !it.expiresAt.IsZero() && !now.Before(it.expiresAt) {
			delete(c.items, k)
		}
	}
}

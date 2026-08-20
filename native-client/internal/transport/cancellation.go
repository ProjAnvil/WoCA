package transport

import (
	"context"
	"sync"
	"time"
)

const (
	requestIDLength         = 43
	maxPendingCancellations = 256
	pendingCancellationTTL  = 30 * time.Second
)

type cancellationRegistration struct {
	generation uint64
	cancel     context.CancelFunc
}

// CancellationRegistry bridges a Wails frontend abort to an upstream request
// context. CancelAll is terminal because it is used during application
// shutdown; a closed registry admits neither pending nor active work.
type CancellationRegistry struct {
	mu            sync.Mutex
	next          uint64
	active        map[string]cancellationRegistration
	pending       map[string]time.Time
	overflowUntil time.Time
	closed        bool
	now           func() time.Time
}

func NewCancellationRegistry() *CancellationRegistry {
	return newCancellationRegistryWithClock(time.Now)
}

func newCancellationRegistryWithClock(now func() time.Time) *CancellationRegistry {
	return &CancellationRegistry{
		active:  make(map[string]cancellationRegistration),
		pending: make(map[string]time.Time),
		now:     now,
	}
}

func (r *CancellationRegistry) register(parent context.Context, id string) (context.Context, func(), bool) {
	if r == nil || !validRequestID(id) {
		return rejectedRegistration(parent)
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return rejectedRegistration(parent)
	}
	now := r.currentTime()
	r.pruneExpiredLocked(now)
	if r.overflowUntil.After(now) {
		r.mu.Unlock()
		return rejectedRegistration(parent)
	}
	if _, exists := r.active[id]; exists {
		r.mu.Unlock()
		return rejectedRegistration(parent)
	}
	_, pending := r.pending[id]
	delete(r.pending, id)
	if pending {
		r.mu.Unlock()
		return rejectedRegistration(parent)
	}
	r.next++
	generation := r.next
	ctx, cancel := context.WithCancel(parent)
	r.active[id] = cancellationRegistration{generation: generation, cancel: cancel}
	r.mu.Unlock()

	unregister := func() {
		r.mu.Lock()
		current, exists := r.active[id]
		if exists && current.generation == generation {
			delete(r.active, id)
		}
		r.mu.Unlock()
		if exists && current.generation == generation {
			cancel()
		}
	}
	return ctx, unregister, true
}

func (r *CancellationRegistry) Cancel(id string) bool {
	if r == nil || !validRequestID(id) {
		return false
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return false
	}
	if registration, exists := r.active[id]; exists {
		r.mu.Unlock()
		registration.cancel()
		return true
	}
	now := r.currentTime()
	r.pruneExpiredLocked(now)
	expiresAt := now.Add(pendingCancellationTTL)
	if r.overflowUntil.After(now) {
		r.overflowUntil = laterDeadline(r.overflowUntil, expiresAt)
		r.mu.Unlock()
		return true
	}
	if pendingUntil, exists := r.pending[id]; exists {
		r.pending[id] = laterDeadline(pendingUntil, expiresAt)
		r.mu.Unlock()
		return true
	}
	if len(r.pending) >= maxPendingCancellations {
		r.overflowUntil = laterDeadline(r.overflowUntil, expiresAt)
		r.mu.Unlock()
		return true
	}
	r.pending[id] = expiresAt
	r.mu.Unlock()
	return true
}

func laterDeadline(current, candidate time.Time) time.Time {
	if candidate.After(current) {
		return candidate
	}
	return current
}

func (r *CancellationRegistry) CancelAll() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	cancels := make([]context.CancelFunc, 0, len(r.active))
	for _, registration := range r.active {
		cancels = append(cancels, registration.cancel)
	}
	clear(r.active)
	clear(r.pending)
	r.overflowUntil = time.Time{}
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (r *CancellationRegistry) currentTime() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

func (r *CancellationRegistry) pruneExpiredLocked(now time.Time) {
	for id, expiry := range r.pending {
		if !expiry.After(now) {
			delete(r.pending, id)
		}
	}
	if !r.overflowUntil.IsZero() && !r.overflowUntil.After(now) {
		r.overflowUntil = time.Time{}
	}
}

func rejectedRegistration(parent context.Context) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(parent)
	cancel()
	return ctx, func() {}, false
}

func (r *CancellationRegistry) isClosed() bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.closed
}

func validRequestID(id string) bool {
	if len(id) != requestIDLength {
		return false
	}
	for index := 0; index < len(id); index++ {
		char := id[index]
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

package transport

import (
	"context"
	"sync"
)

const (
	requestIDLength         = 43
	maxPendingCancellations = 256
)

type cancellationRegistration struct {
	generation uint64
	cancel     context.CancelFunc
}

// CancellationRegistry bridges a Wails frontend abort to an upstream request
// context. CancelAll is terminal because it is used during application
// shutdown; a closed registry admits neither pending nor active work.
type CancellationRegistry struct {
	mu      sync.Mutex
	next    uint64
	active  map[string]cancellationRegistration
	pending map[string]struct{}
	closed  bool
}

func NewCancellationRegistry() *CancellationRegistry {
	return &CancellationRegistry{
		active:  make(map[string]cancellationRegistration),
		pending: make(map[string]struct{}),
	}
}

func (r *CancellationRegistry) register(parent context.Context, id string) (context.Context, func(), bool) {
	if r == nil || !validRequestID(id) {
		return parent, func() {}, false
	}

	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return parent, func() {}, false
	}
	if _, exists := r.active[id]; exists {
		r.mu.Unlock()
		return parent, func() {}, false
	}
	_, pending := r.pending[id]
	delete(r.pending, id)
	r.next++
	generation := r.next
	ctx, cancel := context.WithCancel(parent)
	r.active[id] = cancellationRegistration{generation: generation, cancel: cancel}
	r.mu.Unlock()

	if pending {
		cancel()
	}
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
	if _, exists := r.pending[id]; exists {
		r.mu.Unlock()
		return true
	}
	if len(r.pending) >= maxPendingCancellations {
		r.mu.Unlock()
		return false
	}
	r.pending[id] = struct{}{}
	r.mu.Unlock()
	return true
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
	r.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
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

package relay

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrGenerationConflict rejects a poll from a second live generation of
// the same key (409 — the new generation backs off and the old one's
// lease expires within GenerationStaleAfter, design §6.2).
var ErrGenerationConflict = errors.New("relay: another bridge generation is live")

type generationState struct {
	token    string
	lastPoll time.Time
	polls    map[int]context.CancelCauseFunc
	nextPoll int
}

// GenerationRegistry tracks each key's live bridge generation and its
// in-flight polls (design §6.2): first-come-first-served with lease
// expiry, plus instant cancellation of parked polls on revocation.
type GenerationRegistry struct {
	mu   sync.Mutex
	now  func() time.Time
	gens map[int64]*generationState
}

// NewGenerationRegistry builds an empty registry (now is swappable in
// tests).
func NewGenerationRegistry() *GenerationRegistry {
	return &GenerationRegistry{now: time.Now, gens: map[int64]*generationState{}}
}

// Claim registers gen as keyID's generation. A different generation that
// is still live (last poll within the lease) conflicts; a stale one is
// evicted and gen takes over. adopted reports gen became the key's
// generation (bridge-up audit hooks off it); evicted reports a stale
// predecessor was displaced (bridge-down audit).
func (r *GenerationRegistry) Claim(keyID int64, gen string) (adopted, evicted bool, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	st, ok := r.gens[keyID]
	if ok && st.token != gen {
		if now.Sub(st.lastPoll) < GenerationStaleAfter {
			return false, false, ErrGenerationConflict
		}
		evicted = true
	}
	if !ok || st.token != gen {
		st = &generationState{token: gen, lastPoll: now, polls: map[int]context.CancelCauseFunc{}}
		r.gens[keyID] = st
		return true, evicted, nil
	}
	st.lastPoll = now
	return false, false, nil
}

// Current reports whether gen is keyID's live generation (respond
// scoping: a superseded generation's responds are 404, design §6.2).
func (r *GenerationRegistry) Current(keyID int64, gen string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.gens[keyID]
	return ok && st.token == gen
}

// Online is the bridge-online check for client forwarding (design §6.2:
// last poll within the lease window).
func (r *GenerationRegistry) Online(keyID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.gens[keyID]
	return ok && r.now().Sub(st.lastPoll) < GenerationStaleAfter
}

// RegisterPoll tracks one in-flight long poll so revocation can wake it
// instantly (design §5.1). ok is false when gen is no longer current.
// The returned func unregisters.
func (r *GenerationRegistry) RegisterPoll(keyID int64, gen string, cancel context.CancelCauseFunc) (func(), bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.gens[keyID]
	if !ok || st.token != gen {
		return nil, false
	}
	id := st.nextPoll
	st.nextPoll++
	st.polls[id] = cancel
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(st.polls, id)
	}, true
}

// CancelPolls wakes every in-flight poll of keyID with cause (410 revoked
// immediately, without waiting out the 25s poll window, design §5.1).
func (r *GenerationRegistry) CancelPolls(keyID int64, cause error) {
	r.mu.Lock()
	st, ok := r.gens[keyID]
	var cancels []context.CancelCauseFunc
	if ok {
		cancels = make([]context.CancelCauseFunc, 0, len(st.polls))
		for _, c := range st.polls {
			cancels = append(cancels, c)
		}
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c(cause)
	}
}

// Drop removes keyID's generation state (revocation).
func (r *GenerationRegistry) Drop(keyID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.gens, keyID)
}

package agent

// Per-session permission policy (design §3.3): the permission mode, the
// plan-mode flag and session-scoped grants belong to ONE session, not to the
// process. Serve-mode sessions run concurrently over a shared Agent, so the
// policy rides the run context (the runws pattern: one decoration scopes a
// whole run) and the backing store is injected at assembly — the server owns
// a per-session store; the TUI/CLI/tests keep the agent-global fields via the
// default adapter.

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/projanvil/woca/internal/config"
)

// SessionPolicy is one session's permission state: the effective permission
// mode, the plan-mode flag (plan=true restricts tools to the read-only arm
// until ExitPlanMode is approved), and session grants ("allow for session"
// answers; populated by the approval flow, keyed by rule — see design §3.3).
type SessionPolicy struct {
	Mode   config.PermissionMode
	Plan   bool
	Grants map[string]bool
}

// SessionPolicySource resolves and updates per-session policy.
// Implementations must be safe for concurrent use: sessions run in parallel
// over one agent. For returns a snapshot — struct copy plus a copy of the
// grants map, so a returned policy never shares mutable state with the live
// entry; all mutations go through SetMode, SetPlan and the grant mutators
// (AddGrant/RevokeGrant, which replace the grants map copy-on-write).
type SessionPolicySource interface {
	For(session string) *SessionPolicy
	SetMode(session string, m config.PermissionMode)
	SetPlan(session string, v bool)
	// AddGrant records a session grant ("allow for session", §3.3); the
	// empty key is ignored (it can never match a call — see GrantKey).
	AddGrant(session, key string)
	// RevokeGrant removes one session grant; revoking an absent key is a
	// no-op.
	RevokeGrant(session, key string)
	// ListGrants returns the session's grant keys, sorted.
	ListGrants(session string) []string
}

// sessionPolicyCarrier is the ctx payload: the run's session id plus the
// store that owns its policy.
type sessionPolicyCarrier struct {
	session string
	src     SessionPolicySource
}

type sessionPolicyKey struct{}

// WithSessionPolicy returns a context carrying the run's session id and the
// policy source that stores its policy. A nil source is a no-op (the
// agent-global fallbacks apply).
func WithSessionPolicy(ctx context.Context, session string, src SessionPolicySource) context.Context {
	if src == nil {
		return ctx
	}
	return context.WithValue(ctx, sessionPolicyKey{}, sessionPolicyCarrier{session: session, src: src})
}

// SessionPolicyFrom reports the run-scoped session policy carrier, if any.
func SessionPolicyFrom(ctx context.Context) (session string, src SessionPolicySource, ok bool) {
	if ctx == nil {
		return "", nil, false
	}
	c, ok := ctx.Value(sessionPolicyKey{}).(sessionPolicyCarrier)
	if !ok || c.src == nil {
		return "", nil, false
	}
	return c.session, c.src, true
}

// PerSessionPolicySource is a SessionPolicySource that stores each named
// session's policy in its own entry. Sessions without an entry inherit the
// base policy: the construction-time mode (and its later writes for the
// unnamed "" session — the legacy single-session surface) stay visible to
// every session that never got an explicit write, so pre-session clients
// keep today's behavior.
type PerSessionPolicySource struct {
	mu       sync.Mutex
	base     SessionPolicy
	sessions map[string]*SessionPolicy
}

// NewPerSessionPolicySource builds a per-session store whose sessions start
// from defaultMode.
func NewPerSessionPolicySource(defaultMode config.PermissionMode) *PerSessionPolicySource {
	return &PerSessionPolicySource{
		base:     SessionPolicy{Mode: defaultMode},
		sessions: make(map[string]*SessionPolicy),
	}
}

// For returns a snapshot of the session's policy (the base policy when the
// session has no entry of its own). The grants map is copied too, so the
// snapshot never aliases the live entry's map.
func (s *PerSessionPolicySource) For(session string) *SessionPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.sessions[session]; ok {
		return p.detached()
	}
	cp := s.base
	cp.Grants = copyGrants(s.base.Grants)
	return &cp
}

// SetMode sets the session's permission mode. The unnamed session ("")
// writes the base policy every session inherits; a named session gets its
// own entry seeded from the base.
func (s *PerSessionPolicySource) SetMode(session string, m config.PermissionMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		s.base.Mode = m
		return
	}
	s.entryLocked(session).Mode = m
}

// SetPlan sets the session's plan-mode flag. The unnamed session ("")
// writes the base policy; a named session gets its own entry seeded from
// the base.
func (s *PerSessionPolicySource) SetPlan(session string, v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		s.base.Plan = v
		return
	}
	s.entryLocked(session).Plan = v
}

// entryLocked returns (creating if needed, seeded from the base) the named
// session's own entry. The seed's grants map is copied: a session entry must
// never alias the base map, or a base grant would mutate into (or out of)
// every entry created from it. Callers hold mu.
func (s *PerSessionPolicySource) entryLocked(session string) *SessionPolicy {
	if p, ok := s.sessions[session]; ok {
		return p
	}
	p := s.base
	p.Grants = copyGrants(s.base.Grants)
	s.sessions[session] = &p
	return &p
}

// AddGrant records a session grant (design §3.3). The unnamed session ("")
// writes the base policy sessions inherit at entry creation; a named session
// gets its own entry. The entry's grants map is replaced (copy-on-write), so
// outstanding For snapshots keep their own view.
func (s *PerSessionPolicySource) AddGrant(session, key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		s.base.Grants = withGrant(s.base.Grants, key, true)
		return
	}
	e := s.entryLocked(session)
	e.Grants = withGrant(e.Grants, key, true)
}

// RevokeGrant removes one session grant; revoking an absent key is a no-op.
// Copy-on-write, like AddGrant.
func (s *PerSessionPolicySource) RevokeGrant(session, key string) {
	if key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if session == "" {
		s.base.Grants = withGrant(s.base.Grants, key, false)
		return
	}
	p, ok := s.sessions[session]
	if !ok {
		return // nothing granted, nothing to revoke (no entry creation)
	}
	p.Grants = withGrant(p.Grants, key, false)
}

// ListGrants returns the session's grant keys, sorted (the base's when the
// session has no entry of its own).
func (s *PerSessionPolicySource) ListGrants(session string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.base.Grants
	if p, ok := s.sessions[session]; ok {
		g = p.Grants
	}
	out := make([]string, 0, len(g))
	for k, on := range g {
		if on {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// detached returns a copy of p with its own grants map — the snapshot form
// For returns.
func (p *SessionPolicy) detached() *SessionPolicy {
	cp := *p
	cp.Grants = copyGrants(p.Grants)
	return &cp
}

// copyGrants returns a copy of src (nil stays nil — an empty grant set and an
// absent one read identically at every consultation site).
func copyGrants(src map[string]bool) map[string]bool {
	if src == nil {
		return nil
	}
	return maps.Clone(src)
}

// withGrant returns a new map with key set (or removed), leaving the original
// untouched — the copy-on-write behind every grant mutation, so snapshots can
// never observe a later write.
func withGrant(src map[string]bool, key string, on bool) map[string]bool {
	out := make(map[string]bool, len(src)+1)
	maps.Copy(out, src)
	if on {
		out[key] = true
	} else {
		delete(out, key)
	}
	return out
}

// agentPolicySource adapts an Agent's own mode/planMode/grants fields as a
// SessionPolicySource — the default when construction injected no store.
// The session key is deliberately ignored: without a per-session store every
// session shares the agent-global fields, exactly today's single-session
// behavior (which is why cross-session isolation — the X1 leak — needs a
// real PerSessionPolicySource, not this adapter).
type agentPolicySource struct{ a *Agent }

func (s agentPolicySource) For(string) *SessionPolicy {
	a := s.a
	a.mu.Lock()
	defer a.mu.Unlock()
	return &SessionPolicy{Mode: a.mode, Plan: a.planMode, Grants: copyGrants(a.grants)}
}

func (s agentPolicySource) SetMode(_ string, m config.PermissionMode) { s.a.SetMode(m) }

func (s agentPolicySource) SetPlan(_ string, v bool) { s.a.SetPlanMode(v) }

func (s agentPolicySource) AddGrant(_, key string) { s.a.addGrantGlobal(key, true) }

func (s agentPolicySource) RevokeGrant(_, key string) { s.a.addGrantGlobal(key, false) }

func (s agentPolicySource) ListGrants(string) []string {
	a := s.a
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.grants))
	for k, on := range a.grants {
		if on {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out
}

// PolicySource returns the agent's session-policy source: the store injected
// at construction when one was wired (the server form), otherwise a
// singleton adapter over the agent's own mode/planMode/grants fields (the
// default — every session shares them). Single-session surfaces (TUI
// /permissions, session restore) route their writes through it so both forms
// behave the same.
func (a *Agent) PolicySource() SessionPolicySource {
	if a.policy != nil {
		return a.policy
	}
	return agentPolicySource{a}
}

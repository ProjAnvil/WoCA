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
// over one agent. For returns a snapshot; mutations go through SetMode and
// SetPlan (and, from Task 3 on, the grant mutators).
type SessionPolicySource interface {
	For(session string) *SessionPolicy
	SetMode(session string, m config.PermissionMode)
	SetPlan(session string, v bool)
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
// session has no entry of its own).
func (s *PerSessionPolicySource) For(session string) *SessionPolicy {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.sessions[session]; ok {
		cp := *p
		return &cp
	}
	cp := s.base
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
// session's own entry. Callers hold mu.
func (s *PerSessionPolicySource) entryLocked(session string) *SessionPolicy {
	if p, ok := s.sessions[session]; ok {
		return p
	}
	p := s.base
	s.sessions[session] = &p
	return &p
}

// agentPolicySource adapts an Agent's own mode/planMode fields as a
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
	return &SessionPolicy{Mode: a.mode, Plan: a.planMode}
}

func (s agentPolicySource) SetMode(_ string, m config.PermissionMode) { s.a.SetMode(m) }

func (s agentPolicySource) SetPlan(_ string, v bool) { s.a.SetPlanMode(v) }

// PolicySource returns the agent's session-policy source: the store injected
// at construction when one was wired (the server form), otherwise a
// singleton adapter over the agent's own mode/planMode fields (the default —
// every session shares them). Single-session surfaces (TUI /permissions,
// session restore) route their writes through it so both forms behave the
// same.
func (a *Agent) PolicySource() SessionPolicySource {
	if a.policy != nil {
		return a.policy
	}
	return agentPolicySource{a}
}

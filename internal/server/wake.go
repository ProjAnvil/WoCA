package server

import (
	"context"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/settings"
)

// Idle wake driver (background loop closure, design §5.2): a notification
// queued for an idle session — a background shell or task completion, a cron
// firing — would otherwise sit in the mid-turn queue until the next
// user-initiated run. The driver closes that loop by starting an ordinary
// run whose prompt is the coalesced tagged message the queue already
// produces. Wake runs are ordinary runs in every respect (admission, ledger,
// SSE, permission posture; in dispatch mode they enqueue like any run); only
// their run_started event carries auto/trigger annotations.

// onQueuedNotification is the agent's notification-observer hook (installed
// in New): q was just queued. Quiet items never start runs (D1): a
// user-initiated stop is delivered with the next user run instead of waking
// the agent over what the user just stopped. Unscoped items (Session "") have
// no session to wake — the single-session surfaces (TUI, REPL) own that case
// — and a busy session needs no wake: the running run's tool rounds drain the
// queue in-turn. An idle session hands off to maybeWake on a fresh goroutine
// because the observer contract is synchronous and must not block.
func (s *Server) onQueuedNotification(q agent.QueuedInput) {
	if q.Quiet || q.Session == "" || s.admission.busy(q.Session) {
		return
	}
	go s.maybeWake(q.Session)
}

// maybeWake is the observer path: the session looked idle a moment ago, so
// re-check under the lock and wake. The whole decision — busy re-check,
// policy read, cap check, drain, start — is serialized by wakeMu, so
// notifications completing concurrently coalesce into a single wake run: the
// loser of the mutex finds the queue already drained.
func (s *Server) maybeWake(sid string) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	if s.admission.busy(sid) {
		// A run started in the meantime; its tool rounds deliver the item.
		return
	}
	s.wakeLocked(sid)
}

// wakeAfterRun is the run-finish path (startRunWith's onFinish): the
// finishing run still holds the session gate for the short stretch between
// onFinish and onDone's admission release, so the busy re-check is skipped —
// startRunWith queues the wake run behind the finishing one, and the gate's
// FIFO promotes it only after the finishing transcript has persisted, the
// same ordering a queued user run gets.
func (s *Server) wakeAfterRun(sid string) {
	s.wakeMu.Lock()
	defer s.wakeMu.Unlock()
	s.wakeLocked(sid)
}

// wakeLocked is maybeWake/wakeAfterRun's shared body, already under wakeMu.
func (s *Server) wakeLocked(sid string) {
	if s.agent == nil {
		return
	}
	if s.sessions.isDeleted(sid) {
		// D2: a deleted session is never woken. ReleaseSession already
		// refused new completions and dropped queued ones; this guard holds
		// for the race it cannot cover — a session deleted while one of its
		// runs was still in flight (the run's finish reaches wakeAfterRun
		// after the delete). Drain and discard whatever is still queued for
		// it; nothing is retried or requeued.
		if items := s.agent.DrainQueuedInputsFor(sid); len(items) > 0 {
			s.log.Info().Str("session", sid).Int("items", len(items)).
				Msg("wake skipped for deleted session; queued items discarded")
		}
		if obs := s.agent.Obs(); obs != nil {
			obs.Log("info", "wake_skip", map[string]any{"session": sid, "reason": "deleted"})
		}
		return
	}
	if s.wakeStalled[sid] {
		return // last wake start failed; a user run must clear the stall
	}
	// Nothing wake-worthy means nothing to do — checked before the policy and
	// cap reads so a finishing run with an empty (or Quiet-only) queue (the
	// common case) neither loads settings nor announces a cap it did not hit.
	// Quiet items alone never start a run (D1); they ride the next user run.
	queued := s.agent.WakeableCountFor(sid)
	if queued == 0 {
		return
	}
	on, limit := s.wakePolicy()
	if !on {
		return
	}
	if limit > 0 && s.wakeChain[sid] >= limit {
		// Chain budget exhausted: the item stays queued (the next user run
		// delivers it and resets the budget), and the global stream tells
		// the UI why the session went quiet. A bound, not a fallback —
		// nothing is dropped or retried.
		s.log.Info().Str("session", sid).Int("limit", limit).Int("queued", queued).
			Msg("auto-wake chain capped; notifications stay queued")
		s.publishWakeCapped(sid, queued)
		return
	}
	items := s.agent.DrainQueuedInputsFor(sid)
	if len(items) == 0 {
		return // drained between the count and here; the taker owns delivery
	}
	trigger := items[0].Tag
	// The wake run has no request to carry a workspace, so resolve the
	// session's recorded one from the store — the same value the web client
	// sent at session creation — keeping tools, prompt, hooks and memory
	// scoped to the session's project (and, in dispatch mode, feeding the
	// ledger snapshot's Workspace). A session with no recorded workspace
	// still falls back to the server workdir inside newRegisteredRun/
	// enqueueRun. The base context derives from the server lifecycle
	// (rootCtx), not a detached Background, so a dispatched wake run's
	// workspace binding (resolveRunWorkspace) and cancellation follow the
	// server.
	run, _, _, err := s.startRunWith(s.rootCtx, sid, agent.CoalesceQueuedInputs(items), s.sessions.workspaceOf(sid), nil, runOptions{auto: true, trigger: trigger})
	if err != nil {
		// One attempt, no retry ladder (design §5.2): log the failure, put
		// the items back, and stall this session's driver until a user run
		// resets it — re-arming on every late completion would be exactly
		// the retry loop the design rules out.
		s.wakeStalled[sid] = true
		s.log.Warn().Err(err).Str("session", sid).Str("trigger", trigger).Int("items", len(items)).
			Msg("wake run failed to start; notifications requeued")
		// Requeue each item with its Quiet flag intact: a user stop must stay
		// quiet even on the recovery path.
		for _, q := range items {
			if q.Quiet {
				s.agent.QueueQuietNotificationFor(q.Session, q.Tag, q.Text)
			} else {
				s.agent.QueueNotificationFor(q.Session, q.Tag, q.Text)
			}
		}
		// The stall is panel-visible state (snapshot wake.stalled): signal
		// the wake slice change.
		s.onActivityChanged(sid, agent.ActivityWake)
		return
	}
	s.wakeChain[sid]++
	s.log.Info().Str("session", sid).Str("run", run.ID()).Str("trigger", trigger).Msg("auto-wake run started")
}

// resetWakeChain clears the session's chain budget and any stall: a
// user-initiated run is the design's reset point — the budget restarts, and
// a stalled driver gets another chance (items left queued by a cap or a
// failed start ride the user run through the agent's run-start delivery).
// Clearing a consumed budget or a stall changes panel-visible wake state,
// so it signals the wake slice (a cap hit still additionally emits the
// existing wake_capped event at the moment it happens).
func (s *Server) resetWakeChain(sid string) {
	s.wakeMu.Lock()
	_, hadChain := s.wakeChain[sid]
	wasStalled := s.wakeStalled[sid]
	delete(s.wakeChain, sid)
	delete(s.wakeStalled, sid)
	s.wakeMu.Unlock()
	if hadChain || wasStalled {
		s.onActivityChanged(sid, agent.ActivityWake)
	}
}

// forgetWakeState drops the session's wake-driver bookkeeping (chain budget
// and stall flag) when the session is deleted: nothing else would ever clear
// them — a user run cannot arrive for a session that no longer exists — so
// without this the maps would only grow. Silent by contrast with
// resetWakeChain: a deleted session has no panel left to signal.
func (s *Server) forgetWakeState(sid string) {
	s.wakeMu.Lock()
	delete(s.wakeChain, sid)
	delete(s.wakeStalled, sid)
	s.wakeMu.Unlock()
}

// wakePolicy reads the live auto-wake switch and chain budget. A nil store
// (tests, embedders without the settings API) keeps the design defaults:
// wake on, DefaultAutoWakeLimit chained runs.
func (s *Server) wakePolicy() (on bool, limit int) {
	if s.settings == nil {
		return true, settings.DefaultAutoWakeLimit
	}
	st := s.settings.Load()
	return st.AutoWake, st.AutoWakeLimit
}

// publishWakeCapped announces the cap on the global event stream (the same
// topic and Publish path sessionStateEvent uses, so /api/events clients see
// it) with the session and how many items are holding for a user run.
func (s *Server) publishWakeCapped(sid string, queued int) {
	if _, err := s.hub.Publish(context.Background(), sessionStateTopic, Event{Type: EventWakeCapped, Data: map[string]any{
		"session": sid,
		"queued":  queued,
	}}); err != nil {
		s.log.Debug().Err(err).Str("session", sid).Msg("wake_capped publish failed")
	}
}

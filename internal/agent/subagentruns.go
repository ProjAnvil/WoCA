package agent

// Session-scoped registry of foreground subagent children (graph-native HITL
// design §3.2/§3.7, Task 7). route2 counts it when admitting foreground Task
// calls to the fan-out (the concurrency cap, D12), the child-side node
// wrapper feeds it lifecycle facts (started/waiting/done/failed/stopped), and
// the Subagents activity tab (Task 12) reads its snapshot. Entries are keyed
// by (session, parent tool-call id) — call ids are unique per conversation,
// and the session qualifier keeps concurrent serve-mode sessions from
// colliding the way the speculative registry's run keys do.

import (
	"cmp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/tools"
)

// defaultMaxSubagents caps concurrently active foreground subagent children
// per session (spec decision D12). Hard-coded until Task 10 wires the
// `subagents.max-concurrent` settings key; over-cap calls are REJECTED with
// a reason, never queued.
const defaultMaxSubagents = 8

// SubagentRunState is one child's lifecycle state.
type SubagentRunState string

const (
	// SubagentRunning: the child graph is executing.
	SubagentRunning SubagentRunState = "running"
	// SubagentWaiting: the child paused on an interrupt (approval ask)
	// awaiting the user's answer.
	SubagentWaiting SubagentRunState = "waiting"
	// SubagentDone: the child finished and its report backfilled.
	SubagentDone SubagentRunState = "done"
	// SubagentFailed: the child errored out.
	SubagentFailed SubagentRunState = "failed"
	// SubagentStopped: the child died with its parent run's cancellation.
	SubagentStopped SubagentRunState = "stopped"
)

// terminal reports whether state accepts no further transitions.
func (s SubagentRunState) terminal() bool {
	return s == SubagentDone || s == SubagentFailed || s == SubagentStopped
}

// SubagentRun is the snapshot vocabulary of one registry entry.
type SubagentRun struct {
	// baseTokens is the shared child Agent's usage total at this child's
	// first node: same-def children share one Agent whose usage accumulates
	// across siblings, so the exit node reports the delta (unexported, not
	// part of the wire snapshot).
	baseTokens int
	// Def is the subagent definition's display name ("general" for default
	// children); CallID is the parent's Task tool-call id; Session is the
	// starting run's session id; Origin is the shell-owner form
	// "subagent:<def>#<callID>".
	Def           string           `json:"def"`
	CallID        string           `json:"call_id"`
	Session       string           `json:"session"`
	Origin        string           `json:"origin"`
	State         SubagentRunState `json:"state"`
	Tokens        int              `json:"tokens,omitempty"` // at finish, when available
	Report        string           `json:"report,omitempty"`
	StartedAt     time.Time        `json:"started_at"`
	EndedAt       time.Time        `json:"ended_at,omitempty"`
}

// SubagentEvent is one lifecycle fact, emitted through the registry's
// observer (the session event-bus seam; the activity panel and the
// Subagents tab consume it from Task 12).
type SubagentEvent struct {
	Def     string           `json:"def"`
	CallID  string           `json:"call_id"`
	Session string           `json:"session"`
	State   SubagentRunState `json:"state"`
	Tokens  int              `json:"tokens,omitempty"`
	Report  string           `json:"report,omitempty"`
}

// subagentRuns is the session-scoped registry behind Agent.subagents. Safe
// for concurrent use; observers must not block.
type subagentRuns struct {
	mu       sync.Mutex
	runs     map[string]*SubagentRun
	bases    map[string]int
	max      int
	onChange func(session string)
	onEvent  func(session string, ev SubagentEvent)
}

func newSubagentRuns(max int) *subagentRuns {
	if max <= 0 {
		max = defaultMaxSubagents
	}
	return &subagentRuns{runs: map[string]*SubagentRun{}, bases: map[string]int{}, max: max}
}

func runKey(session, callID string) string { return session + "\x00" + callID }

// seedTokenBase records the shared child Agent's usage total at a child's
// first node (the per-child delta baseline; see SubagentRun.baseTokens).
func (r *subagentRuns) seedTokenBase(session, callID string, base int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.runs[runKey(session, callID)]; ok && !cur.State.terminal() {
		return // an earlier lifetime of this child owns its baseline
	}
	r.bases[runKey(session, callID)] = base
}

// tokenBaseOf returns the seeded baseline (0 when none).
func (r *subagentRuns) tokenBaseOf(session, callID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.bases[runKey(session, callID)]
}

// start registers a child's start, reporting whether this call performed the
// registration. Idempotent: a paused-and-resumed child re-enters its nodes,
// the first entry wins, and a waiting child flips back to running (the
// resume transition).
func (r *subagentRuns) start(run SubagentRun) bool {
	r.mu.Lock()
	key := runKey(run.Session, run.CallID)
	if cur, ok := r.runs[key]; ok && !cur.State.terminal() {
		resumed := cur.State == SubagentWaiting
		if resumed {
			cur.State = SubagentRunning
		}
		ev := SubagentEvent{Def: cur.Def, CallID: cur.CallID, Session: cur.Session, State: cur.State}
		r.mu.Unlock()
		if resumed {
			r.fire(ev)
			r.change(run.Session)
		}
		return false
	}
	// A TERMINAL entry (or none) means the child is starting afresh: a
	// cancelled-then-resumed run re-enters its child nodes after stopSession
	// tore the registry down — re-register so the cap counts it and the
	// SubagentStart/Stop hooks fire again for the resumed lifetime. The old
	// record is replaced (its stopped verdict described the cancelled
	// lifetime, not this one).
	run.State = SubagentRunning
	run.StartedAt = time.Now()
	r.runs[key] = &run
	ev := SubagentEvent{Def: run.Def, CallID: run.CallID, Session: run.Session, State: SubagentRunning}
	r.mu.Unlock()
	r.fire(ev)
	r.change(run.Session)
	return true
}

// waiting marks a child paused on an interrupt (idempotent). An unknown
// entry — a child whose start was never seen — is created, keeping the
// registry conservative for the cap.
func (r *subagentRuns) waiting(run SubagentRun) {
	r.mu.Lock()
	key := runKey(run.Session, run.CallID)
	cur, ok := r.runs[key]
	if !ok {
		run.State = SubagentWaiting
		run.StartedAt = time.Now()
		r.runs[key] = &run
		cur = &run
	} else if cur.State == SubagentRunning {
		cur.State = SubagentWaiting
	} else {
		r.mu.Unlock()
		return
	}
	ev := SubagentEvent{Def: cur.Def, CallID: cur.CallID, Session: cur.Session, State: SubagentWaiting}
	r.mu.Unlock()
	r.fire(ev)
	r.change(cur.Session)
}

// finish moves a child to a terminal state exactly once (a completion racing
// a stop keeps the first verdict), reporting whether this call performed the
// transition. Finished records are pruned per session, oldest first,
// mirroring TaskManager.pruneFinished.
func (r *subagentRuns) finish(run SubagentRun, state SubagentRunState) bool {
	r.mu.Lock()
	key := runKey(run.Session, run.CallID)
	cur, ok := r.runs[key]
	if !ok {
		run.State = state
		run.StartedAt = time.Now()
		run.EndedAt = time.Now()
		run.Report = reportTail(run.Report)
		r.runs[key] = &run
		cur = &run
	} else if cur.State.terminal() {
		r.mu.Unlock()
		return false
	} else {
		cur.State = state
		cur.EndedAt = time.Now()
		cur.Tokens = run.Tokens
		if run.Report != "" {
			cur.Report = reportTail(run.Report)
		}
	}
	session := cur.Session
	delete(r.bases, key) // the lifetime that owned it is terminal
	r.pruneFinishedLocked(session)
	ev := SubagentEvent{Def: cur.Def, CallID: cur.CallID, Session: session, State: cur.State, Tokens: cur.Tokens, Report: cur.Report}
	r.mu.Unlock()
	r.fire(ev)
	r.change(session)
	return true
}

// fire emits one lifecycle event to the installed observer, no lock held.
func (r *subagentRuns) fire(ev SubagentEvent) {
	r.mu.Lock()
	fn := r.onEvent
	r.mu.Unlock()
	if fn != nil {
		fn(ev.Session, ev)
	}
}

// change fires the activity-seam change callback for session.
func (r *subagentRuns) change(session string) {
	r.mu.Lock()
	fn := r.onChange
	r.mu.Unlock()
	if fn != nil {
		fn(session)
	}
}

// SetOnChange installs the activity-seam callback (fires on every state
// transition, with the owning session id).
func (r *subagentRuns) SetOnChange(fn func(session string)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onChange = fn
}

// SetOnEvent installs the lifecycle-event observer (started/waiting/done/
// failed/stopped with def, call id, session and token counts when
// available). It must not block.
func (r *subagentRuns) SetOnEvent(fn func(session string, ev SubagentEvent)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onEvent = fn
}

// activeCount reports the session's running+waiting children — the cap's
// accounting unit (a parked child still holds its slot; it resumes).
func (r *subagentRuns) activeCount(session string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, run := range r.runs {
		if run.Session == session && !run.State.terminal() {
			n++
		}
	}
	return n
}

// capLimit reports the configured maximum (Task 10 wires the settings key).
func (r *subagentRuns) capLimit() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max
}

// stopSession marks every still-active child of session stopped — the
// cancelled-run cleanup (EndRun over a dead ctx: the children died with the
// run). A parked run's ctx is alive, so its waiting children keep their
// entries. It returns how many children were stopped.
func (r *subagentRuns) stopSession(session string) int {
	r.mu.Lock()
	var stopped []SubagentEvent
	for _, run := range r.runs {
		if run.Session == session && !run.State.terminal() {
			run.State = SubagentStopped
			run.EndedAt = time.Now()
			stopped = append(stopped, SubagentEvent{Def: run.Def, CallID: run.CallID, Session: session, State: SubagentStopped})
		}
	}
	r.mu.Unlock()
	for _, ev := range stopped {
		r.fire(ev)
	}
	if len(stopped) > 0 {
		r.change(session)
	}
	return len(stopped)
}

// snapshotFor returns the session's runs in start order (the Subagents-tab
// source; Task 12 wires the activity seam to it).
func (r *subagentRuns) snapshotFor(session string) []SubagentRun {
	r.mu.Lock()
	out := make([]SubagentRun, 0, len(r.runs))
	for _, run := range r.runs {
		if run.Session == session {
			out = append(out, *run)
		}
	}
	r.mu.Unlock()
	slices.SortFunc(out, func(a, b SubagentRun) int {
		return cmp.Or(a.StartedAt.Compare(b.StartedAt), strings.Compare(a.CallID, b.CallID))
	})
	return out
}

// pruneFinishedLocked caps the session's finished records, dropping the
// OLDEST finished first; running/waiting records are always kept. The caller
// holds mu.
func (r *subagentRuns) pruneFinishedLocked(session string) {
	var finished []SubagentRun
	for _, run := range r.runs {
		if run.Session != session || !run.State.terminal() {
			continue
		}
		finished = append(finished, SubagentRun{CallID: run.CallID, EndedAt: run.EndedAt, Session: run.Session})
	}
	if len(finished) <= tools.MaxFinishedRecordsPerSession {
		return
	}
	slices.SortFunc(finished, func(a, b SubagentRun) int {
		return cmp.Or(a.EndedAt.Compare(b.EndedAt), strings.Compare(a.CallID, b.CallID))
	})
	for _, run := range finished[:len(finished)-tools.MaxFinishedRecordsPerSession] {
		delete(r.runs, runKey(run.Session, run.CallID))
	}
}

// reportTail bounds a stored report the way the background-task completion
// body is bounded (a chatty child must not flood the snapshot).
func reportTail(report string) string {
	const cap = 2000
	if len(report) <= cap {
		return report
	}
	return "…\n" + report[len(report)-cap:]
}

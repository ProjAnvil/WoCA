package agent

import (
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
)

// Goal mode, a graph-based persistent-objective loop: the user declares an
// objective and the harness runs an autonomous assess → execute → evaluate
// cycle over the real agent graph until the goal is verified complete,
// blocked, paused, or out of budget.
// The loop itself is a compiled StateGraph (goalgraph.go); this file holds
// the state machine, its manager, and the /goal command parser.
//
// Like the cron scheduler and the task list, goal state is process-scoped on
// the shared Agent (one goal per agent). The web server persists it per
// session as a sidecar and tags the owning session.

// GoalStatus is the lifecycle state of a goal (Codex's goal state machine:
// Active ↔ Paused, → Blocked, → Complete / BudgetLimited).
type GoalStatus string

const (
	GoalActive        GoalStatus = "active"
	GoalPaused        GoalStatus = "paused"
	GoalBlocked       GoalStatus = "blocked"
	GoalComplete      GoalStatus = "complete"
	GoalBudgetLimited GoalStatus = "budget_limited"
)

// Default goal budgets (overridable per goal).
const (
	DefaultGoalMaxCycles = 10
	DefaultGoalMaxTokens = int64(2_000_000)
)

// GoalProgressEntry records the outcome of one assess→execute→evaluate cycle.
type GoalProgressEntry struct {
	Cycle   int    `json:"cycle"`
	Summary string `json:"summary"`
	Verdict string `json:"verdict"`
	Note    string `json:"note,omitempty"`
}

// Goal is the persistent objective the harness pursues autonomously. The
// model can only narrow it (progress notes, blocked, claim completion via the
// UpdateGoal tool); pausing, resuming, clearing, and budget changes are
// user-only (/goal, the web goal API).
type Goal struct {
	Objective  string              `json:"objective"`
	Status     GoalStatus          `json:"status"`
	SessionID  string              `json:"session_id,omitempty"`
	Cycles     int                 `json:"cycles"`
	MaxCycles  int                 `json:"max_cycles"`
	TokensUsed int64               `json:"tokens_used"`
	MaxTokens  int64               `json:"max_tokens"`
	Progress   []GoalProgressEntry `json:"progress,omitempty"`
	Notes      []string            `json:"notes,omitempty"`
	// Claimed records the executor's completion claim awaiting skeptical
	// verification by the evaluate node.
	Claimed       bool      `json:"claimed,omitempty"`
	ClaimEvidence string    `json:"claim_evidence,omitempty"`
	BlockedReason string    `json:"blocked_reason,omitempty"`
	Evidence      string    `json:"evidence,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`

	// usageBase is the session-usage total at goal start; TokensUsed is the
	// delta from it. stalls counts consecutive no-tool cycles.
	usageBase int64
	stalls    int
}

func (g *Goal) clone() *Goal {
	if g == nil {
		return nil
	}
	out := *g
	out.Progress = append([]GoalProgressEntry(nil), g.Progress...)
	out.Notes = append([]string(nil), g.Notes...)
	return &out
}

// goalManager is the agent's goal state. running/graph/pending belong to the
// in-flight goal loop; pending stashes the inner agent-graph interrupt while
// its fresh copy is surfaced to the harness (the inner thread's checkpoint
// must stay unmutated so the execute node can resume the inner graph on the
// answer).
type goalManager struct {
	mu            sync.Mutex
	goal          *Goal
	running       bool
	graph         *lggraph.CompiledGraph
	// thread is the outer goal graph's checkpoint thread; innerThread is
	// the inner agent-graph thread of the paused execute cycle.
	thread        string
	innerThread   string
	pending       *lgtypes.Interrupt
	pendingTurns  int
	pendingPrefix int
	subs          []func(*Goal)
	// runID identifies the goal run in flight; stale AbandonGoalRun calls
	// (a newer run already started) are ignored by comparing against it.
	runID uint64
}

// SetGoal installs a new objective, replacing any previous goal. The loop is
// started by the harness (TUI kicks it after /goal set; the web server starts
// a goal run). sessionID records the owning web session ("" for the TUI).
func (a *Agent) SetGoal(objective, sessionID string, maxCycles int, maxTokens int64) (*Goal, error) {
	objective = strings.TrimSpace(objective)
	if objective == "" {
		return nil, fmt.Errorf("agent: goal objective is required")
	}
	a.goal.mu.Lock()
	if a.goal.running {
		a.goal.mu.Unlock()
		return nil, fmt.Errorf("agent: goal loop is running (pause or clear it first)")
	}
	if maxCycles <= 0 {
		maxCycles = DefaultGoalMaxCycles
	}
	if maxTokens <= 0 {
		maxTokens = DefaultGoalMaxTokens
	}
	now := time.Now()
	a.goal.goal = &Goal{
		Objective: objective,
		Status:    GoalActive,
		SessionID: sessionID,
		MaxCycles: maxCycles,
		MaxTokens: maxTokens,
		CreatedAt: now,
		UpdatedAt: now,
		usageBase: int64(a.usage.snapshot().TotalTokens),
	}
	a.goal.pending = nil
	g := a.goal.goal.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(g)
	return g, nil
}

// RestoreGoal loads a persisted goal (web server restart) without starting
// the loop. A running loop refuses the restore.
func (a *Agent) RestoreGoal(g *Goal) error {
	if g == nil || strings.TrimSpace(g.Objective) == "" {
		return fmt.Errorf("agent: goal to restore is empty")
	}
	a.goal.mu.Lock()
	if a.goal.running {
		a.goal.mu.Unlock()
		return fmt.Errorf("agent: goal loop is running")
	}
	if a.goal.goal != nil {
		a.goal.mu.Unlock()
		return fmt.Errorf("agent: a goal is already set")
	}
	if g.Status == GoalActive {
		// A fresh process cannot resume mid-loop; require an explicit resume.
		g.Status = GoalPaused
	}
	if g.CreatedAt.IsZero() {
		g.CreatedAt = time.Now()
	}
	g.UpdatedAt = time.Now()
	g.usageBase = int64(a.usage.snapshot().TotalTokens) - g.TokensUsed
	g.stalls = 0
	a.goal.goal = g.clone()
	snapshot := a.goal.goal.clone()
	a.goal.mu.Unlock()
	// Notify outside the mutex: subscribers commonly re-enter the goal API
	// (notifyGoal takes goal.mu itself), so notifying under it deadlocks.
	a.notifyGoal(snapshot)
	return nil
}

// Goal returns a snapshot of the current goal (nil when none is set).
func (a *Agent) Goal() *Goal {
	a.goal.mu.Lock()
	defer a.goal.mu.Unlock()
	return a.goal.goal.clone()
}

// GoalRunning reports whether the goal loop is currently executing.
func (a *Agent) GoalRunning() bool {
	a.goal.mu.Lock()
	defer a.goal.mu.Unlock()
	return a.goal.running
}

// PauseGoal requests the loop stop at the next cycle boundary. It takes
// effect between nodes; an in-flight execute cycle finishes first so no work
// is lost.
func (a *Agent) PauseGoal() bool {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil || g.Status != GoalActive {
		a.goal.mu.Unlock()
		return false
	}
	g.Status = GoalPaused
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return true
}

// ResumeGoal re-activates a paused (or exhausted) goal. The harness starts
// the loop again after a successful resume.
func (a *Agent) ResumeGoal() bool {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil || g.Status == GoalActive || g.Status == GoalComplete {
		a.goal.mu.Unlock()
		return false
	}
	g.Status = GoalActive
	g.UpdatedAt = time.Now()
	g.stalls = 0
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return true
}

// ClearGoal drops the objective entirely (user-only; the model cannot clear
// a goal). The in-flight loop ends at the next node boundary.
func (a *Agent) ClearGoal() bool {
	a.goal.mu.Lock()
	had := a.goal.goal != nil
	a.goal.goal = nil
	a.goal.mu.Unlock()
	if had {
		a.notifyGoal(nil)
	}
	return had
}

// GoalSetBudget adjusts the cycle/token budget of the current goal.
func (a *Agent) GoalSetBudget(cycles int, tokens int64) bool {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil {
		a.goal.mu.Unlock()
		return false
	}
	if cycles > 0 {
		g.MaxCycles = cycles
	}
	if tokens > 0 {
		g.MaxTokens = tokens
	}
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return true
}

// goalAddNote appends an executor progress note (UpdateGoal action=progress).
func (a *Agent) goalAddNote(note string) {
	note = strings.TrimSpace(note)
	if note == "" {
		return
	}
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil {
		a.goal.mu.Unlock()
		return
	}
	g.Notes = append(g.Notes, note)
	if len(g.Notes) > 20 {
		g.Notes = g.Notes[len(g.Notes)-20:]
	}
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
}

// goalClaimCompletion records the executor's completion claim (UpdateGoal
// action=claim_complete). The evaluate node verifies it skeptically; an
// unverified claim does not complete the goal.
func (a *Agent) goalClaimCompletion(evidence string) bool {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil {
		a.goal.mu.Unlock()
		return false
	}
	g.Claimed = true
	g.ClaimEvidence = strings.TrimSpace(evidence)
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return true
}

// goalMarkBlocked flags the goal blocked from inside the loop (UpdateGoal
// action=blocked); the router ends the run after the current evaluate.
func (a *Agent) goalMarkBlocked(reason string) bool {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil || g.Status != GoalActive {
		a.goal.mu.Unlock()
		return false
	}
	g.Status = GoalBlocked
	g.BlockedReason = strings.TrimSpace(reason)
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return true
}

// goalSetStatus is the evaluate node's authoritative transition (verified
// complete / blocked / budget-limited). It returns the snapshot for tests.
func (a *Agent) goalSetStatus(status GoalStatus, reason, evidence string) *Goal {
	a.goal.mu.Lock()
	g := a.goal.goal
	if g == nil {
		a.goal.mu.Unlock()
		return nil
	}
	g.Status = status
	switch status {
	case GoalComplete:
		g.Evidence = evidence
	case GoalBlocked:
		g.BlockedReason = reason
	case GoalBudgetLimited:
		g.BlockedReason = reason
	}
	g.UpdatedAt = time.Now()
	out := g.clone()
	a.goal.mu.Unlock()
	a.notifyGoal(out)
	return out
}

// SubscribeGoal registers a change listener receiving a snapshot on every
// transition (nil = cleared). Returns an unsubscribe function.
func (a *Agent) SubscribeGoal(fn func(*Goal)) func() {
	a.goal.mu.Lock()
	a.goal.subs = append(a.goal.subs, fn)
	a.goal.mu.Unlock()
	return func() {
		a.goal.mu.Lock()
		target := reflect.ValueOf(fn).Pointer()
		subs := make([]func(*Goal), 0, len(a.goal.subs))
		for _, s := range a.goal.subs {
			if reflect.ValueOf(s).Pointer() != target {
				subs = append(subs, s)
			}
		}
		a.goal.subs = subs
		a.goal.mu.Unlock()
	}
}

func (a *Agent) notifyGoal(g *Goal) {
	a.goal.mu.Lock()
	subs := make([]func(*Goal), len(a.goal.subs))
	copy(subs, a.goal.subs)
	a.goal.mu.Unlock()
	for _, fn := range subs {
		fn(g)
	}
}

// GoalCommand is a parsed /goal command (shared by the console and the web
// goal API so both frontends accept identical syntax).
type GoalCommand struct {
	Verb      string // set | status | pause | resume | clear | budget | help
	Objective string
	BudgetKey string // cycles | tokens (budget verb)
	BudgetVal int64
	MaxCycles int   // --cycles flag (set verb; 0 = default)
	MaxTokens int64 // --tokens flag (set verb; 0 = default)
}

// ParseGoalCommand parses /goal arguments:
//
//	goal set [--cycles N] [--tokens N] <objective...>
//	goal <objective...>                                (shorthand for set)
//	goal status | pause | resume | clear
//	goal budget cycles N | goal budget tokens N
func ParseGoalCommand(args []string) (GoalCommand, error) {
	if len(args) == 0 {
		return GoalCommand{Verb: "status"}, nil
	}
	switch args[0] {
	case "status":
		return GoalCommand{Verb: "status"}, nil
	case "pause":
		return GoalCommand{Verb: "pause"}, nil
	case "resume", "continue":
		return GoalCommand{Verb: "resume"}, nil
	case "clear", "stop":
		return GoalCommand{Verb: "clear"}, nil
	case "help":
		return GoalCommand{Verb: "help"}, nil
	case "budget":
		if len(args) < 3 {
			return GoalCommand{}, fmt.Errorf("usage: /goal budget <cycles|tokens> <N>")
		}
		val, err := strconv.ParseInt(args[2], 10, 64)
		if err != nil || val <= 0 {
			return GoalCommand{}, fmt.Errorf("budget value must be a positive number")
		}
		switch args[1] {
		case "cycles":
			return GoalCommand{Verb: "budget", BudgetKey: "cycles", BudgetVal: val}, nil
		case "tokens":
			return GoalCommand{Verb: "budget", BudgetKey: "tokens", BudgetVal: val}, nil
		}
		return GoalCommand{}, fmt.Errorf("unknown budget %q (use cycles or tokens)", args[1])
	case "set", "start":
		return parseGoalSetArgs(args[1:])
	default:
		// Free text (no recognized verb) is shorthand for "set" —
		// "/goal <objective>" does what users intuitively expect.
		return parseGoalSetArgs(args)
	}
}

// parseGoalSetArgs parses the objective words and --cycles/--tokens flags of
// a "set" command.
func parseGoalSetArgs(rest []string) (GoalCommand, error) {
	cmd := GoalCommand{Verb: "set"}
	var words []string
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "--cycles":
			if i+1 >= len(rest) {
				return GoalCommand{}, fmt.Errorf("--cycles requires a value")
			}
			n, err := strconv.Atoi(rest[i+1])
			if err != nil || n <= 0 {
				return GoalCommand{}, fmt.Errorf("--cycles must be a positive number")
			}
			cmd.MaxCycles = n
			i++
		case "--tokens":
			if i+1 >= len(rest) {
				return GoalCommand{}, fmt.Errorf("--tokens requires a value")
			}
			n, err := strconv.ParseInt(rest[i+1], 10, 64)
			if err != nil || n <= 0 {
				return GoalCommand{}, fmt.Errorf("--tokens must be a positive number")
			}
			cmd.MaxTokens = n
			i++
		default:
			words = append(words, rest[i])
		}
	}
	cmd.Objective = strings.Join(words, " ")
	if strings.TrimSpace(cmd.Objective) == "" {
		return GoalCommand{}, fmt.Errorf("usage: /goal set [--cycles N] [--tokens N] <objective>")
	}
	return cmd, nil
}

// GoalHelpText is the /goal usage reference.
const GoalHelpText = `goal mode: an autonomous assess → execute → evaluate loop over the agent
graph that keeps working until the goal is verified complete, blocked, or out
of budget. Completion claims are verified by a skeptical evaluator.

usage:
  /goal set [--cycles N] [--tokens N] <objective>   set a goal and start the loop
  /goal <objective>                                  shorthand for set
  /goal                                             show goal status
  /goal pause                                       stop at the next cycle boundary
  /goal resume                                      continue a paused/exhausted goal
  /goal budget cycles N | tokens N                  adjust the budget
  /goal clear                                       drop the goal entirely`

// FormatGoal renders the goal status summary shared by /goal and the web
// goal API.
func FormatGoal(g *Goal) string {
	if g == nil {
		return "no goal set (use /goal set <objective>)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "goal [%s]: %s\n", g.Status, g.Objective)
	fmt.Fprintf(&b, "  cycle %d/%d · %s/%s tokens", g.Cycles, g.MaxCycles, FormatGoalTokens(g.TokensUsed), FormatGoalTokens(g.MaxTokens))
	if g.Claimed {
		b.WriteString(" · completion claim awaiting verification")
	}
	b.WriteString("\n")
	switch g.Status {
	case GoalBlocked:
		fmt.Fprintf(&b, "  blocked: %s\n", g.BlockedReason)
	case GoalComplete:
		fmt.Fprintf(&b, "  evidence: %s\n", g.Evidence)
	case GoalBudgetLimited:
		fmt.Fprintf(&b, "  %s (adjust with /goal budget, then /goal resume)\n", g.BlockedReason)
	case GoalPaused:
		b.WriteString("  paused — /goal resume to continue\n")
	}
	if len(g.Notes) > 0 {
		b.WriteString("  latest executor note: " + g.Notes[len(g.Notes)-1] + "\n")
	}
	if len(g.Progress) > 0 {
		b.WriteString("  cycles:\n")
		start := len(g.Progress) - 5
		if start < 0 {
			start = 0
		}
		for _, p := range g.Progress[start:] {
			summary := p.Summary
			if summary == "" {
				summary = p.Note
			}
			fmt.Fprintf(&b, "    %2d [%s] %s\n", p.Cycle, p.Verdict, summary)
		}
	}
	return b.String()
}

package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/bus"
)

// Agent teams: a teammate is a persistent named agent session with its own
// context — not a one-shot Task subagent. Teammates share one task list (the
// regular TaskCreate/Get/Update/List tools become team-scoped once a team
// exists) with claim-by-owner and dependency gating, and coordinate through
// SendMessage (push delivery: a message pokes an idle teammate into its next
// turn). Isolation is by task ownership in the shared worktree
// (worktree-per-member is deliberately NOT the model).
//
// v1 scope: teammates live in-process next to the lead agent, auto-idle after
// teamIdleTimeout, and stop via TeamStop or context cancellation. Team state
// is in-memory for the session (not persisted across restarts).

const (
	// teamIdleTimeout auto-stops a teammate that stays idle this long
	// (idle teammates terminate).
	teamIdleTimeout = 10 * time.Minute
	// LeadName is the reserved name of the interactive session in a team.
	LeadName = "lead"
)

// maxActiveTeammates caps concurrently alive teammates; the model gets a
// steerable error (not a hang) so it can restructure its plan.
const maxActiveTeammates = 8

type teammateStatus string

const (
	teammateStarting teammateStatus = "starting"
	teammateWorking  teammateStatus = "working"
	teammateIdle     teammateStatus = "idle"
	teammateStopped  teammateStatus = "stopped"
)

// teammate is one persistent team member: its own agent instance (own
// context/history), sharing the lead's workdir and runtime.
type teammate struct {
	name   string
	agent  *Agent
	cancel context.CancelFunc
	status teammateStatus
	note   string
	turns  int
}

// subagentFactory is the narrow seam through which a team builds teammates:
// the rest of the lead agent stays invisible to the manager. *Agent
// satisfies it structurally via newSubagent.
type subagentFactory interface {
	newSubagent(ctx context.Context, def *agents.Definition) (*Agent, error)
}

// TeamManager owns the shared task list, the message bus and the teammates
// spawned by the lead agent.
type TeamManager struct {
	factory subagentFactory
	bus     *bus.Bus
	list    *TaskList

	mu      sync.Mutex
	members map[string]*teammate
	order   []string
	pokes   map[string]chan struct{}
}

// ensureTeam lazily creates the lead's team. From this point the lead's
// TaskCreate/Get/Update/List tools operate on the shared team list (the same
// tools, scoped to the team).
func (a *Agent) ensureTeam() *TeamManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.team != nil {
		return a.team
	}
	t := &TeamManager{
		factory: a,
		bus:     bus.New(),
		list:    newTaskList(),
		members: map[string]*teammate{},
		pokes:   map[string]chan struct{}{},
	}
	a.team = t
	a.taskList = t.list
	return t
}

// currentTeam snapshots the team pointer under a.mu: ensureTeam publishes it
// from a run's tool goroutine while UI and shutdown goroutines read it.
func (a *Agent) currentTeam() *TeamManager {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.team
}

// currentTaskList snapshots the active task list under a.mu (the agent's own
// list until a team re-scopes the task tools to the shared one).
func (a *Agent) currentTaskList() *TaskList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.taskList
}

// poke signals one recipient (member or lead) that something changed.
func (t *TeamManager) poke(name string) {
	t.mu.Lock()
	ch := t.pokes[name]
	if ch == nil {
		ch = make(chan struct{}, 1)
		t.pokes[name] = ch
	}
	t.mu.Unlock()
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (t *TeamManager) waitPoke(name string) <-chan struct{} {
	t.mu.Lock()
	defer t.mu.Unlock()
	ch := t.pokes[name]
	if ch == nil {
		ch = make(chan struct{}, 1)
		t.pokes[name] = ch
	}
	return ch
}

// Send delivers a message and wakes the recipient.
func (t *TeamManager) Send(from, to, content string) {
	t.bus.Send(from, to, content)
	t.poke(to)
}

// claimNext picks the first claimable task (pending, unowned, deps done) for
// owner. Losing the atomic Claim race on a candidate (another member claimed
// it first) keeps the scan going instead of reporting no work.
func (t *TeamManager) claimNext(owner string) (TaskRecord, bool) {
	tasks := t.list.List() // one snapshot to prefilter candidates
	done := make(map[string]bool, len(tasks))
	for _, rec := range tasks {
		done[rec.ID] = rec.Status == TaskRecordCompleted
	}
	for _, rec := range tasks {
		if rec.Status != TaskRecordPending || rec.Owner != "" {
			continue
		}
		blocked := false
		for _, dep := range rec.DependsOn {
			if !done[dep] {
				blocked = true
				break
			}
		}
		if blocked {
			continue
		}
		// Claim re-checks ownership and dependency state under its own lock,
		// so races between this snapshot and the claim are handled there.
		if claimed, ok := t.list.Claim(rec.ID, owner); ok {
			return claimed, true
		}
	}
	return TaskRecord{}, false
}

// Spawn creates a teammate: a persistent agent session with its own context,
// bound to the shared task list and the team message bus.
func (t *TeamManager) Spawn(ctx context.Context, name, prompt string, def *agents.Definition) error {
	if name == "" || name == LeadName {
		return fmt.Errorf("teammate name must be non-empty and not %q", LeadName)
	}
	t.mu.Lock()
	if _, exists := t.members[name]; exists {
		t.mu.Unlock()
		return fmt.Errorf("teammate %q already exists", name)
	}
	active := 0
	for _, m := range t.members {
		if m.status != teammateStopped {
			active++
		}
	}
	if active >= maxActiveTeammates {
		t.mu.Unlock()
		return fmt.Errorf("已达 teammate 上限（%d 个活跃）：请改用顺序 Task，或先 Stop 不需要的 teammate", maxActiveTeammates)
	}
	t.mu.Unlock()

	sub, err := t.factory.newSubagent(ctx, def)
	if err != nil {
		return err
	}
	sub.joinTeam(t, name)
	m := &teammate{name: name, agent: sub, status: teammateStarting}

	mctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	m.cancel = cancel
	t.mu.Lock()
	if _, exists := t.members[name]; exists { // lost a spawn race
		t.mu.Unlock()
		cancel()
		return fmt.Errorf("teammate %q already exists", name)
	}
	t.members[name] = m
	t.order = append(t.order, name)
	t.mu.Unlock()
	go t.run(mctx, m, prompt)
	return nil
}

// ActiveCount reports teammates that have not stopped (for /api/stats).
func (t *TeamManager) ActiveCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := 0
	for _, m := range t.members {
		if m.status != teammateStopped {
			n++
		}
	}
	return n
}

// joinTeam binds a freshly built subagent to the team as member name. The
// field adoption lives in this one Agent method so the rest of the package
// does not poke the subagent's private wiring from the outside.
func (a *Agent) joinTeam(t *TeamManager, name string) {
	a.agentName = name
	a.team = t
	a.taskList = t.list
	a.bus = t.bus // un-gates the SendMessage tool; sends route via the team
	adoptTeamTools(a)
}

// adoptTeamTools rebuilds a teammate's tool binding with the shared-list task
// tools and SendMessage (subagents normally have those filtered out).
func adoptTeamTools(sub *Agent) {
	sub.toolList = append(sub.toolList, sub.taskListTools()...)
	sub.toolList = append(sub.toolList, sub.sendMessageTool()...)
	sub.toolByName = make(map[string]coretools.Tool, len(sub.toolList))
	for _, tool := range sub.toolList {
		sub.toolByName[tool.Name()] = tool
	}
	if len(sub.toolList) > 0 && sub.model.Capabilities().ToolCalling {
		if bound, err := sub.model.BindTools(sub.toolList); err == nil {
			sub.mu.Lock()
			sub.boundModel = bound
			sub.mu.Unlock()
		}
	}
}

// run is the teammate autonomy loop: process inbox messages, then self-claim
// shared tasks, otherwise idle until poked (or the idle timeout stops it).
func (t *TeamManager) run(ctx context.Context, m *teammate, spawnPrompt string) {
	history := []messages.Message{messages.Human(teamSpawnPrompt(m.name, spawnPrompt))}
	defer func() {
		// Every exit path (cancellation, idle timeout, turn error) returns
		// the member's in-progress tasks to the pool so they cannot stay
		// owned forever after the member is gone.
		t.releaseOwned(m.name)
		t.mu.Lock()
		m.status = teammateStopped
		t.mu.Unlock()
	}()
	for {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			m.note = "stopped"
			t.mu.Unlock()
			return
		default:
		}

		var turnPrompt string
		msgs := t.bus.Drain(m.name)
		if len(msgs) > 0 {
			var b strings.Builder
			for _, msg := range msgs {
				fmt.Fprintf(&b, "<team-message from=%q>\n%s\n</team-message>\n", msg.From, msg.Content)
			}
			turnPrompt = b.String()
		} else if rec, ok := t.claimNext(m.name); ok {
			turnPrompt = teamTaskPrompt(rec)
		} else {
			t.mu.Lock()
			m.status = teammateIdle
			t.mu.Unlock()
			select {
			case <-ctx.Done():
				t.mu.Lock()
				m.note = "stopped"
				t.mu.Unlock()
				return
			case <-t.waitPoke(m.name):
			case <-time.After(teamIdleTimeout):
				t.mu.Lock()
				m.note = "idle timeout"
				t.mu.Unlock()
				return
			}
			continue
		}

		t.mu.Lock()
		m.status = teammateWorking
		t.mu.Unlock()
		// A hard cap per turn so a parked permission prompt or a stuck
		// provider call cannot wedge the teammate forever.
		tctx, tcancel := context.WithTimeout(ctx, 10*time.Minute)
		out, err := m.agent.Invoke(tctx, append(history, messages.Human(turnPrompt)), NewThreadID())
		tcancel()
		if err != nil {
			t.mu.Lock()
			m.note = "turn error: " + err.Error()
			t.mu.Unlock()
			// Back off briefly before the next round; the deferred
			// releaseOwned already returned this round's claim.
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}
		history = out
		t.mu.Lock()
		m.turns++
		m.note = ""
		t.mu.Unlock()
	}
}

// releaseOwned returns the teammate's still-in_progress tasks to the pool.
// ReleaseForOwner re-checks ownership under the lock, so a task the lead
// completed in between is left untouched.
func (t *TeamManager) releaseOwned(owner string) {
	for _, rec := range t.list.List() {
		if rec.Owner == owner {
			t.list.ReleaseForOwner(rec.ID, owner)
		}
	}
}

// memberNames snapshots the registered member names under the lock.
func (t *TeamManager) memberNames() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.order...)
}

// Stop terminates a teammate by name.
func (t *TeamManager) Stop(name string, reason string) error {
	t.mu.Lock()
	m, ok := t.members[name]
	var cancel context.CancelFunc
	if ok {
		cancel = m.cancel
	}
	t.mu.Unlock()
	if !ok {
		return fmt.Errorf("no teammate %q", name)
	}
	if reason != "" {
		t.bus.Send(LeadName, name, "shutdown: "+reason)
		t.poke(name)
	}
	if cancel != nil {
		cancel()
	}
	return nil
}

// teamMemberSnapshot is an immutable copy of a teammate's display state,
// taken under the team lock so Status never reads fields the run loop is
// concurrently updating.
type teamMemberSnapshot struct {
	name   string
	status teammateStatus
	note   string
	turns  int
}

// Status renders members, the shared task list and the lead's unread inbox
// (draining it — reading is acknowledging, matching push delivery).
func (t *TeamManager) Status() string {
	var b strings.Builder
	b.WriteString("team members:\n")
	t.mu.Lock()
	names := append([]string(nil), t.order...)
	members := make([]teamMemberSnapshot, 0, len(names))
	for _, n := range names {
		m := t.members[n]
		members = append(members, teamMemberSnapshot{name: m.name, status: m.status, note: m.note, turns: m.turns})
	}
	t.mu.Unlock()
	if len(names) == 0 {
		b.WriteString("  (no teammates spawned yet)\n")
	}
	for _, m := range members {
		note := m.note
		if note != "" {
			note = " — " + note
		}
		fmt.Fprintf(&b, "  %-12s %-8s turns=%d%s\n", m.name, m.status, m.turns, note)
	}
	tasks := t.list.List()
	fmt.Fprintf(&b, "shared tasks (%d):\n", len(tasks))
	for _, rec := range tasks {
		owner := rec.Owner
		if owner == "" {
			owner = "-"
		}
		fmt.Fprintf(&b, "  [%s] %-10s owner=%-8s %s\n", rec.ID, rec.Status, owner, rec.Subject)
	}
	if inbox := t.bus.Drain(LeadName); len(inbox) > 0 {
		b.WriteString("inbox (lead):\n")
		for _, msg := range inbox {
			fmt.Fprintf(&b, "  from %s: %s\n", msg.From, truncate(msg.Content, 200))
		}
	}
	return b.String()
}

// Close stops every teammate (lead shutdown).
func (t *TeamManager) Close() {
	t.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(t.members))
	for _, m := range t.members {
		if m.cancel != nil {
			cancels = append(cancels, m.cancel)
		}
	}
	t.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func teamSpawnPrompt(name, prompt string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are %q, a teammate in a woca agent team. The lead is the interactive session (recipient %q).\n", name, LeadName)
	b.WriteString(`Team protocol:
- Check the shared task list (TaskList). Claim work by TaskUpdate with status "in_progress" — ownership is enforced; tasks whose dependencies are incomplete cannot be claimed. Mark tasks completed when done. If nothing is claimable, say so and stop; you will be woken when there is work or a message.
- Coordinate through SendMessage (recipient = teammate name or "lead"). Messages from teammates arrive between your turns as <team-message> blocks.
- You share the working directory with the other teammates: stick to your own tasks/files and never edit files that belong to someone else's task.
- Keep reports concise; results travel via task updates and messages, not idle waiting.

Spawn brief from the lead:
`)
	b.WriteString(prompt)
	return b.String()
}

func teamTaskPrompt(rec TaskRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You claimed shared task %s: %s\n", rec.ID, rec.Subject)
	if rec.Description != "" {
		fmt.Fprintf(&b, "Details: %s\n", rec.Description)
	}
	b.WriteString("Do the work now, then TaskUpdate this task to completed (or back to pending with a note if blocked). Report a short summary via SendMessage to \"lead\" when finished.\n")
	return b.String()
}

// teamTools builds the lead's team-management tools.
func (a *Agent) teamTools() []coretools.Tool {
	spawn, err := coretools.FromFunc("TeamSpawn",
		`Spawn a named teammate: a persistent agent session with its own context that joins your team. Teammates share the task list (TaskCreate/TaskUpdate become team-scoped with claim ownership) and coordinate via SendMessage; they self-claim pending tasks with completed dependencies and idle out when there is no work. Cost scales with teammate count — spawn 2-5 for genuinely parallel work, not for sequential tasks (use the Task tool for those).`,
		func(ctx context.Context, args struct {
			Name         string `json:"name"`
			Prompt       string `json:"prompt"`
			SubagentType string `json:"subagent_type,omitempty"`
		}) (coretools.Result, error) {
			if args.Name == "" || args.Prompt == "" {
				return coretools.Result{}, fmt.Errorf("TeamSpawn requires name and prompt")
			}
			var def *agents.Definition
			if args.SubagentType != "" {
				d, ok := agents.Find(a.agentDefs, args.SubagentType)
				if !ok {
					return coretools.Result{}, fmt.Errorf("unknown subagent_type %q", args.SubagentType)
				}
				def = &d
			}
			team := a.ensureTeam()
			if err := team.Spawn(ctx, args.Name, args.Prompt, def); err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: fmt.Sprintf(
				"Spawned teammate %s. Your task tools are now team-scoped; coordinate via SendMessage and check TeamStatus. Tell %s what to do (it also self-claims from the shared list).", args.Name, args.Name)}, nil
		})
	if err != nil {
		panic(err)
	}
	status, err := coretools.FromFunc("TeamStatus",
		`Show the agent team: teammates (status, turns), the shared task list with owners, and unread messages addressed to you (reading acknowledges them).`,
		func(ctx context.Context, _ struct{}) (coretools.Result, error) {
			team := a.currentTeam()
			if team == nil {
				return coretools.Result{Content: "No team spawned yet (use TeamSpawn)."}, nil
			}
			return coretools.Result{Content: team.Status()}, nil
		})
	if err != nil {
		panic(err)
	}
	stop, err := coretools.FromFunc("TeamStop",
		`Stop a teammate by name (sends a shutdown message, then terminates). Use TeamStatus to see members.`,
		func(ctx context.Context, args struct {
			Name   string `json:"name"`
			Reason string `json:"reason,omitempty"`
		}) (coretools.Result, error) {
			team := a.currentTeam()
			if team == nil {
				return coretools.Result{Content: "No team spawned yet."}, nil
			}
			if err := team.Stop(args.Name, args.Reason); err != nil {
				return coretools.Result{}, err
			}
			return coretools.Result{Content: fmt.Sprintf("Stopping teammate %s.", args.Name)}, nil
		})
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{spawn, status, stop}
}

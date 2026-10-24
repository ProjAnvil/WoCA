package agent

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
)

// The retain layer: completed runs/goals/tasks stored into long-term memory.
// Every auto write is an L1 episode; the L2 fact layer has a single reviewed
// door — System 2 consolidation — so nothing here calls Remember. The
// mechanics live on memoryRuntime (this file); the Agent delegates at the
// bottom apply the host policy — subagents never write memory — and resolve
// the run workspace before handing over.

// autoRetain reports whether automatic retention is on. With no settings
// loader wired the default is on (tests rely on it); an explicit setting
// wins either way.
func (m *memoryRuntime) autoRetain() bool {
	m.mu.Lock()
	loader := m.loadSettings
	m.mu.Unlock()
	if loader == nil {
		return true
	}
	return loader().LadyMAutoRetain
}

// RetainRun stores a completed run into long-term memory as one L1 episode.
// On success the episode's observation carries the full Q/A material; the L2
// fact layer has a single reviewed door — System 2 consolidation (the
// ADD/UPDATE/DELETE classifier) — so no run fact is written here. Errored or
// cancelled runs record the request and outcome only: a failed answer must
// never distill into a durable fact. Writes are best-effort: errors are
// dropped (the memory subsystem must never fail a run), logged to obs when
// wired. changes is the run's file mutation delta (from the caller's
// FileHistory watermark); when non-empty and the engine carries the
// recall_ops capability, one structured operation record joins the episode.
func (m *memoryRuntime) RetainRun(ctx context.Context, ws string, history, final []messages.Message, runErr error, changes []tools.FileChange) {
	eng := m.Engine()
	if eng == nil {
		return
	}
	if !m.autoRetain() {
		return
	}
	question := runQuestion(history)
	if question == "" {
		return
	}
	outcome := "success"
	if runErr != nil {
		outcome = "error"
		if errors.Is(runErr, context.Canceled) {
			outcome = "cancelled"
		}
	}
	obs := clampUTF8(question, 500)
	if outcome == "success" {
		if answer := lastAIText(final); answer != "" {
			obs = clampUTF8("Q: "+question+"\nA: "+answer, 4000)
		}
	}
	if err := eng.RecordEvent(ctx, ws, "woca", "run", obs, outcome); err != nil {
		m.warn("retain_write_error", map[string]any{"op": "record_event", "error": err.Error()})
	}
	m.recordFileOperation(ctx, eng, ws, question, outcome, changes)
	// System 2 (opportunistic, per-workspace throttle): promote the fresh
	// episodes into facts/playbooks/insights in the background. since=0 puts
	// the cycle in mark mode — the engine's consolidated_at stamp decides
	// what is pending, which (unlike a time floor) never strands episodes
	// a previous aborted cycle failed to reach.
	if s := m.settings(); s.LadyMSystem2 {
		interval := int64(settings.DefaultLadyMSystem2Interval)
		if s.LadyMSystem2Interval >= settings.MinLadyMSystem2Interval {
			interval = int64(s.LadyMSystem2Interval)
		}
		if m.system2Due(ws, interval) {
			engC := eng
			go func() {
				if _, err := engC.Consolidate(context.WithoutCancel(ctx), ws, 0); err != nil {
					m.warn("system2_error", map[string]any{"workspace": ws, "error": err.Error()})
				}
			}()
		}
	}
}

// recordFileOperation stores the run's file mutations as one structured
// operation record (LadyM v0.7.0 resource slots) — the recall_ops capability
// answers "how did I last operate on X?". Engines without the capability
// (the remote data-plane) skip silently; failures only warn, like every
// retain write. Resources are deduplicated paths capped at 20; the action
// maps the delta's shape onto the canonical vocabulary (create/delete/update).
// The paths enter the same trust boundary as the rest of memory: with an
// inherited classifier (LLM=inherit) System 2 consolidation sends episode
// material — including these paths — to the user's own configured model.
func (m *memoryRuntime) recordFileOperation(ctx context.Context, eng memory.Engine, ws, question, outcome string, changes []tools.FileChange) {
	ops, ok := eng.(memory.OperationEngine)
	if !ok || len(changes) == 0 {
		return
	}
	resources := make([]string, 0, len(changes))
	seen := make(map[string]bool, len(changes))
	created, deleted := true, true
	for _, c := range changes {
		if c.Path == "" || seen[c.Path] {
			continue
		}
		seen[c.Path] = true
		resources = append(resources, c.Path)
		if c.Before != nil {
			created = false
		}
		if c.After != nil {
			deleted = false
		}
		if len(resources) == 20 {
			break
		}
	}
	if len(resources) == 0 {
		return
	}
	action := "update"
	switch {
	case deleted:
		action = "delete"
	case created:
		action = "create"
	}
	op := memory.Operation{
		Resources: resources,
		Action:    action,
		Summary:   clampUTF8(question, 200),
		Outcome:   outcome,
	}
	if _, err := ops.RecordOperation(context.WithoutCancel(ctx), ws, op); err != nil {
		m.warn("retain_write_error", map[string]any{"op": "record_operation", "error": err.Error()})
	}
}

// RetainGoal stores a completed goal-mode session into memory as one L1
// episode, mirroring RetainRun's policy: the objective/result narrative is
// consolidation material, not a direct fact, and only a successful goal
// carries the result (a blocked or errored goal's narrative is episode
// history). Goal runs bypass Runner.Run (they execute the goal graph
// directly), so RunGoal's defer is their only retain hook.
func (m *memoryRuntime) RetainGoal(ctx context.Context, ws, objective, status, summary string, runErr error, changes []tools.FileChange) {
	eng := m.Engine()
	if eng == nil {
		return
	}
	if !m.autoRetain() || strings.TrimSpace(objective) == "" {
		return
	}
	outcome := "success"
	switch {
	case runErr != nil:
		outcome = "error"
	case status == string(GoalBlocked), status == string(GoalBudgetLimited), status == string(GoalPaused):
		outcome = status
	}
	obs := clampUTF8(objective, 500)
	if outcome == "success" {
		if ans := strings.TrimSpace(summary); ans != "" {
			obs = clampUTF8("GOAL: "+objective+"\nRESULT("+status+"): "+ans, 4000)
		}
	}
	if err := eng.RecordEvent(ctx, ws, "woca", "goal:"+status, obs, outcome); err != nil {
		m.warn("retain_write_error", map[string]any{"op": "goal_event", "error": err.Error()})
	}
	m.recordFileOperation(ctx, eng, ws, objective, outcome, changes)
}

// RecordTaskEvent logs a completed subagent task as an L1 episode (reads
// shared, writes governed: subagents report events, not facts). Best-effort.
// The workspace resolves from the run ctx (values survive cancellation); the
// write itself runs detached so a TaskStop-cancelled ctx can still record.
func (m *memoryRuntime) RecordTaskEvent(ctx context.Context, ws, name, description, report string, taskErr error) {
	eng := m.Engine()
	if eng == nil {
		return
	}
	outcome := "success"
	if taskErr != nil {
		outcome = "error"
	}
	action := clampUTF8("task:"+description, 200)
	obs := clampUTF8(report, 500)
	if err := eng.RecordEvent(context.WithoutCancel(ctx), ws, "subagent:"+name, action, obs, outcome); err != nil {
		m.warn("retain_write_error", map[string]any{"op": "task_event", "error": err.Error()})
	}
}

// system2Due applies the per-workspace minimum interval between System 2
// cycles and records the trigger time. What counts as pending is the
// engine's consolidated_at mark, not this timestamp.
func (m *memoryRuntime) system2Due(ws string, intervalS int64) bool {
	now := time.Now().Unix()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lastSystem2 == nil {
		m.lastSystem2 = map[string]int64{}
	}
	if last, ok := m.lastSystem2[ws]; ok && now-last < intervalS {
		return false
	}
	m.lastSystem2[ws] = now
	return true
}

// runQuestion returns the run's actual request: the last human message at
// runner entry, skipping known synthetic wrappers injected by the harness.
func runQuestion(msgs []messages.Message) string {
	for _, msg := range slices.Backward(msgs) {
		if msg.Role != messages.RoleHuman {
			continue
		}
		c := strings.TrimSpace(msg.Content)
		if c == "" || strings.HasPrefix(c, "<relevant-memory>") ||
			strings.HasPrefix(c, "<queued-user-input") ||
			strings.HasPrefix(c, "<stop-hook>") ||
			strings.HasPrefix(c, "<task-fork>") {
			continue
		}
		return c
	}
	return ""
}

// Agent delegates: host policy (the subagent write gate, workspace
// resolution) lives here; the mechanics above stay agent-agnostic.

// RetainRun stores a completed run (subagents never pass through the
// runner, so the gate is belt-and-braces). changes is the run's file
// mutation delta from the caller's FileHistory watermark.
func (a *Agent) RetainRun(ctx context.Context, history, final []messages.Message, runErr error, changes []tools.FileChange) {
	if a.subagent {
		return
	}
	a.mem.RetainRun(ctx, a.memoryWorkspaceCtx(ctx), history, final, runErr, changes)
}

// RetainGoal stores a completed goal-mode session.
func (a *Agent) RetainGoal(ctx context.Context, objective, status, summary string, runErr error, changes []tools.FileChange) {
	if a.subagent {
		return
	}
	a.mem.RetainGoal(ctx, a.memoryWorkspaceCtx(ctx), objective, status, summary, runErr, changes)
}

// recordTaskEvent reports a completed subagent task from the parent side
// (the subagent gate applies to subagent-built agents; the parent records).
func (a *Agent) recordTaskEvent(ctx context.Context, name, description, report string, taskErr error) {
	if a.subagent {
		return
	}
	a.mem.RecordTaskEvent(ctx, a.memoryWorkspaceCtx(ctx), name, description, report, taskErr)
}

package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/settings"
)

// The retain layer: completed runs/goals/tasks stored into long-term memory.
// The mechanics live on memoryRuntime (this file); the Agent delegates at
// the bottom apply the host policy — subagents never write memory — and
// resolve the run workspace before handing over.

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

// RetainRun stores a completed run into long-term memory: the Q/A pair as an
// L2 fact (ladyM's attention gate dedups noise) and the run outcome as an L1
// episode (food for System 2). It is the single write choke point, called by
// the runner when a run finishes. Writes are best-effort: errors are dropped
// (the memory subsystem must never fail a run), logged to obs when wired.
func (m *memoryRuntime) RetainRun(ctx context.Context, ws string, history, final []messages.Message, runErr error) {
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
	if err := eng.RecordEvent(ctx, ws, "woca", "run", obs, outcome); err != nil {
		m.warn("retain_write_error", map[string]any{"op": "record_event", "error": err.Error()})
	}
	// The fact is optional (no answer, e.g. an errored run), but episodes
	// alone are consolidation food, so the System 2 trigger below still runs.
	if answer := lastAIText(final); answer != "" {
		q := clampUTF8(question, 2000)
		ans := clampUTF8(answer, 4000)
		if _, err := eng.Remember(ctx, ws, fmt.Sprintf("Q: %s\nA: %s", q, ans), []string{"woca", "auto"}, "woca:auto"); err != nil {
			m.warn("retain_write_error", map[string]any{"op": "remember", "error": err.Error()})
		}
	}
	// System 2 (opportunistic, per-workspace throttle): consolidate the
	// fresh episodes into facts/playbooks/insights in the background. The
	// cycle itself gates on ladyM's episode threshold, so a no-op trigger
	// is cheap (spec §4.5).
	if s := m.settings(); s.LadyMSystem2 {
		interval := int64(settings.DefaultLadyMSystem2Interval)
		if s.LadyMSystem2Interval >= settings.MinLadyMSystem2Interval {
			interval = int64(s.LadyMSystem2Interval)
		}
		if m.system2Due(ws, interval) {
			engC := eng
			go func() {
				if _, err := engC.Consolidate(context.WithoutCancel(ctx), ws); err != nil {
					m.warn("system2_error", map[string]any{"workspace": ws, "error": err.Error()})
				}
			}()
		}
	}
}

// RetainGoal stores a completed goal-mode session into memory: the objective
// and its terminal narrative as an L2 fact, and an episode for System 2. Goal
// runs bypass Runner.Run (they execute the goal graph directly), so RunGoal's
// defer is their only retain hook.
func (m *memoryRuntime) RetainGoal(ctx context.Context, ws, objective, status, summary string, runErr error) {
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
	if err := eng.RecordEvent(ctx, ws, "woca", "goal:"+status, clampUTF8(objective, 500), outcome); err != nil {
		m.warn("retain_write_error", map[string]any{"op": "goal_event", "error": err.Error()})
	}
	// The fact is optional (no summary, e.g. a paused or errored run), but the
	// episode above still feeds System 2.
	if ans := strings.TrimSpace(summary); ans != "" {
		content := "GOAL: " + clampUTF8(objective, 2000) + "\nRESULT(" + status + "): " + clampUTF8(ans, 4000)
		if _, err := eng.Remember(ctx, ws, content, []string{"woca", "auto", "goal"}, "woca:auto"); err != nil {
			m.warn("retain_write_error", map[string]any{"op": "goal_remember", "error": err.Error()})
		}
	}
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
// cycles and records the trigger time.
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
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != messages.RoleHuman {
			continue
		}
		c := strings.TrimSpace(msgs[i].Content)
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
// runner, so the gate is belt-and-braces).
func (a *Agent) RetainRun(ctx context.Context, history, final []messages.Message, runErr error) {
	if a.subagent {
		return
	}
	a.mem.RetainRun(ctx, a.memoryWorkspaceCtx(ctx), history, final, runErr)
}

// RetainGoal stores a completed goal-mode session.
func (a *Agent) RetainGoal(ctx context.Context, objective, status, summary string, runErr error) {
	if a.subagent {
		return
	}
	a.mem.RetainGoal(ctx, a.memoryWorkspaceCtx(ctx), objective, status, summary, runErr)
}

// recordTaskEvent reports a completed subagent task from the parent side
// (the subagent gate applies to subagent-built agents; the parent records).
func (a *Agent) recordTaskEvent(ctx context.Context, name, description, report string, taskErr error) {
	if a.subagent {
		return
	}
	a.mem.RecordTaskEvent(ctx, a.memoryWorkspaceCtx(ctx), name, description, report, taskErr)
}

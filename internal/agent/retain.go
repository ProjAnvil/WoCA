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

// RetainRun stores a completed run into long-term memory: the Q/A pair as an
// L2 fact (ladyM's attention gate dedups noise) and the run outcome as an L1
// episode (food for System 2). It is the single write choke point, called by
// the runner when a run finishes; subagents never pass through the runner.
// Writes are best-effort: errors are dropped (the memory subsystem must never
// fail a run), logged to obs when wired.
func (a *Agent) RetainRun(ctx context.Context, history, final []messages.Message, runErr error) {
	eng := a.MemoryEngine()
	if eng == nil || a.subagent {
		return
	}
	auto := true
	if a.loadSettings != nil {
		auto = a.loadSettings().LadyMAutoRetain
	}
	if !auto {
		return
	}
	ws := a.MemoryWorkspace()
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
		a.obs.Log("warn", "retain_write_error", map[string]any{"op": "record_event", "error": err.Error()})
	}
	// The fact is optional (no answer, e.g. an errored run), but episodes
	// alone are consolidation food, so the System 2 trigger below still runs.
	if answer := lastAIText(final); answer != "" {
		q := clampUTF8(question, 2000)
		ans := clampUTF8(answer, 4000)
		if _, err := eng.Remember(ctx, ws, fmt.Sprintf("Q: %s\nA: %s", q, ans), []string{"woca", "auto"}, "woca:auto"); err != nil {
			a.obs.Log("warn", "retain_write_error", map[string]any{"op": "remember", "error": err.Error()})
		}
	}
	// System 2 (opportunistic, per-workspace throttle): consolidate the
	// fresh episodes into facts/playbooks/insights in the background. The
	// cycle itself gates on ladyM's episode threshold, so a no-op trigger
	// is cheap (spec §4.5).
	if a.loadSettings != nil {
		if s := a.loadSettings(); s.LadyMSystem2 {
			interval := int64(settings.DefaultLadyMSystem2Interval)
			if s.LadyMSystem2Interval >= settings.MinLadyMSystem2Interval {
				interval = int64(s.LadyMSystem2Interval)
			}
			if a.system2Due(ws, interval) {
				engC := eng
				go func() {
					if _, err := engC.Consolidate(context.WithoutCancel(ctx), ws); err != nil && a.obs != nil {
						a.obs.Log("warn", "system2_error", map[string]any{"workspace": ws, "error": err.Error()})
					}
				}()
			}
		}
	}
}

// RetainGoal stores a completed goal-mode session into memory: the objective
// and its terminal narrative as an L2 fact, and an episode for System 2. Goal
// runs bypass Runner.Run (they execute the goal graph directly), so RunGoal's
// defer is their only retain hook.
func (a *Agent) RetainGoal(ctx context.Context, objective, status, summary string, runErr error) {
	eng := a.MemoryEngine()
	if eng == nil || a.subagent {
		return
	}
	auto := true
	if a.loadSettings != nil {
		auto = a.loadSettings().LadyMAutoRetain
	}
	if !auto || strings.TrimSpace(objective) == "" {
		return
	}
	ws := a.MemoryWorkspace()
	outcome := "success"
	switch {
	case runErr != nil:
		outcome = "error"
	case status == string(GoalBlocked), status == string(GoalBudgetLimited), status == string(GoalPaused):
		outcome = status
	}
	if err := eng.RecordEvent(ctx, ws, "woca", "goal:"+status, clampUTF8(objective, 500), outcome); err != nil && a.obs != nil {
		a.obs.Log("warn", "retain_write_error", map[string]any{"op": "goal_event", "error": err.Error()})
	}
	// The fact is optional (no summary, e.g. a paused or errored run), but the
	// episode above still feeds System 2.
	if ans := strings.TrimSpace(summary); ans != "" {
		content := "GOAL: " + clampUTF8(objective, 2000) + "\nRESULT(" + status + "): " + clampUTF8(ans, 4000)
		if _, err := eng.Remember(ctx, ws, content, []string{"woca", "auto", "goal"}, "woca:auto"); err != nil && a.obs != nil {
			a.obs.Log("warn", "retain_write_error", map[string]any{"op": "goal_remember", "error": err.Error()})
		}
	}
}

// recordTaskEvent logs a completed subagent task as an L1 episode (reads
// shared, writes governed: subagents report events, not facts). Best-effort.
func (a *Agent) recordTaskEvent(name, description, report string, taskErr error) {
	eng := a.MemoryEngine()
	if eng == nil || a.subagent {
		return
	}
	outcome := "success"
	if taskErr != nil {
		outcome = "error"
	}
	action := clampUTF8("task:"+description, 200)
	obs := clampUTF8(report, 500)
	if err := eng.RecordEvent(context.Background(), a.memoryWorkspace(), "subagent:"+name, action, obs, outcome); err != nil && a.obs != nil {
		a.obs.Log("warn", "retain_write_error", map[string]any{"op": "task_event", "error": err.Error()})
	}
}

// system2Due applies the per-workspace minimum interval between System 2
// cycles and records the trigger time.
func (a *Agent) system2Due(ws string, intervalS int64) bool {
	now := time.Now().Unix()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.lastSystem2 == nil {
		a.lastSystem2 = map[string]int64{}
	}
	if last, ok := a.lastSystem2[ws]; ok && now-last < intervalS {
		return false
	}
	a.lastSystem2[ws] = now
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

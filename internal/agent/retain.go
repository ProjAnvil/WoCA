package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
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
	answer := lastAIText(final)
	if answer == "" {
		return
	}
	q := clampUTF8(question, 2000)
	ans := clampUTF8(answer, 4000)
	if _, err := eng.Remember(ctx, ws, fmt.Sprintf("Q: %s\nA: %s", q, ans), []string{"woca", "auto"}, "woca:auto"); err != nil {
		a.obs.Log("warn", "retain_write_error", map[string]any{"op": "remember", "error": err.Error()})
	}
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

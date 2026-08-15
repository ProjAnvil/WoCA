package agent

import (
	"context"
	"fmt"

	"github.com/projanvil/langchain-golang/core/messages"
)

// RetainRun stores a completed run into long-term memory: the Q/A pair as an
// L2 fact (ladyM's attention gate dedups noise) and the run outcome as an L1
// episode (food for System 2). It is the single write choke point, called by
// the runner when a run finishes; subagents never pass through the runner.
// Writes are best-effort: errors are dropped (the memory subsystem must never
// fail a run).
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
	question := firstHumanText(history)
	if question == "" {
		return
	}
	outcome := "success"
	if runErr != nil {
		outcome = "error"
	}
	obs := clampUTF8(question, 500)
	_ = eng.RecordEvent(ctx, ws, "woca", "run", obs, outcome)
	answer := lastAIText(final)
	if answer == "" {
		return
	}
	q := clampUTF8(question, 2000)
	ans := clampUTF8(answer, 4000)
	_, _ = eng.Remember(ctx, ws, fmt.Sprintf("Q: %s\nA: %s", q, ans), []string{"woca", "auto"}, "woca:auto")
}

// firstHumanText returns the first human message of the run (the request).
// (lastAIText is shared with the subagent path; see task.go.)
func firstHumanText(msgs []messages.Message) string {
	for _, m := range msgs {
		if m.Role == messages.RoleHuman {
			return m.Content
		}
	}
	return ""
}

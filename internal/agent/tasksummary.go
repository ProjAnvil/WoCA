package agent

import (
	"context"
	"strings"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
)

// Task summaries, a port of Claude Code's `claude ps` support
// (utils/taskSummary + query.ts's maybeGenerateTaskSummary): every few
// model rounds the summarizer condenses what the agent is currently working
// on into one line, kept as the agent's latest task summary for status
// surfaces (/status). Generation is async and best-effort.

// taskSummaryInterval is the model-round cadence between regenerations.
const taskSummaryInterval = 5

const taskSummaryPrompt = `Write one line (max ~80 characters) describing what the agent is currently working on, based on this conversation tail. Use the present continuous tense. Output only the line.

Examples:
- Refactoring the auth middleware
- Fixing failing tests in internal/store
- Drafting the API documentation`

// conversationTail renders the last n messages as plain text for prompts.
func conversationTail(msgs []messages.Message, n int) string {
	if len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	var b strings.Builder
	for _, m := range msgs {
		switch m.Role {
		case messages.RoleHuman, messages.RoleAI:
			b.WriteString(string(m.Role) + ": " + m.Content + "\n")
		case messages.RoleTool:
			b.WriteString("tool: " + summarizeTail(m.Content, 200) + "\n")
		}
	}
	return b.String()
}

func summarizeTail(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

// taskSummaryState carries the periodic summary state.
type taskSummaryState struct {
	mu      sync.Mutex
	rounds  int
	latest  string
	pending bool
}

// maybeBeginTaskSummary counts model rounds and periodically regenerates the
// summary in the background (top-level agents only).
func (a *Agent) maybeBeginTaskSummary(ctx context.Context, msgs []messages.Message) {
	if a.summarizer == nil || a.subagent {
		return
	}
	a.taskState.mu.Lock()
	a.taskState.rounds++
	due := a.taskState.rounds%taskSummaryInterval == 0 && !a.taskState.pending
	if due {
		a.taskState.pending = true
	}
	a.taskState.mu.Unlock()
	if !due {
		return
	}
	tail := conversationTail(msgs, 12)
	go func() {
		defer func() {
			a.taskState.mu.Lock()
			a.taskState.pending = false
			a.taskState.mu.Unlock()
		}()
		resp, err := a.summarizer.Invoke(context.WithoutCancel(ctx), []messages.Message{
			messages.System(taskSummaryPrompt),
			messages.Human(tail),
		})
		if err != nil {
			return // best-effort
		}
		if line := strings.TrimSpace(resp.Content); line != "" {
			a.taskState.mu.Lock()
			a.taskState.latest = line
			a.taskState.mu.Unlock()
		}
	}()
}

// TaskSummary returns the latest one-line description of what the agent is
// working on (empty when none has been generated).
func (a *Agent) TaskSummary() string {
	a.taskState.mu.Lock()
	defer a.taskState.mu.Unlock()
	return a.taskState.latest
}

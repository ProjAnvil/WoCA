package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
)

// Tool-use summaries, a port of Claude Code's toolUseSummaryGenerator +
// query.ts stage 24: after a tool batch completes, a short human-readable
// label ("Read config.json"-style, git-commit-subject length) is generated
// asynchronously with the summarizer model. Generation never blocks the next
// model round; the summary is UI-only (never enters the model context) and
// is consumed non-blockingly while the next round streams. Failures are
// silent — summaries are non-critical.

// toolUseSummarySystemPrompt mirrors Claude Code's generator prompt.
const toolUseSummarySystemPrompt = `Write a short summary label describing what these tool calls accomplished. It appears as a single-line row in a mobile app and truncates around 30 characters, so think git-commit-subject, not sentence.

Keep the verb in past tense and the most distinctive noun. Drop articles, connectors, and long location context first.

Examples:
- Searched in auth/
- Fixed NPE in UserService
- Created signup endpoint
- Read config.json
- Ran failing tests`

// ToolUseSummary is one generated label with the tool calls it describes.
type ToolUseSummary struct {
	ToolCallIDs []string
	Text        string
}

// toolSummary is one in-flight or settled summary.
type toolSummary struct {
	ids  []string
	done chan struct{}
	text string
}

// maxPendingToolSummaries bounds the buffer so abandoned summaries (runs
// that ended before consumption) cannot accumulate.
const maxPendingToolSummaries = 16

// toolSummaryBuffer holds pending summaries.
type toolSummaryBuffer struct {
	mu   sync.Mutex
	list []*toolSummary
}

func (b *toolSummaryBuffer) add(s *toolSummary) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.list = append(b.list, s)
	if len(b.list) > maxPendingToolSummaries {
		b.list = b.list[len(b.list)-maxPendingToolSummaries:]
	}
}

// settled returns and removes finished summaries (non-blocking).
func (b *toolSummaryBuffer) settled() []ToolUseSummary {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []ToolUseSummary
	kept := b.list[:0]
	for _, s := range b.list {
		select {
		case <-s.done:
			if s.text != "" {
				out = append(out, ToolUseSummary{ToolCallIDs: s.ids, Text: s.text})
			}
		default:
			kept = append(kept, s)
		}
	}
	b.list = kept
	return out
}

// waitAll blocks until every pending summary settles or ctx ends, returning
// the settled ones.
func (b *toolSummaryBuffer) waitAll(ctx context.Context) []ToolUseSummary {
	b.mu.Lock()
	pending := append([]*toolSummary(nil), b.list...)
	b.mu.Unlock()

	for _, s := range pending {
		select {
		case <-s.done:
		case <-ctx.Done():
			return b.settled()
		}
	}
	return b.settled()
}

// beginToolUseSummary asynchronously generates a label for one completed
// tool batch. Skipped when disabled, without a summarizer, or for
// subagents (their batches never surface in the UI).
func (a *Agent) beginToolUseSummary(ctx context.Context, calls []messages.ToolCall, results []messages.Message, lastAssistantText string) {
	if !a.emitSummaries || a.summarizer == nil || a.subagent || len(calls) == 0 {
		return
	}
	ids := make([]string, len(calls))
	for i, c := range calls {
		ids[i] = c.ID
	}
	var b strings.Builder
	if lastAssistantText != "" {
		fmt.Fprintf(&b, "User's intent (from assistant's last message): %s\n\n", truncateString(lastAssistantText, 200))
	}
	b.WriteString("Tools completed:\n\n")
	for i, c := range calls {
		in, _ := json.Marshal(c.Args)
		out := ""
		if i < len(results) {
			out = results[i].Content
		}
		fmt.Fprintf(&b, "Tool: %s\nInput: %s\nOutput: %s", c.Name, truncateString(string(in), 300), truncateString(out, 300))
		if i < len(calls)-1 {
			b.WriteString("\n\n")
		}
	}
	b.WriteString("\n\nLabel:")

	s := &toolSummary{ids: ids, done: make(chan struct{})}
	a.toolSummaries.add(s)
	go func() {
		defer close(s.done)
		// Detach from the run's cancellation: the graph stream cancels its
		// context the moment the terminal step is emitted, which would kill
		// an in-flight summary before the runner's end-of-turn wait sees it.
		// Summaries are best-effort and bounded; an abandoned one is dropped
		// when unconsumed.
		resp, err := a.summarizer.Invoke(context.WithoutCancel(ctx), []messages.Message{
			messages.System(toolUseSummarySystemPrompt),
			messages.Human(b.String()),
		})
		if err != nil {
			return // summaries are non-critical
		}
		s.text = strings.TrimSpace(resp.Content)
	}()
}

// TakeReadyToolUseSummaries returns summaries whose generation already
// finished, removing them from the buffer (non-blocking). The runner polls
// this while the next model round streams.
func (a *Agent) TakeReadyToolUseSummaries() []ToolUseSummary {
	return a.toolSummaries.settled()
}

// WaitToolUseSummaries waits for in-flight summaries to settle (bounded by
// ctx) and returns everything ready — used at turn end so the final batch's
// summary is not lost.
func (a *Agent) WaitToolUseSummaries(ctx context.Context) []ToolUseSummary {
	return a.toolSummaries.waitAll(ctx)
}

// PendingToolUseSummaryCount reports unsettled + unconsumed summaries.
func (a *Agent) PendingToolUseSummaryCount() int {
	a.toolSummaries.mu.Lock()
	defer a.toolSummaries.mu.Unlock()
	return len(a.toolSummaries.list)
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

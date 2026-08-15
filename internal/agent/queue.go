package agent

import (
	"strings"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
)

// Mid-turn input queue, a port of Claude Code's queued-command attachments
// (query.ts stage 27): input submitted while a run is in flight is buffered
// and, once the current tool round completes, appended AFTER the tool
// results so the model can address it within the same turn — the API forbids
// interleaving tool results with user messages, so the attachment must come
// after the whole result batch. Each Agent owns its queue, so subagents
// (separate Agent instances) never receive the parent's queued input —
// Claude Code's per-agent drain scoping.

// QueuedInput is one unit of mid-turn input. A non-empty Tag wraps the text
// in a differently-named element (e.g. background-task completions) so the
// model can tell machine notifications apart from typed user input.
type QueuedInput struct {
	Text string
	Tag  string
}

// queuedInputTag wraps queued text so the model can tell it apart from the
// original prompt.
const queuedInputTag = "queued-user-input"

// formatQueuedInput renders one queued input as a message.
func formatQueuedInput(q QueuedInput) messages.Message {
	tag := q.Tag
	if tag == "" {
		tag = queuedInputTag
	}
	return messages.Human("<" + tag + ">\n" + strings.TrimSpace(q.Text) + "\n</" + tag + ">")
}

// queue is the thread-safe mid-turn input buffer.
type queue struct {
	mu    sync.Mutex
	items []QueuedInput
}

func (q *queue) add(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, QueuedInput{Text: text})
}

// addTagged buffers a tagged notification (machine-generated input).
func (q *queue) addTagged(tag, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = append(q.items, QueuedInput{Text: text, Tag: tag})
}

// drain returns and clears all queued inputs in arrival order.
func (q *queue) drain() []QueuedInput {
	q.mu.Lock()
	defer q.mu.Unlock()
	items := q.items
	q.items = nil
	return items
}

func (q *queue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

// QueueInput buffers user input submitted while a run is in flight. The
// next completed tool round attaches it after its results (see toolsNode);
// input queued after the final tool round stays for the next run's first
// tool round. Safe for concurrent use.
func (a *Agent) QueueInput(text string) {
	a.inputs.add(text)
}

// QueueNotification buffers a machine-generated notification (e.g. a
// background-shell completion) for delivery after the next tool round,
// wrapped in its own tag so the model can tell it from typed user input.
func (a *Agent) QueueNotification(tag, text string) {
	a.inputs.addTagged(tag, text)
}

// DrainQueuedInputs returns and clears the queued mid-turn inputs.
func (a *Agent) DrainQueuedInputs() []QueuedInput {
	return a.inputs.drain()
}

// QueuedInputCount reports how many inputs are currently queued.
func (a *Agent) QueuedInputCount() int {
	return a.inputs.len()
}

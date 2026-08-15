package agent

import (
	"context"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
)

// Streaming tool execution, a port of Claude Code's StreamingToolExecutor:
// while the model node is still consuming the response stream, tool calls
// that have already arrived complete are started in the background when the
// permission mode deterministically allows them. The tools node later awaits
// these futures instead of re-running the calls.
//
// Both bundled providers emit each tool call as a single finished chunk when
// its content block closes (anthropic: contentBlockStop; openai:
// output_item.done), so a chunk carrying a ToolCall with ID and Name marks
// the call as stream-complete.
//
// Calls that would need the interaction gate (permission ask,
// AskUserQuestion, plan approval — all implemented via graph interrupts) are
// never pre-started: they wait for the tools node, which can interrupt the
// graph deterministically.

// toolFuture is the eventual result of one speculatively executed tool call.
// cancel aborts the call's context; it is used when the model stream the
// call was dispatched from is restarted (retry/model fallback) and the
// partial response — including its speculative calls — must be discarded.
type toolFuture struct {
	done   chan struct{}
	msg    messages.Message
	cancel context.CancelFunc
}

func newToolFuture(cancel context.CancelFunc) *toolFuture {
	return &toolFuture{done: make(chan struct{}), cancel: cancel}
}

// wait blocks until the background execution finishes. The executing tool
// observes the run context, so cancellation bounds the wait.
func (f *toolFuture) wait() messages.Message {
	<-f.done
	return f.msg
}

// maxSpeculativeFutures bounds the registry so abandoned futures (model
// stream errors) cannot accumulate unboundedly in a long-lived process.
const maxSpeculativeFutures = 128

// speculativeRegistry tracks in-flight speculatively executed calls by tool
// call ID. IDs are unique per model turn (including across concurrent runs
// on one agent), so cross-run contamination is impossible. Futures are
// removed only after the tools node has successfully consumed them; on the
// interrupt path they stay so a same-process resume reuses the result.
type speculativeRegistry struct {
	mu      sync.Mutex
	futures map[string]*toolFuture
	order   []string
}

// add registers f under id, reporting whether it was new. The oldest entries
// are evicted past the cap (their calls then simply re-run in the tools
// node).
func (r *speculativeRegistry) add(id string, f *toolFuture) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.futures == nil {
		r.futures = map[string]*toolFuture{}
	}
	if _, ok := r.futures[id]; ok {
		return false
	}
	r.futures[id] = f
	r.order = append(r.order, id)
	for len(r.order) > 0 && len(r.futures) > maxSpeculativeFutures {
		delete(r.futures, r.order[0])
		r.order = r.order[1:]
	}
	return true
}

// get returns the future for id without removing it.
func (r *speculativeRegistry) get(id string) *toolFuture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.futures[id]
}

// consume removes futures after their results have been merged into state.
func (r *speculativeRegistry) consume(ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		delete(r.futures, id)
	}
}

// cancel aborts and removes the futures for ids — used when the model
// stream they were dispatched from is discarded (stream retry/fallback), the
// equivalent of Claude Code's StreamingToolExecutor.discard().
func (r *speculativeRegistry) cancel(ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if f, ok := r.futures[id]; ok {
			if f.cancel != nil {
				f.cancel()
			}
			delete(r.futures, id)
		}
	}
}

// speculativeAllowed reports whether call may be pre-started during model
// streaming: it mirrors checkPermission minus every path that interrupts
// (ask rules and the mode fallthroughs that would prompt). A call that is
// not deterministically allowed is simply left to the tools node.
func (a *Agent) speculativeAllowed(call messages.ToolCall) bool {
	switch call.Name {
	case "AskUserQuestion", "EnterPlanMode", "ExitPlanMode":
		return false // interaction gates must run in the tools node
	}
	if a.perms != nil {
		if action, ok := a.perms.Action(call.Name, call.Args); ok && action != permissions.ActionAllow {
			return false // deny is pointless to pre-run; ask needs an interrupt
		}
	}
	readOnly := tools.IsReadOnly(call.Name, call.Args)
	cat := tools.CategoryOf(call.Name)

	a.mu.Lock()
	mode := a.mode
	a.mu.Unlock()
	switch mode {
	case config.ModeBypass:
		return true
	case config.ModePlan, config.ModeReadOnly:
		return readOnly || cat == tools.CatMeta
	case config.ModeAcceptEdits:
		return cat == tools.CatWrite || readOnly || cat == tools.CatMeta
	default: // ModeDefault
		return readOnly || cat == tools.CatMeta
	}
}

// dispatchSpeculative starts executing a stream-complete tool call in the
// background. Duplicate dispatches (chunk replays merged by ID) are ignored.
// It reports whether this call started a new speculative execution.
func (a *Agent) dispatchSpeculative(ctx context.Context, call messages.ToolCall) bool {
	if call.ID == "" || call.Name == "" || !a.speculativeAllowed(call) {
		return false
	}
	fctx, cancel := context.WithCancel(ctx)
	f := newToolFuture(cancel)
	if !a.spec.add(call.ID, f) {
		cancel()
		return false
	}
	go func() {
		defer cancel()
		defer close(f.done)
		f.msg = a.runToolCall(fctx, call)
	}()
	return true
}

// runToolCall executes one already-permitted call with its Pre/Post tool-use
// hooks — the shared body of the tools node's execution loop and the
// speculative path. Hook denials become error results; a hook that itself
// fails to execute does not abort the turn here (unlike the tools node,
// which cannot be reproduced from a background goroutine), so execution
// proceeds without the hook's decision.
func (a *Agent) runToolCall(ctx context.Context, call messages.ToolCall) messages.Message {
	preFeedback := ""
	if a.hooks != nil {
		if d, err := a.hooks.PreToolUse(ctx, call.Name, call.Args); err == nil {
			if !d.Allowed {
				content := "Error: tool use denied by hook"
				if d.Feedback != "" {
					content = "Error: " + d.Feedback
				}
				return messages.Tool(call.ID, content)
			}
			preFeedback = d.Feedback
		}
	}
	res := a.execute(ctx, call.Name, call.Args)
	content := res.Content
	if preFeedback != "" {
		content = preFeedback + "\n" + content
	}
	if a.hooks != nil {
		if p, err := a.hooks.PostToolUse(ctx, call.Name, call.Args, content); err == nil && p.Feedback != "" {
			content += "\n" + p.Feedback
		}
	}
	return toolMessageFromResult(call.ID, content, &res)
}

// toolMessageFromResult builds the tool-result message, attaching the image
// content block a tool may carry in its metadata (Read on an image file) —
// Claude Code's image attachments. Providers without tool-result images drop
// the block at translation.
func toolMessageFromResult(callID, content string, res *coretools.Result) messages.Message {
	m := messages.Tool(callID, content)
	if res == nil || len(res.Metadata) == 0 {
		return m
	}
	b64, _ := res.Metadata[tools.MetaImageBase64].(string)
	if b64 == "" {
		return m
	}
	mime, _ := res.Metadata[tools.MetaImageMime].(string)
	m.ContentBlocks = []messages.ContentBlock{messages.ImageBlock{
		Base64:   b64,
		MimeType: mime,
	}}
	return m
}

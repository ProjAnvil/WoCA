package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"runtime/debug"
	"slices"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/permissions"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// Streaming tool execution: while the model node is still consuming the
// response stream, tool calls
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
//
// Run isolation (spec decision F1): the registry is keyed by (run id,
// call id) and every future is derived from the run context — the ctx the
// driver passed into Stream/Invoke/GoalStream, captured by beginRunCtx —
// never from the graph's per-superstep ctx. Tool call ids are unique only
// within one provider conversation, so two concurrent runs on a shared agent
// legally reuse them; keying by run keeps them from poisoning each other,
// and the run ctx keeps a parked run's futures alive across the stream
// teardown its sibling's GraphInterrupt causes (park blocks without
// cancelling the run, so a same-process resume finds its own futures).

// toolFuture is the eventual result of one speculatively executed tool call.
// cancel aborts the call's context; it is used when the model stream the
// call was dispatched from is restarted (retry/model fallback) and the
// partial response — including its speculative calls — must be discarded,
// and when the run ends (EndRun).
type toolFuture struct {
	done   chan struct{}
	msg    messages.Message
	cancel context.CancelFunc
	// call is the dispatched call (F2 collision reporting).
	call messages.ToolCall
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

// maxSpeculativeFutures bounds the registry (per run ids share one budget)
// so abandoned futures cannot accumulate unboundedly in a long-lived
// process.
const maxSpeculativeFutures = 128

// specKey identifies one speculative execution: the run that dispatched it
// plus the tool call id. Call ids are unique only per conversation, so the
// run id is what isolates concurrent runs on a shared agent.
type specKey struct {
	run  string
	call string
}

// speculativeRegistry tracks in-flight speculatively executed calls by
// (run id, call id). Futures are removed after the tools node has
// successfully consumed them, or when their run ends (EndRun); on the
// interrupt path they stay so a same-process resume reuses the result.
type speculativeRegistry struct {
	mu      sync.Mutex
	futures map[specKey]*toolFuture
	order   []specKey
}

// add registers f for call under runID, reporting whether it was new.
// A re-dispatch of the SAME call (chunk replays merged by ID) is ignored.
// A DIFFERENT call reusing a live (run, id) key is a collision (spec F2):
// the new dispatch is rejected loudly — the entry is replaced by an
// already-resolved error result naming both calls, and the first execution
// is cancelled — never silently absorbed. The oldest entries are evicted
// past the cap: eviction only forgets them (bounding the memory abandoned
// futures occupy) — it never cancels, so a future the tools node already
// holds via get still runs to completion and returns its real result; the
// cancel handle stays with the run's own lifecycle (cancel/endRun), which
// still bounds every future of the run.
func (r *speculativeRegistry) add(runID string, call messages.ToolCall, f *toolFuture) bool {
	key := specKey{run: runID, call: call.ID}
	f.call = call
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.futures == nil {
		r.futures = map[specKey]*toolFuture{}
	}
	if existing, ok := r.futures[key]; ok {
		if sameToolCall(existing.call, call) {
			return false // replay of the same call; keep the first execution
		}
		// Collision: poison the id for the rest of the run so every
		// consumer of it gets a loud error naming the clash, and stop the
		// first execution (both calls are rejected).
		if existing.cancel != nil {
			existing.cancel()
		}
		err := newToolFuture(nil)
		err.call = existing.call
		err.msg = messages.Tool(call.ID, specCollisionMessage(runID, existing.call, call))
		close(err.done)
		r.futures[key] = err
		return false
	}
	r.futures[key] = f
	r.order = append(r.order, key)
	for len(r.order) > 0 && len(r.futures) > maxSpeculativeFutures {
		evict := r.order[0]
		r.order = r.order[1:]
		// Forget the entry without cancelling it: the tools node may
		// already hold this future via get and be awaiting its result —
		// an evicted-but-awaited future must still run to completion and
		// deliver its real result. Eviction bounds the memory of
		// abandoned futures only; the cancel handle stays with the
		// run's lifecycle (cancel/endRun), which still bounds it.
		delete(r.futures, evict)
	}
	return true
}

// get returns the future for (runID, callID) without removing it.
func (r *speculativeRegistry) get(runID, callID string) *toolFuture {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.futures[specKey{run: runID, call: callID}]
}

// consume removes futures after their results have been merged into state.
func (r *speculativeRegistry) consume(runID string, ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dropLocked(runID, ids...)
}

// cancel aborts and removes the futures for ids — used when the model
// stream they were dispatched from is discarded (stream retry/fallback).
func (r *speculativeRegistry) cancel(runID string, ids ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range ids {
		if f, ok := r.futures[specKey{run: runID, call: id}]; ok {
			if f.cancel != nil {
				f.cancel()
			}
			delete(r.futures, specKey{run: runID, call: id})
		}
	}
	r.dropLocked(runID, ids...)
}

// endRun cancels and removes every future of runID (spec F1: a run's
// speculative bookkeeping dies with the run — done, error and cancel paths
// alike).
func (r *speculativeRegistry) endRun(runID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, f := range r.futures {
		if key.run != runID {
			continue
		}
		if f.cancel != nil {
			f.cancel()
		}
		delete(r.futures, key)
	}
	r.order = slices.DeleteFunc(r.order, func(k specKey) bool { return k.run == runID })
}

// dropLocked removes ids from the eviction order (futures may already be
// gone; the order must not outlive its entries).
func (r *speculativeRegistry) dropLocked(runID string, ids ...string) {
	r.order = slices.DeleteFunc(r.order, func(k specKey) bool {
		return k.run == runID && slices.Contains(ids, k.call)
	})
	for _, id := range ids {
		delete(r.futures, specKey{run: runID, call: id})
	}
}

// sameToolCall reports whether two dispatches describe the identical call
// (same name and arguments — the id is equal by construction).
func sameToolCall(a, b messages.ToolCall) bool {
	return a.Name == b.Name && reflect.DeepEqual(a.Args, b.Args)
}

// specCollisionMessage names the two dispatches that collided on one
// (run, call id) so the model sees exactly which id clashed.
func specCollisionMessage(runID string, first, dup messages.ToolCall) string {
	return fmt.Sprintf("Error: duplicate speculative tool call id %q within one run (%q): %s collides with %s; both calls rejected — tool call ids must be unique within a run",
		dup.ID, runID, describeToolCall(first), describeToolCall(dup))
}

// describeToolCall renders a call for diagnostics: name plus its arguments,
// truncated.
func describeToolCall(c messages.ToolCall) string {
	args, err := json.Marshal(c.Args)
	if err != nil {
		args = []byte("...")
	}
	s := string(args)
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return fmt.Sprintf("%s(%s)", c.Name, s)
}

// specRunID returns the speculative-registry bucket for ctx's run: the run
// id its ctx carries via tracing.WithRun (every server-driven run — plain,
// goal, btw, wake, and the ee worker — sets it). Runs without a run scope
// (the TUI/REPL single-run surfaces and unit tests) share the "" bucket —
// the same convention as the session "" of tracing.VisibleTo: at most one
// such run is in flight, so bare call ids cannot collide across concurrent
// runs. Concurrent unscoped runs on one shared agent would still interfere;
// scoped ctxs are the contract for multi-run surfaces.
func specRunID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if sc, ok := tracing.RunFrom(ctx); ok {
		return sc.RunID
	}
	return ""
}

// beginRunCtx captures the true run context for runID: the ctx the DRIVER
// passed into Stream/Invoke/GoalStream — the per-run carrier pattern of
// WithMemoryScope, inverted (the run ctx is stored on the agent instead of
// a scope stored in the ctx) because the model node must reach it from the
// graph's per-superstep ctx, which is cancelled when the stream iterator
// ends (a sibling GraphInterrupt pauses the graph and tears down the
// stream while the run itself stays alive parked).
//
// The first live registration wins: goal execute cycles re-enter
// streamWithBudget with the goal graph's derived node ctx, which must not
// clobber the driver's run ctx. A dead entry (its driver skipped EndRun) is
// replaced by the newer ctx.
func (a *Agent) beginRunCtx(runID string, ctx context.Context) {
	a.runCtxMu.Lock()
	defer a.runCtxMu.Unlock()
	if cur, ok := a.runCtxs[runID]; ok && cur.Err() == nil {
		return
	}
	if a.runCtxs == nil {
		a.runCtxs = map[string]context.Context{}
	}
	a.runCtxs[runID] = ctx
}

// runCtxFor returns the captured run context for runID, falling back to the
// caller's ctx when no live entry exists (direct dispatch in unit tests;
// drivers that bypass the Stream/Invoke funnels).
func (a *Agent) runCtxFor(runID string, fallback context.Context) context.Context {
	a.runCtxMu.Lock()
	defer a.runCtxMu.Unlock()
	if ctx, ok := a.runCtxs[runID]; ok && ctx.Err() == nil {
		return ctx
	}
	return fallback
}

// EndRun releases a run's speculative-execution bookkeeping: remaining
// futures are cancelled (the run is over — done, errored or cancelled — so
// stragglers like a tail -f speculated sibling must not outlive it), the
// captured run context is dropped, and the run's batch-memo entries are
// removed (a run cancelled while parked never returns from its tools node,
// so its memoized results would otherwise leak — and, on the "" bucket,
// outlive their run). Every driver calls it on every exit path (runner.Run
// and RunGoal defers; the ee worker). Idempotent and a no-op for runs that
// never speculated. Runs without a tracing scope release the "" bucket (see
// specRunID).
func (a *Agent) EndRun(ctx context.Context) {
	runID := specRunID(ctx)
	a.spec.endRun(runID)
	a.runCtxMu.Lock()
	delete(a.runCtxs, runID)
	a.runCtxMu.Unlock()
	a.batchMemo.Range(func(k, _ any) bool {
		if key, ok := k.(batchMemoKey); ok && key.run == runID {
			a.batchMemo.Delete(k)
		}
		return true
	})
}

// speculativeAllowed reports whether call may be pre-started during model
// streaming: it mirrors checkPermission minus every path that interrupts
// (ask rules and the mode fallthroughs that would prompt). A call that is
// not deterministically allowed is simply left to the tools node.
func (a *Agent) speculativeAllowed(ctx context.Context, call messages.ToolCall) bool {
	switch call.Name {
	case "AskUserQuestion", "EnterPlanMode", "ExitPlanMode":
		return false // interaction gates must run in the tools node
	case "Task", "ScheduleCron":
		// CatMeta but with irreversible side effects (spawning a subagent,
		// registering a cron): a speculated call whose stream is restarted
		// would execute twice.
		return false
	}
	if a.perms != nil {
		if action, ok := a.perms.Action(call.Name, call.Args); ok && action != permissions.ActionAllow {
			return false // deny is pointless to pre-run; ask needs an interrupt
		}
	}
	// Session grants occupy the same slot as in checkPermission (design §3.1:
	// grants are consulted in the gate AND here) — after the explicit rules,
	// before the mode arms. A granted call is deterministically allowed, but
	// grants never lift the read-only gate below: a granted non-read-only
	// call (the shell commands "allow for session" covers) waits for the
	// tools node exactly like a call under an explicit allow rule, because a
	// speculated call whose stream is restarted would have executed twice.
	if key := GrantKey(call); key != "" && a.policyFor(ctx).Grants[key] {
		return tools.IsReadOnly(call.Name, call.Args) || tools.CategoryOf(call.Name) == tools.CatMeta
	}
	// Speculative dispatch only runs read-only (or meta) tools. Writes wait
	// for the real toolsNode regardless of permission mode — a speculated
	// write whose stream is later restarted would have executed twice.
	readOnly := tools.IsReadOnly(call.Name, call.Args)
	cat := tools.CategoryOf(call.Name)
	return readOnly || cat == tools.CatMeta
}

// dispatchSpeculative starts executing a stream-complete tool call in the
// background. Duplicate dispatches (chunk replays merged by ID) are ignored;
// a different call reusing a live id within the same run is rejected loudly
// (see speculativeRegistry.add). It reports whether this call started a new
// speculative execution.
func (a *Agent) dispatchSpeculative(ctx context.Context, call messages.ToolCall) bool {
	// Guards must see every call before it starts: speculative execution
	// would let a tool run behind their back, so it is disabled entirely
	// while guards are installed.
	if len(a.toolGuards) > 0 {
		return false
	}
	if call.ID == "" || call.Name == "" || !a.speculativeAllowed(ctx, call) {
		return false
	}
	runID := specRunID(ctx)
	// Derive from the run context, not the graph's per-superstep ctx: a
	// sibling's GraphInterrupt ends the stream iterator, which cancels the
	// derived ctx — the future must survive that teardown (the run stays
	// alive parked) and die only with the run itself.
	fctx, cancel := context.WithCancel(a.runCtxFor(runID, ctx))
	f := newToolFuture(cancel)
	if !a.spec.add(runID, call, f) {
		cancel()
		return false
	}
	go func() {
		defer cancel()
		defer close(f.done)
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				if len(stack) > 2048 {
					stack = stack[:2048]
				}
				f.msg = messages.Tool(call.ID, fmt.Sprintf("Error: tool %q panicked: %v\n%s", call.Name, rec, stack))
			}
		}()
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
	if h := a.hooksFor(ctx); h != nil {
		if d, err := h.PreToolUse(ctx, call.Name, call.Args); err == nil {
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
	if h := a.hooksFor(ctx); h != nil {
		if p, err := h.PostToolUse(ctx, call.Name, call.Args, content); err == nil && p.Feedback != "" {
			content += "\n" + p.Feedback
		}
	}
	return toolMessageFromResult(call.ID, content, &res)
}

// toolMessageFromResult builds the tool-result message, attaching the image
// content block a tool may carry in its metadata (Read on an image file).
// Providers without tool-result images drop the block at translation.
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

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// recordingGuard captures the calls it gates and replays fixed decisions.
type recordingGuard struct {
	before ToolDecision
	after  ToolPostDecision

	mu    chanMu
	seenC []ToolCallInfo
	seenR []ToolResultInfo
}

type chanMu struct{}

func (g *recordingGuard) BeforeTool(_ context.Context, call ToolCallInfo) ToolDecision {
	g.seenC = append(g.seenC, call)
	return g.before
}

func (g *recordingGuard) AfterTool(_ context.Context, call ToolCallInfo, res ToolResultInfo) ToolPostDecision {
	g.seenR = append(g.seenR, res)
	return g.after
}

func toolLoopConfig(t *testing.T, model language.ChatModel, guards []ToolGuard) Config {
	t.Helper()
	return Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
		ToolGuards:     guards,
	}
}

func toolResultBy(out []messages.Message, id string) string {
	for _, m := range out {
		if m.Role == messages.RoleTool && m.ToolCallID == id {
			return m.Content
		}
	}
	return ""
}

func TestToolGuardDenies(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
			}},
			messages.AI("done"),
		),
	)
	g := &recordingGuard{before: DenyTool("not on my watch")}
	a, err := New(toolLoopConfig(t, model, []ToolGuard{g}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultBy(out, "1"); !strings.Contains(got, "not on my watch") {
		t.Fatalf("tool result = %q, want the guard's deny reason", got)
	}
	if len(g.seenC) != 1 || g.seenC[0].Name != "Bash" {
		t.Fatalf("guard saw %+v", g.seenC)
	}
}

// TestToolGuardAsk: an Ask verdict escalates to the permission prompt even
// in bypass mode — the guard explicitly defers to the human.
func TestToolGuardAsk(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
			}},
			messages.AI("done"),
		),
	)
	g := &recordingGuard{before: AskTool()}
	a, err := New(toolLoopConfig(t, model, []ToolGuard{g}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
	ie := interruptFromError(t, err)
	req, ok := ie.Value.(PermissionRequest)
	if !ok {
		t.Fatalf("interrupt value = %T (%v), want PermissionRequest", ie.Value, ie.Value)
	}
	if req.ToolName != "Bash" {
		t.Fatalf("interrupt tool = %q", req.ToolName)
	}
	out, err := a.Resume(t.Context(), "t", "y")
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultBy(out, "1"); strings.Contains(got, "hi\n") && !strings.Contains(got, "Error") {
		// the tool really ran after approval
	} else {
		t.Fatalf("tool result after approval = %q", got)
	}
}

func TestToolGuardAfterToolFeedback(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
			}},
			messages.AI("done"),
		),
	)
	g := &recordingGuard{before: AllowTool(), after: ToolFeedback("audit note")}
	a, err := New(toolLoopConfig(t, model, []ToolGuard{g}))
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	got := toolResultBy(out, "1")
	if !strings.Contains(got, "hi") || !strings.Contains(got, "audit note") {
		t.Fatalf("tool result = %q, want tool output plus guard feedback", got)
	}
	if len(g.seenR) != 1 || g.seenR[0].ID != "1" {
		t.Fatalf("guard after-tool saw %+v", g.seenR)
	}
}

// ---- ModelHook ----

// funcModelHook replays fixed results and records turns.
type funcModelHook struct {
	before BeforeModelResult
	after  *ModelReply

	turns []*ModelTurn
	reply *ModelReply
}

func (h *funcModelHook) BeforeModel(_ context.Context, turn *ModelTurn) BeforeModelResult {
	h.turns = append(h.turns, turn)
	return h.before
}

func (h *funcModelHook) AfterModel(_ context.Context, turn *ModelTurn, reply *ModelReply) *ModelReply {
	h.turns = append(h.turns, turn)
	h.reply = reply
	return h.after
}

func TestModelHookPrependContext(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("done")))
	rec := &recordingModel{FakeChatModel: model}
	h := &funcModelHook{before: BeforeModelResult{PrependContext: "quarterly revenue is up"}}
	a, err := New(Config{
		Model: rec, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelHooks: []ModelHook{h},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("summarize")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) == 0 {
		t.Fatal("model was not called")
	}
	lastReq := rec.seen[len(rec.seen)-1]
	var injected bool
	for _, m := range lastReq {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "quarterly revenue is up") {
			injected = true
		}
	}
	if !injected {
		t.Fatalf("request missing injected context: %+v", lastReq)
	}
	for _, m := range out {
		if strings.Contains(m.Content, "quarterly revenue is up") {
			t.Fatalf("injected context leaked into the persisted transcript: %+v", m)
		}
	}
	if len(h.turns) == 0 || h.turns[0].Prompt != "summarize" {
		t.Fatalf("model turn info = %+v", h.turns)
	}
}

func TestModelHookSkipsCall(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("real reply")))
	rec := &recordingModel{FakeChatModel: model}
	h := &funcModelHook{before: BeforeModelResult{Reply: &ModelReply{Text: "canned reply"}}}
	a, err := New(Config{
		Model: rec, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelHooks: []ModelHook{h},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.seen) != 0 {
		t.Fatalf("model was called %d times, want 0 (skipped)", len(rec.seen))
	}
	if got := out[len(out)-1]; got.Role != messages.RoleAI || got.Content != "canned reply" {
		t.Fatalf("final message = %+v, want the canned reply", got)
	}
}

func TestModelHookReplacesReply(t *testing.T) {
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("original")))
	h := &funcModelHook{after: &ModelReply{Text: "replaced"}}
	a, err := New(Config{
		Model: model, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelHooks: []ModelHook{h},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := out[len(out)-1]; got.Content != "replaced" {
		t.Fatalf("final message = %+v, want the replaced text", got)
	}
	if h.reply == nil || h.reply.Text != "original" {
		t.Fatalf("AfterModel saw %+v, want the original reply", h.reply)
	}
}

// TestModelHookReplacementKeepsToolCalls: AfterModel replaces the reply
// TEXT; the model's tool intents are structural and survive the
// replacement (gate them with a ToolGuard instead).
func TestModelHookReplacementKeepsToolCalls(t *testing.T) {
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
			}},
			messages.AI("done"),
		),
	)
	h := &funcModelHook{after: &ModelReply{Text: "replaced preamble"}}
	a, err := New(Config{
		Model: model, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelHooks: []ModelHook{h},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultBy(out, "1"); !strings.Contains(got, "hi") {
		t.Fatalf("tool result = %q, want the tool to have run", got)
	}
	if got := out[len(out)-1].Content; got != "replaced preamble" {
		t.Fatalf("final = %q, want the replaced text", got)
	}
}

// ---- ModelWrapper ----

// countingWrapper counts factory applications and Stream calls; its wrapped
// model forwards everything.
type countingWrapper struct {
	factories int
	streams   int
}

type countedModel struct {
	language.ChatModel
	w *countingWrapper
}

func (m *countedModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	m.w.streams++
	return m.ChatModel.Stream(ctx, in, opts...)
}

func (w *countingWrapper) WrapModel(m language.ChatModel) language.ChatModel {
	w.factories++
	return &countedModel{ChatModel: m, w: w}
}

func TestModelWrapperCountsRequests(t *testing.T) {
	fake := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "1", Name: "Bash", Args: map[string]any{"command": "echo hi"}},
			}},
			messages.AI("done"),
		),
	)
	w := &countingWrapper{}
	a, err := New(Config{
		Model: fake, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelWrappers: []ModelWrapper{w},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("run it")}, "t")
	if err != nil {
		t.Fatal(err)
	}
	if out[len(out)-1].Content != "done" {
		t.Fatalf("final = %+v", out[len(out)-1])
	}
	// Two model rounds (tool call + final) must both pass the wrapper.
	if w.streams != 2 {
		t.Fatalf("wrapper saw %d streams, want 2", w.streams)
	}
	if w.factories == 0 {
		t.Fatal("wrapper factory never applied")
	}
}

// TestModelWrapperSurvivesSetModel: the factory is re-applied on every tool
// rebind, so wrappers survive model swaps (the adapters' BindTools returns
// a bare bound copy that would otherwise drop them).
func TestModelWrapperSurvivesSetModel(t *testing.T) {
	fake1 := language.NewFakeChatModel(language.WithResponses(messages.AI("from one")))
	fake2 := language.NewFakeChatModel(language.WithResponses(messages.AI("from two")))
	w := &countingWrapper{}
	a, err := New(Config{
		Model: fake1, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelWrappers: []ModelWrapper{w},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t1")
	if err != nil {
		t.Fatal(err)
	}
	if out[len(out)-1].Content != "from one" {
		t.Fatalf("first final = %+v", out[len(out)-1])
	}
	if err := a.SetModel(fake2); err != nil {
		t.Fatal(err)
	}
	out, err = a.Invoke(t.Context(), []messages.Message{messages.Human("hi")}, "t2")
	if err != nil {
		t.Fatal(err)
	}
	if out[len(out)-1].Content != "from two" {
		t.Fatalf("second final = %+v, want the swapped model's reply", out[len(out)-1])
	}
	if w.streams < 2 || w.factories < 2 {
		t.Fatalf("after SetModel: streams=%d factories=%d, wrapper lost on rebind", w.streams, w.factories)
	}
}

// cacheWrapper short-circuits duplicate requests — the pattern that needs
// call-level wrap power (BeforeModel/AfterModel cannot serve a cached reply
// without calling the model).
type cacheWrapper struct {
	cache map[string]string
	inner int
}

type cachedModel struct {
	language.ChatModel
	w    *cacheWrapper
	key  string
	once sync.Once
	syn  bool
}

func (m *cachedModel) Stream(ctx context.Context, in []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	if reply, ok := m.w.cache[m.key]; ok {
		msg := messages.AI(reply)
		return runnables.NewSliceStream([]messages.Message{msg}), nil
	}
	m.w.inner++
	st, err := m.ChatModel.Stream(ctx, in, opts...)
	if err != nil {
		return nil, err
	}
	// Tee: remember the final text for the cache.
	tee := &teeStream{Stream: st, w: m.w, key: m.key}
	return tee, nil
}

type teeStream struct {
	runnables.Stream[messages.Message]
	w    *cacheWrapper
	key  string
	full strings.Builder
}

func (s *teeStream) Next(ctx context.Context) (messages.Message, bool, error) {
	msg, ok, err := s.Stream.Next(ctx)
	if ok && msg.Role == messages.RoleAI {
		s.full.WriteString(msg.Content)
	}
	if !ok || err != nil {
		if s.full.Len() > 0 {
			s.w.cache[s.key] = s.full.String()
		}
	}
	return msg, ok, err
}

func requestKey(in []messages.Message) string {
	raw, _ := json.Marshal(in)
	return string(raw)
}

func (w *cacheWrapper) WrapModel(m language.ChatModel) language.ChatModel {
	return &cachedModel{ChatModel: m, w: w}
}

func TestModelWrapperCacheShortCircuit(t *testing.T) {
	fake := language.NewFakeChatModel(language.WithResponses(messages.AI("expensive answer")))
	w := &cacheWrapper{cache: map[string]string{}}
	a, err := New(Config{
		Model: fake, Registry: tools.NewRegistry(t.TempDir()),
		SystemPrompt: "test", PermissionMode: config.ModeBypass,
		ModelWrappers: []ModelWrapper{w},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		out, err := a.Invoke(t.Context(), []messages.Message{messages.Human("same question")}, fmt.Sprintf("t%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if got := out[len(out)-1].Content; got != "expensive answer" {
			t.Fatalf("run %d final = %q", i, got)
		}
	}
	if w.inner != 1 {
		t.Fatalf("inner model called %d times, want 1 (second served from cache)", w.inner)
	}
}

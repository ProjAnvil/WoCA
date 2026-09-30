package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

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

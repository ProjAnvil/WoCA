package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/runws"
	"github.com/projanvil/woca/internal/tools"
)

// newRunAgent builds a minimal agent rooted at dir with an optional
// per-workspace prompt builder.
func newRunAgent(t *testing.T, dir string, promptFor func(ws, model string) string) *Agent {
	t.Helper()
	a, err := New(Config{
		Model:           language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:        tools.NewRegistry(dir),
		SystemPrompt:    "BASE:" + dir,
		PermissionMode:  config.ModeBypass,
		WorkDir:         dir,
		SystemPromptFor: promptFor,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestNewStateFollowsRunWorkspace(t *testing.T) {
	var builds int32
	a := newRunAgent(t, "/ws/base", func(ws, model string) string {
		atomic.AddInt32(&builds, 1)
		return "PROMPT:" + ws
	})
	hist := []messages.Message{messages.Human("hi")}

	// Without a builder the startup prompt applies to every run.
	plain, err := New(Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry("/ws/base"),
		SystemPrompt:   "BASE:/ws/base",
		PermissionMode: config.ModeBypass,
		WorkDir:        "/ws/base",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sys := MessagesFrom(plain.NewState(context.Background(), hist))[0]; sys.Content != "BASE:/ws/base" {
		t.Fatalf("plain base prompt = %q", sys.Content)
	}
	if sys := MessagesFrom(plain.NewState(runws.With(context.Background(), "/ws/other"), hist))[0]; sys.Content != "BASE:/ws/base" {
		t.Fatalf("plain workspace prompt = %q", sys.Content)
	}

	// With a builder, the base workspace and each run workspace get their
	// own cached build.
	if sys := MessagesFrom(a.NewState(context.Background(), hist))[0]; sys.Content != "PROMPT:/ws/base" {
		t.Fatalf("base prompt = %q", sys.Content)
	}
	ctx := runws.With(context.Background(), "/ws/other")
	if sys := MessagesFrom(a.NewState(ctx, hist))[0]; sys.Content != "PROMPT:/ws/other" {
		t.Fatalf("workspace prompt = %q", sys.Content)
	}
	if sys := MessagesFrom(a.NewState(ctx, hist))[0]; sys.Content != "PROMPT:/ws/other" {
		t.Fatalf("cached workspace prompt = %q", sys.Content)
	}
	if n := atomic.LoadInt32(&builds); n != 2 {
		t.Fatalf("prompt builder ran %d times, want 2 (cached)", n)
	}
}

func TestSetSystemPromptInvalidatesWorkspaceCache(t *testing.T) {
	var builds int32
	a := newRunAgent(t, "/ws/base", func(ws, model string) string {
		atomic.AddInt32(&builds, 1)
		return "PROMPT:" + ws
	})
	ctx := runws.With(context.Background(), "/ws/other")
	hist := []messages.Message{messages.Human("hi")}
	if sys := MessagesFrom(a.NewState(ctx, hist))[0]; sys.Content != "PROMPT:/ws/other" {
		t.Fatalf("workspace prompt = %q", sys.Content)
	}
	n := atomic.LoadInt32(&builds)

	// A runtime swap drops the caches: the next run in each workspace
	// rebuilds through the builder (which reads live settings).
	a.SetSystemPrompt("NEW-BASE")
	if sys := MessagesFrom(a.NewState(context.Background(), hist))[0]; sys.Content != "PROMPT:/ws/base" {
		t.Fatalf("base prompt after swap = %q", sys.Content)
	}
	if sys := MessagesFrom(a.NewState(ctx, hist))[0]; sys.Content != "PROMPT:/ws/other" {
		t.Fatalf("workspace prompt after swap = %q", sys.Content)
	}
	if atomic.LoadInt32(&builds) != n+2 {
		t.Fatalf("builder ran %d times after swap, want %d (both rebuilt)", atomic.LoadInt32(&builds), n+2)
	}
}

func TestSubagentInheritsRunWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	a := newRunAgent(t, base, nil)
	ctx := runws.With(context.Background(), ws)

	sub, err := a.newSubagent(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sub.workdir != ws {
		t.Fatalf("subagent workdir = %q, want %q", sub.workdir, ws)
	}
	if !strings.Contains(sub.SystemPrompt(), ws) {
		t.Fatalf("subagent prompt = %q, want it to name %q", sub.SystemPrompt(), ws)
	}
	if sub.MemoryWorkspace() != ws {
		t.Fatalf("subagent memory workspace = %q, want %q", sub.MemoryWorkspace(), ws)
	}
	// Tools resolve the inherited workspace through the subagent's registry.
	res := sub.registry.Execute(ctx, "Bash", map[string]any{"command": "pwd"})
	if !strings.Contains(res.Content, ws) {
		t.Fatalf("subagent pwd = %q, want it inside %q", res.Content, ws)
	}

	// Without a run workspace the parent's workdir applies (TUI semantics).
	sub, err = a.newSubagent(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if sub.workdir != base {
		t.Fatalf("subagent workdir = %q, want %q", sub.workdir, base)
	}
}

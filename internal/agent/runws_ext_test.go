package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/hooks"
	"github.com/projanvil/woca/internal/runws"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
)

// namedModel wraps a chat model with a stable Name for prompt rebuild tests.
type namedModel struct {
	language.ChatModel
	name string
}

func (m namedModel) Name() string { return m.name }

// newScopedAgent builds an agent with the per-workspace loaders injected.
func newScopedAgent(t *testing.T, dir string, cfgExtras func(*Config)) *Agent {
	t.Helper()
	cfg := Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "BASE",
		PermissionMode: config.ModeBypass,
		WorkDir:        dir,
	}
	if cfgExtras != nil {
		cfgExtras(&cfg)
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSkillsFollowRunWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	var loads int32
	a := newScopedAgent(t, base, func(c *Config) {
		// The base set loads at startup (main injects loadSkills(workdir)
		// into Skills); SkillsFor covers runs in other workspaces.
		c.Skills = []skills.Skill{{Name: "skill-of-" + base, Description: "d", Body: "do " + base}}
		c.SkillsFor = func(dir string) []skills.Skill {
			atomic.AddInt32(&loads, 1)
			return []skills.Skill{{Name: "skill-of-" + dir, Description: "d", Body: "do " + dir}}
		}
	})

	skill, ok := a.toolByName["Skill"]
	if !ok {
		t.Fatal("Skill tool must be registered when SkillsFor is wired")
	}

	// A run in workspace ws resolves and expands that workspace's skill.
	ctx := runws.With(context.Background(), ws)
	res, err := skill.Invoke(ctx, map[string]any{"skill": "skill-of-" + ws})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, ws) {
		t.Fatalf("expanded skill = %q, want it to reference %q", res.Content, ws)
	}

	// The base run keeps its own skill; the other workspace's is unknown.
	if _, err := skill.Invoke(context.Background(), map[string]any{"skill": "skill-of-" + ws}); err == nil {
		t.Fatal("base run must not see the other workspace's skill")
	}
	if _, err := skill.Invoke(context.Background(), map[string]any{"skill": "skill-of-" + base}); err != nil {
		t.Fatalf("base run must keep its startup skill: %v", err)
	}

	// Repeated resolution is cached per workspace.
	skill.Invoke(ctx, map[string]any{"skill": "skill-of-" + ws})
	if n := atomic.LoadInt32(&loads); n != 1 {
		t.Fatalf("SkillsFor called %d times, want 1 (cached afterwards)", n)
	}
}

func TestAgentDefinitionsFollowRunWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	a := newScopedAgent(t, base, func(c *Config) {
		c.Agents = []agents.Definition{{Name: "reviewer-base", Prompt: "review base"}}
		c.AgentsFor = func(dir string) []agents.Definition {
			return []agents.Definition{{Name: "reviewer-" + dir, Prompt: "review " + dir}}
		}
	})
	ctx := runws.With(context.Background(), ws)
	defs := a.agentsFor(ctx)
	if len(defs) != 1 || defs[0].Name != "reviewer-"+ws {
		t.Fatalf("agentsFor(ws) = %+v, want the ws definition", defs)
	}
	if defs := a.agentsFor(context.Background()); len(defs) != 1 || defs[0].Name != "reviewer-base" {
		t.Fatalf("agentsFor(base) = %+v, want the startup set", defs)
	}
}

func TestHooksFollowRunWorkspace(t *testing.T) {
	base := t.TempDir()
	ws := t.TempDir()
	a := newScopedAgent(t, base, func(c *Config) {
		c.HooksFor = func(dir string) *hooks.Runner {
			return hooks.NewRunner(hooks.Config{}, dir, "woca")
		}
	})
	ctx := runws.With(context.Background(), ws)

	// Each workspace resolves its own runner (cwd is the discriminator).
	h1 := a.hooksFor(ctx)
	h2 := a.hooksFor(ctx)
	if h1 != h2 {
		t.Fatal("hooks for one workspace must be cached (same pointer)")
	}
	hb := a.hooksFor(context.Background())
	if h1 == hb {
		t.Fatal("workspace runner must differ from the base runner")
	}

	// The base agent without HooksFor keeps its startup runner.
	plain := newScopedAgent(t, base, nil)
	if plain.hooksFor(ctx) != plain.hooks {
		t.Fatal("without HooksFor the startup runner applies")
	}
}

func TestSystemPromptTracksModelName(t *testing.T) {
	base := t.TempDir()
	a := newScopedAgent(t, base, func(c *Config) {
		c.SystemPromptFor = func(dir, model string) string {
			return "PROMPT:" + model + "@" + dir
		}
	})
	hist := []messages.Message{messages.Human("hi")}

	sys := func() string {
		return MessagesFrom(a.NewState(context.Background(), hist))[0].Content
	}
	if got := sys(); got != "PROMPT:fake@undefined-model@"+base && !strings.HasPrefix(got, "PROMPT:") {
		t.Fatalf("initial prompt = %q", got)
	}

	// Swapping the model rebuilds the prompt on the next run without any
	// explicit invalidation: the cache key includes the model name.
	m1 := namedModel{language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))), "model-one"}
	m2 := namedModel{language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))), "model-two"}
	if err := a.SetModel(m1); err != nil {
		t.Fatal(err)
	}
	if got := sys(); got != "PROMPT:model-one@"+base {
		t.Fatalf("after SetModel(m1) = %q", got)
	}
	if err := a.SetModel(m2); err != nil {
		t.Fatal(err)
	}
	if got := sys(); got != "PROMPT:model-two@"+base {
		t.Fatalf("after SetModel(m2) = %q, want the new model name", got)
	}
}

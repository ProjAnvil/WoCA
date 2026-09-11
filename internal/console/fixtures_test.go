package console

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
	"github.com/projanvil/woca/internal/tools"
)

type fakeEngine struct {
	remembered []string
	forgotten  []string
}

func (f *fakeEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return []memory.Hit{{ID: "h1", Layer: "L2", Type: "fact", Summary: "hit for " + q, Score: 0.9}}, nil
}

func (f *fakeEngine) SearchCode(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, nil
}

func (f *fakeEngine) Remember(ctx context.Context, ws, content string, tags []string, source string) (string, error) {
	if strings.Contains(content, "drop-me") {
		return "", nil // attention gate
	}
	f.remembered = append(f.remembered, content)
	return "id-1", nil
}

func (f *fakeEngine) RecordEvent(ctx context.Context, ws, a, b, c, d string) error { return nil }

func (f *fakeEngine) List(ctx context.Context, ws, layer string, limit, offset int) ([]memory.Hit, error) {
	if strings.Contains(ws, "empty") {
		return nil, nil
	}
	return []memory.Hit{{ID: "h1", Layer: "L2", Type: "fact", Summary: "stored fact"}}, nil
}

func (f *fakeEngine) Forget(ctx context.Context, id string) error {
	f.forgotten = append(f.forgotten, id)
	return nil
}

func (f *fakeEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return &memory.Stats{Total: 3, Layers: map[string]int{"L1": 1, "L2": 2}}, nil
}

func (f *fakeEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "indexed 1 file", nil
}

func (f *fakeEngine) Consolidate(ctx context.Context, ws string, since float64) (string, error) {
	return "consolidated", nil
}

func (f *fakeEngine) Description() string { return "fake engine" }

func (f *fakeEngine) Close() error { return nil }

// newFullConsole wires every optional dep with cheap stubs.

func newFullConsole(t *testing.T) (*Console, *fakeEngine) {
	t.Helper()
	home := t.TempDir()
	acfg := agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
	}
	// memoryCmd resolves the engine through the agent first, so wire the
	// fake there as well as in deps.
	eng := &fakeEngine{}
	acfg.MemoryEngine = eng
	acfg.Obs = obslog.New(filepath.Join(home, "logs"), false)
	a, err := agent.New(acfg)
	if err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(filepath.Join(home, "woca.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	cfg := &config.Config{Model: "test-model", PermissionMode: config.ModeDefault, WorkDir: t.TempDir()}
	c := New(Deps{
		Agent:        a,
		Cfg:          cfg,
		Version:      "test",
		Settings:     st,
		Memory:       store.NewMemory(),
		MemoryEngine: eng,
		SessionDir:   filepath.Join(home, "sessions"),
		AgentsDir:    filepath.Join(home, "agents"),
		OpenBrowser:  func(url string) {},
		ModelFactory: func(name string) (language.ChatModel, error) {
			return language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))), nil
		},
	})
	return c, eng
}

type errEngine struct{ fakeEngine }

func (e *errEngine) Recall(ctx context.Context, q, ws string, k int) ([]memory.Hit, error) {
	return nil, errBoom
}

func (e *errEngine) Remember(ctx context.Context, ws, content string, tags []string, source string) (string, error) {
	return "", errBoom
}

func (e *errEngine) List(ctx context.Context, ws, layer string, limit, offset int) ([]memory.Hit, error) {
	return nil, errBoom
}

func (e *errEngine) Forget(ctx context.Context, id string) error { return errBoom }

func (e *errEngine) Stats(ctx context.Context, ws string) (*memory.Stats, error) {
	return nil, errBoom
}

func (e *errEngine) Consolidate(ctx context.Context, ws string, since float64) (string, error) {
	return "", errBoom
}

func (e *errEngine) IndexCode(ctx context.Context, root, ws string, force bool) (string, error) {
	return "", errBoom
}

var errBoom = boomErr{}

type boomErr struct{}

func (boomErr) Error() string { return "boom" }

func newAgentWithEngine(t *testing.T, eng memory.Engine) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("ok"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		MemoryEngine:   eng,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func writeSessionFile(dir, name string, msgs []messages.Message) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return session.Save(filepath.Join(dir, name+".jsonl"), msgs)
}

func fmt_Sprintf(format string, a ...any) string {
	return fmt.Sprintf(format, a...)
}

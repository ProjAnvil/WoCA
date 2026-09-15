package console

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/obslog"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/webgui"
)

func newObsConsole(t *testing.T) (*Console, *agent.Agent, *obslog.Store) {
	t.Helper()
	obs := obslog.New(filepath.Join(t.TempDir(), "logs"), false)
	model := language.NewFakeChatModel(language.WithResponses(messages.AI("ok")))
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeDefault,
		Obs:            obs,
	})
	if err != nil {
		t.Fatal(err)
	}
	st, err := settings.Open(t.TempDir() + "/s.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	web := webgui.New(server.Config{Logger: zerolog.Nop()})
	cfg := &config.Config{Model: "test-model", PermissionMode: config.ModeDefault, WorkDir: t.TempDir()}
	return New(Deps{Agent: a, Cfg: cfg, Web: web, Settings: st, Version: "test"}), a, obs
}

func TestDebugLogCommand(t *testing.T) {
	c, a, obs := newObsConsole(t)
	sess := &Session{}

	res, err := c.Execute(t.Context(), "debug-log", nil, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "debug log: off") {
		t.Fatalf("status = %q", res.Output)
	}

	res, err = c.Execute(t.Context(), "debug-log", []string{"on"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "debug log: on") {
		t.Fatalf("on output = %q", res.Output)
	}
	if !obs.Enabled() || !c.DebugLogOn() {
		t.Fatal("store not enabled")
	}
	// Persisted: a fresh Load sees it.
	if re := c.deps.Settings.Load(); !re.DebugLog {
		t.Fatal("debug-log not persisted")
	}

	res, err = c.Execute(t.Context(), "debug-log", []string{"off"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if obs.Enabled() {
		t.Fatal("store not disabled")
	}
	if re := a.Obs(); re == nil {
		t.Fatal("obs store lost")
	}

	if _, err := c.Execute(t.Context(), "debug-log", []string{"wibble"}, sess); err == nil {
		t.Fatal("invalid value should error")
	}
}

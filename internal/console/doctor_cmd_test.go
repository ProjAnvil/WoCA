package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/woca/internal/quota"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
)

func TestDoctorAndAgentctl(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := t.Context()
	s := &Session{}

	res, err := c.Execute(ctx, "doctor", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workdir", "tools", "model", "settings db"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("doctor missing %q:\n%s", want, res.Output)
		}
	}
	// A console with a missing workdir shows the failure marker.
	c2, _ := newAgentConsole(t, nil)
	c2.deps.Cfg.WorkDir = filepath.Join(t.TempDir(), "missing-dir")
	res, _ = c2.Execute(ctx, "doctor", nil, s)
	if !strings.Contains(res.Output, "missing") {
		t.Errorf("doctor workdir failure missing:\n%s", res.Output)
	}

	// /quota with a stub closure.
	c3, _ := newAgentConsole(t, nil)
	c3.deps.Quota = func(ctx context.Context) (quota.Info, error) {
		return quota.Info{Available: true, Type: quota.TypeBalance, Balance: "CNY 1.00"}, nil
	}
	res, err = c3.Execute(ctx, "quota", nil, s)
	if err != nil || !strings.Contains(res.Output, "账户余额") {
		t.Fatalf("quota = %q %v", res.Output, err)
	}
	// Without a closure: unavailable message, no crash.
	c4, _ := newAgentConsole(t, nil)
	res4, err4 := c4.Execute(ctx, "quota", nil, s)
	if err4 != nil {
		t.Fatalf("bare quota errored: %v", err4)
	}
	_ = res4

	// /debug-log toggling with a settings store.
	res, err = c.Execute(ctx, "debug-log", []string{"on"}, s)
	if err != nil {
		t.Fatalf("debug-log on = %+v %v", res, err)
	}
	res, err = c.Execute(ctx, "debug-log", []string{"off"}, s)
	if err != nil {
		t.Fatalf("debug-log off = %+v %v", res, err)
	}
	if _, err := c.Execute(ctx, "debug-log", []string{"bogus"}, s); err == nil {
		t.Fatal("expected debug-log usage error")
	}
	// Without settings: error.
	if _, err := c4.Execute(ctx, "debug-log", []string{"on"}, s); err == nil {
		t.Fatal("expected debug-log disabled error")
	}
}

func TestDoctorFailureModes(t *testing.T) {
	ctx := t.Context()
	s := &Session{}
	// Everything failing: no tools, no model, missing workdir, no settings,
	// no skills, no MCP.
	c, _ := newAgentConsole(t, nil)
	c.deps.Cfg.Model = ""
	c.deps.Cfg.WorkDir = filepath.Join(t.TempDir(), "gone")
	res, err := c.Execute(ctx, "doctor", nil, s)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"✗", "⚠"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("doctor missing failure marker %q:\n%s", want, res.Output)
		}
	}
}

func TestDoctorFullWiring(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ctx := t.Context()

	c, _ := newFullConsole(t)
	c.deps.Memory = store.NewMemory()
	// Credentials via the settings store.
	if err := c.deps.Settings.SetSecret(settings.SecretOpenAIAPIKey, "sk-doc"); err != nil {
		t.Fatal(err)
	}
	res, err := c.Execute(ctx, "doctor", nil, &Session{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"API key in settings db", "session storage", "memory store", "hooks", "permissions"} {
		if !strings.Contains(res.Output, want) {
			t.Errorf("doctor missing %q:\n%s", want, res.Output)
		}
	}
	// Credential on the config wins.
	c.deps.Cfg.APIKey = "flag-key"
	res, _ = c.Execute(ctx, "doctor", nil, &Session{})
	if !strings.Contains(res.Output, "API key on config") {
		t.Errorf("config credential missing:\n%s", res.Output)
	}
	// OAuth fallback with neither key.
	c.deps.Cfg.APIKey = ""
	c.deps.Settings = nil
	if err := os.MkdirAll(filepath.Join(home, ".woca"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".woca", "credentials.json"), []byte(`{"access_token":"t"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Execute(ctx, "doctor", nil, &Session{})
	if !strings.Contains(res.Output, "OAuth token") {
		t.Errorf("oauth fallback missing:\n%s", res.Output)
	}
	// Without any credential: warning.
	if err := os.Remove(filepath.Join(home, ".woca", "credentials.json")); err != nil {
		t.Fatal(err)
	}
	res, _ = c.Execute(ctx, "doctor", nil, &Session{})
	if !strings.Contains(res.Output, "none found") {
		t.Errorf("no-credential warning missing:\n%s", res.Output)
	}
}

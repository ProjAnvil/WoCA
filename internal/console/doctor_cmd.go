package console

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/settings"
)

// doctorCmd implements /doctor (a port of Claude Code's self-check command):
// it probes the pieces a session depends on — working directory, tool
// registry, settings database, credentials, session storage, memory store,
// hooks, permission rules — and reports ok / warn / fail per check.
func (c *Console) doctorCmd() Result {
	type check struct {
		name   string
		status string // ok / warn / fail marker
		detail string
	}
	var checks []check
	add := func(name, status, detail string) {
		checks = append(checks, check{name, status, detail})
	}

	// Working directory.
	d := c.deps
	if st, err := os.Stat(d.Cfg.WorkDir); err == nil && st.IsDir() {
		add("workdir", "✔", d.Cfg.WorkDir)
	} else {
		add("workdir", "✗", fmt.Sprintf("%s missing (%v)", d.Cfg.WorkDir, err))
	}

	// Tool registry.
	if names := d.Agent.ToolNames(); len(names) > 0 {
		add("tools", "✔", fmt.Sprintf("%d registered", len(names)))
	} else {
		add("tools", "✗", "no tools registered")
	}

	// Model/provider.
	if d.Cfg.Model != "" {
		add("model", "✔", fmt.Sprintf("%s (%s)", d.Cfg.Model, d.Cfg.Provider))
	} else {
		add("model", "✗", "no model configured")
	}

	// Settings database.
	if d.Settings != nil {
		_ = d.Settings.Load() // a failed read still reports the path
		add("settings db", "✔", d.Settings.Path())
	} else {
		add("settings db", "⚠", "disabled (--db none); settings/keys are not persisted")
	}

	// Credentials: flag/env key on the config, an encrypted secret in the
	// store, or the saved OAuth token.
	cred := "none found — set an API key via /settings or `woca login`"
	status := "⚠"
	if d.Cfg.APIKey != "" {
		cred, status = "API key on config", "✔"
	} else if d.Settings != nil {
		if d.Settings.HasSecret(settings.SecretAnthropicAPIKey) || d.Settings.HasSecret(settings.SecretOpenAIAPIKey) {
			cred, status = "API key in settings db (encrypted)", "✔"
		}
	}
	if status == "⚠" && oauthTokenPresent() {
		cred, status = "OAuth token (~/.woca/credentials.json)", "✔"
	}
	add("credentials", status, cred)

	// Session storage: the sessions dir must be writable.
	dir := c.deps.SessionDir
	if dir == "" {
		dir = filepath.Join(session.DefaultDir(), "sessions")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		add("session storage", "✗", fmt.Sprintf("%s: %v", dir, err))
	} else if probe := filepath.Join(dir, ".doctor-probe"); os.WriteFile(probe, nil, 0o644) == nil {
		_ = os.Remove(probe)
		add("session storage", "✔", dir+" writable")
	} else {
		add("session storage", "✗", dir+" not writable")
	}

	// Memory store.
	if d.Memory != nil {
		add("memory store", "✔", "configured")
	} else {
		add("memory store", "⚠", "not configured (/memory disabled)")
	}

	// Hooks.
	if hooks := d.Agent.HookSummary(); len(hooks) > 0 {
		add("hooks", "✔", strings.Join(hooks, " "))
	} else {
		add("hooks", "✔", "none configured")
	}

	// Permission rules.
	allow, deny, ask := d.Agent.PermissionRuleCounts()
	detail := fmt.Sprintf("mode=%s rules allow=%d deny=%d ask=%d", d.Agent.Mode(), allow, deny, ask)
	add("permissions", "✔", detail)

	var b strings.Builder
	b.WriteString("woca doctor:\n")
	warns, fails := 0, 0
	for _, ck := range checks {
		switch ck.status {
		case "⚠":
			warns++
		case "✗":
			fails++
		}
		fmt.Fprintf(&b, "  %s %-15s %s\n", ck.status, ck.name+":", ck.detail)
	}
	fmt.Fprintf(&b, "%d warning(s), %d failure(s)\n", warns, fails)
	return Result{Output: b.String()}
}

// oauthTokenPresent reports whether a saved OAuth token file exists.
func oauthTokenPresent() bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(home, ".woca", "credentials.json"))
	return err == nil
}

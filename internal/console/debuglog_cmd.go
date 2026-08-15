package console

import (
	"fmt"

	"github.com/projanvil/woca/internal/settings"
)

// debugLogCmd implements /debug-log: toggles the harness observability log
// (graph/state/session/message traces under ~/.woca/logs, rotated). The
// setting persists to the settings database when available and applies to the
// live store immediately — no restart needed.
func (c *Console) debugLogCmd(args []string) (Result, error) {
	obs := c.deps.Agent.Obs()
	if obs == nil {
		return Result{}, fmt.Errorf("observability log is not wired in this session")
	}

	status := func() string {
		state := "off"
		if obs.Enabled() {
			state = "on"
		}
		root := obs.Root()
		if root == "" {
			root = "(disabled)"
		}
		return fmt.Sprintf("debug log: %s · logs under %s (rotated by size)\n", state, root)
	}

	if len(args) == 0 {
		return Result{Output: status() + "usage: /debug-log <on|off>\n"}, nil
	}
	v, err := settings.ParseBool(args[0])
	if err != nil {
		return Result{}, fmt.Errorf("usage: /debug-log <on|off>")
	}

	// Persist first (so the toggle survives restarts), then apply live.
	if c.deps.Settings != nil {
		cfg := c.deps.Settings.Load()
		cfg.DebugLog = v
		if err := c.deps.Settings.Save(cfg); err != nil {
			return Result{}, err
		}
	}
	obs.SetEnabled(v)
	out := status()
	if v {
		out += "recording graph node dispatches, session traces, and per-run message dumps.\n"
	} else {
		out += "recording stopped (existing log files are kept).\n"
	}
	return Result{Output: out}, nil
}

// DebugLogOn reports the live observability state for status displays.
func (c *Console) DebugLogOn() bool {
	obs := c.deps.Agent.Obs()
	return obs != nil && obs.Enabled()
}

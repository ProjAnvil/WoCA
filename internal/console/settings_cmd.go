package console

import (
	"context"
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/settings"
)

// settingsCmd implements /settings: no args shows the persisted settings
// (secrets masked), `/settings set <key> <value>` updates one, and
// `/settings help` lists keys.
func (c *Console) settingsCmd(args []string) (Result, error) {
	st := c.deps.Settings
	if st == nil {
		return Result{}, fmt.Errorf("settings database is disabled in this session")
	}
	if len(args) == 0 {
		return Result{Output: settings.Describe(st.Load(), st.HasSecret, st.Path()) +
			"use /settings set <key> <value> to update; /settings help lists keys\n"}, nil
	}
	switch args[0] {
	case "help":
		var b strings.Builder
		b.WriteString("settings keys:\n")
		for _, k := range settings.KeyNames() {
			fmt.Fprintf(&b, "  %-20s %s\n", k, settings.KeyHelp(k))
		}
		return Result{Output: b.String()}, nil
	case "set":
		if len(args) < 3 {
			return Result{}, fmt.Errorf("usage: /settings set <key> <value>")
		}
		key, value := args[1], strings.Join(args[2:], " ")
		return c.settingsSet(st, key, value)
	case "get":
		if len(args) != 2 {
			return Result{}, fmt.Errorf("usage: /settings get <key>")
		}
		key := args[1]
		if name, ok := settings.SecretFor(key); ok {
			if st.HasSecret(name) {
				return Result{Output: key + ": (set, encrypted)\n"}, nil
			}
			return Result{Output: key + ": (not set)\n"}, nil
		}
		return Result{Output: key + ": " + settings.KeyValue(st.Load(), key) + "\n"}, nil
	default:
		return Result{}, fmt.Errorf("unknown /settings subcommand %q (use set/get/help)", args[0])
	}
}

// settingsSet applies one update, immediately rebuilding the chat model when
// provider credentials changed so the next turn uses them.
func (c *Console) settingsSet(st *settings.Store, key, value string) (Result, error) {
	if name, ok := settings.SecretFor(key); ok {
		if err := st.SetSecret(name, value); err != nil {
			return Result{}, err
		}
		if err := c.applyModelChange(key); err != nil {
			return Result{Output: fmt.Sprintf("%s saved (encrypted); model rebuild failed: %v\n", key, err)}, nil
		}
		return Result{Output: fmt.Sprintf("%s saved (encrypted); takes effect immediately\n", key)}, nil
	}
	cfg := st.Load()
	if err := settings.Apply(&cfg, key, value); err != nil {
		return Result{}, err
	}
	if err := st.Save(cfg); err != nil {
		return Result{}, err
	}
	switch key {
	case settings.KeyProvider, settings.KeyAnthropicBaseURL, settings.KeyOpenAIBaseURL,
		settings.KeyMaxTokens, settings.KeyTemperature:
		if err := c.applyModelChange(key); err != nil {
			return Result{Output: fmt.Sprintf("%s saved; model rebuild failed: %v\n", key, err)}, nil
		}
		return Result{Output: fmt.Sprintf("%s saved; takes effect immediately\n", key)}, nil
	case settings.KeyCheckpoint, settings.KeyMemory, settings.KeyCheckpointDir, settings.KeyPostgresURL:
		return Result{Output: fmt.Sprintf("%s saved; restart woca to apply the new backend\n", key)}, nil
	}
	return Result{Output: fmt.Sprintf("%s saved\n", key)}, nil
}

// applyModelChange rebuilds the model when the changed key affects it.
func (c *Console) applyModelChange(key string) error {
	if c.deps.RebuildModel == nil {
		return nil
	}
	return c.deps.RebuildModel()
}

// memoryCmd implements /memory over the configured memoryStore:
//
//	/memory                       list all entries
//	/memory list [namespace]      list one namespace
//	/memory get <ns> <key>        print one value
//	/memory put <ns> <key> <v>    store a value
//	/memory del <ns> <key>        delete an entry
func (c *Console) memoryCmd(ctx context.Context, args []string) (Result, error) {
	m := c.deps.Memory
	if m == nil {
		return Result{}, fmt.Errorf("memoryStore is disabled (enable via /settings set memory sqlite|memory|postgres and restart)")
	}
	if len(args) == 0 {
		args = []string{"list"}
	}
	switch args[0] {
	case "list":
		ns := ""
		if len(args) > 1 {
			ns = args[1]
		}
		entries, err := m.List(ctx, ns, "")
		if err != nil {
			return Result{}, err
		}
		if len(entries) == 0 {
			return Result{Output: fmt.Sprintf("memoryStore (%s) is empty\n", m.Backend())}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "memoryStore backend=%s path=%s\n", m.Backend(), m.Path())
		for _, e := range entries {
			val := string(e.Value)
			if len(val) > 80 {
				val = val[:80] + "…"
			}
			fmt.Fprintf(&b, "  %s/%s = %s\n", e.Namespace, e.Key, val)
		}
		return Result{Output: b.String()}, nil
	case "get":
		if len(args) != 3 {
			return Result{}, fmt.Errorf("usage: /memory get <namespace> <key>")
		}
		val, ok, err := m.Get(ctx, args[1], args[2])
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{Output: "not found\n"}, nil
		}
		return Result{Output: string(val) + "\n"}, nil
	case "put":
		if len(args) < 4 {
			return Result{}, fmt.Errorf("usage: /memory put <namespace> <key> <value>")
		}
		if err := m.Put(ctx, args[1], args[2], strings.Join(args[3:], " ")); err != nil {
			return Result{}, err
		}
		return Result{Output: "stored\n"}, nil
	case "del", "delete", "rm":
		if len(args) != 3 {
			return Result{}, fmt.Errorf("usage: /memory del <namespace> <key>")
		}
		if err := m.Delete(ctx, args[1], args[2]); err != nil {
			return Result{}, err
		}
		return Result{Output: "deleted\n"}, nil
	default:
		return Result{}, fmt.Errorf("unknown /memory subcommand %q (use list/get/put/del)", args[0])
	}
}

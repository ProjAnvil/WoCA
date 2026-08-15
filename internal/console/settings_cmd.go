package console

import (
	"context"
	"fmt"
	"strings"

	"github.com/projanvil/woca/internal/memory"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/store"
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
		if name == settings.SecretLadyMEmbeddingAPIKey {
			// The embedding key only feeds the memory engine; rebuild that,
			// not the chat model.
			if err := c.applyMemoryChange(key); err != nil {
				return Result{Output: fmt.Sprintf("%s saved (encrypted); memory engine rebuild failed: %v\n", key, err)}, nil
			}
			return Result{Output: fmt.Sprintf("%s saved (encrypted); takes effect immediately\n", key)}, nil
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
	case settings.KeyMemoryEngine, settings.KeyLadyMDBPath, settings.KeyLadyMEmbedding,
		settings.KeyLadyMEmbeddingModel, settings.KeyLadyMEmbeddingBaseURL,
		settings.KeyLadyMLLM:
		if err := c.applyMemoryChange(key); err != nil {
			return Result{Output: fmt.Sprintf("%s saved; memory engine rebuild failed: %v\n", key, err)}, nil
		}
		return Result{Output: fmt.Sprintf("%s saved; takes effect immediately\n", key)}, nil
	case settings.KeyLadyMAutoRetain, settings.KeyLadyMTopK,
		settings.KeyLadyMSystem2, settings.KeyLadyMSystem2Interval:
		return Result{Output: fmt.Sprintf("%s saved; takes effect next run\n", key)}, nil
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

// applyMemoryChange rebuilds the memory engine when the changed key affects it.
func (c *Console) applyMemoryChange(key string) error {
	if c.deps.RebuildMemory == nil {
		return nil
	}
	return c.deps.RebuildMemory()
}

// memoryCmd implements /memory. With the memory engine wired, the engine
// surface is primary; the legacy KV memoryStore lives under `kv`:
//
//	/memory                       recent engine memories (workspace-scoped)
//	/memory recall <query...>     semantic recall
//	/memory remember <text...>    store a fact (attention-gated)
//	/memory stats                 layer counts + db path
//	/memory forget <id>           delete one memory
//	/memory consolidate           run one System 2 cycle now
//	/memory index [path] [--force] index source into memory
//	/memory kv list|get|put|del … legacy KV memoryStore
func (c *Console) memoryCmd(ctx context.Context, args []string) (Result, error) {
	if eng := c.deps.MemoryEngine; eng != nil {
		return c.memoryEngineCmd(ctx, eng, args)
	}
	m := c.deps.Memory
	if m == nil {
		return Result{}, fmt.Errorf("memory is disabled (/settings set memory-engine ladym)")
	}
	if len(args) > 0 && args[0] == "kv" {
		args = args[1:]
	}
	return c.memoryKVCmd(ctx, m, args)
}

// memoryEngineCmd implements /memory over the factual-memory engine.
func (c *Console) memoryEngineCmd(ctx context.Context, eng memory.Engine, args []string) (Result, error) {
	if len(args) == 0 {
		args = []string{"list"}
	}
	ws := ""
	if c.deps.MemoryWorkspace != nil {
		ws = c.deps.MemoryWorkspace()
	}
	switch args[0] {
	case "list":
		hits, err := eng.List(ctx, ws, "", 20, 0)
		if err != nil {
			return Result{}, err
		}
		if len(hits) == 0 {
			return Result{Output: "no memories yet — they accumulate as you work\n"}, nil
		}
		var b strings.Builder
		fmt.Fprintf(&b, "memory engine: %s (workspace %s)\n", eng.Description(), ws)
		for _, h := range hits {
			line := h.Summary
			if len(line) > 80 {
				line = line[:80] + "…"
			}
			fmt.Fprintf(&b, "  [%s|%s] %s\n", h.Layer, h.Type, line)
		}
		return Result{Output: b.String()}, nil
	case "recall":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /memory recall <query...>")
		}
		hits, err := eng.Recall(ctx, strings.Join(args[1:], " "), ws, 8)
		if err != nil {
			return Result{}, err
		}
		if len(hits) == 0 {
			return Result{Output: "(no hits)\n"}, nil
		}
		var b strings.Builder
		for _, h := range hits {
			fmt.Fprintf(&b, "[%.2f|%s|%s] %s\n", h.Score, h.Layer, h.Type, h.Summary)
		}
		return Result{Output: b.String()}, nil
	case "remember":
		if len(args) < 2 {
			return Result{}, fmt.Errorf("usage: /memory remember <content...>")
		}
		id, err := eng.Remember(ctx, ws, strings.Join(args[1:], " "), []string{"manual"}, "woca:cmd")
		if err != nil {
			return Result{}, err
		}
		if id == "" {
			return Result{Output: "dropped by the attention gate (duplicate or noise)\n"}, nil
		}
		return Result{Output: fmt.Sprintf("stored id=%s\n", id)}, nil
	case "stats":
		st, err := eng.Stats(ctx, ws)
		if err != nil {
			return Result{}, err
		}
		var b strings.Builder
		fmt.Fprintf(&b, "engine: %s\nworkspace: %s\ntotal: %d\n", eng.Description(), ws, st.Total)
		for layer, n := range st.Layers {
			fmt.Fprintf(&b, "  %s: %d\n", layer, n)
		}
		return Result{Output: b.String()}, nil
	case "forget":
		if len(args) != 2 {
			return Result{}, fmt.Errorf("usage: /memory forget <id>")
		}
		if err := eng.Forget(ctx, args[1]); err != nil {
			return Result{}, err
		}
		return Result{Output: "forgotten\n"}, nil
	case "consolidate":
		report, err := eng.Consolidate(ctx, ws)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: report + "\n"}, nil
	case "index":
		root := ""
		force := false
		for _, a := range args[1:] {
			if a == "--force" || a == "-f" {
				force = true
			} else if root == "" {
				root = a
			}
		}
		if root == "" && c.deps.MemoryWorkspace != nil {
			root = c.deps.MemoryWorkspace()
		}
		report, err := eng.IndexCode(ctx, root, ws, force)
		if err != nil {
			return Result{}, err
		}
		return Result{Output: report + "\n"}, nil
	case "kv":
		if c.deps.Memory == nil {
			return Result{}, fmt.Errorf("memoryStore is disabled")
		}
		return c.memoryKVCmd(ctx, c.deps.Memory, args[1:])
	default:
		return Result{}, fmt.Errorf("unknown /memory subcommand %q (use list/recall/remember/stats/forget/consolidate/index/kv)", args[0])
	}
}

// memoryKVCmd implements the legacy KV memoryStore surface (/memory kv …).
func (c *Console) memoryKVCmd(ctx context.Context, m *store.Store, args []string) (Result, error) {
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

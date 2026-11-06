# Plugin contribution surface — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A plugin directory can contribute hooks, MCP servers and slash commands (not just skills/agents), Claude Code plugin layouts install as-is, and plugins can be disabled and installed from a marketplace manifest.

**Architecture:** The manifest loader learns both woca and `.claude-plugin` spellings; server-entry parsing moves from `claudeenv` into `internal/mcp` so plugins and the Claude importer share it; contributions merge through the existing layered-merge plumbing with a new `LevelPlugin` below user level.

**Tech Stack:** Go 1.26, encoding/json manifest decoding, existing `internal/merge`, `internal/hooks`, `internal/mcp`.

**Spec:** `docs/superpowers/specs/2026-11-07-background-loop-and-plugin-surface-design.md` (§5.6)

## Global Constraints

- All comments/commit messages in English.
- Path substitution: `${CLAUDE_PLUGIN_ROOT}` and `${WOCA_PLUGIN_ROOT}` → plugin install dir at LOAD time; `${CLAUDE_PROJECT_DIR}` and `${WOCA_PROJECT_DIR}` → Runner cwd at RUN time. Nothing else is substituted.
- Unsupported Claude/Codex manifest keys are never silent: they land in `Plugin.Ignored` and `/plugins` warns.
- `hooks.Load` keeps accepting woca's shape; Claude's `timeout` (seconds) is an input alias only (`timeout_ms` wins when both set).
- Marketplace github sources construct `https://github.com/<repo>.git`; no network in unit tests beyond existing `plugins.Install` git behavior.

## Review Focus

1. Claude plugin with `hooks` given as an inline JSON object AND as a string path — both must produce runnable hook entries. Test: Task 3.
2. `${CLAUDE_PLUGIN_ROOT}` inside a hook command and inside an MCP server's `env` value — substituted with the install dir, not left literal. Test: Tasks 4–5.
3. Plugin MCP server name colliding with `~/.woca/mcp.json` — the user layer must win (layering below user). Test: Task 7.
4. `disabled-plugins` naming a plugin whose skills/agents were previously merged — filtered at load, restart stated in `/plugins`. Test: Task 6.
5. Hook `timeout` in seconds vs `timeout_ms` — `timeout_ms` wins; seconds converted ×1000. Test: Task 2.

---

### Task 1: Extract MCP server-entry parsing into `internal/mcp`

**Files:**
- Create: `internal/mcp/spec.go`
- Modify: `internal/claudeenv/claudeenv.go` (delegate), `internal/claudeenv/claudeenv_test.go` only if imports change
- Test: `internal/mcp/spec_test.go` (new; move/extend the parse cases from claudeenv tests, which must stay green)

**Interfaces:**
- Produces:
  - `type ServerEntry struct { Name, Type, URL, Command string; HasEnv bool; Args []string; Env map[string]string; Inline string }` (fields exported; claudeenv's private fields move here)
  - `func ParseServerEntry(name string, raw json.RawMessage) (ServerEntry, error)` (same shapes: `type http|sse`+`url`; `type stdio`|omitted+`command`/`args`/`env`)
  - `func (s ServerEntry) Spec() string` — `name=url` or `name=K=V … command args` (env sorted, values shell-word-quoted; args shell-quoted — current `Spec()` behavior, unchanged)
  - helpers `sortedKeys`, `shellQuote`, `shellWord` move with it.

- [ ] **Step 1: Write failing tests** in `spec_test.go` covering: http entry → `name=url`; stdio with args+env → spec with `K=V` prefix and quoted args; unsupported shape → error (copy the exact expected strings from `claudeenv` tests).
- [ ] **Step 2: Run** `go test ./internal/mcp/ -run Spec -v` → FAIL.
- [ ] **Step 3: Implement** by moving the code; `claudeenv.MCPServer` keeps its JSON shape but wraps `mcp.ServerEntry` (or aliases it) so `Servers()`/`Spec()` behave identically.
- [ ] **Step 4: Run** `go test ./internal/mcp/ ./internal/claudeenv/ -v` → PASS.
- [ ] **Step 5: Commit** `refactor(mcp): extract server-entry parsing shared by claudeenv and plugins`

### Task 2: hooks — `timeout` alias and project-dir substitution

**Files:**
- Modify: `internal/hooks/hooks.go` (`Hook` struct, `Load`, `Runner.run`)
- Test: `internal/hooks/hooks_test.go` (extend)

**Interfaces:**
- Produces: `Hook.TimeoutSec int json:"timeout,omitempty"` (normalized to `TimeoutMS = TimeoutSec*1000` in `Load` when `TimeoutMS == 0`); `Runner.run` replaces `${CLAUDE_PROJECT_DIR}`/`${WOCA_PROJECT_DIR}` with `r.cwd` in the command string before executing.

- [ ] **Step 1: Write failing tests:** settings JSON with `"timeout": 2` yields `TimeoutMS 2000`; with both keys, `timeout_ms` wins. A hook command `cat ${CLAUDE_PROJECT_DIR}/flag.txt` run with cwd=tmpdir reads that file (CommandRunner stub or real local executor).
- [ ] **Step 2: Run** `go test ./internal/hooks/ -run 'Timeout|ProjectDir' -v` → FAIL.
- [ ] **Step 3: Implement** normalization in `Load` and substitution in `runMatching` before `r.run`.
- [ ] **Step 4: Run** `go test ./internal/hooks/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(hooks): Claude timeout alias and project-dir substitution`

### Task 3: `merge.LevelPlugin`

**Files:**
- Modify: `internal/merge/merge.go`
- Test: `internal/merge/merge_test.go` (extend)

**Interfaces:**
- Produces: `LevelPlugin Level = 1` with comment (slot between Managed -1 and User 2; 0 unused), `String()` → `"plugin"`.

- [ ] **Step 1: Write failing test:** entries `[plugin(x, LevelPlugin), user(x, LevelUser)]` → user wins; `[plugin(x), managed(x)]` → plugin wins; `String()` case.
- [ ] **Step 2: Run** `go test ./internal/merge/ -v` → FAIL then implement → PASS.
- [ ] **Step 3: Commit** `feat(merge): plugin layer between managed and user`

### Task 4: `skills.Parse` export for command markdown

**Files:**
- Modify: `internal/skills/skills.go`
- Test: `internal/skills/skills_test.go` (extend)

**Interfaces:**
- Produces: `func Parse(defaultName string, data []byte) Skill` — thin export of `parseSkill` (frontmatter `name`/`description`/`args`; unknown keys ignored).

- [ ] **Step 1: Write failing test:** `Parse("myplugin:deploy", []byte("---\ndescription: Ship it\n---\nBody $ARGUMENTS"))` → Skill{Name from defaultName, Description "Ship it", Body containing `$ARGUMENTS`}.
- [ ] **Step 2: Run** → FAIL, implement (rename + keep internal callers) → PASS.
- [ ] **Step 3: Commit** `feat(skills): export Parse for plugin command markdown`

### Task 5: Plugin manifest expansion — contributions, layouts, substitution

**Files:**
- Modify: `internal/plugins/plugins.go` (+ new `internal/plugins/manifest.go` if cleaner)
- Test: `internal/plugins/plugins_test.go` (extend), `internal/plugins/claude_layout_test.go` (new)

**Interfaces:**
- Produces:
  - `type Plugin struct { Manifest Manifest; Path string; Skills []skills.Skill; Agents []agents.Definition; Commands []skills.Skill; Hooks hooks.Config; MCPSpecs []string; Ignored []string }`
  - Manifest decoding accepts: `plugin.json` OR `.claude-plugin/plugin.json` (first wins; name defaults to dir base); fields `skills_dir|skills`, `agents_dir|agents`, `commands_dir|commands` (string or []string), `hooks` (path string or inline object), `mcp_servers|mcpServers` (path string or inline object) — decode polymorphic fields as `json.RawMessage`/`any` and normalize.
  - Command files `commands/*.md` → `skills.Skill` via `skills.Parse("<plugin>:<stem>", data)` then rewrite `$ARGUMENTS`→`{arguments}`, `$1`..`$9`→`{arg1}`..`{arg9}` in Body and declare matching `Args`.
  - Hooks file: `{"hooks": {…}}` via `hooks.Load`-equivalent on bytes (add `hooks.Parse(data []byte) (Config, error)`; `Load` delegates).
  - `mcpServers` object: each entry → `mcp.ParseServerEntry` → `Spec()`.
  - Substitution `substRoot(s string)` replaces `${CLAUDE_PLUGIN_ROOT}`/`${WOCA_PLUGIN_ROOT}` with `Path` in: hook commands, MCP specs.
  - `Ignored` collects Claude/Codex keys present but unsupported: `lspServers`, `outputStyles`, `workflows`, `types`, `experimental`, `apps`, `appTemplates`, `scheduledTasks`, `user_config`.

- [ ] **Step 1: Write failing tests:** (a) woca manifest with `hooks: "hooks/hooks.json"`, `.mcp.json` path, `commands_dir` → loaded Plugin has all three; hook command contains substituted root; command skill named `p1:deploy` with `{arguments}`. (b) `.claude-plugin/plugin.json` layout with inline `hooks` object + inline `mcpServers` + `commands/` dir → same result; `lspServers` present → `Ignored: ["lspServers"]`. (c) manifest without any new fields → behaves exactly as today.
- [ ] **Step 2: Run** `go test ./internal/plugins/ -v` → FAIL.
- [ ] **Step 3: Implement** per interfaces; keep `Load`/`LoadPlugin`/`Install` signatures stable.
- [ ] **Step 4: Run** `go test ./internal/plugins/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(plugins): hooks, MCP and command contributions with Claude layout compat`

### Task 6: Marketplace install, disabled filtering, settings key

**Files:**
- Modify: `internal/plugins/plugins.go` (`Install`, new `resolveMarketplace`), `internal/settings/keys.go`, `internal/settings/settings.go`
- Test: `internal/plugins/marketplace_test.go` (new), settings tests

**Interfaces:**
- Produces:
  - `plugins.Install(src, root)` gains the `"<name>@<source>"` form: clone/copy source → read `.claude-plugin/marketplace.json` `{plugins: [{name, source}]}` → match `name` → resolve: string starting with `./`or`../`or plain relative → subpath of the marketplace copy; `{"source":"github","repo":"owner/repo"}` → `https://github.com/<repo>.git`; plain git URL/path (no `@name@` form) unchanged.
  - `func LoadFiltered(root string, disabled []string) ([]Plugin, []error)`; `Load(root)` = `LoadFiltered(root, nil)`; a disabled plugin is skipped entirely.
  - settings: `KeyDisabledPlugins = "disabled-plugins"`, `Settings.DisabledPlugins []string json:"disabled_plugins"` (name-list pattern of `BuiltinSkills`: all four explicit lists + parse/format).

- [ ] **Step 1: Write failing tests:** local marketplace fixture dir with `.claude-plugin/marketplace.json` listing `toolkit` with relative `source: "./plugins/toolkit"` → `Install("toolkit@<fixture-path>", root)` copies the subplugin and it loads with its skills; `LoadFiltered` with `disabled: ["toolkit"]` skips it; settings round-trip.
- [ ] **Step 2: Run** `go test ./internal/plugins/ ./internal/settings/ -run 'Market|Filtered|Disabled' -v` → FAIL.
- [ ] **Step 3: Implement**; reuse the existing clone helper `Install` uses for git sources.
- [ ] **Step 4: Run** `go test ./internal/plugins/ ./internal/settings/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(plugins): marketplace install and disabled-plugins setting`

### Task 7: Layer wiring in agentbuild

**Files:**
- Modify: `internal/agentbuild/layers.go`, `internal/agentbuild/build.go:140,560` (`pluginListOrEmpty`, `ApplyClaudeEnv`)
- Test: `internal/agentbuild/layers_test.go` (extend)

**Interfaces:**
- Consumes: Tasks 3, 5, 6.
- Produces:
  - `loadSkills(workdir string, builtinAllow []string, pluginList []plugins.Plugin, log zerolog.Logger)` — plugin skills become `merge.Entry{Level: LevelPlugin, Source: "plugin/<name>"}`; `build.go:560` post-merge append removed; `ApplyClaudeEnv` uses the same path.
  - `loadHooksConfig(workdir string, pluginList []plugins.Plugin)` — plugin entries appended after project entries with `Source: "plugin/<name>"` (add `Entry.Source string json:"-"` in hooks for display).
  - `mergeMCPSpecsLayered(flag, workspace, user, plugin, managed …)` — plugin layer below user; dedupe by name keeps higher layer.
  - `pluginListOrEmpty()` → `LoadFiltered(plugins.DefaultRoot(), settings.DisabledPlugins)`; agents keep append behavior with `Source` tagged.
  - Plugin `Commands` append to the skill list after merge (they are namespaced, no conflicts).

- [ ] **Step 1: Write failing tests:** plugin fixture (from Task 5) + user skill with the same name → user wins, conflict logged with "plugin" layer name; plugin hook entry runs after project entries for the same event; plugin MCP name colliding with user `mcp.json` → user spec survives in the merged list; disabled plugin contributes nothing.
- [ ] **Step 2: Run** `go test ./internal/agentbuild/ -v` → FAIL.
- [ ] **Step 3: Implement** per interfaces; keep flag > workspace > user > plugin > managed > builtin ordering.
- [ ] **Step 4: Run** `go test ./internal/agentbuild/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(agentbuild): plugin layer for skills, hooks and MCP specs`

### Task 8: `/plugins` surface and README

**Files:**
- Modify: `internal/console/plugins_cmd.go`, `README.md` (Extending woca → Capability/managed plugins section)
- Test: `internal/console` existing command tests pattern (new case in an existing *_test.go)

**Interfaces:**
- Produces: `/plugins list` shows per-plugin counts `skills/agents/commands/hooks/MCP` and `⚠ ignored: lspServers` lines; `/plugins enable|disable <name>` writes the settings key via `c.deps.Settings`; `/plugins install` output notes that hooks/MCP load on restart.

- [ ] **Step 1: Write failing test:** with a fixture plugin root, `pluginsCmd(["list"])` output contains the counts and ignore warning; `pluginsCmd(["disable","p1"])` persists `disabled-plugins` containing `p1` (assert via the settings store).
- [ ] **Step 2: Run** `go test ./internal/console/ -run Plugins -v` → FAIL.
- [ ] **Step 3: Implement**; README gains the contribution table (woca + Claude spellings), the `${CLAUDE_PLUGIN_ROOT}` note, and `name@marketplace` install syntax.
- [ ] **Step 4: Run** `go test ./internal/console/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(console): plugin enable/disable and contribution reporting`

### Task 9: Full gates

- [ ] **Step 1: Run** `make test`, `make race`, `make lint` → green; `go build ./...`.
- [ ] **Step 2: English-only sweep** of all touched files.
- [ ] **Step 3: Commit** `docs: plugin contribution surface notes`

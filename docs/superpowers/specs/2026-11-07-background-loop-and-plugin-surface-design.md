# Background-task loop closure + plugin contribution surface — design

Date: 2026-11-07
Status: approved for implementation (scope = 方案一 + 方案二; deferred items in §8)
Research baseline: `woca-internal-docs/harness-mechanism-gap-research-2026-11-07.md` (not in repo)

## 1. Intent

Two framework-level gaps versus modern harness agents (Claude Code, Codex CLI):

1. **The background-task loop is open.** woca can start background work
   (`Bash run_in_background`, `Task run_in_background`, `ScheduleCron`), but once
   the agent's turn ends, completions and cron firings only queue. Nothing wakes
   the model; a queued notification is consumed only after the next
   *user-initiated* run reaches a tool round. Claude Code delivers between-turn
   background completions to the model (changelog: "Background task notifications
   delivered between turns are now sent to the model inside `<system-reminder>`
   tags"); Codex covers the idle window with the blocking `wait_agent`. woca has
   neither, so "agent starts a long task, keeps working, acts on completion"
   cannot complete without the user typing.
2. **The plugin unit is skills+agents only.** A Claude Code plugin (manifest
   `.claude-plugin/plugin.json`, contributions `commands/ agents/ skills/ hooks/
   .mcp.json .lsp.json …`) or a Codex plugin (`.codex-plugin/plugin.json`:
   `skills hooks apps mcpServers scheduledTasks`) installs into woca with its
   hooks and MCP servers silently dropped.

## 2. Goals

- Idle wake: a notification (background shell completion, background Task
  completion, cron firing) queued while its session has no active run starts a
  wake run that delivers it, on every surface (web server, TUI, line REPL).
- Notifications become session-scoped: a job started in session A never surfaces
  inside session B's transcript (web server runs many sessions on one Agent).
- Bounded blocking wait (`wait_ms`) on `BashOutput` and `TaskOutput` so the
  model stops burning tool calls on poll loops.
- `BashInput` writes to a background shell's stdin (Codex `write_stdin` parity;
  `executor.Process.Stdin()` already exists, unexposed).
- Fire the declared-but-dead hook events (`SubagentStop`, `Notification`), add
  `SubagentStart` (Codex parity).
- Background Task completion gains a completion notification (today it is
  silent; only `TaskOutput` polling observes it).
- Plugin contributions: `hooks`, MCP servers (`.mcp.json`), `commands/*.md`;
  Claude Code layout compatibility (`.claude-plugin/plugin.json`,
   `hooks/hooks.json`), `${CLAUDE_PLUGIN_ROOT}` substitution, plugin
  enable/disable, minimal `name@marketplace` install resolution.
- Web transcript renders tagged harness notifications as system notices, not
  user bubbles.

## 3. Non-goals / anti-goals

- No persistent/scheduled-task plane (cross-restart cron, cloud routines) — §8.
- No Task resume / `followup_task` — §8.
- No new hook *types* (`prompt` / `agent` / `http`): schema unchanged, `command`
  only. Claude `timeout` (seconds) accepted as input alias only.
- No auto-wake in one-shot `-p` mode (changes one-shot semantics; §8).
- No LSP / outputStyles / workflows / types / apps / scheduledTasks plugin
  contributions (no receiving surface in woca; ignored with a warning).
- No fallbacks: wake failure surfaces as an error/log line, not a retry ladder.
- EE worker form: background shells remain local to the executing worker
  process; cross-instance wake is NOT solved here (documented limitation, §8).

## 4. Baseline (file anchors)

- Queue: `internal/agent/queue.go` — `QueuedInput{Text, Tag}`, drained only in
  `toolsNode` (`internal/agent/agent.go:1619`) after tool results.
- Producers: background shells (`agent.go:606` → `QueueNotification`), cron
  (`agent.go:497`). Task completion produces nothing.
- Shells: `internal/tools/background.go` — `BackgroundShells` (+`BashOutput`,
  `KillShell`), `SetNotify(func(string))`, per-shell `done` channel.
- Tasks: `internal/agent/task.go` — `TaskManager`, `backgroundTask` (no done
  channel), `TaskOutput`/`TaskStop`.
- Cron: `internal/agent/cron.go` — session-lifetime scheduler, fires through
  `QueueNotification`.
- Run identity in ctx: `tracing.WithRun(ctx, runID, sessionID)` set by
  `internal/server/runs.go:427` and `internal/ee/worker/worker.go:199`;
  `tracing.RunFrom(ctx)` reads it. TUI/REPL runs carry none.
- Run drivers: server `startRun`/`Run.start` (`internal/server/runs.go`,
  `run.go`), TUI `submit`/`startStreamGoroutine`/`runDone`
  (`internal/tui/tui.go`), line REPL in `internal/repl`.
- Hooks: `internal/hooks` — 12 events, 2 dead (`SubagentStop`, `Notification`
  have zero call sites); loaded from `~/.woca/settings.json` +
  `<workdir>/.woca/settings.json` (`internal/agentbuild/layers.go:28`).
- Plugins: `internal/plugins/plugins.go` — manifest `name/version/description/
  skills_dir/agents_dir`, contributions skills+agents only; merged post-hoc
  (`agentbuild/build.go:140,560`), no merge level; `/plugins` in
  `internal/console/plugins_cmd.go`.
- MCP specs: `name=K=V … command args` / `name=url` strings;
  `{"mcpServers": {…}}` parsing + spec rendering exists in
  `internal/claudeenv/claudeenv.go` (parseServer/Spec) — to be extracted.
- Skills: `{name}` placeholder expansion (`skills.Expand`), args declared in
  frontmatter.
- Web transcript filter: `web/src/lib/transcript.ts:38` (only
  `<relevant-memory` / `<goal-cycle` today).

## 5. Design

### 5.1 Session-scoped notifications

- `QueuedInput` gains `Session string`. `queue.drain(session string)` returns
  items whose `Session` is empty or equal; `drainAll` keeps legacy behavior.
  `toolsNode` drains by `tracing.RunFrom(ctx).SessionID` (empty = everything,
  which is exactly TUI/REPL single-session semantics).
- `BackgroundShells.Start(ctx, …)` captures `tracing.RunFrom(ctx).SessionID`
  per shell; `SetNotify` signature becomes `func(session, notification string)`.
- `TaskManager.start(ctx, …)` captures the session the same way; background
  Task completion now emits a notification through a new
  `TaskManager.SetNotify(func(session, text string))` wired by the Agent to
  `QueueNotification` — tag `background-task-completed`, text: task id,
  description, final status, bounded tail of the final report (same
  `notificationOutputCap` discipline as shells).
- `cronScheduler.Add` captures the session from the ScheduleCron tool ctx.
- Delivery-at-start fix: `streamWithBudget`/`runTurn` prepend queued items
  (session-matched) to the history *before* the final Human message, so a
  notification is delivered on the next run even when the model answers
  without tool calls. The mid-turn toolsNode drain stays (it serves steering).

### 5.2 Idle wake (observer seam + surface drivers)

The Agent does not own run lifecycle (SSE, persistence, admission, ledger do),
so the core only exposes the seam:

- `Agent.SetNotificationObserver(func(QueuedInput))` — called synchronously
  from producer goroutines (must not block); plus
  `QueuedInputCount(session string) int`. Nil observer = current behavior.

Web server driver (`internal/server`):
- `server.New` installs the observer. Handler: if `q.Session != ""` and the
  session has an active run (`admission`/session state), return — the running
  run's toolsNode will drain it. Otherwise: drain that session's queue, and if
  non-empty coalesce all items into ONE Human message (concatenated tagged
  blocks — mirrors Claude Code's coalescing) and call `startRun` with it.
  Wake runs carry `{"auto": true, "trigger": "<tag>"}` in the `run_started`
  event data and an obs log line. Guard the drain+start under a small mutex so
  two simultaneous completions yield one wake run.
- Every run's `onFinish` re-checks `QueuedInputCount(sid) > 0` → wake (covers
  notifications queued after the final toolsNode drain of a finishing run).
- Kill switch: settings key `auto-wake` (bool, default true), read when the
  observer is installed.
- Chain budget: settings key `auto-wake-limit` (int, default 20, 0 =
  unlimited) caps consecutive wake runs per session without a user-initiated
  run in between (any user run resets the counter). When the cap is hit the
  notification stays queued (delivered on the next user run), and the waker
  emits an obs/log line plus a session-state event so the UI can say so. A
  bound, not a fallback: nothing is dropped or retried.
- Permission posture: a wake run is an ordinary run — `askPermission` parks
  it exactly like a user run (web: session shows `awaiting`, the prompt is
  answered when someone looks; TUI: decision box). No auto-deny, no
  non-interactive mode. Consequence (documented): a parked wake run holds the
  session's admission slot until answered.
- Hooks: wake input never goes through `UserPromptSubmit` (it is
  machine-generated; the REPL continuation must not route through
  `submitHooks`). `Notification` fires instead (§5.5).
- EE worker form is out of scope for wake drivers: the worker's Agent still
  queues notifications, but nothing starts a run there (§8.6).

TUI driver (`internal/tui`):
- Persistent buffered `notifyCh`; observer pushes into it; `Init` arms a
  waiting `tea.Cmd`; on the message: if `m.running` → ignore (drained
  in-turn), else drain+coalesce and `runWithHistory`. `runDone` performs the
  same check.
- Line REPL (`replLoop` in `cmd/woca/main.go`): after each completed run,
  if the queue is non-empty, print and immediately run a continuation turn
  with the coalesced notification (same `submit` path as a prompt, bypassing
  `submitHooks`).

Formatting: wake input is exactly the tagged Human message the queue already
produces (`<background-task-completed>…</background-task-completed>` etc.), so
the model cannot mistake it for user speech.

### 5.3 Blocking wait

- `BashOutput` gains `wait_ms` (≤ 600000): select on the shell's `done`
  channel / timer / ctx. Response shape unchanged (status + output), so a
  wait-timeout reads as "still running".
- `backgroundTask` gains `done chan struct{}` closed exactly once on
  `setFinal`/`stop`; `TaskOutput` gains the same `wait_ms`.
- Tool descriptions updated to steer the model: bounded waits instead of poll
  loops (Codex `wait_agent` guidance).

### 5.4 BashInput

- New tool `BashInput{shell_id, input, close_stdin?}`: writes to
  `proc.Stdin()`; unknown/finished shell or closed stdin → error result.
  Registered next to `BashOutput`/`KillShell`, same permission category.

### 5.5 Hook events fire

- `SubagentStart` (new event, Codex parity): fired at the top of `runTask`
  (both foreground and background), payload = subagent name/type.
- `SubagentStop`: fired when a Task subagent finishes (completed or stopped).
  If a hook exits 2 with feedback, the feedback is appended to the subagent
  report as `<subagent-stop-feedback>` — a documented divergence from Claude
  Code's continue-the-subagent semantics (woca does not re-enter the child).
- `Notification`: fired (a) in `askPermission` immediately before the
  permission interrupt (message `permission required: <tool>`), (b) by the
  server waker when a wake run starts. Feedback is logged to obs, not
  injected (Claude Code parity: Notification stdout is user-facing).
- `loadHooksConfig` and `/hooks` listing learn the new event; README updated.

### 5.6 Plugin contribution surface

Manifest resolution: `plugin.json` OR `.claude-plugin/plugin.json` (first
wins; name defaults to the directory base). New optional fields, woca and
Claude spellings both accepted:

| contribution | woca field | Claude field | shape |
|---|---|---|---|
| skills dir | `skills_dir` | `skills` | path |
| agents dir | `agents_dir` | `agents` | path |
| commands dir | `commands_dir` | `commands` | path or [path] |
| hooks | `hooks` | `hooks` | path or inline `{…}` |
| MCP | `mcp_servers` | `mcpServers` | path or inline `{…}` |

- `Plugin` struct gains `Commands []skills.Skill`, `Hooks hooks.Config`,
  `MCPSpecs []string`, plus `Ignored []string` (unsupported contributions
  found in a Claude/Codex manifest: `lspServers`, `outputStyles`,
  `workflows`, `types`, `apps`, `scheduledTasks`, `experimental`).
- `commands/*.md` → skill: name `"<plugin>:<command>"`, description from
  frontmatter; `$ARGUMENTS`/`$1`…`$9` mapped to `{arguments}`/`{arg1}`… with
  matching declared args (invocation parity with `/plugin:cmd args…`).
- hooks file: woca shape `{"hooks": {<Event>: [{matcher, hooks:[{type,
  command, timeout_ms | timeout}]}]}}`; Claude `timeout` (seconds) accepted as
  alias. Substitution in hook commands: `${CLAUDE_PLUGIN_ROOT}`,
  `${WOCA_PLUGIN_ROOT}` → plugin install path; `${CLAUDE_PROJECT_DIR}` →
  workdir (resolved at run time by the hooks Runner, not at load time).
- `.mcp.json` / inline `mcpServers`: parsed by the spec renderer extracted
  from `claudeenv` into `internal/mcp` (`ParseServerEntry` + `Spec()`),
  `claudeenv` delegates to it. Same substitution in `command`/`args`/`env`
  values (env rendered as `K=V` prefixes, args shell-quoted — existing Spec
  behavior).
- Merge wiring (`internal/agentbuild/layers.go`):
  - `merge.LevelPlugin` (new level, between Managed and User; `String()`
    "plugin"). Plugin skills enter `loadSkills` as merge entries (replacing
    the post-merge append at `build.go:560` and in `ApplyClaudeEnv`) with
    `Source: "plugin/<name>"`.
  - `loadHooksConfig(workdir, plugins)`: plugin entries appended after
    project entries, `Source` set for `/hooks` display.
  - `mergeMCPSpecsLayered` gains the plugin layer below user.
  - Plugin agents keep the current append behavior, `Source` tagged.
- Enable/disable: settings key `disabled-plugins` (name list, registered in
  the settings key lists + struct + parse + format); `plugins.Load` skips
  disabled plugins (skipped set surfaced for `/plugins`); `/plugins
  enable|disable <name>` subcommands; `/plugins list` shows per-plugin
  contribution counts and ignored-contributions warnings.
- Marketplace resolution: `plugins.Install("<name>@<git-url-or-path>")` —
  clone/copy the marketplace source, read `.claude-plugin/marketplace.json`,
  match `name`, resolve its `source`: relative path → subdirectory of the
  marketplace clone; `{"source":"github","repo":"owner/repo"}` → clone that
  repo. Plain `<git-url>`/path install (whole repo = one plugin) unchanged.
- Live wiring: `/plugins install` already merges agents live; hooks/MCP
  contributions require restart (stated in `/plugins` output).

### 5.7 Web transcript

`web/src/lib/transcript.ts`: messages starting with `<background-task-completed>`,
`<scheduled-task>`, or `<queued-user-input>` render as a compact system notice
(kind `notice`, dim style) instead of a user bubble; live SSE path
(`messagesToItems`-equivalent) gets the same filter. i18n label via existing
locale files. vitest coverage for both reload and live paths.

## 6. Approaches considered

1. **Idle wake inside the Agent core** (agent re-Invokes itself when idle):
   rejected — run lifecycle (SSE, session persistence, admission gate, run
   ledger) is surface-owned; a core auto-invoke would bypass all of it. Chosen:
   observer seam + per-surface drivers, assembly-time injection only.
2. **Teach polling via a skill** (no mechanism change): rejected — burns tool
   calls per poll and still cannot act while idle.
3. **Full Claude plugin parity** (LSP, outputStyles, workflows, types, apps):
   rejected this round — woca has no receiving surfaces; ignored-with-warning
   keeps installability honest.
4. **Persistent scheduler now** (runstate `not_before` enqueue): deferred —
  sound EE extension, but doubles the change surface; §8.

## 7. Test strategy

Unit (per package):
- `tools/background`: session capture, `wait_ms` (done/timeout/ctx paths),
  `BashInput` (write, close, unknown/finished errors), notify signature.
- `agent/task`: `done` channel, completion notification (text shape, cap),
  `SubagentStart`/`SubagentStop` firing incl. exit-2 feedback append.
- `agent/queue` + `agent` run-turn: session-scoped drain (match/empty/mismatch),
  prepend-at-run-start, toolsNode still drains mid-turn.
- `agent/cron`: session capture at Add.
- `hooks`: `timeout` alias, new event wiring.
- `plugins`: manifest variants (woca, `.claude-plugin`, inline hooks/mcp,
  arrays), commands→skill mapping incl. `$ARGUMENTS`, substitution,
  marketplace resolution (local dir fixture + relative source; github source
  unit-tested via URL construction only, no network), disabled filtering,
  ignored-contributions warnings.
- `agentbuild/layers`: plugin layers (skills level, hooks order, MCP layering).
- `settings`: new keys in the registration lists.

Integration:
- `internal/server` httptest with FakeChatModel: run 1 starts a background
  shell (scripted), run 1 completes; shell finishes while idle → wake run
  appears (assert second run's SSE stream, tagged Human message persisted in
  the session store, `auto: true` in run_started data). Kill switch off → no
  wake. Two sessions: job from A never lands in B (transcript + queue counts).
  Queued-input API unchanged.
- REPL: continuation turn after run completes with pending notification.
- Web: transcript vitest (reload + live) for the three tags; `npm run build`.

Full gates: `go test ./...`, `-race`, `make lint`, `npm run build` before
`go build` (webdist embed rule).

## 8. Deferred (recorded, not built)

1. Persistent scheduled tasks: cron entries survive restart (session store /
  wocadb), server-side scheduler enqueues runs; EE maps to runstate
  `Enqueue` with a `not_before` column. (Claude cloud scheduled tasks,
  Codex `scheduledTasks`.)
2. Task resume / `followup_task`: keep a finished background Task's subagent
  transcript; `Task{resume: id, prompt}` continues it (Codex
  `followup_task`/`resume_agent`, Claude `SendMessage` to an agent id).
3. One-shot `-p` stays open until background commands exit (Claude Code
   behavior).
4. Hook types `prompt`/`agent`/`http`; `asyncRewake` hooks.
5. `Monitor` tool (watch a script/websocket with a deadline).
6. Cross-instance background state for the EE worker form: jobs live on the
  executing worker's Agent and the worker has no wake driver, so the worker
  surface stays open-loop (notifications are consumed only by a later run
  that happens to execute on the same worker). Needs a ledger-level wake
  (enqueue a run) to close.
7. `PushNotification` desktop/phone primitive; agents/skills frontmatter
   `model`/`allowed-tools`/`disable-model-invocation`/`permission-mode`.

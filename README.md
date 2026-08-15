# woca

A coding-agent **harness** written in Go, modeled on the architecture of
[Claude Code](https://github.com/anthropics/claude-code): an explicit
**LangGraph-style graph** (model → tools loop), a Claude-Code-style tool set
with a permission gate and human-in-the-loop approval, token-level streaming,
plan mode, subagents, skills, hooks, auto-compaction, and a minimal MCP client
— all built on [`langchain-golang`](https://github.com/ProjAnvil/langchain-golang),
with a [cobra](https://github.com/spf13/cobra) CLI, a
[bubbletea](https://github.com/charmbracelet/bubbletea) TUI,
[zerolog](https://github.com/rs/zerolog) structured logging, and OAuth
device-flow login.

`woca` deliberately does **not** use `agents.CreateAgent` (the library's
prebuilt agent). The agent is expressed as an **explicit graph** — nodes,
edges, conditional edges, reducers, checkpoints, and interrupt/resume — the
same shape LangGraph exposes, implemented from scratch in
[`internal/graph`](internal/graph).

## What it is

- **A graph runtime** (`internal/graph`) mirroring the LangGraph `StateGraph`
  API: `AddNode`, `AddEdge`, `AddConditionalEdges`, `AddReducer`,
  `SetEntryPoint`, `Compile`, `Invoke`, `Stream`, plus `Interrupt`/`Resume`
  with checkpoints.
- **A coding agent** (`internal/agent`) expressed as that graph:
  `model → route → tools → model …` with an `add_messages` reducer. The tools
  node runs a **gate** (permission / AskUserQuestion / plan approval) plus
  Pre/Post tool-use hooks.
- **A tool set** (`internal/tools`) in the spirit of Claude Code: `Bash`,
  `Read`, `Write`, `Edit`, `NotebookEdit`, `Glob`, `Grep`, `ListDir`,
  `TodoWrite`, `WebFetch`, `WebSearch`, `AskUserQuestion`, `EnterPlanMode`,
  `ExitPlanMode`, plus `Task` / `TaskOutput` / `TaskStop` (subagents),
  `TaskCreate` / `TaskGet` / `TaskUpdate` / `TaskList` (task list), `Skill`,
  and `SendMessage` — each a langchain `core/tools.Tool` built with
  `FromFunc`.
- **Hooks** (`internal/hooks`) — `PreToolUse` / `PostToolUse` /
  `UserPromptSubmit` / `Stop` shell commands with matchers (`Bash`,
  `Edit|Write`, `Bash(git *)`) loaded from `settings.json`.
- **Skills** (`internal/skills`) — `SKILL.md` files (with YAML frontmatter)
  loaded from `.woca/skills` / `.claude/skills` and exposed via `Skill`.
- **Auto-compaction** (`internal/compact`) — summarize older messages when the
  conversation exceeds a token estimate, keeping the recent tail.
- **Context collection** (`internal/context`) — `git status` injected into the
  system prompt.
- **MCP** (`internal/mcp`) — an MCP JSON-RPC client over both the **stdio** and
  **streamable HTTP** transports (`initialize`, `tools/list`, `tools/call`,
  `resources/list`, `resources/read`, and server→client notifications), exposing
  tools as `mcp__server__tool` and resources as `mcp__server__resources` /
  `mcp__server__resource`.
- **A permission system** with the Claude Code modes and an input-sensitive
  read-only classifier (a read-only `Bash` command auto-allows).
- **Token-level streaming** — the model node streams text deltas through the
  graph (`graph.StreamingNodeFunc`) and the renderer prints them live.
- **A bubbletea TUI** (`internal/tui`) that streams model output live, and
  **OAuth device-flow login** (`internal/oauth`).

## Architecture

```
cmd/woca                cobra CLI: flags, config, REPL, one-shot
internal/graph          LangGraph-style StateGraph runtime (interrupt/resume)
internal/agent          coding-agent graph + gate + plan mode + subagents + tasks
internal/tools          tool set + registry + permission categories + read-only classifier
internal/hooks          PreToolUse/PostToolUse/Stop hook runner
internal/skills         SKILL.md loader
internal/compact        auto-compaction (summarization)
internal/context        git-status context collection
internal/console        shared console slash commands (/web /clear /compact …)
internal/webgui         on-demand web-GUI manager (start/stop, default port 3484)
internal/mcp            minimal MCP stdio client
internal/prompt         system prompt (Claude Code-style sections) + subagent prompt
internal/runner         streaming run loop + interrupt handling + rendering
internal/config         provider/model config + permission modes
internal/logging        zerolog wrapper (level parsing + constructors)
internal/session        JSONL transcript persistence
```

### The agent graph

```
        ┌──────────┐   no tool calls   ┌──────┐
START ─▶│  model   │──────────────────▶│ END  │
        └────┬─────┘                   └──────┘
             │ has tool calls
             ▼
        ┌──────────┐   (gate + hooks)   ┌──────┐
        │  tools   │───────────────────▶│ INT  │
        └────┬─────┘                   └──┬───┘
             │ (always loops back)        │ resume
             └────────────────────────────┘
```

The `tools` node runs a per-call **gate** (sequential, so interrupts pause
deterministically) then executes allowed calls concurrently. Pre/Post tool-use
hooks wrap each call; a Pre hook exiting `2` denies the tool, and a Post hook
exiting `2` feeds feedback back to the model. State is a single `messages`
channel with an `add_messages` reducer.

### Observability log

`--debug-log` (or the persisted `debug-log` setting — the web settings panel
and `/debug-log on|off` toggle it live) turns on full harness logging under
`~/.woca/logs/`, size-rotated (8 MB × 5 per file):

```
logs/woca.log                            main log: run lifecycle, model usage, summaries
logs/ws-<name>-<hash8>/<session>.jsonl   session trace: one JSON event per line
logs/ws-<name>-<hash8>/<session>/<run>.json   message-level dump (full transcript)
```

Every graph-node dispatch (the agent graph and the goal graph) is recorded
with its phase, duration, and state-update summary through the graph runtime's
observer hook. In the web UI, each assistant bubble gains a hover action bar:
**copy as markdown**, plus a **debug 🐞 button** (visible while the log is on)
that copies the workspace/session/run context and log locations for quick
troubleshooting.

### Goal mode (persistent objectives)

`/goal set <objective>` switches the agent from ask-answer to an autonomous
loop that keeps working until the objective is **verified** complete, blocked,
paused, or out of budget — expressed as its own graph on top of the agent
graph:

```
START ─▶ assess ─▶ execute ─▶ evaluate ─┬─ active   ─▶ assess (next cycle)
                                        └─ terminal ─▶ END
```

- **assess** plans the cycle (done-criteria checklist + this cycle's actions);
- **execute** runs a full nested agent turn (tools, permissions, hooks,
  compaction) seeded with a `<goal-cycle N>` message;
- **evaluate** is a skeptical verification gate: the model may
  `claim_complete` via the `UpdateGoal` tool, but only evidence verified by
  the evaluator completes the goal;
- budgets (`--cycles N`, `--tokens N`), idle auto-continue, empty-cycle
  suppression, and mid-cycle interrupt/resume all apply; state follows the
  Codex-style machine `active ↔ paused → blocked / complete / budget_limited`.

The web UI shows a goal chip + detail panel (cycle/token meters, per-cycle
verdicts, pause/resume/clear) with live `goal` SSE events; the TUI footer
shows `goal[status] cycle n/N · xk/2M tok`. Goal state persists per session
(`<id>.goal.json` sidecar) and survives server restarts. See
`docs/agent-loop-comparison.md` §16.

### System prompt

`internal/prompt` mirrors Claude Code's system-prompt structure with original
wording: intro/identity, system rules, doing tasks, executing actions with
care, using your tools, tone and style, output efficiency, session guidance
(AskUserQuestion, Task subagents, plan mode), plus an environment block (cwd,
git, shell, OS, model), collected `git status`, and `CLAUDE.md`/`AGENTS.md`.

### Permission modes

| Mode | Read-only / meta / web | File edits (Write/Edit) | Shell (Bash) |
|------|------------------------|-------------------------|--------------|
| `default` | allowed | ask | ask |
| `acceptEdits` | allowed | allowed | ask |
| `bypassPermissions` | allowed | allowed | allowed |
| `readOnly` / `plan` | allowed | denied | denied |

Read-only is **input-sensitive**: `Bash("git status")` auto-allows, while
`Bash("rm -rf …")` asks.

**Explicit permission rules** (checked before the mode) can be set in
`~/.woca/settings.json` or `.woca/settings.json`:

```json
{
  "permissions": {
    "allow": ["Bash(git *)", "Read"],
    "deny": ["Bash(rm *)"],
    "ask": []
  }
}
```

Matchers are exact tool names, regexes (`Edit|Write`), or `Tool(glob)` forms.
Precedence is `deny` → `ask` → `allow` → mode default.

## Build & run

```bash
make build          # or: go build ./...
make test           # go test ./...
make run            # go run ./cmd/woca

# one-shot
go run ./cmd/woca -p "explain this repo's build system" --model claude-sonnet-4-5

# interactive REPL (with slash commands /help /exit)
go run ./cmd/woca --model claude-sonnet-4-5

# auto-accept file edits but still confirm shell commands
go run ./cmd/woca --accept-edits

# allow everything (use with care)
go run ./cmd/woca -p "fix the failing test" --dangerously-skip-permissions
```

### Web UI

A Vue 3 web UI is embedded into the binary via `go:embed web/dist` and served
with a server-sent-events (SSE) streaming API. Build the frontend, then serve:

```bash
make web            # cd web && npm install && npm run build
make serve          # build web + binary, then run `woca serve`

# or manually (default port 3484):
go run ./cmd/woca serve --addr 127.0.0.1:3484
```

`woca serve` opens `http://127.0.0.1:3484` and accepts the same agent flags as
the CLI (e.g. `--model`, `--permission-mode`, `--workdir`).

The web GUI can also be started **on demand from the console** with the `/web`
slash command (see below): `/web` starts it on the default port 3484, `/web
<port>` picks a port, `/web off` stops it, `/web status` reports it. Set
`WOCA_NO_BROWSER=1` to keep it from opening a browser tab.

HTTP API:

| Endpoint | Description |
|----------|-------------|
| `POST /api/sessions` | create a session → `{"session_id":"..."}` |
| `GET /api/sessions` | list persisted sessions (id/title/updated_at/message_count) |
| `GET /api/sessions/{id}` | fetch a session transcript (full messages incl. tool calls/results) |
| `POST /api/runs` | start a run; body `{"session_id":"...","prompt":"..."}` → `{"run_id":"...","session_id":"..."}` |
| `GET /api/runs/{id}/events` | SSE stream: `delta` / `tool_call` / `tool_result` / `interrupt` / `done` / `error` |
| `POST /api/runs/{id}/answer` | resolve an interrupt: `{"approve":true}` or `{"answers":["..."]}` |
| `POST /api/runs/{id}/cancel` | cancel a run |

Sessions persist to `~/.woca/sessions` as JSONL transcripts; a run continues the
session it was started with, so tool results carry across turns.

The frontend is a static SPA (no server-side rendering) with a fallback to
`index.html`.

### Configuration

| Flag | Description |
|------|-------------|
| `-p, --prompt` | one-shot prompt (runs non-interactively and exits) |
| `--model` | model name (e.g. `claude-sonnet-4-5`, `gpt-4o`) |
| `--provider` | `anthropic` (default) or `openai` |
| `--api-key` | API key (else `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`) |
| `--base-url` | provider base URL (else `ANTHROPIC_BASE_URL` / `OPENAI_BASE_URL`) |
| `--permission-mode` | `default` \| `acceptEdits` \| `bypassPermissions` \| `readOnly` \| `plan` |
| `--accept-edits` | shortcut for `acceptEdits` |
| `--dangerously-skip-permissions` | shortcut for `bypassPermissions` |
| `--read-only` / `--plan` | deny write/shell tools |
| `--workdir` | working directory (default: current directory) |
| `--system-prompt` | extra instructions appended to the system prompt |
| `--log-level` | `trace`\|`debug`\|`info`\|`warn`\|`error`\|`disabled` (default `warn`) |
| `--auto-compact` | token estimate above which the conversation is summarized (`0` disables) |
| `--mcp` | MCP server as `name=command` (repeatable) |
| `--max-tokens`, `--temperature` | sampling controls |
| `--resume` | resume from a transcript file (or `last`) |
| `--checkpoint-dir` | persist graph checkpoints to a directory (file backend) |
| `--resume-checkpoint` / `--resume-value` | resume from a persisted checkpoint |
| `--db` | settings database: sqlite path, `postgres://` DSN, or `none` (default `~/.woca/woca.db`, env `WOCA_DB`) |
| `--version` | print version |

### Persisted settings (database + encrypted secrets)

woca persists its configuration in a **database under `~/.woca`** — sqlite by
default (`~/.woca/woca.db`, pure-Go driver, no cgo) with PostgreSQL optional
via a DSN. Provider credentials (anthropic/openai **base URL + API key**) and
the postgres DSN are stored **encrypted at rest** (AES-256-GCM; the master key
lives in `~/.woca/secret.key`, mode `0600`).

Configure from the terminal (`woca config`), the TUI/REPL (`/settings`), or
the Web UI settings panel — they share one database:

```console
$ woca config set provider openai
$ woca config set openai-api-key sk-...        # stored encrypted
$ woca config set anthropic-base-url https://proxy.example.com
$ woca config list
database: sqlite:/home/you/.woca/woca.db
provider: openai
openai-api-key: (set, encrypted)
checkpoint: sqlite
memory: sqlite
...
```

The **memoryStore** (a namespace+key JSON store) and **graph checkpoints**
(persisted interrupt/resume state) each select a backend — `sqlite` (default),
`postgres`, `file`/`memory`, or `none`:

```console
$ woca config set checkpoint postgres     # restart woca to apply
$ woca config set postgres-url postgres://user:pass@host/woca   # encrypted
$ woca config set memory sqlite
```

API keys resolve in order: `--api-key` flag → `ANTHROPIC_API_KEY` /
`OPENAI_API_KEY` → database (encrypted) → OAuth token. Base URLs resolve the
same way (`--base-url` → env → database).

In an interactive session, `/settings` shows and updates the same values
(provider/model/base URLs/keys/storage backends; credential changes take
effect immediately) and `/memory list|get|put|del` inspects the memoryStore.
The Web UI exposes the same controls under its settings panel
(`GET`/`PUT /api/settings`); backend switches report `restart_required`.

### Hooks

Hooks are configured in `~/.woca/settings.json` and `.woca/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash(git *)", "hooks": [{"type": "command", "command": "echo 'git command incoming'"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "cat"}]}
    ],
    "Stop": [
      {"matcher": "*", "hooks": [{"type": "command", "command": "echo 'turn complete'"}]}
    ]
  }
}
```

Each hook receives a JSON event on stdin; stdout is fed back as context. A
`PreToolUse` hook exiting `2` denies the tool; a `PostToolUse` hook exiting `2`
blocks continuation.

### Skills

Drop a `SKILL.md` with YAML frontmatter under `.woca/skills/<name>/` (or
`.claude/skills/<name>/`, `~/.woca/skills/<name>/`):

```markdown
---
name: commit
description: Write a conventional commit message
---
Write a conventional-commit message for the current diff.
```

The model can then call `Skill(skill="commit")`.

### MCP

`--mcp "filesystem=node /path/to/server.js /tmp"` spawns a stdio MCP server
(via `sh -c`) and exposes its tools as `mcp__<name>__<tool>`; use
`--mcp "name=https://host/mcp"` for a streamable-HTTP server. The client also
supports **resources** (`mcp__<name>__resources` / `mcp__<name>__resource`),
**prompts** (`prompts/list` / `prompts/get`), server→client notifications, and
a GET SSE long-connection (`Client.Listen`).

### REPL

The interactive console (bubbletea TUI on a terminal, line-based REPL
otherwise) supports a Claude-Code-style slash-command set:

| Command | Description |
|---------|-------------|
| `/help` | list available commands |
| `/status` (alias `/config`) | version, provider/model, permission mode, workdir, web-GUI state |
| `/permissions [mode]` | show or set the permission mode |
| `/model [name]` | show or switch the model at runtime |
| `/clear` | clear the conversation and start fresh |
| `/compact` | summarize the conversation to free context |
| `/context` | show context-window usage (tokens, message mix, compact threshold) |
| `/todos` / `/tasks` | show the todo list / shared task list |
| `/skills` / `/mcp` | list loaded skills / connected MCP servers and tools |
| `/web [port\|off\|status]` | start/stop the embedded web GUI (default port 3484) |
| `/exit`, `/quit` | leave the console |

It also supports `!command` to run a shell command whose output is added to the
conversation, tab-completion of slash commands, up/down history, `esc` to
interrupt a run, and `pgup`/`pgdown` to scroll the transcript.

## How it maps to Claude Code

| Claude Code | woca |
|-------------|------|
| agent loop (model ↔ tools) | the `model` / `tools` / `route` graph |
| streaming tool execution (`StreamingToolExecutor.ts`) | `internal/agent/streamingexec.go` — tool calls run while the model still streams |
| model fallback on overload (`withRetry.ts`, `FallbackTriggeredError`) | `internal/model` Router — retry + fallback on both Invoke and Stream paths |
| max-output-tokens recovery (8k→64k retry + resume messages) | `internal/agent/recovery.go` — escalation + up to 3 continuation turns |
| prompt-too-long reactive compaction (`tryReactiveCompact`) | `internal/agent/reactivecompact.go` — compact once, single-retry guard |
| tool-result size budget (`applyToolResultBudget`) | `internal/agent/toolbudget.go` — per-round aggregate budget, largest-first preview replacement |
| queued mid-turn input (`getQueuedCommandAttachments`) | `internal/agent/queue.go` — `QueueInput` + injection after each tool round (`POST /api/runs/{id}/input`) |
| tool-use summaries (`toolUseSummaryGenerator.ts`) | `internal/agent/toolsummary.go` — async labels, UI-only, consumed while the next round streams |
| interactive permission approval (`useCanUseTool`) | `internal/tui` pending approval box — y/enter allows, n/esc denies, run resumes |
| token-budget auto-continue (`tokenBudget.ts`) | `internal/runner/tokenbudget.go` — 500k nudge continuations with diminishing-returns stop |
| goal mode (`/goal` autonomous loop; Codex goal state machine) | `internal/agent/goalgraph.go` — assess → execute → evaluate graph with skeptical verification, budgets, idle continuation, mid-cycle interrupt/resume |
| maxTurns + between-turn tool refresh (stage 28) | `internal/agent/maxturns.go` + `refreshtools.go` (`--max-turns`, MCP re-list) |
| task summaries (`claude ps`) | `internal/agent/tasksummary.go` — periodic one-liner surfaced by `/status` |
| memory prefetch/attachments | `internal/agent/memorystore.go` — relevant memories attached after tool rounds |
| tool registry (`tools.ts`) | `internal/tools` registry + `FromFunc` tools |
| `Bash`, `Edit`, `Read`, `Write`, `Glob`, `Grep`, `TodoWrite`, `WebFetch`, `WebSearch` | same names |
| `AskUserQuestion`, `EnterPlanMode`, `ExitPlanMode` | interrupt-handled in the agent gate |
| `Task` (subagent), `TaskOutput`, `TaskStop` | `internal/agent` subagent runner + task manager |
| `TaskCreate`/`TaskGet`/`TaskUpdate`/`TaskList` | `internal/agent` task list |
| hooks (`PreToolUse`/`PostToolUse`/`Stop`) | `internal/hooks` |
| skills (`SKILL.md`) | `internal/skills` + `Skill` tool |
| auto-compaction / summarization | `internal/compact` |
| context collection (`context.ts`) | `internal/context` |
| MCP servers | `internal/mcp` (stdio JSON-RPC) |
| slash commands (`commands.ts`) | `internal/console` (shared by TUI + REPL) |
| permission modes | `internal/config` + read-only classifier |
| system prompt (`prompts.ts`) | `internal/prompt` (same section structure) |
| CLI (Commander.js) | `cobra` |
| `claude server` (Direct Connect, on-demand web UI) | `/web` console command (`internal/webgui`) |
| telemetry/logging | `zerolog` |
| `-p` one-shot / REPL | `cmd/woca` |

## Notes

- `langchain-golang` is pinned to `v0.5.0`; other dependencies are `cobra`,
  `zerolog`, `bubbletea` (+ `lipgloss`), and their transitive packages.
- Web search uses DuckDuckGo's HTML endpoint (best-effort; no API key).
- Model output is streamed token-by-token (text deltas through the graph);
  tool-call deltas are accumulated by tool-call ID.
- **Streaming model fallback** (like Claude Code): the model Router retries
  transient stream errors with exponential backoff, then switches to the
  `--fallback-model` once retries are exhausted. When the switch happens
  after partial output was streamed, a restart marker resets the
  accumulated message (the streamed text shows a `[switched to fallback
  model…]` notice) and cancels speculative tool calls dispatched from the
  failed attempt, so the final message is always a clean, single-model
  response.
- **Max-output-tokens recovery** (like Claude Code): when a response is
  truncated by the output-token cap (`stop_reason=max_tokens` /
  `finish_reason=length`), the turn is retried once at an escalated 64k cap
  (only when no explicit `--max-tokens` was set), then up to three times by
  injecting a "resume directly, pick up mid-thought" continuation message.
  Exhausted, the truncated output is surfaced as-is.
- **Prompt-too-long reactive compaction** (like Claude Code): when the
  provider rejects the request for exceeding the context window, the
  conversation is force-compacted (summary + recent tail) and the turn
  retried once; a second rejection — or a conversation that cannot shrink —
  surfaces the error instead of looping.
- **Tool-result size budget** (like Claude Code): each tool round (the run
  of tool results between assistant messages) must fit an aggregate
  character budget (default 200k). When it overflows, the largest results
  are replaced by a preview placeholder in the model request — the
  transcript keeps the originals, and replacements are deterministic so the
  model never sees a previously-seen result retroactively change.
- **Mid-turn queued input** (like Claude Code): input submitted while a run
  is in flight (`Agent.QueueInput`, or `POST /api/runs/{id}/input` from the
  web client) is buffered and attached after the next completed tool round,
  wrapped in a `<queued-user-input>` tag, so the model can address it
  within the same turn instead of losing it.
- **Tool-use summaries** (like Claude Code): after each completed tool
  batch, a short "Read config.json"-style label is generated asynchronously
  with the summarizer model. It never blocks the next model round and never
  enters the model context — the interactive runner renders it as a single
  `· label` line while the next round streams (one-shot `--prompt` runs
  skip generation).
- **Interactive permission approval** (like Claude Code): when a run needs
  a permission decision, the TUI shows an approval box above the input —
  `y`/enter allows, `n`/esc denies, and the run resumes immediately.
  AskUserQuestion interrupts collect their answer on the input line
  (multiple answers separated by `;`).
- **MaxTurns** (like Claude Code's SDK option): `--max-turns N` caps the
  model rounds per turn; reaching the cap ends the turn with a
  `<max-turns-reached>` marker. Between rounds the agent also re-lists MCP
  tools, so tools a server adds mid-session become usable immediately, and
  `/status` shows a periodically refreshed one-line "working on: …" task
  summary.
- **Memory attachments** (like Claude Code): when a memoryStore backend is
  configured (`/settings set memory sqlite|postgres`), each turn prefetches
  memories relevant to your message and attaches them after tool rounds, so
  long-term context written via `/memory put` reaches the model.
- **Token-budget auto-continue** (like Claude Code's 500k feature, opt-in):
  set `WOCA_TURN_TOKEN_BUDGET=<tokens>` and the interactive runner nudges
  the model to keep working ("Stopped at N% of token target…") whenever a
  turn ends under 90% of the budget, stopping on diminishing returns
  (3+ continuations adding under 500 tokens each).
- **Streaming tool execution** (like Claude Code): both bundled providers
  emit each tool call as one finished chunk when its content block closes, so
  the model node dispatches it immediately in the background when the
  permission mode deterministically allows it (read-only tools in `default`
  mode; everything in `bypassPermissions`). The tools node later awaits
  these results instead of re-running the calls. Calls that would prompt for
  permission (or any interaction gate) are never pre-started — they wait for
  the tools node so interrupts stay deterministic. After a same-process
  interrupt/resume the already-finished speculative results are reused;
  across a process restart the calls re-run.
- Subagent permission interrupts are fail-closed: a subagent that needs an
  un-granted write permission reports "permission denied" rather than
  prompting.
- The MCP client supports both stdio (newline JSON-RPC) and streamable HTTP
  (POST + JSON/SSE responses, `Mcp-Session-Id`). The HTTP transport supports
  concurrent requests; stdio is serialized per pipe.
- Auto-compaction uses a word/punctuation/CJK token estimator
  (`internal/tokens`) and, above the threshold, first snips old tool results and
  micro-compacts large ones before summarizing the head. After three
  consecutive summarization failures a circuit breaker disables
  auto-compaction (a manual `/compact` still forces it and resets the breaker
  on success), mirroring Claude Code's auto-compact failure fuse.
- Checkpoints can persist to disk (`graph.NewFileCheckpointer`) and be resumed
  cross-process via `CompiledGraph.ResumeCheckpoint`.
- The model can be wrapped in a retry/fallback router (`--fallback-model`),
  custom subagents load from `.woca/agents/*.md` (`Task(subagent_type=...)`),
  and `--search-api`/`--search-api-key` switch WebSearch to a JSON search API.
- The whole codebase is implemented test-first: `go test ./...` covers the
  graph runtime (incl. streaming nodes + checkpoints), agent loop, permission
  gate, hooks (all 9 event groups), compaction, skills, task list, MCP client
  (tools/resources/prompts + concurrency), OAuth flow, message bus, context,
  logging, TUI helpers, console commands, web-GUI manager, and CLI helpers.

## License

MIT.

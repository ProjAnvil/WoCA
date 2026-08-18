<div align="center">

# woca

**A single-binary coding-agent harness — CLI, TUI, and web — built on an explicit agent graph.**

[![CI](https://github.com/ProjAnvil/WoCA/actions/workflows/ci.yml/badge.svg)](https://github.com/ProjAnvil/WoCA/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.26%2B-00ADD8)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

[Features](#features) · [Install](#installation) · [Quickstart](#quickstart) · [Architecture](#architecture) · [Extending](#extending-woca) · [HTTP API](#http-api)

**English** | [简体中文](README.zh-CN.md)

</div>

---

woca is a harness for coding agents, written in Go. It drives any
Anthropic-compatible or OpenAI-compatible model through an explicit,
inspectable **agent graph** — model → tools → model — with a
human-in-the-loop permission gate, token-level streaming, auto-compaction,
subagents, skills, hooks, MCP servers, and an embedded web UI, all shipped as
**one static binary** with no runtime dependencies.

The agent loop is not a hidden `while` loop buried in a framework callback.
It is a graph you can read: nodes, conditional edges, reducers, checkpoints,
and interrupt/resume, powered by the langgraph runtime from
[langchain-golang](https://github.com/ProjAnvil/langchain-golang) and
composed in [`internal/agent`](internal/agent). Every run is streamed
token-by-token, every tool call passes a deterministic permission gate, and
every conversation is persisted as a resumable transcript.

## Contents

- [Features](#features)
- [Requirements](#requirements)
- [Installation](#installation)
- [Quickstart](#quickstart)
  - [One-shot](#one-shot)
  - [Interactive TUI / REPL](#interactive-tui--repl)
  - [Web UI](#web-ui)
- [Providers & models](#providers--models)
- [Permissions](#permissions)
- [Architecture](#architecture)
- [Configuration](#configuration)
- [Sessions, memory & undo](#sessions-memory--undo)
- [Multi-agent](#multi-agent)
  - [Subagents](#subagents)
  - [Agent teams](#agent-teams)
- [Extending woca](#extending-woca)
  - [Skills](#skills)
  - [Custom subagents](#custom-subagents)
  - [Hooks](#hooks)
  - [MCP servers](#mcp-servers)
  - [Capability plugins](#capability-plugins)
  - [Reuse your existing setup](#reuse-your-existing-setup)
- [Slash commands](#slash-commands)
- [HTTP API](#http-api)
- [Observability](#observability)
- [Development](#development)
- [Contributing](#contributing)
- [License](#license)

## Features

**Agent runtime**

- **Explicit agent graph** with conditional edges, an `add_messages`
  reducer, checkpoints, and interrupt/resume — pause any run mid-tool-call
  and continue it later, even across process restarts.
- **Token-level streaming** of text and tool-call deltas through the graph;
  the TUI and web UI render output live while tools execute.
- **Streaming tool execution**: read-only calls are dispatched while the
  model is still streaming; permission-gated calls always wait for the
  deterministic gate so interrupts stay exact.
- **Resilient model I/O**: retry with exponential backoff and an optional
  fallback model on both invoke and stream paths; truncated responses
  (`max_tokens`) are escalated and continued automatically; oversized
  prompts trigger reactive compaction with a single-retry guard; each tool
  round is held under an aggregate size budget with deterministic
  placeholder replacement.
- **Auto-compaction** with a CJK-aware token estimator: old tool results are
  snipped and large ones micro-compacted before the head is summarized; a
  circuit breaker disables it after repeated failures (`/compact` resets).
- **Mid-turn queued input**: submit a message while a run is in flight and
  it is attached after the next tool round — nothing is lost.
- **Thinking / reasoning support**: reasoning effort levels and streamed
  reasoning content, rendered as collapsible blocks in the web UI.

**Tools & safety**

- **20+ built-in tools**: `Bash`, `Read`, `Write`, `Edit`, `NotebookEdit`,
  `Glob`, `Grep`, `ListDir`, `TodoWrite`, `WebFetch`, `WebSearch`,
  `AskUserQuestion`, `EnterPlanMode`/`ExitPlanMode`, `Task`/`TaskOutput`/
  `TaskStop`, `TaskCreate`/`TaskGet`/`TaskUpdate`/`TaskList`, `Skill`,
  `SendMessage`, plus LSP-backed code intelligence.
- **Permission gate** with four modes, explicit allow/deny/ask rules
  (exact names, regexes, `Tool(glob)` matchers), and an input-sensitive
  read-only classifier — `git status` auto-allows, `rm -rf` never does.
- **File-level undo**: every `Write`/`Edit` snapshot is kept per session;
  `/undo` / `/redo` revert and re-apply changes, repeatable.

**Surfaces**

- **One-shot CLI** for scripting and pipes.
- **Interactive TUI** (bubbletea): live streaming, approval prompts, image
  attachments, vim-mode input, themes, scrollable transcript.
- **Embedded web UI**: sessions grouped by workspace, streaming assistant
  bubbles with tool cards, file sidebar with syntax-highlighted previews,
  image uploads and screenshot paste, settings panel — served from the same
  binary over SSE.

**Ecosystem**

- **Skills** (`SKILL.md` + YAML frontmatter), **custom subagents**
  (`.woca/agents/*.md`), **hooks** (9 event groups, matchers, exit-code
  semantics), **MCP** client (stdio + streamable HTTP, tools, resources,
  prompts, notifications, OAuth), and **capability plugins** (managed
  browser automation and PDF tooling) — see
  [Extending woca](#extending-woca).
- **Layered configuration**: flags > workspace `.woca/` > user `~/.woca/`
  for skills, agents, MCP servers, and instruction files.
- **Persisted settings** in SQLite (default) or PostgreSQL, with API keys
  and DSNs **encrypted at rest** (AES-256-GCM).
- **Usage & quota**: per-session token accounting with estimated cost, and
  remaining-quota display for subscription plans (GLM Coding Plan,
  Anthropic/OpenAI OAuth logins, DeepSeek balance).

## Requirements

- macOS or Linux (x86-64 / arm64)
- Go ≥ 1.26 to build from source
- A model provider: an Anthropic-compatible or OpenAI-compatible API key,
  a gateway, or an OAuth login

## Installation

**From source (recommended for now):**

```bash
git clone https://github.com/ProjAnvil/WoCA.git
cd woca
make            # build web frontend + binary into build/woca
```

**With `go install` (CLI only, without the embedded web UI):**

```bash
go install github.com/projanvil/woca/cmd/woca@latest
```

The web UI is embedded at compile time via `go:embed`; run `make web` before
`go build` if you change anything under `web/`.

**Make targets:**

| Target | Description |
|--------|-------------|
| `make` / `make serve` | build frontend + binary; `serve` also launches the web UI |
| `make build` | build the binary to `build/woca` |
| `make test` / `make race` | unit tests (optionally with the race detector) |
| `make lint` | golangci-lint |
| `make install` | install to `/usr/local/bin` |

## Quickstart

Point woca at a provider (persisted, encrypted at rest):

```console
$ woca config set provider glm
$ woca config set glm-api-key <key>
```

or export `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` and skip configuration
entirely.

### One-shot

```bash
woca -p "explain this repo's build system"
woca -p "fix the failing test" --dangerously-skip-permissions   # use with care
```

### Interactive TUI / REPL

```bash
woca                    # bubbletea TUI on a terminal; line REPL otherwise
woca --resume last      # continue your most recent session
```

Press `esc` to interrupt a run, `pgup`/`pgdown` to scroll, type `!cmd` to
run a shell command into the conversation. See
[Slash commands](#slash-commands).

### Web UI

```bash
woca serve              # http://127.0.0.1:3484
```

The server embeds the frontend and exposes the agent over an SSE API —
create sessions, start runs, watch streaming events, resolve permission
prompts, browse workspace files, and manage settings. From an interactive
session, `/web` starts it on demand (`/web <port>`, `/web off`,
`web status`); `woca web start/stop/restart/status` manages it as a
background process.

## Providers & models

| Provider | API | Default model | Notes |
|----------|-----|---------------|-------|
| `anthropic` (default) | Anthropic Messages | `claude-sonnet-4-5` | or OAuth login: `woca login` |
| `openai` | OpenAI Chat Completions | `gpt-4o` | any compatible `--base-url` gateway |
| `glm` | GLM (Anthropic-compatible endpoint) | `glm-4.6` | GLM Coding Plan quota display supported |
| `deepseek` | DeepSeek (Anthropic-compatible endpoint) | `deepseek-chat` | balance display supported |

Any OpenAI-compatible gateway works via `--base-url`
(`OPENAI_BASE_URL` / `ANTHROPIC_BASE_URL` or the persisted setting). Model
fallback (`--fallback-model`), thinking levels (`/thinking`), and per-model
reasoning-effort mapping are wired through the same router.

## Permissions

| Mode | Read-only / meta / web | File edits | Shell |
|------|------------------------|------------|-------|
| `default` | allowed | ask | ask |
| `acceptEdits` | allowed | allowed | ask |
| `bypassPermissions` | allowed | allowed | allowed |
| `readOnly` / `plan` | allowed | denied | denied |

Read-only classification is **input-sensitive**: `Bash("git status")`
auto-allows while `Bash("rm -rf …")` asks. Explicit rules override the mode:

```json
{
  "permissions": {
    "allow": ["Bash(git *)", "Read"],
    "deny":  ["Bash(rm *)"],
    "ask":   []
  }
}
```

Precedence is `deny` → `ask` → `allow` → mode default. Subagents are
fail-closed: an un-granted write permission surfaces as *permission denied*
instead of prompting.

## Architecture

```
cmd/woca                 cobra CLI: flags, config, REPL, one-shot, serve
internal/agent           the coding-agent graph (langchain-golang/langgraph
                         runtime): gate, plan mode, streaming execution,
                         recovery, compaction, teams, goals
internal/lgcp            durable checkpoint savers (file/sqlite/postgres)
                         for the langgraph runtime
internal/tools           tool set + registry + permission categories
internal/permissions     permission modes, rules, read-only classifier
internal/model           provider router: retry, fallback, invocation
internal/runner          streaming run loop + interrupt handling + rendering
internal/console         slash commands shared by TUI, REPL, and web
internal/tui             bubbletea terminal UI
internal/server          web server: sessions, runs, SSE, settings API
internal/session         JSONL transcript persistence
internal/compact         auto-compaction / summarization
internal/tokens          CJK-aware token estimator
internal/skills          SKILL.md loader (layered: flag > workspace > user)
internal/agents          subagent definitions (.woca/agents/*.md)
internal/hooks           hook runner (9 event groups)
internal/mcp             MCP client (stdio + streamable HTTP, OAuth)
internal/plugins         marketplace plugins (skills/agents bundles)
internal/pluginrt        capability-plugin runtimes (browser, pdf)
internal/settings        settings database + encrypted secrets
internal/quota           provider quota/usage display
internal/merge           layered configuration merge
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

The `tools` node runs a per-call **gate** (permissions, AskUserQuestion,
plan approval) sequentially — so interrupts pause deterministically — then
executes allowed calls concurrently. Pre/Post tool-use hooks wrap every
call; a Pre hook exiting `2` denies the tool, a Post hook exiting `2` feeds
its output back to the model. State is a single `messages` channel with an
`add_messages` reducer, so streaming deltas, tool results, and interrupts
all compose.

### Goal mode

`/goal <objective>` switches the agent into an autonomous loop expressed as
its own graph on top of the agent graph:

```
START ─▶ assess ─▶ execute ─▶ evaluate ─┬─ active   ─▶ assess (next cycle)
                                        └─ terminal ─▶ END
```

Each cycle plans (assess), runs a full nested agent turn (execute), then
**verifies with skepticism** (evaluate) — the agent may *claim* completion,
but only verified evidence completes the goal. Budgets (`--cycles`,
`--tokens`), idle auto-continue, and mid-cycle interrupt/resume apply; goal
state persists per session and survives restarts. Manage the loop with
`/goal status`, `/goal pause|resume`, `/goal budget`, `/goal clear`; the web
UI shows cycle/token meters and per-cycle verdicts live.

## Configuration

Settings live in a database under `~/.woca` — SQLite by default (pure Go,
no cgo), PostgreSQL optional via DSN. Credentials and DSNs are stored
**encrypted at rest** (AES-256-GCM; master key in `~/.woca/secret.key`,
mode `0600`). Three entrances share one database:

```console
$ woca config set provider openai
$ woca config set openai-api-key sk-...        # stored encrypted
$ woca config list
```

- **Terminal**: `woca config get|set|list`
- **Interactive**: `/settings`, `/memory list|put|get|del`
- **Web UI**: settings panel (`GET`/`PUT /api/settings`)

Key resolution order: `--api-key` flag → `ANTHROPIC_API_KEY` /
`OPENAI_API_KEY` → database (encrypted) → OAuth token. Graph checkpoints
and the memory store each pick a backend (`sqlite`, `postgres`, `file`,
`memory`, `none`).

Important flags:

| Flag | Description |
|------|-------------|
| `-p, --prompt` | one-shot prompt (runs non-interactively) |
| `--model` / `--provider` | model name and provider |
| `--base-url` / `--api-key` | provider endpoint and credential |
| `--permission-mode` | `default` \| `acceptEdits` \| `bypassPermissions` \| `readOnly` \| `plan` |
| `--workdir` | working directory (default: cwd) |
| `--mcp` | MCP server as `name=command` or `name=https://…` (repeatable) |
| `--fallback-model` | fallback on exhausted retries |
| `--max-turns` | cap model rounds per turn |
| `--auto-compact` | token-estimate compaction threshold (`0` disables) |
| `--resume` | resume a transcript (or `last`) |
| `--db` | settings database: sqlite path, `postgres://` DSN, or `none` |
| `--checkpoint-dir` | persist graph checkpoints for cross-process resume |

## Sessions, memory & undo

- **Sessions** persist as JSONL transcripts under `~/.woca/sessions`,
  grouped by workspace; `/rename` snapshots, `/branch` forks,
  `/rewind` jumps to an earlier point, `/export` dumps markdown or
  resumable JSON, and `--resume <name|last>` continues anywhere.
- **Memory**: with a memory backend configured, each turn prefetches
  memories relevant to your message and attaches them after tool rounds;
  `/memory put` writes long-term context that reaches the model later.
- **Instruction files**: `CLAUDE.md` and `AGENTS.md` are injected from the
  user level down through every directory from the git root to your
  working directory.
- **Undo**: `Write`/`Edit` snapshots are kept per session; `/undo` /
  `/redo` revert and re-apply the last change (repeatable). Changes made
  by `Bash` or outside woca are not tracked.

## Multi-agent

### Subagents

`Task` spawns isolated subagents with their own context window and tool
subset; `TaskOutput`/`TaskStop` manage them. Define custom subagents as
markdown files:

```markdown
<!-- .woca/agents/reviewer.md -->
---
name: reviewer
description: Reviews diffs for correctness and style issues
tools: [Read, Grep, Glob]
---
You are a critical code reviewer. Report only concrete, actionable issues.
```

The model then calls `Task(subagent_type="reviewer", prompt=…)`.

### Agent teams

For parallel work, teams share a worktree and coordinate through a shared
task list with ownership (`TaskUpdate` claims atomically; `DependsOn`
ordering) and message routing (`SendMessage` pokes idle teammates).
Teammates run autonomous loops with per-teammate timeouts; `/team` shows
membership, tasks, and inboxes.

## Extending woca

### Skills

Drop a `SKILL.md` with YAML frontmatter under `.woca/skills/<name>/`:

```markdown
---
name: commit
description: Write a conventional commit message
---
Write a conventional-commit message for the current diff.
```

The model discovers it and can call `Skill(skill="commit")`. Skills resolve
layered: workspace copies override user-level `~/.woca/skills`.

### Custom subagents

See [Subagents](#subagents). Definitions resolve from `.woca/agents/`
(workspace) over `~/.woca/agents/` (user).

### Hooks

Configure lifecycle hooks in `~/.woca/settings.json` or
`.woca/settings.json`:

```json
{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash(git *)", "hooks": [{"type": "command", "command": "echo 'git command incoming'"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "cat"}]}
    ]
  }
}
```

Nine event groups are supported (`PreToolUse`, `PostToolUse`,
`UserPromptSubmit`, `Stop`, …). Each hook receives a JSON event on stdin;
stdout is fed back as context. A `PreToolUse` hook exiting `2` denies the
tool; a `PostToolUse` hook exiting `2` blocks continuation.

### MCP servers

```bash
woca --mcp "filesystem=node /path/to/server.js /tmp" \
     --mcp "search=https://mcp.example.com/mcp"
```

Tools appear as `mcp__<server>__<tool>`, plus resources
(`mcp__<server>__resources`) and prompts. Both stdio (newline JSON-RPC) and
streamable HTTP (POST + JSON/SSE, `Mcp-Session-Id`) transports are
supported; OAuth-protected servers are authorized once with
`woca mcp auth <name> <endpoint>` (tokens stored encrypted). Server
definitions resolve layered: `--mcp` flag > workspace
`.woca/mcp.json` > user `~/.woca/mcp.json`, and tools added mid-session are
picked up between rounds.

### Capability plugins

Managed, versioned tool bundles installed on demand into
`~/.woca/plugins`:

- **browser** — hosted browser automation (navigation, clicks, forms,
  screenshots) for web testing and research
- **pdf** — a sandboxed Python environment for PDF generation and
  processing

Browse and install from the settings panel (`/plugins`), which provisions
the pinned runtime and verifies SHA-256 checksums.

### Reuse your existing setup

woca interoperates with common agent conventions: it reads `CLAUDE.md` /
`AGENTS.md` instruction files, discovers skills from `.claude/skills`, and
— from the web settings panel — can import skills and MCP server
definitions from an existing `~/.claude` setup in one click. If you already
run agent tooling, woca picks up your configuration instead of asking you
to rewrite it.

## Slash commands

Available in the TUI, REPL, and web chat input (prefix `/`, with
completion):

| Command | Description |
|---------|-------------|
| `/help` | list available commands |
| `/status` | version, provider/model, permission mode, run stats |
| `/permissions [mode]` | show or set the permission mode |
| `/model [name]` | show or switch the model at runtime |
| `/thinking [level]` | reasoning effort: off \| low \| medium \| high \| tokens |
| `/quota` | remaining quota for subscription plans |
| `/cost` | cumulative token usage and estimated cost |
| `/clear` | start a fresh conversation |
| `/compact` | summarize the conversation to free context |
| `/context` | context-window usage breakdown |
| `/todos`, `/tasks` | todo list / shared task list |
| `/skills`, `/mcp`, `/agents`, `/plugins` | manage extensions |
| `/undo`, `/redo` | revert / re-apply the last file edit |
| `/rewind`, `/branch`, `/rename`, `/export` | session history navigation |
| `/goal …` | autonomous goal loop (set/status/pause/resume/budget/clear) |
| `/team` | agent team status |
| `/memory …` | inspect and edit the memory store |
| `/settings …` | persisted settings |
| `/init` | generate an `AGENTS.md` project guide |
| `/doctor` | check config, credentials, storage, hooks, permissions |
| `/theme`, `/vim`, `/keybindings` | UI preferences |
| `/web [port\|off\|status]` | embedded web UI control |
| `/output-style` | default \| explanatory \| learning |

## HTTP API

The web server exposes the agent over REST + SSE:

| Endpoint | Description |
|----------|-------------|
| `POST /api/sessions` | create a session (persisted immediately) → `{"session_id":"…","workspace":"…"}` |
| `GET /api/sessions` | list persisted sessions |
| `GET /api/sessions/{id}` | fetch a full transcript |
| `DELETE /api/sessions/{id}` | delete a session (tombstoned against revival) |
| `POST /api/runs` | start a run: `{"session_id":"…","prompt":"…"}` |
| `GET /api/runs/{id}/events` | SSE: `delta` / `reasoning` / `tool_call` / `tool_result` / `goal` / `goal_note` / `interrupt` / `done` / `error` (goal runs stream cycle narration as `goal_note` and end with one unified `delta` reply) |
| `POST /api/runs/{id}/answer` | resolve an interrupt: `{"approve":true}` or `{"answers":[…]}` |
| `POST /api/runs/{id}/input` | queue mid-turn input |
| `POST /api/runs/{id}/cancel` | cancel a run |
| `GET`/`PUT /api/settings` | read/update persisted settings |

## Observability

Enable the observability log (`--debug-log`, the web settings panel, or
`/debug-log on`) for full harness logging under `~/.woca/logs/`,
size-rotated:

```
logs/woca.log                            run lifecycle, model usage, summaries
logs/ws-<name>-<hash8>/<session>.jsonl   one JSON event per line
logs/ws-<name>-<hash8>/<session>/<run>.json   message-level transcript dump
```

Every graph-node dispatch is recorded with phase, duration, and state
summary; in the web UI each assistant bubble gains a debug action that
copies the exact log locations for troubleshooting.

## Development

```bash
make web        # build the frontend (required once before go build)
make build      # build the binary
make test       # unit tests
make race       # tests with the race detector
make lint       # golangci-lint
```

The frontend is a Vue 3 + TypeScript + Vite SPA under [`web/`](web/README.md);
the demo (`npm run dev:demo`) previews the whole UI with mock data. The
graph runtime, agent loop, permission gate, hooks, compaction, skills, MCP
client, and web server are all covered by unit tests — the project is
built test-first.

See [CONTRIBUTING.md](CONTRIBUTING.md) for code style and PR guidelines.

## Contributing

Issues and pull requests are welcome. For anything larger than a bug fix,
please open an issue first to discuss the design. See
[CONTRIBUTING.md](CONTRIBUTING.md).

## License

woca is released under the [MIT License](LICENSE). It is built on
[langchain-golang](https://github.com/ProjAnvil/langchain-golang),
[bubbletea](https://github.com/charmbracelet/bubbletea), and
[cobra](https://github.com/spf13/cobra).

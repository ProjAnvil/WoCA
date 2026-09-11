# woca-agent-sdk (Python)

Python SDK for the [woca](https://github.com/projanvil/woca) agent harness. It drives the
`woca` CLI as an asyncio child process (`woca -p "<prompt>" --output-format stream-json`)
and exposes the NDJSON event stream as an async iterator of dicts — the same architecture
as the Claude Agent SDK.

Zero runtime dependencies. Requires Python >= 3.10.

## Install

```bash
pip install woca-agent-sdk
```

You also need the `woca` binary itself (see Binary resolution below).

## Minimal example

```python
import asyncio
from woca_agent_sdk import WocaOptions, query

async def main():
    async for ev in query(
        "List the Go files in this repo",
        WocaOptions(model="gpt-4o", permission_mode="bypassPermissions"),
    ):
        if ev["type"] == "assistant":
            print(ev["message"])
        elif ev["type"] == "result":
            print(ev["subtype"], ev.get("usage", {}).get("total_tokens"))

asyncio.run(main())
```

Closing the iterator early (e.g. `break` out of `async for`, or `agen.aclose()`) sends
SIGTERM to the child process and awaits its exit, so no orphan woca process is left behind.

## Options

| Option | CLI flag | Description |
| --- | --- | --- |
| `model` | `--model` | Model name |
| `provider` | `--provider` | LLM provider |
| `api_key` | `--api-key` | Provider API key |
| `base_url` | `--base-url` | Provider base URL |
| `workdir` | `--workdir` (+ child `cwd`) | Working directory for the agent |
| `system_prompt` | `--system-prompt` | Appended to the system prompt |
| `permission_mode` | `--permission-mode` | `default` \| `acceptEdits` \| `bypassPermissions` \| `readOnly` \| `plan` |
| `max_turns` | `--max-turns` | Max agent loop turns |
| `max_tokens` | `--max-tokens` | Max output tokens per request |
| `thinking_budget` | `--thinking-budget` | Extended-thinking token budget |
| `temperature` | `--temperature` | Sampling temperature |
| `mcp_servers` | `--mcp` (repeated) | MCP server specs |
| `allow_commands` | `--allow-command` (repeated) | Shell commands to auto-allow |
| `deny_commands` | `--deny-command` (repeated) | Shell commands to deny |
| `db` | `--db` | Session store (`none` disables persistence) |
| `fallback_model` | `--fallback-model` | Fallback model on provider failure |
| `path_to_woca` | — | Explicit path to the woca binary |

## Events and result shape

The SDK emits the wire format of `woca --output-format stream-json` unchanged (snake_case
fields, matching the Go `sdk` package), as plain dicts:

- `{"type": "assistant", "message": ...}` — assistant turn. Note: the **event** `type` is
  `"assistant"`, but `message["role"]` is the raw langchain-golang value (`"ai"` for
  assistant messages, `"tool"` for tool results), not `"assistant"`.
- `{"type": "user", "message": ...}` — tool results and other user-side messages
  (`message["role"] == "tool"`, `message["tool_call_id"]` set).
- `{"type": "result", "subtype": "success" | "error", "result"?, "usage"?,
  "total_cost_usd"?, "num_turns"?, "is_error"?, "error"?}` — terminal event; always the
  last item yielded.

If the process exits non-zero without a result event, iteration raises
`RuntimeError("woca exited with exit code N and no result event")`.

## Permission modes

`permission_mode` maps to woca's tool-permission system. `default` prompts a human for
risky actions — which can stall a headless run — so automation typically needs
`bypassPermissions`. **Warning:** `bypassPermissions` lets the agent run any tool (shell
commands, file writes) without confirmation. Only use it inside a sandboxed environment
(container, VM, disposable worktree) and never on a machine with unprotected credentials
or production access. A safer middle ground is `acceptEdits` (auto-accept file edits,
still confirm shell commands) or `readOnly`.

## Binary resolution

The woca executable is resolved in this order:

1. `WocaOptions.path_to_woca` (explicit path wins)
2. `$WOCA_PATH` environment variable
3. `woca` from `$PATH`

If none is found, `query` raises `FileNotFoundError`. The child process inherits
`os.environ`, and its stderr is streamed to the parent's stderr so diagnostics stay
visible.

## Development

```bash
pip install pytest
python -m pytest tests/ -v   # runs against tests/fake_woca.sh (no real woca needed)
```

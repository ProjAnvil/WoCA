# woca-agent-sdk

TypeScript SDK for the [woca](https://github.com/projanvil/woca) agent harness. It drives the
`woca` CLI as a child process (`woca -p "<prompt>" --output-format stream-json`) and exposes the
NDJSON event stream as an async iterable — the same architecture as the Claude Agent SDK.

Zero runtime dependencies. Requires Node.js >= 18.

## Install

```bash
npm install woca-agent-sdk
```

You also need the `woca` binary itself (see Binary resolution below).

## Minimal example

```ts
import { query } from "woca-agent-sdk";

const q = query({
  prompt: "List the Go files in this repo",
  options: { model: "gpt-4o", permissionMode: "bypassPermissions" },
});

for await (const msg of q) {
  if (msg.type === "assistant") console.log(msg.message);
}

const res = await q.result; // WocaResult: usage, total_cost_usd, num_turns, ...
console.log(res.subtype, res.usage?.total_tokens);
```

Cancel a running query (sends SIGTERM to the child process), or wire an `AbortSignal`:

```ts
q.cancel();
// or: query({ prompt, options: { signal: abortController.signal } })
```

## Options

| Option | CLI flag | Description |
| --- | --- | --- |
| `model` | `--model` | Model name |
| `provider` | `--provider` | LLM provider |
| `apiKey` | `--api-key` | Provider API key |
| `baseUrl` | `--base-url` | Provider base URL |
| `workdir` | `--workdir` (+ child `cwd`) | Working directory for the agent |
| `systemPrompt` | `--system-prompt` | Override system prompt |
| `permissionMode` | `--permission-mode` | `default` \| `acceptEdits` \| `bypassPermissions` \| `readOnly` \| `plan` |
| `maxTurns` | `--max-turns` | Max agent loop turns |
| `maxTokens` | `--max-tokens` | Max output tokens per request |
| `thinkingBudget` | `--thinking-budget` | Extended-thinking token budget |
| `temperature` | `--temperature` | Sampling temperature |
| `mcpServers` | `--mcp` (repeated) | MCP server specs |
| `allowCommands` | `--allow-command` (repeated) | Shell commands to auto-allow |
| `denyCommands` | `--deny-command` (repeated) | Shell commands to deny |
| `db` | `--db` | Session store (`none` disables persistence) |
| `fallbackModel` | `--fallback-model` | Fallback model on provider failure |
| `pathToWoca` | — | Explicit path to the woca binary |
| `signal` | — | `AbortSignal`; aborting cancels the query |

## Events and result shape

The SDK emits the wire format of `woca --output-format stream-json` unchanged (snake_case fields,
matching the Go `sdk` package):

- `{ type: "assistant", message }` — assistant turn. Note: the **event** `type` is
  `"assistant"`, but `message.role` is the raw langchain-golang value (`"ai"` for assistant
  messages, `"tool"` for tool results), not `"assistant"`.
- `{ type: "user", message }` — tool results and other user-side messages
  (`message.role === "tool"`, `message.tool_call_id` set).
- `{ type: "result", subtype: "success" | "error", result?, usage?, total_cost_usd?, num_turns?,
  is_error?, error? }` — terminal event; also resolves `q.result`.

If the process exits non-zero without a result event, iteration and `q.result` reject with
`woca exited with exit code N and no result event`. `q.cancel()` rejects `q.result` with
`woca query cancelled`.

## Permission modes

`permissionMode` maps to woca's tool-permission system. `default` prompts a human for risky
actions — which can stall a headless run — so automation typically needs `bypassPermissions`.
**Warning:** `bypassPermissions` lets the agent run any tool (shell commands, file writes)
without confirmation. Only use it inside a sandboxed environment (container, VM, disposable
worktree) and never on a machine with unprotected credentials or production access. A safer
middle ground is `acceptEdits` (auto-accept file edits, still confirm shell commands) or
`readOnly`.

## Binary resolution

The woca executable is resolved in this order:

1. `options.pathToWoca` (explicit path wins)
2. `$WOCA_PATH` environment variable
3. `woca` from `$PATH`

The child process inherits `process.env`, and its stderr is streamed to the parent's stderr so
diagnostics stay visible.

## Development

```bash
npm install
npm test   # builds with tsc, then runs node:test against test/fake-woca.sh
```

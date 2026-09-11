# woca Go SDK

Package `sdk` is the public, in-process Go SDK for the woca agent harness.
One `Query` call assembles the same agent the CLI runs — model, tools, hooks,
skills, MCP servers, permissions — and streams its progress as typed events.
Embedding `sdk` in your own Go program is equivalent to running
`woca -p "<prompt>"` from a subprocess, without the subprocess.

```go
import "github.com/projanvil/woca/sdk"
```

## Minimal example

```go
s, err := sdk.Query(context.Background(), "list the files in this repo",
	sdk.WithModel("claude-sonnet-4-5"),
	sdk.WithPermissionMode("bypassPermissions"),
	sdk.WithMaxTurns(10),
)
if err != nil {
	panic(err)
}
defer s.Close()
for ev := range s.Events() {
	switch ev.Type {
	case sdk.EventAssistant:
		fmt.Println("assistant:", ev.Message.Content)
	case sdk.EventResult:
		fmt.Println("done:", ev.Result.Subtype, "cost:", ev.Result.CostUSD)
	}
}
```

The event stream ends with exactly one `EventResult`, then closes. Drain
`Events()` (or call `Close`) so the run goroutine does not leak.

## Options

`Query` takes functional options. Each maps to the equivalent `woca` CLI flag
(same semantics as `woca -p ...`):

| Option | Options field | CLI flag |
|---|---|---|
| `WithModel(string)` | `Model` | `--model` |
| `WithProvider(string)` | `Provider` (anthropic \| openai \| glm \| deepseek) | `--provider` |
| `WithAPIKey(string)` | `APIKey` | `--api-key` |
| `WithBaseURL(string)` | `BaseURL` | `--base-url` |
| `WithWorkdir(string)` | `Workdir` | `--workdir` |
| `WithSystemPrompt(string)` | `ExtraPrompt` (appended to the system prompt) | `--system-prompt` |
| `WithPermissionMode(string)` | `PermissionMode` (default \| acceptEdits \| bypassPermissions \| readOnly \| plan) | `--permission-mode` |
| `WithMaxTurns(int)` | `MaxTurns` | `--max-turns` |
| `WithMaxTokens(int)` | `MaxTokens` | `--max-tokens` |
| `WithThinkingBudget(int)` | `ThinkingBudget` | `--thinking-budget` |
| `WithTemperature(float64)` | `Temperature` | `--temperature` |
| `WithMCPServers(...string)` | `MCPServers` (name=command or name=http(s)://endpoint) | `--mcp` |
| `WithAllowCommands(...string)` | `AllowCommands` | `--allow-command` |
| `WithDenyCommands(...string)` | `DenyCommands` | `--deny-command` |
| `WithDB(string)` | `DB` (sqlite path, postgres:// DSN, or "none") | `--db` |
| `WithFallbackModel(string)` | `FallbackModel` | `--fallback-model` |
| `WithCheckpointDir(string)` | `CheckpointDir` | `--checkpoint-dir` |
| `WithInterruptHandler(func)` | `OnInterrupt` | — (no CLI equivalent) |

The zero `Options` is the CLI's default posture with headless-safe interrupt
handling. Embedded runs pin the log level to `error` and discard log output so
the agent cannot spam the host process.

## Interrupts

Without an interrupt handler the SDK matches the CLI's non-interactive
posture: permission requests are denied (`"n"`), `ask_user` questions get
empty answers, and plan approvals are rejected (`"n"`).

Set `WithInterruptHandler` to override. The handler receives an
`sdk.Interrupt` (exactly one of `Permission` / `AskUser` / `Plan` set, per
`Kind`) and returns the graph resume value:

- `permission` and `plan_approval`: return `"y"` or `"n"`.
- `ask_user`: return `sdk.AskUserAnswer{Answers: []string{...}}` — one answer
  per question, in question order.

## Resource lifecycle

Every `Session` holds an agent, a plugin manager, and build-time teardown
hooks. **Always `defer s.Close()`** after a successful `Query`. `Close`
cancels the run if it is still going, waits for the run goroutine, and
releases everything. It is safe to call twice.

## Wire parity with stream-json

`Event.WireJSON()` serializes an event into the flattened stream-json shape
emitted by `woca -p --output-format stream-json` — same keys and values (key
order may differ), so one parser handles both the CLI subprocess output and
the in-process SDK. This is the format `eval/woca_agent.py` consumes.

## Known limitations

- For anthropic streaming runs, token usage is always reported as zero. This
  is a gap in the dependency layer: langchain-golang v0.6.3 only delivers
  usage through callbacks, which streaming runs do not receive.

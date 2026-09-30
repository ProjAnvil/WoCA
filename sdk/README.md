# woca Go SDK

Package `sdk` is the public, in-process Go SDK for the woca agent harness.
One `Query` call assembles the same agent the CLI runs — model, tools, hooks,
skills, MCP servers, permissions — and streams its progress as typed events.
Embedding `sdk` in your own Go program is equivalent to running
`woca -p "<prompt>"` from a subprocess, without the subprocess.

```go
import "github.com/projanvil/woca/sdk"
// optional native Langfuse reporting:
import "github.com/projanvil/woca/sdk/langfuse"
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
		if ev.Result.CostUSD != nil {
			fmt.Println("cost:", *ev.Result.CostUSD)
		}
		fmt.Println("done:", ev.Result.Subtype)
	}
}
```

The event stream ends with exactly one `EventResult`, then closes. Drain
`Events()` (or call `Close`) so the run goroutine does not leak.
`Result.CostUSD` is a `*float64`; nil means the model has no pricing data.

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
| `WithPlugins(...Plugin)` | `Plugins` | — (see Plugins below) |
| `WithTelemetry(...TelemetrySink)` | `Telemetry` | — (see Telemetry below) |

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
- `subagent_permission`: a foreground subagent's permission pause, carrying
  the subagent name in `Subagent` and the inner request in `Permission`;
  answer with `"y"` or `"n"`. The default handler auto-denies it.

## Plugins

`WithPlugins` attaches processing plugins to the run — the in-process
extension surface for embedding apps (the settings-file hooks stay available
for end users). Plugins are injected at construction time and observe the
run lifecycle in stream order:

```go
type auditPlugin struct{}

func (auditPlugin) Name() string { return "audit" }
func (auditPlugin) Observe(_ context.Context, ev sdk.RunEvent) {
	switch ev.Kind {
	case sdk.RunEventStart: // ev.Prompt, ev.SessionID, ev.RunID
	case sdk.RunEventMessage: // ev.Message: assistant text/tool calls, tool results
	case sdk.RunEventEnd: // ev.Result: the terminal envelope (usage, cost)
	}
}

s, err := sdk.Query(ctx, "…", sdk.WithPlugins(auditPlugin{}))
```

`Observe` is called sequentially from the run goroutine — keep it fast or
dispatch internally. Every `RunEvent` carries the run's `SessionID`/`RunID`
so plugins can correlate with their own records. Gating and transformation
hooks (tool guards, model hooks) are planned follow-ups; a plugin that
implements only `Name()` is accepted and ignored.

## Telemetry (and Langfuse)

`WithTelemetry` installs span sinks for everything the harness records —
LLM calls (`gen_ai.request.model`, usage tokens), tool invocations, skill
loads. Sinks are process-wide and deduped by identity, so passing the same
sink to every `Query` installs it exactly once:

```go
lf, err := langfuse.New(langfuse.Config{
	Host:      "https://cloud.langfuse.com",
	PublicKey: "pk-lf-…",
	SecretKey: "sk-lf-…",
})
if err != nil { … }
defer lf.Close() // flushes the pending batch

s, err := sdk.Query(ctx, "…",
	sdk.WithTelemetry(lf), // any sdk.TelemetrySink
)
```

The `sdk/langfuse` subpackage exports directly to Langfuse's OTLP endpoint
(`/api/public/otel/v1/traces`, Basic auth, real-time v4 ingestion) with the
spans grouped into one Langfuse trace per run and tagged with
`langfuse.session.id`. Delivery is batched, bounded, and lossy by design:
when Langfuse is unreachable, exports fail fast and drop — never affecting
the agent. `Stats()` reports `sent`/`dropped` accounting. The subpackage is
optional: programs that never import it carry no Langfuse code.

The enterprise stack keeps its collector bridge (credentials stay
platform-side there); the in-process sink serves the personal and embedded
forms.

## Resource lifecycle

Every `Session` holds an agent, a plugin manager, and build-time teardown
hooks. **Always `defer s.Close()`** after a successful `Query`. `Close`
cancels the run if it is still going, waits for the run goroutine, and
releases everything. It is safe to call twice.

## Wire parity with stream-json

`Event.WireJSON()` serializes an event into the flattened stream-json shape
emitted by `woca -p --output-format stream-json` — same keys and values (key
order may differ), so one parser handles both the CLI subprocess output and
the in-process SDK. This is the format `eval/woca_agent.py` consumes. One
caveat: `Message.Role` is normalized to `"assistant"` / `"user"` in SDK
events, while CLI stream-json message lines carry the raw langchain-golang
values (`"ai"` / `"tool"`); only the result envelope is fully identical.

## Known limitations

(none currently — token usage for streaming runs is reported correctly since
langchain-golang v0.7.0, which yields a terminal usage-only chunk)

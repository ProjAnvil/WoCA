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
| `WithLogOutput(io.Writer)` / `WithLogLevel(string)` | `LogOutput` / `LogLevel` | — (silent by default) |
| `WithSandbox(endpoint, key)` | `Sandbox` / `SandboxAPIKey` | routes every process side effect through the remote sandbox executor |

## Multi-turn sessions

`Query` is one prompt, one run. For conversations, keep a `Session` and
`Ask` repeatedly — the transcript carries between turns and the per-turn
result reports that turn's usage (not the session cumulative totals):

```go
s, err := sdk.NewSession(ctx,
	sdk.WithProvider("anthropic"), sdk.WithModel("claude-sonnet-4-5"),
	sdk.WithAPIKey(key), sdk.WithDB("none"),
)
if err != nil { panic(err) }
defer s.Close()

for _, q := range []string{"read the Makefile", "what did it say?"} {
	evs, err := s.Ask(ctx, q) // one turn, one event stream
	if err != nil { panic(err) }
	for ev := range evs {
		if ev.Type == sdk.EventResult {
			fmt.Println(q, "->", ev.Result.Text)
		}
	}
}
```

Only one turn may stream at a time: a second `Ask` while one is in flight
returns `ErrAskInProgress`; after `Close` it returns `ErrSessionClosed`.

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
so plugins can correlate with their own records.

A plugin may also implement the gating and transformation capabilities
(re-exported aliases of the harness types) in any combination:

```go
// Memory-style context injection (the pattern the built-in ladyM memory
// uses internally): attach retrieved context ahead of each model request.
type knowledgePlugin struct{ fetch func(prompt string) string }

func (knowledgePlugin) Name() string { return "knowledge" }
func (p knowledgePlugin) BeforeModel(_ context.Context, turn *sdk.ModelTurn) sdk.BeforeModelResult {
	return sdk.BeforeModelResult{PrependContext: p.fetch(turn.Prompt)}
}
func (knowledgePlugin) AfterModel(_ context.Context, _ *sdk.ModelTurn, r *sdk.ModelReply) *sdk.ModelReply {
	return nil // keep the reply unchanged
}

// Policy gate: deny dangerous tools, audit the rest.
type guardPlugin struct{}

func (guardPlugin) Name() string { return "guard" }
func (guardPlugin) BeforeTool(_ context.Context, call sdk.ToolCallInfo) sdk.ToolDecision {
	if call.Name == "Bash" {
		return sdk.AskTool() // escalate to the permission interrupt
	}
	return sdk.AllowTool()
}
func (guardPlugin) AfterTool(_ context.Context, call sdk.ToolCallInfo, res sdk.ToolResultInfo) sdk.ToolPostDecision {
	return sdk.ToolFeedback("audited")
}
```

Semantics worth knowing:

- **ToolGuard** — `BeforeTool` runs after the permission gate, before
  execution: `DenyTool(reason)` skips the call and feeds the reason to the
  model, `AskTool()` escalates to the permission interrupt (in the
  headless default posture that means denied — override with
  `WithInterruptHandler`). `AfterTool` feedback is appended to the tool
  result. Guards run before the settings-file hooks; any deny wins; and
  installing any guard disables speculative tool execution (no tool starts
  behind the guard's back).
- **ModelHook** — `BeforeModel` may `PrependContext` (request-scoped, a
  `<context>` block inserted after the system prompt; never persisted to
  the transcript) or return a `ModelReply` to skip the model call
  entirely. `AfterModel` may replace the reply's TEXT (tool calls survive;
  gate those with a ToolGuard). While any model hook is installed the
  model node buffers its stream (no live token streaming) so replacement
  stays consistent.
- **ModelWrapper** — `WrapModel(m sdk.ChatModel) sdk.ChatModel` wraps the
  model itself at assembly time and again on every tool rebind: the
  call-level capability for caching, retries, fan-out, or alternate
  backends (what the hooks cannot do — serving a cached reply without
  calling the model, for instance). Wrappers sit inside the chat-span
  decorator (a cache hit still records its Langfuse span; put
  `UsageMetadata` on synthesized replies), the first registered wrapper is
  the outermost, and implementations never need `BindTools` — the factory
  is re-applied after every bind.

```go
type cachingPlugin struct{ cache map[string]messages.Message }

func (c *cachingPlugin) Name() string { return "cache" }
func (c *cachingPlugin) WrapModel(m sdk.ChatModel) sdk.ChatModel {
	return &cachingModel{ChatModel: m, cache: c.cache}
}
```

(The `sdk.ChatModel` alias exposes the harness's model interface; the
wrapped types come from the same `langchain-golang` binding the SDK uses.)

## Telemetry

`WithTelemetry` installs custom span sinks for everything the harness
records — LLM calls (`gen_ai.request.model`, usage tokens), tool
invocations, skill loads:

```go
type mySink struct{}

func (mySink) Record(_ context.Context, sp sdk.Span) {
	fmt.Println("span:", sp.Name, sp.Kind)
}

s, err := sdk.Query(ctx, "…", sdk.WithTelemetry(mySink{}))
```

Sinks are process-wide and deduped by identity (the same sink may be
passed to every Query).

The built-in export path needs no code: woca speaks the OTLP protocol
natively, and the Langfuse plugin is enabled by configuration — the
`langfuse_*` settings keys (personal UI: settings → plugins) or the
`WOCA_LANGFUSE_HOST/PUBLIC_KEY/SECRET_KEY` environment variables — for
every form (CLI, serve, TUI, embedded). See the repository README's
observability section. Enterprise deployments configure it from the
console instead (individual users cannot).

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

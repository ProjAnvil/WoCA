# woca Agent SDK Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 提供 woca agent SDK:P0 Go in-process SDK(`sdk/` 包)+ P1 TS/Python 子进程 SDK(`sdk/typescript/`、`sdk/python/`)。

**Architecture:** Go SDK 是 `internal/agentbuild`(组合根)+ `internal/runner`(回合执行)的薄公开封装:把 `sdk.Options` 翻译成 `agentbuild.Options`,用 renderer 回调把 graph stream chunk 转成强类型事件经 channel 流出,最后合成 result 事件。TS/Python SDK 是 `woca -p --output-format stream-json` 的子进程包装:spawn → 逐行解析 NDJSON → 异步迭代器。

**Tech Stack:** Go(模块 `github.com/projanvil/woca`,仅复用现有 internal 包,零新依赖)、TypeScript(tsc 构建,node:test 测试,零运行时依赖)、Python(asyncio,pytest 测试,零运行时依赖)。

**Spec:** `docs/superpowers/specs/2026-09-12-woca-agent-sdk-design.md`

## Global Constraints

- **不改 `internal/sdkout` 的 wire 格式**:`eval/woca_agent.py` 依赖 stream-json 逐行格式;`sdkout` 包代码一行不动,用 parity 测试锁两边输出一致。
- **Go SDK 不新增任何第三方依赖**,只用 go.mod 已有模块与 stdlib。
- **sdk 导出 API 不得泄漏 internal 类型**(如 `agent.Agent`、`settings.Store`、`messages.Message` 不得出现在导出签名里)。
- TS SDK 唯一 devDependency 是 `typescript`;测试用 `node:test`,不引 vitest/tsx。
- Python SDK 零运行时依赖;测试用 `pytest`(dev 依赖),async 测试用 `asyncio.run()` 包裹,不引 pytest-asyncio。
- 无头 interrupt 默认行为与 `cmd/woca/main.go:681-727` 对齐:PermissionRequest → `"n"`,AskUserRequest → `agent.AskUserAnswer{}`,PlanApprovalRequest → `"n"`。
- 每个 Task 完成后跑 `go build ./... && go vet ./sdk/`(Go 任务);全部完成后跑项目既有测试确认零回归。

---

### Task 1: Go SDK 事件类型与 wire 转换

**Files:**
- Create: `sdk/event.go`
- Test: `sdk/event_test.go`

**Interfaces:**
- Consumes: `internal/agent`(`agent.MessagesFrom`、`agent.UsageTotals`)、`internal/cost`(`cost.Estimate`、`cost.Usage`)、`github.com/projanvil/langchain-golang/core/messages`(`messages.Message`,字段 `Role`、`Content`、`ToolCalls[]{ID,Name,Args}`、`ToolCallID`)。
- Produces(后续任务与 TS/Python schema 都依赖这些名字):
  - `type EventType string`,常量 `EventAssistant`/`EventUser`/`EventResult`
  - `type ToolCall struct { ID string; Name string; Args map[string]any }`
  - `type Message struct { Role string; Content string; ToolCallID string; ToolCalls []ToolCall }`
  - `type Usage struct { Requests, InputTokens, OutputTokens, TotalTokens, CacheReadTokens, CacheWriteTokens int }`(json tag 与 `agent.UsageTotals` 相同)
  - `type Result struct { Subtype, Text string; Usage Usage; CostUSD *float64; NumTurns int; IsError bool; ErrorText string }`
  - `type Event struct { Type EventType; Message *Message; Result *Result }`
  - `func MessageFrom(m messages.Message) *Message`
  - `func (e Event) WireJSON() ([]byte, error)` — 产出与 `sdkout` 完全相同的扁平 JSON 行

- [ ] **Step 1: 写失败测试**

`sdk/event_test.go`:

```go
package sdk

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/sdkout"
)

func TestMessageFromAssistantWithToolCalls(t *testing.T) {
	m := messages.Message{
		Role:    messages.RoleAI,
		Content: "thinking",
		ToolCalls: []messages.ToolCall{
			{ID: "tc1", Name: "Bash", Args: map[string]any{"command": "ls"}},
		},
	}
	got := MessageFrom(m)
	if got == nil || got.Role != "assistant" || got.Content != "thinking" {
		t.Fatalf("unexpected message: %+v", got)
	}
	if len(got.ToolCalls) != 1 || got.ToolCalls[0].Name != "Bash" || got.ToolCalls[0].Args["command"] != "ls" {
		t.Fatalf("tool calls mismatch: %+v", got.ToolCalls)
	}
}

// TestWireParityWithSdkout locks the Go SDK wire format to sdkout's
// stream-json lines (consumed by eval/woca_agent.py).
func TestWireParityWithSdkout(t *testing.T) {
	var buf strings.Builder
	w, err := sdkout.New(sdkout.FormatJSON, &buf)
	if err != nil {
		t.Fatal(err)
	}
	transcript := []messages.Message{
		{Role: messages.RoleSystem, Content: "sys"},
		{Role: messages.RoleHuman, Content: "hi"},
		{Role: messages.RoleAI, Content: "hello"},
	}
	usage := agent.UsageTotals{Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15}
	w.Final(transcript, usage, "unknown-model", 1, nil)

	ev := ResultEvent(transcript, Usage{
		Requests: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
	}, "unknown-model", 1, nil)
	wire, err := ev.WireJSON()
	if err != nil {
		t.Fatal(err)
	}

	var fromSdkout, fromSDK map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(buf.String())), &fromSdkout); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wire, &fromSDK); err != nil {
		t.Fatal(err)
	}
	if len(fromSdkout) != len(fromSDK) {
		t.Fatalf("key count differs: sdkout=%v sdk=%v", fromSdkout, fromSDK)
	}
	for k, v := range fromSdkout {
		got, ok := fromSDK[k]
		if !ok {
			t.Fatalf("sdk wire missing key %q (sdkout=%v)", k, v)
		}
		g1, _ := json.Marshal(v)
		g2, _ := json.Marshal(got)
		if string(g1) != string(g2) {
			t.Fatalf("key %q differs: sdkout=%s sdk=%s", k, g1, g2)
		}
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./sdk/ -run TestMessageFrom -v 2>&1 | head -5`
Expected: FAIL,`package github.com/projanvil/woca/sdk: no Go files` 或编译错误(目录尚不存在)

- [ ] **Step 3: 实现 `sdk/event.go`**

```go
// Package sdk is the public, in-process Go SDK for the woca agent harness:
// one Query call assembles the same agent the CLI runs (model, tools, hooks,
// skills, MCP, permissions) and streams its progress as typed events.
package sdk

import (
	"encoding/json"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/cost"
)

// EventType classifies a streamed event.
type EventType string

const (
	EventAssistant EventType = "assistant" // model message (text and/or tool calls)
	EventUser      EventType = "user"      // tool result message
	EventResult    EventType = "result"    // terminal envelope, always the last event
)

// ToolCall is one model-requested tool invocation.
type ToolCall struct {
	ID   string         `json:"id"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

// Message is an assistant or tool-result transcript message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// Usage mirrors agent.UsageTotals (token accounting for the whole run).
type Usage struct {
	Requests         int `json:"requests"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	TotalTokens      int `json:"total_tokens"`
	CacheReadTokens  int `json:"cache_read_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Result is the terminal envelope of a run.
type Result struct {
	Subtype   string   `json:"subtype"` // success | error
	Text      string   `json:"result,omitempty"`
	Usage     Usage    `json:"usage"`
	CostUSD   *float64 `json:"total_cost_usd,omitempty"`
	NumTurns  int      `json:"num_turns,omitempty"`
	IsError   bool     `json:"is_error,omitempty"`
	ErrorText string   `json:"error,omitempty"`
}

// Event is one streamed item: exactly one of Message / Result is set.
type Event struct {
	Type    EventType
	Message *Message
	Result  *Result
}

// MessageFrom converts an internal transcript message to the public shape.
// Role is normalized to the stream-json wire values ("assistant" / "user").
func MessageFrom(m messages.Message) *Message {
	msg := &Message{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
	for _, tc := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Name, Args: tc.Args})
	}
	return msg
}

// ResultEvent builds the terminal event from a finished run, mirroring
// sdkout.Writer.Final (usage, cost estimate, last AI text, error marking).
func ResultEvent(transcript []messages.Message, u agent.UsageTotals, model string, numTurns int, runErr error) Event {
	r := &Result{Usage: Usage{
		Requests:         u.Requests,
		InputTokens:      u.InputTokens,
		OutputTokens:     u.OutputTokens,
		TotalTokens:      u.TotalTokens,
		CacheReadTokens:  u.CacheReadTokens,
		CacheWriteTokens: u.CacheWriteTokens,
	}, NumTurns: numTurns}
	if est, known := cost.Estimate(model, cost.Usage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens, CacheWriteTokens: u.CacheWriteTokens,
	}); known {
		r.CostUSD = &est
	}
	if runErr != nil {
		r.Subtype = "error"
		r.IsError = true
		r.ErrorText = runErr.Error()
	} else {
		r.Subtype = "success"
		r.Text = LastAIText(transcript)
	}
	return Event{Type: EventResult, Result: r}
}

// LastAIText returns the content of the last non-empty assistant message.
func LastAIText(transcript []messages.Message) string {
	for i := len(transcript) - 1; i >= 0; i-- {
		if transcript[i].Role == messages.RoleAI && transcript[i].Content != "" {
			return transcript[i].Content
		}
	}
	return ""
}

// WireJSON serializes the event in the flattened stream-json shape, byte
--compatible with sdkout (the format eval/woca_agent.py parses).
func (e Event) WireJSON() ([]byte, error) {
	wire := map[string]any{"type": string(e.Type)}
	if e.Message != nil {
		data, err := json.Marshal(e.Message)
		if err != nil {
			return nil, err
		}
		wire["message"] = json.RawMessage(data)
	}
	if r := e.Result; r != nil {
		wire["subtype"] = r.Subtype
		if r.Text != "" {
			wire["result"] = r.Text
		}
		usage, err := json.Marshal(r.Usage)
		if err != nil {
			return nil, err
		}
		wire["usage"] = json.RawMessage(usage)
		if r.CostUSD != nil {
			wire["total_cost_usd"] = *r.CostUSD
		}
		if r.NumTurns != 0 {
			wire["num_turns"] = r.NumTurns
		}
		if r.IsError {
			wire["is_error"] = true
		}
		if r.ErrorText != "" {
			wire["error"] = r.ErrorText
		}
	}
	return json.Marshal(wire)
}
```

注意:parity 测试里 model 用 `"unknown-model"` 是为了让 `cost.Estimate` 返回 `known=false`,两边都不含 `total_cost_usd` 字段。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./sdk/ -v`
Expected: 两个测试 PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/event.go sdk/event_test.go
git commit -m "feat(sdk): public event types with stream-json wire parity"
```

---

### Task 2: Options 翻译与无头 interrupt 默认处理

**Files:**
- Create: `sdk/options.go`
- Create: `sdk/interrupt.go`
- Test: `sdk/options_test.go`

**Interfaces:**
- Consumes: `internal/agentbuild`(`agentbuild.Options`)、`internal/agent`(`agent.PermissionRequest`、`agent.AskUserRequest`、`agent.AskUserAnswer`、`agent.PlanApprovalRequest`)。
- Produces:
  - `type Options struct { ... }`(字段见下)
  - `type Option func(*Options)`;构造器 `WithModel(string)`、`WithProvider(string)`、`WithAPIKey(string)`、`WithBaseURL(string)`、`WithWorkdir(string)`、`WithSystemPrompt(string)`、`WithPermissionMode(string)`、`WithMaxTurns(int)`、`WithMaxTokens(int)`、`WithThinkingBudget(int)`、`WithTemperature(float64)`、`WithMCPServers(...string)`、`WithAllowCommands(...string)`、`WithDenyCommands(...string)`、`WithDB(string)`、`WithFallbackModel(string)`、`WithCheckpointDir(string)`、`WithInterruptHandler(func(Interrupt) (any, error))`
  - `type Interrupt struct{ Kind string; Permission *PermissionInterrupt; AskUser *AskUserInterrupt; Plan *PlanInterrupt }`(对外不泄漏 agent 类型)
  - `func (o Options) buildOptions(prompt string) agentbuild.Options`
  - `func (o Options) interruptHandler() runner.InterruptHandler`

- [ ] **Step 1: 写失败测试**

`sdk/options_test.go`:

```go
package sdk

import (
	"errors"
	"testing"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
)

func TestBuildOptionsTranslatesFields(t *testing.T) {
	o := Options{
		Model: "claude-sonnet-4-5", Provider: "anthropic", APIKey: "k",
		BaseURL: "https://x", Workdir: "/tmp/w", ExtraPrompt: "be brief",
		PermissionMode: "bypassPermissions", MaxTurns: 3, MaxTokens: 4096,
		ThinkingBudget: 1024, Temperature: 0.5,
		MCPServers: []string{"s=cmd"}, AllowCommands: []string{"git"},
		DenyCommands: []string{"rm -rf"}, DB: "none",
		FallbackModel: "gpt-4o", CheckpointDir: "/tmp/cp",
	}
	bo := o.buildOptions("do it")
	want := agentbuild.Options{
		Prompt: "do it", Model: "claude-sonnet-4-5", Provider: "anthropic",
		APIKey: "k", BaseURL: "https://x", MaxTokens: 4096, ThinkingBudget: 1024,
		Temperature: 0.5, Mode: "bypassPermissions", Workdir: "/tmp/w",
		ExtraPrompt: "be brief", LogLevel: "error", MCPServers: []string{"s=cmd"},
		FallbackModel: "gpt-4o", CheckpointDir: "/tmp/cp",
		AllowCommands: []string{"git"}, DenyCommands: []string{"rm -rf"},
		DB: "none", MaxTurns: 3,
	}
	if bo.Prompt != want.Prompt || bo.Model != want.Model || bo.Mode != want.Mode ||
		bo.DB != want.DB || bo.MaxTurns != want.MaxTurns || bo.LogLevel != want.LogLevel ||
		bo.Workdir != want.Workdir || bo.ExtraPrompt != want.ExtraPrompt ||
		bo.FallbackModel != want.FallbackModel || bo.CheckpointDir != want.CheckpointDir ||
		bo.Temperature != want.Temperature || bo.ThinkingBudget != want.ThinkingBudget ||
		len(bo.MCPServers) != 1 || len(bo.AllowCommands) != 1 || len(bo.DenyCommands) != 1 {
		t.Fatalf("translation mismatch: %+v", bo)
	}
}

func TestDefaultInterruptHandlerAutoDenies(t *testing.T) {
	h := (Options{}).interruptHandler()

	v, err := h(agent.PermissionRequest{ToolName: "Bash", Reason: "r"})
	if err != nil || v != "n" {
		t.Fatalf("permission: got %v, %v", v, err)
	}
	v, err = h(agent.AskUserRequest{})
	if err != nil {
		t.Fatal(err)
	}
	ans, ok := v.(agent.AskUserAnswer)
	if !ok || len(ans.Answers) != 0 {
		t.Fatalf("ask-user: got %#v", v)
	}
	v, err = h(agent.PlanApprovalRequest{Plan: "p"})
	if err != nil || v != "n" {
		t.Fatalf("plan: got %v, %v", v, err)
	}
	_, err = h(struct{}{})
	if err == nil {
		t.Fatal("unknown interrupt type must error")
	}
}

func TestCustomInterruptHandlerWins(t *testing.T) {
	sentinel := errors.New("denied by user code")
	o := Options{OnInterrupt: func(i Interrupt) (any, error) {
		if i.Kind != "permission" || i.Permission.ToolName != "Bash" {
			t.Fatalf("unexpected interrupt: %+v", i)
		}
		return nil, sentinel
	}}
	_, err := o.interruptHandler()(agent.PermissionRequest{ToolName: "Bash"})
	if !errors.Is(err, sentinel) {
		t.Fatalf("got %v", err)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./sdk/ -run TestBuildOptions -v`
Expected: FAIL 编译错误(`Options` 未定义)

- [ ] **Step 3: 实现 `sdk/options.go` 与 `sdk/interrupt.go`**

`sdk/options.go`:

```go
package sdk

import (
	"github.com/projanvil/woca/internal/agentbuild"
)

// Options configures one headless run. The zero value is the CLI's default
// posture with headless-safe interrupt handling (everything denied).
type Options struct {
	Model          string
	Provider       string // anthropic | openai | glm | deepseek
	APIKey         string
	BaseURL        string
	Workdir        string
	ExtraPrompt    string // appended to the system prompt
	PermissionMode string // default | acceptEdits | bypassPermissions | readOnly | plan
	MaxTurns       int
	MaxTokens      int
	ThinkingBudget int
	Temperature    float64
	MCPServers     []string // name=command or name=http(s)://endpoint
	AllowCommands  []string
	DenyCommands   []string
	DB             string // sqlite path, postgres:// DSN, or "none"
	FallbackModel  string
	CheckpointDir  string
	// OnInterrupt overrides the headless default (deny everything). The
	// returned value is the graph resume value — see package docs.
	OnInterrupt func(Interrupt) (any, error)
}

// Option mutates Options (functional-option style).
type Option func(*Options)

func WithModel(v string) Option            { return func(o *Options) { o.Model = v } }
func WithProvider(v string) Option         { return func(o *Options) { o.Provider = v } }
func WithAPIKey(v string) Option           { return func(o *Options) { o.APIKey = v } }
func WithBaseURL(v string) Option          { return func(o *Options) { o.BaseURL = v } }
func WithWorkdir(v string) Option          { return func(o *Options) { o.Workdir = v } }
func WithSystemPrompt(v string) Option     { return func(o *Options) { o.ExtraPrompt = v } }
func WithPermissionMode(v string) Option   { return func(o *Options) { o.PermissionMode = v } }
func WithMaxTurns(v int) Option            { return func(o *Options) { o.MaxTurns = v } }
func WithMaxTokens(v int) Option           { return func(o *Options) { o.MaxTokens = v } }
func WithThinkingBudget(v int) Option      { return func(o *Options) { o.ThinkingBudget = v } }
func WithTemperature(v float64) Option     { return func(o *Options) { o.Temperature = v } }
func WithMCPServers(v ...string) Option    { return func(o *Options) { o.MCPServers = v } }
func WithAllowCommands(v ...string) Option { return func(o *Options) { o.AllowCommands = v } }
func WithDenyCommands(v ...string) Option  { return func(o *Options) { o.DenyCommands = v } }
func WithDB(v string) Option               { return func(o *Options) { o.DB = v } }
func WithFallbackModel(v string) Option    { return func(o *Options) { o.FallbackModel = v } }
func WithCheckpointDir(v string) Option    { return func(o *Options) { o.CheckpointDir = v } }
func WithInterruptHandler(f func(Interrupt) (any, error)) Option {
	return func(o *Options) { o.OnInterrupt = f }
}

// buildOptions translates into the composition-root options. LogLevel is
// pinned to error: an embedded agent must not spam the host process.
func (o Options) buildOptions(prompt string) agentbuild.Options {
	return agentbuild.Options{
		Prompt:         prompt,
		Model:          o.Model,
		Provider:       o.Provider,
		APIKey:         o.APIKey,
		BaseURL:        o.BaseURL,
		MaxTokens:      o.MaxTokens,
		ThinkingBudget: o.ThinkingBudget,
		Temperature:    o.Temperature,
		Mode:           o.PermissionMode,
		Workdir:        o.Workdir,
		ExtraPrompt:    o.ExtraPrompt,
		LogLevel:       "error",
		MCPServers:     o.MCPServers,
		FallbackModel:  o.FallbackModel,
		CheckpointDir:  o.CheckpointDir,
		AllowCommands:  o.AllowCommands,
		DenyCommands:   o.DenyCommands,
		DB:             o.DB,
		MaxTurns:       o.MaxTurns,
	}
}
```

`sdk/interrupt.go`:

```go
package sdk

import (
	"fmt"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/runner"
)

// Interrupt is the public view of a graph pause: exactly one field is set.
type Interrupt struct {
	Kind       string // permission | ask_user | plan_approval
	Permission *PermissionInterrupt
	AskUser    *AskUserInterrupt
	Plan       *PlanInterrupt
}

type PermissionInterrupt struct {
	ToolName   string
	ToolArgs   map[string]any
	Reason     string
	IsReadOnly bool
}

type AskUserInterrupt struct {
	Questions []string // question texts; answers are []string in question order
}

type PlanInterrupt struct {
	Plan string
}

// interruptHandler returns the runner handler: the caller's OnInterrupt when
// set, else the headless default matching the CLI's non-interactive posture
// (deny permissions, empty answers, reject plans).
func (o Options) interruptHandler() runner.InterruptHandler {
	return func(value any) (any, error) {
		if o.OnInterrupt != nil {
			return o.OnInterrupt(interruptFrom(value))
		}
		switch value.(type) {
		case agent.PermissionRequest:
			return "n", nil
		case agent.AskUserRequest:
			return agent.AskUserAnswer{}, nil
		case agent.PlanApprovalRequest:
			return "n", nil
		default:
			return nil, fmt.Errorf("sdk: unknown interrupt value %T", value)
		}
	}
}

func interruptFrom(value any) Interrupt {
	switch v := value.(type) {
	case agent.PermissionRequest:
		return Interrupt{Kind: "permission", Permission: &PermissionInterrupt{
			ToolName: v.ToolName, ToolArgs: v.ToolArgs, Reason: v.Reason, IsReadOnly: v.IsReadOnly,
		}}
	case agent.AskUserRequest:
		qs := make([]string, 0, len(v.Questions))
		for _, q := range v.Questions {
			qs = append(qs, q.Question)
		}
		return Interrupt{Kind: "ask_user", AskUser: &AskUserInterrupt{Questions: qs}}
	case agent.PlanApprovalRequest:
		return Interrupt{Kind: "plan_approval", Plan: &PlanInterrupt{Plan: v.Plan}}
	default:
		return Interrupt{Kind: fmt.Sprintf("unknown:%T", value)}
	}
}
```

注意:实现时先确认 `tools.Question` 的文本字段名(`q.Question`);若不是,用实际字段名。自定义 handler 的返回值约定:permission/plan 返回 `"y"`/`"n"`,ask_user 返回 `agent.AskUserAnswer`——这一约定写进包注释即可,SDK 不再封装 resume 值类型(保持薄)。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./sdk/ -v`
Expected: 全部 PASS

- [ ] **Step 5: Commit**

```bash
git add sdk/options.go sdk/interrupt.go sdk/options_test.go
git commit -m "feat(sdk): options translation and headless interrupt defaults"
```

---

### Task 3: Query / Session 核心

**Files:**
- Create: `sdk/sdk.go`
- Test: `sdk/sdk_test.go`

**Interfaces:**
- Consumes: Task 1 的 `Event`/`MessageFrom`/`ResultEvent`,Task 2 的 `Options`/`buildOptions`/`interruptHandler`;`internal/agentbuild.BuildAgent(ctx, opts, log)` 返回 `*agentbuild.App`(字段 `Agent *agent.Agent`、`Cfg config.Config`、`PluginMgr *pluginrt.Manager`、`Teardown []func()`);`internal/runner`(`runner.New(agent, handler, renderer)`、`Runner.Run(ctx, history)`);`internal/logging`(`logging.ParseLevel`、`logging.NewConsole(io.Writer, level)`);`agent.MessagesFrom(state map[string]any)`;`agent.DecodeInterruptValue` 由 runner 内部调用,SDK 无需直接碰。
- Produces(对外主 API):
  - `func Query(ctx context.Context, prompt string, opts ...Option) (*Session, error)`
  - `type Session struct`,方法 `Events() <-chan Event`、`Close() error`

- [ ] **Step 1: 写失败测试(端到端,fake Anthropic server)**

`sdk/sdk_test.go`:

```go
package sdk

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeAnthropic answers every /v1/messages call with a one-line completion.
// Mirror the streaming SSE form the provider client expects; if the client
// requests non-streaming, the plain JSON body is equivalent. The reference
// pattern is captureAnthropic in internal/config/thinking_test.go.
func fakeAnthropic(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-sonnet-4-5","stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello from fake"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}

event: message_stop
data: {"type":"message_stop"}

`)
	}))
}

func TestQueryStreamsAndFinishes(t *testing.T) {
	srv := fakeAnthropic(t)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s, err := Query(ctx, "say hi",
		WithProvider("anthropic"),
		WithModel("claude-sonnet-4-5"),
		WithAPIKey("test-key"),
		WithBaseURL(srv.URL),
		WithDB("none"),
		WithWorkdir(t.TempDir()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	var sawAssistant bool
	var res *Result
	for ev := range s.Events() {
		switch ev.Type {
		case EventAssistant:
			sawAssistant = true
		case EventResult:
			res = ev.Result
		}
	}
	if res == nil {
		t.Fatal("no result event")
	}
	if res.IsError {
		t.Fatalf("run errored: %s", res.ErrorText)
	}
	if res.Text != "hello from fake" {
		t.Fatalf("result text = %q", res.Text)
	}
	if !sawAssistant {
		t.Fatal("no assistant event streamed")
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v", res.Usage)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./sdk/ -run TestQueryStreams -v`
Expected: FAIL 编译错误(`Query` 未定义)

- [ ] **Step 3: 实现 `sdk/sdk.go`**

```go
package sdk

import (
	"context"
	"io"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/runner"
)

// Session is one headless run. Events streams until the result event, then
// closes. Close cancels the run (if still going) and releases the agent,
// plugin manager, and build-time teardown hooks.
type Session struct {
	events chan Event

	cancel context.CancelFunc
	done   chan struct{}

	ap        *agentbuild.App
	closeOnce sync.Once
}

// Query starts one headless agent run and returns its session. The caller
// must drain Events (or call Close) to avoid leaking the run goroutine.
func Query(ctx context.Context, prompt string, opts ...Option) (*Session, error) {
	o := Options{}
	for _, fn := range opts {
		fn(&o)
	}

	level, err := logging.ParseLevel("error")
	if err != nil {
		return nil, err
	}
	log := logging.NewConsole(io.Discard, level)

	ap, err := agentbuild.BuildAgent(ctx, o.buildOptions(prompt), log)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	s := &Session{
		events: make(chan Event, 64),
		cancel: cancel,
		done:   make(chan struct{}),
		ap:     ap,
	}
	go s.run(runCtx, o, prompt)
	return s, nil
}

// Events returns the run's event stream (closed after the result event).
func (s *Session) Events() <-chan Event { return s.events }

// Close cancels the run and releases all resources. Safe to call twice.
func (s *Session) Close() error {
	s.cancel()
	<-s.done
	s.closeOnce.Do(func() {
		_ = s.ap.PluginMgr.Close()
		_ = s.ap.Agent.Close()
		for _, td := range s.ap.Teardown {
			td()
		}
	})
	return nil
}

// run drives the runner, converting stream chunks into events. It mirrors
// the CLI's one-shot path (cmd/woca/main.go run, the opts.Prompt branch):
// submit hooks prepend feedback, turns count tool messages + 1, the result
// event mirrors sdkout.Final.
func (s *Session) run(ctx context.Context, o Options, prompt string) {
	defer close(s.done)
	defer close(s.events)

	render := func(chunk lggraph.StreamChunk) {
		if chunk.Mode != lggraph.StreamUpdates {
			return
		}
		updates, _ := chunk.Payload.(map[string]any)
		for _, nodeUpdate := range updates {
			state, _ := nodeUpdate.(map[string]any)
			for _, m := range agent.MessagesFrom(state) {
				switch m.Role {
				case messages.RoleAI:
					if m.Content == "" && len(m.ToolCalls) == 0 {
						continue
					}
					s.send(ctx, Event{Type: EventAssistant, Message: MessageFrom(m)})
				case messages.RoleTool:
					s.send(ctx, Event{Type: EventUser, Message: MessageFrom(m)})
				}
			}
		}
	}

	a := s.ap.Agent
	r := runner.New(a, o.interruptHandler(), render)

	if fb, err := a.RunSessionStart(ctx); err == nil && fb != "" {
		s.ap.Log.Info().Str("feedback", fb).Msg("session-start hook feedback")
	}
	if fb, err := a.RunUserPromptSubmit(ctx, prompt); err == nil && fb != "" {
		prompt = fb + "\n\n" + prompt
	}

	history := []messages.Message{messages.Human(prompt)}
	result, runErr := r.Run(ctx, history)
	if endErr := a.RunSessionEnd(context.WithoutCancel(ctx), "oneshot"); endErr != nil && runErr == nil {
		runErr = endErr
	}

	turns := 1
	for _, m := range result {
		if m.Role == messages.RoleTool {
			turns++
		}
	}
	s.send(ctx, ResultEvent(result, a.Usage(), s.ap.Cfg.Model, turns, runErr))
}

// send delivers an event unless the run was cancelled.
func (s *Session) send(ctx context.Context, ev Event) {
	select {
	case s.events <- ev:
	case <-ctx.Done():
	}
}
```

注意:`turns` 计数处 CLI 用的是 `stripSystem(result)`(去掉 leading system 消息后再数 tool 消息);leading system 消息 Role 是 system 不是 tool,直接数 result 里 RoleTool 效果相同,但为了与 CLI 严格一致,若 result 非空且 `result[0].Role == messages.RoleSystem`,先 `result = result[1:]` 再数。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./sdk/ -v`
Expected: 全部 PASS。若 fake server 的 SSE 格式与 provider 客户端期望不符,按 `internal/config/thinking_test.go` 的 `captureAnthropic` 实际格式修正 fake 响应(这是本任务唯一需要现场校准的点)。

- [ ] **Step 5: Commit**

```bash
git add sdk/sdk.go sdk/sdk_test.go
git commit -m "feat(sdk): Query/Session headless run API"
```

---

### Task 4: Go SDK 文档(example + README)

**Files:**
- Create: `sdk/example_test.go`
- Create: `sdk/README.md`

**Interfaces:**
- Consumes: Task 3 的 `Query`/`Session`/`Event`。
- Produces: godoc 示例,包使用文档。

- [ ] **Step 1: 写 example**

`sdk/example_test.go`:

```go
package sdk_test

import (
	"context"
	"fmt"

	"github.com/projanvil/woca/sdk"
)

func ExampleQuery() {
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
}
```

(示例函数无 `// Output:` 注释,godoc 只展示不执行。)

- [ ] **Step 2: 写 README**

`sdk/README.md` 内容:包定位(in-process 嵌入同一套 harness)、最小示例(同 ExampleQuery)、Options 字段表(对齐 `woca -p` 的 flag 语义,注明对应 flag 名)、无头 interrupt 默认行为与 `WithInterruptHandler` 覆盖、资源释放约定(必须 `defer s.Close()`)、与 stream-json 的 wire parity 承诺。

- [ ] **Step 3: 验证**

Run: `go build ./... && go vet ./sdk/ && go test ./sdk/`
Expected: 全绿

- [ ] **Step 4: Commit**

```bash
git add sdk/example_test.go sdk/README.md
git commit -m "docs(sdk): usage example and README"
```

---

### Task 5: TypeScript SDK(`sdk/typescript/`)

**Files:**
- Create: `sdk/typescript/package.json`
- Create: `sdk/typescript/tsconfig.json`
- Create: `sdk/typescript/src/index.ts`
- Create: `sdk/typescript/test/query.test.mjs`
- Create: `sdk/typescript/test/fake-woca.sh`(假二进制)
- Create: `sdk/typescript/README.md`

**Interfaces:**
- Consumes: 真实 `woca` 二进制的 `-p`/`--output-format stream-json` 及 flag 集(`internal/agentbuild/options.go:104-137` 的 RegisterFlags)。
- Produces(npm 包 `woca-agent-sdk`):
  - `query(args: { prompt: string; options?: WocaOptions }): WocaQuery`
  - `interface WocaQuery extends AsyncIterable<WocaEvent> { cancel(): void; result: Promise<WocaResult> }`
  - `interface WocaOptions { model?; provider?; apiKey?; baseUrl?; workdir?; systemPrompt?; permissionMode?: "default"|"acceptEdits"|"bypassPermissions"|"readOnly"|"plan"; maxTurns?: number; maxTokens?: number; thinkingBudget?: number; temperature?: number; mcpServers?: string[]; allowCommands?: string[]; denyCommands?: string[]; db?: string; fallbackModel?: string; pathToWoca?: string; signal?: AbortSignal }`
  - `type WocaEvent = { type: "assistant"; message: WocaMessage } | { type: "user"; message: WocaMessage } | { type: "result"; subtype: "success"|"error"; result?: string; usage?: WocaUsage; total_cost_usd?: number; num_turns?: number; is_error?: boolean; error?: string }`

- [ ] **Step 1: 写假二进制与失败测试**

`sdk/typescript/test/fake-woca.sh`:

```bash
#!/usr/bin/env bash
# Fake woca: echoes a fixed stream-json session regardless of args.
# FAKE_WOCA_FAIL=1 makes it exit non-zero without a result event.
if [ "$FAKE_WOCA_FAIL" = "1" ]; then
  echo "boom" >&2
  exit 1
fi
# Echo back the args as the first event so tests can assert flag mapping.
printf '{"type":"assistant","message":{"role":"assistant","content":%s}}\n' "$(printf '%s' "$*" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')"
echo '{"type":"assistant","message":{"role":"assistant","content":"hello","tool_calls":[{"id":"t1","name":"Bash","args":{"command":"ls"}}]}}'
echo '{"type":"user","message":{"role":"tool","tool_call_id":"t1","content":"file.go"}}'
echo '{"type":"result","subtype":"success","result":"done","usage":{"requests":1,"input_tokens":10,"output_tokens":5,"total_tokens":15},"num_turns":2}'
```

`sdk/typescript/test/query.test.mjs`(跑编译产物,先 `npm run build`):

```js
import { test } from "node:test";
import assert from "node:assert/strict";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { query } from "../dist/index.js";

const fake = path.join(path.dirname(fileURLToPath(import.meta.url)), "fake-woca.sh");

test("streams events and resolves result", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake, model: "m", permissionMode: "bypassPermissions", maxTurns: 3 } });
  const types = [];
  for await (const ev of q) types.push(ev.type);
  assert.deepEqual(types, ["assistant", "assistant", "user", "result"]);
  const res = await q.result;
  assert.equal(res.subtype, "success");
  assert.equal(res.usage.input_tokens, 10);
});

test("maps options to CLI flags", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake, model: "m", maxTurns: 3, permissionMode: "bypassPermissions", denyCommands: ["rm -rf"], db: "none" } });
  let first;
  for await (const ev of q) { first = first ?? ev; }
  const echoed = first.message.content;
  for (const f of ["--model m", "--max-turns 3", "--permission-mode bypassPermissions", "--deny-command rm -rf", "--db none", "--output-format stream-json"])
    assert.ok(echoed.includes(f), `missing flag: ${f} in ${echoed}`);
});

test("non-zero exit without result rejects", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake } });
  process.env.FAKE_WOCA_FAIL = "1";
  await assert.rejects(async () => { for await (const _ of q) {} }, /exit code 1/);
  delete process.env.FAKE_WOCA_FAIL;
});

test("cancel terminates the process", async () => {
  const q = query({ prompt: "hi", options: { pathToWoca: fake } });
  q.cancel();
  await assert.rejects(q.result, /cancelled/);
});
```

注意:`query.test.mjs` 里 `process.env.FAKE_WOCA_FAIL` 在 spawn 前设置才能传给子进程;实现用 spawn 的 `env: { ...process.env }` 透传即可。

- [ ] **Step 2: 跑测试确认失败**

Run: `cd sdk/typescript && npm install && npm test`
Expected: FAIL(`../dist/index.js` 不存在)

- [ ] **Step 3: 实现**

`sdk/typescript/package.json`:

```json
{
  "name": "woca-agent-sdk",
  "version": "0.1.0",
  "description": "TypeScript SDK for the woca agent harness (drives woca -p --output-format stream-json)",
  "type": "module",
  "main": "dist/index.js",
  "types": "dist/index.d.ts",
  "files": ["dist"],
  "scripts": {
    "build": "tsc -p tsconfig.json",
    "test": "npm run build && node --test test/"
  },
  "engines": { "node": ">=18" },
  "devDependencies": { "typescript": "^5.5.0" }
}
```

`sdk/typescript/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "NodeNext",
    "moduleResolution": "NodeNext",
    "outDir": "dist",
    "rootDir": "src",
    "declaration": true,
    "strict": true,
    "skipLibCheck": true
  },
  "include": ["src/**/*.ts"]
}
```

`sdk/typescript/src/index.ts`(核心实现):

```ts
import { spawn, type ChildProcess } from "node:child_process";
import { createInterface } from "node:readline";

export interface WocaUsage {
  requests: number; input_tokens: number; output_tokens: number;
  total_tokens: number; cache_read_tokens?: number; cache_write_tokens?: number;
}
export interface WocaToolCall { id: string; name: string; args: Record<string, unknown> }
export interface WocaMessage {
  role: string; content?: string; tool_call_id?: string; tool_calls?: WocaToolCall[];
}
export type WocaEvent =
  | { type: "assistant"; message: WocaMessage }
  | { type: "user"; message: WocaMessage }
  | WocaResult;
export interface WocaResult {
  type: "result"; subtype: "success" | "error"; result?: string;
  usage?: WocaUsage; total_cost_usd?: number; num_turns?: number;
  is_error?: boolean; error?: string;
}

export interface WocaOptions {
  model?: string; provider?: string; apiKey?: string; baseUrl?: string;
  workdir?: string; systemPrompt?: string;
  permissionMode?: "default" | "acceptEdits" | "bypassPermissions" | "readOnly" | "plan";
  maxTurns?: number; maxTokens?: number; thinkingBudget?: number; temperature?: number;
  mcpServers?: string[]; allowCommands?: string[]; denyCommands?: string[];
  db?: string; fallbackModel?: string;
  /** Explicit woca binary path; falls back to $WOCA_PATH then PATH. */
  pathToWoca?: string;
  signal?: AbortSignal;
}

function buildArgs(prompt: string, o: WocaOptions): string[] {
  const args = ["-p", prompt, "--output-format", "stream-json"];
  const push = (flag: string, v: string | number | undefined) => {
    if (v !== undefined && v !== "") args.push(flag, String(v));
  };
  push("--model", o.model); push("--provider", o.provider);
  push("--api-key", o.apiKey); push("--base-url", o.baseUrl);
  push("--workdir", o.workdir); push("--system-prompt", o.systemPrompt);
  push("--permission-mode", o.permissionMode);
  push("--max-turns", o.maxTurns); push("--max-tokens", o.maxTokens);
  push("--thinking-budget", o.thinkingBudget); push("--temperature", o.temperature);
  push("--db", o.db); push("--fallback-model", o.fallbackModel);
  for (const v of o.mcpServers ?? []) args.push("--mcp", v);
  for (const v of o.allowCommands ?? []) args.push("--allow-command", v);
  for (const v of o.denyCommands ?? []) args.push("--deny-command", v);
  return args;
}

function resolveBinary(o: WocaOptions): string {
  return o.pathToWoca ?? process.env.WOCA_PATH ?? "woca";
}

export interface WocaQuery extends AsyncIterable<WocaEvent> {
  cancel(): void;
  result: Promise<WocaResult>;
}

export function query(args: { prompt: string; options?: WocaOptions }): WocaQuery {
  const o = args.options ?? {};
  const child: ChildProcess = spawn(resolveBinary(o), buildArgs(args.prompt, o), {
    stdio: ["ignore", "pipe", "inherit"],
    env: { ...process.env },
    ...(o.workdir ? { cwd: o.workdir } : {}),
  });

  let cancelled = false;
  const cancel = () => { cancelled = true; child.kill("SIGTERM"); };
  o.signal?.addEventListener("abort", cancel, { once: true });

  const lines = createInterface({ input: child.stdout! });
  let resultEv: WocaResult | undefined;

  const result = new Promise<WocaResult>((resolve, reject) => {
    child.on("error", reject);
    child.on("close", (code) => {
      if (cancelled) return reject(new Error("woca query cancelled"));
      if (resultEv) return resolve(resultEv);
      reject(new Error(`woca exited with exit code ${code} and no result event`));
    });
  });
  // Prevent unhandled rejection when the caller only iterates.
  result.catch(() => {});

  async function* iterate(): AsyncGenerator<WocaEvent> {
    for await (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      const ev = JSON.parse(trimmed) as WocaEvent;
      if (ev.type === "result") resultEv = ev;
      yield ev;
    }
    await result;
  }

  return { [Symbol.asyncIterator]: () => iterate(), cancel, result };
}
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd sdk/typescript && npm install && npm test`
Expected: 4 个测试全 PASS(假二进制需要可执行权限:`chmod +x test/fake-woca.sh`;若 CI/本机无 python3,把 args 回显那行改成 `jq` 之外的纯 bash:`content` 固定为 `"$*"` 粗暴转义也可,测试只断言子串)

- [ ] **Step 5: 写 README 并 commit**

`sdk/typescript/README.md`:安装、最小示例(`for await (const msg of query(...))`)、Options 表、权限模式警告(自动化场景用 `bypassPermissions` 并在沙箱里跑)、二进制解析顺序。

```bash
chmod +x sdk/typescript/test/fake-woca.sh
git add sdk/typescript/
git commit -m "feat(sdk): TypeScript SDK wrapping woca -p stream-json"
```

---

### Task 6: Python SDK(`sdk/python/`)

**Files:**
- Create: `sdk/python/pyproject.toml`
- Create: `sdk/python/src/woca_agent_sdk/__init__.py`
- Create: `sdk/python/src/woca_agent_sdk/query.py`
- Create: `sdk/python/tests/test_query.py`
- Create: `sdk/python/tests/fake_woca.sh`(复用 Task 5 的脚本内容)
- Create: `sdk/python/README.md`

**Interfaces:**
- Consumes: 同 Task 5(woca CLI flag 集)。
- Produces(pip 包 `woca-agent-sdk`):
  - `async def query(prompt: str, options: WocaOptions | None = None) -> AsyncIterator[dict]` — 产出 dict 事件(保持 wire 字段名:`type`/`message`/`subtype`/`result`/`usage`/`total_cost_usd`/`num_turns`/`is_error`/`error`)
  - `@dataclass WocaOptions`,字段与 TS 对齐(snake_case:`path_to_woca`、`api_key`、`base_url`、`system_prompt`、`permission_mode`、`max_turns`、`max_tokens`、`thinking_budget`、`mcp_servers`、`allow_commands`、`deny_commands`、`fallback_model`、`workdir`、`model`、`provider`、`db`、`temperature`)

- [ ] **Step 1: 写假二进制与失败测试**

`sdk/python/tests/fake_woca.sh`:与 Task 5 的 `fake-woca.sh` 内容相同(含 `FAKE_WOCA_FAIL` 分支),`chmod +x`。

`sdk/python/tests/test_query.py`:

```python
import asyncio
import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent / "src"))

from woca_agent_sdk import WocaOptions, query

FAKE = str(Path(__file__).resolve().parent / "fake_woca.sh")


def run(coro):
    return asyncio.run(coro)


def test_streams_events_and_result():
    async def main():
        events = [ev async for ev in query("hi", WocaOptions(path_to_woca=FAKE))]
        return events

    events = run(main())
    assert [e["type"] for e in events] == ["assistant", "assistant", "user", "result"]
    result = events[-1]
    assert result["subtype"] == "success"
    assert result["usage"]["input_tokens"] == 10


def test_maps_options_to_flags():
    async def main():
        async for ev in query(
            "hi",
            WocaOptions(
                path_to_woca=FAKE, model="m", max_turns=3,
                permission_mode="bypassPermissions", deny_commands=["rm -rf"], db="none",
            ),
        ):
            return ev  # first event echoes the CLI args

    echoed = run(main())["message"]["content"]
    for flag in ["--model m", "--max-turns 3", "--permission-mode bypassPermissions",
                 "--deny-command rm -rf", "--db none", "--output-format stream-json"]:
        assert flag in echoed, f"missing {flag} in {echoed}"


def test_nonzero_exit_raises():
    os.environ["FAKE_WOCA_FAIL"] = "1"
    try:
        async def main():
            async for _ in query("hi", WocaOptions(path_to_woca=FAKE)):
                pass

        with pytest.raises(RuntimeError, match="exit code 1"):
            run(main())
    finally:
        del os.environ["FAKE_WOCA_FAIL"]
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd sdk/python && pip install pytest && python -m pytest tests/ -v`
Expected: FAIL(`No module named 'woca_agent_sdk'`)

- [ ] **Step 3: 实现**

`sdk/python/pyproject.toml`:

```toml
[build-system]
requires = ["setuptools>=68"]
build-backend = "setuptools.build_meta"

[project]
name = "woca-agent-sdk"
version = "0.1.0"
description = "Python SDK for the woca agent harness (drives woca -p --output-format stream-json)"
requires-python = ">=3.10"

[tool.setuptools.packages.find]
where = ["src"]
```

`sdk/python/src/woca_agent_sdk/__init__.py`:

```python
from .query import WocaOptions, query

__all__ = ["WocaOptions", "query"]
```

`sdk/python/src/woca_agent_sdk/query.py`:

```python
"""Drive the woca agent harness headlessly via `woca -p --output-format stream-json`."""

from __future__ import annotations

import asyncio
import json
import os
import shutil
from dataclasses import dataclass, field
from typing import AsyncIterator, Optional


@dataclass
class WocaOptions:
    model: Optional[str] = None
    provider: Optional[str] = None
    api_key: Optional[str] = None
    base_url: Optional[str] = None
    workdir: Optional[str] = None
    system_prompt: Optional[str] = None
    permission_mode: Optional[str] = None  # default|acceptEdits|bypassPermissions|readOnly|plan
    max_turns: Optional[int] = None
    max_tokens: Optional[int] = None
    thinking_budget: Optional[int] = None
    temperature: Optional[float] = None
    mcp_servers: list[str] = field(default_factory=list)
    allow_commands: list[str] = field(default_factory=list)
    deny_commands: list[str] = field(default_factory=list)
    db: Optional[str] = None  # "none" disables persistence
    fallback_model: Optional[str] = None
    path_to_woca: Optional[str] = None  # falls back to $WOCA_PATH then PATH


def _build_args(prompt: str, o: WocaOptions) -> list[str]:
    args = ["-p", prompt, "--output-format", "stream-json"]

    def push(flag: str, value) -> None:
        if value is not None and value != "":
            args += [flag, str(value)]

    push("--model", o.model)
    push("--provider", o.provider)
    push("--api-key", o.api_key)
    push("--base-url", o.base_url)
    push("--workdir", o.workdir)
    push("--system-prompt", o.system_prompt)
    push("--permission-mode", o.permission_mode)
    push("--max-turns", o.max_turns)
    push("--max-tokens", o.max_tokens)
    push("--thinking-budget", o.thinking_budget)
    push("--temperature", o.temperature)
    push("--db", o.db)
    push("--fallback-model", o.fallback_model)
    for v in o.mcp_servers:
        args += ["--mcp", v]
    for v in o.allow_commands:
        args += ["--allow-command", v]
    for v in o.deny_commands:
        args += ["--deny-command", v]
    return args


def _resolve_binary(o: WocaOptions) -> str:
    if o.path_to_woca:
        return o.path_to_woca
    if env := os.environ.get("WOCA_PATH"):
        return env
    if found := shutil.which("woca"):
        return found
    raise FileNotFoundError("woca binary not found; set WocaOptions.path_to_woca or $WOCA_PATH")


async def query(prompt: str, options: Optional[WocaOptions] = None) -> AsyncIterator[dict]:
    """Run one headless woca session, yielding stream-json events as dicts.

    The last event is always the result envelope (type == "result").
    Raises RuntimeError when woca exits non-zero without a result event.
    """
    o = options or WocaOptions()
    proc = await asyncio.create_subprocess_exec(
        _resolve_binary(o), *_build_args(prompt, o),
        stdout=asyncio.subprocess.PIPE,
        stderr=None,  # inherit: human diagnostics stay visible
        cwd=o.workdir,
        env={**os.environ},
    )
    assert proc.stdout is not None
    saw_result = False
    async for raw in proc.stdout:
        line = raw.decode().strip()
        if not line:
            continue
        ev = json.loads(line)
        if ev.get("type") == "result":
            saw_result = True
        yield ev
    code = await proc.wait()
    if not saw_result and code != 0:
        raise RuntimeError(f"woca exited with exit code {code} and no result event")
```

- [ ] **Step 4: 跑测试确认通过**

Run: `cd sdk/python && python -m pytest tests/ -v`
Expected: 3 个测试全 PASS

- [ ] **Step 5: 写 README 并 commit**

`sdk/python/README.md`:同 TS README 的 Python 版(`async for ev in query(...)`)。

```bash
chmod +x sdk/python/tests/fake_woca.sh
git add sdk/python/
git commit -m "feat(sdk): Python SDK wrapping woca -p stream-json"
```

---

### Task 7: 收尾 — 全量回归验证

**Files:**
- Modify: 无(纯验证)

- [ ] **Step 1: Go 全量**

Run: `go build ./... && go test ./sdk/ ./internal/sdkout/ ./internal/agentbuild/ ./cmd/woca/`
Expected: 全绿(sdkout 零改动,其测试必须原样通过)

- [ ] **Step 2: stream-json 线上格式冒烟**

Run: `go run ./cmd/woca -p "say ok" --output-format stream-json --db none` (需要真实 API key;若无 key 则跳过并在交付说明中注明)
Expected: 逐行 JSON 事件,最后一行 `{"type":"result",...}`,与改动前格式一致

- [ ] **Step 3: TS / Python 测试**

Run: `cd sdk/typescript && npm test` 与 `cd sdk/python && python -m pytest tests/ -v`
Expected: 全绿

- [ ] **Step 4: golangci-lint(若仓库 CI 使用)**

Run: `golangci-lint run ./sdk/...`(若本机装了)
Expected: 无新告警

---

## Self-Review 记录

- **Spec coverage**:P0(Task 1-4)✓、P1 TS(Task 5)✓、P1 Python(Task 6)✓、wire parity 承诺(Task 1 测试)✓、无头 interrupt 默认(Task 2)✓、资源释放(Task 3 Close)✓。Spec 中"sdkout 改为基于 sdk 事件类型"的主方案被替换为 Global Constraint 的零改动 + parity 测试(回退方案),降低回归风险——这是对 spec 的有意偏离,spec 允许("若改动面过大,退化为…")。
- **Placeholder scan**:Task 3 Step 4 与 Task 5 Step 4 各有一个现场校准点(fake SSE 格式、args 回显的 shell 转义),已在步骤中写明校准方法,非占位符。
- **Type consistency**:`Event`/`Message`/`Result`/`Usage` 在 Task 1 定义,Task 3 消费一致;TS `WocaEvent`/`WocaResult` 与 Python dict 的 wire 字段名与 Go `WireJSON()` 的 json key 逐一对应(`total_cost_usd`/`num_turns`/`is_error` snake_case 与 wire 一致)。

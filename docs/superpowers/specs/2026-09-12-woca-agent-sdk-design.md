# woca Agent SDK 设计(P0 Go SDK + P1 TS/Python SDK)

日期:2026-09-12
状态:已批准范围(P0 + P1;bridge 不做 SDK——bridge 是常驻 service,已有 REST+SSE 程序化通道,见 `internal/server/server.go:341-401`)

## 背景与目标

woca 个人版已具备完整无头模式:`woca -p "..." --output-format json|stream-json`(`cmd/woca/main.go:448-472` + `internal/sdkout/sdkout.go`),协议对齐 Claude Code,已被 eval harness(`eval/woca_agent.py`)生产使用。缺口是 SDK 形态:

- **P0 — Go in-process SDK**:组合根 `internal/agentbuild.BuildAgent` 在 `internal/` 下,外部 Go 项目无法 import。提供公开包 `sdk`,让外部 Go 程序直接嵌入同一套 agent harness(model、tools、hooks、skills、MCP、权限)。
- **P1 — TS/Python SDK**:对 `woca -p --output-format stream-json` 做子进程封装(Claude Agent SDK 同款架构),Go 侧零改动,协议已被验证。

## 非目标

- bridge / woca-server 的 HTTP 客户端 SDK(常驻 service,直接调 REST+SSE 即可)。
- 新的 wire 协议(ACP/JSON-RPC 不引入)。
- 无头模式本身的功能增强(token delta、`--json-schema` 等,后续单独做)。

## P0:Go SDK(`sdk/` 包)

### 包位置与导入路径

新建仓库根目录 `sdk/`,package 名 `sdk`,导入路径 `github.com/projanvil/woca/sdk`。同在模块内,可以 import `internal/`;外部消费者只依赖 `sdk` 的导出 API。TS/Python SDK 放 `sdk/typescript/`、`sdk/python/` 子目录(Go 工具链忽略非 Go 子目录)。

### API 形态

```go
package sdk

// Options 是 agentbuild.Options 的对外子集(值语义,字段同义)。
type Options struct {
    Model, Provider, APIKey, BaseURL string
    Workdir, ExtraPrompt             string
    PermissionMode                   string // default | acceptEdits | bypassPermissions | readOnly | plan
    MaxTurns, MaxTokens              int
    ThinkingBudget                   int
    Temperature                      float64
    MCPServers, AllowCommands, DenyCommands []string
    DB               string // "none" 关闭持久化,默认沿用 CLI 行为
    FallbackModel    string
    CheckpointDir    string
    // OnInterrupt 可选:自定义权限/提问/计划审批处理。nil = 无头默认
    // (权限 deny、提问返回空、计划拒绝),与 CLI 无头行为一致。
    OnInterrupt func(Interrupt) (any, error)
}

type EventType string // "assistant" | "user" | "result"
type Event struct {
    Type    EventType
    Message *Message  // assistant/user:role、content、tool_calls、tool_call_id
    Result  *Result   // result:最终信封
}
type Result struct {
    Subtype   string // success | error
    Text      string
    Usage     Usage  // input/output/cache tokens
    CostUSD   *float64
    NumTurns  int
    IsError   bool
    ErrorText string
}

// Query 运行一次无头会话,以 channel 流式产出事件,最后一条必是 result。
// 返回的 cancel 用于中途停止;调用方必须 drain 或 cancel,避免 goroutine 泄漏。
func Query(ctx context.Context, prompt string, opts ...Option) (*Session, error)

type Session struct {
    Events() <-chan Event
    Close() error // 取消运行并释放资源(plugin manager、agent、sandbox)
}
```

Option 函数式:`WithModel(...)` 等,或直接用 `WithOptions(Options)`。实现上:`Query` 把 Options 翻译成 `agentbuild.Options`,调 `agentbuild.BuildAgent`,用 `runner.New(agent, interruptHandler, renderer)` 跑 `runner.Run`,renderer 把 `lggraph.StreamChunk` 转成 `Event`(与 `sdkout.Step` 相同的提取逻辑),`runner.Run` 返回后合成 result 事件(与 `sdkout.Final` 相同:usage、cost 估算、last AI text、numTurns 按 tool 消息计数)。

### 关键实现决策

1. **与 sdkout 共享转换逻辑,不复制**:`sdkout` 的事件序列化(messageJSON、lastAItext、cost 估算)抽到一个两边都能用的位置。方案:在 `sdk` 包内定义导出事件类型 + 转换函数,`sdkout` 改为基于 `sdk` 的事件类型做 JSON 序列化(保持 stream-json 线上格式逐字节不变,`eval/woca_agent.py` 依赖它)。若改动面过大,退化为 sdk 内部私有转换 + 一个对照测试锁两种输出一致。
2. **无头 interrupt 默认**:与 `cmd/woca/main.go` 的 `interruptHandler`(非交互分支)对齐:PermissionRequest → deny,AskUserRequest → 空,PlanApprovalRequest → 拒绝。`Options.OnInterrupt` 可覆盖。
3. **生命周期**:`Session.Close` 负责 `PluginMgr.Close()`、`Agent.Close()`、`Teardown` —— 镜像 `cmd/woca/main.go:374-375`。
4. **不改 agentbuild 签名**:sdk 只做翻译层,`agentbuild.BuildAgent` 保持现状,REPL/TUI/serve 零影响。

## P1:TS / Python SDK(子进程包装)

两个语言各一个包,API 对齐 Claude Agent SDK 的 `query()`:

- **`sdk/typescript/`** — npm 包 `woca-agent-sdk`:
  ```ts
  import { query } from "woca-agent-sdk";
  for await (const msg of query({ prompt: "...", options: { model, permissionMode: "bypassPermissions", maxTurns } })) { ... }
  ```
  实现:`child_process.spawn("woca", ["-p", prompt, "--output-format", "stream-json", ...flags])`,按行解析 stdout NDJSON → 异步迭代器;`AbortSignal` → SIGTERM;非零退出且无 result 事件 → reject。二进制路径:`options.pathToWoca` → env `WOCA_PATH` → PATH 查找。
- **`sdk/python/`** — pip 包 `woca-agent-sdk`,API 同构:
  ```python
  async for msg in query(prompt="...", options=WocaOptions(model=..., permission_mode="bypassPermissions")): ...
  ```
  实现:`asyncio.create_subprocess_exec`,行解析同上。
- 事件 schema 两边共用一份 TS/Python 类型定义,对齐 `sdkout` 的 `event` struct(`assistant`/`user`/`result` 三种 type)。
- 各自带 README(安装、最小示例、权限模式警告)和最小测试(mock 一个输出固定 NDJSON 的假 woca 可执行文件)。

## 测试策略

- Go:`sdk/` 包单测 —— mock/fake 的轻验证(事件转换、interrupt 默认行为、Options 翻译);一个对照测试锁 `sdk` 事件 JSON 与 `sdkout` stream-json 行格式一致。不跑真实模型(项目现有测试均不打真实 API)。
- TS/Python:假二进制产生固定 stream-json 输出,验证解析、参数拼接、取消语义。
- 端到端验证:`go build ./...` + 项目现有测试套件不回归;手工冒烟 `woca -p` 输出格式不变。

## 风险

- sdk 依赖 `internal/agentbuild` 等内部包 → 同模块内合法;但 sdk 的导出 API 不能泄漏 internal 类型(如 `agent.Agent`、`settings.Store`),转换层要封干净。
- stream-json 无 token delta(P1 的事件粒度是消息级,与 Claude Agent SDK 的消息级对齐,可接受)。

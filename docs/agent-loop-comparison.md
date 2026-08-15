# Agent Loop 对比：Claude Code vs woca（langgraph 版）

对比对象：

- **Claude Code**：`/Users/yuhaochen/Github/Claude-code`（从 `cli.js.map` 反解重建的源码，核心在 `src/query.ts` 的 `queryLoop`，约 1900 行、29 个标注阶段）
- **woca**：`/Users/yuhaochen/Documents/codebase/projanvil/woca`（Go，自研 langgraph 风格图运行时 `internal/graph/`，agent 图在 `internal/agent/agent.go`）

---

## 1. 架构总览

| 维度 | Claude Code | woca |
|---|---|---|
| 循环形态 | 单个 `while(true)` 异步生成器 `queryLoop`（`src/query.ts:259`），每轮通过整体替换不可变 `State` 对象 + `continue` 驱动 | 显式状态图：`model` 节点 → 条件路由 `route` → `tools` 节点 → 无条件回边 → END（`internal/agent/agent.go:260-278`） |
| 状态管理 | `messages` 数组随循环演化，`getMessagesAfterCompactBoundary` 截取压缩边界后的消息 | `messages` channel + `MessagesReducer`（add_messages 语义，同 ID 替换，`agent.go:40-65`） |
| 外层驱动 | SDK/UI 消费 async generator | `internal/runner/runner.go:43-103` 消费 `graph.Stream`，处理中断（answer 后 `StreamResume`）与 Stop hook 反馈续跑（上限 3 次） |
| 终止条件 | 无 tool_use + 所有恢复路径 + stop 钩子放行 + token 预算耗尽（`Terminal{reason}`） | `route` 无 ToolCalls → END，或 recursionLimit（默认 50） |

## 2. 每轮迭代的流水线对比

Claude Code 每轮在调模型**之前**依次做（`query.ts` 阶段【8】-【13】）：

1. 工具结果大小预算（`applyToolResultBudget`，超大 tool_result 落盘/替换）
2. History Snip（裁剪旧工具结果细节，feature 门控）
3. Microcompact（按 tool_use_id 的轻量局部压缩）
4. Context Collapse（多轮折叠为摘要投影，feature 门控）
5. 系统提示词拼装（基础 prompt + systemContext）
6. **Autocompact**（全量摘要压缩，带 consecutiveFailures 熔断器）
7. 硬阻塞限制检查（autocompact 关闭时上下文超限直接报错退出）

woca 在每次 `Invoke`/`Stream` 前做（`agent.go:404-447` `CompactHistory`）：Snip → Microcompact → summarize（`internal/compact/compact.go`），带 `maxCompactFails=3` 熔断器。

**差距**：woca 已对齐 Snip/Microcompact/Autocompact + 熔断器；缺工具结果大小预算、context collapse（可视为可选演进）、以及压缩后的 413 反应式恢复（见 §3）。

## 3. 模型调用阶段对比

Claude Code（`query.ts` 阶段【17】）：

- **运行时模型选择**：plan 模式且上下文 >200k 时切大窗口模型
- **流式回退（streaming fallback）**：主模型过载中途切 fallbackModel，发 tombstone 清掉半成品消息、剥离 thinking 签名、重建流式工具执行器
- **流式工具执行**：模型还在输出时工具就已开始执行（`StreamingToolExecutor`）
- **可恢复错误扣留（withheld errors）**：prompt-too-long / max-output-tokens / media-size 错误先不吐给调用方，等恢复路径决定
- **tool_use 输入回填**：在克隆消息上回填派生字段，保证发回 API 的原始消息字节不变（prompt caching 安全）
- **task_budget**：API 级输出预算，跨压缩边界携带 remaining

woca：

- 模型重试/降级：`internal/model/model.go` Router（✅ 已对齐：Invoke 与 Stream 均支持瞬时错误重试 + fallback 切换；Stream 路径经 `woca_stream_restart` 标记块重置半成品消息）
- 流式输出：`modelNode`（`agent.go`）逐 chunk emit delta，结束后合并完整 AI 消息
- 流式工具执行：`internal/agent/streamingexec.go`（已对齐）——modelNode 收到流内完整 tool call 即后台派发，toolsNode 按 call ID 消费 future
- 工具执行：`toolsNode`（`agent.go`）对未被预执行的调用并行执行

**差距：无——P1（6 项）与 P2（6 项）路线图至此全部完成。**

## 4. 工具执行阶段对比

Claude Code（阶段【23】【27】）：

- `runTools` 串行执行或 `StreamingToolExecutor.getRemainingResults` 收尾
- 权限：`canUseTool` 回调 + PermissionMode 规则 + yolo/bash 分类器
- hooks：PreToolUse/PostToolUse/`defer` 决策、>50K 输出落盘
- 工具执行后追加附件：队列命令（用户 mid-turn 输入）、记忆预取结果、技能发现结果、文件变更通知
- 轮间刷新工具列表（新 MCP server 即时可用）
- 工具使用摘要（Haiku 异步生成，下一轮流式期间消费）
- maxTurns 限制、`claude ps` 周期任务摘要

woca：

- `toolsNode`：gate（AskUserQuestion/EnterPlanMode/ExitPlanMode 走 `graph.Interrupt`；其余 `checkPermission` 可中断询问）→ Pre/PostToolUse hooks → **并行执行** tool calls（`agent.go:569-596`）
- 权限：mode + allow/deny/ask 规则（`~/.woca/settings.json` / `.woca/settings.json` 合并）
- 子代理：Task 工具 spawn（`agent.go:752-784`），定义从 `~/.woca/agents` 加载
- Stop hook 反馈续跑：runner 层，上限 3 次

**差距**：队列命令/附件注入（用户在工具执行期间的新输入）、记忆预取（woca 无 memoryStore）、工具使用摘要、maxTurns、轮间工具刷新。

## 5. 中断/恢复（checkpoint）对比

| 能力 | Claude Code | woca 现状 |
|---|---|---|
| 会话持久化 | sessionStorage + 会话 transcript | JSONL（`~/.woca/last-session.jsonl`、`~/.woca/sessions/*.jsonl`） |
| 图状态 checkpoint | 无显式 graph checkpointer（消息数组即状态） | 自研 `Checkpointer`（Put/Get by key）：Memory + File（JSON 文件） |
| 跨进程恢复 | `--resume` / fork / branch | `--resume-checkpoint <key> --resume-value <v>`（`agent.ResumeCheckpoint`） |
| 数据库后端 | —（自有生态） | **无 sqlite/pg 后端（本次补齐）** |

woca 的 `interrupt.go` 提供 human-in-the-loop：Interrupt 值经 context 注入，resume 值回填。TUI 内权限中断目前**自动拒绝**（`tui.go:418-430`），无交互式批准 UI——这也是与 Claude Code 的明显差距。

## 6. 配置体系对比

| 维度 | Claude Code | woca 现状 |
|---|---|---|
| 设置文件 | `~/.claude/settings.json` 分层合并（user/project/managed） | `~/.woca/settings.json` 仅承载 hooks + 权限规则 |
| Provider 配置 | OAuth + apiKey helper + 设置内 model/provider | 仅 CLI flags + 环境变量（`resolveKey`/`resolveBaseURL`，`main.go:590-640`），**无持久化 provider 配置** |
| 密钥存储 | 系统钥匙串/keychain | `~/.woca/credentials.json` **明文**（0600） |
| memoryStore | 完整记忆系统（prefetch + attachments + extractMemories） | **无** |
| TUI 设置界面 | `/config` 完整设置 UI | `/config` 是 `/status` 别名（只读） |
| WebUI 设置界面 | — | SettingsPanel 纯前端 localStorage，无后端 API |

## 7. 追赶路线图（按优先级）

**P0（已实现 ✅）——持久化配置与存储层**
1. ✅ sqlite 默认数据库 `~/.woca/woca.db`（纯 Go 驱动，无 cgo），postgres 可选（`--db` / `WOCA_DB`）
2. ✅ settings 表：provider/model/baseUrl 等；secrets 表：apiKey 与 postgres DSN AES-256-GCM 加密存储（主密钥 `~/.woca/secret.key` 0600）
3. ✅ graph checkpointer 的 sqlite/postgres 后端（`graph.SQLCheckpointer`，接口不变，Put/Get by key；并修复跨进程恢复时 `messages` 类型还原）
4. ✅ memoryStore（namespace+key 的 JSON KV）：memory/sqlite/postgres 后端（`internal/store`）
5. ✅ TUI/REPL `/settings` 命令族 + `/memory` 命令 + `woca config list|get|set` + WebUI `GET/PUT /api/settings` + SettingsPanel 扩展
6. ✅ 密钥解析优先级：flag > env > 数据库（加密） > OAuth

**P1——agent loop 核心能力补齐**
7. ✅ 流式工具执行（`internal/agent/streamingexec.go`，对标 `StreamingToolExecutor.ts`）：两个 provider 都在内容块结束时一次性产出完整 tool call，modelNode 收到即后台派发；仅权限模式确定性放行的调用可预执行（default 模式=只读工具，bypass=全部），需询问/交互门的调用不预执行以保证中断语义；toolsNode 按 call ID 消费 future，不重复执行；同进程中断恢复复用已完成的预执行结果。单测验证时间重叠、单次执行、门控路径不回归。
8. ✅ Stream 路径的模型回退（`internal/model/model.go`，对标 `withRetry.ts` + `FallbackTriggeredError`）：Router.Stream 从透传改为带重试/回退——瞬时错误按指数退避重试 primary（默认 2 次），耗尽后切换 fallback（`onFallback` 通知一次，fallback 自带一轮重试）；已交付部分输出后的重启先发 `woca_stream_restart` 标记块，modelNode 收到即重置累计消息（tombstone 等价）并取消失败 attempt 派发的预执行调用（`StreamingToolExecutor.discard()` 等价）；消费者取消不重试。单测覆盖重试透明性、回退序列、不可重试、耗尽、取消、标记 JSON 往返；agent 级测试验证回退后最终消息不含失败 attempt 的部分文本、预执行 2 次取消 + 1 次完成。
9. ✅ max_output_tokens 恢复（`internal/agent/recovery.go` + runner/agent 双路径接入，对标 `query.ts:1309-1377`）：检测 `stop_reason=max_tokens` / `finish_reason=length` 的截断尾部；先走 8k→64k 升级重试（`MaxTokensEscalator`，main.go 仅在用户未显式设置 max-tokens 时接线，同一请求无恢复消息，轮末还原原模型），再注入恢复消息（"Resume directly — no apology…"）最多 3 次续写；耗尽后按原样返回截断输出并跳过 Stop hooks（镜像 Claude Code 的 apiError 早退）。Invoke 与流式 runner 路径均有单测（恢复注入、耗尽计数、升级路径无恢复消息+模型还原）。
10. ✅ prompt-too-long 反应式压缩恢复（`internal/agent/reactivecompact.go`，对标 `query.ts:1179-1297`）：`IsPromptTooLongError` 识别各 provider 的上下文超限错误（anthropic "prompt is too long: N tokens > M maximum"、openai "maximum context length"/`context_length_exceeded`、vertex "exceeds the maximum number of tokens"）；`ReactiveCompactHistory` 无视估算阈值强制压缩（Snip→Microcompact→摘要，预算取配置值或当前估算的 1/4）；压缩确实收缩（≥10% 或消息数减少）才重试该轮，**单次重试守卫**（= hasAttemptedReactiveCompact）——第二次超限直接浮出；无法收缩（如单条巨大 prompt）直接浮出原始错误。Invoke 与流式 runner 双路径接入。附带修复 compact 包"最后一条消息超大时会被一并摘要掉"的既有缺陷（现在总是保留最后一条）。
11. ✅ 工具结果大小预算（`internal/agent/toolbudget.go`，对标 `applyToolResultBudget` + `toolResultStorage.ts`）：modelNode 每次组装模型请求前，对每个"工具轮"（连续 tool result 消息段）施加聚合字符预算（默认 200k = `MAX_TOOL_RESULTS_PER_MESSAGE_CHARS`）；超限时从最大结果开始替换为确定性占位（保留 2000 字符预览 + 移除量说明 + "重新以更窄参数运行工具"指引，ToolCallID 保留）。幂等 + 按轮确定性 → 重复应用字节一致（prompt cache 安全），且模型已见过的未替换轮次永不被追溯替换（Claude Code 的 frozen 语义由确定性自然得出）；transcript/状态保留原始内容，仅发给模型的请求被预算。`ToolResultBudgetChars` 可配置（0=默认 200k，负=禁用）。
12. ✅ 队列命令附件（`internal/agent/queue.go`，对标阶段【27】 `getQueuedCommandAttachments`）：`Agent.QueueInput` 线程安全地缓存运行期间提交的用户输入；toolsNode 在工具执行完成、产出结果批之后排空队列，把每条输入包装为 `<queued-user-input>` Human 消息**严格追加在 tool results 之后**（API 禁止 tool_result 与 user 消息交叉），模型可在本轮内回应；中断-恢复的重执行 drain 已空的队列不会重复注入；子代理（独立 Agent 实例）天然隔离（对应 Claude Code 的 per-agent drain scoping）。Web 侧提供 `POST /api/runs/{id}/input` 端点 + `queueRunInput` 客户端。

**P2——体验与生态**
13. ✅ 工具使用摘要（`internal/agent/toolsummary.go`，对标 `toolUseSummaryGenerator.ts` + 阶段【24】：批次后用 summarizer 异步生成 git-commit-subject 风格的一行标签（沿用 Claude Code 原版系统提示词与 Input/Output 截断 300 字符的 prompt 结构）；不阻塞下一轮模型调用（goroutine）；**UI-only**——永不进入模型上下文；runner 在下一轮流式期间非阻塞轮询消费（`TakeReadyToolUseSummaries`），turn 结束时有界等待（2s）以免末批摘要丢失（对应阶段【21】的 await）；生成与运行取消解耦（`WithoutCancel`——图流在终止步发出即 cancel 会杀死在途摘要）；失败静默；子代理跳过；仅交互式会话开启（`-p` 单发关闭，对应 isNonInteractiveSession 门控）；缓冲上限 16。REPL 经 `DefaultSummaryRenderer` 渲染为 `· <label>` 行。
14. ✅ TUI 权限交互式批准（`internal/tui/tui.go`）：权限中断不再自动拒绝——`interruptHandler` 把 `PermissionRequest`/`PlanApprovalRequest` 发为 `interruptMsg` 并阻塞 run goroutine；Model 进入 pending 态在输入框上方渲染圆角批准框（工具名 + 原因 + "y = allow · enter = allow · n / esc = deny"）；按键解析后经 answerCh 返回，runner 继续 StreamResume；决定回显进 transcript；`AskUserQuestion` 中断则由输入行收集答案（多问以 ";" 分隔）回车提交为 `AskUserAnswer`。单测覆盖批准/拒绝/esc/plan/问答及端到端（default 模式 Write 中断 → 按 y → 续跑完成且文件写入恰好一次）。
15. ✅ token 预算自动续写（`internal/runner/tokenbudget.go`，对标 `query/tokenBudget.ts` + 阶段【22e】）：runner 自然结束分支（Stop hooks 之后，顺序同 Claude Code 22d→22e）检查轮累计输出 token——预算（`Runner.TokenBudget`，默认 `DefaultTurnTokenBudget`=500k，经 `WOCA_TURN_TOKEN_BUDGET` 环境变量选择启用，未设即禁用）未满 90% 且未收益递减（连续 ≥3 次续写且本次与上次增量均 <500 token）时注入 nudge 消息（"Stopped at N% of token target (X / Y). Keep working — do not summarize."）续写；判定矩阵、tracker 状态推进与 Claude Code 一致；输出量按轮增量累计（AI 文本 + tool call 参数估算，工具结果不计）。单测覆盖决策矩阵（禁用/90% 阈值/递减三条件组合）、续写注入（恰好每续写一条 nudge）、递减停止（小增量下 1+3 轮封顶）、预算耗尽停止、默认禁用。
16. ✅ maxTurns（`internal/agent/maxturns.go`，对标阶段【28】）：`Config.MaxTurns`（CLI `--max-turns`，0=无限）按 turn 计数——toolsNode 完成一轮工具即一个 model round，达上限追加 `<max-turns-reached>` 标记消息并经 `graph.Command{Goto: END}` 短路结束（tools→model 为无条件边，标记需短路）；计数在 runner.Run / agent.Invoke 入口重置（一次 Run/Invoke = 一个 turn，恢复续跑不重置）。测试验证恰好 N 轮停止、标记存在、默认无限。
17. ✅ 轮间工具列表刷新（`internal/agent/refreshtools.go`，对标阶段【28】 refreshTools）：`Config.RefreshTools` 钩子在每轮工具结束后重取 extra 工具（main.go 保留 MCP clients 重新 tools/list——热加的工具下一轮可用），集合变化时重建 toolList/byName 并重绑模型；toolsNode 末尾调用（图节点顺序执行，无半更新竞态）。测试验证新工具入列、模型重绑、集合未变不重绑。
18. ✅ 任务摘要（`internal/agent/tasksummary.go`，对标 `claude ps`）：每 5 个 model round 用 summarizer 异步生成一行"正在做什么"（现在进行时、~80 字符、best-effort），`Agent.TaskSummary()` 供 `/status` 展示（"working on: …"）。测试验证周期触发与内容。
19. ✅ 记忆预取/附件（`internal/agent/memorystore.go`，对标 startRelevantMemoryPrefetch + 阶段【27】）：配置了 memoryStore（P0 的 sqlite/pg KV）时，每个 model turn 开始异步检索与最新用户消息关键词匹配的记忆（朴素包含匹配、上限），工具轮结束后将已沉淀且未注入过的条目作为 `<relevant-memory>` Human 附件追加在结果批之后（进入模型上下文）；同一记忆不重复注入（有界去重集）。main.go 已将 settings 选定的 memoryStore 接入 agent。测试验证匹配/排除/JSON 解引/不重复注入。

woca 已对齐的部分：auto-compact + 熔断、Snip/Microcompact、并行工具执行、权限规则、plan mode、子代理、技能、MCP、hooks（Pre/PostToolUse/UserPromptSubmit/Stop/SessionStart）、fallback 模型（Invoke 路径）、todos/tasks、Web GUI、会话恢复。

---

## 附：关键文件索引

**Claude Code**
- 主循环：`src/query.ts:259-1928`（queryLoop，29 个阶段）
- 压缩：`src/services/compact/`（autoCompact/microCompact/snipCompact/reactiveCompact/grouping）
- 工具编排：`src/services/tools/toolOrchestration.ts`、`StreamingToolExecutor.ts`
- stop hooks：`src/query/stopHooks.ts`；token 预算：`src/query/tokenBudget.ts`
- 系统提示词：`src/constants/prompts.ts`、`systemPromptSections.ts`
- 权限：`src/utils/permissions/`；工具注册：`src/tools.ts`
- 走读文档：`docs/WALKTHROUGH-GUIDE.md`

**woca**
- 图运行时：`internal/graph/`（graph.go/compiled.go/interrupt.go/checkpoint.go/state.go）
- agent 图：`internal/agent/agent.go`（model/tools 节点、路由、压缩、子代理）
- 外层驱动：`internal/runner/runner.go`
- 压缩：`internal/compact/compact.go`
- 配置：`internal/config/config.go`（含 thinkingBudget 通路）；CLI 组装：`cmd/woca/main.go:385-476`
- P3 新增：后台 shell `internal/tools/background.go`、图像 `internal/tools/image.go`、
  用量累计 `internal/agent/usage.go`、成本 `internal/cost/cost.go`、
  命令 `internal/console/{init,cost,rewind}_cmd.go`、fork `internal/agent/task.go`
- P4 新增：自检/导出/命名 `internal/console/{doctor,session,agents}_cmd.go`、
  输出风格与分段缓存 `internal/prompt/outputstyles.go`、插件 `internal/plugins/`、
  cron `internal/agent/cron.go`、worktree `internal/agent/worktree.go`、
  LSP `internal/tools/lsp.go`、SDK 输出 `internal/sdkout/`、通知 `internal/notify/`、
  主题/vim `internal/tui/theme.go`、MCP OAuth `internal/mcp/auth.go`、
  托管设置 `internal/settings/managed.go`
- Web：`internal/server/server.go` + `web/`（Vue3 + Pinia）
- TUI：`internal/tui/tui.go`；命令：`internal/console/console.go`

---

# 第二轮差距分析（v2，2026-08-15）：agent loop 之外的能力面

第一轮已覆盖 query loop 全部 12 项追赶（P1+P2 完成并带测试）。本轮把对比面扩展到
工具、命令、钩子、系统提示词、会话系统与产品能力，逐类给出精确 diff。

## 8. 工具集 diff（Claude Code 44 个 vs woca 27 个）

**共有且对齐**（20）：Bash/Read/Write/Edit/NotebookEdit/Glob/Grep/AskUserQuestion/
EnterPlanMode/ExitPlanMode/TodoWrite/WebFetch/WebSearch/Task(+Output/Stop)/
TaskCreate/Get/Update/List/SendMessage、Skill、MCP（含 resources/prompts 适配）。
P3 新增：BashOutput/KillShell（后台 shell 查询/终止，配 Bash `run_in_background`）。
P4 新增：ScheduleCron（会话级定时任务）、LSP（诊断）、EnterWorktree/ExitWorktree（隔离
工作区）、McpAuth（MCP OAuth 授权流程）。

**woca 独有**：`ListDir`（Claude Code 无独立工具）。

**缺失（按价值排序）**：

| 工具 | Claude Code 位置 | 说明 |
|---|---|---|
| ~~后台任务~~ | `BashTool/prompt.ts:39` | ✅ 已对齐：Bash `run_in_background`（`internal/tools/background.go`）+ `BashOutput`/`KillShell` 工具 + 完成通知经 queue 注入（`<background-task-completed>` 附件，含 2000 字符输出尾） |
| Sleep/Monitor | `SleepTool/` | 后台事件轮询与"等待通知"模式；woca 的 Sleep 只在测试里有 |
| ~~ScheduleCron~~ | `ScheduleCronTool/` | ✅ 已对齐（`internal/agent/cron.go`）：5 字段 cron（`*/n`、区间、列表）会话级调度；到期以 `<scheduled-task>` 通知经 queue 注入（分钟粒度去重）；create/list/delete 三动作；随会话消亡不持久化 |
| ~~图像输入~~ | `FileReadTool/imageProcessor.ts` | ✅ 已对齐：Read 读 png/jpg/jpeg/gif/webp → `ImageBlock` 附着在 tool_result（`internal/tools/image.go`）；魔数校验 + 5MB 上限 + 长边 1568px 缩放（重编码 JPEG）；两条执行路径（toolsNode/预执行）均支持 |
| ~~forkSubagent~~ | `AgentTool/forkSubagent.ts` | ✅ 已对齐：Task `fork=true` 继承当前对话（modelNode 每轮快照 transcript，fork 时去掉父 system prompt 后接 `<task-fork>` 指令）；与 run_in_background 可组合 |
| ~~LSPTool~~ | `LSPTool/` | ✅ 已对齐（`internal/tools/lsp.go`）：`LSP` 工具拉取单文件诊断——stdio JSON-RPC 客户端（initialize → didOpen → textDocument/diagnostic 拉取），服务器由 `WOCA_LSP_SERVER` 配置（如 gopls）；传输层可注入，端到端测试用假语言服务器驱动真实帧协议。跳转/引用等 LSP 能力未做 |
| ~~EnterWorktree/ExitWorktree~~ | 同名目录 | ✅ 已对齐（`internal/agent/worktree.go`）：`git worktree add` 到 `.woca-worktrees/<name>`（默认建 `woca/<name>` 分支）；工具注册表的 workdir 改为可变 `Dir` 引用，enter 后所有文件/shell 工具落到隔离区、exit 回基线（可顺带删 worktree）；子代理仍以基线目录生成 |
| ~~McpAuthTool~~ | 同名目录 | ✅ 已对齐（`internal/mcp/auth.go` + `internal/agent/mcpauth.go`）：MCP OAuth 2.0 全流程——protected-resource/authorization-server 发现（相对端点解析）、RFC 7591 动态客户端注册、PKCE S256 + 本地回调监听、token 存加密 secrets（`mcp_<name>_token`）；HTTP transport 按 supplier 注入 Bearer；入口 = `McpAuth` 工具 + `woca mcp auth` CLI |
| PowerShellTool | 同名目录 | Windows 支持（woca 无 Windows 专用 shell 工具） |
| ToolSearch | `ToolSearchTool/`（feature-gated） | 海量工具时按需检索 |

（BriefTool/RemoteTrigger/SuggestBackgroundPR/Synthetic/Tungsten/REPL/Workflow/
Team*/VerifyPlanExecution 为 ant 内部或 feature-gated，不列为差距。）

## 9. 斜杠命令 diff（104 个 vs woca ~30 个）

woca 已有：help/status/config/permissions/model/clear/compact/context/cost/
init/rewind/branch/doctor/export/rename/agents/output-style/plugins/theme/vim/
keybindings/todos/tasks/skills/mcp/settings/memory/web/exit。

**缺失（用户可感知，按价值排序）**：
1. ✅ ~~`/init`~~——分析代码库并生成 AGENTS.md（存在 CLAUDE.md 时更新之）；以 `Result.Prompt` 交给驱动器提交完整 agent 回合，REPL/TUI 均接线
2. ✅ ~~`/rewind` + `/branch`~~——`/rewind` 列出用户轮回退点并截断对话（`/rewind n` 回到第 n 轮之前；带 tag 的合成附件不计入轮次）；`/branch` 把当前对话快照到 `~/.woca/sessions/<name>-<ts>.json` 供 `--resume` 分叉。文件快照回退不在范围
3. ✅ ~~`/cost`~~——`internal/agent/usage.go` 累计每次模型调用的 UsageMetadata（含 cache read/write 细节，压缩/清空后不丢）+ `internal/cost` 价格表估算（USD，claude/gpt/glm/deepseek 家族，anthropic 缓存 0.1/1.25 系数）；`/context` 也显示 session usage 行
4. ✅ ~~`/export`、`/rename`~~——`/export [path]` 导出 markdown transcript（.json 后缀则导出可 `--resume` 的完整状态）；`/rename <name>` 以固定名覆写 sessions 快照（可反复刷新同名）；`/tag`、`/share` 未做
5. ✅ ~~`/doctor`、`/keybindings`~~——`/doctor` 检查 workdir/工具集/模型/settings DB/凭据（config→DB secret→OAuth token 三级）/会话目录可写/memory/hooks/权限规则并汇总警告失败数；`/keybindings` 打印键位参考；`/hooks`、`/terminalSetup` 未做
6. ✅ ~~`/output-style`、`/theme`、`/vim`~~——输出风格（default/explanatory/learning，运行时重建系统提示词 + settings 持久化）；主题（dark/light/high-contrast，TUI 样式热切换）；vim 模态输入（normal/insert、h l 0 $ w b x D dd，状态行指示）
7. `/review`、`/pr_comments`、`/issue`、`/install-github-app`——GitHub 工作流
8. ✅ ~~`/agents`~~——list/show/new/rm 管理 `~/.woca/agents` 定义（new 写模板、rm 删文件并即时卸载，agent 定义可运行时替换）；编辑正文仍需手改文件
9. ✅ ~~`/plugins`~~——list/install/rm 插件（见 §14）；`/sandbox-toggle`、`/add-dir`、`/files`、`/diff`、`/copy`、`/env` 等未做

## 10. Hook 事件 diff（12 vs 12）

woca 已有：PreToolUse/PostToolUse/UserPromptSubmit/Stop/SubagentStop/SessionStart/
SessionEnd/Notification/PreCompact/PostCompact/PermissionRequest/PostSampling。
**已补齐**（P3-4）：`SessionEnd`（REPL/TUI/one-shot 退出点触发，`endSession`）；
`PermissionRequest`（询问前运行，exit 2 或 stdout "allow"/"deny" 可预批/预拒，
否则落回交互中断）；`PostSampling`（每次模型响应后异步运行，
`context.WithoutCancel` 与图流解耦）。仍有 StatusLine/FileSuggestion 等 UI 型钩子（低优先）。

## 11. 系统提示词与上下文

woca 已对齐：intro/tone/tool 使用/todos 指引/环境块/项目指令注入（CLAUDE.md 与
AGENTS.md 均支持）/git 上下文（internal/context）、输出风格系统与分段缓存（P4-10，
见下）。

**已补齐（P4-10）**：输出风格（`internal/prompt/outputstyles.go`：default/
explanatory/learning 三预设，人格段落以追加方式拼装——共享前缀字节不变，prompt-cache
友好；`/output-style` 运行时切换 + settings 持久化 + `Agent.SetSystemPrompt` 热替换）；
提示词分段缓存（memory 文件段按 path+size+mtime 缓存，环境段按输入缓存——重复构建
字节一致且免读盘，风格切换/`/clear`/`/compact` 后重建不扰动 provider 缓存）。
**仍缺**：技能命令注入系统提示词；markdown 产物规则与"常见误区"等细节段落；
自适应段落（模型能力探测）。

## 12. 思考与多模态

- **thinkingConfig**：✅ 已对齐请求侧通路——`Config.ThinkingBudget`（CLI `--thinking-budget`，
  anthropic provider 经 fork 的 `WithThinking({"type":"enabled","budget_tokens":N})` 透传）；
  预算钳制 [1024, 31999]（Claude Code 的 MAX_THINKING_TOKENS 语义），`max_tokens > budget`
  不变式在 Normalize 与 BuildModel 双重保障（升级恢复路径 64k cap 恒高于钳制后的预算，
  thinking 在升级模型上保留）。响应侧 thinking 块由 fork 的 partner 流透传（签名剥离在
  stream restart 路径处理）。openai partner 无请求侧旋钮（原生推理档位），预算忽略。
- **图像**：✅ 已对齐输入通路（见 §8 图像输入行）；无粘贴图片入口（仅 Read 工具）。

## 13. 会话系统

woca 已有：JSONL 落盘、`--resume last`、plan/todos/tasks 状态恢复、web 会话、
`/rewind`（对话级回退）与 `/branch`（快照分叉，P3-5）。
**缺失**：rewind 的文件快照恢复（Claude Code 会同时回退文件）、会话元数据
（rename/tag）、导出、transcript 检索、`--resume` 的交互式选择 UI、后台任务表持久化。

## 14. 其它产品能力

- **插件生态**：✅ 已对齐（P4-11，`internal/plugins`）：manifest（plugin.json：name/
  version/description/skills_dir/agents_dir）+ `~/.woca/plugins` 安装根；贡献的 skills
  以 `<plugin>-<skill>` 命名空间并入、agents 直接并入；安装源 = 本地目录（拷贝）或
  git URL（`git clone --depth 1`，marketplace 分发单元）；入口 `woca plugin list/
  install/remove` CLI + `/plugins` 命令（install 后 agents 即时生效，skills 重启生效）。
  与 Claude Code 的差异：无 marketplace 目录协议与 hooks/commands 贡献
- **设置层级**：✅ 已对齐管控层（P4-15，`internal/settings/managed.go`）：
  `--managed-settings`/`WOCA_MANAGED_SETTINGS`/`/etc/woca/managed-settings.json`
  企业文件——非空字段覆盖 DB 读取并锁定（`/settings` 与 `Save` 拒改并报
  "managed and locked"）；settingsSync/policyLimits 未做
- **SDK 模式**：✅ 已对齐输出（P4-13，`internal/sdkout`）：`--output-format json`
  （单 result 对象：result/usage/total_cost_usd/num_turns/is_error）与
  `stream-json`（assistant/user 事件流 + 最终 result 信封，逐行 JSON）；
  agent SDK 库形态与 headless profiler 未做
- **通知**：✅ 已对齐（P4-14，`internal/notify`）：BEL + iTerm2/ConEmu OSC 9 桌面
  通知，TUI 在权限中断与回合结束触发，`WOCA_NOTIFY=0` 关闭
- **限流体验**：rate-limit 消息、usage 重置提示、mock-limits；woca 仅重试。
- **内部/实验**（不列为差距）：voice、mobile、bridge/remote、buddy、VCR、
  telemetry、fastMode、chicago/computer-use。

## 15. 追赶路线图 v2（P3/P4）

**P3（核心能力，已全部完成 ✅ 2026-08-15）**
1. ✅ Bash 后台任务：`run_in_background` + 完成通知（queue 注入，`<background-task-completed>` 标签 + 2k 输出尾）+ `BashOutput`/`KillShell` 查询终止（`internal/tools/background.go`；通知经 `Agent.QueueNotification` 走既有 queue 附件机制，空闲期完成的通知留队至下一 run 的首个工具轮）
2. ✅ `/init` 项目说明生成（`internal/console/init_cmd.go`：`Result.Prompt` 由 REPL/TUI 提交为完整回合；优先更新已存在的 CLAUDE.md，否则生成 AGENTS.md）
3. ✅ `/cost`：UsageMetadata 逐调用累计（`internal/agent/usage.go`，modelNode 记录，压缩/清空不丢）+ 成本估算（`internal/cost`）+ /context 联动
4. ✅ Hook 补齐：SessionEnd（退出点）、PermissionRequest（预批/预拒/落回询问）、PostSampling（异步分析）
5. ✅ `/rewind` + `/branch`：对话级回退（用户轮列表 + 截断）与快照分叉（sessions 目录 + `--resume` 提示）
6. ✅ 图像输入：Read 图片 → ImageBlock 附着 tool_result + 魔数校验/5MB 上限/1568px 长边缩放（`internal/tools/image.go`，两条执行路径均接线）
7. ✅ thinkingConfig 通路（`--thinking-budget`；预算钳制 + max_tokens 联动不变式；升级恢复保留 thinking；请求侧验证有假端点测试）
8. ✅ forkSubagent（Task `fork=true` 继承父对话快照；`<task-fork>` 指令包裹；可与后台任务组合）

**P4（体验/生态，已全部完成 ✅ 2026-08-15）**
9. ✅ `/doctor`（workdir/工具集/模型/DB/凭据三级/会话目录可写/memory/hooks/权限规则九项检查 + 警告/失败汇总）、`/export`（markdown transcript 或 .json 可恢复状态）、`/rename`（固定名会话快照，覆写刷新）、`/agents`（list/show/new/rm，定义可运行时替换）
10. ✅ 输出风格系统（三预设 + 追加式拼装 + 运行时切换/持久化）+ 提示词分段缓存（memory/环境段按输入键缓存，重建字节一致）
11. ✅ 插件加载器（plugin.json manifest + skills/agents 贡献 + 本地/git URL 安装 + `/plugins` 与 `woca plugin` CLI）
12. ✅ ScheduleCron（会话级 cron 调度 → `<scheduled-task>` queue 注入，分钟去重）、LSP 工具（stdio JSON-RPC 诊断拉取，`WOCA_LSP_SERVER` 配置，真实帧协议端到端测试）、EnterWorktree/ExitWorktree（`Dir` 可变 workdir 引用，全部工具切换到隔离区）
13. ✅ `--output-format json/stream-json`（`internal/sdkout`：assistant/user 事件流 + result 信封含 usage/cost/turns）
14. ✅ 通知（BEL + OSC 9，中断与回合结束触发）、主题（dark/light/high-contrast 热切换）、vim 模态输入（normal/insert + h l 0 $ w b x D dd + 状态行指示）、`/keybindings` 参考
15. ✅ MCP OAuth（发现/动态注册/PKCE/本地回调/Bearer 注入/加密存 token；`McpAuth` 工具 + `woca mcp auth` CLI）、企业设置层级（managed-settings 覆盖 + 锁定）

## 附：v2 对比证据索引

- 工具目录：`src/tools/`（44 目录）；woca：`internal/tools/registry.go:33-53`
- 命令目录：`src/commands/`（104 项）；woca：`internal/console/console.go:82-102`
- Hook 事件：`src/utils/hooks/`（12 事件）；woca：`internal/hooks/hooks.go`（9）
- 后台 Bash：`src/tools/BashTool/prompt.ts:39,314-317`
- 图像处理：`src/tools/FileReadTool/imageProcessor.ts`
- forkSubagent：`src/tools/AgentTool/forkSubagent.ts`
- 输出风格：`src/constants/outputStyles.ts` + prompts.ts:152-157
- 成本：`src/cost-tracker.ts`（323 行）
- thinkingConfig：`src/services/api/claude.ts:712-755`
- 插件：`src/plugins/` + `src/utils/plugins/pluginLoader.ts`

## 16. Goal 模式（P5，✅ 2026-08-15）

现代 harness（Claude Code `/goal`、Codex goal 状态机）都在"单次对话"之上提供
**持久目标模式**：用户定义"完成"长什么样，harness 负责循环、验证和停止。
woca 的实现是显式的三节点 goal 图（`internal/agent/goalgraph.go`），复用既有
graph 运行时的全部原语：

```
START ─▶ assess ─▶ execute ─▶ evaluate ─┬─ (active)  ─▶ assess（下一循环）
                                        └─ (terminal) ─▶ END
```

| 原语 | woca 实现 |
|---|---|
| 状态机（Codex：Active↔Paused→Blocked→Complete/BudgetLimited） | `internal/agent/goal.go` `Goal` + manager；模型只能 progress/blocked/claim_complete（`UpdateGoal` 工具），暂停/恢复/清除/改预算为用户专属（`/goal`、web API） |
| 验证门控（Anthropic Planner→Generator→Evaluator，Evaluator 必须多疑） | `evaluate` 节点：怀疑式评估器只信工具证据，输出 `{verdict, evidence, gaps}` JSON；执行者 `claim_complete` 声明必须经它验证，未验证 → 重置声明继续循环 |
| 隔离专家 | `assess`/`evaluate` 为无工具的单次聚焦模型调用；`execute` 嵌套运行**完整 agent 子图**（权限/钩子/压缩/流式工具执行全部生效） |
| 续传调度（Codex idle 检测 + 空调用抑制） | TUI：run 结束/命令后 `maybeKickGoal`（active+idle 即续跑）；web：用户 run 结束后 `continueGoalLoop`；空转抑制 = 连续 2 个零工具轮次 → blocked |
| 预算边界 | `--cycles`/`--tokens`（默认 10 循环 / 2M tokens，按会话 usage 增量计）；耗尽 → budget_limited |
| 中断/恢复 | execute 内层图的中断（权限/提问）以**全新 InterruptError** 透传给 harness，内层 checkpoint 暂存于 manager；应答后 `GoalResume` 重入 execute 节点、`StreamResume` 恢复内层图（无需重做本循环） |
| 持久化（Codex PR #18073） | web 侧 `<sessionID>.goal.json` sidecar（订阅 goal 变更写入，clear 删除）；重启后 GET 恢复展示、`resume` 经 `RestoreGoal` 装回并续跑 |
| API（Codex PR #18074） | `GET/POST /api/sessions/{id}/goal`；goal run 的 SSE 流在 delta/tool 事件之外携带 `goal` 状态事件 |
| TUI（Codex PR #18077） | footer `goal[status] cycle n/N · xk/2M tok` 指示；`/goal set|status|pause|resume|budget|clear` |

转录约定：循环种子消息为 `<goal-cycle N>` Human 消息（与 `<scheduled-task>`、
`<stop-hook>` 同族）；终态报告（🎯 完成 / ⛔ 受阻 / ⏸ 预算）作为 AI 消息落回
会话转录，随 run onDone 持久化。

## 17. 可观测性（P5b，✅ 2026-08-15）

现代 harness 的标配：完整执行轨迹记录（Claude Code 的 telemetry/日志、
OpenTelemetry 风格 span）。woca 的实现分层：

| 层 | 实现 |
|---|---|
| 运行时埋点 | `internal/graph` 新增 `Observer` 接口（`WithObserver(name, ob)` 编译选项）：runInternal 在每次节点 dispatch 前后发 `NodeEvent{Graph, Node, Phase(start/end/interrupt/error), DurationMS, Update, Values}`；nil observer 零开销 |
| 记录器 | `internal/obslog`：进程级 Store（默认关闭，atomic 开关），实现 graph.Observer + `Log`/`DumpMessages`/`LogUsage` |
| 三级日志 | workspace 聚合目录 `~/.woca/logs/ws-<name>-<hash8>/`；session 级 `<session>.jsonl`（graph node/tool/run 事件流，含消息摘要与 tool_calls）；message 级 `<session>/<run>.json`（完整 transcript dump）+ 主日志 `woca.log` |
| 轮转 | 自研 `rotatingWriter`：按大小 8MB 轮转链（path → path.1 → … 保留 5 份），dump 文件按 run 覆盖无需轮转 |
| 开关三入口 | `--debug-log` flag（优先级最高）> settings DB `debug-log`（`/debug-log on\|off` 命令 / Web 设置面板即时切换，PUT 后直接 `Store.SetEnabled`，无需重启） |
| 接线 | server `startRun`/`startGoalRun` 与 TUI 每次 run：`SetObsRun(workspace, session, run)` 定界 → run_start/run_end 事件 + 完整 dump；modelNode 每次调用记 usage |
| Web 排查 | 气泡 hover 操作条：复制 Markdown（常驻）+ debug 🐞（debug-log 开启后显示，复制 workspace/session/run/时间戳与三级日志路径） |

### 可观测性落地后的第一个实战排障（2026-08-15）

用户报告：ListDir 工具输出正常，但 run 以 `context canceled` 失败。三级日志 84ms
内两次复现定位：tools 轮完成后第二个 model 调用一个 RTT 即报 canceled。逐层修复：

1. **router 误杀（internal/model）**：`scheduleRetry` 曾把错误字符串含
   "context canceled" 一律当用户取消不重试——连接层取消（网关关闭复用连接）被误判。
   改为仅 `s.ctx.Err() != nil`（真取消）不重试，其余按瞬时错误走重试/backoff/fallback。
2. **真根因（分层揭开）**：woca 的 openai partner 默认走 **/responses API**，把
   tool 结果序列化为 DeepSeek 不认的 input 项 → 网关 400；langchain 在 cancel()
   后读取响应 body，400 被**伪装成 context canceled**（84ms=RTT）。禁用 keep-alive
   后真错误现形。
3. **修复（config + fork commit 7b6abea，以伪版本 v0.0.0-20260815100741-7b6abea24eb8 引入）**：woca openai provider 默认
   `WithChatCompletions()`（/chat/completions 是所有兼容网关的最小公分母）；fork 修复
   Chat Completions tools 序列化（描述符须嵌套于 `function` 字段，平铺的 Responses
   形态会被 400 拒绝）；另注入 `DisableKeepAlives` 的模型 HTTP client
   （`WOCA_KEEPALIVE_MODELS=1` 可关）防网关切断复用连接上的流。
4. **debug-log 动态载入补全**：server/TUI 每次 run 开始从 settings DB 刷新开关
   （跨进程 toggle 下一个 run 即生效）；`--debug-log` flag `LockOpen()` 全程锁定。

验证：ListDir 场景 96 delta + done；7 轮工具调用全绿；动态开关 off 后 trace 零增长。

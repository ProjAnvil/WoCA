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
- 配置：`internal/config/config.go`；CLI 组装：`cmd/woca/main.go:385-476`
- Web：`internal/server/server.go` + `web/`（Vue3 + Pinia）
- TUI：`internal/tui/tui.go`；命令：`internal/console/console.go`

---

# 第二轮差距分析（v2，2026-08-15）：agent loop 之外的能力面

第一轮已覆盖 query loop 全部 12 项追赶（P1+P2 完成并带测试）。本轮把对比面扩展到
工具、命令、钩子、系统提示词、会话系统与产品能力，逐类给出精确 diff。

## 8. 工具集 diff（Claude Code 44 个 vs woca 20 个）

**共有且对齐**（20）：Bash/Read/Write/Edit/NotebookEdit/Glob/Grep/AskUserQuestion/
EnterPlanMode/ExitPlanMode/TodoWrite/WebFetch/WebSearch/Task(+Output/Stop)/
TaskCreate/Get/Update/List/SendMessage、Skill、MCP（含 resources/prompts 适配）。

**woca 独有**：`ListDir`（Claude Code 无独立工具）。

**缺失（按价值排序）**：

| 工具 | Claude Code 位置 | 说明 |
|---|---|---|
| 后台任务（`run_in_background` + 完成通知 + 查询/终止） | `BashTool/prompt.ts:39` | woca Bash 仅有 `timeout_ms`；无后台执行、无输出轮询工具（BashOutput/KillShell 类）、无完成通知注入 |
| Sleep/Monitor | `SleepTool/` | 后台事件轮询与"等待通知"模式；woca 的 Sleep 只在测试里有 |
| ScheduleCron | `ScheduleCronTool/` | 定时任务（Claude Code 的 cron 生态） |
| 图像输入 | `FileReadTool/imageProcessor.ts` | Read 读图片 → 图像块（多模态）；woca Read 仅 UTF-8 文本，无图像校验/缩放（query.ts 的 ImageSizeError 路径） |
| forkSubagent | `AgentTool/forkSubagent.ts` | 把当前对话 fork 进子代理（继承上下文）；woca Task 只能全新子代理 |
| LSPTool | `LSPTool/` | LSP 诊断/代码智能 |
| EnterWorktree/ExitWorktree | 同名目录 | git worktree 隔离工作区管理 |
| McpAuthTool | 同名目录 | MCP 服务器 OAuth 授权流程 |
| PowerShellTool | 同名目录 | Windows 支持（woca 无 Windows 专用 shell 工具） |
| ToolSearch | `ToolSearchTool/`（feature-gated） | 海量工具时按需检索 |

（BriefTool/RemoteTrigger/SuggestBackgroundPR/Synthetic/Tungsten/REPL/Workflow/
Team*/VerifyPlanExecution 为 ant 内部或 feature-gated，不列为差距。）

## 9. 斜杠命令 diff（104 个 vs woca ~17 个）

woca 已有：help/status/config/permissions/model/clear/compact/context/todos/tasks/
skills/mcp/settings/memory/web/exit。

**缺失（用户可感知，按价值排序）**：
1. `/init`——生成 CLAUDE.md/AGENTS.md 项目说明（高频入口）
2. `/rewind` + `/branch`——会话检查点回退与分支（依赖文件历史；woca 只有 `--resume`）
3. `/cost` + `/usage`——token 用量与成本统计（woca 仅保留最后一条 UsageMetadata，无累计）
4. `/export`、`/rename`、`/tag`、`/share`——会话导出/整理/分享
5. `/doctor`、`/hooks`、`/keybindings`、`/terminalSetup`——自检与配置 UI
6. `/output-style`、`/theme`、`/vim`——输出风格（Explanatory/Learning）、主题、vim 输入模式
7. `/review`、`/pr_comments`、`/issue`、`/install-github-app`——GitHub 工作流
8. `/agents`——子代理定义的管理 UI（woca 只能手改文件）
9. `/sandbox-toggle`、`/add-dir`、`/files`、`/diff`、`/copy`、`/env` 等

## 10. Hook 事件 diff（12 vs 9）

woca 已有：PreToolUse/PostToolUse/UserPromptSubmit/Stop/SubagentStop/SessionStart/
Notification/PreCompact/PostCompact。**缺失**：`SessionEnd`（会话结束清理）、
`PermissionRequest`（权限决策钩子——可在询问前改写/预批）、`PostSampling`（采样后
异步分析）。另有 StatusLine/FileSuggestion 等 UI 型钩子（低优先）。

## 11. 系统提示词与上下文

woca 已对齐：intro/tone/tool 使用/todos 指引/环境块/项目指令注入（CLAUDE.md 与
AGENTS.md 均支持）/git 上下文（internal/context）。

**缺失**：输出风格系统（outputStyles——整套人格/教学风格切换）；提示词分段缓存
（systemPromptSections 的 `/clear`、`/compact` 前缓存）；技能命令注入系统提示词；
markdown 产物规则与"常见误区"等细节段落；自适应段落（模型能力探测）。

## 12. 思考与多模态

- **thinkingConfig**：Claude Code 全链路传递 thinking 预算（含 max_output_tokens
  恢复时的升限联动、thinking 签名剥离）；woca 完全没有 thinking 参数通路
  （fork 的 partner 流会透传 thinking 块，但请求侧不配置预算）。
- **图像**：请求侧无图像输入通路（Read 图片、粘贴图片、校验/缩放）。

## 13. 会话系统

woca 已有：JSONL 落盘、`--resume last`、plan/todos/tasks 状态恢复、web 会话。
**缺失**：rewind（按文件快照回退对话）、branch/fork、会话元数据（rename/tag）、
导出、transcript 检索、`--resume` 的交互式选择 UI、后台任务表持久化。

## 14. 其它产品能力

- **插件生态**：Claude Code 的 plugin loader + marketplaces（README 称 fully
  working）；woca 只有 skills/agents 文件式加载。
- **设置层级**：managed settings / settingsSync / policyLimits（企业管控）；
  woca 只有用户级 + 项目级 settings.json。
- **SDK 模式**：`--output-format json/stream-json`、agent SDK、headless
  profiler；woca `-p` 仅文本输出。
- **通知**：终端铃/iTerm 通知（notifier）；woca 无。
- **限流体验**：rate-limit 消息、usage 重置提示、mock-limits；woca 仅重试。
- **内部/实验**（不列为差距）：voice、mobile、bridge/remote、buddy、VCR、
  telemetry、fastMode、chicago/computer-use。

## 15. 追赶路线图 v2（P3/P4）

**P3（核心能力，建议优先）**
1. Bash 后台任务：`run_in_background` + 完成通知（queue 注入）+ 输出查询/终止
2. `/init` 项目说明生成
3. `/cost`：UsageMetadata 累计 + 成本估算 + /context 联动
4. Hook 补齐：SessionEnd、PermissionRequest、PostSampling
5. `/rewind` + `/branch`：会话检查点（JSONL 分段 + 快照恢复）
6. 图像输入：Read 图片 → ContentBlocks 图像块 + 校验/缩放
7. thinkingConfig 通路（请求侧预算配置 + 与 max_tokens 恢复联动）
8. forkSubagent（Task fork 当前对话上下文）

**P4（体验/生态）**
9. `/doctor`、`/export`、`/rename`、`/agents` 管理 UI
10. 输出风格系统 + 提示词分段缓存
11. 插件加载器（marketplace 级）
12. ScheduleCron、LSPTool、worktree 工具
13. `--output-format json/stream-json` SDK 模式
14. 通知、主题、vim 模式、keybindings
15. MCP OAuth（McpAuthTool）、企业设置层级

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

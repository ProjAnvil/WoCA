# Concurrency Hardening Design（woca 并发加固设计）

- 日期：2026-08-17
- 状态：设计已定稿，待实施计划（writing-plans）
- 依据：并发审计结论（LSP 符号级核对全部锚点命中）+ 业界对照调研（opencode / OpenHands / Cline / Claude Code SDK，见附录）

## 1. 背景与目标

对 run/tool/task/teammate 四层 goroutine 骨架的审计结论：骨架健康（ctx 级联取消、带背压的 pump channel、原子 Claim、teammate 用 WithoutCancel 脱离父 run），不做重构。本 spec 修复两类问题：

1. **并发正确性缺口**：同 session 重叠 run 丢消息、memory prefetch 单槽互踩、refreshExtraTools 锁内网络 I/O、sessionStore 锁内文件 I/O；
2. **资源管理**：LSP 每次冷启动、workspace 缓存无界、teammate/task 无上限、SSE 静默丢事件。

## 2. 范围

**纳入**：P0 session 门；P1 三项竞态修复；P2 LSP 池 / workspace LRU / teammate-task 上限；P3 五项打磨；前端配合。

**明确排除（决策记录）**：

- **全局/per-workspace run 信号量**——已考虑并否决。woca 在 model 层已有完整瞬态重试（`internal/model` 的 `retryStream`：指数退避、`IsRetryable`、`switchModel`/`onFB` 降级；`agent.Config.ModelMaxRetries` 默认 2），并发流撞 provider 限流由 429+退避自节流；业界四家均无全局 cap。若 `/api/stats` 数据显示失控再评估。
- **Subagent 模板缓存**——已考虑并否决。业界无可参考实现；模板克隆的可变字段完备性风险高（漏一个字段即跨 run 状态泄漏），收益（图编译/BindTools 开销）不值得。`newSubagent` 维持每次新建。
- **排队持久化**——队列仅内存态，重启即丢（与 opencode "崩溃恢复有意推迟"一致）。

## 3. P0：Session 门（同 session run 串行）

### 3.1 问题

普通 run 无 single-flight：同 session 两个 run 各自加载同一历史、各自整存（`internal/server/server.go` 的 `newRegisteredRun`/`startRun` 落盘路径），后写覆盖先写，先完成 run 的回复丢失。goal 循环有 claim 单飞，普通 run 没有。

### 3.2 设计

新组件 `internal/server/admission.go`：`admission`，Server 持有。

```
admission
└─ sessions sync.Map  sessionID → *sessionGate   // 互斥锁 + FIFO waiter 列表
```

run goroutine 流程（handleCreateRun 照旧创建并注册 Run，状态 `queued`）：

1. 入 `sessionGate` FIFO，emit `run_queued{position}`；队头提升时其余 waiter 收 `queue_position{position}`；
2. 等待 ready 或 ctx.Done；
3. 获得后 emit `run_started` → 调用现有 `run.start(history, onDone)`；
4. **onDone 完成 transcript 落盘之后**才释放 session 门（显式调用，位于 onDone 返回之后，不进 defer 链）。

**释放时序是正确性核心**：下一条排队的 run 被提升时，前一条的回复必然已持久化，从根上消除覆盖丢消息。

### 3.3 与现有 handleQueueInput 的职责划分

- `queueInput`（现有，`server.go`）：目标 run **正在运行**时向其注入用户输入（等价 opencode 的 steer/queue delivery），行为不变；
- session 门（新增）：run 结束后按 FIFO 接力下一条 POST 的 run。
- 两者不重叠：POST 遇活 run 时进 session 门排队，此时 queueInput 不适用（活 run 属于前一条 POST）。

### 3.4 边界情况

- **队列深度上限**：`maxQueuedPerSession = 8`，超出 POST 返回 409（防 runaway 客户端；正常使用不可达）；
- **排队中取消**：waiter 的 select 带 ctx.Done，退出时从 FIFO 摘除并 emit cancelled，复用现有 `handleCancel`/`cancelRun`；
- **park（中断挂起）**：持有 session 门不释放（语义上仍是活跃 run），由 P3 parked TTL 兜底；
- **goal run**：同过 session 门；agent 级 goal claim 保留为纵深防御；
- **停机**：Serve ctx 取消 → 所有 waiter 中止，在跑 run 走现有取消路径。

### 3.5 HTTP/SSE 面

POST `/api/runs` 立即返回，run 含 `state: queued|running` 与 `queuePosition`；新增事件 `run_queued` / `queue_position` / `run_started`。

## 4. P1：竞态修复

### 4.1 memory prefetch 挂到 run 维度

现状：`memoryRuntime.prefetch`（`internal/agent/memorystore.go`）是 agent 级单槽，run B 的 `Begin` 覆盖 run A 未 `TakeAttachments` 的结果（现有 concurrency 测试已承认该互踩）。
设计：仿 `runws` ctx 装饰新增 per-run `memoryScope`（内含 `memoryPrefetch`）。`Begin(ctx,…)` 优先写 ctx 中的 scope；`TakeAttachments(ctx)` 从 scope 读并清空；ctx 无 scope 时回退现有 agent 级字段（CLI 单 run 路径不回归）。`beginMemoryPrefetch`/`takeMemoryAttachments` 签名补 ctx。

### 4.2 refreshExtraTools 去阻塞

现状：`refreshMu` 罩住 MCP re-list 网络 I/O（`internal/agent/refreshtools.go`），慢 server 拖慢所有 run 每一轮。
设计：`singleflight.Group` + `refreshTTL = 5min`。工具轮发现未过期直接跳过；过期则 `DoChan` 后台触发，本轮用旧绑定继续；完成后在 `refreshMu`（此后只保护微秒级换绑）下原子替换。`RefreshToolsNow` 绕过 TTL、仍被 singleflight 去重。

### 4.3 sessionStore 锁纪律

- 内存 index（`loadIndex` 启动加载）成为唯一事实源；`mu` 只保护 map 增删，锁内零 I/O；
- `recordMeta`：锁内改 map → 锁外 marshal → 经串行 writer（独立 `writeMu`）写 + rename，防交错；
- `list()`：只读内存索引，**零 JSONL 读**；`sessionMeta` 增加 `MessageCount`，save 时更新（msgs 在手）；旧 index 缺字段显示 0，下次 save 自愈。

## 5. P2：资源池化

### 5.1 LSP server 池

现状：每次 `lsp` tool call 冷启动 server → 诊断 → 关闭（`internal/tools/lsp.go` 的 `lspTool`）。
设计：`lspPool`，键 `(workspace, command)`：

- **复用**：已就绪 transport 直接 Call；池 mu 只做查找，Call 在锁外；
- **回收**：空闲 5 分钟后台关闭；
- **崩溃**：Call 报进程错误 → 摘除、重启重试一次；
- **熔断**：spawn 连续 2 次失败 → 标记 broken，冷却 10 分钟内直接报错不再尝试（吸收 opencode broken-set 教训，防 crash loop）；
- **关停**：Server 停机统一 shutdown。

`lspTransport` 接口保留为测试 fake 注入缝。

### 5.2 workspace 缓存 LRU

`wsPrompts`/`wsSkills`/`wsAgents`/`wsHooks` 四个 `sync.Map`（`internal/agent/agent.go`）→ 容量 32 的 LRU；`InvalidateWorkspacePrompts` 整表失效语义不变。

### 5.3 teammate/task 硬上限

TeamManager Spawn 时活跃 teammate 超 8 → 返回带指引的错误文本（"已达 teammate 上限，请改用顺序 Task"）；TaskManager 后台 task 同理上限 16。常量。**错误而非挂起**：模型可感知并重构计划，且避免 teammate 互等死锁（Claude Code SDK 为挂起等待，记录为已知备选）。

## 6. P3：打磨

### 6.1 SSE 序号与重放

- `Event` 增加单调 `seq`（Run 内原子计数，从 1 起）；
- `emit` 保持非阻塞，满时递增丢弃计数（暴露到 stats）；
- 重连：客户端带 `Last-Event-ID`（= seq），服务端从 `Run.history` 补发其后事件；超出 history 保留窗口的缺口返回全量快照事件兜底；
- 前端检测 seq 跳变即主动触发重放。

### 6.2 投机执行只读门

speculative dispatch（`internal/agent/streamingexec.go`）只放行只读工具，写操作等正式 toolsNode 执行；只读判定复用 `PermissionRequest.IsReadOnly` / 工具自声明（实现时确认判定来源）。

### 6.3 parked run TTL

park 时启动 2 小时 timer，超时自动 `cancelRun` + emit timeout 事件。业界无先例（OpenHands 无超时、opencode 推迟持久中断），标注为本地决策。

### 6.4 /api/stats

返回：active/queued runs、per-session 队列深度、teammate/task 计数、SSE 丢弃计数、LSP 池状态（活跃/broken）。为后续阈值调优提供数据。

### 6.5 panic recover 审计（实现期必做）

`toolsNode` 的 tool goroutine（`agent.go`）逐个审计：缺 recover 则补——panic 转结构化 ERROR 工具结果（含堆栈摘要），不得带崩进程（裸 WaitGroup 拦不住 panic）。

## 7. 前端配合（web/）

- run 状态机增加 `queued`：排队徽标 + 位置（`queue_position` 驱动）；排队中可取消；
- EventSource seq 跳变检测 → 拉重放补齐；
- `/api/stats` 展示（侧栏或设置页一处）。

## 8. 测试策略

全程 `go test -race`：

- **session 门**：FIFO 顺序；排队中取消；**落盘后才释放**的时序断言（hook `sessionStore.save`，断言释放晚于持久化完成）；深度超限 409；
- **prefetch**：并发两 run 的 Begin/TakeAttachments 互不覆盖（改造现有承认互踩的 concurrency 用例为断言不互踩）；
- **refresh**：fake 慢 MCP——工具轮不被阻塞、TTL 内不二次刷新、后台完成后换绑生效；
- **sessionStore**：并发 recordMeta 无交错；list() 零 JSONL 读（fake loader 断言不被调用）；
- **LSP 池**：fake transport——复用、崩溃重启、broken 熔断、空闲回收；
- **LRU**：容量淘汰与整表失效；
- **SSE**：seq 单调、重放补发、丢弃计数；
- 回归：server/agent 现有全量测试。

## 9. 落地顺序

P0（session 门，含前端 queued 徽标）→ P1（三项相互独立）→ P2（三项相互独立）→ P3（stats 随最后）。每步独立可合入、可回滚。

## 10. 风险与实现期确认清单

1. **释放时序依赖**：session 门释放以 onDone 同步落盘完成为前提；若未来 onDone 异步化需重新评估；
2. **SSE 补发窗口**：受 `Run.history` 保留策略限制，超窗兜底为全量快照；
3. **实现时确认四项**（审计中结构与结论一致但未读函数体）：`emit` 非阻塞细节、`recordMeta` 锁内 rename、`toolsNode` recover 现状、投机执行写工具现状；
4. 工作区现有 `store_test.go` 编译错误（`sql.List` 签名不匹配）与本 spec 无关，实施前先修复以获得干净基线。

## 11. 附录：业界对照（2026-08-17 调研）

| 决策点 | opencode | OpenHands | Cline | Claude Code SDK | woca 结论 |
|---|---|---|---|---|---|
| session 并发 | SessionRunCoordinator 每 session 串行；并发 turn 报错；steer/queue delivery | FIFOLock 串行 | 会话内排队 | 排队不拒绝 | session 门 + FIFO 排队 ✅ |
| 全局上限 | 无 | 无 | — | 无（限流是实际上限） | 不做，provider retry 兜底 ✅ |
| 事件恢复 | seq+aggregate_id，/sync/history 补发 | REST 历史 + WS since + ID 去重 | — | — | seq + Run.history 补发 ✅ |
| LSP | 按 root 常驻复用 + broken 熔断 | — | VSCode 内置 | — | 池化 + 熔断 + 空闲回收 ✅ |
| subagent 上限 | — | — | — | 有，达到时挂起 | 报错 + 指引（差异化保留） |
| 挂起超时 | 推迟实现 | 无 | — | — | 2h TTL（本地决策） |

来源：[Cline Tasks](https://docs.cline.bot/core-workflows/task-management) · [Claude Agent SDK Sessions](https://code.claude.com/docs/en/agent-sdk/sessions) · [Claude Agent SDK Subagents](https://code.claude.com/docs/en/agent-sdk/subagents) · [claude-agent-sdk-python#558](https://github.com/anthropics/claude-agent-sdk-python/issues/558) · [#812](https://github.com/anthropics/claude-agent-sdk-python/issues/812) · [deepwiki: opencode 并发](https://deepwiki.com/search/when-a-client-sends-a-new-chat_c35e537c-5753-43ff-aeaf-73108bb84abf) · [事件回放](https://deepwiki.com/search/how-does-a-client-recover-even_2e518dc-e764-4ffa-9444-2e7291bcbc14) · [LSP](https://deepwiki.com/search/how-does-opencode-manage-langu_ba3c4530-9336-48fe-bef8-0094501eece5) · [OpenHands 并发](https://deepwiki.com/search/three-questions-about-concurre_b2390073-7a3-46ab-a4d2-5e10ee03b70e)

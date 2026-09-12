# 子 agent HITL 改造设计:权限中断冒泡与可恢复子 agent

日期:2026-09-12
状态:已批准方向(用户确认"保持上下文隔离,补中断传播与可恢复性";要求 subagent 驱动的多阶段审批执行)

## 背景

Task 工具 spawn 的子 agent 是独立编译的 graph(`internal/agent/agent.go:1598` `newSubagent` → 全新 `agent.New`),这保证了上下文隔离(独立 history,主 agent 只拿回报告)——与 Claude Code 等业界 harness 一致,**保留不动**。

现状的洞:子 agent 继承了父的 permission mode,遇到写/Shell 工具会在自己的 graph 里触发 `lggraph.Interrupt`(`agent.go:1588` `askPermission`),但没人接——`sub.Invoke` 返回 `*lgtypes.GraphInterrupt` 错误,`runSubagent`(`internal/agent/task.go:312-357`)把它当普通错误,整个子 agent run 夭折,主 agent 只收到一段错误文本。对比 Claude Code:子 agent 的权限 prompt 会冒泡到主会话由用户批准。此外子 agent 用 MemorySaver + 一次性 thread(`task.go:318,366`),进程崩溃即丢,`--resume-checkpoint` 只救主 agent。

## 目标

1. 前台 Task 子 agent 内的权限中断冒泡为**主 graph 的审批中断**,走现有三条审批路径(CLI y/N、SDK `OnInterrupt`、server `POST /api/runs/{id}/answer`),用户批准后子 agent 从暂停点继续。
2. 主 agent 开持久 checkpoint(`--checkpoint-dir` / serve 的 sqlite saver)时,子 agent 的暂停状态同样持久化,跨进程 `--resume-checkpoint` 能把跑到一半等审批的子 agent 一起恢复。

## 非目标(YAGNI)

- 后台 Task(`run_in_background`)子 agent:它跑在独立 goroutine,不在主 graph 流内,无法 pause 主 graph;维持现状(中断即报错),文档注明。
- team/teammate 模式(`internal/agent/team.go`)的中断传播。
- 子 agent 内 AskUserRequest / PlanApprovalRequest 的接力(子 agent 不应直接问用户;维持报错)。
- 不改 langgraph 的 AddSubgraph 静态子图化(见方案选型)。
- 前端 Web UI 的新审批卡片(见"审批呈现"——复用现有 permission 载荷,前端零改动)。

## 方案选型

**A. AddSubgraph 真子图化**:把 Task 工具改造成 graph 节点/Send 扇出。v0.7.0 的中断冒泡、NS 路由、子图 checkpoint 命名空间全部免费获得。但 Task 是模型可调用的普通工具(动态定义、可并行、有 TaskOutput/TaskStop 配套),把它从 toolsNode 里拆成图结构是对 ReAct 循环的地面重建,风险与工作量都过大。**否决**。

**B. 中断接力(推荐)**:保持命令式子 agent 运行。子 agent 的 `GraphInterrupt` 被 Task handler 捕获,换成新的信封类型在主 graph 里重新 `lggraph.Interrupt`;父 graph 恢复时 handler 重入,用**确定性 thread ID** 找到暂停的子 agent,把用户的答案 resume 进去。机制与现有 `askPermission` 完全同构(节点重执行 → 同一位置再调 Interrupt → 拿到排队答案),只是多穿了一层。langgraph 的 subgraph 能力在此用不上,因为子 agent 不是注册子图;v0.7.0 对本方案的关键支撑是 durable saver 的跨进程恢复语义已验证(870250d 等)。

**C. 子 agent 内自动 deny**:把中断转成"权限被拒"文本让子 agent 继续。便宜但达不成"用户批准"的目标。**否决**。

## 设计(方案 B)

### 确定性 thread ID 与暂停状态寻址

子 agent thread ID 派生:`<parentThread>/<toolCallID>`。父 graph 恢复时 toolsNode 重执行,同一 AI 消息重放,tool call ID 稳定 → 派生出同一个 thread ID,无需额外状态即可寻址暂停的子 agent。

暂停状态存放分两档,与主 agent 的 checkpoint 档位对齐:

- **父有 durable saver**(CheckpointDir 或 serve 装配的 sqlite saver):子 agent 共享同一 saver(`newSubagent` 增加 saver 注入),`sub.Resume(ctx, thread, answer)` 跨进程可恢复。
- **父是 MemorySaver**(个人版默认):跨进程本来就恢复不了主 agent;进程内用 `map[threadID]*Agent` 缓存暂停的子 agent 实例(agent 新增 `pendingSubagents` 字段,带锁),恢复时直接对缓存实例 Resume。

### 新中断信封 SubagentInterrupt

```go
// SubagentInterrupt wraps a foreground Task subagent's permission pause so
// the parent graph surfaces it for approval. Answer semantics are the
// inner permission's ("y"/"n").
type SubagentInterrupt struct {
    Subagent   string            `json:"subagent"`    // display name (definition or type)
    ToolCallID string            `json:"tool_call_id"`
    ThreadID   string            `json:"thread_id"`   // derived subagent thread
    Inner      PermissionRequest `json:"inner"`
}
```

注册进 `internal/agent/interruptenc.go` 的 encode/decode 两个 switch(类型名 `"agent.SubagentInterrupt"`),保持持久 saver 的往返无损。答案仍是 `"y"`/`"n"` 标量,resume 信封零改动。

### Task handler 接力流程(internal/agent/task.go)

前台路径(`runSubagent` 的 `onText == nil` 分支)改造:

1. 派生 threadID = `<parentThread>/<toolCall.ID>`:parentThread 用 `a.LastThreadID()`(`agent.go:902`,Stream 启动时即设置,toolsNode 执行期间就是当前 run 的 thread)。
2. **重入检测**:若该 threadID 已有暂停子 agent(查 pendingSubagents map,或 durable 模式下查 saver 的 GetState),说明这是父 graph 恢复后的重执行 → 跳到第 4 步直接再调 `lggraph.Interrupt` 取答案。
3. **首次执行**:照常 `sub.Invoke(ctx, history, threadID)`。若返回 `*lgtypes.GraphInterrupt` 且 decode 后是 PermissionRequest:缓存子 agent 实例(memory 档)→ 调 `lggraph.Interrupt(ctx, encodeInterruptPayload(SubagentInterrupt{...}))` pause 主 graph。
4. **取答案**:`lggraph.Interrupt` 在恢复后返回答案(`parseAnswer` 同款 "y"/"n")。deny → 子 agent 丢弃,Task 返回"用户拒绝了权限"文本;allow → `sub.Resume(ctx, threadID, "y")` 继续子 agent。
5. 子 agent 后续再次暂停(顺序多个权限)→ 回到第 3 步的暂停分支,逐个审批(与 runner 现行的单中断逐个处理语义一致)。
6. 子 agent 正常完成 → 清理 pendingSubagents 缓存,返回最终文本。

非 PermissionRequest 的子 agent 中断(AskUser 等)维持现状:作为错误文本返回。

### 审批呈现(三条路径)

- **CLI**(`cmd/woca/main.go` `interruptHandler`):新增 `case agent.SubagentInterrupt`,打印 `Subagent '<name>' requests permission: <tool> — <reason>`,y/N 读取复用现有 `readYesNo`;非交互(headless)自动 deny——与现有语义一致。
- **SDK**(`sdk/interrupt.go`):`Interrupt` 新增 kind `"subagent_permission"`,携带 `Subagent string` + 内层 `PermissionInterrupt`;默认 handler 自动 deny(与无头姿态一致)。
- **Server**(`internal/server/run.go` `park`):interrupt 事件的 kind/payload 是泛型信封,server 的 interrupt handler 把 SubagentInterrupt 以 `kind="permission"` + payload 增加 `subagent` 字段透出,**前端零改动**(现有 permission 审批卡片照用)。确认 server 侧 handler 的组装点(runs.go 的 InterruptHandler)在实现时落地。

### 错误与边界

- 子 agent resume 失败(checkpoint 丢失/saver 不可用)→ Task 返回错误文本,主 agent 自行决定重试,不 crash。
- 同一批并发工具调用中一个 Task 暂停:继承现有 toolsNode 重执行语义(与现行权限中断相同,不引入新问题)。
- 嵌套 Task(子 agent 再有 Task 工具):机制按层递归可用;若子 agent 工具集本就不含 Task,则无此路径,实现时核实并在 spec 偏差里记录。
- `runTurn` 的 GraphInterrupt 转换(`agent.go:960-962`)只对主 graph;子 agent 的 GraphInterrupt 由 Task handler 用 `errors.As` 捕获,不经过 runTurn。

## 测试策略

全部用 FakeChatModel + 真实 woca graph,不打真实 API:

1. **接力端到端**:子 agent 脚本化请求一个写工具 → 主 agent 的 InterruptHandler 收到 SubagentInterrupt(断言 Subagent 名与内层 tool)→ 答 "y" → 子 agent 完成,Task 返回其最终文本。
2. **deny 路径**:答 "n" → Task 返回拒绝文本,子 agent 不执行写操作。
3. **顺序多权限**:子 agent 连续两个写工具 → 两次冒泡,逐一批准。
4. **跨进程(durable 档)**:sqlite saver,第一次 Invoke 暂停后,新建 Agent 实例(模拟新进程)Resume → 子 agent 续跑完成。
5. **信封往返**:SubagentInterrupt 过 encode/decode + sqlite saver 序列化后类型与字段无损。
6. **后台 Task 维持现状**:后台子 agent 权限中断仍返回错误文本(回归锁)。
7. SDK 层:`sdk/interrupt.go` 新 kind 的单测。

## 风险

- toolsNode 重执行语义是接力方案的基石,虽有 askPermission 先例,但 Task handler 更重(子 agent 完整 run);测试 1-3 即为兜底。
- pendingSubagents 内存缓存在主 agent 长寿会话里是弱引用泄漏面:Task 完成/拒绝时清理,TTL 不加(暂停中的子 agent 本就由主 graph 的暂停生命周期约束)。

# Agent 组件化重写：team / cron / memory 窄接口注入（设计 + 实施）

日期：2026-08-16。**状态：已实施完毕**（三个组件全部落地，验收标准全过；
实施细节见文末"实施记录"）。前置：两轮 code clean 已修完全部并发 P0 与 P2
遗留（见 `internal/agent/concurrency_test.go` 与 git 历史）；本设计处理评审
的最后一个架构级发现——"agent 上帝包 / Agent 上帝对象"。

## 1. 问题陈述（as-is）

`internal/agent` 是一个 40+ 文件的包，`Agent` 是一个 40+ 字段的结构体，至少
承载 8 个正交关注点：graph 编排、工具集管理、权限、compaction、team、cron、
goal、memory、usage/obs。具体坏味道（评审证据）：

1. **子文件反向操作 Agent 私有字段**：team.go/task.go/goal*.go/memorystore.go
   的方法全部以 `*Agent` 为接收者，读写字段横跨多个关注点
   （`joinTeam()` 已封装 team→子 Agent 的字段写入，是本次的种子方向）。
2. **子系统与宿主双向耦合**：`TeamManager` 持有 `lead *Agent`，但整个文件
   只用到一个能力 `newSubagent`；cron 的全部状态嵌在 Agent 里，但触发只需要
   一个能力 `QueueNotification`。
3. **不可独立测试**：memory 的 prefetch/retain/system2 语义只能通过构造完整
   Agent 来测（测试里遍布 `&Agent{memEngine: f, workdir: "/ws"}` 字面量）。
4. **锁职责混杂**：memory 的 system2 节流用 `a.mu`（Agent 全局锁），engine
   指针热替换的同步约定隐式依赖"整指针替换"约定。

### 现状依赖方向（实测，非猜测）

```
TeamManager ──(仅 1 处: newSubagent)──▶ Agent
Agent ──(ensureTeam/currentTeam/Close/StatusText/工具注册)──▶ TeamManager

cronState(嵌入 Agent) ──(仅 1 处: QueueNotification)──▶ Agent 的输入队列

memory 子系统字段: memory / memEngine / memPrefetch / memWorkspace / lastSystem2
  ──(loadSettings, obs 日志, a.mu, subagent 门禁)──▶ Agent
Agent graph 节点 ──(turn 开始 prefetch / 工具轮后 attach / retain)──▶ memory 子系统
```

关键洞察：三个子系统对宿主的真实需求面积极小（各 1–2 个能力），
这是教科书式的 DIP/ISP 适用场景。

## 2. 目标设计（to-be）

### 2.1 设计原则

- **DIP（依赖倒置）**：组件依赖窄接口，不依赖 `*Agent`。
- **ISP（接口隔离）**：每个接口只暴露消费方实际调用的方法。
- **SRP**：每个组件只有一个变更原因（调度逻辑变了改 cronScheduler，
  记忆策略变了改 memoryRuntime）。
- **KISS/YAGNI**：接口保持在包内不导出（包外没有第二实现的需求）；
  不拆包（组件间共享 graph/messages 类型，拆包收益低于成本）；
  Agent 的公共 API 保持不变（server/console/main 零改动）。
- **策略在边缘、机制在组件**：如"subagent 不写记忆"是宿主策略，
  保留在 Agent 委托层；memoryRuntime 只提供机制。

### 2.2 组件契约

#### cronScheduler（cron.go，状态提取 + 注入投递）

```go
// notificationSink is the single capability the scheduler needs from its
// host: delivering a due prompt into the session's input queue.
type notificationSink func(tag, text string)

type cronScheduler struct {
    notify  notificationSink // injected at construction
    mu      sync.Mutex
    next    int
    entries []*cronEntry
    cancel  context.CancelFunc
}

func newCronScheduler(notify notificationSink) *cronScheduler
func (s *cronScheduler) Add(name, prompt, spec string) (string, error)
func (s *cronScheduler) Delete(id string) bool
func (s *cronScheduler) List() []cronEntry
func (s *cronScheduler) Start()          // idempotent ticker loop
func (s *cronScheduler) Stop()           // teardown (Agent.Close)
func (s *cronScheduler) fireDue(now time.Time) // test hook
```

Agent 侧：字段 `cron *cronScheduler`（New() 时注入 `a.QueueNotification`）；
`addCron/deleteCron/listCron/startCron/stopCron/fireDueCron` 变为一行委托，
ScheduleCron 工具与 `Agent.Close()` 不变。

#### subagentFactory（team.go，宿主依赖收窄为接口）

```go
// subagentFactory is the narrow seam through which a team builds teammates:
// the rest of the lead agent stays invisible to the manager.
type subagentFactory interface {
    newSubagent(def *agents.Definition) (*Agent, error)
}

type TeamManager struct {
    factory subagentFactory // was: lead *Agent
    bus     *bus.Bus
    list    *TaskList
    mu      sync.Mutex
    members map[string]*teammate
    order   []string
    pokes   map[string]chan struct{}
}
```

`*Agent` 因已有 `newSubagent` 方法而隐式实现该接口（Go 结构化类型），
`ensureTeam()` 传 `a` 即可，调用点文本不变。测试可注入 fake factory
构造不依赖完整 lead 的 TeamManager。

#### memoryRuntime（memorystore.go + retain.go，值组件 + 自有锁）

```go
// memoryRuntime owns the long-term-memory subsystem: the KV store fallback,
// the factual engine, per-turn prefetch, the run workspace and the System 2
// throttle. It is a value field of Agent (never nil), disabled by a nil
// engine. All fields are guarded by mu; engine calls (IO) happen outside it.
type memoryRuntime struct {
    mu           sync.Mutex
    engine       memory.Engine
    store        *store.Store     // keyword-match fallback
    workspace    string           // current run workspace ("" = workdir)
    prefetch     memoryPrefetch   // per-turn recall state
    lastSystem2  map[string]int64 // per-workspace throttle
    loadSettings func() settings.Settings
    log          func(level, event string, fields map[string]any) // obs sink, nil-safe
}

func (m *memoryRuntime) Begin(ctx context.Context, msgs []messages.Message)
func (m *memoryRuntime) TakeAttachments() []messages.Message
func (m *memoryRuntime) Engine() memory.Engine
func (m *memoryRuntime) SetEngine(e memory.Engine)
func (m *memoryRuntime) SetWorkspace(ws string)
func (m *memoryRuntime) Workspace() string
func (m *memoryRuntime) RetainRun(ctx, history, final []messages.Message, runErr error)
func (m *memoryRuntime) RetainGoal(ctx context.Context, objective, status, summary string, runErr error)
func (m *memoryRuntime) RecordTaskEvent(ctx context.Context, name, description, report string, taskErr error)
func (m *memoryRuntime) system2Due(ws string, intervalS int64) bool
```

Agent 侧：5 个 memory 字段合并为 `mem memoryRuntime`（**值**字段：包内测试
的 `&Agent{mem: memoryRuntime{engine: f}}` 字面量写法得以保留）。
graph 节点改调 `a.mem.Begin / a.mem.TakeAttachments`；公共方法
`MemoryEngine/SetMemoryEngine/SetMemoryWorkspace/MemoryWorkspace/RetainRun/
RetainGoal` 保持签名不变，成为带策略门禁（`a.subagent`、workspace 解析）
的委托层。`memoryWorkspaceCtx`（ctx 值装饰）是包级函数，本就独立，不动。

### 2.3 并发模型

| 状态 | as-is 锁 | to-be 锁 |
| --- | --- | --- |
| prefetch 消费 + injected 标记 | memPrefetch.mu | memoryRuntime.mu（统一） |
| engine 热替换 / workspace | a.mu / 约定 | memoryRuntime.mu |
| system2 节流 | a.mu | memoryRuntime.mu |
| cron entries | cronState.mu（嵌入） | cronScheduler.mu（同语义） |
| team members/cancel/status | t.mu（已达标） | 不变 |

约束：**memoryRuntime.mu 与 cronScheduler.mu 的临界区内不做 IO、不回调
宿主**（engine 调用一律在锁外取快照后进行）——与既有 `refreshMu → a.mu`
的单向锁序一致，不引入新的锁交错。`Begin` 的后台 recall goroutine 在锁外
执行 `engine.Recall`，仅结果写回时短暂持锁。

### 2.4 迁移策略（每步独立可交付、测试全绿）

1. **cron**（最小、自包含）：提取结构体 + 注入 notify；Agent 方法变委托。
2. **team**：`lead *Agent` → `factory subagentFactory`；ensureTeam 传参改造。
3. **memory**（最大）：字段合并为值组件、方法搬家、Agent 留委托层；
   同步更新包内测试的字面量构造（`memEngine: f` → `mem: memoryRuntime{engine: f}`）。
4. 每步跑 `go test -race ./internal/agent/`；终态跑全仓 race + vet。

### 2.5 明确不做（本次范围外）

- **不拆 Go 包**：组件共享 graph/messages/config 类型，拆包会产生循环或
  依赖倒挂；等出现第二实现（如进程外 team）再拆。
- **不接口化 goal**：goalManager 已是独立状态结构，goalgraph 与 graph 节点
  耦合是本质复杂度（goal 循环就是 graph），强行隔离收益为负。
- **不改公共 API**：server/console/tui/main 零改动是硬约束（用委托层保证）。
- **不导出窄接口**：包内 seam 足够（YAGNI）；导出会冻结签名。

## 3. 验收标准

1. `grep -n "a\.memPrefetch\|a\.memEngine\|a\.memWorkspace\|a\.lastSystem2\|a\.memory\b"` 在
   agent 包非测试代码中零命中（字段全部收进 memoryRuntime）。
2. TeamManager 不再出现 `lead *Agent` 字段；`t.lead` 调用点全部消失。
3. cron 调度逻辑（parse/match/fire/ticker）可脱离 Agent 实例化测试。
4. 包外调用方（server/console/main/tui）diff 为零。
5. `go vet` + 全量 `go test -race` 绿；既有测试（含 concurrency_test.go）
   语义不变地通过。

## 4. 实施记录（2026-08-16 落地）

按第 2.4 节顺序完成，每步 `go test -race ./internal/agent/` 全绿：

1. **cron**：`cronState` 嵌入字段 → `cron *cronScheduler`（`newCronScheduler(
   a.QueueNotification)` 注入投递）。Agent 的 addCron/deleteCron/listCron/
   startCron/stopCron/fireDueCron 变为一行委托；`Agent.Close()` 与
   ScheduleCron 工具零改动。新增 `TestCronSchedulerStandalone`（无 Agent
   实例的组件级测试：注入 sink、分钟去重、删除后静默）。
2. **team**：`lead *Agent` → `factory subagentFactory`（单方法接口，
   `*Agent` 经 `newSubagent` 隐式实现）；`ensureTeam` 传 `a`，Spawn 调用
   `t.factory.newSubagent(def)`。
3. **memory**：5 个字段（memory/memEngine/memPrefetch/memWorkspace/
   lastSystem2）合并为值组件 `mem memoryRuntime`；`Begin/TakeAttachments/
   RetainRun/RetainGoal/RecordTaskEvent/system2Due` 搬家到组件（workspace
   由宿主解析后传入）；Agent 留下带策略门禁（subagent 写禁、workspace
   解析链）的委托层与 `obsLogSink()`。锁统一为 `memoryRuntime.mu`
   （替换原来 memPrefetch.mu + a.mu 两把），引擎调用全部锁外快照执行。
   测试字面量机械迁移为 `mem: memoryRuntime{engine: f, ...}`。
   新增 `TestMemoryRuntimeStandalone`（脱离 Agent 的 prefetch + retain）。

验收复核（全部通过）：旧字段 `a.memPrefetch/a.memEngine/a.memWorkspace/
a.lastSystem2` 在非测试代码零命中；TeamManager 无 lead 字段；cron 与
memory 均有无 Agent 的组件测试；server/console/tui/main/runner 本轮零
改动；`go vet` + 全仓 `go test -race` 绿。

实施中的语义保真要点：loadSettings 为 nil 时 autoretain 默认开（测试契约，
`memoryRuntime.autoRetain()` 保留）；System2 在 nil loader 时默认关（与原
实现一致）；obs 写失败日志经 `m.warn`（nil-sink 安全）。

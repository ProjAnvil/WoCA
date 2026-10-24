# 依赖升级：langchain-golang v0.11.1 + LadyM v0.8.3

分支 `feat/memory-deps-upgrade`（from `main`）
工作区 `.worktrees/memory-deps-upgrade`
调研结论（逐签名核对）：`woca-deps-upgrade-research` 记忆 + 2026-10-24 会话报告。

## 1. 为什么要做

- langchain-golang v0.11.1（f2c3546）：checkpoint 稳定性修复（exit-durability anchor ID 预留、并发 batch 按 run ID 校验、清理失败保留操作错误）+ Go 1.26 现代化，无公共 API 变化。
- LadyM v0.5.2 → v0.8.3（82 提交）：并发数据面（`Engine.Scope`）、recall_ops 操作历史检索、L4 图自维护、CJK tokenizer 配置、Prometheus /metrics、system2 leader 选举。兼容性已逐签名核对：woca 调用的引擎方法原样存在，config/schema 只增不减，facade/adapter 不变。

## 2. 约束

- 用户纪律：不直接改 fork；woca 只消费 remote tag，零本地 replace。
- 形态分叉走接缝：新能力用 capability 接口 + 类型断言 + nil-guard，不改 `memory.Engine` 主接口，不加运行时形态嗅探。
- 记忆写路径永不阻塞 run：读失败降级空结果（既有语义，不许回退）。
- 每个任务后跑该任务的验证命令；最终全量 `go test ./... -race`。

## Task 1: langchain-golang v0.11.0 → v0.11.1 + checkpoint v0.3.8

1. `go get github.com/projanvil/langchain-golang@v0.11.1 github.com/projanvil/langchain-golang/langgraph/checkpoint/postgres@v0.3.8 github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite@v0.3.8 && go mod tidy`
2. `go build ./...`
3. `go test ./internal/memory/... ./internal/cpserde/... ./internal/tokens/... ./internal/runner/... -count=1`
   - Expected: 全绿（API 无变化，预期零代码适配）。

Commit: `deps: langchain-golang v0.11.1, checkpoint v0.3.8`

## Task 2: LadyM v0.5.2 → v0.8.3（编译层）

1. `go get github.com/ProjAnvil/LadyM@v0.8.3 && go mod tidy`
2. `go build ./...` —— 预期零代码改动可编译（调研结论）。
3. 检查 `cfg.EmbeddingCJKTokenizer` 新默认值 `"dict"` 对内嵌构建的影响：读 LadyM v0.8.3 `engine` 构建 + tokenizer 解析路径，确认 dict 数据缺失时的行为。若会改变 v0.5.2 的构建/分词行为，在 `internal/memory/ladym.go` Build 里显式设置该字段保持旧行为，并留注释。
4. `go test ./internal/memory/... -count=1`
   - Expected: 全绿。

Commit: `deps: LadyM v0.8.3`

## Task 3: 内嵌引擎拆全局锁（v0.6.0 Scope）

现状：`internal/memory/ladym.go` 的 `LadyM` 用一把 mutex 串行所有调用 + `withWorkspace` 的 SetWorkspace/恢复 舞步（注释引用 ladyM v0.3.1 无同步）。v0.6.0 起引擎并发安全：引擎自身 mu 只护 llmClassify/agents 懒加载；写路径用 `Engine.Scope(ws)`（Scope.Remember/RecordEvent 签名与引擎同名方法一致）；引擎显式接收 workspace 的方法（Recall/SearchCode/List/StatsFor/Consolidate/Proceduralize/ExtractMentalModels/PredictForwardIntents/Decay/CountRecentEpisodes）不需要锁。

1. 写测试 `TestLadyMConcurrentWorkspaces`（`ladym_engine_test.go`）：两个 workspace 并发 Remember + Recall + RecordEvent（goroutine + waitgroup），断言各自读回各自 workspace 的内容、互不串写。`go test ./internal/memory -run TestLadyMConcurrentWorkspaces -race`——对现实现也绿（表征测试，记录进台账）。
2. 重构：读路径去锁；Remember/RecordEvent 走 `l.eng.Scope(ws)`；Consolidate 组合里所有引擎调用本就带 ws 参数，去锁；锁只剩 `Close`（保留 mu 或改 sync.Once，择一，注释说明）。
3. 更新 `LadyM` 结构体注释（删掉 v0.3.1 时代的无同步描述，写明代 v0.6.0 的 Scope/存储层串行契约）。
4. `go test ./internal/memory -race -count=1`
   - Expected: 全绿。

Commit: `perf(memory): drop the embedded engine global lock`

## Task 4: Consolidate 并入 L4 自维护

1. 写测试 `TestConsolidateSweepsDanglingEdges`：构造内嵌引擎 → 直接 SQL 往 sqlite 库插一条指向不存在 memory 的 edge（造悬空边）→ `LadyM.Consolidate` → 断言该 edge 被清掉。先跑，RED（现实现不清）。
2. 实现：`Consolidate` 组合在 Decay 之后追加 `l.eng.MaintainL4(ws)`，把报告计数并进返回字符串。
3. 再跑该测试，GREEN。

Commit: `feat(memory): sweep dangling L4 edges in consolidation cycles`

## Task 5: 资源槽 + recall_ops 最小接缝（内嵌专属能力）

设计：capability 接口（不改 `memory.Engine`）：

```go
// internal/memory/operations.go
type Operation struct { Resources []string; Action, Summary, Outcome string }
type OperationHit struct { ID, Action, Summary, Outcome string; Resources []string; Score float64 }
type OperationEngine interface {
    RecordOperation(ctx, workspace string, op Operation) (string, error)
    RecallOperations(ctx, workspace, resource string, topK int) ([]OperationHit, error)
}
```

1. 写测试：内嵌 `LadyM` 实现 `OperationEngine`——RecordOperation 带 resources 落库，RecallOperations 按资源路径查回。先写先跑（编译失败=RED）。
2. 实现（查 LadyM v0.8.3 `engine.OperationEvent`/`OperationsQuery` 字段后对齐）。
3. retain 接线：`Agent.RetainRun` 计算 `a.FileHistory().Pending(ctx)` 的去重路径列表（截断上限 20），传给 `memoryRuntime.RetainRun`（签名加 resources 参数）；runtime 侧对引擎做 `OperationEngine` 类型断言，命中且 resources 非空时在 RecordEvent 后追加一条 RecordOperation（action="edit_files"）。远程引擎不实现该接口 → 企业版此能力关闭（与 ErrIndexRequiresSandbox 同类语义）。
4. 查询面：`internal/server/memoryapi.go` 加 `GET /api/memory/operations?workspace=&resource=&top_k=`，走同断言；远程引擎返回 501（沿用既有错误码约定，先查 httpx/codes.go 有无贴合码，无则按既有规范处理）。
5. `go test ./internal/memory/... ./internal/agent/... ./internal/server/... -count=1`
   - Expected: 全绿。

Commit: `feat(memory): record file operations for recall_ops (embedded engine)`

## Task 6: 全量验证

1. `go build ./...`
2. `go test ./... -race -count=1`（全仓 80+ 包）
   - Expected: 全绿。已知环境噪音：pluginrt pinchtab 测试在机器高载下可能假失败（单独复跑甄别）。
3. 不动 web 前端，无需 npm build。

Commit: 无改动则跳过。

## 评审焦点（Final review 用）

- Task 3 拆锁后：`Remember` 的注意力门 `Metadata["gated"]=="dropped"` 契约是否原样保留；并发下 L0 working buffer 是否按 workspace 隔离（Scope 语义）。
- Task 5：resources 是否可能含敏感路径进入第三方模型（consolidation 用继承分类器）——个人版本地模型可接受，留注释。
- LadyM adapter 在 langchain v0.11.1 下的行为（不只编译）：race 全量里 memory 包测试即覆盖。

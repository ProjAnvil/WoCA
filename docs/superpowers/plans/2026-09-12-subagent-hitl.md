# Subagent HITL(权限中断冒泡 + 可恢复子 agent)Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Task 工具的前台子 agent 内权限中断冒泡为主 graph 的审批中断,用户批准后子 agent 从暂停点继续;共享 checkpointer 使子 agent 暂停状态可跨进程恢复。

**Architecture:** 中断接力(非 AddSubgraph 子图化):子 agent 保持独立 graph 与上下文隔离;Task handler 捕获子 agent 的 `*lgtypes.GraphInterrupt`,包装成 `SubagentInterrupt` 信封在主 graph 重新 `lggraph.Interrupt`;恢复时 toolsNode 重执行,handler 用确定性派生 thread ID 找到子 agent 的 checkpoint,把答案 resume 进去。toolsNode 增加执行期中断捕获 + 已完成工具结果备忘(防止重执行重放副作用)。

**Tech Stack:** Go,langchain-golang v0.7.0(langgraph Interrupt panic 语义、Saver.DeleteThread、CompiledGraph.GetState)。

**Spec:** `docs/superpowers/specs/2026-09-12-subagent-hitl-design.md`

## Global Constraints

- **子 agent 保持独立 graph 与独立上下文**,不用 AddSubgraph;只新增中断接力与 checkpoint 共享。
- **后台 Task(run_in_background)行为不变**:子 agent 权限中断仍返回错误文本。
- 子 agent 内非 PermissionRequest 的中断(AskUser/PlanApproval)维持现状:作为错误返回。
- `lggraph.Interrupt` 是 **panic 语义**(`*types.GraphInterrupt`),严禁从 toolsNode 的执行 goroutine 直接抛出(会被 agent.go 的 recover 吞成工具错误文本);必须捕获后在节点主 goroutine 重抛。
- 答案仍是 `"y"`/`"n"` 标量;interruptenc 信封只对 struct 载荷编码。
- 不改 langchain-golang 依赖;不改 `internal/sdkout` wire 格式。
- 每个 Task 完成后 `go build ./... && go vet ./internal/agent/` 必须干净;测试全部用 FakeChatModel,不打真实 API。

---

### Task 1: SubagentInterrupt 信封类型与编码注册

**Files:**
- Modify: `internal/agent/agent.go`(`PermissionRequest` 类型定义附近,约 :115-135)
- Modify: `internal/agent/interruptenc.go:30-49`(encode switch)与 :66-80(decode switch)
- Test: `internal/agent/interruptenc_test.go`(若不存在则新建;先 grep 确认)

**Interfaces:**
- Produces:
  - `type SubagentInterrupt struct { Subagent string; ToolCallID string; ThreadID string; Inner PermissionRequest }`,json tag 分别为 `subagent`/`tool_call_id`/`thread_id`/`inner`
  - 经 `encodeInterruptPayload`/`decodeInterruptPayload` 往返无损

- [ ] **Step 1: 写失败测试**

`internal/agent/interruptenc_test.go`(若已有同名文件则追加):

```go
func TestSubagentInterruptRoundTrip(t *testing.T) {
	orig := SubagentInterrupt{
		Subagent:   "researcher",
		ToolCallID: "call_123",
		ThreadID:   "run-abc/sub-call_123",
		Inner: PermissionRequest{
			ToolName: "Write", Reason: "write file",
			ToolArgs: map[string]any{"path": "/tmp/x"},
		},
	}
	enc := encodeInterruptPayload(orig)
	// 模拟持久 saver 的 JSON 往返。
	blob, err := json.Marshal(enc)
	if err != nil {
		t.Fatal(err)
	}
	var back any
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatal(err)
	}
	got, ok := decodeInterruptPayload(back).(SubagentInterrupt)
	if !ok {
		t.Fatalf("decoded %T, want SubagentInterrupt", decodeInterruptPayload(back))
	}
	if got.Subagent != orig.Subagent || got.ToolCallID != orig.ToolCallID ||
		got.ThreadID != orig.ThreadID || got.Inner.ToolName != "Write" ||
		got.Inner.ToolArgs["path"] != "/tmp/x" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestSubagentInterruptRoundTrip -v`
Expected: FAIL,编译错误 `undefined: SubagentInterrupt`

- [ ] **Step 3: 实现**

`internal/agent/agent.go`,在 `PlanApprovalRequest` 定义后追加:

```go
// SubagentInterrupt wraps a foreground Task subagent's permission pause so
// the parent graph surfaces it for approval. The answer semantics are the
// inner permission's ("y"/"n"); ThreadID is the derived subagent checkpoint
// thread used to resume the child after approval.
type SubagentInterrupt struct {
	Subagent   string            `json:"subagent"`
	ToolCallID string            `json:"tool_call_id"`
	ThreadID   string            `json:"thread_id"`
	Inner      PermissionRequest `json:"inner"`
}
```

`internal/agent/interruptenc.go`:
- encode switch 的 case 列表(`case PermissionRequest, AskUserRequest, AskUserAnswer, PlanApprovalRequest, GoalDelta:`)末尾加 `SubagentInterrupt`。
- decode switch 加 `case "agent.SubagentInterrupt": out = &SubagentInterrupt{}`(注意现有 case 用的是不带包路径前缀的 `%T` 输出格式,先读文件确认现有条目格式再对齐——现有为 `"agent.PermissionRequest"` 样式)。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./internal/agent/ -run TestSubagentInterruptRoundTrip -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go internal/agent/interruptenc.go internal/agent/interruptenc_test.go
git commit -m "feat(agent): SubagentInterrupt envelope type with serde round-trip"
```

---

### Task 2: 工具调用 ID 传递、thread 派生与子 agent 共享 saver

**Files:**
- Modify: `internal/agent/task.go`(新增 helper)
- Modify: `internal/agent/agent.go:1375`(execute 调用点装饰 ctx)与 `newSubagent`(:1608-1619 的 Config)
- Test: `internal/agent/task.go` 的 helper 测试放 `internal/agent/taskrelay_test.go`(新建)

**Interfaces:**
- Consumes: `a.saver`(`agent.go:264`,graph 编译用的 checkpointer,永不为 nil——默认 MemorySaver);`a.LastThreadID()`(:903)。
- Produces:
  - `func withToolCallID(ctx context.Context, id string) context.Context`
  - `func toolCallIDFrom(ctx context.Context) string`
  - `func deriveSubagentThread(parentThread, callID, prompt string) string` — 形状 `<parentThread>/sub-<callID>-<sha256(prompt)前8字节hex>`;callID 为空时退化为 `<parentThread>/sub-<sha256(prompt)前8字节hex>`
  - `func (a *Agent) subagentStarted(ctx context.Context, sub *Agent, thread string) bool` — 用 `sub.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: thread})` 判断 checkpoint 是否存在(有 Values 或 Interrupts 即已启动);err 时返回 false
  - `newSubagent` 的 Config 新增 `Checkpointer: a.saver`

- [ ] **Step 1: 写失败测试**

`internal/agent/taskrelay_test.go`:

```go
package agent

import (
	"context"
	"strings"
	"testing"
)

func TestDeriveSubagentThreadDeterministic(t *testing.T) {
	a := deriveSubagentThread("run-1", "call_9", "do the thing")
	b := deriveSubagentThread("run-1", "call_9", "do the thing")
	if a != b {
		t.Fatalf("not deterministic: %q vs %q", a, b)
	}
	if !strings.HasPrefix(a, "run-1/sub-call_9-") {
		t.Fatalf("shape = %q", a)
	}
	// 同 prompt 不同 call ID 不碰撞;无 call ID 时退化为 prompt 摘要。
	if c := deriveSubagentThread("run-1", "call_10", "do the thing"); c == a {
		t.Fatal("different call IDs must not collide")
	}
	if d := deriveSubagentThread("run-1", "", "do the thing"); !strings.HasPrefix(d, "run-1/sub-") {
		t.Fatalf("no-callID fallback shape = %q", d)
	}
}

func TestToolCallIDContextRoundTrip(t *testing.T) {
	ctx := withToolCallID(context.Background(), "call_42")
	if got := toolCallIDFrom(ctx); got != "call_42" {
		t.Fatalf("got %q", got)
	}
	if got := toolCallIDFrom(context.Background()); got != "" {
		t.Fatalf("empty ctx gave %q", got)
	}
}
```

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run 'TestDeriveSubagentThread|TestToolCallID' -v`
Expected: FAIL 编译错误

- [ ] **Step 3: 实现**

`internal/agent/task.go` 顶部(import 加 `crypto/sha256`、`encoding/hex`)追加:

```go
// toolCallIDKey carries the executing tool call's ID through ctx so the Task
// tool can derive a stable subagent thread across interrupt re-executions.
type toolCallIDKey struct{}

func withToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

func toolCallIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(toolCallIDKey{}).(string)
	return v
}

// deriveSubagentThread mints the subagent's checkpoint thread: stable across
// the parent toolsNode's interrupt re-executions (same replayed tool call),
// unique per call.
func deriveSubagentThread(parentThread, callID, prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	digest := hex.EncodeToString(sum[:8])
	if callID == "" {
		return parentThread + "/sub-" + digest
	}
	return parentThread + "/sub-" + callID + "-" + digest
}

// subagentStarted reports whether the derived thread already has a
// checkpoint on the shared saver — i.e. this handler execution is a
// re-entry after a parent-graph resume, not a fresh spawn.
func (a *Agent) subagentStarted(ctx context.Context, sub *Agent, thread string) bool {
	snap, err := sub.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: thread})
	if err != nil {
		return false
	}
	return len(snap.Values) > 0 || len(snap.Interrupts) > 0
}
```

(task.go 的 import 加 `lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"`。)

`internal/agent/agent.go`:
- 执行调用点(约 :1375 `res := a.execute(ctx, call.Name, call.Args)` 所在 goroutine 开头)改为 `res := a.execute(withToolCallID(ctx, call.ID), call.Name, call.Args)`。
- `newSubagent` 的 `New(Config{...})`(:1608-1619)加一行 `Checkpointer: a.saver,` 并加注释:`// Share the parent's saver so a paused subagent resumes across processes when the parent is durable (and in-process otherwise).`

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/agent/ -run 'TestDeriveSubagentThread|TestToolCallID' -v && go test ./internal/agent/ -count=1`
Expected: 新测试 PASS;包内既有测试全绿(共享 saver 不破坏现状,因为子 agent thread 与父不同)

- [ ] **Step 5: Commit**

```bash
git add internal/agent/task.go internal/agent/agent.go internal/agent/taskrelay_test.go
git commit -m "feat(agent): tool-call ctx IDs, derived subagent threads, shared checkpointer"
```

---

### Task 3: toolsNode 执行期中断捕获与结果备忘

**Files:**
- Modify: `internal/agent/agent.go` toolsNode(:1291-1433)与 Agent 结构体(:240-270 区域,加字段)
- Test: `internal/agent/taskrelay_test.go`(追加)

**Interfaces:**
- Consumes: Task 2 的 helper;`lgtypes.GraphInterrupt`(panic 载荷)。
- Produces:
  - Agent 新字段 `batchMemo`(已完成工具结果备忘,结构自定,需并发安全,键为 tool call ID,值为 `messages.Message`)
  - toolsNode 行为契约:执行 goroutine 里的 `*lgtypes.GraphInterrupt` panic 被捕获得存,wg.Wait 后由节点主 goroutine 重抛第一个;重执行时已完成调用直接取用备忘结果,不再执行

- [ ] **Step 1: 写失败测试**

测试要点:同一批两个调用,一个已成功执行(计数),一个抛出 GraphInterrupt panic;节点重执行时成功那个不得重跑。用例骨架:

```go
// TestToolsNodeInterruptSkipsCompletedCalls: a batch [countingTool, pausingTool]
// pauses mid-execution; on re-execution the counting tool must not re-run.
func TestToolsNodeInterruptSkipsCompletedCalls(t *testing.T) {
	// 用一个自定义 tools.Tool:countingTool 记录调用次数并返回 "ok";
	// pausingTool 调用 lggraph.Interrupt(ctx, map[string]any{"kind":"test"}) (会 panic)。
	// 通过 New(Config{...}) 建 agent,Registry 注册这两个工具,FakeChatModel
	// 第一次响应带这两个 tool calls,第二次响应(恢复后)为最终文本。
	// 用 runner 或直接 a.Stream + ResumeStream 驱动:
	//   1. Stream 跑到 pause(runner.InterruptHandler 收到 value,答 "y")
	//   2. 恢复后断言 countingTool.calls == 1(若重跑则为 2)
	// 参考现有测试驱动方式:internal/agent/streamfallback_test.go 的 agent 构造,
	// internal/agent/usage_test.go:34 的 Invoke 驱动。InterruptHandler 用法见
	// internal/runner/runner.go:95 的 Run(ctx, history)。
}
```

注意:这个测试需要真实的 tools.Tool 实现——参考 `coretools.FromFunc` 用法(internal/agent/task.go:200)构造两个函数工具;pausingTool 的函数体里 `lggraph.Interrupt(ctx, "pause-here")`(从任务外传入的 ctx 不行,必须用工具收到的 ctx)。驱动方式:不用 runner,直接用 `a.Stream(ctx, history, thread)` 消费到 interrupt chunk,再 `a.ResumeStream(ctx, thread, "y")` 消费完——参考 `agent.go:1018` ResumeStream 签名与现有流式测试。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestToolsNodeInterruptSkipsCompletedCalls -v`
Expected: FAIL(countingTool 被重跑,calls == 2;或 GraphInterrupt 被 recover 吞掉变成错误文本——两种失败都证明现状不work)

- [ ] **Step 3: 实现**

`internal/agent/agent.go`:

1. Agent 结构体加字段(带上锁或用 sync.Map,注释语义:同一批工具调用在 interrupt 重执行间的结果备忘,节点正常返回时清理本批键):

```go
	// batchMemo holds completed tool results of the in-flight call batch so
	// an interrupt re-execution does not re-run tools that already executed
	// (side effects). Keys are tool call IDs; entries are deleted when the
	// tools node returns normally.
	batchMemo sync.Map // map[string]messages.Message
```

2. toolsNode 改造:
   - 执行 goroutine 的 `recover()` 里(agent.go:1366-1374)首先判断 `if gi, ok := rec.(*lgtypes.GraphInterrupt); ok { 存到 per-index 槽位; return }`(新增 `paused := make([]*lgtypes.GraphInterrupt, len(calls))` 切片,goroutine 写入自己的下标,wg.Wait 后读无竞态)。
   - 正常完成的 `results[i]`(执行成功后):`a.batchMemo.Store(call.ID, results[i])`。
   - 执行循环开头(每个 call):`if m, ok := a.batchMemo.Load(call.ID); ok { results[i] = m.(messages.Message); continue }`(放在 futures/overrides 判断之后、run 判断之前的合适位置——备忘命中的调用也应跳过 gate:gate 循环里同样先查备忘,命中则直接填结果并 continue,避免重ask权限)。
   - `wg.Wait()` 之后、结果合并之前:找到第一个非 nil 的 `paused[i]`,若有则 `panic(paused[i])`(节点主 goroutine 重抛,graph executor 捕获转 pause)。
   - 节点正常返回前(两处 return:MaxTurns cap 与正常 return):`for _, call := range calls { a.batchMemo.Delete(call.ID) }`。

3. 注意 gate 阶段的备忘命中:重执行时 gate 循环对已完成调用直接 `results[i] = memo; continue`,不再走 checkPermission(避免重复 ask 消耗 resume 队列)。

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/agent/ -run TestToolsNodeInterruptSkipsCompletedCalls -v && go test ./internal/agent/ -count=1`
Expected: 新测试 PASS(calls == 1);全包绿。特别注意既有 interrupt/权限相关测试(grep 找 `GraphInterrupt` 的测试)不能破。

- [ ] **Step 5: Commit**

```bash
git add internal/agent/agent.go internal/agent/taskrelay_test.go
git commit -m "feat(agent): capture execution-phase interrupts, memoize completed tool results"
```

---

### Task 4: runSubagent 前台接力核心

**Files:**
- Modify: `internal/agent/task.go`(`runSubagent` 前台路径 :312-323 与 `runTask` 调用点 :256-261)
- Test: `internal/agent/taskrelay_test.go`(追加端到端)

**Interfaces:**
- Consumes: Task 1 的 `SubagentInterrupt`;Task 2 的 `deriveSubagentThread`/`toolCallIDFrom`/`subagentStarted`/共享 saver;Task 3 的执行期中断捕获;`agent.DecodeInterruptValue`/`parseAnswer`/`decodeResumeValue`;`sub.Resume(ctx, thread, "y")`(agent.go:971)。
- Produces: 前台 Task 子 agent 权限中断冒泡行为;`runTask` 前台分支改调新函数 `a.runForegroundTask(ctx, history, def, subagentName(def, args), prompt)`。

- [ ] **Step 1: 写失败测试(端到端:allow / deny / 顺序双权限)**

```go
// TestForegroundSubagentPermissionRelay: main model scripts [Task tool call, final];
// subagent model (Config.SubagentModel) scripts [Write tool call, final "sub done"].
// 驱动:a.Stream 到 interrupt chunk → 断言 DecodeInterruptValue 得 SubagentInterrupt,
// Subagent 名与 Inner.ToolName=="Write" 正确 → ResumeStream "y" → 子 agent 完成,
// 最终 transcript 的 Task 工具结果含 "sub done"。
//
// TestForegroundSubagentPermissionDenied: 同上但答 "n" → Task 结果含拒绝文本,
// 子 agent 的 Write 未执行(用假 Write 或读 tools 注册表的副作用计数)。
//
// TestForegroundSubagentTwoSequentialPermissions: 子 agent 脚本 [Write, Write, final],
// 两次冒泡逐一点 y → 完成。
```

测试构造参考:父 agent `New(Config{Model: 父Fake, SubagentModel: 子Fake, PermissionMode: config.ModeDefault, Registry: tools.NewRegistry(t.TempDir()), ...})`;Write 工具需要权限(default mode 下写工具触发 ask)。FakeChatModel 脚本参考 `internal/agent/usage_test.go:24` 与 streamfallback_test.go。interrupt chunk 检测参考 `internal/runner/runner.go:64-75` 的 chunkInterrupts 逻辑(values/updates payload 里的 `lgcheckpoint.ReservedInterrupt`)。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestForegroundSubagent -v`
Expected: FAIL(现状:子 agent 中断使整个 Task 报错,无 SubagentInterrupt 出现)

- [ ] **Step 3: 实现**

`internal/agent/task.go`,新增接力函数并改 `runTask` 前台分支(`report, err := a.runSubagent(ctx, history, nil, def)` → `report, err := a.runForegroundTask(ctx, history, def, subagentName(def, args), prompt)`):

```go
// runForegroundTask runs a foreground subagent with interrupt relay: a
// permission pause inside the subagent re-interrupts the PARENT graph as a
// SubagentInterrupt, so the user approves it on the main session; on resume
// this handler re-executes, finds the child's checkpoint by its derived
// thread, and resumes it with the answer.
func (a *Agent) runForegroundTask(ctx context.Context, history []messages.Message, def *agents.Definition, name, prompt string) (string, error) {
	thread := deriveSubagentThread(a.LastThreadID(), toolCallIDFrom(ctx), prompt)
	sub, err := a.newSubagent(ctx, def)
	if err != nil {
		return "", err
	}
	started := a.subagentStarted(ctx, sub, thread)
	var pending SubagentInterrupt
	for {
		var msgs []messages.Message
		if !started {
			msgs, err = sub.Invoke(ctx, history, thread)
			started = true
		} else {
			// Re-entry after a parent resume: re-raise at the same point to
			// collect the queued answer (langgraph consumes resume values in
			// Interrupt call order, one per execution).
			answer := decodeResumeValue(decodeInterruptPayload(lggraph.Interrupt(ctx, encodeInterruptPayload(pending))))
			if !parseAnswer(answer) {
				_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
				return fmt.Sprintf("Subagent %q paused for a permission (%s) the user denied; the task was aborted.", name, pending.Inner.ToolName), nil
			}
			msgs, err = sub.Resume(ctx, thread, "y")
		}
		var gi *lgtypes.GraphInterrupt
		if !errors.As(err, &gi) {
			if err == nil {
				_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
				return lastAIText(msgs), nil
			}
			return "", err
		}
		req, ok := decodeInterruptPayload(gi.Interrupt.Value).(PermissionRequest)
		if !ok {
			// Non-permission pauses (AskUser etc.) stay fatal for subagents.
			return "", err
		}
		pending = SubagentInterrupt{
			Subagent:   name,
			ToolCallID: toolCallIDFrom(ctx),
			ThreadID:   thread,
			Inner:      req,
		}
		// Pause the parent graph. On the pause pass this call never returns
		// (panics GraphInterrupt); the re-execution restarts this function
		// and takes the started==true branch above.
		answer := decodeResumeValue(decodeInterruptPayload(lggraph.Interrupt(ctx, encodeInterruptPayload(pending))))
		if !parseAnswer(answer) {
			_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
			return fmt.Sprintf("Subagent %q paused for a permission (%s) the user denied; the task was aborted.", name, pending.Inner.ToolName), nil
		}
		msgs, err = sub.Resume(ctx, thread, "y")
		if err == nil {
			_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
			return lastAIText(msgs), nil
		}
		// The child paused again (sequential permission): loop to relay it.
		var gi2 *lgtypes.GraphInterrupt
		if !errors.As(err, &gi2) {
			return "", err
		}
		req2, ok := decodeInterruptPayload(gi2.Interrupt.Value).(PermissionRequest)
		if !ok {
			return "", err
		}
		pending = SubagentInterrupt{Subagent: name, ToolCallID: toolCallIDFrom(ctx), ThreadID: thread, Inner: req2}
		// Pause the parent again immediately; re-execution restarts at top.
		lggraph.Interrupt(ctx, encodeInterruptPayload(pending))
	}
}
```

注意:末尾这次 `lggraph.Interrupt` 的返回值刻意丢弃——它永不返回(无排队答案则 pause);恢复后整个函数重执行,走 `started==true` 分支消费下一个答案。若实现时发现 fork 语义与此有出入(例如同一执行内第二次 Interrupt 直接消费到值),以实际行为为准调整,并在报告中记录偏差。

`task.go` import 增加 `errors`、`lgtypes "github.com/projanvil/langchain-golang/langgraph/types"`。

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/agent/ -run TestForegroundSubagent -v && go test ./internal/agent/ -count=1`
Expected: 三个新测试 PASS;全包绿

- [ ] **Step 5: Commit**

```bash
git add internal/agent/task.go internal/agent/taskrelay_test.go
git commit -m "feat(agent): relay foreground subagent permission pauses to the parent graph"
```

---

### Task 5: 跨进程恢复(durable 档)

**Files:**
- Test: `internal/agent/taskrelay_test.go`(追加)
- Modify: 按测试暴露的问题决定(预期零改动;若子 agent graph 拓扑重建不一致导致 resume 失败,修 newSubagent)

**Interfaces:**
- Consumes: Task 4 的接力;sqlite checkpoint saver(`github.com/projanvil/langchain-golang/langgraph/checkpoint/sqlite`,用法参考 internal/agent 或 internal/runstate 里既有 sqlite saver 装配,grep `sqlite.New`)。

- [ ] **Step 1: 写失败测试**

```go
// TestSubagentRelayCrossProcess: sqlite saver 落 t.TempDir();父 agent A1 跑到
// SubagentInterrupt 暂停;新建父 agent A2(同一 saver、同配置,模拟新进程),
// 用同一 parent thread ResumeStream/Resume "y" → 子 agent 从暂停点续跑完成,
// 断言最终报告与 Write 副作用各发生一次。
```

关键点:恢复方是**父 graph 的 Resume**(主 thread),不是直接动子 thread;子的 pausing checkpoint 由共享 sqlite saver 持久化,A2 的 toolsNode 重执行时 `subagentStarted` 命中。parent thread 需要固定——用 `a.WithThreadID` 等价物或直接 `Stream(ctx, history, "parent-thread-1")` 指定。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./internal/agent/ -run TestSubagentRelayCrossProcess -v`
Expected: FAIL 或验证通过——若直接 PASS,说明链路已通,跳过 Step 3 直接 Commit(在报告中注明)

- [ ] **Step 3: 修复暴露的问题**

最可能的问题:`newSubagent` 重建的 graph 与暂停时拓扑不一致(saver 注入后 checkpoint namespace 变化),或 `a.saver` 在 MemorySaver 档下 GetState 语义差异。按报错修,原则:不动 Task 1-4 的契约。

- [ ] **Step 4: 跑测试确认通过 + 回归**

Run: `go test ./internal/agent/ -count=1`
Expected: 全绿

- [ ] **Step 5: Commit**

```bash
git add -A internal/agent/
git commit -m "test(agent): cross-process subagent resume via shared sqlite saver"
```

---

### Task 6: 三条审批路径呈现(CLI / server / SDK)

**Files:**
- Modify: `cmd/woca/main.go` `interruptHandler`(:681-727)
- Modify: `internal/server/run.go` `handleInterrupt`(:190-201)
- Modify: `sdk/interrupt.go`(Interrupt 新 kind)
- Test: `cmd/woca/main_test.go`(或既有 interrupt 测试文件,grep 确认)、`internal/server/serve_test.go` 风格的新 case、`sdk/options_test.go`

**Interfaces:**
- Consumes: `agent.SubagentInterrupt`(Task 1)。
- Produces:
  - CLI: `case agent.SubagentInterrupt` — 打印 `\nSubagent '<v.Subagent>' requests permission: <v.Inner.ToolName> — <v.Inner.Reason>`,交互读 y/N,非交互 deny(与 PermissionRequest 分支同构)
  - Server: `case agent.SubagentInterrupt: return r.park("permission", map 载荷)`,载荷 = 内层 PermissionRequest 字段 + `subagent: v.Subagent`(前端零改动)
  - SDK: `Interrupt.Kind == "subagent_permission"`,新增字段 `Subagent string` + 复用 `Permission *PermissionInterrupt` 载内层;默认 handler 自动 deny

- [ ] **Step 1: 写失败测试**

- CLI:现有 interruptHandler 测试模式(grep `interruptHandler` cmd/woca 下的测试)加一条:非交互(stdin 非 TTY 场景)传入 SubagentInterrupt 得 "n";交互路径给 stdin 喂 "y\n" 得 "y"。
- Server:`internal/server/serve_test.go:209-216` 的 table 加一行 `{agent.SubagentInterrupt{Subagent: "r", Inner: agent.PermissionRequest{ToolName: "Write"}}, "permission", ...}`——先读该测试理解 answer JSON 形状再加。
- SDK:`sdk/options_test.go` 加 `TestInterruptFromSubagentPermission`:调 `interruptFrom(agent.SubagentInterrupt{...})`,断言 Kind=="subagent_permission"、Subagent 名、Permission.ToolName;并断言默认 handler 对它返回 "n"。

- [ ] **Step 2: 跑测试确认失败**

Run: `go test ./cmd/woca/ -run Interrupt -v; go test ./internal/server/ -run Interrupt -v; go test ./sdk/ -run SubagentPermission -v`
Expected: FAIL(未知类型分支报错 / 未定义字段)

- [ ] **Step 3: 实现**

- `cmd/woca/main.go` interruptHandler 的 switch 加:

```go
		case agent.SubagentInterrupt:
			fmt.Fprintf(stderr, "\nSubagent %q requests permission: %s — %s\n", v.Subagent, v.Inner.ToolName, v.Inner.Reason)
			if !interactive {
				fmt.Fprintln(stderr, "  (non-interactive: denied; use --dangerously-skip-permissions or --accept-edits to allow)")
				return "n", nil
			}
			fmt.Fprint(stderr, "  Allow? [y/N] ")
			return readYesNo(stdin)
```

- `internal/server/run.go` handleInterrupt 加:

```go
	case agent.SubagentInterrupt:
		return r.park("permission", map[string]any{
			"subagent":     v.Subagent,
			"tool_name":    v.Inner.ToolName,
			"tool_args":    v.Inner.ToolArgs,
			"reason":       v.Inner.Reason,
			"is_read_only": v.Inner.IsReadOnly,
		})
```

(先读 PermissionRequest 分支的 park 载荷形状,对齐字段名——若它是直接 `r.park("permission", v)` 传 struct,则 SubagentInterrupt 也可直接传 struct,json tag 已备。)

- `sdk/interrupt.go`:`Interrupt` 加 `Subagent string` 字段;`interruptFrom` 加 case:

```go
	case agent.SubagentInterrupt:
		return Interrupt{Kind: "subagent_permission", Subagent: v.Subagent, Permission: &PermissionInterrupt{
			ToolName: v.Inner.ToolName, ToolArgs: v.Inner.ToolArgs, Reason: v.Inner.Reason, IsReadOnly: v.Inner.IsReadOnly,
		}}
```

默认 handler 的 switch(Options.interruptHandler 的无 OnInterrupt 分支)加 `case agent.SubagentInterrupt: return "n", nil`。

- [ ] **Step 4: 跑测试确认通过**

Run: `go test ./cmd/woca/ ./internal/server/ ./sdk/ -count=1`
Expected: 全绿

- [ ] **Step 5: Commit**

```bash
git add cmd/woca/ internal/server/ sdk/
git commit -m "feat: surface subagent permission interrupts on CLI, server, and SDK paths"
```

---

### Task 7: 后台 Task 回归锁与全量收尾

**Files:**
- Test: `internal/agent/bgtask_test.go`(追加回归用例)

- [ ] **Step 1: 写回归测试**

```go
// TestBackgroundSubagentInterruptStaysError: 后台 Task(run_in_background)的子 agent
// 触发权限中断时,行为不变:task 以 Error: 前缀的报告完成(TaskCompleted),
// 主 agent 不 pause。驱动方式参考 bgtask_test.go 既有用例。
```

- [ ] **Step 2: 跑测试确认通过(现状即正确,这是锁定)**

Run: `go test ./internal/agent/ -run TestBackgroundSubagentInterruptStaysError -v`
Expected: PASS(若 FAIL 说明接力改动意外影响了后台路径,回 Task 4 修)

- [ ] **Step 3: 全量回归**

Run: `go build ./... && go vet ./... && go test ./... `(后台跑,10 分钟超时)
Expected: EXIT 0

- [ ] **Step 4: SDK TS/Python 与 lint**

Run: `cd sdk/typescript && npm test; cd ../python && python3 -m pytest tests/ -q; golangci-lint run ./internal/agent/... ./sdk/... ./cmd/...`
Expected: 全绿(本次未动 TS/Python 行为,防回归)

- [ ] **Step 5: Commit**

```bash
git add internal/agent/
git commit -m "test(agent): lock background subagent interrupt behavior (stays an error)"
```

---

## Self-Review 记录

- **Spec coverage**:信封(Task 1)✓、确定性 thread + 共享 saver(Task 2)✓、执行期中断捕获与备忘(spec 的"工具调用重执行"风险项,Task 3)✓、接力核心(Task 4)✓、跨进程(Task 5)✓、三路径呈现(Task 6)✓、后台回归锁(Task 7)✓。spec 的 pendingSubagents map 被更薄的"共享 saver + GetState 重入检测"替代——这是 spec 的有意精炼(MemorySaver 也是 saver,新编译的 graph 可 resume 同一 thread),在 Task 2 注释中声明。
- **Placeholder scan**:Task 3 Step 1 的测试是骨架 + 确切参考点(pausingTool 用工具收到的 ctx 调 lggraph.Interrupt),属故意留形——完整端到端 harness 在 Task 4 才齐备;Task 5 Step 3 是条件分支(测试先 PASS 则跳过),不是占位。
- **Type consistency**:`SubagentInterrupt` 字段(Task 1)= Task 4 构造 = Task 6 三处消费 ✓;`deriveSubagentThread`/`subagentStarted`/`withToolCallID` 签名 Task 2 定义、Task 4 消费 ✓;`lgtypes.GraphInterrupt` 的 `.Interrupt.Value` 与 runner.go:145 的既有用法一致 ✓。
- **已知风险记录**:Task 4 末尾"同执行内第二次 Interrupt"的 fork 语义以实现时实测为准(已在步骤内注明)。

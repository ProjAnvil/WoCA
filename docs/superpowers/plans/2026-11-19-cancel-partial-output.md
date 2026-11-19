# 取消轮部分输出持久化 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 2026-11-19 选型 B 翻案 spec(v2):执行中 run 取消时持久化部分转写(partial 文本+已提交消息),对每个 dangling tool_call 合成终态 tool result(abort 原因+verify 指引),取消时 emit 合成 tool_result 事件,live/replay/reload 三面闭环,消除半轮工具卡 spinner。

**Architecture:** Run 增三段积累器(committed/provisional/deltaBuf,render 写、cancel 读)+startedAt;cancel 分支在 finish 前 emit 合成 EventToolResult;completeRun 的 runCompletion 增 partial,派生公式=base+human?+partial+synth+notice;web cancel() 成功回调内本地翻开卡。goal 双通道靠 provisional-批次取代规则去重(C1)。

**Tech Stack:** Go(internal/server)、Vue3+TS(web 两处小改)、vitest、Playwright(dedicated+fakemodel 工具 lane)。

**Spec:** docs/superpowers/specs/2026-11-19-cancel-partial-output-design.md(v2 审计修订版——spec 是权威,本计划从其论证)

## Global Constraints

- 注释/代码/commit message 全英文;合成 tool result 内容为固定英文串(逐字=spec D2)。
- 不新增 machinetag 前缀;四种 synthContent 不以任何 Prefixes 开头(断言钉)。
- queued-cancel 公式不动(partial=nil);`ranClean`/`maybeWake`/finalizer 顺序不动;retain runner 级不改。
- web 改动后 `npm run build` 再 `go build ./...`;每 task 目标测试+全量门禁。

## Review Focus

1. **goal 双通道重复(C1)**——partial 内每条 cycle 消息恰一次;mid-cycle cancel provisional 单份。Task 2 Step 1 的双通道用例是本计划最高风险钉。
2. **Human-后置不变量**——dangling AI 恒 partial 末条(steers/relevant-memory/goal-cycle 都在批次后);派生顺序测试钉。
3. **合成事件序**——tool_result 先于 done、seq 单调;command run 取消零合成事件。
4. **本地翻卡时机**——随 cancelRun 成功回调,失败不翻(stream.ts onDone 陈旧注释同步改)。
5. **下一轮 replay 合法**——e2e 显式断言请求体含合成 result、无 dangling tool_use。

---

### Task 1: 积累器 + startedAt + 合成函数(纯函数面)

**Files:**
- Modify: `internal/server/run.go`(Run 增 committed/provisional/deltaBuf/startedAt;render 写入:批→committed+清 provisional+清 deltaBuf;GoalDelta→provisional;string/GoalFinalReply→deltaBuf;cancel 路径读)
- Create: `internal/server/toolabort.go`(synthContent 四因由+elapsed 一位小数;dangling 收集;partial 组装 committed+provisional+flush)
- Test: `internal/server/toolabort_test.go`

**Interfaces:**
- Produces: `func synthToolAbortContent(reason string, elapsed time.Duration) string`(四串逐字);`func danglingToolCalls(partial []messages.Message) []messages.ToolCall`;`func synthesizeAborted(partial []messages.Message, reason string, elapsed time.Duration) []messages.Message`(Tool 消息,按 call 序)。
- Consumes: 既有 `StopReason*` 常量(stopmark.go)。

- [ ] **Step 1:** 失败测试:四 reason 文案逐字+elapsed 格式;dangling 判定(有结果不合成/多 call 全合成/跨 committed+provisional);synth 不撞 machinetag.Prefixes。
- [ ] **Step 2:** 实现;`go test ./internal/server/ -run ToolAbort` 绿。
- [ ] **Step 3:** Commit `feat(server): synthesize terminal results for aborted tool calls`。

### Task 2: render 积累 + goal 双通道去重

**Files:**
- Modify: `internal/server/run.go`(render/appendEvent 面接积累器;startWith 记 startedAt)
- Test: `internal/server/run_partial_test.go`

- [ ] **Step 1:** 失败测试:①批次消息→committed,同轮 delta 先流后批→deltaBuf 清;②**C1 钉**:goal fixture(newGoalTestServer)一 Read 调用,GoalDelta+外层批次双发→partial 每消息恰一次(SSE 计数对照 tool_call:2 vs partial:1);③mid-cycle cancel(批次未到)→provisional 单份;④command run 取消→partial 空。
- [ ] **Step 2:** 实现积累器写入(provisional 仅 GoalDelta 路径);测试绿+`go test ./internal/server/` 全量。
- [ ] **Step 3:** Commit `feat(server): accumulate the partial transcript in Run`。

### Task 3: cancel 面贯通——emit 合成事件 + completeRun partial 派生

**Files:**
- Modify: `internal/server/run.go`(startWith cancel 分支:emit 合成 EventToolResult→finish(Done))
- Modify: `internal/server/runcompletion.go`(runCompletion 增 partial;cancel 且 partial!=nil→新派生公式)
- Modify: `internal/server/stopmark.go`(derivedCancelledHistory 增 partial/synth 参,保持 Clone;queued 调用点 partial=nil)
- Modify: `internal/server/runs.go`、`goalapi.go`(onDone 闭包带 partial)
- Test: 扩展 `internal/server/stopmark_test.go`、新增链路测试

- [ ] **Step 1:** 失败测试:①emit 序(tool_result 先于 done,seq 单调,事件环含合成事件);②派生公式=base+human?+partial+synth+notice、Human-后置不变量用例(steers 场景);③queued-cancel 不变(partial=nil);④完整链路:startWith 真取消→save→load 回读四段齐全;⑤goal 取消(human=nil)同构。
- [ ] **Step 2:** 实现;`go test ./internal/server/ ./internal/agent/ ./internal/tui/` 全绿(既有 stopnotice/truncate 系列不回归)。
- [ ] **Step 3:** Commit `feat(server): persist partial output of cancelled runs`。

### Task 4: web 两处——cancel() 本地翻卡 + 陈旧注释

**Files:**
- Modify: `web/src/stores/sessions.ts`(cancel() 成功回调对开卡翻终态「已中止/Aborted」;失败不翻)
- Modify: `web/src/stores/sessions/stream.ts`(292-297 陈旧注释改写)
- Modify: `web/src/i18n/locales/{zh-CN,en}/chat.ts`(翻卡 copy 键)
- Test: `web/src/stores/sessions.test.ts`(扩展)

- [ ] **Step 1:** 失败测试:cancel 后开卡(result===undefined)翻为终态;cancelRun 失败不翻。
- [ ] **Step 2:** 实现;i18n 双语同步;vitest+vue-tsc+build 绿。
- [ ] **Step 3:** Commit `feat(web): flip open tool cards on cancel`。

### Task 5: e2e——工具 lane + 四断言

**Files:**
- Modify: `e2e/tests/personal/stopnotice.spec.ts`(fakemodel 加工具 lane:一轮真 tool_call(Bash sleep)+续答;记录请求体)

- [ ] **Step 1:** 工具 lane:流中(tool 执行中)停止→①cancelling tab 部分文本+开卡「已中止」;②reload 后 partial 气泡+工具卡合成文案+notice 卡一 run 一卡;③GET /api/sessions 转写含合成 Tool 消息;④再发一轮,断言 fakemodel 收到的请求 messages 含 synthContent 子串且无 dangling tool_use。
- [ ] **Step 2:** 连续 3 次绿;personal 全套不回归;Commit `test(e2e): partial output survives cancel`。

### Task 6: 全量门禁收口

- [ ] **Step 1:** `make check`、web vitest 全量、vue-tsc、npm run build+go build、`go test ./...` 全绿。
- [ ] **Step 2:** spec 执行记录回写(审计裁定与 OQ 已在 spec §5)。

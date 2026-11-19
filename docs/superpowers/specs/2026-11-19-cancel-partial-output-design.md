# 取消轮部分输出持久化(选型 B 翻案)— Design Spec

日期:2026-11-19 · 状态:audited(审计发现已内嵌并修订,见「裁定与审计记录」;v2 Ready for planning) · 基线:main @ 9e39c7e9
来源:F1 设计时的选型 B 被 spinner 顾虑否决;2026-11-18 对标调研显示该顾虑已被业界在数据层消解(证据:woca-internal-docs/harness-cancel-semantics-research-pm.md §Q1/Pattern A/Implications 1)。用户令翻案出 spec。

## 0. 总览与非目标

**做什么**:用户停止一个执行中的 run 时,持久化该轮的部分转写——已流出的部分助手文本、已提交的完整消息(含 tool_call)——并为每个无结果的 tool_call 合成终态 tool result(内容标注 abort 原因与「outcome unknown, verify before retrying」指引)。消除两处 spinner:reload 后的半轮工具卡、live 视角(cancelling tab 本地闭环,他 tab/replay 由合成 SSE 事件闭环)。

**业界对标**(research-pm.md,行号为该文档内引用):
- Codex:`"aborted by user after {secs}s"`、`"aborted before dispatch"`(codex-rs/core/src/tools/parallel.rs:515-529)。
- Claude Code:`[Tool call interrupted: …outcome is unknown…]` 合成 error tool_result。
- dsh:`TOOL_OUTCOME_UNKNOWN`/`TOOL_NOT_STARTED` + retry 指引;OpenCode:`{type:"aborted"}`。
- 主流四家(Claude Code/Codex/OpenCode/dsh)无一家留 dangling tool_use;spinner 问题在数据层解决,而非丢内容。

**非目标**:
- 排队中取消(queued-cancel)的部分持久化——该 run 从未执行,无部分态,维持现公式(prompt+notice),finalizer 队列行为不变。
- dispatch 模式(EE worker)运行的取消——F1 既定裁定:该路径用户不可取消,出范围。
- never-started tool call 的丢弃(OpenCode/dsh 细化)——woca 的 tool_call 消息只在模型节点 superstep 提交后才可见,可见即已派发;不存在可区分的 never-started 类,不引入。
- reasoning 部分文本持久化——干净轮也不持久 reasoning(转写无 reasoning 消息),保持 parity。
- `<run-stopped>` 标签语义、位置(reason 封闭集)——不动。
- truncate drop_last_turn 锚点行为——现状锚在最后一个 Human(即 stop notice),只掉标签、保留 prompt;部分内容随该锚保留,与今天行为一致,不另行扩展(如需回滚整轮用 keep=n)。
- goal run 的 cycle 级细化——goal 部分态经同一积累器自然覆盖,不做额外 cycle 语义。

## 1. 现状核证(file:line @ 基线)

- 取消路径:`internal/server/run.go:168-178` startWith——runFn 返回 `(nil, err)` 且 `r.ctx.Err() != nil` 时 `finish(Done,{cancelled,reason})` 后 `onDone(nil)`。runner 侧 `internal/runner/runner.go:153` cancel 落 superstep 边界返回 `(nil, streamErr)`,部分态不上抛。
- 持久化:`internal/server/runcompletion.go:59-88` completeRun——`final != nil` 才 save;cancel 分支用 `derivedCancelledHistory`(`internal/server/stopmark.go:57-63`)= `clone(base) + human + stopNotice`,部分内容全部丢弃。
- 消息面已有积累点:`internal/server/run.go:198-260` render——StreamUpdates 批次与 GoalDelta 消息过 `renderMessage`(string/GoalFinalReply 增量直发 EventDelta 不过);**转写消息与 delta 今天只转发 SSE,不落 Run 状态**。
- 事件回放:`internal/server/run.go:868-885` appendEvent——事件环 `r.history` + ledger mirror;reattach 重放同一事件流。合成事件走 `emit` 即三路(live/replay/mirror)一次到位。
- run 起始时间:Run 结构无 startedAt(需新增以支持 after Xs 文案)。
- web cancelling tab:`web/src/stores/sessions.ts:676-714` cancel() 先 `abortRun` 再本地 `pushStoppedNotice`(设计注释见 stream.ts onDone:「the cancelling tab aborts its stream before this event」);开着的工具卡无终态→spinner。
- web reload 面:`web/src/lib/transcript.ts` NOTICE_TAGS 前缀→notice 卡;Tool 消息带 result 渲染为终态卡——**合成 result 持久化后 reload 天然闭环,web 零改动**。
- 消费排除缝:`internal/agent/retain.go:222-236` runQuestion 向后扫非 notice 的 Human;`internal/tui/tui.go:1329-1352` historyForRun 末位 Human 是 stop notice 时折叠前一个真 Human;`internal/machinetag/machinetag.go:42-53` Prefixes 五前缀表 + `ci/check-machinetag-contract.sh` 与 web `MACHINE_TAG_PREFIXES` 对拍。

## 2. 设计

### D1 Run 部分转写积累器

Run 增加积累器(render 写、cancel 路径读,mu 保护;仅 `run.start`/`run.startGoal` 路径——command runs 无 render 面自然为空积累器,D3/D4 对其 no-op;`/btw` runs 积累照常但本就零持久化,live-only 合成事件是既有行为面的自然延伸):

- `committed []messages.Message`:StreamUpdates 批次消息(renderMessage 面)。注意 string/GoalFinalReply 增量不过 renderMessage(run.go:199-202 直发 EventDelta)。
- `provisional []messages.Message`:**goal 专用**——GoalDelta 的 renderMessage 消息。goal 图每条 cycle 消息会双通道到达:GoalDelta custom 块(goalgraph.go:529-538→run.go:219-222)与外层图 execute 节点的 updates 批次(goalgraph.go:567-570)各一次(审计 C1,fixture 实证 tool_call/tool_result 各 ×2)。**批次到达时丢弃 provisional**(批次是该节点消息的权威拷贝,含 `<goal-cycle>` Human 与 cycle AI 文本;provisional 只在 cycle 未提交的窗口里有值)——与 deltaBuf 同一条取代规则。
- `deltaBuf string`:StreamCustom string/GoalFinalReply 的助手增量累积;批次带来 AI 消息时清空(fork 的 updates 块恒在节点事件之后成束到达,stream.go:160-166,一轮 delta 全部先于其批次,reset 规则健全;buffered model-hooks 模式 emit(full.Content) 亦先于批次,agent.go:1653-1656)。**已知留白(接受并记录)**:transient-error/fallback 重启时被弃尝试的 delta 与 `"\n[reason]\n"` 标记已流出(agent.go:1613-1622),服务端看不见重启——cancel 落在尝试 2 会持久化尝试 1 文本+标记。这与用户 live 看到的流一致(他们看见了那段文本),诚实保留。
- reasoning 增量不入积累器;批次 AI 消息自带 reasoning content_blocks 随消息保留(干净轮转写同构——干净轮的 AI 消息携带 reasoning blocks,transcript.ts:135-141;仅 flush 出来的在途消息丢其在途 reasoning)。
- cancel 时合成 partial = committed + provisional + [deltaBuf 非空则 `messages.AI(deltaBuf)`]。

### D2 合成终态 tool result

dangling 判定:partial(committed+provisional)中 AI 消息的 ToolCalls 里,凡 ToolCallID 无对应已积累 Tool 消息者,合成:

```
messages.Tool(toolCallID, synthContent(reason, elapsed))
```

内容(英文,model-visible 且工具卡直显;不以任何 machinetag 前缀开头):

- user_stop:`Tool call aborted by user after {secs}s — outcome unknown. Verify the effect before retrying.`
- parked_ttl:`Tool call aborted: approval timed out — outcome unknown. Verify the effect before retrying.`
- server_stop:`Tool call aborted by server shutdown — outcome unknown. Verify the effect before retrying.`
- lease_lost:`Tool call aborted: run lease lost to another instance — outcome unknown. Verify the effect before retrying.`

`{secs}` = run 起始(startWith 进入,提升后——排队时间天然不含)到取消的一位小数秒(Run 新增 startedAt;parked 等待计入,documented)。合成内容为纯英文正文、无 name 参数(`messages.Tool` 只有 id+content 两参,fork messages.go:85;web 工具卡名字取自 call 卡,transcript.ts:154-168)。**有意偏离既有先例**:`answerDanglingToolCalls`(goalgraph.go:255-267)与 ε 门 `crashInterruptedMarker`(epsilon.go:58-59)合成的都是 "Error:" 前缀终态;本设计用中性 abort 文案(与 Codex/Claude Code 对齐),不撞 Prefixes 断言钉住。

### D3 持久化派生公式变更

`derivedCancelledHistory` 增参 `partial []messages.Message, synth []messages.Message`:

```
derived = clone(base) + [human?] + partial + synth + [stopNotice]
```

- 顺序:partial 内消息保持流序(消息内 tool_use 与结果按 id 配对,合成 Tool 追加在 partial 之后、notice 之前,满足「结果随调用之后」的模型约束)。**支撑不变量(测试钉)**:Human 消息只会出现在已完成的 tools 批次之后(mid-turn steers=queue.Format、`<relevant-memory>` 附件、`<max-turns-reached>`、goal `<goal-cycle>` 都在批次 newMsgs 里;hooks/approval 节点不发消息)——所以 cancel 落在模型提交与 tools 提交之间时,dangling AI 恒为 partial 末条,尾部追加合成 Tool 永远合法。
- `completeRun` 的 runCompletion 增 `partial` 字段:执行中 run 的 cancel 分支填入;queued-cancel(base==nil)与 goal 排队取消保持 partial=nil(现公式不动)。goal 执行中取消:human=nil 公式照旧,partial 照填。
- save 沿用 WithoutCancel + stripSystem 姿势;`ranClean`/`maybeWake` 语义不动。

### D4 取消时的合成 SSE 事件

cancel 分支在 `finish(Done)` **之前**,对每个 dangling call `emit(EventToolResult{…synthContent…})`(payload 与真实 tool_result 同形:`toolResultPayload{ToolCallID, Content}`,run.go:246-250)。效果:live 他 tab、reattach replay(事件环)、ledger mirror 三路闭环,开卡到终态。作用域:model/goal/command/btw 全部经 startWith 的取消分支统一发(command runs 积累器恒空→无事件;btw live-only 有事件零持久化=既有行为面)。appendEvent 单临界区保证 seq 单调;late-answer 门不受影响——cancelRun 已清 pending(run.go:988-996),取消窗内的迟到 list-form answer 仍会缓冲并发 `interrupt_resolved` 先于 done(run.go:528-565),与合成 tool_result 的 event 序各自独立、无交错临界区。

### D5 web cancelling tab 本地闭环

cancelling tab abort 流在先、收不到 D4 事件——cancel() 在本地对当前会话所有开着的工具卡打终态。**时机(审计 I3)**:本地翻卡随 `api.cancelRun` 的成功回调走(sessions.ts:700-713,与 pushStoppedNotice 同一 `.then`);请求失败不翻卡(run 还活着,attached 流继续更新)。spinner 判定=卡 `result === undefined`(ChatItemView.vue:159,365)。视觉态双语「已中止/Aborted」,不写转写;reload 后以服务端合成 result 为准。

**web 既有陈旧注释须同步**:stream.ts:292-297「A cancelled run persists no assistant message server-side」在本设计落地后为假,注释与钉住该语义的测试一并更新(逻辑本身安全)。

## 3. 接缝逐条

1. **`<run-stopped>`**:不变,仍是派生历史末条 Human;部分内容与合成 result 落在它之前。
2. **retain.runQuestion**(retain.go:226-236):向后扫非 notice Human。partial **可以含 Human**(steers/`<relevant-memory>`/`<max-turns-reached>`/`<goal-cycle>` 都在批次里):前两者被 Prefixes 跳过;`<goal-cycle>`、`<max-turns-reached>` 不在 Prefixes——取消的 goal run 之后下一 run 的 runQuestion 会取到 goal-cycle prompt。**这与干净 goal run 完全同构**(其 final 同样含 `<goal-cycle>`,par­ity 成立,非缺口)。runner 级 RetainRun 仍收 final=nil/err=cancel(runner 不改),行为不变。
3. **TUI historyForRun**(tui.go:1329-1358):末位 Human=stop notice 折叠逻辑照旧。注意该函数今天仅测试引用(command_test.go),且折叠**丢弃 partial**(base=history[:i])——重试语义本就弃半轮,照实记录;下一轮模型输入含合成 result——闭环 dangling,正是设计目的。
4. **finalizer 队列**:queued-cancel 走 completeRun(partial=nil) 现公式;drain 顺序不变。
5. **machinetag 契约检查**:无新标签、Prefixes 不动;新增断言:四种 synthContent 均不以任何 Prefixes 前缀开头(防未来文案回归)。web MACHINE_TAG_PREFIXES 零改动。
6. **goal runs**:双通道消息面(C1 的 provisional 规则见 D1);GoalFinalReply 增量入 deltaBuf;goal 取消派生=base+partial+synth+notice(human=nil)。**web 渲染注意(审计 M6)**:goal partial 的 flush 文本无 `<goal-report>` 包裹,reload 渲染为 goal_note 卡而非助手气泡(transcript.ts:143-150)——e2e 断言按此分径。顺带登记(本 spec 出范围,开票跟进):web `addToolCall` 无去重、`addToolResult` 落第一张卡(items.ts:74-93)——goal runs 今天就双卡永旋,是既有 web bug 非本设计引入。
7. **wake 路径**:与主 run 同缝(startRunWith 同一 start/onDone),自然覆盖。
8. **undo/truncate**:drop_last_turn 锚 stop-notice 现状保留(§0 非目标;partial 在锚之前随历史保留,changesapi.go:209-222);门内 save 已 WithoutCancel(d45d4002)。
9. **checkpoint/ε 门**:无冲突——answer 门以 done 为界,goal resume 重建新图,无路径 resume 一个已取消 run 的 thread(仅活 run 的 ResumeStream,runner.go:182);会话标题取首个 Human(session.go:585-592),不受影响;todos sidecar 与合成「outcome unknown」语义一致。

## 4. 测试与验收

Go(server 包):
- 合成公式单测:四 reason 文案逐字、elapsed 格式(startedAt 提升后起算、parked 等待计入)、dangling 判定(有结果的不合成、多 call 全合成)、synth 不撞 Prefixes。
- 派生公式单测:partial+synth 顺序、deltaBuf flush、queued-cancel partial=nil 不变、stripSystem、**Human-后置不变量**(dangling AI 恒 partial 末条)。
- **goal 双通道单测(C1 回归钉)**:GoalDelta 消息+外层批次同发→partial 内每条 cycle 消息恰一次;mid-cycle cancel(批次未到)→provisional 保留一次。
- 取消时序单测:emit 序(tool_result 先于 done,seq 单调);command run 取消零合成事件。
- 完整链路:startWith 真取消→completeRun save→load 回读含 partial 文本+合成 result+notice。

e2e(personal,dedicated 实例 + fakemodel,扩展 stopnotice 姿势;**须加工具 lane**——fakemodel 一轮先发真 tool_call 再续答,工具在 bypassPermissions 下真执行且慢(如 Bash sleep;slow-stream 单 lane 永远开不出工具卡,tool_call 事件只在模型节点提交时发):
1. 流中停止后:cancelling tab 部分回复文本可见、开着的工具卡翻「已中止」终态(无 spinner)。
2. reload 后:部分助手气泡 + 工具卡终态(合成文案)+ stop notice 卡三者可见且去重(一 run 一卡)。goal run 分径:goal partial reload 为 goal_note 卡(transcript.ts:143-150)。
3. 服务端真相:GET /api/sessions/{id} 转写含合成 Tool 消息。
4. 下一轮模型可见:**显式断言**(fakemodel 记录请求体)下一次请求 messages 含合成 result 子串且无 dangling tool_use(审计 OQ3 裁定:显式,非回显)。

## 5. 裁定与审计记录

审计(2026-11-19,system-architect;含 fixture 实证):Needs revision→已修订。**C1 goal 双通道重复积累**→D1 增 provisional 通道与批次取代规则(goal cycle 消息 GoalDelta+外层批次各到达一次,实证 tool_call/tool_result ×2;不修则部分转写带重复 tool_use id,下一轮 replay 被供应商拒——answerDanglingToolCalls 防的正是这类)。I1 partial 可含 Human→§3.2 改 parity 论证+Human-后置不变量入测试;I2 重启污染→接受并记录(用户 live 看见过该文本);I3 本地翻卡随 cancelRun 成功回调;I4 作用域=model/goal(command 零事件/btw live-only);I5 stream.ts:292-297 陈旧注释同步。M1-M7:toolName 句删除、§1 string 不过 renderMessage 勘误、reasoning=content_blocks 论证修正、§3.9 括注改写、historyForRun 折叠弃 partial 照实、goal partial reload=goal_note 分径、行号漂移修。Nit:与 answerDanglingToolCalls/crashInterruptedMarker 的「Error:」先例有意偏离已声明;parked-TTL elapsed 含 parked 等待已记录。

OQ 裁定:OQ1 接受双源文案(英文 model-visible+双语本地翻卡,工具内容本就原样渲染);OQ2 接受(startedAt=提升后起算,排队天然不含,parked 计入);OQ3 显式断言(fakemodel 已解析请求体,stopnotice.spec.ts:90-98,确定性)。

**顺带发现(出本 spec 范围,待开票)**:web `addToolCall` 无去重+`addToolResult` 落第一张卡(items.ts:74-93)——goal runs 今天就双卡永旋(既有 web bug,C1 实证时暴露)。

## 6. 状态

v2(审计修订版)——Ready for planning。

## 7. 执行记录(2026-11-20)

六任务全部落地,commits 2594ce62(toolabort 纯函数面)→ 20b819e2(render 积累器)→ dae73b78(cancel 面贯通:emit 合成事件+completeRun partial 派生)→ b47c0232(web cancel() 本地翻卡+陈旧注释)→ 0599c97d(e2e 工具 lane)。门禁:go test ./... 绿、vitest 756/756、vue-tsc 0、make check 0、stopnotice e2e ×3 连绿。

执行裁定(偏离与发现,详见 plan 工作区 ledger):

- **mid-tool cancel 是引擎真竞态**:fork 的 stream emitter(fork graph/stream.go `emit`)在 `select { send / ctx.Done }` 里对已就绪双臂随机选择——取消后 tools 批次约五成被丢(→dangling→合成)、五成送达(→工具自身的取消输出答掉调用)。两态都合法、都闭环;Go 单测用确定性的 parked-approval 窗口钉合成支,e2e 工具 lane 按双态析取断言(合成文案或 `Exit code -1.`),下一轮无 dangling tool_use 为硬断言。
- **parkedTTL 既有测试预期合法变化**:parked ask 的 AI+toolcall 已提交,新派生公式下转写从 2 段(prompt+notice)变 4 段(+partial+synth)——测试已按新语义更新;queued-cancel/clean-run 钉不变。
- elapsed 求值点:emit 侧与持久化侧各算一次(同一 goroutine 链,µs 级间隔);e2e 断言 emit 内容与持久化逐字节一致(Go 链路测试钉)。
- 顺带修复:前场 toolabort_test.go 的 gofmt 版本漂移(PATH gofmt 1.27.4 vs golangci 内嵌 1.27.3)由 `golangci-lint fmt` 归一。
- 存量并行 flake(基线可复现,与本设计无关):chatcolumn 1440/1800 的 dedicated 实例 settings PUT 503(settings DB 初始化竞态,4 workers 负载下);hitl 并行审批 UI flake(既有票)。
- **终审修复(d96afdaa)**:park-resume 后 fork 会把已完成 sibling 的 writes 按原消息 id 重放为 updates(graph.go:1446-1450),积累器盲 append 会双计——取消后持久化重复 tool_result,严格供应商下一轮拒收(C1 同类)。修法=积累器按 graph-stamped id 幂等(committed/provisional 双通道;无 id 消息保持原 append 行为);取代规则不变。

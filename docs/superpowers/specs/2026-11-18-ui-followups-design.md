# UI 审计后续五项 — Design Spec(v2,审计修订版)

日期:2026-11-18 · 状态:audited(审计发现已内嵌,见「审计记录」节) · 基线:main @ e7c9143e
来源:2026-11-17 前端审计+修复闭环(1b91ff58/e7c9143e)的停机项,用户已拍板「全部按建议做」。证据 woca-internal-docs/ui-audit-2026-11-17{,-postfix}/。

## 0. 总览与非目标

五项:F1 停止标记持久化(后端)> F2 设置保存语义(原型先行)> F3 宽屏阅读列 > F4 双抽屉 focus trap > F5 高亮/棘轮 token 卫生。F6 图标栏原型 gate(只出原型)。

非目标:
- sub-100 z-index token 化(用户裁决不做)。
- dispatch 模式(EE worker)运行的取消持久化——该路径今天无法被用户取消(`handleCancel` 的 `lookupRun` 找不到派发 stub,runs.go:143-147、734-737),取消语义本身不存在,出范围。
- 停止轮的部分工具事件持久化(见 F1 选型 B)。
- shiki 主题重写为 CSS 变量主题(F5 只登记,见 OQ6 裁定)。
- 641-1023 任何代码实施。

## F1 停止生成标记跨重载持久化(后端)

### 现状(审计核证)

- 取消:`internal/server/run.go:152-169`——ctx 取消时 `final=nil`,`onDone(nil)`。
- 持久化门:`internal/server/runs.go:578-584` `if final != nil` 才 save(`context.WithoutCancel`,`stripSystem`)。**取消轮连用户 prompt 都不落盘,刷新即丢**。
- 排队中取消(runs.go:538-541、goalapi.go:220-224)直接 finish 后 return,**onDone 永不执行**——同型 UX 洞,纳入本项。
- wake 路径(wake.go:124→runs.go:488 startRunWith)与主 run 同缝,history 在 runs.go:572 为外层局部量,闭包可及(审计 OQ1 裁定:无需新 helper)。
- 前端一半已有:e7c9143e 的瞬时通知卡(SSE `done{cancelled}`,run.go:156 发/streaming.ts:172-174 收)。
- 通知协议:持久化通知=Human 消息+注册标签前缀;web `web/src/lib/transcript.ts:17-25` `NOTICE_TAGS` 前缀匹配→kind=notice;标签带属性时前缀匹配仍成立(`'<scheduled-task'` 无闭括号先例)。
- 双写竞争不存在(审计核证):admission release 在 onDone(save 之后)才放行下一 run(runs.go:585-589、admission.go:103-119)。

### 设计

取消时持久化派生历史,两条公式(I1 修正:goal 路径无本轮 human,goalapi.go:227 load 后直接 startGoal):

- 主 run(runs.go:578)与排队中取消(runs.go:538):`final = append(slices.Clone(history), human, messages.Human(stopTag))`。Clone 必须保留——session.go:158-160 load 返回内存共享切片,直接 append 会污染底层(M4)。
- goal run(goalapi.go:231):`final = append(slices.Clone(history), messages.Human(stopTag))`。

要点:

1. 标签载荷:`<run-stopped run="<runID>" reason="user_stop|parked_ttl"/>`(M3:parkedRunTTL 超时 run.go:387-390 走同一取消缝,reason 区分,前端可区分文案)。
2. 派生 save 沿用既有姿势:`context.WithoutCancel` + `stripSystem`(M1,统一防御)。`ranClean` 语义不动,`maybeWake` 照旧不唤醒。
3. **模型可见性裁定(I2)**:标签作为 model-visible 的 harness 通知保留在历史里(与 `<scheduled-task>` 同类),语义无害且是事实上下文(「上一轮被停止了」)。但两个消费方必须把机器标签排除:
   - `internal/agent/retain.go:223-238` `runQuestion` 跳过表加 `'<run-stopped'`(否则 goal 取消轮的记忆 episode 问题会变成机器标签);
   - `internal/tui/tui.go:1328-1337` `historyForRun` 的末位 Human 折叠:末位为 `<run-stopped` 标签时不折叠为 prompt(跳过该标签再取末位 Human,取不到则无 prompt 展开)。
4. **渲染缝裁定(I4)**:ChatItemView 的 notice 分支特判 `<run-stopped` 前缀→渲染 `t('chat.generationStopped')`(e7c9143e 已有键,zh-CN/chat.ts:93)+ reason=parked_ttl 时换 `chat.runTimedOut`(新键,zh「等待审批超时已停止」/en)。其余 notice 维持 raw 内容渲染(既有约定)。transcript.ts 的 NOTICE_TAGS 加 `'<run-stopped'`。
5. TUI 显示面安全(审计 OQ2 核证):/resume 只装载 history 不重渲(tui.go:1291-1298 清屏增量),持久标签不会在 TUI 冒泡。
6. /btw(runs.go:343-346)维持不持久化。

### 方案选型

A(采纳)派生历史+标签,零新状态。B(否决,记后续)agent 取消时返回部分 final:侵入 executor 且半轮工具事件无配对结果的渲染语义(前端 tool_call 无 result=运行中 spinner),需配套定义,单独 spec。

### 测试

Go:取消后 messages 末尾= human prompt + `<run-stopped` 标签;排队中取消同样;goal 取消无重复 human;clean 完成无标签;retain runQuestion 跳过标签;historyForRun 不把标签折为 prompt。Web(vitest):isNoticeContent 新前缀;ChatItemView notice 特判渲染 i18n 键与 reason 分支;刷新后无双卡。e2e(personal):流式停止→reload→notice 卡在;复用 e2e/regression fakemodel。

## F2 设置面板「保存」语义重构(原型先行)

### 现状(审计核证,OQ3/I6 修正)

唯一保存按钮(SettingsPanel.vue:317-329)→ `serverSettings.saveServer()`(serverSettings.ts:201-255)发**单个 `PUT /api/settings` 全量 patch**,仅按可用性门控增删键(msgPlaneAvailable/langfuseConsoleManaged,serverSettings.ts:230-243);**不存在分区端点**——websearch/claudeenv/plugins 有各自 PUT(WebSearchSection.vue:5,119;sections.ts:54,138),provider/storage/memory/msgplane/langfuse 全走同一 patch。服务端对缺省键视作不变(密钥注释 serverSettings.ts:244-246 证实此语义)→ **同端点键子集 PUT 可行,但区→键映射表今天不存在,需新建**。

### 设计(两案原型给用户选,拍板后实施)

原型 A(分区保存,推荐)/ 原型 B(全局保存+分区 dirty)布局图见 plan 附件;共用实施要点:

1. 新建**区→键映射表**(serverSettings.ts):appearance/preferences 即时区(无保存件,区头角标「即时生效 / Applied instantly」);provider/storage/memory/msgplane/langfuse 各列键清单,保存=同端点 `PUT` 仅本区键子集。
2. **dirty 快照放 serverSettings 仓**(OQ3 裁定):loadServer 时按区分组快照,表单 ref 全在该仓(serverSettings.ts:120-158);组件只读 dirty。
3. 「密钥加密存储」提示移入存储/模型区;全局底栏删除(A)或仅 dirty 可点(B)。
4. EE 键可用性门控纪律不变(bridge-phase1 的版本域键规则)。

### 测试

vitest:区→键映射完整性(对照四张显式清单);子集 PUT 只含本区键;dirty 门;偏好区无保存件;sections 序列测试不回归。

## F3 宽屏阅读列(≥1440)

### 现状(审计核证,I7 修正)

`--chat-col` 三档 860/960/1100(tokens.css:268-279,≥1440/≥1800 媒体抬档),**纯媒体查询驱动,无用户选择机制**(v1 的「用户已选」措辞删除)。九个消费点(OQ4 全量枚举,防回归清单):`.message`(chat.css:297)、`.stream-card`(panels.css:529)、`.msg-skeleton-wrap`(base.css:83)、`.stream-liveness`(overlays.css:298,chat-messages 之外)、`.queue-badge`(chat.css:246,渲染于 Composer)、`.chat-input-inner`(composer.css:15)、`.input-toolbar`(composer.css:51)、`.attach-tray`(composer.css:420)、`.ac-popup`(composer.css:496)。goal 报告/回放条不消费 --chat-col(GoalPanel 无引用,该担忧撤销)。

### 设计(I3 裁定:改档而非叠加 min() 钳制,消除断崖与覆盖面问题)

v1 的 `min(var(--chat-col), 760px)` 叠加方案有两个洞:1439→1440 时 860→760 的收窄断崖;以及 chat-messages 与 liveness/composer 是兄弟节点,只钳 transcript 不传导,九个消费点要逐个覆盖(I7)。改为**直接改档**:

```
≥1440: --chat-col: 960px → 760px
≥1800: --chat-col: 1100px → 760px(封顶)
基础 860 与 <1440 区间不动
```

单一变量机制,九个消费点零改动自动一致对齐;860→760 的跨 1440 变化是本设计的**刻意语义**(「宽屏=窄阅读列」,与 2026-11-17 审计 P1-1 的行长批评一致),写进 tokens.css 注释。表格/代码块横向滚动行为不变。

### 测试

e2e(personal):1440/1800 视口断言 `.message` 与 `.chat-input-inner` 计算宽 ≤760 且相等(三缘对齐回归锚);1024 视口断言 860 不变;375 不受影响。

## F4 双抽屉 focus trap(复用既有 composable)

### 现状(C1 修正:大半已存在)

`web/src/composables/useFocusTrap.ts`(101 行)已存在且已接入 HitlStack.vue:44、WModal.vue:19、WCommandPalette.vue:16、GrantsPanel.vue:30(WConfirmDialog 经 WModal 间接有);含 Esc、Shift+Tab 环绕、焦点归还、body 滚动锁引用计数、`trapStack` 分层(only-topmost acts)。GrantsPanel 嵌套已由 trapStack 正确解决(OQ5 闭环,**无需二级 trap**,补一条嵌套走查测试即可)。**useFocusTrap 无单测**(M5)。

剩余缺口:左抽屉(≤640,Sidebar.vue,只挂了 Esc 监听)与右工作区抽屉(≤1100 overlay,panels.css `.right-sidebar.open`)未接 trap;右抽屉连 Esc 关闭都没有(workspace.ts:77-98 仅开合)。

### 设计

1. 左抽屉、右工作区抽屉接 `useFocusTrap(container, openRef)`;右抽屉补 Esc。
2. 新增 `useFocusTrap.test.ts`:Tab 环绕/Shift+Tab 逆向/关闭还焦/active=false 零监听/trapStack 顶层优先(含嵌套走查)。
3. WConfirmDialog/设置 overlay 维持现状(非模态场景少,记后续)。

## F5 高亮色登记与 tokens 棘轮(轻量)

### 现状(C2 修正)

- `--link` 已是 token 且 markdown 已引用(tokens.css:35,196,312;chat.css:395-396)——v1 该项已完成,删除。
- `check-tokens.mjs` 扫描面=src 全部 .vue/.css(ROOT=src,第 8 行起),styles/ 一直在扫;豁免是 **ALLOWED 棘轮**(chat.css:5、panels.css:1、misc.css:30、ConnectView.vue:9、LoginGate.vue:1,共 46 行存量)。
- shiki:每 token 内联样式携带 `--shiki-light/--shiki-dark` 自定义属性(highlight.ts:72-75,双主题 codeToHtml),暗色靠 panels.css:413-414 变量覆盖;token 化只能自写主题对象把 color 写成 `var(--x)` 字符串=重写两套 github 主题(OQ6 裁定:**不做**)。

### 设计

1. shiki 主色**登记制**:tokens.css 增注释性条目 `--code-accent-{keyword,string,comment,number}` 等,取值=当前 github-light/dark 主题中该 token 类的实际色,旁注来源与「视觉冻结」声明。**取舍标准(M6)**:按 Go/TypeScript 代码里出现频次前五的 token 类(keyword/string/comment/function/number)登记,其余不登记。
2. ALLOWED 棘轮**冻结**:46 行存量不清理(同 z-index 裁决:无收益 churn 不做),脚本加「不新增条目」断言(清单长度对拍),新违规零容忍。
3. 例外规则注释块:mono 主题下允许的非灰阶仅 danger 族(既有)、--link(既有)、code 高亮(来源 github 主题,冻结);新增饱和色须过 token 评审。

## F6 平板图标栏原型(gate,不实施)

产出三视图静态原型(56px 图标栏态/点开 overlay 态/现状 280px 态),HTML+tokens 直出,落 woca-internal-docs,用户拍板是否立项。零产品代码。

## 审计记录(v2 修订摘要)

审计裁定 NEEDS-REVISION(code-reviewer subagent,2026-11-18),发现内嵌处理:
- C1(F4 前提错)→ F4 重写为复用 useFocusTrap+两抽屉接入+补单测。
- C2(F5 两处失实)→ F5 重写(--link 删除/棘轮冻结/shiki 登记制+取舍标准)。
- I1 goal 无 human 公式独立;I2 模型可见性裁定+runQuestion/historyForRun 排除;I4 渲染缝=ChatItemView 特判;I5 排队取消纳入;I6/F2 saveServer 表述更正+映射表新建+dirty 入仓;I3/F3 改档方案替代 min() 钳制;I7 九消费点经改档自动覆盖并列表防回归。
- OQ1 dispatch 出范围;OQ3/OQ4/OQ5/OQ6 答案分别写入 F2/F3/F4/F5。
- M1 stripSystem 沿用;M2 行号勘误(runs.go:572、cron.go:234);M3 reason 属性;M4 Clone 必要性注明;M5 补 composable 单测;M6 shiki 取舍标准。

# UI 审计后续五项 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 2026-11-18 spec 的 F1-F5(停止标记持久化/设置保存语义/宽屏阅读列/双抽屉 focus trap/token 卫生)+ F6 原型 gate。

**Architecture:** F1 走后端取消缝派生历史持久化(run.go/runs.go/goalapi.go)+ 前端既有 notice 协议;F2 在 serverSettings 仓建区→键映射与 dirty 快照,同端点键子集 PUT;F3 改 `--chat-col` 媒体档;F4 复用既有 `useFocusTrap.ts` 接两抽屉;F5 tokens 登记制+棘轮冻结。F6 纯原型交付。

**Tech Stack:** Go(server 转写持久化/retain/TUI history)、Vue3+TS+Pinia(web)、vitest、Playwright e2e(regression fakemodel 基建)。

**Spec:** docs/superpowers/specs/2026-11-18-ui-followups-design.md(v2 审计修订版——spec 是权威,本计划从其论证)

## Global Constraints

- 注释/代码/commit message 全英文;zh-CN 语言包值除外;i18n 新键 zh-CN 与 en 必须同步。
- 不新增颜色;所有样式值引既有 token(shiki 登记条目除外,值为现主题色冻结)。
- 交互目标 ≥24×24;破坏性操作有 aria-label。
- 每 task 结束跑该 task 的目标测试;web 改动后跑 `cd web && npx vue-tsc --noEmit -p tsconfig.json`;Go 改动跑 `go build ./...`。提交信息 conventional 风格英文。
- e2e 回归步骤须带 workspace(见 e2e/regression 既有约定);fakemodel SSE 必须发 `data: [DONE]` 且非流式调回 JSON。

## Review Focus

1. **取消轮的下一 run 把 `<run-stopped` 标签当 prompt 展开**——TUI `historyForRun` 折叠与 wake 路径;测试:Task 1 Step 5 的 historyForRun 用例。
2. **goal 取消持久化重复 human**(goal 路径无本轮输入)——Task 1 Step 4 断言 messages 中 human 恰一条。
3. **子集 PUT 丢可用性门控键**(msgPlaneAvailable=false 时误带/漏带 msg-plane-*)——Task 5 Step 3 的门控用例。
4. **`--chat-col` 改档波及 <1440**——Task 6 Step 3 的 1024 视口 860 断言。
5. **trap 嵌套走查**(HitlStack trap + GrantsPanel trap 同开)——Task 7 Step 2 的 trapStack 顶层用例。

---

### Task 1: F1 后端 — 取消轮派生历史持久化

**Files:**
- Modify: `internal/server/runs.go`(578 onDone 取消分支、538 排队取消分支)
- Modify: `internal/server/goalapi.go`(220 排队取消、231 onDone)
- Create: `internal/server/stopmark.go`(标签构造与派生历史 helper)
- Modify: `internal/agent/retain.go:223-238`(runQuestion 跳过表)
- Modify: `internal/tui/tui.go:1328-1337`(historyForRun 折叠守卫)
- Test: `internal/server/stopmark_test.go`、`internal/agent/retain_test.go`(扩展)、`internal/tui` 相关测试(扩展)

**Interfaces:**
- Produces: `func stopNotice(runID, reason string) messages.Message`(Human,内容 `<run-stopped run="<id>" reason="user_stop|parked_ttl"/>`);`func derivedCancelledHistory(history []messages.Message, human *messages.Message, runID, reason string) []messages.Message`(Clone+可选 human+标签;goal 调用传 nil human)。常量 `StopReasonUser = "user_stop"`、`StopReasonParkedTTL = "parked_ttl"`。
- Consumes: 既有 `s.sessions.save(context.WithoutCancel(...), sid, stripSystem(final), workspace)` 姿势(runs.go:578-584)。

- [ ] **Step 1: 写失败测试** `stopmark_test.go`:①主 run 公式=Clone(history)+human+标签且不改底层 history 切片(别名污染用例);②goal 公式(nil human)无重复 human;③标签 reason 两种取值;④parkedRunTTL 路径(run.go:387)reason=parked_ttl。retain_test:runQuestion 对 `<run-stopped .../>` 结尾历史取到的是**前一条真实 human**。tui historyForRun:末位为标签时 prompt 取其前末位 human,取不到为空。
- [ ] **Step 2: 跑测试确认失败** `go test ./internal/server/ -run StopMark ./internal/agent/ -run RunQuestion`(helper 未定义)。
- [ ] **Step 3: 实现** stopmark.go(helper)+ runs.go/goalapi.go 四个取消分支接 helper 后走既有 save(WithoutCancel+stripSystem);`ranClean`/`maybeWake` 不动。
- [ ] **Step 4: 跑 Go 全量** `go build ./... && go test ./internal/server/ ./internal/agent/ ./internal/tui/` 全绿(含既有 /btw 不持久化断言不回归)。
- [ ] **Step 5: Commit** `feat(server): persist cancelled runs with a stop notice`。

### Task 2: F1 前端 — notice 标签渲染与双卡回归

**Files:**
- Modify: `web/src/lib/transcript.ts`(NOTICE_TAGS 加 `'<run-stopped'`)
- Modify: `web/src/components/ChatItemView.vue`(notice 分支特判)
- Modify: `web/src/i18n/locales/zh-CN/chat.ts`、`en/chat.ts`(新键 `runTimedOut`:「等待审批超时已停止」/"Stopped after waiting too long for approval";`generationStopped` 已有)
- Test: `web/src/lib/transcript.test.ts`、`web/src/components/ChatItemView.test.ts`(扩展)

**Interfaces:**
- Consumes: Task 1 的标签格式 `<run-stopped run="…" reason="…"/>`。
- Produces: notice item 渲染规则——content 以 `<run-stopped` 开头→文案 `reason==='parked_ttl' ? t('chat.runTimedOut') : t('chat.generationStopped')`;其余 notice 维持 raw。

- [ ] **Step 1: 失败测试**:isNoticeContent 新前缀;ChatItemView 渲染两种 reason 的 i18n 文案而非原始标签;刷新路径(messagesToItems)与 live 瞬时卡共存时 items 只含一个 run-stopped notice(同一 run id 去重——沿用 e7c9143e 的 stoppedRunNotices 集合语义)。
- [ ] **Step 2: 确认失败** `cd web && npx vitest run src/lib/transcript.test.ts src/components/ChatItemView.test.ts`。
- [ ] **Step 3: 实现** + 双语键;`npx vue-tsc --noEmit` 过。
- [ ] **Step 4: 全量 vitest** `npx vitest run`(≥666)。
- [ ] **Step 5: Commit** `feat(web): render persisted run-stopped notices`。

### Task 3: F1 e2e — 停止→reload→通知仍在

**Files:**
- Add: `e2e/regression/` 新用例(挂入既有 ordered 框架,编号顺延)或 `e2e/tests/personal/` 扩展(选一处,与 00-06 编号约定一致)

**Interfaces:**
- Consumes: fakemodel 慢流式(既有 slow 模式先例:chunk 间隔 300ms);`/api/agent/permissions` bypass。

- [ ] **Step 1: 写用例**:发起慢流 run→点停止→断言 notice 卡→`page.reload()`→断言 notice 卡仍在且为 notice 类;附 1024 视口回归断言消息列宽(为 Task 6 预铺,可拆)。
- [ ] **Step 2: 跑** `npx playwright test <spec> --project=chromium`(按 e2e 既有跑法;webServer 起真二进制)。
- [ ] **Step 3: Commit** `test(e2e): cancelled-run stop notice survives reload`。

### Task 4: F2 原型 gate — 设置保存双原型(交付拍板,不实施)

**Files:**
- Create: `woca-internal-docs/settings-save-prototype/{prototype-a.html,prototype-b.html}`(仓外,不进 git;HTML+tokens.css 内联直出)

- [ ] **Step 1: 产两版静态原型**(spec F2 的 A 分区保存/B 全局 dirty 图,真实 tokens 取值、zh 文案),每版含偏好区角标、分区保存态(dirty/干净)、安全提示新位置。
- [ ] **Step 2: 交付用户选 A/B**(阻塞 Task 5;用户未拍板则 Task 5 跳过并记录)。

### Task 5: F2 实施 — 区→键映射+子集 PUT+dirty(前置:Task 4 拍板)

**Files:**
- Modify: `web/src/stores/serverSettings.ts`(映射表、分区快照、`saveSection(id)`、dirty computed)
- Modify: `web/src/components/SettingsPanel.vue`(分区保存件/即时角标/底栏按案拆除/安全提示迁移)
- Modify: i18n zh-CN/en settings 命名空间(角标「即时生效 / Applied instantly」等)
- Test: `web/src/stores/serverSettings.test.ts`(新建)、`SettingsPanel.test.ts`(扩展)

**Interfaces:**
- Produces: `SECTION_KEYS: Record<SectionId, string[]>`(appearance/preferences 为空数组=即时区);`saveSection(id: SectionId): Promise<void>`(同端点 `PUT /api/settings` 仅本区键,可用性门控沿用 serverSettings.ts:230-243 规则);`sectionDirty(id): boolean`。
- Consumes: 服务端「缺省键=不变」语义(serverSettings.ts:244-246)。

- [ ] **Step 1: 失败测试**:映射表覆盖四张显式清单全部键(对照 woca settings 已登记清单逐键断言,防漏);saveSection 只发本区键;msgPlaneAvailable=false 时模型/存储区子集不含 msg-plane-*;sectionDirty 初 false、改后 true、保存后 false。
- [ ] **Step 2: 确认失败** vitest。
- [ ] **Step 3: 实现**(案 A/B 按 Task 4 拍板;EE 门控纪律不变)。tsc 过。
- [ ] **Step 4: 全量 vitest + Commit** `feat(web): per-section settings save with dirty state`。

### Task 6: F3 — --chat-col 改档(≥1440 760 / ≥1800 760 封顶)

**Files:**
- Modify: `web/src/styles/tokens.css:268-279`(两处媒体档值+注释「wide screens read narrow by design」与九消费点防回归清单注释)
- Test: e2e(personal)视口断言(挂 Task 3 spec 或独立)

**Interfaces:**
- Consumes: spec F3 九消费点清单(纯变量改档,零选择器改动)。

- [ ] **Step 1: 失败断言**(e2e):1440 视口 `.message` 与 `.chat-input-inner` computed width ≤760 且差 ≤1px;1800 同;1024 视口 =860;375 不变。
- [ ] **Step 2: 改档跑断言过**(先 `npm run build` 再起真二进制,遵仓规)。
- [ ] **Step 3: 截图抽检** 1440 zh/en 对话页前后对比(链接/代码块视觉零变化,列宽变化符合预期)。
- [ ] **Step 4: Commit** `feat(web): narrow reading column on wide viewports`。

### Task 7: F4 — 两抽屉接 useFocusTrap + 单测补齐

**Files:**
- Modify: `web/src/components/Sidebar.vue`(左抽屉接 trap,替换现有手挂 Esc)
- Modify: `web/src/components/WorkspaceSidebar.vue`(右抽屉接 trap+Esc)
- Create: `web/src/composables/useFocusTrap.test.ts`
- Test: 同上新建文件

**Interfaces:**
- Consumes: 既有 `useFocusTrap(container: Ref<HTMLElement|null>, active: Ref<boolean>|boolean, opts?)`(trapStack 分层、还焦、滚动锁已内建)。

- [ ] **Step 1: 失败测试**:Tab 首尾环绕/Shift+Tab 逆向/active=false 零监听/关闭还焦/trapStack 双 trap 时仅顶层动作(嵌套走查=Review Focus 5)。
- [ ] **Step 2: 确认失败** vitest。
- [ ] **Step 3: 两组件接入**(右抽屉 Esc 经 trap opts 或复用 trap 内建 esc)。
- [ ] **Step 4: 全量 vitest + tsc + Commit** `feat(web): focus-trap both drawers, unit-test the composable`。

### Task 8: F5 — code-accent 登记 + 棘轮冻结

**Files:**
- Modify: `web/src/styles/tokens.css`(注释性 `--code-accent-{keyword,string,comment,function,number}` 亮暗两套,值=现 github 主题实际色,注明来源与冻结声明;例外规则注释块)
- Modify: `web/scripts/check-tokens.mjs`(ALLOWED 清单长度对拍断言:冻结现状,新增即 fail)
- Test: `web/scripts/check-tokens.mjs` 自身跑通 + 手工注入一条违规验证 fail(后还原)

- [ ] **Step 1: 取值**:从 highlight.ts 双主题实际渲染(Go/TS 样本)读取五类 token 色写入 tokens。
- [ ] **Step 2: 棘轮断言**实现并验证(注入→fail→还原→pass)。
- [ ] **Step 3: `npm run check:tokens` 全绿 + Commit** `chore(web): register code accents, freeze token ratchet`。

### Task 9: F6 — 平板图标栏三视图原型(gate,不实施)

**Files:**
- Create: `woca-internal-docs/tablet-icon-rail-prototype/{icon-rail,expanded,current-280}.html`(仓外)

- [ ] **Step 1: 三视图静态原型**(56px 图标栏/点开 overlay/现状 280px,tokens 直出)。
- [ ] **Step 2: 交付拍板**(是否立项为后续 spec;无代码)。

---

## 执行说明

- Task 1-3 串行(同链);Task 4→5 串行(拍板 gate);Task 6/7/8 相互独立可并行派工;Task 9 独立。
- Task 4/9 交付物在仓外,拍板记录回写 spec「审计记录」节后追加。
- 全部完成后走 final review(参照 2026-11-17 批次的 SDD 终审姿势)。

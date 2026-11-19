# F6 平板图标栏 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 落地 2026-11-19 F6 spec(v2):641-1023px 带内左侧栏折叠为 56px 图标栏,会话列表点开为贴栏浮层(scrim 遮聊天),复用既有 trap/对话框/列表组件。

**Architecture:** layout.css 新增带媒体规则 + Sidebar.vue 内 railShell(MQ 观测)分叉两套子树;浮层=WorkspaceSection+SessionList 直用;useFocusTrap 门控=WorkspaceSidebar 惯例镜像 + Esc 让位守卫 + band 退出复位 watcher(自建小增强件)。对话框簇留在共享 aside 根。

**Tech Stack:** Vue3+TS+Pinia、layout.css、vitest(matchMedia stub 惯例=Sidebar.test.ts:50-65)、Playwright(内联 setViewportSize 惯例)。

**Spec:** docs/superpowers/specs/2026-11-19-tablet-icon-rail-design.md(v2 审计修订版——spec 是权威,本计划从其论证)

## Global Constraints

- 注释/代码/commit message 全英文;i18n 新键 `sidebar.sessions` zh-CN 与 en 同步。
- 不新增颜色,全部引既有 token;rail 56px/触控 44×44 为局部尺寸常量(注释登记出处=原型)。
- 禁止 SessionList 内 matchMedia(形态分叉只在 Sidebar 装配层);`#app-sidebar` id 三带持续保留(ChatPanel aria-controls 依赖)。
- web 改动后 `npm run build` 再 `go build ./...`;每 task 目标测试+全量门禁(make check、vitest、vue-tsc)。

## Review Focus

1. **rail 被 scrim 盖死**(spec I1)——rail z21 锚定;测试:浮层开时 rail 按钮可点且不被 dim(Task 2 Step 3)。
2. **Esc 连坐关浮层留孤儿对话框**(spec I2)——esc 回调 `.overlay, .modal-backdrop, .w-modal-backdrop` 让位守卫;测试:浮层+ws-dialog 同开,Esc 只关对话框(Task 3 Step 2)。
3. **跨带残留**——band 退出两个方向 flyoutOpen 复位、trap 释放(行为代理断言:body overflow 恢复/焦点可逃逸,Sidebar.test.ts:223-233 范本;trapStack 模块私有不可直断)。
4. **对话框双实例**——分叉子树不得复制 ws-dialog/FolderPicker/StatusModal(M6);测试:两形态下 ws-dialog 各恰好一实例。
5. **右栏 overlay 同开栈序**——浮层开时 ⌘J 开右栏,z30 压上,Esc 先关右栏(Task 3 Step 3 走查)。

---

### Task 1: CSS 带 + rail 骨架

**Files:**
- Modify: `web/src/styles/layout.css`(带媒体规则:rail 56px、rail-btn 44×44、隐藏 280 列内容)
- Modify: `web/src/components/Sidebar.vue`(railShell MQ 观测 `(min-width: 641px) and (max-width: 1023px)`;两套子树分叉;对话框簇留共享根)
- Modify: `web/src/i18n/locales/{zh-CN,en}/sidebar.ts`(`sidebar.sessions` 一键)

**Interfaces:**
- Produces: Sidebar 内 `railShell = ref(false)` + MQ watcher(惯例复制 Sidebar.vue:41-48 的 ≤640 观测);`flyoutOpen = ref(false)`(Task 2 用)。
- Consumes: 既有 ws-dialog/StatusModal/SettingsPanel 处理器直挂 rail 钮(状态钮带 hasStatusSurface 条件)。

- [ ] **Step 1:** 失败测试先行——Sidebar.test.ts 加 matchMedia stub 分支(带查询串):641-1023 下 rail 四钮渲染、280 列内容不渲染、≥1024/≤640 两端不渲染 rail;`#app-sidebar` id 在三带都在。
- [ ] **Step 2:** 实现 CSS+分叉。rail 按钮先只做 会话(占位不开浮层)+新建工作区+状态+设置;i18n 键落地。
- [ ] **Step 3:** vitest+vue-tsc+build 绿;Commit `feat(web): tablet-band 56px icon rail skeleton`。

### Task 2: 浮层 + 幕布 + 开合

**Files:**
- Modify: `web/src/styles/layout.css`(flyout/scrim:z20/z19,rail 浮层开时 z21,scrim color-mix+blur+cursor:pointer)
- Modify: `web/src/components/Sidebar.vue`(flyout 结构:头+WorkspaceSection+SessionList;五条收起路径)
- Modify: `web/src/components/SessionList.vue`(收起经 props 回调注入,组件零形态感知)

**Interfaces:**
- Produces: SessionList 新 prop `onSelectExtra?: () => void`(selectSession 尾调;≤640 路径照旧由 ui.sidebarOpen 承担,Sidebar 传 `() => (flyoutOpen.value = false)`)。
- Consumes: 原型 token/结构(expanded.html 逐条对照:flyout-head、side-section 复用 layout.css 既有规则)。

- [ ] **Step 1:** 失败测试:点会话钮→浮层开+aria-expanded/aria-controls 生效+scrim 可见;点幕布/再点钮/关闭钮/选中会话四路收起(Esc 在 Task 3)。
- [ ] **Step 2:** 实现浮层;rail active 左缘 3px 墨条。
- [ ] **Step 3:** 断言浮层开时 rail 按钮仍可交互(不被 scrim dim——z21 生效的代理断言:getComputedStyle zIndex + 按钮可点)。
- [ ] **Step 4:** vitest+build;Commit `feat(web): sessions flyout for the tablet rail`。

### Task 3: trap + 让位守卫 + 跨带复位

**Files:**
- Modify: `web/src/components/Sidebar.vue`(useFocusTrap 接入;esc 让位守卫;band 退出 watcher 复位 flyoutOpen)
- Test: `web/src/components/Sidebar.test.ts`(扩展)

- [ ] **Step 1:** 失败测试:浮层开 trap 圈焦(Tab 循环/首焦);Esc 收起;浮层+ws-dialog 同开 Esc 只关对话框(守卫);band 退出两方向复位(行为代理:body overflow/焦点逃逸,「仅一个 listener」断言参照 Sidebar.test.ts:221)。
- [ ] **Step 2:** 实现:trap active=flyoutOpen&&railShell;esc 回调镜像 Sidebar.vue:54-63 守卫;watch railShell 变 false→flyoutOpen=false。
- [ ] **Step 3:** 走查测试:浮层开时模拟 ⌘J 开右栏→Esc 先关右栏再关浮层(trapStack LIFO)。
- [ ] **Step 4:** vitest+vue-tsc+build;Commit `feat(web): trap the sessions flyout with dialog yield guard`。

### Task 4: e2e 冒烟(768/1023)

**Files:**
- Create: `e2e/tests/personal/rail.spec.ts`(dedicated 实例姿势=stopnotice.spec.ts)

- [ ] **Step 1:** 768/1023 两视口:rail 可见+聊天列宽=视口-56(712/967 computed 断言,盒测量参照 chatcolumn.spec.ts);开浮层→幕布可见→Esc 收起;选中会话即关。
- [ ] **Step 2:** `npx playwright test tests/personal/rail.spec.ts` 连续 3 次绿;Commit `test(e2e): tablet rail smoke at 768/1023`。

### Task 5: 全量门禁收口

- [ ] **Step 1:** `make check`、web vitest 全量、vue-tsc、npm run build+go build 全绿;确认 check:tokens 基线不动(无新色值)。
- [ ] **Step 2:** e2e personal 全套不回归(`npx playwright test tests/personal/`)。

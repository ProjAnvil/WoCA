# F6 平板图标栏(641-1023)— Design Spec

日期:2026-11-19 · 状态:audited(审计发现已内嵌并修订,见「裁定与审计记录」;v2 Ready for planning) · 基线:main @ 9e39c7e9
来源:2026-11-17 审计 P0②(641-1023 无中间形态)→2026-11-18 三视图原型交付 gate。原型:woca-internal-docs/tablet-icon-rail-prototype/{icon-rail,expanded,current-280}.html(静态自查,token 内联自 tokens.css)。实测:1024px 视口现状 280px 侧栏占约 27%,聊天列约 744px;56px rail 后约 5.5%,聊天列约 968px。

## 0. 总览与非目标

**做什么**:641-1023px(平板带)把左侧栏从 280px 静态列折叠为 56px 图标栏;会话/工作区内容点开为贴栏浮层(280px 覆盖层+幕布),复用现有列表组件与既有对话框。≥1024 与 ≤640 形态不动。

**非目标**:
- ≥1024 右栏 overlay 阈值(1100)与 ≤640 抽屉形态——不动。
- 会话搜索/拖拽/多选——不做。
- ≥1024 侧栏改 rail 或可折叠——另案(用户 2026-11-18 已拍板 641-1023 维持原型 gate,本 spec 只管该带)。
- 新增色 token——全部复用 tokens.css 既有变量(rail 宽 56/触控 44×44 为局部尺寸常量,注释登记出处=原型)。

## 1. 现状核证(file:line @ 基线)

- `web/src/styles/layout.css:11-58`:`.sidebar` 280px flex 列;`@media (max-width: 640px)` 切 overlay 抽屉 + `.sidebar-backdrop`。
- 左抽屉状态:`web/src/stores/ui.ts:10-24` sidebarOpen(open/close/toggle);trap 门控 `web/src/components/Sidebar.vue:41-48` drawerShell=matchMedia(`max-width: 640px`)(0d04e971 的宽度门控教训:跨形态放大窗不得残留滚动锁/Tab 囚禁)。
- 右抽屉惯例:`web/src/components/WorkspaceSidebar.vue:20-33` drawerShell=matchMedia(`max-width: 1100px`),`useFocusTrap(panel, computed(() => open && drawerShell))`。
- trap 基建:`web/src/composables/useFocusTrap.ts:18` trapStack 嵌套栈(Esc 只归栈顶)。
- 侧栏内容组件:WorkspaceSection+SessionList(`web/src/components/SessionList.vue`,含 running/awaiting 徽标与 runStateOf);新建工作区 ws-dialog、StatusModal、SettingsPanel 均既有(Sidebar.vue:130-137 触发钮,对话框簇 159-211 挂在 aside 尾部)。状态入口条件渲染 `v-if="statusStore.hasStatusSurface"`(Sidebar.vue:145,个人 serve 无 /api/status 时隐藏)。
- 「抽屉选中会话即关」惯例:≤640 抽屉已做(2026-11-17 polish 波),浮层沿用。
- 触控底线:icon-btn 24px 底线(T1),rail 按钮 44×44(原型 ≥44px 下限,平板触控)。

## 2. 设计

### D1 形态切换(CSS)

`layout.css` 新增 `@media (min-width: 641px) and (max-width: 1023px)`:

- `.sidebar` 在该带渲染为 56px rail(`flex: 0 0 56px`),栏内只留:品牌格(56×56)、四个 rail 按钮(会话/新建工作区/状态/设置)、底部弹性空隙。280px 列内容(工作区区节+会话列表)在该带移入浮层(D3),不占布局宽。
- 模板分叉:Sidebar.vue 内 `railShell` 状态(见 D5)控制两套子树渲染(280 列内容 vs rail+浮层)。**双轨求值,以 CSS 为准**:layout.css 写 `@media` 规则,Sidebar.vue 挂同一断点串的 MQ 观测驱动 DOM——与 ≤640 现状(layout.css media + Sidebar.vue:48 JS MQ)同构,漂移窗口限 change 事件一帧。对话框簇(ws-dialog/FolderPicker/StatusModal)保持在共享 aside 根、子树分叉之外——`wsDialogOpen` 单一 ref 使 rail 钮与浮层头钮天然共用一态,分叉复制会出双实例。**禁止**在核心路径嗅探视口做行为分叉——本项全部行为差异都在 Sidebar 组件装配层。
- 已知留白(登记不改):`web/src/stores/workspace.ts:12` 的 `LEFT_SIDEBAR_WIDTH=280` 被 `workspaceMaxWidth()` 按常量扣减,641-1023 带内实际 56——带内右栏走 overlay(panels.css:355-374)CSS 遮蔽,无行为回归,仅注释口径失真。
- 聊天列自然放宽(56px 取代 280px,无其他改动;--chat-col 各档不动)。

### D2 rail 按钮

- 会话(rail 主按钮):`aria-expanded` 绑浮层开态,`aria-controls="#rail-flyout"`;active 态=浮层开(active 带左缘 3px 墨条,出处=原型 expanded.html)。
- 新建工作区/状态/设置:直接复用既有处理器(ws-dialog/StatusModal/SettingsPanel),无新面。状态钮沿用 `v-if="statusStore.hasStatusSurface"` 条件(Sidebar.vue:145)。
- 尺寸:44×44,图标 lucide 同源;`title`+`aria-label` 双语。i18n 盘点:复用 `sidebar.newWorkspace`/`sidebar.settings`/`status.entry`/`common.close`;新增 `sidebar.sessions` 一键两用(会话钮 label + 浮层头标题)。

### D3 浮层(会话列表)

- 结构:`aside.rail-flyout` absolute、`left:56px`、宽 280px、`top/bottom:0`,`z-index:20`;内部=浮层头(标题「会话」+新建工作区 icon-btn+关闭钮)+ WorkspaceSection+SessionList 既有组件直用(样式从 layout.css 既有规则继承,原型已逐条对照)。
- **rail 锚定(审计 I1)**:浮层开启期间 rail 自身 `position:relative; z-index:21`(出处=原型,「压在浮层投影之上,读作浮层的锚」)——否则全屏 scrim 盖死 rail,「再点会话钮收起」命中的是幕布,rail 被 dim+blur、hover/active 失效。
- 幕布:`.flyout-scrim` absolute 全屏(`z-index:19`),`color-mix(in srgb, var(--bg) 55%, transparent)` + `backdrop-filter: blur(2px)`(现网 overlay 幕布惯用法,panels.css .overlay/layout.css .sidebar-backdrop 同族),`aria-hidden="true"`、`cursor:pointer`,点击收起。
- 收起路径(全量):点幕布 / 再点会话 rail 钮 / Esc(trap)/ 浮层关闭钮 / 选中会话即关(惯例沿用)。实现缝:`SessionList.selectSession` 今天只关 `ui.sidebarOpen`(SessionList.vue:26-29);浮层收起经 props 回调注入(SessionList 不嗅探形态),或 ui store 增 railFlyoutOpen 由 selectSession 一并关——二选一在 plan 定,禁止 SessionList 内 matchMedia。
- 开启时滚动锁:随 trap 免费获得(useFocusTrap 的引用计数 body `overflow:hidden`,非独立代码);body 锁不锁 `.chat-messages` 内滚,带内真隔离来自幕布挡指针+trap 圈焦点——body 锁是冗余保险。

### D4 裁定点:幕布遮聊天(原型形态批准)

浮层开启时幕布覆盖聊天列(变暗+轻模糊),聊天不可交互。理由:①触控关闭目标=整面幕布,平板误触率高;②防止「浮层开着还在聊天打字」的两态含糊;③与 ≤640 backdrop、右栏 overlay 的既视感一致。非阻塞式(无幕布、聊天可继续交互)否决。

### D5 阈值边界

- **640↔641**:≤640 整个 sidebar 切汉堡抽屉形态(既有),rail 不渲染;flyoutOpen 与 sidebarOpen 两套独立状态。**门控**=镜像 0d04e971 的效果门控(trap active = flyoutOpen && railShell,该提交本身不重置状态、stale sidebarOpen 无害);**复位**=新增 band 退出 watcher `flyoutOpen=false`(自建小增强件,仓库无既有先例——不复位则重入带浮层意外重开)。
- **1023↔1024**:回 280px 静态列;同 watcher 复位、trap 随门控释放。
- 实现:Sidebar.vue 新挂 `matchMedia('(min-width: 641px) and (max-width: 1023px)')` 观测(Sidebar.vue:41-48 惯例复制)。带界穿越会使 SessionList 重挂、分组折叠态复位(collapsed 组件本地态)——罕见低害,登记。
- z-index 全景(带内核证):scrim 19 < flyout 20 < rail 21 < 右栏 overlay 30(panels.css:371)< .overlay 40(ws-dialog)< .modal-backdrop 150 < WModal 410 < toast 500 < palette 600。右栏与浮层可同开(⌘J 仍可达):右栏 z30 压上,trapStack 依开序 LIFO,Esc 先关右栏再关浮层,与视觉一致。

### D6 focus trap

`useFocusTrap(flyoutEl, computed(() => flyoutOpen && railShell.value))`——WorkspaceSidebar.vue:33 惯例镜像;首焦聚是默认行为(useFocusTrap.ts:88-92,opts 无 initialFocus)。**Esc 让位守卫(审计 I2)**:ws-dialog/FolderPicker/StatusModal 不带 trap(Sidebar.vue:36-39 注释),trapStack 栈顶仍是浮层——Esc 会先关浮层、对话框孤儿悬空。浮层 trap 的 esc 回调镜像 ≤640 抽屉的守卫:`activeElement.closest('.overlay, .modal-backdrop, .w-modal-backdrop')` 命中即让位(Sidebar.vue:54-63;回归测试范本 Sidebar.test.ts:173-196)。

## 3. 测试与验收

vitest(组件级,镜像 Sidebar trap 既有测试姿势):
1. 641-1023 matchMedia mock 下:rail 渲染、280 列内容不渲染;浮层开 aria-expanded=true、controls 指向浮层;点幕布/Esc/再点/选中会话→收起。
2. 边界重置:band 进入→离开(两个方向),flyoutOpen 复位、trap 释放(trapStack 空)。
3. 徽标继承:SessionList 在浮层内 running/awaiting 徽标照常(runStateOf 直用)。
4. 双语:zh-CN/en 快照 rail 按钮标签。

e2e(personal,768 与 1023 两个视口,内联 `page.setViewportSize` 惯例=chatcolumn.spec.ts:186):
1. rail 可见、聊天列宽 = 视口-56(768 档断言 712、1023 档 967,computed)。
2. 开浮层→幕布可见→Esc 收起;选中会话即关。

门禁:check:tokens(无新色值)、vue-tsc、npm run build 后 go build、make check。

## 4. 裁定与审计记录

审计(2026-11-19,code-reviewer):Needs revision→已修订——I1 rail z21 锚定、I2 Esc 让位守卫、M1 双轨求值表述、M2 workspace.ts 留白登记、M3 状态钮条件、M4 `sidebar.sessions` 新键、M5 initialFocus 勘误、M6 对话框簇挂共享根、M7 0d04e971 引述改「效果门控+新增复位 watcher」、行号与 e2e 数字修正。OQ 裁定(审计建议):OQ1 承载新建工作区钮(与浮层头共用 wsDialogOpen 单态);OQ2 不放工作区文件入口;OQ3 e2e 发送冒烟缓做(选中会话即关已覆盖 selectSession 经浮层路径)。架构方向与全部惯例引用经 HEAD 核证成立。

## 5. 状态

v2(审计修订版)——Ready for planning。

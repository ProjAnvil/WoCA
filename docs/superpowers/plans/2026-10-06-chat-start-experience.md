# Chat Start Experience Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 新会话居中时分问候 + composer FLIP 沉底转场 + 消息入场/等待动画 + markdown 双格式复制（主钮源码、侧下拉纯文字）与代码块复制。

**Architecture:** 单一 `isGreeting` 谓词驱动 `.chat--greeting` 布局态（flex auto-margin 居中，不可插值故 FLIP transform 补偿）；消息动画走 fresh 标记法（store live-push 打标、keyframe 插入时播一次）；代码块装饰走渲染后 DOM 后处理 + 事件委托（v-html 逐 tick 替换，不能注入进 sanitized HTML）；流式平滑是显示层缓冲（store 恒为 source of truth）。

**Tech Stack:** Vue 3 `<script setup>` + pinia + vue-i18n（en/zh-CN 全树一致）、marked v18 + DOMPurify、vitest + happy-dom、Playwright（回归）。

**Spec:** `woca-internal-docs/chat-start-experience-research.md`（§3 综合方案 + §3.5 审计修订记录——本 plan 依据修订版实现；两份都要读）

## Global Constraints

- `.chat-input` 根类名与根结构不可改（5 个 e2e spec 锚定：personal/native-dev/bridge/edge/relay）；`textarea` + Enter 提交路径不可破（ChatPanel.test.ts）。
- i18n：`web/src/i18n/locales/{en,zh-CN}/chat.ts` 双语同步增删改（locales.test.ts 强制全树一致）。
- 黑白灰语义 token（组件禁裸 hex/color 字面量，`npm run check:tokens` 校验）；motion 三铁律：微动效位移 ≤6px、enter 慢于 leave、只动 transform/opacity（border/color 过渡属低频一次性 carve-out；FLIP 属布局态转场不受 6px 管辖）。
- 验证链：`cd web && bun run test` → `npm run build`（含 vue-tsc）→ 仓库根 `go build ./...`。改前端必须先 npm run build 再 go build。
- 每任务一提交，前缀 `web:`；提交前该任务全部测试绿。
- reduced-motion：全局豁免已存在（tokens.css:352-362），FLIP 需 JS 侧 matchMedia 守卫。
- 不在本计划范围的拍板项（勿顺手实施）：工作区感知问候、shiki 聊天高亮、820px→--chat-col 统一。

---

### Task 1: greeting 时间分段纯函数

**Files:**
- Create: `web/src/lib/greeting.ts`
- Test: `web/src/lib/greeting.test.ts`

**Interfaces:**
- Produces: `greetingSlot(d?: Date): 'morning' | 'afternoon' | 'evening' | 'lateNight'`（Task 2 消费，拼 `chat.greeting.${slot}` i18n 键）

- [ ] **Step 1: 写失败测试**

```ts
// web/src/lib/greeting.test.ts
import { describe, expect, it } from 'vitest'
import { greetingSlot } from './greeting'

const at = (h: number) => new Date(2026, 9, 6, h, 0)

describe('greetingSlot', () => {
  it.each([5, 8, 11])('%i:00 → morning', (h) => expect(greetingSlot(at(h))).toBe('morning'))
  it.each([12, 15, 17])('%i:00 → afternoon', (h) => expect(greetingSlot(at(h))).toBe('afternoon'))
  it.each([18, 20, 21])('%i:00 → evening', (h) => expect(greetingSlot(at(h))).toBe('evening'))
  it.each([22, 23, 0, 3, 4])('%i:00 → lateNight', (h) => expect(greetingSlot(at(h))).toBe('lateNight'))
})
```

- [ ] **Step 2: 跑测试确认失败**

Run: `cd web && bun run test src/lib/greeting.test.ts`
Expected: FAIL（模块不存在）

- [ ] **Step 3: 实现**

```ts
// web/src/lib/greeting.ts
export type GreetingSlot = 'morning' | 'afternoon' | 'evening' | 'lateNight'

/** Time-of-day bucket for the empty-session greeting: 05–11 morning,
 * 12–17 afternoon, 18–21 evening, else late night (local time). */
export function greetingSlot(d = new Date()): GreetingSlot {
  const h = d.getHours()
  if (h >= 5 && h < 12) return 'morning'
  if (h >= 12 && h < 18) return 'afternoon'
  if (h >= 18 && h < 22) return 'evening'
  return 'lateNight'
}
```

- [ ] **Step 4: 跑测试确认通过** — `cd web && bun run test src/lib/greeting.test.ts` → PASS

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/greeting.ts web/src/lib/greeting.test.ts
git commit -m "web: greeting time-slot helper for the empty-session start screen"
```

---

### Task 2: 问候态（布局 + 问候语 + 二段式 chips + i18n 重构）

**Files:**
- Modify: `web/src/components/ChatPanel.vue`（isGreeting、模板、删 WEmptyState/usePrompt/emptyPrompts）
- Delete: `web/src/components/ui/WEmptyState.vue`（唯一消费者即 ChatPanel，先 `grep -rn WEmptyState web/src` 复核；组件无专属测试文件）
- Modify: `web/src/components/composer/Composer.vue`（greeting prop + .greeting-chips）
- Modify: `web/src/i18n/locales/en/chat.ts`、`web/src/i18n/locales/zh-CN/chat.ts`
- Modify: `web/src/styles/chat.css`（greeting 规则；删 .empty-prompt-chip 块）
- Test: `web/src/components/ChatPanel.test.ts`（扩充）

**Interfaces:**
- Consumes: `greetingSlot`（Task 1）
- Produces: ChatPanel 暴露 `isGreeting` 布局态类 `.chat--greeting`（Task 3 FLIP 用）；Composer 新 prop `greeting?: boolean`

- [ ] **Step 1: 写失败测试（追加到 ChatPanel.test.ts 的 describe 内）**

```ts
import { greetingSlot } from '../lib/greeting'

it('shows the centered greeting state on an empty session', () => {
  const wrapper = mountPanel()
  expect(wrapper.get('.chat').classes()).toContain('chat--greeting')
  expect(wrapper.get('.chat-greeting-text').text()).toBe(
    i18n.global.t(`chat.greeting.${greetingSlot()}`),
  )
})

it('fills the composer from a greeting chip prompt', async () => {
  const wrapper = mount(ChatPanel, {
    global: { plugins: [i18n, createPinia()] },
    stubs,
    attachTo: document.body,
  })
  try {
    await wrapper.get('.greeting-chip').trigger('click')
    expect((wrapper.get('textarea').element as HTMLTextAreaElement).value).not.toBe('')
    expect(document.activeElement).toBe(wrapper.get('textarea').element)
  } finally {
    wrapper.unmount()
  }
})

it('leaves the greeting state once a message exists', async () => {
  const wrapper = mountPanel()
  const s = useSessionStore()
  s.activeId = 's1'
  s.items = { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] }
  await wrapper.vm.$nextTick()
  expect(wrapper.get('.chat').classes()).not.toContain('chat--greeting')
  expect(wrapper.find('.chat-greeting').exists()).toBe(false)
})
```

（`items`/`activeId` 是 store state，直接赋值即可；如 TS 报 Record 类型不匹配，用 `s.$patch({ activeId: 's1', items: { s1: [...] } })`。）

- [ ] **Step 2: 跑测试确认失败** — `bun run test src/components/ChatPanel.test.ts` → 新增用例 FAIL

- [ ] **Step 3: i18n 双语改造**

en/chat.ts：删除 `emptyHint: 'Start a new conversation.',` 行；`emptyPrompt1..3` 三行字符串替换为嵌套对象；在 `awaitingConfirm` 行后插入 greeting 块：

```ts
  greeting: {
    morning: 'Morning — how can I help?',
    afternoon: 'Good afternoon — how can I help?',
    evening: 'Good evening — how can I help?',
    lateNight: 'Working late — how can I help?',
  },
```

```ts
  emptyPrompt1: { title: 'Walk this repo', prompt: 'Summarize this repository’s structure and key modules' },
  emptyPrompt2: { title: 'Review recent changes', prompt: 'Review the latest diffs and flag risks' },
  emptyPrompt3: { title: 'Add a small feature', prompt: 'Describe a small feature and implement it in this workspace' },
```

zh-CN/chat.ts 同位置同样改：删 `emptyHint: '开始一个新对话吧。',`；

```ts
  greeting: {
    morning: '早上好，今天从哪里开始？',
    afternoon: '下午好，需要我做点什么？',
    evening: '晚上好，有什么可以帮你？',
    lateNight: '夜深了，需要我搭把手吗？',
  },
```

```ts
  emptyPrompt1: { title: '理解这个仓库', prompt: '概述这个仓库的结构与关键模块' },
  emptyPrompt2: { title: '审查最近改动', prompt: '审查最新的改动并指出其中的风险' },
  emptyPrompt3: { title: '加一个小功能', prompt: '描述一个小功能，并在当前工作区里实现它' },
```

- [ ] **Step 4: ChatPanel.vue 改造**

script：删除 `WEmptyState` import、`emptyPrompts` computed、`usePrompt` 函数（chips 移入 Composer）；新增：```ts
import { greetingSlot } from '../lib/greeting'
// ...
/* ------------------------------ empty state ------------------------------- */

// The single source of truth for the centered "greeting" layout: no items,
// no run in flight, no history fetch — exactly when the transcript is
// genuinely empty. Drives the .chat--greeting layout class, the greeting
// block and the composer's chip row (and, in the next task, the FLIP).
const isGreeting = computed(
  () => store.activeItems.length === 0 && !busy.value && !store.historyLoading,
)
const greetingKey = computed(() => greetingSlot())
```

模板：`.chat` 根加绑定；WEmptyState 的 `<Transition name="fade">…</Transition>` 整块（194-202 行）替换为：

```html
      <Transition name="fade">
        <div v-if="isGreeting" class="chat-greeting">
          <h1 class="chat-greeting-text">{{ t(`chat.greeting.${greetingKey}`) }}</h1>
        </div>
      </Transition>
```

`<Composer ref="composerEl" />` → `<Composer ref="composerEl" :greeting="isGreeting" />`

- [ ] **Step 5: Composer.vue 加 chips**

script 加：

```ts
const props = defineProps<{ greeting?: boolean }>()
```

模板 `.chat-input-inner` div 结束后、根 div 结束前插入（外层 Transition 让 chips 与问候块同步淡出离场；stagger 放弃——spec §3.3 偏差注记）：

```html
    <Transition name="fade">
      <div v-if="props.greeting" class="greeting-chips">
        <button
          v-for="key in ['emptyPrompt1', 'emptyPrompt2', 'emptyPrompt3'] as const"
          :key="key"
          class="greeting-chip"
          type="button"
          @click="fillPrompt(t(`chat.${key}.prompt`))"
        >
          <span class="greeting-chip-title">{{ t(`chat.${key}.title`) }}</span>
          <span class="greeting-chip-desc">{{ t(`chat.${key}.prompt`) }}</span>
        </button>
      </div>
    </Transition>
```

- [ ] **Step 6: chat.css**

替换 `.empty-prompt-chip` 三段规则（约 113-135 行，含注释行 `/* One-click sample prompts… */`）为：

```css
/* -------------------------------------------------------------------------- */
/* Greeting state: the empty session centers greeting + composer + chips       */
/* -------------------------------------------------------------------------- */

.chat--greeting .chat-stream {
  flex: 0 1 auto;
  margin-top: auto;
}
.chat--greeting .chat-input {
  margin-bottom: auto;
  border-top-color: transparent;
  background: transparent;
}
.chat-greeting {
  text-align: center;
  padding-bottom: var(--space-7);
}
.chat-greeting-text {
  margin: 0;
  font-size: var(--text-xl);
  line-height: var(--lh-xl);
  font-weight: 600;
  letter-spacing: -0.01em;
  color: var(--text);
}

/* Two-tier suggestion cards under the composer (greeting state only). */
.greeting-chips {
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: var(--space-3);
  margin-top: var(--space-4);
  max-width: 640px;
  margin-inline: auto;
}
.greeting-chip {
  display: flex;
  flex-direction: column;
  gap: 2px;
  max-width: 100%;
  text-align: left;
  min-height: 44px;
  padding: var(--space-3) var(--space-4);
  border: 1px solid var(--border);
  border-radius: var(--radius-lg);
  background: var(--surface);
  font-family: inherit;
  cursor: pointer;
  transition:
    background var(--dur-fast) var(--ease-out),
    border-color var(--dur-fast) var(--ease-out),
    transform 80ms var(--ease-out);
}
.greeting-chip:hover {
  background: var(--surface-hover);
  border-color: color-mix(in srgb, var(--brand) 45%, var(--border));
}
.greeting-chip:focus-visible {
  outline: 2px solid var(--focus);
  outline-offset: 2px;
}
.greeting-chip:active {
  background: var(--surface-active);
  transform: scale(0.98);
}
.greeting-chip-title {
  font-size: var(--text-sm);
  font-weight: 600;
  color: var(--text);
}
.greeting-chip-desc {
  font-size: var(--text-xs);
  color: var(--muted);
}
```

- [ ] **Step 7: 跑测试 + 全量确认** — `bun run test`（locales.test 树比较同时验证双语一致）→ 全绿
- [ ] **Step 8: Commit**

```bash
git add web/src/components/ChatPanel.vue web/src/components/composer/Composer.vue \
  web/src/components/ui/WEmptyState.vue \
  web/src/i18n/locales/en/chat.ts web/src/i18n/locales/zh-CN/chat.ts \
  web/src/styles/chat.css web/src/components/ChatPanel.test.ts
git commit -m "web: centered time-of-day greeting start screen with two-tier prompt chips"
```

---

### Task 3: FLIP 转场（composer 沉底/回升）

**Files:**
- Modify: `web/src/components/ChatPanel.vue`（watch FLIP + liveness Transition）
- Modify: `web/src/styles/tokens.css`（--ease-flip）
- Modify: `web/src/styles/composer.css`（基础 .chat-input 加边线过渡）
- Modify: `web/src/styles/overlays.css`（liveness fade 延迟）
- Test: `web/src/components/ChatPanel.flip.test.ts`（新建）

**Interfaces:**
- Consumes: `isGreeting`（Task 2）、`composerEl.$el`（.chat-input 根）
- Produces: 无对外接口（行为）

- [ ] **Step 1: 写失败测试**

```ts
// web/src/components/ChatPanel.flip.test.ts
import { mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import i18n from '../i18n'
import ChatPanel from './ChatPanel.vue'
import { useSessionStore } from '../stores/sessions'

const stubs = { ChatItemView: true }

function mountPanel() {
  return mount(ChatPanel, { global: { plugins: [i18n, createPinia()], stubs } })
}

describe('composer FLIP dock transition', () => {
  beforeEach(() => {
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('skips the FLIP entirely under prefers-reduced-motion', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: true }))
    const wrapper = mountPanel()
    const s = useSessionStore()
    s.activeId = 's1'
    s.items = { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] }
    await new Promise((r) => setTimeout(r, 0))
    const root = wrapper.get('.chat-input').element as HTMLElement
    expect(root.style.transform).toBe('')
  })

  it('compensates the layout jump with an invert transform before play', async () => {
    vi.stubGlobal('matchMedia', vi.fn().mockReturnValue({ matches: false }))
    let calls = 0
    vi.spyOn(Element.prototype, 'getBoundingClientRect').mockImplementation(function () {
      calls++
      return { top: calls === 1 ? 300 : 600, ...({} as DOMRect) } as DOMRect
    })
    const wrapper = mountPanel()
    calls = 0 // mount 后归零：首测调用即 watch 回调里的 First 量测
    const s = useSessionStore()
    s.activeId = 's1'
    s.items = { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] }
    await new Promise((r) => setTimeout(r, 0))
    // happy-dom 的 rAF 是真实 16ms 定时器：断言时双帧尚未 drain，transform
    // 必须停在 Invert 相的补偿值上——这正是被测行为。
    const root = wrapper.get('.chat-input').element as HTMLElement
    expect(root.style.transform).toBe('translateY(-300px)')
    expect(root.style.transition).toContain('none')
  })
})
```

- [ ] **Step 2: 确认失败** — `bun run test src/components/ChatPanel.flip.test.ts` → FAIL
- [ ] **Step 3: tokens.css 加曲线**（motion 段 `--ease-spring` 行后）

```css
  --ease-flip: cubic-bezier(0.32, 0.72, 0, 1);
```

- [ ] **Step 4: ChatPanel.vue 加 FLIP 编排**

script（`isGreeting` 之后）：

```ts
import { nextTick } from 'vue' // 并入现有 vue import

// Greeting→conversation is a layout-state transition (auto margins are not
// interpolable), so the position change is compensated with a FLIP: measure
// the composer before the re-render (pre-flush watcher), after nextTick the
// class has re-laid it out, then play the delta as a transform. Runs in both
// directions; mount never animates (no immediate flag).
const prefersReducedMotion = () =>
  typeof window !== 'undefined' && window.matchMedia?.('(prefers-reduced-motion: reduce)').matches

let flipFallbackTimer: number | undefined

watch(isGreeting, async () => {
  const root = composerEl.value?.$el as HTMLElement | undefined
  if (!root || prefersReducedMotion()) return
  const first = root.getBoundingClientRect().top
  await nextTick()
  const last = root.getBoundingClientRect().top
  const dy = first - last
  if (Math.abs(dy) < 1) return
  root.style.transition = 'none'
  root.style.transform = `translateY(${dy}px)`
  requestAnimationFrame(() =>
    requestAnimationFrame(() => {
      const clear = () => {
        root.style.transition = ''
        root.style.transform = ''
        root.removeEventListener('transitionend', clear)
      }
      root.addEventListener('transitionend', clear)
      window.clearTimeout(flipFallbackTimer)
      flipFallbackTimer = window.setTimeout(clear, 400)
      root.style.transition =
        'transform 320ms var(--ease-flip), border-top-color 180ms var(--ease-in-out) 140ms'
      root.style.transform = ''
    }),
  )
})

onBeforeUnmount(() => window.clearTimeout(flipFallbackTimer))
```

模板：stream-liveness 条外包 Transition：

```html
    <Transition name="fade">
      <div v-if="running" class="stream-liveness" role="status">
        …原内容不变…
      </div>
    </Transition>
```

- [ ] **Step 5: overlays.css**（.stream-liveness 规则后加）+ composer.css 基础规则

composer.css 的基础 `.chat-input` 规则（4-9 行）追加一行（两态共有的边线过渡——离开问候态时类被移除、基础规则必须还挂着 transition，否则首向现线失效；FLIP 播放期的 inline transition 已并列包含它）：

```css
  transition: border-top-color 180ms var(--ease-in-out) 140ms;
```

overlays.css：

```css
/* The strip appears only after the composer's FLIP has essentially docked;
   otherwise both animate through each other on the first send. */
.stream-liveness.fade-enter-active {
  transition-delay: 200ms;
}
```

- [ ] **Step 6: 跑测试** — `bun run test src/components/ChatPanel.flip.test.ts` → PASS；再 `bun run test` 全量
- [ ] **Step 7: Commit**

```bash
git add web/src/components/ChatPanel.vue web/src/components/ChatPanel.flip.test.ts \
  web/src/styles/tokens.css web/src/styles/composer.css web/src/styles/overlays.css
git commit -m "web: FLIP dock transition for the composer with reduced-motion guard"
```

---

### Task 4: 用户消息 msg-send 入场（fresh 标记法）

**Files:**
- Modify: `web/src/types.ts`（user 变体加 `fresh?: boolean`）
- Modify: `web/src/stores/sessions.ts`（所有 live push `kind: 'user'` 字面量加 `fresh: true`）
- Modify: `web/src/components/ChatItemView.vue`（.message--new 绑定）
- Modify: `web/src/styles/motion.css`（msg-send）
- Test: `web/src/components/ChatItemView.test.ts`（新建）

**Interfaces:**
- Produces: ChatItem user 变体 `fresh?: boolean`；`.message--new` 类契约

- [ ] **Step 1: 枚举 push 站点** — `grep -n "kind: 'user'" web/src -r`（已知：sessions.ts send 乐观入列 ~353-358、agent 命令 ~421、/goal ~488；以 grep 实时结果为准，全部加 `fresh: true`）

- [ ] **Step 2: 写失败测试**

```ts
// web/src/components/ChatItemView.test.ts
import { mount } from '@vue/test-utils'
import { createPinia } from 'pinia'
import { describe, expect, it } from 'vitest'
import i18n from '../i18n'
import ChatItemView from './ChatItemView.vue'
import type { ChatItem } from '../types'

function mountItem(item: ChatItem) {
  return mount(ChatItemView, {
    props: { item },
    global: { plugins: [i18n, createPinia()] },
  })
}

const user = (fresh?: boolean): ChatItem =>
  ({ id: 'u1', kind: 'user', content: 'hi', ...(fresh ? { fresh } : {}) }) as ChatItem

describe('user message entrance marking', () => {
  it('marks fresh live-pushed user bubbles', () => {
    const w = mountItem(user(true))
    expect(w.get('.message').classes()).toContain('message--new')
  })
  it('does not mark history-loaded user bubbles', () => {
    const w = mountItem(user())
    expect(w.get('.message').classes()).not.toContain('message--new')
  })
})
```

- [ ] **Step 3: 确认失败** — `bun run test src/components/ChatItemView.test.ts` → FAIL
- [ ] **Step 4: 实现**

types.ts user 变体：

```ts
  | { id: string; kind: 'user'; content: string; attachments?: Attachment[]; /** Set on live sends (this process) — drives the msg-send entrance. */ fresh?: boolean }
```

sessions.ts：grep 到的每个 `kind: 'user'` 对象字面量加 `fresh: true`。
ChatItemView.vue `.message` 根：

```html
  <div v-if="isText" class="message" :class="[isUser ? 'user' : 'assistant', { 'message--new': isUser && item.kind === 'user' && item.fresh }]">
```

motion.css 文件尾追加：

```css
/* User send "promotion": a fresh live-pushed user bubble rises from the
 * composer's edge — the counterpart motion to the FLIP dock-down. History
 * loads keep the shared msg-rise (they rise as one block). */
@keyframes msg-send {
  from {
    opacity: 0;
    transform: translateY(6px);
  }
  to {
    opacity: 1;
    transform: translateY(0);
  }
}
.message.message--new {
  animation: msg-send 240ms var(--ease-out) both;
}
```

- [ ] **Step 5: 跑测试 + 全量** → PASS
- [ ] **Step 6: Commit**

```bash
git add web/src/types.ts web/src/stores/sessions.ts web/src/components/ChatItemView.vue \
  web/src/styles/motion.css web/src/components/ChatItemView.test.ts
git commit -m "web: msg-send entrance for live user bubbles via fresh flag"
```

---

### Task 5: 首个输出前的 shimmer 占位

**Files:**
- Modify: `web/src/components/ChatPanel.vue`（awaitingFirstOutput + 模板）
- Modify: `web/src/styles/chat.css`（shimmer）
- Test: `web/src/components/ChatPanel.test.ts`（扩充）

**Interfaces:**
- Produces: `.msg-shimmer-row`（greeting/对话态通用的等待首块占位；不触碰 items 生命周期——answer 气泡仍是首 delta 懒创建）

- [ ] **Step 1: 写失败测试（追加）**

```ts
it('shows the first-output shimmer while a run has produced nothing yet', async () => {
  const wrapper = mountPanel()
  const s = useSessionStore()
  s.activeId = 's1'
  s.items = { s1: [{ id: 'u1', kind: 'user', content: 'hi' }] }
  s.status = { s1: 'running' }
  await wrapper.vm.$nextTick()
  expect(wrapper.find('.msg-shimmer-row').exists()).toBe(true)
})

it('hides the shimmer once any output block exists', async () => {
  const wrapper = mountPanel()
  const s = useSessionStore()
  s.activeId = 's1'
  s.items = {
    s1: [
      { id: 'u1', kind: 'user', content: 'hi' },
      { id: 'a1', kind: 'assistant', content: 'h', streaming: true },
    ],
  }
  s.status = { s1: 'running' }
  await wrapper.vm.$nextTick()
  expect(wrapper.find('.msg-shimmer-row').exists()).toBe(false)
})
```

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现** — ChatPanel script：

```ts
// A run is live but no block has rendered yet (reasoning/answer/tool all
// lazy-created): show a shimmer placeholder row instead of an empty beat.
const awaitingFirstOutput = computed(() => {
  if (store.activeStatus !== 'running') return false
  const items = store.activeItems
  return items.length > 0 && items[items.length - 1].kind === 'user'
})
```

模板：`</Transition>`（error-banner 块后）、greeting Transition 之前插入：

```html
      <div v-if="awaitingFirstOutput" class="message assistant msg-shimmer-row" aria-hidden="true">
        <div class="message-bubble">
          <div class="msg-shimmer-bars"></div>
        </div>
      </div>
```

（不放 Avatar，避免与真实气泡抢视觉锚点；`.message` 类让它自带 msg-rise 入场。）

chat.css 追加：

```css
/* First-output shimmer: run is live, no block rendered yet (spec §3.C4). */
.msg-shimmer-bars {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
  min-width: 180px;
  padding: 2px 0;
}
.msg-shimmer-bars::before,
.msg-shimmer-bars::after {
  content: '';
  height: var(--lh-chat);
  border-radius: var(--radius);
  background: linear-gradient(
    90deg,
    var(--surface-active) 25%,
    var(--surface-hover) 50%,
    var(--surface-active) 75%
  );
  background-size: 200% 100%;
  animation: msg-shimmer 1400ms linear infinite;
}
.msg-shimmer-bars::before {
  width: 62%;
}
.msg-shimmer-bars::after {
  width: 94%;
}
@keyframes msg-shimmer {
  from {
    background-position: 200% 0;
  }
  to {
    background-position: -200% 0;
  }
}
```

- [ ] **Step 4: 跑测试 + 全量** → PASS
- [ ] **Step 5: Commit**

```bash
git add web/src/components/ChatPanel.vue web/src/styles/chat.css web/src/components/ChatPanel.test.ts
git commit -m "web: shimmer placeholder while the first output block is pending"
```

---

### Task 6: reasoning 思考秒数

**Files:**
- Modify: `web/src/types.ts`（reasoning 加 `startedAt?/durationMs?`）
- Modify: `web/src/stores/sessions/items.ts`（appendReasoning 记 startedAt、closeReasoning 补 durationMs）
- Modify: `web/src/components/ChatItemView.vue`（live 秒表 + meta 切换）
- Modify: `web/src/i18n/locales/{en,zh-CN}/chat.ts`
- Test: `web/src/stores/sessions.items.test.ts`（新建）

**Interfaces:**
- Produces: reasoning 变体 `startedAt?: number; durationMs?: number`（ChatItemView 消费）

- [ ] **Step 1: 写失败测试**

```ts
// web/src/stores/sessions.items.test.ts
import { ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { createItemMutations } from './items'
import type { ChatItem } from '../../types'

function setup() {
  const items = ref<Record<string, ChatItem[]>>({})
  const state = {
    items,
    streamingId: ref<Record<string, string | null>>({}),
    reasoningId: ref<Record<string, string | null>>({}),
    goalNoteId: ref<Record<string, string | null>>({}),
    runId: ref<Record<string, string>>({}),
  }
  return createItemMutations(state)
}

describe('reasoning duration', () => {
  it('records startedAt on creation and durationMs on close', () => {
    const now = 1_700_000_000_000
    let t = now
    vi.spyOn(Date, 'now').mockImplementation(() => t)
    const m = setup()
    m.appendReasoning('s1', 'think')
    m.closeReasoning('s1')
    const r = m.list('s1')[0] as Extract<ChatItem, { kind: 'reasoning' }>
    expect(r.startedAt).toBe(now)
    m.appendReasoning('s1', 'more')
    t = now + 2500 // 推进时间在 append 与 close 之间，差值路径才被真实覆盖
    m.closeReasoning('s1')
    const r2 = m.list('s1')[1] as Extract<ChatItem, { kind: 'reasoning' }>
    expect(r2.durationMs).toBe(2500)
    expect(r2.streaming).toBe(false)
  })
})
```

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现**

types.ts reasoning 变体加字段：

```ts
  | {
      id: string
      kind: 'reasoning'
      content: string
      /** True while the model is still streaming this thinking segment. */
      streaming: boolean
      /** Unix ms when the segment started (elapsed/duration labels). */
      startedAt?: number
      /** Total thinking time, set when the segment closes. */
      durationMs?: number
    }
```

items.ts：`appendReasoning` 创建行（111）改为：

```ts
    push(sid, { id, kind: 'reasoning', content: text, streaming: true, startedAt: Date.now() })
```

`closeReasoning`（117-128）冻结分支改为：

```ts
    if (idx >= 0 && ls[idx].kind === 'reasoning') {
      const cur = ls[idx] as Extract<ChatItem, { kind: 'reasoning' }>
      const next = [...ls]
      next[idx] = {
        ...cur,
        streaming: false,
        durationMs: cur.startedAt !== undefined ? Math.max(0, Date.now() - cur.startedAt) : undefined,
      }
      items.value[sid] = next
    }
```

ChatItemView.vue script：

```ts
const reasoningDurationSec = computed(() => {
  const it = props.item
  if (it.kind !== 'reasoning' || it.durationMs === undefined) return null
  return Math.max(1, Math.round(it.durationMs / 1000))
})

// Live "Thinking · Ns" ticker (livenessTimer precedent in ChatPanel).
const reasoningElapsed = ref(0)
let reasoningTimer: number | undefined
watch(
  reasoningStreaming,
  (live) => {
    window.clearInterval(reasoningTimer)
    if (live) {
      const start = props.item.kind === 'reasoning' ? props.item.startedAt : undefined
      reasoningElapsed.value = start ? Math.max(0, Math.round((Date.now() - start) / 1000)) : 0
      reasoningTimer = window.setInterval(() => {
        reasoningElapsed.value++
      }, 1000)
    }
  },
  { immediate: true },
)
onBeforeUnmount(() => window.clearInterval(reasoningTimer))
```

（`onBeforeUnmount` 并入现有 vue import。）reasoning 卡 `:meta` 改为：

```html
    :meta="reasoningStreaming
      ? t('chat.thinkingSeconds', { count: reasoningElapsed })
      : reasoningDurationSec !== null
        ? t('chat.thoughtFor', { count: reasoningDurationSec })
        : t('chat.charCount', { count: reasoningContent.length })"
```

i18n：en 加 `thinkingSeconds: 'Thinking · {count}s',`、`thoughtFor: 'Thought for {count}s',`；zh 加 `thinkingSeconds: '思考中 · {count}s',`、`thoughtFor: '思考了 {count}s',`（放 thinkingDone 行旁）。

- [ ] **Step 4: 跑测试 + 全量** → PASS
- [ ] **Step 5: Commit**

```bash
git add web/src/types.ts web/src/stores/sessions/items.ts web/src/components/ChatItemView.vue \
  web/src/i18n/locales/en/chat.ts web/src/i18n/locales/zh-CN/chat.ts \
  web/src/stores/sessions.items.test.ts
git commit -m "web: live and retrospective thinking durations on reasoning cards"
```

---

### Task 7: markdownToPlain

**Files:**
- Modify: `web/src/lib/markdown.ts`
- Test: `web/src/lib/markdown.test.ts`（新建）

**Interfaces:**
- Produces: `markdownToPlain(src: string): string`（Task 9 消费）

- [ ] **Step 1: 写失败测试**

```ts
// web/src/lib/markdown.test.ts
import { describe, expect, it } from 'vitest'
import { markdownToPlain } from './markdown'

describe('markdownToPlain', () => {
  it('passes plain text through', () => {
    expect(markdownToPlain('just text')).toBe('just text')
  })
  it('drops heading and emphasis markers', () => {
    expect(markdownToPlain('## Title\n\n**bold** and *em* and ~~del~~')).toBe(
      'Title\n\nbold and em and del',
    )
  })
  it('renders links as label (href)', () => {
    expect(markdownToPlain('[docs](https://x.dev)')).toBe('docs (https://x.dev)')
  })
  it('keeps code blocks verbatim without fences', () => {
    expect(markdownToPlain('```js\nconst a = 1\n```')).toBe('const a = 1')
  })
  it('prefixes list items and indents nesting', () => {
    expect(markdownToPlain('- a\n- b')).toBe('- a\n- b')
    expect(markdownToPlain('1. a\n2. b')).toBe('1. a\n2. b')
  })
  it('joins table cells visibly', () => {
    expect(markdownToPlain('| a | b |\n| --- | --- |\n| 1 | 2 |')).toBe('a ｜ b\n1 ｜ 2')
  })
  it('prefixes blockquote lines', () => {
    expect(markdownToPlain('> quoted')).toBe('> quoted')
  })
  it('keeps hr and strips inline html', () => {
    expect(markdownToPlain('a\n\n---\n\n<b>bold</b>')).toBe('a\n\n---\n\nbold')
  })
  it('handles empty input', () => {
    expect(markdownToPlain('')).toBe('')
  })
})
```

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现** — markdown.ts 追加：

```ts
import type { Token, Tokens } from 'marked'

/**
 * Markdown source → plain text ("copy as plain text"): marked-lexer walk,
 * no DOM dependency. Structural markers survive as their readable forms
 * (lists keep bullets, links become "label (href)", tables join cells).
 */
export function markdownToPlain(src: string): string {
  return blocks(marked.lexer(src)).replace(/\n{3,}/g, '\n\n').trim()
}

function blocks(tokens: Token[]): string {
  const parts: string[] = []
  for (const tk of tokens) {
    switch (tk.type) {
      case 'space':
        continue
      case 'heading':
      case 'paragraph':
        parts.push(inline((tk as Tokens.Heading).tokens ?? []))
        break
      case 'code':
        parts.push((tk as Tokens.Code).text)
        break
      case 'blockquote':
        parts.push(
          blocks((tk as Tokens.Blockquote).tokens ?? [])
            .split('\n')
            .map((l) => `> ${l}`)
            .join('\n'),
        )
        break
      case 'list':
        parts.push(
          ((tk as Tokens.List).items ?? []).map((it, i) => listItem(it, i, 0, (tk as Tokens.List).ordered)).join('\n'),
        )
        break
      case 'table': {
        const tb = tk as Tokens.Table
        const rows = [tb.header, ...tb.rows].map((cells) =>
          cells.map((c) => inline(c.tokens ?? [])).join(' ｜ '),
        )
        parts.push(rows.join('\n'))
        break
      }
      case 'hr':
        parts.push('---')
        break
      case 'html':
        parts.push((tk as Tokens.HTML).text.replace(/<[^>]*>/g, ''))
        break
      default:
        if ('text' in tk) parts.push(String((tk as { text: string }).text))
    }
  }
  return parts.filter((p) => p !== '').join('\n\n')
}

function listItem(it: Tokens.ListItem, index: number, depth: number, ordered: boolean): string {
  const pad = '  '.repeat(depth)
  const marker = it.task ? `- [${it.checked ? 'x' : ' '}] ` : `${ordered ? `${index + 1}.` : '-'} `
  return blocks((it.tokens ?? []) as Token[])
    .split('\n')
    .map((l, i) => (i === 0 ? `${pad}${marker}${l}` : `${pad}  ${l}`))
    .join('\n')
}

function inline(tokens: Token[]): string {
  let out = ''
  for (const tk of tokens) {
    switch (tk.type) {
      case 'escape':
      case 'text': {
        const t = tk as Tokens.Text
        out += t.tokens?.length ? inline(t.tokens) : t.text
        break
      }
      case 'strong':
      case 'em':
      case 'del':
        out += inline((tk as Tokens.Strong).tokens ?? [])
        break
      case 'link': {
        const l = tk as Tokens.Link
        const label = inline(l.tokens ?? [])
        out += label === l.href ? label : `${label} (${l.href})`
        break
      }
      case 'image':
        out += (tk as Tokens.Image).text || ''
        break
      case 'codespan':
        out += (tk as Tokens.Codespan).text
        break
      case 'br':
        out += '\n'
        break
      case 'html':
        out += (tk as Tokens.HTML).text.replace(/<[^>]*>/g, '')
        break
      default:
        if ('raw' in tk) out += String((tk as { raw: string }).raw)
        else if ('text' in tk) out += String((tk as { text: string }).text)
    }
  }
  return out
}
```

- [ ] **Step 4: 跑测试** → 全绿（marked v18 实装验证过：`Tokens.ListItem` 无 `ordered` 字段（在 List 上，故参数传入）、`<b>bold</b>` 走 paragraph 内 inline html token——以上实现即修正版，10/10 断言已实测通过；**断言即契约，勿反向改断言迁就实现**）
- [ ] **Step 5: Commit**

```bash
git add web/src/lib/markdown.ts web/src/lib/markdown.test.ts
git commit -m "web: markdownToPlain lexer walk for copy-as-plain-text"
```

---

### Task 8: WDropdown 组件

**Files:**
- Create: `web/src/components/ui/WDropdown.vue`
- Test: `web/src/components/ui/WDropdown.test.ts`

**Interfaces:**
- Produces: `<WDropdown v-model="open">` + `#trigger`/`#menu` 插槽；ARIA menu 键盘契约（Task 9 消费）

- [ ] **Step 1: 写失败测试**

```ts
// web/src/components/ui/WDropdown.test.ts
import { mount } from '@vue/test-utils'
import { describe, expect, it } from 'vitest'
import WDropdown from './WDropdown.vue'

const mountDd = () =>
  mount(WDropdown, {
    props: { modelValue: false },
    slots: {
      trigger: '<button class="trig">t</button>',
      menu: '<button role="menuitem" class="mi">one</button><button role="menuitem" class="mi2">two</button>',
    },
    global: { stubs: { teleport: true } },
  })

describe('WDropdown', () => {
  it('opens on trigger click and syncs aria-expanded', async () => {
    const w = mountDd()
    expect(w.find('.w-dropdown-menu').exists()).toBe(false)
    await w.get('.trig').trigger('click')
    expect(w.emitted('update:modelValue')?.[0]).toEqual([true])
  })
  it('renders the menu when open with role=menu', () => {
    const w = mount(WDropdown, {
      props: { modelValue: true },
      slots: {
        trigger: '<button class="trig">t</button>',
        menu: '<button role="menuitem">one</button>',
      },
      global: { stubs: { teleport: true } },
    })
    expect(w.find('.w-dropdown-menu[role="menu"]').exists()).toBe(true)
  })
  it('closes on Escape and returns focus to the trigger', async () => {
    const w = mount(WDropdown, {
      props: { modelValue: true },
      slots: {
        trigger: '<button class="trig">t</button>',
        menu: '<button role="menuitem">one</button>',
      },
    })
    await w.get('.trig').trigger('click')
    // 菜单 Teleport 到 body——从 document 查（WModal.test.ts 先例），wrapper 查不到
    const menu = document.body.querySelector('.w-dropdown-menu')!
    expect(menu).toBeTruthy()
    menu.dispatchEvent(new KeyboardEvent('keydown', { key: 'Escape', bubbles: true }))
    await w.vm.$nextTick()
    expect(w.emitted('update:modelValue')?.at(-1)).toEqual([false])
    w.unmount()
  })
})
```

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现**

```vue
<!-- web/src/components/ui/WDropdown.vue -->
<script setup lang="ts">
import { nextTick, onBeforeUnmount, ref, watch } from 'vue'

// Minimal ARIA menu-button dropdown for split actions. The menu teleports
// to body with fixed positioning (chat action bars sit near the viewport
// bottom, so it opens upward by default and flips only when there is no
// room above but room below). Opening is click/keyboard only — never hover.
const open = defineModel<boolean>({ default: false })

const triggerEl = ref<HTMLElement | null>(null)
const menuEl = ref<HTMLElement | null>(null)
const menuStyle = ref<Record<string, string>>({})

function place() {
  const el = triggerEl.value
  if (!el) return
  const r = el.getBoundingClientRect()
  const menuH = menuEl.value?.offsetHeight || 160
  const roomAbove = r.top > menuH + 8
  const roomBelow = r.bottom + menuH + 8 <= window.innerHeight
  const up = roomAbove || !roomBelow
  menuStyle.value = {
    position: 'fixed',
    left: `${Math.min(r.left, window.innerWidth - 200)}px`,
    ...(up ? { bottom: `${window.innerHeight - r.top + 8}px` } : { top: `${r.bottom + 8}px` }),
  }
}

function focusFirst() {
  const first = menuEl.value?.querySelector<HTMLElement>('[role="menuitem"]')
  first?.focus()
}

function onTriggerKeydown(e: KeyboardEvent) {
  if (e.key === 'ArrowDown' && !open.value) {
    e.preventDefault()
    open.value = true
    void nextTick(focusFirst)
  }
}

function onMenuKeydown(e: KeyboardEvent) {
  const items = Array.from(menuEl.value?.querySelectorAll<HTMLElement>('[role="menuitem"]') ?? [])
  const idx = items.indexOf(document.activeElement as HTMLElement)
  switch (e.key) {
    case 'Escape':
      e.preventDefault()
      open.value = false
      triggerEl.value?.focus()
      return
    case 'Tab':
      open.value = false
      return
    case 'ArrowDown':
      e.preventDefault()
      items[(idx + 1 + items.length) % items.length]?.focus()
      return
    case 'ArrowUp':
      e.preventDefault()
      items[(idx - 1 + items.length) % items.length]?.focus()
      return
    case 'Home':
      e.preventDefault()
      items[0]?.focus()
      return
    case 'End':
      e.preventDefault()
      items[items.length - 1]?.focus()
  }
}

function onDocPointerdown(e: PointerEvent) {
  const t = e.target as Node
  if (menuEl.value?.contains(t) || triggerEl.value?.contains(t)) return
  open.value = false
}

watch(open, async (on) => {
  if (on) {
    await nextTick()
    place()
    focusFirst()
    document.addEventListener('pointerdown', onDocPointerdown)
    window.addEventListener('scroll', closeOnExternal, true)
    window.addEventListener('resize', closeOnExternal)
  } else {
    document.removeEventListener('pointerdown', onDocPointerdown)
    window.removeEventListener('scroll', closeOnExternal, true)
    window.removeEventListener('resize', closeOnExternal)
  }
})

function closeOnExternal() {
  open.value = false
}

onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerdown)
  window.removeEventListener('scroll', closeOnExternal, true)
  window.removeEventListener('resize', closeOnExternal)
})
</script>

<template>
  <span
    ref="triggerEl"
    class="w-dropdown-trigger"
    :aria-haspopup="'menu'"
    :aria-expanded="open"
    @click="open = !open"
    @keydown="onTriggerKeydown"
  >
    <slot name="trigger" :toggle="() => (open = !open)" />
  </span>
  <Teleport to="body">
    <div v-if="open" ref="menuEl" class="w-dropdown-menu" role="menu" :style="menuStyle" @keydown="onMenuKeydown">
      <slot name="menu" />
    </div>
  </Teleport>
</template>
```

（打开路径在 span 上统一 toggle：trigger 插槽内的按钮**不要**再自绑 click——冒泡到 span 会双触发翻回 false。菜单在 Teleport 外不冒泡回 span，点外关闭由 onDocPointerdown 负责。）

样式加进 `web/src/styles/overlays.css` 尾部：

```css
/* WDropdown (split-button menus) */
.w-dropdown-menu {
  min-width: 190px;
  padding: var(--space-2);
  background: var(--surface);
  border: 1px solid var(--border-strong);
  border-radius: var(--radius-lg);
  box-shadow: var(--shadow-lg);
  z-index: var(--z-popover);
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.w-dropdown-menu [role='menuitem'] {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  height: 32px;
  padding: 0 var(--space-3);
  border: none;
  border-radius: var(--radius-sm);
  background: none;
  font-family: inherit;
  font-size: var(--text-sm);
  color: var(--text-2);
  cursor: pointer;
}
.w-dropdown-menu [role='menuitem']:hover,
.w-dropdown-menu [role='menuitem']:focus-visible {
  background: var(--surface-hover);
  color: var(--text);
  outline: none;
}
```

- [ ] **Step 4: 跑测试** → PASS
- [ ] **Step 5: Commit**

```bash
git add web/src/components/ui/WDropdown.vue web/src/components/ui/WDropdown.test.ts web/src/styles/overlays.css
git commit -m "web: WDropdown ARIA menu component for split-button actions"
```

---

### Task 9: 气泡级 split button 双格式复制

**Files:**
- Modify: `web/src/components/ChatItemView.vue`（bubble-actions 重构）
- Modify: `web/src/i18n/locales/{en,zh-CN}/chat.ts`
- Modify: `web/src/styles/overlays.css`（is-menu-open 常显）
- Test: `web/src/components/ChatItemView.test.ts`（扩充）

**Interfaces:**
- Consumes: `markdownToPlain`（Task 7）、`WDropdown`（Task 8）
- Produces: `.bubble-actions.is-menu-open` 契约（菜单开着时动作条不消失）

- [ ] **Step 1: 写失败测试（追加）**

```ts
const assistant = (content = '**hi** _world_'): ChatItem =>
  ({ id: 'a1', kind: 'assistant', content, streaming: false }) as ChatItem

function mountAssistant(content?: string) {
  return mount(ChatItemView, {
    props: { item: assistant(content) },
    global: { plugins: [i18n, createPinia()] },
  })
}

describe('bubble copy split button', () => {
  it('main button copies the markdown source', async () => {
    const write = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText: write } })
    const w = mountAssistant()
    await w.get('.bubble-action.copy-main').trigger('click')
    expect(write).toHaveBeenCalledWith('**hi** _world_')
  })

  it('dropdown plain-text item copies stripped text', async () => {
    const write = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText: write } })
    const w = mountAssistant()
    try {
      await w.get('.bubble-action.copy-side').trigger('click') // span 上 toggle 打开
      // 菜单 Teleport 到 body——从 document 查（wrapper 查不到）
      const items = Array.from(document.body.querySelectorAll('[role="menuitem"]'))
      expect(items).toHaveLength(2)
      await items[1].dispatchEvent(new MouseEvent('click', { bubbles: true }))
      await w.vm.$nextTick()
      expect(write).toHaveBeenCalledWith('hi world')
    } finally {
      w.unmount()
    }
  })

  it('pins the action bar while the menu is open', async () => {
    const w = mountAssistant()
    expect(w.get('.bubble-actions').classes()).not.toContain('is-menu-open')
    await w.get('.bubble-action.copy-side').trigger('click')
    expect(w.get('.bubble-actions').classes()).toContain('is-menu-open')
  })
})
```

（断言按位置取第二项，与 locale 无关。）

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现** — i18n（en/zh 同步）：`copyMenu: 'Copy options',` / `copyMenu: '复制选项',`、`copyPlain: 'Copy as plain text',` / `copyPlain: '仅复制纯文字',`、`copiedMarkdown: 'Markdown copied',` / `copiedMarkdown: '已复制 Markdown',`、`copiedPlain: 'Plain text copied',` / `copiedPlain: '已复制纯文字',`。

ChatItemView.vue：lucide import 加 `ChevronDown`；import WDropdown 与 `markdownToPlain`。script：

```ts
const copied = ref<'md' | 'plain' | 'debug' | ''>('')
const copyMenuOpen = ref(false)

function flash(kind: 'md' | 'plain' | 'debug') {
  copied.value = kind
  window.setTimeout(() => {
    copied.value = ''
  }, 1800)
}

// （copyText 改 kind 类型含 'plain'；copyMarkdown 不变；新增：）
function copyPlain() {
  copyText(markdownToPlain(textContent.value), 'plain')
}

const copiedAnnouncement = computed(() =>
  copied.value === 'md' ? t('chat.copiedMarkdown') : copied.value === 'plain' ? t('chat.copiedPlain') : '',
)
```

模板 `.bubble-actions` 块整体替换为：

```html
      <div v-if="!isUser && !streaming" class="bubble-actions" :class="{ 'is-menu-open': copyMenuOpen }">
        <button
          class="bubble-action copy-main"
          :title="t('chat.copyMarkdown')"
          :aria-label="t('chat.copyMarkdown')"
          @click="copyMarkdown()"
        >
          <Check v-if="copied === 'md'" :size="12" />
          <Copy v-else :size="12" />
        </button>
        <WDropdown v-model="copyMenuOpen">
          <template #trigger>
            <button
              class="bubble-action copy-side"
              :title="t('chat.copyMenu')"
              :aria-label="t('chat.copyMenu')"
            >
              <ChevronDown :size="12" />
            </button>
          </template>
          <template #menu>
            <button role="menuitem" type="button" @click="copyMarkdown(); copyMenuOpen = false">
              <Check :size="12" /> {{ t('chat.copyMarkdown') }}
            </button>
            <button role="menuitem" type="button" @click="copyPlain(); copyMenuOpen = false">
              <Copy :size="12" /> {{ t('chat.copyPlain') }}
            </button>
          </template>
        </WDropdown>
        <button
          v-if="serverSettings.debugLog"
          class="bubble-action debug"
          :title="t('chat.copyDebug')"
          :aria-label="t('chat.copyDebug')"
          @click="copyDebugInfo()"
        >
          <Check v-if="copied === 'debug'" :size="12" />
          <Bug v-else :size="13" />
        </button>
        <span class="sr-only" role="status" aria-live="polite">{{ copiedAnnouncement }}</span>
      </div>
```

overlays.css 显隐条件追加一条（与 hover/focus-within 并列）：

```css
.bubble-actions.is-menu-open {
  opacity: 1;
}
```

- [ ] **Step 4: 跑测试 + 全量** → PASS
- [ ] **Step 5: Commit**

```bash
git add web/src/components/ChatItemView.vue web/src/components/ChatItemView.test.ts \
  web/src/i18n/locales/en/chat.ts web/src/i18n/locales/zh-CN/chat.ts web/src/styles/overlays.css
git commit -m "web: split-button copy — markdown source by default, plain text via side menu"
```

---

### Task 10: 代码块头部条 + 复制（三消费点）

**Files:**
- Create: `web/src/lib/codeBlocks.ts`
- Modify: `web/src/components/ChatItemView.vue`、`web/src/components/FilePreview.vue`、`web/src/components/InterruptModal.vue`
- Modify: `web/src/styles/chat.css`
- Modify: `web/src/i18n/locales/{en,zh-CN}/chat.ts`
- Test: `web/src/lib/codeBlocks.test.ts`

**Interfaces:**
- Produces: `decorateCodeBlocks(root: HTMLElement, labels: { copy: string; copied: string }): void`（DOM 后处理 + 每容器一次事件委托）

- [ ] **Step 1: 写失败测试**

```ts
// web/src/lib/codeBlocks.test.ts
import { describe, expect, it, vi } from 'vitest'
import { renderMarkdown } from './markdown'
import { decorateCodeBlocks } from './codeBlocks'

const labels = { copy: 'Copy code', copied: 'Code copied' }

function makeRoot(md: string) {
  const root = document.createElement('div')
  root.innerHTML = renderMarkdown(md)
  return root
}

describe('decorateCodeBlocks', () => {
  it('wraps fenced blocks with a language header and copy button', () => {
    const root = makeRoot('```go\nfmt.Println(1)\n```')
    decorateCodeBlocks(root, labels)
    expect(root.querySelector('.code-block .code-head .code-lang')?.textContent).toBe('go')
    expect(root.querySelector('.code-block .code-copy')).toBeTruthy()
  })
  it('labels untagged blocks as text', () => {
    const root = makeRoot('```\nplain\n```')
    decorateCodeBlocks(root, labels)
    expect(root.querySelector('.code-lang')?.textContent).toBe('text')
  })
  it('is idempotent', () => {
    const root = makeRoot('```go\nx\n```')
    decorateCodeBlocks(root, labels)
    decorateCodeBlocks(root, labels)
    expect(root.querySelectorAll('.code-block')).toHaveLength(1)
  })
  it('delegated click copies the raw code', async () => {
    const write = vi.fn().mockResolvedValue(undefined)
    Object.assign(navigator, { clipboard: { writeText: write } })
    const root = makeRoot('```go\nfmt.Println(1)\n```')
    decorateCodeBlocks(root, labels)
    root.querySelector('.code-copy')!.dispatchEvent(new MouseEvent('click', { bubbles: true }))
    await Promise.resolve()
    expect(write).toHaveBeenCalledWith('fmt.Println(1)\n')
  })
})
```

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现**

```ts
// web/src/lib/codeBlocks.ts
export interface CodeCopyLabels {
  copy: string
  copied: string
}

const ICONS = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="9" y="9" width="13" height="13" rx="2"/><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"/></svg><svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M20 6 9 17l-5-5"/></svg>`

/**
 * Post-render DOM decoration for fenced code blocks: wraps each <pre> in a
 * .code-block with a language header and a copy button. Runs AFTER
 * sanitization on the live DOM (never inside the sanitized HTML string —
 * v-html replaces it every tick) and binds one delegated click listener per
 * container. Idempotent; call again after content swaps.
 */
export function decorateCodeBlocks(root: HTMLElement, labels: CodeCopyLabels): void {
  if (!root.dataset.codeCopyBound) {
    root.dataset.codeCopyBound = '1'
    root.addEventListener('click', (e) => {
      const btn = (e.target as HTMLElement).closest<HTMLButtonElement>('.code-copy')
      if (!btn) return
      const code = btn.closest('.code-block')?.querySelector('pre code')
      if (!code) return
      void navigator.clipboard.writeText(code.textContent ?? '').then(() => {
        btn.classList.add('copied')
        btn.setAttribute('aria-label', labels.copied)
        window.setTimeout(() => {
          btn.classList.remove('copied')
          btn.setAttribute('aria-label', labels.copy)
        }, 1800)
      })
    })
  }
  for (const pre of Array.from(root.querySelectorAll('pre'))) {
    if (pre.parentElement?.classList.contains('code-block')) continue
    const code = pre.querySelector('code')
    const lang =
      Array.from(code?.classList ?? [])
        .find((c) => c.startsWith('language-'))
        ?.slice('language-'.length) ?? ''
    const wrap = document.createElement('div')
    wrap.className = 'code-block'
    const head = document.createElement('div')
    head.className = 'code-head'
    const label = document.createElement('span')
    label.className = 'code-lang'
    label.textContent = lang || 'text'
    const btn = document.createElement('button')
    btn.type = 'button'
    btn.className = 'code-copy'
    btn.setAttribute('aria-label', labels.copy)
    btn.innerHTML = ICONS
    head.append(label, btn)
    pre.replaceWith(wrap)
    wrap.append(head, pre)
  }
}
```

i18n：en `copyCode: 'Copy code',`、`codeCopied: 'Code copied',`；zh `copyCode: '复制代码',`、`codeCopied: '已复制代码',`。

ChatItemView.vue：`.markdown-body` div 加 `ref="mdBody"`；script：

```ts
import { decorateCodeBlocks } from '../lib/codeBlocks'
const mdBody = ref<HTMLElement | null>(null)

// Decorate after every settled render (mount / content change / stream
// end); never mid-stream — v-html replaces the subtree each tick.
watch(
  [textHtml, streaming],
  async () => {
    if (streaming.value) return
    await nextTick()
    if (mdBody.value) decorateCodeBlocks(mdBody.value, { copy: t('chat.copyCode'), copied: t('chat.codeCopied') })
  },
  { immediate: true, flush: 'post' },
)
```

FilePreview.vue：markdown-body div 加 `ref="mdBody"`；script：

```ts
import { decorateCodeBlocks } from '../lib/codeBlocks'
const mdBody = ref<HTMLElement | null>(null)
watch(
  mdHtml,
  async () => {
    await nextTick()
    if (mdBody.value) decorateCodeBlocks(mdBody.value, { copy: t('chat.copyCode'), copied: t('chat.codeCopied') })
  },
  { immediate: true, flush: 'post' },
)
```

InterruptModal.vue：plan 的 markdown-body div 加 `ref="mdBody"`；同样模式 watch `planHtml`。

chat.css（`.markdown-body pre` 规则后）：

```css
/* Fenced-block chrome: language header + copy (decorated post-render). */
.code-block {
  margin: 0.8em 0;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  overflow: hidden;
}
.code-block pre {
  margin: 0;
  border: none;
  border-radius: 0;
}
.code-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 var(--space-3);
  height: 28px;
  background: var(--surface-2);
  border-bottom: 1px solid var(--border);
}
.code-lang {
  font-family: var(--font-mono);
  font-size: 11px;
  color: var(--muted);
  text-transform: uppercase;
  letter-spacing: 0.06em;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.code-copy {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 24px;
  height: 24px;
  border: none;
  border-radius: var(--radius-sm);
  background: none;
  color: var(--muted);
  cursor: pointer;
}
.code-copy:hover,
.code-copy:focus-visible {
  color: var(--text);
  outline: none;
}
.code-copy svg:last-child {
  display: none;
}
.code-copy.copied svg:first-child {
  display: none;
}
.code-copy.copied svg:last-child {
  display: block;
  color: var(--text);
}
```

- [ ] **Step 4: 跑测试 + 全量** → PASS（`.markdown-body pre` 原 margin 由 .code-block 接管）
- [ ] **Step 5: Commit**

```bash
git add web/src/lib/codeBlocks.ts web/src/lib/codeBlocks.test.ts \
  web/src/components/ChatItemView.vue web/src/components/FilePreview.vue web/src/components/InterruptModal.vue \
  web/src/styles/chat.css web/src/i18n/locales/en/chat.ts web/src/i18n/locales/zh-CN/chat.ts
git commit -m "web: code-block headers with copy buttons across markdown consumers"
```

---

### Task 11: 流式平滑释放（显示层缓冲）

**Files:**
- Create: `web/src/composables/useSmoothStream.ts`
- Modify: `web/src/components/ChatItemView.vue`（display 接线 + render-tick emit）
- Modify: `web/src/components/ChatPanel.vue`（@render-tick → follow()）
- Test: `web/src/composables/useSmoothStream.test.ts`

**Interfaces:**
- Produces: `useSmoothStream({ content, streaming, onTick?, maxDirect? }) → { display: Ref<string> }`；ChatItemView 新 emit `render-tick`
- Consumes: ChatPanel 的 `follow()`（useStickToBottom）
- spec 偏差注记：spec §3.C3 写「streaming:false 后 150ms 内清空」，实现为立即同步 flush——终态与 store 逐字节一致的目标不变，语义更严格。

- [ ] **Step 1: 写失败测试**

```ts
// web/src/composables/useSmoothStream.test.ts
import { ref } from 'vue'
import { describe, expect, it, vi } from 'vitest'
import { useSmoothStream } from './useSmoothStream'

function setup() {
  const content = ref('')
  const streaming = ref(false)
  const ticks: number[] = []
  let frames: FrameRequestCallback[] = []
  const raf = vi.fn((cb: FrameRequestCallback) => {
    frames.push(cb)
    return frames.length
  })
  vi.stubGlobal('requestAnimationFrame', raf)
  const s = useSmoothStream({
    content: () => content.value,
    streaming: () => streaming.value,
    onTick: () => ticks.push(1),
  })
  const drain = (n: number, t = 1_000_000) => {
    for (let i = 0; i < n; i++) {
      const f = frames
      frames = []
      t += 40
      f.forEach((cb) => cb(t))
    }
  }
  return { s, content, streaming, ticks, get frames() { return frames } }
}

describe('useSmoothStream', () => {
  it('passes content through when not streaming', () => {
    const { s, content } = setup()
    content.value = 'hello'
    expect(s.display.value).toBe('hello')
  })

  it('releases words gradually while streaming', () => {
    const { s, content, streaming, drain } = setup()
    streaming.value = true
    content.value = 'aa bb cc'
    drain(1)
    expect(s.display.value.length).toBeLessThan('aa bb cc'.length)
    drain(20)
    expect(s.display.value).toBe('aa bb cc')
  })

  it('flushes to the exact store content when streaming ends', () => {
    const { s, content, streaming, drain } = setup()
    streaming.value = true
    content.value = 'xx yy'
    drain(1)
    streaming.value = false
    expect(s.display.value).toBe('xx yy')
  })

  it('accelerates on big backlog (code-block burst)', () => {
    const { s, content, streaming, drain } = setup()
    streaming.value = true
    content.value = 'a'.repeat(400)
    drain(5)
    expect(s.display.value.length).toBeGreaterThanOrEqual(360)
  })

  it('bypasses for oversized content', () => {
    const { s, content, streaming } = setup()
    streaming.value = true
    content.value = 'x'.repeat(20_001)
    expect(s.display.value).toBe('x'.repeat(20_001))
  })
})
```

- [ ] **Step 2: 确认失败**
- [ ] **Step 3: 实现**

```ts
// web/src/composables/useSmoothStream.ts
import { onScopeDispose, ref, watch } from 'vue'

export interface SmoothStreamOpts {
  /** Store-held source of truth (never lagging semantics). */
  content: () => string
  streaming: () => boolean
  /** Called on every released chunk (stick-to-bottom re-follow). */
  onTick?: () => void
  /** Above this size the smoother bypasses entirely (re-parse fuse). */
  maxDirect?: number
}

const CJK = /[\u{4E00}-\u{9FFF}\u{3040}-\u{30FF}\u{FF00}-\u{FFEF}]/u

function nextChunkLen(s: string, from: number): number {
  const rest = s.slice(from)
  if (!rest) return 0
  if (CJK.test(rest[0])) return 1
  const m = rest.match(/^\s*\S+/)
  return m ? m[0].length : 1
}

/**
 * Display-layer smoothing for streamed assistant text: the store stays the
 * source of truth; this composable releases it to the screen word-by-word
 * (CJK: char-by-char) on a rAF cadence, catching up fast when the network
 * is far ahead. Flushing on stream end is immediate and exact.
 */
export function useSmoothStream(opts: SmoothStreamOpts) {
  const maxDirect = opts.maxDirect ?? 20_000
  const display = ref(opts.content())
  let released = opts.content().length
  let raf = 0
  let last = 0

  function tick(now: number) {
    const total = opts.content()
    const backlog = total.length - released
    if (backlog <= 0) {
      raf = 0
      return
    }
    if (backlog > 300) released = Math.max(released, total.length - 40)
    const interval = backlog > 120 ? 12 : 32
    if (now - last >= interval) {
      last = now
      released += nextChunkLen(total, released)
      display.value = total.slice(0, released)
      opts.onTick?.()
    }
    raf = requestAnimationFrame(tick)
  }

  watch(
    () => opts.content(),
    (total) => {
      if (!opts.streaming() || total.length > maxDirect) {
        released = total.length
        display.value = total
        return
      }
      if (!raf) {
        last = 0
        raf = requestAnimationFrame(tick)
      }
    },
  )

  watch(
    () => opts.streaming(),
    (live) => {
      if (!live) {
        released = opts.content().length
        display.value = opts.content()
        if (raf) {
          cancelAnimationFrame(raf)
          raf = 0
        }
      }
    },
    { immediate: true },
  )

  onScopeDispose(() => raf && cancelAnimationFrame(raf))
  return { display }
}
```

ChatItemView.vue 接线：

```ts
const emit = defineEmits<{ (e: 'render-tick'): void }>()

const smooth = useSmoothStream({
  content: () => (props.item.kind === 'assistant' ? props.item.content : ''),
  streaming: () => streaming.value,
  onTick: () => emit('render-tick'),
})
// textHtml 改为消费平滑后的呈现内容：
const shownContent = computed(() => (streaming.value ? smooth.display.value : textContent.value))
const textHtml = computed(() => (isText.value ? renderMarkdown(shownContent.value) : ''))
```

ChatPanel.vue：`<ChatItemView v-for="item in store.activeItems" :key="item.id" :item="item" @render-tick="follow" />`（ChatPanel.test 的 stubs 不受影响）。

- [ ] **Step 4: 跑测试 + 全量** → PASS
- [ ] **Step 5: Commit**

```bash
git add web/src/composables/useSmoothStream.ts web/src/composables/useSmoothStream.test.ts \
  web/src/components/ChatItemView.vue web/src/components/ChatPanel.vue
git commit -m "web: word-paced display smoothing for streamed replies with stick-to-bottom re-follow"
```

---

### Task 12: 收尾验证批次

**Files:**
- Modify:（按清理结果）复查 `grep -rn "emptyHint\|empty-prompt-chip\|WEmptyState" web/src`——WEmptyState.vue 已在 Task 2 删除（无测试文件），确认无残留死引用
- Modify: `e2e/tests/personal/personal.spec.ts`（新增 greeting/FLIP 断言块）

- [ ] **Step 1: 全量测试** — `cd web && bun run test` → 全绿
- [ ] **Step 2: tokens 校验** — `cd web && npm run check:tokens` → 通过
- [ ] **Step 3: 前端构建** — `cd web && npm run build`（vue-tsc + vite）→ 通过
- [ ] **Step 4: Go 构建** — 仓库根 `go build ./...` → 通过
- [ ] **Step 5: e2e（personal spec + 新增 greeting 断言）**

在 `e2e/tests/personal/personal.spec.ts` 追加一个 test block（沿用该文件既有的页面对象/选择器风格）：

```ts
test('empty session shows the centered greeting and docks on first send', async ({ page }) => {
  await page.goto('/')
  // 问候态：居中问候可见且匹配四问候之一；chips 在 composer 下
  await expect(page.locator('.chat-greeting-text')).toBeVisible()
  await expect(page.locator('.chat-greeting-text')).toContainText(/how can I help|从哪里开始|做点什么|帮你|搭把手/i)
  await expect(page.locator('.greeting-chip')).toHaveCount(3)
  // chip 点击 → 填入 composer（fillPrompt 契约）
  await page.locator('.greeting-chip').first().click()
  await expect(page.locator('.chat-input textarea')).not.toHaveValue('')
  // 发送 → 离开问候态；FLIP 落定后无残留 transform
  await page.locator('.chat-input textarea').fill('hello from the greeting test')
  await page.keyboard.press('Enter')
  await expect(page.locator('.chat--greeting')).toHaveCount(0)
  await page.waitForTimeout(600)
  expect(await page.locator('.chat-input').evaluate((el) => getComputedStyle(el).transform)).toBe('none')
})
```

（问候语正则覆盖 en/zh 两套文案；transform 断言覆盖 400ms 兜底清理路径。）

Run: `cd e2e && npx playwright test tests/personal/personal.spec.ts` → 通过。

**裁剪声明（spec §六.2 要求全量 5 spec）**：其余 4 spec（native-dev/bridge/edge/relay）需要各自后端拓扑与真机/栈环境，本批改动不触碰 `.chat-input` 锚点与其发送链路（由 personal spec + 组件测试覆盖）；全量 5 spec 回归留给用户下次统一跑栈时执行。

- [ ] **Step 6: Commit（如有清理/新增改动）**

```bash
git add -A web/src e2e/tests/personal/personal.spec.ts
git commit -m "web: chat start experience — e2e greeting coverage and cleanup"
```

---

## Self-Review（已自查）

1. **Spec 覆盖**：§3.A→Task 1/2；§3.B→Task 3；§3.C1/C2→Task 4、C4→Task 5、C5→Task 6、C3→Task 11；§3.D 气泡级→Task 7/8/9、代码块级→Task 10；§3.5 审计修订全部体现（fresh 标记法=C-1/I-5、decorate 触发=C-2、WDropdown 挂载=I-2、chips 填 prompt=I-7、startedAt 三处联动=I-3、/btw 与反向 FLIP 裁决=行为不变无需代码）。P2 可选拍板项（工作区问候/shiki/820px）明确排除。
2. **占位符扫描**：无 TBD/TODO；所有代码块完整可落地；唯一动态枚举点是 Task 4 Step 1 的 grep（有已知锚点兜底）。
3. **类型一致性**：`greetingSlot` 返回值与 `chat.greeting.${slot}` 键名一致；`fresh/startedAt/durationMs` 在 types.ts 定义、items.ts 写入、ChatItemView 消费；`markdownToPlain`/`decorateCodeBlocks`/`WDropdown`/`useSmoothStream` 签名在任务间一致；`copied` 联合类型扩展为含 `'plain'` 并贯穿 flash/copyText/模板。

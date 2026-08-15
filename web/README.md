# woca web

woca 的 Web 前端（Vue 3 + TypeScript + Vite）。构建产物会被 Go 二进制通过
`go:embed web/dist` 内嵌，实现单二进制交付。

## 技术栈

- Vue 3（`<script setup lang="ts">`）
- Pinia（会话 / 工作区 / 设置状态）
- @vueuse/core（`useDark` 主题切换）
- lucide-vue-next（图标）
- marked + dompurify（Markdown 渲染 + XSS 消毒）
- @microsoft/fetch-event-source（SSE 流式客户端）

## 开发

```bash
npm install
npm run dev        # http://localhost:5173（/api 代理到 127.0.0.1:8899）
```

## 构建

```bash
npm run build      # vue-tsc 类型检查 + vite build，输出到 dist/
```

## 演示（standalone demo）

一个不依赖 Go 后端、可单独启动的演示工程，用丰富的 mock 数据预览全部 UI
（会话、流式消息、工具卡片、Markdown 预览、审批弹窗、主题）：

```bash
npm run dev:demo       # http://localhost:5174
npm run build:demo     # 构建到 dist-demo/
```

演示数据在 `demo/data.ts`；演示页顶部有「权限审批 / 提问 / 计划审批」按钮，
可直接触发三类审批弹窗。生产应用**不含**这些 mock 数据。

## 目录结构

```
src/
├─ main.ts                 # 入口：createApp + pinia
├─ style.css               # 设计令牌（灰白 + 阴影，light/dark）
├─ types.ts                # 会话 / 消息 / 工作区文件 / transcript 类型
├─ lib/markdown.ts         # marked + dompurify 渲染
├─ api/client.ts           # REST + SSE 客户端（fetch-event-source）
├─ stores/
│  ├─ settings.ts          # 主题 + 设置面板开关 + 偏好
│  ├─ sessions.ts          # 会话 + 消息 + 运行状态（含 demo 模式）
│  └─ workspace.ts         # 工作区文件树 + Markdown 预览
└─ components/
   ├─ Sidebar.vue          # 左侧：会话 + 工作区 + 底部设置入口
   ├─ SessionList.vue
   ├─ WorkspaceTree.vue    # 递归文件树
   ├─ TreeNode.vue
   ├─ SettingsPanel.vue    # 卡片式设置（主题/偏好/模型）
   ├─ ChatPanel.vue        # 右侧对话窗口
   ├─ ChatItemView.vue     # 消息 / 工具调用 / 工具结果卡片
   ├─ InterruptModal.vue   # 权限 / 提问 / 计划审批弹窗
   └─ MarkdownView.vue     # 文件 Markdown 预览弹层
demo/
├─ index.html              # 演示入口
├─ main.ts
├─ App.vue                 # 演示布局 + 审批触发按钮
└─ data.ts                 # 丰富 mock 数据
```

## 主题

主题通过 CSS 变量实现，`.dark` 类挂在 `<html>` 上。`useDark` 持久化到
`localStorage["woca-theme"]`，并跟随系统偏好；`index.html` 内置一段内联脚本在
首帧前应用主题，避免闪烁。

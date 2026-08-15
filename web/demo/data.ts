import type { ChatItem, CompletionEntry, Interrupt, Session, WorkspaceFile } from '@/types'

// Rich mock data for the standalone demo. The real app fetches all of this
// from the Go backend; this file only exists to preview the UI in isolation.

export const demoSkills: CompletionEntry[] = [
  { name: 'clear', description: 'clear the conversation and start fresh' },
  { name: 'compact', description: 'summarize the conversation to free context' },
  { name: 'model', description: 'show or switch the model' },
  { name: 'status', description: 'show version, model, mode, and session stats' },
  { name: 'commit', description: '为暂存区的变更创建一条 conventional commit 提交' },
  { name: 'pr', description: '创建、查看或合并 GitHub Pull Request' },
  { name: 'review', description: '对当前分支的改动做代码审查，输出逐条意见' },
  { name: 'test', description: '运行测试套件并总结失败原因' },
]

export const demoSubagents: CompletionEntry[] = [
  { name: 'reviewer', description: '代码审查子代理：检查正确性、可读性与安全隐患' },
  { name: 'frontend', description: '前端专家：Vue/TS 组件与样式实现' },
  { name: 'backend', description: '后端专家：Go 服务、API 与存储设计' },
]

const WS_WOCA = '/Users/demo/code/woca'
const WS_DOCS = '/Users/demo/code/woca-docs'

export const demoSessions: Session[] = [
  { id: 'demo-s1', title: '接入 Web UI 的可行性调研', updatedAt: '', loaded: true, workspace: WS_WOCA, workspaceName: 'woca' },
  { id: 'demo-s3', title: '为 graph 增加文件检查点', updatedAt: '', loaded: true, workspace: WS_WOCA, workspaceName: 'woca' },
  { id: 'demo-s4', title: '重构 runner 的流式渲染', updatedAt: '', loaded: true, workspace: WS_WOCA, workspaceName: 'woca' },
  { id: 'demo-s2', title: '修复 Bash 工具权限判断', updatedAt: '', loaded: true, workspace: WS_DOCS, workspaceName: 'woca-docs' },
]

export const demoItems: Record<string, ChatItem[]> = {
  'demo-s1': [
    {
      id: 's1-1',
      kind: 'user',
      content: '调研一下 woca 接入 Web UI 的可行性，重点看 SSE 和前端选型。',
    },
    {
      id: 's1-2',
      kind: 'assistant',
      streaming: false,
      content:
        '可行性很高，woca 现有架构已经为「流式 Web UI」预留了天然接缝：\n\n- `graph.Step` 就是 SSE 的事件载荷\n- `runner.InterruptHandler` 对应审批弹窗\n- `runner.Renderer` 直接换成 SSE 发射器\n\n先搜一下业界做法：',
    },
    {
      id: 's1-3',
      kind: 'tool_call',
      toolCallId: 'tc1',
      name: 'WebSearch',
      args: { query: 'Go embed single binary SPA SSE streaming 2025' },
    },
    {
      id: 's1-4',
      kind: 'tool_result',
      toolCallId: 'tc1',
      content:
        '1. server-sent-events.com — Go Streaming Patterns for SSE\n2. dev.to — Server-Sent Events in Go: Building Real-Time AI Streaming Without WebSockets\n3. pkg.go.dev/embed — standard library embed (Go 1.16+)',
    },
    {
      id: 's1-5',
      kind: 'assistant',
      streaming: false,
      content:
        '推荐组合：\n\n```go\nfunc (s *Server) Stream(w http.ResponseWriter, r *http.Request) {\n    flusher, _ := w.(http.Flusher)\n    w.Header().Set("Content-Type", "text/event-stream")\n    for ev := range run.events {\n        fmt.Fprintf(w, "event: %s\\ndata: %s\\n\\n", ev.Type, ev.JSON)\n        flusher.Flush()\n    }\n}\n```\n\n| 维度 | SSE | WebSocket |\n| --- | --- | --- |\n| 方向 | 单向 server→client | 全双工 |\n| 断线 | 原生自动重连 | 需自己实现 |\n\n**结论**：SSE + POST，前端 Vue 或 Svelte 都可以。',
    },
  ],
  'demo-s2': [
    {
      id: 's2-1',
      kind: 'user',
      content: 'Bash 工具的只读判断在 `git diff` 这类命令上误判了。',
    },
    {
      id: 's2-2',
      kind: 'tool_call',
      toolCallId: 'tc2',
      name: 'Read',
      args: { file_path: 'internal/tools/readonly.go' },
    },
    {
      id: 's2-3',
      kind: 'tool_result',
      toolCallId: 'tc2',
      content:
        'func IsReadOnly(name string, args map[string]any) bool {\n\tswitch name {\n\tcase "Read", "Glob", "Grep", "ListDir":\n\t\treturn true\n\t}\n\treturn false\n}',
    },
    {
      id: 's2-4',
      kind: 'tool_call',
      toolCallId: 'tc3',
      name: 'Edit',
      args: {
        file_path: 'internal/tools/readonly.go',
        old_string: 'case "Read", "Glob", "Grep", "ListDir":',
        new_string: 'case "Read", "Glob", "Grep", "ListDir", "git diff":',
      },
    },
    {
      id: 's2-5',
      kind: 'tool_result',
      toolCallId: 'tc3',
      content: '已应用修改（1 处替换）。',
    },
    {
      id: 's2-6',
      kind: 'assistant',
      streaming: false,
      content:
        '已在只读分类器里加入 `git diff`。注意：更稳妥的做法是让只读判断识别 `git` 前缀的命令子集，而不是枚举具体命令。',
    },
  ],
  'demo-s3': [
    {
      id: 's3-1',
      kind: 'user',
      content: '希望 graph 的检查点能落盘，支持跨进程恢复。',
    },
    {
      id: 's3-2',
      kind: 'tool_call',
      toolCallId: 'tc4',
      name: 'Bash',
      args: { command: 'ls internal/graph/' },
    },
    {
      id: 's3-3',
      kind: 'tool_result',
      toolCallId: 'tc4',
      content:
        'checkpoint.go\ncheckpoint_test.go\ncompiled.go\ngraph.go\ngraph_test.go\ninterrupt.go\nstate.go',
    },
    {
      id: 's3-4',
      kind: 'tool_call',
      toolCallId: 'tc5',
      name: 'Write',
      args: {
        file_path: 'internal/graph/filecheckpoint.go',
        content: 'package graph\n\n// FileCheckpointer persists checkpoints to disk.\n// ...\n',
      },
    },
    {
      id: 's3-5',
      kind: 'tool_result',
      toolCallId: 'tc5',
      content: '已写入 internal/graph/filecheckpoint.go（128 行）',
    },
    {
      id: 's3-6',
      kind: 'assistant',
      streaming: false,
      content: '已实现 `graph.NewFileCheckpointer`，通过 `--checkpoint-dir` 开启即可。',
    },
  ],
  'demo-s4': [
    {
      id: 's4-1',
      kind: 'user',
      content: '给 runner 加一个渲染器接口，方便不同前端复用。',
    },
    {
      id: 's4-2',
      kind: 'assistant',
      streaming: false,
      content:
        '这是第 4 个示例会话，用来演示会话列表的滚动、相对时间，以及多会话切换。真实模式下这些会话来自 `GET /api/sessions`。',
    },
  ],
}

export const demoInterrupts = {
  permission: {
    kind: 'permission',
    payload: {
      tool_name: 'Bash',
      tool_args: { command: 'rm -rf build/' },
      reason: '运行命令：rm -rf build/',
      is_read_only: false,
    },
  },
  ask: {
    kind: 'ask',
    payload: {
      questions: [
        {
          question: '选择部署目标？',
          header: '部署',
          options: [
            { label: 'Docker', description: '容器化部署' },
            { label: '裸机', description: '直接二进制部署' },
            { label: 'Kubernetes', description: '集群部署' },
          ],
        },
        {
          question: '需要哪些附加能力？',
          header: '能力',
          multi_select: true,
          options: [
            { label: 'MCP', description: '模型上下文协议' },
            { label: 'OAuth 登录' },
            { label: 'Web UI' },
          ],
        },
      ],
    },
  },
  plan: {
    kind: 'plan',
    payload: {
      plan: '## 实施计划\n\n1. 抽出 SSE hub\n2. 接入 `go:embed` 单二进制\n3. 前端打通审批交互\n\n预计 3 个里程碑完成。',
    },
  },
} satisfies Record<'permission' | 'ask' | 'plan', Interrupt>

const readmeMd = `# woca

一个用 Go 写的 coding-agent **harness**，模仿 Claude Code 的架构。

## 特性

- 图运行时（\`internal/graph\`）
- 工具集（\`internal/tools\`）
- 权限系统 + 只读分类器
- token 级流式输出
- MCP 客户端

## 快速开始

\`\`\`bash
make build
go run ./cmd/woca -p "explain this repo"
\`\`\`
`

const designMd = `# Web UI 设计

## 布局

- 左侧 sidebar：会话 + 工作区 + 设置
- 右侧：对话窗口

## 视觉

灰白风格、卡片、简单线条、柔和阴影。
`

const archMd = `# 架构

\`\`\`
cmd/woca            cobra CLI
internal/graph      LangGraph 风格运行时
internal/agent      编码 agent 图 + 权限门禁
internal/server     HTTP + SSE + 会话持久化
web/                Vue 3 前端（embed 进二进制）
\`\`\`
`

const mainGo = `package main

import (
	"context"
	"flag"
	"fmt"
	"os"
)

func main() {
	prompt := flag.String("p", "", "one-shot prompt")
	flag.Parse()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if *prompt == "" {
		fmt.Fprintln(os.Stderr, "usage: woca -p \\"prompt\\"")
		os.Exit(1)
	}
	run(ctx, *prompt)
}

func run(ctx context.Context, prompt string) error {
	fmt.Println(prompt)
	return nil
}
`

const agentGo = `package agent

// Agent wires the model, tools and permission gate into a runnable graph.
type Agent struct {
	model      ChatModel
	registry   *Registry
	permission PermissionMode
}

// Run streams one conversation turn as a sequence of Steps.
func (a *Agent) Run(ctx context.Context, msgs []Message) error {
	for step := range a.graph.Invoke(ctx, msgs) {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.emit(step)
	}
	return nil
}
`

const chatVue = `<script setup lang="ts">
import { computed, ref } from 'vue'
import ChatItemView from './ChatItemView.vue'
import { useSessionStore } from '../stores/sessions'

const store = useSessionStore()
const draft = ref('')
const busy = computed(() => store.activeStatus === 'running')
</script>

<template>
  <div class="chat">
    <ChatItemView v-for="item in store.activeItems" :key="item.id" :item="item" />
  </div>
</template>
`

export const demoWorkspace: WorkspaceFile = {
  name: 'woca',
  path: 'woca',
  kind: 'dir',
  children: [
    { name: 'README.md', path: 'woca/README.md', kind: 'file', content: readmeMd },
    { name: 'go.mod', path: 'woca/go.mod', kind: 'file', content: 'module github.com/projanvil/woca\n\ngo 1.24\n' },
    { name: 'Makefile', path: 'woca/Makefile', kind: 'file' },
    {
      name: 'cmd',
      path: 'woca/cmd',
      kind: 'dir',
      children: [
        {
          name: 'woca',
          path: 'woca/cmd/woca',
          kind: 'dir',
          children: [
            { name: 'main.go', path: 'woca/cmd/woca/main.go', kind: 'file', content: mainGo },
            { name: 'main_test.go', path: 'woca/cmd/woca/main_test.go', kind: 'file' },
          ],
        },
      ],
    },
    {
      name: 'internal',
      path: 'woca/internal',
      kind: 'dir',
      children: [
        {
          name: 'agent',
          path: 'woca/internal/agent',
          kind: 'dir',
          children: [
            { name: 'agent.go', path: 'woca/internal/agent/agent.go', kind: 'file', content: agentGo },
            { name: 'task.go', path: 'woca/internal/agent/task.go', kind: 'file' },
          ],
        },
        {
          name: 'graph',
          path: 'woca/internal/graph',
          kind: 'dir',
          children: [
            { name: 'graph.go', path: 'woca/internal/graph/graph.go', kind: 'file' },
            { name: 'compiled.go', path: 'woca/internal/graph/compiled.go', kind: 'file' },
            { name: 'checkpoint.go', path: 'woca/internal/graph/checkpoint.go', kind: 'file' },
          ],
        },
        {
          name: 'server',
          path: 'woca/internal/server',
          kind: 'dir',
          children: [
            { name: 'server.go', path: 'woca/internal/server/server.go', kind: 'file' },
            { name: 'run.go', path: 'woca/internal/server/run.go', kind: 'file' },
            { name: 'session.go', path: 'woca/internal/server/session.go', kind: 'file' },
          ],
        },
        {
          name: 'runner',
          path: 'woca/internal/runner',
          kind: 'dir',
          children: [{ name: 'runner.go', path: 'woca/internal/runner/runner.go', kind: 'file' }],
        },
        { name: 'tools', path: 'woca/internal/tools', kind: 'dir', children: [] },
      ],
    },
    {
      name: 'web',
      path: 'woca/web',
      kind: 'dir',
      children: [
        {
          name: 'src',
          path: 'woca/web/src',
          kind: 'dir',
          children: [
            {
              name: 'components',
              path: 'woca/web/src/components',
              kind: 'dir',
              children: [
                { name: 'ChatPanel.vue', path: 'woca/web/src/components/ChatPanel.vue', kind: 'file', content: chatVue },
                { name: 'Sidebar.vue', path: 'woca/web/src/components/Sidebar.vue', kind: 'file' },
              ],
            },
          ],
        },
        { name: 'README.md', path: 'woca/web/README.md', kind: 'file', content: designMd },
      ],
    },
    {
      name: 'docs',
      path: 'woca/docs',
      kind: 'dir',
      children: [
        { name: 'design.md', path: 'woca/docs/design.md', kind: 'file', content: designMd },
        { name: 'architecture.md', path: 'woca/docs/architecture.md', kind: 'file', content: archMd },
      ],
    },
  ],
}

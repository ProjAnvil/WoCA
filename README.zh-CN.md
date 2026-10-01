<div align="center">

# woca

**单二进制编码代理 harness —— 以 Web UI 为主，另有 TUI 与 CLI，构建在显式代理图之上。**

[![CI](https://github.com/ProjAnvil/WoCA/actions/workflows/ci.yml/badge.svg)](https://github.com/ProjAnvil/WoCA/actions/workflows/ci.yml)
[![Go Version](https://img.shields.io/badge/go-1.26%2B-00ADD8)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

[特性](#特性) · [安装](#安装) · [快速上手](#快速上手) · [架构](#架构) · [扩展](#扩展-woca) · [HTTP-API](#http-api)

[English](README.md) | **简体中文**

</div>

---

woca 是一个用 Go 编写的编码代理（coding agent）harness。它通过一个显式、可检
视的**代理图**（agent graph）—— 模型 → 工具 → 模型 —— 驱动任意
Anthropic 兼容或 OpenAI 兼容的模型，带有人工介入的权限门、token 级流式输
出、自动压缩、子代理、skills、hooks、MCP 服务器与内嵌 Web UI，以**单一静
态二进制**交付，无任何运行时依赖。

代理循环不是藏在框架回调里的隐式 `while` 循环。它是一张你能读懂的图：节
点、条件边、reducer、checkpoint 与 interrupt/resume，运行时来自
[langchain-golang](https://github.com/ProjAnvil/langchain-golang) 的
langgraph 包，组装于 [`internal/agent`](internal/agent)。
每次运行都逐 token 流式输出，每次工具调用都经过确定性的权限门，每段会话都
持久化为可恢复的 transcript。

## 目录

- [特性](#特性)
- [环境要求](#环境要求)
- [安装](#安装)
- [快速上手](#快速上手)
  - [Web UI（推荐）](#web-ui推荐)
  - [交互式 TUI / REPL](#交互式-tui--repl)
  - [单次执行](#单次执行)
- [供应商与模型](#供应商与模型)
- [权限](#权限)
- [架构](#架构)
  - [版本与仓库布局](#版本与仓库布局)
  - [Bridge（私有云形态）](#bridge私有云形态)
- [配置](#配置)
- [会话、记忆与撤销](#会话记忆与撤销)
- [多代理](#多代理)
  - [子代理](#子代理)
  - [Agent 团队](#agent-团队)
- [扩展 woca](#扩展-woca)
  - [Skills](#skills)
  - [自定义子代理](#自定义子代理)
  - [Hooks](#hooks)
  - [MCP 服务器](#mcp-服务器)
  - [能力插件](#能力插件)
  - [复用现有配置](#复用现有配置)
- [斜杠命令](#斜杠命令)
- [HTTP API](#http-api)
- [可观测性](#可观测性)
- [开发](#开发)
- [参与贡献](#参与贡献)
- [许可证](#许可证)

## 特性

**代理运行时**

- **显式代理图**：条件边、`add_messages` reducer、checkpoint 与
  interrupt/resume —— 任意运行可在工具调用中间暂停，稍后继续，甚至跨进程
  重启恢复。
- **token 级流式输出**：文本与工具调用增量经图流式传递；TUI 与 Web UI 在
  工具执行的同时实时渲染。
- **流式工具执行**：只读调用在模型仍在输出时即被派发；受权限门约束的调用
  始终等待确定性门控，保证中断语义精确。
- **健壮的模型 I/O**：invoke 与 stream 双路径均支持指数退避重试与可选
  fallback 模型；`max_tokens` 截断自动升档并续写；超长 prompt 触发带单次
  重试保护的反应式压缩；每个工具轮次受聚合大小预算约束，占位替换具有确
  定性。
- **自动压缩**：CJK 感知的 token 估算器；先裁剪旧工具结果、再微压缩大结
  果，最后才摘要对话头部；连续失败后熔断（`/compact` 可重置）。
- **轮中引导（对齐 Codex）**：TUI 或 web UI 中在运行中提交的消息会立即加入当前运行的实时输入，而不等待本轮结束；若在有待发引导时中断运行，会立即将其作为新回合重新发送，而不会丢失。
- **思考 / 推理支持**：reasoning effort 级别与流式推理内容，Web UI 渲染为
  可折叠块。

**工具与安全**

- **20+ 内置工具**：`Bash`、`Read`、`Write`、`Edit`、`NotebookEdit`、
  `Glob`、`Grep`、`ListDir`、`TodoWrite`、`WebFetch`、`WebSearch`、
  `AskUserQuestion`、`EnterPlanMode`/`ExitPlanMode`、`Task`/`TaskOutput`/
  `TaskStop`、`TaskCreate`/`TaskGet`/`TaskUpdate`/`TaskList`、`Skill`、
  `SendMessage`，以及 LSP 代码智能。
- **权限门**：四种模式、显式 allow/deny/ask 规则（精确名、正则、
  `Tool(glob)` 匹配器）与输入敏感的只读分类器 —— `git status` 自动放行，
  `rm -rf` 永远询问。
- **文件级撤销**：每次 `Write`/`Edit` 前做快照；`/undo` / `/redo` 可反复
  撤销与重做。

**交互面**

- **内嵌 Web UI**（推荐的交互方式）：按工作区分组的会话、带工具卡的流
  式气泡、语法高亮的文件侧栏、图片上传与截图粘贴、设置面板 —— 由同一
  二进制经 SSE 提供，并通过 `woca web start | stop | restart | status`
  以后台守护进程方式管理。
- **交互式 TUI**（bubbletea）：实时流式输出、审批提示、vim 输入模式、主
  题、可滚动 transcript。
- **单次 CLI**：适合脚本与管道。

**生态**

- **Skills**（`SKILL.md` + YAML frontmatter）、**自定义子代理**
  （`.woca/agents/*.md`）、**hooks**（12 组事件、匹配器、退出码语义）、
  **MCP** 客户端（stdio + streamable HTTP，工具 / 资源 / prompts / 通知 /
  OAuth）、**能力插件**（托管浏览器自动化、PDF 工具链与 Office 文档支
  持）—— 见
  [扩展 woca](#扩展-woca)。
- **分层配置**：skills、agents、MCP 服务器与指令文件均按 flag > 工作区
  `.woca/` > 用户 `~/.woca/` 解析。
- **持久化设置**：默认 SQLite（可选 PostgreSQL），API key 与 DSN
  **静态加密**（AES-256-GCM）存储。
- **用量与额度**：按会话的 token 记账与成本估算，订阅套餐余量显示（GLM
  Coding Plan、Anthropic/OpenAI OAuth 登录、DeepSeek 余额）。

## 环境要求

- macOS 或 Linux（x86-64 / arm64）
- 从源码构建需 Go ≥ 1.26
- 一个模型供应商：Anthropic 兼容或 OpenAI 兼容的 API key、网关，或 OAuth
  登录

## 安装

**从源码构建（当前推荐）：**

```bash
git clone https://github.com/projanvil/woca.git
cd woca
make            # 构建 web 前端 + 二进制，输出 build/woca
```

**使用 `go install`：**

```bash
go install github.com/projanvil/woca/cmd/woca@latest
```

Web UI 在编译期经 `go:embed` 内嵌，`web/dist` 是构建产物（不纳入 git
跟踪）。裸 `go build` / `go install` 仍可编译，但二进制只会提供占位页
（"woca web UI not built"），直到你执行一次 `make web`（或 `make`）构
建前端 —— 此后每次改动 `web/` 下内容都需重新执行。

**Make 目标：**

| 目标 | 说明 |
|------|------|
| `make` / `make serve` | 构建前端 + 二进制；`serve` 额外启动 Web UI |
| `make build` | 构建二进制到 `build/woca` |
| `make test` / `make race` | 单元测试（可选开启竞态检测） |
| `make lint` | golangci-lint |
| `make install` | 安装到 `/usr/local/bin` |

## 快速上手

指定一个供应商（持久化，静态加密）：

```console
$ woca config set provider glm
$ woca config set glm-api-key <key>
```

或者直接导出 `ANTHROPIC_API_KEY` / `OPENAI_API_KEY`，跳过配置。

### Web UI（推荐）

Web UI 是使用 woca 的主要方式。以后台守护进程方式管理：

```bash
woca web start          # 后台启动 Web UI → http://127.0.0.1:3484
woca web status         # 查看是否在运行
woca web restart        # 重启后台 Web UI
woca web stop           # 停止
```

服务器内嵌前端并通过 SSE API 暴露代理能力 —— 创建会话、发起运行、观看流
式事件、处理权限审批、浏览工作区文件、管理设置。

其他运行方式：`woca serve` 在前台运行服务器（`--addr`、
`--no-browser`）；交互会话中 `/web` 按需启动（`/web <port>`、
`/web off`、`/web status`）。

### 交互式 TUI / REPL

```bash
woca                    # 终端下为 bubbletea TUI；否则为行式 REPL
woca --resume last      # 继续最近一次会话
```

审批提示用 `y`/`n` 回答，`pgup`/`pgdown` 滚动；中断正在进行的轮次：
TUI 中连按两次 `esc`（`esc-esc`）或 `ctrl+c`，或从 Web UI 取消
（`POST /api/runs/{id}/cancel`）。行式 REPL 中以 `!` 前缀执行 shell 命令，
输出纳入对话。见[斜杠命令](#斜杠命令)。

**文件上下文：** 在行首或空格后输入 `@` 可补全并附加工作区内的文件或目录；
被引用的内容会展开进发给模型的消息，而对话记录仍展示你原始的 `@path` 文本。

**引导（Steering）：** 运行中输入内容后按 `enter` 会立即送入当前运行的实时
输入队列，而不是等到本轮结束（对齐 Codex）；底部状态栏会显示
“steering (N): ...”。若在有待发送引导内容时中断运行（`esc-esc` 或
`ctrl+c`），会立即将其作为新的一轮重新发送，而不会丢失。

**输入框：** `ctrl+j` 插入换行而不提交；`ctrl+g` 在 `$VISUAL`（回退到
`$EDITOR`）中打开当前草稿，返回后用保存的文件内容替换草稿。`shift+tab`
循环切换权限模式（default → acceptEdits → plan → readOnly →
bypassPermissions），与 web 输入框行为一致。ctrl+j/ctrl+g 均可
重新绑定——见 `/keybindings`。

**按键绑定：** `/keybindings` 显示当前生效的输入框、会话与 vim 模式按键；
`/keybindings open` 创建（或报告路径）`~/.woca/keybindings.json`，一个针对
输入框动作（`composer:newline`、`composer:editor`）的扁平 `{"按键": "动作"}`
覆盖文件。

### 单次执行

```bash
woca -p "解释这个仓库的构建体系"
woca -p "修复失败的测试" --dangerously-skip-permissions   # 谨慎使用
```

## 供应商与模型

| 供应商 | API | 默认模型 | 说明 |
|--------|-----|----------|------|
| `anthropic`（默认） | Anthropic Messages | `claude-sonnet-4-5` | 也可 OAuth 登录：`woca login` |
| `openai` | OpenAI Chat Completions | `gpt-4o` | 任意兼容 `--base-url` 网关 |
| `glm` | GLM（Anthropic 兼容端点） | `glm-4.6` | 支持 GLM Coding Plan 额度显示 |
| `deepseek` | DeepSeek（Anthropic 兼容端点） | `deepseek-chat` | 支持余额显示 |

任意 OpenAI 兼容网关通过 `--base-url`（`OPENAI_BASE_URL` /
`ANTHROPIC_BASE_URL` 或持久化设置）接入。模型 fallback
（`--fallback-model`）、思考级别（`/thinking`）与按模型的
reasoning-effort 映射走同一路由。

## 权限

| 模式 | 只读 / 元信息 / 网络 | 文件编辑 | Shell |
|------|----------------------|----------|-------|
| `default` | 放行 | 询问 | 询问 |
| `acceptEdits` | 放行 | 放行 | 询问 |
| `bypassPermissions` | 放行 | 放行 | 放行 |
| `readOnly` / `plan` | 放行 | 拒绝 | 拒绝 |

只读分类是**输入敏感**的：`Bash("git status")` 自动放行，而
`Bash("rm -rf …")` 询问。显式规则优先于模式：

```json
{
  "permissions": {
    "allow": ["Bash(git *)", "Read"],
    "deny":  ["Bash(rm *)"],
    "ask":   []
  }
}
```

优先级为 `deny` → `ask` → `allow` → 模式默认。子代理 fail-closed：未授权
的写权限表现为 *permission denied*，而不是弹窗询问。

## 架构

```
cmd/woca                 cobra CLI：flags、config、REPL、单次执行、serve

# 代理核心
internal/agent           编码代理图（langchain-golang/langgraph 运行时）：
                         门控、plan 模式、流式执行、恢复、压缩、团队、目标
internal/agentbuild      所有运行代理的入口共享的组装根
internal/runner          流式运行循环 + 中断处理 + 渲染
internal/tools           工具集 + 注册表 + 权限分类
internal/permissions     权限模式、规则、只读分类器
internal/model           供应商路由：重试、fallback、调用
internal/compact         自动压缩 / 摘要
internal/tokens          CJK 感知 token 估算器
internal/prompt          系统提示词组装
internal/promptref       @path 提示引用展开
internal/contextx        供提示词使用的运行时上下文（git 状态等）
internal/diffutil        Web UI Changes 视图的小型 unified diff
internal/cost            会话成本估算
internal/quota           供应商额度 / 用量显示

# 交互面
internal/tui             bubbletea 终端 UI
internal/repl            行式 REPL 输入解析与编辑
internal/console         TUI / REPL / web 共享的斜杠命令
internal/slashcmds       内置斜杠命令表（共享且无 import 环）
internal/keybindings     用户可覆盖的 TUI 动作映射
internal/servefront      服务器角色前端：serve（Web UI 服务器）与
                         web（后台守护进程）
internal/webd            Web GUI 后台守护进程（pidfile、start/stop）
internal/webgui          按需 Web UI 服务（/web 命令）
internal/webfx           web 框架：net/http 之上 gin/fiber 风格的 API
internal/sdkout          机器可读的无头输出（json / stream-json）

# Web 与 API 管线
internal/server          web 服务器：会话、运行、SSE、设置 API
internal/gateway         所有部署共享的 L1 边缘中间件
internal/searchapi       通用 JSON web 搜索 API 客户端
internal/fsb             工作区文件浏览器（jailed，os.OpenRoot）

# 存储与状态
internal/session         JSONL transcript 持久化
internal/settings        设置数据库 + 加密 secrets
internal/wocadb          设置数据库打开器（sqlite / postgres）
internal/store           命名空间 + key 的 JSON 文档存储
internal/memory          ladyM 记忆引擎适配器
internal/runstate        外置运行账本（runs + 事件流）
internal/runws           run 级工作区载体
internal/merge           分层配置合并

# 平台与运维
internal/bus             进程内消息总线（SendMessage）
internal/obslog          harness 可观测日志（按大小轮转 JSONL）
internal/settingswatch   管理面写入向各角色的传播
internal/tracing        可观测核心（span 词汇 + Sink 接口）
internal/logging         zerolog 封装
internal/notify          终端通知
internal/oauth           OAuth 2.0 设备授权许可客户端
internal/config          面向用户的配置
internal/claudeenv       ~/.claude skills/MCP 导入
internal/cpserde         checkpoint 负载序列化
internal/platform/       共享平台原语（authn、authz、httpx、
                         machineid …）
internal/skills          SKILL.md 加载器（分层：flag > 工作区 > 用户）
internal/agents          子代理定义（.woca/agents/*.md）
internal/hooks           hook 运行器（12 组事件）
internal/mcp             MCP 客户端（stdio + streamable HTTP、OAuth）
internal/plugins         市场插件（skills/agents 捆绑包）
internal/pluginrt        能力插件运行时（browser、pdf、office）
```

### 版本与仓库布局

woca 以同一仓库构建的**两个版本**交付：

- **个人版** —— `woca` 二进制：CLI/TUI/REPL 加本地 `woca serve` Web
  UI，零外部依赖（sqlite、一切进程内）。个人用户安装的就是它。
- **企业版** —— 由同一核心加企业树组装的服务器角色：多租户认证（API
  key、native-client DPoP 会话）、基于 NATS 消息面的队列派发、独立配
  置服务、可观测性（OTel/Prometheus/Grafana）与 registry DSN 发现。
  共七个角色二进制，均无交互界面：

  - `woca-server` —— 模块化单体 API 面：业务 API 加 console 管理权
    威；自身不提供页面
  - `woca-worker` —— 从共享 runstate 账本认领运行并经代理执行（探针
    端口默认 `:9092`）
  - `woca-config` —— 独立的设置管理面；唯一的配置写入口，附带自己的
    console SPA
  - `woca-tenant` —— 租户 / 用户 / API key 管理 CLI（附带个人版→企
    业版记忆迁移工具）
  - `woca-bridge` —— 面向用户机器的私有云执行面（组织凭据登录；自身
    不携带密钥管理）
  - `woca-edge` —— 无状态边缘接入点；控制面唯一的公开组件
  - `woca-ops` —— 租户运维 console（组织凭据由它签发），外加
    `/skillsvc/` 下的 skill 资产 console

  与 registry 耦合的角色（`woca-server`、`woca-worker`、
  `woca-config`、`woca-edge`）对 nacos registry **fail-closed**：它们
  要求 `WOCA_NACOS_URL`，缺失时拒绝启动，因此最小企业部署需包含一个
  nacos registry（开发级 standalone 清单见
  `deploy/k8s/00-nacos.yaml`；端到端验证见
  `deploy/vm/nacos-verify.sh`）。

交付轴与版本轴正交：目前两个版本均为**自托管**（见
`deploy/selfhosted/`、`deploy/k8s/`、`deploy/vm/`）；托管云形态是同一
份企业代码的未来打包方式，而非第三个版本。

```
internal/        核心，两个版本共享（agent、tools、server、fsb …）
internal/ee/     仅企业版：msgplane、tenant、configsvc、worker、obs、
                 discovery、nacosrt、nativeauth、dpop、authcore、
                 consoleauth、tenantcmd、memcmd、skillsvc（skill 资产
                 API；其 console SPA 是 woca-ops 上的 skillsvcui）、
                 opsconsole/opsui（租户运维 console API + SPA）、
                 bridge（私有云 bridge 服务）、entrygw（私有核心机器
                 registry）、edgegw（无状态边缘接入点）、edition
                 （woca-server 的组装根）
internal/platform/machineid  共享机器身份（enrollment token、Ed25519
                 身份、不透明访问令牌）
web/             共享 Web UI（个人版 + 企业版 web 面）
ee/              企业版 web console（configsvc-web、opsconsole-web、
                 skillsvc-web）
deploy/selfhosted/  企业版 docker-compose 栈（数据面 + 完整栈）
deploy/k8s/         Kubernetes 清单（server + worker、nacos registry、
                    可观测性管线）
deploy/vm/          registry fail-closed 验证（nacos-verify.sh）
deploy/micro/       按角色的分发包（二进制 + start/stop 脚本）
deploy/collector/   通往 Langfuse / 可观测栈的 OTLP trace 桥
deploy/observability/  OTel Collector + Prometheus + Jaeger + Grafana 栈
deploy/terraform/   全栈的 AWS 可用区优先部署
cmd/woca         个人版入口（全功能）
cmd/woca-*       企业版角色入口（无交互界面）
```

### Bridge（私有云形态）

`woca-bridge` 是产品化的私有云执行面：同样的 serve 面加上产品身份模
型 —— 组织凭据由私有核心上的租户运维 console 签发（bridge 自身不携
带密钥管理；未登记的 bridge 仅保留 loopback 信任），URL+key 登录配
cookie 会话（吊销从运维 console 经配置生成级联下发），工作区文件浏
览器（`/api/fs/*`，以 `os.OpenRoot` 关进沙箱），以及 fail-closed 绑
定检查（未认证的非 loopback 绑定拒绝启动）。已登记的 bridge 只会看
到一个控制面对端 —— 无状态边缘接入点（`cmd/woca-edge`、
`internal/ee/edgegw`；无数据库、无文件，每次机器令牌校验都回源服务
面）。其后私有核心一分为二：registry 路由
（`/gw/enroll|renew|heartbeat|resolve|bridges`，挂载在 woca-server 的
`WOCA_GW_ADDR` 监听器上）与配置拉取（woca-config 上的
`/gw/config`）。一次性 enrollment token 换得本地 Ed25519 身份加短
TTL 不透明访问令牌；拉取的设置安装为受管覆盖层（锁定，
`/api/settings` 中的 `managed_fields`），控制面故障时 bridge 继续运
行，状态页显示 STALE 标记。同一出站通道还承载 NAT 穿透数据面：客户
端流量停靠在边缘，直到 bridge 的隧道取走并用自己的 handler 栈应答
（SSE 流原样透传、flush 保留），因此任意位置的 web 都能指挥藏在
NAT 之后、零入站端口的 bridge。loopback bridge 保留零登录的个人体
验；个人版 `woca` 二进制不链接其中任何部分。

完整的私有云阶段随之交付：TLS 直接终结（`WOCA_TLS_CERT/KEY`）、长轮
询配置通道（pull 之上的 push 语义）、worker 主导的登记（指纹审批
—— 无需粘贴 token）、bridge 的只读运维视图（HTTP + SPA 的实时会话
+ 审计；用户凭据是租户运维 console 签发的组织 key —— bridge 自身不
携带密钥管理）、全局健康灯与 Markdown 诊断导出、登录页 QR 配对，以
及 native client 的内嵌 bridge（捆绑二进制、零登录 loopback 配置、
带 TOFU 锁定与 OS 钥匙串密钥存储的组织 key 登录）、三入口连接页
（内嵌 / 经边缘的私有云（可选择目标 bridge）/ 直连 bridge —— 底层
同一套 profile 机制）、bridge 本地的云接入设置（设置数据库中的
`edge.*` key、SPKI TOFU 锁定、带来源标签的环境变量覆盖，页面位于
设置 → 云接入），以及两个开发 compose 组（微服务孪生加带
`public`/`internal` 网络分层、bridge 作为独立容器的边缘 compose 组
—— 部署手册见 `deploy/selfhosted/BRIDGE.md`）。

边界是被强制执行的，而非口头约定：个人版二进制从不链接
`internal/ee`（其依赖基线固定在 `ci/deps-personal.txt`），企业树从
不链接交互前端，核心包只能通过接缝触及企业能力 ——
`server.MsgPlane`、`server.NativeAuth`、`servefront.Edition` 与
`wocadb.DSNResolver`。`make check-editions` 守护全部三个方向。

> **布局变更（2026-08-22）。** 企业包移入 `internal/ee/`（import 路
> 径相应变更 —— 例如 `internal/msgplane` → `internal/ee/msgplane`；
> 企业版 web console 移到 `ee/`）；`deploy/enterprise/` 现为
> `deploy/selfhosted/`。个人版二进制不再附带 `tenant` 与 `memory` 子
> 命令（请使用 `woca-tenant`），`woca serve --auth apikey` 现在需要
> `woca-server` 二进制 —— 多租户认证是企业版能力。

### 代理图

```
        ┌──────────┐   no tool calls   ┌──────┐
START ─▶│  model   │──────────────────▶│ END  │
        └────┬─────┘                   └──────┘
             │ has tool calls
             ▼
        ┌──────────┐   (gate + hooks)   ┌──────┐
        │  tools   │───────────────────▶│ INT  │
        └────┬─────┘                   └──┬───┘
             │ (always loops back)        │ resume
             └────────────────────────────┘
```

`tools` 节点先逐调用运行**门控**（权限、AskUserQuestion、plan 审批）—— 因
此中断暂停是确定性的 —— 再并发执行放行的调用。Pre/Post 工具 hook 包裹每
次调用；Pre hook 以 `2` 退出即拒绝该工具，Post hook 以 `2` 退出则将其输
出回喂给模型。状态是单一 `messages` 通道加 `add_messages` reducer，流式
增量、工具结果与中断因此可组合。

### 目标模式

`/goal <objective>` 将代理切换为自主循环，表达为架在代理图之上的另一张
图：

```
START ─▶ assess ─▶ execute ─▶ evaluate ─┬─ active   ─▶ assess (next cycle)
                                        └─ terminal ─▶ END
```

每个循环先规划（assess）、再执行完整的嵌套代理轮（execute）、然后**怀疑
式验证**（evaluate）—— 代理可以*声称*完成，但只有经验证的证据才能完结目
标。预算（`--cycles`、`--tokens`）、空闲自动续跑与循环中中断/恢复均生效；
目标状态按会话持久化，重启后仍在。用 `/goal status`、`/goal
pause|resume`、`/goal budget`、`/goal clear` 管理循环；Web UI 实时显示循
环 / token 计量与逐循环裁定。

## 配置

设置存放在 `~/.woca` 下的数据库 —— 默认 SQLite（纯 Go、无 cgo），可通过
DSN 换 PostgreSQL。凭据与 DSN **静态加密**（AES-256-GCM；主密钥在
`~/.woca/secret.key`，权限 `0600`）。三个入口共享同一数据库：

```console
$ woca config set provider openai
$ woca config set openai-api-key sk-...        # 加密存储
$ woca config list
```

- **终端**：`woca config get|set|list`
- **交互会话**：`/settings`、`/memory list|put|get|del`
- **Web UI**：设置面板（`GET`/`PUT /api/settings`）

解析顺序：`--api-key` → `ANTHROPIC_API_KEY` / `OPENAI_API_KEY` → 数据库
（加密）→ OAuth token。图 checkpoint 与 memory store 各自选择后端
（`sqlite`、`postgres`、`file`、`memory`、`none`）。

常用 flags：

| Flag | 说明 |
|------|------|
| `-p, --prompt` | 单次执行的 prompt（非交互运行） |
| `--model` / `--provider` | 模型名与供应商 |
| `--base-url` / `--api-key` | 供应商端点与凭据 |
| `--permission-mode` | `default` \| `acceptEdits` \| `bypassPermissions` \| `readOnly` \| `plan` |
| `--workdir` | 工作目录（默认当前目录） |
| `--mcp` | MCP 服务器：`name=command` 或 `name=https://…`（可重复） |
| `--fallback-model` | 重试耗尽后的 fallback 模型 |
| `--max-turns` | 每轮对话的模型轮数上限 |
| `--auto-compact` | 压缩触发的 token 估算阈值（`0` 关闭） |
| `--resume` | 恢复 transcript（或 `last`） |
| `--db` | 设置数据库：sqlite 路径、`postgres://` DSN 或 `none` |
| `--checkpoint-dir` | 持久化图 checkpoint 以跨进程恢复 |

## 会话、记忆与撤销

- **会话**以 JSONL transcript 持久化在 `~/.woca/sessions`，按工作区分组；
  `/rename` 命名快照、`/branch` 分叉、`/rewind` 回跳、`/export` 导出
  markdown 或可恢复 JSON，`--resume <name|last>` 随处继续。
- **记忆**：配置 memory 后端后，每轮会预取与你的消息相关的记忆并在工具轮
  后附加；`/memory put` 写入的长期上下文会在之后触达模型。
- **指令文件**：`CLAUDE.md` 与 `AGENTS.md` 从用户级到 git root 直至工作目
  录逐级注入。
- **撤销**：`Write`/`Edit` 快照按会话保留；`/undo` / `/redo` 反复撤销 /
  重做最近一次变更。`Bash` 造成的外部变更不跟踪。

## 多代理

### 子代理

`Task` 派生隔离的子代理，拥有独立上下文窗口与工具子集；
`TaskOutput`/`TaskStop` 负责管理。自定义子代理以 markdown 定义：

```markdown
<!-- .woca/agents/reviewer.md -->
---
name: reviewer
description: 审查 diff 的正确性与风格问题
tools: Read, Grep, Glob
---
你是一名严格的代码审查者，只报告具体、可执行的问题。
```

之后模型可调用 `Task(subagent_type="reviewer", prompt=…)`。

### Agent 团队

并行工作场景下，团队共享 worktree，通过带所有权的共享任务列表
（`TaskUpdate` 原子认领、`DependsOn` 排序）与消息路由（`SendMessage` 唤醒
空闲队友）协作。队友运行自主循环并带单队友超时；`/team` 查看成员、任务与
收件箱。

## 扩展 woca

### Skills

将带 YAML frontmatter 的 `SKILL.md` 放进 `.woca/skills/<name>/`：

```markdown
---
name: commit
description: 撰写 conventional commit 信息
---
为当前 diff 撰写一条 conventional-commit 信息。
```

模型会发现它并调用 `Skill(skill="commit")`。Skill 分层解析：工作区副本优
先于用户级 `~/.woca/skills`。

### 自定义子代理

见[子代理](#子代理)。定义按 `.woca/agents/`（工作区）优于
`~/.woca/agents/`（用户）解析。

### Hooks

在 `~/.woca/settings.json` 或 `.woca/settings.json` 配置生命周期 hook：

```json
{
  "hooks": {
    "PreToolUse": [
      {"matcher": "Bash(git *)", "hooks": [{"type": "command", "command": "echo 'git command incoming'"}]}
    ],
    "PostToolUse": [
      {"matcher": "Edit|Write", "hooks": [{"type": "command", "command": "cat"}]}
    ]
  }
}
```

支持十二组事件：`PreToolUse`、`PostToolUse`、`UserPromptSubmit`、`Stop`、
`SessionStart`、`SessionEnd`、`SubagentStop`、`Notification`、`PreCompact`、
`PostCompact`、`PermissionRequest`、`PostSampling`。每个 hook 从 stdin 收到
JSON 事件；stdout 回喂为上下文。`PreToolUse` hook 以 `2` 退出即拒绝工具；
`PostToolUse` hook 以 `2` 退出则阻断继续。

### MCP 服务器

```bash
woca --mcp "filesystem=node /path/to/server.js /tmp" \
     --mcp "search=https://mcp.example.com/mcp"
```

工具以 `mcp__<server>__<tool>` 出现，另有资源（`mcp__<server>__resources`）
与 prompts。支持 stdio（换行分隔 JSON-RPC）与 streamable HTTP（POST +
JSON/SSE、`Mcp-Session-Id`）两种传输；OAuth 保护的服务器用 `woca mcp auth
<name> <endpoint>` 一次授权（token 加密存储）。服务器定义分层解析：
`--mcp` flag > 工作区 `.woca/mcp.json` > 用户 `~/.woca/mcp.json`，会话中途
新增的工具会在轮次间被自动拾取。

### 能力插件

托管、带版本的工具捆绑包，按需安装到 `~/.woca/plugins`：

- **browser** —— 托管浏览器自动化（导航、点击、表单、截图），用于 web
  测试与调研
- **pdf** —— 沙箱化 Python 环境，用于 PDF 生成与处理
- **office** —— 在同一沙箱化 Python 环境中读写 Office 文档（Word
  `.docx`、Excel `.xlsx`、PowerPoint `.pptx`）

在设置面板（`/plugins`）浏览与安装，运行时按 pin 下载并校验 SHA-256。

### 复用现有配置

woca 与常见代理约定互操作：读取 `CLAUDE.md` / `AGENTS.md` 指令文件、从
`.claude/skills` 发现 skills，并可在 Web 设置面板一键从既有 `~/.claude`
环境导入 skills 与 MCP 服务器定义。如果你已经在用代理工具，woca 直接继承
你的配置，而不是要求你重写一遍。

## 斜杠命令

在 TUI、REPL 与 web 聊天输入框可用（前缀 `/`，带补全）：

| 命令 | 说明 |
|------|------|
| `/help` | 列出可用命令 |
| `/status` | 版本、供应商/模型、权限模式、运行统计 |
| `/permissions [mode]` | 查看或设置权限模式 |
| `/model [name]` | 运行时查看或切换模型 |
| `/thinking [level]` | 推理力度：off \| low \| medium \| high \| tokens |
| `/quota` | 订阅套餐剩余额度 |
| `/cost` | 累计 token 用量与成本估算 |
| `/clear` | 开启全新对话 |
| `/compact` | 摘要对话以释放上下文 |
| `/context` | 上下文窗口用量明细 |
| `/todos`、`/tasks` | todo 列表 / 共享任务列表 |
| `/skills`、`/mcp`、`/agents`、`/plugins` | 管理扩展 |
| `/tools` | 列出生效的 agent 工具及其分类与权限安全级别 |
| `/hooks` | 列出已配置的 hook 事件，未配置时明确提示 |
| `/undo`、`/redo` | 撤销 / 重做最近一次文件编辑 |
| `/rewind`、`/branch`、`/rename`、`/export` | 会话历史导航 |
| `/goal …` | 自主目标循环（set/status/pause/resume/budget/clear） |
| `/team` | Agent 团队状态 |
| `/memory …` | 检视与编辑 memory store |
| `/settings …` | 持久化设置 |
| `/init` | 生成 `AGENTS.md` 项目指南 |
| `/doctor` | 体检配置、凭据、存储、hooks、权限 |
| `/theme`、`/vim`、`/keybindings` | UI 偏好（`/keybindings open` 编辑覆盖配置）|
| `/web [port\|off\|status]` | 内嵌 Web UI 控制 |
| `/output-style` | default \| explanatory \| learning |

## HTTP API

Web 服务器经 REST + SSE 暴露代理能力：

| 端点 | 说明 |
|------|------|
| `POST /api/sessions` | 创建会话（立即持久化）→ `{"session_id":"…","workspace":"…"}` |
| `GET /api/sessions` | 列出持久化会话 |
| `GET /api/sessions/{id}` | 获取完整 transcript |
| `DELETE /api/sessions/{id}` | 删除会话（墓碑标记防复活） |
| `POST /api/runs` | 发起运行：`{"session_id":"…","prompt":"…"}` |
| `GET /api/runs/{id}/events` | SSE：`delta` / `reasoning` / `tool_call` / `tool_result` / `goal` / `goal_note` / `interrupt` / `done` / `error`（goal 运行的循环过程走 `goal_note`，结束时以一条统一 `delta` 回复收尾） |
| `POST /api/runs/{id}/answer` | 响应中断：`{"approve":true}` 或 `{"answers":[…]}` |
| `POST /api/runs/{id}/input` | 排队轮中输入 |
| `POST /api/runs/{id}/cancel` | 取消运行 |
| `GET`/`PUT /api/settings` | 读取 / 更新持久化设置 |

## 可观测性

开启可观测日志（`--debug-log`、Web 设置面板或 `/debug-log on`）后，完整
harness 日志写入 `~/.woca/logs/`，按大小轮转：

```
logs/woca.log                            运行生命周期、模型用量、摘要
logs/ws-<name>-<hash8>/<session>.jsonl   每行一个 JSON 事件
logs/ws-<name>-<hash8>/<session>/<run>.json   消息级 transcript dump
```

每次图节点派发都记录相位、耗时与状态摘要；Web UI 中每个助手气泡附带调试
动作，一键复制确切日志位置便于排障。

## 开发

```bash
make web        # 构建前端（go build 前需执行一次）
make build      # 构建二进制
make test       # 单元测试
make race       # 带竞态检测的测试
make lint       # golangci-lint
```

前端是 [`web/`](web/README.md) 下的 Vue 3 + TypeScript + Vite SPA；demo
（`npm run dev:demo`）用 mock 数据预览完整 UI。图运行时、代理循环、权限
门、hooks、压缩、skills、MCP 客户端与 web 服务器均有单元测试 —— 本项目以
测试先行方式构建。

代码风格与 PR 规范见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 参与贡献

欢迎 issue 与 pull request。大于 bug fix 级别的改动请先开 issue 讨论设
计。见 [CONTRIBUTING.md](CONTRIBUTING.md)。

## 许可证

woca 以 [MIT License](LICENSE) 发布。基于
[langchain-golang](https://github.com/projanvil/langchain-golang)、
[bubbletea](https://github.com/charmbracelet/bubbletea) 与
[cobra](https://github.com/spf13/cobra) 构建。

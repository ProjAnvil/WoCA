# 主流沙箱平台 API 调研与适配策略

日期：2026-10-19
状态：已定稿
关联：`2026-10-17-space-sandbox-research.md`（SPACE 专项）、`2026-10-19-sandbox-console-design.md`（沙箱管理台与执行链路设计，本文产出对其的修订）

## 1. 结论

1. **平台 API 收敛在两组公共操作上**：沙箱生命周期（create/delete/超时）+ 四类执行面（命令执行、文件读写、输出流、归档往返）。差异集中在模板模型、流式机制（SSE/WebSocket/轮询）、网络策略 API。
2. **两大风格阵营**：REST 资源风格（E2B、Daytona、Cloudflare bridge）与 RPC/SDK 风格（microsandbox JSON-RPC、Modal、CodeSandbox、SPACE gRPC+HTTP）。
3. **E2B 协议正在事实标准化**：开源项目 agent-sandbox 实现了 E2B 兼容 API；SPACE 提供 E2B 兼容层（ECW）。多家后端可经一个 E2B adapter 接入。
4. **Cloudflare sandbox bridge 的 HTTP API 与 woca 契约 v2 高度重合**（argv/cwd、SSE exec、文件读写、tar 往返、session）。它验证了契约 v2 的形状选型。
5. **适配建议：provider 注册表架构**。通用契约 v2 保留为主通道（自建/类 Cloudflare 后端直连），官方 Go SDK 的平台（Daytona、Modal）内置原生 provider，闭源 SDK-only 平台（CodeSandbox）走 sidecar 适配。

## 2. 平台清单

| 平台 | 开源 | 隔离技术 | 部署形态 | API 形态 |
|---|---|---|---|---|
| E2B | 基础设施部分开源，云闭源 | Firecracker microVM | 云（自托管 infra 可行但重） | REST（生命周期）+ envd（沙箱内） |
| Daytona | 开源 | OCI/Docker 容器 + gVisor 级隔离 | 云 + 自托管 | REST + 多语言 SDK（含 Go） |
| SPACE | 开源（Apache 2.0） | Firecracker microVM | 自托管（K8s/裸机，需 KVM） | HTTP + SSE + 异步 operation |
| Cloudflare Sandboxes | 闭源（beta） | Cloudflare Containers | 云（bridge Worker 部署到自己账户） | REST + SSE |
| Modal | 闭源 | 容器（Firecracker 底座） | 云 | SDK-only（Python/JS/Go） |
| microsandbox | 开源（Apache 2.0） | libkrun microVM | 本地/自托管（macOS 用 Hypervisor.framework，Linux 需 KVM） | JSON-RPC 2.0 + MCP |
| CodeSandbox SDK | 闭源 | Firecracker microVM | 云 | Node SDK-only |
| agent-sandbox | 开源 | 容器 | 自托管 | REST（原生）+ REST（E2B 兼容） |

## 3. API 形状逐家拆解

### 3.1 E2B（事实标准的种子）

生命周期（`api.e2b.app`，`X-API-Key` 头）：

```
POST   /v2/sandboxes                  {templateID, timeout, envVars, network{allowOut,denyOut,
                                       egressProxy}, mcp, iam, volumeMounts} → {sandboxID, envdAccessToken, ...}
GET    /sandboxes / /sandboxes/{id}
DELETE /sandboxes/{id}
POST   /sandboxes/{id}/pause|resume|connect|refreshes|snapshots|timeout
```

- 沙箱内操作（命令执行、文件）经沙箱内 envd 守护进程，用 `envdAccessToken` 认证
- 网络策略最全：allowOut/denyOut（CIDR/域名）、egressProxy、maskRequestHost
- 模板模型：templateID 必填，从模板派生沙箱
- **兼容生态**：agent-sandbox 的 `/e2b/v1/sandboxes` 端点兼容 E2B 协议与 SDK；SPACE 的 ECW 兼容经测试的 E2B Python SDK

### 3.2 Daytona（唯一官方 Go SDK 的云平台）

```
POST   /api/sandbox                   {} → sandbox 对象（Bearer API key）
POST   /api/sandbox/{id}/exec         {command, cwd, timeout} → {exit_code, result}
```

- SDK 面（Python/TS/Ruby/Go/Java）：`sandbox.process.exec / code_run`、`sandbox.fs.*`（类 Node fs CRUD）、**内置 LSP 支持**、Git 操作
- 状态事件：Socket.IO 流 + 轮询兜底
- 创建 <90ms；快照（v32 Node 18 之类的版本化 snapshot）
- Go SDK：`github.com/daytona/clients/sdk-go`——woca 同为 Go，可直连

### 3.3 SPACE（已详调研，此处仅列 API 要点）

- 生命周期异步：create/pause/suspend/delete 返回 operation ID 轮询
- 命令经 command session：attach token + 独立数据面连接 + SSE
- 模板 = Firecracker recipe；warm pool / base snapshot 提速
- ECW 暴露 E2B 兼容面

### 3.4 Cloudflare Sandboxes（与契约 v2 最接近）

sandbox bridge（参考实现 Worker，部署到自己账户，Bearer `SANDBOX_API_KEY`）：

```
POST   /v1/sandbox                    → {id}
DELETE /v1/sandbox/:id                → 204
GET    /v1/sandbox/:id/running        → {running}
POST   /v1/sandbox/:id/exec           {argv:[], timeout_ms, cwd} → SSE 流
         事件：stdout/stderr(base64 块)、exit{exit_code}、error{error, code}
GET    /v1/sandbox/:id/file/*         → 原始字节（application/octet-stream）
PUT    /v1/sandbox/:id/file/*         ← 原始字节，≤32MiB，路径限定 /workspace
POST   /v1/sandbox/:id/persist        → tar 归档（?excludes=）
POST   /v1/sandbox/:id/hydrate        ← tar 归档，≤32MiB
POST   /v1/sandbox/:id/session        → {id}（工作目录/env/命令状态隔离）
POST   /v1/sandbox/:id/mount          挂载 R2/S3 bucket
```

- argv 元素按 ANSI-C `$'...'` 转义防注入
- **persist/hydrate 就是契约 v2 的 archive 端点**；file 读写是原始字节版

### 3.5 Modal（SDK-only，Go SDK 可用）

- `Sandbox.create(app, image, params)`：cpu/memory/gpu、timeoutMs、workdir、env、secrets、blockNetwork、outboundCidrAllowlist/outboundDomainAllowlist、pty
- `sb.exec(argv...)` 返回进程句柄：stdout/stderr 流式迭代器、wait、returncode
- `sb.filesystem.read/write_text`、`sb.snapshot_filesystem()`（快照转 Image，30 天 TTL）
- 网络策略细粒度（CIDR/域名 allowlist，密钥在沙箱外解析的 outbound header 改写）
- Go SDK：`github.com/modal-labs/modal-client/go`

### 3.6 microsandbox（JSON-RPC，本地部署友好）

```
POST /api/v1/rpc   JSON-RPC 2.0，Bearer JWT（namespace 作用域）
  sandbox.start    {sandbox, namespace, config{image, memory, ...}}
  sandbox.stop     {sandbox, namespace}
  sandbox.command.run  （转发到沙箱内 portal）
  sandbox.repl.run     {language, code}
  sandbox.metrics.get
GET  /api/v1/health
POST /mcp          MCP 端点（tools/list、tools/call 映射内部 JSON-RPC）
```

- libkrun microVM：**macOS 上用 Hypervisor.framework，不需要嵌套虚拟化**；Linux 需 KVM
- 有云 beta（控制面 REST：sandboxes/volumes/quota/audit，`msb_` key）
- 对企业内网/办公网关部署有独特吸引力

### 3.7 CodeSandbox SDK（闭源，Node SDK-only）

- `sdk.sandboxes.create({id: templateId, vmTier, hibernationTimeoutSeconds, ...})`
- `sandbox.shells.run(cmd)` / `shells.js.run` / `shells.python.run`
- `sandbox.fs.*`：类 Node fs（writeTextFile/readTextFile/readdir/copy/remove/watch/batchWrite/download）
- `sandbox.fork()`（含运行进程的 live fork）、`hibernate()`/`resume()`（内存快照）
- 无公开 REST 契约；接 woca 需 Node sidecar 适配层

### 3.8 agent-sandbox（开源，E2B 兼容实现）

- 原生 REST `/api/v1/sandbox`（create/list/get/delete）+ E2B 兼容面 `/e2b/v1/sandboxes`
- 自托管容器隔离；适合作为"开箱即用的 E2B 形状后端"

## 4. 横向对比

| 维度 | E2B | Daytona | SPACE | CF bridge | Modal | microsandbox | CodeSandbox |
|---|---|---|---|---|---|---|---|
| 命令入参 | 经 envd | command 字符串 | argv（session） | **argv 数组** | **argv 数组** | code/command | command 字符串 |
| 输出流 | WebSocket/轮询 | Socket.IO | SSE+数据面 | **SSE** | 流式迭代器 | RPC 响应 | Promise |
| 文件读写 | envd API | fs SDK | exec 内命令 | **原始字节路径式** | filesystem API | mount | fs SDK |
| 归档往返 | 无原生 | 快照 | base snapshot | **tar persist/hydrate** | snapshot 转 Image | 无原生 | git 持久化 |
| 长进程 | 有 | 有 | session | exec 即流 | exec 流 | portal | shells |
| 网络策略 | **最全**（CIDR/域名/代理） | 无公开 API | egress profile | 无（bridge 层无） | **细粒度** | scope 枚举 | 无公开 API |
| 模板模型 | templateID | snapshot/language | Firecracker recipe | Dockerfile | Image | OCI image | template fork |
| Go 可直连 | REST 可 | **官方 Go SDK** | REST 可 | REST 可 | **官方 Go SDK** | JSON-RPC 可 | 否（Node） |
| 快照/恢复 | snapshots/pause | snapshot | 滚动快照+分支 | 无 | filesystem 快照 | 无 | hibernate/fork |
| 自托管成熟度 | 低（infra 重） | 高 | 中（需 KVM 节点池） | 不适用（CF 云） | 不适用 | **高（单机即用）** | 不适用 |

## 5. 收敛点与分歧点

**收敛点**（woca 契约 v2 已覆盖，无需改）：

1. create/delete + 标识符句柄
2. 命令执行带 cwd/timeout；argv 数组是更安全的主流形状（CF/Modal/SPACE 均如此）
3. 文件读写（路径式）
4. 流式输出（SSE 为主流）
5. 归档/快照往返（CF tar、E2B/Daytona/CSB 各有形态）

**分歧点**（adapter 各自吸收，不进契约 v2）：

1. 模板 vs 镜像 vs fork——统一抽象为后端侧预配置，woca 只管 `POST /sandbox/create {}`（模板选择留在后端配置里）
2. 网络策略 API——E2B/Modal 最强，CF bridge 无。契约 v2 不含（设计文档边界 1 维持），但**建议 v3 评估** `network` 字段
3. 快照/暂停——语义差异大（内存快照 vs 文件快照 vs git），v2 不进
4. 流式机制——SSE（CF/SPACE）vs WebSocket（E2B）vs 轮询（woca v2 契约）。adapter 把上游流聚合成契约的同步 exec / 轮询 output

## 6. 适配策略：provider 注册表

### 6.1 架构

```
                Executor 接口（woca 内部不变）
                        │
        ┌───────────────┼────────────────────────┐
        ▼               ▼                        ▼
  HTTPContract     NativeProvider           SidecarAdapter
  (契约 v2 客户端)  (Go 原生实现)             (契约 v2 服务进程)
        │               │                        │
        ▼               ▼                        ▼
  自建后端        Daytona(Go SDK)          CodeSandbox(Node SDK)
  CF bridge*     Modal(Go SDK)            其他 SDK-only 平台
  agent-sandbox* E2B(REST+envd)
  SPACE(经适配层/ECW)
  microsandbox(JSON-RPC)

  * CF bridge / agent-sandbox 与契约形状接近但非逐字段一致，
    仍需薄 adapter（可部署在 woca-worker 进程内，非独立 sidecar）
```

三层适配器，按成本从低到高：

1. **契约 v2 客户端**（零适配）：完全实现 CONTRACT.md 的后端
2. **Go 原生 provider**（进程内，数百行）：实现 `Executor` 接口，内部调各家 REST/SDK/JSON-RPC。`internal/platform/executor/providers/{e2b,daytona,modal,microsandbox,cfbridge}/`
3. **Sidecar adapter**（独立进程，暴露契约 v2）：为 SDK-only 闭源平台（CodeSandbox 的 Node SDK）准备

### 6.2 逐家适配矩阵

| 平台 | 适配层 | 成本估计 | 要点 |
|---|---|---|---|
| Cloudflare bridge | Go provider | 低（~300 行） | argv/cwd/tar 直接对上；SSE→同步聚合 + 轮询 output；32MiB 文件上限要在 provider 里分块或走 tar |
| agent-sandbox | Go provider | 低 | E2B 兼容面 + 原生面，形状规整 |
| E2B | Go provider | 中（~600 行） | 生命周期 REST + envd 命令/文件两段认证；网络策略强项暂不透出 |
| SPACE | 适配层或 ECW | 中 | 异步 operation 轮询要包装成同步 create；session/SSE→Start 映射；建议按调研报告 P0 先做独立适配层验证 |
| Daytona | Go provider（官方 SDK） | 低-中 | SDK 与 Executor 语义对齐好；Socket.IO 可不用（轮询兜底即可） |
| Modal | Go provider（官方 SDK） | 中 | exec 流式句柄→Start 映射自然；filesystem API→文件方法直译；网络策略暂不透出 |
| microsandbox | Go provider（JSON-RPC） | 低-中 | JSON-RPC 客户端简单；文件面弱（靠 mount），归档往返需经 command.run 用 tar 命令实现 |
| CodeSandbox | Node sidecar | 高 | 独立进程 + 契约 v2 服务化 + 生命周期运维；v1 暂缓，v1.x 再做 |

### 6.3 管理台数据模型修订

`sandbox_backends` 表增加 `type` 列（原设计缺失，本次修订）：

```
type ∈ {contract-http, e2b, daytona, modal, microsandbox, cf-bridge, sidecar}
```

- `type` 决定 worker 侧 provider 装配路径
- verify 探活按 type 分派到对应 provider 的探活实现（探活步骤不变，执行面换 provider）
- 前端表单按 type 动态渲染字段（endpoint/api key/region/target 等）

### 6.4 兼容策略的两个方向

1. **吸进来**（已选）：provider 注册表把各家后端接进 woca。管理台多 type 档案。
2. **走出去**（建议 v3 评估）：把 woca 的契约 v2 实现一份 **E2B 兼容面**。理由：E2B 协议正在事实标准化（agent-sandbox、SPACE ECW 都在做），woca 自建沙箱后端若同时暴露 E2B 兼容 API，可直接复用 E2B 生态的 SDK/工具链，也方便第三方 agent 平台接入 woca 沙箱。成本低（一个翻译层），收益是生态位。

## 7. 对设计文档的修订清单

1. `sandbox_backends` 表加 `type TEXT NOT NULL DEFAULT 'contract-http'` 列
2. 架构图与 §4.5：worker 按 type 装配 provider（契约客户端或 Go 原生 provider），不再只有单一 HTTP 契约路径
3. `Executor` 接口与契约 v2 解耦的表述强化：契约 v2 是**互操作格式**，`Executor` 是**内部抽象**；native provider 不经契约直接实现 Executor
4. verify 探活改为按 type 分派
5. 边界 1（egress）补充：E2B/Modal 的网络策略 API 存在但 v1 不透出，v3 契约评估 `network` 字段
6. 开放问题增加：E2B 兼容面（走出去方向）是否纳入 v3

## 8. 参考

- E2B API：https://docs.e2b.dev/api-reference/sandboxes/create-sandbox-v2
- agent-sandbox API：https://agent-sandbox.github.io/api
- Daytona：https://docs.daytona.io 、Go SDK `github.com/daytona/clients/sdk-go`
- Cloudflare sandbox bridge：https://developers.cloudflare.com/sandbox/bridge/http-api
- Modal Sandboxes：https://modal.com/docs/guide/vm-sandboxes 、Go SDK `github.com/modal-labs/modal-client/go`
- microsandbox：https://github.com/microsandbox/microsandbox（JSON-RPC/MCP 架构见 DeepWiki 分析）
- CodeSandbox SDK：https://codesandbox.io/docs/sdk/create 、https://codesandbox.io/docs/sdk/filesystem
- SPACE：https://github.com/perplexityai/SPACE

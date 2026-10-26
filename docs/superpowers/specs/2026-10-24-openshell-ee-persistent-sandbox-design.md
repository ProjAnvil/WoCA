# OpenShell 作为 woca EE 默认沙箱：持久工作区闭环设计

> **已作废（2026-10-26）。** 选型结论改为自建轻量沙箱后端，角色划分、存储模型与配置面全部重写。
> 现行设计见 `2026-10-26-woca-ee-sandbox-plane-design.md`。本文保留作为调研记录。

日期：2026-10-24
状态：设计主线已确认，待编写实施计划
前置资料：`docs/openshell-compose-research.md`、`sandbox-lifecycle-routing-research.md`

## 1. 目标

将 OpenShell 作为 woca EE 新部署的默认后台沙箱。用户通过 woca 创建项目工作区。用户本地不需要项目代码。代码生成、命令执行、测试和 Web 服务都在远程环境完成。

首个闭环必须证明：

1. EE 部署后默认启用 OpenShell。
2. 用户从空工作区创建远程应用。
3. 应用测试在 OpenShell 内通过。
4. 用户通过 woca 的鉴权 URL 访问 Web 页面。
5. 第二轮 run 复用同一个有效沙箱。
6. 第二轮修改不会重置运行环境。
7. 代码每轮同步回 woca 宿主。
8. Stop/Start 保留代码，不保留旧进程内存。
9. 任一沙箱环节失败都不会回退到本地执行。

个人开发者路径追加三个目标：

10. 一个企业版 compose 文件里包含 OpenShell 全部服务。
11. 一个 demo 容器内完成建 run、调工具、断言产物。
12. 开发者不需要模型 key，也不改任何生产配置。

## 2. 已确认的决策

| 决策点 | 选择 |
|---|---|
| 生命周期 | 持久 ExecutionWorkspace，不是每 run 一个临时沙箱 |
| 首期部署 | 单台远程 Linux，Docker Compose |
| 部署文件 | 直接写进企业版 `deploy/selfhosted/docker-compose.full.yml`，不另建文件 |
| 开发者入口 | 同一文件里的 `demo` profile。demo 容器内完成沙箱调用，无需模型 key |
| 代码权威副本 | 远程 woca 宿主共享目录 |
| 运行环境 | OpenShell 沙箱持续保留，按需 Stop/Start |
| 接入方式 | 独立 OpenShell adapter |
| 用户认证 | woca 统一认证和授权，不要求用户登录 OpenShell |
| Demo | 远程生成 Web 应用，测试通过，页面可访问，第二轮复用环境 |
| EE 默认行为 | 新部署默认启用 OpenShell，off-host run 不允许本地回退 |

### 2.1 选择 adapter 而不是原生 provider

adapter 负责 OpenShell gRPC、归档流、进程会话、Stop/Start 和服务转发。OpenShell 的 mTLS 凭据只保存在 adapter。

原生 Go provider 少一个服务。它会把 gRPC 依赖带入 woca 主模块，并把 OpenShell 凭据分发到 config 和 worker。首期不采用。

### 2.2 选择宿主权威副本

当前 `agentbuild` 会从宿主目录读取项目上下文、技能和配置。现有文件树和预览也读取宿主目录。保留宿主权威副本可以复用这些路径。

OpenShell 内的工作目录不是唯一数据源。它是当前运行代次中的执行副本。每轮结束后必须同步回宿主。

## 3. 非目标

首期不做：

- 多台 Docker 主机调度。
- 跨机器自动迁移。
- Kubernetes 部署。
- 自动故障转移和 gateway 高可用。
- 按租户配置独立 OpenShell OIDC workspace。
- GPU 和 per-run 动态资源选择。
- 任意 TCP 或 WebSocket 服务代理。
- managed service 在 run 之外修改 `/workspace` 的场景。
- 自动空闲回收；首期 demo 使用显式 Stop。
- 旧进程内存、PID 或终端状态恢复。
- 完整 Git worktree 协作平台。
- 个人版默认开启沙箱。
- 让现有所有 backend 支持持久生命周期。

多机路由和动态资源可以在后续复用本设计的 workspace ID、backend 绑定和运行代次。

## 4. 总体架构

```text
用户浏览器 / API 客户端
          │
          │ woca 身份认证
          ▼
     woca edge / server
          │
          ├─ session、run、文件树、服务鉴权代理
          │
          ▼
  PostgreSQL + run queue
          │
          ▼
      woca-worker ×2
          │
          │ OpenShell backend API
          ▼
   openshell-adapter ×1
          │
          │ mTLS / gRPC
          ▼
   openshell-gateway ×1
          │
          ├─ Docker socket
          ├─ supervisor containers
          └─ workload containers
```

宿主共享目录：

```text
/var/lib/woca/workspaces/<workspace_id>/
```

server 和每个 worker 都以相同路径挂载该目录。adapter 不挂载项目目录。adapter 只接收和返回归档流。

OpenShell gateway 独占 Docker socket。worker、server 和 adapter 不挂载 Docker socket。

## 5. 核心概念

| 概念 | 职责 | 生命周期 |
|---|---|---|
| Tenant / User | 身份和资源归属 | 由 woca 管理 |
| ExecutionWorkspace | 一个用户项目工作副本的逻辑环境 | 显式删除前持续存在 |
| Chat session | 对话历史，引用一个 ExecutionWorkspace | 独立于沙箱 |
| Run | 一次 Agent 执行 | 占用工作区的一段时间 |
| Sandbox | OpenShell 中的运行环境 | 可 Ready、Stop、Start、Delete |
| Runtime generation | 同一沙箱的一次运行代次 | Stop/Start 或重建后改变 |
| Managed service | 工作区中声明的 Web 服务 | 可跨 run 保留，按 desired state 恢复 |
| Host data revision | 宿主权威代码的版本 | 每次成功同步后递增 |

OpenShell workspace 是权限作用域。ExecutionWorkspace 是 woca 的执行环境。两者不能直接混用术语。

首期所有 OpenShell 沙箱可放在一个内部 OpenShell workspace。woca 必须在每次调用前校验 tenant、user 和 ExecutionWorkspace 归属。

## 6. 数据模型

### 6.1 `execution_workspaces`

在现有业务数据库中增加记录。首期不新建工作区微服务。

| 字段组 | 必要信息 |
|---|---|
| 身份 | `id`、`tenant_id`、`user_id` |
| 项目归属 | `project_ref`、`worktree_ref`、`workspace_kind` |
| 宿主存储 | `host_path`、`data_revision` |
| 后端绑定 | `backend_id`、`openshell_scope`、`sandbox_name` |
| 运行定位 | `runtime_generation`、`runtime_state`。generation 由 woca/adapter 单调维护，不要求 OpenShell 提供同名字段 |
| 执行互斥 | `active_run_id`、`fence_token` |
| 生命周期 | `last_used_at`、`last_error`、`created_at`、`updated_at` |

约束：

- `id` 使用服务端生成的 UUID。
- `host_path` 由服务端在配置的根目录下生成。
- 客户端不能提交任意宿主路径。
- 默认工作区在 tenant、user、project、worktree 作用域内唯一。
- `sandbox_name` 在 backend 内唯一。
- `active_run_id` 为空时，工作区才能 Stop 或 Delete。
- `fence_token` 单调递增。旧持有者不能提交新操作。

首期可以把逻辑工作区和当前沙箱绑定放在同一记录中。不需要另建通用资源目录。

### 6.2 session 和 run 的修改

session 增加 `execution_workspace_id`。run 增加：

- `execution_workspace_id`
- `execution_mode`
- `workspace_fence`

`execution_mode` 首期为 `persistent_openshell`。旧 run 没有该字段时保持旧行为。

旧 worker 不能领取新模式 run。worker capability 必须显式包含 `sandbox-persistent-v1`。server 在没有合格 worker 时返回 503。

### 6.3 `execution_workspace_services`

保存用户声明的服务，不把服务进程混同于 run-scoped 后台命令。

| 字段 | 说明 |
|---|---|
| `workspace_id` | 所属工作区 |
| `name` | 服务名，例如 `web` |
| `argv` | 服务端保存的启动命令 |
| `cwd` | 工作区内的相对目录 |
| `port` | 沙箱内 HTTP 端口 |
| `desired_state` | `running` 或 `stopped` |
| `actual_state` | 当前观测状态 |
| `process_generation` | 当前服务进程代次 |
| `last_error` | 最近启动或运行错误 |

同一工作区内的服务名唯一。服务定义由 woca 管理。用户不能借服务定义访问其他沙箱。

## 7. 生命周期与状态机

### 7.1 Runtime 状态

```text
provisioning -> ready
ready -> stopping -> stopped
stopped -> starting -> ready
任意非终态 -> recovering -> ready / stopped / error
任意非终态 -> deleting -> deleted
```

`ready` 表示运行环境可用。它不要求存在活跃 run。

`error` 表示需要人工或 reconciler 处理。`deleted` 是终态。

### 7.2 管理操作

**Acquire**

1. 校验调用者对工作区的权限。
2. 原子占用工作区和 queued run。
3. 增加 `fence_token`。
4. 没有沙箱时创建。
5. 创建时使用 retained canonical main process，不用一次性命令代替主进程。
6. 不使用 `--no-keep`，不依赖容器原生 restart policy。
7. 沙箱停止时 Start。
8. 校验 sandbox name 和 runtime generation。
9. 建立本轮执行器。
10. 只有首次创建或合格恢复时才上传宿主代码。

**Release**

1. 停止或收编本轮所有 run-scoped 进程。
2. 取消未完成的 exec。
3. 将远程代码同步到宿主 staging 目录。
4. 校验 `active_run_id` 和 `fence_token`。
5. 原子替换宿主权威目录。
6. 递增 `data_revision`。
7. 使相关上下文缓存失效。
8. 释放工作区占用。
9. 保留沙箱，不调用 Delete。

**Stop**

只在无活跃 run 时执行。先进入 `stopping`，阻止新的 Acquire。再停止 managed services，并确认写入者已结束。随后完成最终同步，最后停止沙箱。`desired_state` 不因 Stop 改变。Stop 不删除代码。

**Start**

启动既有沙箱并建立新 runtime generation。按 `desired_state` 重启 managed services。旧 PID、内存和终端不恢复。

**Delete runtime**

只删除 OpenShell 沙箱和服务运行态。宿主代码默认保留。

**Delete workspace**

这是单独的显式操作。先确认无活跃 run，再删除 runtime。宿主数据只有收到独立确认参数后才删除。

### 7.3 临时 verify 与用户工作区分开

配置台的 verify 仍创建临时沙箱，完成后销毁。verify 不复用用户 ExecutionWorkspace。

持久 provider 的 Release 不能改变 verify 的 Delete 语义。

## 8. 队列、串行和 fencing

### 8.1 同一工作区串行

同一 ExecutionWorkspace 同时只能有一个活跃 run。不同工作区可以并行。

多个聊天可以引用同一工作区。另一个聊天提交 run 时，run 保持 queued。worker 不应占住配额等待。

当前每 session 一个活跃 run 的限制可以保留。它不能替代跨 session 的工作区互斥。

### 8.2 原子 Claim

数据库事务必须同时完成：

1. 选择一个可执行的 queued run。
2. 确认工作区没有 `active_run_id`。
3. 确认工作区不处于 `recovering`、`stopping` 或 `deleting`。
4. 将 run 设置为 running。
5. 写入工作区的 `active_run_id`。
6. 增加并记录 `fence_token`。

PostgreSQL 使用行锁和条件更新。SQLite 使用现有事务模式。普通 SELECT 后再判断不够。

### 8.3 Heartbeat 和 Release

Heartbeat 必须同时匹配：

- run ID
- worker ID
- workspace ID
- fence token

Release、Stop、Delete 和宿主同步提交也必须匹配 fence。

### 8.4 Lease 失效

持久模式不能沿用“过期就直接 requeue”的行为。

失去 lease 后：

1. 当前 worker 收到取消信号。
2. adapter 请求取消该 run 的远程 exec。
3. 工作区进入 `recovering`。
4. 旧写入未确认停止前，不允许新 run。
5. 副作用未知的 run 标记失败或丢失，不自动重放。
6. 确认写入停止后，再执行同步和恢复。

fence 只能阻止过期协调请求。它不能自动停止远程进程。

## 9. 文件同步与上下文一致性

### 9.1 首次创建

宿主目录可以为空。worker 将宿主目录打成 tar.gz，上传到沙箱 `/workspace`。

demo 不从用户本地上传项目 fixture。应用文件由 Agent 在远程创建。

### 9.2 后续 run

有效沙箱继续运行时，不再每轮上传和替换 `/workspace`。否则沙箱内未同步修改会被旧宿主副本覆盖。

### 9.3 每轮回收

远程目录下载为完整归档。下载内容先进入宿主 staging 目录。

提交前必须检查：

- workspace ID 和 fence 仍有效。
- 未压缩归档没有超过 `sandbox-archive-max-bytes`。
- 没有绝对路径和 `..` 逃逸。
- 没有逃逸 symlink。
- 文件权限和目录结构可恢复。
- `.git` 仍由宿主保留。

提交成功后才能将 run 标记成功。同步失败时，宿主旧目录不得覆盖远程新目录。

### 9.4 上下文缓存

`agentbuild` 的项目上下文、技能和权限配置从宿主读取。每次成功同步后，必须使该工作区的相关缓存失效，或把 `data_revision` 纳入缓存 key。

否则第二轮 run 可能读取第一轮之前的项目规则。

### 9.5 文件树和预览

现有文件树和预览继续读取宿主权威副本。它们展示的是最近一次成功同步的 `data_revision`，不是沙箱内未提交的实时文件。

API 响应应携带该 revision，避免用户误解。

## 10. Provider 与 adapter 接口

### 10.1 woca 内部能力接口

不把生命周期方法塞入所有工具的 `Executor`。在 provider/worker 管理层增加能力接口：

- `Executor`
- `WorkspaceArchiver`
- `PersistentRuntime`
- `ManagedServices`
- `IsSandbox() bool`

`contract-http` 和 `cfbridge` 保持原有临时模式。请求持久模式但 backend 不支持时必须报错。

`IsSandbox()` 必须保留。file-history undo、worktree、MCP stdio 和 LSP 等宿主能力门禁继续生效。持久化不等于开放这些能力。

新增 backend type：`openshell`。该类型表示 woca 通过受管 adapter 使用 OpenShell。

### 10.2 adapter 的逻辑操作

| 操作 | 必要语义 |
|---|---|
| Create ephemeral | verify 专用，返回临时沙箱 |
| Ensure runtime | 按稳定名称创建或获取沙箱，支持 idempotency key |
| Start runtime | 启动停止的沙箱，返回新 generation |
| Stop runtime | 停止计算，保留环境 |
| Delete runtime | 幂等删除沙箱 |
| Exec | argv、cwd、env、stdin、timeout、独立 stdout/stderr |
| Start process | 返回 process/session handle |
| Process I/O | stdin、输出游标、kill、EOF |
| Restore archive | 分块 stdin 解包到 `/workspace` |
| Export archive | 流式输出 `/workspace` 的 tar.gz |
| Start service | 启动 managed process，并建立 OpenShell service endpoint |
| Stop service | 停止指定服务，不影响其他服务 |
| Route service | 将 woca 已鉴权的 HTTP 请求转发到指定沙箱服务 |

OpenShell gateway 的单个入站 gRPC message 上限为 1 MiB。归档必须分块。adapter 还要遵守 supervisor 的 pending stdin 上限，并实现背压。

### 10.3 adapter 的进程归属

每个进程必须带以下内部标签：

- workspace ID
- run ID 或 service ID
- fence token 或 process generation
- `run-scoped` 或 `managed-service`

Release 只清理对应 run 的进程。它不能杀死 desired state 为 running 的 managed service。

### 10.4 adapter 重启

adapter 的 exec 输出游标可以只保存在内存。丢失这些游标时，当前 run 失败并进入恢复流程。不能伪造输出或自动重放。

OpenShell 沙箱和 managed service 的定义仍在。adapter 重启后通过 gateway 和 woca 数据库重新核对资源。

## 11. Web 服务闭环

### 11.1 不直接公开 OpenShell URL

OpenShell 的远程 service URL 需要 gateway 身份。终端用户没有该身份。

woca 提供路径型代理：

```text
/api/execution-workspaces/{workspace_id}/services/{service_name}/
```

该路径只映射数据库中已登记的服务。它不是通用 HTTP 代理。

### 11.2 鉴权

- 读取页面需要 workspace read 权限。
- 启动、停止和删除服务需要 workspace write 权限。
- server 校验 tenant、user 和 workspace 归属。
- adapter 和 OpenShell 端口不向用户网络开放。
- 其他用户访问同一 URL 必须得到 403。
- 客户端不能通过 Host、URL 或 service name 选择任意上游。

首期只支持 HTTP 请求和静态前端资源。WebSocket、任意 TCP 和 Server-Sent Events 长连接不在 demo 范围内。

### 11.3 工作区写入边界

首期 managed service 只能用于不在 run 之外修改 `/workspace` 的 HTTP 应用。服务可以写日志和临时目录。需要数据库、上传或服务端持久写的应用不在首个闭环内。

demo 的 Python 服务只读取代码和返回页面。后续支持服务写工作区时，必须先定义一致性快照或停写同步，不能用宿主旧 revision 覆盖服务写入。

### 11.4 Stop/Start 后的服务

Stop 会停止服务进程。Start 后，reconciler 按 `desired_state=running` 重新执行服务命令。

新进程的 PID 和 process generation 可以改变。页面内容来自持久代码，不来自旧进程内存。

## 12. EE 默认启用与部署

### 12.1 Compose 服务

在 `deploy/selfhosted/docker-compose.full.yml` 增加：

- `openshell-init`
- `openshell-gateway`
- `openshell-adapter`
- `openshell-bootstrap`

网络：

- adapter 加入 `woca-app`，供 server 的服务代理访问。
- adapter 加入 `woca-infra`，供 config 和 worker 访问。
- adapter 和 gateway 加入独立的 `openshell-control`。
- gateway 不加入 woca 业务网络。

端口：

- woca 业务入口继续使用 `8080`。
- OpenShell gRPC 只发布到宿主 `127.0.0.1:18080`。
- OpenShell health 只发布到宿主 `127.0.0.1:18081`。
- Docker driver 的 `grpc_endpoint` 同步指向 `127.0.0.1:18080`。

OpenShell gateway 使用 mTLS。证书 SAN 必须覆盖 `openshell-gateway` 和 `127.0.0.1`。adapter 使用专用客户端证书。

woca 到 adapter 的内部 API 使用独立 Bearer key。adapter 不发布宿主端口。该 key 与 OpenShell 客户端证书分开保存和轮换。

### 12.2 镜像和配置

首期固定 OpenShell `0.1.5` 的 gateway、supervisor 和 sandbox runtime。生产部署固定 digest。

设置：

```text
OPENSHELL_TELEMETRY_ENABLED=false
```

woca 专用 sandbox image：

- `WORKDIR /workspace`
- 非 root `USER`
- 包含 shell、tar、gzip、grep、find 和 Python 标准库
- 不包含 woca 模型密钥
- 固定 image digest

demo 使用 Python 标准库实现 Web 应用和 `unittest`。这样不需要依赖外部包仓库。

### 12.3 宿主卷

server 和所有 worker 挂载：

```text
/var/lib/woca/workspaces:/var/lib/woca/workspaces
```

OpenShell gateway 使用：

```text
/var/lib/openshell:/var/lib/openshell
/var/run/docker.sock:/var/run/docker.sock
```

第二个路径只给 gateway。

### 12.4 默认 backend bootstrap

fresh EE 部署时，`openshell-bootstrap` 通过 config API 完成：

1. 等待 config 和 adapter 可用。
2. 创建 `openshell-ee` backend。
3. 保存 adapter API key 到现有加密 secrets。
4. 执行全量 verify。
5. 启用该 backend。
6. 将默认 execution mode 设置为 `persistent_openshell`。

只有没有任何 backend 配置时，bootstrap 才自动创建和启用。已有部署的手工配置不能被重启覆盖。

bootstrap 失败必须让部署检查失败。不能显示“安装成功”但让首个 run 再失败。

模型设置不属于 bootstrap。生产部署的模型由操作者提供。开发者路径用 `sandbox-init` 先发布模型设置，见 12.7。

个人版和其他 backend 的行为不变。

### 12.5 默认拒绝本地执行

EE off-host 姿态继续保持：

- 无 enabled backend：创建远程 run 返回 403。
- 无合格 worker：返回 503。
- adapter 或 OpenShell 失败：当前 run 失败。
- 不支持持久模式的 backend：明确报错。
- 任何情况下都不静默使用 worker 本地 Executor。

### 12.6 开发者一条命令路径的服务清单

OpenShell 的全部服务直接写进企业版 `deploy/selfhosted/docker-compose.full.yml`。不另建 compose 文件。

生产栈常驻的服务：

| 服务 | 作用 | 网络 | 宿主挂载 |
|---|---|---|---|
| `openshell-init` | 一次性生成 JWT 与 TLS 材料 | `openshell-control` | `/var/lib/openshell` |
| `openshell-gateway` | OpenShell 控制面。持有 Docker socket | `openshell-control` | Docker socket、`/var/lib/openshell`、gateway.toml |
| `openshell-adapter` | 唯一 OpenShell 客户端。实现归档、进程和服务转发 | `woca-app`、`woca-infra`、`openshell-control` | 客户端证书，只读 |
| `openshell-bootstrap` | 一次性注册、verify 并启用 backend | `woca-infra` | 无 |

`demo` profile 才启动的服务：

| 服务 | 作用 | 网络 | 宿主挂载 |
|---|---|---|---|
| `fakemodel` | 脚本模型。复用 `e2e/sandboxlab/fakemodel` | `woca-infra` | 无 |
| `sandbox-init` | 一次性写入模型设置 | `woca-infra` | 无 |
| `sandbox-demo` | 跑完整断言后退出。退出码即结论 | `woca-app`、`woca-infra` | 共享工作区卷 |

另外新增共享卷 `woca-workspaces`，挂到 `woca-server`、`woca-worker` 和 `sandbox-demo` 的 `/data`。这同时修掉企业栈当前缺少共享工作区的问题。

demo 容器不挂 Docker socket。它只能通过 woca API、adapter 的只读接口和共享卷观察结果。

### 12.7 启动顺序

worker 启动时会构造模型。provider 为空时构造直接失败，worker 会退出。所以模型设置必须先于 worker 存在。

Compose 的 `depends_on` 图无法表达“先写配置再起执行面”。因此开发者路径分四段，由一个 Make 目标驱动。

```text
阶段 1  基础面   pg、nacos、nacos-init、woca-config、woca-server-1、woca-edge、
                 openshell-init、openshell-gateway、openshell-adapter、fakemodel
阶段 2  配置面   sandbox-init 写入模型设置；openshell-bootstrap 注册并启用 backend
阶段 3  执行面   woca-worker-1、woca-worker-2
阶段 4  验收面   sandbox-demo
```

`sandbox-init` 做两件事：

1. 通过 config API 写入模型设置。provider 指向 `http://fakemodel:8080`。
2. 等待 config 发布新 pack，再退出。

config 控制台默认开放。`.env` 里设了 `WOCA_CONSOLE_USER` 和 `WOCA_CONSOLE_PASSWORD` 时，`sandbox-init` 必须先登录再写入。

`sandbox-init` 写入的模型 key 是脚本模型的固定值。它不进入任何日志或 run 事件。

`openshell-bootstrap` 做的事：

1. 等待 config 和 adapter 健康。
2. 确认没有已存在的 backend。已有则跳过，不覆盖人工配置。
3. 创建 `openshell-ee` backend。endpoint 指向 adapter 内部地址。
4. 执行全量 verify。verify 会真实创建、调用、归档并销毁一个 OpenShell 沙箱。
5. 启用该 backend。
6. 失败时以非零退出，并打印 OpenShell 的原始错误。

开发者入口是 `make ent-sandbox-demo`。该目标按四段顺序执行，并汇总每段结果。

### 12.8 demo 容器内的步骤

demo 容器是一个 Python 脚本，只用标准库。它按顺序完成：

1. 等待 config、adapter、OpenShell gateway 健康。
2. 通过 config API 确认 backend 已启用，且最近一次 verify 成功。
3. 在 server 的开放 console 上铸造租户 key。沿用 sandboxlab 的做法。
4. 提交一个 run。workspace 指向共享卷。
5. 遇到 503 时按间隔重试。worker 的 capability ack 每 10 秒刷新一次。
6. 轮询 run 的事件流。出现 error 事件立即失败并打印事件内容。
7. 断言共享卷出现脚本模型写出的产物文件。
8. 通过 adapter 的只读接口断言 OpenShell 侧确实存在过沙箱，并记录沙箱名和状态。
9. 打印结论摘要和后续操作提示。成功退出 0，失败退出 1。

演示流程不包含 403 姿态。bootstrap 已经启用了 backend，403 只在没有任何 backend 时出现。该姿态由 sandboxlab 覆盖。

### 12.9 宿主前提

| 前提 | 说明 |
|---|---|
| Docker 28+ | Docker Desktop 或 Docker Engine |
| Landlock ABI 3+ | Linux 6.2+ 且 Landlock 启用 |
| seccomp user notification | 内核提供，OpenShell 会主动探测 |
| macOS 额外要求 | Docker Desktop，并开启 host networking |
| 端口 | 宿主 18080 和 18081 空闲 |
| 磁盘 | OpenShell 三个镜像约 2 GB |

OrbStack 不在 OpenShell 官方支持矩阵内。开发者路径不能因此变成不可用。真实探测发生在第一步：verify 会创建沙箱，OpenShell 会做资格探测并给出原因。

adapter 必须把 OpenShell 的资格失败翻译成可执行提示。例如内核缺少 Landlock 时，提示应写明需要 Linux 6.2+ 且启用 Landlock，而不是只回一句创建失败。

### 12.10 与生产启动的关系

- 生产启动不带 profile。OpenShell 四个服务照常启动。
- 开发者路径用 `make ent-sandbox-demo`，它只额外启动 demo profile 的服务。
- bootstrap 只在没有任何 backend 配置时创建和启用。已有部署的人工配置不被重启覆盖。
- demo profile 不改变生产服务的环境变量和卷。
- 同一个 compose 文件同时服务开发和生产。差异只在 profile 和 `.env` 开关。

### 12.11 开发者路径的验收清单

demo 容器内的断言：

- config API 中出现 `openshell-ee` backend，且 verify 全步骤通过。
- 租户 key 铸造成功。
- run 提交成功，且没有出现本地执行。
- 共享卷出现脚本模型写出的约定产物文件。
- adapter 报告本次 run 用到的 OpenShell 沙箱。
- demo 容器退出码为 0。

由 Make 目标或 e2e 断言：

- `docker compose -f docker-compose.full.yml config` 通过。
- 四段启动全部成功。
- worker 日志中没有本地 Executor 执行的记录。
- `docker compose down` 后没有遗留 workload 或 supervisor 容器。

## 13. 安全边界

1. woca 是用户侧唯一授权入口。
2. adapter 是 OpenShell 用户凭据的唯一持有者。
3. OpenShell gateway 不接受来自用户网络的请求。
4. Docker socket 不出现在 worker、server 或 adapter 上。
5. sandbox 内不放 woca 的模型密钥。
6. 默认网络策略 deny-by-default。
7. demo 不需要沙箱外网访问。
8. 服务代理只访问数据库登记过的 workspace 和 service。
9. 所有管理操作校验资源归属和 fence。
10. API key、证书和私钥不进入日志、run event 或页面响应。
11. 归档恢复必须阻止路径和 symlink 逃逸。
12. 同一 OpenShell workspace 不代表用户可共享文件。文件隔离仍由独立 sandbox 保证。

首期采用 woca 统一授权模型。OpenShell 不再对每个终端用户做二次 membership 校验。后续可以增加该层纵深防御。

## 14. 故障语义

| 故障 | 必须行为 |
|---|---|
| adapter 不可达 | run 失败，不回退本地 |
| OpenShell 创建超时 | 按 idempotency key 查询，不能盲目重复创建 |
| 首次上传失败 | run 失败，回收未确定资源 |
| 每轮下载失败 | run 不能成功，保留远程现场，进入 recovering |
| worker 失去 lease | 取消远程 exec，旧写入停止前阻塞下一 run |
| adapter 重启 | 当前 exec 可失败，沙箱定义不丢失，禁止自动重放 |
| gateway 重启 | 从持久状态核对沙箱，不创建同名替代品 |
| Stop 与新 run 并发 | 通过状态条件和 fence 使其中一方等待或失败 |
| 旧 worker 完成较晚 | fence 不匹配，禁止覆盖宿主新 revision |
| managed service 崩溃 | 记录 actual_state 和错误，不自动删除工作区 |
| 远程沙箱丢失 | 只有确认旧写入停止后，才能从宿主 revision 重建 |
| 无法确认旧写入停止 | 保持 recovering，报告需要恢复，不启动新 writer |

首期不承诺自动处理所有故障。无法安全自动恢复时，必须停在明确状态。

## 15. 可观测性

新增指标：

- workspace 各状态数量
- active run 和 queued run 数量
- Acquire、Create、Start、Stop 时长
- archive 上传和下载的字节数、时长、失败数
- recovering workspace 数量
- managed service 状态和重启次数
- adapter 到 gateway 的请求错误率
- service proxy 的响应状态和延迟
- orphan sandbox 和 orphan process 数量

日志字段：

- tenant ID、workspace ID、run ID
- backend ID、sandbox name、runtime generation
- fence token、data revision
- service ID、process generation

日志不能记录提示词、代码内容、API key 或证书私钥。

关键操作增加 OTel span：Acquire、Execute、ExportArchive、CommitRevision、Release、Stop、Start 和 RouteService。

## 16. 远程 Demo 脚本化验收

demo 分两级。开发者路径用第一级。完整闭环用第二级。

| 级别 | 模型 | 触发方式 | 证明内容 |
|---|---|---|---|
| 一级 | 脚本模型 | `make ent-sandbox-demo` | 沙箱调用链路真实可用。产物落到共享卷 |
| 二级 | 真实模型 | 操作者在控制台配置 key 后执行 | 跨轮次复用、页面可访问、Stop/Start |

### 16.1 前置条件

- 远程 Linux 满足 Docker 28+。
- Landlock 和 seccomp 探测通过。
- 用户本地目录没有项目代码。
- 用户只有 woca URL 和访问凭据。
- EE 使用完整 Compose 和两个 worker。
- OpenShell 使用真实镜像，不用 fakesandbox。

### 16.2 第一轮：从空目录创建应用

用户创建空 ExecutionWorkspace，并提交提示词：

> 使用 Python 标准库创建一个 HTTP 应用。监听 127.0.0.1:8000。首页返回“OpenShell EE Demo v1”。提供 app.py、test_app.py 和 README.md。使用 unittest 覆盖首页，并运行测试。在 `$HOME/.woca-ee-demo-runtime-marker` 写入 `runtime-v1`，不要把它放进项目目录。

验收：

1. run 成功。
2. 测试在 OpenShell 内通过。
3. 宿主出现三个文件和新的 data revision。
4. 文件树能看到代码。
5. 沙箱仍为 ready，没有被 Delete。
6. workspace 外的 runtime marker 内容为 `runtime-v1`，供下轮核验。

通过服务管理 API 启动 `web`：

```text
argv = ["python3", "app.py"]
cwd = "."
port = 8000
```

验收：

- 返回 woca 鉴权 URL，不返回 OpenShell 直连地址。
- 浏览器显示 `OpenShell EE Demo v1`。
- 记录 sandbox name、runtime generation 和 service process generation。

### 16.3 第二轮：复用环境修改页面

从另一个聊天 session 选择同一 ExecutionWorkspace，提交：

> 将首页文本改为“OpenShell EE Demo v2”。更新对应测试并运行。保持其他文件不变。

验收：

1. sandbox name 不变。
2. runtime generation 不变。
3. 第一轮 runtime marker 仍存在。
4. 本轮没有发生全量 restore。
5. 测试通过。
6. 页面显示 `OpenShell EE Demo v2`。
7. managed service process generation 不变。
8. 宿主文件更新，data revision 增加。
9. 第二个聊天的历史仍独立保存。

### 16.4 Stop/Start

1. 停止 workspace。
2. 确认 URL 不再可用。
3. 确认宿主代码仍在。
4. 启动 workspace。
5. 确认 runtime generation 增加。
6. 确认 managed service 按 desired state 重启。
7. 确认页面仍显示 v2。
8. 接受 service process generation 改变。

### 16.5 权限和清理

- 另一个用户访问文件和服务 URL，均返回 403。
- sandbox 访问未允许的外网地址，连接被拒绝。
- 停止并删除 demo runtime。
- 默认保留宿主代码。
- 独立确认删除 workspace 数据后，才删除宿主目录。
- Docker 中没有遗留 demo workload 或 supervisor。

## 17. 测试策略（TDD）

所有实现任务先写失败测试，再写最小实现。

### 17.1 单元测试

| 模块 | 必测行为 |
|---|---|
| workspace schema | 默认工作区唯一，sandbox name 唯一，非法状态被拒绝 |
| Claim | 同工作区串行，不同工作区并行，fence 原子写入 |
| Heartbeat/Release | 错误 worker 或 fence 不能续期、释放或提交 |
| Reaper | 持久 run 过期后不直接 requeue |
| provider | OpenShell 实现 Executor、archiver、PersistentRuntime 和 IsSandbox |
| adapter | idempotent create、进程归属、背压和输出游标 |
| archive | staging、原子提交、大小限制、`.git` 保留和逃逸拒绝 |
| context cache | revision 提交后失效 |
| service proxy | 归属校验、路径映射、非登记服务拒绝 |
| bootstrap | 仅空配置自动启用，已有配置不被覆盖 |

### 17.2 集成测试

1. 两个 worker、两个聊天、同一工作区：只有一个 run 执行。
2. 两个不同工作区：可以并行执行。
3. 首次并发 Acquire：只创建一个有效沙箱。
4. 第二轮 Acquire：不 Delete，不重复 Create，不全量 Restore。
5. 下载失败：不提交宿主 revision，不报告 run 成功。
6. lease 丢失：旧写入停止前不启动下一 run。
7. Stop 与 Acquire 竞争：不能出现半启动状态。
8. adapter 重启：不盲重放，不伪造输出。
9. verify 临时沙箱：仍被完整清理。
10. 现有 `contract-http`、`cfbridge` 和个人版行为不回归。

### 17.3 远程 e2e

新增独立 build tag 和 Make 目标，避免普通 `go test ./...` 需要 Docker。

e2e 使用真实 full Compose 和 OpenShell。覆盖第 16 节的全部 demo 步骤。

另有一套面向开发者的 e2e。它在企业版 compose 文件上跑 demo profile。断言内容与 12.11 一致。它的作用是让任何人克隆仓库后能自己验证整条链路。

网络失败要区分：

- 远程宿主或 Docker 不满足前提：测试环境失败。
- OpenShell 能启动但 woca 链路失败：产品测试失败。

不能把缺少企业宿主当作普通 skip 来掩盖交付要求。

## 18. 里程碑与完成标准

| 里程碑 | 内容 | 退出条件 |
|---|---|---|
| M0 | 企业版 compose 加 OpenShell 服务、共享工作区卷和 preflight | compose 静态校验通过，OpenShell 官方最小沙箱在同宿主跑通 |
| M1 | adapter 的临时 contract、exec、archive 和 verify | 现有 contract 与 verify 测试通过 |
| M2 | `make ent-sandbox-demo` 四段启动与 demo 容器断言 | demo 退出码 0，12.11 全部通过 |
| M3 | ExecutionWorkspace、持久绑定、串行和 fencing | 两 worker 的并发与故障测试通过 |
| M4 | Acquire/Release、宿主 revision 和 Stop/Start | 第二轮复用与同步测试通过 |
| M5 | managed service 和 woca 鉴权代理 | Web URL、权限和重启测试通过 |
| M6 | EE 默认 bootstrap 和二级真实模型 demo | 第 16 节二级全部通过 |
| M7 | 故障注入、观测、部署文档和清理 | 无本地回退，无孤儿资源 |

最终完成不是“容器启动了”。最终完成有两条线：

- 开发者线：一个人克隆仓库，执行 `make ent-sandbox-demo`，在企业版 compose 上拿到一个退出码为 0 的沙箱调用结果。
- 完整线：用户本地无代码，从空工作区生成应用，跨轮次复用环境，页面可访问，宿主代码可恢复，故障不会静默改变执行边界。

## 19. 主要代码改造面

- `internal/ee/worker/`：Acquire、Release、恢复和 managed service 协调。
- `internal/runstate/`：workspace-aware Claim、Heartbeat、Finish 和 reaper。
- `internal/runstate/snapshot.go`：workspace ID、mode 和 fence。
- `internal/server/`：工作区 API、服务 API、鉴权代理和 run 门控。
- `internal/server/session.go`：session 到 ExecutionWorkspace 的关联。
- `internal/wocadb/`：新表、约束和迁移。
- `internal/platform/executor/providers/`：`openshell` backend 注册和能力接口。
- `internal/agentbuild/`：同步后的上下文缓存失效。
- `internal/ee/configsvc/`：OpenShell backend、verify 和 bootstrap 支持。
- `ee/configsvc-web/`：backend 状态与持久能力展示。
- `deploy/selfhosted/`：OpenShell 服务、网络、卷和默认配置。
- `deploy/selfhosted/docker-compose.full.yml`：OpenShell 四个服务、共享工作区卷、demo profile。
- `deploy/selfhosted/openshell/`：gateway.toml、证书生成和 adapter 镜像构建。
- `deploy/selfhosted/demo/`：demo 容器脚本和 Make 目标。
- `e2e/`：真实 OpenShell 的远程闭环套件。

adapter 作为独立组件存放。其精确模块路径、API 路径和内部状态结构在实施计划中确定。

## 20. 后续实施计划的输入

实施计划应按 M0 到 M7 排序。每个任务必须包含精确文件、先失败的测试、最小实现和验证命令。

实施前先建立隔离 worktree，并记录测试基线。任何中途设计变化都要回到本 spec 修订，不能在实现中静默改变生命周期或安全边界。

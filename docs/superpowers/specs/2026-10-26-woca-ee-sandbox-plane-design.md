# woca 企业版沙箱执行面：自建轻量方案（设计）

日期：2026-10-26
状态：设计初稿，待评审
取代：`docs/superpowers/specs/2026-10-24-openshell-ee-persistent-sandbox-design.md`（OpenShell 路线作废，角色划分与存储模型一并重写）
前置调研：`docs/openshell-compose-research.md`、`sandbox-lifecycle-routing-research.md`

## 1. 目标

个人开发者用一个企业版 compose 文件，就能跑通企业版沙箱调用。调用在一个 container 内完成。沙箱实现在 woca 侧自建，不引入外部一揽子方案。

验收标准：

1. `make ent-sandbox-demo` 之后，demo 容器退出码为 0。
2. demo 容器内完成：铸租户 key、提交 run、等待结果、断言产物。
3. 用户本地没有项目代码。代码在远程沙箱里生成。
4. 沙箱后端的参数在 woca 管理台里改，改完不用重启任何角色。
5. 任何沙箱环节失败，run 明确失败，不回退本地执行。

## 2. 已定决策

| 决策点 | 选择 |
|---|---|
| 沙箱实现 | woca 自建。沙箱就是容器，隔离靠 container runtime |
| 隔离档位 | 默认 runc，可选 gVisor。不做 microVM |
| 角色 | server → woca-scheduler，worker → woca-executor，新增 executor-proxy |
| 编排位置 | scheduler 跑 agent 循环，调用工具时走远程沙箱 |
| 执行面角色 | executor 是沙箱宿主，暴露 contract v2 HTTP 服务，持有工作区目录 |
| 代码权威 | executor 磁盘上的工作区目录 |
| 配置方式 | woca-config 管理台。不用配置文件，不新建配置服务 |
| 热生效 | 配置写入后经 pack v2 下发。executor 内存配置即时更新，新建沙箱用新参数 |
| 后端可插拔 | provider 抽象保留。工作区不记录后端类型 |
| 出口管控 | 首期不做。沙箱 `network=none`，无外网。模型调用在调度器侧 |
| 硬隔离 | 首期不做。最高档是 gVisor，容器级内核共享 |
| runtime 访问权 | executor 跑在容器里，直连宿主 runtime socket。权限收窄后续做 |
| 工作区 ID | proxy 生成。ID 是执行环境的身份，不是聊天 session 的 ID |
| 工作区放置 | proxy 按负载选 executor，绑定持久化。同一工作区固定落在一个 executor |
| 工作区存储 | 共享卷。scheduler 与 executor 挂同一路径，文件树现有实现直接可用 |
| executor 规模 | 可横向扩容。扩容出的实例只接新工作区 |
| 个人版 | 不接入这条路径。个人版继续本地执行 |
| 外部方案 | 全部不接：OpenShell、E2B、Daytona、Runloop、CubeSandbox、OpenSandbox、agent-sandbox |

### 2.1 为什么自建

自建换来三件事：

1. 控制面只有一套。沙箱生命周期、策略、凭据、路由都在 woca 手里，不与外部控制面重叠。
2. 组件数量最少。executor 一个进程 + 沙箱容器。没有 gateway、supervisor、adapter、egress sidecar。
3. 抽象由 woca 定义。contract v2 已经是 woca 自己的线格式，外部实现可以后接。

代价：

1. 出口管控要自己做。第一版用 `network=none`，沙箱无外网。
2. 隔离不是硬件级。跑真正不可信的第三方代码时不够。
3. 沙箱不能跨节点迁移。容器留在创建它的宿主。

### 2.2 明确砍掉的东西

| 砍掉 | 原因 | 后续 |
|---|---|---|
| OpenShell gateway / supervisor / adapter | 三件套加每节点控制面，与 woca 职责重叠 | 不接 |
| ExecutionWorkspace 独立表 | 工作区就是 executor 磁盘上的一个目录，不需要新实体 | 不建表 |
| runtime 状态机（provisioning/ready/stopped/recovering…） | 沙箱是容器，状态由 container runtime 给。executor 侧不需要第二套状态机 | 不做 |
| fence token 与代次比较 | 同一工作区不会有两个写者。`runs` 表的部分唯一索引加 executor 侧文件锁就够 | 多副本并发写时再引入 |
| managed service 与端口代理 | 首期不需要在沙箱里跑常驻服务 | 需要时按端口映射补 |
| 快照、挂起、恢复、分支 | 容器级没有这些语义 | 不做 |
| 每租户策略、每 workspace 后端绑定 | 工作区不记后端。后端参数是平台策略 | 不做 |
| 沙箱复用（跨 run 保留同一容器） | 首期 per-run 创建销毁。代码靠工作区目录延续 | 需要时按 driver 能力位加 |
| 独立配置服务 | 配置面已有 woca-config 与 pack 通道 | 不做 |

## 3. 角色与数据流

```
用户 / API 客户端
        │
        ▼
woca-scheduler ×2 ── 编排 agent 循环、授权、写 run 事件
        │  执行请求（contract v2）
        ▼
executor-proxy ── 流控、路由
        │
        ▼
woca-executor ×N ── contract v2 沙箱服务、容器管理、工作区目录
        │
        ▼
沙箱容器（runc 或 gVisor）
```

代码与产物的流向：

```
executor 磁盘工作区目录 ──归档上传──▶ 沙箱容器 /workspace
executor 磁盘工作区目录 ◀──归档下载── 沙箱容器 /workspace
```

调度器不碰文件字节。文件只在 executor 与沙箱之间流动。

### 3.1 executor 的定位

executor 是一个普通的服务进程，职责只有四件：

1. 暴露 contract v2 的沙箱服务。
2. 用 container runtime 起容器、注入执行命令、回收容器。
3. 持有工作区目录，做归档上传与下载。
4. 提供工作区文件树与文件读取（管理台与前端要用）。

executor 不跑 agent 循环，不持有模型 key，不做授权判断。

### 3.2 executor-proxy 的定位

proxy 是调度器、队列与流控合在一起的转发层。四件事：

1. **生成工作区 ID 并放置。** 新工作区第一次来时，proxy 生成 ID，选一个 executor，把绑定写进数据库。
2. **亲和路由。** 已有工作区按绑定表走，回到同一个 executor。
3. **准入与流控。** 每个 executor 有并发上限。超限的请求进有界队列，队列满则快速失败。
4. **负载感知的选择。** 新工作区分配给当前在跑 run 最少的 executor。

proxy 无状态。它不持有会话，不缓存文件，不保存工作区内容。绑定关系在数据库里，负载信息在 `config_acks` 表里。

### 3.3 队列与调度模型
微服务里有四种常见做法：

| 做法 | 特点 | 我们的取舍 |
|---|---|---|
| 拉取式工作队列 | 任务方轮询领任务。谁空闲谁领，天然均衡，但没有放置决策 | woca 现在就是这个。任务是整个 run，没有亲和需求 |
| 推送式调度器 | 中心选目标再派发。掌握全局状态，能做亲和与容量分配 | 采用。任务是工具调用，必须回到固定 executor |
| 一致性哈希 | 按 key 哈希到实例。无需中心表，扩缩容要重映射 | 不采用。绑定要跨 proxy 重启保持，哈希做不到 |
| 舱壁隔离加快速失败 | 每实例自己限流，超出直接拒绝，不排队 | 采用，作为有界队列的兜底 |

我们的组合是：**推送式调度器 + 持久化绑定 + 舱壁限流 + 有界队列**。

具体规则：

1. **放置**：新工作区按在跑 run 数最少选 executor，在跑数相同则按已分配工作区数最少。选完写绑定表。
2. **亲和**：绑定一旦建立就不改。同一个工作区的所有工具调用都回同一个 executor。
3. **舱壁**：每个 executor 一个并发上限。超限的请求进该 executor 的队列。
4. **有界队列**：每条队列有长度上限与排队超时。两者任一越界，返回 503。
5. **公平**：每个租户在 proxy 侧的排队额度有上限。一个租户不能占满全部队列。
6. **负载上报**：executor 每 10 秒刷新自己的 ack，带上当前在跑 run 数。复用现有的 `config_acks` 表与 15 秒存活窗口。
7. **不健康**：ack 超过存活窗口的 executor 视为不健康。绑定到它的工作区标记为待恢复。

横向扩容有一条好处：工作区目录在共享卷上，不绑 executor。新 executor 挂同一个卷就能接管已有工作区，只要改绑定记录。不用拷贝目录。

### 3.4 调度器不建第二个 agent

worker 今天的执行循环是这样调的（`internal/ee/worker/worker.go`）：

```go
final, err := w.agent.Invoke(ctx, history, run.ThreadID)
```

`agent.Invoke` 接受 run 作用域的 ctx。调度器已有的 agent 同样可以这样调。所以调度器接管编排后：

1. 沿用 `internal/ee/worker` 的 claim、事件镜像、transcript 保存逻辑。
2. 用调度器自己那一个 agent 调 `Invoke`。
3. 在 ctx 里注入 run 作用域的远程沙箱 executor（`runexec.WithRunExecutor`）。

不建第二个 agent，不重写执行循环。agent 本身支持并发 run：run 作用域的 workspace 是 ctx 上的值装饰，system prompt 环境、文件与 shell 工具、记忆预取都按它解析。

### 3.5 门控从 worker 改成 executor

现在远端租户能不能建 run，看的是有没有活的 sandbox worker。`internal/server/runs.go` 的 `enqueueRun` 里调 `hasLiveSandboxWorker`，没有就返回 503。能力标记来自 worker 的 ack（`cmd/woca-worker/main.go` 的 `workerCapabilities`）。

EE 改成 executor 之后，这套要换：

1. executor 注册到 nacos，声明 `sandbox` 能力。
2. 调度器查 executor 的在线 ack，替代 `hasLiveSandboxWorker`。
3. 403 的语义不变：没有 enabled backend 时，远端租户不能建 run。

## 4. 沙箱契约
现有 contract v2（`internal/platform/executor/CONTRACT.md`，15 个端点）继续用。要点：

1. 调度器侧的 client 是现成的 `executor.NewSandbox`（`internal/platform/executor/sandbox.go`），已经实现 `Executor` 接口加 `PutArchive` / `GetArchive`。
2. executor 侧要实现同样的 15 个端点。现成的服务端参考是 `internal/platform/executor/fakesandbox/server.go` 的 `Handler()`：它把 15 条路由挂在 `http.ServeMux` 上，executor 的服务照这个形状写，差别只在容器操作换成真实调用。
3. `create` 的 `cwd` 语义要明确：它承载的是**工作区 ID**，不是路径。executor 把它映射到自己的 `workspace_root/<id>` 目录，并校验 ID 只含安全字符集（字母、数字、连字符、下划线），拒绝 `/`、`..` 与绝对路径。调度器因此不需要知道 executor 的磁盘布局。

client 侧要改一处：`internal/platform/executor/sandbox.go` 的 `create` 现在固定发 `{"cwd": "/workspace"}`。它要改成发本次 run 的工作区 ID。`CONTRACT.md` 里 `cwd` 的描述同步改写。

需要加一个端点：

| 操作 | 端点 | 语义 |
|---|---|---|
| 同步工作区 | `POST /sandbox/{id}/workspace/sync` | 把沙箱内 `/workspace` 同步回 executor 磁盘的工作区目录。返回成功或失败 |

理由：产物必须由调度器确认收到才能算 run 成功。同步放在 executor 内部、由 executor 主动完成，调度器就看不到失败。

这个端点是 contract v2 的增补。`fakesandbox` 与 `contract` 测试要同步更新。

上传方向不需要新端点。executor 在 `create` 返回前把工作区目录同步进容器，这是 executor 的内部动作。

`archive` 两个端点保留 contract 原语义（请求体与响应体都是 tar.gz 流），用于手工导入导出。调度器不调用它们，因为调度器不碰文件字节。

其余端点语义不变：

- `exec` 走 `docker exec`，argv、cwd、env、stdin、超时、独立 stdout/stderr、退出码。
- `start` / `output` / `input` / `kill` 对应容器内的长进程。
- `read` / `write` / `stat` / `mkdir` / `glob` / `grep` 走容器内的 `cat`、`sh`、基础工具。
- `archive` 保持原语义：请求体是 tar.gz 流。用于一次性导入导出，不是工作区同步。

## 5. executor 侧实现

### 5.0 宿主、executor 与沙箱的关系

```text
宿主：跑 container runtime 的机器（物理机 / 云主机 / VM）
  └─ container runtime：runc / runsc / kata。隔离强度由它决定
       ├─ executor 容器：woca 角色。建容器、exec 命令、管工作区
       └─ 沙箱容器：用户代码进程
```

三点要分清：

1. executor 不是虚拟化宿主。它是一个普通容器，和沙箱容器是兄弟。
2. 隔离由 container runtime 提供，不由 executor 提供。换 runtime 就换隔离强度。
3. executor 持有 runtime 的全量 API 权限，比沙箱更靠近信任边界。所以它所在的节点必须完全受信。

Kata 模式下沙箱会多一层轻量虚拟机。首期不做。

### 5.1 容器

executor 用 container runtime 的 API 管理容器：

1. 按配置的镜像与资源限制创建容器。
2. 工作目录指向容器内 `/workspace`。
3. 隔离档位由 runtime 决定：`runc` 或 `runsc`。
4. 网络默认 `none`。
5. 容器不注入 woca 的二进制。执行命令靠 runtime 的 exec 接口。

不注入 woca 二进制的好处：沙箱里没有 woca 的代码，也没有 woca 的凭据。代价是文件操作要靠容器内的 `sh`、`cat`、`find`、`grep`、`tar`。所以基础镜像要固定带这些工具。

### 5.2 执行命令

`exec` 映射到 runtime 的 exec 接口。要求：

- stdout 与 stderr 分开。
- stdin 可写。
- 退出码透传。
- 超时后杀掉进程并返回失败。

长进程用 runtime 的 exec 加一个会话句柄。输出靠轮询增量读。第一版不做 PTY。

会话状态在 executor 内存里：输出缓冲、offset 游标、退出码、进程句柄。executor 重启这些就没了，对应的 run 判失败。executor 不做会话持久化。

### 5.3 工作区同步

工作区目录在共享卷上。scheduler 与 executor 挂同一个卷、同一路径，卷根记作 `workspace_root`。

每个工作区一个子目录，目录名是工作区 ID。executor 校验 ID 只含安全字符集，拒绝 `/`、`..` 与绝对路径。

上传：把宿主目录打成 tar.gz，灌进容器，解包到 `/workspace`。这是 executor 在 `create` 里的内部动作。

下载：容器内 `tar` 打包 `/workspace`，流式写回宿主 staging 目录，校验后原子替换。这是 `POST /sandbox/{id}/workspace/sync` 的动作。

规则沿用现有实现（`internal/ee/worker/worker_sandbox.go`）：

- 归档大小上限走 `sandbox-archive-max-bytes`。
- 拒绝绝对路径、`..` 逃逸、逃逸 symlink。
- `.git` 不上传，宿主保留。
- 同步失败不能算成功。

### 5.4 工作区文件由 scheduler 提供

executor 不提供文件接口。工作区目录在共享卷上，scheduler 挂同一个卷，直接读它。

调度器侧现有的文件树与文件预览走宿主文件系统（`internal/server/workspace.go` 与 `internal/server/fsapi.go`）。现有实现直接可用，不用改成经 executor 转发。

代价是 scheduler 能读到卷上的所有工作区目录。所以：

1. 文件树与预览的路径必须限制在卷根内。
2. 结果按租户过滤。一个租户不能通过路径参数看到别人的工作区。

这两点顺带修掉现在的一个问题：这两个接口现在接受任意路径参数，客户端可以传 `?workspace=/etc` 拿到目录树。

## 6. 配置面

### 6.1 落在哪里

配置落在已有的 `sandbox_backends` 表上，不进 `settings.Settings`，不建新表。

给 `sandbox_backends` 加一列 `options`（JSON 文本）。内容是后端参数。

```json
{
  "image": "ghcr.io/…/woca-sandbox:0.1",
  "runtime": "runc",
  "cpu": 2,
  "memory_mb": 4096,
  "env": {}
}
```

只有五个字段。理由：

- `image` 是最常改的旋钮，必须在管理台。
- `runtime` 是隔离档位，是产品承诺的一部分。
- `cpu` 与 `memory_mb` 用于容量规划。
- `env` 注入沙箱环境变量，不含密钥。

不放进 options 的：

| 项 | 放哪 | 原因 |
|---|---|---|
| 工作区根目录 | executor 进程参数 | 部署路径，不是策略 |
| 网络模式 | 固定 `none` | 首期不开放出口，等白名单代理 |
| 沙箱复用 | 不做 | 首期 per-run 创建销毁 |
| 凭据 | 现有 secrets 表 | 已有机制 |

`SandboxRow`（`internal/ee/nacosrt/pack.go`）加 `Options` 字段，pack v2 携带它。pack 版本不变：新增的是可选字段，旧 pack 解析成空值。

两个约束：

1. `Options` 用 `omitempty`。空的沙箱列表在 pack 里不出现，`nacos-init` 发布的初始 pack 字节不变，watcher 的指纹不会反复触发 apply。
2. pack 是全量清单语义且解码严格。configsvc 发出带 `options` 的新 pack 后，**全角色要同步升级**。旧 worker 收到新 pack 会解码失败。这与 pack v2 升级时的约定一致。

管理台（`ee/configsvc-web/src/components/SandboxSection.vue`）在现有后端行上直接编辑这些字段。现有 CRUD 路由（`internal/ee/configsvc/handlers_sandboxes.go`）加一个 `options` 字段，不需要新路由。

### 6.2 热生效语义

链路沿用现成的：

```
管理台写库 → notifyConfig() → nacos 发布 pack v2
   → executor 收到 pack → materializeTables 落表 → OnChange 回调
```

executor 的 `OnChange` 里更新内存里的后端参数。调度器不需要参与。

生效边界，明确写下来：

1. **新建的沙箱**立即用新参数。
2. **运行中的沙箱**不重建，继续用旧参数。
3. 想让运行中的沙箱也用新参数，操作员显式重启该沙箱。

不做自动重建。自动重建会丢进程状态，也会让一次配置改动放大成一次故障。

### 6.3 不做的配置

- 不做 per-tenant 覆盖。
- 不做 per-workspace 覆盖。工作区不记后端类型。
- 不做配置文件。compose 的环境变量只保留部署必需项，不承载沙箱行为参数。
- 不做配置版本历史。审计走现有日志。

## 7. 工作区模型

### 7.1 工作区 ID 与绑定

工作区 ID 由 proxy 生成，格式 `ws_<uuid>`。它标识一个执行环境，不是聊天 session 的 ID。多个聊天 session 可以指向同一个工作区，它们共享同一份代码和同一个 executor。

ID 与 executor 无关。绑定关系单独存，这样 executor 换机器后工作区 ID 不变。

绑定要持久化，因为两次 run 之间工作区仍然属于某个 executor。所以要一张绑定表：

| 列 | 用途 |
|---|---|
| `workspace_id` | 主键。proxy 生成的 ID |
| `executor_id` | 绑定的 executor 实例 |
| `state` | `active` 或 `degraded`（executor 不健康时） |
| `created_at` / `updated_at` | 审计与运维 |

工作区 ID 交给 executor 后，executor 映射到 `workspace_root/<id>` 目录。executor 校验 ID 只含安全字符集，拒绝 `/`、`..` 与绝对路径。

其余元数据放在两处：

- 工作区路径与 revision：executor 侧维护。
- 工作区与活跃 run 的对应：调度器侧靠 `runs` 表的一列。

### 7.2 互斥

同一工作区同时只允许一个活跃 run。跨聊天、跨调度器副本都要成立。

实现分两层，都很轻：

1. **数据库层**：`runs` 表加一列 `workspace_id`，加一个部分唯一索引，只在 `status='running'` 时生效。同一工作区不可能有两个 running run。崩溃时 reaper 把过期 run 标失败，锁自动释放。
2. **执行层**：executor 对工作区目录加文件锁。两个调度器副本同时抢同一个工作区时，executor 侧也会挡住第二个。

不建独立的锁表。锁就是 running run 这一行。

不用的字段放一边：

| 原设计 | 现状 | 原因 |
|---|---|---|
| `fence_token` | 删 | 同一工作区不会有两个写者，不需要代次比较 |
| `runtime_state` 状态机 | 删 | 沙箱是容器，状态由 container runtime 给 |
| `active_run_id` 字段 | 删 | 唯一索引已经表达了这个约束 |
| `data_revision` | 保留，executor 侧维护 | 前端要显示代码版本 |
| `sandbox_name` / `runtime_generation` | 删 | 容器名与容器 ID 由 executor 内部管 |

### 7.3 跨轮次延续

代码延续靠工作区目录，不靠沙箱容器。

- 第二轮 run 复用同一个目录。
- 沙箱是新的容器。
- 上传阶段把目录内容灌进新容器。

因此开发服务器、装在容器里的依赖这些不跨轮次。这是轻量的代价，要在管理台和文档里写明。

沙箱复用（跨 run 保留同一容器）首期不做。

## 8. 隔离与出口

### 8.1 隔离档位

| 档位 | runtime | 要求 | 强度 |
|---|---|---|---|
| default | runc | 无 | 共享宿主内核 |
| hardened | runsc（gVisor） | 宿主装好 runsc | 用户态内核 |

管理台选档。`runsc` 没装时 executor 明确报错，不静默降级。

gVisor 的代价要说清：系统调用被用户态内核翻译，官方数据是多数场景 10% 到 50% 额外开销，小文件随机读写与高频短连接更差。它的系统调用覆盖不是 100%。

### 8.2 出口

第一版 `network=none`。沙箱完全没有外网。

理由：模型调用在调度器侧。沙箱里不需要访问模型。demo 用 Python 标准库，不下载依赖。

需要外网时再开 `bridge` 加白名单代理。这条排在首期之后。

### 8.3 凭据

模型 key 在调度器侧，不进沙箱。executor 侧没有模型 key。

沙箱后端的 API key 存现有 secrets 表，键名 `sandbox-api-key:<id>`，经 pack 的 `Secrets` 传给 executor。executor 与调度器之间的内部调用用它。

## 9. 安全边界

1. 调度器是唯一用户授权入口。它校验租户与工作区归属。
2. executor 不做授权。它信任来自调度器的调用。
3. executor 不做授权。它信任来自调度器的调用。
4. executor 的服务端口只在内网。不对用户网络开放。
5. executor 持有 runtime 全量权限，所以它所在的节点必须完全受信。
6. scheduler 挂了工作区共享卷，能读到卷上所有工作区目录。文件树与预览的路径必须限制在卷根内并按租户过滤。
7. 沙箱里没有 woca 的凭据与代码。
8. 工作区文件接口限制在工作区根内。拒绝穿越。
9. 归档导入导出拒绝绝对路径、`..` 与逃逸 symlink。
10. 沙箱失败一律让 run 失败。不回退本地执行。
11. 管理台改配置有审计日志，沿用现有 `sandboxLog` 形状。

## 10. 故障语义

### 10.1 三个概念

| 东西 | 是什么 | 生命周期 |
|---|---|---|
| executor 容器 | woca 自己的角色进程，跑在 Compose 里 | 长期运行 |
| 沙箱容器 | 宿主上的真实容器，跑用户代码 | 每个 run 一个，run 结束销毁 |
| 命令进程 | 沙箱容器内的进程 | 单次 exec |

执行命令不新建容器。所有工具调用都通过 runtime 的 exec 接口在同一个沙箱容器里起进程。

### 10.2 故障表

| 故障 | 行为 |
|---|---|
| executor 不可达 | run 失败，错误写明是执行面不可达 |
| 容器创建失败 | run 失败，错误带 runtime 的原始原因 |
| 上传失败 | run 失败，销毁容器，宿主目录不动 |
| 执行超时 | 杀掉进程，run 失败 |
| 同步失败 | run 失败。宿主保留旧目录，错误写明同步未完成 |
| executor 崩溃 | run 标记失败。工作区锁随事务释放。旧容器由 executor 启动时清理 |
| 调度器崩溃 | run 标记失败。不自动重放 |
| 沙箱逃逸 | 由 runtime 的隔离能力兜底。woca 不做检测 |

### 10.3 遗留容器

遗留容器是宿主上的真实沙箱容器。它出现在 run 或 executor 异常结束、没走到 destroy 的时候，在 `docker ps -a` 里看得见。

沙箱容器创建时必须打标签：`woca.sandbox=1`、`woca.workspace=<id>`、`woca.run=<id>`、`woca.executor=<id>`。清理按标签筛，只删不属于当前活跃沙箱集合的那些。

容器里没被 kill 的残留进程不用单独清。容器删除时它们一起消失。

不做自动重放。副作用未知的 run 重放会重复写文件、重复调用外部接口。

## 11. 部署形态

### 11.1 企业版 compose

在 `deploy/selfhosted/docker-compose.full.yml` 里：

| 服务 | 变化 |
|---|---|
| `woca-scheduler` | 原 `woca-server`。内嵌 run 执行循环：自己从队列 claim run，在进程内用已有的 agent 跑。保留 `WOCA_RUN_DISPATCH=queue` 与 `WOCA_RUNS_OFF_HOST=true`，只是领 run 的不再是独立 worker 进程 |
| `woca-executor` | 原 `woca-worker` 改名。新增沙箱服务端口，不再领 run、不跑 agent 循环 |
| `executor-proxy` | 新增。转发与流控 |
| `woca-worker` | EE 首期不启动。角色代码保留，个人版与过渡期用 |
| 个人版 `woca` | 不接入这条路径。个人版继续本地执行，不经过 scheduler、proxy、executor |

executor 可横向扩容。工作区目录在共享卷上，不绑 executor。新 executor 挂同一个卷就能接管已有工作区，只要改绑定记录。

个人版与 EE 的边界：`internal/ee` 下的角色只进企业版。个人版二进制不链接它们。`make check-editions` 守这条边界。

调度器内嵌的执行循环直接复用 `internal/ee/worker` 现有的 claim、事件镜像、transcript 保存逻辑。不重写一套。

新增的卷：`woca-workspaces`。scheduler 与 executor 都挂，路径相同，卷根就是 `workspace_root`。

executor 拿 container runtime 的访问权，走容器内直连：executor 跑在容器里，挂载宿主 runtime socket，直接调用容器管理接口。

代价要写明：executor 拿到的是 runtime 的全量权限。它能做的比沙箱需要的多，包括挂载宿主目录、给容器加特权、改容器网络。所以 executor 进程本身必须放在受信节点，不对用户网络开放。

权限收窄（socket 代理只放行建容器、删容器、执行命令、读写文件四类调用）排在后面。触发条件是 executor 所在节点不再完全受信。

### 11.2 开发者 demo

同一个 compose 文件加 `demo` profile：

| 服务 | 作用 |
|---|---|
| `fakemodel` | 脚本模型。复用 `e2e/sandboxlab/fakemodel` |
| `sandbox-init` | 一次性写入模型设置与沙箱后端参数 |
| `sandbox-demo` | 跑完整断言后退出。退出码即结论 |

启动顺序分四段，由 `make ent-sandbox-demo` 驱动：

1. 基础面：pg、nacos、nacos-init、woca-config、woca-scheduler、executor-proxy、woca-executor、fakemodel。
2. 配置面：sandbox-init 写模型设置与后端参数。woca-executor 启动前必须拿到。
3. 执行面：woca-executor 注册到 nacos，声明能力。
4. 验收面：sandbox-demo。

### 11.3 开发者宿主的限制

- container runtime 版本要满足 executor 的要求。
- 选 `hardened` 档时宿主要装好 runsc。没装就明确报错。
- macOS 上的容器运行时没有官方支持列表。`runsc` 档在 macOS 上不保证可用。

## 12. 远程 demo 验收

### 12.1 一级：脚本模型

1. 用户在空工作区提交提示词。
2. 调度器编排 agent 循环，通过 executor 沙箱服务执行。
3. 沙箱内跑测试，通过。
4. 产物同步回 executor 磁盘的工作区目录。
5. demo 容器断言目录里出现预期文件。
6. demo 容器断言沙箱容器被回收。

### 12.2 二级：跨轮次

1. 第二个 run 提交到同一工作区。
2. 断言工作区锁让第二个 run 排队或直接拒绝，不并行写。
3. 断言代码延续：第二个 run 能读到第一轮的文件。
4. 断言沙箱是新的容器。环境不延续，这一点要在文档里写明。

### 12.3 三级：配置热生效

1. 管理台改后端参数，把镜像换成一个带标记文件的基础镜像。
2. 断言不需要重启任何角色。
3. 新建的沙箱用新镜像，标记文件存在。
4. 运行中的沙箱不受影响。

### 12.4 权限与清理

- 另一个用户访问同一工作区，返回 403。
- 沙箱内访问外网，连接失败。
- run 失败时不回退本地执行。worker 日志里没有本地执行的记录。
- 沙箱销毁后没有遗留容器。

## 13. 测试策略

先写失败测试，再写实现。

| 层 | 必测 |
|---|---|
| contract | 新增 sync 端点；create 的 cwd 语义；fakesandbox 同步更新 |
| executor 沙箱服务 | 15 端点全覆盖；文件面；归档往返；symlink 与路径逃逸拒绝 |
| 工作区锁 | 同工作区串行（部分唯一索引 + 文件锁）；崩溃后自动释放 |
| 配置热生效 | 管理台改参数后新建沙箱用新参数；运行中沙箱不变；缺 runsc 时明确报错 |
| 调度器 | 沙箱不可达时 run 失败且不回退本地 |
| e2e | 一级到三级 demo 步骤；权限；清理 |

e2e 用独立 build tag 和 Make 目标。普通 `go test ./...` 不碰容器。

## 14. 里程碑

| 里程碑 | 内容 | 退出条件 |
|---|---|---|
| M0 | 角色改名与编排位置调整 | 调度器本地跑通一个 run，事件走 PG |
| M1 | contract 增补 sync 端点与测试 | contract 测试全绿 |
| M2 | executor 沙箱服务（Docker driver） | 15 端点与归档往返测试通过 |
| M3 | 工作区目录与工作区锁 | 串行与崩溃恢复测试通过 |
| M4 | 管理台参数与热生效 | 改参数后新建沙箱用新参数 |
| M5 | compose 改造与 demo profile | `make ent-sandbox-demo` 退出码 0 |
| M6 | 故障注入、观测、文档 | 无本地回退、无遗留容器 |

## 15. 改造面

- `internal/platform/executor/CONTRACT.md`：新增 sync 端点，改写 `cwd` 语义。
- `internal/platform/executor/sandbox.go`：client 侧 create 发改工作区 ID，新增 `SyncWorkspace` 调用。
- `internal/platform/executor/fakesandbox/`：实现新端点。
- `internal/server/runs.go`：门控从 `hasLiveSandboxWorker` 改成 executor 在线判定。
- `internal/server/workspace.go`、`internal/server/fsapi.go`：路径限制在共享卷根内，并按租户过滤。
- `internal/ee/executor/`（新）：沙箱服务、容器管理、工作区目录、文件面。订阅 nacos pack，`OnChange` 里更新内存里的后端参数。
- `internal/ee/proxy/`（新）：转发与流控。
- `internal/ee/configsvc/handlers_sandboxes.go`：`options` 字段与校验。
- `internal/ee/nacosrt/pack.go`：`SandboxRow.Options`。
- `internal/wocadb/`：`runs` 加 `workspace_id` 列与部分唯一索引。`sandbox_backends` 加 `options` 列。不建新表。
- `ee/configsvc-web/src/components/SandboxSection.vue`：参数编辑。
- `cmd/woca-scheduler/`、`cmd/woca-executor/`：角色改名与装配。
- `deploy/selfhosted/docker-compose.full.yml`：服务重组、卷、demo profile。

## 16. 开放问题

1. executor 崩溃后遗留容器的清理时机。启动时清还是定时清。
2. proxy 的三个参数取什么默认值：每 executor 并发上限、每队列长度、排队超时。

已定但有触发条件的：

| 事项 | 触发条件 | 做法 |
|---|---|---|
| 出口白名单代理 | 真实项目要在沙箱里装依赖、拉代码 | 沙箱请求过代理，按域名放行，代理在出口注入凭据 |
| 硬隔离档位 | 开始跑第三方上传的代码，或对外开放租户 | Kata 轻量虚拟机或 microVM。宿主要支持虚拟化 |
| runtime 权限收窄 | executor 所在节点不再完全受信 | socket 代理只放行沙箱需要的调用 |
| 工作区重平衡 | executor 长期下线，或要重新分布负载 | 共享卷上目录不绑 executor，改绑定记录即可 |

## 17. 数据库改动清单

三处改动。

| 改动 | 位置 | 用途 |
|---|---|---|
| `sandbox_backends.options` | 配置面表 | 后端参数。经 pack v2 下发 |
| `runs.workspace_id` | run 队列表 | 工作区归属与互斥 |
| `workspace_bindings` | 新表 | 工作区到 executor 的绑定 |

`runs` 上加一个部分唯一索引，只在 `status='running'` 时生效。同一工作区不可能有两个 running run。

`workspace_bindings` 是这次唯一的新表。理由：工作区在两次 run 之间没有 run 行，但仍然属于某个 executor。绑定不持久化，工作区就会漂移到别的 executor，代码目录就找不到了。

`runs.config_snapshot` 里已经带了 workspace 路径。加这一列是为了让数据库能做唯一约束。快照里的路径继续保留，旧调用不受影响。

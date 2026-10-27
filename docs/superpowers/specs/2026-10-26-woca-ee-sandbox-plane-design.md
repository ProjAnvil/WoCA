# woca 企业版沙箱执行面：自建轻量方案（设计）

日期：2026-10-26
状态：设计初稿，待评审
修订：2026-10-26 下午。整合可行性评估结论（2 条 P0、4 条 P1、若干 P2），新增 §8.4/§8.5 沙箱逃逸与外窜防御。出口改为开关：默认关（`network=none`），开 = 直出；不做白名单与镜像网络规则继承（§8.2）。随后裁决（§5.3）：woca 不做任何文件搬运——删除自动上传/同步链路（含 sync 端点），沙箱 bind-mount 工作区目录在原地做事，卷与外部是否同步是用户的事。
取代：2026-10-24 OpenShell 路线（作废，相关文档已删，本文件是沙箱执行面唯一设计文档）

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
| 出口 | 开关。默认关（`network=none`，无外网）；开 = 容器默认网络直出。不做域名白名单，不继承镜像网络规则（§8.2） |
| 硬隔离 | 首期不做。最高档是 gVisor，容器级内核共享 |
| runtime 访问权 | executor 跑在容器里，直连宿主 runtime socket。权限收窄后续做 |
| 工作区 ID | proxy 生成。ID 是执行环境的身份，不是聊天 session 的 ID |
| 工作区放置 | proxy 按负载选 executor，绑定持久化。同一工作区固定落在一个 executor |
| 工作区存储 | 共享卷。scheduler 与 executor 挂同一路径，文件树现有实现直接可用 |
| executor 规模 | 可横向扩容。扩容出的实例只接新工作区 |
| 个人版 | 不接入这条路径。个人版继续本地执行 |
| 外部方案 | 全部不接：OpenShell、E2B、Daytona、Runloop、CubeSandbox、OpenSandbox、agent-sandbox |
| workspace 身份映射 | `workspace_bindings.source_key`（tenant_id + 规范化路径）做查找键。session 创建时 allocate-or-bind（§7.1） |
| 后端入口 | `sandbox_backends.Endpoint` 指向 proxy。真实 executor 地址在 `config_acks.addr`，不进配置表 |
| 容器加固 | 默认 CapDrop=ALL、no-new-privileges、read-only rootfs、默认 seccomp、pids 上限。零新依赖（§8.4） |
| Docker 依赖 | `github.com/moby/moby/client`（模块已拆分，v0.6.2 起 go.mod 干净）+ `moby/moby/api`。不用 `docker/docker` +incompatible 单体，不裸写 HTTP |
| executor 角色形态 | 新增角色，不是 worker 改名（worker 的 agent/claim 装配对 executor 全是死代码）。worker 代码保留，EE 不启动 |
| proxy 路由键 | woca 自有 client 每请求带 `X-Woca-Workspace` 内部头（CONTRACT.md 记一句）。proxy 完全无状态，重启不影响在跑 run。租户从绑定表解析，不信请求头 |
| proxy 队列默认值 | 每 executor 并发 4、队列长 32、排队超时 30s（须小于后端调用超时） |
| degraded 行为 | fail-closed。degraded 工作区的新 run 明确拒绝；ack 回窗自动恢复，或运维显式改绑定（§3.3） |
| run 并发 | 首期每调度器副本串行（复用 `Worker.Step`）。并发旋钮进开放问题（§3.4、§16） |
| 文件搬运 | 不做。woca 无权把文件从一个 owner 搬到另一台机器。沙箱 bind-mount 工作区目录原地做事；卷与外部的同步是用户的事（§5.3） |

### 2.1 为什么自建

自建换来三件事：

1. 控制面只有一套。沙箱生命周期、策略、凭据、路由都在 woca 手里，不与外部控制面重叠。
2. 组件数量最少。executor 一个进程 + 沙箱容器。没有 gateway、supervisor、adapter、egress sidecar。
3. 抽象由 woca 定义。contract v2 已经是 woca 自己的线格式，外部实现可以后接。

代价：

1. 出口只有开关。关（默认）沙箱无外网；开就是直出，没有白名单——粒度换简单。
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
| 出口白名单 / 镜像网络规则继承 | 出口只做开关，粒度换简单 | 触发式（§16） |
| 工作区自动上传/同步（tar 搬运） | 文件所有权原则：把文件从一个 owner 转移到另一台机器不是 woca 的权力。各模式的 workspace 只在自身管控的文件上做事（§5.3） | 不做 |

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
executor 磁盘工作区目录 ══ bind-mount ══▶ 沙箱容器 /workspace（同一份文件，原地读写）
```

调度器不碰文件字节。executor 不搬文件：工作区目录直接挂载进沙箱，沙箱在原地读写，产物落在卷上。

### 3.1 executor 的定位

executor 是一个普通的服务进程，职责只有四件：

1. 暴露 contract v2 的沙箱服务。
2. 用 container runtime 起容器、注入执行命令、回收容器。
3. 持有工作区目录，把它 bind-mount 进沙箱。
4. 提供工作区文件树与文件读取（管理台与前端要用）。

executor 不跑 agent 循环，不持有模型 key，不做授权判断。

### 3.2 executor-proxy 的定位

proxy 是调度器、队列与流控合在一起的转发层。五件事：

1. **生成工作区 ID 并放置。** 新工作区第一次来时，proxy 生成 ID，选一个 executor，把绑定写进数据库。
2. **allocate-or-bind。** 按来源键（tenant_id + 规范化路径）查绑定表：命中即返回既有工作区（§7.1），未命中才走放置。
3. **亲和路由。** 已有工作区按绑定表走，回到同一个 executor。
4. **准入与流控。** 每个 executor 有并发上限。超限的请求进有界队列，队列满则快速失败。
5. **统一入口。** `sandbox_backends.Endpoint` 指向 proxy。调度器的 `NewSandbox` 与 configsvc 的 verify 都拿这一个地址发 contract v2 请求，proxy 按请求里的工作区路由到 executor。真实 executor 地址不进配置表，proxy 从 `config_acks.addr` 取。

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
6. **负载上报**：executor 每 10 秒刷新自己的 ack，带上当前在跑 run 数。`config_acks` 加一列 `load`（整数）承载它；`capabilities` 保持标记串不动。复用现有的 15 秒存活窗口。
7. **不健康与 degraded（fail-closed）**：ack 超过存活窗口的 executor 视为不健康，绑定到它的工作区标记 `degraded`。degraded 期间这些工作区的新 run **明确拒绝**（503，错误写明工作区 degraded 与恢复条件）。不排队——排队会在恢复后突发执行，副作用不可预期。不自动迁移到别的 executor——共享卷让迁移在数据上可行，但涉及在跑容器与工作区锁的交接，走显式重平衡（§16）。恢复只有两条路：executor 的 ack 回到存活窗口内，绑定自动回 `active`；或运维显式改绑定。degraded 不隐含任何静默降级。

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

并发口径要写明：`Worker.Step` 严格串行——claim 一个 run 跑到底（`internal/ee/worker/worker.go`）。调度器内嵌这套循环后，每个副本同一时刻只有 1 个 run，副本数 ×2 = 全局 2 并发 run。proxy 的舱壁（§3.3）限的是每个 executor 的沙箱工具调用，不是 run 级并发，两把尺子不要混。首期显式接受每副本串行：行为与今天的 worker 一致，不需要新的并发控制。每副本并发 run 数的旋钮列进开放问题（§16）。

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
3. `create` 的 body 加一个独立的 `workspace` 字段，承载**工作区 ID**。`cwd` 保持原语义（容器内工作目录，默认 `/workspace`）。不复用 `cwd` 装工作区 ID——那是语义复用，两个概念迟早分叉。executor 把 workspace ID 映射到自己的 `workspace_root/<id>` 目录，并校验 ID 只含安全字符集（字母、数字、连字符、下划线），拒绝 `/`、`..` 与绝对路径。调度器因此不需要知道 executor 的磁盘布局。

client 侧要改两处：`internal/platform/executor/sandbox.go` 的 `create` 现在固定发 `{"cwd": "/workspace"}`。改成发 `{"workspace": "<本次 run 的工作区 ID>", "cwd": "/workspace"}`。另外每个请求带 `X-Woca-Workspace: <id>` 内部头——proxy 靠它做无状态路由，不解析请求体。`CONTRACT.md` 同步补字段与头。

不新增端点。文件搬运不做（§5.3）：没有 upload 方向的端点，也没有 sync 端点——沙箱 bind-mount 工作区目录，产物直接落在卷上，不需要谁确认"收到"。

`archive` 两个端点保留 contract 原语义（请求体与响应体都是 tar.gz 流），**仅限用户手工导入导出**——那是用户在行使自己的文件所有权。调度器与 executor 的自动链路都不调用它们。

其余端点语义不变：

- `exec` 走 `docker exec`，argv、cwd、env、stdin、超时、独立 stdout/stderr、退出码。
- `start` / `output` / `input` / `kill` 对应容器内的长进程。
- `read` / `write` / `stat` / `mkdir` / `glob` / `grep` 走容器内的 `cat`、`sh`、基础工具。
- `archive` 保持原语义：请求体是 tar.gz 流。仅限用户手工导入导出。

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
2. 工作目录 `/workspace` 是 bind-mount 的工作区目录（§5.3）。
3. 隔离档位由 runtime 决定：`runc` 或 `runsc`。
4. 网络由 options 决定：关（`none`，默认）或开（`bridge`，容器默认网络直出）。
5. 容器不注入 woca 的二进制。执行命令靠 runtime 的 exec 接口。
6. 容器创建参数固定带加固 profile（CapDrop、read-only rootfs 等），见 §8.4。

不注入 woca 二进制的好处：沙箱里没有 woca 的代码，也没有 woca 的凭据。代价是文件操作要靠容器内的 `sh`、`cat`、`find`、`grep`、`tar`。所以基础镜像要固定带这些工具。

### 5.2 执行命令

`exec` 映射到 runtime 的 exec 接口。要求：

- stdout 与 stderr 分开。
- stdin 可写。
- 退出码透传。
- 超时后杀掉进程并返回失败。

长进程用 runtime 的 exec 加一个会话句柄。输出靠轮询增量读。第一版不做 PTY。

会话状态在 executor 内存里：输出缓冲、offset 游标、退出码、进程句柄。executor 重启这些就没了，对应的 run 判失败。executor 不做会话持久化。

### 5.3 工作区挂载

工作区目录在共享卷上。scheduler 与 executor 挂同一个卷、同一路径，卷根记作 `workspace_root`。

每个工作区一个子目录，目录名是工作区 ID。executor 校验 ID 只含安全字符集，拒绝 `/`、`..` 与绝对路径。

**沙箱 bind-mount 自己的工作区目录到容器 `/workspace`**——只挂这个工作区的子目录，不挂卷根。没有上传，没有下载，没有 staging：沙箱读写的就是卷上那份文件，产物直接落卷。这是文件所有权原则（§2）：woca 不把文件从一个 owner 搬到另一台机器；沙箱只在 executor 管控的工作区目录上原地做事。卷与用户本地（或任何外部）是否同步，是用户自己的事——用户可以在沙箱里跑 git，也可以用 `archive` 端点手工导入导出。

此前"永不 bind-mount、文件进出只走归档通道"的设计作废：那条通道的存在意义就是搬运，而搬运本身被否决。挂载带来的安全姿态变化在 §8.4 如实记录。

保留的通道检查只作用于用户手工 `archive` 导入（解包侧拒绝绝对路径、`..`、逃逸 symlink，大小上限走 `sandbox-archive-max-bytes`）。`.git` 是工作区目录的一部分，随目录挂载，无特殊处理。

### 5.4 工作区文件由 scheduler 提供

executor 不提供文件接口。工作区目录在共享卷上，scheduler 挂同一个卷，直接读它。

调度器侧现有的文件树与文件预览走宿主文件系统（`internal/server/workspace.go` 与 `internal/server/fsapi.go`）。现有实现直接可用，不用改成经 executor 转发。EE 下解析链固定：session → workspace_id → `workspace_root/<id>`（§7.1）。客户端不再传路径参数，没有穿越空间。

代价是 scheduler 能读到卷上的所有工作区目录。所以：

1. 文件树与预览的路径必须限制在卷根内。
2. 结果按租户过滤。一个租户不能看到别人的工作区。

顺带修掉现在的一个问题：这两个接口现在接受任意路径参数，客户端可以传 `?workspace=/etc` 拿到目录树。

## 6. 配置面

### 6.1 落在哪里

配置落在已有的 `sandbox_backends` 表上，不进 `settings.Settings`，不建新表。

给 `sandbox_backends` 加一列 `options`（JSON 文本）。内容是后端参数。

```json
{
  "image": "ghcr.io/…/woca-sandbox:0.1",
  "runtime": "runc",
  "network": "none",
  "cpu": 2,
  "memory_mb": 4096,
  "env": {}
}
```

只有六个字段。理由：

- `image` 是最常改的旋钮，必须在管理台。
- `runtime` 是隔离档位，是产品承诺的一部分。
- `network` 是出口开关：`none`（默认，无外网）或 `bridge`（直出）。只此两值，非法值拒绝。不做域名白名单，不继承镜像网络规则。
- `cpu` 与 `memory_mb` 用于容量规划。
- `env` 注入沙箱环境变量，不含密钥。

不放进 options 的：

| 项 | 放哪 | 原因 |
|---|---|---|
| 工作区根目录 | executor 进程参数 | 部署路径，不是策略 |
| 沙箱复用 | 不做 | 首期 per-run 创建销毁 |
| 凭据 | 现有 secrets 表 | 已有机制 |

`SandboxRow`（`internal/ee/nacosrt/pack.go`）加 `Options` 字段，pack v2 携带它。pack 版本不变：新增的是可选字段，旧 pack 解析成空值。

两个约束：

1. `Options` 用 `omitempty`。空的沙箱列表在 pack 里不出现，`nacos-init` 发布的初始 pack 字节不变，watcher 的指纹不会反复触发 apply。
2. 解码兼容性（已对代码核实）：`DecodePack`（`internal/ee/nacosrt/pack.go:199`）是裸 `json.Unmarshal` 加版本号检查，没有 `DisallowUnknownFields`，也没有自定义 `UnmarshalJSON`。未知字段被静默忽略。所以 pack 版本不变时，旧角色收到带 `options` 的新 pack **不会解码失败**，只是看不到这个字段。发布耦合相应降级：**想让谁用上新参数，谁才需要升级**，不是全角色同步。pack 版本号 bump 仍然是硬断裂——那时才要求全角色同步升级。

管理台（`ee/configsvc-web/src/components/SandboxSection.vue`）在现有后端行上直接编辑这些字段。现有 CRUD 路由（`internal/ee/configsvc/handlers_sandboxes.go`）加一个 `options` 字段，不需要新路由。

verify（测试连接）要改道。现在是 configsvc 进程内 `providers.New` 直建沙箱（`internal/ee/configsvc/handlers_sandboxes_verify.go:53`），新架构下真实沙箱只在 executor，configsvc 直建测不到真实执行面。verify 改为拿 `Endpoint`（即 proxy，§3.2）走同一条 contract 路径：临时工作区 → 建容器 → 执行一条命令 → 销毁。

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
| `source_key` | 查找键。`tenant_id` + 规范化来源键（展开家目录、去掉尾部斜杠的绝对路径字符串）。唯一索引 |
| `executor_id` | 绑定的 executor 实例 |
| `state` | `active` 或 `degraded`（executor 不健康时，§3.3 规则 7） |
| `created_at` / `updated_at` | 审计与运维 |

#### 为什么必须有 source_key

ID 是 `ws_<uuid>`，不可预测，主键本身当不了查找键。而今天 runs 与 sessions 只带客户端传入的路径字符串（`internal/server/sessions.go` 的 `createSession`、`RunSnapshot.Workspace`）。没有 source_key 会留下三个洞：

1. proxy 判不了"同一工作区又来了"，亲和路由无从执行。
2. `enqueueRun` 插入 run 行时拿不到 workspace_id，§7.2 的部分唯一索引无值可写。
3. 文件树缺 session → workspace_id → 卷路径的解析链（§5.4）。

source_key 一次闭掉三个。tenant_id 拼进键里，两个租户用同一路径字符串也不会共享工作区。

#### 分配时机：session 创建时 allocate-or-bind

1. `createSession` 拿到客户端工作区路径，规范化后拼上 tenant_id 组成 source_key，调 proxy 的 allocate-or-bind。
2. proxy 按 source_key 查绑定表：命中即返回既有 workspace_id（同一工作区又来了）；未命中则生成新 ID、按 §3.3 规则 1 选 executor、写绑定。
3. workspace_id 存进 session 元数据。`enqueueRun` 时 run 行直接继承，不再从路径反查。所有 run 入口（含 `/btw` 这类带 workspace 串的注册路径）统一走这条继承。
4. 旧 session 补齐：升级前创建的 session 没有 workspace_id，首次 enqueue 时按同一条 allocate-or-bind 路径现场补齐。这是一次性迁移动作，不是失败回退——映射拿不到照样拒绝，不降级。

个人版不走这条路径。createSession 只在 EE 编排下调 proxy。

评估过的替代方案：把 allocate-or-bind 推迟到 run 入队时做，session 只存路径。否掉。它省不掉 source_key（文件树在第一次 run 之前就要解析工作区），却让每次 enqueue 都查一次绑定，且文件树与入队两条路径各自解析身份，多一套分叉。session 创建时一次定型，下游读同一个值。

工作区 ID 交给 executor 后，executor 映射到 `workspace_root/<id>` 目录。executor 校验 ID 只含安全字符集，拒绝 `/`、`..` 与绝对路径。

其余元数据放在两处：

- 工作区路径与 revision：executor 侧维护。
- 工作区与活跃 run 的对应：调度器侧靠 `runs` 表的一列。

### 7.2 互斥

同一工作区同时只允许一个活跃 run。跨聊天、跨调度器副本都要成立。

实现分两层，都很轻：

1. **数据库层**：`runs` 表加一列 `workspace_id`（值继承自 session，见 §7.1），加一个部分唯一索引，只在 `status='running'` 时生效。同一工作区不可能有两个 running run。崩溃时 reaper 把过期 run 标失败，锁自动释放。
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
- 沙箱是新的容器，挂载同一个目录。
- 没有上传阶段——目录就是文件本身，挂上即用。

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

出口是开关，`options.network` 两值：`none`（默认）或 `bridge`。只做开和关，不做域名白名单，不继承镜像或 compose 的网络规则。

关（默认）：沙箱没有网络栈，只有 loopback，完全没有外网。这不只是没有外网——连同一宿主上的邻居容器都打不到，包括 executor。沙箱与 executor 是兄弟容器，但"沙箱直攻 executor 服务端口"这条路被它顺带封死。executor 端口只在内网是第二道独立防线，两道互不依赖。

开：沙箱进容器默认网络，经 NAT 直出。装依赖、拉代码从此可用。代价要写明：

1. 数据可以外带。"数据不出边界"的叙事在开启后不再成立。开是操作员的显式决策，改动走管理台审计日志（§9 第 11 条）。
2. 沙箱经 NAT 可达宿主发布的任何端口。executor 与 proxy 的服务端口不得发布到宿主公网，compose 里只绑内部网络。
3. 不把 Docker 的跨网络隔离当安全边界。默认 bridge 与 compose 自定义网络默认互隔离是实现事实，但可达性受宿主网络配置影响，威胁模型按"开 = 沙箱可外联"对待。

生效边界沿用 §6.2：改开关只对新建沙箱生效，运行中的沙箱不重建。

白名单代理与凭据注入列触发式（§16）：开关满足了装依赖与拉代码，细粒度管控等真实需求出现再做。形态已经收敛，规格就是 §16 那四件事。

### 8.3 凭据

模型 key 在调度器侧，不进沙箱。executor 侧没有模型 key。

沙箱后端的 API key 存现有 secrets 表，键名 `sandbox-api-key:<id>`，经 pack 的 `Secrets` 传给 executor。executor 与调度器之间的内部调用用它。

### 8.4 沙箱逃逸与外窜防御

先把威胁面列全，再按 ROI 排措施。人-服务器的调用鉴权不在本节范围（§9 第 1 条）。

#### 威胁分类

| 威胁 | 路径 | 现状 |
|---|---|---|
| 网络外窜 | 数据外带、C2 回连、拉恶意载荷 | 默认关（`none`）封死整类：无网络栈、只有 loopback、打不到邻居容器含 executor（§8.2）。开关打开后这类威胁回归，是操作员显式决策 |
| 容器→宿主内核逃逸 | runc CVE 一类 | 缺口。默认 runc 共享内核；档位（§8.1）只换 runtime，没有容器加固 |
| 资源耗尽 | fork 炸弹；灌满磁盘拖死邻居工作区 | 一半。cpu/mem 已在 options；pids 已封。磁盘：工作区是 bind-mount，沙箱直接写卷，无写入上限——靠巡检告警与每工作区目录隔离（只拖死自己），硬配额列触发式 |
| 共享卷侧攻击 | symlink、路径逃逸、跨工作区越权 | 挂载即直写：每个沙箱只挂自己的 `workspace_root/<id>` 子目录（不挂卷根），跨工作区越权被挂载边界挡住；工作区内 symlink/垃圾文件只影响该工作区自身（自伤，可接受）。手工 archive 导入的解包检查保留（§5.3）。卷根内其余路径的防护靠文件面 OpenRoot jail（§5.4） |
| executor 全权 socket 被利用 | 沙箱攻陷 executor 后横向移动 | 已列触发式后续（socket 代理）。出口关闭时沙箱打不到 executor（§8.2），这条利用链要先过内核逃逸；出口开启时叠加"executor 端口不发布到宿主"的部署约束（§8.2） |

#### 防御清单（ROI 从高到低）

| # | 措施 | 成本 | 砍掉的攻击面 | 状态 |
|---|---|---|---|---|
| 1 | `network=none` 默认档 | 零 | 整类网络外窜 + 沙箱直攻 executor。零成本封死一整类，收益最大的单项 | 已定。开关打开时此行失效（§8.2） |
| 2 | 容器加固 profile | 约 20 行容器创建参数，零新依赖 | 特权提升与内核攻击面的一大块（runc 利用链的常见前置） | 本轮新增，默认启用 |
| 3 | pids 上限 + 磁盘显式边界 | 一行参数 + 巡检 | fork 炸弹；灌满磁盘拖死邻居工作区 | 本轮新增 |
| 4 | gVisor 档 | 宿主装 runsc，10–50% 开销 | 内核逃逸类（用户态内核翻译系统调用） | 已规划，跑第三方代码时触发 |
| 5 | egress 白名单代理 + 凭据注入 | 一个新组件 | 出口开关打开后的细粒度管控 | 已列触发条件（§16） |
| 6 | executor socket 代理 | 一个代理组件 | executor 被攻陷后的横向移动 | 已列触发条件（§16） |

排序逻辑：先按"每单位成本砍掉的攻击面"排，再按是否已被更低成本措施覆盖排。第 1、2 项合计把"沙箱内进程能碰到的东西"压到最小；3 防的是邻居受害而不是逃逸；4 到 6 各自等一个触发条件，不提前建。

#### 容器加固 profile（第 2 项）

容器创建参数固定带：

- `CapDrop=ALL`。不保留任何 Linux capability。
- `no-new-privileges`。
- read-only rootfs；`/tmp` 挂 tmpfs；`/workspace` 是唯一可写目录。
- 默认 seccomp profile。收紧的 woca 专属 profile 列为后续。
- masked paths 保持 Docker 默认（`/proc/kcore` 等），显式不放宽。
- 不挂任何 device、不加 `privileged`、不挂 docker.sock。

联动 §5.1：基础镜像必须自带 `sh`、`cat`、`find`、`grep`、`tar`。read-only rootfs 下不能指望运行中装包，这条从"建议"升级为硬约束。

兼容性：加固是 executor 的固定行为，不进 options（§6.1 字段清单不含它）。开发宿主（OrbStack）对个别旗标支持不一，executor 提供进程参数 `WOCA_SANDBOX_HARDENING`：`full`（默认，全部旗标）与 `minimal`（demo 用，关 no-new-privileges、read-only rootfs、masked paths，保留 CapDrop=ALL、seccomp、pids 上限）。demo compose 置 `minimal`（§11.3）。`minimal` 只验证功能路径，不是安全承诺；生产必须 `full`，宿主不支持就明确报错，不静默降级。

#### pids 与磁盘（第 3 项）

- pids 上限：固定值（初版 512），不进 options。底线参数不是管理台旋钮。
- 磁盘是显式边界，第一版不是硬配额。三件事收口：
  1. 每工作区目录隔离：沙箱只挂自己的子目录，灌满只拖死自己（bind-mount 边界）。
  2. per-run 容器销毁收口容器内其余可写层：`/tmp` tmpfs 等随容器删除消失，不累积。
  3. executor 定期巡检宿主磁盘水位与工作区目录大小，超限告警。手工 `archive` 导入导出走 `sandbox-archive-max-bytes` 上限。
- 文件系统级每工作区硬配额依赖卷类型（local driver pquota、NFS、EFS 各不相同），列为触发式后续（§16）。

### 8.5 宿主内核与 runc 升级纪律

沙箱共享宿主内核（gVisor 档除外）。runc/containerd 逃逸类 CVE 从公开到打补丁的窗口，就是 woca 的暴露窗口。加固 profile 收窄利用面，不消灭这一类。

纪律写进运维文档：

1. 节点的 runtime（runc/containerd）与宿主内核安全更新纳入例行流程。
2. 容器逃逸类高危 CVE 定响应 SLO：建议 7 天内完成节点滚动升级。
3. executor 滚动重启不丢工作区（共享卷）；在跑 run 按故障语义标失败（§10）。
4. 不做内核漏洞扫描与自动升级。手动纪律，执行记录走日志审计。

## 9. 安全边界

1. 调度器是唯一用户授权入口。它校验租户与工作区归属。
2. executor 不做授权。它信任来自调度器的调用。
3. executor 的服务端口只在内网。不对用户网络开放。
4. 出口默认关：沙箱没有网络栈，打不到 executor 与任何邻居容器（§8.2、§8.4）。开关打开是显式平台决策，走审计日志。
5. executor 持有 runtime 全量权限，所以它所在的节点必须完全受信。
6. scheduler 挂了工作区共享卷，能读到卷上所有工作区目录。文件树与预览的路径必须限制在卷根内并按租户过滤。
7. 沙箱里没有 woca 的凭据与代码。
8. 工作区文件接口限制在工作区根内。拒绝穿越。
9. 手工归档导入导出拒绝绝对路径、`..` 与逃逸 symlink；沙箱只挂载自己的工作区子目录，不挂卷根。
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
| 容器创建失败（含挂载失败） | run 失败，错误带 runtime 的原始原因 |
| 执行超时 | 杀掉进程，run 失败 |
| executor 失联（ack 超窗） | 绑定工作区标 degraded，新 run 503 拒绝（§3.3 规则 7）。ack 回窗自动恢复 active |
| executor 崩溃 | run 标记失败。工作区锁随事务释放。旧容器由 executor 启动时清理 |
| 调度器崩溃 | run 标记失败。不自动重放 |
| 沙箱逃逸 | 由 runtime 隔离与 §8.4 加固 profile 承接。woca 不做逃逸检测 |

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
| `woca-executor` | 新增角色，取代 worker 在 EE 执行面的位置（不是改名）。沙箱服务端口，不领 run、不跑 agent 循环 |
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

demo 断言"沙箱容器被回收"的机制：executor 提供一个内网查询端点（按 `woca.sandbox=1` 标签列容器，如 `GET /internal/sandboxes?run=<id>`），sandbox-demo 调它断言列表为空。选它而不是给 demo 挂只读 docker.sock：后者给 demo 容器 runtime 读权限，多一个权限面；查询端点复用 §10.3 的标签清理逻辑，代码量最小。该端点是 executor 内部运维面，不进 contract v2。

启动顺序分四段，由 `make ent-sandbox-demo` 驱动：

1. 基础面：pg、nacos、nacos-init、woca-config、woca-scheduler、executor-proxy、woca-executor、fakemodel。
2. 配置面：sandbox-init 写模型设置与后端参数。woca-executor 启动前必须拿到。
3. 执行面：woca-executor 注册到 nacos，声明能力。
4. 验收面：sandbox-demo。

### 11.3 开发者宿主的限制

- container runtime 版本要满足 executor 的要求。
- 选 `hardened` 档时宿主要装好 runsc。没装就明确报错。
- macOS 上的容器运行时没有官方支持列表。`runsc` 档在 macOS 上不保证可用。
- 加固旗标在 macOS 开发宿主（OrbStack）支持不一。demo 把 executor 的 `WOCA_SANDBOX_HARDENING` 置 `minimal`（§8.4），生产必须 `full`。

## 12. 远程 demo 验收

### 12.1 一级：脚本模型

1. 用户在空工作区提交提示词。
2. 调度器编排 agent 循环，通过 executor 沙箱服务执行。
3. 沙箱内跑测试，通过。
4. 产物直接落在卷上的工作区目录（bind-mount，无同步环节）。
5. demo 容器断言目录里出现预期文件。
6. demo 容器断言沙箱容器被回收（经 executor 内部查询端点，§11.2）。

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
- 出口默认关：沙箱内访问外网，连接失败。
- 开关验收（人工）：管理台打开 `network=bridge` 后新建沙箱可外联，运行中沙箱不受影响。demo 不依赖外网，此条不进自动断言。
- run 失败时不回退本地执行。worker 日志里没有本地执行的记录。
- 沙箱销毁后没有遗留容器。

## 13. 测试策略

先写失败测试，再写实现。

| 层 | 必测 |
|---|---|
| contract | create 的 workspace 字段与路由头；15 端点（无 sync）；fakesandbox 同步更新 |
| executor 沙箱服务 | 15 端点全覆盖；文件面；工作区 bind-mount；手工 archive 通道检查 |
| 身份映射 | 同一 tenant 同一路径两次 allocate-or-bind 返回同一 workspace_id；不同租户同路径得到不同 ID；旧 session 首次入队补齐 |
| 工作区锁 | 同工作区串行（部分唯一索引 + 文件锁）；崩溃后自动释放 |
| 配置热生效 | 管理台改参数后新建沙箱用新参数；运行中沙箱不变；缺 runsc 时明确报错；verify 经 proxy 走通；`options.network` 只接受 `none`/`bridge` |
| 调度器 | 沙箱不可达时 run 失败且不回退本地；degraded 工作区新 run 503 |
| 容器加固 | full profile 下容器无 capability、rootfs 只读、pids 上限生效；minimal profile 仅功能路径 |
| e2e | 一级到三级 demo 步骤；权限；清理 |

e2e 用独立 build tag 和 Make 目标。普通 `go test ./...` 不碰容器。

## 14. 里程碑

| 里程碑 | 内容 | 退出条件 |
|---|---|---|
| M0 | 角色改名与编排位置调整 | 调度器本地跑通一个 run，事件走 PG |
| M1 | contract 增补 workspace 字段与路由头 | contract 测试全绿 |
| M2 | executor 沙箱服务（Docker driver，依赖已定 `moby/moby/client`，Docker 操作走窄接口 `Backend`，单测打 fake、真 Docker 只进 `-tags dockerlab`；工作区 bind-mount） | 15 端点与手工 archive 往返测试通过 |
| M3 | 工作区目录与工作区锁 | 串行与崩溃恢复测试通过 |
| M4 | 管理台参数与热生效 | 改参数后新建沙箱用新参数 |
| M5 | compose 改造与 demo profile | `make ent-sandbox-demo` 退出码 0 |
| M6 | 故障注入、观测、文档 | 无本地回退、无遗留容器 |

## 15. 改造面

- `internal/platform/executor/CONTRACT.md`：`create` 加 `workspace` 字段与路由头（无 sync 端点）。
- `internal/platform/executor/sandbox.go`：client 侧 create 发工作区 ID（无 SyncWorkspace）。
- `internal/platform/executor/fakesandbox/`：实现新端点。
- `internal/server/runs.go`：门控从 `hasLiveSandboxWorker` 改成 executor 在线判定；`enqueueRun` 从 session 继承 workspace_id（§7.1）。
- `internal/server/sessions.go`：`createSession` 调 proxy 的 allocate-or-bind，session 元数据存 workspace_id。
- `internal/server/workspace.go`、`internal/server/fsapi.go`：解析链固定 session → workspace_id → 卷内路径，限制在共享卷根内，按租户过滤。
- `internal/ee/executor/`（新）：沙箱服务、容器管理（含 §8.4 加固 profile 与 pids 上限）、工作区目录、文件面、内部容器查询端点（§11.2）。订阅 nacos pack，`OnChange` 里更新内存里的后端参数。
- `internal/ee/proxy/`（新）：转发与流控、allocate-or-bind、degraded 标记与恢复。
- `internal/ee/configsvc/handlers_sandboxes.go`：`options` 字段与校验。
- `internal/ee/configsvc/handlers_sandboxes_verify.go`：verify 从进程内 `providers.New` 直建改为经 proxy（§6.1）。
- `internal/ee/nacosrt/pack.go`：`SandboxRow.Options`。
- `internal/wocadb/`：`runs` 加 `workspace_id` 列与部分唯一索引。`sandbox_backends` 加 `options` 列。`config_acks` 加 `load` 列。新表 `workspace_bindings`（含 `source_key` 唯一索引，§17）。
- `ee/configsvc-web/src/components/SandboxSection.vue`：参数编辑。
- `cmd/woca-scheduler/`、`cmd/woca-executor/`：角色改名与装配。
- `deploy/selfhosted/docker-compose.full.yml`：服务重组、卷、demo profile。改名波及面不止 compose：terraform 模板、k8s manifests、prometheus rules、otelcol 配置都引用旧服务名，改名时一并过。

## 16. 开放问题

1. executor 崩溃后遗留容器的清理时机。启动时清还是定时清。
2. 每调度器副本的并发 run 数旋钮。首期固定 1（串行，§3.4）；什么时候要、取值多少待定。

已定但有触发条件的：

| 事项 | 触发条件 | 做法 |
|---|---|---|
| 出口白名单代理 | 装依赖、拉代码之外的细粒度出口需求，或要在出口注入凭据（装依赖、拉代码已由开关满足） | 一个组件四件事（业界收敛形态）：默认拒绝 + FQDN 显式清单；凭据在出口注入，不进沙箱（沙箱内代码拿不到私仓 token）；DNS 收口（只过滤 HTTP/SNI 不够，有直连绕过面）；出口连接审计 |
| 硬隔离档位 | 开始跑第三方上传的代码，或对外开放租户 | Kata 轻量虚拟机或 microVM。宿主要支持虚拟化 |
| runtime 权限收窄 | executor 所在节点不再完全受信 | socket 代理只放行沙箱需要的调用 |
| 工作区重平衡 | executor 长期下线，或要重新分布负载 | 共享卷上目录不绑 executor，改绑定记录即可。degraded 期间不自动迁移（§3.3 规则 7），迁移走这张表 |
| 每工作区磁盘硬配额 | 共享卷规模变大，或多租户混跑 | local driver pquota / NFS / EFS 按卷类型选配额机制 |
| 收紧版 seccomp profile | 默认 profile 覆盖不了需求，或出现针对性绕过 | woca 专属 profile，按实际用到的系统调用白名单化 |

## 17. 数据库改动清单

四处改动。

| 改动 | 位置 | 用途 |
|---|---|---|
| `sandbox_backends.options` | 配置面表 | 后端参数。经 pack v2 下发 |
| `runs.workspace_id` | run 队列表 | 工作区归属与互斥。值继承自 session（§7.1） |
| `workspace_bindings` | 新表 | 工作区到 executor 的绑定。含 `source_key` 查找键（tenant_id + 规范化路径，唯一索引） |
| `config_acks.load` | 心跳表 | executor 在跑 run 数。proxy 放置决策用（§3.3 规则 6）。`capabilities` 保持标记串不动 |

`runs` 上加一个部分唯一索引，只在 `status='running'` 时生效。同一工作区不可能有两个 running run。

`workspace_bindings` 是这次唯一的新表。理由：工作区在两次 run 之间没有 run 行，但仍然属于某个 executor。绑定不持久化，工作区就会漂移到别的 executor，代码目录就找不到了。`source_key` 让 proxy 能按来源找到既有绑定（allocate-or-bind）；没有它，亲和路由与 run 行的 workspace_id 都无从谈起（§7.1）。

`runs.config_snapshot` 里已经带了 workspace 路径。加这一列是为了让数据库能做唯一约束。快照里的路径继续保留，旧调用不受影响。

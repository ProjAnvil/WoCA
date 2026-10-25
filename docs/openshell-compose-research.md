# OpenShell 接入 woca 企业版 Compose 的可行性研究

日期：2026-10-24

状态：静态研究完成。尚未执行运行 PoC。

## 1. 结论

OpenShell 可以部署到 woca 企业版 Compose。宿主必须满足 OpenShell 的内核要求。现有 Compose 仍需修改。

OpenShell 不能直接作为 woca 的 `contract-http` 后端。双方协议不同。woca 要求 contract v2 的 15 个 HTTP 端点。OpenShell 提供 gRPC API。

最小可行方案是增加独立适配服务。适配服务对 woca 暴露 contract v2。它对 OpenShell 调用 gRPC。woca 配置台仍注册 `contract-http`。

因此，需要区分三个结论：

| 目标 | 结论 | 前提 |
|---|---|---|
| 在 Compose 启动 OpenShell gateway | 可行 | 改端口、挂载和配置 |
| 用 OpenShell 创建独立沙箱 | 可行 | Docker 28+、Landlock ABI 3+、seccomp 探测通过 |
| 用 OpenShell 执行 woca run | 需要适配 | 实现 contract v2、归档和进程会话 |
| 直接用于企业生产 | 暂不建议 | 先补认证、故障恢复和多节点路由 |

本轮没有改部署文件，也没有实现适配器。

## 2. 研究基线

- 最新 stable release：`v0.1.5`，发布于 2026-10-20。
- 研究源码：OpenShell `v0.1.5` 与 `main`。
- `main` commit：`c4688aa8715ea75d6c39fe468361dbbc16c691fa`。
- Compose、公共 proto 和 Go SDK 的关键内容在两者间一致。
- 许可证：Apache License 2.0。
- woca 企业入口：`deploy/selfhosted/docker-compose.full.yml`。
- woca 沙箱契约：`internal/platform/executor/CONTRACT.md`。

已完成的静态验证：

1. 官方 Compose 通过 `docker compose config --quiet`。
2. `gateway:0.1.5` 镜像存在。
3. `supervisor:0.1.5` 镜像存在。
4. `sandbox:0.1.5` 镜像存在。

本机 Docker daemon 未运行。本机 socket 指向 OrbStack。官方支持矩阵列出 Docker Desktop，没有列出 OrbStack。因此，本轮不能宣称本机运行通过。

## 3. OpenShell 的部署要求

### 3.1 宿主要求

Docker driver 需要 Docker Engine 或 Docker Desktop 28.0+。沙箱镜像支持 `linux/amd64` 和 `linux/arm64`。

Linux 沙箱边界需要：

- Landlock ABI 3+。上游 Linux 6.2+ 提供该版本。
- Landlock 必须实际启用。
- seccomp user notification 必须可用。
- `SECCOMP_IOCTL_NOTIF_ADDFD` 必须可用。
- broker 必须能读取指定子进程内存。
- socket 必须支持 `SO_BINDTODEVICE`。

OpenShell 会主动探测这些能力。探测失败会拒绝启动工作负载。只检查内核版本不够。

GPU 不是 OpenShell 的运行前提。Docker GPU 沙箱才需要 NVIDIA CDI。

### 3.2 容器拓扑

官方 Compose 启动 `init` 和 `gateway`。gateway 使用宿主 Docker socket 创建 sibling containers。这是 DooD，不是 DinD。

gateway 不需要 `privileged`。它默认以 UID 0 运行。官方建议生产环境改用专用 UID，并显式加入 docker group。

Docker socket 仍等同高权限宿主接口。该权限只能给 OpenShell 控制面。不要挂到 woca worker。

Docker driver 适用于单机和单 gateway。官方 Compose 也按每宿主一个 gateway 设计。

### 3.3 数据目录

`/var/lib/openshell` 必须使用 bind mount。宿主路径和容器路径必须相同。

原因是 gateway 提取 runtime binary 后，会把该路径交给宿主 Docker daemon。named volume 不能满足这个路径约束。

该目录还保存数据库、JWT key 和 TLS 材料。备份和权限策略必须覆盖这些内容。

## 4. 与企业 Compose 的直接冲突

### 4.1 端口冲突

woca 默认发布业务端口 `8080`。OpenShell 默认也发布宿主 `8080`。

不能只改 OpenShell 的宿主端口。host-mode supervisor 默认回连 `127.0.0.1:8080`。端口改变后，必须同步设置 Docker driver 的 `grpc_endpoint`。

保留 woca `8080` 时，可采用：

```yaml
# OpenShell gateway
ports:
  - "127.0.0.1:18080:8080"
  - "127.0.0.1:18081:8081"
```

```toml
[openshell.drivers.docker]
grpc_endpoint = "http://127.0.0.1:18080"
```

另一种方案是修改 woca 的 `ENT_BUSINESS_PORT`。该方案会改变现有业务入口。默认不建议。

### 4.2 服务名冲突

woca 已有 `gateway` 服务。新增服务应命名为 `openshell-gateway` 和 `openshell-init`。

不要直接覆盖现有 `gateway` 定义。

### 4.3 默认认证不适合企业

官方 Compose 默认关闭 TLS。它还允许匿名用户。该配置只适合受信本机环境。

企业部署至少应启用 mTLS。adapter 应持有专用客户端证书。woca worker 不应持有该证书。

mTLS 不是完整的多租户授权。未配置 OIDC role 时，本地认证用户会被视为 Platform Admin。生产多租户场景需要 OIDC、workspace 和 membership 设计。

### 4.4 镜像版本必须一致

不要混用 `latest`。PoC 应固定：

```text
ghcr.io/nvidia/openshell/gateway:0.1.5
ghcr.io/nvidia/openshell/supervisor:0.1.5
ghcr.io/nvidia/openshell/sandbox:0.1.5
```

gateway、supervisor 和 sandbox runtime 应保持同一 release。生产环境还应固定 digest。

OpenShell 默认启用匿名运行统计。企业配置可设置：

```text
OPENSHELL_TELEMETRY_ENABLED=false
```

## 5. woca 为什么不能直连 OpenShell

woca 当前运行时只注册两种 provider：`contract-http` 和 `cfbridge`。证据见 `internal/platform/executor/providers/registry.go:30-49`。

woca 每个 run 的流程固定：

1. 创建一个沙箱。
2. 上传完整 workspace。
3. 把工具调用转到 `/workspace`。
4. 下载完整产物。
5. 销毁沙箱。

证据见 `internal/ee/worker/worker_sandbox.go:44-93`。任何失败都会使 run 失败。系统不会回退到本地执行。

OpenShell 没有实现 contract v2 的 HTTP 路径。它还缺少 woca 需要的现成归档接口。

OpenShell Go SDK 可以创建沙箱、执行命令和流式读取输出。SDK 的 `Files().Upload/Download` 当前返回 `ErrTransportNotAvailable`。证据见 `sdk/go/docs/src/api/files.md` 和 `sdk/go/openshell/v1/file_client.go:37-40,78-81`。

因此，verify 只通过 create 和 exec 不够。worker 强制要求归档接口。证据见 `internal/ee/worker/worker_sandbox.go:139-147`。

## 6. 推荐架构：独立 contract v2 adapter

### 6.1 拓扑

```text
woca-config ─┐
             ├─ HTTP contract v2 ─> openshell-adapter ─ gRPC ─> openshell-gateway
woca-worker ─┘                              │                       │
                                            │                       ├─ Docker socket
                                            │                       ├─ supervisor containers
                                            │                       └─ workload containers
                                            └─ 会话缓存、游标、归档流
```

建议给 gateway 和 adapter 建独立 control network。adapter 同时加入 `woca-infra`。

这样配置台和两个 worker 都能访问 adapter。它们不能直接访问 gateway。OpenShell sandbox 仍由宿主 Docker 管理，不加入 woca 的业务网络。

### 6.2 为什么先选 adapter

优点：

- woca 核心代码无需改动。
- 配置台继续使用 `contract-http`。
- 可复用 contract 和 sandboxlab 测试。
- OpenShell 的 gRPC 依赖不进入 woca 主模块。
- mTLS 私钥只留在 adapter。

代价：

- 多一个有状态服务。
- adapter 必须保存进程会话和输出游标。
- adapter 故障会影响正在执行的 run。

原生 Go provider 是后续选项。它能减少一次网络跳转。它也会扩大 woca 的依赖面，并把 OpenShell 凭据分发到 config 和 worker 进程。

### 6.3 adapter 必须实现的语义

| woca contract v2 | OpenShell 实现方式 | 关键工作 |
|---|---|---|
| create | `CreateSandbox` + `WaitReady` | 生成唯一名称，固定 image 和 policy |
| exec | `ExecSandbox` | 聚合 stdout/stderr，保留 exit code |
| start/output/input/kill | `ExecSandboxInteractive` | 保存会话、输出游标和取消语义 |
| read/write/stat/mkdir/glob/grep | 沙箱内 helper 命令 | 限制在允许路径，保持二进制安全 |
| upload archive | 非 TTY stdin 流 + `tar` | 分块输入，清空并解包到 `/workspace` |
| download archive | 流式 `tar` stdout | 流式返回，避免整包进内存 |
| destroy | `DeleteSandbox` | 幂等处理已删除实例 |

高层 Go SDK 不能覆盖全部语义。其 `ExecOptions` 没有 stdin 和 execution timeout 字段。其 `Interactive` 固定启用 TTY。woca 需要分离 stdout/stderr。

adapter 可能需要使用 SDK 生成的低层 gRPC client。公共 proto 已支持非 TTY interactive、stdin、独立输出流和执行超时。

OpenShell gateway 的单个入站 gRPC message 上限为 1 MiB。归档上传必须分块。不能把一个 512 MiB 文件塞进单次 exec 请求。

### 6.4 工作目录

woca 固定使用 `/workspace`。OpenShell 在 OCI image 没有有效 `WorkingDir` 时，默认使用 `/sandbox`。

建议构建 woca 专用 sandbox image：

- 设置 `WORKDIR /workspace`。
- 使用非 root `USER`。
- 安装 `tar`、`gzip`、`grep`、`find` 和基础 shell 工具。
- 不预置 woca 的模型密钥。
- 固定 image digest。

不要靠 adapter 随意改写所有路径。目录不一致会破坏文件工具和归档语义。

## 7. 企业部署仍需解决的问题

### 7.1 单宿主与多宿主

单宿主 Compose 中，两个 worker 可以共享一个 OpenShell gateway。

AWS app-N 拓扑不同。每个 app node 有独立 Docker daemon。OpenShell sandbox 只存在于创建它的宿主。

woca 当前只有一个全局 enabled backend。所有 worker 也共享一个 endpoint。普通负载均衡会把后续请求转到错误宿主。

多宿主生产需要以下一种设计：

1. 每节点部署 gateway 和 adapter，并让 worker 只访问本节点 adapter。
2. 增加路由层，按 sandbox ID 固定路由到创建节点。
3. 改用 OpenShell Kubernetes driver，并重新设计 woca 的调度关系。

第一种仍需处理 config verify 和全局 endpoint 语义。现阶段不能把单宿主 PoC 当成多宿主验证。

### 7.2 租户映射

woca contract create 只传 `cwd`。它没有 tenant、image、policy、CPU、memory 或 GPU 字段。

第一版 adapter 只能使用静态模板和策略。它适合单信任域 PoC。

按租户隔离需要扩展 backend 配置或 run metadata。还要映射 OpenShell workspace 和 membership。只新增 Compose 服务不能完成该目标。

### 7.3 会话恢复

woca contract 用 offset 轮询进程输出。OpenShell 的 exec RPC 不提供 woca 所需的直接重放接口。

adapter 必须在内存或持久存储中保留输出。它还要处理 adapter 重启、worker 重启和孤儿沙箱清理。

第一版可以 fail-run 并回收沙箱。生产环境需要 TTL、账本和定期 reconciler。

### 7.4 企业 Compose 已有的 workspace 问题

`docker-compose.full.yml` 的 server 和 worker 只共享 sessions 与 crypto。它们没有共享 run workspace。

README 要求 server 与 worker 共享同一目录。证据见 `deploy/selfhosted/README-FULL.md:306-308`。

sandboxlab 已提供正确示例。server 和 worker 都挂载同一个 `/data`。证据见 `e2e/sandboxlab/docker-compose.yml:129-132,177-184`。

正式验证 OpenShell 前，应先在 full Compose 增加同路径共享 workspace。这个问题与 OpenShell 无关，但会阻断产物回传。

## 8. 建议的 PoC 顺序

### 阶段 A：验证 OpenShell 本体

使用受支持的 Linux 宿主。不要使用当前 OrbStack 环境作为企业结论依据。

1. 检查 Docker 28+。
2. 检查 Landlock 和 seccomp。
3. 固定三个 `0.1.5` 镜像。
4. 启动 `openshell-init` 和 `openshell-gateway`。
5. 检查 `/healthz` 和 `/readyz`。
6. 创建最小 sandbox。
7. 执行 `pwd`、文件写入和网络 deny 测试。
8. 删除 sandbox，并检查 sibling containers 已清理。

阶段 A 只证明 OpenShell 能运行。

### 阶段 B：验证 adapter

1. 实现 contract v2 全量端点。
2. 复用 `internal/platform/executor/sandbox_contract_test.go`。
3. 测试 stdin、env 和长进程。
4. 测试 symlink、mode 和 archive round-trip。
5. 测试 adapter 重启和 sandbox TTL。
6. 测试 512 MiB 边界的流式行为。

阶段 B 只证明协议适配正确。

### 阶段 C：验证 woca 全链路

1. 修复 full Compose 的共享 workspace。
2. 在配置台注册 `contract-http`。
3. 执行 verify。
4. 启用 backend。
5. 确认无 backend 时返回 403。
6. 确认无 sandbox worker 时返回 503。
7. 执行真实 run。
8. 检查产物回到共享 workspace。
9. 确认失败时没有本地回退。
10. 检查 sandbox 和进程已回收。

可复用 `make sandboxlab` 的驱动和断言。最终还要跑 full Compose，而不是只跑 fakesandbox。

## 9. 最终判断

建议立项做 PoC。技术路径成立，没有必要改 OpenShell 内核或 woca 的执行模型。

第一阶段范围应限定为：单 Linux 宿主、单信任域、固定镜像、固定策略、两个 woca worker、一个 adapter、一个 OpenShell gateway。

以下目标应放到后续阶段：多宿主调度、按租户 workspace、动态 GPU、动态 policy、会话持久恢复和高可用。

## 10. 主要证据

OpenShell 官方资料：

- 仓库：https://github.com/NVIDIA/OpenShell
- 架构：https://docs.nvidia.com/openshell/latest/about/architecture
- 支持矩阵：https://docs.nvidia.com/openshell/latest/about/support-matrix
- Docker runtime：https://docs.nvidia.com/openshell/latest/how-it-works/sandboxes/runtimes
- 容器部署：https://docs.nvidia.com/openshell/latest/how-it-works/gateways/container-deployment
- 认证：https://docs.nvidia.com/openshell/latest/how-it-works/gateways/authentication
- workspace：https://docs.nvidia.com/openshell/latest/how-it-works/workspaces
- stable release：https://github.com/NVIDIA/OpenShell/releases/tag/v0.1.5

woca 本地资料：

- `deploy/selfhosted/docker-compose.full.yml`
- `deploy/selfhosted/README-FULL.md:236-312`
- `internal/platform/executor/CONTRACT.md`
- `internal/platform/executor/providers/registry.go`
- `internal/ee/worker/worker_sandbox.go`
- `internal/ee/configsvc/handlers_sandboxes_verify.go`
- `e2e/sandboxlab/docker-compose.yml`
- `e2e/sandboxlab/sandboxlab_test.go`

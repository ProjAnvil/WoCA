# SPACE 沙箱机制调研：企业版 woca 可用性评估

- 日期：2026-10-17
- 状态：调研完成，待决策
- 范围：Perplexity 开源 SPACE（Sandboxed Platform for Agentic Code Execution）与企业版 woca 的集成可行性

## 1. 结论

**技术上可用，架构方向吻合，但不是"配置即用"。**

- woca 已预留执行后端抽象（`Executor` 接口）和远程沙箱执行器（`internal/platform/executor/sandbox.go`），设计参照正是 E2B/Daytona 风格 HTTP API。
- SPACE 提供 E2B 兼容层（ECW）和原生 HTTP API，能作为该执行器的后端。
- 直接落地存在 4 个硬缺口：契约不匹配、生命周期粒度错误、5 个接口未实现、基础设施不满足（Firecracker 需要 `/dev/kvm`）。
- 评估：写一个薄适配层 + 补齐 woca 侧缺口后可用。预计分四期落地，见第 6 节。

## 2. SPACE 是什么

Perplexity 于 2026-10-15 开源（Apache 2.0）。仓库：`github.com/perplexityai/SPACE`。Rust 实现，经 HTTP API + SSE 使用。

### 2.1 核心机制

| 层 | 机制 | 说明 |
|---|---|---|
| 隔离 | Firecracker microVM | 每任务独立 VM、独立 guest OS。边界是 hypervisor，不是容器 namespace |
| 控制 | 无状态中心控制平面 | 追踪沙箱状态、路由后端、决定创建与销毁 |
| 凭据 | 节点级注入 | 凭据不进沙箱。需要时从外部瞬时注入，支持 BYOK |
| 网络 | 节点级出口控制 | VM 外网关执行 egress 规则，egress profile 可配置 |
| 状态 | 滚动快照 | 每分钟全量内存+文件快照，保留一周。支持暂停/恢复/分支 |

### 2.2 API 模型

三个概念：

- **Template**：起始环境（rootfs + 配置）
- **Sandbox**：模板实例，独立 VM + 可写文件系统，可跑多条命令
- **Command Session**：attach 后启动进程，经独立数据面连接读输出

生命周期操作（create/pause/suspend/delete）异步：返回 operation ID，轮询结果。命令输出走独立连接，使用会话级 attachment token。

### 2.3 生产数据（Perplexity 自报，未独立复核）

- 2026-07 起承载全部 Computer 会话，日创建沙箱数百万
- 沙箱创建中位延迟约 60ms（此前 185ms），依赖 warm pool + base snapshot
- 2026-09 红队测试：108 次逃逸尝试，0 次 VM 到宿主逃逸。发现若干网络限制绕过，已修复
- CoreWeave、Akamai 已上线托管，Crusoe 计划跟进

### 2.4 部署要求

- Kubernetes 非必需（架构文档描述了无 K8s 运行），但官方部署路径是 Kind（本地）和 EKS（参考）
- Firecracker 需要 KVM 硬件虚拟化（`/dev/kvm`）。这要求裸金属或 `.metal` 实例
- 仓库含 Rust crates、gRPC proto、Firecracker 模板配方、operator CLI、warm pool、base snapshot、egress profile 文档
- API 字段标记 `incubating` 的不在支持契约内

## 3. woca 现状

### 3.1 执行后端抽象已就位

`internal/platform/executor/executor.go:50-76` 定义 `Executor` 接口（Exec/Start/ReadFile/WriteFile/MkdirAll/Stat/Glob/Grep）。三个实现：

- `Local`：`os/exec` + 本地文件系统，无 OS 级隔离（默认）
- `Sandbox`：远程沙箱 HTTP 客户端（Phase 3，默认关闭）
- 全库无 firecracker/microVM/gVisor/kata 引用

### 3.2 远程沙箱执行器现状

`internal/platform/executor/sandbox.go` 的 HTTP 契约（6 个端点）：

| 操作 | 端点 | 状态 |
|---|---|---|
| 创建 | `POST /sandbox/create` | 已实现 |
| 执行 | `POST /sandbox/{id}/exec` | 已实现 |
| 读文件 | `GET /sandbox/{id}/read?path=` | 已实现 |
| 写文件 | `POST /sandbox/{id}/write` | 已实现 |
| Glob | `POST /sandbox/{id}/glob` | 已实现 |
| 销毁 | `DELETE /sandbox/{id}` | 已实现 |

未实现（返回 `ErrSandboxNotImplemented`）：`Start`（长进程）、`Stat`、`MkdirAll`、`Grep`。

启用路径：`WOCA_SANDBOX_ENDPOINT` / `WOCA_SANDBOX_API_KEY` / `WOCA_SANDBOX_TIMEOUT_MS`（`internal/agentbuild/options.go:93-106`），worker 与 serve 两入口统一走 `SandboxFromEnv()`。

### 3.3 企业版相关事实

- `server.Config.RunsOffHost`（`internal/server/server.go:70-74`）注释明确规划了"远端租户 + 沙箱化 worker 执行"形态
- 企业执行平面有两条路线：bridge（跑在用户自己机器，信任用户内网）与中心沙箱（信任集中执行环境）。两者互补，服务不同安全等级的租户
- 现有部署形态：VM 上 docker compose（terraform 参考实现，默认 `t4g.medium`，无 `/dev/kvm`）；K8s worker 是普通 pod（无特权、无设备）。**当前任何部署形态都无法直接跑 Firecracker**

## 4. 适配性分析

### 4.1 对得上的部分

| woca 需求 | SPACE 供给 | 判定 |
|---|---|---|
| E2B 风格 HTTP 后端 | 原生 HTTP API + E2B 兼容层（ECW） | 匹配，需薄适配 |
| `Exec` 同步语义 | command session + 输出收集 | 可映射 |
| 沙箱创建/销毁 | 异步 operation + 轮询 | 适配层封装 |
| 长进程 `Start`（MCP stdio/LSP/后台 shell） | session attach + SSE 流 | 数据模型匹配，woca 侧未实现 |
| 多命令复用环境 | sandbox 承载多 command session | 匹配 |
| Go 调 Rust 服务 | HTTP API，语言无关 | 无障碍 |

### 4.2 对不上的部分（硬缺口）

**缺口 1：契约细节不匹配。**
woca 把 argv 用空格 join 成单个 `command` 字符串（含空格参数会碎）；`Env`、`Stdin` 被丢弃；无 `cwd` 字段。对接前必须修订契约为 argv 数组 + cwd + env。此缺口与 SPACE 无关，是 woca 自身缺陷。

**缺口 2：生命周期粒度错误。**
`internal/agentbuild/build.go:94-103` 在 `BuildAgent()` 时 provision 一次，进程退出时销毁。一个 worker 进程的所有租户、所有 run 共用一个沙箱。**当前形态不具备租户间隔离**，与 SPACE 的"每任务一个 VM"设计冲突。必须改为 per-run（或 per-session）provision。改造牵动 tools registry、LSP 池、MCP 连接的装配方式（它们在 BuildAgent 时绑死 executor）。

**缺口 3：workspace 引导缺失。**
沙箱里没有代码。`RunSnapshot.Workspace` 是 worker 宿主机路径。需要补 git clone / 快照上传 / 产物回传。设计文档 §8.3 本来就规划了 workspace snapshot 生命周期，尚未实现。SPACE 的文件快照机制可复用。

**缺口 4：基础设施不满足。**
Firecracker 需要 `/dev/kvm`。K8s pod 需特权 + 设备插件改造；EC2 `t4g.medium` 无 KVM 嵌套（需 `.metal`）。需要新增独立沙箱节点池，或使用 CoreWeave/Akamai 托管 SPACE。

**缺口 5：文件浏览双轨。**
`fsb`（Web `/api/fs/*`）直接 `os.OpenRoot` 宿主机，不走 Executor。远端租户形态下文件侧栏与沙箱内容会脱节。

### 4.3 SPACE 带来的增量价值

woca 现有空白恰好是 SPACE 的卖点：

- **凭据治理**：woca 的 `WOCA_SANDBOX_API_KEY` 是明文 env，沙箱内凭据注入无机制。SPACE 的节点级凭据注入 + BYOK 直接填补
- **出口控制**：woca 应用层无 per-run 出口控制（只有 bashgate 前缀表）。SPACE 的 egress profile 提供网络层控制
- **暂停/恢复**：woca 的中断恢复在 agent graph checkpoint 层。SPACE 提供执行环境层的暂停/恢复/分支，两层叠加可支撑长任务
- **私有化**：SPACE 支持 on-prem 和离线运行，符合企业版私有化交付需求

## 5. 风险

| 风险 | 等级 | 说明 |
|---|---|---|
| 项目成熟度 | 高 | 开源仅 2 天（2026-10-15）。无供应商支持，incubating 字段边界可变。生产数据全部来自 Perplexity 自报 |
| 运维能力 | 高 | 团队需具备 Rust/K8s/Firecracker/warm pool 运维能力。60ms 创建延迟是预热池运营的结果，不是免费的 |
| 红队遗留 | 中 | 网络限制绕过已修复，但同类问题可能再现。沙箱内出口策略需要独立验证 |
| ARM 兼容 | 低 | Firecracker 支持 aarch64，但 woca 现有 terraform 实例无 KVM，需换实例类型而非换架构 |

## 6. 落地路径（建议）

### P0：PoC 验证（预计 1-2 天）

1. Kind 起 SPACE，跑通官方 walkthrough
2. 写薄适配层（约 200 行）：把 woca 的 6 端点契约映射到 SPACE 原生 API（或 ECW）
3. 单 run 验证 `WOCA_SANDBOX_ENDPOINT` 指向适配层，跑通 Exec/ReadFile/WriteFile/Glob

### P1：契约修订与生命周期修正

1. 修订 woca 沙箱契约：argv 数组 + cwd + env + stdin
2. 补齐 `Stat`/`MkdirAll`/`Grep`
3. 沙箱生命周期改 per-run provision（工厂模式），改造 `worker.execute()`

### P2：长进程与 workspace 生命周期

1. `Start` 映射：SPACE session attach + SSE → woca `Process` 接口（MCP stdio、LSP、后台 shell、hooks 恢复）
2. workspace snapshot 生命周期（§8.3）：git clone / 快照上传 / 产物回传
3. `fsb` 路由到沙箱

### P3：生产化

1. 沙箱基础设施：独立 KVM 节点池（裸金属/`.metal`）或托管 SPACE（CoreWeave/Akamai）
2. egress profile 策略化：按租户配置出口规则
3. 凭据注入机制：对接 SPACE 节点级注入，替换明文 env
4. 可观测：沙箱指标接入现有 OTel/Prometheus 栈

## 7. 开放问题

1. bridge 路线与中心沙箱路线的产品定位切分：哪些租户走哪条路？
2. ECW 与 woca 自定义契约的映射成本 vs 直连 SPACE 原生 API 的映射成本，孰低？（P0 时验证）
3. 沙箱节点池的成本模型：warm pool 常驻内存 vs 冷启动延迟的权衡
4. SPACE 语义化版本契约（`docs/versioning.md`）对升级路径的约束

## 8. 参考

- SPACE 仓库：https://github.com/perplexityai/SPACE
- Perplexity 开源公告：https://www.perplexity.ai/hub/blog/open-sourcing-space
- Perplexity 沙箱安全博客：https://www.perplexity.ai/hub/blog/secure-sandboxes-for-agents
- woca 沙箱执行器：`internal/platform/executor/sandbox.go`
- woca 执行后端抽象：`internal/platform/executor/executor.go`
- 启用路径：`internal/agentbuild/options.go:93-106`
- 生命周期绑定：`internal/agentbuild/build.go:94-103`

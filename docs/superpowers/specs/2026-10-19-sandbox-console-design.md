# 沙箱管理与企业执行链路（设计）

日期：2026-10-19
状态：待评审（brainstorming 产出，三项关键决策已与需求方对齐；2026-10-19 晚按平台 API 调研修订，见第 9 节）
前置调研：`docs/superpowers/specs/2026-10-17-space-sandbox-research.md`、`docs/superpowers/specs/2026-10-19-sandbox-platform-api-survey.md`

## 1. 背景与目标

企业版 woca 当前没有沙箱执行。远端租户被 default-deny 拒绝创建 run，原因正是其工具会未沙箱化地跑在宿主机上（`internal/ee/tenant/tenantauth_test.go:87-92`）。执行后端抽象（`Executor`）和远程沙箱执行器（`sandbox.go`）已预埋但默认关闭，且生命周期是 per-process，不具备租户隔离。

需求：**配置管理台（woca-config）对受支持的沙箱后端做登记、检测、连接管理；企业版用户经沙箱执行任务。**

已对齐的三项决策：

| 决策点 | 选择 |
|---|---|
| 本期范围 | 一步到位：管理台 + 执行链路（per-run 沙箱、解除远端租户 default-deny） |
| 数据建模 | 多后端档案（表路线），支持多个沙箱后端并存与健康状态展示 |
| v1 沙箱类型 | 通用 HTTP 契约（woca 自有 6 端点形状的修订版 v2）；SPACE/E2B/Daytona 经外部适配层接入 |

## 2. 方案对比（关键决策记录）

**D1 契约：修订自有通用契约 vs 绑定 SPACE 原生 API**
选修订自有契约（v2）。理由：管理台与 worker 只认一个契约，后端可替换；SPACE 经适配层或 ECW 接入（调研报告 P0 结论）；绑定单一平台会锁死刚开源 2 天的项目。

**D2 沙箱行配置下发：扩 Pack vs worker 拉取**
选扩 Pack。理由：nacos pack 是配置下发唯一通道（configsvc.go:158-167），worker 已有 apply + ack + OnChange 全链路；新开拉取通道违背"单一写面"设计。

**D3 per-run 沙箱注入：重建 agent vs run 上下文覆盖**
倾向 run 上下文覆盖（仿 `WithRunWorkspace`）。M3 第一个任务是核实 tools registry 的 executor 解析路径：若 run context 可插，改动收敛；若绑死在 BuildAgent，则退回 per-run BuildAgent（model client 复用池）。此为最大技术风险点，规格在此显式标注。

**D4 workspace 引导：git clone vs 归档往返**
选归档往返（tar.gz 上传/下载）。理由：`RunSnapshot.Workspace` 是宿主机路径，git clone 需要凭据注入与仓库元数据，超出 v1；归档往返不引入新信任面。增量同步与 git 模式留待后续。

## 3. 总体架构

```
                 ┌──────────────────────────────────────────────┐
                 │ woca-config（configsvc + SPA）                │
                 │  /api/sandboxes CRUD + /verify 全链路探活      │
                 │  sandbox_backends 表 + secrets(加密 key)       │
                 └──────────────┬───────────────────────────────┘
                                │ notifyConfig()（既有单一通道）
                                ▼
                        nacos config pack v2
                                │ apply + ack + OnChange（既有机制）
                ┌───────────────┴───────────────┐
                ▼                               ▼
        woca-server                      woca-worker
        RunsOffHost 门控                  per-run 沙箱执行：
        （有 enabled 后端 →                provision → workspace 上传 →
        /api/runs 对远端租户开放）          执行 → 产物回收 → destroy
                │                               │
                └──────── run 派发（msgplane） ──┘
                                                │ 通用 HTTP 契约 v2
                                                ▼
                                ┌───────────────────────────────┐
                                │ 沙箱后端（可插拔）              │
                                │ SPACE 适配层 / E2B / Daytona   │
                                │ / 自建 / 测试用 fake server    │
                                └───────────────────────────────┘
```

## 4. 详细设计

### 4.1 通用 HTTP 契约 v2

修订 `internal/platform/executor/sandbox.go` 的 wire 契约。v1 缺陷（argv 空格 join、env/stdin 丢弃、无 cwd）一并修复。请求头 `X-Woca-Sandbox-Contract: 2`。

| 操作 | 端点 | 请求 → 响应 |
|---|---|---|
| 创建 | `POST /sandbox/create` | `{}` → `{sandbox_id}` |
| 同步执行 | `POST /sandbox/{id}/exec` | `{argv:[], cwd, env:{}, stdin, timeout_ms}` → `{exit_code, stdout, stderr}` |
| 启动长进程 | `POST /sandbox/{id}/start` | `{argv:[], cwd, env:{}}` → `{session_id}` |
| 读输出 | `GET /sandbox/{id}/sessions/{sid}/output?offset=` | `{events:[{stream, data}], done, next_offset}` |
| 写输入 | `POST /sandbox/{id}/sessions/{sid}/input` | `{data}` |
| 终止 | `POST /sandbox/{id}/sessions/{sid}/kill` | `{}` |
| 读文件 | `GET /sandbox/{id}/read?path=` | `{content}`（base64） |
| 写文件 | `POST /sandbox/{id}/write` | `{path, content, mode}`（base64） |
| Stat | `GET /sandbox/{id}/stat?path=` | `{mode, size, mtime_ms, is_dir}` |
| 建目录 | `POST /sandbox/{id}/mkdir` | `{path}` |
| Glob | `POST /sandbox/{id}/glob` | `{pattern}` → `{matches}` |
| Grep | `POST /sandbox/{id}/grep` | `{root, pattern, max_matches}` → `{matches}` |
| 上传归档 | `PUT /sandbox/{id}/archive` | tar.gz body |
| 下载归档 | `GET /sandbox/{id}/archive` | tar.gz body |
| 销毁 | `DELETE /sandbox/{id}` | — |

设计要点：

1. 输出轮询式（offset 游标）而非 SSE。适配层简单，fake server 简单，网络穿透友好。
2. `Executor` 接口全部 8 个方法可在此契约上实现，包括 `Start`（映射 start/output/input/kill 四端点）。
3. 契约文档独立成文 `internal/platform/executor/CONTRACT.md`，作为适配层实现方的规范。

### 4.2 数据模型与 Pack v2

新表（configsvc 包内迁移，仿 `managed_skills`，`wocadb.WithMigrateLock`）：

```sql
CREATE TABLE IF NOT EXISTS sandbox_backends (
  id            TEXT PRIMARY KEY,   -- uuid
  name          TEXT NOT NULL UNIQUE,
  type          TEXT NOT NULL DEFAULT 'contract-http',
                -- contract-http | e2b | daytona | modal | microsandbox | cfbridge | sidecar
  endpoint      TEXT NOT NULL,      -- scheme://host:port
  timeout_ms    INTEGER NOT NULL DEFAULT 120000,
  enabled       INTEGER NOT NULL DEFAULT 0,  -- v1 恰好一个 enabled
  last_probe_at INTEGER,            -- unix ms，nullable
  last_probe_ok INTEGER,            -- 0/1，nullable
  last_probe_error TEXT,
  created_at    INTEGER NOT NULL,   -- unix ms
  updated_at    INTEGER NOT NULL
);
```

- `type` 决定 worker 侧 provider 装配路径与 verify 探活的执行面（见第 9 节）。
- API key 不入此表。存 secrets 表（AES-256-GCM），键名 `sandbox-api-key:<backend_id>`。
- `ExportPack` 按前缀 `sandbox-api-key:` 动态收集后端密钥进 `Pack.Secrets`（`ManagedSecretKeys` 是静态清单，此处需要动态命名空间，二者并存）。
- `Pack.Version` 1→2：新增 `Sandboxes []SandboxRow`（不含 key）字段。codec 守卫更新：worker 收到 v1 pack 视为"无沙箱配置"（向后兼容读），写侧只产 v2。滚动升级注意：全角色须同步升级到产 v2 的 configsvc 之后版本。
- worker 侧 `materializeTables` 增加 sandbox_backends upsert；`St.SetManaged` overlay 同理。

### 4.3 configsvc API

| 路由 | Scope | 说明 |
|---|---|---|
| `GET /api/sandboxes` | settings:read | 列表，key 只回 `key_set: bool` |
| `POST /api/sandboxes` | settings:write | 创建；校验 endpoint URL 与 name 唯一 |
| `PUT /api/sandboxes/{id}` | settings:write | 更新；key 非空才覆写 |
| `DELETE /api/sandboxes/{id}` | settings:write | 删除；enabled 后端不可删（先停用） |
| `POST /api/sandboxes/{id}/verify` | settings:write | 全链路探活 |

每次写操作后走既有 `notifyConfig()` 发布 pack（发布失败 = 保存失败）。

**verify 探活步骤**（每步独立超时，取 `min(timeout_ms, 30s)`；按 `type` 分派到对应 provider 的执行面）：

1. `create` 建临时沙箱
2. `exec` 跑 `["echo", "woca-verify"]` 断言输出
3. `write` + `read` 往返断言
4. `glob` + `stat` 断言
5. `start` + `output` 轮询 + `kill` 断言长进程
6. 小归档 `PUT`/`GET` 往返断言
7. `delete` 销毁

结果写回 `last_probe_*` 三列，响应体 `{ok, total_latency_ms, steps:[{name, ok, latency_ms, error}]}`。任何一步失败即终止并销毁已建沙箱（best-effort）。**绝不回显 key**。

### 4.4 前端 SandboxSection

`ee/configsvc-web/src/components/SandboxSection.vue`：

- 列表：name / endpoint / timeout / enabled 徽章 / 最近探测状态徽章（ok 与否 + 时间）
- 表单：name、endpoint、api key（password input，非空才发送，显示 `key_set`）、timeout_ms
- "测试连接"按钮：独立 fetch `POST /api/sandboxes/{id}/verify`（照 OpsSection 的非 draft 通道模式），展示分步结果
- 启用开关：启用某后端时其余自动停用（服务端强制，前端乐观更新）
- `App.vue` sections 数组加 `sandbox` 项；`i18n/locales/{en,zh-CN}.ts` 补文案；`make ee-web` 同步嵌入

### 4.5 worker 执行链路

**per-run 沙箱生命周期**（`internal/ee/worker/worker.go` execute()）：

1. 读取 materialized 配置中 enabled 的后端（无 → 本地执行器，行为不变）
2. `executor.NewSandbox(cfg)` provision（契约 v2 create）
3. workspace 上传：tar 宿主机 `snap.Workspace` → `PUT archive`（大小上限 `sandbox-archive-max-bytes`，默认 512MB，超限拒绝 run）
4. 执行：run 上下文注入沙箱 executor
5. 产物回收：`GET archive` → 解包回宿主机 workspace（v1 全量往返；fsb/Changes 视图因此继续可用）
6. `Close()` destroy（defer，异常路径同样销毁）

**executor 注入机制（D3 风险点）**：`agentbuild` 增加 `WithRunExecutor(ctx, exec)`，run 作用域覆盖 build 时绑定的 executor。tools registry、hooks、plugin 运行时从 run context 解析 executor，缺省回退 build 时实例。**M3 首个任务为可行性核实**：若 executor 在 BuildAgent 装配期绑死且无法改为 context 解析，则退回 per-run BuildAgent + 共享 model client 池，规格在此修订。

**Start 类消费者的降级矩阵**（v1 明确接受）：

| 消费者 | 沙箱 run 中行为 |
|---|---|
| MCP stdio server | 不可用，agent 收到明确错误（走 start/output/kill 可用但 v1 不做绑定，见开放问题 3） |
| LSP | 不可用，明确错误 |
| 后台 shell | 经 start session 可用 |
| hooks | 经 exec 可用 |

**RunsOffHost 门控解除**（server 侧）：

- 判定条件：配置中存在 enabled 且最近一次 probe ok 的沙箱后端
- 条件成立 → `RunsOffHost=true`，`/api/runs` 对远端租户开放
- 条件不成立 → 维持现状 default-deny（既有测试 `TestRemoteTenantDeniedHostSurfaces` 语义不变，作为无沙箱姿态的回归锚点）
- 安全不变量：run 只能派给上报 sandbox-capable 的 worker。worker 在 config ack 中增加 `sandbox` 能力标记；server 派发前校验，无可派 worker 时拒绝创建并明示原因

**应用层防护不撤**：bashgate 前缀表、permissions 门、只读分类器在沙箱 run 中照常生效（纵深防御）。

### 4.6 安全设计

1. key 全程加密：secrets 表 AES-256-GCM；wire 上 `key_set: bool`；verify 响应不含 key
2. pack 传输：nacos 内网通道，与既有 ManagedSecretKeys 同一信任面，不新增暴露
3. 归档上限：上传/下载双向大小限制，防 worker 磁盘耗尽
4. 临时沙箱强制销毁：verify 与 run 两条路径均 defer destroy，泄漏可经后端自身 TTL 兜底（契约建议适配层实现 TTL，v2 文档注明）
5. 审计：configsvc 沙箱 CRUD/verify 写结构化日志（复用现有 logger 字段模式）；操作审计表暂不引入（开放问题 4）

## 5. 降级与边界（v1 明确不做）

1. **egress 控制**：通用契约不含出口策略 API。沙箱内命令的出口由后端/适配层负责（SPACE 有 egress profile，E2B/Modal 有 allowlist API，自建 fake 无）。管理台 v1 不管理出口策略；契约 v3 评估 `network` 字段（E2B 的 allowOut/denyOut 形状为参照）。
2. **增量 workspace 同步**：全量 tar 往返。大 repo 性能损耗显著，M4 后按需优化（rsync 式 diff 或 git 模式）。
3. **按租户路由后端**：v1 全局单一 enabled 后端。表结构已支持多档案，路由规则留待多租户需求明确后设计。
4. **暂停/恢复/快照**：契约 v2 不含生命周期暂停类端点。SPACE 的差异化能力（暂停/分支/快照）在 v3 契约评估。
5. **个人版**：不接入。个人版 `woca` 二进制不链接 `internal/ee`（`make check-editions` 守护），沙箱管理是企业版能力。

## 6. 测试策略（TDD）

| 层 | 测试 |
|---|---|
| 契约 v2 client | fake sandbox server（httptest）驱动 `Executor` 全 8 方法红绿 |
| fake server 固件 | `internal/platform/executor/fakesandbox/`，同时供单测与 e2e 复用 |
| configsvc | CRUD handler 测试、verify 全步骤成功/中途失败/超时路径、key 不回显断言 |
| pack v2 | round-trip：ExportPack → Runtime.apply → materialize 断言；v1 pack 向后兼容读 |
| worker | per-run 集成：本地 workspace → 沙箱执行 → 产物回写断言；无后端时行为不变回归 |
| server 门控 | 有/无 enabled 后端两姿态的 `/api/runs` 矩阵；sandbox-capable 校验 |
| e2e | compose 栈：server + worker + nacos + fake sandbox 容器；远端租户建 run → 沙箱执行 → 产物可见 |

每任务先写失败测试再实现（superpowers RED-GREEN）。

## 7. 里程碑

| 里程碑 | 内容 | 关键产出 |
|---|---|---|
| M1 | 契约 v2 client + fake server + CONTRACT.md | `Executor` 8 方法全绿 |
| M2 | 档案表 + configsvc CRUD/verify + pack v2 + SandboxSection | 管理台可登记/检测/下发 |
| M3 | worker per-run 执行 + workspace 往返 + RunsOffHost 门控 | 远端租户经沙箱跑通 run |
| M4 | e2e compose 栈 + 边界加固 + 文档 | 可交付 |

M1→M2 串行（契约先定）；M2 前端与 M3 worker 侧可并行。

## 8. 开放问题

1. D3 注入机制可行性：tools registry 的 executor 解析路径需 M3 首日核实，两种预案已列。
2. probe ok 与 enabled 的关系：enabled 但 probe 长期失败的后端，RunsOffHost 判定是否自动降级？（倾向：不自动降级，状态徽章警示 + 人工停用，避免探测抖动引发门控震荡）
3. MCP stdio 在沙箱内的绑定：契约 v2 的 start/kill 已能支撑，v1 不做的理由是连接管理复杂度。是否提前到 v1.1？
4. configsvc 写操作审计表：本设计只记日志。与 opsconsole 的 gw_audit 是否统一，待治理需求明确。
5. 滚动升级：pack v2 要求全角色同步升级。部署文档需明确升级顺序（先升 worker 后升 configsvc）。
6. E2B 兼容面（"走出去"方向）：woca 自建沙箱后端若同时暴露 E2B 兼容 API，可复用 E2B 生态 SDK/工具链。E2B 协议正在事实标准化（agent-sandbox、SPACE ECW 均实现兼容层）。是否纳入 v3？

## 9. 平台适配修订（2026-10-19 晚增补）

依据 `2026-10-19-sandbox-platform-api-survey.md` 对 7 家主流平台 API 的调研，本设计修订如下：

### 9.1 provider 注册表架构

`Executor` 接口是**内部抽象**，通用 HTTP 契约 v2 是**互操作格式**，二者解耦。worker 按 `sandbox_backends.type` 装配三层适配器（成本从低到高）：

1. **契约 v2 客户端**：完全实现 CONTRACT.md 的后端（自建、经适配层的 SPACE）
2. **Go 原生 provider**（进程内，`internal/platform/executor/providers/{e2b,daytona,modal,microsandbox,cf-bridge}/`）：实现 `Executor` 接口，内部调各家 REST/官方 Go SDK（Daytona、Modal 有官方 Go SDK）/JSON-RPC
3. **Sidecar adapter**（独立进程暴露契约 v2）：SDK-only 闭源平台（CodeSandbox Node SDK），v1 暂缓

调研同时确认：Cloudflare sandbox bridge 的 HTTP API（argv/cwd、SSE exec、文件读写、tar persist/hydrate、session）与契约 v2 高度重合，验证了 v2 形状选型；其 32MiB 单文件上限提示 provider 层需要分块或走归档通道。

### 9.2 对前文的修订点

- §4.2 表结构：增加 `type` 列（已改）
- §4.3 verify：按 type 分派 provider（已改）
- §4.5 worker：provision 一步按 type 走 provider 装配路径，不再只有 HTTP 契约客户端单一路径
- §5 边界 1：补充 E2B/Modal 网络策略 API 的存在与 v3 评估（已改）
- §8 开放问题 6：E2B 兼容面（已加）

### 9.3 里程碑影响

M1 契约 v2 + fake server 不变；新增 **M1.5：provider 注册表骨架 + 首个 Go provider（建议 cf-bridge 或 daytona，成本最低）**，用于验证注册表抽象；其余 provider（e2b/modal/microsandbox/space-adapter）排入 M4 后的迭代，M2/M3 主线不依赖。

## 10. 设计审计修订（2026-10-19，子代理审计后）

审计对照代码库逐条核实了 15 组技术假设。3 项 Critical、8 项 Major 的结论与本节的裁决如下（证据链见审计记录）：

### 10.1 Critical 修订

**C-1 路径映射（原设计完全缺失）**：工具以 `snap.Workspace` 宿主绝对路径为基准（`dir.go:41` → bash/file 闭包），沙箱是独立文件系统。裁决：

- 契约约定沙箱工作根为 `/workspace`，`create` 请求带 `cwd` 默认 `/workspace`
- worker 侧实现 **PathTranslatingExecutor 包装器**：把宿主 `snap.Workspace` 前缀翻译为 `/workspace`（双向：请求路径出、错误/匹配路径回），包装在沙箱 executor 外层
- 归档 tar：包内路径 = 相对 workspace 根的相对路径；上传解包到 `/workspace`，下载后解包回宿主 workspace 根
- CONTRACT.md 写死 tar 约定：相对路径、保留权限位、保留符号链接、默认排除 `.git`（可配）

**C-2 绕过 executor 的工具面（原降级矩阵未覆盖）**：Glob/Grep/ListDir（search.go 直连 os）、NotebookEdit、图片读（image.go）、FileHistory undo、EnterWorktree、contextx git status 在沙箱 run 中会静默操作宿主机。裁决（v1）：

- **路由进 executor**：search.go 的 Glob/Grep/ListDir、notebook.go、图片读改为经 Executor 接口（Local 实现行为不变，Sandbox 实现走契约）
- **明确禁用**：FileHistory undo/redo、EnterWorktree、contextx git status 在沙箱 run 中返回"沙箱 run 不支持"错误（安全不变量优先）
- 降级矩阵据此扩充

**C-3 D3 定案**：审计证实 executor 在 ≥6 层闭包/字段捕获（build.go:94-231 全链），hooks 硬连线 local（hooks.go:162-164），MCP stdio 在 BuildAgent 期已 spawn（layers.go:311-317），后台 shell 用 `context.Background()`（background.go:93）。裁决：

- **选 run 上下文覆盖（WithRunExecutor），放弃 per-run BuildAgent**（后者与进程级副作用全面冲突：tracing teardown、技能符号链接、MCP 进程、插件运行时）
- 但 M3 范围按审计证据重估：工具闭包改 ctx 解析 + C-2 的路由改造 + hooks 接入 executor 链 + 后台 shell ctx 捕获 + MCP/LSP 门控，全部显式入 M3
- 若 M3 中发现重铺仍然过大，退路是**轻量 per-run 装配函数**（复用 model/settings/skills，跳过 MCP stdio 与插件安装），不复用副作用缠身的 BuildAgent

### 10.2 Major 修订

| # | 裁决 |
|---|---|
| M-1 | DecodePack 守卫从精确匹配改为范围接受（1、2 均可，按版本分支解释 Sandboxes）；滚动升级"worker 先于 configsvc"为硬约束，写入部署文档 |
| M-2 | materializeTables 增加全量清单语义：pack 中不存在的 sandbox_backends 行删除（含 enabled 行）；managed_skills/mcp 的既有缺陷不在本期修，仅沙箱表带删除传播 |
| M-3 | RunsOffHost 动态化：localGate 对 `/api/runs` 的豁免改为 per-request 判定（查 server 自身 apply 后的物化表/overlay）；**queue 派发（WOCA_RUN_DISPATCH=queue）为前置条件**，写入设计与部署文档 |
| M-4 | config_acks 加 `capabilities` 列（DDL 两处、写点两处）；worker 增加周期性能力 ack（10s ticker 重写 ack 行，含 sandbox 能力标记），解决 15s live 窗口的静默剪除问题 |
| M-5 | 采纳审计简化：ExportPack 本就全量收集 secrets（SecretNames），无需前缀收集代码；DELETE 后端时必须 `SetSecret("sandbox-api-key:<id>", "")` 清密钥 |
| M-6 | SandboxRow 含 last_probe_* 三列进 pack；verify 成功后走 notifyConfig 发布（探测是低频操作员动作，pack 发布成本可接受） |
| M-7 | hooks 的 DefaultCommandRunner（local 硬连线）改为可注入 executor 的 runner；沙箱 run 中 hooks 经 run executor 执行 |
| M-8 | MCP/LSP 门控实现层：工具 Invoke 时解析 run executor，若为沙箱类型则返回"sandbox run 中不可用"明确错误（类型标记判断，不用环境探测） |

### 10.3 Minor 裁决

- m-1：type 枚举定为 7 值（含 `sidecar`）；provider 目录名 `cfbridge`
- m-2：门控只看 enabled，probe 只做徽章警示（与开放问题 2 对齐，消除 §4.5 与 §8.2 的矛盾）
- m-3：sandbox_backends 的 DDL 在 configsvc.New 与 nacosrt worker 侧 apply 路径各建一次（幂等 CREATE IF NOT EXISTS），不依赖启动顺序
- m-4：worker 读 enabled 后端的路径 = 查物化表（新增读取函数），不走 settings overlay
- m-5：config_acks 的 DDL/写点共四处改动清单入计划
- m-6：contextx git status 定案（计划审计后修订）：`SystemPromptFor` 按 workspace 缓存、签名无 ctx、build 期闭包，改造成本失衡；git status 只读宿主 workspace 且与上传沙箱的 workspace 同源，非新增暴露面。保留现状，部署文档记录此限制
- m-7：`sandbox-archive-max-bytes` 定义为 settings 键（settings.Settings 新字段 + keys.go 注册），worker 上传与下载**双向**检查

### 10.4 修订后的 M3 任务面（供计划引用）

1. PathTranslatingExecutor + 归档往返（C-1）
2. 工具闭包 ctx 化：bash/read/write/edit 四工具 + Registry 解析器 + newSubagent 传递
3. C-2 路由：search/notebook/image 进 executor；filehistory/worktree/contextx 禁用门控
4. hooks executor 化（M-7）
5. 后台 shell ctx 捕获
6. MCP/LSP 门控（M-8）
7. worker per-run provision/teardown + 能力 ack 心跳（M-4）
8. RunsOffHost 动态门控（M-3）

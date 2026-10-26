# woca EE 沙箱执行面 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 按 spec 实现 EE 沙箱执行面：scheduler 内嵌 run 执行循环、新角色 woca-executor（contract v2 沙箱宿主 + Docker driver）与 executor-proxy（无状态路由/流控），一条 compose 跑通 `make ent-sandbox-demo`。

**Architecture:** 复用 `internal/ee/worker`（claim/事件镜像/沙箱生命周期）由 scheduler 经 servefront 新 seam `Edition.RunLoop` 内嵌；executor 是**新增**角色（非 worker 改名），Docker 操作收敛在窄接口 `Backend` 后面（单测打 fake，真 Docker 只进 `-tags dockerlab`）；proxy 无状态，按 `X-Woca-Workspace` 头路由，绑定/负载全在 PG（`workspace_bindings` + `config_acks`）。

**Tech Stack:** Go 1.26、`github.com/moby/moby/client`（+`moby/moby/api`，模块已拆分）、`golang.org/x/sys/unix` Flock（已在依赖树）、Vue3 + vitest（configsvc-web）。

**Spec:** `docs/superpowers/specs/2026-10-26-woca-ee-sandbox-plane-design.md`（执行者必读 §3 §4 §5 §6 §7 §8 §11 §17）

## Global Constraints

- 普通 `go test ./...` 不碰 Docker/容器；真 Docker 只进 `-tags dockerlab` + Make 目标（沿 complab/netlab/sandboxlab 惯例）。
- `make check-editions` 必须保持通过：`internal/` 非 ee 包不得 import `internal/ee`；servefront 只出 seam。
- 个人版（cmd/woca、cmd/woca-relay）不链接任何新 EE 面。
- Go 代码遵循 use-modern-go 技能；每个任务先写失败测试再实现；新包语句覆盖率目标 ≥85%。
- 改前端后必须先 `npm run build` 再 go build（webdist 内嵌）。
- 失败语义 fail-closed：沙箱任何环节失败 → run 失败，不回退本地；不静默降级。
- 工作区 ID 字符集 `[A-Za-z0-9_-]`；归档拒绝绝对路径、`..`、逃逸 symlink；`.git` 不上传；上限走 `sandbox-archive-max-bytes`。
- 沙箱容器标签：`woca.sandbox=1`、`woca.workspace=<id>`、`woca.run=<id>`、`woca.executor=<id>`。
- `options` 六字段（image/runtime/network/cpu/memory_mb/env），`network` 只允许 `none|bridge`，`runtime` 只允许 `runc|runsc`；pack 版本不变、`Options` 带 `omitempty`。
- 加固 profile 固定行为（不进 options）；`WOCA_SANDBOX_HARDENING=full|minimal`（minimal 仅 demo，报错不降级）。
- proxy 默认：每 executor 并发 4、队列长 32、排队超时 30s（必须小于后端 TimeoutMs）。
- Docker client 显式 `client.WithHost("unix:///var/run/docker.sock")`，不用 FromEnv。
- 提交信息用 conventional commits（feat/test/docs/chore/refactor）。

## Review Focus

1. **恶意 workspace ID**（`../`、绝对路径、空、大写）→ create 400/拒绝，任何路径不得逃出 `workspace_root`。→ Task 5 步 1、Task 6 步 1。
2. **options 非法值**（`network:"Bridge"`、`runtime:"kata"`、负 cpu、JSON 烂尾）→ configsvc 400，pack 不发布。→ Task 3 步 1。
3. **同工作区并发 run**（跨 session、跨调度器副本）→ 部分唯一索引挡 claim + executor flock 挡 create，两层各测。→ Task 1 步 1、Task 5 步 3、Task 14 步 3。
4. **输出洪泛**：进程狂喷 stdout/stderr → 每会话缓冲上限 8 MiB，截断抬 offset 地板，不 OOM。→ Task 6 步 1。
5. **executor 重启**：会话全灭、对应 run 判失败、flock 内核自动释放、遗留容器按标签清理。→ Task 5 步 4、Task 18 步 1。

---

### Task 1: DB schema — workspace_bindings、runs.workspace_id、config_acks.load

**Files:**
- Modify: `internal/wocadb/db.go`（两套方言迁移序列各加一段）
- Create: `internal/wocadb/workspacebindings.go`
- Test: `internal/wocadb/workspacebindings_test.go`

**Interfaces:**
- Produces: `type WorkspaceBinding struct { WorkspaceID, SourceKey, TenantID, ExecutorID, State string; CreatedAt, UpdatedAt int64 }`；`(d *DB) EnsureWorkspaceBinding(b WorkspaceBinding) (WorkspaceBinding, error)`（`ON CONFLICT(source_key) DO NOTHING` 后重查，幂等 allocate）；`(d *DB) WorkspaceBindingBySourceKey(key string) (WorkspaceBinding, bool, error)`；`(d *DB) WorkspaceBindingByID(id string) (WorkspaceBinding, bool, error)`；`(d *DB) SetWorkspaceBindingState(id, state string) error`；`(d *DB) WorkspaceBindings() ([]WorkspaceBinding, error)`。
- DDL：`workspace_bindings(workspace_id TEXT PRIMARY KEY, source_key TEXT NOT NULL UNIQUE, tenant_id TEXT NOT NULL, executor_id TEXT NOT NULL, state TEXT NOT NULL DEFAULT 'active', created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL)`；`runs` 加 `workspace_id TEXT NOT NULL DEFAULT ''` + 部分唯一索引 `ON runs(workspace_id) WHERE status='running' AND workspace_id<>''`；`config_acks` 加 `load INTEGER NOT NULL DEFAULT 0`。
- 注意 PG 占位符 ph(i) 内部 +1 的历史坑（sqlite `?` 测不出），PG 分支核对序号。

- [ ] **Step 1: 写失败测试**：迁移幂等（Open 两次）；EnsureWorkspaceBinding 同 source_key 二次调用返回同一行；部分唯一索引——插两条 `status='running' AND workspace_id='ws_a'` 第二条必须报错，第一条 finish 后第三条成功；SetConfigAck 读写 load。
- [ ] **Step 2: 跑测试确认失败**（表/列不存在）。
- [ ] **Step 3: 实现迁移与 DAL**（sqlite + PG 双分支）。
- [ ] **Step 4: 跑 `go test ./internal/wocadb/ -run "WorkspaceBinding|Load" -v` 全绿；`go build ./...`。**
- [ ] **Step 5: Commit** `feat(wocadb): workspace bindings, runs.workspace_id partial unique index, ack load column`

### Task 2: contract v2 — create.workspace 字段、sync 端点、X-Woca-Workspace 头

**Files:**
- Modify: `internal/platform/executor/CONTRACT.md`（16 端点表 + workspace 字段 + 可选头）
- Modify: `internal/platform/executor/sandbox.go`（`SandboxConfig` 加 `Workspace string`；create body 加 `workspace`；`withHeaders`/每请求加 `X-Woca-Workspace`；新增 `SyncWorkspace`）
- Modify: `internal/platform/executor/fakesandbox/server.go`（挂 `POST /sandbox/{id}/workspace/sync`，记录 sync 调用供断言）
- Test: `internal/platform/executor/sandbox_wire_test.go`、`sandbox_contract_test.go`

**Interfaces:**
- Produces: `func (s *Sandbox) SyncWorkspace(ctx context.Context) error`（POST `/sandbox/{id}/workspace/sync`，非 2xx 返回错误）；`SandboxConfig{Endpoint, APIKey string, Timeout time.Duration, Workspace string}`。
- Consumes: 现有 `Executor` 接口、`PutArchive/GetArchive` 不动。

- [ ] **Step 1: 写失败测试**：wire 测试断言 create body 含 `"workspace":"ws_x"`、所有请求带 `X-Woca-Workspace: ws_x` 头、SyncWorkspace 打到 `/sandbox/{id}/workspace/sync` 且非 2xx 报错；fakesandbox sync 返回 200。
- [ ] **Step 2: 跑 `go test ./internal/platform/executor/ -run "Sync|Workspace" -v` 确认失败。**
- [ ] **Step 3: 实现 client 与 fakesandbox 改动 + CONTRACT.md 更新**（端点 15→16 表加一行；§create 补 workspace 字段与头的说明）。
- [ ] **Step 4: 全量 `go test ./internal/platform/executor/` 绿（含既有 15 端点契约测试）。**
- [ ] **Step 5: Commit** `feat(executor): contract v2 workspace field, sync endpoint and routing header`

### Task 3: pack options + configsvc CRUD 校验 + 管理台编辑

**Files:**
- Modify: `internal/ee/nacosrt/pack.go`（`SandboxRow.Options *BackendOptions json:"options,omitempty"`）
- Create: `internal/ee/nacosrt/options.go`（`type BackendOptions struct{ Image string json:"image"; Runtime string json:"runtime"; Network string json:"network"; CPU int json:"cpu"; MemoryMB int json:"memory_mb"; Env map[string]string json:"env,omitempty" }` + `func (o *BackendOptions) Normalize() error`——network 空→`none`，仅 `none|bridge`；runtime 空→`runc`，仅 `runc|runsc`；cpu/memory 负数拒绝）
- Modify: `internal/ee/configsvc/handlers_sandboxes.go`（create/update 接受 `options` JSON 文本，校验失败 400）
- Modify: `ee/configsvc-web/src/components/SandboxSection.vue`（行内编辑六字段：image 文本、runtime 下拉 runc/runsc、network 下拉 none/bridge、cpu/memory 数字、env 键值表）
- Test: `internal/ee/nacosrt/options_test.go`、`internal/ee/configsvc/handlers_sandboxes_test.go`（补）、`ee/configsvc-web/src/components/__tests__/SandboxSection.test.ts`

**Interfaces:**
- Produces: `BackendOptions` 上述字段与 `Normalize()`；pack 携带 options，版本号不变。
- 管理台校验规则进 UI 与后端双处（后端为准）。

- [ ] **Step 1: 写失败测试**：`Normalize`——`"Bridge"` 拒绝、空 network 默认 none、`kata` 拒绝、负 cpu 拒绝；handler 测试——options 非法值 POST/PUT 返回 400 且不落库、合法值往返；Vue 测试——六字段渲染、network 选了 bridge 后保存 payload 含 `options.network:"bridge"`、非法提交展示错误。
- [ ] **Step 2: 三处分别跑测试确认失败**（`go test ./internal/ee/nacosrt/ ./internal/ee/configsvc/`、`make ee-web-test`）。
- [ ] **Step 3: 实现 Go 三文件；Vue 组件改完后 `cd ee/configsvc-web && npm run build`。**
- [ ] **Step 4: `go test ./internal/ee/nacosrt/ ./internal/ee/configsvc/` 绿 + `make ee-web-test` 绿 + `go build ./...`。**
- [ ] **Step 5: Commit** `feat(config): sandbox backend options through pack and console`

### Task 4: executor Backend 窄接口 + fakeBackend

**Files:**
- Create: `internal/ee/executor/backend.go`
- Create: `internal/ee/executor/fake_backend.go`
- Test: `internal/ee/executor/fake_backend_test.go`

**Interfaces:**
- Produces:
```go
type Hardening int // HardeningFull | HardeningMinimal
type CreateSpec struct { Image string; Runtime string; Network string; Labels map[string]string; PidsLimit int64; Hardening Hardening; WorkDir string }
type ExecSpec struct { Cmd []string; Dir string; Env []string; Stdin []byte; Timeout time.Duration }
type ExecResult struct { Stdout, Stderr []byte; ExitCode int }
type OutputEvent struct { Stream string; Data []byte; Start int }
type State struct { Running bool; ExitCode int; Labels map[string]string }
type Container interface {
    ID() string
    State(ctx context.Context) (State, error)
    CopyIn(ctx context.Context, dstDir string, tarball io.Reader) error
    CopyOut(ctx context.Context, path string) (io.ReadCloser, error)
    Exec(ctx context.Context, req ExecSpec) (ExecResult, error)
    Start(ctx context.Context, req ExecSpec) (Process, error)
    Remove(ctx context.Context, force bool) error
}
type Process interface {
    WriteStdin(p []byte) error
    CloseStdin() error
    Snapshot(offset int) (events []OutputEvent, next int, done bool, exit int)
    Kill(ctx context.Context) error
    Close() error
}
type Backend interface {
    Create(ctx context.Context, spec CreateSpec) (Container, error)
    List(ctx context.Context, labels map[string]string) ([]Container, error) // 供遗留 GC
}
```
- fakeBackend 用 `os/exec` + 临时目录实现（`sh` 必在），不 import 任何 docker 包。

- [ ] **Step 1: 写失败测试**：fake create→Exec(`echo hi`) 得 stdout、退出码透传（`exit 3`）；Start 长进程（`sh -c 'while :; do echo tick; sleep 0.05; done'`）Snapshot 增量与 offset；WriteStdin/CloseStdin；Kill 后 done；CopyIn/CopyOut tar 往返；Remove 后 State 报错；List 按 label 过滤。
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/ee/executor/ -v` 绿。**
- [ ] **Step 3: Commit** `feat(executor): narrow Backend interface with fake implementation`

### Task 5: workspace store + dirlock + 同步机制

**Files:**
- Create: `internal/ee/executor/workspace.go`（ID 校验、目录打包上传、CopyOut→staging→原子替换）
- Create: `internal/ee/executor/dirlock.go`
- Test: `internal/ee/executor/workspace_test.go`、`dirlock_test.go`

**Interfaces:**
- Produces: `func ValidWorkspaceID(id string) bool`（`^[A-Za-z0-9_-]+$` 且非空）；`type WorkspaceStore struct{...}`，`NewWorkspaceStore(root string, maxBytes int64)`；`(s) Dir(id string) (string, error)`（校验后返回 `root/<id>`，确保存在）；`(s) Upload(ctx, c Container, id string) error`（打包目录：跳过 `.git`、拒绝 symlink 指向根外与绝对路径、超 `maxBytes` 报错 → `CopyIn` 到 `/workspace`）；`(s) Sync(ctx, c Container, id string) error`（`CopyOut /workspace` → staging 目录校验同规则 → `os.Rename` 原子替换；失败保留旧目录）；`type DirLock struct{...}`，`LockDir(path string) (*DirLock, error)`——`unix.Flock(dirfd, LOCK_EX|LOCK_NB)` EINTR 重试，占用返回 `ErrWorkspaceBusy`，`Close()` 释放。

- [ ] **Step 1: 写失败测试**：`ValidWorkspaceID` 拒 `../etc`、`/abs`、空、`ws id`；Upload 打包跳过 `.git`、根外 symlink 报错、超限报错；Sync 往返内容一致 + 失败保留旧目录；flock——同进程二次 LockDir 得 `ErrWorkspaceBusy`、Close 后可再锁、锁不依赖文件内容。
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/ee/executor/ -run "Workspace|DirLock" -v` 绿。**
- [ ] **Step 3: Commit** `feat(executor): workspace store with tar sync and flock mutual exclusion`

### Task 6: executor HTTP 服务（16 端点）

**Files:**
- Create: `internal/ee/executor/service.go`（路由挂载，形状抄 `fakesandbox/server.go`）
- Create: `internal/ee/executor/sessions.go`（procSession/registry/8MiB 截断/TTL reaper）
- Test: `internal/ee/executor/service_test.go`

**Interfaces:**
- Produces: `type ServiceConfig struct{ Backend Backend; Store *WorkspaceStore; MaxConcurrent int; Logger zerolog.Logger; Hardening Hardening; Options func() BackendOptions; Labels func(runID string) map[string]string; ActiveRuns func() int }`；`func NewService(cfg ServiceConfig) http.Handler`。
- 语义：`create` 前抢 workspace flock（持有到 destroy）；create 内 Store.Upload；`exec` 同步聚合并带 Timeout kill；`start/output/input/kill` 会话内存态；`POST /sandbox/{id}/workspace/sync` → Store.Sync；`destroy` 释放锁+Remove（幂等 2xx）；`ActiveRuns` 供 ack load。
- **Review Focus 测试**：会话输出超 8MiB 截断且 `next_offset` 单调、旧事件丢弃后 offset 地板抬升；create 对非法 workspace 400。

- [ ] **Step 1: 写失败测试**：httptest 起服务打 fakeBackend——16 端点全流程（create→exec→read/write/stat/mkdir/glob/grep→start/output offset/input/kill→sync→destroy）；同 workspace 二次 create 503/409（busy）；非法 ID 400；输出洪泛截断；destroy 幂等。
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/ee/executor/ -run Service -v` 绿，包覆盖率 ≥85%（`go test ./internal/ee/executor/ -cover`）。**
- [ ] **Step 3: Commit** `feat(executor): 16-endpoint sandbox service over Backend`

### Task 7: dockerBackend（dockerlab）+ 加固 profile

**Files:**
- Create: `internal/ee/executor/hardening.go`（纯逻辑：`HardeningSpec(level Hardening) dockerless` 结构：CapDrop=["ALL"]、no-new-privileges、ReadonlyRootfs、Tmpfs{"/tmp":""}、masked 默认、PidsLimit=512；minimal 关 nnp/readonly/masked 保留 capdrop/seccomp/pids）
- Create: `internal/ee/executor/docker_backend.go`（`//go:build dockerlab`，moby client；`client.New(client.WithHost("unix:///var/run/docker.sock"), client.WithAPIVersionNegotiation())`）
- Create: `internal/ee/executor/dockerlab_test.go`（`//go:build dockerlab`，真实容器冒烟：alpine 镜像、create/exec/kill 阶梯/CopyIn/Out）
- Modify: `Makefile`（`executor-dockerlab-test:` 目标，沿 sandboxlab 惯例）

**Interfaces:**
- Consumes: Task 4 的 `Backend`/`Container`/`Process`。
- Produces: `func NewDockerBackend(host string) (Backend, error)`；`func KillArgv(sid string, sig string) []string`（`sh -c 'kill -<sig> $(cat /tmp/.woca-ses-<sid>.pid)'`）与 `func WrapArgv(sid string, argv []string) []string`（`sh -c 'echo $$ > /tmp/.woca-ses-<sid>.pid; exec "$@"' sh <argv…>`）——纯函数进 hardening.go 同级文件 `prockill.go`，plain 测试覆盖。

- [ ] **Step 1: 写失败测试（plain）**：HardeningSpec full/minimal 字段差集断言；WrapArgv/KillArgv 生成的命令行字符串精确断言。
- [ ] **Step 2: 实现 hardening.go + prockill.go，plain 测试绿。**
- [ ] **Step 3: 实现 docker_backend.go + dockerlab 冒烟测试；`go build -tags dockerlab ./...` 编译过。**
- [ ] **Step 4: `make executor-dockerlab-test`（本机 OrbStack 有 docker 则跑；无则记录跳过）。**
- [ ] **Step 5: Commit** `feat(executor): docker driver behind dockerlab tag with hardening profile`

### Task 8: executor 配置面（pack 订阅 + OnChange + executor ack）

**Files:**
- Create: `internal/ee/executor/config.go`
- Modify: `internal/ee/worker/worker_ack.go:20`（`AckHeartbeat` 加 `role string` 参数，调用点同步）
- Test: `internal/ee/executor/config_test.go`

**Interfaces:**
- Produces: `type RuntimeParams struct{ BackendOptions + Enabled bool }`；`type ParamsSource struct{...}`，`NewParamsSource(db *wocadb.DB) *ParamsSource`，`(p) Snapshot() RuntimeParams`（读 materialized sandbox_backends + EnabledSandbox）；`(p) Refresh()`（OnChange 回调里调）。ack 心跳：`Role:"executor"`、`Capabilities:"sandbox"`（Enabled 时）、`Load: ActiveRuns()`、10s 间隔、15s 窗口语义不变。

- [ ] **Step 1: 写失败测试**：materialized 表带 options 行 → Snapshot 返回 Normalize 后参数；无 enabled 行 → Enabled=false 且 Capabilities 空；AckHeartbeat 写出的行 Role=executor、Load=活跃数（用 wocadb 内存库断言）。
- [ ] **Step 2: 确认失败 → 实现（worker_ack 参数化 + 全调用点）→ `go test ./internal/ee/executor/ ./internal/ee/worker/ -v` 绿。**
- [ ] **Step 3: Commit** `feat(executor): live backend params from pack and executor ack heartbeat`

### Task 9: worker 库件外移（供 scheduler 复用）

**Files:**
- Modify: `internal/ee/worker/`（新增 `sessions_blob.go`：从 `cmd/woca-worker/main.go:293-321` 移出 `NewFileSessions(dir string, log zerolog.Logger) Sessions`；新增 `states_db.go`：从 :326-342 移出 `NewDBSessionStates(db *wocadb.DB) SessionStates`）
- Modify: `cmd/woca-worker/main.go`（改用导出构造器）
- Test: 移出代码的既有测试跟随（无则补最小构造器测试）

- [ ] **Step 1: 移出并补测试**（blobSessions 落盘/加载一条、states MarkRunning/ClearIfRun 一条）。
- [ ] **Step 2: `go test ./internal/ee/worker/ ./cmd/...` 绿（cmd 测试仅编译断言时用 `go vet ./cmd/...`）。**
- [ ] **Step 3: Commit** `refactor(worker): export FileSessions and DBSessionStates constructors`

### Task 10: servefront RunLoop seam + schedulerloop

**Files:**
- Modify: `internal/servefront/serve.go`（`Edition` 加 `RunLoop func(ctx context.Context, deps RunLoopDeps)`；`RunLoopDeps{Agent *agent.Agent; Settings *settings.Store; Ledger *runstate.Ledger; Plane server.MsgPlane; SessionDir string; Obs Observability; Log zerolog.Logger}`；在 `ed.Start` 同时点调用，nil 不启动）
- Create: `internal/ee/schedulerloop/schedulerloop.go`（装配 `worker.New(worker.Config{...})`：Ledger/Agent 直接透传；Bus=`eventbus.NewLocalWithRetention(1024)`；Sessions=`worker.NewFileSessions`；States=`worker.NewDBSessionStates`；Idle=适配 Plane；DB=`settings.DB()`；SecretFor=GetSecret 前缀闭包；MaxArchiveBytes=settings；Metrics 从 Obs 适配；ID=`"sched-"+shortHost()+"-"+pid`；`go w.AckHeartbeat(ctx, 0, "scheduler", gen, "")`）
- Create: `internal/ee/edition/idle.go`（`func IdleWaiter(p server.MsgPlane) worker.IdleWaiter`——透传 plane 内持 waiter）
- Test: `internal/ee/schedulerloop/schedulerloop_test.go`

- [ ] **Step 1: 写失败测试**：空队列时 loop tick 空转不 panic、ctx 取消即退出；`RunLoop` 为 nil 时 servefront 照常起（既有 servefront 测试回归）。
- [ ] **Step 2: 确认失败 → 实现 seam 与装配 → `go test ./internal/ee/schedulerloop/ ./internal/servefront/ -v` 绿 + `make check-editions` 绿。**
- [ ] **Step 3: Commit** `feat(scheduler): embed worker run loop via servefront RunLoop seam`

### Task 11: 纯改名 woca-server → woca-scheduler

**Files:**
- `git mv cmd/woca-server cmd/woca-scheduler`
- Modify: `Makefile:396`（SERVER_ARTIFACTS）、`:476-477`（check-server-artifacts 硬编码路径）、`:227`（ent-full-up 服务列表）、`:332/:362`（lab roles，如引用）；`deploy/selfhosted/docker-compose.full.yml`（服务名、image、ROLE_PKG、WOCA_SERVICE_NAME、**network alias @477 喂 Prometheus DNS-SD 必改**）、`docker-compose.dev.yml`、`.env.example`；`deploy/selfhosted/gateway/nginx-full.conf:11-12` upstream；`deploy/k8s/20-woca-server.yaml`（文件改名 + 内文）；`deploy/terraform/aws/templates/compose-app.yml.tftpl` 及引用；`deploy/observability/prometheus/prometheus.yml:24-36`、`woca-rules.yaml`、`otelcol/config.yaml`；`e2e/sandboxlab/docker-compose.yml`、`e2e/netlab/docker-compose.enterprise.yml`、`e2e/lab/enterprise.go`、`e2e/netlab/enterprise_test.go`；`internal/ee/nacosrt/client.go:2` 注释、`internal/servefront/serve.go` 注释。
- 纯 rename + 引用替换，零行为改动（nacos pack dataId 是常量不受影响）。

- [ ] **Step 1: git mv + 全量引用替换（grep 驱动：`grep -rn "woca-server" --exclude-dir=.git` 清零为止，历史 docs/plans 除外）。**
- [ ] **Step 2: 验证：`make build build-server check-server-artifacts lint` 全过 + `go build ./...`。**
- [ ] **Step 3: Commit（单笔纯 rename）** `refactor: rename woca-server role to woca-scheduler`

### Task 12: run 门控从 worker ack 改为 executor ack

**Files:**
- Modify: `internal/server/runs.go:531-556`（`hasLiveSandboxWorker` → `hasLiveExecutor`：`a.Role == "executor"` + 15s 窗口 + capabilities 含 `sandbox`；`errNoSandboxWorker` 文案改 executor）
- Test: `internal/server/runs_gate_test.go`（既有测试文件补）

- [ ] **Step 1: 写失败测试**：executor 角色+capability+新鲜 ack → 放行；worker 角色（即便带 sandbox）→ 503；过期 → 503；读库失败 → 503。**
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/server/ -run "Executor|Sandbox" -v` 绿。**
- [ ] **Step 3: Commit** `feat(server): gate off-host runs on live executor acks`

### Task 13: internal/ee/proxy — 绑定/路由/流控/转发

**Files:**
- Create: `internal/ee/proxy/proxy.go`（`Config{DB *wocadb.DB; Limits Limits; Logger}`；`Limits{PerExecutor int=4; QueueLen int=32; QueueTimeout=30s; TenantQuota int}`；`Handler() http.Handler` 挂 contract 转发 + `POST /internal/bindings`）
- Create: `internal/ee/proxy/bind.go`（allocate-or-bind：`EnsureWorkspaceBinding` + 放置策略 load ASC→绑定数 ASC→executor_id 稳定排序；degraded 扫描循环 5s：config_acks 15s 窗口外 → `SetWorkspaceBindingState(degraded)`、回窗 → active）
- Create: `internal/ee/proxy/route.go`（`X-Woca-Workspace` 头 → 绑定 → `config_acks.addr`（Role=executor 且新鲜）；缺头/无绑定/不健康 → 503 带原因与 Retry-After）
- Create: `internal/ee/proxy/queue.go`（每 executor `sem chan struct{}` + 有界 FIFO waiter + 排队超时 + per-tenant 入队计数拒绝）
- Create: `internal/ee/proxy/forward.go`（单个 `httputil.ReverseProxy`，Rewrite 按 ctx 目标改写，流式透传不读 body；create 的小 JSON body 缓冲读 workspace 后重设）
- Test: `internal/ee/proxy/*_test.go`

- [ ] **Step 1: 写失败测试**：同 source_key 并发 allocate 只产生一条绑定且双方拿到同 ID；不同租户同路径不同绑定；放置选 load 最小 executor、并列选绑定数最小；ack 过期 → 绑定 degraded → 请求 503（错误体含 degraded 与恢复说明）；ack 回窗 → active → 转发恢复；队列满 503、排队超时 503（`Limits.QueueTimeout` 缩短驱动）、客户端断连释放队列位；单租户灌满额度后其他租户仍可入队；转发保持 tar.gz 流（大 body 不落内存——用 io.Pipe 计数断言）。**
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/ee/proxy/ -v` 绿，覆盖率 ≥85%。**
- [ ] **Step 3: Commit** `feat(proxy): stateless workspace router with bounded queues and bindings`

### Task 14: session 身份 + enqueue 继承 + claim 互斥

**Files:**
- Modify: `internal/server/sessions.go`（`createSession`：runsOffHost 且有 enabled backend 时读 Endpoint 调 `POST {Endpoint}/internal/bindings {"tenant_id","source_key"}`（source_key=tenant+规范化路径），workspace_id 存 session 元数据；失败 503 不兜底；本地/个人版不调用）
- Modify: `internal/server/runs.go`（`enqueueRun`：session 元数据有 workspace_id 则写入 run 行；旧 session 无值时按同 API 现场补齐一次）
- Modify: `internal/runstate/runstate.go:171`（`Claim` 的选取子查询加 `NOT EXISTS (SELECT 1 FROM runs r2 WHERE r2.workspace_id = runs.workspace_id AND r2.workspace_id <> '' AND r2.status = 'running')`，冲突行跳过取下一条）
- Modify: `internal/server/workspace.go`、`fsapi.go`（EE 下解析链 session → workspace_id → `workspace_root/<id>`；路径限制在卷根内——个人版行为不变）
- Test: `internal/server/sessions_test.go` 补、`internal/runstate/runstate_test.go` 补

- [ ] **Step 1: 写失败测试**：同租户同路径两 session 同 workspace_id；不同租户不同；proxy 不可达 → createSession 503；enqueue 写入 runs.workspace_id；runstate——同 workspace 一条 running 时第二条 queued 不被 claim（claim 返回第三条无关 run），running 结束后可 claim；workspace.go 解析拒绝卷外路径（`/etc`）。**
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/server/ ./internal/runstate/ -v` 绿。**
- [ ] **Step 3: Commit** `feat(server): session workspace identity and per-workspace run mutual exclusion`

### Task 15: 调度器侧沙箱流程改 workspace + SyncWorkspace

**Files:**
- Modify: `internal/ee/worker/worker_sandbox.go`（`provisionRunSandbox`：`SandboxConfig.Workspace = <run 的 workspace_id>`（从 snapshot/session 解析传入）；run 成功末尾 `SyncWorkspace` 替代 `GetArchive` 回写；sync 失败 → run error）
- Test: `internal/ee/worker/worker_sandbox_test.go`（补）

- [ ] **Step 1: 写失败测试**：provision 后 create 请求体含正确 workspace；成功路径调用 sync 且成功；sync 失败 → run 判 error 且错误含 "sync"；无 workspace_id（个人/过渡）行为不回归（本地 executor 路径不变）。**
- [ ] **Step 2: 确认失败 → 实现 → `go test ./internal/ee/worker/ -v` 绿。**
- [ ] **Step 3: Commit** `feat(worker): run-scoped workspace id and sync-based artifact confirmation`

### Task 16: cmd/woca-executor + cmd/executor-proxy 入口

**Files:**
- Create: `cmd/woca-executor/main.go`（flags：`--workspace-root`、`--listen`、`WOCA_SANDBOX_HARDENING`；nacosrt Runtime Service `woca-executor` Role `executor`；ParamsSource OnChange；ack 心跳；Service 挂 Backend=docker（fakeBackend 仅 `WOCA_DEV_FAKE_BACKEND=1` 显式开发开关，日志大声警告）；启动时标签 GC（Task 18 复用））
- Create: `cmd/executor-proxy/main.go`（`WOCA_DB` nacos:// 解析（wocadb DSNResolver seam）→ wocadb.Open → proxy.Handler；不注册 nacos、不写 ack）
- Modify: `internal/ee/configsvc/handlers_sandboxes_verify.go`（verify 改道：先 `POST {Endpoint}/internal/bindings` 用固定 `source_key=verify:<backend-id>` 拿临时 workspace，再以带 workspace 的 client 走 contract 全路径 create→exec→destroy；不再在 configsvc 进程内 `providers.New` 直连）
- Test: `cmd/woca-executor/main_test.go`（最小编译/装配）、`internal/ee/configsvc/handlers_sandboxes_verify_test.go`（改道断言：请求打向 proxy 地址、create 带 verify workspace、destroy 后绑定保留复用）

**Interfaces:**
- Consumes: Task 6 `NewService`、Task 7 `NewDockerBackend`、Task 8 `NewParamsSource`、Task 13 `Handler()` 与 `/internal/bindings`。

- [ ] **Step 1: 写失败测试**：verify 测试——httptest 起 fake proxy（记录 bindings 与转发），断言 verify 的 create 打到 proxy、body 含 `workspace`（verify:<id> 前缀绑定复用同一 workspace）、configsvc 进程内不再出现直连 backend 的调用。
- [ ] **Step 2: 确认失败 → 实现两入口与 verify 改道。**
- [ ] **Step 3: 验证：`go test ./internal/ee/configsvc/ ./cmd/woca-executor/ -v`、`go build ./...`、`make check-editions`（新 cmd 属 EE 面，勿链接进个人版）。**
- [ ] **Step 4: Commit** `feat(cmd): executor and proxy entrypoints, verify rerouted through proxy`

### Task 17: compose full 重排 + demo profile + make ent-sandbox-demo

**Files:**
- Modify: `deploy/selfhosted/docker-compose.full.yml`（`woca-scheduler` ×2：加 `woca-workspaces` 卷 + `WOCA_RUN_DISPATCH=queue` + `WOCA_RUNS_OFF_HOST=true`；新 `woca-executor`：挂 `/var/run/docker.sock` + 卷 + `--workspace-root /data/workspaces`；新 `executor-proxy`；woca-worker 服务移出 EE 栈（dev 栈保留）；新卷 `woca-workspaces`；`demo` profile：`fakemodel`（复用 `e2e/sandboxlab/fakemodel`）、`sandbox-init`（写模型 settings + sandbox backend options）、`sandbox-demo`（四段启动断言、退出码即结论、断言经 executor 标签查询端点无遗留容器））
- Create: `e2e/sandboxlab/demo/`（demo 容器脚本/镜像）
- Modify: `Makefile`（`ent-sandbox-demo:` 驱动四段顺序；artifact 加 woca-executor/executor-proxy）
- Test: demo 本身即 e2e（`make ent-sandbox-demo` 退出码 0）

- [ ] **Step 1: compose 改排 + demo profile + Makefile 目标。demo 容器断言按 spec §12 全四级：一级脚本模型（空工作区→产物落卷→沙箱容器回收，回收经 executor 标签查询端点断言）；二级跨轮次（第二个 run 读到第一轮文件、同工作区锁生效、沙箱是新容器）；三级热生效（管理台换带标记文件的镜像→不重启→新建沙箱有标记→运行中不受影响）；权限与清理（他人访问 403、外网连接失败、无本地回退、无遗留容器）。**
- [ ] **Step 2: 本机跑 `make ent-sandbox-demo`（OrbStack）；失败则修到退出码 0。**
- [ ] **Step 3: Commit** `feat(deploy): EE sandbox compose with demo profile`

### Task 18: 故障语义 + 遗留容器 GC + 文档收尾

**Files:**
- Create: `internal/ee/executor/gc.go`（启动 + 周期：`List({woca.sandbox=1, woca.executor=<self>})`，不在活跃集合 → Remove force）
- Test: `internal/ee/executor/gc_test.go`（fakeBackend 造遗留容器断言清理与保留）
- Modify: `deploy/selfhosted/README-FULL.md`（角色/卷/demo 说明替换旧名；补 §8.5 运维纪律：runtime 与内核安全更新例行化、逃逸类高危 CVE 7 天滚动升级 SLO、executor 滚动重启语义）

- [ ] **Step 1: 写失败测试（GC 保留活跃、清掉孤儿）→ 实现 → 绿。**
- [ ] **Step 2: 故障注入测试核对**：executor 不可达 → run 失败不回退（既有 worker 测试覆盖面确认，缺则补）；`go test ./... -count=1` 全绿 + `make lint`。**
- [ ] **Step 3: Commit** `feat(executor): labeled leftover container GC and fault semantics polish`

---

## 执行顺序与依赖

Task 1→2→3 顺序无相互依赖可并行；4→5→6 严格顺序；7 依赖 4；8 依赖 1/3；9 独立；10 依赖 9；11 独立（纯 rename，宜早做减小后续 diff 噪音，但避免与 12-16 同批）；12 独立；13 依赖 1/8；14 依赖 13；15 依赖 2/14；16 依赖 6/7/8/13；17 依赖 16；18 收尾。

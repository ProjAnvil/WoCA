# 沙箱管理台与企业执行链路 — 实施计划

日期：2026-10-19
设计依据：`docs/superpowers/specs/2026-10-19-sandbox-console-design.md`（含第 9、10 节审计修订）
调研依据：`docs/superpowers/specs/2026-10-17-space-sandbox-research.md`、`2026-10-19-sandbox-platform-api-survey.md`

## 总则

- 每任务先写失败测试（RED），再实现（GREEN），再 `go build ./... && go vet ./...`。
- 修改既有文件前必须先读目标文件；本计划标注的行号以 2026-10-19 代码为准，若有漂移以符号名为准。
- 个人版二进制不得链接 `internal/ee`（`make check-editions` 必须保持通过）。
- 验证命令统一在仓库根执行。

## M1 契约 v2 + fake server

### 任务 1：契约文档 CONTRACT.md

- 文件路径：`internal/platform/executor/CONTRACT.md`（新建）
- 要做的：写通用 HTTP 契约 v2 规范，内容：
  1. 请求头：所有请求带 `Authorization: Bearer <key>`（可选）与 `X-Woca-Sandbox-Contract: 2`
  2. 端点表（15 个，照设计 §4.1 的表逐条写成 curl 示例 + JSON 形状）
  3. `exec` 请求体：`{"argv":["..."],"cwd":"/workspace","env":{"K":"V"},"stdin":"<base64>","timeout_ms":30000}`；响应 `{"exit_code":0,"stdout":"<base64>","stderr":"<base64>"}`
  4. `start` → `{"session_id":"..."}`；`output?offset=N` → `{"events":[{"stream":"stdout|stderr","data":"<base64>"}],"done":true,"exit_code":0,"next_offset":M}`；`input` `{"data":"<base64>"}`；`kill`
  5. 文件面：`read?path=` → `{"content":"<base64>"}`；`write` `{"path","content","mode"}`；`stat?path=` → `{"mode":420,"size":1,"mtime_ms":0,"is_dir":false}`；`mkdir` `{"path"}`；`glob` `{"pattern"}` → `{"matches":["..."]}`；`grep` `{"root","pattern","max_matches":200}` → `{"matches":[{"path","line","text"}]}`
  6. 归档：`PUT /archive`（`Content-Type: application/gzip`，tar.gz 体）；`GET /archive` → tar.gz 体。tar 约定：条目路径为相对 workspace 根的相对路径；保留权限位与符号链接；默认排除 `.git`；解包目标为沙箱 `/workspace`
  7. 错误语义：非 2xx + `{"error":"..."}`；建议后端实现沙箱 TTL 兜底销毁
  8. 注意：stdout/stderr/grep text 均 base64 传输（保二进制安全）；env 为对象非数组
- 验证：文档覆盖 15 端点 + tar 约定；`ls internal/platform/executor/CONTRACT.md` 存在

### 任务 2：契约 v2 wire 类型与 Exec 修订

- 文件路径：`internal/platform/executor/sandbox.go`
- 要做的：
  1. `do()` 中为每个请求加头 `req.Header.Set("X-Woca-Sandbox-Contract", "2")`
  2. `Exec` 请求体替换（删除 `strings.Join`）：
     ```go
     body := map[string]any{
         "argv":  append([]string{req.Name}, req.Args...),
         "cwd":   req.Dir,
         "env":   envSliceToMap(req.Env),
         "stdin": base64.StdEncoding.EncodeToString(req.Stdin),
     }
     ```
     响应体 `stdout/stderr` 从 string 改 base64 解码。`envSliceToMap`（新 helper，`K=V` 拆分）放同文件。
  3. `create` 请求体从 `nil` 改为 `{"cwd":"/workspace"}`
  4. `WriteFile` 请求体加 `"mode": int(perm)`
- 验证：新建 `sandbox_wire_test.go`：httptest server 捕获请求体，断言 argv/env/stdin/cwd/mode 的 JSON 形状与 base64 编码；`go test ./internal/platform/executor/ -run TestSandboxWire`

### 任务 3：Start 与 sandboxProcess

- 文件路径：`internal/platform/executor/sandbox_process.go`（新建）
- 要做的：
  1. `Sandbox.Start`（替换 sandbox.go:148 的 stub）：`POST /sandbox/{id}/start` `{"argv","cwd","env"}` → `{"session_id"}`；构造 `*sandboxProcess`
  2. `sandboxProcess` 实现 `Process` 接口：
     - 字段：`s *Sandbox, sid string, stdinW io.WriteCloser, stdoutR io.Reader, exitCh chan error, closeOnce sync.Once`
     - 构造时：`io.Pipe()` 两侧；起 goroutine：把写入 `stdinW` 的数据转发到 `input` 端点（batch，~4KB 或 flush 间隔）；起 goroutine 轮询 `output?offset=`（200ms 间隔），`stdout` 事件 base64 解码后写 pipe 写端（`StartOptions.Stdout` 非 nil 时直写该 writer，`Stdout()` 返回 nil），`stderr` 事件写 `StartOptions.Stderr`（nil 丢弃）；`done=true` 时按 `exit_code` 决定 `exitCh <- nil` 或 `exitCh <- &ExitError{Code}`，关闭 pipe
     - `Stdin()` 返回 `stdinW`；`Stdout()` 返回 `stdoutR`；`Wait()` 阻塞 `exitCh`；`Kill()` 调 `kill` 端点并触发清理
  3. 删除 `sandbox.go` 中 `Start` 的 `ErrSandboxNotImplemented` stub 与顶部注释中"Start not available"表述
- 验证：`sandbox_process_test.go`（httptest 假后端）：Start `["cat"]` → 写 stdin "hi" → Stdout 读到 "hi" → 关闭 stdin → Wait nil；Start `["sh","-c","exit 3"]` → Wait 返回 `*ExitError{3}`；Kill 后 Wait 返回

### 任务 4：文件面补齐

- 文件路径：`internal/platform/executor/sandbox.go`
- 要做的：
  1. `Stat`：`GET .../stat?path=` → `{"mode","size","mtime_ms","is_dir"}`；实现 `sandboxFileInfo`（`fs.FileInfo`：Name/Size/Mode/ModTime/IsDir/Sys），新类型放同文件或 `sandbox_file.go`
  2. `MkdirAll`：`POST .../mkdir` `{"path"}`（perm 忽略，契约不传）
  3. `Grep`：`POST .../grep` `{"root","pattern":pattern.String(),"max_matches":200}` → 组装 `[]GrepMatch`
  4. 删除三个 stub 与 `ErrSandboxNotImplemented` 中对应项（变量保留给 provider 未实现场景）
- 验证：`sandbox_fs_test.go`：stat/mkdir/grep 对照 httptest 假后端断言；`go test ./internal/platform/executor/`

### 任务 5：归档端点

- 文件路径：`internal/platform/executor/sandbox.go`（追加）
- 要做的：
  ```go
  // PutArchive uploads a gzipped tar of the workspace into the sandbox.
  func (s *Sandbox) PutArchive(ctx context.Context, r io.Reader) error
  // GetArchive downloads the sandbox workspace as a gzipped tar.
  func (s *Sandbox) GetArchive(ctx context.Context) (io.ReadCloser, error)
  ```
  实现：`do` 不能复用（非 JSON），新 helper `doRaw(ctx, method, path, contentType string, body io.Reader) (*http.Response, error)`（同样带契约头与 Bearer；状态码 ≥300 时读 body 限 4KB 报错）。
- 验证：`sandbox_archive_test.go`：上传 1KB 随机 gzip → GET 回读 → 字节一致

### 任务 6：fakesandbox 测试服务器

- 文件路径：`internal/platform/executor/fakesandbox/server.go`（新建包）
- 要做的：完整实现契约 v2 全部端点的内存版：
  1. `New() *Server`（`http.Handler` 挂载；内部 `map[sandboxID]*fakeSandbox` + mutex）
  2. `fakeSandbox`：`fs map[string][]byte`（路径→内容）、`dirs map[string]bool`、`sessions map[string]*fakeSession`
  3. `exec/start`：用 `os/exec.Command` 在 fake server 进程内真实执行（`cwd` 传请求值；env 覆盖）；`exec` 聚合输出同步返回；`start` 起 session，输出写入 ring buffer 供 `output?offset=` 读取，`input` 写进程 stdin，`kill` 杀进程
  4. 文件面：内存 map 实现 read/write/stat/mkdir/glob（`path.Match` 遍历 keys）/grep（遍历文件逐行正则）
  5. 归档：`PUT` 解 tar.gz 写入 fs map；`GET` 从 fs map 打 tar.gz
  6. 导出 `Handler() http.Handler` 与 `Addr(t *testing.T) string`（自选随机端口）等测试辅助
- 验证：`server_test.go` 自举（create→exec→文件→归档全链路 curl 级测试）

### 任务 7：Executor 合规测试（M1 收口）

- 文件路径：`internal/platform/executor/sandbox_contract_test.go`（新建）
- 要做的：对照 `fakesandbox` 跑 `Sandbox` 的全部 8 个 `Executor` 方法 + `Process` 生命周期 + 归档往返（`var _ Executor = (*Sandbox)(nil)` 之外的运行时验证）。关键断言：Exec exit code 语义（非零不返回 error）、Start 的 Stdin/Stdout/Wait/Kill、Grep 跳过隐藏目录的语义与 fake 行为一致、归档 round-trip 保权限位
- 验证：`go test ./internal/platform/executor/...` 全绿；`go vet ./internal/platform/executor/...`

## M1.5 provider 注册表

### 任务 8：注册表骨架

- 文件路径：`internal/platform/executor/providers/registry.go`（新建包 `providers`）
- 要做的：
  ```go
  package providers

  // Config is the resolved connection profile for one sandbox backend.
  type Config struct {
      Type     string        // contract-http | cfbridge | e2b | daytona | modal | microsandbox | sidecar
      Endpoint string
      APIKey   string
      Timeout  time.Duration
  }

  // New builds an Executor for the backend type. Unknown types error.
  func New(cfg Config) (executor.Executor, error)
  ```
  v1 只注册 `contract-http`（调 `executor.NewSandbox`，转换字段）与 `cfbridge`（任务 9）。`New` 返回前对 Endpoint 做 URL 校验（仿 sandbox.go:57-59）。
- 验证：`registry_test.go`：known type 构造成功、unknown type 报错、坏 URL 报错

### 任务 9：cfbridge provider

- 文件路径：`internal/platform/executor/providers/cfbridge/cfbridge.go`（新建）
- 要做的：实现 `executor.Executor`，对接 Cloudflare sandbox bridge API（Bearer `SANDBOX_API_KEY`）：
  1. 生命周期：`POST /v1/sandbox` → `{id}`；`DELETE /v1/sandbox/{id}`；`GET /v1/sandbox/{id}/running`
  2. `Exec`：`POST /v1/sandbox/{id}/exec` `{argv, timeout_ms, cwd}`——响应是 SSE 流：逐事件解析（`stdout`/`stderr` base64 聚合、`exit` 取 exit_code、`error` 记为 stderr 并 exit_code=-1）
  3. `Start`：同 exec 端点但保持 SSE 连接，`sandboxProcess` 变体直接消费流事件（stdout 事件→pipe，exit→exitCh）；stdin 不可用（CF bridge 无 input 端点），`Stdin()` 返回写入即丢弃的 `io.NopCloser`（文档注明限制）
  4. 文件面：`GET/PUT /v1/sandbox/{id}/file/<path>` 原始字节（路径逐段 `url.PathEscape`）；`Stat` 用 `exec ["stat","-c","%a %s %Y %F",path]` 解析（或 `ls -ld --time-style=+%s`，测试钉死）；`Glob`/`Grep`/`MkdirAll` 经 `exec` 跑 `find`/`grep`/`mkdir -p`（命令形状在测试中钉死）
  5. 归档：`POST /v1/sandbox/{id}/persist`（?excludes=.git）→ tar 体；`POST /v1/sandbox/{id}/hydrate` ← tar 体（≤32MiB，超限报错）
  6. 构造时 `New(cfg)`：先 `POST /v1/sandbox` 建沙箱；`Close()` 销毁（仿 Sandbox 生命周期）
- 验证：`cfbridge_test.go`：httptest 模拟 CF bridge 形状（SSE 手工编 event 流）驱动全部方法；32MiB 超限路径

## M2 configsvc + Pack v2 + 前端

### 任务 10：sandbox_backends DDL 与 SandboxRow

- 文件路径：`internal/ee/configsvc/configsvc.go`（New 迁移块，:96-125 后追加）、`internal/ee/nacosrt/pack.go`
- 要做的：
  1. configsvc.New 迁移块加（仿 managed_skills 的 `tsCol` 方言处理）：
     ```sql
     CREATE TABLE IF NOT EXISTS sandbox_backends (
       id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
       type TEXT NOT NULL DEFAULT 'contract-http',
       endpoint TEXT NOT NULL, timeout_ms INTEGER NOT NULL DEFAULT 120000,
       enabled INTEGER NOT NULL DEFAULT 0,
       last_probe_at INTEGER, last_probe_ok INTEGER, last_probe_error TEXT,
       created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
     )
     ```
  2. pack.go 加类型：
     ```go
     // SandboxRow is one managed sandbox backend, carried in pack v2.
     type SandboxRow struct {
         ID, Name, Type, Endpoint string
         TimeoutMs                int64
         Enabled                  bool
         LastProbeAt              int64 // unix ms, 0 = never
         LastProbeOK              bool
         LastProbeError           string
     }
     ```
- 验证：`configsvc_test.go` 追加：New 后 `sandbox_backends` 表存在（sqlite 内存库）

### 任务 11：CRUD handlers

- 文件路径：`internal/ee/configsvc/handlers_sandboxes.go`（新建）、`internal/ee/configsvc/configsvc.go`（Handler() 注册路由，仿 :184-194）
- 要做的：
  1. 路由：`GET /api/sandboxes`（read）、`POST /api/sandboxes`（write）、`PUT /api/sandboxes/{id}`（write）、`DELETE /api/sandboxes/{id}`（write）
  2. 校验：`type` ∈ 7 枚举（contract-http/cfbridge/e2b/daytona/modal/microsandbox/sidecar，常量切片放文件顶部）；endpoint URL 合法；name 唯一（DB 约束 + 友好错误）
  3. POST：插行 + `SetSecret("sandbox-api-key:"+id, key)`（key 可空）+ `notifyConfig()`
  4. PUT：部分更新（key 非空才覆写 secret）；`enabled=true` 时同事务把其余行 `enabled=0`（v1 单 enabled）+ `notifyConfig()`
  5. DELETE：`enabled=1` 的行拒绝（409，提示先停用）；删除行 + `SetSecret("sandbox-api-key:"+id, "")`（空值即删除，settings.go:674-677 语义）+ `notifyConfig()`
  6. GET 列表：回 `key_set: bool`（`HasSecret`），**绝不回 key**
  7. CRUD 写操作与 verify 均写结构化日志（仿 configsvc 现有 logger 字段模式：action/backend_id/type/operator，verify 额外记 ok 与耗时；设计 §4.6.5）
- 验证：`handlers_sandboxes_test.go`：CRUD 全路径 + key 不回显断言（响应体不含 key 明文）+ enabled 唯一 + enabled 不可删 + secret 随删清理

### 任务 12：verify 探活

- 文件路径：`internal/ee/configsvc/handlers_sandboxes.go`（追加）、`internal/ee/configsvc/configsvc.go`（注册 `POST /api/sandboxes/{id}/verify`，write scope）
- 要做的：
  1. 读行 + `GetSecret("sandbox-api-key:"+id)` → `providers.Config` → `providers.New`（type 分派即由此完成）
  2. 分步探活（每步超时 `min(timeout_ms, 30s)`）：`exec ["echo","woca-verify"]` 断言 stdout；`WriteFile("/workspace/.woca-verify",...)`+`ReadFile` 往返；`Glob`（匹配刚写文件）；`Stat`；`Start ["sh","-c","echo long"]`+读输出+`Kill`；`PutArchive`/`GetArchive` 小往返（gzip 空 tar 或单文件）
  3. providers.New 的构造本身覆盖 create 步；任何一步失败即终止，`defer` best-effort 清理（Executor 无 Close 时跳过——`providers.New` 返回 `executor.Executor`，契约族有 `Close()`，定义 `interface{ Close() error }` 断言可选清理）
  4. 回写 `last_probe_at/ok/error` + `notifyConfig()`
  5. 响应：`{"ok":bool,"total_latency_ms":N,"steps":[{"name","ok","latency_ms","error"}]}`；不含 key
- 验证：`handlers_sandboxes_verify_test.go`：httptest 起 `fakesandbox.Handler()` 作为 contract-http 后端 → verify 全绿；用坏 endpoint → 首步失败、ok=false、error 落库；f2b 等 v1 未注册 type → verify 报 "provider not available"（同时 POST /api/sandboxes 对未注册 type 允许登记——type 枚举是契约全集，注册表是运行时能力）

### 任务 13：Pack v2

- 文件路径：`internal/ee/nacosrt/pack.go`
- 要做的：
  1. `Pack` 加字段 `Sandboxes []SandboxRow \`json:"sandboxes,omitempty"\``
  2. `packVersion` 1→2
  3. `DecodePack`（:154-156）守卫重写：
     ```go
     if p.Version < 1 || p.Version > packVersion { return nil, fmt.Errorf(...) }
     // v1 packs predate sandbox backends: treated as an empty set.
     if p.Version < 2 { p.Sandboxes = nil }
     ```
  4. `ExportPack`（:51-116）：追加查询 `sandbox_backends` 全表 → `[]SandboxRow`（表不存在时静默跳过——兼容仅有 v1 表结构的部署）
- 验证：`pack_test.go` 追加：Export→Decode round-trip 含 sandbox 行；手工构造 v1 JSON → Decode 成功且 Sandboxes 为 nil；v3 拒绝

### 任务 14：worker 物化与读取

- 文件路径：`internal/ee/nacosrt/runtime.go`（materializeTables :247-273）、`internal/ee/nacosrt/sandboxes.go`（新建）
- 要做的：
  1. materializeTables 前置 ensure DDL（幂等 CREATE，仿 configsvc 但无锁场景按包内 helper）；随后 **全量清单语义**：upsert pack 中的行 + `DELETE FROM sandbox_backends WHERE id NOT IN (pack 行)`（空 pack 清空全表；prepared in-place 拼 id 占位符）
  2. 新文件导出：
     ```go
     // EnabledSandbox returns the enabled backend row, or nil when none.
     func EnabledSandbox(db *sql.DB) (*SandboxRow, error)
     ```
- 验证：`runtime_test.go` 追加：apply 含 2 行的 pack → 表有 2 行；再 apply 只含 1 行的 pack → 旧行被删；EnabledSandbox 返回唯一 enabled

### 任务 15：前端 SandboxSection

- 文件路径：`ee/configsvc-web/src/components/SandboxSection.vue`（新建）+ `SandboxSection.test.ts`（新建）、`src/App.vue`、`src/types.ts`、`src/i18n/locales/en.ts`、`src/i18n/locales/zh-CN.ts`
- 要做的：
  1. types.ts：`SandboxBackend {id,name,type,endpoint,timeout_ms,enabled,key_set?,last_probe_at?,last_probe_ok?,last_probe_error?}` 与 `VerifyResult`
  2. App.vue：`sections` computed（:182-190）加 `{id:'sandbox', label:t('nav.sandbox'), icon:Box}`（lucide `Box` 需同步 import）；模板加 `<SandboxSection v-if="active==='sandbox'"/>`
  3. SandboxSection.vue：列表（name/type/endpoint/enabled 徽章/probe 状态徽章+时间）；表单（name/endpoint/type 下拉 7 项/timeout_ms/api key password input 非空才发送）；"测试连接"按钮独立 fetch `POST /api/sandboxes/{id}/verify`（照 OpsSection.vue:22-35 模式），分步结果折叠展示；启用开关（PUT enabled，服务端保证唯一）；删除按钮（enabled 时禁用）
  4. i18n 两个 locale 补 `nav.sandbox` 与全部文案
  5. SandboxSection.test.ts（照 ModelSection.test.ts / StorageSection.test.ts 的 vitest 惯例）：组件渲染、key 只显 `key_set` 不回显、type 下拉 7 项、表单校验
- 验证：`cd ee/configsvc-web && bun run test` 通过；`bun run build` 成功；`make ee-web` 后 `go build ./internal/ee/configsvc/` 成功（embed 更新）

### 任务 16：M2 收口回归

- 要做的：全量 `go test ./internal/ee/... ./internal/platform/executor/...`；`make check-editions`；前端 `bun run test`（存在，Makefile ee-web-test 目标可佐证）
- 验证：以上全部通过

## M3 执行链路

### 任务 17：PathTranslatingExecutor

- 文件路径：`internal/platform/executor/translate.go`（新建）
- 要做的：
  ```go
  // Translate wraps an Executor and remaps host workspace paths to the
  // sandbox root (and results back), per the contract's /workspace rule.
  type Translate struct {
      Inner     Executor
      HostRoot  string // absolute host workspace path
      SandboxRoot string // contract default: /workspace
  }
  ```
  实现 `Executor` 全 8 方法：请求路径 `HostRoot/... → SandboxRoot/...`（`strings.TrimPrefix` + `filepath.Join` 归一）；响应路径反向（Glob matches、GrepMatch.Path、Stat 的 fileInfo.Name 之外的完整路径来源——Stat 若拿不到全路径则原样返回并注释限制）；`Exec` 的 `Dir` 同样翻译。**先读 registry.go/dir.go 确认工具传给 executor 的路径形态（绝对 vs 相对），若为相对路径则翻译规则改为"相对 workspace 根"直传。**
- 验证：`translate_test.go`：fake Executor 记录收到的路径，断言双向翻译

### 任务 18：WithRunExecutor 与工具闭包 ctx 化

- 文件路径：`internal/runexec/runexec.go`（新建包，仿 `internal/runws` 的 ctx 值载体模式——`internal/agent` 已 import `internal/tools`，放 agent 包会造成循环依赖）、`internal/tools/registry.go`、`internal/tools/bash.go`、`internal/tools/file.go`、`internal/agent/agent.go`
- 要做的：
  1. runexec 包：
     ```go
     package runexec

     func WithRunExecutor(ctx context.Context, x executor.Executor) context.Context
     func RunExecutorFrom(ctx context.Context) (executor.Executor, bool)
     ```
     （agent 与 tools 各自 import 本包；不引入反向依赖）
  2. registry.go：`Registry` 加方法 `func (r *Registry) ExecFor(ctx context.Context) executor.Executor`——ctx 有 run executor 用之，否则回退 `r.exec` 字段（字段保留，build 路径不变）；同时导出 `func (r *Registry) ExecForFn() func(context.Context) executor.Executor` 供工具闭包注入
  3. bash.go（:52 `x.Exec`）、file.go（:36/:82/:107 等所有 executor 调用点）：工具闭包从捕获 `x` 改为捕获 `execFor func(ctx) executor.Executor`（由 `defaultTools` 装配处经 `r.ExecForFn()` 注入；调用点 `x.Exec(...)` 改 `execFor(ctx).Exec(...)`）
  4. agent.go:1817 `newSubagent`：子 Registry 构造（`NewRegistryWithExecutor(ws, a.exec)`）改为传入父级的 `execFor` 解析逻辑（ctx 会随 Invoke 传到子代理工具）
- 验证：`internal/runexec/runexec_test.go` + 工具单测：ctx 注入 fake executor → 工具操作落在 fake；无注入 → 落在 build 时 executor（行为不变回归）

### 任务 19：绕过工具路由进 executor

- 文件路径：`internal/tools/search.go`、`internal/tools/notebook.go`、`internal/tools/image.go`（经 file.go:29 调用）
- 要做的：
  1. search.go:28 ListDir：`os.ReadDir` 改 `execFor(ctx).Glob(ctx, filepath.Join(dir,"*"))` + Stat 区分目录，或给 Executor 加只读 `ListDir`？**不加接口**——用 Glob+Stat 组合实现（保持接口冻结）；:144-160 walkMatches 的 Glob/Grep 分支改调 `ExecFor` 的 `Glob/Grep`
  2. notebook.go:29 `os.ReadFile/os.WriteFile` → `ReadFile/WriteFile`
  3. image.go:85 `os.ReadFile` → `ReadFile`（路径翻译由 Translate 层兜底）
  4. **先读三个文件**确认现有签名与调用链，保持 Local 路径行为字节级不变（Local Executor 的这些方法语义需与原 os 调用一致——若 Local 缺少对应行为差异，以测试钉死）
- 验证：三文件各自单测：注入记录型 fake executor 断言调用；Local 回归测试不变

### 任务 20：宿主面工具禁用门控

- 文件路径：`internal/platform/executor/sandbox.go`（加类型标记）、`internal/tools/filehistory.go`、`internal/agent/worktree.go`
- 要做的：
  1. executor 包加：
     ```go
     // IsSandbox reports whether x executes in a remote sandbox.
     func IsSandbox(x Executor) bool { _, ok := x.(*Sandbox); return ok }
     // ErrSandboxUnsupported is returned by host-bound surfaces during sandbox runs.
     var ErrSandboxUnsupported = errors.New("sandbox: operation not supported during sandboxed runs")
     ```
     （`Translate` 加 `IsSandbox()` 委托方法，包装后仍可断言；cfbridge 等未来 provider 同样可加）
  2. filehistory undo/redo、worktree EnterWorktree：入口处 `if x, ok := runexec.RunExecutorFrom(ctx); ok && executor.IsSandbox(x) { return ErrSandboxUnsupported }`（工具层转为用户可见错误文案）
  3. **contextx git status 不做门控**（2026-10-19 计划审计后定案）：`SystemPromptFor` 按 workspace 缓存、签名无 ctx、build 期闭包，改造成本失衡；git status 只读宿主 workspace 且与上传沙箱的 workspace 同源，非新增暴露面。保留现状，在任务 28 部署文档记录此限制（设计 §10.3 m-6 已同步修订）
- 验证：单测：沙箱 ctx 下 filehistory/worktree 入口返回 ErrSandboxUnsupported；local ctx 行为不变

### 任务 21：hooks executor 化

- 文件路径：`internal/hooks/hooks.go`（:162-164 附近，`CommandRunner` 与 `ExecutorCommandRunner`）、`internal/agentbuild/build.go`（:124 Runner 构造）
- 要做的（方案已钉死：`CommandRunner` 签名自带 ctx 且 `ExecutorCommandRunner(x)` 已存在，走 executor 化分支）：
  1. hooks.go：`ExecutorCommandRunner(x)` 增强为经 ctx 解析——`DefaultCommandRunner`/`ExecutorCommandRunner` 的命令执行点改为：先 `runexec.RunExecutorFrom(ctx)`（有则用 run executor），否则回退构造时注入的 x（未注入时 `executor.NewLocal()` 保持现状）
  2. build.go:124：`hooks.NewRunner(..., hooks.WithCommandRunner(hooks.ExecutorCommandRunner(x)))`（**先读 hooks.NewRunner 的 option 形态**，若 WithCommandRunner 不存在则加）
- 验证：hooks 单测：ctx 注入 run executor → 命令经其执行；无注入 → build 时 executor；均不回归 local 默认

### 任务 22：后台 shell ctx 捕获

- 文件路径：`internal/tools/background.go`、`internal/tools/bash.go`
- 要做的：
  1. `BackgroundShells.Start` 签名（background.go:78）**加 ctx 参数**：`Start(ctx context.Context, command, workdir, baseWorkdir string)`；bash.go:38 调用点同步传 ctx
  2. `BackgroundShells` 的 `exec` 字段改 `execFor func(ctx) executor.Executor`（构造函数 `NewBackgroundShellsWithExecutor` 相应调整或新增 `WithExecFor` 变体，保持旧构造兼容）
  3. :103 `x.Start(context.Background(), ...)` 改 `execFor(ctx).Start(context.WithoutCancel(ctx), ...)`——`context.WithoutCancel` 保留 ctx values（含 run executor 解析所需值）、去除 cancel，Go 1.26.4 可用（go.mod 已核实）
- 验证：background 单测：注入 fake execFor，断言 Start 收到的 ctx 保留 run executor 值（values 断言）且不受父 cancel 影响

### 任务 23：MCP/LSP 门控

- 文件路径：`internal/mcp/tool.go`（MCP 工具的 Invoke 入口）、`internal/tools/lsp.go`（:342 `lspTool` 的 Invoke）
- 要做的（插入点已钉死——MCP 工具是 `agent.Config.ExtraTools`，**不在** tools.Registry）：
  1. mcp/tool.go：Tool 的 Invoke 入口前置 `if x, ok := runexec.RunExecutorFrom(ctx); ok && executor.IsSandbox(x) { return errors.New("MCP stdio servers are not available during sandboxed runs") }`（包 ErrSandboxUnsupported 包装）
  2. lsp.go lspTool：同款前置检查，文案 "LSP is not available during sandboxed runs"
  3. 检查用 `executor.IsSandbox`（含 Translate 委托）
- 验证：单测：沙箱 ctx 下 mcp 工具与 lspTool 返回该错误；local ctx 不拦截

### 任务 24：settings 键 + worker per-run 执行

- 文件路径：`internal/settings/settings.go` + `internal/settings/keys.go`、`internal/ee/worker/worker.go`、`cmd/woca-worker/main.go`
- 要做的：
  1. keys.go：注册 `sandbox-archive-max-bytes`（int64，默认 536870912，Apply 校验 >0）；settings.Settings 加对应字段 + payload 透出
  2. **worker 装配（审计 Maj-1 补）**：`worker.Config` 新增字段 `DB *wocadb.DB` 与 `SecretFor func(backendID string) string`；cmd/woca-worker/main.go:138 构造 worker.Config 处传入 `db` 与 `func(id string) string { return ap.Settings.GetSecret("sandbox-api-key:" + id) }`（secret 经 nacosrt apply 落 settings.Store 已核实，runtime.go:226-231）
  3. worker.go `execute()`（:134-197，**先读全函数**），snap 解析后：
     ```go
     row, err := nacosrt.EnabledSandbox(w.cfg.DB)
     if err != nil { /* finish run with explicit failure — DB 读失败也不静默回退本地 */ }
     if row != nil {
         exec, err := providers.New(providers.Config{Type: row.Type, Endpoint: row.Endpoint,
             APIKey: w.cfg.SecretFor(row.ID), Timeout: time.Duration(row.TimeoutMs) * time.Millisecond})
         if err != nil { /* fail run */ }
         tx := &executor.Translate{Inner: exec, HostRoot: snap.Workspace, SandboxRoot: "/workspace"}
         defer func() { _ = closeIfClosable(exec) }()
         if err := uploadWorkspace(ctx, tx, snap.Workspace, maxBytes); err != nil { /* fail run */ }
         defer func() { _ = downloadWorkspace(ctx, tx, snap.Workspace, maxBytes) }() // best-effort 回收
         ctx = runexec.WithRunExecutor(ctx, tx)
     }
     ```
  4. `uploadWorkspace`/`downloadWorkspace`：tar.gz（条目相对 workspace 根、保权限位与符号链接、排除 `.git`）；**双向** maxBytes 检查（上传前 stat、下载时 ContentLength/解包累计，超限报错中止）；下载解包回 `snap.Workspace`（覆盖式，宿主 `.git` 保留）
  5. **fail-run 语义**：任何一步失败 run 标记失败并附明确 error message，绝不静默回退本地执行（含 EnabledSandbox 的 DB 错误）
  6. 工具传给 executor 的路径形态为**宿主绝对路径**（dir.go:41 For(ctx) 产出，计划审计已核实），Translate 的翻译规则按绝对前缀处理
- 验证：`worker_sandbox_test.go`：fakesandbox 后端 → 执行 run → 断言命令在沙箱 fs 生效且产物回传宿主 workspace；下载超限 → run 失败；无后端 → 行为与现状一致（回归）；坏后端 → run 失败且不落本地

### 任务 25：能力 ack 心跳

- 文件路径：`internal/wocadb/db.go`（DDL 两处 :274/:459）、`internal/wocadb/configack.go`、`internal/ee/nacosrt/runtime.go`、`internal/ee/nacosrt/nacosrt.go` 或 Runtime 所在文件（加访问器）、`internal/settingswatch/settingswatch.go`、`cmd/woca-worker/main.go`
- 要做的：
  1. configack.go：`ConfigAck` 加 `Capabilities string`（逗号分隔，如 `"sandbox"`）；写函数加 capabilities 参数（**改造现有 SetConfigAck 签名**，同步修两处调用点）
  2. db.go：SQLite 与 PG 两处 DDL 加列（PG 用 `ALTER TABLE config_acks ADD COLUMN IF NOT EXISTS capabilities TEXT NOT NULL DEFAULT ''`，仿 configsvc.go:116-123 的 ALTER 模式）
  3. 两个 ack 写点（runtime.go:277-287、settingswatch.go:45-54）带 capabilities；worker 传 `"sandbox"` 当 EnabledSandbox != nil
  4. nacosrt Runtime 加只读访问器 `func (r *Runtime) LastGeneration() string`（ticker 需要，`pack` 字段私有）
  5. cmd/woca-worker/main.go：起 10s ticker goroutine 重写本 worker 的 ack 行（generation 取 `LastGeneration()`，capabilities 按 enabled 状态）——解决 15s live 窗口剪除
- 验证：`configack_test.go`：capabilities 读写；runtime/settingswatch 单测断言新参数；LastGeneration 单测；worker ticker 用短周期注入测试

### 任务 26：RunsOffHost 动态门控

- 文件路径：`internal/server/server.go`、`internal/servefront/serve.go`（:380 装配）、`cmd/woca-server/main.go`、`internal/ee/nacosrt/sandboxes.go`
- 要做的：
  1. `server.Config` 加 `SandboxEnabled func() bool`（核心包函数类型，合法；nil 恒 false）
  2. server.go:410-417 localGate：`/api/runs`（与 `/api/models`）的豁免条件从 `s.runsOffHost` 改为 `s.runsOffHost && s.sandboxEnabled()`——`gateway.RequireLocalTenant` 的 exempt 是构造期前缀表（gateway.go:192），改为：豁免列表仍按 runsOffHost 构造，但 `/api/runs` 的 POST handler 入口加前置检查 `if !s.sandboxEnabled() { 403 }`（最小侵入，不重构 gate 类型；`/api/models` 保持原豁免不变——门控只管 run 创建）
  3. **servefront 不 import internal/ee（check-editions 红线）**：servefront 的装配结构加 `SandboxEnabled func() bool` 透传字段；取值函数在 `cmd/woca-server/main.go`（企业入口，可 import nacosrt）注入 `func() bool { row, _ := nacosrt.EnabledSandbox(db); return row != nil }`（db 为 main 作用域现成的 wocadb；个人版 serve 路径传 nil）
  4. **派发校验插入点（钉死）**：`internal/server/runs.go` 的 `enqueueRun()`（:520-570）入口处查 `wocadb.ConfigAcks`——存在 live（15s 窗口）且 capabilities 含 `sandbox` 的 worker 才放行入队；否则 `POST /api/runs` 返回 503 + `"no sandbox-capable worker available"`（server 侧需要 DB 句柄——经 server.Config 既有 DB 面或新增字段，**读 server.Config 现有字段选定**）
- 验证：`server_gate_test.go` 矩阵：无后端→远端 403（现状回归）；有 enabled 后端→远端 201；有后端但无 sandbox-capable live worker→503

## M4 e2e 与收尾

### 任务 27：e2e compose 栈

- 文件路径：`e2e/sandboxlab/docker-compose.yml`（新建，仿 netlab 的独立 project name + 一次性卷模式，**先读 e2e/netlab/ 的组织方式**）、`e2e/sandboxlab/sandboxlab_test.go`（`//go:build sandboxlab` tag）
- 要做的：
  1. compose 组件：nacos + postgres + woca-server + woca-worker + fakesandbox 容器（fakesandbox 需可容器化：加 `cmd/woca-fakesandbox/main.go` 薄入口包裹 `fakesandbox.Handler()`，仅测试构建 tag 或独立小 main）
  2. 测试流程：configsvc API 登记后端（contract-http → fakesandbox）→ verify → 启用 → 远端租户 key `POST /api/runs` → 等 run 完成 → 断言 workspace 产物出现在宿主挂载卷
  3. 负路径：无后端时远端 403
- 验证：`go test -tags sandboxlab ./e2e/sandboxlab/`（本地需 docker；CI 可选）

### 任务 28：文档与收尾

- 文件路径：`deploy/selfhosted/README.md`（或相应部署文档）、`CONTRIBUTING.md`（如涉及）
- 要做的：
  1. 部署文档新增"沙箱后端"章节：type 表、KVM/容器要求、升级顺序（**worker 先于 configsvc**，pack v2 硬约束）、queue 派发前置、fake 用于验证
  2. `go test ./...` 全量 + `make check-editions` + `make ee-web` 最终确认
- 验证：全部绿；git status 无意外改动

## 执行批次（子代理派单）

| 批次 | 任务 | 依赖 |
|---|---|---|
| B1 | M1 任务 1-7 | 无 |
| B2 | M1.5 任务 8-9 | B1 |
| B3 | M2 任务 10-16 | B1；**任务 12（verify 依赖 providers.New）必须排在任务 8 完成之后**——即 B3 整体排在 B2 后串行 |
| B4 | M3 任务 17-26 | B1+B2+B3 |
| B5 | M4 任务 27-28 | B4 |

每批完成后主代理跑一次两段式评审（规格符合度 + 代码质量），Critical 修复后才进下一批。

## 计划审计记录（2026-10-19，子代理审计后修订）

初版计划经子代理审计：1 Critical（runexec 放 agent 包会循环依赖→已改 `internal/runexec` 独立包）、6 Major（worker 装配缺失→任务 24 已补 DB/SecretFor；MCP 门控定位错误→任务 23 已钉到 mcp/tool.go + lsp.go；contextx 门控不可落地→任务 20 定案保留现状+文档记录；background 签名→任务 22 已改签名；批次依赖缺口→上表已修；servefront 的 check-editions 冲突→任务 26 已钉企业入口注入）、8 Minor（双向大小限制/审计日志/LastGeneration 访问器/枚举拼写 cfbridge/EnabledSandbox 错误语义 fail-run/前端组件测试/hooks 方案钉死/行号漂移以符号为准——均已修入对应任务）。审计抽验确认：go 1.26.4（WithoutCancel 可用）、secret 落点 settings.Store（runtime.go:226-231）、工具路径形态为宿主绝对路径、RequireLocalTenant 为构造期前缀表。


## 沙箱体系（2026-10-19 落地）

- 契约 v2 是所有沙箱后端的统一抽象：`internal/platform/executor/CONTRACT.md`（15 端点、tar.gz 归档相对路径约定、/workspace 沙箱根）。fakesandbox 是契约参考实现与测试基建。
- provider 注册表 `internal/platform/executor/providers`：type ∈ {contract-http, cfbridge, e2b, daytona, modal, microsandbox, sidecar}；运行时已注册前两个，其余是登记+verify 报"not available"的占位。
- run executor 注入：`internal/runexec`（With/From，独立包防循环依赖）。沙箱 run 中工具经 ctx 解析 executor；filehistory undo/worktree/MCP stdio/LSP 被门控禁用；contextx git status 保留宿主读取（有意的限制，见部署文档）。
- 企业配置链：sandbox_backends 表 → configsvc CRUD/verify API → Pack v2 → worker 物化（全量清单语义）→ EnabledSandbox。全新部署（2026-10-19 确认无存量环境）：pack 严格 v2、无混版窗口、无升级顺序约束；nacos-init 初始 pack 必须 version:2。
- worker 执行链红线：任何失败 fail-run，绝不静默回退本地执行。归档双向限额 sandbox-archive-max-bytes（默认 512MB）；下载解包跳过 .git 与逃逸 symlink（终审加固）。

## Web Search 能力（2026-10-21 调研 + 实施）

- 调研结论 `docs/websearch-research.md`；实施结论 `docs/websearch-free-providers.md`；计划 `docs/superpowers/plans/2026-10-21-websearch-free-providers.md`。分支 `feat/websearch-free-providers`（from main），工作区 `.worktrees/websearch-free-providers`。

- **架构结论**：「MCP vs 内置 tool calling」是伪二选一。三层：① 模型服务端 hosted search（不能依赖，绑死厂商）② **客户端内置工具（主力，必需品）** ③ MCP（可替换后端）。收敛形态 = 工具壳恒定 + provider 注册表，MCP 只是 provider 之一。参考 `Twigpine/zero` 的 `searchBackend` 接口与 `anomalyco/opencode` 的 provider 注册表。
- **woca 现状**：`internal/tools/web.go`（WebFetch/WebSearch，DDG HTML 抓取，正则解析）+ `internal/searchapi/`（通用 JSON API client）+ `--search-api` 全局单点替换。**10 项差距 G1–G10 见文档 §1.2。**
- **两条 P0**：① 无法区分「被拦」与「搜不到」——DDG 返回 challenge 页时解析出 0 条并谎报 `No results found`，模型会断定信息不存在（社区 jcode #1765 同一失败模式）。② **DDG 2026 年按 IP 信誉拦截，数据中心 IP 确定性失败（HTTP 202 + JS challenge），换 UA 无效**——本机住宅 IP 跑通不构成验证，woca 生产形态大概率中招。
- **免 key 链**：`openrouter:web_search` → `searxng 自建` → `tavily-keyless`（`X-Tavily-Access-Mode: keyless`，唯一免 key 且有 SLA）→ `ddg-api` → `ddg-lite`。社区共识「2026 年没有 ToS-clean 的免 key SERP」。排除 Mojeek（robots 禁止）/ Bing（2025-08-11 退役）/ Google CSE（2027-01-01 全停）。
- **安全红线**：第三方抓取代理（Jina/Firecrawl/Browserbase）会绕过本地 SSRF 与域名白名单，进兜底链前必须先过白名单；不重试 JS challenge；`web_fetch` 必须能独立于 search 的 `content` 工作。
- **实测基线**：12 个常见文档页去标签后 text 中位数 54,949B，50% 超 50KB → **落盘是主路径不是异常路径**。woca 现截到 20,000B，多数文档页只剩约 36%。
- **woca 两个已知缺陷**：`maxWebChars=20000` 实为**字节**（Go `len(string)`），中文只拿约 6,667 字，且 `out[:20000]` 会切坏汉字；`ValidateURL` 只校验 hostname 不解析 IP，`SSRFSafeTransport` 库里有但 `fetchHTTP` 未用 → DNS rebinding 可绕过。
- **谱系坑（避免重复查错）**：`opencode` 是两个项目——`opencode-ai/opencode`（Go，2025-09 归档，只有 fetch 无 web_search）与 `anomalyco/opencode`（TS，provider 注册表）。Go 线延续为 `charmbracelet/crush`。`gh api repos/sst/opencode` 会 302 到 TS 版；`modelcontextprotocol` 字符串不含 `mcp` 子串，grep `mcp` 会漏掉官方 SDK。**不能用 tag 判断历史语言栈**（tag 指向 TS 重写之后的 commit）。
- **项目惯例**：go.mod 直接依赖仅 18 个，MCP（3,365 行）与 SSRF 均自研 → 加依赖需论证。建议只加 `goquery` + `html-to-markdown/v2`。MCP 客户端选型与 web search **正交**，2025→mcp-go、2026→官方 go-sdk 是时间线迁移，不是分歧。

### 已实施（2026-10-21，只做搜索链路，免费/免 key，不做付费）

- 新包 `internal/websearch/`：Provider 接口 + Router（冷却/故障转移/限流/域名过滤/统计）。工具壳不变，只换后端。
- 免费链：`ddg-html` → `ddg-lite`（默认开）→ `searxng`（需 `WOCA_SEARCH_SEARXNG`）→ `tavily` keyless（需 `WOCA_SEARCH_TAVILY=1`，请求形状已核对 opencode 源码：`POST /search` + `X-Tavily-Access-Mode: keyless`）。`--search-api` 走 generic provider 且**独占不回落**（回落 = 查询外发第三方）。
- **错误语义是核心**：0 条结果 = 成功（`No results found for this query.`）；429/503/**202**/200+挑战页 = `ErrRateLimited`（文案含 "do not retry or rephrase"）。故障转移**只认限流**，网络错/401/非503的5xx 直接返回。
- **P0 的准确表述**（读代码后修正调研）：非 200 时 `fetchHTTP` 本来就报错（只是裸 `HTTP 202` 没用），真正说谎的是 **200 + 挑战页** 那条。
- 反爬标记必须是**短语不是单词**（`anomaly-modal` 而非 `anomaly`），否则搜 "anomaly detection" 会把正常空结果误报成限流。`Retry-After` 采纳但**上限 10 分钟**。
- **goquery 不是新依赖**：v1.10.0 已在模块图（经 `langchain-golang/core/documentloaders`），go.sum 有 `h1:`、源码已缓存、`GOPROXY=off` 可跑。只是 indirect→direct，**版本未升，供应链零变化**。构建图 +4 包。
- **CI 有强制依赖基线** `ci/deps-personal.txt`（`make check-deps` diff 校验），改依赖必须 `make deps-baseline`。本次 +8 行含 main 既有漂移 2 行（`core/stores`、`internal/runexec`）。
- **测试坑**：`cmd/woca` 与 `internal/agentbuild` 的 `TestMcpAuthCmdFullFlow` / `TestNewMcpAuthRunnerFullFlow` 会**挂死**（`mcp.RunOAuth`，`mcp/auth.go:144`，等 OAuth 回调）。main 上同样挂 → 既有问题。回归时用 `-skip 'TestNewMcpAuthRunnerFullFlow|TestMcpAuthCmdFullFlow'`。
- 离线/网络受限环境下避免 `go list -m -versions`（sum.golang.org 会 EOF）；判定依赖是否在图里用 `go mod why` + `go.sum` + `GOMODCACHE` + `GOPROXY=off go run` 实测。
- 未做（下一分支）：WebFetch 改造（HTML→MD/rune 阈值/落盘）、SSRF（`ValidateURL` 不解析 IP）、settings 层键、`openrouter:web_search`（属 llm 层，`internal/` 无 openrouter 代码）、付费 provider。

## 测试套件约定（e2e/）

三个套件，都用 build tag 隔离，普通 `go test ./...` 不触发，都要配 Makefile 目标：

| 套件 | 关注点 | docker |
|---|---|---|
| `netlab`（tag netlab） | 网络层区域隔离：client → relay/edge → 隧道 → 内部 bridge；正反两面都测（跨区必须不可达） | 需要 |
| `sandboxlab`（tag sandboxlab） | 沙箱执行链：注册 backend → verify → enable → 创建 run → worker 执行 → 回传产物；先测 403 门禁 | 需要 |
| `complab`（tag complab，2026-10-22 加） | 进程内组件是否真能干活 + 组件间组合（搜索 → 抓取） | **不需要** |

`e2e/lab/` 是 Playwright 用的后端宿主（`package main`），不是共享 helper 库。
网络类集成测试的失败语义要分层：**没有外网 → skip（环境问题）；有外网但后端全被拦 → FAIL（真发现）**。

## SWE-bench 评测（2026-10-20 起，分支 feat/swebench-eval）

- 路线：只用官方 harness（SWE-bench/SWE-bench_Verified enriched 数据集 + swebench pip 包锁 5.0.2），不投第三方 benchmark 仓库。代码在 eval/swebench/（prepare/generate/grade + audit），依赖 uv 管理（pyproject + uv.lock，uv run --project eval/swebench）。
- 官方耦合铁律：镜像名只能直读数据集 image 列（含 1776 版本段，不可从 instance_id 推导）；5.x CLI 无 --namespace/--arch；--instance_ids 是 nargs="+"。
- token 效率基线（woca 0.3.0，--db none，o200k_base）：system 1,982 + 工具 4,080（49 个）= 6,062；对比 OpenHuman 4.6k（20 工具）、其他 6 家中位 7.7k。
- 冒烟集 pinned：eval/swebench/instances.txt，10 实例 8 仓库。集成链路 prepare/audit/gold 预热已实跑通过（gold 10/10 resolved，OrbStack 仿真 ~3 分钟）；ARM 镜像拉取必须显式 --platform linux/amd64（amd64-only manifest，默认 pull 404），预拉带 2400s×3 重试（Docker Hub 未登录限速可低至 <2MB/s）。仅剩 generate/grade 待 API key。

## 仓库约定：不要回退自动化进程做的现代化改写（用户 2026-10-23 明确要求）

- 这个仓库里有一个自动化进程（提交身份也是 `yuhaochen <cyh278889789@gmail.com>`，
  与手动提交无法从 git 元数据区分）会持续做「现代 Go 改写」并自行提交、推送：
  `strings.Index`→`strings.Cut`、`strings.Split`→`strings.SplitSeq`、
  `if x < 1 { x = 1 }`→`max(x, 1)`、删掉它认为没用的 import 等。
  go.mod 是 `go 1.26.4`，这些写法都合法，仓库里本来也在用。
- **用户明确要求保留这些改写**。之前我误以为是破坏而 `git checkout -- .` 回退过
  一次，那份改动从未提交，因此**已从 git 中永久丢失，无法恢复**。
- **工具层面无法复现**：已用实验验证 `gofmt`（Go 1.27.2）绝不做这类改写；
  `gopls check`（v0.23.0）对样本文件也报不出现代化诊断；`go fix` 默认只跑传统
  fixer。所以不能再靠工具重新生成一遍。
- **正确做法**：不回退它的改动。它偶尔会把构建改坏（曾出现
  `undefined: errors`——删掉了仍在使用的 import），遇到时**只补回缺失的 import
  或修具体编译错误**，不要整体 revert。
- 识别方法：一堆没碰过的文件在同一秒被改，且是现代化改写而非空格调整。

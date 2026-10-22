# woca Web Search 能力调研报告

> 日期：2026-10-21
> 范围：woca 现有 web 能力盘点 + 市面上 harness agent 的 web search 实现路线（MCP vs 内置 tool calling）
> 方法：官方仓库源码/官方文档一手核实 + 本地实测

---

## 0. 结论先行

**「MCP 还是内置 tool calling」是个伪二选一。** 市面主流 harness 的真实结构是三层：

```
第 1 层  模型服务端 server-side search   ← Claude API / OpenAI Responses / Gemini grounding
第 2 层  客户端内置工具（harness 自己实现）← 事实上的主力
第 3 层  MCP 外部服务器                  ← 可替换 provider，不是替代品
```

- **第 1 层不能依赖**：它绑定模型厂商，Bedrock 没有、Vertex 部分没有、换模型就失效。
- **第 2 层是必需品**：所有严肃 harness 都自己实现了 `WebSearch`/`WebFetch`，因为搜索是 agent 的**基础生存能力**，不能取决于用户有没有配 MCP。
- **第 3 层是可替换后端**：opencode 的做法最成熟——内置工具壳 + **provider 注册表**，MCP 只是 provider 之一。

**woca 该走的路**：保留并强化第 2 层，把它重构成 **provider 注册表 + 一条免 key 降级链**，
MCP/HTTP API 作为可插拔 provider 挂进来。而不是「要么内置、要么 MCP」。

**一条必须提前知道的坏消息**（详见 §2.8）：woca 现在默认的 DuckDuckGo 抓取，
**在你本机是好的，在企业 worker / CI / 容器里大概率是坏的**——2026 年 DDG 按 IP 信誉拦截，
数据中心 IP 会稳定拿到 challenge 页。而 woca 目前**无法区分「被拦」和「搜不到」**，
模型会据此断定「信息不存在」。这是本次调研里最需要处理的一条。

---

## 1. woca 现状盘点

### 1.1 已有的东西（比想象中多）

| 位置 | 内容 |
|---|---|
| `internal/tools/web.go` | `WebFetch`（GET + 正则去标签 + 20k 截断，硬编码 SSRF 校验）、`WebSearch`（DuckDuckGo HTML 抓取）、`WebSearchToolWithBackend(backend)` 扩展点 |
| `internal/searchapi/` | 通用 JSON 搜索 API 客户端：`{q}` 模板 / `?q=` 追加、Bearer、`results`/`items` 双字段兼容、`Format()` |
| `internal/agentbuild/build.go:178-187` | `--search-api` 命中时，用 `WebSearchToolWithBackend` **整体替换** 同名 `WebSearch` 工具 |
| `internal/tools/registry.go` | `CatWeb` 权限类别；`CatWeb` 归为只读且默认免确认 |
| `internal/mcp/` | 自研 MCP 客户端，3,365 行，支持 stdio + StreamableHTTP + OAuth token |
| `internal/agentbuild/layers.go` | MCP spec 四层合并（flag > workspace > user > managed），skills 同样有 managed 层 |
| `woca-plugins/golang/browser` | pinchtab CDP 浏览器插件，4 个工具（导航/正文/快照/动作），自带 SSRF 防护 |

**实测（2026-10-21，本机）**：`html.duckduckgo.com/html/` 返回 200，正则解析出 10 条链接 + 10 条摘要，
`uddg` 跳转解包正常；`lite.duckduckgo.com/lite/` 同样 200。**基线在本机是能用的。**

> ⚠️ **但这个结果不能外推到生产。** 本机是住宅 IP。2026 年 DuckDuckGo 按 **IP 信誉**拦截，
> 数据中心 IP（企业 worker / CI / 容器）会稳定拿到 challenge 页。见 §2.8。

### 1.2 真实差距（按严重度）

| # | 差距 | 影响 |
|---|---|---|
| G1 | **无 provider 抽象**。`--search-api` 是全局单点替换，无回退、无冷却、无失败切换 | 后端挂了 = 能力归零 |
| G2 | **无法区分「被拦」和「搜不到」**。DDG 返回 challenge 页（HTTP 200 或 202）时，woca 照样解析出 0 条 → 向模型谎报 `No results found` | **最严重的一条**。模型会断定「这个信息不存在」，进而编造或反复改写 query（社区 issue jcode #1765 记录过同样的失败模式） |
| G3 | **UA 固定** `woca/0.1` | 指纹明显。但注意：**换 UA 不解决根本问题**——DDG 判定在 IP 信誉层（§2.8），UA 池只是基础礼貌 |
| G4 | **无请求节流**。agent 循环可连续打出多次搜索 | 触发 DDG 限流的概率高 |
| G5 | **正则解析**（`web.go:20-23`） | DOM 结构一变就静默失效；对比 Crush 用 `golang.org/x/net/html` 做真 DOM 遍历 |
| G6 | **WebFetch 只做去标签纯文本**，无 Markdown、无正文抽取、无大页落盘；截断上限 `maxWebChars = 20000` | 文档页抓取质量差；实测常见文档页 text 中位数 54,949B，**多数页被裁到只剩约 36%**（§4.7） |
| G6a | **截断常量名不副实且会切坏 UTF-8**：`web.go:17` `maxWebChars = 20000` 实际是**字节**（`web.go:72` 用 `len(out)`，Go 对 string 返回字节数）；`out[:20000]` 按字节切片，**可能把汉字从中间切两半**，产出非法 UTF-8 | 中文页（3 字节/字）实际只拿到约 6,667 字；尾部可能吐出半个字 |
| G7 | **无 DNS 重绑定防护**。`ValidateURL` 只校验 hostname，不解析 IP；`fetchHTTP` 用裸 `http.Client`（`security.SSRFSafeTransport` 存在但未被使用） | SSRF 可被 DNS rebinding 绕过 |
| G8 | **无 managed/企业配置层**。MCP 与 skills 都有 managed 层，web search 只有 CLI flag | 企业版无法统一配置搜索后端 |
| G9 | **Web 工具不经 executor**，恒定跑在宿主进程 | 沙箱 run 中 web 访问不受沙箱网络约束（有意为之，但需显式记录） |
| G10 | 结果无元数据（发布时间、域名） | 时效性问题无法判断 |

---

## 2. 市面 harness 的实测路线

### 2.1 最关键的参考：opencode —— provider 注册表

仓库已从 `sst/opencode` 迁到 **`anomalyco/opencode`**（TypeScript，dev 分支）。

**核心结构**（一手核实）：

```
packages/core/src/websearch.ts              # WebSearch 服务：provider 注册表 + 调度内核
packages/core/src/tool/plugin/websearch.ts  # 工具壳：name="websearch"，input={query}
packages/core/src/plugin/websearch/
    index.ts      # 注册表：[exa, firecrawl, parallel, tavily, tinyfish]
    mcp.ts        # 通用 MCP JSON-RPC 桥（供各 provider 复用）
    exa.ts        # https://mcp.exa.ai/mcp      tool=web_search_exa
    parallel.ts   # https://search.parallel.ai/mcp  tool=web_search
packages/core/src/config/plugin/websearch.ts # 配置：{ provider: "random" | <ID> } | false
```

**可迁移的四个设计**：

1. **MCP 桥是通用件，不是一次性接线**（`mcp.ts`）
   POST `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":...,"arguments":...}}`，
   `Accept: application/json, text/event-stream`，响应兼容**直接 JSON 与 SSE `data:` 行**，
   25s 超时，256KB 响应上限。

2. **provider 冷却 + 429 故障转移**（`websearch.ts` 的 `query` 主循环）
   - `cooldowns: Map<providerID, {until, error}>`
   - 只有 **HTTP 429** 才触发换 provider，其他错误直接失败（不重试）
   - 冷却时长读 `Retry-After` 头
   - 所有 provider 都在冷却 → 返回最后一个 error

3. **会话亲和（session affinity）**
   `preferred: Map<sessionID, {provider}>`——一个 session 一旦选中 provider 就粘住，
   避免同一会话里搜索结果来源跳动。

4. **凭据链**
   provider 注册 `{type:"key"}`（存储的 key）或 `{type:"env", names:["EXA_API_KEY"]}`（环境变量）。
   Exa 的 key 走 query param `?exaApiKey=`。

**这一条直接回答了用户的问题**：opencode 的 `websearch` 是**内置 tool calling**（工具定义在自己手里），
但它的**后端是 Exa / Parallel 的 MCP 端点**。MCP 在这里是传输层与 provider 来源，不是二选一的架构选择。

**补充事实（一手核实）**：

- **「opencode」这个名字对应两个不同的项目**，别搞混：

  | 仓库 | 语言 | 状态 | web 能力 |
  |---|---|---|---|
  | `opencode-ai/opencode` | **Go** | **已归档**（2025-03-16 建，HEAD `73ee4932`，末次 push 2025-09-18） | 只有 `fetch`（5MB，需审批，UA `opencode/1.0`）+ `mark3labs/mcp-go v0.17.0`。**没有 web_search** |
  | `sst/opencode` → `anomalyco/opencode` | **TypeScript/Bun** | 活跃（2025-04-30 建） | 内置 `websearch` + provider 注册表；MCP 客户端用 effect 生态**自研** |

  Go 版 README 首行原文：
  > "Archived: Project has Moved ... The project has continued under the name **Crush**,
  > developed by **the original author** and the Charm team."

  **所以真实谱系是 `opencode(Go)` → 归档 → `crush(Go)`，同一作者、同一条 Go 代码线的延续。**
  搜索能力是在 Crush 这一代才补上的。「Go 版 opencode 的搜索实现」这个前提本身不成立。

  > ⚠️ **别走错仓库**：`gh api repos/sst/opencode` 会 302 到 `anomalyco/opencode`（TS），
  > 按 `v0.x` tag 取到的全是 TS。**要看 Go 实现必须去 `opencode-ai/opencode`**（已归档，HEAD 稳定）。
  > 本次调研里两个人先后在这一步上取错过。
- **5 个 provider 里 4 个走 MCP、1 个走 REST**：Exa(`mcp.exa.ai/mcp`)、Firecrawl(`mcp.firecrawl.dev/v2/mcp`)、
  Parallel(`search.parallel.ai/mcp`)、TinyFish(`agent.tinyfish.ai/mcp`)；Tavily 直连 `api.tavily.com/search`。
- **Tavily 支持 keyless 模式**：无 key 时发 `X-Tavily-Access-Mode: keyless` 仍可用。
  **这是唯一一个「免 key 且有 SLA」的后端**，对 woca 的「默认免配置」诉求很关键。
- **opencode 没有用通用 MCP client**，而是自写约 60 行的 `tools/call` 直发器：不握手、不建会话、不做工具发现。
  省掉全部握手开销，也避免把 MCP server 变成用户可见工具。
- **webfetch 的 CF 对抗**：`403` + `cf-mitigated: challenge` → 降级用裸 UA `"opencode"` 重试一次。

### 2.2 Go 系参考：charmbracelet/crush

`internal/agent/tools/`，**全部内置，零依赖后端，无 MCP 参与搜索**：

| 文件 | 作用 |
|---|---|
| `search.go:70` | **DDG 抓取引擎**（无工具壳）：`searchDuckDuckGo()`，端点 `ddgLiteEndpoint = "https://lite.duckduckgo.com/lite/?q="`（`:68`） |
| `web_search.go:53` | 给 sub-agent 用的**工具壳**，调 `searchDuckDuckGo`，`max_results` 默认 10、上限 20 |
| `web_fetch.go` | 给 sub-agent 用的抓页壳，超过 `LargeContentThreshold`(50KB) **落临时 .md 文件** |
| `fetch.go` | **主 agent 的老 fetch**（100KB，带 `permission.Request` 审批），与上面那套**并存不替代** |
| `fetch_helpers.go` | HTML→Markdown、去噪标签、JSON 格式化 |

**所以 Crush 的真实结构是「一个引擎 + 两套壳 + 一个遗留工具」**，不是并行实现。

**并且：Crush 的 DuckDuckGo 是硬编码的、零配置的、不可替换的。**

`internal/config/*.go` 全量搜 `websearch|web_search|SearchEngine|searchEngine` → **零命中**。
唯一能关掉它的是通用键 `options.disabled_tools`（`config.go:471`）。

**这正是 Crush 与 opencode(TS) 的路线分水岭**：

| | Crush (Go) | opencode (TS) |
|---|---|---|
| 后端 | **硬编码 DDG**，不可换 | provider 注册表，可插拔 |
| 配置 | 无（零配置） | `websearch: {provider: ...}` |
| 失败处理 | 换后端做不到，只能告诉模型等 | 冷却 + 换 provider |

**两段式是刻意的省 token 设计**（`web_search.md.tpl` 首行）：
> "Search the web via DuckDuckGo; returns titles, URLs, and snippets.
> **Follow up with web_fetch to get full page content.**"

即 `web_search` **只回标题/URL/摘要**，要正文必须再调 `web_fetch`。对 woca 的 token 效率目标有直接参考价值。

**三级 web 访问阶梯**（这是 Crush 最值得抄的结构）：

| 层级 | 工具 | 契约 | 成本 |
|---|---|---|---|
| L1 取原文 | `fetch` | URL → 文本/Markdown/HTML | 低 |
| L2 发现 | `web_search` | query → 标题/URL/摘要 | 低 |
| L3 代理式抽取 | `agentic_fetch` | prompt（+可选 URL）→ **派一个只带 web 工具的 subagent** 循环 search+fetch 后作答 | 高 |

`agentic_fetch` 的工具描述原文（`internal/agent/templates/agentic_fetch.md`）：
> Fetch a URL or search the web using an AI sub-agent that can extract, summarize, and answer questions.
> Slower and costlier than fetch; use fetch for raw content or API responses.

**关键点**：L3 是对「Claude Code 的 WebFetch lossy by design」的**客户端等价实现**——
不需要模型厂商的 server-side 能力，用 subagent 就能做出「给模型它要的答案而不是整页」。
woca 已有 `Task`/subagent 机制，落地 L3 的边际成本很低。

**一个容易看漏但很重要的设计——按 agent 类型分发工具 + 权限分层**：

| 工具 | 主 agent（coder） | sub-agent | 是否需审批 |
|---|---|---|---|
| `fetch` | ✅ 无条件挂载 | — | **需要**（`permission.Request`） |
| `sourcegraph` | ✅ 无条件挂载 | — | 需要 |
| `agentic_fetch` | 需 `AllowedTools` 显式开启 | — | 需要 |
| `web_search` / `web_fetch` | ❌ | ✅ | **不需要**（注释明说 "no permissions needed"） |

三个可迁移点：
1. **权限跟着挂载位置走**：跑在主 agent 的高风险动作要审批，跑在 sub-agent 的只读动作不要。
2. **sub-agent 工具集是精选的**：`agentic_fetch` 的 subagent 只拿
   `web_fetch / web_search / glob / grep / sourcegraph / view`，用**小模型**跑，**并且不加载 hooks**
   （`coordinator.go:428`，注释 "subagent doesn't get custom hooks"）。
   这是 token 与安全的双重收敛——对 woca 的 `Task` 机制直接适用。
3. **MCP 工具命名与配置键**：工具名 `mcp_<server>_<tool>`；配置 `MCPStdio / MCPSse / MCPHttp ([]string)` + `MCP map[string]MCPServer`。

**可直接抄的四点**：

- **anomaly 检测**（`search.go:56-64, 90-110`）
  HTTP 202 = 挑战页；正文含 `anomaly-modal` / `/anomaly.js` / `Unfortunately, bots use DuckDuckGo too`
  → 返回 `errSearchRateLimited`，且**错误文案直接命令模型**：
  > "Do not retry or rephrase; wait a few minutes or fetch known URLs directly"

  这是**提示词层面的防护**，比单纯报错有效得多。

- **随机化请求头**（`setRandomizedHeaders`，11 个 UA + 5 个 Accept-Language 池，加 `Sec-Fetch-*`、`DNT` 抛硬币）
- **全局节流**（`maybeDelaySearch`，两次搜索间隔随机 500–2000ms，全局 mutex）
- **真 DOM 解析**（`golang.org/x/net/html` 遍历，按 class `result-link` / `result-snippet` 取值）

### 2.3 Twigpine/zero —— Go 世界里最贴合 woca 形态的参考

`Twigpine/zero`（Go，1.7k stars，**今天仍在活跃提交**）。**go.mod 里 0 个 MCP 依赖**——
web 能力 100% 内置自研，**搜索这条路径上完全没有 MCP 参与**。

顺带纠正一个容易搞混的点——**MCP 客户端选型与 web search 实现完全正交**，别混在一起决策。
Go 系三个 agent 在 MCP 上走了三条不同的路：

| 项目 | 时间 | MCP 客户端 | 版本 |
|---|---|---|---|
| opencode（Go 时代，归档） | 2025-04 | 第三方 `mark3labs/mcp-go` | v0.17.0（`go.mod:23`，`internal/llm/agent/mcp-tools.go`） |
| Crush | 2026-08 | 官方 `modelcontextprotocol/go-sdk` | `v1.7.1-0.20260825092617-...`（**pseudo-version，跟 main 分支**，正式线是 v1.7.0 / v1.8.0） |
| zero | — | **完全自研**（`internal/mcp/` 全套含 OAuth，go.mod 零 MCP 依赖） | — |
| woca（现状） | — | 自研（`internal/mcp/`，3,365 行） | — |

**这不是「选型分歧」，是「时间线迁移」**（按时间排就清楚了）：

- **2025 年**：官方 go-sdk 还很早期（v1.0.0 才对应 spec 2025-06-18），当时开工的项目选 `mcp-go` 是**当时的合理选择**。
- **2026 年**：官方 SDK 追平并超出——v1.7.0+ 支持 spec 2026-07-28，向后兼容到 2024-11-05，
  有 `auth` / `oauthex` / `jsonrpc` 分包，带 OpenSSF Scorecard。**新项目集体切到官方 SDK。**
- `mark3labs/mcp-go`（9,178★ > go-sdk 5,252★）星标仍更高，主要是历史存量与服务端场景的惯性。

> 排查经验一条：`modelcontextprotocol` 这个字符串里**不含 `mcp` 子串**。
> 只 grep `mcp` 会漏掉官方 SDK——本次调研里两个人各自栽过一次。

`internal/tools/web_search.go`（一手核实）：

```go
// searchBackend discovers URLs for a query. It is an interface so any hosted
// search API (or a fake, in tests) can be dropped in without touching the tool.
// nil means no backend is configured.
type searchBackend interface {
    Search(ctx context.Context, query string, limit int) ([]searchResult, error)
}

type searchResult struct {
    Title, URL, Snippet string
    Score float64   // 可选 [0,1] 相关性信号，0 视为"未提供"
}

func NewWebSearchTool() Tool { return newWebSearchToolWithBackend(defaultSearchBackend()) }
func newWebSearchToolWithBackend(backend searchBackend) Tool { ... }  // 测试注入
```

常量：`limit` 默认 5 / 上限 10，超时 10s，响应体上限 256KB，重定向上限 5。
依赖 `internal/redaction`（SSRF）与 `internal/sandbox`（沙箱感知）。

**四个可迁移点**：

1. **`nil` backend 是一等状态**——「没配后端」和「搜不到」是两回事，工具据此给不同反馈。
2. **构造函数双入口**：生产走 `defaultSearchBackend()`（读 env），测试走注入。**这正是 TDD 需要的接缝。**
3. **`searchResult` 带 `Score`**，让 provider 可以回传相关性，渲染层可选用。
4. **工具描述里显式写清职责边界**：
   > "Search the web for a query and return ranked results (title, URL, snippet).
   > **Complements web_fetch, which retrieves a single known URL.**"

   一句话把「发现」与「取回」分开，减少模型误用。

后端配置（env 驱动）：`ZERO_WEB_SEARCH_BACKEND=duckduckgo|searxng|brave`，
`ZERO_WEB_SEARCH_SEARXNG_URL`、`ZERO_WEB_SEARCH_BRAVE_KEY`、`ZERO_SEARCH_ALLOWED_DOMAINS`。

### 2.4 其他 Go 系：trpc-agent-go / cagent / Plandex

**trpc-agent-go（腾讯 trpc 生态，`trpc-go/trpc-agent-go`）—— Go 世界里抽象最干净的一个**

```
tool/duckduckgo/      自建 DDG 客户端：3 个后端 api / html / lite，全部免 key
tool/google/search/   Google CSE：engine_id + api_key 必填
tool/youcom/          You.com：需 key
tool/webfetch/        区分 httpfetch（客户端抓）与 geminifetch/claudefetch（厂商托管）
tool/webfetch/llmops/ 抓取后用 LLM 做摘要
```

要点：
- **切后端只换 `clientOption`，工具壳不动**——正是我们要的 provider 抽象形态。
- `api` 后端走 DDG 官方 API（`api.duckduckgo.com`），比抓 HTML 稳。
- **明确区分「客户端抓」与「厂商托管抓」**，和本文第 0 节的三层划分完全对齐。

**docker/cagent（Go，Docker 官方）—— 合规侧做得最重**

`pkg/tools/builtin/fetch.go`：
robots.txt / Host 与 IP 双重白名单 / TOFU（第一次访问要人工确认）/ 响应体 `io.LimitReader` 1MB /
**剥离 Authorization、Cookie、X-API-Key、X-Auth-Token 等凭据头** / `Proxy-Authorization` 全覆盖。
**没有内置 web 搜索**，要搜索靠 MCP 扩展（但 cagent 支持 MCP gateway，Docker 把 MCP 当一等公民）。

**Plandex（Go，另一个 terminal agent）—— 完全没有 web 能力**

`plandex-ai/plandex`（Go，1 万+ stars，2025-09 迁到 `plandex-ai`，现在 37% PLpgSQL + 63% Go）
**完全没有 web 工具**——内置只有 `load_plan_files` / `build` / `terminal` / `code` 等构建类工具。

它联网的**唯一**途径是选 **Perplexity sonar-reasoning-pro** 这类「online models」——
即把联网能力**外包给模型厂商**（第 1 层）。另外 `plandex load <url>` 会用 chromedp 无头浏览器抓页面，
但那是**开发用的隐藏命令**（`dev` 命令组），不是 agent 工具。

**这是最好的反面教材**：一个 terminal agent 不内置搜索，用户就只能手动贴文档，或被迫绑死某个模型厂商。
**恰恰证明第 0 节的论点——第 2 层必须自己有。**

### 2.5 Python 系 harness

| 项目 | 搜索实现 | 位置 | 后端 / key |
|---|---|---|---|
| **Aider** | `WebScrape` 内置工具（默认**禁用**） | `aider/website.py` | `playwright` + `trafilatura` → Markdown；`--enable-web-scrapes` 开启 |
| **OpenHands** | 三类并存：内置 `web_read`/`browse`、`BrowserAgent`（Playwright）、MCP | `openhands/microagent/`、`openhands/agenthub/browser_agent/`、`openhands/mcp/` | Playwright 自托管浏览器；MCP 可选 |
| **SWE-agent** | 「web 能力不是内置工具」，靠**上游获取 + 环境自举** | `sweagent/run/run_single.py`、`sweagent/environment/` | 容器直连外网或 `--env-setup` 装依赖；无 WebSearch 工具 |
| **Devika** | 内置工具化 + 读服务 | `src/agents/reader_agent.py` | `search_engine` 配置 + `trafilatura` |
| **OpenManus** | 工具化 | `app/tool/search/` | Google / Bing / DuckDuckGo / Baidu，缺 key 时自动降级到 DDG |
| **deep-research（dzhng）** | 纯 prompt-driven | `src/prompt.ts` + `firecrawl` | Firecrawl |

**Python 系的关键共识**：

1. **scrape + 抽取库是标配**：`trafilatura`（正文抽取准确率最高）、`firecrawl`（付费托管抽取 + 搜索）、`playwright`（JS 渲染 + 交互）。
2. **SWE-agent 的路线值得注意**：它根本不做 WebSearch 工具，而是把「联网」下沉到环境层（容器能上网 + `--env-setup`）。
   这是**依赖简化**的极致，但也意味着**agent 无法主动检索**——只适合 benchmark 场景，不适合通用 harness。
3. **OpenManus 的降级链值得抄**：多个搜索引擎按 key 可用性自动降级，最后兜底 DDG。这就是 provider 注册表的雏形。
4. **OpenHands 的三层并存**印证了第 0 节的分层：内置 + 浏览器 agent + MCP，各管一段。

### 2.6 闭源/商业 harness

| 产品 | 层级 | 后端 | 可换 provider | 关键限制 |
|---|---|---|---|---|
| **Claude Code / Agent SDK** | 第 1 层（Anthropic server-side hosted） | Anthropic 自有索引 | 否（换 provider 只能加 MCP） | $10/千次；**每 session 200 次（主 agent + 全部 subagent 共享）**；Bedrock 无；WebFetch 经小模型抽取（lossy），100k 字符 + 15 分钟缓存 |
| **Codex CLI** | 第 1 层（OpenAI hosted tool） | OpenAI 索引 | 否 | 默认 `cached`（OpenAI 预建索引，不抓活页）；`--search` 或 `web_search="live"` 切实时；沙箱默认 `network_access=false` |
| **Gemini CLI** | 第 1 层（grounding） | Google 搜索 | 否 | `google_web_search` 由模型决定何时调；`web_fetch` 走 API `urlContext`，失败才回退本地抓取 |
| **Cursor** | 第 2 层（客户端） | 未公开（早期用 Tavily） | 部分（可加 MCP） | 官方不披露后端 |
| **Windsurf** | 第 2 层 | 未公开 | 部分 | **服务端可移除工具**：企业管理员在 server-side 关闭内置 web 搜索 → 即使 `allowed_tools` 有也不可用；读页走本机 |
| **Cline** | 第 2 层 + 插件 | Exa（`web_search` 插件） | 是（插件化） | `fetch_web_content`（读页，本机无头浏览器）内置；`web_search`（检索）是插件 |
| **Kilo Code** | 第 2 层 | **Exa / Parallel / kilo-exa** | 是（`KILO_WEBSEARCH_PROVIDER` env） | 内置但**后端可换**——最接近我们目标形态的商业产品 |
| **Roo Code** | **纯 MCP** | 无内置 | 是 | **没有内置 web 工具**，全部靠 MCP / 自定义 mode |
| **GitHub Copilot** | 第 2 层 | `#fetch` = **本机无头浏览器**（Playwright/Chrome DevTools） | 部分 | 默认禁用 JS；**云端防火墙管不到 MCP server** |
| **Amp** | 第 2 层 | Parallel Search + Extract API | 否 | 挂着 Fetch API 也为 agent 优化 |
| **Factory Droid** | 第 2 层 | `WebSearch` + `FetchUrl` | 是（MCP 可替换） | 权限系统可拦 |
| **OpenRouter 系 harness** | 第 1 层（厂商 hosted） | `openrouter:web_search` server tool | 否 | **零配置即可用**：走 vendor 层，跟沙箱网络无关。若 woca 接 OpenRouter  provider，这是一条白送的路径 |

**三条规律**：

1. **绑死第 1 层的都被厂商锁死**：Claude 换到 Bedrock 就没有 `WebSearch`；
   Codex 的搜索走 OpenAI 索引，跟沙箱网络策略无关。
2. **自己做第 2 层的都走向「内置 + 可换后端」**：Kilo Code 用 env 切 Exa/Parallel，
   Factory Droid 允许 MCP 替换。**和 opencode 的 provider 注册表是同一个收敛方向。**
3. **Roo Code 是唯一的纯 MCP 派**——也是唯一一个开箱不能联网的。这从反面证明了第 0 节的论点。

---

## 2.7 治理：服务端 web 工具 ≠ 沙箱网络（对 woca 沙箱体系直接相关）

**GitHub 的措辞最清晰**（Agentic Workflow Firewall 文档）：

> "`hosted-web` is separate from and independent of `network.allowed`. Firewalls that set `network.allowed` but not `hosted-web` are closed by default."
> "The hosted web tools run on GitHub's infrastructure, not in your workflow container."

即：**Agent HQ 托管的 WebFetch/WebSearch 与容器出网是两套独立策略**，不配 `hosted-web` 就默认全关。

**Codex 的官方文档也确认了这一点**：

> "Web search is a hosted tool. It **does not** use the permission profile's network proxy or domain allowlist,
> and it **can remain available when command network access is disabled**."

**对 woca 的直接含义**：woca 现有沙箱（`internal/platform/executor`）管的是 shell/文件的出网（G9），
但**内置 web 工具必须有一个独立于沙箱 `network` 的策略维度**。
否则企业版里「禁沙箱出网」不会禁掉 WebSearch——而用户会以为禁掉了，这是一个预期错配。

---

## 2.8 DuckDuckGo 在 2026 年的真实可用性（**这条改变了默认值判断**）

**先说结论：DDG 抓取不能再当「默认后端」，只能当降级项。**

### 一手实测（本机，2026-10-21）

```
https://html.duckduckgo.com/html/?q=golang+mcp+server   → HTTP 200，解析出 10 条 result__a + 10 条摘要
https://lite.duckduckgo.com/lite/?q=golang+mcp+server   → HTTP 200，解析出 10 条 result-link
```

**但这不代表生产可用。** 实测成功是因为本机是**住宅/家庭 IP**。

### 2026 年 DDG 反爬的真实机制

| 现象 | 机制 |
|---|---|
| HTTP **202** | challenge 中间页，不是结果页 |
| HTTP 200 + 空结果 | DDG 在正文里放 bot-check 页，正则照样解析出 0 条 |
| HTTP 429 | 限流；住宅 IP 几十次后才触发，**数据中心 IP 更快** |
| 失败与 UA 无关 | 判定主要依据**IP 信誉**，不是指纹 |

**多方交叉验证**（sst-opensource/adr-gallery ADR-0043《Web Search Integration》、niyogi、huggingface 等）：
- 「对**数据中心/VPS IP 是确定性失败**——HTTP 202 + JS 挑战」
- 「住宅 IP 更宽容，几十次后才限流」
- 「换 UA / 加 Sec-Fetch 头**都不解决**，因为判定在 IP 层」
- 社区共识原文：**"No ToS-clean keyless SERP API exists in 2026"**

### 这对 woca 意味着什么

woca 的部署形态（企业版 worker、CI、容器化沙箱、云端 runner）大概率落在**数据中心 IP**。
也就是：**本机跑得好好的 WebSearch，到了生产环境会静默返回 0 条。**

这正是 G2 最危险的地方——见下条。

### 附带的一条工程教训（值得单独记）

社区 issue（jcode #1765）记录了一个真实的失败模式：

> 搜索被限流时返回「No results found」，模型**据此断定「这个信息不存在」**，
> 然后开始编造，或者花十轮反复改写 query。

**所以「被拦」和「搜不到」必须给模型两种不同的反馈。** 这一条和 Crush 的
`errSearchRateLimited` 文案（"Do not retry or phrase; wait a few minutes or fetch known URLs directly"）
是同一个结论的两种表达。**woca 现在缺的就是这个区分。**

---

## 3. 后端选型（免 key 默认 + 付费可插拔）

价格数据核实于 2026-10，来自各厂商 pricing 页。**免 key 一栏请结合 §2.8 一起看。**

| 后端 | 免 key | 价格/千次 | 2026 现状 | 判定 |
|---|---|---|---|---|
| **OpenRouter `openrouter:web_search`** | 是 | $0（走 vendor） | 接 OR provider 时零配置可用 | **白送，优先接** |
| **SearXNG 自建**（`format=json`） | 是 | 自建成本 | 唯一的 ToS-clean 免 key 路径 | **推荐默认** |
| **Tavily keyless** | 是（`X-Tavily-Access-Mode: keyless`） | — | 社区实测可用 | **推荐次选**，免 key 且有 SLA |
| DuckDuckGo **Instant Answer API**（`api.duckduckgo.com`，trpc 默认） | 是 | 0 | 返回结构化 JSON，比抓 HTML 稳，**结果是精简摘要不是完整 SERP** | 可做兜底之一 |
| DuckDuckGo lite / html（抓取） | 是 | 0 | **数据中心 IP 确定性失败**（§2.8） | 降级项，不做默认 |
| You.com keyless（`?profile=free`） | 是 | — | **100 次/天**，$100 信用额度 | 免 key 链的候补 |
| openserp（**自托管**） | 是 | 自建成本 | Go 实现，成功代理 Google/Bing/DDG | 企业自建候选，但底层引擎许可存疑 |
| Mojeek | — | — | **robots.txt 明确禁止 `/search`** | **排除（不合规）** |
| Brave Search API | 否 | $5 | 老免费额度疑已取消，**需重新核实** | 待定 |
| Tavily（带 key） | 1,000 次/月 | $4–8 | agent 场景默认选择 | 带 key 档首选 |
| Exa | $10 试用 + $10/月 | $7（+内容 $1） | opencode 内置 provider | 带 key 档 |
| Parallel | 最高 5,000 次/月 | $1–5 | 免费额度高 | 带 key 档 |
| Serper | 2,500 次（一次性） | $0.30–1 | 最便宜的 Google SERP | 带 key 档 |
| Jina Reader（`r.jina.ai`） | 是（20 RPM） | token 计价 | URL→Markdown，WebFetch 失败时的兜底 | **可用但有风险，见下** |
| Bing Search API | — | — | **2025-08-11 已退役** | 排除 |
| Google CSE | — | — | **已关闭新客户，2027-01-01 全停** | 排除 |

**修订后的选型建议**：

1. **不要再把 DDG 当唯一默认。** 本机跑通 ≠ 生产可用。§2.8 已给出证据。
2. **免 key 优先链**：`OpenRouter:web_search`（若 provider 是 OR）→ `SearXNG 自建` → `Tavily keyless` → `DDG`（最后兜底）。
3. **带 key 档**优先于免 key 链：Brave / Tavily / Exa / Parallel / `--search-api` / MCP。
4. **自建 SearXNG 是企业版的合理答案**：woca 已有企业配置面与沙箱体系，
   下发一个内网 SearXNG 端点在架构上是自然的，且 ToS 干净。
6. **排除 Mojeek**（robots.txt 禁止）、**Bing**、**Google CSE**。

### 两个必须记住的坑

**坑 1：Jina Reader 会绕过本地 SSRF 与域名策略。**
`r.jina.ai/<url>` 能把任意 URL 转成 Markdown，**包括你本地策略本该封掉的目标**
（内网地址、被封禁站点）。把它加进 WebFetch 兜底链时，**必须先过域名白名单**，
否则等于给 SSRF 开了一条旁路。同理适用于任何第三方抓取代理（Firecrawl、Browserbase）。

**坑 2：Go 侧没有搜索 API 的官方 SDK。**
Tavily / Brave / Exa / Serper 全都没有官方 Go SDK。要么自写（配合 `goquery` 解析），
要么用 OpenAPI 生成器从 OpenAPI 规范生成。**建议自写**，每个 provider 大约 60 行。

### Go 依赖结论

| 库 | 用途 | 是否建议 |
|---|---|---|
| `PuerkitoBio/goquery` | CSS 选择器解析 HTML | **建议**（Crush v1.13.0、zero 都在用） |
| `JohannesKaufmann/html-to-markdown/v2` | HTML→Markdown | **建议**（Crush v1.6.0；v1 已停更，直接上 v2） |
| `golang.org/x/time/rate` | 限流器 | **建议**（比自己写 ticker 稳） |
| `lestrrat-go/backoff` | 指数退避 | 可选 |
| `temoto/robotstxt` | robots.txt 解析 | 可选（docker/cagent 的合规做法） |
| `markusmobius/go-domdistiller` | 正文抽取 | 可选（`go-shiori/go-readability` 已归档，别用） |

### 免 key 的现实：没有免费的午餐

社区 ADR-0043 的原话值得引用：

> "No ToS-clean keyless SERP API exists in 2026."

**这意味着 woca 必须在产品层面做一次明确表态**（见 §4.5 待定问题 1）：
要么接受「默认后端不保证 SLA」，要么在首次搜索失败时**引导用户配一个 key 或内网端点**，
而不是假装一切正常。

---

## 4. 设计方向（待确认）

> 本节是规格草案，不是实施计划。确认后按 superpowers 流程写实施计划。

### 4.1 目标形态

```
                  ┌─────────────────────────────────────────┐
   WebSearch      │  工具壳（内置，恒在，名字不变）            │
   WebFetch  ────▶│  权限类别 CatWeb，策略维度独立于沙箱 network│
                  └────────────────────┬────────────────────┘
                                       ▼
                          SearchProvider 注册表
   ┌──────────┬──────────┬──────────┬─────────────┬──────────────┐
   │ ddg-api  │ ddg-lite │ tavily   │  brave      │ generic-http │
   │ (默认)   │ (降级)   │ (keyless)│  (key)      │ (--search-api│
   │ 免 key   │ 免 key   │ 免 key   │             │  / MCP 桥)   │
   └──────────┴──────────┴──────────┴─────────────┴──────────────┘
                                       ▼
              调度内核：冷却表 + 429 故障转移 + 会话亲和 + 节流 + anomaly 识别
```

**四个已被市场验证的结构约束**：

1. **工具壳名字恒定**（`WebSearch` / `WebFetch`）。换后端不该改工具名——否则系统提示词、
   权限规则、用户习惯全都跟着变。opencode / Crush / Kilo Code 全都遵守。
2. **策略维度独立于沙箱 network**（GitHub `hosted-web`、Codex hosted tool 都是这么切的）。
   woca 已有沙箱 executor，需要新增一个「web 出口策略」维度，默认跟随、可单独关。
3. **按 agent 类型分发**（Crush 的做法）：主 agent 给 `WebFetch`，`WebSearch` 给 sub-agent。
   避免主上下文被搜索结果污染。
4. **三级访问阶梯**：`WebFetch`（原文）→ `WebSearch`（发现）→ subagent 式抽取（贵，兜底）。

### 4.2 免 key 优先的降级链

```
优先级 1（配了就用）：brave / tavily / exa / parallel / --search-api / MCP 桥
优先级 2（免 key 链）：openrouter:web_search → searxng(自建) → tavily-keyless → ddg-api → ddg-lite
优先级 3：全部不可用 → 明确报错 + 引导配置
```

**四条不可省略的规则**：

1. **命中 anomaly / 429 → 进冷却表，换下一个，绝不重试同一个。**
2. **「被拦」与「搜不到」必须给模型不同反馈。**
   - 被拦 → `"Search backend is rate-limiting this machine. Do not retry or rephrase; wait a few minutes or fetch known URLs directly."`
   - 搜不到 → `"No results found for this query."`
   混为一谈会让模型断定「信息不存在」进而编造（§2.8 的 jcode #1765 教训）。
3. **CDP 挑战不重试**。遇到 JS challenge，重试多少次都是同一个结果，只会放大限流。
4. **「本地能跑通」不算验证通过**。测试必须用注入式 fake backend（zero 的
   `newWebSearchToolWithBackend` 就是为这个准备的），并且要有明确的一条
   「生产（数据中心 IP）环境」验证清单。

### 4.3 接口草案（Go）

```go
// internal/searchapi/provider.go
type Provider interface {
    ID() string
    Name() string
    Available(cfg Config) bool                                          // 凭据/配置是否就绪
    Search(ctx context.Context, query string, limit int) ([]Result, error)
}

type Result struct {
    // 必选
    Title, URL, Snippet string
    // 可选（见下方契约说明）
    Content   string     // 正文片段，仅 API 类后端提供
    Published *time.Time
    Score     float64    // 可选 [0,1] 相关性；0 = 未提供
}

// 调度：冷却表 + 429 故障转移 + 会话亲和 + 节流
type Router struct {
    providers []Provider
    cooldowns map[string]cooldown
    affinity  map[string]string // sessionID -> providerID
}

// 构造函数双入口（TDD 必需）：
func NewWebSearchTool() Tool                       { return newWebSearchToolWithBackend(defaultBackend()) }
func newWebSearchToolWithBackend(p Provider) Tool  { ... }   // 测试注入
```

**四个必须照搬的细节**：

1. **`nil` provider 是一等状态**——「没配后端」和「搜不到」必须给模型不同反馈。
2. **构造函数双入口**——生产读 env/配置，测试注入 fake。**没有这个接缝就没法 TDD。**
3. **`Search` 带 `limit`**，让 provider 自己决定取多少（zero 默认 5 / 上限 10；Crush 默认 10 / 上限 20）。
4. **结果契约分「必选 + 可选」两层**（三个参考实现是 2:1，不是统一）：

   | 必选 | 可选 |
   |---|---|
   | `title` / `url` / `snippet` | `content`（正文片段）/ `published`（发布时间）/ `score`（相关性） |

   - **Crush、zero 不返回正文**，要正文必须再调 `web_fetch`——省 token 的刻意设计。
   - **OpenCode 允许返回可选 `content`**（结果里带 chunk）。
   - **woca 的取舍**：结果契约里**保留 `content` 字段**。免费/抓取类后端不填，
     付费 API 类后端（Tavily / Exa / Parallel）本来就在响应里带 chunk，**不填等于浪费已付的钱**。
     但渲染时按 token 预算截断，且**付费后端填了 `content` 就可以少发一次 `WebFetch`**。

   **安全约束（跟 §4.6 红线 3 是同一条）**：正文只能来自**本次搜索结果里的 URL**，
   且必须过 `WebFetch` 那套 SSRF 校验；**不得**对 `content` 里的链接做二次自动抓取，
   否则等于让外部内容驱动出网，把 SSRF 防护绕过去。

**错误分类（最该被 TDD 覆盖的部分）**：

```go
var (
    ErrRateLimited = errors.New("search backend is rate-limiting this machine; " +
        "do not retry or rephrase; wait a few minutes or fetch known URLs directly")
    ErrNoBackend = errors.New("no search backend configured; set --search-api or configure a provider")
)
```

| 情形 | 返回给模型 | 换 provider？ |
|---|---|---|
| 无后端 | `ErrNoBackend` | 否（无后可换） |
| HTTP 429 / anomaly 标记 | `ErrRateLimited` | **是**，该 provider 进冷却 |
| 网络错 / 5xx / 401 | 原始错误 | 否 |
| 正常但 0 条 | `No results found for this query.` | 否 |

**最后一行是关键**：0 条结果是**成功**，不是失败。绝不能跟「被拦」混为一谈（§2.8）。

### 4.4 优先级建议

| 优先级 | 项目 | 对应差距 |
|---|---|---|
| **P0** | **区分「被拦」与「搜不到」**：anomaly 检测（HTTP 202 + 3 个标记）+ 两种错误文案 | **G2** |
| **P0** | **在目标部署网络里实测一次**（企业 worker / CI / 容器），拿到 DDG 的真实成功率 | G2 / §2.8 |
| P0 | 节流（请求间隔 + 冷却表）+ UA 池 | G3 G4 |
| P0 | WebFetch 换 HTML→Markdown + 大页落盘 | G6 |
| P1 | provider 注册表（先 2 个：ddg、generic-http 兼容现有 `--search-api`） | G1 |
| P1 | 冷却 + 429 故障转移 + 会话亲和 | G1 |
| P1 | SSRF 换 `SSRFSafeTransport`（DNS 解析 + IP 固定） | G7 |
| P2 | managed 配置层（settings.json / 企业配置面下发） | G8 |
| P2 | 结果元数据（发布时间） | G10 |
| P2 | DOM 解析替换正则 | G5 |

**P0 的第一条和第二条是同一件事的两半**：先让工具说真话，再确认真话是什么。
只做第一条不做第二条，等于不知道自己到底有没有被拦。

### 4.5 待定问题（需要你拍板）

| # | 问题 | 选项 | 我的倾向 |
|---|---|---|---|
| 1 | **免 key 默认后端的合规立场**。社区共识是 2026 年没有 ToS-clean 的免 key SERP。 | a) 继续用 DDG，接受无 SLA<br>b) 默认 SearXNG 自建<br>c) 首次失败即引导用户配 key | **b + c**：企业版下发内网 SearXNG 端点；个人版用 Tavily keyless，失败时明确引导 |
| 2 | **企业版是否内置统一托管搜索端点** | a) 是，进 managed 配置层<br>b) 否，沿用 `--search-api` | **a**。woca 已有 managed 层（MCP/skills 都有），搜索是唯一缺口（G8） |
| 3 | **Go 依赖取舍**。woca 现在直接依赖只有 18 个，MCP 也是自研的。 | a) 零新依赖，全自研<br>b) 加 `goquery` + `html-to-markdown/v2`<br>c) 全量加 | **b**。这三个 Go agent 里 Crush 与 zero 都选了这两个库（`goquery v1.13.0`、`html-to-markdown v1.6.0`），是事实标准；Markdown 转换自研不划算 |
| 4 | **MCP 客户端是否换成官方 go-sdk**（**与 web search 无关，可独立决策**） | a) 保持自研（现状）<br>b) 换 `modelcontextprotocol/go-sdk` | **a（本次不动）**。woca 自研 3,365 行已支持 stdio + StreamableHTTP + OAuth，替换是纯风险，且收益为零（搜索不走 MCP）。<br>**但方向要记下来**：Go 生态在 2025→2026 已从 `mcp-go` 集体迁到官方 go-sdk（Crush 2026-08 切、`opencode(Go)` 2025-04 选 mcp-go 是当时的合理选择）。将来若重写 MCP 层，**默认应选官方 go-sdk** |
| 5 | **WebFetch 的超限行为**（实测支撑见 §4.7：12 个常见文档页 text 中位数 54,949B，**50% 超过 50KB**） | a) 保持 20k 字节硬截断<br>b) 学 Crush 大页落盘<br>c) 学 Claude Code 放到 100k | **b**。落盘比提上限更省 token——大页写临时 .md，只把路径回给模型，让 agent 用 `Read`/`Grep` 按需取。<br>**关键推论**：既然一半的页会落盘，落盘就**不是异常路径而是主路径**，必须按主路径的质量要求实现（临时文件清理、并发唯一性、错误回退） |
| 6 | **中文场景的阈值口径**（`maxWebChars` 名不副实，见 G6a） | a) 沿用字节口径，调大数值<br>b) 改用 rune 口径 | **b**。Go 的 `len(string)` 返回字节，中文 3 字节/字。用 rune 口径语义才正确，且**必须避免 `s[:n]` 按字节切片切坏汉字**（当前 `web.go:73` 就有这个隐患） |
| 7 | **WebSearch 给主 agent 还是只给 sub-agent** | a) 主 agent 也要<br>b) 只给 sub-agent（Crush 做法） | **a 起步**。woca 现在是主 agent 就有，先保持；但要把 §4.1 的三级阶梯做出来，让模型有便宜路径可选 |

### 4.6 Go 侧工程清单（依赖 + 反爬红线）

**Go 库选型**（对照 woca「最小依赖」惯例，go.mod 直接依赖现为 18 个）：

| 用途 | 推荐 | 说明 |
|---|---|---|
| HTML 解析 | `github.com/PuerkitoBio/goquery` v1.13.0 | 14,991★，Crush 与 zero 都在用 |
| HTML→Markdown | `github.com/JohannesKaufmann/html-to-markdown/v2` | **直接上 v2**，v1 已停更（Crush 仍锁 v1.6.0） |
| 限流 | `golang.org/x/time/rate` | 比手写 ticker 稳 |
| 退避 | `github.com/lestrrat-go/backoff/v2` | 比 Crush 手搓 sleep 更规范 |
| robots.txt | `github.com/temoto/robotstxt` | cagent 的合规做法，可选 |
| SSRF | **用现成的**：`security.SSRFSafeTransport` | 库里已有，只是 `fetchHTTP` 没用（G7） |

**不要碰的**：`go-shiori/go-readability`（已归档）。正文抽取若要更强的，看 `markusmobius/go-domdistiller`。

**版本钉法（踩过的坑）**：若在 `go.mod` 里写 `github.com/modelcontextprotocol/go-sdk v1.7.1`，
`go mod tidy` 会失败——**不存在这个 tag**。官方 tag 是 `v1.7.0` / `v1.8.0`（模块路径 `mcp`）；
Crush 用的 `v1.7.1-0.20260825092617-32404652e60b` 是 **pseudo-version（钉 commit）**，`go get` 拿不到。
**要钉稳定版就写 `v1.8.0`**，需要新 API 才用 `go get ...@<commit>`。

**搜索 API 没有官方 Go SDK**——Tavily / Brave / Exa / Serper 全都没有。每个 provider 自写约 60 行即可，不值得引生成器。

**反爬与兜底的三条红线**：

1. **弃用 User-Agent 伪装思路。** DDG 判定在 IP 信誉层，换 UA 不解决问题。
   UA 池（Crush 那套 11 个 UA + `Sec-Fetch-*`）**只留作基础礼貌**，不当对抗手段。
2. **不重试 CDP/JS challenge。** 遇到 HTTP 202 或 anomaly 标记，重试只会加剧限流。
   直接进冷却、换 provider、告诉模型等几分钟。
3. **第三方抓取代理会绕过本地 SSRF 与域名白名单。** `r.jina.ai/<url>`、Firecrawl、Browserbase
   都能拿到你本地策略本该封掉的目标。**若把 Jina 加进 WebFetch 兜底链，必须先过域名白名单**，
   否则等于给 woca 的 SSRF 防护开一条旁路。

### 4.7 安全与预算规格（四条强制条款）

**先看一个实证对照**——它决定了下面三条不是「防御性建议」：

| 实现 | 允许 `content` | 域名过滤 | 脱敏 |
|---|---|---|---|
| OpenCode（TS） | ✅ 可选 content | ❌ 仅 GitHub Copilot 路径透传，自建 provider 路径没有 | ❌ 零命中 |
| Crush | ❌ | ❌ 零命中 | ❌ 零命中 |
| **zero** | ❌ | ✅ `filterWebSearchByDomains` | ✅ `redactWebSearchText` → `internal/redaction` |

**「允许正文」与「不做过滤/脱敏」在实现上是共现的**——唯一返正文的那家，是唯一没做任何防护的那家。
**所以照抄任何一家都会带洞进来。** 下面三条是必须补的最小集。

**条款 1：`web_fetch` 必须能独立工作。**
`content` 只是加速路径，不是依赖。付费后端挂了/被限流时，抓正文不能跟着一起死。

**条款 2：snippet 与 content 必须过同一条 redaction 管线。**
zero 的四个出口**全部脱敏**（错误、过滤失败、空结果、正常结果）——注意「错误出口」也在内。
搜索结果是不可信输入，错误分支最容易漏。

**条款 3：`allowed_domains` 过滤必须在 content 进入 context 之前。**
zero 的注释原话：*"Filter by domains BEFORE the empty-result short-circuit"*——**先筛，后装载**。
顺序反了等于先把外部内容读进内存再判断能不能用。

**条款 4（预算口径）：统一在「进 context 的文本量」这一层，且超限落盘而非截断。**

**关键实证：在 Crush 的同一个 `web_fetch` 调用里，网络侧放行 5MB，文本侧只放行 50,000 字节，
相差 100 倍，两侧之间不做任何换算。**

```
web_fetch.go:47   content, err := FetchURLAndConvert(ctx, client, params.URL)
                    └─ fetch_helpers.go:46   maxSize := int64(5 * 1024 * 1024)   // 5MB
                    └─ fetch_helpers.go:47   io.ReadAll(io.LimitReader(resp.Body, maxSize))
                        ↑ 网络层预算，HTML→Markdown 转换【前】
                  …转换…
web_fetch.go:51   hasLargeContent := len(content) > LargeContentThreshold         // 50,000
                    ↑ 文本层预算，转换【后】，超限落盘
```

这比「各家口径不一」硬得多——它直接证明**字节数无法跨 HTML→Markdown 转换边界换算**。

| 位置 | 值 | 层级 / 时机 |
|---|---|---|
| Crush `fetch_helpers.go:46-47` | `5 * 1024 * 1024`（5MB） | 网络层，`io.LimitReader`，转换**前** |
| Crush `fetch.go:22` | `MaxFetchSize = 100 * 1024`（100KB） | 网络层，**老** fetch 路径，转换**前** |
| Crush `fetch_types.go` | `LargeContentThreshold = 50000` | **文本层（字节）**，转换**后**，超限落盘 |
| OpenCode `plugin/websearch/mcp.ts:7` | `MAX_RESPONSE_BYTES = 256 * 1024` | 单次 MCP 响应整包（含 JSON-RPC 信封，多结果共享） |
| OpenCode `html-markdown.ts:50` | `MAX_MARKDOWN_BYTES = 5 * 1024 * 1024` | 网络/转换层 5MB |

> 注：5MB 在 **Crush 和 OpenCode 里各有一份**，不是某家独有。

两条规则：

- **预算定义在「进 context 的文本量」层**（rune / token），**不用网络响应字节**。
  上面的调用链就是反证：同一调用内两侧差 100 倍且互不换算。
- **中文场景阈值必须单独校准。** `LargeContentThreshold = 50000` 是**字节**（Go 的 `len(string)`
  返回字节数），UTF-8 下每汉字 3 字节 → **50,000 字节 ≈ 1.6 万个汉字**，远少于「5 万字」的直觉。
  **woca 面向中文用户，沿用这个阈值会让中文正文过早落盘，必须上调或改用 rune 口径。**
- **超限用落盘，不用截断。** 截断永久丢内容；落盘后模型还能用 `Read`/`Grep` 找回。
  Crush 做法：`os.CreateTemp(workingDir, "page-*.md")` → **只把路径回给模型**。

**实测：阈值定在哪，直接决定「落盘」是例外还是常态。**

取 12 个常见技术文档页（MDN / pkg.go.dev / RFC / Python / React / K8s / Docker / Rust / PG / TS / Nginx），
按 woca 现有的去标签方式抽出纯文本后量字节数（2026-10-21 实测）：

```
page                         raw HTML   text bytes  >50KB?
MDN Array.reduce              190,700       19,023  no
Go net/http pkg               483,872      163,014  YES
Go context pkg                 97,101       24,277  no
RFC 7231                      235,053      208,044  YES
Python asyncio docs           177,177       46,353  no
React useEffect               601,984       44,661  no
K8s Pods concept              547,061       47,423  no
Dockerfile reference        1,066,390      140,026  YES
Rust std Vec                  955,558      240,430  YES
PostgreSQL SELECT             118,042       62,475  YES
TypeScript handbook           321,570       31,303  no
Nginx ngx_http_core           157,055       66,435  YES
──────────────────────────────────────────────────────────
n=12  median=54,949B  p75=163,014B  max=240,430B
超过 50,000 字节的比例：6/12 = 50%
```

**结论**：median 54,949B 就压在 50,000 阈值线上——**按 Crush 的阈值，一半的常见文档页会走落盘。**
落盘在这个量级下**不是异常路径，而是主路径之一**，
所以它的实现质量、临时文件清理、并发唯一性都得按主路径对待，不能当边角料写。

> 口径说明：上表是**纯文本**（对齐 woca 现有 `stripHTML` 行为）。转成 Markdown 后体积会更大一些，
> 所以真实落盘比例只会更高，不会更低。

**顺带查出 woca 自己的一个命名问题**：`internal/tools/web.go:17` 的
`maxWebChars = 20000` 实际是**字节**上限（`web.go:72` 用的是 `len(out)`，Go 对 string 返回字节数）。
- 名字叫 "Chars"，其实是 bytes → 中文页按 3 字节/字算，**实际只能拿到约 6,667 个汉字**。
- 对照上面 median 54,949B：**woca 现在把多数英文文档页裁到只剩约 36%**。

这条已并入 §1.2 的 G6。

---

## 5. 证据索引

### 5.1 一手核实（本次直接读源码 / 跑命令）

| 结论 | 证据 |
|---|---|
| opencode provider 注册表 | `anomalyco/opencode@dev`: `packages/core/src/websearch.ts`、`plugin/websearch/{index,mcp,exa,parallel}.ts` |
| opencode 冷却/亲和/429 故障转移 | 同上，`websearch.ts` 的 `randomProvider` / `query` / `cooldownMillis`（读 `Retry-After`） |
| opencode MCP 桥 | `packages/core/src/plugin/websearch/mcp.ts`：25s 超时、256KB 上限、兼容直接 JSON 与 SSE `data:` |
| opencode 不用通用 MCP client | 自写约 60 行 `tools/call` 直发器，不做握手/工具发现 |
| opencode 曾无 websearch（Go 时代） | Go 版止于 2025-05-31（`f3da73553c45`），`internal/llm/tools/` 只有 `fetch.go` + `sourcegraph.go` |
| Crush 内置 DDG Lite | `charmbracelet/crush@main`: `internal/agent/tools/search.go`：`ddgLiteEndpoint`、anomaly markers、`maybeDelaySearch`、`setRandomizedHeaders` |
| Crush 三级阶梯 | `fetch.go` / `web_search.go` / `internal/agent/agentic_fetch_tool.go` + `templates/agentic_fetch.md` |
| Crush 抓页落盘 | `internal/agent/tools/web_fetch.go`：`LargeContentThreshold = 50000` |
| **Crush 用官方 go-sdk** | `crush@main` `go.mod:50` → `github.com/modelcontextprotocol/go-sdk v1.7.1-0.20260825092617-32404652e60b` |
| Crush 用 goquery + html-to-markdown | 同 `go.mod`：`PuerkitoBio/goquery v1.13.0`、`JohannesKaufmann/html-to-markdown v1.6.0` |
| zero 的 searchBackend 接口 | `Twigpine/zero@main`: `internal/tools/web_search.go`（`searchBackend` 接口、`newWebSearchToolWithBackend` 测试注入、256KB/10s/重定向 5） |
| zero 零 MCP 依赖 | `zero@main` `go.mod` 搜 `mcp|modelcontextprotocol` → 0 命中 |
| Tavily keyless | opencode `plugin/websearch/tavily.ts`：`X-Tavily-Access-Mode: keyless` |
| woca 现状 | `internal/tools/web.go`、`internal/searchapi/`、`internal/agentbuild/build.go:178`、`internal/agentbuild/layers.go` |
| woca SSRF 只校验 hostname | `langchain-golang@v0.11.0/core/security/security.go:60` `ValidateURL`；`SSRFSafeTransport` 存在于库但 `fetchHTTP` 未使用 |
| woca web 工具不经 executor | `internal/tools/default.go:31-32`（`webFetchTool()`/`webSearchTool()` 无 `execFor`）对比 `bashTool(d, bg, execFor)` |
| woca 无 web 搜索设置项 | `internal/settings/keys.go` 无相关键；`web/` 前端搜 `search-api` 零命中；`README.md` 只在工具清单里提了一句 |
| DDG 本机实测通过 | 2026-10-21 curl `html.duckduckgo.com/html/?q=golang+mcp+server` → 200；正则解析出 10 条 `result__a` + 10 条 `result__snippet` |
| Go SDK/库活跃度 | `gh repo view`：mark3labs/mcp-go 9,178★；modelcontextprotocol/go-sdk 5,252★（官方，与 Google 共同维护）；goquery 14,991★；html-to-markdown 3,835★；zero 1,739★（Go，当日活跃） |
| Plandex 无 web 能力 | `plandex-ai/plandex` 内置工具表（`load_plan_files`/`build`/`terminal`/`code`）；`plandex load <url>` 是 `dev` 隐藏命令组 |
| Crush subagent 不加载 hooks | `internal/agent/coordinator.go:428` 注释 "subagent doesn't get custom hooks" |
| Crush MCP 命名/配置键 | 工具名 `mcp_<server>_<tool>`；`MCPStdio` / `MCPSse` / `MCPHttp` ([]string) + `MCP map[string]MCPServer` |
| opencode(TS) webfetch CF 对抗 | `packages/core/src/tool/plugin/webfetch.ts`：5MB 上限，默认 markdown，`403 + cf-mitigated: challenge` → 裸 UA `"opencode"` 重试一次 |
| opencode(TS) MCP 默认 codemode | `tool/plugin/webfetch.ts` 注释："MCP servers are enabled by default (codemode: true)" |

**复核时的 commit SHA**（调研当日 main/dev 分支头部）：

| 仓库 | 分支 | SHA | 日期 |
|---|---|---|---|
| `charmbracelet/crush` | main | `d633f60a` | 2026-10-21 |
| `anomalyco/opencode` | dev | `539a8fc9` | 2026-10-21 |
| `Twigpine/zero` | main | `5f7447a2` | 2026-10-21 |
| `trpc-go/trpc-agent-go` | main | `12bbbd8b` | 2026-10-21 |
| `docker/cagent` | main | `f3bcf205` | 2026-10-21 |
| `opencode-ai/opencode`（归档） | HEAD | `73ee4932` | 2025-09-17 |

### 5.2 引用的外部结论（二手，采信前建议复核）

| 结论 | 来源 |
|---|---|
| DDG 对数据中心 IP 确定性失败（HTTP 202 + JS challenge），UA 无效 | sst-opensource/adr-gallery ADR-0043《Web Search Integration》、niyogi、huggingface 多篇 2026 讨论 |
| 「被拦」被模型误读为「不存在」 | jcode issue #1765 |
| Mojeek robots.txt 禁止 `/search` | Mojeek 官方 robots.txt |
| Brave 免费额度疑已取消 | 社区反馈（与 serpex.dev 2026-10 报价冲突，**需重新核实**） |
| Bing Search API 2025-08-11 退役 / Google CSE 2027-01-01 全停 | 微软、Google 官方公告（经 serpex.dev 汇总） |
| GitHub `hosted-web` 独立于 `network.allowed` | GitHub Agentic Workflow Firewall 文档 |
| Codex web search 不受沙箱网络策略约束 | OpenAI Codex 官方文档 |
| Claude Code 每 session 200 次、`$10`/千次、Bedrock 无 | Anthropic 官方文档 + Claude Agent SDK 实测（parallel.ai 2026-10-08） |
| `go-shiori/go-readability` 已归档 | GitHub 仓库状态 |

> **复核提示**：§5.2 里标「需重新核实」的 Brave 免费额度，以及 DDG 在 woca 目标部署环境（企业 worker / CI / 容器）的实际表现，
> 是落地前必须先验证的两项。本机住宅 IP 跑通不构成验证。

### 5.3 取证方法备注（给后续复查者避坑）

本次调研里，**三个人先后在「判断某仓库是不是 Go 项目」这一步上取错过**，代价是得出完全相反的结论。
把方法固化下来：

**陷阱：不能用 tag / 版本名判断「某个仓库是否曾经是某个语言」。**

踩坑路径（真实发生过）：

1. `git ls-remote --tags anomalyco/opencode` 拿到 `v0.0.55 / v0.1.43 / v0.2.0 / v0.15.17`
2. 按 tag 去 raw 取 `go.mod` → **全部 404**
3. 据此推断「该仓库没有 Go 代码」❌

**真实原因**：tag 指向的是 **TS 重写之后**的 commit（v0.x 版本号是 TS 时代打的），
而 Go 代码存在于**没有任何 tag 覆盖的早期 commit** 里。

> 按 tag / commit-ish 取文件，只能证明「该引用处没有」，**不能证明「仓库历史里没有」**。

**正确做法**：

| 目的 | 命令 |
|---|---|
| 按早期 commit 直接取文件 | `raw.githubusercontent.com/<repo>/<sha>/go.mod` |
| 找「最后一次含某文件的提交」 | `gh api 'repos/<o>/<r>/commits?path=go.mod&per_page=5'` → 最后一条即该文件被删/最后修改的提交 |

**交叉核验后的完整时间线**（两人独立得出、结论一致）：

```
2025-03-16  opencode-ai/opencode 创建（Go，module github.com/opencode-ai/opencode）
2025-04-30  anomalyco/opencode 创建，早期 commit 仍为 Go（同源，mcp-go 同为 v0.17.0）
2025-05-31  anomalyco 侧最后一个含 go.mod 的提交 f3da7355 → 之后被 TS 重写覆盖
2025-09-18  opencode-ai/opencode 末次 push（Go，随后归档）
              README: "continued under the name Crush, by the original author + Charm team"
              ↓
2026-08     charmbracelet/crush（Go）→ MCP 客户端换成官方 modelcontextprotocol/go-sdk v1.7.1
```

**两个 grep 陷阱**（同样低成本、同样会翻车）：

- `modelcontextprotocol` 字符串里**不含 `mcp` 子串**——只 grep `mcp` 会漏掉官方 SDK。
- `gh api repos/sst/opencode` 会 **302 到 `anomalyco/opencode`**（TS 版）。
  要 Go 实现必须去 `opencode-ai/opencode`（已归档，HEAD `73ee4932`）。

# WebSearch：免费 provider 链 + 降级 + 说真话

分支 `feat/websearch-free-providers`（from `main` @ `b748484`）
工作区 `.worktrees/websearch-free-providers`
（已合并到 main，分支与 worktree 已删除）

---

## 1. 范围

**做**：搜索链路。把 `WebSearch` 从「单一 DDG 正则抓取」改成
「provider 注册表 + 免费链降级 + 被拦/搜不到区分 + 节流冷却 + SLA 埋点」。

**不做**（下一个分支）：

- `WebFetch` 的 HTML→Markdown 转换、rune 口径阈值、大页落盘（G6）。
- `SSRFSafeTransport` 替换（G7）。
- managed 配置层下发搜索端点（G8）。
- `openrouter:web_search`——它是对 OpenRouter **chat completions 请求**加
  `plugins:[{"id":"web"}]`，属于 `internal/llm` 层而非工具层。
  已核实 `internal/` 下不存在 openrouter 相关代码（grep 零命中），
  本分支不具备接入条件。
- 任何付费 provider（Brave / Tavily keyed / Exa / Parallel）。

**范围外的两个既有缺口，本分支顺手修掉（因为它们就在搜索热路径上）**：

- `parseDuckDuckGo` 的正则解析 → goquery（G5）。
- 「被拦」谎报成 `No results found.`（G2，本分支的 P0）。

---

## 2. 依赖决策（用户问：自研还是用 goquery）

**结论：用 goquery。它不是「新增依赖」，是「把已在图里的依赖提为直接依赖」。**

实测（`go mod why` + `go.sum` + `GOMODCACHE` + 离线 `go run`，GOPROXY=off）：

| 事实 | 证据 |
|---|---|
| goquery **已在模块图** | `go list -m github.com/PuerkitoBio/goquery` → `v1.10.0` |
| 传递路径（非我们引入） | `langchain-golang/core/documentloaders` → goquery → cascadia |
| **go.sum 已有完整 `h1:` 校验和** | `PuerkitoBio/goquery v1.10.0 h1:6fiXdL…`（zip 级，不只 go.mod） |
| **源码已在模块缓存** | `GOMODCACHE/github.com/!puerkito!bio/goquery@v1.10.0` |
| **离线可用，零网络** | `GOPROXY=off go run` 解析成功 |

**版本不升**：MVS 已钉 `v1.10.0`，不追 `v1.13.0`。追新需要一个不在 go.sum 里的
新校验和 = 真实的网络取包 + 供应链新增面。保持 v1.10.0 = 供应链零变化。

**构建图增量（唯一真实成本）**：4 个包

```
github.com/PuerkitoBio/goquery
github.com/andybalholm/cascadia
golang.org/x/net/html
golang.org/x/net/html/atom
```

`golang.org/x/net`、`golang.org/x/time` 在 go.mod 里已是 indirect 且带完整 `h1:`，
`rate`（节流要用）和 `html` 都可直接用，不算新增模块版本。
go.sum 新增的 34 行**全部是 `/go.mod` 行**（goldmark / x/crypto / x/mod / x/net /
x/sync / x/sys / x/term / x/text / x/tools / x/xerrors 的历史版本），
是 MVS 图遍历产物，**没有任何 `h1:` zip 行 = 没有新下载任何模块代码**。

**CI 影响**：`ci/deps-personal.txt` 是 CI 强制基线（`make check-deps` diff 校验）。
引入后需 `make deps-baseline` 重生成，diff 会多上面 4 行。
Makefile 的准入红线是「personal 二进制不得链 pgx/NATS/Redis/OIDC 类」，
goquery 是纯 Go、无 cgo、MIT 的小库，不触碰该红线。

**为什么不自研**：自研只有两条路——

1. 继续正则 —— 就是我们现在要修掉的 G5。
2. 用 `golang.org/x/net/html` 自写树遍历 —— 少 2 个包，但多约 80 行手写 DOM 遍历，
   且要自己处理畸形 HTML、属性顺序、CDATA。Crush 和 zero 两个 Go harness 都选了
   goquery，是事实标准。

**注意**：用户提到的「各个通路的统一代码抽象」与 goquery **正交**，
那层（`Provider` 接口 + `Router`）无论选哪条路都得自己写，本计划照写。

---

## 3. 接口设计

新包 `internal/websearch/`。

```go
// provider.go
type Result struct {
    // 必选
    Title, URL, Snippet string
    // 可选：免费/抓取类后端不填
    Content   string
    Published *time.Time
    Score     float64 // [0,1]，0 = 未提供
}

type Provider interface {
    ID() string          // "ddg-html" / "searxng" / ...
    Name() string
    Available() bool     // 配置是否就绪；构造时带配置，故无需参数
    Search(ctx context.Context, query string, limit int) ([]Result, error)
}

var (
    ErrNoBackend = errors.New("no search backend configured; set --search-api 或配置一个 provider")
    ErrRateLimited = errors.New("search backend is rate-limiting this machine; " +
        "do not retry or rephrase; wait a few minutes or fetch known URLs directly")
)

// Failover：该错误是否意味着「此后端当前不可用，值得换下一个」。
// 429/503/202/anomaly → true；网络错/5xx(非503)/401/403 → false。
func Failover(err error) bool
```

**错误分类表（本分支最该被 TDD 覆盖的部分）**

| 情形 | 返回给模型 | 换 provider？ |
|---|---|---|
| 无可用 provider | `ErrNoBackend` | 否 |
| HTTP 429 / 503 / 202 | `ErrRateLimited`（带 Retry-After 冷却） | **是** |
| HTTP 200 但命中 anomaly 标记 | `ErrRateLimited` | **是** |
| 网络错 / 401 / 403 / 其他 5xx | 原始错误 | 否 |
| 正常但 0 条 | **成功**，空切片 → 渲染 `No results found for this query.` | 否 |

**P0 的准确表述**（实测读 `web.go` 后修正）：

当前 `fetchHTTP` 对非 200 直接返回 `HTTP %d` 错误，所以 **HTTP 202 路径其实已经报错了**，
只是报了个没用的 `HTTP 202`，模型不知道那是限流、会去重试。
真正说谎的是另一条路径：**DDG 返回 HTTP 200 + 挑战页**（无 `result__a` 锚点）
→ `parseDuckDuckGo` 返回空 → 工具回 `No results found.`。
所以 P0 的两半是：

1. 200 + 0 条 + 命中 anomaly 标记 → `ErrRateLimited`（**不是** "No results found"）。
2. 202/429/503 → `ErrRateLimited`（**不是** 裸 `HTTP 202`）。

「0 条且无 anomaly 标记」= 真的没搜到，才是成功。

**Router**

```go
type Router struct {
    providers []Provider
    cooldown  map[string]time.Time
    limiter   *rate.Limiter
    // ...
}
func (r *Router) Search(ctx, query string, limit int) ([]Result, error)
```

算法：取限流令牌 → 按序遍历 provider，跳过冷却中的 → 成功即返回 →
`Failover(err)` 为真则写冷却并换下一个 → 非 failover 错误立即返回 →
全部 failover 失败则返回 `ErrRateLimited` → 无 provider 返回 `ErrNoBackend`。

**域名过滤（§4.7 条款 3「先筛后装载」）**：provider 返回后、进 context 之前过滤，
精确匹配或 `.suffix` 匹配。`allowed` 与 `denied` 同时给时以 `denied` 优先。

**SLA 埋点**：per-provider 计数器 + 延迟 + 最近错误，`Router.Stats()` 快照；
可选 `OnEvent` 钩子供 `obslog` 接入。本分支不接 otel。

---

## 4. 免费链

| 序 | Provider | 凭据 | 默认 | 说明 |
|---|---|---|---|---|
| 1 | `ddg-html` | 无 | **开** | 现状保留，goquery 解析 + anomaly 检测 |
| 2 | `ddg-lite` | 无 | **开** | `lite.duckduckgo.com/lite/`，兜底端点 |
| 3 | `searxng` | 无（需端点） | 关 | `?format=json`，自建或公共实例 |
| 4 | `tavily` | 无（keyless） | 关 | `X-Tavily-Access-Mode: keyless`，唯一免 key 且有 SLA |
| — | `generic` | 可选 | `--search-api` 时 | 包裹现有 `internal/searchapi` |

**`--search-api` 语义**：保持**独占覆盖**（设了就只有它，不静默回落到第三方）。
理由：显式配置若悄悄回落 DDG，等于把企业用户的查询外发给第三方。

**为什么 searxng / tavily 默认关**：免 key 不等于零信任成本——两者都会把查询发给
第三方。DDG 是现状所以保留默认开；新增的免 key 后端一律显式开启。

---

## 5. 任务拆分（每个约 2–5 分钟编码，TDD：先 RED 后 GREEN）

| # | 任务 | 文件 | 验收 |
|---|---|---|---|
| 1 | ~~写计划~~ | 本文件 | — |
| 2 | 包骨架 + 错误分类 | `provider.go` `classify.go` | 分类表单测全绿 |
| 3 | DDG provider | `ddg.go` `http.go` | goquery 解析 + 202/200+anomaly → ErrRateLimited |
| 4 | SearXNG + Tavily | `searxng.go` `tavily.go` | httptest 驱动，JSON 解析 + 429 → ErrRateLimited |
| 5 | Router | `router.go` | 冷却跳过、故障转移、限流、域名过滤 |
| 6 | 接线 + 统计 | `internal/tools/web.go`、`agentbuild`、`stats.go` | 既有 web 测试全绿，新增 Router 测试 |
| 7 | 回归 + CI + 文档 | — | `go test ./...`、`make check-deps` 通过 |

---

## 6. 验收

- [x] `go build ./...` 与 `go vet ./internal/...` 干净。
- [x] `go test ./internal/websearch/` 79 个用例全绿（含 `-race`）。
- [x] 新增测试全部用 `httptest` 驱动，**不依赖真实网络**（本地住宅 IP 跑通不构成验证）。
- [x] 「被拦」与「搜不到」在工具层有专门的回归用例
      （`TestWebSearchToolRateLimitIsNeverNoResults`）。
- [x] `ci/deps-personal.txt` 已重生成（+8 行，其中 6 行本分支、2 行 main 既有漂移）。
- [x] 运维文档 `docs/websearch-free-providers.md`：数据中心 IP 下 DDG 大概率失败、
      如何配 searxng/tavily、生产验证清单。

## 7. 执行结果

| # | 任务 | 结果 |
|---|---|---|
| 1 | 写计划 | 本文件 |
| 2 | 包骨架 + 错误分类 | `provider.go` `classify.go` — 14 用例 |
| 3 | DDG provider | `ddg.go` `http.go` — goquery 解析；202 / 200+挑战页 → `ErrRateLimited` |
| 4 | SearXNG + Tavily | `searxng.go` `tavily.go` — Tavily keyless 请求形状已核对 opencode 源码 |
| 5 | Router | `router.go` — 冷却/故障转移/限流/域名过滤/统计，含并发与冷却到期用例 |
| 6 | 接线 + 统计 | `internal/tools/web.go`、`agentbuild/build.go`、`generic.go`；事件落到 zerolog |
| 7 | 回归 + CI + 文档 | 见上 |

**执行中修正的一处设计**：动手前的判断是「DDG 返回 challenge 页时谎报 No results」。
读代码后确认更精确的两条路径——非 200 时 `fetchHTTP` 本来就返回错误（只是报了个没用的
裸 `HTTP 202`），真正说谎的是 **200 + 挑战页** 那条。所以 P0 是两半：
① 200+0 条+标记 → `ErrRateLimited`；② 202/429/503 → `ErrRateLimited`（不是裸状态码）。

**顺带修掉的既有缺陷**：`unwrapDDGURL` 在二次解码失败时原本返回原始 href，
实际应返回一次解码后的 target（该行为有专门的 edge 用例覆盖，已随函数一起迁移）。

**已知未覆盖**：`WOCA_SEARCH_PROVIDERS` 里写错 id 会被静默丢弃。`Config.Validate()`
能报出来，但工具构造时没有 logger 可用，启动告警属后续工作。

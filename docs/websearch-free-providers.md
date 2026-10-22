# WebSearch 后端：免费 provider 链

代码：`internal/websearch/`　调研依据：`docs/websearch-research.md`

## 1. 它解决什么

改造前 `WebSearch` 是「一条正则抓 DuckDuckGo HTML」。三个问题：

1. **说谎。** DuckDuckGo 对不信任的客户端返回 **HTTP 200 + 挑战页**（不是错误码）。
   旧解析器找不到结果锚点，回给模型 `No results found.`
   ——模型据此断定「这个信息不存在」，然后开始编。
2. **没有降级。** 后端只有一个，它挂了就整个工具挂了。
3. **没有后端预算。** 无法回答「这个后端现在可用吗」，也就谈不上 SLA。

## 2. 结构

```
WebSearch 工具壳（不变）
        │
   websearch.Router     冷却表 / 故障转移 / 限流 / 域名过滤 / 统计
        │
   ┌────┼────┬─────────┬────────┐
 ddg-html  ddg-lite  searxng  tavily   (+ generic = --search-api)
```

`Provider` 接口只有四个方法，新增后端约 60 行：

```go
type Provider interface {
	ID() string
	Name() string
	Available() bool
	Search(ctx context.Context, query string, limit int) ([]Result, error)
}
```

## 3. 免费链与配置

| 序 | Provider | 凭据 | 默认 | 条件 |
|---|---|---|---|---|
| 1 | `ddg-html` | 无 | **开** | — |
| 2 | `ddg-lite` | 无 | **开** | — |
| 3 | `searxng` | 无 | 关 | 需 `WOCA_SEARCH_SEARXNG` 端点 |
| 4 | `tavily` | 无（keyless） | 关 | 需 `WOCA_SEARCH_TAVILY=1` |
| — | `generic` | 可选 | `--search-api` 时 | 独占，不回落 |

**为什么 searxng / tavily 默认关**：免 key ≠ 零信任成本，两者都会把查询发给第三方。
DDG 是现状所以保留默认开；新增后端一律显式开启。

```sh
# 企业内网自建实例（唯一完全不外发查询的免 key 方案）
export WOCA_SEARCH_SEARXNG=https://searx.internal.example.com
export WOCA_SEARCH_PROVIDERS=searxng,ddg-html   # 自建优先

# 只在一个域名集合内搜索
export WOCA_SEARCH_ALLOWED_DOMAINS=go.dev,pkg.go.dev
export WOCA_SEARCH_DENIED_DOMAINS=spam.example.net
```

`--search-api <endpoint>` 保持**独占覆盖**：设了就只有它，不静默回落到公共后端。
显式配置若悄悄回落 DDG，等于把企业用户的查询外发给第三方。

## 4. 错误语义（本分支的核心）

| 情形 | 模型看到 | 换 provider？ |
|---|---|---|
| 无可用后端 | `no search backend configured; ...` | 否 |
| HTTP 429 / 503 / **202** | `search backend is rate-limiting this machine; do not retry or rephrase; ...` | **是**，该后端进冷却 |
| HTTP 200 但命中反爬标记且 0 条 | 同上 | **是** |
| 网络错 / 401 / 403 / 其他 5xx | 原始错误 | 否 |
| 正常但 0 条 | **成功**：`No results found for this query.` | 否 |

最后一行是整套设计的支点：**0 条结果是成功，不是失败。**
把它和「被拦」混为一谈，就会让模型把「我没拿到」当成「世界上没有」。

反爬标记是**短语**而不是单词（`anomaly-modal` 而非 `anomaly`）：
搜 "anomaly detection" 的页面正文里本来就带 "anomaly"，用单词会把正常的空结果
误报成限流。

「不重试」是写进错误文案里的（*do not retry or rephrase*）。挑战页重试多少次都
是同一个结果，只会放大限流。

## 5. 调度规则

- **限流**：全局令牌桶，默认最小间隔 1s。免费后端按 IP 信誉判定，突发请求正是它们找的信号。
- **冷却**：被限流的后端默认冷却 1 分钟；后端给的 `Retry-After` 会被采纳，但
  **上限 10 分钟**——否则一个恶意 `Retry-After: 86400` 就能把某个后端永久停掉，
  把所有流量赶到它指定的地方。
- **故障转移只认限流**。网络错、401、非 503 的 5xx 一律直接返回：在任意错误上都换后端，
  会把真实的配置错误伪装成时好时坏的抖动，没人能排查。
- **域名过滤在装载前**。先筛后进 context；顺序反了等于先把外部内容读进来再判断能不能用。

## 6. SLA 现状（照实说）

| Provider | 契约 | 现实 |
|---|---|---|
| `ddg-html` / `ddg-lite` | **无**。HTML 抓取，无公开接口约定，选择器随时可能失效 | 按 IP 信誉拦截；**数据中心 IP 会确定性失败**（HTTP 202 + JS 挑战），换 UA 无效 |
| `searxng` | 无（自建除外） | 公共实例多数禁用 `format=json`（403）；自建的稳定但需自己运维 |
| `tavily` (keyless) | 厂商有可用性承诺，是免 key 里唯一有 SLA 的 | 免 key 额度未公开 |
| `generic` | 取决于你配的端点 | — |

**社区共识：2026 年没有 terms-of-use 清净的免 key SERP。** 上面的免费链是
「能用且诚实」的方案，不是「有 SLA」的方案。要真 SLA 只有两条路：付费 API，
或自建 SearXNG。

### 可观测性

每个 provider 有计数器（attempts / successes / rate_limited / failures /
results / 总延迟 / 最近错误 / 最近成功时间），`Router.Stats()` 可取快照；
每次尝试发一个 `Event`，默认经 `websearch.SetEventHook` 落到 zerolog
（限流/失败/无后端为 Warn，其余 Debug）。

```go
// 可用性 = Successes/Attempts；限流压力 = RateLimited/Attempts
stats := tools.WebSearchRouter().Stats()
```

## 7. 生产验证清单（**必须做，本机跑通不算**）

本机住宅 IP 能拿到 DDG 结果，**不构成验证**。woca 的生产形态是企业 worker / CI /
容器，几乎必然是数据中心 IP。在目标网络里跑一次：

```sh
# 1. DDG 在你自己的部署网络里到底行不行
curl -sS -o /tmp/ddg.html -w '%{http_code}\n' \
  -A 'Mozilla/5.0 (compatible; woca/0.3)' \
  'https://html.duckduckgo.com/html/?q=golang+mcp'
# 200 + 有 result__a 锚点 = 可用
# 202 / 200 但含 anomaly-modal = 被拦，DDG 在这套网络里不可用
```

- [ ] 在企业 worker / CI / 容器里各跑一次上面的 curl，记录真实成功率。
- [ ] 若 DDG 不可用 → 配 `WOCA_SEARCH_SEARXNG` 自建实例，或 `WOCA_SEARCH_TAVILY=1`。
- [ ] 选公共 SearXNG 实例时先确认它启用了 `format=json`（否则只会拿到 403）。
- [ ] 用 `Router.Stats()` 观察一周，确认限流比例符合预期。

## 8. 本分支没做的（下一个分支）

- **WebFetch 改造**：HTML→Markdown、rune 口径阈值、大页落盘。
  实测 12 个常见文档页纯文本中位数 54,949 字节，**50% 超过 50KB**——落盘是主路径不是异常路径。
  另：`maxWebChars = 20000` 名不副实，它是字节数（中文约 6,667 字），且 `out[:20000]` 会切坏汉字。
- **SSRF**：`security.ValidateURL` 只校验 hostname 不解析 IP，DNS rebinding 可绕过；
  `SSRFSafeTransport` 库里有但 `fetchHTTP` 未用。
- **settings 层**：目前只有环境变量，没有 `internal/settings` 键（调研 G8）。
- **`openrouter:web_search`**：它是给 OpenRouter chat completions 请求加
  `plugins:[{"id":"web"}]`，属于 `internal/llm` 层不是工具层；已确认 `internal/` 下
  没有 openrouter 相关代码，本分支不具备接入条件。
- **付费 provider**：Brave / Tavily keyed / Exa / Parallel，按本次范围一律不做。

## 9. 依赖说明

引入 goquery 用于 DDG 的 HTML 解析。它**不是新依赖**：`v1.10.0` 早已在模块图里
（经 `langchain-golang/core/documentloaders` 传入），`go.sum` 有完整 `h1:` 校验和，
源码已在模块缓存，离线可用。我们只是把它从间接依赖提为直接依赖，**版本未升**，
供应链零变化。

`ci/deps-personal.txt` 增加 6 个包：`goquery`、`cascadia`、`x/net/html`、
`x/net/html/atom`、`x/time/rate`、`internal/websearch`。
（另有 `core/stores`、`internal/runexec` 两行是 main 上既有的基线漂移，本次一并吸收。）

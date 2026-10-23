# Web 搜索配置：做成产品内的设置功能

分支 `feat/websearch-settings`（from `main` @ `b541397`）
工作区 `.worktrees/websearch-settings`

## 1. 为什么要做

上一轮（`feat/websearch-free-providers`）把搜索后端做成了 provider 注册表，
但配置只能靠环境变量 + 一份 md 文档。用户原话：

> 我不需要你把 provider 做成这种配置文档一样的配置，而是要你做进个人版的
> 配置页面里，企业版的配置管理台的页面里。我要你把这个配置，做成一个产品里的
> 设置的功能。

按产品取舍后的结论：

- **企业版的价值不是"锁死不让改"**。用户指出「企业也在往外调用大模型」，
  所以「禁止外发搜索查询」不是硬需求。企业版就复用现有 managed overlay
  下发一份默认配置，不为它新造机制。
- **这一轮的范围**：内置几个免 key provider 的**开关 + 拖动排序 + 可加自定义端点**，
  并在数据模型上**留好口子**，以后用户能指定自己的搜索路由。

## 2. 现成的挂靠点（探查结论）

项目里已经有「内置插件列表 + 逐个启停 + 改完热生效」的完整模式，照抄它：

| 能力 | 现成实现 |
|---|---|
| 列表 + 逐个启停 | `PluginsSection.vue` + `PUT /api/plugins` 的 `enabled: string[]` allowlist |
| 改完立刻生效 | `pluginsapi.go` 保存后 `s.agent.RefreshToolsNow(ctx)` |
| 每轮重新求值 | `agentbuild/build.go:285` 的 `RefreshTools` 闭包读 `st.Load()` |
| 排序 | 数据模型天然有序（`[]string`），但**全仓库没有排序 UI**，需要新写 |
| 未知名校验 | `pluginsapi.go:74-83` 拒绝未知名，「拼写错误不能悄悄藏掉一个插件」 |

参照物：`internal/server/pluginsapi.go`（100 行，最短的完整样例）。

## 3. 数据模型

`internal/settings/settings.go` 新增字段：

```go
// SearchProviders is the ordered list of enabled web-search backends.
// Empty means "use the built-in default chain".
SearchProviders []string `json:"search_providers"`
// SearchSearXNGEndpoint is a self-hosted SearXNG instance base URL.
SearchSearXNGEndpoint string `json:"search_searxng_endpoint"`
// SearchCustomEndpoint is a user-supplied JSON search API (the former
// --search-api). This is the extension point: today one endpoint, later a
// named list of user-defined routes.
SearchCustomEndpoint string `json:"search_custom_endpoint"`
SearchAllowedDomains []string `json:"search_allowed_domains"`
SearchDeniedDomains  []string `json:"search_denied_domains"`
```

`search_custom_key` 走 secret（`settings.Secret*` + `SecretFor`），不进明文表。

**为什么不用 bool 表达开关**：开关统一成「是否在启用列表里」。这样企业下发一个
非空列表就锁定了集合，**绕开 managed overlay 的零值坑**（`false` 和空列表都等于
未托管，无法锁定）。这是本轮能不建表、走 settings 路线的关键。

### 环境变量的处置

**全部移除**（用户要求）：`WOCA_SEARCH_PROVIDERS` / `WOCA_SEARCH_SEARXNG` /
`WOCA_SEARCH_TAVILY` / `WOCA_SEARCH_ALLOWED_DOMAINS` / `WOCA_SEARCH_DENIED_DOMAINS`。
配置只有一个来源 = settings。

**保留 `--search-api` / `--search-api-key` 两个 CLI flag**：它们不在环境变量里，
且是命令行上明写的一次性意图，语义和藏在环境里的变量不同。它们填充的是同一个
`search_custom_endpoint` / `search_custom_key`，本次运行覆盖设置值。

### 分层约定

`internal/websearch` **不 import `internal/settings`**——它是底层库，`Config`
保持纯净（只有字段）。转换发生在调用点（agentbuild、server），各约 6 行。

## 4. 任务拆分

### 阶段一：后端（无 UI，可独立测试）

| # | 任务 | 文件 |
|---|---|---|
| 1.1 | `Settings` 加 5 个字段 + `applyPairs` + `Save` | `internal/settings/settings.go` |
| 1.2 | 加 `Key*` 常量 + `KeyNames`/`KeyHelp`/`Apply`/`KeyValue` + secret 常量与 `SecretFor` | `internal/settings/keys.go` |
| 1.3 | `Config` 去掉 `LoadConfig()`（env），保留纯字段 + `Build()`；provider id 白名单校验 | `internal/websearch/config.go` |
| 1.4 | router 从「启动时全局单例」改为可重建：`SetWebSearchConfig(cfg)` | `internal/tools/web.go` |
| 1.5 | agentbuild 启动时注入配置；`RefreshTools` 里每轮重读 | `internal/agentbuild/build.go` |
| 1.6 | managed overlay 支持下发/锁定（`applyOverlay`/`lockedFields`/`checkWritable`） | `internal/settings/managed.go` |

### 阶段二：配置 API

| # | 任务 | 文件 |
|---|---|---|
| 2.1 | `GET /api/websearch`：provider 目录（id/名称/说明/是否需要端点/启用/顺序）+ 当前配置 | `internal/server/websearchapi.go`（新） |
| 2.2 | `PUT /api/websearch`：整体覆盖，拒绝未知 provider，保存后 `RefreshToolsNow` | 同上 |
| 2.3 | `POST /api/websearch/test`：跑一次真实搜索，返回服役 provider / 条数 / 耗时 / 首条标题 | 同上 |
| 2.4 | 路由注册 + 鉴权 scope | `internal/server/server.go` |

### 阶段三：个人版 UI（`web/`，Vue 3）

| # | 任务 | 文件 |
|---|---|---|
| 3.1 | `AppSettings` / `AppSettingsUpdate` 类型 | `web/src/types.ts` |
| 3.2 | `getWebSearch` / `putWebSearch` / `testWebSearch` | `web/src/api/` |
| 3.3 | 新 section：provider 卡片列表（**原生 HTML5 拖动**排序 + 开关）+ SearXNG 端点 + 自定义端点 + 域名名单 + 测试按钮 | `web/src/components/settings/WebSearchSection.vue`（新） |
| 3.4 | 导航项 + `v-if` | `web/src/components/SettingsPanel.vue` |
| 3.5 | i18n（中/英） | `web/src/i18n/locales/*/settings.ts` |

### 阶段四：企业版配置台（`ee/configsvc-web` + configsvc）

| # | 任务 | 文件 |
|---|---|---|
| 4.1 | console API 暴露搜索配置 | `internal/ee/configsvc/payload.go`、`handlers_settings.go` |
| 4.2 | 控制台侧的测试搜索端点 | `internal/ee/configsvc/handlers_websearch.go`（新） |
| 4.3 | 新 section（同个人版形态 + 锁定态置灰） | `ee/configsvc-web/src/components/WebSearchSection.vue`（新） |
| 4.4 | nav + types + i18n（中/英） | `ee/configsvc-web/src/` |

### 阶段五：清理

| # | 任务 |
|---|---|
| 5.1 | 删 `docs/websearch-free-providers.md` |
| 5.2 | 「数据中心 IP 会被 DuckDuckGo 拦」这条信息搬进 UI 帮助文案（不能随文档消失） |
| 5.3 | `README.md` / `README.zh-CN.md` 里 `internal/websearch` 的描述同步 |
| 5.4 | `ci/deps-personal.txt` 复核 |

## 5. 明确不做

- **不照 sandbox 建表**（路线 B，17 个文件）。开关统一成列表后零值坑消失，settings 路线够用。
- **不引拖拽库**（项目现在零 sortable 依赖）。用原生 HTML5 `draggable` + `dragover`。
- **不做 per-provider 高级参数**（超时、冷却）。全用默认值。
- **不做独立的连通性探针**（sandbox 那种 `/verify`）。「测试搜索」一个按钮覆盖。
- **不做审批流、审计面板**（现有配置台本来也没有）。
- **不做用量统计面板**。SLA 数据留在 `Router.Stats()` 和日志里。

## 6. 验收

- [x] 个人版设置页能开关、拖动排序、填端点，保存后**下一轮对话立刻生效**（不用重启）。
- [x] 「测试搜索」按钮能显示哪条通路在服役、几条结果、耗时。
- [x] 企业版配置台能下发同样的配置，客户端被锁定字段置灰。
- [x] 环境变量全部失效（grep 无残留；另有测试断言 env 不会渗进链路）。
- [x] `docs/websearch-free-providers.md` 已删，帮助文案里有「数据中心 IP 会被拦」的提示。
- [x] `go build ./...`、`go vet ./...`、`make check-deps` 通过；新增后端逻辑有单测。
- [x] 前端构建通过；个人版 464 个测试、配置台 15 个测试全绿。

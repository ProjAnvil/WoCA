# complab — 组件集成测试

`e2e/` 下的第三个套件。定位和另两个不一样：

| 套件 | 关注点 | 需要 docker |
|---|---|---|
| `netlab` | 网络层区域隔离（前端 → relay/edge → 隧道 → 内部 bridge） | 是 |
| `sandboxlab` | 沙箱执行链（注册 backend → 创建 run → worker 执行 → 回传产物） | 是 |
| **`complab`** | **进程内组件是否真的能干活，以及组件之间串起来是否可用** | 否 |

```sh
make complab           # 全部组件（含需要外网的）
make complab-offline   # 只跑不需要外网的
```

build tag `complab` 保证普通 `go test ./...` 不会碰网络。

## 覆盖的组件

| 测试 | 组件 | 外网 |
|---|---|---|
| `TestToolRegistryExposesCoreTools` | 工具注册表（agent 实际拿到的工具集） | 否 |
| `TestFileToolsRoundTrip` | Write → Read → Edit → Grep → Glob → ListDir | 否 |
| `TestBashToolRunsCommand` | Bash（经默认 executor） | 否 |
| `TestSettingsStoreRoundTrip` | 设置库：开库 / 明文写读 / **加密 secret 往返** / 重开持久化 | 否 |
| `TestWebSearchReturnsResults` | **WebSearch：调用后真的拿到结果** | 是 |
| `TestWebSearchSurvivesABlockedProvider` | 降级链路（链首后端被拦 → 换后端照样出结果） | 是 |
| `TestWebFetchReturnsText` | WebFetch 抓正文 | 是 |
| `TestWebSearchThenFetch` | 组合：搜索结果里的 URL 真的能抓 | 是 |

## 两条设计取舍

### 1. 不断言是哪条通路在服役

`TestWebSearchReturnsResults` 只要求「有结果」，不要求「是 ddg-html 出的」。

免费后端的可用性随网络环境变化（DuckDuckGo 按 IP 信誉拦截，数据中心 IP 会
确定性失败），写死 provider 会让这个测试在任何一次后端切换后误报。服役的
provider 只作为 `t.Logf` 输出，用于失败时诊断。

`TestWebSearchSurvivesABlockedProvider` 就是把「不用管具体通路」做成断言：
链首放一个永远报 `ErrRateLimited` 的假后端，要求请求照样出结果，并且假后端
只被调用 1 次（挑战页不重试）。

### 2. 没有外网 → skip；有外网但后端全被拦 → FAIL

这两种情况的结论完全不同，所以处置也不同：

- **没有外网**：环境问题，不是组件坏了。skip，并说明是 dial 失败。
- **有外网但所有免费后端都被拒**：这是真发现。说明这套网络（CI / 企业 worker /
  容器的典型表现）用不了默认链路，必须配自建 SearXNG 或 Tavily keyless。
  此时 **FAIL**，错误信息里直接给出要设的环境变量。

这个区分是刻意的：如果两种都 skip，那「默认后端在你这套网络里根本不可用」
这件事就永远不会被发现——而那正是这套搜索改造要解决的核心问题。

### 3. 结果内容不做断言

查询词固定为 `golang programming language`，但只断言「结果条数 > 0」和
「能解析出 URL」，不断言具体是哪个网站。搜索结果会变，断言内容只会养出
一个脆弱的测试。

## 加新组件

在对应文件里加一个 `Test*`，需要外网的在开头调 `requireNetwork(t)`。
helper 都在 `harness_test.go`：

- `newRegistry(t)` / `toolNamed(t, reg, name)` / `invoke(t, tool, args)` —— 走生产注册表
- `installEventLog(t)` —— 采集 router 事件，失败时给出事件轨迹
- `resultURLs` / `countResults` —— 解析工具输出

注意：设置库会把主密钥写到 `WOCA_HOME`，测试里必须重定向到 `t.TempDir()`，
否则会往用户家目录写东西。

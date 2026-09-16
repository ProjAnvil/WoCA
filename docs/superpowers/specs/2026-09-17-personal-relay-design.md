# 个人版 Remote Relay 设计（woca-relay)

- 日期： 2026-09-17
- 状态： 已通过三轮审计（代码库、外部论证、spec 审计）,v2 修订完成，待实现
- 范围： 仅个人版（`woca` 二进制 + 新 `woca-relay` 二进制）。企业版 `internal/ee/` 零改动、零 import。

## 1. 背景与问题

个人版 `woca` 二进制托管完整 web SPA + API，默认绑 `127.0.0.1:3484`，本地信任
（`authn.NewLocal()` 无条件放行，internal/platform/authn/authn.go:82-84)。它
没有任何远程访问能力：机器在 NAT 后，网页端 / native 客户端 / 未来手机端都
无法从公网触达。

企业版已有同构能力（scenario-four:`woca-edge` 公网接入点 + bridge 出站长轮询
隧道），但它依赖 woca-server / woca-config / Postgres 整套私有核心，且代码在
`internal/ee/`,个人版不可 import(CI `check-editions` 强制）。

**目标**：给个人版一个轻量远程控制通道——一台公网 VPS 跑单个 `woca-relay`
二进制，NAT 后的 `woca`（作为 bridge）出站连它，任意地方的浏览器 / native
客户端用一把密钥连上 relay，即可完整操控 bridge。

**非目标（YAGNI)**:

- 不动企业版任何代码与协议。
- 不做 P2P 打洞（WebRTC/ICE)、不做 UDP。
- 不做配置下发、多租户、审计中心、enrollment 审批流（企业版能力）。
- 不做 E2E 加密（列为 v2 演进方向，见 §11)。
- 不做手机端 App(native client 的 relay profile 为未来手机端铺路）。

## 2. 关键决策记录

| 决策 | 结论 | 理由 |
|---|---|---|
| 传输协议 | **HTTP 长轮询 + 分块 POST 回传**（非 WebSocket) | 对齐 Anthropic Claude Code Remote Control（出站 HTTPS 轮询 + 流式路由）与 woca 企业版既定姿态；零新依赖（根模块无任何 WS 库，`ci/deps-personal.txt` 门禁，全文 229 行已核对）;SSE 有企业版 `streamRecorder` 的已验证**语义**可照抄（代码在 `internal/ee/bridge`，不可 import，必须同语义重写，CI 强制） |
| 密钥模型 | **对称共享密钥（rendezvous key)，不分 bridge/client 类型** | 用户明确决策。relay CLI 颁发，同一把 key 配进 bridge、交给用户；双方各自连 relay,relay 按 key 配对中转 |
| key ↔ bridge 基数 | **1:1，带代际租约**（一把 key 同一时刻只服务一个 bridge 代际；代际有过期驱逐） | 防静默劫持 + 防崩溃后永久占坑（见 §6.2） |
| 认证唯一防线 | relay 侧会话校验是唯一认证（见 §7 威胁模型） | 个人版 handler 无条件信任一切请求，这不是纵深防御，是唯一防线 |
| key 前缀 | `wcr_` | `woca_` 是企业版管理词汇，避免支持与日志取证歧义 |
| 吊销错误码 | **`revoked`**（不是 `key_revoked`) | 前端词汇表只认 `revoked`:WocaApp.vue:137、transport.ts:48-50、streaming.ts:151-157，后端 `authn.CodeRevoked`(authn.go:26)。用错码 SPA 永不显示吊销提示，核心产品行为静默失效 |

## 3. 组件

### 3.1 `woca-relay`（新二进制）

- 位置： `cmd/woca-relay` + `internal/relay`。归属个人版树，不得 import
  `internal/ee/`（CI 强制）。
- 职责： 公网唯一入口。
  - client 面： HTTPS 服务（托管现有 SPA + 登录 API + 转发 API/SSE 到 tunnel)。
  - bridge 面： tunnel 端点（长轮询取请求 + 回传响应）。
  - key 权威： 内置 sqlite，存 key 哈希与会话。
  - CLI: key 管理（见 §4.3)。
- 部署： 单二进制 + 一个 sqlite 文件。TLS 必配（§7.2)，或经
  `--behind-proxy` 前置 Caddy / CF Tunnel / Tailscale Funnel（见 §7.3)。

### 3.2 bridge 隧道客户端（`internal/relayclient`，编进现有 `woca`)

- 出站长轮询循环（默认 4 个 worker)，断线指数退避 + 抖动重连。
- 取到的请求交给**进程内** `Server.Handler()`（internal/server/server.go:342)
  执行——与本地 web 完全同一代码路径，不走 TCP loopback。响应经
  streamRecorder **同语义重写**的分块写出回传（§6.3)。
- 本地状态面： 新增本地 API `/api/relay/status`(relayclient 连接状态、
  key 前缀、代际、最近错误如"已吊销")，供 `woca relay status` CLI 与本地
  web 状态页读取。**"零前端改动"承诺仅限 relay 托管的 SPA**；本地 woca 的
  web 增加 relay 状态展示属允许范围。
- 配置与 CLI:
  - `woca relay connect --url https://relay.example.com --key wcr_…`（写入
    settings 持久化，`serve` 时自动拉起；key 走 settings 既有 secrets 静态
    加密机制，internal/settings 的 SetSecret，不明文落盘）。
  - `woca relay status` / `woca relay disconnect`。
  - env 等价物： `WOCA_RELAY_URL` / `WOCA_RELAY_KEY`（容器首启 / 无头部署）。

### 3.3 client(web SPA / native client)

- **web SPA: 零前端改动**。SPA 已有登录门： `POST /api/auth/login`
  body `{api_key}`(web/src/api/bridge.ts:17)；配套 `/api/auth/logout`
  (bridge.ts:21)、`/api/auth/me`(bridge.ts:144-146)；所有 API 走相对路径
  (transport.ts:9 `streamBase = ''`)。SPA 实际只用 cookie 会话
  (LoginGate.vue:55 注释明确 key 不被存储，login 响应里的 token 字段被
  忽略）。relay 挂载这三个路由 + SPA 静态资源（复用 `webfx.SPA`,
  internal/webfx/spa.go:15——注意它与个人版 staticHandler 语义不同：
  `/api/` 未命中返回 404 而非回落 index.html，这正是 relay 想要的行为）。
- `/api/auth/me` 决策： **挂载**并返回 SPA 期望的 Me 形状
  `{user_id, remark, role}`(bridge.ts:138-142),user_id 用 key 的稳定
  派生（哈希前 8 位）,remark 用 key 的 name,role 固定 `admin`。
- **native client**: profile 系统是数据驱动（`configstore.Profile` 仅
  {ID, BaseURL, 时间戳},native-client/internal/configstore/store.go:23-28)。
  新增"relay profile"= 一条 URL 数据 + 一个静态 Bearer `CredentialProvider`
  实现（接口存在于 native-client/internal/transport/handler.go:23);传输层
  (`FlushInterval: -1`,SSE 安全，handler.go:165）不改动。
  - **Bearer 契约**: relay 的 login 响应在 SetCookie 的**同时** body 返回
    `{"token": "<opaque 会话 token>", "expires_at": …}`（对齐企业版先例
    internal/ee/edgegw/auth.go:254-259)。native 存此 token 做 Bearer——
    这是必需的，因为 native 的直连 SSE 绕开资产代理、需要显式 Bearer
    (transport.ts:27-35 setStreamAuth;NativeGate.vue:48)。logout 同时接受
    cookie 或 Bearer（对齐 edgegw/auth.go:268-276)。native 会话与 web 会话
    同一张 sessions 表，同样 7d 滑动 + 30d 绝对，吊销 key 即全删。

## 4. 密钥生命周期

### 4.1 格式与生成

- 格式： `wcr_` + 32 字节 CSPRNG(crypto/rand）的 base64url(256 位熵）。
- 存储： relay 只存 `SHA-256(key)`；高熵随机 token 用 SHA-256 是正确构造
  (bcrypt/argon2 是密码场景，此处反而引入 DoS 面）。按哈希值查库，天然规避
  时序比较问题。
- 展示： 完整 key 仅 `create` 时输出一次；`list` 显示 name + 前 8 字符
  （其中第 5–8 字符是 24 bit 密钥材料——仅暴露 24/256 bit，不影响剩余
  232 bit 熵，属可接受的信息泄露）+ 元数据。任何日志不得出现完整 key。

### 4.2 配对时序

```
relay:  woca-relay key create home-mac  → 输出 wcr_xxx(仅一次)
                  │ 同一把 wcr_xxx 分两处配置
    ┌─────────────┴──────────────┐
    ▼                            ▼
bridge: woca relay connect      浏览器/native: 打开 relay URL,
        --url … --key wcr_xxx   登录页输入 wcr_xxx
    │                            │
    ▼                            ▼
GET /tunnel/poll (Bearer key    POST /api/auth/login {api_key}
  + X-Woca-Relay-Generation)    → relay 校验哈希,签发会话
→ relay 校验哈希,登记 bridge      (cookie + body token)
    │                            │
    └──── relay 按 key 配对 ─────┘
    client 请求 → 会话解析出 key_id → 停放进该 key 队列
    → bridge poll 取走 → 进程内执行
    → POST /tunnel/respond/{id} 分块回传 → relay 逐块流给 client
```

### 4.3 CLI

```sh
woca-relay key create <name>    # stdout 输出完整 key(仅一次);name 唯一
woca-relay key list             # name / 前缀 / 创建时间 / 最后使用 / 状态 / bridge 在线
woca-relay key revoke <name>    # 立即吊销(见 §5)
```

### 4.4 会话(client 侧）

- 登录成功 → opaque 会话 token(128 位 CSPRNG):
  - web: cookie `woca_relay_session`,`HttpOnly; SameSite=Lax`;
    `Secure` 的判定见 §7.3 代理信任模型。
  - native: body `{"token","expires_at"}`(见 §3.3)。
- 双重过期： **7 天滑动(idle)+ 30 天绝对上限**（对齐 OWASP：滑动过期必须有
  绝对封顶，否则被盗凭证可永久续命）。sqlite 中**会话 token 只存 SHA-256
  哈希**。登出 = 服务端删除会话（仅清 cookie 不算数）。
- `POST /api/auth/login` 限速： 每 IP 每分钟 10 次，**stdlib 手写有界令牌桶**
  （`golang.org/x/time/rate` 不在 `ci/deps-personal.txt` 基线，引入会触发
  check-deps 失败）;per-IP 状态表需有界 + 惰性驱逐（防内存 DoS);IP 来源的
  信任规则见 §7.3。统一错误文案 + 失败日志。

## 5. 吊销语义（核心产品行为）

`woca-relay key revoke <name>` 触发，relay 主动、即时地通知**两侧**:

1. **bridge 侧**: relay 按 key 跟踪在途长轮询连接，吊销时立即取消它们——
   **进行中的 poll 立刻返回** `410 {"code":"revoked"}`（不等 25s wait 到期）;
   之后的 poll 同样 410。bridge 的 relayclient 收到 401/410 `revoked` 后
   **终止重连**（终态，非退避重试）,`/api/relay/status` 标记"key 已吊销",
   需运维重新配 key。
2. **client 侧**: 删除该 key 派生的全部会话（cookie 与 native Bearer 同表）;
   进行中的 SSE 流注入错误事件后关闭；之后所有请求 →
   `401 {"code":"revoked"}`，前端吊销提示（WocaApp.vue:137 的既有路径）。
3. 停放进队列但未取走的请求： 全部以错误释放。
4. 吊销事件写 relay 本地审计日志（连同登录失败、bridge 上下线）。

## 6. 隧道协议（bridge ↔ relay，长轮询形态）

与企业版 scenario-four 同构，独立实现（不 import `internal/ee/`)。

### 6.1 端点

| 端点 | 方向 | 说明 |
|---|---|---|
| `GET /tunnel/poll?wait=25s` | bridge → relay | Bearer key 认证 + `X-Woca-Relay-Generation: <代际 token>` 头。长轮询取一个停放请求；25s 无请求返回 204。key 未知 → 401;key 已吊销 → 410 `{"code":"revoked"}`；同 key 另一代际在线 → 409（见 §6.2) |
| `POST /tunnel/respond/{id}` | bridge → relay | Bearer key + 代际头。回传响应：`X-Woca-Relay-Status` + `X-Woca-Relay-Headers`(base64 JSON)+ 分块流式 body。**relay 必须校验 {id} 属于该 Bearer key 当前代际的停放请求，否则 404**（跨 key / 跨代际伪造 respond 不可行） |

### 6.2 停放、路由与代际租约

- **代际（generation)**: bridge 每次（重）建立隧道时本地生成随机代际
  token(128 位）,poll/respond 全程携带。relay 记录每 key 的活跃代际 +
  最后 poll 时间。
  - 同 key 新代际到达且旧代际仍"在线"（最后 poll < 45s)→ 409，新代际
    退避重试；旧代际超过 45s 无任何 poll（崩溃、半开连接）→ 视为死亡，
    新代际接管。**先到先占，但坑位会过期释放**。
  - 代际变化后，旧代际的 respond 一律 404（防僵尸连接回写）。
- **bridge 在线判定**: 最后 poll 时间 < 45s（宽松于 25s wait + 网络余量）。
  离线 → client 请求立即 `503 {"code":"bridge_offline","retry_after":5}`。
- **停放队列**: 每 key 上限 64 个；满 → 503。client 断开 → 停放项标记作废，
  bridge 侧 respond 时收到 410 Gone 并以 CloseWithError 同语义解除 handler
  阻塞（goroutine 不泄漏）。

### 6.3 响应回传与 SSE 保真（最易错的点，审计修正）

- bridge 侧以 `streamRecorder` **同语义重写**封装 `http.ResponseWriter`:
  **状态 + headers 在第一次 Write/WriteHeader 时冻结提交，Flush() 是 no-op
  边界提示**。原因：账本 SSE 路径在 probe flush 之后才设
  `Content-Type: text/event-stream`(internal/server/runs.go:704-711),
  按 flush 提交会丢 Content-Type,SPA 的 onopen 检查会拒收流。快照时丢弃
  Content-Length（对齐 tunnel.go:315-326)。
- 回传 body 每产生一块立即 `POST /tunnel/respond` 流式写出；relay 收到每一块
  **立即 flush 给浏览器**（禁止聚合/缓冲，HTTP/1.1 chunked 或 h2)。
- **逐跳头过滤**: relay 从 `X-Woca-Relay-Headers` 恢复响应头时，必须剔除
  `Connection`、`Keep-Alive`、`Transfer-Encoding`、`TE`、`Trailer`、`Upgrade`
  ——woca 的 SSE 路径会设 `Connection: keep-alive`(sse.go:30、runs.go:711),
  原样写给浏览器是协议违规。其余头透传；relay 对 event-stream 响应自设
  `X-Accel-Buffering: no`。
- **SSE 心跳（必做，非可选）**: 已核实现有 woca SSE **无任何心跳**（进程内
  路径 sse.go:71-94 仅在事件到达时写；账本路径 runs.go:744-747 idle 分支纯
  sleep;全库无 `: ping` 注释输出）。没有心跳，空闲 SSE 会死在 relay 前端
  代理 / 中间盒的 60–100s idle 窗口。注入点： relayclient 在 recorder 的
  pipe 写路径上，对 `text/event-stream` 响应对包一层心跳 writer——数据静默
  20s 即写入单行 `: ping\n\n`。帧安全： SSE 帧都是单次 Write(sse.go:84、
  runs.go:725),io.Pipe 串行化写调用，独立单行写不会撕裂帧；心跳 goroutine
  需容忍 pipe 阻塞并在 ctx 取消时退出。注入放在 bridge 侧的另一收益：
  同时保活 bridge→relay 的 respond 上传连接。
- **请求体上限**: client → bridge 请求 body 上限 4 MB（对齐企业版
  TestClientBodyLimit 先例）；超限 → 413。流式上传不做（YAGNI)。

### 6.4 头部卫生

- **relay 停放前**: 剥离 client 的 `Cookie` 与 `Authorization`(relay 自己的
  会话凭证对 bridge 无意义，不过隧道）。
- **bridge 执行前**: 剥离所有 `X-Woca-` 前缀头（超集覆盖 `X-Woca-Relay-*`,
  对齐企业版先例 tunnel.go:218-220，防伪造）。
- **bridge 执行时注入**: `X-Woca-Relay-Remote: 1`（在剥离之后由本进程注入，
  relay/client 均不可伪造）供本地审计标记远程来源；`RemoteAddr` 取 relay
  透传的真实 client IP，用于日志/审计。

### 6.5 断线与重连

- poll 失败（网络抖动）: 指数退避（1s 起，封顶 60s)+ 20% 抖动，无限重试；
  每次重连生成新代际（§6.2 的 45s 规则保证旧代际自动让位）。
- 收到 401/410 `revoked` / key 未知： 终态，停止重连。
- relay 重启：
  - 停放队列（内存）丢失 → 未完成的 client 请求 504。
  - **进行中的 respond POST 失败** → bridge 侧以 CloseWithError 同语义解除
    handler 阻塞（对齐 tunnel.go:263-266)。
  - **已建立的 SSE 到 client 直接断开** → 依赖 SPA 既有的 Last-Event-ID
    断点续传/重同步路径（sse.go:45-59),relay 无需补偿。
  - client 会话在 sqlite，不受影响。

## 7. 安全模型

### 7.1 威胁模型（必须显式陈述）

- **认证真空**: 个人版 handler 栈无条件信任一切请求
  (authn.NewLocal,authn.go:82-84)。relay 的会话校验 + "隧道是远程流量唯一
  入口" + 信令头剥离是**唯一防线**。任何让未认证请求抵达 handler 的 bug
  等价于把 shell 交给互联网。
- **对称 key 的代价**: 一把 key 同时能开 client 会话和声明 bridge 身份。
  key 泄露 = 全部信任泄露： 攻击者既能操控 bridge，也能抢注 bridge 身份。
  缓解： 代际先到先占（§6.2)、吊销即时双侧生效（§5)、key 只存哈希。
- **relay 能看明文**: relay 终止 TLS 并转发明文（非 E2E)。relay 被攻破的
  最坏结果 = 转发中的流量可见 + key 哈希库（哈希不可逆，无静态秘密可偷
  完整 key)。E2E 加密列为 v2(§11)。
- bridge key 不出设备；不明文写配置文件（settings secrets 静态加密）、
  不进日志。

### 7.2 红线（对齐企业版 R1/R6 精神）

- 公网绑定 + 无 TLS → **拒绝启动**(fail-closed);`--insecure` 仅 dev,
  且必须显式传参。
- 匿名可及的路由仅限： SPA 静态资源、`/api/health`、`/api/auth/login`。
  其余全路由要会话（client 面）或 Bearer key + 代际（tunnel 面）。

### 7.3 代理信任模型

- 默认（直连 TLS): **不采信**任何 `X-Forwarded-*` 头（client 可伪造
  `X-Forwarded-Proto: http` 骗 relay 下发不带 Secure 的 cookie，造成降级
  窃取）。Secure 恒真，限速按 TCP RemoteAddr。
- `--behind-proxy`（显式开启，用于 Caddy/CF Tunnel 前置）: 才采信
  `X-Forwarded-Proto`（判定 Secure）与 `X-Forwarded-For`（限速取真实
  IP)。此模式下 relay 应只绑内网/loopback，文档标注。

## 8. relay 存储

sqlite(直接用 `modernc.org/sqlite`，**不 import `internal/wocadb`**——它会
blank-import pgx(internal/wocadb/db.go:15)，违背个人版依赖卫生）。迁移采用
版本化 `schema_migrations` 模式（模板照抄：
native-client/internal/configstore/store.go:36-41，注意它是独立 Go module,
只抄模式不 import)。

```sql
CREATE TABLE keys (
  id          INTEGER PRIMARY KEY,
  name        TEXT NOT NULL UNIQUE,
  key_hash    BLOB NOT NULL UNIQUE,   -- SHA-256
  prefix      TEXT NOT NULL,          -- 前 8 字符,展示用
  created_at  TIMESTAMP NOT NULL,
  revoked_at  TIMESTAMP,              -- NULL = 有效
  last_used_at TIMESTAMP
);
CREATE TABLE sessions (
  token_hash  BLOB PRIMARY KEY,       -- SHA-256
  key_id      INTEGER NOT NULL REFERENCES keys(id),
  created_at  TIMESTAMP NOT NULL,     -- 绝对过期 = created_at + 30d
  touched_at  TIMESTAMP NOT NULL      -- 滑动过期 = touched_at + 7d
);
```

relay 零业务数据： runs、对话、文件全部留在 bridge。

## 9. 错误矩阵

| 场景 | client 看到 | bridge 行为 |
|---|---|---|
| bridge 离线（最后 poll ≥ 45s) | 503 `bridge_offline` + retry_after | — |
| key 已吊销（client) | 401 `{"code":"revoked"}` → 吊销提示 | — |
| key 已吊销（bridge) | （同上，bridge 已下线） | 在途 poll 立即 410，终态停止重连，状态标记 |
| 同 key 另一代际在线 | — | 409，退避重试，旧代际 45s 后自然让位 |
| respond 跨 key/跨代际/未知 id | — | 404 |
| 停放中 relay 重启 | 504 | poll 自动重连（新代际） |
| 进行中 respond 因 relay 重启失败 | SSE 断开，SPA Last-Event-ID 重同步 | CloseWithError 解除 handler 阻塞 |
| 请求 body > 4 MB | 413 | — |
| 停放队列满（64/key) | 503 | — |
| 未登录访问 API | 401 → 登录页 | — |

## 10. 测试策略（镜像企业版测试范式）

参照 internal/ee/edgegw/edgegw_test.go 与 internal/ee/bridge/bridge_test.go
的 in-process 组装（`httptest.NewServer` + 假对端），复刻：

- 单元： keystore（创建/吊销/哈希查找）、会话（滑动+绝对过期）、停放队列
  （满/超时/取消）、代际租约（409 / 45s 驱逐 / 僵尸 respond 404)、
  fail-closed bind 检查（对齐 TestCheckBindFailClosed)、限速器有界性。
- 集成（全链路，进程内 relay + bridge handler + client):
  - 基本 round-trip(GET/POST，对齐 TestTunnelRoundTrip)
  - **SSE 逐帧透传**(对齐 TestTunnelSSE): Content-Type 正确（账本路径的
    flush-before-headers 怪序不丢头）、分块即时性、20s 静默后 `: ping`
    心跳到达、逐跳头被过滤
  - 认证： 错误 key / 无会话 / 匿名路由白名单（对齐 TestTunnelAuth)
  - 吊销： 双侧即时通知（在途 poll 立刻 410 + 会话删除 + SSE 关闭，
    对齐 TestRevocationAndGrace)+ 错误码确为 `revoked`（前端契约）
  - body 上限 413(对齐 TestClientBodyLimit)
  - respond 的 key/代际隔离（对齐 TestRespondIsBridgeScoped)
  - 同 key 第二代际 409 → 旧代际过期后接管
  - native 契约： login 响应带 body token,Bearer 可用，logout 双通道
- e2e: `make` 目标起 relay + `woca`(relayclient)+ curl 走完
  create key → connect → login → 发起 run → SSE → revoke → 双侧错误。

## 11. v2 演进方向（本次不做，记录在案）

- **E2E 加密**: 对称 key 派生加密密钥,client 与 bridge 端到端加密,relay
  转密文（对齐 Happy Coder 的"relay 全盲"模型）。
- **传输升级**: 若未来需要亚秒级请求下达，可把 poll/respond 换成单条 WSS
  多路复用（外部审计已给出修正清单： 连接级内存上限、RequestData/Reset 帧、
  25s ping/pong、close code 4001 终态语义）。
- 手机端 App(native client 的 relay profile 已是其传输前体）。
- 多 bridge 选择页（当前 1 key = 1 bridge 代际，多机器 = 多 key)。

## 12. 构建与 CI

- `cmd/woca-relay` 加入 Makefile 构建产物清单；**不加入 SERVER_ARTIFACTS**
  (Makefile:271)，避免被 dist 打包并被 check-server-artifacts 的
  TUI/frontend 门禁误伤。
- `check-editions`(Makefile:365-378): `./internal/...` 已在扫描范围，
  `internal/relay`、`internal/relayclient` 自动被"不得 import internal/ee"
  覆盖；**只需把 `./cmd/woca-relay` 加入扫描路径**。
- `check-deps`(Makefile:109-112): 现仅覆盖 `./cmd/woca`。为
  `cmd/woca-relay` 新建独立基线文件（如 `ci/deps-relay.txt`，预期仅 stdlib
  + modernc.org/sqlite + zerolog + 既有 internal 包，**零新增第三方库**),
  并扩展 Makefile 目标。
- `internal/relayclient` 进入 `./cmd/woca` 依赖闭包： 预期 `ci/deps-personal.txt`
  第三方依赖零新增（协议层全 stdlib： net/http、io.Pipe、crypto/sha256、
  encoding/base64/json；日志走 internal/logging，密钥走 internal/settings
  secrets)，跑 `make deps-baseline` 确认 import 图变化后提交。
- 限速器 stdlib 手写（`golang.org/x/time/rate` 不在基线）。

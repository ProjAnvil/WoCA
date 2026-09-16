# 个人版 Remote Relay 设计（woca-relay)

- 日期： 2026-09-17
- 状态： 已通过双审计（代码库 + 外部论证）,待实现
- 范围： 仅个人版（`woca` 二进制 + 新 `woca-relay` 二进制）。企业版 `internal/ee/` 零改动。

## 1. 背景与问题

个人版 `woca` 二进制托管完整 web SPA + API，默认绑 `127.0.0.1:3484`，本地信任
（`authn.NewLocal()` 无条件放行，internal/platform/authn/authn.go:82)。它没有
任何远程访问能力：机器在 NAT 后，网页端 / native 客户端 / 未来手机端都无法从
公网触达。

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
| 传输协议 | **HTTP 长轮询 + 分块 POST 回传**（非 WebSocket) | 对齐 Anthropic Claude Code Remote Control（出站 HTTPS 轮询 + 流式路由）与 woca 企业版既定姿态；零新依赖（根模块无任何 WS 库，`ci/deps-personal.txt` 门禁）;SSE 已有 `streamRecorder` 等已测试原语可复用 |
| 密钥模型 | **对称共享密钥（rendezvous key)，不分 bridge/client 类型** | 用户明确决策。relay CLI 颁发，同一把 key 配进 bridge、交给用户；双方各自连 relay,relay 按 key 配对中转 |
| key ↔ bridge 基数 | **1:1**（一把 key 同一时刻只服务一个 bridge 连接；先到先占） | 防静默劫持：已有 bridge 在线时，同 key 的新 bridge 声明被拒；多台机器 = 多把 key |
| 认证唯一防线 | relay 侧会话校验是唯一认证（见 §7 威胁模型） | 个人版 handler 无条件信任一切请求，这不是纵深防御，是唯一防线 |
| key 前缀 | `wcr_` | `woca_` 是企业版管理词汇，避免支持与日志取证歧义 |

## 3. 组件

### 3.1 `woca-relay`（新二进制）

- 位置： `cmd/woca-relay` + `internal/relay`。归属个人版树，不得 import
  `internal/ee/`（CI 强制）。
- 职责： 公网唯一入口。
  - client 面： HTTPS 服务（托管现有 SPA + 登录 API + 转发 API/SSE 到 tunnel)。
  - bridge 面： tunnel 端点（长轮询取请求 + 回传响应）。
  - key 权威： 内置 sqlite，存 key 哈希与会话。
  - CLI: key 管理（见 §4.3)。
- 部署： 单二进制 + 一个 sqlite 文件。TLS 必配（§7.2)，或前置
  Caddy / CF Tunnel / Tailscale Funnel。

### 3.2 bridge 隧道客户端（`internal/relayclient`，编进现有 `woca`)

- 出站长轮询循环（默认 4 个 worker)，断线指数退避 + 抖动重连。
- 取到的请求交给**进程内** `Server.Handler()`（internal/server/server.go:342)
  执行——与本地 web 完全同一代码路径，不走 TCP loopback。响应经
  `streamRecorder` 同语义的分块写出回传（§6.3)。
- 配置与 CLI:
  - `woca relay connect --url https://relay.example.com --key wcr_…`（写入
    settings 持久化，`serve` 时自动拉起）。
  - `woca relay status` / `woca relay disconnect`。
  - env 等价物： `WOCA_RELAY_URL` / `WOCA_RELAY_KEY`（容器首启 / 无头部署）。
- key 存储： 个人版 settings 库（与现有设置同等保护）；不明文落盘到配置文件。

### 3.3 client(web SPA / native client)

- **web SPA: 零前端改动**。SPA 已有登录门： `POST /api/auth/login`
  body `{api_key}`(web/src/api/bridge.ts:17)，配套 `/api/auth/logout`、
  `/api/auth/me`；所有 API 走相对路径。relay 挂载这三个路由 + SPA 静态资源
  （复用 `webfx.SPA`,internal/webfx/spa.go:15）即获得完整登录 + 使用体验。
- **native client**: profile 系统是数据驱动（`configstore.Profile` 仅
  {ID, BaseURL, 时间戳})。新增"relay profile"= 一条 URL 数据 + 一个静态
  Bearer `CredentialProvider` 实现；传输层（`FlushInterval: -1`,SSE 安全）
  不改动。凭据存 OS 钥匙串，沿用现有存储模式。

## 4. 密钥生命周期

### 4.1 格式与生成

- 格式： `wcr_` + 32 字节 CSPRNG(crypto/rand）的 base64url(256 位熵）。
- 存储： relay 只存 `SHA-256(key)`；高熵随机 token 用 SHA-256 是正确构造
  (bcrypt/argon2 是密码场景，此处反而引入 DoS 面）。按哈希值查库，天然规避
  时序比较问题。
- 展示： 完整 key 仅 `create` 时输出一次；`list` 只显示 name + 短前缀
  （前 8 字符，非密钥部分）+ 元数据。任何日志不得出现完整 key。

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
POST /tunnel/poll (Bearer key)  POST /api/auth/login {api_key}
→ relay 校验哈希,登记 bridge    → relay 校验哈希,签发会话 cookie
    │                            │
    └──── relay 按 key 配对 ─────┘
    client 的每个请求 → 停放进 key 队列 → bridge poll 取走
    → 进程内执行 → POST /tunnel/respond/{id} 分块回传 → relay 流给 client
```

### 4.3 CLI

```sh
woca-relay key create <name>    # stdout 输出完整 key(仅一次);name 唯一
woca-relay key list             # name / 前缀 / 创建时间 / 最后使用 / 状态 / bridge 在线
woca-relay key revoke <name>    # 立即吊销(见 §5)
```

### 4.4 会话(client 侧）

- 登录成功 → opaque 会话 token(128 位 CSPRNG),cookie 下发：
  `HttpOnly; Secure(TLS 下;经终结器时按 X-Forwarded-Proto 判定);SameSite=Lax`;
  cookie 名 `woca_relay_session`（避开企业版的 `woca_session`)。
- 双重过期： **7 天滑动(idle)+ 30 天绝对上限**（对齐 OWASP：滑动过期必须有
  绝对封顶，否则被盗 cookie 可永久续命）。
- sqlite 中**会话 token 同样只存 SHA-256 哈希**。
- 登出 = 服务端删除会话（仅清 cookie 不算数）。
- `POST /api/auth/login` 限速（如每 IP 每分钟 10 次）+ 统一错误文案 + 失败日志。

## 5. 吊销语义（核心产品行为）

`woca-relay key revoke <name>` 触发，relay 主动、即时地通知**两侧**:

1. **bridge 侧**: 进行中的长轮询立即返回
   `410 {"code":"key_revoked"}`;bridge 的 relayclient 收到后**终止重连**
   （这是终态，不是退避重试）,CLI / web 状态页显示"key 已吊销"，需运维重新
   配 key 才能再连。
2. **client 侧**: 删除该 key 派生的全部会话；进行中的 SSE 流注入错误事件后
   关闭；之后所有请求 → `401 {"code":"key_revoked"}`，前端跳回登录页。
3. 停放进队列但未取走的请求： 全部以错误释放。
4. 吊销事件写 relay 本地审计日志（连同登录失败、bridge 上下线）。

## 6. 隧道协议（bridge ↔ relay，长轮询形态）

与企业版 scenario-four 同构，独立实现（不 import `internal/ee/`)。

### 6.1 端点

| 端点 | 方向 | 说明 |
|---|---|---|
| `GET /tunnel/poll?wait=25s` | bridge → relay | Bearer key 认证。长轮询取一个停放请求；25s 无请求返回 204。key 已吊销 → 410；同 key 已有 bridge 在线且非本连接代际 → 409 |
| `POST /tunnel/respond/{id}` | bridge → relay | Bearer key 认证。回传响应：`X-Woca-Relay-Status` + `X-Woca-Relay-Headers`(base64 JSON)+ 分块流式 body |

### 6.2 请求停放与路由

- relay 内存中为每把 key 维护一个停放队列 + bridge 在线状态（最后 poll 时间)。
- client 请求到达 → 校验会话 → 查 key 的 bridge 在线？ 离线 → 立即
  `503 {"code":"bridge_offline","retry_after":5}`;在线 → 停放（上限 64 个/
  key，超过 → 503）等待 poll 取走。
- bridge 4 个 poll worker 并发取请求，并发执行。

### 6.3 响应回传与 SSE 保真（最易错的点，审计修正）

- bridge 侧用 `streamRecorder` 同语义封装 `http.ResponseWriter`:
  **状态 + headers 在第一次 Write/WriteHeader 时冻结提交，Flush() 是 no-op
  边界提示**。原因：账本 SSE 路径在 probe flush 之后才设
  `Content-Type: text/event-stream`(internal/server/runs.go:704-711),
  若按 flush 提交会丢掉 Content-Type,SPA 的 onopen 检查会拒收流。
- 回传 body 每产生一块立即 `POST /tunnel/respond` 流式写出；relay 收到每一块
  **立即 flush 给浏览器**（禁止聚合/缓冲，HTTP/1.1 chunked 或 h2)。
- SSE 响应头原样透传（`Content-Type: text/event-stream`,
  `Cache-Control: no-cache`);relay 对 event-stream 响应自设
  `X-Accel-Buffering: no`。
- 空闲 SSE 流的存活依赖应用层心跳： bridge 侧 SSE 产生方需周期发
  `: ping` 注释（20–30s)。现有 woca SSE 是否已有心跳，实现时核查；
  没有则在 relayclient 层对 event-stream 补心跳（仅在无数据时注入注释行，
  属协议合法的 comment)。
- 请求体上限： client → bridge 的请求 body 上限 4 MB（对齐企业版
  `TestClientBodyLimit` 先例）；超限 → 413。流式上传不做（YAGNI)。
- 取消传播： client 断开 → relay 取消停放/标记 id 作废 → bridge 侧
  `CloseWithError` 同语义解除 handler 阻塞（goroutine 不泄漏）。

### 6.4 头部卫生

- bridge 执行请求前剥离所有 relay 信令头（`X-Woca-Relay-*`)，防伪造
  （先例： `X-Woca-*` stripping,internal/ee/bridge/tunnel.go:218-220)。
- relayclient 执行时注入 `X-Woca-Relay-Remote: 1`(relay 不可伪造，因为
  bridge 只信本进程内注入）供本地审计标记远程来源；`RemoteAddr` 取 relay
  透传的真实 client IP，用于日志/审计。

### 6.5 断线与重连

- poll 失败（网络抖动）: 指数退避（1s 起，封顶 60s)+ 20% 抖动，无限重试。
- 收到 401/410 `key_revoked` / `key_unknown`: 终态，停止重连。
- relay 重启： 停放队列丢失（内存），未完成的 client 请求 → 504;bridge
  poll 自动重连恢复；client 会话在 sqlite 不受影响。

## 7. 安全模型

### 7.1 威胁模型（必须显式陈述）

- **认证真空**: 个人版 handler 栈无条件信任一切请求
  （authn.NewLocal,authn.go:82)。relay 的会话校验 + "隧道是远程流量唯一
  入口" + 信令头剥离是**唯一防线**。任何让未认证请求抵达 handler 的 bug
  等价于把 shell 交给互联网。
- **对称 key 的代价**: 一把 key 同时能开 client 会话和声明 bridge 身份。
  key 泄露 = 全部信任泄露： 攻击者既能操控 bridge，也能抢注 bridge 身份。
  缓解： 先到先占（§2)、吊销即时双侧生效（§5)、key 只存哈希。
- **relay 能看明文**: relay 终止 TLS 并转发明文（非 E2E)。relay 被攻破的
  最坏结果 = 转发中的流量可见 + key 哈希库（哈希不可逆，无静态秘密可偷
  完整 key)。E2E 加密列为 v2(§11)。
- bridge 私钥/密钥不出设备；key 不明文写配置文件、不进日志。

### 7.2 红线（对齐企业版 R1/R6 精神）

- 公网绑定 + 无 TLS → **拒绝启动**(fail-closed);`--insecure` 仅 dev,
  且必须显式传参。
- 匿名可及的路由仅限： SPA 静态资源、`/api/health`、`/api/auth/login`。
  其余全路由要会话（client 面）或 Bearer key(tunnel 面）。

## 8. relay 存储

sqlite(直接用 `modernc.org/sqlite`，**不 import `internal/wocadb`**——它会
blank-import pgx，违背个人版依赖卫生）。迁移采用版本化
`schema_migrations` 模式（模板： native-client/internal/configstore/store.go:36-41)。

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
| bridge 离线 | 503 `bridge_offline` + retry_after | — |
| key 已吊销（client) | 401 `key_revoked` → 登录页 | — |
| key 已吊销（bridge) | （同上，bridge 已下线） | 410,终止重连，状态页标记 |
| 停放中 relay 重启 | 504 | poll 自动重连 |
| 请求 body > 4 MB | 413 | — |
| 停放队列满（64/key) | 503 | — |
| 未登录访问 API | 401 → 登录页 | — |

## 10. 测试策略（镜像企业版测试范式）

参照 internal/ee/edgegw/edgegw_test.go 与 internal/ee/bridge/bridge_test.go
的 in-process 组装（`httptest.NewServer` + 假对端），复刻：

- 单元： keystore（创建/吊销/哈希查找）、会话（滑动+绝对过期）、停放队列
  （满/超时/取消）、fail-closed bind 检查（对齐 TestCheckBindFailClosed)。
- 集成（全链路，进程内 relay + bridge handler + client):
  - 基本 round-trip(GET/POST，对齐 TestTunnelRoundTrip)
  - **SSE 逐帧透传**(对齐 TestTunnelSSE): 验证 Content-Type、分块即时性、
    心跳注释存活
  - 认证： 错误 key / 无会话 / 匿名路由白名单（对齐 TestTunnelAuth)
  - 吊销： 双侧通知（410 终态 + 401 + SSE 关闭，对齐 TestRevocationAndGrace)
  - body 上限 413(对齐 TestClientBodyLimit)
  - respond 的 key 隔离（对齐 TestRespondIsBridgeScoped)
  - 同 key 第二个 bridge 声明被拒（先到先占）
- e2e: `make` 目标起 relay + `woca`(relayclient)+ curl 走完
  create key → connect → login → 发起 run → SSE → revoke → 双侧错误。

## 11. v2 演进方向（本次不做，记录在案）

- **E2E 加密**: 对称 key 派生加密密钥,client 与 bridge 端到端加密,relay
  转密文（对齐 Happy Coder 的"relay 全盲"模型）。
- **传输升级**: 若未来需要亚秒级请求下达，可把 poll/respond 换成单条 WSS
  多路复用（外部审计已给出修正清单： 连接级内存上限、RequestData/Reset 帧、
  25s ping/pong、close code 4001 终态语义）。
- 手机端 App(native client 的 relay profile 已是其传输前体）。
- 多 bridge 选择页（当前 1 key = 1 bridge，多机器 = 多 key)。

## 12. 构建与 CI

- `cmd/woca-relay` 加入 Makefile 产物清单（不进企业版 dist 打包）。
- 扩展 `check-editions`: `cmd/woca-relay` 与 `internal/relay` 纳入
  "不得 import internal/ee" 扫描。
- 扩展 `check-deps`: 为 `cmd/woca-relay` 建依赖基线（预期仅 stdlib +
  modernc.org/sqlite + zerolog 等现有个人版依赖，**零新增第三方库**）。
- `internal/relayclient` 进入 `./cmd/woca` 依赖闭包后，跑 `make deps-baseline`
  更新 `ci/deps-personal.txt`（预期无新增，仅 import 图变化）。

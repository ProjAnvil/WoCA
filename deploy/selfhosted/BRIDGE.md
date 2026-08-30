# woca-bridge 部署手册（私有云第一阶段）

> 产品设计：《woca 产品化改造设计》v1.5+（第一阶段：Bridge 形态）。
> bridge = 你自己持有的私有云 coding agent：代码与执行不出你的机器，
> 公网上只有一个无状态的**边缘接入点（woca-edge）**，配置面/服务面
> 退在私网里承担配置治理与运行期服务。

## 角色与产物

| 产物 | 角色 | 网络位置 |
|---|---|---|
| `woca-bridge` | 执行面：用户机器/服务器，托管 SPA + API，组织 key 登录，出站注册到边缘接入点 | 用户侧（可 NAT 后），出站-only |
| `woca-edge` | 边缘接入点：公网唯一入口。无状态（R7）——TLS 终结、机器通道转发、客户端流量按 bridge_id 经出站隧道路由 | 公网 / DMZ / VPS / CF Tunnel |
| `woca-server` | 私有核心·服务面：机器注册表（enrollment/renew/heartbeat/resolve/bridges，监听 `WOCA_GW_ADDR`）+ **租户运维管理台**（v1.7 §6.7） | 私网 |
| `woca-config` | 私有核心·配置面：配置权威源 + 配置台（专注 woca 设置域）+ `/gw/config` 下发 | 私网 |

v1.4 的单体「入口网关挂 woca-config」形态已按 v1.5 拆分：注册类
`/gw/*` 在 woca-server 的网关监听器上，配置拉取在 woca-config 上；
bridge 只见边缘接入点一个对端（D5），核心内部拓扑对它透明。
v1.7 把租户与 key 管理、bridge 注册治理、审计从配置台上收到
**woca-server 的租户运维管理台**（与 `/gw/*` 同一个私网监听器）；
配置台只管 woca 自己的配置域。

## 一、控制面（管理员一次）

**租户运维管理台**（woca-server 的 `WOCA_GW_ADDR` 监听器，仅私网可达）
是整套系统唯一的管理入口：租户与 key 的签发/吊销、enrollment token
签发、pending 批准、bridge 吊销、治理审计、`edge_spki_pin`（k3s 模式）。
配置台（woca-config :8080）只管模型/存储/记忆/消息面等 woca 设置域。
等价的 API 形式：

```sh
# 私有核心：服务面 + 配置面（共享同一个数据库）
woca-server serve --db "postgres://…" --auth apikey   # WOCA_GW_ADDR=:8090
woca-config --db "postgres://…" --addr :8080

# 边缘接入点（公网侧唯一部署；TLS 必配，或前置 CF Tunnel/Tailscale Funnel）
woca-edge --addr :443 \
  --tls-cert cert.pem --tls-key key.pem \
  --service-url http://server.internal:8090 \
  --config-url http://config.internal:8080

# 签发一次性 enrollment token（12h 有效、只能用一次；运维管理台）
# 管理台登录：部署时设 WOCA_CONSOLE_USER/WOCA_CONSOLE_PASSWORD 则为
# 用户名密码登录（curl 需先 POST /api/console/login 换会话 cookie 再
# 带 -b "woca_console_session=<cookie>"）；未设则免登录开放，直接：
curl -X POST http://server.internal:8090/api/gw/enrollments
# → {"enrollment_token":"woca_enroll_…","expires_in_s":43200}
```

配置下发内容 = 配置面的 settings 域（provider、base URL、密钥、
storage/memory 后端、**模型白名单** `model-whitelist`）。bridge 侧这些域
变为只读（写 403、前端锁图标 + Managed 横幅）。

## 一·五、租户凭据与核心会话（v1.8）

运维在**管理台「租户与 key」页**签发 `woca_` key（备注用户名 + 角色；
完整 key 仅显示一次，经离线渠道交付）。**凭据格式全场唯一 `woca_`**，
登录校验与会话签发统一在私有核心（§2.1）：

- **bridge 零用户凭据**——不存 key 哈希、不存用户表，只持机器身份；
  场景三直连的登录经通道二转发核心校验（RFC 7662 introspection 模式，
  §2.7），bridge 仅按 ≤60s 内存缓存校验结果（非权威、不落盘）；
- **场景四在接入点执法**——登录/每请求会话校验由 edge 回源核心完成
  （R7 无状态不破），身份断言经已认证隧道带给 bridge，凭据不出 edge；
- **吊销即时**——管理台吊销 key 即杀其全部核心会话：新登录立即被拒；
  已建会话在下一次在线校验时失效（≤60s 缓存窗口为最坏延迟），
  401/SSE 关闭携带 `{"code":"revoked"}`；
- **降级语义**——核心不可达时新登录 fail-closed；已建会话存活至自然
  过期（能力单调退化，Tailscale 模型）；
- 管理台另示每张 key 的**最后使用时间**（核心侧权威——校验都发生在
  核心）与**活跃会话清单/逐会话吊销**。

## 二、bridge 侧（三种接入路径，按推荐排序）

| 路径 | 做法 | 适用 |
|---|---|---|
| A. **经边缘接入点（场景四，完整形态）** | bridge 零入站——只要能出站 443，家里/公司 NAT 后均可被远程指挥；浏览器连 `https://edge.example.com`（URL + key） | bridge 藏身私网 |
| B. Tailscale Serve/Funnel 或 VPS+Caddy | bridge 绑 loopback，反代/隧道提供 HTTPS（场景三变体） | 零证书运维 |
| C. bridge 直连公网（场景三） | `--addr 0.0.0.0:8080 --auth apikey` + `WOCA_TLS_CERT/KEY` 自带证书 | 高级用户 |

```sh
# 1. 用户凭据：管理台「租户与 key」页签发组织 key（bridge 上无 key 管理，
#    woca-bridge key 命令已随 v1.7 移除；未注册的 bridge 无法登录任何用户）
#    → 组织 key 离线交付给使用者

# 2. 起服务（R1：非 loopback 绑定 + 无认证 = 拒绝启动）
WOCA_TLS_CERT=cert.pem WOCA_TLS_KEY=key.pem \
woca-bridge serve --db ~/.woca/woca.db --addr 0.0.0.0:8080 --auth apikey

# 3.（可选）云接入 —— 三种注册路径（v1.7 §6.5），配置全部落在 settings 库（edge.* 键）：
#    a) 环境变量预置（容器首启 / 无头部署首选——apikey 姿态下首次注册前
#       无法登录 web 页，注册必须走 env）：
WOCA_EDGE_URL=https://edge.example.com WOCA_ENROLL_TOKEN=woca_enroll_… woca-bridge serve …
#    b) web config 页粘贴：浏览器打开 bridge → 设置 → 云接入 → 填边缘接入点
#       地址 + enrollment token → 保存（注册成功后 token 自动作废）；
#       loopback/本地信任姿态可直接操作，apikey 姿态需要已注册的 admin key
#    c) worker-led（免粘贴）：bridge 打印指纹 → 管理台批准
woca-bridge gateway request --url https://edge.example.com
```

注册成功后（场景四）：任意地方的浏览器打开 `https://edge.example.com`——
单 bridge 自动直进；多 bridge 出现选择页（或用带 bridge id 的直达链接
`/edge/select?bridge=<id>`）。native 客户端同样支持直达链接：连接页 URL
栏粘贴 `https://edge.example.com/?bridge=<id>` 即跳过选择步。登录凭据是管理台签发的**组织 key**（cookie 会话 7 天滑动；
吊销则会话立即失效）。远程客户端流量经出站隧道直达 bridge，
SSE 流式透传；`local` 信任模式**不会**开启隧道（apikey 姿态才允许）。

## 二·五、bridge 版 web 页面的三项能力

1. **接入点切换（bridge 切换，网页端）**：经边缘接入点打开的 web 页，
   设置 → **接入点** 列出注册表里的全部 bridge（在线状态 + 当前标记），
   点击即经 `/edge/select` 重设路由并整页重载——与 native 客户端连接页
   的选桥步骤对等。直连 bridge 或个人版 origin 探测不到边缘清单，
   该区自动隐藏（零形态分支）。
2. **远端配置的 MCP / skill（只读可见）**：设置 → **远端配置** 展示配置面
   下发的技能与 MCP（`GET /api/managed`，从 `~/.woca/managed` 物化文件
   回读——页面看到的就是 agent 实际合并的最低层），并带配置版本、
   上次同步与 STALE 缓存指示。内容只读：调整在配置台（woca-config）
   的「Skills & MCP」区操作，本地自己的配置始终优先（分层合并）。
3. **扫描 bridge 自身的 `.claude` 并导入**：设置 → **技能与 MCP** 在
   bridge 上扫描的是 **bridge 机器自己的** `~/.claude/skills` 与
   `~/.claude.json`（与个人版同一条 claudeenv 通道）：勾选导入即软链到
   `~/.woca/skills`、物化 `~/.woca/mcp.json`，落在用户层——**优先于**远端
   下发的 managed 层。容器部署想导入宿主机的 Claude 环境时，把宿主
   `~/.claude` 只读挂进容器 HOME，例如在 edge-bridge compose 里加：
   `- ~/.claude:/root/.claude:ro`。
4. **member 是完整交互用户**（authz 角色表）：设置页的全部只读面
   （`/api/settings`、`/api/managed`、`/api/claudeenv`、`/api/plugins`、
   `/api/stats`、`/api/quota`）对 member 开放——读面不含密钥（只回
   `*_set` 布尔）；写面里技能/MCP 导入（`PUT /api/claudeenv`）与插件
   启停/安装（plugins:write）同属用户扩展面。仍属 admin 的治理面：
   `/api/audit`、`/api/sessions/admin`、`/api/cloudaccess`（settings:write）。
   设置页左下角提供身份与「退出登录」（bridge 直连与公网入口同一路由，
   两端各自吊销核心会话）。
5. **存储/记忆随配置包下发**：配置台的存储区（checkpoint/memory 的分列
   PG 字段、remote 记忆端点、各自密码）随 bridge 配置包一起下发；
   bridge 在 agent 构建**前**做一次阻塞式初始拉取（agentbuild 的
   OnSettingsReady 接缝），使 PG/remote 连接在构建期即用云端坐标打开。
   桥上设置页的存储区渲染为只读 + managed 提示；模型服务商区在受管
   形态整体置灰（目录即管控）。`builtin-plugins` 键可由配置面锁定，
   锁定后桥上插件区显示「组织下发」徽标、开关禁用。

## 三、安全红线（R1–R7）——多副本说明

1. **R1 fail-closed**：无认证 + 非 loopback 绑定 → 拒绝启动
   （woca-edge 同款：公网绑定 + 明文 HTTP → 拒绝启动）。
2. **R2 全路由认证**：匿名仅 SPA 静态资源、`/api/health`、`/api/auth/login`。
3. **R3 SSE 鉴权**：cookie 会话兼容 EventSource；Bearer 走 fetch-event-source；
   场景四下 SSE 经边缘接入点中转，鉴权语义不变。
4. **R4 密钥零外泄**：bridge 上没有任何 key 列表/管理路由（v1.7）；管理台
   只在签发时完整显示一次 key，之后仅 kid + 备注 + 最后使用；配置下发的
   provider 密钥只存在于配置面与 bridge 的加密 settings 存储。
5. **R5 审计**：bridge 本地审计记登录/登出/登录失败、run 创建、workspace
   新建、云接入配置变更（`GET /api/audit`，admin key 可查）；key 签发/吊销、
   bridge 注册/吊销、登录失败等**治理事件**落服务面审计（管理台可查）。
6. **R6 文档红线**：**无认证的 bridge 绑公网 = 把 shell 交给互联网**。
7. **R7 接入点无状态**：woca-edge 不持久化任何配置、密钥、key 哈希或注册表
   副本——机器令牌每次回源服务面校验（`POST /gw/resolve`）；被攻破的最坏
   结果是转发中的流量可见，没有静态秘密可偷。可随时重建。**多副本需要
   负载均衡按 bridge 粘性路由**：客户端停泊与 bridge 轮询必须落在同一副本
   才会合（停泊区是进程内存，D7 的"多副本"以此为前提）。

## 联调（开发环境）

无证书接入姿态是**启动参数**，不是页面开关：`woca-server` 进程以
`WOCA_INSECURE_BRIDGES=1` 启动时开启（每次启动覆盖 `gw_settings` 行，
env 即权威；管理台与 API 不暴露该项——放宽 TLS 是部署决策，红线 R6）。
两套 dev compose 已默认带上。

- 开启后，bridge 侧以开发姿态接入自签/无证书边缘：
  `woca-bridge gateway enroll --url https://edge.local --token … --insecure`
  （或 env `WOCA_EDGE_INSECURE=1` + serve）。
- 网关姿态经匿名 `GET /gw/hello` 探测：`{"service":"woca-entrygw","allow_insecure":…}`。
- **SPKI 钉住（TOFU）**：首次 TLS 注册时锁定边缘证书指纹（settings 键
  `edge.spki_pin`），之后变更硬拒绝并告警；换证书需先「注销」再重新注册。
  隧道与配置通道共用同一钉住。**k3s 模式（可选，更强）**：管理台
  「bridge 注册」页设置 `edge_spki_pin` 后，签发的 enrollment token 内嵌该
  指纹——bridge 在花费一次性凭证前先验证接入点身份，投毒注册（假接入点
  骗 bridge 注册）从机制上不可行。

## 开发环境（dev 栈）

两套 compose：

- **微服务孪生** `make dev-up` / `make dev-down`：full 的最小孪生
  （server×1 / worker×1 / config / tenant / gateway / edge / 单 PG / 单
  NATS / ladyM），与 full 同端口互为 drop-in。网络模型**打平**：
  full 的五区（dmz/app/data/infra/mgmt，与 Terraform 一致）在 dev
  折叠为单一网络——联调环境不需要威胁模型。bridge 工具箱容器
  `make dev-bridge`（profile 隔离）已改为经 **woca-edge** 接入
  （`WOCA_EDGE_URL=http://woca-edge:8080`）——注册/受管配置/心跳/隧道
  全部走边缘接入点，与产品形态一致。LLM 追踪 `make dev-langfuse`
  （profile: langfuse，同样 opt-in）：Langfuse v4 全家桶；trace 走
  **常驻**的 woca-collector 桥（mgmt 段，地址由 nacos 条目
  `woca-obs.otlp` 下发，角色无需感知 Langfuse 地址/凭据），profile
  关闭时桥直接丢弃。UI 在 0.0.0.0:3000（对运维开放），默认登录
  admin@woca.local / woca-langfuse-admin。
- **产品化边缘组** `make edge-up` / `make edge-down`（v1.5 §6.3）：控制面
  单容器组——edge + config + server + pg，`public`/`internal` 双网络分层
  代替 DMZ（public 只挂 edge，internal 为私有核心；dev 姿态 edge 走
  明文 HTTP；**租户运维管理台** 127.0.0.1:9091、配置台 127.0.0.1:9086
  均 loopback 发布）。bridge 为**独立容器组**
  `make edge-bridge`（`docker-compose.edge-bridge.yml`），经宿主发布端口
  连边缘——同一台机器上演练「NAT 后 bridge + 公网 edge」的完整场景四：

  1. `make edge-up`（管理台/配置台默认开放；需要登录则在 compose 里设
     WOCA_CONSOLE_USER/WOCA_CONSOLE_PASSWORD）；
  2. 浏览器开 `http://127.0.0.1:9091`（运维管理台）→「bridge 注册」签发
     enrollment token；「租户与 key」签发组织 key（登录 bridge 用）；
  3. `make edge-bridge`：apikey 姿态下首次注册经 env（`.env` 里
     `EDGE_ENROLL_TOKEN=<token>` 预置；web 页粘贴需已注册的 admin key）；
     或浏览器开 `http://127.0.0.1:3490` → 设置 → 云接入 粘贴 token
     （edge URL 预填 `http://host.docker.internal:8443`）；
  4. 注册完成：浏览器开 `http://127.0.0.1:8443`（边缘）→ 用组织 key 登录
     ——场景四闭环，status 页全绿。

## 四、运维要点

- **缓存即真相**：边缘/核心不可达后 bridge 用本地配置照常运行，状态页/
  指示灯标黄 STALE；空响应/解析失败绝不清空配置。配置变更走 30s 长轮询
  （`/gw/config?wait=30s`，版本变了立即返回）。
- **状态页**：sidebar「状态」入口——云通道注册状态、配置版本与同步新鲜度、
  隧道服务状态、治理清单；一键导出诊断 Markdown（local 免登录姿态同样可用）。
- **服务面治理（租户运维管理台，仅私网）**：租户与 key（签发/吊销/最后
  使用）、enrollment token 签发（仅显示一次）、worker-led 注册批准、
  已注册 bridge 清单（心跳新鲜度）与一键**吊销**（访问令牌即时作废、
  接入点下次回源即拒）、治理审计（注册/吊销/签发/批准事件，R5 服务面审计）。
- **管理页（bridge，只读）**：设置 → 管理（仅 admin key 可见）——在线会话
  （"已登录设备"）与审计日志；没有任何 key 管理（v1.7）。设置 → 云接入
  （仅 admin）——边缘地址、enrollment token、注册状态、注销。
- **机器身份**：enrollment token 一次性、短 TTL、哈希存储；bridge 私钥本地
  生成不出设备（settings 库加密存储）；访问令牌 opaque 短 TTL（6h），吊销
  即时生效。v1.4 的 `~/.woca/bridge-gateway.json` 首次启动自动迁入并删除；
  `WOCA_GATEWAY_URL` 旧环境变量仍被接受（`WOCA_EDGE_URL` 优先）。

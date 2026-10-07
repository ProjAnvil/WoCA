# netlab — 网络层 docker 隔离集成测试机制（设计）

日期：2026-10-06
状态：已定稿（自主模式下依据目标说明直接推进实现）

## 1. 背景与目标

woca 现有的端到端测试分两层：

- `e2e/lab`（Go，进程内拓扑 enterprise|static|relay）+ Playwright（loopback 端口）：真实
  handler/真实登录，但**所有组件共享一个网络命名空间（127.0.0.1）**，不存在网络边界。
- `ci/e2e-relay.sh`：个人版 relay 链路的 loopback shell 冒烟。
- `deploy/selfhosted/docker-compose.edge*.yml`：企业版**开发环境**组（面向人工操作：宿主机
  发布端口、`host.docker.internal` 回连、`.env` 必填），不是测试机制。

缺的是：**把个人版与企业私有云版的网络层独立拆出来，用 docker 网络隔离模拟真实网络分区
（外网 / 传输网 / 内网），跑最终发布形态的二进制（dist 角色镜像），对联通性做自动化验证
与排查**。这就是 netlab。

明确目标（对应用户需求）：

1. 个人版与企业版两套网络层测试**互相独立**，可单独运行；
2. 测试对象是**最终二进制**（`make dist-role` 产出的 linux 角色包镜像，非进程内桩）；
3. 用 docker 网络隔离还原真实拓扑：外网 web 前端 → 中转站（relay）/接入点（edge）→
   隧道 → 内网 bridge；断言正向联通（frontend 穿透隧道打通内网 bridge）与**反向隔离**
   （frontend 直连内网 bridge IP 必须不通）；
4. 密钥按“通”为主，附“不通密钥要正确报错”的负路径（401 在网络边界正确浮出，而不是挂起）；
5. 从 Playwright e2e 中拆解出来：netlab 只测网络层，不含 UI 语义。

## 2. 方案对比

**A. 扩展现有 dev compose 组（edge-up / edge-bridge）加测试脚本**
复用现成文件，但它们面向人工操作：bridge 经 `host.docker.internal:8443`（宿主机端口）回
连，绕过了 docker 网络隔离本身；个人版完全没有 docker 拓扑；`.env` 必填。改造成本高且
把 dev 与 test 耦合在一起。弃。

**B.（选定）`e2e/netlab/` 独立机制：每版一个专用 compose 文件 + Go 测试驱动**
- 拓扑声明式可审（compose 文件即网络拓扑图）；
- 与 dev 栈、Playwright 完全解耦（独立目录、独立 project name、一次性卷）；
- 负路径（L3 隔离断言）直接利用 docker 内建的网络隔离语义；
- 驱动用 `go test -tags netlab`，普通 `go test ./...` 与 CI 不会误跑。

**C. 纯 testcontainers 风格（Go 代码内编排容器，无 compose 文件）**
灵活但拓扑不可声明式审阅，且用户明确要求“使用 docker-compose 文件”。弃。

## 3. 拓扑设计

### 3.1 个人版（personal / relay 中转站链）

```
  front 网络（模拟公网一侧）          wan 网络（模拟家庭宽带上行）
 ┌─────────┐        ┌──────────┐  ┌──────────────┐
 │ client  │───────▶│  relay   │◀─│  woca (home) │
 │ (curl)  │  http  │woca-relay│  │ woca serve + │
 └─────────┘        │ :8080    │  │ relayclient  │
   外网浏览器替身    └──────────┘  │ :8080        │
                    双宿：front+wan└──────────────┘
                                   仅 wan（NAT 隐藏替身）
```

| 服务 | 镜像 | 网络 | 要点 |
|---|---|---|---|
| `relay` | `woca-relay:netlab`（dist 包） | front+wan | `serve --addr :8080 --insecure`（明文 HTTP 为 dev 姿态，与 lab/CI 一致）；`WOCA_RELAY_DB` 落在卷上 |
| `woca` | `woca:netlab`（dist 包） | **仅 wan** | `serve --addr 0.0.0.0:8080 --no-browser`；隧道靠首启 env：`WOCA_RELAY_URL=http://relay:8080` + `WOCA_RELAY_KEY`（`relayclient/persist.go` 的 headless 路径，env 绕过 relay-url 的 TLS 红线是产品既有设计） |
| `client` | `curlimages/curl:8.10.1` | **仅 front** | 外网 web frontend 替身（浏览器语义用 curl 表达） |

关键顺序约束：`wcr_` 密钥必须先在 relay 上铸造再注入 woca（与真实运维一致：密钥在
中转站打印一次、带到家庭侧）。驱动分两阶段拉起：relay → exec `key create` → woca。

### 3.2 企业私有云版（enterprise / edge 接入点链）

```
 front（公网一侧）      wan（模拟边缘↔远端站点的互联网）     core（私有云内网，internal）
┌────────┐     ┌──────────┐     ┌─────────┐   ┌────────┐ ┌────────┐ ┌──────┐ ┌───────┐
│ client │────▶│   edge   │────▶│ bridge  │   │ server │ │ config │ │ pg   │ │ nacos │
│ (curl) │     │woca-edge │     │woca-brid│   │  :8080 │ │ :8080  │ └──────┘ └───────┘
└────────┘     │  :8080   │     │ge :8080 │   │GW :8090│ └────────┘
 外网浏览器     └──────────┘     └─────────┘   └────────┘
 替身           三宿：front+wan+core  仅 wan      仅 core（entrygw 注册表 + opsconsole API）
```

| 服务 | 网络 | 要点 |
|---|---|---|
| `edge` | front+wan+core | `WOCA_EDGE_DEV_HTTP=1`（明文，dev 姿态）；`WOCA_SERVICE_URL=http://server:8090`、`WOCA_CONFIGSVC_URL=http://config:8080`、nacos 强制项 |
| `bridge` | **仅 wan** | `WOCA_EDGE_URL=http://edge:8080` + `WOCA_ENROLL_TOKEN`（env 首启注册路径）+ `WOCA_EDGE_INSECURE=1`；`--auth apikey` |
| `server` | 仅 core | `WOCA_GW_ADDR=:8090`（entrygw 机器注册表 + opsconsole 管理 API 同监听）；`WOCA_INSECURE_BRIDGES=1`（无证书 bridge 的 dev 姿态，与 dev 组一致） |
| `config` | 仅 core | `/gw/config` 拉取面 |
| `pg` / `nacos` / `nacos-init` | 仅 core | server/config/edge 对 nacos fail-closed，属于网络层最小可行支撑；`WOCA_PG_PASSWORD` 在 netlab 内置默认值（一次性卷） |
| `client` | **仅 front** | 外 web frontend 替身 |

无头签发（open console 为产品默认 dev 姿态，`consoleauth.go`）：
- 注册令牌：`POST /api/gw/enrollments`（server:8090）→ `enrollment_token`
- 租户用户密钥：`POST /api/tenant/users` `{"remark":...,"role":"tenant-admin"}` → `key`（`woca_`）
两者由驱动 `docker compose exec server wget` 完成——不引入 ops/tenant 等非网络层角色，
**企业拓扑只保留网络层 + 其 fail-closed 依赖**。

## 4. 测试矩阵

正向（联通）：
- P1 front→relay/edge `/api/health` 200；
- P2 **隧道全链**：client 在 front 上登录（`POST /api/auth/login`，密钥经网络边界验证）
  → 带 cookie 拉 tunneled `/api/sessions` 得 JSON 数组（请求真实穿过
  停靠队列→wan→内网 bridge→本地 stack→回程）；
- P3 个人版 SSE 穿隧道：`/api/events` 响应头 `content-type: text/event-stream`、
  `x-accel-buffering: no`（回显 ci/e2e-relay.sh 契约）；企业版同契约经 edge；
  ——首跑即抓到真实缺口：edgegw 的 respond 回放不像 relay 那样为 SSE 补
  `X-Accel-Buffering: no`（edge 前若有缓冲代理会卡死事件流），已按设计 §6.3
  对齐修复（internal/ee/edgegw/edgegw.go handleRespond +
  TestTunnelSSE 断言）；
- P4 企业版 SPA 经隧道加载：`GET /`（edge 停靠 → bridge 应答 HTML）；
- P5 企业版注册→上线：bridge 起动后 `GET /api/edge/bridges` 出现 live 条目
  （注册流量真实走过 bridge→wan→edge→core→entrygw）。

反向（隔离，L3 断言，探测目标 IP 取自 `docker inspect`；分区矩阵由驱动经
DOCKER-USER 护栏显式注入，见 §4.1）：
- N1 client → 内网 bridge（wan 侧 IP）：必须不通（外网无法直连家庭/站点内网）；
- N2 client → core（server 的 core 侧 IP）：必须不通（企业核心不对公网暴露）；
- N3 bridge → server 直连（core 侧 IP）：必须不通（bridge 单控制面伙伴姿态：只能经 edge）。

负路径（错误浮出，不挂起）：
- E1 个人版错误 `wcr_` 密钥登录 → 401；
- E2 企业版错误 `woca_` 密钥经 edge 登录 → 401（错误从 core 穿整个链路浮出到外网 client）。

### 4.1 网络分区的实际执行（DOCKER-USER 护栏）

实测发现（2026-10，Docker Engine 29.4 / Docker Desktop）：Docker 28 起重构了
iptables —— 旧的 `DOCKER-ISOLATION-STAGE-1/2` 跨 bridge DROP 链消失，新
`DOCKER-FORWARD` 链对**任何从 bridge 进入的转发流量直接 ACCEPT**。因此：

- 不同用户自定义网络的容器**默认可以按 IP 互访**（实测数据传输成功）；
- `internal: true` 也只挡出网方向，**从其它 bridge 进入 internal 网络的流量
  照样放行**（实测确认）。

所以 netlab 不能依赖"engine 默认隔离"这一旧假设。驱动改为在 docker 官方
文档指定的策略挂钩 `DOCKER-USER` 链（先于 `DOCKER-FORWARD` 被评估、daemon
从不改写）注入分区矩阵：

- 个人版：DROP front×wan 双向；
- 企业版：DROP front×wan、front×core、wan×core 双向；
- 所有放行对话（client↔relay/edge、home/bridge↔relay/edge、edge↔core）
  都是**同 bridge L2 交换**，不经过 FORWARD 链，不受影响；
- 规则幂等（-C 先查再 -I），teardown 时在 bridge 尚存时先行删除；
- 旧引擎（仍默认隔离 bridge 的版本）上这些规则是无害冗余，行为不变。

工具镜像 `e2e/netlab/iptables/`（alpine + iptables）经
`--privileged --net host` 一次性注入/删除。另注：排查跨网问题时不要用
busybox `nc -z`（实测该版本连接成功也返回 exit 1），用 curl 或真实数据传输。

不做（本次范围外，明确记录）：TLS/安全语义、吊销级联（ci/e2e-relay.sh 已覆盖）、UI 语义
（Playwright 已覆盖）、worker/NATS 业务面（compose.full 已覆盖）。

## 5. 驱动与运行

- 位置：`e2e/netlab/`（Go 包 + 两个 compose 文件），与 `e2e/lab`（进程内）并列，
  构成“网络层独立拆解”的第三层测试；
- 驱动：`//go:build netlab` 的 Go 测试；`TestPersonalRelayChain` 与
  `TestEnterpriseEdgeChain` 各自独立成链（可 `-run` 单选）；
- 二进制：Makefile 目标先 `make dist-role`（GOOS=linux，arm64 本机）产出
  `build/dist/{woca,woca-relay,woca-server,woca-config,woca-edge,woca-bridge}.tar.gz`，
  compose `build:` 现场烘镜像——测试对象即最终分发形态；
- 目标：`make netlab-personal` / `make netlab-enterprise` / `make netlab`（两者）；
- 排查支持（对应“真实的网络层联通问题排查”）：
  - 失败时自动 `docker compose logs` 全量转储再 teardown；
  - `NETLAB_KEEP=1` 保留现场（不 teardown），README 提供手工联通排查命令集
    （从 client 容器 curl、`docker exec` 进各角色、inspect 取 IP 等）；
- 一次性资源：project/network/volume 一律 `woca-netlab-*` 前缀，teardown 带 `-v`。

## 6. 风险与既知边界

- 本机 docker desktop 为 linux/arm64：`dist-role` 默认 GOARCH 跟随本机即可；
- nacos/pg 镜像需本机已拉取或可拉取（本机已缓存）；
- 隧道轮询节奏（25s 长轮询）意味着 client 探测需要重试窗（驱动统一 60–120s 预算）；
- bridge 侧明文 HTTP edge / relay 依赖 dev 姿态开关（`--insecure`、`WOCA_EDGE_INSECURE`、
  `WOCA_EDGE_DEV_HTTP`），与既有 lab/CI/dev compose 的处理完全一致，非 netlab 新引入的口子。

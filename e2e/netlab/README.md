# netlab — 网络层 docker 隔离集成测试

把 woca 个人版与企业私有云版的**网络层**从端到端测试里独立拆出来，用
docker 网络隔离还原真实网络分区，跑**最终分发形态的二进制**
（`make dist-role` 角色包 → `build/dist/Dockerfile` 镜像），对联通性做
自动化验证与人工排查。设计文档：
`docs/superpowers/specs/2026-10-06-netlab-design.md`。

```
测试金字塔（拆解后）：
  netlab（本目录）   网络层：跨 docker 网络隔离，真实角色镜像，联通 + 隔离断言
  e2e/lab + Playwright  UI/交互：进程内拓扑，loopback 端口
  ci/e2e-relay.sh    个人版 relay 链路 loopback 冒烟（含吊销级联）
```

## 运行

```sh
make netlab-personal      # 个人版：client(front) → woca-relay → 隧道 → 家庭 woca(wan)
make netlab-enterprise    # 企业版：client(front) → woca-edge → 隧道 → bridge(wan) ＋ 私有核心(core)
make netlab               # 两者
```

前提：本机 docker 引擎跑 linux 容器（角色包按 `GOOS=linux GOARCH=<本机>`
交叉编译）。两个拓扑互相独立，可只跑其一。

断言矩阵（详见设计文档 §4）：

- 正向：公网侧 health、隧道登录、tunneled `/api/sessions`、SSE 头契约
  （`text/event-stream` + `x-accel-buffering: no`）、企业版 SPA 经隧道加载、
  bridge 注册上线；
- 隔离（负向）：client → 内网 bridge IP 不通；client → 核心 IP 不通；
  bridge → 核心直连不通（单控制面伙伴）；
- 错误浮出：错误密钥登录必须在边界得到 401，而不是挂起。

## 网络分区是如何强制执行的（重要）

Docker 28/29 起引擎移除了旧版跨 bridge 隔离（`DOCKER-ISOLATION-STAGE`
链），实测（Engine 29.4）**不同网络的容器默认可按 IP 互访**，连
`internal: true` 也挡不住从其它 bridge 进入的流量。因此 netlab 的分区
矩阵（front × wan × core 两两隔离）由测试驱动显式注入 docker 官方策略
挂钩 `DOCKER-USER` 链（先于 `DOCKER-FORWARD` 评估、daemon 从不改写）：

- 工具镜像：`iptables/Dockerfile`（alpine + iptables，`--privileged --net host` 运行）；
- 个人版注入 front×wan 双向 DROP；企业版注入三对；规则幂等，teardown 删除；
- 在仍默认隔离的旧引擎上这些规则是无害冗余。

排查跨网问题时**不要用 busybox `nc -z`**（实测连接成功也返回 exit 1），
用 curl 或真实数据传输。

## 失败排查

失败时驱动自动转储全部容器日志；`NETLAB_KEEP=1` 让现场保留不拆：

```sh
NETLAB_KEEP=1 make netlab-personal     # 失败后栈仍在
docker compose -f e2e/netlab/docker-compose.personal.yml ps
docker compose -f e2e/netlab/docker-compose.personal.yml logs relay
docker compose -f e2e/netlab/docker-compose.enterprise.yml logs edge bridge server
```

手工联通排查（从"外网浏览器替身"发起）：

```sh
# 公网一侧能到 relay / edge 吗
docker compose -f e2e/netlab/docker-compose.personal.yml exec client \
  curl -sS http://relay:8080/api/health
docker compose -f e2e/netlab/docker-compose.enterprise.yml exec client \
  curl -sS http://edge:8080/api/health

# 隔离验证：目标容器在对方网络里的 IP（应不可达）
docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{if eq $k "woca-netlab-p-wan"}}{{$v.IPAddress}}{{end}}{{end}}' woca-netlab-p-woca
docker compose -f e2e/netlab/docker-compose.personal.yml exec client \
  curl -m 5 http://<上面的IP>:8080/api/health     # 必须超时失败

# 隧道两侧各自视角
docker compose -f e2e/netlab/docker-compose.personal.yml exec relay \
  ./woca-relay --db /var/lib/woca-relay/relay.db key list     # bridge online?
docker compose -f e2e/netlab/docker-compose.enterprise.yml exec client \
  curl -sS http://edge:8080/api/edge/bridges                  # bridge live?
```

排查完清理：

```sh
docker compose -f e2e/netlab/docker-compose.personal.yml   down -v --remove-orphans
docker compose -f e2e/netlab/docker-compose.enterprise.yml down -v --remove-orphans
```

## 拓扑文件

- `docker-compose.personal.yml` — `front`（client+relay）/ `wan`（relay+woca）
  两网；`woca` 仅在 wan 上（NAT 隐藏替身），两阶段拉起（先在 relay 铸
  `wcr_` 密钥再注入家庭侧，与真实运维一致）。
- `docker-compose.enterprise.yml` — `front`（client+edge）/ `wan`（edge+bridge）
  / `core`（edge+server+config+pg+nacos，`internal: true` 无外联）三网；
  bridge 仅在 wan 上；注册令牌与租户密钥经 server 的 open-console 管理
  API（:8090，仅 core）无头签发。

## 边界（本机制不覆盖）

TLS/安全语义、密钥吊销级联（`ci/e2e-relay.sh`）、UI 语义（Playwright）、
worker/NATS 业务面（`deploy/selfhosted/docker-compose.full.yml`）。明文
HTTP 与免证书 bridge 是刻意的 dev 姿态，与既有 lab/CI/dev 组一致。

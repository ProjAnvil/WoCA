# woca-enterprise — 微服务版(企业版)持久化层

对应《woca 微服务化改造设计》§4(目标架构)/ §5.2(装配差异)/ §9(数据
模型)/ §10(运行状态外置与事件流)的开发环境:PostgreSQL(主 + 流复制热备)
与 NATS JetStream(3 节点 RAFT 集群)作为一套 `docker compose` 拓扑,按组件
类别切分网段,对外只经 **唯一网关**(nginx TCP 直通)以高端口分发。这是
Phase 1+ 各微服务角色(woca-scheduler / scheduler / worker / …)落地时共享的数据面。

## 拓扑

```
宿主机(仅网关发布高端口)        edge 172.28.30.0/24          数据网段
────────────────────►  gateway(nginx) ──────►   pg   172.28.10.0/24
  35432 → PG 写                  │                     pg-primary(写)
  35433 → PG 读(备)              │                     pg-replica(流复制热备)
  34222 → NATS 客户端             │              mq    172.28.20.0/24
  38222 → NATS 监控               └── 未来的 woca 微      nats-js-1/2/3
                                       服务角色挂这里       (JetStream,R3 流)
```

- **网段隔离**:`pg` / `mq` / `edge` 三个独立 subnet,只有 gateway 同时接入
  三个网段;PG 与 NATS 不发布任何宿主机端口,外部唯一入口是 gateway。
  新版 Docker 网络栈默认不再做网段间转发隔离(DOCKER-USER 全放行),因此
  compose 里的 `firewall` 一次性服务会在 Docker VM 的 DOCKER-USER 链装入
  跨段 DROP 规则——合法流量本就全部段内或经网关,任何跨段转发的包都是
  绕过尝试;`make ent-down` 会移除这些规则。
- **多实例**:NATS 3 节点 JetStream 集群(元数据 RAFT,流 R3);PG 主 +
  流复制热备(`pg_basebackup -R` 引导,复制槽 `woca_replica`)。
- **高端口**:35432 / 35433 / 34222 / 38222;宿主侧端口可在 `.env` 用 `ENT_*` 覆盖(容器内侧为固定值)。

## 使用

```sh
cd deploy/selfhosted
docker compose up -d          # 或从仓库根:make ent-up
docker compose ps
docker compose logs -f nats-init   # 流初始化一次性任务的结果
docker compose down           # 保留数据卷;down -v 连数据一起删
```

连接串(宿主机视角,走网关):

```sh
# woca 单体/未来 server 角色(读写 → 主库)
WOCA_DB="postgres://woca:woca-enterprise-dev@127.0.0.1:35432/woca?sslmode=disable" woca serve

# psql 两个路径
psql "postgres://woca:woca-enterprise-dev@127.0.0.1:35432/woca?sslmode=disable"   # 写
psql "postgres://woca:woca-enterprise-dev@127.0.0.1:35433/woca?sslmode=disable"   # 只读(热备)

# NATS(客户端 / 监控)
nats --server nats://127.0.0.1:34222 stream report
curl -s http://127.0.0.1:38222/healthz | jq
```

应用表由 woca 的 Go 后端在连接时幂等自建(`wocadb.migrate`、
`lgcp.ensureSchema`、`store.NewSQL`,均为 `CREATE TABLE IF NOT EXISTS`),
initdb 只负责复制角色与实例级约定(UTC、read committed)。

## JetStream 流

`nats-init`(一次性任务,幂等:已存在则跳过)预置两条平台流:

| 流 | subjects | 保留策略 | 副本 | 用途(设计文档) |
|---|---|---|---|---|
| `WOCA_RUNS` | `runs.>` | work queue | 3 | run 队列:Enqueue/Claim/Ack,一条消息恰好投给一个 worker(§5.4) |
| `WOCA_EVENTS` | `events.>` | limits(24h / 5G) | 3 | 事件扇出:eventbus remote 实现 + SSE `Last-Event-ID` 补齐窗口(§10) |

## 未来的微服务角色如何接入

新角色的 compose 服务只需接入 `edge` 网段,数据面一律走网关:

```yaml
services:
  woca-worker:
    # …
    environment:
      WOCA_DB: postgres://woca:...@gateway:35432/woca?sslmode=disable
      WOCA_NATS: nats://gateway:34222
    networks:
      - woca-enterprise_edge   # external 声明见下
networks:
  woca-enterprise_edge:
    external: true
```

网段即命名空间:`pg`/`mq` 段内直连的资格只保留给数据服务自身;这保证了
"仅通过一个 gateway 分发"的边界在开发环境与目标 K8s 拓扑里形状一致。

## 运维备忘

- **故障切换不在本环境自动化**:备库提升(`pg_ctl promote` / `SELECT pg_promote()`)
  为手动操作;NATS 集群容忍单节点故障(R3 流可读可写)。
- **重置**:`make ent-down` 再 `down -v`、重新 `ent-up` 即回到全新状态
  (initdb 重跑、流重建、隔离规则重装)。
- 监控:`http://127.0.0.1:38222/varz|jsz|healthz`(NATS 节点 1);
  `pg_stat_replication`(主库)验证复制在跑。

## 密码与端口

开发默认值在 [.env](.env)(已入库,方便开箱即用);接任何半正式环境前
务必改掉两个密码。端口覆盖:改 `.env` 里的 `ENT_*` 后 `docker compose up -d`
重建 gateway 即可。

## 租户与 API key 认证(Phase 1)

`woca-scheduler serve --auth apikey`(或 env `WOCA_AUTH=apikey`)后,除 `/api/health`
外全部 API 要求 Bearer 凭证;默认 `local` 模式信任本机(个人版行为不变)。
凭证用 `woca-tenant tenant` 管理(走同一 `--db`/`WOCA_DB`):

```sh
# 建 tenant 并发 key(secret 只显示一次)
woca-tenant tenant --db "postgres://woca:…@127.0.0.1:35432/woca?sslmode=disable" create acme
woca-tenant tenant --db … issue <tenant-id> --role member --name ci     # --user 省略则自动建用户
woca-tenant tenant --db … keys <tenant-id> && woca-tenant tenant --db … revoke <prefix>

curl -H "Authorization: Bearer woca_…" http://127.0.0.3484/api/sessions
```

- **key 形态** `woca_<前缀12hex>_<密钥48hex>`:库中只存 SHA-256,前缀是非
  机密查找句柄;角色(tenant-admin/member/viewer)在签发时冻结为 scope 集。
- **PG RLS**:wocadb 五张业务表(`settings`/`secrets`/`checkpoints`/
  `memory`/`session_state`)自动加 `tenant_id` 列并启用
  `FORCE ROW LEVEL SECURITY`;policy 按 `app.tenant_id` GUC 过滤,未设置时
  默认 `local`(存量单租户部署无感)。注意:**超级用户绕过 RLS**,应用
  连接必须用建表的非超户角色(本环境 `woca` 即初始超户,生产应另建
  `NOBYPASSRLS` 应用角色——initdb 的角色约定文件是落点)。
- **默认拒绝**:Phase 3 沙箱落地前,非 local 租户在全部宿主面
  (runs/workspace/settings/memory…)一律 403,仅 health 与 SPA 可达;
  local 租户的 key 也受 role scope 约束(viewer 只读)。
- sqlite(个人版)不建租户列:隔离由 store 层 `tenant:` 命名空间前缀 +
  路由门承担,行为与今日一致。

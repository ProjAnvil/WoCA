# woca-enterprise: full deployment architecture

Docker Compose deployment matching the microservices design doc (§4/§5/§6.3),
combining woca and ladyM with multi-instance load balancing, database physical
isolation, and network-zone isolation.

## Architecture

```
  host ports (via gateway only)    edge 172.28.30.0/24     data segments
  ─────────────────────────►  gateway ────────────►  woca-pg  172.28.10.0/24
     :8080 business entry         │                     woca-mq  172.28.20.0/24
     :8081 tenant admin           │                     ladym-pg  172.29.10.0/24
     :8082 skill-svc SPA          ├── woca-server ×2
     :8083 component discovery    ├── woca-worker ×2
     :8084 ladyM console          ├── ladym-api ×2
                                 └── ladym-worker
```

### Network zones (physical isolation)
- **woca-edge**: stateless services (server/worker) + the gateway. Horizontally
  scalable; only the gateway publishes host ports.
- **woca-data**: woca PG primary/replica + NATS JetStream cluster. Only woca
  services and the gateway attach here.
- **ladym-data**: ladyM PG (pgvector) + API ×2 + System 2 worker + console.
  Physically isolated from woca data (§6.3.3: independent fault domains,
  backup cycles, and scaling).

### Database physical isolation (§6.3.3)
- **woca-pg** (PostgreSQL 16): sessions, runs, run_events, settings, tenant,
  checkpoints. OLTP workload.
- **ladym-pg** (PostgreSQL 17 + pgvector): memory entries with HNSW vector
  indices. Analytical/vector workload.
- No cross-database joins; consistency by application-level orchestration.

### Exposed surfaces (via the gateway only)
| Port | Surface | Target |
|------|---------|--------|
| :8080 | Business entry (API + SPA + SSE) | woca-server ×2 (least_conn) |
| :8081 | Tenant management | woca-tenant (Phase 4 SPA) |
| :8082 | Skill-svc management SPA | woca-server /skills/ |
| :8083 | Component discovery & config | health + stats endpoints |
| :8084 | ladyM management console | ladym-console |

## Prerequisites

1. **woca images**: the Dockerfiles in this directory build from the repo
   root; `docker compose -f docker-compose.full.yml build` builds them.
2. **ladyM image**: build the ladyM enterprise image from the ladyM repo:
   ```bash
   cd ../ladyM && docker build -t ladym-enterprise:local -f Dockerfile.enterprise .
   ```
3. **Environment**: `cp .env.example .env` and set passwords.

## Deploy

```bash
docker compose -f docker-compose.full.yml up -d
```

## Verify

```bash
# Business entry (health)
curl http://localhost:8080/api/health

# Component discovery
curl http://localhost:8083/health
curl http://localhost:8083/ladym/health

# Skill-svc SPA
open http://localhost:8082

# ladyM console
open http://localhost:8084
```

## Issue an API key (tenant admin)

```bash
docker exec woca-ent-tenant woca-tenant --db "$WOCA_DB" tenant create "My Org"
docker exec woca-ent-tenant woca-tenant --db "$WOCA_DB" tenant issue <tenant-id>
```

## Scaling

The woca-server and woca-worker instances are stateless (all state in the
shared PG); scale horizontally:

```bash
docker compose -f docker-compose.full.yml up -d --scale woca-server-1=3
```

## Teardown

```bash
docker compose -f docker-compose.full.yml down -v
```

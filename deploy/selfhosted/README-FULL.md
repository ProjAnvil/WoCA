# woca-enterprise: full deployment architecture

Docker Compose deployment matching the microservices design doc (§4/§5/§6.3),
combining woca and ladyM with multi-instance load balancing, database physical
isolation, and network-zone isolation.

## Architecture

```
  host ports (via gateway only)    edge 172.28.30.0/24     data segments
  ─────────────────────────►  gateway ────────────►  woca-pg  172.28.10.0/24
     :8080 business entry         │                     woca-mq  172.28.20.0/24
     :9082 skill-svc SPA          ├── woca-server ×2     ladym-pg  172.29.10.0/24
     :9083 component discovery    ├── woca-worker ×2
     :9084 ladyM console          ├── ladym-api ×2
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
| :9082 | Skill-svc management SPA | woca-server /skills/ |
| :9083 | Component discovery & config | health + stats endpoints |
| :9084 | ladyM management console | ladym-console |

## Prerequisites

1. **woca images**: the Dockerfiles in this directory build from the repo
   root; `docker compose -f docker-compose.full.yml build` builds them.
2. **ladyM image**: build the ladyM enterprise base image from the ladyM
   repo (v0.5.1+ for CJK segmentation):
   ```bash
   cd ../ladyM && docker build -t ladym-enterprise:local .
   ```
   The compose stack then builds the dict-baked image
   (`ladym-enterprise:local-dict`) from that base automatically: a thin
   data layer (Dockerfile.ladym-dict) carrying the sha256-pinned CJK
   segmentation dictionary at /opt/ladym/dict, so CJK recall works fully
   offline — ladyM never downloads dictionaries itself.
   `LADYM_DICT_VARIANT` in .env picks zh (default) | zh_s | zh_t | jp.
   Air-gapped builds: place the pinned files under
   `deploy/selfhosted/dict/` before building. To refresh the dictionary:
   rebuild the layer and restart the ladyM services.
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
curl http://localhost:9083/health
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

## Observability & IT console (design §12.3)

Every server artifact embeds the OTel SDK: `/healthz` + `/metrics` on each
instance, OTLP export to a collector, RED metrics per route, and worker
run metrics. The IT pane is Grafana + Alertmanager (not homegrown):

```bash
make ent-obs-up        # collector + jaeger + prometheus + alertmanager + grafana
open http://localhost:8085
```

Details, env contract, and the VM (Consul/Nacos) address-source mode live
in `deploy/observability/README.md`; Kubernetes layouts in
`deploy/k8s/README.md`. To surface the Grafana link inside the woca SPA,
set `WOCA_GRAFANA_URL` on the woca-server containers (served via
`/api/health`).

## Lifecycle & service governance (compose layer)

The full stack mirrors the microservices architecture (design §5.3):

- **Segments**: edge (woca roles + gateway) / pg (PostgreSQL primary +
  hot-standby) / mq (NATS JetStream ×3, streams provisioned by the
  `nats-init` one-shot) / ladym (physically isolated data plane) / obs
  (observability, via the overlay). Cross-segment forwarded traffic is
  dropped by the `firewall` one-shot in Docker's DOCKER-USER chain.
- **Health & self-healing**: every long-running service declares a Docker
  healthcheck (`/healthz` on woca-server:8080 and woca-worker:9091, NATS
  monitor, pg_isready) plus `restart: unless-stopped`.
- **Addressing**: service DNS names only; the gateway is the sole
  host-facing entry (:8080 business, :9082 skills SPA, :9083 discovery,
  :9084 ladyM console, :8085 Grafana with the overlay; the tenant ops
  console publishes separately on 127.0.0.1:9091). Tenant management is
  the ops console — one console, one concept; multi-tenancy arrives as
  an evolution of it, not a second surface (v1.7 §6.7).

```bash
make ent-full-up     # all roles + data plane + firewall + observability
make ent-full-down   # teardown incl. firewall rules (volumes kept)
```

## Post-deploy: model configuration is required

A fresh stack ships no LLM provider settings (keys are tenant assets). Until
you seed them, runs fail at the model call — the run ledger records the
error and the web UI surfaces it. Seed via SQL against woca-pg-primary
(storage keys are underscored; API keys go in the encrypted `secrets`
table, which the CLI manages) or point `WOCA_*` provider env at your
gateway-side secret store:

```sql
-- example: OpenAI-compatible endpoint
INSERT INTO settings(key, value, updated_at) VALUES
  ('provider', 'openai', 0),
  ('model', 'your-model', 0),
  ('openai_base_url', 'https://api.openai.com/v1', 0)
ON CONFLICT (key) DO UPDATE SET value = excluded.value;
```

The API key itself must be encrypted by a woca process (AES-GCM under the
machine master key) — set it from any machine running the personal `woca`
binary against the same database, or through `/api/settings` on the gateway.

Memory is already wired: `memory_engine=remote` + the `WOCA_LADYM_*` env
in docker-compose.full.yml point the execution roles at the ladyM
enterprise API (design §6.3/§12.2 — the endpoint is infrastructure config,
env wins over the settings rows).

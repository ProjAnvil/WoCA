# woca-enterprise: full deployment architecture

Docker Compose deployment matching the microservices design doc (§4/§5/§6.3),
combining woca and ladyM with multi-instance load balancing, database physical
isolation, and network-zone isolation.

## Architecture

```
  host ports (via gateway only)
  ─────────────────────────►  gateway (dmz 172.28.30.0/24)
     :8080 business entry         │
     :9082 skill-svc SPA          ├── woca-server ×2 ┐
     :9083 component discovery    ├── woca-worker ×2 ├ app 172.28.32.0/24
     :9086 config console         └── woca-tenant    ┘ (legs: data + infra)
     :3000 Langfuse ops UI
                                 woca-pg ×2     data  172.28.10.0/24
                                 NATS ×3, ladyM, nacos,
                                 woca-collector + Langfuse
                                                infra 172.28.20.0/24
                                 woca-config, woca-ops
                                                mgmt  172.28.50.0/24
```

The five zones are 1:1 with the Terraform reference
(`deploy/terraform/aws`): dmz/app is the split edge, infra folds the
former mq/ladym/obs segments (nacos rides infra — runtime infrastructure,
not a management surface — so an mgmt outage never blocks role
restarts), and the firewall one-shot drops every cross-zone pair.

### Network zones (physical isolation)
- **woca-dmz**: the gateway only — the public half of the edge. The business
  roles never share a segment with it.
- **woca-app**: woca-server/worker/tenant (the business half of the edge),
  with data + infra legs.
- **woca-data**: woca PG primary/replica.
- **woca-infra**: the platform substrate — NATS JetStream ×3, ladyM (its PG
  remains an independent database fault domain, §6.3.3), nacos, the
  woca-collector trace bridge and Langfuse.
- **woca-mgmt**: the consoles only (woca-config, woca-ops).

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

## LLM tracing (Langfuse)

[Langfuse](https://langfuse.com) v4 ships with the full stack itself
(`langfuse-*` services, infra zone 172.28.20.0/24):
web + worker over their own PostgreSQL, ClickHouse, Redis and MinIO, all
isolated from every other segment by the firewall rules like the rest of
obs. The gen_ai spans every role emits are routed through the
`woca-collector` bridge, so tool/LLM traces land as Langfuse observations
with no extra instrumentation.

- UI: `http://<host>:3000` (`ENT_LANGFUSE_PORT`) — served by the gateway
  (the DMZ entry), reverse-proxied into the infra zone; no direct
  container publish. Sign-in callbacks need the externally visible origin
  (`LANGFUSE_NEXTAUTH_URL=http://<host>:3000`). Browser-side media upload
  (MinIO direct) is not exposed through the DMZ — same ruling as the
  Terraform side.
- Session reporting: every LLM call records a `chat` span carrying
  `run_id`/`session_id`/`tenant_id` tags (internal/tracing), the
  collector renames `session_id`→`session.id` (and `user_id`→`user.id`)
  onto Langfuse's OTLP conventions — sessions group traces in the UI
  out of the box, no extra instrumentation.
- Bootstrap: `LANGFUSE_INIT_*` seeds org `woca`, project `woca-project`,
  admin `admin@woca.local` / `woca-langfuse-admin`, and pins the project
  API keys to `pk-lf-woca-local` / `sk-lf-woca-local` on first boot.
- Trace routing: the roles never learn the Langfuse address — they push
  credential-less OTLP to the `woca-collector` bridge (mgmt segment, dual-
  homed into obs; `deploy/collector/`), resolved through the registry
  entry `woca-obs.otlp` exactly like the DSN. The bridge owns the Langfuse
  path + Basic auth (`LANGFUSE_OTLP_AUTH` — the default matches the key
  pair above; override the pair in `.env` and regenerate the header with
  `printf 'pk:sk' | base64`) and fans traces out to the observability
  overlay's collector (Jaeger/Prometheus) when the overlay is up. The
  bridge also runs standalone: `deploy/collector/docker-compose.collector.yml`.
- Note: fresh v4 installs run in events-only mode, where the legacy v3
  REST query endpoints (`GET /api/public/traces` etc.) answer 4xx —
  OTLP ingestion and the UI are unaffected.
- Required `.env` additions (see `.env.example`):
  `LANGFUSE_NEXTAUTH_SECRET`, `LANGFUSE_SALT`, `LANGFUSE_ENCRYPTION_KEY`
  (`openssl rand -hex 32`), `LANGFUSE_PG_PASSWORD`,
  `LANGFUSE_CLICKHOUSE_PASSWORD`, `LANGFUSE_REDIS_AUTH`,
  `LANGFUSE_MINIO_ROOT_PASSWORD`.

The dev twin gets the same group behind a profile: `make dev-langfuse`
(everything dev-defaulted; plain `make dev-up` stays trace-free).

## Lifecycle & service governance (compose layer)

The full stack mirrors the microservices architecture (design §5.3):

- **Segments**: edge (woca roles + gateway) / pg (PostgreSQL primary +
  hot-standby) / mq (NATS JetStream ×3, streams provisioned by the
  `nats-init` one-shot) / ladym (physically isolated data plane) / obs
  (observability — Langfuse in this file, the metrics/trace pipeline via
  the overlay). Cross-segment forwarded traffic is dropped by the
  `firewall` one-shot in Docker's DOCKER-USER chain.
- **Health & self-healing**: every long-running service declares a Docker
  healthcheck (`/healthz` on woca-server:8080 and woca-worker:9092, NATS
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

## Sandbox backends (off-host run execution)

Runs created by remote tenants execute on workers whose tool calls run
inside a sandbox backend registered and enabled through the config
console (`woca-config`, 沙箱后端 section). The worker provisions one
sandbox per run: it uploads the run workspace as a tar.gz, routes every
tool call through the sandbox (paths translated host-workspace →
`/workspace`), and downloads the products back (`.git` excluded, host
`.git` preserved). Any failure in that chain fails the run explicitly —
the worker never falls back to executing locally.

### Backend types

| `type` | What it talks to | Requirements |
|---|---|---|
| `contract-http` | Any service implementing the generic HTTP contract v2 (`internal/platform/executor/CONTRACT.md`) | The contract implementation's endpoint + API key. Use the `woca-fakesandbox` image (`make dist-role ROLE=woca-fakesandbox`) to validate the chain without a real sandbox |
| `cfbridge` | Cloudflare sandbox bridge (SSE exec, tar persist/hydrate) | A Cloudflare account with the sandbox bridge deployed; single-file limit 32 MiB rides the archive channel |
| `e2b` / `daytona` / `modal` | SaaS sandbox platforms | Registrable now; the Go providers land post-v1 — verify reports "provider not available" until then |
| `microsandbox` | Self-hosted microsandbox cluster | Same as above (registrable, provider pending); self-hosted Firecracker/KVM hosts |
| `sidecar` | SDK-only platforms exposed as a contract v2 sidecar process | v1 deferred |

The type enum is the full contract set — registration accepts every
value above; the worker's provider registry decides what can actually
run (today: `contract-http`, `cfbridge`).

### Verify, enable, and the gate semantics

- **Verify** (`POST /api/sandboxes/{id}/verify` from the console's
  "test connection" button) walks the whole chain against the backend —
  create, exec, write/read round-trip, glob/stat, start/kill, archive
  round-trip — and records per-step latency and the last probe result on
  the row (badge in the console). Probe state is advisory only: an
  enabled-but-failing backend shows a warning badge; it never auto-flips
  the gate (avoiding probe flaps gating run creation).
- **Enable** is global and single: enabling one backend disables the
  others in the same write. An enabled backend cannot be deleted (409) —
  disable it first (which also clears its stored API key).
- **API keys** are stored encrypted in the shared secrets table under
  `sandbox-api-key:<id>` — every role must share one `WOCA_HOME` master
  key volume (the compose stacks already do), or workers cannot decrypt
  the backend key the config plane wrote.
- **Run gating**: with `WOCA_RUN_DISPATCH=queue` +
  `WOCA_RUNS_OFF_HOST=true` on woca-server, `POST /api/runs` from remote
  tenants answers 403 while no backend is enabled, and 503
  ("no sandbox-capable worker available") while no live worker acks the
  `sandbox` capability (the worker rewrites its capability ack every
  10 s; the dispatcher's live window is 15 s). Queue dispatch is a hard
  prerequisite for the off-host posture — runs execute on workers, not
  in the server process.

### Pack version

The config pack is version 2 (it carries `sandbox_backends`). Both the
config plane and the workers run the same generation — a fresh deployment
has no mixed-version window, and the decoder accepts v2 only: a pack of
any other version is refused outright instead of half-applying.

### Limits and known restrictions

- `sandbox-archive-max-bytes` (settings key, default 536870912 —
  512 MiB) bounds the workspace archive in BOTH directions: a run whose
  upload or product download exceeds it fails explicitly (the products
  never silently truncate).
- During a sandboxed run the worker disables host-bound surfaces:
  file-level undo/redo, worktree enter, MCP stdio servers, and LSP
  return "not available during sandboxed runs". The system prompt's git
  status block is the one deliberate exception (design §10.3 m-6): it
  reads the host workspace read-only — the same tree the archive upload
  carries into the sandbox — so it is kept as-is rather than rebuilt
  around a context-free cache.
- The run workspace must be a directory shared between the dispatching
  woca-server and the executing workers (the same volume both roles
  mount); products land back in it after each run.

Reference e2e: `make sandboxlab` (e2e/sandboxlab) drives this whole
chain against the containerized fakesandbox, including the 403/503
gate postures.


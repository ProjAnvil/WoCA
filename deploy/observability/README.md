# woca Observability Stack (service governance, design §12.3)

The IT operations pane for the woca enterprise deployment: **OTel Collector
→ Prometheus (+ Alertmanager) + Jaeger, viewed through Grafana** — an
off-the-shelf stack, not a homegrown console. The business/tenant admin
pane stays the woca SPA, which embeds a link to Grafana (see below).

## Layout

```
deploy/observability/
├── otelcol/config.yaml                  collector: OTLP in → Prometheus/Jaeger out
├── prometheus/prometheus.yml            scrape config (compose service names)
├── prometheus/rules/woca-rules.yaml     alert rules (service down, 5xx, worker stall…)
├── alertmanager/alertmanager.yml        routing (point the webhook at your channel)
├── grafana/provisioning/                datasources + dashboard auto-provisioning
│   └── dashboards/woca-services.json    the "woca Services" dashboard (deliverable)
└── docker-compose.observability.yml     overlay onto docker-compose.full.yml
```

## What each woca binary exposes (Phase 3 governance baseline)

| artifact | endpoints | notes |
|---|---|---|
| `woca-server` | `/healthz`, `/metrics` on the business port | probes answer from the moment the port binds, before the agent finishes building; nginx blocks `/metrics` on the public entry (Prometheus scrapes inside the network) |
| `woca-worker` | `/healthz`, `/metrics` on `--obs-addr` (default `:9091`, `off` disables) | dedicated probe port — the worker has no business HTTP surface |

Metrics produced by `internal/ee/obs`:

- `http.server.request.count{http_route, http_request_method, http_response_status_code}`
- `http.server.request.duration_seconds_*` (histogram, same labels)
- `http.server.active_requests{http_route,…}` (in-flight; SSE connections live here)
- `woca.worker.claimed`, `woca.worker.runs{status}`, `woca.worker_run_duration_seconds_*`
- Go runtime instruments (`go_goroutine_count`, `go_memory_*`, …)

Traces: every tool/LLM/skill span recorded by `internal/telemetry` flows to
the OTLP endpoint with `gen_ai.*` attributes (OTel GenAI semconv) plus
`tenant_id`/`run_id`/`session_id` tags.

## Environment contract (§12.2: infrastructure config layer)

| variable | meaning | default |
|---|---|---|
| `WOCA_SERVICE_NAME` | `service.name` resource attribute | binary name |
| `WOCA_OTEL_EXPORTER_OTLP_ENDPOINT` | OTLP collector (`http://…:4318`) | unset = no OTLP push (metrics still scrape) |
| `WOCA_OTEL_EXPORTER_OTLP_INSECURE` | `false` to use TLS | insecure (in-cluster default) |
| `WOCA_OBS_ADDR` (worker) | probe listener | `:9091`, `off` disables |
| `WOCA_GRAFANA_URL` | ops-console link surfaced via `/api/health` → SPA | unset = hidden |

Tenant/business configuration (model allowlists, quotas, flags) never rides
this layer — it belongs to tenant-svc (§12.2 separation).

## Run it (compose)

```sh
# full stack first (deploy/selfhosted/README-FULL.md), then:
make ent-obs-up     # collector + jaeger + prometheus + alertmanager + grafana,
                    # and recreates app containers with OTLP env
open http://localhost:8085          # Grafana → dashboards → woca / woca Services
make ent-obs-logs
make ent-obs-down                   # stop the obs services
```

The compose stack is also the verification harness for the alert rules:
stop a worker container and `WocaServiceDown` fires within ~2 minutes.

## Kubernetes

`deploy/k8s/` re-declares the same pipeline with `kubernetes_sd_configs`
(pod annotation `metrics.woca.dev/scrape`) instead of static targets — see
its README. The Grafana dashboard JSON is shared between both layouts.

## Address sources for VM deployments (§12.1)

Registration/discovery is a deployment-layer concern; woca imports no
registry SDK and never self-registers. For VM/bare-metal private
deployments, config values may reference **Nacos** or **Consul** as the
address source, resolved once at startup over plain HTTP:

```
WOCA_DB="nacos://nacos:8848/WOCA/woca-db.dsn"          # group/dataId
WOCA_DB="consul://consul:8501/config/woca/db"          # KV key
# optional credentials:
WOCA_DISCOVERY_USERNAME / WOCA_DISCOVERY_PASSWORD      # Nacos login
WOCA_DISCOVERY_TOKEN                                   # Consul ACL token
```

Verify against a real Nacos with `deploy/vm/nacos-verify.sh`.

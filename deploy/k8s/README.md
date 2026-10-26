# woca on Kubernetes (design §5.3 / §12)

Plain-namespace manifests for the microservices deployment: woca-scheduler and
woca-worker roles plus the nacos registry and the observability pipeline.
No Helm chart yet — these files are the readable reference for one.

## Manifests

| file | contents |
|---|---|
| `00-namespace.yaml` | the `woca` namespace |
| `00-nacos.yaml` | standalone nacos StatefulSet + Service (dev-grade) + the `nacos-init` Job publishing the initial config pack |
| `01-infra-config.yaml` | ConfigMap `woca-infra` (OTLP endpoint via cluster DNS) + placeholder Secret `woca-secrets` (`WOCA_DB` DSN) |
| `10-observability.yaml` | OTel Collector, Prometheus (kubernetes_sd scraping + alert rules), Alertmanager, Grafana (provisioned), Jaeger |
| `20-woca-scheduler.yaml` | Deployment (probes `/healthz`), Service, HPA (CPU; SSE-connection scaling later) |
| `21-woca-worker.yaml` | Deployment (probes on `:9092`), Service, HPA (CPU fallback; KEDA queue-depth sketch in comments) |

## Registry coupling (§12.1): nacos, fail-closed

These roles are **registry-coupled**: `woca-scheduler` and `woca-worker`
register themselves with nacos, heartbeat every 5s, and take tenant/business
configuration from the delivered config pack (`woca-config.pack`,
`DEFAULT_GROUP`). `WOCA_NACOS_URL` is **required** — without it the process
refuses to start (fail-closed, no fallback posture); a registry that has
never delivered the initial pack is likewise fatal. See
`cmd/woca-scheduler/main.go`, `cmd/woca-worker/main.go`, and
`internal/ee/nacosrt/` for the contract; `deploy/vm/nacos-verify.sh` proves
it end-to-end against a real standalone nacos.

Practical consequences:

- `00-nacos.yaml` ships a dev-grade standalone nacos (single replica,
  embedded storage, auth off). **Production should point
  `WOCA_NACOS_URL` at an external nacos cluster instead**; when the
  registry has auth enabled, the roles authenticate via
  `WOCA_DISCOVERY_USERNAME` / `WOCA_DISCOVERY_PASSWORD`.
- The woca client speaks the nacos v1 HTTP API only, so the Service
  exposes just port 8848.
- The `nacos-init` Job publishes the empty pack
  (`{"version":2,"generation":"","settings":{}}`) — wait for it before
  rolling out 20/21, or the role pods crashloop on the missing pack.
- Cluster DNS still addresses *infrastructure* endpoints (OTel collector);
  nacos owns *role* registration and config delivery. Server-role DSNs may
  also be registry references (`nacos://nacos:8848/<group>/<dataId>`, see
  `internal/ee/discovery/`) — these manifests keep a plain DSN in
  `woca-secrets` instead.

## Role coverage

These manifests cover **woca-scheduler** and **woca-worker** only. The full
stack — woca-config (config plane), woca-tenant, woca-edge, woca-ops,
woca-bridge — is not packaged here; see `deploy/selfhosted/` (compose) and
`deploy/terraform/` for the complete role set, and `deploy/micro/` for the
per-role distribution packages.

## Configuration layers (§12.2)

- **Infrastructure config** (ports, DSNs, OTel endpoint) → ConfigMap/Secret
  in `01-infra-config.yaml`, managed by GitOps/Helm, applied with releases.
- **Tenant/business config** (model allowlists, quotas, feature flags) →
  the config plane (woca-config), published to nacos as the config pack and
  materialized by each role. Never move these into ConfigMaps.

## Deploy

```sh
# images are baked from the per-role distribution packages (adjust tags):
make dist-images   # woca-scheduler:local, woca-worker:local, ... from the
                   # make <role> tar packages (deploy/micro/README.md)
# load into your cluster (kind):
kind load docker-image woca-scheduler:local woca-worker:local

kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl apply -f deploy/k8s/00-nacos.yaml
kubectl -n woca wait --for=condition=complete job/nacos-init --timeout=120s
kubectl apply -f deploy/k8s/01-infra-config.yaml   # edit the DSN secret first
kubectl apply -f deploy/k8s/10-observability.yaml
# Grafana dashboard (shared with the compose stack):
kubectl -n woca create configmap woca-grafana-dashboards \
  --from-file=deploy/observability/grafana/provisioning/dashboards/
kubectl apply -f deploy/k8s/20-woca-scheduler.yaml
kubectl apply -f deploy/k8s/21-woca-worker.yaml
```

PostgreSQL/NATS are not deployed here: point `woca-secrets.WOCA_DB` at your
managed instance (CloudNativePG, RDS, …). The PG-queue (`SKIP LOCKED`)
serves until traffic justifies NATS (design §5.4).

## Scaling

- `woca-scheduler`: HPA on CPU (70%, 2–6). The better long-term signal is SSE
  connection count (`http_server_active_requests`) via a custom-metrics
  adapter.
- `woca-worker`: HPA on CPU is the always-available fallback; the target
  scaler is KEDA over pending runs (`SELECT count(*) FROM runs WHERE
  status='queued'`) — sketch included in `21-woca-worker.yaml`.

## Probes

Both roles expose `/healthz` (liveness+readiness): the server on its
business port, the worker on `:9092` (its default obs listener; the same
port is advertised to nacos). `/metrics` is scraped by Prometheus through
the `metrics.woca.dev/scrape|port|job` pod annotations — no public
exposure.

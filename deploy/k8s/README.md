# woca on Kubernetes (design §5.3 / §12)

Plain-namespace manifests for the microservices deployment: woca-server and
woca-worker roles plus the observability pipeline. No Helm chart yet —
these files are the readable reference for one.

## Manifests

| file | contents |
|---|---|
| `00-namespace.yaml` | the `woca` namespace |
| `01-infra-config.yaml` | ConfigMap `woca-infra` (OTLP endpoint via cluster DNS) + placeholder Secret `woca-secrets` (`WOCA_DB` DSN) |
| `10-observability.yaml` | OTel Collector, Prometheus (kubernetes_sd scraping + alert rules), Alertmanager, Grafana (provisioned), Jaeger |
| `20-woca-server.yaml` | Deployment (probes `/healthz`), Service, HPA (CPU; SSE-connection scaling later) |
| `21-woca-worker.yaml` | Deployment (probes on `:9091`), Service, HPA (CPU fallback; KEDA queue-depth sketch in comments) |

## Service discovery (§12.1)

Zero registry coupling: services address each other by **cluster DNS**
(`otel-collector.woca.svc`, `woca-server.woca.svc`), instance
registration/removal is the K8s lifecycle's job, and the code never
imports a registry client. Consul/Nacos are only address *sources* for VM
deployments (see `deploy/observability/README.md`).

## Configuration layers (§12.2)

- **Infrastructure config** (ports, DSNs, OTel endpoint) → ConfigMap/Secret
  in `01-infra-config.yaml`, managed by GitOps/Helm, applied with releases.
- **Tenant/business config** (model allowlists, quotas, feature flags) →
  tenant-svc, versioned and audited. Never move these into ConfigMaps.

## Deploy

```sh
# images are built from the repo root (adjust the tag/registry):
docker build -f deploy/selfhosted/Dockerfile.server -t woca-server:local .
docker build -f deploy/selfhosted/Dockerfile.worker -t woca-worker:local .
# load into your cluster (kind):
kind load docker-image woca-server:local woca-worker:local

kubectl apply -f deploy/k8s/00-namespace.yaml
kubectl apply -f deploy/k8s/01-infra-config.yaml   # edit the DSN secret first
kubectl apply -f deploy/k8s/10-observability.yaml
# Grafana dashboard (shared with the compose stack):
kubectl -n woca create configmap woca-grafana-dashboards \
  --from-file=deploy/observability/grafana/provisioning/dashboards/
kubectl apply -f deploy/k8s/20-woca-server.yaml
kubectl apply -f deploy/k8s/21-woca-worker.yaml
```

PostgreSQL/NATS are not deployed here: point `woca-secrets.WOCA_DB` at your
managed instance (CloudNativePG, RDS, …). The PG-queue (`SKIP LOCKED`)
serves until traffic justifies NATS (design §5.4).

## Scaling

- `woca-server`: HPA on CPU (70%, 2–6). The better long-term signal is SSE
  connection count (`http_server_active_requests`) via a custom-metrics
  adapter.
- `woca-worker`: HPA on CPU is the always-available fallback; the target
  scaler is KEDA over pending runs (`SELECT count(*) FROM runs WHERE
  status='queued'`) — sketch included in `21-woca-worker.yaml`.

## Probes

Both roles expose `/healthz` (liveness+readiness): the server on its
business port, the worker on `:9091`. `/metrics` is scraped by Prometheus
through the `metrics.woca.dev/scrape|port|job` pod annotations — no public
exposure.

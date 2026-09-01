# woca-collector (trace bridge)

The single ingestion point for woca's LLM-trace reporting. Every role
pushes credential-less OTLP here; the collector owns the two things woca's
exporter deliberately does not — the Langfuse ingestion path and its Basic
auth — plus the tag rename onto Langfuse's conventions:

```
roles ──OTLP──► woca-collector ──► langfuse-web /api/public/otel (+ session.id mapping)
                        └────────► observability collector (full stack: Jaeger/Prometheus)
```

Why a hop instead of SDK-direct: credentials stay in exactly one place,
the roles learn only one address, and the observability stack stays
pluggable behind the same bridge.

## Placement

- **dev** (`config-dev.yaml`): always on, on the flat dev network (dev
  is a 联调 harness — no zone model). When the langfuse profile is off,
  exports fail fast and drop.
- **full** (`config-full.yaml`): the infra zone — the same zone as NATS,
  ladyM, nacos and Langfuse, so the roles' existing infra leg carries the
  push (no extra network interface) and the fan-out to langfuse-web and
  the observability overlay's collector (Jaeger/Prometheus; dropped when
  the overlay is off) stays intra-zone.
- **standalone** (`config-standalone.yaml` + `docker-compose.collector.yml`):
  run the bridge as its own unit; roles reach it through the nacos entry
  (`DEFAULT_GROUP/woca-obs.otlp`), the Langfuse address and credentials
  come from the environment.

## Address discovery

The roles' `WOCA_OTEL_EXPORTER_OTLP_ENDPOINT` accepts a deployment-layer
registry reference, resolved once at startup exactly like `WOCA_DB`:

```
WOCA_OTEL_EXPORTER_OTLP_ENDPOINT=nacos://nacos:8848/DEFAULT_GROUP/woca-obs.otlp
```

Both stack compose files publish that entry (`woca-obs.otlp` → the local
bridge), so the address lives in the registry, not in the roles' config.

## Delivery contract

Async and bounded end to end: recording a span enqueues into an in-memory
batch (no network on the request path), a background flush pushes every 5s,
overflow drops, and a downstream outage never affects the roles (fail-fast
exporters here; backoff in the Go SDK). Ingestion into Langfuse itself is
asynchronous on the Langfuse side (queue → worker → ClickHouse) — expect
about a minute of delay.

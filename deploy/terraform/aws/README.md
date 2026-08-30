# woca private deployment on AWS — zone-first Terraform

Hand-written Terraform that builds the enterprise network architecture of
`deploy/selfhosted/docker-compose.full.yml` for real on AWS: every compose
zone becomes a subnet, every dual-homed service's legs become ENIs
on the node that runs it, and the compose DOCKER-USER isolation matrix
becomes NACLs + security groups. The compose subsets under `templates/`
mirror the original file service-by-service (no generation tooling; edit
both sides by hand and keep the correspondence readable).

## Zone → node → service mapping (five zones: dmz / app / data / infra / mgmt)

| tf subnet | compose zone | node | services (compose subsets in templates/) | extra ENIs (bridge legs) |
|-----------|--------------|------|-------------------------------------------|--------------------------|
| dmz (public) | woca-dmz | dmz-1 | gateway (nginx) | infra (ladym stats, langfuse UI proxy), mgmt (config console proxy) |
| app | woca-app | app-N (default 1) | woca-server, woca-worker, woca-tenant | data, infra |
| data | woca-data | data-1 | woca-pg-primary, woca-pg-replica | — |
| infra | woca-infra | infra-1 | nats-js-1/2/3, nats-init, ladym-pg, ladym-api-1/2, ladym-worker (dict image), **nacos + nacos-init, woca-collector (trace bridge), langfuse-\* (web/worker/pg/clickhouse/redis/minio)** | — |
| mgmt | woca-mgmt | mgmt-1 | woca-config, woca-ops | data, infra, app (config/ops legs) |

(The compose full stack carries the same five zones since the 2026-08-30
re-sync — zone names map 1:1; the former edge/mq/ladym/obs segments are
gone there too.)

**nacos belongs to infra, not mgmt**: nacosrt is a hard startup gate for
every business role (registry + heartbeat + config-pack delivery — roles
refuse to start without it), which is runtime infrastructure, not a
management surface. Keeping the mgmt zone console-only means an mgmt
outage never blocks role restarts/scale-outs — and the app nodes' compose
mgmt leg (which existed only to reach nacos) folds away, 5 legs → 3.

**The trace bridge and Langfuse ride infra for the same reason** (the
compose obs segment folds into infra, as woca-mq + ladym-data did): the
woca-collector is on every role's hot path — roles address it through the
nacos entry `woca-obs.otlp` exactly like the DSN, never learning the
Langfuse address or credentials — and Langfuse's backing stores are
platform infrastructure. The collector fans out to langfuse-web on the
same node (docker bridge, no network policy involved); the Langfuse UI
reaches ops through the DMZ entry: NLB :3000 → gateway nginx →
infra:3000.

The woca-edge zone splits into dmz + app subnets (the v2 research
upgrade); woca-mq and ladym-data merge into the infra zone; every other
zone maps 1:1. Dual-homed services keep their legs as ENIs, so legal
cross-zone flows (gateway→ladym/config, server→pg/nats/nacos/ladym) stay
IN-zone at the subnet each leg lives in — exactly the topology the
DOCKER-USER matrix enforced on one host.

## Prerequisites

1. Images in ECR (see `ecr.tf` for the repository set):
   - Note: the woca-edge access point (client tunnels, /gw/obs public
     telemetry push) is not yet modeled in this reference — the DMZ
     node currently runs the nginx gateway half only; adding the edge
     node is tracked with the v1.5 topology follow-up.
   - woca role images: `GOARCH=amd64 make dist-images` (arm64 hosts: plain
     `make dist-images`) builds every role package (`make <role>` tar, the
     packaged start.sh as entrypoint) into `woca-<role>:local` — then tag
     and push. See deploy/micro/README.md.
   - ladyM: build the dict-baked image
     (`docker build -f deploy/selfhosted/Dockerfile.ladym-dict` on top of
     the operator-built `ladym-enterprise:local`) and push it.
   - Third-party images (postgres:16-alpine, postgres:17-alpine,
     nats:2.11-alpine, nacos/nacos-server:v2.4.3, nginx:1.29-alpine,
     pgvector/pgvector:pg17, synadia/nats-box, curlimages/curl,
     redis:7-alpine, clickhouse/clickhouse-server:25.12,
     cgr.dev/chainguard/minio, docker.langfuse.com/langfuse/langfuse[:-worker]:4.24.0,
     ghcr.io/open-telemetry/...collector-contrib:0.136.0): retag+push
     into ECR or let the private subnets pull them via NAT (default).
2. `terraform init` (S3 backend is a placeholder — configure for real use,
   see versions.tf).
3. Copy `terraform.tfvars.example` → `terraform.tfvars`, set the key pair
   and passwords (or rely on the random_password defaults and read them
   from state).

## Deploy

```sh
terraform apply
```

Then bootstrap the config plane exactly like the compose stack: sign in to
the config console (NLB :9086), issue a local admin key, and write the
provider/storage settings — same operator flow as README-FULL.md.

## Network policy (the real thing this stack exists for)

- **NACLs** (`network.tf`): per-subnet, stateless — the DOCKER-USER matrix
  translated to "intra-subnet allow + explicit cross flows + deny other
  VPC CIDRs". DMZ additionally admits the published service ports from the
  internet (NLB preserves client IPs).
- **SGs** (`security.tf`): stateful instance-level policy — intra-zone
  self-reference (docker bridge semantics), the one genuine cross-zone
  flow (dmz→app:8080), SSH from `admin_cidr` only.
- **DMZ egress** (`allow_dmz_egress`, default false): the dmz subnet has no
  NAT route — the gateway node cannot phone out. Set true (adds a NAT
  route + opens the SG) only for first-boot image pulls, then lock it.
- **Trace bridge flows**: app roles → collector `:4318` rides their
  existing infra-leg ENIs (same path as nats:4222 / nacos:8848 — intra-
  subnet at the NACL/SG layers, no new rules); the gateway → langfuse-web
  `:3000` reverse proxy is one explicit cross flow (NACL + SG).

## Known v1 boundaries (honest list)

- Single AZ (`availability_zone` variable); multi-AZ is a later profile.
- app_count default 1: scaling to 2+ needs a shared sessions/crypto volume
  (EFS/NFS — gap report P1), not just instance count.
- nacos standalone (single point, as in compose — gap report P1); the
  nacos-init publishes are parameterized here (node IPs from terraform),
  which the original compose hardcodes.
- TLS terminates at the NLB or none; the gateway serves plain HTTP behind
  it (add ACM + NLB TLS listener when certificates exist).
- Upgrades: `terraform taint`/replace rolls a whole node; in-place image
  refresh is `docker compose pull && up -d` on the node.
- The infra node now also carries Langfuse (web/worker + pg/clickhouse/
  redis/minio) — size the instance accordingly (ClickHouse is the memory
  hog); splitting the obs workloads onto a dedicated node is a later
  profile. Langfuse's browser-side media upload (MinIO direct) is not
  exposed through the DMZ — add an nginx location if needed.
- Langfuse sign-in callbacks: set `LANGFUSE_NEXTAUTH_URL=http://<nlb-
  host>:3000` on the infra node's `/etc/woca/.env` (deploy-time, see
  terraform.tfvars.example) so NextAuth sees the external origin.

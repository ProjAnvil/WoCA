# woca private deployment on AWS — zone-first Terraform

Hand-written Terraform that builds the enterprise network architecture of
`deploy/selfhosted/docker-compose.full.yml` for real on AWS: every compose
network zone becomes a subnet, every dual-homed service's legs become ENIs
on the node that runs it, and the compose DOCKER-USER isolation matrix
becomes NACLs + security groups. The compose subsets under `templates/`
mirror the original file service-by-service (no generation tooling; edit
both sides by hand and keep the correspondence readable).

## Zone → node → service mapping (five zones: dmz / app / data / infra / mgmt)

| tf subnet | compose networks | node | services (compose subsets in templates/) | extra ENIs (bridge legs) |
|-----------|------------------|------|-------------------------------------------|--------------------------|
| dmz (public) | woca-edge (gateway half) | dmz-1 | gateway (nginx) | infra (ladym stats), mgmt (config console proxy) |
| app | woca-edge (business half) | app-N (default 1) | woca-server, woca-worker, woca-tenant | data, infra |
| data | woca-pg | data-1 | woca-pg-primary, woca-pg-replica | — |
| infra | woca-mq + ladym-data | infra-1 | nats-js-1/2/3, nats-init, ladym-pg, ladym-api-1/2, ladym-worker (dict image), **nacos + nacos-init** | — |
| mgmt | woca-mgmt (minus nacos) | mgmt-1 | woca-config, woca-ops | data, infra, app (config/ops legs) |

**nacos belongs to infra, not mgmt**: nacosrt is a hard startup gate for
every business role (registry + heartbeat + config-pack delivery — roles
refuse to start without it), which is runtime infrastructure, not a
management surface. Keeping the mgmt zone console-only means an mgmt
outage never blocks role restarts/scale-outs — and the app nodes' compose
mgmt leg (which existed only to reach nacos) folds away, 5 legs → 3.

The woca-edge zone splits into dmz + app subnets (the v2 research
upgrade); woca-mq and ladym-data merge into the infra zone; every other
zone maps 1:1. Dual-homed services keep their legs as ENIs, so legal
cross-zone flows (gateway→ladym/config, server→pg/nats/nacos/ladym) stay
IN-zone at the subnet each leg lives in — exactly the topology the
DOCKER-USER matrix enforced on one host.

## Prerequisites

1. Images in ECR (see `ecr.tf` for the repository set):
   - woca binaries: `make` release chain tags (P0-1 upstream), then
     `docker build` + `docker push` each `deploy/selfhosted/Dockerfile.*`
     for the target arch (default arm64/graviton).
   - ladyM: build the dict-baked image
     (`docker build -f deploy/selfhosted/Dockerfile.ladym-dict` on top of
     the operator-built `ladym-enterprise:local`) and push it.
   - Third-party images (postgres:16-alpine, nats:2.11-alpine,
     nacos/nacos-server:v2.4.3, nginx:1.29-alpine, pgvector/pgvector:pg17,
     synadia/nats-box, curlimages/curl): retag+push into ECR or let the
     private subnets pull them via NAT (default).
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

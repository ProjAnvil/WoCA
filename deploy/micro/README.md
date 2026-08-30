# woca microservices package

A self-contained distribution of every microservice role plus one generic
start/stop controller. Build it with, from the repo root:

```bash
make dist-micro
# → build/dist/woca-micro-<version>-<os>-<arch>.tar.gz (+ sha256)
```

## Layout

```text
woca-micro/
├── bin/           every server-role binary (woca-server, woca-worker,
│                  woca-config, woca-edge, woca-ops, woca-bridge, woca-tenant)
├── scripts/svc.sh the generic controller — one script for every role
├── env/           per-role assembly: common.env sourced first, then
│                  <role>.env (a role file overrides shared values)
├── logs/          created on demand; each role's stdout/stderr
├── run/           created on demand; pid files + svc.log (start/stop audit)
├── README.md      this file
└── VERSION        git describe of the build
```

## Usage

```bash
tar -xzf woca-micro-*.tar.gz && cd woca-micro
$EDITOR env/common.env            # WOCA_DB, WOCA_NACOS_URL, ...
$EDITOR env/woca-server.env       # role-specific values

scripts/svc.sh woca-server start
scripts/svc.sh woca-server status
scripts/svc.sh status             # every role in bin/
scripts/svc.sh woca-server restart
scripts/svc.sh woca-server stop
```

The script has **no role-specific branches**: anything particular to a
role (bind address, TLS files, upstream URLs) lives only in its env file.
Adding a role means dropping the binary into `bin/` and adding an env
file.

## PID recording

- `run/<role>.pid` — the live pid; stale files (process gone, or the pid
  was recycled to an unrelated process) are detected and cleaned.
- `run/svc.log` — one tab-separated line per start/stop attempt:
  `timestamp  role  action  pid  outcome`. This is the audit trail of
  every start and stop the package performed.

```text
2026-08-30T21:04:11+0800	woca-ops	start	4817	ok
2026-08-30T21:05:02+0800	woca-ops	stop	4817	terminated by SIGTERM after 0s
```

## Stop semantics

Stop sends SIGTERM — every role's graceful shutdown path (the webfx
lifecycle; the edge parks clients up to 30s and streams SSE, so its grace
is 40s) — and waits `WOCA_SVC_STOP_TIMEOUT` (default 45s) before SIGKILL.
The outcome (graceful or forced) is recorded in `run/svc.log`.

## What this package is not

This is the bare-metal / VM distribution form. The compose stacks live in
`deploy/selfhosted`, the Kubernetes manifests in `deploy/k8s`, and the
Terraform reference in `deploy/terraform` — those remain the
container-based deployment paths.

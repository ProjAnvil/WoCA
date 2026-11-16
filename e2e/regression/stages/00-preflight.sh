#!/usr/bin/env bash
# Stage 00 — preflight: dependencies, the docker engine, free ports, and a
# clean runtime directory. Nothing here touches the repo's build state.
set -u
source "$(dirname "$0")/../lib.sh"
stage "00-preflight"

section "dependencies"
for c in go docker curl jq git bun; do
  need_cmd "$c"
done
ok "go/docker/curl/jq/git/bun present"

section "docker engine"
docker info >/dev/null 2>&1 || die "docker engine unreachable — start OrbStack (or Docker Desktop) first"
ok "docker engine reachable ($(docker version --format '{{.Server.Version}}' 2>/dev/null))"

section "ports"
personal_busy=""
for p in "$REG_PORT_PERSONAL" "$REG_PORT_FAKEMODEL"; do
  if ! port_free "$p"; then personal_busy="$personal_busy $p"; fi
done
[ -z "$personal_busy" ] || die "personal regression ports busy:$personal_busy"
ok "personal ports free ($REG_PORT_PERSONAL, $REG_PORT_FAKEMODEL)"

ent_busy=""
for p in 8080 9082 9083 9086 9091 8453 3490; do
  if ! port_free "$p"; then ent_busy="$ent_busy $p"; fi
done
if [ -n "$ent_busy" ]; then
  die "enterprise ports busy:$ent_busy — stop the stacks using them (docker ps / make dev-down) and rerun"
fi
ok "enterprise ports free (8080 9082 9083 9086 9091 8453 3490)"

section "runtime dir"
mkdir -p "$BIN" "$HOME_DIR" "$WS" "$LOGS" "$pids"
: >"$RUN/audit.log"
[ "${REG_KEEP_STATE:-0}" = "1" ] || : >"$STATE"
ok "runtime at $RUN"

section "repositories"
[ -d "$REPO_ROOT/deploy/selfhosted" ] || die "run from the woca repo"
ok "repo root $REPO_ROOT"

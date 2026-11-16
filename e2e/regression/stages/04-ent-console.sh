#!/usr/bin/env bash
# Stage 04 — enterprise control plane on the REAL dev compose stack:
#   boot (config/ops/edge/nacos/pg/nats/ladym) → gated console logins →
#   provider key + base URL through the config console → model preset →
#   config dispatch live → tenant + woca_ key + enrollment token on the
#   ops console. The fake model keeps running for stage 05's bridge chat.
set -u
source "$(dirname "$0")/../lib.sh"
stage "04-ent-console"
cd "$REPO_ROOT"

CFG="http://127.0.0.1:9086"
OPS="http://127.0.0.1:9091"
EDGE="http://127.0.0.1:8453"
FAKE="http://127.0.0.1:$REG_PORT_FAKEMODEL"
FAKE_DOCKER="http://host.docker.internal:$REG_PORT_FAKEMODEL"

ent_compose() { docker compose -f "$ENT_COMPOSE_FILE" --env-file "$ENT_ENV_FILE" "$@"; }

# ── the scripted model (host) serves both editions ────────────────────
section "start fake model (host)"
"$BIN/reg-fakemodel" --addr "127.0.0.1:$REG_PORT_FAKEMODEL" >"$LOGS/fakemodel-ent.log" 2>&1 &
reg_pid fakemodel $!
wait_http "$FAKE/healthz" 20 || die "fake model did not come up"
ok "fake model :$REG_PORT_FAKEMODEL (docker reach: $FAKE_DOCKER)"

# ── boot the dev stack ────────────────────────────────────────────────
# Hermetic start: drop any previous project state (containers, networks,
# volumes included) so a rerun is byte-for-byte reproducible and orphan
# containers from older compose revisions cannot block name allocation.
section "compose down (hermetic reset)"
ent_compose --profile bridge --profile langfuse down -v --remove-orphans --timeout 20 >"$LOGS/ent-down.log" 2>&1 || true
ok "previous project state dropped"

section "compose up (dev stack, no bridge)"
if ent_compose up -d --build >"$LOGS/ent-up.log" 2>&1; then
  ok "compose up"
else
  tail -40 "$LOGS/ent-up.log" >&2
  die "compose up failed (see $LOGS/ent-up.log)"
fi

section "wait for the planes"
wait_http "$EDGE/api/health" 300 200 || die "edge :8453 did not come up (docker compose -f $ENT_COMPOSE_FILE --env-file $ENT_ENV_FILE logs edge)"
ok "edge :8453 healthy"
wait_http "$CFG/" 120 200 || die "config console :9086 did not come up"
ok "config console :9086 up"
wait_http "$OPS/" 120 200 || die "ops console :9091 did not come up"
ok "ops console :9091 up"

# ── gated console logins ──────────────────────────────────────────────
section "config console login (gated)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$CFG/api/console/login" \
  -H 'Content-Type: application/json' -d '{"username":"reg-admin","password":"WRONG"}')
[ "$code" = "401" ] && ok "wrong password rejected (401)" || fail "wrong-password gate" "got $code"
code=$(curl -s -c "$RUN/cfg.jar" -o "$LOGS/cfg-login.json" -w '%{http_code}' -X POST "$CFG/api/console/login" \
  -H 'Content-Type: application/json' -d '{"username":"reg-admin","password":"reg-console-pass"}')
[ "$code" = "200" ] || die "config console login -> $code ($(cat "$LOGS/cfg-login.json"))"
ok "config console signed in (reg-admin)"

section "ops console login (gated)"
code=$(curl -s -c "$RUN/ops.jar" -o "$LOGS/ops-login.json" -w '%{http_code}' -X POST "$OPS/api/console/login" \
  -H 'Content-Type: application/json' -d '{"username":"reg-admin","password":"reg-console-pass"}')
[ "$code" = "200" ] || die "ops console login -> $code ($(cat "$LOGS/ops-login.json"))"
ok "ops console signed in (reg-admin)"

# ── provider + model through the config console ───────────────────────
section "configure provider (PUT /api/settings)"
code=$(curl -s -b "$RUN/cfg.jar" -o "$LOGS/cfg-put-settings.json" -w '%{http_code}' -X PUT "$CFG/api/settings" \
  -H 'Content-Type: application/json' -d '{
    "provider": "anthropic", "model": "fake-claude",
    "anthropic_api_key": "reg-dummy-key",
    "anthropic_base_url": "'"$FAKE_DOCKER"'"
  }')
[ "$code" = "200" ] || die "config PUT /api/settings -> $code ($(cat "$LOGS/cfg-put-settings.json"))"
ok "provider anthropic -> fake model, key stored"

section "model preset (POST /api/models)"
code=$(curl -s -b "$RUN/cfg.jar" -o "$LOGS/cfg-model.json" -w '%{http_code}' -X POST "$CFG/api/models" \
  -H 'Content-Type: application/json' -d '{
    "name": "fake-claude", "provider": "anthropic", "model": "fake-claude",
    "max_tokens": 4096, "temperature": 0,
    "base_url": "'"$FAKE_DOCKER"'", "api_key": "reg-dummy-key"
  }')
[ "$code" = "200" ] || die "POST /api/models -> $code ($(cat "$LOGS/cfg-model.json"))"
preset_id=$(jq -r '.id // empty' "$LOGS/cfg-model.json")
[ -n "$preset_id" ] && ok "model preset saved ($preset_id)" || fail "model preset id" "no id in response"
state_set preset_id "$preset_id"

section "machine catalog (GET /gw/models needs enrollment — skipped here)"
# /gw/* is machine-authenticated; the bridge exercises it in stage 05.

section "config dispatch reached the roles"
# Contract asserted: business roles are live AND the delivered pack carries
# the operator's provider. NOT asserted: config_lagging==false — roles
# materialize packs back into the shared dev DB (restamping updated_at), so
# the live generation forever chases the ack generation in this topology;
# the flag flaps by construction (ticket: pack generation should
# fingerprint the exported content, not the live store sums).
live_roles=""
for _ in $(seq 1 20); do
  body=$(curl -s -b "$RUN/cfg.jar" "$CFG/api/config/status")
  live_roles=$(printf '%s' "$body" | jq -r '[.services[] | select(.live == true) | .role] | join(",")')
  case "$live_roles" in *scheduler*worker*|*worker*scheduler*) break ;; esac
  sleep 1.5
done
case "$live_roles" in
  *scheduler*worker*|*worker*scheduler*) ok "roles live in the config inventory ($live_roles)" ;;
  *) die "scheduler/worker never live: $live_roles" ;;
esac

pack=$(docker exec woca-dev-nacos sh -c 'curl -s "http://localhost:8848/nacos/v1/cs/configs?dataId=woca-config.pack&group=DEFAULT_GROUP"' 2>/dev/null)
pack_provider=$(printf '%s' "$pack" | jq -r '.settings.provider // empty')
pack_key=$(printf '%s' "$pack" | jq -r '.secrets["anthropic_api_key"] // empty')
[ "$pack_provider" = "anthropic" ] && [ "$pack_key" = "reg-dummy-key" ] \
  && ok "pack in the registry carries provider+key (generation $(printf '%s' "$pack" | jq -r '.generation'))" \
  || die "pack missing the operator's provider (provider=$pack_provider key=${pack_key:+set})"

# ── tenant + key + enrollment on the ops console ──────────────────────
section "tenant + woca_ key"
code=$(curl -s -b "$RUN/ops.jar" -o "$LOGS/ops-tenant.json" -w '%{http_code}' -X POST "$OPS/api/tenant/tenants" \
  -H 'Content-Type: application/json' -d '{"name":"regression"}')
[ "$code" = "200" ] || die "create tenant -> $code ($(cat "$LOGS/ops-tenant.json"))"
tenant_id=$(jq -r '.id // empty' "$LOGS/ops-tenant.json")
[ -n "$tenant_id" ] || die "no tenant id"
ok "tenant created ($tenant_id)"

code=$(curl -s -b "$RUN/ops.jar" -o "$LOGS/ops-key.json" -w '%{http_code}' -X POST "$OPS/api/tenant/users" \
  -H 'Content-Type: application/json' -d '{"remark":"reg-client","role":"member","tenant_id":"'"$tenant_id"'"}')
[ "$code" = "200" ] || die "issue key -> $code ($(cat "$LOGS/ops-key.json"))"
user_key=$(jq -r '.key // empty' "$LOGS/ops-key.json")
case "$user_key" in woca_*) : ;; *) die "issued key not woca_ prefixed: $user_key" ;; esac
ok "woca_ key issued for reg-client (${user_key:0:12}…)"
state_set tenant_id "$tenant_id"
state_set user_key "$user_key"

section "enrollment token"
code=$(curl -s -b "$RUN/ops.jar" -o "$LOGS/ops-enroll.json" -w '%{http_code}' -X POST "$OPS/api/gw/enrollments" \
  -H 'Content-Type: application/json' -d '{}')
[ "$code" = "200" ] || die "issue enrollment -> $code ($(cat "$LOGS/ops-enroll.json"))"
enroll=$(jq -r '.enrollment_token // empty' "$LOGS/ops-enroll.json")
case "$enroll" in woca_enroll_*) : ;; *) die "enrollment token not woca_enroll_ prefixed: $enroll" ;; esac
ok "enrollment token issued (single-use)"
state_set enroll_token "$enroll"

# The fake model keeps running — stage 05 drives the bridge chat through
# it. run.sh's teardown (or REG keep-mode) owns its lifetime from here.

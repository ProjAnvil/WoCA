#!/usr/bin/env bash
# Stage 05 — the enterprise CLIENT path on the real stack: enroll the
# bridge container with the one-time token, sign in at the EDGE with the
# tenant's woca_ key, and drive a full conversation through the tunnel
# (edge -> bridge -> scripted model), answering the bridge's approval
# interrupt like a user would. Asserts the workspace file landed INSIDE
# the bridge container.
set -u
source "$(dirname "$0")/../lib.sh"
stage "05-ent-client"
cd "$REPO_ROOT"

EDGE="http://127.0.0.1:8453"
OPS="http://127.0.0.1:9091"
user_key=$(state_get user_key)
enroll=$(state_get enroll_token)
[ -n "$user_key" ] && [ -n "$enroll" ] || die "stage 04 facts missing (user_key/enroll_token) — run stage 04 first"

ent_compose() { docker compose -f "$ENT_COMPOSE_FILE" --env-file "$ENT_ENV_FILE" "$@"; }

# ── enroll the bridge ─────────────────────────────────────────────────
section "compose up bridge (enrolled via one-time token)"
if DEV_ENROLL_TOKEN="$enroll" ent_compose --profile bridge up -d --build bridge >"$LOGS/ent-bridge-up.log" 2>&1; then
  ok "bridge container up"
else
  tail -40 "$LOGS/ent-bridge-up.log" >&2
  die "bridge up failed (see $LOGS/ent-bridge-up.log)"
fi

section "wait for enrollment"
bridges=""
for _ in $(seq 1 60); do
  bridges=$(curl -s -b "$RUN/ops.jar" "$OPS/api/gw/bridges")
  n=$(printf '%s' "$bridges" | jq '. | length')
  [ "${n:-0}" -ge 1 ] && break
  sleep 3
done
[ "${n:-0}" -ge 1 ] || die "bridge never enrolled (ops sees nothing: $bridges)"
ok "bridge registered in the ops inventory ($(printf '%s' "$bridges" | jq -c '.[0] | keys | join(",")') row present)"

# ── client login at the EDGE ──────────────────────────────────────────
section "client login at edge (woca_ key)"
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$EDGE/api/auth/login" \
  -H 'Content-Type: application/json' -d '{"api_key":"woca_DEFINITELY_WRONG"}')
[ "$code" = "401" ] && ok "wrong key rejected at edge (401)" || fail "wrong-key gate" "got $code"
code=$(curl -s -c "$RUN/edge.jar" -o "$LOGS/edge-login.json" -w '%{http_code}' -X POST "$EDGE/api/auth/login" \
  -H 'Content-Type: application/json' -d '{"api_key":"'"$user_key"'"}')
[ "$code" = "200" ] || die "edge login -> $code ($(cat "$LOGS/edge-login.json"))"
ok "edge login ok ($(jq -r '.role // "?"' "$LOGS/edge-login.json") role, tenant $(jq -r '.tenant_id // "?"' "$LOGS/edge-login.json"))"

got=$(curl -s -b "$RUN/edge.jar" "$EDGE/api/edge/bridges" | jq '. | length')
[ "${got:-0}" -ge 1 ] && ok "edge lists the enrolled bridge" || die "edge bridge list empty"

# ── tunneled conversation ─────────────────────────────────────────────
section "conversation through the tunnel (edge -> bridge -> model)"
sid=$(curl -s -b "$RUN/edge.jar" -X POST "$EDGE/api/sessions" -H 'Content-Type: application/json' \
  -d '{"workspace":"/workspace"}' | jq -r '.session_id')
[ -n "$sid" ] && [ "$sid" != "null" ] || die "tunneled session create failed"

rid=$(curl -s -b "$RUN/edge.jar" -X POST "$EDGE/api/runs" -H 'Content-Type: application/json' \
  -d '{"session_id":"'"$sid"'","workspace":"/workspace","prompt":"woca-reg:chat"}' | jq -r '.run_id')
[ -n "$rid" ] && [ "$rid" != "null" ] || die "tunneled run create failed"

sse="$LOGS/sse-ent.sse"
: >"$sse"
curl -sN -b "$RUN/edge.jar" --max-time 300 "$EDGE/api/runs/$rid/events" >"$sse" 2>>"$RUN/logs/sse.err" &
cpid=$!

answered=0
final=""
for _ in $(seq 1 600); do
  final=$(curl -s -b "$RUN/edge.jar" "$EDGE/api/sessions/$sid" | jq -r '[.messages[] | select(.role == "ai") | .content] | last // empty' 2>/dev/null)
  case "$final" in *"woca-reg:chat complete"*) break ;; esac
  n_int=$(awk '$0 == "event: interrupt" { grab = 1; next } grab && /^data: / { if ($0 ~ /"cards"/) n++; grab = 0 } END { print n + 0 }' "$sse")
  if [ "$n_int" -gt "$answered" ]; then
    answered=$((answered + 1))
    [ "$answered" -le 6 ] || { kill "$cpid" 2>/dev/null; die "ent: too many interrupts"; }
    ns=$(awk -v want="$answered" '
      $0 == "event: interrupt" { grab = 1; next }
      grab && /^data: / { if ($0 ~ /"cards"/) n++; if (n == want) { line = $0; sub(/^data: /, "", line); print line; exit } grab = 0 }
    ' "$sse" | jq -r '.cards[0].ns // .cards[0].id // empty')
    [ -n "$ns" ] || { kill "$cpid" 2>/dev/null; die "ent: interrupt without card ns"; }
    code=$(curl -s -b "$RUN/edge.jar" -o "$LOGS/ent-answer-$answered.json" -w '%{http_code}' -X POST "$EDGE/api/runs/$rid/answer" \
      -H 'Content-Type: application/json' -d '{"answers":[{"key":"'"$ns"'","value":{"approve":true}}]}')
    [ "$code" = "200" ] || { kill "$cpid" 2>/dev/null; die "ent answer -> $code ($(cat "$LOGS/ent-answer-$answered.json"))"; }
  fi
  grep -q '^event: error' "$sse" 2>/dev/null && { kill "$cpid" 2>/dev/null; die "ent run errored: $(tail -4 "$sse")"; }
  sleep 0.5
done
kill "$cpid" 2>/dev/null

case "$final" in
  *"woca-reg:chat complete"*) ok "final reply delivered through the tunnel" ;;
  *) die "ent conversation never completed (last: $final; sse tail: $(tail -4 "$sse" | tr '\n' ' '))" ;;
esac

grep -q '^event: interrupt' "$sse" && ok "bridge approval surfaced through the tunnel and was answered" || fail "ent HITL" "no interrupt in tunnel stream"
grep -q '^event: tool_call' "$sse" && ok "tool call streamed through the tunnel" || fail "ent tool_call" "absent"

# The product landed inside the bridge container's workspace volume.
inside=$(docker exec woca-dev-bridge cat /workspace/out.txt 2>/dev/null)
case "$inside" in *woca-reg-chat-side*) ok "workspace file landed inside the bridge container" ;; *) fail "bridge workspace file" "out.txt: '$inside'" ;; esac

state_set ent_sid "$sid"

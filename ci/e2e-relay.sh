#!/usr/bin/env bash
# ci/e2e-relay.sh — personal relay end-to-end (design §10), all on
# loopback with throwaway state:
#   create key → bridge connect → relay serve + woca serve → client login
#   → tunneled API → SSE headers → revoke → client 401 + bridge revoked.
set -euo pipefail
cd "$(dirname "$0")/.."

TMP="$(mktemp -d)"
RELAY_PID=""
WOCA_PID=""
cleanup() {
  [ -n "$WOCA_PID" ] && kill "$WOCA_PID" 2>/dev/null || true
  [ -n "$RELAY_PID" ] && kill "$RELAY_PID" 2>/dev/null || true
  rm -rf "$TMP"
}
trap cleanup EXIT

RELAY_ADDR=127.0.0.1:18443
WOCA_ADDR=127.0.0.1:13484

go build -o "$TMP/woca-relay" ./cmd/woca-relay
go build -o "$TMP/woca" ./cmd/woca

# 1. Key create — the full key is printed on stdout exactly once.
KEY="$("$TMP/woca-relay" --db "$TMP/relay.db" key create e2e | awk '/^wcr_/{print $1; exit}')"
[ -n "$KEY" ] || { echo "FAIL: no key minted"; exit 1; }

# 2. Relay serve (loopback plain HTTP is legal; --insecure not needed).
"$TMP/woca-relay" --db "$TMP/relay.db" serve --addr "$RELAY_ADDR" --log-level warn &
RELAY_PID=$!

# 3. Bridge: persist the config, then serve (the tunnel starts with it).
export WOCA_DB="$TMP/woca.db"
"$TMP/woca" relay connect --url "http://$RELAY_ADDR" --key "$KEY" >/dev/null
"$TMP/woca" serve --addr "$WOCA_ADDR" --no-browser --log-level warn &
WOCA_PID=$!

for _ in $(seq 1 50); do
  curl -fsS "http://$RELAY_ADDR/api/health" >/dev/null 2>&1 && break || sleep 0.2
done
for _ in $(seq 1 75); do
  curl -fsS "http://$WOCA_ADDR/api/health" >/dev/null 2>&1 && break || sleep 0.4
done

# 4. Client login (cookie jar) — the body token contract is asserted too.
curl -fsS -c "$TMP/cookies" -H 'Content-Type: application/json' \
  -d "{\"api_key\":\"$KEY\"}" "http://$RELAY_ADDR/api/auth/login" > "$TMP/login.json"
grep -q '"token"' "$TMP/login.json" || { echo "FAIL: login response lacks the body token (native contract)"; exit 1; }

# 5. Tunneled API: the relay forwards /api/sessions to the bridge.
OUT=""
for _ in $(seq 1 50); do
  OUT="$(curl -fsS -b "$TMP/cookies" "http://$RELAY_ADDR/api/sessions" 2>/dev/null)" && break || sleep 0.5
done
echo "${OUT:-}" | grep -q '\[' || { echo "FAIL: tunneled /api/sessions: ${OUT:-<empty>}"; exit 1; }

# 6. SSE through the tunnel: event-stream content type survives the
# flush-before-headers order, X-Accel-Buffering is set, hop-by-hop
# Connection does not leak.
HEADERS="$(curl -sS -b "$TMP/cookies" -D - -o /dev/null --max-time 5 "http://$RELAY_ADDR/api/events" 2>/dev/null || true)"
echo "$HEADERS" | grep -qi 'content-type: text/event-stream' || { echo "FAIL: SSE content type"; echo "$HEADERS"; exit 1; }
echo "$HEADERS" | grep -qi 'x-accel-buffering: no' || { echo "FAIL: X-Accel-Buffering"; echo "$HEADERS"; exit 1; }

# 7. Revoke → the sweep cascades: client requests hit the login gate,
# the bridge's status marks the key revoked.
"$TMP/woca-relay" --db "$TMP/relay.db" key revoke e2e >/dev/null
sleep 2 # revocation sweep interval (1s) plus margin
CODE="$(curl -s -o /dev/null -w '%{http_code}' -b "$TMP/cookies" "http://$RELAY_ADDR/api/sessions")"
[ "$CODE" = "401" ] || { echo "FAIL: post-revoke client status $CODE, want 401"; exit 1; }
"$TMP/woca" relay status --server "http://$WOCA_ADDR" > "$TMP/status.txt" 2>/dev/null || true
grep -qi 'revoked' "$TMP/status.txt" || { echo "FAIL: bridge status not revoked"; cat "$TMP/status.txt"; exit 1; }

# 8. The audit trail recorded the revocation.
grep -q '"event":"revocation"' "$TMP/relay.db.audit.log" || { echo "FAIL: revocation not audited"; exit 1; }

echo "e2e-relay: OK"

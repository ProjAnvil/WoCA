#!/usr/bin/env sh
# VM/bare-metal adaptation verification (design §12.1): prove the current
# full-stack registry posture against a real standalone Nacos. The server
# role (woca-server) registers itself, heartbeats every 5s, and
# materializes the delivered config pack at startup — and refuses to
# start at all without WOCA_NACOS_URL (fail-closed, no fallback). See
# internal/ee/nacosrt/ for the client contract this script exercises over
# the nacos v1 HTTP API.
#
# Usage: ./deploy/vm/nacos-verify.sh   (requires docker + go; run from the
# repo root)
set -eu

DIR=$(mktemp -d /tmp/woca-nacos-verify.XXXXXX)
SRV_PID=""
cleanup() {
    [ -z "$SRV_PID" ] || kill "$SRV_PID" 2>/dev/null || true
    docker rm -f woca-nacos-verify >/dev/null 2>&1 || true
    rm -rf "$DIR"
}
trap cleanup EXIT

echo "== start nacos (standalone, embedded storage)"
docker run -d --name woca-nacos-verify -p 8848:8848 -e MODE=standalone \
    nacos/nacos-server:v2.5.1 >/dev/null
until curl -sf http://localhost:8848/nacos/v1/console/health/readiness >/dev/null 2>&1; do
    sleep 2
done

echo "== publish the initial (empty) config pack"
# Roles fail-closed on a missing pack: the stack's nacos-init step
# publishes the empty pack before any role starts (nacosrt.Runtime.Start
# treats a missing woca-config.pack as "stack never initialized").
curl -sf -X POST "http://localhost:8848/nacos/v1/cs/configs" \
    --data-urlencode "dataId=woca-config.pack" \
    --data-urlencode "group=DEFAULT_GROUP" \
    --data-urlencode 'content={"version":1,"generation":"","settings":{}}' >/dev/null

echo "== build woca-server"
go build -o "$DIR/woca-server" ./cmd/woca-server

echo "== fail-closed: no WOCA_NACOS_URL means no process"
# Scrub the operator shell's WOCA_NACOS_URL so an exported value can't
# turn this step into a false pass/fail.
if env -u WOCA_NACOS_URL WOCA_DB="sqlite:$DIR/woca-fail.db" "$DIR/woca-server" serve \
    --addr 127.0.0.1:18098 --no-browser >"$DIR/fail.log" 2>&1; then
    echo "FAIL: woca-server started without WOCA_NACOS_URL" >&2
    exit 1
fi
grep -q nacosrt "$DIR/fail.log" || {
    echo "FAIL: refusal did not come from the registry gate:" >&2
    cat "$DIR/fail.log" >&2
    exit 1
}
echo "   refused to start: $(tail -1 "$DIR/fail.log")"

echo "== register + heartbeat + config pull against the real registry"
WOCA_DB="sqlite:$DIR/woca-server.db" WOCA_NACOS_URL="http://localhost:8848" \
    "$DIR/woca-server" serve --addr 127.0.0.1:18099 --no-browser \
    >"$DIR/serve.log" 2>&1 &
SRV_PID=$!

i=0
until curl -sf "http://localhost:8848/nacos/v1/ns/instance/list?serviceName=woca-server" \
    | grep -q '"healthy":true'; do
    i=$((i + 1))
    if [ "$i" -gt 30 ]; then
        echo "FAIL: instance never showed up healthy in nacos" >&2
        cat "$DIR/serve.log" >&2
        exit 1
    fi
    kill -0 "$SRV_PID" 2>/dev/null || {
        echo "FAIL: woca-server exited before registering:" >&2
        cat "$DIR/serve.log" >&2
        exit 1
    }
    sleep 2
done

grep -q "initial config pack applied" "$DIR/serve.log" || {
    echo "FAIL: config pack was never materialized:" >&2
    cat "$DIR/serve.log" >&2
    exit 1
}

echo "PASS: woca-server registered healthy in nacos (heartbeat running) and applied the delivered config pack"

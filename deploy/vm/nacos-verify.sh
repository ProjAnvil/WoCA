#!/usr/bin/env sh
# VM/bare-metal adaptation verification (design §12.1): prove a woca
# binary resolves its database target from a real Nacos config entry —
# no registry SDK, no self-registration, one plain HTTP fetch at startup.
#
# Usage: ./deploy/vm/nacos-verify.sh   (requires docker + go)
set -eu

DIR=$(mktemp -d /tmp/woca-nacos-verify.XXXXXX)
trap 'docker rm -f woca-nacos-verify >/dev/null 2>&1 || true' EXIT

echo "== start nacos (standalone, embedded storage)"
docker run -d --name woca-nacos-verify -p 8848:8848 -e MODE=standalone \
    nacos/nacos-server:v2.1.0 >/dev/null
until curl -sf http://localhost:8848/nacos/v1/console/server/state >/dev/null 2>&1; do
    sleep 2
done

echo "== publish the woca-db config entry"
curl -sf -X POST "http://localhost:8848/nacos/v1/cs/configs?dataId=woca-db.dsn&group=WOCA&content=sqlite:$DIR/woca.db" >/dev/null

echo "== build woca and resolve through nacos://"
go build -o "$DIR/woca" ./cmd/woca
"$DIR/woca" tenant list --db "nacos://localhost:8848/WOCA/woca-db.dsn"

if [ -f "$DIR/woca.db" ]; then
    echo "PASS: database opened at the Nacos-resolved path"
else
    echo "FAIL: $DIR/woca.db was not created" >&2
    exit 1
fi

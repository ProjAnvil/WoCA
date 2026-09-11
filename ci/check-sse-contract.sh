#!/usr/bin/env bash
# Fails when the Go SSE event constants (internal/server/ssevents.go) and the
# TS union (web/src/api/events.ts) drift apart. The Go side allows gofmt's
# const-block alignment around "="; the TS side matches single-quoted names.
set -euo pipefail
cd "$(dirname "$0")/.."
go_names=$(grep -oE 'Event[A-Za-z]+[[:space:]]*=[[:space:]]*"[a-z_]+"' internal/server/ssevents.go | grep -oE '"[a-z_]+"' | tr -d '"' | sort)
ts_names=$(grep -oE "'[a-z_]+'" web/src/api/events.ts | tr -d "'" | sort -u)
diff <(echo "$go_names") <(echo "$ts_names")

#!/usr/bin/env bash
# Fails when the web's machine-tag prefix literals (web/src/lib/transcript.ts
# — RUN_STOPPED_TAG and the NOTICE_TAGS family) drift from the Go single
# source (internal/machinetag/machinetag.go). Only the prefixes both sides
# own are compared: the web additionally renders <background-task-completed
# (internal/agent/queue), while Go's <stop-hook>/<task-fork>/
# <relevant-memory> are model-input the web renders differently or skips, so
# they carry no exact-literal twin. Mirrors check-sse-contract.sh: the Go
# side tolerates gofmt's const-block alignment around "=".
set -euo pipefail
cd "$(dirname "$0")/.."

ts=web/src/lib/transcript.ts
status=0
while IFS= read -r name; do
  value=$(grep -oE "${name}[[:space:]]+=[[:space:]]*\"[^\"]+\"" internal/machinetag/machinetag.go | grep -oE '"[^"]+"' | tr -d '"')
  if [ -z "$value" ]; then
    echo "machinetag contract: constant $name missing from internal/machinetag/machinetag.go" >&2
    status=1
    continue
  fi
  if ! grep -qF "'$value'" "$ts"; then
    echo "machinetag contract: $name ($value) is not a quoted literal in $ts" >&2
    status=1
  fi
done <<'EOF'
RunStopped
ScheduledTask
QueuedUserInput
EOF
exit $status

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
  # `|| true`: a constant renamed out of the Go source empties the pipeline,
  # and set -e would exit before the friendly message below could print.
  value=$(grep -oE "${name}[[:space:]]+=[[:space:]]*\"[^\"]+\"" internal/machinetag/machinetag.go | grep -oE '"[^"]+"' | tr -d '"') || true
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

# Fixture-presence contract: the e2e hitl spec's MACHINE_TAG_PREFIXES array
# must carry every machine-tag prefix the scripted model must SKIP when it
# derives "last user message". If a prefix drifts here, the turn boundary
# breaks and approvals get phantom re-asked under a new namespace (see the
# 2026-11-19 hitl un-quarantine). Match forms: the fixture quotes full tags
# (Go value verbatim, e.g. '<stop-hook>') for closed-tag families and bare
# prefixes (Go value minus a trailing ">", e.g. '<run-stopped') where the
# tag carries attributes — both forms satisfy the startsWith skip, so both
# are accepted here.
spec=e2e/tests/personal/hitl.spec.ts
prefixes_line=$(grep -n 'MACHINE_TAG_PREFIXES' "$spec" | head -1 | cut -d: -f2-)
if [ -z "$prefixes_line" ]; then
  echo "machinetag contract: MACHINE_TAG_PREFIXES not found in $spec" >&2
  exit 1
fi
while IFS= read -r name; do
  value=$(grep -oE "${name}[[:space:]]+=[[:space:]]*\"[^\"]+\"" internal/machinetag/machinetag.go | grep -oE '"[^"]+"' | tr -d '"') || true
  if [ -z "$value" ]; then
    echo "machinetag contract: constant $name missing from internal/machinetag/machinetag.go" >&2
    status=1
    continue
  fi
  stripped=${value%>}
  if ! printf '%s' "$prefixes_line" | grep -qF "'$value'" \
     && ! printf '%s' "$prefixes_line" | grep -qF "'$stripped'"; then
    echo "machinetag contract: $name ($value) missing from MACHINE_TAG_PREFIXES in $spec" >&2
    status=1
  fi
done <<'EOF'
RelevantMemory
StopHook
TaskFork
RunStopped
QueuedUserInput
EOF
exit $status

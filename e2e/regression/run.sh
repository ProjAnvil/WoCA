#!/usr/bin/env bash
# woca functional-regression orchestrator: runs the numbered stages in
# order, stops at the first failure, and prints the audit summary. Every
# stage is a standalone script under stages/ — rerun any single one.
#
# Usage:
#   e2e/regression/run.sh                  # full run (stages 00..06 + teardown)
#   e2e/regression/run.sh 03 04            # only these stages (facts permitting)
#   REG_SKIP_E2E=1  e2e/regression/run.sh  # skip the browser suite (stage 02)
#   REG_SKIP_UNIT=1 e2e/regression/run.sh  # skip go test ./... in stage 01
#   REG_KEEP=1      e2e/regression/run.sh  # keep stacks/processes for debugging
#
# Requirements: OrbStack (or Docker), go, bun, curl, jq, git.
set -u
REG_ROOT="$(cd "$(dirname "$0")" && pwd)"
source "$REG_ROOT/lib.sh"

# Everything the run started must die with it — including stage 04's fake
# model, which deliberately outlives its stage for the bridge chat.
cleanup() {
  if [ "${REG_KEEP:-0}" != "1" ]; then
    for name in personal fakemodel; do stop_pid "$name"; done
    if [ -f "$RUN/state.env" ] && [ -n "$(state_get ent_up)" ]; then
      docker compose -f "$ENT_COMPOSE_FILE" --env-file "$ENT_ENV_FILE" \
        --profile bridge --profile langfuse down -v --remove-orphans --timeout 20 \
        >"$LOGS/ent-teardown.log" 2>&1 || true
    fi
  else
    echo "REG_KEEP=1: processes and the enterprise stack left running for debugging"
  fi
}
trap cleanup EXIT

all_stages="00-preflight 01-build 02-browser-e2e 03-personal 04-ent-console 05-ent-client 06-plugins"

if [ $# -gt 0 ]; then
  wanted="$*"
else
  wanted="$all_stages"
fi

failed=0
for st in $wanted; do
  script="$REG_ROOT/stages/$st.sh"
  if [ ! -x "$script" ]; then
    echo "unknown stage: $st (available: $all_stages)" >&2
    exit 2
  fi
  echo ""
  echo "════════ stage $st ════════"
  if ! bash "$script"; then
    failed=1
    echo ""
    echo "STAGE FAILED: $st — see $LOGS and $RUN/audit.log"
    exit 1
  fi
  # Remember that the enterprise stack is ours to tear down.
  case "$st" in 04-*) state_set ent_up 1 ;; esac
done

echo ""
echo "════════ audit summary ═══════"
passed=$(grep -c '^regression\.pass' "$RUN/audit.log" 2>/dev/null || echo 0)
failedn=$(grep -c '^regression\.fail' "$RUN/audit.log" 2>/dev/null || echo 0)
echo "PASS: $passed   FAIL: $failedn"
grep '^regression\.fail' "$RUN/audit.log" 2>/dev/null | sed 's/^/  /'
if [ "$failedn" -gt 0 ]; then
  echo "(non-fatal assertion failures above — the run continued)"
  exit 3
fi
echo "ALL GREEN"

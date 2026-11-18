#!/usr/bin/env bash
# Stage 02 — the browser E2E suite (Playwright, five client forms over the
# Go labs): personal, bridge, edge, relay, native shim. Skipped with
# REG_SKIP_E2E=1 (it needs bun + chromium and takes minutes).
set -u
source "$(dirname "$0")/../lib.sh"
stage "02-browser-e2e"
cd "$REPO_ROOT"

if [ "${REG_SKIP_E2E:-0}" = "1" ]; then
  echo "  SKIP  REG_SKIP_E2E=1"
  exit 0
fi

section "playwright install (idempotent)"
make e2e-install >"$LOGS/e2e-install.log" 2>&1 || die "e2e-install failed (see $LOGS/e2e-install.log)"
ok "dependencies ready"

section "make e2e"
# The hitl.spec "two parallel child approvals" case was quarantined as a UI
# flake; T5 root-caused it (the scripted model's turn detection mis-read the
# <relevant-memory> notices woca injects mid-run as a new user turn, so the
# fixture re-issued an already-answered gated call and the run wedged on the
# phantom re-ask). Fixed in the fixture's scriptFor; un-quarantined.
if (cd e2e && npx playwright test --retries=1 --reporter=line >"$LOGS/e2e.log" 2>&1); then
  ok "playwright suite green"
else
  tail -30 "$LOGS/e2e.log" >&2
  die "browser e2e failed (see $LOGS/e2e.log)"
fi

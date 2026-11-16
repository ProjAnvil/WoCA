#!/usr/bin/env bash
# Stage 01 — build: compile everything the later stages run against the
# CURRENT working tree: the personal binary, the regression stubs, the
# enterprise console SPAs (ee-web) and the linux role packages the dev
# compose stack bakes into its images.
set -u
source "$(dirname "$0")/../lib.sh"
stage "01-build"
cd "$REPO_ROOT"

section "go build ./..."
make -s build >/dev/null || die "go build failed"
go build ./... || die "go build ./... failed"
ok "go build"

section "regression binaries"
go build -o "$BIN/woca" ./cmd/woca || die "building woca failed"
go build -o "$BIN/reg-fakemodel" ./e2e/regression/fakemodel || die "building reg-fakemodel failed"
go build -o "$BIN/reg-mcpstub" ./e2e/regression/mcpstub || die "building reg-mcpstub failed"
ok "woca + reg-fakemodel + reg-mcpstub -> $BIN"

if [ "${REG_SKIP_UNIT:-0}" != "1" ]; then
  section "unit tests"
  if go test ./... -count=1 >"$LOGS/unittests.log" 2>&1; then
    ok "go test ./..."
  else
    fail "go test ./..." "see $LOGS/unittests.log"
    die "unit tests failed — fix before functional regression"
  fi
fi

section "enterprise console SPAs (make ee-web)"
if make ee-web >"$LOGS/ee-web.log" 2>&1; then
  ok "ee-web consoles built"
else
  die "make ee-web failed (see $LOGS/ee-web.log)"
fi

section "linux role packages (dist-role)"
for role in woca-scheduler woca-ops woca-worker woca-config woca-edge woca-tenant woca-bridge; do
  if GOOS=linux make dist-role ROLE="$role" >"$LOGS/dist-$role.log" 2>&1; then
    ok "dist-role $role"
  else
    die "dist-role $role failed (see $LOGS/dist-$role.log)"
  fi
done

state_set bin_woca "$BIN/woca"
state_set bin_fakemodel "$BIN/reg-fakemodel"
state_set bin_mcpstub "$BIN/reg-mcpstub"

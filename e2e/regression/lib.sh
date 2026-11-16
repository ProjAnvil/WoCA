#!/usr/bin/env bash
# Shared helpers for the woca functional-regression stages.
# Sourced by stages/*.sh; never executed directly.
#
# Conventions:
#   - A stage is a sequence of checks; every check calls `check`/`ok`/`die`.
#   - Cross-stage facts live in $RUN/state.env (KEY=VALUE lines).
#   - Bash 3.2 compatible (macOS default shell): no mapfile, no assoc arrays.

# ── locations ─────────────────────────────────────────────────────────
REG_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # e2e/regression
REPO_ROOT="$(cd "$REG_ROOT/../.." && pwd)"
RUN="$REPO_ROOT/e2e/.runtime/regression"
BIN="$RUN/bin"
HOME_DIR="$RUN/home"          # WOCA_HOME for personal-form runs
WS="$RUN/ws"                  # personal regression workspace
LOGS="$RUN/logs"
STATE="$RUN/state.env"

REG_PORT_PERSONAL=13884        # personal `woca serve`
REG_PORT_FAKEMODEL=13885       # scripted anthropic fake (host)
ENT_COMPOSE_FILE="$REPO_ROOT/deploy/selfhosted/docker-compose.dev.yml"
ENT_ENV_FILE="$REG_ROOT/ent.env"

# The runtime tree must exist before the first ok()/fail() writes the audit
# log — stages and the orchestrator both source this file.
mkdir -p "$BIN" "$HOME_DIR" "$WS" "$LOGS" "$RUN/pids"

# ── output ────────────────────────────────────────────────────────────
_stage=""
_section=""

section() { _section="$1"; printf '\n== %s\n' "$1"; }
stage()   { _stage="$1"; _section=""; printf '\n# stage %s\n' "$1"; }

ok()   { printf '  PASS  %s\n' "${1:-$_section}"; echo "regression.pass $_stage ${1:-$_section}" >>"$RUN/audit.log"; }
fail() { printf '  FAIL  %s\n' "${1:-$_section}"; echo "regression.fail $_stage ${1:-$_section} :: ${2:-}" >>"$RUN/audit.log"; }

# check <name> <cmd...>: runs the command; PASS on zero exit.
check() {
  local name="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$name"; else fail "$name" "exit $?"; return 1; fi
}

die() { printf '  FATAL %s\n' "$1" >&2; echo "regression.fatal $_stage :: $1" >>"$RUN/audit.log"; exit 1; }

# ── state (cross-stage facts) ─────────────────────────────────────────
state_set() { # key value — last write wins
  grep -v "^$1=" "$STATE" 2>/dev/null >"$STATE.tmp"; mv "$STATE.tmp" "$STATE"; echo "$1=$2" >>"$STATE"
}
state_get() { # key — prints value, empty when missing
  [ -f "$STATE" ] || return 0
  awk -F= -v k="$1" '$1==k{sub(/^[^=]*=/,""); print; exit}' "$STATE"
}

# ── environment ───────────────────────────────────────────────────────
need_cmd() { command -v "$1" >/dev/null 2>&1 || die "missing dependency: $1 (stage $_stage)"; }

port_free() { # port — true when nothing listens
  ! (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null
}
wait_port() { # host port timeout_s
  local host="$1" port="$2" t="${3:-60}" i
  for ((i = 0; i < t * 4; i++)); do
    (exec 3<>"/dev/tcp/$host/$port") 2>/dev/null && return 0
    sleep 0.25
  done
  return 1
}
wait_http() { # url timeout_s [expected-status]
  local url="$1" t="${2:-60}" want="${3:-200}" i code
  for ((i = 0; i < t * 2; i++)); do
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 3 "$url" 2>/dev/null)
    [ "$code" = "$want" ] && return 0
    sleep 0.5
  done
  echo "wait_http: $url never returned $want (last=$code)" >&2
  return 1
}

# ── process registry (pidfiles under $RUN/pids) ───────────────────────
pids="$RUN/pids"
reg_pid() { echo "$2" >"$pids/$1"; }
stop_pid() { # name [signal]
  local f="$pids/$1"
  [ -f "$f" ] || return 0
  local pid; pid="$(cat "$f")"
  kill "${2:-TERM}" "$pid" 2>/dev/null
  for _ in 1 2 3 4 5 6 7 8 9 10; do kill -0 "$pid" 2>/dev/null || break; sleep 0.3; done
  kill -9 "$pid" 2>/dev/null
  rm -f "$f"
}

# ── SSE helpers ───────────────────────────────────────────────────────
# sse_wait <url> <output-file> <grep-pattern> <timeout-s> [curl-extra...]
# Streams the SSE endpoint into output-file until a line matches the
# pattern (the stream keeps running afterwards; it is killed here).
sse_wait() {
  local url="$1" out="$2" pat="$3" t="${4:-90}"; shift 4 || shift $#
  : >"$out"
  curl -sN --max-time "$((t + 30))" "$@" "$url" >"$out" 2>>"$RUN/logs/sse.err" &
  local cpid=$! i
  reg_pid "sse-$cpid" "$cpid" 2>/dev/null || true
  for ((i = 0; i < t * 4; i++)); do
    grep -q -- "$pat" "$out" 2>/dev/null && { kill "$cpid" 2>/dev/null; return 0; }
    sleep 0.25
  done
  kill "$cpid" 2>/dev/null
  return 1
}

# sse_json <file> <event-name> <jq-expr>: extract from the FIRST occurrence
# of `event: <name>` — applies jq to that event's data payload.
sse_json() {
  local file="$1" ev="$2" expr="$3"
  awk -v ev="$ev" '
    $0 == "event: " ev { grab = 1; next }
    grab && /^data: / { sub(/^data: /, ""); print; exit }
  ' "$file" | jq -r "$expr"
}

# sse_last_json <file> <event-name> <jq-expr>: same but the LAST occurrence.
sse_last_json() {
  local file="$1" ev="$2" expr="$3"
  awk -v ev="$ev" '
    $0 == "event: " ev { grab = 1; data = ""; next }
    grab && /^data: / { data = $0; grab = 0; next }
    END { if (data != "") { sub(/^data: /, "", data); print data } }
  ' "$file" | jq -r "$expr"
}

# ── json helpers ──────────────────────────────────────────────────────
jget() { # json key — prints value (string/number)
  printf '%s' "$1" | jq -r "$2 // empty"
}

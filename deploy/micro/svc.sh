#!/usr/bin/env bash
# svc.sh — the generic start/stop controller for every woca microservice
# role. One script serves all roles; role specifics (addresses, env, flags)
# live ONLY in env/<role>.env, sourced at start time — common.env first,
# the role file second, so a role can override shared values.
#
# Usage (from anywhere inside the unpacked package):
#   scripts/svc.sh <role> <start|stop|status|restart>
#   scripts/svc.sh status        # every role present in bin/
#
# PID bookkeeping:
#   run/<role>.pid   the live pid (removed on stop; stale files detected
#                    via kill -0 plus a cmdline check against pid reuse)
#   run/svc.log      one line per start/stop attempt — timestamp, role,
#                    action, pid, outcome — the audit trail of every
#                    start and stop this package has performed
#   logs/<role>.log  the role process's stdout/stderr since first start
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BIN_DIR="$ROOT/bin"
ENV_DIR="$ROOT/env"
RUN_DIR="$ROOT/run"
LOG_DIR="$ROOT/logs"
EVENT_LOG="$RUN_DIR/svc.log"
# Longest graceful stop in the fleet is the edge's 40s SSE window; the
# default covers it with margin. Override per deployment if needed.
STOP_TIMEOUT="${WOCA_SVC_STOP_TIMEOUT:-45}"

mkdir -p "$RUN_DIR" "$LOG_DIR"

log_event() { # role action pid detail
    printf '%s\t%s\t%s\t%s\t%s\n' \
        "$(date '+%Y-%m-%dT%H:%M:%S%z')" "$1" "$2" "$3" "$4" >>"$EVENT_LOG"
}

pid_file() { echo "$RUN_DIR/$1.pid"; }

read_pid() { cat "$(pid_file "$1")" 2>/dev/null || true; }

# pid_alive <role>: true when the pid file names a live process whose
# command line is this role's binary — kill -0 alone would also match a
# recycled pid now owned by an unrelated process.
pid_alive() {
    local pid
    pid="$(read_pid "$1")"
    [ -n "$pid" ] || return 1
    kill -0 "$pid" 2>/dev/null || return 1
    ps -ww -p "$pid" -o args= 2>/dev/null | grep -q "$BIN_DIR/$1"
}

svc_start() {
    local role="$1" pid pf
    pf="$(pid_file "$role")"
    if pid_alive "$role"; then
        echo "$role: already running (pid $(read_pid "$role"))"
        return 0
    fi
    rm -f "$pf"
    if [ ! -x "$BIN_DIR/$role" ]; then
        echo "$role: binary not found in $BIN_DIR" >&2
        return 1
    fi
    # Role assembly is env-only: the script knows nothing per-role. set -a
    # makes every KEY=value in the env files a real environment variable
    # for the launched process (plain sourcing would leave them as
    # unexported shell variables the child never sees).
    set -a
    if [ -f "$ENV_DIR/common.env" ]; then . "$ENV_DIR/common.env"; fi
    if [ -f "$ENV_DIR/$role.env" ]; then . "$ENV_DIR/$role.env"; fi
    set +a
    nohup "$BIN_DIR/$role" >>"$LOG_DIR/$role.log" 2>&1 &
    pid=$!
    echo "$pid" >"$pf"
    sleep 1
    if ! kill -0 "$pid" 2>/dev/null; then
        log_event "$role" start-failed "$pid" "exited immediately; see logs/$role.log"
        rm -f "$pf"
        echo "$role: start failed (process exited immediately); tail of logs/$role.log:" >&2
        tail -n 5 "$LOG_DIR/$role.log" >&2 || true
        return 1
    fi
    log_event "$role" start "$pid" "ok"
    echo "$role: started (pid $pid)"
}

svc_stop() {
    local role="$1" pid waited=0 pf
    pf="$(pid_file "$role")"
    if [ ! -f "$pf" ]; then
        echo "$role: not running (no pid file)"
        return 0
    fi
    pid="$(read_pid "$role")"
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
        log_event "$role" stop "${pid:-?}" "stale pid file removed"
        rm -f "$pf"
        echo "$role: not running (stale pid file cleaned)"
        return 0
    fi
    # SIGTERM is the graceful path every role implements (webfx shutdown
    # grace). SIGKILL only after the timeout.
    kill -TERM "$pid"
    while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt "$STOP_TIMEOUT" ]; do
        sleep 1
        waited=$((waited + 1))
    done
    if kill -0 "$pid" 2>/dev/null; then
        kill -KILL "$pid" 2>/dev/null || true
        log_event "$role" stop "$pid" "SIGKILL after ${STOP_TIMEOUT}s (grace exceeded)"
        echo "$role: force-killed (pid $pid) after ${STOP_TIMEOUT}s grace"
    else
        log_event "$role" stop "$pid" "terminated by SIGTERM after ${waited}s"
        echo "$role: stopped (pid $pid, ${waited}s)"
    fi
    rm -f "$pf"
}

svc_status_one() {
    if pid_alive "$1"; then
        echo "$1: running (pid $(read_pid "$1"))"
    else
        echo "$1: stopped"
    fi
}

svc_status_all() {
    local any=0 b
    for b in "$BIN_DIR"/*; do
        [ -f "$b" ] || continue
        svc_status_one "$(basename "$b")"
        any=1
    done
    [ "$any" -eq 1 ] || { echo "no binaries in $BIN_DIR" >&2; return 1; }
}

usage() {
    sed -n '2,15p' "$0" | sed 's/^# \{0,1\}//'
    exit 2
}

[ $# -ge 1 ] || usage
if [ $# -eq 1 ] && [ "$1" = "status" ]; then
    svc_status_all
    exit 0
fi
[ $# -eq 2 ] || usage
role="$1"
action="$2"
case "$action" in
    start) svc_start "$role" ;;
    stop) svc_stop "$role" ;;
    status) svc_status_one "$role" ;;
    restart) svc_stop "$role"; svc_start "$role" ;;
    *) usage ;;
esac

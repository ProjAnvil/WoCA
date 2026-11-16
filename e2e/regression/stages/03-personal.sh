#!/usr/bin/env bash
# Stage 03 — personal edition functional surface, black-box over the real
# `woca serve` binary and the scripted fake model:
#   chat (plain + tool round) · graph HITL approval round · hooks ·
#   skills · MCP (stdio stub bound and EXECUTED) · plugin management API.
set -u
source "$(dirname "$0")/../lib.sh"
stage "03-personal"
cd "$REPO_ROOT"

# Never leak processes out of a failed stage.
trap 'stop_pid personal; stop_pid fakemodel' EXIT

PERSONAL="http://127.0.0.1:$REG_PORT_PERSONAL"
FAKE="http://127.0.0.1:$REG_PORT_FAKEMODEL"

# ── clean slate: the persistent memory (LadyM episodes under WOCA_HOME)
# replays earlier runs' markers into fresh conversations, which would
# contaminate the scripted fake — each stage run starts from nothing.
# REG_KEEP_HOME=1 keeps the tree for debugging.
section "clean slate"
if [ "${REG_KEEP_HOME:-0}" != "1" ]; then
  rm -rf "$HOME_DIR" "$WS"
  mkdir -p "$HOME_DIR" "$WS"
fi

# ── seed the workspace BEFORE boot: skills/hooks/mcp all load at startup ──
section "seed workspace"
mkdir -p "$WS/.woca/skills/regression-skill"
cat >"$WS/.woca/skills/regression-skill/SKILL.md" <<'EOF'
---
name: regression-skill
description: Regression canary skill — proves the workspace skill layer loads.
---

# regression-skill

Do exactly what the user asks; append the marker `REG_SKILL_LOADED` when
this skill is invoked.
EOF
cat >"$WS/.woca/settings.json" <<'EOF'
{
  "hooks": {
    "PostToolUse": [
      {
        "matcher": "Write",
        "hooks": [{"type": "command", "command": "echo REG_HOOK_FIRED"}]
      }
    ]
  }
}
EOF
cat >"$WS/.woca/mcp.json" <<EOF
{"mcpServers": {"mcpstub": {"command": "$BIN/reg-mcpstub"}}}
EOF
ok "workspace seeded (skill + PostToolUse hook + mcp.json)"

# ── processes ─────────────────────────────────────────────────────────
section "start fake model + woca serve"
# A stale listener on our port would silently absorb every check — the
# stage must boot ITS OWN processes.
port_free "$REG_PORT_PERSONAL" || die "port $REG_PORT_PERSONAL busy — a stale serve is running; stop it first"
port_free "$REG_PORT_FAKEMODEL" || die "port $REG_PORT_FAKEMODEL busy"
"$BIN/reg-fakemodel" --addr "127.0.0.1:$REG_PORT_FAKEMODEL" >"$LOGS/fakemodel.log" 2>&1 &
reg_pid fakemodel $!
wait_http "$FAKE/healthz" 20 || die "fake model did not come up (see $LOGS/fakemodel.log)"

# The serve process's cwd is the workspace: MCP servers connect at agent
# build time from <serve-cwd>/.woca/mcp.json (per-run workspaces do not
# reconnect MCP — connections are process-wide by design).
cd "$WS"
WOCA_HOME="$HOME_DIR" "$BIN/woca" serve --addr "127.0.0.1:$REG_PORT_PERSONAL" >"$LOGS/personal-serve.log" 2>&1 &
reg_pid personal $!
wait_http "$PERSONAL/api/settings" 40 || die "woca serve did not come up (see $LOGS/personal-serve.log)"
ok "fake model :$REG_PORT_FAKEMODEL, woca serve :$REG_PORT_PERSONAL"

# ── provider configuration through the settings API ────────────────────
section "provider via PUT /api/settings"
code=$(curl -s -o "$LOGS/put-settings.json" -w '%{http_code}' -X PUT "$PERSONAL/api/settings" \
  -H 'Content-Type: application/json' -d '{
    "provider": "anthropic", "model": "fake-claude",
    "anthropic_api_key": "reg-dummy-key",
    "anthropic_base_url": "'"$FAKE"'"
  }')
[ "$code" = "200" ] || { cat "$LOGS/put-settings.json" >&2; die "PUT /api/settings -> $code"; }
got=$(curl -s "$PERSONAL/api/settings" | jq -r '.anthropic_api_key_set')
[ "$got" = "true" ] || die "api key not persisted (anthropic_api_key_set=$got)"
ok "provider anthropic -> fake model, key persisted"

# ── skills ────────────────────────────────────────────────────────────
section "skill layer"
got=$(curl -s "$PERSONAL/api/completions?workspace=$WS" | jq -r '.skills[]?.name' | grep -c '^regression-skill$')
[ "$got" -ge 1 ] || die "regression-skill missing from /api/completions"
ok "workspace skill listed (regression-skill)"

# ── helpers ───────────────────────────────────────────────────────────
new_session() {
  curl -s -X POST "$PERSONAL/api/sessions" -H 'Content-Type: application/json' \
    -d '{"workspace":"'"$WS"'"}' | jq -r '.session_id'
}

last_ai() { # sid — the session's last assistant message content
  curl -s "$PERSONAL/api/sessions/$1" | jq -r '[.messages[] | select(.role == "ai") | .content] | last // empty' 2>/dev/null
}

tool_texts() { # sid — every tool message content, newline-joined
  curl -s "$PERSONAL/api/sessions/$1" | jq -r '[.messages[] | select(.role == "tool") | .content] | join("\n")' 2>/dev/null
}

# Count/extract only ANSWERABLE interrupt events: the stream also carries
# card-less interrupt broadcasts (compact re-park markers), which must not
# be answered.
count_answerable() { # file
  awk '
    $0 == "event: interrupt" { grab = 1; next }
    grab && /^data: / { if ($0 ~ /"cards"/) n++; grab = 0 }
    END { print n + 0 }
  ' "$1"
}
nth_interrupt_ns() { # file n — card NS of the nth answerable interrupt
  awk -v want="$2" '
    $0 == "event: interrupt" { grab = 1; next }
    grab && /^data: / {
      if ($0 ~ /"cards"/) { n++ }
      if (n == want) { line = $0; sub(/^data: /, "", line); print line; exit }
      grab = 0
    }
  ' "$1" | jq -r '.cards[0].ns // .cards[0].id // .payload.cards[0].ns // .payload.cards[0].id // empty'
}

# run_and_finish <sid> <prompt> <sse-file> <expected-final>: starts the
# run, streams its events with ONE capture (reconnecting replays history
# and would double-answer old interrupts), answers every NEW approval
# interrupt with allow, and returns the run id once the session's last
# assistant message carries the expected final text.
run_and_finish() {
  local sid="$1" prompt="$2" sse="$3" expect="$4" rid answered=0 i got n_int ns code
  rid=$(curl -s -X POST "$PERSONAL/api/runs" -H 'Content-Type: application/json' \
    -d '{"session_id":"'"$sid"'","workspace":"'"$WS"'","prompt":"'"$prompt"'"}' | jq -r '.run_id')
  [ -n "$rid" ] && [ "$rid" != "null" ] || { echo "no run_id" >&2; return 1; }
  : >"$sse"
  curl -sN --max-time 300 "$PERSONAL/api/runs/$rid/events" >"$sse" 2>>"$RUN/logs/sse.err" &
  local cpid=$!
  for ((i = 0; i < 600; i++)); do
    got=$(last_ai "$sid")
    case "$got" in *"$expect"*) kill "$cpid" 2>/dev/null; echo "$rid"; return 0 ;; esac
    n_int=$(count_answerable "$sse")
    if [ "$n_int" -gt "$answered" ]; then
      answered=$((answered + 1))
      [ "$answered" -le 6 ] || { kill "$cpid" 2>/dev/null; echo "too many interrupts" >&2; return 1; }
      ns=$(nth_interrupt_ns "$sse" "$answered")
      [ -n "$ns" ] || { kill "$cpid" 2>/dev/null; echo "interrupt without card ns: $(tail -3 "$sse")" >&2; return 1; }
      code=$(curl -s -o "$LOGS/answer-$rid-$answered.json" -w '%{http_code}' -X POST "$PERSONAL/api/runs/$rid/answer" \
        -H 'Content-Type: application/json' \
        -d '{"answers":[{"key":"'"$ns"'","value":{"approve":true}}]}')
      [ "$code" = "200" ] || { kill "$cpid" 2>/dev/null; echo "answer -> $code: $(cat "$LOGS/answer-$rid-$answered.json")" >&2; return 1; }
    fi
    grep -q '^event: error' "$sse" 2>/dev/null && { kill "$cpid" 2>/dev/null; echo "run errored: $(sse_last_json "$sse" error '.message // .')" >&2; return 1; }
    sleep 0.5
  done
  kill "$cpid" 2>/dev/null
  echo "run $rid did not reach '$expect' within 300s (last: $got)" >&2
  return 1
}

# ── plain chat (no tools) ─────────────────────────────────────────────
section "chat: plain reply"
sid=$(new_session)
[ -n "$sid" ] && [ "$sid" != "null" ] || die "session create failed"
rid=$(run_and_finish "$sid" "just say hi" "$LOGS/sse-plain.sse" "woca-reg:plain reply") || die "plain run failed"
ok "plain chat round-trip (model -> final text)"

# ── tool round + HITL approval + hook + file side effect ───────────────
section "chat: tool round + HITL approval + hook"
sid2=$(new_session)
rid2=$(run_and_finish "$sid2" "woca-reg:chat" "$LOGS/sse-chat.sse" "woca-reg:chat complete") || die "chat run failed"

grep -q 'tool_call' "$LOGS/sse-chat.sse" || die "no tool_call event in chat run"
grep -q '^event: interrupt' "$LOGS/sse-chat.sse" && ok "HITL approval interrupt surfaced and answered" || fail "HITL" "no interrupt event"
toolres=$(tool_texts "$sid2")
case "$toolres" in *Wrote*bytes*) ok "Write tool executed (graph model->hooks->approval->tools)" ;; *) fail "Write execution" "result: $toolres" ;; esac
case "$toolres" in *REG_HOOK_FIRED*) ok "PostToolUse hook fired (marker in tool result)" ;; *) fail "PostToolUse hook" "marker missing" ;; esac
grep -q 'woca-reg-chat-side' "$WS/out.txt" 2>/dev/null && ok "file side effect landed in workspace" || fail "write side effect" "out.txt missing/wrong"

# ── MCP: stub server bound AND executed ────────────────────────────────
section "MCP: mcpstub bound + executed"
sid3=$(new_session)
rid3=$(run_and_finish "$sid3" "woca-reg:mcp" "$LOGS/sse-mcp.sse" "woca-reg:mcp complete") || die "mcp run failed"
mcpres=$(tool_texts "$sid3")
case "$mcpres" in *echo:reg-mcp-ping*) ok "mcp__mcpstub__echo executed through stdio MCP" ;; *) fail "MCP execution" "result: $mcpres" ;; esac

# ── plugin management API (personal plane) ─────────────────────────────
section "plugin management API"
count=$(curl -s "$PERSONAL/api/plugins" | jq '.plugins | length')
[ "${count:-0}" -gt 0 ] && ok "GET /api/plugins catalog ($count entries)" || die "empty plugin catalog"
code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "$PERSONAL/api/plugins" \
  -H 'Content-Type: application/json' -d '{"enabled":["browser"]}')
[ "$code" = "200" ] || die "PUT /api/plugins -> $code"
enabled=$(curl -s "$PERSONAL/api/plugins" | jq -r '.plugins[] | select(.name == "browser") | .enabled')
[ "$enabled" = "true" ] && ok "plugin enable persisted (browser)" || die "browser not enabled after PUT"
# Restore the default (nothing force-enabled) so later stages start clean.
curl -s -o /dev/null -X PUT "$PERSONAL/api/plugins" -H 'Content-Type: application/json' -d '{"enabled":[]}'

state_set personal_url "$PERSONAL"
state_set ws "$WS"

section "stop personal processes"
stop_pid personal
stop_pid fakemodel
ok "serve + fakemodel stopped"

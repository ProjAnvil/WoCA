#!/usr/bin/env bash
# Stage 06 — org plugin loading: install MindForge (github.com/projanvil/
# MindForge, a Claude-style marketplace plugin) through the headless CLI,
# then boot the personal serve against the same home and prove its
# contributed SKILLS and AGENTS actually load into the session surface.
set -u
source "$(dirname "$0")/../lib.sh"
stage "06-plugins"
cd "$REPO_ROOT"

PERSONAL="http://127.0.0.1:$REG_PORT_PERSONAL"
MINDFORGE_MARKET="https://github.com/projanvil/MindForge"

# ── install from the org's marketplace ────────────────────────────────
section "woca plugin install mindforge-en@$MINDFORGE_MARKET"
if HOME="$HOME_DIR" WOCA_HOME="$HOME_DIR" "$BIN/woca" plugin install "mindforge-en@$MINDFORGE_MARKET" >"$LOGS/plugin-install.log" 2>&1; then
  ok "install command succeeded: $(grep -o 'installed [^ ]*' "$LOGS/plugin-install.log" | head -1)"
else
  cat "$LOGS/plugin-install.log" >&2
  die "plugin install failed (see $LOGS/plugin-install.log)"
fi

section "plugin list shows it"
listing=$(HOME="$HOME_DIR" WOCA_HOME="$HOME_DIR" "$BIN/woca" plugin list 2>/dev/null)
case "$listing" in *mindforge-en*) ok "mindforge-en listed ($(printf '%s' "$listing" | grep -c 'git-guru\|code-reviewer\|api-design' 2>/dev/null || true) canary rows)" ;; *) die "mindforge-en missing from plugin list" ;; esac

# ── prove the contributions LOAD: boot serve over the same home ───────
section "contributions load into the session surface"
port_free "$REG_PORT_PERSONAL" || die "port $REG_PORT_PERSONAL busy — a stale serve is running; stop it first"
# Reuse the fake model stage 04 left running; start (and own) one only
# when running stage 06 standalone.
if ! wait_http "http://127.0.0.1:$REG_PORT_FAKEMODEL/healthz" 2; then
  port_free "$REG_PORT_FAKEMODEL" || die "port $REG_PORT_FAKEMODEL busy"
  "$BIN/reg-fakemodel" --addr "127.0.0.1:$REG_PORT_FAKEMODEL" >"$LOGS/fakemodel-p6.log" 2>&1 &
  reg_pid fakemodel $!
  wait_http "http://127.0.0.1:$REG_PORT_FAKEMODEL/healthz" 20 || die "fake model did not come up"
else
  ok "reusing the stage-04 fake model :$REG_PORT_FAKEMODEL"
fi

cd "$WS"
HOME="$HOME_DIR" WOCA_HOME="$HOME_DIR" "$BIN/woca" serve --addr "127.0.0.1:$REG_PORT_PERSONAL" >"$LOGS/personal-serve-p6.log" 2>&1 &
reg_pid personal $!
trap 'stop_pid personal; stop_pid fakemodel' EXIT
wait_http "$PERSONAL/api/settings" 40 || die "serve did not come up (see $LOGS/personal-serve-p6.log)"

code=$(curl -s -o "$LOGS/put-settings-p6.json" -w '%{http_code}' -X PUT "$PERSONAL/api/settings" \
  -H 'Content-Type: application/json' -d '{"provider":"anthropic","model":"fake-claude","anthropic_api_key":"reg-dummy-key","anthropic_base_url":"http://127.0.0.1:'"$REG_PORT_FAKEMODEL"'"}')
[ "$code" = "200" ] || die "PUT /api/settings -> $code"

completions=$(curl -s "$PERSONAL/api/completions?workspace=$WS")
skill_hit=$(printf '%s' "$completions" | jq -r '.skills[]?.name' | grep -c 'git-guru\|go-development\|api-design' || true)
agent_hit=$(printf '%s' "$completions" | jq -r '.agents[]?.name' | grep -c 'code-reviewer\|system-architect\|golang-backend-engineer' || true)
[ "${skill_hit:-0}" -ge 1 ] && ok "mindforge skills loaded ($skill_hit matches in /api/completions)" || fail "mindforge skills" "none of git-guru/go-development/api-design listed"
[ "${agent_hit:-0}" -ge 1 ] && ok "mindforge agents loaded ($agent_hit matches in /api/completions)" || fail "mindforge agents" "none of code-reviewer/system-architect listed"

section "a mindforge-served conversation still works"
sid=$(curl -s -X POST "$PERSONAL/api/sessions" -H 'Content-Type: application/json' -d '{"workspace":"'"$WS"'"}' | jq -r '.session_id')
reply=""
for _ in $(seq 1 60); do
  reply=$(curl -s "$PERSONAL/api/sessions/$sid" | jq -r '[.messages[] | select(.role == "ai") | .content] | last // empty' 2>/dev/null)
  case "$reply" in *"woca-reg:plain reply"*) break ;; esac
  curl -s -X POST "$PERSONAL/api/runs" -H 'Content-Type: application/json' \
    -d '{"session_id":"'"$sid"'","workspace":"'"$WS"'","prompt":"just say hi"}' >"$LOGS/p6-run.json" 2>/dev/null
  sleep 1
done
case "$reply" in
  *"woca-reg:plain reply"*) ok "chat works with the plugin loaded" ;;
  *) fail "chat with plugin" "last: $reply" ;;
esac

section "stop stage processes"
stop_pid personal
stop_pid fakemodel
trap - EXIT
ok "stopped"

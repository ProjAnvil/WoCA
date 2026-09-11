#!/usr/bin/env bash
# Fake woca: echoes a fixed stream-json session regardless of args.
# FAKE_WOCA_FAIL=1 makes it exit non-zero without a result event.
if [ "$FAKE_WOCA_FAIL" = "1" ]; then
  echo "boom" >&2
  exit 1
fi
# FAKE_WOCA_HANG=1 prints one event then blocks, so tests can observe that
# breaking out of iteration kills the child (exec keeps no orphan sleep).
if [ "$FAKE_WOCA_HANG" = "1" ]; then
  echo '{"type":"assistant","message":{"role":"assistant","content":"first"}}'
  exec sleep 60
fi
# Echo back the args as the first event so tests can assert flag mapping.
printf '{"type":"assistant","message":{"role":"assistant","content":%s}}\n' "$(printf '%s' "$*" | python3 -c 'import json,sys; print(json.dumps(sys.stdin.read()))')"
echo '{"type":"assistant","message":{"role":"assistant","content":"hello","tool_calls":[{"id":"t1","name":"Bash","args":{"command":"ls"}}]}}'
echo '{"type":"user","message":{"role":"tool","tool_call_id":"t1","content":"file.go"}}'
echo '{"type":"result","subtype":"success","result":"done","usage":{"requests":1,"input_tokens":10,"output_tokens":5,"total_tokens":15},"num_turns":2}'

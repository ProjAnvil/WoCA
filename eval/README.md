# woca × Harbor (Terminal-Bench) evaluation

This directory wires woca into [Harbor](https://www.harborframework.com), the
official harness for [Terminal-Bench 2.0](https://www.tbench.ai), so the whole
harness — model loop, tools, permissions, compaction — can be measured on a
public benchmark the same way Claude Code / Codex / Gemini CLI are.

The integration follows Harbor's "installed agent" pattern (like Claude Code):
`install()` uploads a statically linked linux/amd64 woca binary into the task
container; `run()` executes `woca -p <instruction> --output-format
stream-json` headless in the task working directory with permissions bypassed.

## Layout

- `woca_agent.py` — the Harbor agent adapter (`--agent eval.woca_agent:WocaAgent`)
- `tasks/hello-woca/` — zero-cost smoke task (local, ubuntu container, one file write)

## One-time setup

```bash
uv tool install harbor          # installs harbor / hb / hr
```

## Smoke test (hello-woca)

```bash
make eval-binary                       # -> build/woca-linux-amd64 (~61 MB static)
export DEEPSEEK_API_KEY=...            # or OPENAI_API_KEY / ANTHROPIC_API_KEY
PYTHONPATH=. harbor run --path eval/tasks/hello-woca \
    --agent eval.woca_agent:WocaAgent \
    --model deepseek/deepseek-v4-pro \
    -n 1 -y
```

## Terminal-Bench 2.0 run

```bash
export DEEPSEEK_API_KEY=...
PYTHONPATH=. harbor run -d terminal-bench@2.0 \
    --agent eval.woca_agent:WocaAgent \
    --model deepseek/deepseek-v4-pro \
    -n 4 -y -o eval/results
```

Useful flags: `-i '<glob>'` includes only matching tasks, `-t org/name` runs a
single registry task, `--install-only` verifies just the install phase,
`-k N` retries per trial, `--agent-timeout-multiplier` scales the per-task
agent budget. Results (per-trial reward, agent log `woca.txt`, usage metrics)
land under the `-o` jobs dir.

## Model spec → woca flags

The harbor `--model provider/model` maps to woca as: `anthropic`, `openai`,
`deepseek`, `glm` pass through natively to `--provider`; every other
openai-compatible provider (groq, mistral, together, …) routes through
woca's `openai` provider with `--base-url`. The API key is resolved from the
provider's conventional env var (`DEEPSEEK_API_KEY`, `OPENAI_API_KEY`, …) and
handed to woca via a shell variable so it never appears in logged command
lines.

## Adapter kwargs (`--ak key=value`)

- `binary_path` — host path to the linux/amd64 binary (default
  `build/woca-linux-amd64`; env fallback `WOCA_BINARY_PATH`)
- `max_turns` → `--max-turns`, `thinking_budget` → `--thinking-budget`,
  `temperature` → `--temperature`, `fallback_model` → `--fallback-model`,
  `extra_prompt` → `--system-prompt`,
  `permission_mode` → `--permission-mode` (default `bypassPermissions`)

## Notes

- woca runs with `--db none`: no settings store, no memory/checkpoint
  backends — the benchmark measures the bare harness loop.
- Task containers are linux; on Apple Silicon Docker runs amd64 task images
  under emulation, which slows agent runs (raise `--agent-timeout-multiplier`
  if tasks time out).
- The binary upload is ~61 MB per trial on local Docker; if it ever matters
  (remote envs, large sweeps), gzip it in `install()` before upload.

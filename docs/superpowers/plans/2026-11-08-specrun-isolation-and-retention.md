# Speculative run isolation + background retention — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Concurrent sessions' runs stop interfering through speculative tool execution (corruption/stall/self-loss), and background state stops growing without bound.

**Architecture:** Key the speculative registry per (run, call) with futures on the run context; cap shell buffers and finished-record retention in the managers; cache cron next-fire; small deferred tests + a frontend debounce max-wait.

**Tech Stack:** Go 1.26 (internal/agent, internal/tools), Vue 3 + Pinia (web/src/stores/activity.ts), vitest.

**Spec:** `docs/superpowers/specs/2026-11-08-specrun-isolation-and-retention-design.md` (decisions F1–F6 are binding).

## Global Constraints

- Work in `/Users/yuhaochen/Documents/codebase/projanvil/woca/.worktrees/bg-loop-plugin-surface`; one commit per task; English code/comments/commits (zh-CN locale strings excepted).
- The langgraph fork is read-only — fixes land in woca.
- No fallbacks: collisions and truncation are loud and attributed; nothing silently dropped.
- TDD: the investigation's throwaway repro (`internal/server/stall_repro_test.go`, untracked) is the RED evidence — port its five scenarios into a permanent test file in Task 1 and delete the throwaway.
- Gates: `go test ./...`, `go test -race` on changed packages, `make lint` (only the 2 known pre-existing e2e/lab errcheck findings), `cd web && bun run test && bun run build`, then `go build ./cmd/woca`. Never commit web/package-lock.json.

## Review Focus

1. Two sessions, colliding call IDs, one parked: B gets B's own tool result (not A's cancelled artifact) — the corruption case.
2. A's speculated long-running sibling + B's colliding call: B does not block on A's future.
3. Single session park→approve→resume: the run's own speculated result survives (real output, not `Exit code -1`).
4. A shell streaming >1 MiB: buffer stays capped, `BashOutput`/tails show the truncation marker with the dropped byte count, the tail is preserved.
5. Released session: its shell/task records are gone from snapshots; `activity.released` remains (invariant).

---

### Task 1: Per-run speculative registry and run-ctx futures

**Files:** internal/agent/streamingexec.go (registry keying, ctx derivation), internal/agent/agent.go (dispatch/match/consume sites ~1379/1529/1616-1686; run-ctx carrier following the memory-scope pattern in internal/agent/memoryscope.go), internal/server/run.go (run end → consume the run's futures, if the server must trigger it — prefer an agent-side hook keyed by run id so TUI/SDK runs are covered), tests.
**Requirements:** Spec F1+F2. Key = run id (from `tracing.RunFrom(ctx)`) + call id; futures derived from a per-run context carrier (the context the runner passed to `agent.Stream`/`Invoke`, NOT the graph's per-superstep derived ctx), so a sibling's interrupt-pause does not kill them; futures consumed/cancelled when the run ends (every terminal path: done, error, cancel); duplicate call id within one run → loud error result naming the collision. Port the five repro scenarios from internal/server/stall_repro_test.go into a permanent file (e.g. internal/server/runisolation_test.go), delete the throwaway. Existing suites stay green (streamingexec_test, interrupt/resume, wake).
**Commit:** `fix(agent): speculative tools are isolated per run and live on the run context`

### Task 2: Background retention caps

**Files:** internal/tools/background.go (buffer cap + dropped counter + marker in Output/Tail), internal/tools/todo.go untouched, internal/agent/task.go (finished-record cap per session), internal/agent/activity.go (ReleaseSession removes records).
**Requirements:** Spec F3+F4. `MaxBufferedOutputBytes = 1 << 20` keep-tail with dropped count; marker `[N older bytes truncated]` prepended by Output/Tail when dropped>0 (once, not per poll); `MaxFinishedRecordsPerSession = 20` newest finished shells and tasks per session (running always kept; unscoped "" bucket same rule); ReleaseSession deletes the session's records. Cron F5: cache next-fire per entry at add/fire (internal/agent/cron.go). Tests for each cap + marker + release cleanup + snapshot reflects pruning.
**Commit:** `fix(tools): bounded background buffers and finished-record retention`

### Task 3: Deferred tests and debounce max-wait

**Files:** internal/server/wake_test.go (Quiet stall-requeue branch: failed start requeues a Quiet item and it never wakes), internal/agent/activity_test.go (emitActivity release-race contract: an observer call racing ReleaseSession cannot queue/wake — assert the documented best-effort), web/src/stores/activity.ts (max-wait: force the fetch when a debounced refresh has been pending >1s), web/src/stores/activity.test.ts.
**Requirements:** Spec F6 + the two deferred tests. vitest for the max-wait (fake timers).
**Commit:** `test: wake quiet-requeue and activity race contracts; feat(web): activity debounce max-wait` (split if cleaner)

### Task 4: Gates and docs

**Requirements:** All gates green; README.md + README.zh-CN.md note the BashOutput truncation behavior (1 MiB tail, marker) and the finished-item history cap; remove the throwaway repro file if any remnant.
**Commit:** `docs: bounded background output and history`

# Run-isolation for speculative tools + background retention — design

Date: 2026-11-08
Status: approved for implementation (user directed completion of the two filed follow-ups)
Builds on: the three completed plans of branch feature/bg-loop-plugin-surface.

## 1. Intent

Close the two follow-ups filed by the activity-panel final review:

1. **Cross-session run interference (the "runs-layer stall").** Investigation (deterministic Go repro, untracked `internal/server/stall_repro_test.go`) disproved the global-lock theory: a parked run holds nothing. The real defect is in speculative tool execution: `modelNode` speculatively runs read-only tool calls during model streaming into an **agent-global registry keyed by bare call ID** (`internal/agent/streamingexec.go` — its own comment assumes IDs are unique across concurrent runs; the e2e's per-conversation `call_N` numbering violates that), with futures derived from the **model-stream ctx**. When a sibling tool's `GraphInterrupt` pauses the graph, the fork's stream iterator cancels its derived ctx → every speculated sibling is killed and resolves to `Exit code -1`. Consequences: (a) session A parks → its dead `call_1` future persists globally → session B's `call_1` consumes A's cancelled artifact (result corruption); (b) if A's speculated sibling is still running, B blocks in `toolFuture.wait` (stall); (c) single-session: approve A's prompt → resume consumes its own cancelled future (`Exit code -1` instead of the real output).
2. **Unbounded retention.** Background shell output buffers grow forever (`backgroundShell.buf`, even while running); finished shell/task records are never pruned (ids never reused, `TaskManager.visible` scans O(next)); `ReleaseSession` kills a session's items but keeps their records; cron `next()` rescans (~45ms for never-matching specs) on every snapshot. Small deferred items: Quiet stall-requeue branch untested; `emitActivity` release-race contract untested; the frontend activity refetch debounce has no max-wait (a sustained signal cadence could postpone a fetch indefinitely).

## 2. Decisions

| # | Decision |
|---|---|
| F1 | Speculative registry is keyed by **(run id, call id)**; futures are derived from the **run context**, not the per-superstep stream context. A run's futures are consumed/cancelled when its run ends. Same-process resume (parked run keeps its ctx alive) still reuses its own futures. |
| F2 | Duplicate speculative IDs within a registry are rejected loudly (error result naming the collision), never silently absorbed — defense even though per-run keying already isolates runs. |
| F3 | Shell output buffers are capped (`MaxBufferedOutputBytes = 1 MiB`, keep-tail) with a dropped-bytes counter; `BashOutput`/tails prepend `[N older bytes truncated]` when bytes were dropped. Model-facing and honest, no silent loss. |
| F4 | Finished shell/task records are capped per session (keep the newest `MaxFinishedRecordsPerSession = 20`; running records always kept); `ReleaseSession` removes the session's records entirely (todos/queue already dropped). `activity.released` and session tombstones stay forever — they ARE the invariant and are tiny. |
| F5 | Cron `next_fire_at` is computed once per entry (at add and after each fire) and cached; snapshots read the cache. |
| F6 | The frontend activity debounce gains a max-wait (force fetch after 1s pending). |

## 3. Approach

Semantic-owner rule as before: run isolation lands in the agent's speculative-execution layer (registry keying, ctx derivation, run-lifetime consumption — the run ctx is threaded via a per-run carrier following the existing memory-scope pattern); buffer/record caps land in the managers (`BackgroundShells`, `TaskManager`); the cron cache in `cronScheduler`; debounce max-wait in the activity store. The investigation's five repro scenarios become permanent tests.

## 4. Out of scope

Fork changes (none needed); cross-process resume of speculated work (documented pre-existing gap); per-session Agent instances; PG-ledger form wiring.

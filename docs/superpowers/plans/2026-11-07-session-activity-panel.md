# Session activity panel — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Per-session activity panel (Todo / Background / Scheduled) in the web chat header, backed by session-scoped, correctly-terminating background state and a signal-only change event.

**Architecture:** Fix the background layer's ownership/termination/status defects at their semantic owners (managers, executor, server run assembly), add an Agent activity seam + REST snapshot/actions + `activity_changed` signal, then a Pinia activity store and a header-anchored popover component.

**Tech Stack:** Go 1.26 (internal/agent, internal/tools, internal/platform/executor, internal/server), Vue 3 + Vite + Pinia + vitest (web/), existing `ui/` primitives and tokens.

**Spec:** `docs/superpowers/specs/2026-11-07-session-activity-panel-design.md` (binding decisions D1–D4, form, tabs).
**Per-task guidance (outside repo, read the cited lines):** `/Users/yuhaochen/Documents/codebase/projanvil/woca-internal-docs/bg-tasks-panel-pm-2026-11-07.md` (PM doc) and `/Users/yuhaochen/Documents/codebase/projanvil/woca-internal-docs/bg-tasks-panel-uiux-2026-11-07.md` (UIUX doc). Where a PM section says "waits on D1/D2/D3" or "waits on the UIUX prototype", apply the spec's decision table.

## Global Constraints

- Work in `/Users/yuhaochen/Documents/codebase/projanvil/woca/.worktrees/bg-loop-plugin-surface`; one commit per task; code/comments/commits in English (zh-CN locale strings stay Chinese; i18n changes always touch both locales).
- TDD: a failing test that reproduces each N1–N5 defect before the fix (N3 especially: record the pre-fix failure in the task report).
- No fallbacks: unknown or foreign-session ids are explicit not-found; unsupported deployment form returns 501; no swallowed errors (no `catch {}` in new frontend code); no retry ladders.
- Fix at the semantic owner: session checks inside each manager; run identity in the server's run assembly; process-tree termination in the executor; the server never re-filters.
- Form differences only via `server.Config` injected in servefront; no request-origin sniffing in core paths.
- Global SSE stream carries no item content: `activity_changed` = session id only (D4); clients pull `GET` snapshots.
- SSE contract sync: `ci/check-sse-contract.sh` must pass — the new event type lands in Go and `web/src/api/events.ts` in the SAME commit (B8).
- Frontend: Pinia setup-store style (`web/src/stores/memory.ts` as reference), tokens only, reuse `ui/` primitives (`WDropdown`, `WIconButton`, `WChip`, `WConfirmDialog`), black/white/gray theme; no new visual system.
- Gates (T1): `go test ./...`, `go test -race` on changed packages, `make lint` (only the 2 known pre-existing e2e/lab errcheck findings allowed), `ci/check-sse-contract.sh`, `cd web && bun run test && bun run build && bun run check:tokens`, then `go build ./cmd/woca`. Never commit `web/package-lock.json`.

## Review Focus

1. A foreign session's shell/task id passed to `BashOutput`/`KillShell`/`BashInput`/`TaskOutput`/`TaskStop` or to the REST actions returns not-found, and no error text lists another session's ids (B2, B4, B8).
2. Killing `sh -c 'sleep 100 & sleep 100'` terminates the whole tree and the item leaves `running` promptly (`Wait` returns) (B3).
3. Stopping a run or an approval timeout leaves its background subagents running with honest status (D3, B4).
4. A user stop from the panel never wakes the agent; the stop notification reaches the model on the next user run (D1, B6).
5. Deleting a session with running items stops them and never triggers a wake for it; restart restores todos from the sidecar (D2, B9).

---

### Task 1: B1 — Server run-scope invariant (N2)

**Requirements:** PM doc lines 647–661. Every server-initiated run (plain, goal, session-less, btw, wake) carries `tracing.WithRun(ctx, runID, sid)` with the final non-empty sid; shared helper `newRunContext`. Tests in new `internal/server/runscope_test.go` (`TestGoalRunCarriesRunScope`, `TestRunWithoutSessionIDScopesGeneratedSession`).
**Commit:** `fix(server): every run carries its session run scope`

### Task 2: B2 — Background shells — session-scoped access, lifecycle facts, change hook (N1)

**Requirements:** PM doc lines 662–719 (and §4.1–4.2 lines 199–226 for the ownership rule and status vocabulary). Foreign ids → not-found in `BashOutput`/`KillShell`/`BashInput`; unknown-id errors list only the caller's own ids; started/ended timestamps, exit code, who stopped it; a change hook the activity seam (B7) subscribes to.
**Commit:** `fix(tools): session-scoped background shell access and lifecycle facts`

### Task 3: B3 — Process-tree termination and shutdown cleanup (N3, N6)

**Requirements:** PM doc lines 720–742. Executor starts background processes in their own process group and kills the group; `WaitDelay` so pipe-holding grandchildren cannot block `Wait`; `Agent.Close` terminates background shells. Reproduction test first (pre-fix failure recorded).
**Commit:** `fix(executor): terminate background process trees and clean up on close`

### Task 4: B4 — Background Task / cron — session scope, status semantics, detach, timestamps, next_fire_at (N1, N4, N11)

**Requirements:** PM doc lines 743–796. Apply D3: background subagent ctx detaches from the launching run (`context.WithoutCancel`, values kept) — run stop / approval timeout no longer cancels it. Status vocabulary per PM §4.2 (failed and stopped distinct from completed); foreign ids not-found; numeric id ordering; cron `next_fire_at`; change hook.
**Commit:** `fix(agent): session-scoped background tasks with honest status`

### Task 5: B5 — Todos per session

**Requirements:** PM doc lines 797–817 (+ §4.5 lines 389–429). TodoWrite state keyed by run session; `""` session keeps today's single-session semantics for TUI/REPL.
**Commit:** `fix(tools): todos are scoped to the run's session`

### Task 6: B6 — User-stop semantics (D1)

**Requirements:** PM doc lines 818–860 (+ §4.6 lines 430–454). Apply D1: a user-initiated stop (panel/REST) produces a notification stating the user stopped it, queued as a Quiet notification that never triggers the wake driver; it is delivered at the next user run.
**Commit:** `feat(agent): user stops are quiet and attributed`

### Task 7: B7 — Agent activity seam

**Requirements:** PM doc lines 861–911. Per-session snapshot (todos, shells, tasks, cron) + change observer aggregating the B2/B4/B5 hooks; user-stop entry points that use B6 semantics.
**Commit:** `feat(agent): session activity snapshot and change seam`

### Task 8: B8 — HTTP API, `activity_changed` signal, assembly switch (+ SSE contract)

**Requirements:** PM doc lines 912–948 (+ §4.3 lines 227–326, §4.4 lines 327–388, §4.8 lines 466–479). Apply D4 (signal-only payload: session id). REST snapshot + kill/stop/delete actions with existing authz scopes; 501 when the assembly switch says unsupported; server httptest integration tests. The new event type is added to `web/src/api/events.ts` in this same commit so `ci/check-sse-contract.sh` passes.
**Commit:** `feat(server): session activity API and activity_changed signal`

### Task 9: B9 — Todo sidecar, delete-session cleanup, wake guard (N5, N10)

**Requirements:** PM doc lines 949–971 (+ §4.7 lines 455–465). Apply D2: deleting a session stops its running background items (the API reports counts so the UI confirm can list them) and the wake driver never starts a run for a deleted session; todos persisted per session in a sidecar (goal sidecar precedent) and restored on load.
**Commit:** `fix(server): persist session todos and clean up background work on delete`

### Task 10: F1 — Frontend API client and event handling

**Requirements:** PM doc lines 972–1012. Typed REST client functions for snapshot/actions; global-stream handling of `activity_changed` (and the already-typed `wake_capped`, `run_started.auto`) feeding F2. vitest coverage.
**Commit:** `feat(web): activity API client and event wiring`

### Task 11: F2 — Activity store and session wiring

**Requirements:** PM doc lines 1013–1058. Pinia setup store keyed by session: fetch on session switch and on `activity_changed` for the active session; actions call REST and re-fetch; 501 → `unavailable` state (never an empty list); delete-session confirm shows running counts (D2). vitest coverage.
**Commit:** `feat(web): session activity store`

### Task 12: F3 — Panel component and header entry

**Requirements:** PM doc lines 1059–1084; UIUX doc option A lines 96–144, interaction spec lines 196–266, data needs lines 273–285. Header button left of the Memory button with badge (running count; danger signal when a failed item is unviewed); anchored non-modal popover, collapsed by default; tabs Todo / Background / Scheduled with counts; per-item fields, actions (stop/kill/delete with confirm where destructive), empty and unavailable states, keyboard + a11y per UIUX §4.5; transcript notice cards unchanged. i18n en + zh-CN. vitest for rendering states and actions.
**Commit:** `feat(web): session activity panel in the chat header`

### Task 13: T1 — End-to-end integration and gates

**Requirements:** PM doc lines 1085–1107 (+ acceptance criteria §6 lines 520–571). Run every gate in Global Constraints; integration test(s) covering the Review Focus list end to end; README/README.zh-CN user-facing notes for the panel.
**Commit:** `test: session activity panel end-to-end gates` (docs may be a separate `docs:` commit)

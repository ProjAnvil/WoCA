# Session activity panel (todos / background / scheduled) — design

Date: 2026-11-07
Status: approved for implementation (user delegated all open decisions to the implementer; decisions below are binding)
Builds on: `2026-11-07-background-loop-and-plugin-surface-design.md` (same branch)
Research (outside the repo): `woca-internal-docs/bg-tasks-panel-pm-2026-11-07.md` (requirements, contracts, per-task guidance §9), `woca-internal-docs/bg-tasks-panel-uiux-2026-11-07.md` (audit, layouts, interaction spec)

## 1. Intent

The web UI shows nothing about a session's live work: TodoWrite and every background tool (Bash `run_in_background`, `BashOutput`, `BashInput`, `KillShell`, Task `run_in_background`, `TaskOutput`, `TaskStop`, `ScheduleCron`) render only as generic tool cards, and the server exposes no list/act API. Users cannot see what is running, what finished, or stop it. Add a per-session activity panel in the chat header, backed by session-scoped server state.

Research also found correctness defects in the background layer that the panel would surface verbatim (PM doc §1.2 N1–N11). They are fixed first:

- N1 shell/Task tool access is not session-checked (foreign ids readable/killable; unknown-id errors list every session's shell ids)
- N2 goal runs and session-less `POST /api/runs` carry no/empty run scope
- N3 kill signals only `bash`, not its process tree; grandchildren keep pipes open so `Wait` never returns
- N4 background Task inherits the tool-call ctx (run stop cancels it) and errors/cancellations are recorded as `completed`
- N5/N6 deleting a session or shutting down leaves background work running (and waking deleted sessions)
- N10 server never persists todos; N11 task list sorted as strings (`task-10` before `task-2`)

## 2. Decisions

| # | Decision |
|---|---|
| Form | Header-anchored popover (UIUX option A): a button in the ChatPanel header left of the Memory button; click opens an anchored, non-modal dropdown panel; collapsed by default. Not a side sheet, no sidebar changes (respects the two vetoed patterns). |
| Tabs | Three: Todo / Background (shells + subagent tasks) / Scheduled (cron). |
| Badge | Running background count; turns to the danger signal when a failed item is unviewed. |
| Transcript notices | Unchanged (dual track): transcript keeps the collapsed `notice` cards (what the model received); the panel is live state. |
| D1 user stop | Stopping from the panel does not wake the agent; the completion notification says the user stopped it and is delivered with the next user run (Quiet notification). |
| D2 delete session | Delete with running background items: the confirm dialog lists the counts; confirming stops all of them; a deleted session is never woken. |
| D3 run stop / approval timeout | Background subagents no longer die with the launching run (detached like shells, `context.WithoutCancel`); status reflects reality; stop them explicitly from the panel or `TaskStop`. |
| D4 events | Signal-only `activity_changed` event carrying only the session id on the global stream; the client re-fetches the session snapshot via REST. No content on the global stream. |
| Team task list | Not in the panel (process-wide shared list; needs team↔session ownership design first). |
| EE / multi-instance | Assembly switch: activity endpoints return 501 in dispatch mode; the panel shows "unavailable", never an empty list. |

## 3. Approach

Ownership stays at the semantic layer (user rule): session checks live in each manager (shells, tasks, cron, todos); run identity is set by the server's run assembly; process-tree termination lives in the executor; the server does not re-filter. Form differences enter only through `server.Config` assembly seams.

Contracts (REST snapshot + actions, `activity_changed`, todo sidecar) follow PM doc §4.3–4.5 with D1–D4 applied. UI follows UIUX doc §3 option A and §4 (state machine, micro-interactions, tokens, a11y, edge cases) using existing primitives (`WDropdown` positioning, `WIconButton` badge, panel-pop motion, black/white/gray tokens).

## 4. Out of scope

Team task list in the panel; EE worker / multi-instance activity; subagent-internal todos and shells (separate registries, PM N7); persistent cron; notice-card redesign.

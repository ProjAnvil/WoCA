# Graph-Native HITL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move all human-in-the-loop (permission asks, AskUser, plan approval) into dedicated pre-execution graph supersteps and foreground subagents onto Send/AddSubgraph topology, with session-scoped policy, once/session grants, and the matching web surfaces.

**Architecture:** Two-phase superstep split (`hooks` node derives decisions without interrupting; `approval` node asks once per batch), `route2` fans foreground Task calls out as `types.Send` into `AddSubgraph` child agent graphs whose results return via `Command{Graph: ParentGraph}`; the relay/batchMemo machinery is deleted. Drivers (server/TUI/CLI/SDK/goal) consume an interrupt LIST and answer scalar-or-NS-map.

**Tech Stack:** Go (langchain-golang v0.12.5, read-only fork), Vue 3 + pinia + vite web, Playwright e2e.

**Spec:** `docs/superpowers/specs/2026-11-16-graph-native-hitl-design.md`

## Global Constraints

- Worktree `.worktrees/graph-hitl`, branch `feature/graph-hitl` from main.
- Fork `langchain_port/langchain_golang` is READ-ONLY; only consume exported API.
- All code/comments/commits in English (zh-CN locale files excepted). No fallback/grace designs: explicit errors, closed outcome sets.
- Red test first, then green, per task. `go test ./internal/agent/... ./internal/server/... ./internal/runner/...` green before each commit; `go build ./...` too. Web changes: `cd web && bun run build` before any `go build` that embeds webdist; `bun run test` green.
- New settings key registered in all four explicit settings lists (see internal/settings conventions); edition differences only via assembly-injected seams (no runtime edition sniffing).
- Archived red tests live in `woca-internal-docs/relay-idempotency-repro-2026-11-08_test.go.txt` (+ `-x-2026-11-09_test.go.txt`); revive per task as instructed. They reuse `taskrelay_test.go` helpers (`pumpStream`, `transcriptModel`, `subReportModel`, `memoProbeTool`, `pausingTestTool`, `relayProbeExec`, `writeCall`).
- Checkpoint rule: never execute a paused batch ungated; legacy/unknown pause states must resolve to interrupted markers.

## Review Focus

1. Legacy relay-era checkpoints (pending interrupt on the `tools` node) must produce interrupted markers, never ungated execution — pinned by Task 9's legacy test.
2. Session grants must never override an explicit deny rule — pinned by Task 3's deny-wins test.
3. Def fingerprint change (edit) under a paused thread must error naming the def on resume, not silently re-run — pinned by Task 7's def-edit test.
4. Partial NS-map answers re-pause with exactly the unanswered children — pinned by Task 8's partial-answer test.
5. Concurrency cap rejects over-cap Task calls with a tool-result error; never queues, never deadlocks the superstep — pinned by Task 7's cap test.
6. Two concurrent sessions with different modes must not see each other's policy — pinned by Task 2's X1 revival.

---

### Task 1: Guard-ask denial is final (N5)

**Files:**
- Modify: `internal/agent/agent.go:1622-1631` (gate loop guard arm)
- Test: `internal/agent/relayidem_n5_test.go` (revive `TestReproN5GuardAskDenialOverridden` from the archive; drop its R-file siblings)

**Interfaces:**
- Produces: unchanged signatures; behavior contract "guard ask denial → run[i]=false, continue".

- [ ] **Step 1: Revive the red test** — copy `TestReproN5GuardAskDenialOverridden` verbatim; run `go test ./internal/agent/ -run TestReproN5 -v` expecting FAIL ("executed 1 time(s)").
- [ ] **Step 2: Fix** — in the `ToolAsk` branch, when `askPermission` returns false set `run[i] = false` and `continue` (deny final); on true, skip the regular gate but still run PreToolUse hooks (set `run[i] = true`, `consumed[i]` from the ask, continue past `a.gate`).
- [ ] **Step 3: Green + commit** — `feat(agent): guard-escalated ask denial is final`.

### Task 2: SessionPolicy seam — per-session mode/plan, rules + hooks reach subagents (D-S, A10, A2, X1)

**Files:**
- Create: `internal/agent/sessionpolicy.go` — `type SessionPolicy struct{ Mode config.PermissionMode; Plan bool; Grants map[string]bool }`, `type SessionPolicySource interface { For(session string) *SessionPolicy; SetMode(session string, m config.PermissionMode); SetPlan(session string, v bool) }`, ctx carrier `WithSessionPolicy(ctx, session string, src SessionPolicySource)` / `SessionPolicyFrom(ctx) (session string, src SessionPolicySource, ok bool)` (pattern: `runws.From`, agent.go:905-961). Default source when none injected: process-global singleton mirroring today's fields (X1 stays red against the default).
- Create: `internal/agent/sessionpolicy_test.go`
- Modify: `internal/agent/agent.go` — `checkPermission`/gate read mode+plan via `SessionPolicyFrom(ctx)`; `EnterPlanMode`/`ExitPlanMode` write through the source; `newSubagent` (agent.go:1979-1992) additionally passes `Permissions: a.perms`, `Hooks`, `HooksFor`, `ToolGuards` (live reads, not snapshots).
- Modify: `internal/server/agentapi.go:70-92` — `/api/agent/permissions` takes `{session, mode}`; server injects a server-owned `SessionPolicySource` at assembly (find the agent construction seam in `internal/agentbuild`); TUI console `mode` command routes through the same source (session = active session).
- Test: revive X1 (`relayidem_x_test.go`) with a per-session source — A enters plan, B approves exit, A's ExitPlanMode still asks; plus a new `TestSubagentInheritsRulesAndHooks` (deny rule + PreToolUse hook fire inside a subagent; today both are silently skipped).

**Interfaces:**
- Produces: `SessionPolicySource` consumed by Tasks 3/4/5; `SessionPolicyFrom(ctx)` used by gate paths; `Grants map[string]bool` populated by Task 3.

- [ ] Steps: red (X1 + inheritance test) → implement seam + wiring → green → `feat(agent): per-session permission policy seam; subagents inherit rules and hooks`.

### Task 3: Session grants (D-G once/session)

**Files:**
- Create: `internal/agent/grants.go` — `GrantKey(call messages.ToolCall) string` (Bash: `"bash:"+first-two-space-segments+"*"`; WebFetch: `"domain:"+host; other tools: ""), `ApplyGrant(policy *SessionPolicy, key string)`.
- Modify: `internal/agent/sessionpolicy.go` (Grants already declared), `agent.go` gate: grants consulted AFTER explicit `a.perms` rules, before mode; `streamingexec.go:313-335` `speculativeAllowed` also consults grants.
- Modify: `internal/agent/agent.go` ask path returns the grant key so Task 5's ApprovalAnswer can carry it.
- Test: `internal/agent/grants_test.go` — once vs session (second identical Bash call does not ask), deny-rule beats session grant, grant key shape, spec-executed call honors grants.

**Interfaces:**
- Produces: `GrantKey(call) string`; answer shape `{Outcome: "allow"|"deny", Grant: "none"|"session"}` (Task 5).

- [ ] Steps: red grants tests → implement → green → `feat(agent): once/session approval grants keyed by command prefix or domain`.

### Task 4: Enforced plan mode

**Files:**
- Modify: `internal/agent/agent.go` gate — plan=true behaves like ModeReadOnly restriction (reuse the `ModePlan/ModeReadOnly` arm, agent.go:1917-1918) for ALL sessions' calls; `ExitPlanMode` approval flips plan off (already via source).
- Test: `TestPlanModeBlocksWrites` (Write denied while plan on, without asking; ExitPlanMode asks; after approval Write proceeds).

- [ ] Steps: red → implement → green → `feat(agent): plan mode enforces read-only tooling`.

### Task 5: `hooks` + `approval` supersteps (S1′, fixes N2a/b, R4, R5)

**Files:**
- Create: `internal/agent/approval.go` — types `Ask{CallID, ToolName, ArgsSummary, Reason, Origin, SessionRuleKey string}`, `ApprovalRequest{Asks []Ask}`, `ApprovalAnswer{Decisions map[string]AskDecision}` with `AskDecision{Outcome string; Grant string}`; node builders `a.hooksNode()` and `a.approvalNode()`; decisions channel name `"decisions"`, entry type `callDecision{Run bool; Override *coretools.Result; PreFeedback string}`.
- Modify: `internal/agent/agent.go:609-642` graph build: `model →(route1)→ hooks → approval →(route2)→ tools`; `route1` = old `route` (agent.go:1875); `route2` (new, this task: everything → `tools`; Send fan-out lands in Task 7); `toolsNode` slimmed to read `decisions` (spec-future skip + execution + PostToolUse + drain + summaries + MaxTurns), gate loop deleted; recursion: verify `cfg.RecursionLimit` default covers +2 supersteps/round, bump default if marginal.
- Modify drivers for the new single-interrupt ask envelope: `internal/server/run.go:206-227`, `cmd/woca/main.go:916+`, TUI ask loop, `sdk/interrupt.go` — all decode BOTH the new `ApprovalRequest` and the legacy `PermissionRequest`/`SubagentInterrupt` (the relay lives until Task 7). Web compat (audit M4): when the envelope carries exactly one ask, the server ALSO emits the legacy flat fields (`tool_name/tool_args/reason/subagent`) so the old InterruptModal keeps working until Task 11.
- Keep `batchMemo` + `ReplayInterruptConsumption` until Task 7 (audit M1b): between Task 5 and Task 7 the relay still re-enters the tools node; deleting the memo here would re-execute siblings (mid-window red).
- Legacy guard from day one (audit C1): the new tools node treats a pending interrupt / queued resume value addressed to itself as a legacy checkpoint — synthesize interrupted markers for the batch and error, NEVER execute ungated. Task 9 keeps only the crash-face synthesis + README.
- Tests: revive R4, R5, N2a, N2b (relay still present; hooks now fire once in the `hooks` node), plus `TestAskEnvelopeCarriesGrantKey`, `TestApprovalNodeResumesOnce`, `TestLegacyToolsPauseMarkedInterrupted` (moved here from Task 9 per C1).

**Interfaces:**
- Produces: `ApprovalRequest/ApprovalAnswer` wire types (Tasks 7/8 extend to lists/maps); `decisions` channel consumed by tools node; `route2` extension point for Task 7.

- [ ] Steps: red (R4/N2a first) → split nodes + envelope + driver decode → green all four + new → `feat(agent): pre-execution hooks/approval supersteps; decisions persist in graph state`.

### Task 6: Child subagent registry + AddSubgraph build (C3 shape)

**Files:**
- Create: `internal/agent/subagents.go` — `defFingerprint(def agents.Definition) string` (fnv over name+prompt+tools+model, 4 hex chars), `subagentNodeName(def) string` = `"subagent:"+sanitize(def.Name)+"-"+fp`, `type childRegistry struct{...}` mapping node name → child `*Agent` (built via the Task-2-extended newSubagent), `rebuildGraph()` extracted from `agent.go:609-642` registering `g.AddSubgraph(name, child.Graph())` for the union of defs across the wsAgents cache; wsAgents `Set` schedules a debounced rebuild under `a.mu`.
- Modify: `internal/agent/agent.go` — graph construction moves into `rebuildGraph()`; missing/edited def on resume errors naming the def.
- Test: `internal/agent/subagents_registry_test.go` — fingerprint stability/variation, node-name collisions across workspaces resolve, def edit changes node name, rebuild keeps other node names stable.

**Interfaces:**
- Produces: `subagentNodeName(def)`, `rebuildGraph()`, child `*Agent` per node (Task 7 builds Send args against these).

- [ ] Steps: red registry tests → implement → green → `feat(agent): content-addressed subagent child registry and graph rebuild`.

### Task 7: Send fan-out, ParentGraph backfill, relay deletion, concurrency cap (β core)

**Files:**
- Modify: `internal/agent/task.go` — delete `runForegroundTask`/`relaySeq`/`subagentStarted`/`SubagentInterrupt` handling; foreground Task becomes `sendArgs(call, def, session) map[string]any` (`{messages, subagent:{def, parentCallID, session}}`); `route2` in `agent.go` emits Sends for foreground Task calls with resolvable defs under the cap, everything else to `tools` as overrides (unknown def / over-cap → tool-result error).
- Modify: child exit — child graph terminal returns `lgtypes.Command{Graph: lgtypes.ParentGraph, Update: {"messages": [messages.Tool(parentCallID, report)]}}` (fork types.go:31; subgraph.go:324-327 unwrap).
- Create: `internal/agent/subagentruns.go` — session-scoped active-children registry (start/finish/scan) feeding the cap (default 8 from settings key `subagents.max-concurrent`, Task 10) and later the Subagents tab.
- Modify: `internal/agent/streamingexec.go:239-247` — `specRunID` suffixes `"/"+toolCallIDFrom(ctx)` when present (per-child key isolation). Child construction ALSO finalizes spec §3.7's backend facts here: child registry entries share the SESSION's `BackgroundShells` (pass the parent registry into the child config), `BackgroundShellStatus.Owner` field lands in `internal/tools/background.go`, and lifecycle events (started/waiting/done/failed/stopped + tokens) publish through the `subagentruns` registry.
- Concurrency cap: hard-coded `defaultMaxSubagents = 8` until Task 10 wires the settings key (the existing `maxBackgroundTasks` is a const at task.go:361, not config-read).
- Tests: revive R1 (child speculated sibling runs once — child instance persists), N1 (40-iteration misroute → 0; answered at the Agent layer with a map resume — full driver routing lands in Task 8), new `TestSubgraphParallelSameDef` (two Sends one def, both reports backfill to their call ids), `TestSubgraphCapRejects` (9th foreground Task → override error, no queue), `TestTaskBackgroundUnchanged`, `TestSubagentShellOwnerAttribution` (Owner + session-shared shells). Delete superseded `TestForegroundSubagent*` relay tests and adapt `TestResumePathsStoreLastThread` (taskrelay_test.go:650, relay semantics) to the subgraph shape. "Deny & stop subagent" affordance is NOT implemented (follow-up ticket; the UI ships deny-only).

**Interfaces:**
- Produces: child pause interrupts bubble natively (Task 8 wires answer routing); `subagentruns` snapshot API for Task 12.

- [ ] Steps: red (N1 first) → Send routing + backfill + registry + deletions → green → `feat(agent): foreground subagents as Send fan-out subgraphs with parent-command result backfill`.

### Task 8: Interrupt-list drivers + NS-map answers (M1, M2)

**Files:**
- Modify: `internal/runner/runner.go:149-162` — iterate the pause's interrupt list; one card per interrupt (main `ApprovalRequest` or child value + NS); answer = scalar when 1 pending, `map[ns]any` when >1.
- Modify: `internal/server/run.go` park/answer — pending list per run (id+ns+kind), answer body `{answers: [{key, value}]}` with scalar fallback; `internal/agent/goalgraph.go:497-501` forwards the interrupt list, `GoalResume` returns map when >1 (M2).
- Modify: `cmd/woca/main.go` handler + TUI ask loop iterate cards; `sdk/interrupt.go` aggregate payload + list answer.
- Tests: revive X2 (asserts the documented barrier), new `TestPartialMapAnswerRepausesRemainder` (answer one of two children; exactly the other re-pauses), `TestCrossProcessSubgraphResume` (fresh Agent over same sqlite; completed sibling replays, paused child resumes; the fork lacks this coverage), `TestGoalMultiInterruptMapResume`.

**Interfaces:**
- Consumes: Task 5 envelope; Task 7 child interrupts.
- Produces: final wire protocol `{answers:[{key,value}]}` (frontend Task 11 consumes).

- [ ] Steps: red (partial-map + cross-process) → implement → green → `feat(server,runner): multi-interrupt pauses with NS-keyed answers`.

### Task 9: ε-min + legacy checkpoint detection + README (D4)

**Files:**
- Create: `internal/agent/epsilon.go` — on `Resume`/`ResumeCheckpoint` (agent.go:1117/1137): snapshot inspection via the fork checkpoint shape (`Checkpoint.Next` + `PendingWrites`, fork langgraph/checkpoint/checkpoint.go:49-50 — NOT internal/runstate); an unfinished superstep with missing tool results and no pending interrupt → synthesize `messages.Tool(callID, "Error: interrupted before completion; the tool may or may not have run. Verify before retrying.")` for every unanswered call and close the turn interrupted; never invoke the paused batch. (Legacy tools-node detection already shipped in Task 5.)
- Modify: `README.md:72-74` + `README.zh-CN.md:68-69` — "pauses (pending approvals) resume across restarts; calls interrupted by a crash are marked interrupted and never re-run".
- Tests: revive R2, R3 (both now expect interrupted markers, zero re-execution), new `TestLegacyToolsNodeCheckpointMarkedInterrupted`.

- [ ] Steps: red (R2) → implement → green → `feat(agent): interrupted-marker resume; README pause/crash semantics`.

### Task 10: Settings key + EE console seam (D12)

**Files:**
- Modify: `internal/settings` (four explicit lists) + wherever `maxBackgroundTasks` reads config — add `subagents.max-concurrent` (int, default 8) consumed by Task 7's registry; EE: configsvc pack key + console edit field (follow the builtin-plugins key pattern), injected at assembly.
- Test: settings round-trip + default + EE-pack delivery test (follow existing configsvc test patterns).

- [ ] Steps: red → implement → green → `feat(settings): subagents.max-concurrent (default 8), console-managed in EE`.

### Task 11: Web — approval stack, grants, session store

**Files:**
- Create: `web/src/components/hitl/HitlStack.vue`, `HitlCard.vue`, `web/src/components/activity/OriginBadge.vue`, `web/src/components/settings/GrantsPanel.vue`
- Modify: `web/src/stores/sessions.ts:56,129` + `stream.ts:204,210` (interrupt single slot → ordered per-ID map, replay dedupe), `web/src/api/chat.ts:56-60` (answer body `{answers:[{key,value}]}` + session-scoped mode API), `web/src/types.ts:166`, i18n `locales/{zh-CN,en}/chat.ts` (PM key list), `composer/ComposerChips.vue:62` session note.
- Test: vitest for store map/replay dedupe, HitlStack rendering N cards + grant button rule-key preview, GrantsPanel list/revoke; testids per spec §4.2.

- [ ] Steps: red vitest → implement → green + `bun run build` → `feat(web): multi-card approval stack with once/session grants`.

### Task 12: Web — Subagents tab, initiator column, transcript

**Files:**
- Create: `web/src/components/activity/SubagentTab.vue`, `SubagentRow.vue`, `TranscriptView.vue`
- Modify: `web/src/components/activity/ActivityPanel.vue` (TABS + width 400→440), `BackgroundTab.vue` (initiator column), i18n `activity.ts`.
- Backend feeding: expose the Task-7 `subagentruns` snapshot + shell `Owner` through the activity API seam (`internal/agent/activity`) — frontend only here; all backend facts landed in Task 7.
- Test: vitest (rows, expand, empty state, initiator filter) + Go test for Owner field propagation.

- [ ] Steps: red → implement → green → `feat(web,activity): subagents tab, shell initiator attribution, readonly transcripts`.

### Task 13: e2e + full gates

**Files:**
- Create/extend: `web/e2e` Playwright specs per PM scenarios ①-⑤ (two parallel subagent approvals; session grant suppresses re-ask + rule listed; deny → child continues; TTL timeout card; subagents tab/overlay/filter).
- Run gates: `go test ./...` (+ `-race` on internal/agent,internal/server,internal/runner), coverage of touched packages ≥ baseline, `cd web && bun run test && bun run build`, Playwright suite green, `go build ./...`.
- [ ] Steps: write e2e → run all gates → fix → `test(e2e): graph-native HITL scenarios`.

### Task 14: Sweep + docs

- [ ] Fix stale comments (`agent.go:361-369`, `task.go:1000-1004`, `sdk/plugin.go:89`), delete now-dead relay test helpers (`relayProbeExec`, `subReportModel`, `promptFileModel`, `holModel` — only where no remaining test uses them), README upgrade note (legacy threads → interrupted markers), English gate over all new files, final full-suite run, merge-ready summary.

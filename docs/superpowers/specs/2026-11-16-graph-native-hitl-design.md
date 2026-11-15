# Graph-Native HITL — Design Spec

Date: 2026-11-16 · Status: draft for audit · Baseline: main @ fdc4f2dd
Research: ../.. (woca-internal-docs/relay-idempotency-analysis-2026-11-08.md, v4)

## 1. Problem

Reproduced defects (12 red tests, archived): relay rebuilds subagents (R1), cross-process rerun re-executes approved calls (R2/R3), gate re-derivation misroutes consent after plan approval / mode switch (N2a/b), parallel Task approvals misroute 35/40 (N1), guard-ask denial overridden (N5), hooks re-fire on resume (R4/R5), plan/mode leak across sessions (X1), child approval waits for slowest sibling (X2), subagents drop user permission rules (A10) and hooks/guards (A2).

Root cause: mid-execution approvals modeled as one positional graph pause per node (analysis §3).

## 2. Decision (user-approved 2026-11-16)

Go straight to the final form, leveraging langchain-golang natively (fork stays read-only):

1. **S1′ approval node**: all gates/hooks/asks run in a dedicated superstep between model and tools; decisions persist in graph state; the tools node never interrupts.
2. **β subagent topology**: foreground Task calls fan out via `types.Send` into `AddSubgraph` child agent graphs; child interrupts bubble with per-task NS; resumes are maps keyed by NS/ID; completed siblings persist at pause and replay without re-run. X2 (superstep barrier) is accepted as a known cost.
3. Subagents **live-inherit** the parent session's permission mode + rules + session grants (A10/A2 fixed with it).
4. **D-G grants**: approval answers are once or session (prefix/domain key; never override deny; agent messages never count as approval). Persistent tier deferred (follow-up ticket).
5. **D-S**: permission mode and plan flag are per-session; plan mode becomes enforced (read-only tool whitelist + ExitPlanMode approval).
6. **D6**: background shells are session-owned with an Owner attribution; subagent lifecycle becomes session-visible.
7. **D12**: foreground+background subagent concurrency cap, default 8, settings key (personal) / console key (EE); over-cap rejects with a reason, never queues.
8. **D4 ε-min**: resuming a thread whose last turn crashed mid-superstep synthesizes `interrupted` tool results for calls with missing results; README rewritten (pauses resume across restarts; crashed calls are marked interrupted, never re-run).
9. Out of scope: EE crash delivery semantics (D7), worktree isolation (D11), persistent grants, ε-full replay declarations.

## 3. Backend design

### 3.1 Graph topology (internal/agent)

```
model ──(route1)──▶ hooks ──▶ approval ──(route2)──▶ tools ────────────┐
                                            │                          ├─▶ model (loop)
                                            └──*types.Send──▶ subagent:<def>-<fp>
```

Audit fix C1: gates/hooks and the user ask are TWO supersteps, because an interrupting node re-executes from its start on resume (fork graph.go:2817-2821) — hooks inside the asking node would re-fire (the original R4/R5 defect).

- `route1`: AI msg with tool calls → `hooks`; else END.
- `hooks` node (sequential, NEVER interrupts): today's gate loop minus every user interaction — speculative-future skip (spec calls already ran PreToolUse at streamingexec.go:387-398; skip both gate and hook), guards (N5 fix: ask-denial `continue`s), PermissionRequest/Notification hooks, PreToolUse hooks, rule/mode/grant evaluation (grants consulted here AND in speculativeAllowed). Writes the `decisions` channel: per call {outcome: allow|deny|ask, override result?, preFeedback?, consumed:0}. AskUser/plan-approval calls produce their inputs here. Re-execution-safe: no side effects, no Interrupt calls, pure derivation (batchMemo deleted entirely).
- `approval` node: collects every `ask` decision + AskUserQuestion + plan approval into ONE interrupt `ApprovalRequest{Asks: []Ask{CallID, ToolName, ArgsSummary, Reason, Origin, SessionRuleKey}}`; resume value `ApprovalAnswer{Decisions: map[callID]{Outcome: allow|deny, Grant: none|session}}`; merges ask outcomes into `decisions` and AskUser/plan override tool messages into `messages`. Its pre-interrupt work is pure list-building, so resume re-execution is harmless.
- `route2`: per foreground Task call with resolvable def under the concurrency cap → `*types.Send{Node: "subagent:<name>-<fp>", Arg: childState}`; everything else (non-Task calls, background Tasks, rejected/unknown-def Task calls) → `tools` as pre-decided entries.
- `tools` node: reads `decisions`; executes allowed calls concurrently; NO gate logic, NO interrupts (old tools-node interrupts are gone; mid-turn drain, PostToolUse, summaries, MaxTurns stay). Two extra supersteps per round: account under the existing recursion budget (verify default; bump if tight).

### 3.2 Subagent children (internal/agent/task.go rewrite)

- Child registration: node name `subagent:<sanitized-def-name>-<fp4>` where fp4 = short hash of the def's content fingerprint (prompt+tools+model). Content addressing makes cross-workspace same-name defs distinct and def edits change the node name. The parent graph registers the UNION of defs seen across workspaces (wsAgents cache + agents dir); a def-set change (wsAgents Set / LRU eviction) schedules a debounced parent recompile under `a.mu`. No per-run graph pinning: resume compatibility rests on stable node names (planResume re-plans from the checkpoint's Next; task IDs hash node+arg, graph taskid.go); a def removed/edited under a paused thread → explicit resume error naming the def (no silent re-run).
- Audit fix C2 (result backfill): the child agent graph ends with a terminal step that returns `lgtypes.Command{Graph: lgtypes.ParentGraph, Update: map[string]any{"messages": [messages.Tool(parentCallID, finalReport)]}}` (fork types.go:31; invokeSubgraph turns ParentCommandError into the node's Command result, subgraph.go:324-327). `parentCallID` rides the child state. The child's own messages therefore stay in the child namespace; only the report merges into the parent.
- `Send.Arg` = child initial state: `{messages: [Human(prompt) | fork history], subagent: {def, parentCallID, session, origin}}`; unknown keys fall back to LastValue (fork state.go:51-56). Parallel calls of one def = multiple tasks of one node (arg-hash task IDs; reference TestSubgraphParallelInterruptsMapResume).
- Per-def child Agent is a singleton → per-run keys incorporate `toolCallIDFrom(ctx)` (withToolCallThread exists) so parallel same-def children cannot collide in spec/batchMemo-style registries.
- Delete: `SubagentInterrupt`, `runForegroundTask` re-entry, `relaySeq`, `subagentStarted`, exec-phase `batchMemo` + `ReplayInterruptConsumption`. Goal loop relay is KEPT but map-aware (M2 below).
- Background Tasks / TaskOutput / TaskStop: unchanged through `tools`.
- Concurrency cap: session-scoped active-foreground-children registry (`a.subagents`, also the Subagents-tab source); `route2` rejects over-cap Task calls to `tools` as override errors ("subagent concurrency limit reached (N); retry later or run in background").

### 3.3 Session state + grants (internal/agent + server)

- `SessionPolicy` (mode, plan, session grants) in a server-owned per-session store, injected via ctx (pattern: runws.From, agent.go:905-961). `/api/agent/permissions` takes session id (agentapi.go:70-92 today global); TUI SetMode goes session-scoped too. Subagent `checkPermission` reads the live session policy + user rules (A10 fix) and runs hooks/guards (A2 fix: pass Hooks/HooksFor/ToolGuards into the child config). Headless/EE default: asks fail closed (deny).
- Plan mode enforced: plan=true restricts to read-only/CatMeta (reuse the ModeReadOnly arm); ExitPlanMode approval flips it off.
- Grant `session` tier: keyed by Bash command prefix (first 2 segments) or WebFetch domain; consulted before mode; NEVER overrides explicit deny (permissions.Rules precedence deny>ask>allow already enforces; grants slot after explicit rules).

### 3.4 Interrupt wire protocol + drivers (audit fix M1)

A pause carries EITHER the main approval interrupt OR one superstep's child interrupts — never both (interrupts aggregate per superstep, graph.go:1827-1845). Protocol:

- Server `handleInterrupt` receives the pause's interrupt list (each with its fork NS/ID + decoded value); renders one card per interrupt; the answer API takes a list `[{key: ns-or-id, answer}]`. Resume value: fork map keyed by NS when >1 pending (resume.go:553-567), scalar when exactly 1 (ApprovalAnswer for the main node, single child answer otherwise). The server translates cards→map/scalar. Per-card cancel = deny that card (the main approval interrupt is atomic — all its asks answered in one ApprovalAnswer).
- Partial child answers: unanswered children re-pause with only the remainder (fork native). runner.go:149-153 iterates the interrupt list; goal relay (goalgraph.go:497-501) forwards the LIST and GoalResume returns the map when >1 (M2).
- TUI/CLI iterate cards; SDK OnInterrupt gets the aggregate and returns the list.

### 3.5 ε-min (D4) + legacy checkpoints (audit fix M3)

`Resume`/`ResumeCheckpoint` (agent.go:1117/1137): load the pause snapshot; if its pending interrupts name the `tools` node (impossible in the new topology — interrupts never live there) the thread is a LEGACY relay-era checkpoint → never resume it: synthesize `Error: interrupted (pre-upgrade checkpoint); the tool may or may not have run — verify before retrying.` tool results for all unanswered calls, close the turn as interrupted. Same synthesis applies to any thread whose last superstep crashed with missing results (detection: Snapshot.Next/Tasks with no pending interrupt and missing tool results, snapshot.go:20-38). No tool ever re-executes.

### 3.6 Settings / EE

- New settings key `subagents.max-concurrent` (int, default 8; registered in the four explicit settings lists; personal UI in settings panel). EE: configsvc-managed key in the config pack, console-editable; seam-injected at assembly. No edition sniffing in core paths.

### 3.7 Activity surfaces (backend facts)

- `BackgroundShellStatus` gains `Owner string` ("main" / "subagent:<def>#<callID>"); subagents use the session-shared `BackgroundShells` (A6 id collisions disappear).
- Subagent lifecycle events (started/tool-progress/waiting/done/failed/stopped + token usage) published on the session event bus; `a.subagents` registry exposes the Subagents snapshot (def, call id, state, current tool phrase, elapsed, tokens).

## 4. Frontend design

Filled from PM + UIUX research (both audited against modern-web-guidance). Files to touch (PM, verified against web/src): `components/InterruptModal.vue:34-47` (single approve/deny → multi-card stack), `stores/sessions.ts:56,129` + `stream.ts:204` (interrupt single slot `Record<sid, Interrupt|null>` → per-ID Map), `api/chat.ts:56-60` (answer body gains `request_id`/`scope`), `types.ts:166` (payload id/source/ns), `composer/ComposerChips.vue:62` (mode chip → session-scoped), `activity/ActivityPanel.vue` + `BackgroundTab.vue` + new `SubagentsTab.vue`, i18n `locales/{zh-CN,en}/{chat,activity}.ts`.

### 4.1 Behavior spec (PM)

- **Multi-pending approvals: parallel stacked cards, not a queue.** Parallel subagents must show N origins at once; session decisions often need comparing requests. Card: origin badge (main / subagent name), tool + args summary (collapsed JSON >2KB), risk cue (write=red dot / `is_read_only`=green), elapsed, buttons [Allow once][Allow for session][Deny]. The session button previews the rule key it will create (`cmd:npm *` / `domain:github.com`).
- **Deny semantics**: the child receives the denial text and continues (primary); secondary link "Deny & stop subagent" on subagent cards.
- **Timeout/cancel**: per-card cancel (answer=cancelled for that request only, run keeps parked); 2h TTL cards turn into a persisted "timed out" state (server already emits EventTimeout, `run.go:246-250`), not a run error.
- **Granted-rules management**: session card footnote "N granted · manage" + Activity panel "grants" chip → list (rule key, origin, time), per-rule revoke.
- **Subagents tab**: row state machine `running → waiting-permission ⇄ running → done|failed|stopped`; waiting rows embed "go to approval" (focuses that card) + pending-count badge; empty state; readonly transcript opens as an in-panel overlay (user previously vetoed right-side sheets; jump-away loses context). Shells tab gains Initiator column (main dot / subagent monogram), click header to filter.
- **Session mode chip**: composer chip relabeled "this session"; header chip showing the session's current mode (plan mode shown solid + "exit needs approval").
- i18n keys (zh/en pairs): `chat.approveOnce/approveSession/denyRequest/denyAndStopSubagent/sessionRuleKey/pendingCount/requestCancelled/requestTimedOut/sourceMain/grantedRules`, `activity.subagents/subagentsEmpty/initiator/viewTranscript`.
- e2e testid contract: `permission-card[data-request-id]`, `permission-source`, `permission-tool`, `btn-approve-once/btn-approve-session/btn-deny`, `session-rule-key`, `subagent-row[data-subagent-id]`, `subagent-status`, `subagent-transcript`, `initiator-col`, `session-mode-chip`.
- modern-web-guidance self-audit (folded in): card stack = `role="list"/"listitem"` + `aria-live="polite"` announcements; per-button aria-labels include origin+tool; transcript overlay = native `<dialog>.showModal()` (no hand-rolled focus trap, focus returns to the invoking row); subagent tree = `role="tree"/"treeitem"`; keyboard = ↑↓/Enter row navigation reused from `BackgroundTab.vue:185-199`; one shared interval for elapsed ticks.

### 4.2 Visual/component spec (UIUX)

Materials: grayscale as texture, status color only as signal (tokens.css:199-215/315-330 semantic families; danger red is the retained alarm exception in mono). Motion restricted to the motion.css:1-14 four primitives (fade/rise/settle/reveal) + panel-pop; entry 320ms rise, answered-card settle 120ms, badge breathing reuses act-breathe.

- **Approval stack (HitlStack)**: modal overlay (z-overlay, scrim 32%) replacing InterruptModal's render layer; cards 520px wide (`.interrupt-card`, panels.css:667); 2px left risk rail (act-row precedent BackgroundTab.vue:272-297); origin badge = subagent name or `main` (mono 10.5px uppercase, radius-full, brand-soft) + 4-char call-id short code; tool name semibold + one-line mono ellipsis args (title=full); the session button inlines the rule key (`bash: git push*`, mono on warn-soft). Risk tiers: read_only→neutral, file write→warn, network/destructive→danger. Parallel stacked (never a serial queue); >3 cards fold into a "N more" count row (grid-rows 0fr↔1fr reveal); stack scrolls at 84vh. Per-card state machine pending→answering→resolved/error(inline alert+retry).
- **Shells tab initiator column**: origin badge at act-row head (`main` as muted 60% text by default; subagent name + #shortid); `OriginBadge.vue` new and shared with approval cards.
- **Subagents tab (list + in-panel expand, not a tree)**: current graph is main→subagent one level (plan 2026-09-12-subagent-hitl.md:356), trees waste the 400px panel; grandchild support reserved via `+N` fold + one indent level (--space-5). Row: status dot (running=brand breathing / done=ok / failed=danger / stopped=muted, color+text dual channel per BackgroundTab.vue:317-337), name, elapsed, tokens (tabular-nums). Click row → in-panel reveal expand of the readonly transcript (mono, max-h 200px scroll, copy button, `role="log"`); a centered overlay inside the popover would double-scrim — rejected.
- **Session mode chip**: popover gets a "applies to this session only" note line; active item gains a "this session" suffix badge; on switch the chip label fades 240ms to the new value + aria-live polite announcement (no toast — mode switches are frequent; failures keep the existing toast.error path ComposerChips.vue:62-70).
- **a11y reconciliation (PM × UIUX merged)**: the stack keeps modal semantics (role="dialog" + focus trap per WModal precedent; cards are role="group" aria-labelledby tool name); new-card announcements are aria-live polite with the announcer node inside the scrim container (never on inert background DOM); subagent list = role="list"/"listitem" + aria-expanded, ↑/↓ roving + Enter expand + Esc collapse copying BackgroundTab.vue:184-200; focus ring `outline: 2px solid var(--focus)`; touch targets ≥44×44 under 640px (panels.css:643-660 precedent).
- **testid contract (final; kebab + dynamic id suffix per `ws-row-${id}` convention)**: `hitl-stack`, `hitl-card-${requestId}`, `hitl-allow-${requestId}`, `hitl-grant-session-${requestId}`, `hitl-deny-${requestId}`, `hitl-rule-key-${requestId}`, `hitl-queue-more`, `grants-chip`, `grants-row-${key}`, `act-origin-${shellId}`, `initiator-col`, `sub-tab`, `sub-row-${subagentId}`, `sub-status-${subagentId}`, `sub-transcript-${subagentId}`, `mode-chip`, `mode-session-note`.
- **Components**: new `HitlStack.vue`, `HitlCard.vue`, `OriginBadge.vue`, `SubagentTab.vue`, `SubagentRow.vue`, `TranscriptView.vue`, `GrantsPanel.vue`; modified `ActivityPanel.vue` (TABS + width 400→440), `BackgroundTab.vue`, `ComposerChips.vue`, `sessions` store (interrupt single-slot → ordered per-ID map; dedupe on reconnect replay, stream.ts:210).
- **Edge cases**: same callId pausing twice folds into a count, not a new card; subagent ends while its card pending → card greys out "branch ended", buttons disabled; reconnect replay dedupes by request id.

## 5. Test plan

- Red-first: the 12 archived repros become permanent tests updated to the new topology — N1 asserts NS-routed correct delivery; R2/R3 assert interrupted-markers; X1 asserts session isolation; X2 flips to asserting the documented barrier; R1/R4/R5/N2a/N2b/N5 assert the fixed semantics.
- New: subgraph pause/resume incl. cross-process (fresh Agent over the same sqlite — fork lacks this coverage), partial-map answers, child-result backfill via ParentGraph command, concurrency-cap rejection, session grants (once/session, deny-never-overridden, grants in speculativeAllowed), plan enforcement, A10 rules inheritance, hooks/guards running inside subagents exactly once, ε-min synthesis, legacy-checkpoint detection (interrupt pending on `tools` → interrupted markers, never ungated execution), goal-loop multi-interrupt map resume, def-edit node-name change → resume error.
- Coverage: `go test ./internal/agent/... ./internal/server/...` coverage ≥ existing; race on touched packages; web vitest for new components/stores; Playwright e2e per §4.1 scenarios.

## 6. Rollout / compatibility

- Breaking: interrupt wire protocol (server↔web/TUI/SDK) — version the payload; old clients get scalar fallback for single asks.
- Checkpoint compat: old threads (relay-era subagent threads `*/sub-*`) are NOT resumable under the new topology → ε-min applies on first load (interrupted markers); document in upgrade notes.
- README: pause/resume promise rewritten per D4.

# Background-task loop closure — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Notifications (background shell/task completion, cron firings) reach the model even when its session is idle, waiting is blockable, background shells accept stdin, and the dead hook events fire.

**Architecture:** The mid-turn queue becomes session-scoped (`tracing.RunFrom` carries the session id already set by the server and EE worker); the Agent exposes a notification-observer seam; each surface (web server, TUI, line REPL) owns a wake driver that starts an ordinary run with the coalesced tagged message. Blocking waits reuse existing `done` channels; `BashInput` exposes the existing `executor.Process.Stdin()`.

**Tech Stack:** Go 1.26, langchain-golang graph, bubbletea TUI, Vue 3 web UI (vitest).

**Spec:** `docs/superpowers/specs/2026-11-07-background-loop-and-plugin-surface-design.md` (§5.1–5.5, §5.7)

## Global Constraints

- All comments/commit messages in English (zh-CN locale files excepted).
- Frontend changes: `npm run build` in `web/` BEFORE any `go build` (webdist embed rule).
- No fallback/retry ladders: wake failure logs and stops; nothing is dropped silently.
- Session key semantics: `""` = unscoped (delivered to any run); a scoped item never enters another session's run.
- Tools may import `internal/tracing` (verified: tracing imports only tenancy+zerolog, no cycle).

## Review Focus

1. Scoped notification for session A while session B's run drains mid-turn → must stay queued for A. Test: Task 1 (drain mismatch).
2. Notification queued after the last tool round of a finishing run, model answers next run without tool calls → still delivered (run-start prepend). Test: Task 5.
3. Two completions while idle → exactly one wake run, both payloads coalesced into one Human message. Test: Task 8.
4. Wake chain over `auto-wake-limit` → no new run, `wake_capped` event, items stay queued; user run resets the counter. Test: Task 8.
5. `wait_ms` bounded by the 600000 ms cap and honors ctx cancellation (interrupted run unblocks, no goroutine leak). Test: Tasks 2–3.

---

### Task 1: Session-scoped mid-turn queue

**Files:**
- Modify: `internal/agent/queue.go`
- Modify: `internal/agent/agent.go:1619` (toolsNode drain), `runTurn` and `streamWithBudget` bodies (run-start prepend)
- Test: `internal/agent/queue_test.go` (extend)

**Interfaces:**
- Produces (later tasks rely on these exact names):
  - `type QueuedInput struct { Text, Tag, Session string }`
  - `func (a *Agent) QueueNotificationFor(session, tag, text string)`
  - `func (a *Agent) DrainQueuedInputsFor(session string) []QueuedInput`
  - `func (a *Agent) QueuedInputCountFor(session string) int`
  - `func sessionFromCtx(ctx context.Context) string` (reads `tracing.RunFrom(ctx).SessionID`; `""` when absent)
  - existing `QueueInput`/`QueueNotification`/`DrainQueuedInputs`/`QueuedInputCount` unchanged (unscoped: Session "")

- [ ] **Step 1: Write the failing tests** in `queue_test.go`: `queue.drain("a")` returns unscoped + "a" items and leaves "b" items queued; `drain("")` returns everything; `QueuedInputCountFor("b")` counts only unscoped+"b". Agent-level test: with two notifications queued (`For("a")`, `For("b")`), a run whose ctx has `tracing.WithRun(ctx, "r1", "a")` receives only the "a" one.
- [ ] **Step 2: Run** `go test ./internal/agent/ -run 'Queue|Session' -v` → FAIL (Session field missing).
- [ ] **Step 3: Implement** `Session` on `QueuedInput`, session-aware `addTagged`/`drain(session)`/`count(session)`, the three new exported methods, `sessionFromCtx`. Change `toolsNode` (agent.go:1619) to `a.inputs.drain(sessionFromCtx(ctx))`. Add `prependQueued(ctx, history)` — drain(session); insert `formatQueuedInput` items *before* the trailing Human message (or append if history doesn't end in Human) — and call it at the top of both `runTurn` and `streamWithBudget` on the local history copy.
- [ ] **Step 4: Run** `go test ./internal/agent/ -v` → PASS (existing `TestBackgroundNotificationIdleDeliveredNextRun` still green).
- [ ] **Step 5: Commit** `feat(agent): session-scoped mid-turn queue delivered at run start`

### Task 2: Background shells — session capture, wait_ms, BashInput

**Files:**
- Modify: `internal/tools/background.go`, `internal/tools/default.go:20-21` (register), `internal/tools/bash.go` (tool description mentions wait/stdin)
- Test: `internal/tools/background_test.go`, `internal/tools/bash_test.go` (extend)

**Interfaces:**
- Consumes: Task 1 `QueueNotificationFor` (only at the agent wiring site, not here).
- Produces:
  - `func (b *BackgroundShells) SetNotify(fn func(session, notification string))` (signature change)
  - `func (b *BackgroundShells) Wait(ctx context.Context, id string, d time.Duration) (BackgroundShellStatus, bool)` — blocks until done/timeout/ctx; `ok=false` unknown id
  - `func (b *BackgroundShells) WriteStdin(id, input string, closeStdin bool) error`
  - `const MaxWaitMS = 600000` and exported `const NotificationOutputCap` (current unexported value)
  - tool `BashOutput` args gain `WaitMS int json:"wait_ms,omitempty"`; new tool `BashInput{ShellID, Input string; CloseStdin bool}`.

- [ ] **Step 1: Write failing tests:** Start a `sleep 1` shell with ctx `tracing.WithRun(ctx, "r", "sess-1")`; assert notify callback receives `("sess-1", …)`. `BashOutput` with `wait_ms=3000` on a `sleep 0.3 && echo done` shell returns exit 0 output within the call (measure elapsed ≥ 0.3s); `wait_ms=200` on `sleep 5` returns "still running" promptly. `BashInput{input:"hi\n"}` into `cat` shell → later `BashOutput` shows "hi"; `BashInput` on unknown id and on finished shell → error results, no hang.
- [ ] **Step 2: Run** `go test ./internal/tools/ -run 'Background|Bash' -v` → FAIL.
- [ ] **Step 3: Implement:** store `session` on `backgroundShell` captured in `Start` via `tracing.RunFrom(ctx)`; thread it through the `onDone` closure. `Wait`: select on `sh.done` / `time.After(d)` / `ctx.Done()`. `WriteStdin`: write to `sh.proc.Stdin()`, close when asked; guard finished shells (exited → error). Register `BashInput` in `default.go` next to `killShellTool(bg)`; keep `MustTool` + description pattern. Update the Bash tool description (bash.go:16) to mention `BashInput`.
- [ ] **Step 4: Run** `go test ./internal/tools/ -v` → PASS; update the `SetNotify` caller `internal/agent/agent.go:606` to the two-arg form routing into `QueueNotificationFor(session, "background-task-completed", text)`, then `go build ./...` green.
- [ ] **Step 5: Commit** `feat(tools): session-scoped shell notifications, BashOutput wait_ms, BashInput`

### Task 3: Background tasks — done channel, session, completion notification, TaskOutput wait_ms

**Files:**
- Modify: `internal/agent/task.go` (backgroundTask, TaskManager, taskOutput, runTask background branch)
- Test: `internal/agent/bgtask_test.go` (extend)

**Interfaces:**
- Consumes: Task 1 `QueueNotificationFor`; Task 2 `tools.NotificationOutputCap`.
- Produces:
  - `backgroundTask` gains `session string` and `done chan struct{}` (closed exactly once in `setFinalIfRunning` and `stop`, under the mutex)
  - `func (m *TaskManager) start(ctx context.Context, subject, description string) (*backgroundTask, error)` (new ctx param; captures `sessionFromCtx(ctx)`)
  - `func (m *TaskManager) SetNotify(fn func(session, text string))`
  - `taskOutputArgs` gains `WaitMS int json:"wait_ms,omitempty"` (cap `MaxWaitMS` from tools)

- [ ] **Step 1: Write failing tests:** background Task completing with ctx session "s1" → notify receives ("s1", text containing the task ID and the final report tail, capped). `TaskOutput{wait_ms: 5000}` on a task whose model sleeps ~0.3s returns the completed report without a second poll; `wait_ms=200` returns "still running". TaskStop closes `done` (a `Wait` returns promptly).
- [ ] **Step 2: Run** `go test ./internal/agent/ -run 'Task|Background' -v` → FAIL.
- [ ] **Step 3: Implement** per interfaces; in the background goroutine after `setFinalIfRunning`, call the notify callback with a `fmt.Sprintf` text mirroring `backgroundShell.notification()` shape (task id, status, description, report tail capped by `tools.NotificationOutputCap`); skip notify when the task was stopped by TaskStop (status Stopped → still notify, with status line; simpler and honest). Wire `a.tasks.SetNotify` in `agent.New` next to the shells wiring (agent.go:606) → `QueueNotificationFor(session, "background-task-completed", text)`.
- [ ] **Step 4: Run** `go test ./internal/agent/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(agent): background task completion notification and blocking TaskOutput`

### Task 4: Cron entries capture their session

**Files:**
- Modify: `internal/agent/cron.go` (cronEntry, Add, fireDue, notificationSink), `internal/agent/agent.go:497` (sink wiring), `internal/agent/agent.go addCron`
- Test: `internal/agent/cron_test.go` (extend)

**Interfaces:**
- Produces: `type notificationSink func(session, tag, text string)`; `func (s *cronScheduler) Add(session, name, prompt, spec string) (string, error)`; `func (a *Agent) addCron(ctx context.Context, name, prompt, spec string) (string, error)`.

- [ ] **Step 1: Write failing test:** scheduler with sink capturing (session, tag, text); `Add("s1", …)` then `fireDue(now)` → sink receives `("s1", "scheduled-task", …)`.
- [ ] **Step 2: Run** `go test ./internal/agent/ -run Cron -v` → FAIL.
- [ ] **Step 3: Implement:** `cronEntry.Session`; `fireDue` passes `e.Session`; scheduler built with `a.QueueNotificationFor` as the sink (matches the 3-arg signature); `addCron` takes ctx and captures `sessionFromCtx(ctx)`; update the ScheduleCron tool call site.
- [ ] **Step 4: Run** `go test ./internal/agent/ -run Cron -v` → PASS.
- [ ] **Step 5: Commit** `feat(agent): cron entries are session-scoped`

### Task 5: Observer seam and coalescing on the Agent

**Files:**
- Modify: `internal/agent/queue.go`, `internal/agent/agent.go`
- Test: `internal/agent/backgroundnotify_test.go` (extend)

**Interfaces:**
- Produces:
  - `func (a *Agent) SetNotificationObserver(fn func(QueuedInput))` (nil = unset; called synchronously from producer goroutines)
  - `func CoalesceQueuedInputs(items []QueuedInput) messages.Message` — ONE Human message; each item's `<tag>\ntext\n</tag>` block joined by a blank line
- Covers spec: run-start delivery for queued-before-run notifications without tool calls.

- [ ] **Step 1: Write failing tests:** (a) queue a notification while no run is in flight; observer received the exact `QueuedInput`; a subsequent `Invoke` with a FakeChatModel that answers in plain text (no tool calls) still shows the tagged Human message in the output history (run-start prepend path, not toolsNode). (b) `CoalesceQueuedInputs` of two items yields one message containing both tags.
- [ ] **Step 2: Run** `go test ./internal/agent/ -run 'Notification|Coalesce' -v` → FAIL.
- [ ] **Step 3: Implement:** observer stored under `a.mu` (or atomic.Value); called at the end of `QueueNotificationFor` after the item is added. `CoalesceQueuedInputs` is a pure function in queue.go.
- [ ] **Step 4: Run** `go test ./internal/agent/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(agent): notification observer seam and queued-input coalescing`

### Task 6: Fire SubagentStart / SubagentStop / Notification hooks

**Files:**
- Modify: `internal/hooks/hooks.go` (Config field, Event field, Runner methods), `internal/agent/task.go` (fire sites), `internal/agent/agent.go` (askPermission), `internal/agentbuild/layers.go` (merge list), `internal/console/console.go` hooks listing, `README.md` (hooks table)
- Test: `internal/hooks/hooks_test.go`, `internal/agent/hooksgap_test.go` (extend)

**Interfaces:**
- Produces:
  - `Config.SubagentStart []Entry json:"SubagentStart"`
  - `Event.AgentName string json:"agent_name,omitempty"`
  - `func (r *Runner) SubagentStart(ctx context.Context, name string) (string, error)`
  - `func (r *Runner) SubagentStop(ctx context.Context, name string) (string, bool, error)` (signature gains name)
  - Agent wrappers `runSubagentStart`/`runSubagentStop` mirroring existing hook wrappers.

- [ ] **Step 1: Write failing tests:** hooks Runner with a command-type SubagentStart/SubagentStop entry sees `agent_name` in the JSON stdin. Agent-level: a foreground Task fires both (order: start before, stop after); a SubagentStop hook exiting 2 with stdout appends `<subagent-stop-feedback>` to the returned report. `askPermission` path: a Notification hook entry observes message `permission required: <tool>` (test via existing permission-interrupt fixtures).
- [ ] **Step 2: Run** `go test ./internal/hooks/ ./internal/agent/ -run 'Subagent|Notification' -v` → FAIL.
- [ ] **Step 3: Implement:** fire `SubagentStart` at the top of `runTask` (name = `subagentName(def, args)`); `SubagentStop` after foreground report and after background `setFinalIfRunning` (feedback append as above; blocked flag has no re-run loop — documented). `Notification` in `askPermission` immediately before `lggraph.Interrupt` (`_, _ = h.Notification(ctx, "permission required: "+call.Name)`). Add the event to `loadHooksConfig`'s merge list and `/hooks` output; README hooks section + tools table get one line each.
- [ ] **Step 4: Run** `go test ./internal/hooks/ ./internal/agent/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(hooks): fire SubagentStart/SubagentStop/Notification events`

### Task 7: Settings keys `auto-wake` and `auto-wake-limit`

**Files:**
- Modify: `internal/settings/keys.go`, `internal/settings/settings.go`
- Test: settings package tests (mirror existing key tests)

**Interfaces:**
- Produces: `KeyAutoWake = "auto-wake"`, `KeyAutoWakeLimit = "auto-wake-limit"`; `Settings.AutoWake bool json:"auto_wake"` (default TRUE via `Default()`, pattern `LadyMSystem2`), `Settings.AutoWakeLimit int json:"auto_wake_limit"` (default `DefaultAutoWakeLimit = 20`; 0 in Load → default; negative stored value → treat as 0=unlimited), both registered in every explicit key list (`KeyNames`, parse switch, apply switch, format, get) exactly like `KeyLangfuseEnabled`/`KeyLadyMTopK`.

- [ ] **Step 1: Write failing tests:** round-trip set/get via the store for both keys; `Default()` has AutoWake true and limit 20; KeyNames contains both.
- [ ] **Step 2: Run** `go test ./internal/settings/ -run 'AutoWake|Keys' -v` → FAIL.
- [ ] **Step 3: Implement** following the `LadyMSystem2` (default-true bool) and `LadyMTopK` (int with default) patterns.
- [ ] **Step 4: Run** `go test ./internal/settings/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(settings): auto-wake and auto-wake-limit keys`

### Task 8: Server wake driver

**Files:**
- Create: `internal/server/wake.go`
- Modify: `internal/server/admission.go` (`busy`), `internal/server/runs.go` (`startRunWith`, run_started data, onFinish hook, `handleCreateRun` resets chain), `internal/server/server.go` (`New` installs observer)
- Test: `internal/server/wake_test.go` (new; harness = `newTestServer`/`newTestAgent` pattern from server_test.go)

**Interfaces:**
- Consumes: Tasks 1–5, 7 (`QueueNotificationFor`, `DrainQueuedInputsFor`, `QueuedInputCountFor`, `CoalesceQueuedInputs`, `SetNotificationObserver`, settings).
- Produces:
  - `func (a *admission) busy(sessionID string) bool` (running or queued waiters)
  - `func (s *Server) startRunWith(ctx, sessionID string, human messages.Message, workspace string, atts []runAttachment, opts runOptions) (*Run, string, int, error)`; `type runOptions struct{ auto bool; trigger string }`; existing `startRun` = zero-opts wrapper
  - `EventWakeCapped = "wake_capped"` in `internal/server/ssevents.go`, published on `s.hub` (same Publish path as `sessionStateEvent`)
  - `run_started` event data gains `"auto": true, "trigger": "<tag>"` only for wake runs.

- [ ] **Step 1: Write failing integration tests** (FakeChatModel agent: response 1 calls Bash `run_in_background` with `sleep 0.2 && echo bg-done`, response 2 plain text):
  1. POST `/api/runs` (session s1) → completes; shell finishes while idle → poll `GET /api/sessions/s1` store (or `/api/runs` list) until a second run exists; assert its SSE history contains `run_started` with `auto:true` and the final persisted history contains a Human message with `<background-task-completed>` and `bg-done`.
  2. Two shells completing concurrently → exactly one wake run, one coalesced Human message.
  3. `auto-wake=off` in settings → no second run; `QueuedInputCountFor("s1")` stays 1.
  4. Chain: model re-arms background shells each wake; set `auto-wake-limit=2` → at most 2 wake runs, `wake_capped` event observed via `/api/events` (or hub spy), items remain queued; a user POST resets and delivers them.
  5. Session affinity: session s2's queued notification never appears in s1's runs.
- [ ] **Step 2: Run** `go test ./internal/server/ -run Wake -v` → FAIL.
- [ ] **Step 3: Implement** `internal/server/wake.go`: `onQueuedNotification(q)` → if `q.Session == ""` or `admission.busy(q.Session)` return; else `maybeWake(q.Session)`. `maybeWake(sid)` under `s.wakeMu`: re-check busy; read policy (settings nil → default on/20); chain counter `map[string]int` incremented per wake, reset in `handleCreateRun`; over limit → log + hub `wake_capped` + return. Drain `DrainQueuedInputsFor(sid)`, empty → return; `startRunWith(ctx.Background(), sid, CoalesceQueuedInputs(items), "", nil, runOptions{auto: true, trigger: firstTag})` (resolve workspace like `handleCreateRun` does via `resolveRunWorkspace`). Every run's `onFinish` (runs.go, next to `clearSessionState`) calls `s.maybeWake(sid)` — guard: only when the finishing run succeeded (skip on cancelled/error). `New` installs the observer.
- [ ] **Step 4: Run** `go test ./internal/server/ -v` → PASS (full package, race on this package: `go test -race ./internal/server/`).
- [ ] **Step 5: Commit** `feat(server): idle wake driver for queued notifications`

### Task 9: TUI wake driver

**Files:**
- Create: `internal/tui/wake.go`
- Modify: `internal/tui/tui.go` (`New`, `Init`, `Update` runDone branch)
- Test: `internal/tui/wake_test.go` (new; Model built like `command_test.go:47` `New(a, nil, cfg)`)

**Interfaces:**
- Produces: `type notificationQueued struct{ item agent.QueuedInput }`; Model gains `notifyCh chan agent.QueuedInput` (buffered 64, created in `New`); `func (m *Model) armNotifications() tea.Cmd`; `func (m *Model) maybeWake() tea.Cmd`.

- [ ] **Step 1: Write failing test:** agent with FakeChatModel (plain text answers), `New(a, nil, cfg)`; `a.QueueNotification("background-task-completed", "shell 1 done")` → observer pushes to `notifyCh`; feed `notificationQueued{}` through `m.Update` → `m.running == true` and `m.history` last Human contains the tag; feed `runDone{result: …}` → `m.running == false`, then next `notificationQueued` while running is a no-op.
- [ ] **Step 2: Run** `go test ./internal/tui/ -run Wake -v` → FAIL.
- [ ] **Step 3: Implement:** `New` installs `a.SetNotificationObserver` (non-blocking send, `select default` — the agent queue is the source of truth); `Init` returns `tea.Batch(m.spinner.Tick, m.armNotifications())`; `Update` handles `notificationQueued` → `tea.Batch(m.maybeWake(), m.armNotifications())`; `maybeWake`: `m.running` → nil; drain `DrainQueuedInputsFor("")`, empty → nil; print a dim `⏵ background notification` line; append coalesced message to `m.history`; `m.running = true`; return `m.runWithHistory(history)`. `runDone` branch appends `m.maybeWake()` to its returned cmd (after the existing interrupt/steer handling).
- [ ] **Step 4: Run** `go test ./internal/tui/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(tui): wake driver for idle notifications`

### Task 10: Line-REPL continuation

**Files:**
- Modify: `cmd/woca/main.go` (`replLoop` struct + `newREPLLoop` + `submit`)
- Test: `cmd/woca/runflow_test.go` (extend, `fakeOpenAI` harness)

**Interfaces:**
- Consumes: `agent.CoalesceQueuedInputs`, `QueuedInputCountFor("")`, `DrainQueuedInputsFor("")`, settings `AutoWake`/`AutoWakeLimit` (thread `wakeEnabled bool, wakeLimit int` into `newREPLLoop` from the App's settings).

- [ ] **Step 1: Write failing test:** REPL flow where the model's first response contains a background-shell start… simpler deterministic path: after the first prompt's run completes, inject `a.QueueNotification("background-task-completed", "shell done-marker")` via the test hook (expose the agent or use the fake model that calls Bash run_in_background; prefer the real path: model response = Bash tool call `run_in_background:true` `echo done-marker`, second response plain text) → combined output contains `done-marker` delivered in a continuation turn without new stdin input.
- [ ] **Step 2: Run** `go test ./cmd/woca/ -run REPL -v` → FAIL.
- [ ] **Step 3: Implement:** in `submit`, after a successful `r.r.Run`, loop up to `wakeLimit` times while `r.agent.QueuedInputCountFor("") > 0`: print a dim `⏵ background notification` line, append `agent.CoalesceQueuedInputs(DrainQueuedInputsFor(""))` to history, run again. Gate on `wakeEnabled`. Do NOT route through `submitHooks`.
- [ ] **Step 4: Run** `go test ./cmd/woca/ -v` → PASS.
- [ ] **Step 5: Commit** `feat(repl): continuation turn for idle notifications`

### Task 11: Web transcript — tagged notifications as system notices

**Files:**
- Modify: `web/src/types.ts` (ChatItem union: `{ kind: 'notice'; id: string; content: string }`), `web/src/lib/transcript.ts` (`messagesToItems`), `web/src/components/ChatItemView.vue` (render), locale files (`en.ts`, `zh-CN.ts` — zh stays Chinese)
- Test: `web/src/lib/transcript.test.ts` (extend)

**Interfaces:**
- Produces: `kind: 'notice'` ChatItem rendered dim/monospace (match the goal_note visual family), never a user bubble; live SSE path uses the same `messagesToItems` filter so both reload and stream agree.

- [ ] **Step 1: Write failing vitest:** a Human message `<background-task-completed>\n…\n</background-task-completed>` maps to one `notice` item, not `user`; same for `<scheduled-task>` and `<queued-user-input>`; plain user text still maps to `user`.
- [ ] **Step 2: Run** `cd web && npx vitest run src/lib/transcript.test.ts` → FAIL.
- [ ] **Step 3: Implement** the filter branch before the `kind: 'user'` push; add the notice branch in `ChatItemView.vue`; label via i18n (`transcript.notice`).
- [ ] **Step 4: Run** `cd web && npm run build && npx vitest run` → PASS; then `go build ./...` (embed rule).
- [ ] **Step 5: Commit** `feat(web): render harness notifications as system notices`

### Task 12: Full gates and docs

**Files:**
- Modify: `README.md` (tools table: BashInput, wait_ms; background/cron paragraphs: idle wake + `auto-wake` settings; hooks table: SubagentStart)

- [ ] **Step 1: Run** `make test` → green; `make race` → green; `make lint` → clean; `cd web && npm run build` then `go build ./...`.
- [ ] **Step 2: English-only sweep** of all touched files (comments, strings; zh-CN locale excepted).
- [ ] **Step 3: Commit** `docs: background loop closure notes`

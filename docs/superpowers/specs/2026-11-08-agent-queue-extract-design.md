# Queue extraction from the agent package — design

Date: 2026-11-08
Status: approved (user directed: merge arch-cleanup, then start the named next task — the queue extraction)
Follows: `2026-11-08-arch-cleanup-design.md` F1 (activity extraction) — same recipe, second step of un-budding `internal/agent`. The arch review named the queue as the next candidate: its `TaggedCount`/`DropScope` exports are already the seam shape.

## Decision

Extract `internal/agent/queue.go` (~384 lines: the mid-turn notification/input buffer) into package `internal/agent/queue`:

- **Mechanics move** (subpackage, no `internal/agent` import — import guard test like activity's): `QueuedInput{Text,Tag,Session,Quiet}`, the thread-safe buffer (`Add`, `AddTagged` with a reject-predicate for released sessions, session-scoped `Drain`/`Count`/`WakeableCount`/`TaggedCount`, `DropScope`), `Coalesce`, `Format` (the `<tag>` wrapper), `SessionFrom(ctx)` (reads `tracing.RunFrom`).
- **Policy stays in agent**: the observer (`SetNotificationObserver`), the Quiet decision (`queueBackgroundCompletion` keyed on `StoppedBy`), the released predicate (delegating to the activity seam), `prependQueued` composition at run start, and the `toolsNode` drain call sites. The Agent keeps EVERY public signature unchanged (`QueueInput`, `QueueInputFor`, `QueueNotification(For)`, `QueueQuietNotificationFor`, `DrainQueuedInputs(For)`, `QueuedInputCount(For)`, `WakeableCountFor`, `QueuedNotificationCountFor`, `CoalesceQueuedInputs`, `SetNotificationObserver`, `DropScopedInputsFor`) via thin delegates + type aliases (`type QueuedInput = queue.QueuedInput` etc.) — server, TUI, REPL, sdk, and the activity seam call sites untouched.
- Tests: mechanics tests move to the subpackage; agent wiring tests (observer firing, released-silence interplay, run-start prepend, toolsNode drain) stay and must pass unchanged.
- Also rides along (same god-package diet, trivial): nothing else — goal/task splits stay out (architect: goal next quarter).

## Out of scope

Any behavior change to queue semantics (drain visibility, Quiet, released rejection, coalescing); the goal/task/backgroundTask subsystems; renaming public API.

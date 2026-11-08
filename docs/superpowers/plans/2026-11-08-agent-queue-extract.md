# Queue extraction — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the mid-turn queue mechanics out of `internal/agent` into `internal/agent/queue`, public API byte-identical.

**Architecture:** Same dependency-inversion recipe as the activity extraction: mechanics in the subpackage (no agent import, guard test), policy wiring stays on Agent, delegates + aliases keep every signature.

**Tech Stack:** Go 1.26.

**Spec:** `docs/superpowers/specs/2026-11-08-agent-queue-extract-design.md` (binding).

## Global Constraints

- Work in `/Users/yuhaochen/Documents/codebase/projanvil/woca/.worktrees/agent-queue` (branch feature/agent-queue-extract from main 6933eb1e); English code/comments/commits.
- No behavior change to queue semantics (drain visibility `""`-unscoped rule, Quiet flag, released rejection atomicity, coalescing format).
- Public `Agent` signatures unchanged; call sites in server/TUI/REPL/sdk/activity untouched.
- Gates: `go test ./...`, `-race` on internal/agent/..., `make lint` (only the 2 known pre-existing e2e/lab errcheck findings), `go build ./cmd/woca` (no web changes expected).

## Review Focus

1. The released-rejection stays atomic with `DropScope` (the T7-fix invariant: reject predicate evaluated under the buffer lock — preserve exactly).
2. `internal/agent/queue` never imports `internal/agent` (guard test, trimpath-safe helper — reuse the go.mod-walk pattern from the arch batch).
3. Every public signature byte-identical; the sdk alias contract test untouched and green.
4. The observer still fires outside all locks, after the item is queued; Quiet requeue paths unchanged.

---

### Task 1: Extract internal/agent/queue

**Files:** new internal/agent/queue package (from internal/agent/queue.go + mechanics parts of queue_test.go), internal/agent wiring (delegates + aliases; queueNotification/queueBackgroundCompletion/prependQueued/toolsNode stay), import guard test (go.mod-walk fallback pattern).
**Requirements:** Spec decision. Mechanics: QueuedInput/Add/AddTagged(reject)/Drain/Count/WakeableCount/TaggedCount/DropScope/Coalesce/Format/SessionFrom. Policy stays: observer wiring, Quiet decision, released predicate, run-start prepend. All tests green; behavior identical.
**Commit:** `refactor(agent): extract the mid-turn queue into its own package`

### Task 2: Gates

**Requirements:** All Global Constraints gates green; tree clean; ledger updated.
**Commit:** none unless fixes needed.

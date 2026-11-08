# Queue follow-ups — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development.

**Goal:** Land the three follow-ups ledgered by the queue-extraction final review.

**Spec:** none needed (micro-batch; decisions below are the spec).

## Decisions
1. **Single-live-buffer pin** (internal/agent): a test asserting the activity seam's `Queue` dep IS `&a.inputs` (the one buffer the delegates use) — reflect/pointer identity via a wiring test in internal/agent.
2. **Canonical tag-source pointer** (web/src/lib/transcript.ts): one comment line at the notice-tag matching code (`isNoticeContent` / tag prefixes) naming `internal/agent/queue.Format` as the canonical `<tag>` producer, so the coupling is discoverable.
3. **Guard widening**: `internal/agent/queue` import guard fails on ANY `github.com/projanvil/woca/internal/*` import except `internal/tracing` (plus stdlib/langchain allowed). `internal/agent/activity` guard: same rule, whitelist `{internal/tracing, internal/tools}` (it legitimately uses tools.StopOrigin) — enumerate its current internal imports first and whitelist exactly those.

## Constraints
Worktree /Users/yuhaochen/Documents/codebase/projanvil/woca/.worktrees/queue-followups (branch feature/queue-followups from main e7ad0ba9). English. No behavior changes — tests/comments only except the guard-rule change (still test-only). Gates: `go test ./internal/agent/... ./sdk/`, web comment needs no build (comment-only TS line — but run `cd web && bun run test` once to be safe), `go build ./...`. One commit: `test: pin the seam buffer and widen extraction import guards` (+ web comment rides along).

## Review Focus
1. The pin test actually catches a second-buffer wiring (fails if `New` hands the seam a different Buffer).
2. Guard whitelists match reality (queue: tracing only; activity: exactly its current internal imports — no over-whitelisting).
3. Comment-only web change (no logic), naming the exact Go symbol path.

---

### Task 1: all three items
**Files:** internal/agent (new pin test), internal/agent/queue/import_test.go + internal/agent/activity/import_test.go (guard rules), web/src/lib/transcript.ts (comment).
**Commit:** `test: pin the seam buffer and widen extraction import guards`

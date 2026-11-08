# Architecture cleanup batch — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix the server.Config drift bug (TUI `/web` panel 501) and land the approved architecture cleanups (activity subpackage, ops-console dedup, sdk guard, reverse-dep removal, probe dedup, same-origin doc).

**Architecture:** One shared base-server-config assembly; dependency-inverted `agent/activity` subpackage; single ops-console assembly; contract-pinned sdk aliases.

**Tech Stack:** Go 1.26; vitest not needed (no web logic changes beyond config wiring).

**Spec:** `docs/superpowers/specs/2026-11-08-arch-cleanup-design.md` (F2/F1/F4/F5/F6/F7/F10 decisions binding).
**Audit evidence:** `/Users/yuhaochen/Documents/codebase/projanvil/woca-internal-docs/arch-review-2026-11-08.md` (read the cited file:line anchors per task).

## Global Constraints

- Work in `/Users/yuhaochen/Documents/codebase/projanvil/woca/.worktrees/arch-cleanup` (branch feature/arch-cleanup from main 98b742e5); one commit per task; English code/comments/commits.
- No external API shape changes: sdk aliases and `Agent`'s activity methods keep signatures.
- No new visual system; no fallback designs; seams not sniffing.
- Gates per task: `go build ./...`, focused package tests; final task runs `make test`, `-race` on changed packages, `make lint` (only the 2 known pre-existing e2e/lab errcheck findings allowed), `cd web && bun run build` only if web files changed (then first), `go build ./cmd/woca`.

## Review Focus

1. TUI `/web` panel works: activity endpoints return 200 (not 501) in the embedded Web UI, and the consistency test fails if either constructor drifts again.
2. `agent/activity` subpackage does not import `internal/agent` (no cycle); all existing tests pass unchanged; public `Agent` API unchanged.
3. Both ops-console entry points behave identically after dedup (mount paths, auth, static).
4. `grep -r 'internal/websearch' internal/settings` is empty after F6 and nothing else grew a reverse dep.
5. F10 recorded where a reader looks (webfx doc + README), stating same-origin as deliberate.

---

### Task 1: F2 — BaseServerConfig + fix TUI /web 501

**Files:** internal/agentbuild/app.go (new `(*App) BaseServerConfig() server.Config`), cmd/woca/main.go (~727 refactor onto it), internal/servefront/serve.go (refactor its server.Config literal onto it), test in internal/agentbuild or cmd/woca.
**Requirements:** Spec F2. Shared base sets the ~16 identical fields INCLUDING `ActivityAPI: true` for non-dispatch forms; servefront overrides `ActivityAPI: !dispatchQueue` (and its form-specific fields); TUI path overrides only its specifics. Consistency test: the TUI-built config equals servefront's (non-dispatch mode) on the shared field set. Add a regression test proving the TUI server answers activity endpoints 200 (drive webgui manager like existing web tests, or at minimum assert the config field).
**Commit:** `fix(server): single base server config; TUI web gets the activity API`

### Task 2: F5 — sdk alias contract guard

**Files:** sdk (find the 19 `type X = agent.X` aliases), new sdk contract test.
**Requirements:** Spec F5. Test pins the alias list explicitly (names + their agent-package targets); adding/removing/repointing an alias without updating the list fails with a clear message.
**Commit:** `test(sdk): pin the agent type alias contract`

### Task 3: F4 — ops-console assembly dedup

**Files:** find the two verbatim assembly sites (audit F4 anchors; likely cmd/woca-ops or opsconsole wiring vs servefront), extract one shared assembly.
**Requirements:** Spec F4. Both entry points mount identical routes/static/auth through the shared function; behavior-identical (test: both respond on the ops paths).
**Commit:** `refactor(ops): single ops-console assembly`

### Task 4: F6 + F7 + F10 — reverse dep, probe dedup, same-origin doc

**Files:** internal/settings (remove the websearch import — audit F6 anchor), the three `defaultObsAddr` sites (audit F7), internal/platform/httpserver or webfx package doc + README (F10).
**Requirements:** Spec F6/F7/F10. Invert the dependency at the right owner (move the logic or inject a callback — judge from what settings actually uses it for); one shared probe helper; two-paragraph same-origin note (English README + zh mirror stays Chinese).
**Commit:** `refactor: drop settings→websearch reverse dep; shared obs probe; same-origin note` (split if cleaner)

### Task 5: F1 — extract agent/activity subpackage

**Files:** new internal/agent/activity package (from internal/agent/activity.go + activity_test.go), internal/agent wiring (Agent keeps thin delegating methods), no changes in internal/server/sdk call sites.
**Requirements:** Spec F1. Subpackage defines narrow exported interfaces (snapshot/action/observer surfaces it needs) implemented by the agent package's managers — dependency inversion; subpackage must NOT import internal/agent (enforce with a test or rely on the compiler). All existing tests pass unchanged. Public `Agent` method signatures unchanged.
**Commit:** `refactor(agent): extract the activity surface into its own package`

### Task 6: gates

**Requirements:** `make test`, `-race` on changed packages, `make lint`, `go build ./cmd/woca`; tree clean; README/zh touched only by F10.
**Commit:** none (verification only) unless fixes needed.

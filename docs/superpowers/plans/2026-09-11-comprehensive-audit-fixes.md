# Comprehensive Audit Fixes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix all actionable findings from the 2026-09-11 six-axis audit of the woca harness (product surface, code conventions, module layout, tests, frontend, docs/build).

**Architecture:** Small, independently committable fixes grouped by subsystem. No behavior changes to the personal-edition agent loop; enterprise roles get only additive mounts. All fixes land on branch `fix/comprehensive-audit-2026-09-11` in worktree `.worktrees/comprehensive-fixes`.

**Tech Stack:** Go 1.26 (single module `github.com/projanvil/woca`), golangci-lint v2, GitHub Actions, Vue3+Vite+bun frontends, Wails v2 native client.

**Spec:** The audit findings are summarized in the Appendix of this file (they were produced by six read-only exploration agents against commit `5da2754`).

## Global Constraints

- Module path stays lowercase `github.com/projanvil/woca` everywhere (go.mod:1). Only docs referencing the wrong case may change.
- Personal binary must never import `internal/ee`; `make check-editions` and `make check-deps` (ci/deps-personal.txt baseline) must stay green. Regenerate the baseline only via `make deps-personal` when a personal-edition dependency set legitimately changes, and call it out in the commit message.
- `gofmt -l .` must stay empty; `go vet ./...` must pass; `go test ./...` must pass.
- No new third-party dependencies without explicit justification in the commit message.
- Doc fixes must match code, not aspirations: every README claim touched must be verified against the referenced file.
- Commits: conventional-commit style already used by the repo (`feat(scope): …`, `fix(scope): …`, `docs: …`, `chore: …`, `ci: …`).
- Do NOT read the contents of `deploy/selfhosted/.env`; only untrack it.

---

### Task 1: Rename `internal/context` directory to match package `contextx`

**Files:**
- Rename: `internal/context/` → `internal/contextx/`
- Modify: every file importing `github.com/projanvil/woca/internal/context`

**Interfaces:**
- Consumes: nothing
- Produces: import path `github.com/projanvil/woca/internal/contextx`, package name unchanged (`contextx`)

- [ ] **Step 1: Find importers**

Run: `grep -rl 'woca/internal/context"' --include='*.go' cmd internal native-client`
Expected: a short list (agent/runner/tui area). Record it.

- [ ] **Step 2: Move the directory and rewrite imports**

```bash
git mv internal/context internal/contextx
grep -rl 'woca/internal/context"' --include='*.go' cmd internal native-client \
  | xargs sed -i '' 's|woca/internal/context"|woca/internal/contextx"|g'
```

- [ ] **Step 3: Verify**

Run: `go build ./... && go vet ./... && gofmt -l .`
Expected: all clean. (Note: `go build ./...` does not cover `native-client` — run `cd native-client && go build ./...` only if it appeared in the importer list.)

- [ ] **Step 4: Commit**

```bash
git add -A && git commit -m "refactor(contextx): rename internal/context dir to match package name"
```

---

### Task 2: Repository hygiene — gitignore, untrack artifacts, remove stray lockfile

**Files:**
- Modify: `.gitignore`
- Untrack (git rm --cached): `native-client/native-client`, `ee/configsvc-web/dist/`, `ee/opsconsole-web/dist/`, `ee/skillsvc-web/dist/`, `internal/ee/configsvc/spa/`, `internal/ee/opsui/spa/`, `internal/ee/skillsvc/spa/`, `web/dist/index.html`, `deploy/selfhosted/.env`
- Delete: `web/package-lock.json` (bun is the toolchain; Makefile:8)

**Interfaces:**
- Consumes: nothing
- Produces: Tasks 3 and 4 depend on the untracked `web/dist` and `internal/ee/*/spa` states (embed stubs must exist before untracked dirs compile on a fresh clone).

- [ ] **Step 1: Update .gitignore**

Append to `.gitignore`:

```gitignore
# macOS
.DS_Store

# Build artifacts that must never be committed
web/dist/
ee/configsvc-web/dist/
ee/opsconsole-web/dist/
ee/skillsvc-web/dist/
internal/ee/configsvc/spa/
internal/ee/opsui/spa/
internal/ee/skillsvc/spa/
native-client/native-client

# Local deploy environment (use .env.example)
deploy/selfhosted/.env
```

(If `web/dist` or other entries already exist in `.gitignore`, reconcile instead of duplicating.)

- [ ] **Step 2: Untrack artifacts (do not delete local files)**

```bash
git rm -r --cached ee/configsvc-web/dist ee/opsconsole-web/dist ee/skillsvc-web/dist \
  internal/ee/configsvc/spa internal/ee/opsui/spa internal/ee/skillsvc/spa \
  web/dist deploy/selfhosted/.env 2>&1
git rm --cached native-client/native-client web/package-lock.json
```

NOTE: `git rm --cached web/dist` removes the tracked `web/dist/index.html`; Tasks 3/4 add the stub mechanism that keeps `go build` working afterwards. Do not commit Task 2 without Tasks 3+4 in the same branch state — either land them together or verify `go build ./...` still passes before committing.

- [ ] **Step 3: Verify**

Run: `git status --short | head -30` and `git ls-files | grep -E 'dist/|spa/|\.env$|native-client/native-client$' || echo CLEAN`
Expected: only intended deletions staged; grep prints CLEAN.

- [ ] **Step 4: Commit (jointly with Tasks 3 and 4)**

```bash
git commit -m "chore(repo): untrack build artifacts, ignore .DS_Store and deploy .env, drop npm lockfile"
```

---

### Task 3: Web embed robustness — stub fallback + freshness test

**Files:**
- Create: `web/stub/index.html` (hand-written static page)
- Create: `web/dist/.gitkeep` (tracked, keeps `//go:embed all:dist` compilable on fresh clone)
- Modify: `web/embed.go`
- Modify: `web/embed_test.go`

**Interfaces:**
- Produces: unchanged public API `webdist.DistFS()` (callers in `cmd/woca`, `cmd/woca-bridge` untouched). Behavior change: when `web/dist` contains no real build, DistFS serves the stub page instead of a broken index referencing missing hashed assets.

- [ ] **Step 1: Write the failing test**

Append to `web/embed_test.go`:

```go
// TestDistFSRealBuildOrStub asserts the embedded FS is internally
// consistent: either it is the stub (no assets referenced), or every
// asset referenced by index.html exists in the FS.
func TestDistFSRealBuildOrStub(t *testing.T) {
	fsys := DistFS()
	index, err := fs.ReadFile(fsys, "index.html")
	if err != nil {
		t.Fatalf("index.html missing: %v", err)
	}
	if bytes.Contains(index, []byte("woca web UI not built")) {
		return // stub page, nothing referenced
	}
	for _, m := range regexp.MustCompile(`(?:src|href)="(/assets/[^"]+)"`).FindAllSubmatch(index, -1) {
		if _, err := fs.Stat(fsys, strings.TrimPrefix(string(m[1]), "/")); err != nil {
			t.Errorf("index.html references missing asset %s", m[1])
		}
	}
}
```

Run: `go test ./web/ -run TestDistFSRealBuildOrStub -v`
Expected: FAIL on a fresh checkout state (current index.html references `index-*.js` that is not embedded) — confirms the bug.

- [ ] **Step 2: Create stub and gitkeep**

`web/stub/index.html`: a minimal static page (inline CSS, no external refs) whose body text contains exactly `woca web UI not built` plus "run `make web`" instructions. Create empty `web/dist/.gitkeep`.

- [ ] **Step 3: Implement fallback in `web/embed.go`**

Keep the existing `//go:embed all:dist`; add a second embed `//go:embed all:stub` into `stubFS`. In `DistFS()`: `fs.Stat(dist, "dist/index.html")`; if it exists AND `fs.Glob(dist, "dist/assets/*")` is non-empty, sub-FS into `dist/`; otherwise sub-FS into `stub/`. Preserve the existing exported signature.

- [ ] **Step 4: Run tests**

Run: `go test ./web/ -v && go build ./cmd/woca`
Expected: PASS; binary builds without `make web` (serves stub) and with `make web` (serves real UI). Verify the real path too: `make web && go test ./web/ -v` still PASS (now exercising the assets-exist branch).

- [ ] **Step 5: Commit (with Task 2)**

```bash
git add web/ && git commit -m "fix(web): serve stub page when dist is not built; assert asset freshness in test"
```

---

### Task 4: Enterprise SPA embed stubs (configsvc / opsui / skillsvc)

**Files:**
- Create: `internal/ee/configsvc/spa/.gitkeep`, `internal/ee/opsui/spa/.gitkeep`, `internal/ee/skillsvc/spa/.gitkeep`
- Modify: `internal/ee/configsvc/embed.go`, `internal/ee/opsui/embed.go`, `internal/ee/skillsvc/embed.go` (whichever embed directives exist)
- Modify: `Makefile` (`ee-web` target comment), `.github/workflows/ci.yml` (build step runs `make ee-web` first — see Task 6 interface)

**Interfaces:**
- Consumes: Task 2's untracking of the spa dirs.
- Produces: fresh-clone `go build ./cmd/woca-config ./cmd/woca-ops` succeeds and serves a "console not built" placeholder; CI builds real consoles via `make ee-web` before `go build`.

- [ ] **Step 1: Inspect current embed directives**

Run: `grep -n 'go:embed' internal/ee/configsvc/embed.go internal/ee/opsui/embed.go internal/ee/skillsvc/embed.go`

- [ ] **Step 2: Add .gitkeep files + empty-dir fallback**

For each of the three packages: keep `//go:embed all:spa`; where the handler currently serves the SPA FS, check whether `spa/index.html` exists; if not, serve HTTP 503 with body `<role> console not built; run make ee-web`. Match the existing handler style in each file.

- [ ] **Step 3: Verify both paths**

Run: `go build ./cmd/woca-config ./cmd/woca-ops && make ee-web && go build ./cmd/woca-config ./cmd/woca-ops`
Expected: both builds succeed. (Do not commit the regenerated `spa/` output — Task 2's gitignore excludes it; confirm with `git status --short internal/ee/`.)

- [ ] **Step 4: Commit (with Tasks 2 and 3)**

```bash
git commit -m "fix(ee): graceful placeholder when console SPAs are not built"
```

---

### Task 5: golangci-lint v2 migration + CI lint gate

**Files:**
- Rewrite: `.golangci.yml` (v2 schema)
- Modify: `.github/workflows/ci.yml` — add lint step
- Fix: whatever the migrated config reports (expected: errcheck/staticcheck hits)

**Interfaces:**
- Produces: `golangci-lint run` green locally and in CI. Later tasks must keep it green.

- [ ] **Step 1: Migrate config to v2**

Rewrite `.golangci.yml` in v2 schema preserving the current linter set (errcheck, govet, ineffassign, staticcheck — which in v2 subsumes gosimple — unused, plus gofmt/goimports as formatters):

```yaml
version: "2"
linters:
  default: none
  enable:
    - errcheck
    - govet
    - ineffassign
    - staticcheck
    - unused
  exclusions:
    paths:
      - web/wailsjs
      - node_modules
formatters:
  enable:
    - gofmt
    - goimports
run:
  timeout: 5m
```

Run: `golangci-lint config verify && golangci-lint run ./... 2>&1 | head -60`

- [ ] **Step 2: Fix or deliberately exclude findings**

Fix all findings that are real (expected clusters: unchecked errors, printf issues). For intentional discards (`_ =` best-effort publishes), prefer adding a brief comment over excluding. If a finding class is genuinely noisy, add a targeted `exclusions.rules` entry with a comment justifying it — never blanket-disable a linter.

- [ ] **Step 3: Add CI lint step**

In `.github/workflows/ci.yml`, after the `go vet` step:

```yaml
      - name: golangci-lint
        uses: golangci/golangci-lint-action@v8
        with:
          version: v2.5.0
```

(Verify the action's current major version tag when editing; pin a specific v2.x.)

- [ ] **Step 4: Verify**

Run: `golangci-lint run ./...` — zero findings. `go test ./...` still green.

- [ ] **Step 5: Commit**

```bash
git commit -m "ci(lint): migrate .golangci.yml to v2 and gate on it in CI"
```

---

### Task 6: CI expansion — frontend tests, native-client tests, Postgres integration

**Files:**
- Modify: `.github/workflows/ci.yml`
- Modify: `Makefile` — add `web-test`, `ee-web-test`, align `check`

**Interfaces:**
- Consumes: Task 5 (lint step placement), Tasks 2–4 (artifact state).
- Produces: CI jobs `go` (existing + postgres service + lint), `ui` (bun + vitest for web/ and ee/*-web), `native` (macos, go test in native-client module).

- [ ] **Step 1: Add Postgres service to the Go job**

In ci.yml's Go job add:

```yaml
    services:
      postgres:
        image: postgres:16
        env:
          POSTGRES_PASSWORD: postgres
        ports: ["5432:5432"]
        options: >-
          --health-cmd "pg_isready" --health-interval 5s --health-retries 10
```

Then read `internal/wocadb/rls_pg_test.go:29`, `internal/wocadb/rls_lgcp_test.go:26`, `internal/runstate/pg_live_test.go:24` for the exact env var names and expected DSN shape, and export them in the test step, e.g.:

```yaml
        env:
          WOCA_TEST_PG_DSN: postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
          WOCA_PG_TEST_DSN: postgres://postgres:postgres@localhost:5432/postgres?sslmode=disable
```

If a test creates its own database, follow what the test file actually expects — do not invent a DSN shape. Run locally first if a Docker postgres is available; otherwise validate by pushing and reading the CI log.

- [ ] **Step 2: UI test job**

```yaml
  ui:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: oven-sh/setup-bun@v2
      - name: web tests
        run: cd web && bun install --frozen-lockfile && bun run test
      - name: ee console tests
        run: |
          for d in ee/configsvc-web ee/opsconsole-web ee/skillsvc-web; do
            (cd "$d" && bun install --frozen-lockfile && bun run test) || exit 1
          done
```

Check each console's `package.json` has a `test` script before wiring it; if one lacks it, add `"test": "vitest run"` there.

- [ ] **Step 3: native-client job**

First verify locally: `cd native-client && go test ./...`. If it passes on macOS without extra setup, add:

```yaml
  native:
    runs-on: macos-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26"
      - run: cd native-client && go test ./...
```

If local run reveals missing cgo/webkit deps, restrict to the pure-Go packages (`go test ./internal/... .`) and note the limitation in a comment.

- [ ] **Step 4: Makefile targets**

Add:

```make
web-test:
	cd web && bun run test

ee-web-test:
	cd ee/configsvc-web && bun run test
	cd ee/opsconsole-web && bun run test
	cd ee/skillsvc-web && bun run test

test-ui: web-test ee-web-test
```

Add `web-test ee-web-test` (and `lint`) to the `check` target's dependency line so `make check` ≈ CI. Update the `.PHONY` list accordingly.

- [ ] **Step 5: Verify + commit**

Run: `make check` locally (minus CI-only services). Commit: `ci: run frontend, native-client, and Postgres-backed tests in CI`.

---

### Task 7: Server request-path context propagation + swallowed-error logging

**Files:**
- Modify: `internal/server/session.go` (lines ~90, 156, 183, 280 area)
- Modify: `internal/server/runs.go` (~527, ~549)
- Modify: `internal/server/sessionstate.go` (~180-193)
- Test: extend an existing `internal/server/*_test.go` or add `internal/server/ctx_test.go`

**Interfaces:**
- Produces: blob load/save/delete and hub publish accept the caller's `context.Context`; no `context.Background()` on request-serving paths; publish/delete failures logged via the package's existing logger.

- [ ] **Step 1: Read the call chains**

Read `internal/server/session.go` fully; map which functions are invoked from HTTP handlers (have `r.Context()` available) vs startup reconcile (Background is fine, keep). Only change request-path call sites: thread `ctx` through `sessionStore.load/save/delete` signatures.

- [ ] **Step 2: Write the failing test**

```go
func TestLoadBlobHonorsCallerContext(t *testing.T) {
	// Arrange a sessionStore whose blob backend blocks until ctx is done,
	// then call load with an already-cancelled context and require a
	// context.Canceled error instead of a completed load.
}
```

Implement against the real blob interface used in `session.go` (check `internal/server/session.go` imports for the blobstore abstraction). Run it: expect FAIL (current code ignores caller ctx).

- [ ] **Step 3: Thread ctx through + log swallowed errors**

- `sessionStore.load/save/delete`: take `ctx context.Context` first param; handlers pass `r.Context()`; startup reconcile passes `context.Background()` with a comment.
- `runs.go:527`: give the queued run a cancellable context derived from the server lifecycle context (find the server struct's root ctx; if none exists, add one on server construction and cancel it in shutdown). Do not leave `func(){}` as the cancel.
- `sessionstate.go:180-193`: on `hub.Publish` error, log at debug/warn with the package's logger instead of `_, _ =`.
- `session.go:280-281`: log blob-delete / goal-file-remove failures at debug level.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/server/ -count=1` — all green including the new test.

- [ ] **Step 5: Commit**

```bash
git commit -m "fix(server): propagate request contexts to blob IO and log publish failures"
```

---

### Task 8: Code-quality sweep — panic helper, %w wrapping, duplicated run-start blocks

**Files:**
- Create: `internal/tools/musttool.go` (or the package where `coretools.FromFunc` is imported — check first)
- Modify: `internal/tools/*.go` (~19 panic sites), `internal/agent/task.go` (9 sites)
- Modify: `internal/plugins/plugins.go:145`, `internal/tools/image.go:92`, `internal/pluginrt/browser.go:326`, `internal/pluginrt/tools.go:216,383` (%v → %w)
- Modify: `internal/server/run.go` (dedup `start`/`startGoal` at :88-113), `internal/tui/tui.go` (dedup :592-623)

**Interfaces:**
- Produces: unexported `mustTool(t coretools.Tool, err error) coretools.Tool` (adjust types to the real signature) used by all construction sites.

- [ ] **Step 1: mustTool helper**

Read `internal/tools/file.go:71` for the exact `FromFunc` signature. Create the helper in the same package:

```go
// mustTool panics on tool-construction errors; construction failure is a
// programmer error (static definitions), not runtime input.
func mustTool(t *coretools.Tool, err error) *coretools.Tool {
	if err != nil {
		panic(err)
	}
	return t
}
```

Replace every `x, err := coretools.FromFunc(...); if err != nil { panic(err) }` pair with `x := mustTool(coretools.FromFunc(...))`. `internal/agent/task.go` imports the helper from `internal/tools` only if the helper is exported; otherwise keep a local copy — prefer exporting `MustTool` from tools and reusing it if the import direction already exists (agent already imports tools — verify).

- [ ] **Step 2: %v → %w**

At the five listed sites, change `%v` to `%w` for the error argument (keep other args as-is). Run `go vet ./...` (printf checks %w validity).

- [ ] **Step 3: Dedup run-start blocks**

In `internal/server/run.go`, extract the shared body of `start`/`startGoal` into one helper taking a `runFn` func parameter; both public methods become thin wrappers. Same for the paired goroutine block in `internal/tui/tui.go:592-623` — extract a `startStreamGoroutine(...)` helper. Keep behavior identical: this is a pure refactor, no test changes expected.

- [ ] **Step 4: Verify**

Run: `go build ./... && go test ./internal/tools/ ./internal/agent/ ./internal/server/ ./internal/tui/ -count=1`

- [ ] **Step 5: Commit**

```bash
git commit -m "refactor: mustTool helper for tool construction, %%w error wrapping, dedup run-start blocks"
```

---

### Task 9: SSE event contract — typed Go events + shared TS types + drift check

**Files:**
- Create: `internal/server/ssevents.go` — event-name constants + typed payload structs
- Modify: `internal/server/run.go`, `internal/server/runs.go`, `internal/server/sse.go` (replace string literals)
- Create: `web/src/api/events.ts` — TS union mirroring the Go constants + payload interfaces
- Modify: `web/src/api/streaming.ts:92-148` (use the union, remove `as` guesses where types now exist)
- Create: `ci/check-sse-contract.sh` + CI step

**Interfaces:**
- Produces:
  - Go: `const (EventDelta = "delta"; EventReasoning = "reasoning"; EventGoalNote = "goal_note"; EventToolCall = "tool_call"; EventToolResult = "tool_result"; EventInterrupt = "interrupt"; EventInterruptResolved = "interrupt_resolved"; EventGoal = "goal"; EventRunQueued = "run_queued"; EventQueuePosition = "queue_position"; EventRunStarted = "run_started"; EventResync = "resync"; EventAuthRevoked = "auth_revoked"; EventDone = "done"; EventError = "error")`
  - TS: `export const SSE_EVENT_TYPES = [...] as const; export type SSEEventType = typeof SSE_EVENT_TYPES[number]` plus per-event payload interfaces.

- [ ] **Step 1: Inventory actual events**

Grep the real emitted names first: `grep -rn 'Type: "' internal/server/ && grep -rn 'event: ' internal/server/` — the constant list above is from the audit; trust the grep, adjust the list to reality, and note any frontend-only names in `web/src/api/streaming.ts`.

- [ ] **Step 2: Go constants + payloads**

Create `ssevents.go` with the constants and a struct per payload (start with the three most-structured ones: tool_call, tool_result, interrupt; keep `map[string]any` only where the payload is genuinely dynamic). Replace literals at `run.go:143,153,281`, `runs.go:683,704,715`, `sse.go:75` and any others found in Step 1.

- [ ] **Step 3: TS mirror + drift check**

`web/src/api/events.ts` exports the union and payload interfaces; `streaming.ts` switches on `SSEEventType`. `ci/check-sse-contract.sh`:

```bash
#!/usr/bin/env bash
# Fails when the Go SSE event constants and the TS union drift apart.
set -euo pipefail
go_names=$(grep -oE 'Event[A-Za-z]+ = "[a-z_]+"' internal/server/ssevents.go | grep -oE '"[a-z_]+"' | tr -d '"' | sort)
ts_names=$(grep -oE '"[a-z_]+"' web/src/api/events.ts | tr -d '"' | sort -u)
diff <(echo "$go_names") <(echo "$ts_names")
```

Wire into ci.yml: `- run: ci/check-sse-contract.sh` (chmod +x).

- [ ] **Step 4: Verify**

Run: `go test ./internal/server/ -count=1 && cd web && bun run test && cd .. && ci/check-sse-contract.sh`

- [ ] **Step 5: Commit**

```bash
git commit -m "feat(server,web): typed SSE event contract with CI drift check"
```

---

### Task 10: Test flake fixes — webd dynamic ports, edgegw polling wait

**Files:**
- Modify: `internal/webd/webd_test.go` (ports 3990/3991/3992/3999/3484)
- Modify: `internal/webd/webd.go` only if tests need an injectable addr (prefer not)
- Modify: `internal/ee/edgegw/edgegw_test.go:582,653`

**Interfaces:**
- Produces: webd tests bind `127.0.0.1:0`; edgegw parked-request test polls instead of fixed 700ms sleep.

- [ ] **Step 1: webd ports**

Read `internal/webd/webd_test.go:61-228` and the spawn API. Replace fixed ports with port 0 and read back the actual bound address (if the spawn helper doesn't expose it, derive a free port by opening `net.Listen("tcp", "127.0.0.1:0")`, capturing the port, closing, and passing it in — acceptable in tests). Remove the `t.Skip("cannot bind listener")` only if it becomes unreachable; otherwise keep it as a safety net but it should no longer trigger.

- [ ] **Step 2: edgegw polling**

Replace the `time.Sleep(700ms)` + skip with a `require.Eventually`-style poll (the repo may not vendor testify — check go.mod; if absent, write a 2-second-deadline loop with 20ms interval). On deadline, `t.Fatal` (a real failure), not `t.Skip`.

- [ ] **Step 3: Run with race + repetition**

Run: `go test ./internal/webd/ ./internal/ee/edgegw/ -count=3 -race`

- [ ] **Step 4: Commit**

```bash
git commit -m "test(webd,edgegw): dynamic ports and polling waits to remove flake sources"
```

---

### Task 11: Resolve woca-ops / woca-worker default port collision (9091)

**Files:**
- Modify: `cmd/woca-worker/main.go:219` (`defaultObsAddr`)
- Sweep: `grep -rn 9091 deploy/ cmd/ internal/ docs/ README*.md`

**Interfaces:**
- Produces: worker obs default becomes `:9092`; ops keeps `127.0.0.1:9091`.

- [ ] **Step 1: Change worker default to `:9092`** including its header comment (main.go:7 "default :9091").

- [ ] **Step 2: Sweep and fix every reference** found by the grep (compose files, docs). `deploy/selfhosted/docker-compose.full.yml` worker obs port mappings must match the new container-side default or explicitly set the env var — prefer setting the env explicitly in compose so the default is documented in one place.

- [ ] **Step 3: Verify**

Run: `go build ./cmd/... && grep -rn 9091 cmd/ | grep -v woca-ops || echo "9091 now ops-only"`

- [ ] **Step 4: Commit**

```bash
git commit -m "fix(worker): move default obs port to 9092 to avoid clash with woca-ops"
```

---

### Task 12: Mount skillsvc or formally retire it

**Files:**
- Modify: `cmd/woca-server/main.go` (composition root)
- Modify: `README.md` role list (Task 14 coordinates)

**Decision (auto mode):** mount it. `woca-server` already assembles `opsconsole.New(...)` (main.go:142); add skillsvc alongside under its own route prefix (e.g. `/skillsvc` API + `/skillsvc/` SPA), following the exact mount pattern opsconsole uses. The personal binary is unaffected (skillsvc is under `internal/ee`).

- [ ] **Step 1: Read the seams**

Read `internal/ee/skillsvc/` (constructor + `SpaHandler()` at embed.go:13 + Store dependencies) and how `cmd/woca-server/main.go` mounts `opsconsole`. Identify what Store backing skillsvc needs (wocadb? its own tables?) and whether migrations exist.

- [ ] **Step 2: Mount**

Wire `skillsvc.New(...)` + SPA in woca-server with the same auth posture as the sibling opsconsole mount. If the Store requires schema not covered by existing migrations, STOP, revert, and instead mark skillsvc as "not yet shipped": add a package comment saying so and exclude it from the README — commit that instead and note the blocker in the final report.

- [ ] **Step 3: Test**

Run: `go build ./cmd/woca-server && go test ./internal/ee/skillsvc/ -count=1`. If a smoke test for the mount is feasible cheaply (httptest against the assembled mux), add it.

- [ ] **Step 4: Commit**

```bash
git commit -m "feat(server): mount skillsvc console and API (previously unreachable)"
```

---

### Task 13: deploy/k8s + deploy/vm reality fix (nacos fail-closed)

**Files:**
- Create: `deploy/k8s/00-nacos.yaml` (nacos standalone StatefulSet+Service, dev-grade)
- Modify: `deploy/k8s/20-woca-server.yaml`, `deploy/k8s/21-woca-worker.yaml` (add `WOCA_NACOS_URL` env), add missing roles doc note
- Rewrite: `deploy/k8s/README.md` (remove "Zero registry coupling" claim; document nacos prerequisite and the fail-closed behavior)
- Modify: `deploy/vm/nacos-verify.sh:2-3` comment to describe current registration+heartbeat posture

**Interfaces:**
- Consumes: exact env var names from `cmd/woca-worker/main.go:158-161`, `cmd/woca-server/main.go:77-93`, `internal/ee/nacosrt/`.

- [ ] **Step 1: Read the nacos client** (`internal/ee/nacosrt/pack.go`, server main 77-93) to learn the exact URL format and any namespace/credential env vars.

- [ ] **Step 2: Write 00-nacos.yaml** — single-replica nacos (image `nacos/nacos-server:v2.5.1` or current stable; `MODE=standalone`), ClusterIP service port 8848, plus a comment that production should use an external nacos cluster.

- [ ] **Step 3: Wire env in 20/21**: `WOCA_NACOS_URL: http://nacos:8848` (match the scheme/path the client expects from Step 1).

- [ ] **Step 4: Rewrite deploy/k8s/README.md** — truthful architecture: registry-coupled, fail-closed without nacos; list which roles the manifests cover (server, worker) and which they don't (config/tenant/edge/ops/bridge — point at deploy/selfhosted for the full stack).

- [ ] **Step 5: Fix nacos-verify.sh header comment** to match current behavior (registration + heartbeat + config pull).

- [ ] **Step 6: Commit**

```bash
git commit -m "fix(deploy): add nacos to k8s manifests and correct registry-coupling docs"
```

---

### Task 14: README.md (English) accuracy pass

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Role list** (README.md:320-321): list all 7 enterprise roles (`woca-server`, `woca-worker`, `woca-config`, `woca-tenant`, `woca-bridge`, `woca-edge`, `woca-ops`) with one line each; add `woca-ops` description (tenant ops console — it issues organization credentials per the bridge section, so this also fixes the self-contradiction).

- [ ] **Step 2: Architecture package map** (287-311): either enumerate the real top-level `internal/` packages with one line each (group by area: agent core / surfaces / storage / web plumbing / platform) or replace the flat list with grouped subsections. Must mention: `gateway`, `servefront`, `webd`/`webgui`/`webfx`, `wocadb`, `runstate`/`runws`, `bus`, `platform/`, `slashcmds`, `obslog`, `settingswatch`, `sdkout`, `searchapi`, `keybindings`, `diffutil`, `promptref`. Remove the stale claim about `internal/discovery` if the dir is empty (verify: `ls internal/discovery`; if empty and untracked, delete the dir).

- [ ] **Step 3: Fix contradictions**: 209-212 interrupt claim — TUI supports esc-esc / ctrl+c (internal/tui/tui.go:100); rewrite to say interruption works in TUI and web. Capability plugins (599-607): add `office` (docx/xlsx/pptx) per `internal/pluginrt/tools.go:190` and `woca-plugins/python/office/`; update the architecture line 307 `(browser, pdf)` → `(browser, pdf, office)`.

- [ ] **Step 4: Nacos prerequisite**: in the enterprise section, state plainly that all enterprise roles fail-closed require `WOCA_NACOS_URL` and the minimal deployment includes a nacos registry.

- [ ] **Step 5: Module path casing**: every `go install` / clone command and prose reference uses lowercase `github.com/projanvil/woca`. GitHub badge URLs may keep display case (URLs are case-insensitive) but the `go install` line (151-155) must be lowercase.

- [ ] **Step 6: Deploy layout list** (344-345): add `deploy/micro/`, `deploy/collector/`, `deploy/observability/`, `deploy/terraform/` with one line each (read each dir's README first — one line only, no invention).

- [ ] **Step 7: De-reference internal doc numbers** where cheap: replace "(v1.7)", "R7", "§6.3" with plain prose, or footnote that they reference internal design docs. Do not rewrite the bridge section wholesale — only remove dangling references.

- [ ] **Step 8: Commit**

```bash
git commit -m "docs(readme): correct role list, package map, plugin list, interrupt and nacos facts"
```

---

### Task 15: README.zh-CN.md sync

**Files:**
- Modify: `README.zh-CN.md`

- [ ] **Step 1:** Port Task 14's corrected content into the Chinese file: translate the missing `### Editions & repository layout` and `### The bridge (private-cloud form)` sections (from the corrected English text, not the current one), plus the office plugin, interrupt fix, and nacos prerequisite.

- [ ] **Step 2:** Verify section anchors match between languages (the header links `[架构](#architecture)`-style pairs). Run a side-by-side heading diff: `grep -n '^#' README.md` vs `grep -n '^#' README.zh-CN.md` — every English section must have a Chinese counterpart.

- [ ] **Step 3: Commit**

```bash
git commit -m "docs(readme-zh): sync enterprise sections and corrections with English README"
```

---

### Task 16: CONTRIBUTING.md + Makefile accuracy

**Files:**
- Modify: `CONTRIBUTING.md`
- Modify: `Makefile`

- [ ] **Step 1:** CONTRIBUTING.md:9 — Node.js ≥ 20 → bun (match `Makefile:8`; check `web/package.json` packageManager field for the version to state). Line 16 — correct `make build` description (builds `./cmd/woca` only, to `build/woca`).

- [ ] **Step 2:** Makefile — fix the stale "Phase 3" comment above `build-personal` (roles now exist); add missing `.PHONY` entries for `ent-obs-up/down/logs`, `dev-bridge`, `dev-langfuse` and any other non-file targets lacking it (`grep -n '^[a-z-]*:' Makefile` vs the `.PHONY` lines).

- [ ] **Step 3: Commit**

```bash
git commit -m "docs(contributing): bun toolchain, accurate build scope; makefile phony cleanup"
```

---

### Task 17: Test-file descriptive renames

**Files:**
- Rename: every `gaps_test.go` / `gaps2_test.go` / `finalgaps_test.go` / `batchN_test.go` / duplicated `ladym_test.go`

- [ ] **Step 1: Inventory**

Run: `find internal -name 'gaps*_test.go' -o -name 'finalgaps_test.go' -o -name 'batch*_test.go' -o -name 'smallfuncs_test.go'`

- [ ] **Step 2:** For each, read the test names inside and `git mv` to a descriptive name (e.g. `internal/agent/gaps2_test.go` → `internal/agent/interrupt_resume_test.go` — derive from content, never guess). `internal/settings/ladym_test.go` vs `internal/memory/ladym_test.go`: keep both only if they test different integrations; rename to `ladym_settings_test.go`-style only where collision is confusing.

- [ ] **Step 3: Verify + commit**

Run: `go test ./... ` (spot-check renamed packages). Commit: `test: descriptive names for coverage-gap test files`.

---

### Task 18: Final verification gate

- [ ] **Step 1:** `gofmt -l .` empty; `go vet ./...`; `golangci-lint run ./...`; `go build ./...`; `go test ./... -count=1`; `go test ./... -race -count=1` (background, long)
- [ ] **Step 2:** `make check-editions && make check-deps && make check-server-artifacts`; `ci/check-sse-contract.sh`
- [ ] **Step 3:** `make web && make ee-web && go build ./cmd/...` then `git status` clean (artifacts ignored)
- [ ] **Step 4:** `cd native-client && go test ./...`; `cd web && bun run test`
- [ ] **Step 5:** Push branch and read the actual CI run green (gh run watch), or if pushing is not authorized, state that CI verification is pending.

---

## Appendix: audit findings summary (commit 5da2754)

Product: README lists 4/7 enterprise roles (woca-ops/brdige/edge missing); skillsvc full-stack orphan (zero mounts of `SpaHandler()`); nacos fail-closed undocumented; deploy/k8s contradicts code ("zero registry coupling"); woca-ops vs woca-worker port 9091 collision; README cites inaccessible design-doc numbers; deploy/selfhosted/.env tracked.

Conventions: .golangci.yml is v1 format (v2 refuses it) and CI never lints; ~65 panic sites with 30+ copy-pasted 3-line FromFunc blocks; 5 `%v` error-wrapping sites; duplicated start/startGoal (server/run.go) and goroutine pair (tui/tui.go); `context.Background()` on server request paths (session.go:156,183,280; runs.go:527,549; sessionstate.go:180-193 swallowed publish errors); servefront package doc says "Future" about woca-server which exists.

Modules: dir `internal/context` vs package `contextx`; module-path casing drift in README (ProjAnvil/WoCA) vs go.mod (projanvil/woca) — code imports all lowercase, LadyM/langchain-golang upstream casing inconsistent (document only); console→webgui→server reverse dependency (accepted: woca serves web anyway — no change this round); agent.go 1727 lines (deferred — cosmetic split, regression risk).

Tests: native-client module tests never run in CI; frontend vitest never in CI/Makefile; PG-gated tests (rls_pg_test.go, pg_live_test.go) always skip in CI (no postgres service); webd tests hardcode ports 3990-3992/3484; edgegw_test.go:653 skips on a 700ms timing assumption; gaps/batch-style test filenames in ~16 packages.

Frontend: Mach-O binary `native-client/native-client` tracked in git; ee console minified dist double-committed (ee/*/dist + internal/ee/*/spa); web/dist tracks only index.html → bare `go build` yields a runtime-broken SPA; SSE contract hand-synced strings both ends; three ee consoles byte-identical scaffolding (accepted — consolidation deferred); web/ has both bun.lock and package-lock.json; root node_modules is a stray vitest cache (ignored, local-only).

Docs/build: README interrupt contradiction (209-212 vs 220-224); office plugin missing from plugin section; zh-CN README missing entire enterprise sections (568 vs 716 lines); CONTRIBUTING says Node.js but Makefile uses bun, and misdescribes `make build`; Makefile stale "Phase 3" comment, missing .PHONY entries, `check` ≠ CI gates; .gitignore lacks .DS_Store; docs/superpowers re-added after removal commit (accepted as useful — no change).

Deferred with rationale (recorded here so the decision is auditable): console→webgui dependency inversion (no practical benefit — woca links the server anyway); internal/contextx package renames of repl/webd/webfx/webgui/servefront (churn without functional gain); agent.go split (pure cosmetics, regression risk); vendoring LadyM/langchain-golang (module proxy immutability suffices while repos are public); ee console shared-component extraction (3× duplication acknowledged, defer until a fourth console appears).

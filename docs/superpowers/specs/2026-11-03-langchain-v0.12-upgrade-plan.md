# langchain-golang v0.12 upgrade plan (blocked on a fork fix)

Date: 2026-11-03. Status: **implemented same day** — the fork shipped the
nested-Stream emitter fix as v0.12.5 (5710cd7), woca bumped to v0.12.5 /
checkpoint v0.3.9, and all required adaptations plus the adaptive-family
adoption landed (merge-time fragment stripping covers both openai
tool_call_chunk slices and anthropic NonStandard tool_use partials — the
latter also fixing a pre-existing duplicate-tool_use replay hazard; tokens
counts ToolCalls unconditionally; StreamTasks dead mode dropped; adaptive
families map levels to WithEffort with a construction-time temperature
guard). Draining and progressive tool-args stay deferred as stated below.

## Blocker

Fork v0.12.0 (commit c7777b5, #9458) strips a nested run's own Stream
emitter at `run` entry: `Stream` installs its emitter into ctx before
calling `run`, and `stripNestedInvokeCarriers` cannot tell it from an
inherited one. A graph node that Streams a child graph with its own
ThreadID — exactly woca's goal cycle shape (`internal/agent/goalgraph.go`
goalExecuteNode streams the inner agent graph on a fresh per-cycle thread) —
receives zero chunks; every goal cycle fails with "goal execute stream
ended without a terminal state". Reproduced minimally, root-caused, and a
validated patch handed to the fork session:
`../langchain_port/2026-11-03-nested-stream-emitter-fix-spec.md` (emitter
as a `run` parameter; patch verified against the fork's full test suite
and against woca's three broken packages via a `replace` wiring).

Release ask: fork v0.12.5. On release: bump root + checkpoint modules
(whatever the fork ships with it), then land the items below.

## Required adaptations (compile-clean today; semantic changes)

1. **Strip tool-call stream fragments in `MergeStreamChunks`**
   (`internal/agent/stream.go`). v0.12.0's tool-call-delta protocol
   (#41257) makes the openai Chat Completions stream yield one chunk per
   args slice carrying `messages.ToolCallChunkBlock` in `ContentBlocks`
   (v0.11.1 yielded nothing mid-stream; the terminal assembled chunk with
   complete `ToolCalls` is unchanged). Today `MergeStreamChunks` appends
   `ContentBlocks` wholesale, so fragments would accumulate into the final
   AI message → persisted history/graph state, the token estimate, and
   cross-provider replay (anthropic's `passthroughBlock` would forward
   `tool_call_chunk` blocks to the wire on model switch/fallback). Skip
   `ToolCallChunkBlock` blocks when merging; complete calls still arrive
   via the terminal chunk's `ToolCalls`.
2. **Count tool-call tokens unconditionally in `tokens.Estimate`**
   (`internal/tokens/tokens.go:142-155`). The estimate counts `ToolCalls`
   only when `len(m.ContentBlocks) == 0`. Post-v0.12 openai tool turns
   carry blocks (until #1 strips them — fix both anyway); anthropic turns
   have carried NonStandard partial blocks all along, silently suppressing
   tool tokens there. Count text/image blocks plus `ToolCalls` always; no
   double-count risk (openai CC ignores AI ContentBlocks on serialization;
   anthropic converts `ToolCalls` itself).
3. **Minor cleanup with #1**: `streamModes` requests `StreamTasks` but no
   pump has a case for it and the "observability bridge" comment is stale
   (obs comes from `observedNode`). Drop the mode or wire it — decide when
   touching the file.

## Adoption candidates

4. **anthropic adaptive-thinking families** (`claude-opus-5*`,
   `claude-fable-5*`, `claude-sonnet-5-5*`). The fork now validates per
   request: these families reject manual thinking configs (budgets), and
   fable-5/sonnet-5-5 also reject non-default sampling (temperature != 1).
   woca is blind to them today: `ThinkingLevelsFor` offers the full budget
   ladder for any `claude-*` except 3.x, and `BuildModel` forwards
   `WithTemperature` whenever configured — an operator preset naming one
   of these models + any thinking level or temperature fails every
   request with the fork's error. Adopt: name-pattern gate in
   `config.ThinkingLevelsFor` mapping low/medium/high to the fork's
   `WithEffort` (effort + profile-default adaptive thinking) instead of
   budget tokens; `BuildModel` fails fast on Temperature > 0 for the
   sampling-rejecting families (explicit error, no silent drop). Keep the
   name patterns in sync with the fork's `assertValidModelConfiguration`.
5. **RunControl cooperative draining (v0.12.0 #9281)** — evaluated, NOT
   adopted as an interrupt replacement: draining stops at the next
   superstep boundary, so an esc-interrupt would visibly stall until the
   in-flight model turn completes. Possible targeted fit: server graceful
   shutdown / run-lease loss (today hard-cancels and drops the partial
   transcript; a drained run could persist final state — needs
   `Run.startWith` to treat `*types.GraphDrainedError` as a
   final-persist). Deferred pending adjudication; the goal loop's
   hand-rolled `/goal pause` boundary check is the in-repo precedent.
6. **`agents.ToolErrorMiddleware` (v0.12.3)** — no plug-in point: woca has
   no prebuilt-agent wrap chain and pre-converts tool errors to content at
   `Registry.Execute`/`Agent.execute`. Not adopted.
7. **Progressive tool-args UI** from the new per-slice deltas — possible
   later; the fragments are stripped per #1, and both renderers currently
   consume only complete calls.

## Corrections to prior findings

- The deferred "concurrent-run delta cross-contamination" bug is NOT
  library-side. woca registers no DeltaChannel at all (its messages
  channel is a full-state BinaryOperator via `AddReducer`), so the whole
  v0.12.0 delta-channel persistence wave does not apply to woca's graphs.
  The plausible owners are woca's own overlap windows: the gateless `/btw`
  run path (`internal/server/runs.go:323-345`), shared-agent mutable state
  (`a.inputs` queue, `a.setTranscript` last-wins, `lastThread`), and
  `session_state.run_id` pointer flips. Move it to the
  concurrency-hardening backlog with these leads.

## What v0.12 brings that woca gets for free on re-bump

Delta-channel persistence correctness wave (#8544/#9114/#9269/#9275) for
any future DeltaChannel adoption; pinned-resume double-apply fix; openai
sparse-index and sticky tool-call assembly fixes; anthropic
fable-5/sonnet-5-5 request validation (interacts with #4); checkpoint
v0.3.9; tool_end emission fixes (woca consumes none — its obs bridge is
node-boundary, not StreamEvents).

## Other dependency work completed 2026-11-03 (independent of this plan)

All direct Go deps except the langchain family bumped (pgx v5.12.0,
nats-server v2.15.2, nats.go v1.55.0, otel v1.47.0 family, prometheus
client v1.25.0, modernc sqlite v1.61.1, goquery v1.13.0, x/* minors);
web npm deps updated within semver ranges (majors — typescript 7, vitest 5,
@vueuse 15, happy-dom 21 — deliberately not jumped); LadyM verified at
remote latest v0.8.4 (its dict sha256 pins track LadyM's cjkDictRegistry,
unchanged in v0.8.4).

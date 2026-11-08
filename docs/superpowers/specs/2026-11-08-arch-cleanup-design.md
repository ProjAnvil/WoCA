# Architecture cleanup batch — design

Date: 2026-11-08
Status: approved (user picked "都做" from the arch-review decision list; F3/F9 stay unsplit by recommendation, F8 naming deferred)
Source audit: `woca-internal-docs/arch-review-2026-11-08.md` (F1/F2/F4/F5/F6/F7/F10)

## Decisions

| # | Decision |
|---|---|
| F2 | `server.Config` is hand-built twice and has drifted: the TUI `/web` path (`cmd/woca/main.go:727` `webgui.New(server.Config{…})`) omits `ActivityAPI` and `FS`, so the activity panel endpoints return 501 in the embedded Web UI while `servefront` works. Fix: `agentbuild.App.BaseServerConfig()` produces the shared base (the ~16 fields both paths set identically: Agent/Static/Logger/SessionDir/WorkDir/Settings/Memory/MemoryEngine/RebuildMemory/RebuildModel/CfgLive/…/ActivityAPI), both constructors start from it and override only what differs; a consistency test pins the TUI path to servefront's values for the shared fields. ActivityAPI for the TUI path is ON (personal single-process form — no dispatch). |
| F1 | Extract `internal/agent/activity.go` (+ its test) into package `agent/activity` as the first step of un-budding the agent god package. The subpackage must not import `internal/agent` (no cycles). It receives an injected bundle: the four managers' snapshot/action surfaces (shells `*tools.BackgroundShells`, tasks `*agent.TaskManager` — see note, cron `*agent.CronScheduler`-ish, todos) plus the queue hooks it already uses; `Agent.ActivityFor` and friends stay as the public API (thin delegation), so `internal/server` and the sdk surface do not change. NOTE: TaskManager/cronScheduler are unexported types inside package agent — if exporting them wholesale is too invasive, move activity into the subpackage with narrow exported interfaces defined in the subpackage and implemented by the agent package (dependency inversion, preferred). Goal/task/queue splits are explicitly OUT of this batch (architect: goal re-enters next quarter). |
| F4 | Deduplicate the ops-console assembly that exists verbatim twice (find both sites — audit says ops-console 装配逐字×2, likely cmd/woca-ops vs servefront/opsconsole path); extract one shared assembly function used by both. |
| F5 | Pin the sdk's 19 `agent`-package type aliases as an explicit list in a guard test: any change to the aliased set (add/remove/rename) must consciously update the list, so the external contract cannot drift silently. |
| F6 | Remove the only cross-layer reverse dependency: `internal/settings` → `internal/websearch`. Find what settings actually uses (probably search-endpoint validation or provider defaults) and invert it (callback/registration injected from an upper layer, or move the offending piece to websearch itself). |
| F7 | Deduplicate the `defaultObsAddr`-style probe boilerplate that exists in three places into one shared helper (platform-level, used by the three call sites). |
| F10 | No CORS middleware anywhere: this is a deliberate same-origin constraint (web UI served by the same origin that owns the API; remote forms go through edge/bridge tunnels). Record it explicitly where a reader would look (webfx package doc + README security/permissions section) so it reads as a decision, not an omission. |

## Non-goals

Splitting `internal/server` (F3) or `tools/background.go` (F9); renaming `agent/agents`, `webd/webgui` (F8, deferred); any goal/task/queue extraction beyond activity; changing any external API shape (sdk aliases and `Agent` activity methods keep their signatures).

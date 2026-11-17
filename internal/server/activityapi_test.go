package server

// Session activity API integration tests (Task 8, PM §4.3/§4.4/§4.8): the
// REST snapshot, per-item output tails and user stop actions, the
// signal-only activity_changed event on the global stream, the deployment
// assembly switch (501) and the authz scope wiring. Every wait is
// deadline-bounded; the scripted model cycles, so scenarios that start runs
// pin the response sequence that keeps them bounded.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/authz"
	"github.com/projanvil/woca/internal/platform/tenancy"
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// activityEnv bundles one activity-test server with its agent and tool
// registry, so scenarios can arm state directly through the tool layer (the
// same seams a model turn drives) and then assert through HTTP.
type activityEnv struct {
	s   *Server
	ts  *httptest.Server
	a   *agent.Agent
	reg *tools.Registry
}

func (e *activityEnv) url(format string, args ...any) string {
	return e.ts.URL + fmt.Sprintf(format, args...)
}

// newActivityServer serves an activity-test server. delay throttles the
// scripted model's calls (holding a run open across a completion, see
// newWakeAgent); responses cycle. mods mutate the Config after defaults
// (ActivityAPI on, no settings store).
func newActivityServer(t *testing.T, delay time.Duration, responses []messages.Message, mods ...func(*Config)) *activityEnv {
	return newActivityServerAgentMod(t, delay, responses, nil, mods...)
}

// newActivityServerAgentMod is newActivityServer with a hook over the agent
// config. The snapshot-shape test pins a distinct subagent model with it:
// subagent shells now land in the session-shared manager (graph-native HITL
// §3.7), so a scripted child replaying the main arm round would add shells
// and completions the scenario never meant to cover.
func newActivityServerAgentMod(t *testing.T, delay time.Duration, responses []messages.Message, agentMod func(*agent.Config), mods ...func(*Config)) *activityEnv {
	t.Helper()
	reg := tools.NewRegistry(t.TempDir())
	a := newWakeAgentReg(t, delay, reg, responses...)
	if agentMod != nil {
		acfg := agent.Config{
			Model:          language.NewFakeChatModel(language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}), language.WithResponses(responses...)),
			Registry:       reg,
			SystemPrompt:   "test",
			PermissionMode: config.ModeBypass,
			RecursionLimit: 10,
		}
		agentMod(&acfg)
		var err error
		a, err = agent.New(acfg)
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := Config{
		Agent:       a,
		Static:      fstest.MapFS{},
		Logger:      zerolog.Nop(),
		SessionDir:  t.TempDir(),
		WorkDir:     t.TempDir(),
		ActivityAPI: true,
	}
	for _, m := range mods {
		m(&cfg)
	}
	s := New(cfg)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return &activityEnv{s: s, ts: ts, a: a, reg: reg}
}

// actCall scripts one AI turn with a single tool call.
func actCall(name string, args map[string]any) messages.Message {
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
		ID:   fmt.Sprintf("act-%d", wakeToolSeq.Add(1)),
		Name: name,
		Args: args,
	}}}
}

// callSessionTool invokes a registry tool directly with ctx's run scope set
// to session, exactly like callTodoWrite in the agent package's seam tests:
// the tool layer owns the semantics, no model turn needed.
func callSessionTool(t *testing.T, reg *tools.Registry, session, name string, args map[string]any) {
	t.Helper()
	var tool coretools.Tool
	for _, tl := range reg.All() {
		if tl.Name() == name {
			tool = tl
			break
		}
	}
	if tool == nil {
		t.Fatalf("tool %s not registered", name)
	}
	ctx := tracing.WithRun(t.Context(), "run-direct", session)
	if _, err := tool.Invoke(ctx, args); err != nil {
		t.Fatalf("%s invoke: %v", name, err)
	}
}

// startSessionShell arms a background shell owned by session through the
// registry and returns its id.
func startSessionShell(t *testing.T, reg *tools.Registry, session, command string) string {
	t.Helper()
	ctx := tracing.WithRun(t.Context(), "run-direct", session)
	id, err := reg.Background().Start(ctx, command, "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// activityDo issues one activity-API request and returns status + body.
func activityDo(t *testing.T, method, url string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

// wantCode asserts the standard error envelope {"error":..., "code":...}.
func wantCode(t *testing.T, body []byte, code string) {
	t.Helper()
	var env struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("bad error body %q: %v", body, err)
	}
	if env.Code != code {
		t.Fatalf("error code = %q (body %q), want %q", env.Code, body, code)
	}
	if env.Error == "" {
		t.Fatalf("error message empty: %q", body)
	}
}

// parseRFC3339UTC asserts an RFC3339 UTC timestamp and returns it.
func parseRFC3339UTC(t *testing.T, s string) time.Time {
	t.Helper()
	if !strings.HasSuffix(s, "Z") {
		t.Fatalf("time %q is not UTC (RFC3339 Z)", s)
	}
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("time %q: %v", s, err)
	}
	return ts
}

// activityJSON fetches the snapshot as a generic map.
func activityJSON(t *testing.T, env *activityEnv, sid string) map[string]any {
	t.Helper()
	status, body := activityDo(t, http.MethodGet, env.url("/api/sessions/%s/activity", sid))
	if status != http.StatusOK {
		t.Fatalf("GET activity %s: %d %s", sid, status, body)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("bad snapshot %q: %v", body, err)
	}
	return m
}

// waitForActivity polls the snapshot until pred holds (bounded).
func waitForActivity(t *testing.T, env *activityEnv, sid, desc string, pred func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m := activityJSON(t, env, sid)
		if pred(m) {
			return m
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s in session %s", desc, sid)
	return nil
}

func shellEntries(t *testing.T, snap map[string]any) []map[string]any {
	t.Helper()
	raw, _ := snap["shells"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

func taskEntries(t *testing.T, snap map[string]any) []map[string]any {
	t.Helper()
	raw, _ := snap["tasks"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

func cronEntries(t *testing.T, snap map[string]any) []map[string]any {
	t.Helper()
	raw, _ := snap["crons"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, _ := r.(map[string]any)
		out = append(out, m)
	}
	return out
}

// Snapshot shape: one scripted run arms todos, two background shells (ids
// in creation order), a background subagent task and a cron; the snapshot
// carries every field, arrays never null, times RFC3339 UTC. auto-wake is
// off so the completions queue quietly instead of starting wake runs (which
// would cycle the scripted responses again); that also pins wake.enabled.
func TestActivitySnapshotShape(t *testing.T) {
	armRound := messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "TodoWrite", Args: map[string]any{
			"todos": []map[string]any{
				{"content": "Add activity API", "status": "in_progress", "active_form": "Adding activity API"},
				{"content": "Write tests", "status": "pending"},
			},
		}},
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "Bash", Args: map[string]any{
			"command": "sleep 30", "run_in_background": true}},
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "Bash", Args: map[string]any{
			"command": "echo second", "run_in_background": true}},
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "Task", Args: map[string]any{
			"description": "Audit flaky tests", "prompt": "Audit flaky tests", "run_in_background": true}},
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "ScheduleCron", Args: map[string]any{
			"name": "nightly-check", "prompt": "Run the test suite and report failures", "cron": "0 2 * * *"}},
	}}
	env := newActivityServerAgentMod(t, 0, []messages.Message{
		armRound,
		messages.AI("armed"),
		messages.AI("task report"),
	}, func(ac *agent.Config) {
		// Distinct subagent model: the background Task's child must not
		// replay the arm round (its shells would join the session-shared
		// manager — a different scenario than this shape test pins).
		ac.SubagentModel = language.NewFakeChatModel(language.WithResponses(messages.AI("task report")))
	}, func(c *Config) { c.Settings = wakeSettings(t, settings.KeyAutoWake, "false") })

	runID, sid, _ := postRun(t, env.ts.URL, "s1", "arm everything")
	waitRunDoneInProc(t, env.s, runID)

	// The quick shell and the task complete on their own; both queue a
	// notification (auto-wake off keeps them queued). Whether a completion
	// STAYS pending or is consumed by the arming run's own mid-turn drain
	// (delivered into the transcript before the run ends) is timing — the
	// fast echo shell regularly beats the drain — so the deterministic
	// contract is total accounting: both completions landed somewhere, none
	// lost. The wait is terminal-stable: the run is done, auto-wake is off
	// (nothing drains the queue afterwards) and the sleeper never finishes,
	// so once the sum reaches 2 it stays 2.
	snap := waitForActivity(t, env, sid, "task completed and both completions accounted for", func(m map[string]any) bool {
		tasks := taskEntries(t, m)
		if len(tasks) != 1 || tasks[0]["status"] != "completed" {
			return false
		}
		wake, _ := m["wake"].(map[string]any)
		pending, _ := wake["pending_notifications"].(float64)
		return int(pending)+taggedBlocks(sessionMessages(t, env.ts.URL, sid)) == 2
	})

	// Identity and server_time.
	if snap["session_id"] != sid {
		t.Fatalf("session_id = %v, want %q", snap["session_id"], sid)
	}
	parseRFC3339UTC(t, snap["server_time"].(string))

	// Todos: model order preserved, active_form only where written.
	todos, _ := snap["todos"].(map[string]any)
	if todos == nil {
		t.Fatal("todos object missing")
	}
	items, _ := todos["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("todo items = %v, want 2", todos["items"])
	}
	first := items[0].(map[string]any)
	if first["content"] != "Add activity API" || first["status"] != "in_progress" ||
		first["active_form"] != "Adding activity API" {
		t.Fatalf("first todo = %v", first)
	}
	second := items[1].(map[string]any)
	if second["content"] != "Write tests" || second["status"] != "pending" {
		t.Fatalf("second todo = %v", second)
	}
	if _, has := second["active_form"]; has {
		t.Fatalf("second todo must omit active_form: %v", second)
	}
	parseRFC3339UTC(t, todos["updated_at"].(string))

	// Shells: numeric id order; the sleeper is running, the echo completed.
	// (The tool round starts both concurrently, so which command got which
	// id is racy — find them by command.)
	shells := shellEntries(t, snap)
	if len(shells) != 2 || shells[0]["id"] != "1" || shells[1]["id"] != "2" {
		t.Fatalf("shell ids = %v, want [1 2] in order", shells)
	}
	var sleepShell, echoShell map[string]any
	for _, sh := range shells {
		switch sh["command"] {
		case "sleep 30":
			sleepShell = sh
		case "echo second":
			echoShell = sh
		}
	}
	if sleepShell == nil || echoShell == nil {
		t.Fatalf("shells by command = %v", shells)
	}
	if sleepShell["status"] != "running" {
		t.Fatalf("running shell = %v", sleepShell)
	}
	if sleepShell["exit_code"] != nil || sleepShell["ended_at"] != nil {
		t.Fatalf("running shell must have null exit_code/ended_at: %v", sleepShell)
	}
	if sleepShell["stopped_by"] != "" {
		t.Fatalf("running shell stopped_by = %v, want \"\"", sleepShell["stopped_by"])
	}
	parseRFC3339UTC(t, sleepShell["started_at"].(string))
	if echoShell["status"] != "completed" || echoShell["exit_code"] != float64(0) {
		t.Fatalf("completed shell = %v", echoShell)
	}
	parseRFC3339UTC(t, echoShell["ended_at"].(string))

	// Task: honest completed status, ended_at set, not stopped.
	tasks := taskEntries(t, snap)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %v, want 1", snap["tasks"])
	}
	task := tasks[0]
	if task["id"] != "task-1" || task["description"] != "Audit flaky tests" {
		t.Fatalf("task identity = %v", task)
	}
	if task["subagent"] != "general" || task["status"] != "completed" || task["stopped_by"] != "" {
		t.Fatalf("task facts = %v", task)
	}
	parseRFC3339UTC(t, task["started_at"].(string))
	parseRFC3339UTC(t, task["ended_at"].(string))

	// Cron: full prompt, spec, next fire within reach, never fired.
	crons := cronEntries(t, snap)
	if len(crons) != 1 {
		t.Fatalf("crons = %v, want 1", snap["crons"])
	}
	cron := crons[0]
	if cron["id"] != "cron-1" || cron["name"] != "nightly-check" || cron["spec"] != "0 2 * * *" ||
		cron["prompt"] != "Run the test suite and report failures" {
		t.Fatalf("cron identity = %v", cron)
	}
	parseRFC3339UTC(t, cron["created_at"].(string))
	if cron["last_fired_at"] != nil {
		t.Fatalf("last_fired_at = %v, want null", cron["last_fired_at"])
	}
	next := parseRFC3339UTC(t, cron["next_fire_at"].(string))
	if next.Before(time.Now().Add(-time.Minute)) || next.After(time.Now().Add(367*24*time.Hour)) {
		t.Fatalf("next_fire_at %v out of the 366-day window", next)
	}

	// Wake: disabled by settings here (the default-on posture is asserted in
	// the unknown-session test), nothing capped or stalled. The pending count
	// is whatever the mid-turn-drain split left (1 or 2), but it must be the
	// queue's own count and account, with the delivered blocks, for both
	// completions.
	wake, _ := snap["wake"].(map[string]any)
	if wake["enabled"] != false || wake["capped"] != false || wake["stalled"] != false {
		t.Fatalf("wake = %v, want enabled=false capped=false stalled=false", wake)
	}
	pending := int(wake["pending_notifications"].(float64))
	if q := env.a.QueuedNotificationCountFor(sid); q != pending {
		t.Fatalf("snapshot pending_notifications = %d, agent queue holds %d (must agree)", pending, q)
	}
	if sum := pending + taggedBlocks(sessionMessages(t, env.ts.URL, sid)); sum != 2 {
		t.Fatalf("pending (%d) + delivered completions = %d, want 2 total", pending, sum)
	}
}

// An unknown session yields the empty snapshot (200, [] arrays, null
// todos.updated_at) — the same posture as GET /api/sessions/{id}.
func TestActivitySnapshotUnknownSessionIsEmpty(t *testing.T) {
	env := newActivityServer(t, 0, nil)

	status, body := activityDo(t, http.MethodGet, env.url("/api/sessions/%s/activity", "no-such-session"))
	if status != http.StatusOK {
		t.Fatalf("GET activity unknown session = %d %s, want 200", status, body)
	}
	var raw struct {
		SessionID  string          `json:"session_id"`
		ServerTime string          `json:"server_time"`
		Todos      json.RawMessage `json:"todos"`
		Shells     json.RawMessage `json:"shells"`
		Tasks      json.RawMessage `json:"tasks"`
		Crons      json.RawMessage `json:"crons"`
		Subagents  json.RawMessage `json:"subagents"`
		Wake       struct {
			Enabled              bool `json:"enabled"`
			Capped               bool `json:"capped"`
			Stalled              bool `json:"stalled"`
			PendingNotifications int  `json:"pending_notifications"`
		} `json:"wake"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("bad snapshot %q: %v", body, err)
	}
	if raw.SessionID != "no-such-session" {
		t.Fatalf("session_id = %q", raw.SessionID)
	}
	parseRFC3339UTC(t, raw.ServerTime)
	if string(raw.Shells) != "[]" || string(raw.Tasks) != "[]" || string(raw.Crons) != "[]" ||
		string(raw.Subagents) != "[]" {
		t.Fatalf("arrays must render [] not null: shells=%s tasks=%s crons=%s subagents=%s",
			raw.Shells, raw.Tasks, raw.Crons, raw.Subagents)
	}
	var todos struct {
		Items     json.RawMessage `json:"items"`
		UpdatedAt *string         `json:"updated_at"`
	}
	if err := json.Unmarshal(raw.Todos, &todos); err != nil {
		t.Fatalf("bad todos %q: %v", raw.Todos, err)
	}
	if string(todos.Items) != "[]" {
		t.Fatalf("todos.items = %s, want []", todos.Items)
	}
	if todos.UpdatedAt != nil {
		t.Fatalf("todos.updated_at = %v, want null (never written)", *todos.UpdatedAt)
	}
	// No settings store: the design default is wake on.
	if !raw.Wake.Enabled || raw.Wake.Capped || raw.Wake.Stalled || raw.Wake.PendingNotifications != 0 {
		t.Fatalf("wake = %+v, want enabled defaults", raw.Wake)
	}
}

// subagentSectionModel scripts the subagents-section scenario parent: it
// re-issues the arm batch (one background shell + one foreground Task)
// until BOTH call ids carry tool results — a transcript-driven model, not a
// cursor-scripted FakeChatModel, because the fan-out re-binds tools per
// round and every bind is a fresh cursor copy that would re-arm forever
// (the same reason subgraphfanout_test.go's N1 parent is transcript-driven).
type subagentSectionModel struct{}

func (m *subagentSectionModel) pick(input []messages.Message) messages.Message {
	answered := map[string]bool{}
	for _, msg := range input {
		if msg.Role == messages.RoleTool {
			answered[msg.ToolCallID] = true
		}
	}
	if answered["sub-sec-shell"] && answered["sub-sec-task"] {
		return messages.AI("after the child")
	}
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: "sub-sec-shell", Name: "Bash", Args: map[string]any{
			"command": "sleep 30", "run_in_background": true}},
		{ID: "sub-sec-task", Name: "Task", Args: map[string]any{
			"description": "Summarize the tree", "prompt": "Summarize the tree"}},
	}}
}

func (m *subagentSectionModel) Invoke(_ context.Context, input []messages.Message, _ ...runnables.Option) (messages.Message, error) {
	return m.pick(input), nil
}

func (m *subagentSectionModel) Stream(_ context.Context, input []messages.Message, _ ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return runnables.NewSliceStream([]messages.Message{m.pick(input)}), nil
}

func (m *subagentSectionModel) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	for i, in := range inputs {
		out[i], _ = m.Invoke(ctx, in, opts...)
	}
	return out, nil
}

func (m *subagentSectionModel) BindTools([]coretools.Tool) (language.ChatModel, error) {
	return m, nil
}

func (m *subagentSectionModel) Capabilities() language.ChatModelCapabilities {
	return language.ChatModelCapabilities{ToolCalling: true, Streaming: true}
}

func (m *subagentSectionModel) InputSchema() schema.Schema {
	return schema.Schema{"type": "array", "description": "chat messages"}
}

func (m *subagentSectionModel) OutputSchema() schema.Schema { return schema.Schema{"type": "object"} }

// The Subagents section (graph-native HITL §3.7, Task 12): a foreground
// Task child registers in the session's runs registry and surfaces in the
// snapshot with its lifecycle facts (def, call id, origin, done state,
// report, timestamps); shells carry the initiator attribution — "main" for
// shells the root graph started. Auto-wake is off so the sleeping shell's
// eventual completion can never cycle the scripted responses.
func TestActivitySubagentsSectionAndShellOwner(t *testing.T) {
	env := newActivityServerAgentMod(t, 0, nil, func(ac *agent.Config) {
		ac.Model = &subagentSectionModel{}
		ac.SubagentModel = language.NewFakeChatModel(language.WithResponses(messages.AI("child report")))
	}, func(c *Config) { c.Settings = wakeSettings(t, settings.KeyAutoWake, "false") })

	runID, sid, _ := postRun(t, env.ts.URL, "s1", "run a subagent")
	waitRunDoneInProc(t, env.s, runID)

	snap := waitForActivity(t, env, sid, "one done subagent run", func(m map[string]any) bool {
		subs, _ := m["subagents"].([]any)
		return len(subs) == 1
	})
	subs, _ := snap["subagents"].([]any)
	sub, _ := subs[0].(map[string]any)
	if sub["def"] != "general" || sub["state"] != "done" {
		t.Fatalf("subagent run = %v", sub)
	}
	if sub["report"] != "child report" {
		t.Fatalf("subagent report = %v", sub)
	}
	callID, _ := sub["call_id"].(string)
	if callID == "" || sub["origin"] != "subagent:general#"+callID {
		t.Fatalf("subagent identity = %v", sub)
	}
	if _, ok := sub["started_at"].(string); !ok {
		t.Fatalf("subagent started_at = %v", sub["started_at"])
	}
	if _, ok := sub["ended_at"].(string); !ok {
		t.Fatalf("subagent ended_at = %v", sub["ended_at"])
	}

	// Shell initiator: the arm round's shell was started by the main graph.
	shells := shellEntries(t, snap)
	if len(shells) != 1 || shells[0]["owner"] != "main" {
		t.Fatalf("shells = %v, want one main-owned shell", shells)
	}
}

// Output tail: the default window, byte clamping, rune-boundary cuts and
// the 400 on malformed tail_bytes. Expectations are computed against the
// shell's own final output so the contract (suffix + boundary) is what is
// asserted, not a hardcoded buffer.
func TestActivityShellOutputTail(t *testing.T) {
	env := newActivityServer(t, 0, nil)
	const command = "printf 'ααββγγδε'"
	id := startSessionShell(t, env.reg, "sess-tail", command)
	base := env.url("/api/sessions/sess-tail/background/shells/%s/output", id)

	// Wait for the process to finish so the buffer is final.
	var full struct {
		Output     string `json:"output"`
		Truncated  bool   `json:"truncated"`
		TotalBytes int    `json:"total_bytes"`
		Status     string `json:"status"`
		ID         string `json:"id"`
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		status, body := activityDo(t, http.MethodGet, base)
		if status != http.StatusOK {
			t.Fatalf("GET output = %d %s", status, body)
		}
		if err := json.Unmarshal(body, &full); err != nil {
			t.Fatalf("bad output body %q: %v", body, err)
		}
		if full.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell never completed: %+v", full)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if full.Output != "ααββγγδε" || full.TotalBytes != len("ααββγγδε") || full.Truncated {
		t.Fatalf("default tail = %+v", full)
	}
	if full.ID != id {
		t.Fatalf("output id = %q, want %q", full.ID, id)
	}

	// A cut inside a multi-byte rune must back off to the rune boundary.
	status, body := activityDo(t, http.MethodGet, base+"?tail_bytes=5")
	if status != http.StatusOK {
		t.Fatalf("tail_bytes=5 = %d %s", status, body)
	}
	var cut struct {
		Output     string `json:"output"`
		Truncated  bool   `json:"truncated"`
		TotalBytes int    `json:"total_bytes"`
	}
	if err := json.Unmarshal(body, &cut); err != nil {
		t.Fatal(err)
	}
	want := full.Output[full.TotalBytes-5:]
	for len(want) > 0 && !utf8.RuneStart(want[0]) { // trim partial rune bytes
		want = want[1:]
	}
	// The last 5 bytes start mid-rune; only whole runes survive.
	if want != "δε" {
		t.Fatalf("rune-boundary expectation changed: got %q", want)
	}
	if cut.Output != want || !cut.Truncated || cut.TotalBytes != full.TotalBytes {
		t.Fatalf("tail_bytes=5 = %+v, want output %q truncated", cut, want)
	}

	// 0 selects the default window; a huge value is clamped to the cap.
	for _, q := range []string{"?tail_bytes=0", "?tail_bytes=99999999", "?tail_bytes=262144"} {
		status, body := activityDo(t, http.MethodGet, base+q)
		if status != http.StatusOK {
			t.Fatalf("GET output %s = %d %s", q, status, body)
		}
		var v struct {
			Output    string `json:"output"`
			Truncated bool   `json:"truncated"`
		}
		if err := json.Unmarshal(body, &v); err != nil {
			t.Fatal(err)
		}
		if v.Output != full.Output || v.Truncated {
			t.Fatalf("GET output %s = %+v, want the whole small buffer", q, v)
		}
	}

	// Malformed values are rejected, never guessed.
	for _, q := range []string{"?tail_bytes=abc", "?tail_bytes=-1", "?tail_bytes=1.5"} {
		status, body := activityDo(t, http.MethodGet, base+q)
		if status != http.StatusBadRequest {
			t.Fatalf("GET output %s = %d %s, want 400", q, status, body)
		}
		wantCode(t, body, "S-B-0100")
	}
}

// Ownership (PM §5, guessable ids): a foreign or unknown item id answers
// 404 with byte-identical bodies (no existence oracle), and a foreign stop
// attempt changes nothing for the owning session.
func TestActivityForeignItemIs404(t *testing.T) {
	env := newActivityServer(t, 0, nil)
	id := startSessionShell(t, env.reg, "sess-a", "sleep 30")

	// Foreign reads and writes on the shell.
	paths := []string{
		env.url("/api/sessions/sess-b/background/shells/%s/output", id),
		env.url("/api/sessions/sess-b/background/shells/%s/kill", id),
	}
	for _, p := range paths {
		method := http.MethodGet
		if strings.HasSuffix(p, "/kill") {
			method = http.MethodPost
		}
		status, body := activityDo(t, method, p)
		if status != http.StatusNotFound {
			t.Fatalf("%s %s = %d %s, want 404", method, p, status, body)
		}
		wantCode(t, body, "S-B-0004")
	}

	// Unknown id: same status, same body — indistinguishable.
	status, unknown := activityDo(t, http.MethodGet, env.url("/api/sessions/sess-a/background/shells/%s/output", "999"))
	if status != http.StatusNotFound {
		t.Fatalf("unknown id = %d, want 404", status)
	}
	_, foreign := activityDo(t, http.MethodGet, env.url("/api/sessions/sess-b/background/shells/%s/output", id))
	if string(unknown) != string(foreign) {
		t.Fatalf("unknown (%q) and foreign (%q) bodies differ: id leak", unknown, foreign)
	}

	// A foreign cron delete is a 404 too, and the other session's snapshot
	// never lists the shell (AC1 isolation).
	status, body := activityDo(t, http.MethodDelete, env.url("/api/sessions/sess-b/background/crons/%s", "cron-1"))
	if status != http.StatusNotFound || !strings.Contains(string(body), "S-B-0004") {
		t.Fatalf("foreign cron delete = %d %s, want 404 S-B-0004", status, body)
	}
	if shells := shellEntries(t, activityJSON(t, env, "sess-b")); len(shells) != 0 {
		t.Fatalf("sess-b sees sess-a's shells: %v", shells)
	}
	if shells := shellEntries(t, activityJSON(t, env, "sess-a")); len(shells) != 1 || shells[0]["status"] != "running" {
		t.Fatalf("sess-a's shell must be untouched: %v", shells)
	}
}

// Kill through REST (D1 end to end): the shell stops with user attribution,
// a repeat kill reports already_finished, and the quiet completion never
// starts a wake run.
func TestActivityKillShellViaRESTIsQuietUserStop(t *testing.T) {
	env := newActivityServer(t, 0, []messages.Message{
		bgShellCall("sleep 30"),
		messages.AI("armed"),
		messages.AI("noted"),
	})
	runID, sid, _ := postRun(t, env.ts.URL, "s1", "start the shell")
	waitRunDoneInProc(t, env.s, runID)

	snap := waitForActivity(t, env, sid, "one running shell", func(m map[string]any) bool {
		shells := shellEntries(t, m)
		return len(shells) == 1 && shells[0]["status"] == "running"
	})
	shellID := snap["shells"].([]any)[0].(map[string]any)["id"].(string)

	status, body := activityDo(t, http.MethodPost, env.url("/api/sessions/%s/background/shells/%s/kill", sid, shellID))
	if status != http.StatusOK {
		t.Fatalf("kill = %d %s", status, body)
	}
	var killResp struct {
		Shell           map[string]any `json:"shell"`
		AlreadyFinished bool           `json:"already_finished"`
	}
	if err := json.Unmarshal(body, &killResp); err != nil {
		t.Fatal(err)
	}
	if killResp.AlreadyFinished {
		t.Fatalf("kill of a running shell reported already_finished: %s", body)
	}
	if killResp.Shell["status"] != "stopped" || killResp.Shell["stopped_by"] != "user" {
		t.Fatalf("killed shell = %v, want stopped/user", killResp.Shell)
	}
	if killResp.Shell["exit_code"] != nil {
		t.Fatalf("signal-killed exit_code = %v, want null", killResp.Shell["exit_code"])
	}

	// The snapshot agrees once the process settles (the kill signal is
	// asynchronous; ended_at lands on exit).
	snap = waitForActivity(t, env, sid, "shell stopped by user", func(m map[string]any) bool {
		shells := shellEntries(t, m)
		return len(shells) == 1 && shells[0]["status"] == "stopped" &&
			shells[0]["stopped_by"] == "user" && shells[0]["ended_at"] != nil
	})
	_ = snap

	// Kill race: the item already finished — 200 with already_finished, no
	// error, final state unchanged.
	status, body = activityDo(t, http.MethodPost, env.url("/api/sessions/%s/background/shells/%s/kill", sid, shellID))
	if status != http.StatusOK {
		t.Fatalf("second kill = %d %s, want 200", status, body)
	}
	if err := json.Unmarshal(body, &killResp); err != nil {
		t.Fatal(err)
	}
	if !killResp.AlreadyFinished || killResp.Shell["status"] != "stopped" {
		t.Fatalf("second kill = %s, want already_finished over stopped", body)
	}

	// D1: the quiet completion queues but never wakes.
	waitForQueueCount(t, env.a, sid, 1)
	if n := env.a.WakeableCountFor(sid); n != 0 {
		t.Fatalf("wakeable = %d, want 0 (user stop must not wake)", n)
	}
	time.Sleep(500 * time.Millisecond)
	if ids := registeredRuns(t, env.s); len(ids) != 1 {
		t.Fatalf("%d runs after the user stop, want 1 (no wake run)", len(ids))
	}
}

// Stop a running subagent task through REST: user attribution, idempotent
// repeat, empty output after the stop. The scripted model's 3s delay holds
// both the parent run and the subagent's first call open, so the stop lands
// while the task is provably running and everything settles in bounds.
func TestActivityStopTaskViaREST(t *testing.T) {
	env := newActivityServer(t, 3*time.Second, []messages.Message{
		actCall("Task", map[string]any{
			"description": "Audit flaky tests", "prompt": "Audit flaky tests", "run_in_background": true}),
		messages.AI("armed"),
		messages.AI("report"),
	})
	runID, sid, _ := postRun(t, env.ts.URL, "s1", "start the task")

	snap := waitForActivity(t, env, sid, "one running task", func(m map[string]any) bool {
		tasks := taskEntries(t, m)
		return len(tasks) == 1 && tasks[0]["status"] == "running"
	})
	taskID := snap["tasks"].([]any)[0].(map[string]any)["id"].(string)

	status, body := activityDo(t, http.MethodPost, env.url("/api/sessions/%s/background/tasks/%s/stop", sid, taskID))
	if status != http.StatusOK {
		t.Fatalf("stop = %d %s", status, body)
	}
	var stopResp struct {
		Task            map[string]any `json:"task"`
		AlreadyFinished bool           `json:"already_finished"`
	}
	if err := json.Unmarshal(body, &stopResp); err != nil {
		t.Fatal(err)
	}
	if stopResp.AlreadyFinished {
		t.Fatalf("stop of a running task reported already_finished: %s", body)
	}
	if stopResp.Task["status"] != "stopped" || stopResp.Task["stopped_by"] != "user" {
		t.Fatalf("stopped task = %v, want stopped/user", stopResp.Task)
	}
	parseRFC3339UTC(t, stopResp.Task["ended_at"].(string))

	// Idempotent repeat: already_finished over the unchanged state.
	status, body = activityDo(t, http.MethodPost, env.url("/api/sessions/%s/background/tasks/%s/stop", sid, taskID))
	if status != http.StatusOK {
		t.Fatalf("second stop = %d %s", status, body)
	}
	if err := json.Unmarshal(body, &stopResp); err != nil {
		t.Fatal(err)
	}
	if !stopResp.AlreadyFinished || stopResp.Task["status"] != "stopped" {
		t.Fatalf("second stop = %s, want already_finished over stopped", body)
	}

	// Output after a stop: the stopped task has no report.
	status, body = activityDo(t, http.MethodGet, env.url("/api/sessions/%s/background/tasks/%s/output", sid, taskID))
	if status != http.StatusOK {
		t.Fatalf("task output = %d %s", status, body)
	}
	var out struct {
		ID         string `json:"id"`
		Status     string `json:"status"`
		Output     string `json:"output"`
		TotalBytes int    `json:"total_bytes"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != taskID || out.Status != "stopped" || out.Output != "" || out.TotalBytes != 0 {
		t.Fatalf("stopped task output = %+v, want empty", out)
	}

	waitRunDoneInProc(t, env.s, runID)
	// The user stop is quiet: exactly the one user run, ever.
	time.Sleep(300 * time.Millisecond)
	if ids := registeredRuns(t, env.s); len(ids) != 1 {
		t.Fatalf("%d runs, want 1 (no wake run for a user stop)", len(ids))
	}
}

// Cron deletion: the foreign session cannot delete (404, entry intact),
// the owner can ({"deleted":true}), and a repeat is a 404.
func TestActivityDeleteCronViaREST(t *testing.T) {
	env := newActivityServer(t, 0, []messages.Message{
		actCall("ScheduleCron", map[string]any{
			"name": "nightly-check", "prompt": "Run the test suite", "cron": "0 2 * * *"}),
		messages.AI("scheduled"),
	})
	runID, sid, _ := postRun(t, env.ts.URL, "s1", "schedule the check")
	waitRunDoneInProc(t, env.s, runID)

	snap := waitForActivity(t, env, sid, "one cron", func(m map[string]any) bool {
		return len(cronEntries(t, m)) == 1
	})
	cronID := snap["crons"].([]any)[0].(map[string]any)["id"].(string)

	status, body := activityDo(t, http.MethodDelete, env.url("/api/sessions/sess-other/background/crons/%s", cronID))
	if status != http.StatusNotFound {
		t.Fatalf("foreign cron delete = %d %s, want 404", status, body)
	}
	wantCode(t, body, "S-B-0004")
	if crons := cronEntries(t, activityJSON(t, env, sid)); len(crons) != 1 {
		t.Fatalf("foreign delete removed the cron: %v", crons)
	}

	status, body = activityDo(t, http.MethodDelete, env.url("/api/sessions/%s/background/crons/%s", sid, cronID))
	if status != http.StatusOK {
		t.Fatalf("cron delete = %d %s", status, body)
	}
	var del struct {
		Deleted bool   `json:"deleted"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(body, &del); err != nil {
		t.Fatal(err)
	}
	if !del.Deleted || del.ID != cronID {
		t.Fatalf("delete response = %s", body)
	}

	if crons := cronEntries(t, activityJSON(t, env, sid)); len(crons) != 0 {
		t.Fatalf("cron still listed after delete: %v", crons)
	}
	status, body = activityDo(t, http.MethodDelete, env.url("/api/sessions/%s/background/crons/%s", sid, cronID))
	if status != http.StatusNotFound {
		t.Fatalf("repeat delete = %d %s, want 404", status, body)
	}
}

// activity_changed on /api/events (D4): a signal-only frame carrying the
// kind and the session id — no content — for todo writes, shell starts and
// kills.
func TestActivityChangedSignalOnGlobalStream(t *testing.T) {
	env := newActivityServer(t, 0, nil)
	frames, stop := subscribeGlobalEvents(t, env.ts.URL)
	defer stop()

	expect := func(kind, sid string) {
		t.Helper()
		f := awaitEvent(t, frames, EventActivityChanged)
		var data map[string]any
		if err := json.Unmarshal([]byte(f.data), &data); err != nil {
			t.Fatalf("bad activity_changed data %q: %v", f.data, err)
		}
		if len(data) != 2 {
			t.Fatalf("activity_changed carries content: %q (D4: signal only)", f.data)
		}
		if data["kind"] != kind || data["session_id"] != sid {
			t.Fatalf("activity_changed = %q, want kind=%s session=%s", f.data, kind, sid)
		}
	}

	callSessionTool(t, env.reg, "sess-ev", "TodoWrite", map[string]any{
		"todos": []map[string]any{{"content": "one", "status": "pending"}}})
	expect("todos", "sess-ev")

	shellID := startSessionShell(t, env.reg, "sess-ev", "sleep 30")
	expect("shells", "sess-ev")

	status, body := activityDo(t, http.MethodPost, env.url("/api/sessions/sess-ev/background/shells/%s/kill", shellID))
	if status != http.StatusOK {
		t.Fatalf("kill = %d %s", status, body)
	}
	expect("shells", "sess-ev") // the killed shell's exit fires the change
}

// Assembly switch off (PM §4.8): every activity route answers 501 S-B-0501
// and no activity_changed is published (the observer is never installed).
func TestActivityAPIDisabledReturns501(t *testing.T) {
	env := newActivityServer(t, 0, nil, func(c *Config) { c.ActivityAPI = false })

	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/sessions/s1/activity"},
		{http.MethodGet, "/api/sessions/s1/background/shells/1/output"},
		{http.MethodPost, "/api/sessions/s1/background/shells/1/kill"},
		{http.MethodGet, "/api/sessions/s1/background/tasks/task-1/output"},
		{http.MethodPost, "/api/sessions/s1/background/tasks/task-1/stop"},
		{http.MethodDelete, "/api/sessions/s1/background/crons/cron-1"},
	}
	for _, tc := range cases {
		status, body := activityDo(t, tc.method, env.url("%s", tc.path))
		if status != http.StatusNotImplemented {
			t.Fatalf("%s %s = %d %s, want 501", tc.method, tc.path, status, body)
		}
		wantCode(t, body, "S-B-0501")
	}

	// No signal either: changes stay local to the agent.
	frames, stop := subscribeGlobalEvents(t, env.ts.URL)
	defer stop()
	callSessionTool(t, env.reg, "sess-off", "TodoWrite", map[string]any{
		"todos": []map[string]any{{"content": "x", "status": "pending"}}})
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case f, ok := <-frames:
			if ok && f.event == EventActivityChanged {
				t.Fatalf("activity_changed published with the API off: %+v", f)
			}
		case <-deadline:
			return // silence, as required
		}
	}
}

// Without an agent there is no activity to serve: same 501.
func TestActivityAPINoAgentReturns501(t *testing.T) {
	s := New(Config{
		Static:      fstest.MapFS{},
		Logger:      zerolog.Nop(),
		SessionDir:  t.TempDir(),
		WorkDir:     t.TempDir(),
		ActivityAPI: true,
	})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)

	status, body := activityDo(t, http.MethodGet, ts.URL+"/api/sessions/s1/activity")
	if status != http.StatusNotImplemented {
		t.Fatalf("GET activity without agent = %d %s, want 501", status, body)
	}
	wantCode(t, body, "S-B-0501")
}

// denyScopeAuthorizer denies exactly one scope (the injected Config.Authz
// seam): reads pass, the run-control writes are 403 S-B-0003 — the viewer
// posture of PM §5.
type denyScopeAuthorizer struct{ scope string }

func (d denyScopeAuthorizer) Authorize(_ context.Context, _ tenancy.Context, scope string) error {
	if scope == d.scope {
		return authz.ErrForbidden
	}
	return nil
}

func TestActivityAuthzWriteScopes(t *testing.T) {
	env := newActivityServer(t, 0, nil, func(c *Config) {
		c.Authz = denyScopeAuthorizer{scope: authz.ScopeRunsWrite}
	})

	// Reads carry sessions:read — they pass and reach the handler.
	status, body := activityDo(t, http.MethodGet, env.url("/api/sessions/s1/activity"))
	if status != http.StatusOK {
		t.Fatalf("GET activity = %d %s, want 200 (read scope granted)", status, body)
	}
	status, body = activityDo(t, http.MethodGet, env.url("/api/sessions/s1/background/shells/1/output"))
	if status == http.StatusForbidden {
		t.Fatalf("shell output read wrongly forbidden: %s", body)
	}
	if status != http.StatusNotFound {
		t.Fatalf("shell output read = %d %s, want 404 (passed the scope gate)", status, body)
	}

	// Writes carry runs:write — denied before the handler runs.
	status, body = activityDo(t, http.MethodPost, env.url("/api/sessions/s1/background/shells/1/kill"))
	if status != http.StatusForbidden {
		t.Fatalf("kill = %d %s, want 403", status, body)
	}
	wantCode(t, body, "S-B-0003")
	status, body = activityDo(t, http.MethodPost, env.url("/api/sessions/s1/background/tasks/task-1/stop"))
	if status != http.StatusForbidden {
		t.Fatalf("task stop = %d %s, want 403", status, body)
	}
	wantCode(t, body, "S-B-0003")
	status, body = activityDo(t, http.MethodDelete, env.url("/api/sessions/s1/background/crons/cron-1"))
	if status != http.StatusForbidden {
		t.Fatalf("cron delete = %d %s, want 403", status, body)
	}
	wantCode(t, body, "S-B-0003")
}

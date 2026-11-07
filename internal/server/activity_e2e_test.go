package server

// TestActivityEndToEnd (Task 13, PM T1): one chained scenario over the real
// HTTP surface that ties the whole panel story together. A scripted model
// run in session A arms todos, a background shell and a cron; an idle
// session B sees none of it and cannot touch A's items (404 S-B-0004); the
// global stream signals each mutation in order; a REST kill is a quiet user
// stop (D1: never wakes, notification reaches the model on A's next user
// run); deleting A releases its background work and still never wakes it.
// The deeper per-facet contracts (snapshot field shapes, tree termination,
// wake caps, restart restore) are pinned by the focused tests in
// activityapi_test.go, wake_test.go and sessionlifecycle_test.go; every
// wait here is deadline-bounded.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
)

// createIdleSession creates a real session through the API and returns its id.
func createIdleSession(t *testing.T, env *activityEnv) string {
	t.Helper()
	resp, err := http.Post(env.url("/api/sessions"), "application/json",
		strings.NewReader(`{"workspace":""}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/sessions = %d, want 200", resp.StatusCode)
	}
	var created struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	if created.SessionID == "" {
		t.Fatal("created session has no id")
	}
	return created.SessionID
}

func TestActivityEndToEnd(t *testing.T) {
	// A known SessionDir so the todo sidecar's lifecycle is observable.
	dir := t.TempDir()
	arm := messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "TodoWrite", Args: map[string]any{
			"todos": []map[string]any{
				{"content": "Arm the panel demo", "status": "completed"},
				{"content": "Watch the background shell", "status": "in_progress"},
			},
		}},
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "Bash", Args: map[string]any{
			"command": "sleep 30", "run_in_background": true}},
		{ID: fmt.Sprintf("act-%d", wakeToolSeq.Add(1)), Name: "ScheduleCron", Args: map[string]any{
			"name": "nightly-check", "prompt": "Run the test suite", "cron": "0 2 * * *"}},
	}}
	env := newActivityServer(t, 0, []messages.Message{
		arm,
		messages.AI("armed"),
		messages.AI("noted"),
	}, func(c *Config) { c.SessionDir = dir })
	// No settings store: auto-wake keeps its design default (on), so the
	// D1 assertions below hold under the default posture, not a disabled one.

	// The stream is live-only: subscribe before the run that mutates.
	frames, stopFrames := subscribeGlobalEvents(t, env.ts.URL)
	defer stopFrames()
	expectChange := func(kind, sid string) {
		t.Helper()
		f := awaitEvent(t, frames, EventActivityChanged)
		var data map[string]any
		if err := json.Unmarshal([]byte(f.data), &data); err != nil {
			t.Fatalf("bad activity_changed data %q: %v", f.data, err)
		}
		if data["kind"] != kind || data["session_id"] != sid {
			t.Fatalf("activity_changed = %q, want kind=%s session=%s", f.data, kind, sid)
		}
	}

	// --- Session A arms the full surface through a real model run. ---
	runID, sidA, _ := postRun(t, env.ts.URL, "sess-a", "arm everything")
	waitRunDoneInProc(t, env.s, runID)

	// Every mutation signals on the global stream. The tool round runs the
	// calls concurrently, so the three frames' order is not a contract —
	// assert the exact multiset for this session (signal-only, one frame
	// per mutation, nothing else interleaved).
	seen := map[string]int{}
	for range 3 {
		f := awaitEvent(t, frames, EventActivityChanged)
		var data map[string]any
		if err := json.Unmarshal([]byte(f.data), &data); err != nil {
			t.Fatalf("bad activity_changed data %q: %v", f.data, err)
		}
		if data["session_id"] != sidA {
			t.Fatalf("activity_changed = %q, want session %s", f.data, sidA)
		}
		kind, _ := data["kind"].(string)
		seen[kind]++
	}
	want := map[string]int{"todos": 1, "shells": 1, "crons": 1}
	if fmt.Sprint(seen) != fmt.Sprint(want) {
		t.Fatalf("activity_changed kinds = %v, want %v", seen, want)
	}

	// The todo sidecar landed next to the transcript (restart-restore input).
	if _, err := os.Stat(filepath.Join(dir, sidA+".todos.json")); err != nil {
		t.Fatalf("todo sidecar after arming: %v", err)
	}

	snap := waitForActivity(t, env, sidA, "armed snapshot", func(m map[string]any) bool {
		shells := shellEntries(t, m)
		return len(shells) == 1 && shells[0]["status"] == "running" &&
			len(cronEntries(t, m)) == 1
	})
	shellID := shellEntries(t, snap)[0]["id"].(string)

	// --- Session B: idle, isolated, and unable to act on A's items. ---
	sidB := createIdleSession(t, env)
	snapB := activityJSON(t, env, sidB)
	if sh := shellEntries(t, snapB); len(sh) != 0 {
		t.Fatalf("B sees A's shells: %v", sh)
	}
	if cr := cronEntries(t, snapB); len(cr) != 0 {
		t.Fatalf("B sees A's crons: %v", cr)
	}
	status, body := activityDo(t, http.MethodPost,
		env.url("/api/sessions/%s/background/shells/%s/kill", sidB, shellID))
	if status != http.StatusNotFound {
		t.Fatalf("B killing A's shell = %d %s, want 404", status, body)
	}
	wantCode(t, body, "S-B-0004")
	// A's item is untouched by the foreign attempt.
	if sh := shellEntries(t, activityJSON(t, env, sidA)); len(sh) != 1 || sh[0]["status"] != "running" {
		t.Fatalf("A's shell changed after B's foreign kill: %v", sh)
	}

	// --- Kill through the panel's REST path: quiet user stop (D1). ---
	killStart := time.Now()
	status, body = activityDo(t, http.MethodPost,
		env.url("/api/sessions/%s/background/shells/%s/kill", sidA, shellID))
	if status != http.StatusOK {
		t.Fatalf("kill = %d %s, want 200", status, body)
	}
	var killResp struct {
		Shell map[string]any `json:"shell"`
	}
	if err := json.Unmarshal(body, &killResp); err != nil {
		t.Fatal(err)
	}
	if killResp.Shell["status"] != "stopped" || killResp.Shell["stopped_by"] != "user" {
		t.Fatalf("killed shell = %v, want stopped/user", killResp.Shell)
	}
	waitForActivity(t, env, sidA, "stopped by user", func(m map[string]any) bool {
		shells := shellEntries(t, m)
		return len(shells) == 1 && shells[0]["status"] == "stopped" &&
			shells[0]["stopped_by"] == "user" && shells[0]["ended_at"] != nil
	})
	if elapsed := time.Since(killStart); elapsed > 3*time.Second {
		t.Fatalf("stop settled in %v, want <= 3s (AC9)", elapsed)
	}
	expectChange("shells", sidA) // the exit signals too

	// The completion queued Quiet: a user stop never wakes the session.
	waitForQueueCount(t, env.a, sidA, 1)
	if n := env.a.WakeableCountFor(sidA); n != 0 {
		t.Fatalf("wakeable = %d, want 0 (user stop must not wake)", n)
	}
	time.Sleep(500 * time.Millisecond)
	if ids := registeredRuns(t, env.s); len(ids) != 1 {
		t.Fatalf("%d runs after the user stop, want 1 (no wake run)", len(ids))
	}

	// --- A's next USER run delivers the attributed notification. ---
	nextID, _, _ := postRun(t, env.ts.URL, sidA, "what happened to my shell")
	waitRunDoneInProc(t, env.s, nextID)
	waitForTranscript(t, env.ts.URL, sidA, "user-stop notification", func(msgs []messages.Message) bool {
		for _, m := range msgs {
			if m.Role == messages.RoleHuman &&
				strings.Contains(m.Content, "background-task-completed") &&
				strings.Contains(m.Content, "was killed by the user") {
				return true
			}
		}
		return false
	})
	if ids := registeredRuns(t, env.s); len(ids) != 2 {
		t.Fatalf("%d runs total, want exactly 2 (user + user, no wake)", len(ids))
	}

	// --- Delete releases everything and never wakes the session (D2). ---
	status, body = activityDo(t, http.MethodDelete, env.url("/api/sessions/%s", sidA))
	if status != http.StatusOK {
		t.Fatalf("delete = %d %s, want 200", status, body)
	}
	var rep sessionDeleteReport
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatal(err)
	}
	if !rep.Deleted || rep.CronsDeleted != 1 || !rep.TodosDropped {
		t.Fatalf("delete report = %+v, want crons_deleted=1 todos_dropped", rep)
	}
	if _, err := os.Stat(filepath.Join(dir, sidA+".todos.json")); !os.IsNotExist(err) {
		t.Fatalf("todo sidecar survived the delete: %v", err)
	}
	// Release stops RUNNING work and deletes crons outright; terminal shell
	// history (already stopped above) may remain, but nothing may run and
	// the schedule must be gone (a deleted session is never woken by it).
	snap = activityJSON(t, env, sidA)
	for _, sh := range shellEntries(t, snap) {
		if sh["status"] == "running" {
			t.Fatalf("running shell survived the delete: %v", sh)
		}
	}
	if cr := cronEntries(t, snap); len(cr) != 0 {
		t.Fatalf("deleted session still lists crons: %v", cr)
	}
	// The queued notification must not fire a run for the deleted session.
	time.Sleep(1 * time.Second)
	if ids := registeredRuns(t, env.s); len(ids) != 2 {
		t.Fatalf("%d runs after the delete, want 2 (no wake for a deleted session)", len(ids))
	}
}

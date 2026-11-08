package server

// Session lifecycle tests (Task 9, B9): the todo sidecar survives a server
// restart (N10), and deleting a session tears down its background work with
// a counted report and never wakes it again (N5, D2) — the release silences
// the session, the store tombstone keeps the wake driver from starting a run
// for it, and the wake bookkeeping maps are pruned with the session.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tools"
)

// steerCall scripts one AI turn with a single FOREGROUND Bash tool call —
// the tool round's runtime is what holds the run open (the mid-turn queue
// drain fires right after the tool's results).
func steerCall(t *testing.T, command string) messages.Message {
	t.Helper()
	return messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{{
		ID:   fmt.Sprintf("steer-%d", wakeToolSeq.Add(1)),
		Name: "Bash",
		Args: map[string]any{"command": command},
	}}}
}

// N10: a todo write in server 1 lands in a per-session sidecar; server 2,
// started over the same SessionDir, shows the list in its activity snapshot
// (live wins, else sidecar) and seeds the live store from the sidecar before
// the session's next run, so TodoWrite's merge baseline includes the
// pre-restart items.
func TestTodosSidecarSurvivesServerRestart(t *testing.T) {
	dir := t.TempDir()
	newEnv := func() *activityEnv {
		return newActivityServer(t, 0, []messages.Message{
			actCall("TodoWrite", map[string]any{"todos": []map[string]any{
				{"content": "Ship the sidecar", "status": "pending"},
			}, "merge": true}),
			messages.AI("merged"),
		}, func(c *Config) { c.SessionDir = dir })
	}
	sid := "s1"

	// Server 1: the observer writes the sidecar synchronously with the
	// change (the goal sidecar's contract), next to the transcript.
	env1 := newEnv()
	env1.a.SetTodosFor(sid, []tools.Todo{
		{Content: "Add activity API", Status: "in_progress", ActiveForm: "Adding activity API"},
		{Content: "Write tests", Status: "pending"},
	})
	sidecar := filepath.Join(dir, sid+".todos.json")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("todo sidecar not written: %v", err)
	}

	// Server 2 over the same SessionDir: the fresh process has no live list
	// (TodosKnown=false), so the snapshot reads the sidecar.
	env2 := newEnv()
	snap := activityJSON(t, env2, sid)
	todos, _ := snap["todos"].(map[string]any)
	if todos == nil {
		t.Fatalf("snapshot has no todos block: %v", snap)
	}
	if _, hasTS := todos["updated_at"].(string); !hasTS {
		t.Fatalf("todos.updated_at = %v, want the sidecar timestamp", todos["updated_at"])
	}
	items, _ := todos["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("restored todo items = %v, want the 2 sidecar items", items)
	}

	// The next run restores the sidecar BEFORE it starts, so the scripted
	// TodoWrite(merge) appends to the restored list, not to an empty one.
	runID, _, _ := postRun(t, env2.ts.URL, sid, "add one more")
	waitRunDoneInProc(t, env2.s, runID)
	live, _, ok := env2.a.TodosFor(sid)
	if !ok || len(live) != 3 {
		t.Fatalf("post-run todos = %v (ok=%v), want 3 items (2 restored + 1 merged)", live, ok)
	}
	if live[2].Content != "Ship the sidecar" {
		t.Fatalf("merged item = %+v, want it appended after the restored items", live[2])
	}
}

// N5/D2: deleting a session with running background items stops them (by
// user), reports the counts in the response, drops the todo sidecar with the
// transcript, prunes the wake driver's bookkeeping — and no run ever starts
// for the session again, even when a completion arrives after the delete.
func TestDeleteSessionStopsBackgroundWorkAndNeverWakes(t *testing.T) {
	env := newActivityServer(t, 0, []messages.Message{messages.AI("ok"), messages.AI("noted")})
	sid := "s1"
	env.a.SetTodosFor(sid, []tools.Todo{{Content: "ship it", Status: "in_progress"}})
	shellID := startSessionShell(t, env.reg, sid, "echo delete-me && sleep 30")
	// Pretend the wake driver tracked this session; delete must prune it.
	env.s.wakeMu.Lock()
	env.s.wakeChain[sid] = 3
	env.s.wakeStalled[sid] = true
	env.s.wakeMu.Unlock()

	status, body := activityDo(t, http.MethodDelete, env.url("/api/sessions/%s", sid))
	if status != http.StatusOK {
		t.Fatalf("DELETE session: status %d body %s", status, body)
	}
	var rep struct {
		Deleted      bool `json:"deleted"`
		ShellsKilled int  `json:"shells_killed"`
		TasksStopped int  `json:"tasks_stopped"`
		CronsDeleted int  `json:"crons_deleted"`
		TodosDropped bool `json:"todos_dropped"`
	}
	if err := json.Unmarshal(body, &rep); err != nil {
		t.Fatalf("bad DELETE body %s: %v", body, err)
	}
	if !rep.Deleted || rep.ShellsKilled != 1 || rep.TasksStopped != 0 || rep.CronsDeleted != 0 || !rep.TodosDropped {
		t.Fatalf("DELETE report = %+v, want deleted, 1 shell, 0 tasks, 0 crons, todos dropped", rep)
	}

	// The killed shell's record went with the session (F4: ReleaseSession
	// removes the session's records entirely): it killed with user
	// attribution — the report above counted it — then became unobservable,
	// settling invisibly like the kill endpoint's async teardown.
	for _, s := range env.reg.Background().ListFor(sid) {
		if s.ID == shellID {
			t.Fatalf("deleted session's shell record survived: %+v", s)
		}
	}
	if _, _, err := env.reg.Background().OutputFor(sid, shellID); !errors.Is(err, tools.ErrShellNotFound) {
		t.Fatalf("deleted session's shell still resolvable: %v", err)
	}

	// The todo sidecar and the live list died with the session.
	if _, err := os.Stat(filepath.Join(env.s.sessions.dir, sid+".todos.json")); !os.IsNotExist(err) {
		t.Fatal("todo sidecar survived the delete")
	}
	if _, _, ok := env.a.TodosFor(sid); ok {
		t.Fatal("live todo list survived the delete")
	}

	// Wake bookkeeping pruned with the session.
	env.s.wakeMu.Lock()
	_, chain := env.s.wakeChain[sid]
	_, stalled := env.s.wakeStalled[sid]
	env.s.wakeMu.Unlock()
	if chain || stalled {
		t.Fatalf("wake maps not pruned on delete: chain=%v stalled=%v", chain, stalled)
	}

	// A completion arriving after the delete is refused outright (the
	// released session): nothing queues, so nothing can ever wake it.
	env.a.QueueNotificationFor(sid, "background-task-completed",
		"Background shell 1 finished (exit code 0): echo delete-me\nOutput (tail):\ndelete-me")
	if n := env.a.QueuedInputCountFor(sid); n != 0 {
		t.Fatalf("queued count = %d, want 0 (released session refuses completions)", n)
	}

	// AC17: within the settle window no run appears for the session — not
	// from the killed shell's completion, not from anything else.
	time.Sleep(1 * time.Second)
	if ids := registeredRuns(t, env.s); len(ids) != 0 {
		t.Fatalf("%d runs started for a deleted session: %v", len(ids), ids)
	}
}

// Tombstone guard: wakeLocked on a deleted session (a delete racing a run
// still in flight can reach it without the agent-side release) drains and
// discards whatever is queued instead of starting a run.
func TestWakeSkipsDeletedSession(t *testing.T) {
	a := newWakeAgent(t, 0, messages.AI("ok"))
	s, _ := newWakeServer(t, a, nil)
	sid := "s1"
	a.QueueNotificationFor(sid, "background-task-completed",
		"Background shell 1 finished (exit code 0): echo gone\nOutput (tail):\ngone")
	s.sessions.delete(t.Context(), sid)
	s.wakeLocked(sid)
	if ids := registeredRuns(t, s); len(ids) != 0 {
		t.Fatalf("wake started %d runs for a deleted session: %v", len(ids), ids)
	}
	if n := a.QueuedInputCountFor(sid); n != 0 {
		t.Fatalf("queued = %d, want 0 (drained and discarded)", n)
	}
}

// Store-level sidecar edge cases: roundtrip, cleared-to-empty, absent and
// corrupt sidecars, mem-only stores, broken roots, and the no-resurrect rule
// for tombstoned sessions.
func TestSessionStoreTodosSidecar(t *testing.T) {
	dir := t.TempDir()
	st := newSessionStore(dir)
	at := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	st.saveTodos("t1", []tools.Todo{{Content: "a", Status: "pending"}}, at)
	items, got, ok := st.loadTodos("t1")
	if !ok || len(items) != 1 || items[0].Content != "a" || !got.Equal(at) {
		t.Fatalf("loadTodos = %v %v %v, want the saved list and time", items, got, ok)
	}
	// A cleared-to-empty list persists as empty (ok=true), never absent.
	st.saveTodos("t1", nil, at)
	if items, _, ok := st.loadTodos("t1"); !ok || len(items) != 0 {
		t.Fatalf("cleared sidecar = %v (ok=%v), want an empty list with ok=true", items, ok)
	}
	if _, _, ok := st.loadTodos("missing"); ok {
		t.Fatal("missing sidecar should load ok=false")
	}
	// A corrupt sidecar degrades to absent.
	if err := os.WriteFile(filepath.Join(dir, "t2.todos.json"), []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := st.loadTodos("t2"); ok {
		t.Fatal("corrupt sidecar should load ok=false")
	}
	// delete removes the sidecar and tombstones the id; a save racing the
	// delete cannot resurrect it, and isDeleted answers the wake guard.
	st.saveTodos("t3", []tools.Todo{{Content: "x", Status: "pending"}}, at)
	st.delete(t.Context(), "t3")
	if _, err := os.Stat(filepath.Join(dir, "t3.todos.json")); !os.IsNotExist(err) {
		t.Fatal("delete should remove the todo sidecar")
	}
	if !st.isDeleted("t3") {
		t.Fatal("isDeleted should report the tombstone")
	}
	if st.isDeleted("alive") {
		t.Fatal("isDeleted should not report a live session")
	}
	st.saveTodos("t3", []tools.Todo{{Content: "zombie", Status: "pending"}}, at)
	if _, err := os.Stat(filepath.Join(dir, "t3.todos.json")); !os.IsNotExist(err) {
		t.Fatal("a tombstoned session's sidecar must not resurrect")
	}
	// Mem-only stores never touch disk; a broken root warns instead of
	// panicking or silently returning like saveGoal.
	mem := newSessionStore("")
	mem.saveTodos("m", []tools.Todo{{Content: "m", Status: "pending"}}, at)
	if _, _, ok := mem.loadTodos("m"); ok {
		t.Fatal("mem-only store should not persist todos")
	}
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := newSessionStore(filepath.Join(blocker, "sessions"))
	bad.saveTodos("b", []tools.Todo{{Content: "b", Status: "pending"}}, at)
	if _, _, ok := bad.loadTodos("b"); ok {
		t.Fatal("a failed write must not read back as persisted")
	}
}

// Root-fix regression (review round 1): a steer posted to one session's
// in-flight run rides only that session's runs. The unscoped form was
// visibility-drained by ANY session's tool round — here A's — leaking B's
// user input into A's transcript and starving B's run of it.
func TestSteerScopedToItsSessionRun(t *testing.T) {
	a := newWakeAgent(t, 0,
		steerCall(t, "sleep 1"), // B round 1: hold B inside its tool round
		steerCall(t, "echo a"),  // A round 1: fast tool, so A's drain runs early
		messages.AI("a done"),   // A round 2
		messages.AI("b done"),   // B round 2
	)
	s, ts := newWakeServer(t, a, nil)

	// B starts first and is inside its 1s tool round (its first model call
	// consumed response 0 within microseconds of the POST).
	bRun, bSid, _ := postRun(t, ts.URL, "s-b", "b work")
	time.Sleep(150 * time.Millisecond)

	// The user steers B's in-flight run through the HTTP endpoint.
	resp, err := http.Post(ts.URL+"/api/runs/"+bRun+"/input", "application/json",
		strings.NewReader(`{"text":"steer: use port 8081"}`))
	if err != nil {
		t.Fatal(err)
	}
	var queued struct {
		OK     bool `json:"ok"`
		Queued int  `json:"queued"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&queued); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if !queued.OK || queued.Queued != 1 {
		t.Fatalf("steer response = %+v, want ok queued 1", queued)
	}

	// A's run executes a tool round while the steer is queued; its drain
	// must not take B's item.
	aRun, aSid, _ := postRun(t, ts.URL, "s-a", "a work")
	waitRunDoneInProc(t, s, aRun)
	waitRunDoneInProc(t, s, bRun)

	for _, m := range sessionMessages(t, ts.URL, aSid) {
		if strings.Contains(m.Content, "port 8081") || strings.Contains(m.Content, "queued-user-input") {
			t.Fatalf("B's steer leaked into session %s's transcript: %q", aSid, m.Content)
		}
	}
	found := false
	for _, m := range sessionMessages(t, ts.URL, bSid) {
		if m.Role == messages.RoleHuman && strings.Contains(m.Content, "port 8081") {
			found = true
		}
	}
	if !found {
		t.Fatalf("B's steer was never delivered to B's run (transcript: %+v)", sessionMessages(t, ts.URL, bSid))
	}
	if n := a.QueuedInputCountFor(bSid); n != 0 {
		t.Fatalf("B queue after delivery = %d, want 0", n)
	}
}

// Root-fix + defense regression (review round 1): deleting session A while
// A's run is in flight must discard ONLY A's queued items. B's steer (HTTP,
// for B's in-flight run) and B's background notification survive A's
// tombstone wake discard and the post-release re-check, and are delivered
// to B's run — not to A's, not dropped.
func TestDeleteSessionKeepsOtherSessionsQueuedInput(t *testing.T) {
	a := newWakeAgent(t, 0,
		steerCall(t, "sleep 2"),   // B round 1: hold B inside its tool round
		steerCall(t, "sleep 0.4"), // A round 1: hold A in flight across the delete
		messages.AI("a done"),     // A round 2
		messages.AI("b done"),     // B round 2
	)
	s, ts := newWakeServer(t, a, nil)

	// B starts first; at +150ms it is inside its 2s tool round.
	bRun, bSid, _ := postRun(t, ts.URL, "s-b", "b work")
	time.Sleep(150 * time.Millisecond)

	// The user steers B's in-flight run, and a B background completion
	// queues — both while A's queue-discard paths are about to run.
	resp, err := http.Post(ts.URL+"/api/runs/"+bRun+"/input", "application/json",
		strings.NewReader(`{"text":"steer: use port 8081"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	a.QueueNotificationFor(bSid, "background-task-completed",
		"Background shell 7 finished (exit code 0): echo b-bg\nOutput (tail):\nb-bg")

	// A's run starts (its 0.4s tool round holds it in flight) and the
	// session is deleted mid-flight.
	aRun, _, _ := postRun(t, ts.URL, "s-a", "a work")
	time.Sleep(100 * time.Millisecond)
	status, body := activityDo(t, http.MethodDelete, ts.URL+"/api/sessions/s-a")
	if status != http.StatusOK {
		t.Fatalf("DELETE session: status %d body %s", status, body)
	}

	// A finishes clean: wakeAfterRun(A) hits the tombstone branch and the
	// R4 re-check calls maybeWake(A) — both may discard; both must touch
	// only A-scoped items.
	waitRunDoneInProc(t, s, aRun)
	waitRunDoneInProc(t, s, bRun)

	// B's run delivered both items to B.
	var sawSteer, sawNote bool
	for _, m := range sessionMessages(t, ts.URL, bSid) {
		if m.Role != messages.RoleHuman {
			continue
		}
		sawSteer = sawSteer || strings.Contains(m.Content, "port 8081")
		sawNote = sawNote || strings.Contains(m.Content, "b-bg")
	}
	if !sawSteer || !sawNote {
		t.Fatalf("B's steer=%v notification=%v after A's delete+finish; both must reach B (transcript: %+v)",
			sawSteer, sawNote, sessionMessages(t, ts.URL, bSid))
	}
	// Nothing woke the deleted session, and B's queue is fully drained.
	if ids := registeredRuns(t, s); len(ids) != 2 {
		t.Fatalf("%d runs total, want exactly 2 (the two user runs; no wake for the deleted session): %v", len(ids), ids)
	}
	if n := a.QueuedInputCountFor(bSid); n != 0 {
		t.Fatalf("B queue after delivery = %d, want 0", n)
	}
}

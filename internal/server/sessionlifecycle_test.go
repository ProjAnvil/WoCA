package server

// Session lifecycle tests (Task 9, B9): the todo sidecar survives a server
// restart (N10), and deleting a session tears down its background work with
// a counted report and never wakes it again (N5, D2) — the release silences
// the session, the store tombstone keeps the wake driver from starting a run
// for it, and the wake bookkeeping maps are pruned with the session.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tools"
)

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

	// The shell was stopped by the user (kill attribution records before the
	// signal lands; the exit settles asynchronously).
	deadline := time.Now().Add(5 * time.Second)
	for {
		var st tools.BackgroundShellStatus
		for _, s := range env.reg.Background().ListFor(sid) {
			if s.ID == shellID {
				st = s
			}
		}
		if st.Killed && st.StoppedBy == tools.StopByUser && !st.Running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell never settled as user-stopped: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
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

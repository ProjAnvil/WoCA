package server

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/session"
)

func TestSessionStoreLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	st := newSessionStore(dir)
	if err := st.save(t.Context(), "s1", []messages.Message{messages.Human("hi")}, ""); err != nil {
		t.Fatal(err)
	}
	// A fresh store (server restart) resolves the transcript from disk and
	// caches it.
	st2 := newSessionStore(dir)
	got := st2.load(t.Context(), "s1")
	if len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("disk load = %+v", got)
	}
	// Missing sessions and unpersisted stores return nil.
	if st2.load(t.Context(), "missing") != nil {
		t.Fatal("missing session should load nil")
	}
	memOnly := newSessionStore("")
	if memOnly.load(t.Context(), "s1") != nil {
		t.Fatal("mem-only store should not resolve unknown ids")
	}
}

func TestSessionStoreSaveErrors(t *testing.T) {
	// Persistence disabled: saves stay in memory and never error.
	mem := newSessionStore("")
	if err := mem.save(t.Context(), "a", []messages.Message{messages.Human("x")}, ""); err != nil {
		t.Fatal(err)
	}
	// A session dir whose parent is a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := newSessionStore(filepath.Join(blocker, "sessions"))
	if err := bad.save(t.Context(), "a", []messages.Message{messages.Human("x")}, ""); err == nil {
		t.Fatal("save under a file parent should error")
	}
}

func TestSessionStoreIndexVariants(t *testing.T) {
	// Mem-only stores have an empty index.
	if idx := newSessionStore("").loadIndex(); len(idx) != 0 {
		t.Fatalf("mem-only index = %+v", idx)
	}

	dir := t.TempDir()
	st := newSessionStore(dir)
	// A corrupt index degrades to empty rather than erroring.
	if err := os.WriteFile(filepath.Join(dir, "index.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if idx := st.loadIndex(); len(idx) != 0 {
		t.Fatalf("corrupt index = %+v, want empty", idx)
	}

	// Recording metadata for a session that already has workspace metadata
	// but whose new entry carries none keeps the previous workspace.
	// (recordMeta locks internally — no caller-side lock.)
	if err := st.save(t.Context(), "s", []messages.Message{messages.Human("x")}, "/ws/one"); err != nil {
		t.Fatal(err)
	}
	st.recordMeta(sessionMeta{ID: "s"})
	idx := st.loadIndex()
	if idx["s"].Workspace != "/ws/one" || idx["s"].WorkspaceName != "one" {
		t.Fatalf("workspace not preserved across metadata-only updates: %+v", idx["s"])
	}

	// delete() drops the index entry too.
	st.delete(t.Context(), "s")
	if _, ok := st.loadIndex()["s"]; ok {
		t.Fatal("index entry survived delete")
	}
}

func TestSessionStoreDeleteVariants(t *testing.T) {
	// Mem-only delete: no disk work, but the tombstone still applies.
	mem := newSessionStore("")
	if err := mem.save(t.Context(), "a", []messages.Message{messages.Human("x")}, ""); err != nil {
		t.Fatal(err)
	}
	mem.delete(t.Context(), "a")
	if mem.load(t.Context(), "a") != nil {
		t.Fatal("mem delete should drop the transcript")
	}

	// Index writes onto an unusable dir are best-effort (no panic, no error).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := newSessionStore(filepath.Join(blocker, "sessions"))
	bad.recordMeta(sessionMeta{ID: "x", Workspace: "/w"})
}

func TestWorkspaceDisplayNameEdges(t *testing.T) {
	cases := map[string]string{
		"":             "Uncategorized workspace",
		"   ":          "Uncategorized workspace",
		"/":            "/",
		".":            ".",
		"/a/b/project": "project",
	}
	for in, want := range cases {
		if got := workspaceDisplayName(in); got != want {
			t.Errorf("workspaceDisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSessionStoreGoalSidecar(t *testing.T) {
	dir := t.TempDir()
	st := newSessionStore(dir)
	st.saveGoal("g1", &agent.Goal{Objective: "obj", Status: agent.GoalActive})

	g := st.loadGoal("g1")
	if g == nil || g.Objective != "obj" {
		t.Fatalf("loadGoal = %+v", g)
	}
	if st.loadGoal("missing") != nil {
		t.Fatal("missing sidecar should load nil")
	}

	// A corrupt sidecar degrades to nil.
	corrupt := filepath.Join(dir, "g2.goal.json")
	if err := os.WriteFile(corrupt, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st.loadGoal("g2") != nil {
		t.Fatal("corrupt sidecar should load nil")
	}

	// nil deletes the sidecar; a mem-only store is a no-op.
	st.saveGoal("g1", nil)
	if _, err := os.Stat(filepath.Join(dir, "g1.goal.json")); !os.IsNotExist(err) {
		t.Fatal("saveGoal(nil) should remove the sidecar")
	}
	mem := newSessionStore("")
	mem.saveGoal("any", &agent.Goal{Objective: "x"})
	if mem.loadGoal("any") != nil {
		t.Fatal("mem-only store should not persist goals")
	}

	// Write failures (dir under a file parent) are swallowed.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := newSessionStore(filepath.Join(blocker, "sessions"))
	bad.saveGoal("x", &agent.Goal{Objective: "y"})
}

func TestSessionStoreListVariants(t *testing.T) {
	// A nonexistent dir lists nothing (restart before first save).
	missing := newSessionStore(filepath.Join(t.TempDir(), "nope"))
	if list, err := missing.list(); err != nil || len(list) != 0 {
		t.Fatalf("list(missing dir) = %v, %v", list, err)
	}

	// list() is index-only, so a dir that is a regular file no longer
	// errors — it lists the (empty) in-memory index.
	file := t.TempDir() + "/notadir"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if list, err := newSessionStore(file).list(); err != nil || len(list) != 0 {
		t.Fatalf("list(file) = %v, %v", list, err)
	}

	dir := t.TempDir()
	st := newSessionStore(dir)
	// A corrupt transcript is never reconciled into the index, so it is
	// skipped rather than failing the listing.
	if err := os.WriteFile(filepath.Join(dir, "corrupt.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A transcript with no human message falls back to the default title.
	if err := st.save(t.Context(), "only-ai", []messages.Message{messages.AI("hi")}, "/ws/one"); err != nil {
		t.Fatal(err)
	}
	// A long first human message truncates for the title.
	long := strings.Repeat("あいうえおかき", 12) // 84 runes > 40
	if err := st.save(t.Context(), "long-title", []messages.Message{messages.Human(long)}, "/ws/one"); err != nil {
		t.Fatal(err)
	}

	list, err := st.list()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]sessionSummary{}
	for _, s := range list {
		byID[s.ID] = s
	}
	if _, ok := byID["corrupt"]; ok {
		t.Fatal("corrupt transcript should be skipped")
	}
	if got := byID["only-ai"].Title; got != "New session" {
		t.Fatalf("AI-only title = %q, want New session", got)
	}
	want := string([]rune(strings.TrimSpace(long))[:40]) + "…"
	if got := byID["long-title"].Title; got != want {
		t.Fatalf("long title = %q, want %q", got, want)
	}
}

// list() reads only the in-memory index: sessions still list after their
// on-disk .jsonl is deleted (proving zero JSONL reads).
func TestSessionStoreListReadsIndexOnly(t *testing.T) {
	dir := t.TempDir()
	s := newSessionStore(dir)
	msgs := []messages.Message{
		{Role: messages.RoleHuman, Content: "hello world"},
		{Role: messages.RoleAI, Content: "hi"},
	}
	if err := s.save(t.Context(), "sess-a", msgs, "/tmp/ws-a"); err != nil {
		t.Fatal(err)
	}
	// Delete the transcript file: the old list() implementation would drop
	// this session; the new one is unaffected.
	files, _ := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	for _, f := range files {
		_ = os.Remove(f)
	}
	list, err := s.list()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v; want 1 summary from index", list, err)
	}
	if list[0].MessageCount != 2 || list[0].Title != "hello world" {
		t.Fatalf("summary = %+v, want MessageCount=2 Title=hello world", list[0])
	}
}

// Concurrent recordMeta calls (via save) must not interleave or race
// (-race guards this).
func TestSessionStoreConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	s := newSessionStore(dir)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			id := fmt.Sprintf("sess-%d", i%3)
			_ = s.save(t.Context(), id, []messages.Message{{Role: messages.RoleHuman, Content: "x"}}, "/tmp/ws")
		})
	}
	wg.Wait()
	list, err := s.list()
	if err != nil || len(list) != 3 {
		t.Fatalf("list = %v, %v; want 3 sessions", list, err)
	}
}

// Startup reconciliation: a leftover .jsonl in the directory that is not in
// the index should be absorbed when a new store starts.
func TestSessionStoreStartupReconcile(t *testing.T) {
	dir := t.TempDir()
	legacy := []messages.Message{{Role: messages.RoleHuman, Content: "legacy chat"}}
	if err := session.Save(filepath.Join(dir, "sess-legacy.jsonl"), legacy); err != nil {
		t.Fatal(err)
	}
	s := newSessionStore(dir)
	list, err := s.list()
	if err != nil || len(list) != 1 || list[0].ID != "sess-legacy" {
		t.Fatalf("list = %v, %v; want reconciled legacy session", list, err)
	}
	if list[0].Title != "legacy chat" || list[0].MessageCount != 1 {
		t.Fatalf("summary = %+v", list[0])
	}
}

// Startup reconciliation prunes zombie entries: a crash window can leave
// index.json holding an entry whose transcript was deleted (delete's flush
// never reached disk), and list would show a session whose load is nil.
func TestSessionStoreStartupReconcilePrunesZombies(t *testing.T) {
	dir := t.TempDir()
	s := newSessionStore(dir)
	if err := s.save(t.Context(), "keep", []messages.Message{{Role: messages.RoleHuman, Content: "alive"}}, "/ws"); err != nil {
		t.Fatal(err)
	}
	if err := s.save(t.Context(), "zombie", []messages.Message{{Role: messages.RoleHuman, Content: "dead"}}, "/ws"); err != nil {
		t.Fatal(err)
	}
	// Simulate the crash window: index.json already records both sessions,
	// but the zombie's transcript is lost.
	if err := os.Remove(filepath.Join(dir, "zombie.jsonl")); err != nil {
		t.Fatal(err)
	}
	s2 := newSessionStore(dir)
	list, err := s2.list()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "keep" {
		t.Fatalf("list = %+v, want only the session with a live transcript", list)
	}
	if s2.load(t.Context(), "zombie") != nil {
		t.Fatal("zombie session should not resolve")
	}
	// The index is cleaned up (written back to disk).
	idx := s2.loadIndex()
	if _, ok := idx["zombie"]; ok {
		t.Fatal("zombie entry survived in index.json")
	}
	if _, ok := idx["keep"]; !ok {
		t.Fatal("live entry dropped from index.json")
	}
}

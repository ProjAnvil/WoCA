package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
)

func TestSessionStoreLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	st := newSessionStore(dir)
	if err := st.save("s1", []messages.Message{messages.Human("hi")}, ""); err != nil {
		t.Fatal(err)
	}
	// A fresh store (server restart) resolves the transcript from disk and
	// caches it.
	st2 := newSessionStore(dir)
	got := st2.load("s1")
	if len(got) != 1 || got[0].Content != "hi" {
		t.Fatalf("disk load = %+v", got)
	}
	// Missing sessions and unpersisted stores return nil.
	if st2.load("missing") != nil {
		t.Fatal("missing session should load nil")
	}
	memOnly := newSessionStore("")
	if memOnly.load("s1") != nil {
		t.Fatal("mem-only store should not resolve unknown ids")
	}
}

func TestSessionStoreSaveErrors(t *testing.T) {
	// Persistence disabled: saves stay in memory and never error.
	mem := newSessionStore("")
	if err := mem.save("a", []messages.Message{messages.Human("x")}, ""); err != nil {
		t.Fatal(err)
	}
	// A session dir whose parent is a regular file cannot be created.
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := newSessionStore(filepath.Join(blocker, "sessions"))
	if err := bad.save("a", []messages.Message{messages.Human("x")}, ""); err == nil {
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
	if err := st.save("s", []messages.Message{messages.Human("x")}, "/ws/one"); err != nil {
		t.Fatal(err)
	}
	st.mu.Lock()
	st.recordMeta(sessionMeta{ID: "s"})
	st.mu.Unlock()
	idx := st.loadIndex()
	if idx["s"].Workspace != "/ws/one" || idx["s"].WorkspaceName != "one" {
		t.Fatalf("workspace not preserved across metadata-only updates: %+v", idx["s"])
	}

	// delete() drops the index entry too.
	st.delete("s")
	if _, ok := st.loadIndex()["s"]; ok {
		t.Fatal("index entry survived delete")
	}
}

func TestSessionStoreDeleteVariants(t *testing.T) {
	// Mem-only delete: no disk work, but the tombstone still applies.
	mem := newSessionStore("")
	if err := mem.save("a", []messages.Message{messages.Human("x")}, ""); err != nil {
		t.Fatal(err)
	}
	mem.delete("a")
	if mem.load("a") != nil {
		t.Fatal("mem delete should drop the transcript")
	}

	// Index writes onto an unusable dir are best-effort (no panic, no error).
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := newSessionStore(filepath.Join(blocker, "sessions"))
	bad.mu.Lock()
	bad.recordMeta(sessionMeta{ID: "x", Workspace: "/w"})
	bad.mu.Unlock()
}

func TestWorkspaceDisplayNameEdges(t *testing.T) {
	cases := map[string]string{
		"":             "未分类工作区",
		"   ":          "未分类工作区",
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
	if list, err := missing.list(); err != nil || list != nil {
		t.Fatalf("list(missing dir) = %v, %v", list, err)
	}

	// A dir that exists but is a regular file errors.
	file := t.TempDir() + "/notadir"
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := newSessionStore(file).list(); err == nil {
		t.Fatal("list(file) should error")
	}

	dir := t.TempDir()
	st := newSessionStore(dir)
	// A corrupt transcript is skipped rather than failing the listing.
	if err := os.WriteFile(filepath.Join(dir, "corrupt.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A transcript with no human message falls back to the default title.
	if err := st.save("only-ai", []messages.Message{messages.AI("hi")}, ""); err != nil {
		t.Fatal(err)
	}
	// A long first human message truncates for the title.
	long := strings.Repeat("很长的会话标题", 12) // 60 runes > 40
	if err := st.save("long-title", []messages.Message{messages.Human(long)}, ""); err != nil {
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
	if got := byID["only-ai"].Title; got != "新会话" {
		t.Fatalf("AI-only title = %q, want 新会话", got)
	}
	want := string([]rune(strings.TrimSpace(long))[:40]) + "…"
	if got := byID["long-title"].Title; got != want {
		t.Fatalf("long title = %q, want %q", got, want)
	}
}

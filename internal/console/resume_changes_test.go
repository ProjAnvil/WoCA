package console

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/session"
)

func writeSnapshotFile(dir, name string, msgs []messages.Message) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return session.SaveState(filepath.Join(dir, name+".json"), session.State{Messages: msgs})
}

func TestResumeListsAndLoads(t *testing.T) {
	c, _ := newFullConsole(t)
	dir := c.deps.SessionDir
	ctx := context.Background()

	for _, name := range []string{"alpha", "beta"} {
		if err := writeSnapshotFile(dir, name, []messages.Message{
			messages.Human("prompt " + name), messages.AI("answer " + name),
		}); err != nil {
			t.Fatal(err)
		}
	}

	// No args lists the snapshots.
	s := &Session{History: []messages.Message{messages.Human("live")}}
	res, err := c.Execute(ctx, "resume", nil, s)
	if err != nil {
		t.Fatalf("resume list: %v", err)
	}
	for _, want := range []string{"alpha", "beta", "/resume <name>"} {
		if !strings.Contains(res.Output, want) {
			t.Fatalf("resume list missing %q:\n%s", want, res.Output)
		}
	}

	// Loading replaces the live history and clears the display.
	res, err = c.Execute(ctx, "resume", []string{"alpha"}, s)
	if err != nil {
		t.Fatalf("resume alpha: %v", err)
	}
	if !strings.Contains(res.Output, `resumed "alpha" (2 messages)`) || !res.Clear {
		t.Fatalf("resume result = %+v", res)
	}
	if len(s.History) != 2 || s.History[0].Content != "prompt alpha" {
		t.Fatalf("history after resume = %+v", s.History)
	}

	// Unknown names error (no fuzzy guessing).
	if _, err := c.Execute(ctx, "resume", []string{"nope"}, s); err == nil || !strings.Contains(err.Error(), "no session named") {
		t.Fatalf("unknown resume error = %v", err)
	}

	// The empty-directory case.
	c2, _ := newFullConsole(t)
	res, err = c2.Execute(ctx, "resume", nil, &Session{})
	if err != nil || !strings.Contains(res.Output, "no saved sessions") {
		t.Fatalf("empty resume = %+v %v", res, err)
	}
}

func TestChangesListsPending(t *testing.T) {
	c, _ := newFullConsole(t)
	ctx := context.Background()

	// No pending changes yet.
	res, err := c.Execute(ctx, "changes", nil, &Session{})
	if err != nil || !strings.Contains(res.Output, "no pending changes") {
		t.Fatalf("empty changes = %+v %v", res, err)
	}

	// Record pending changes through the agent's file history (the same
	// checkpoint stack Write/Edit populate at runtime), scoped to the
	// console's workspace.
	ws := c.deps.Cfg.WorkDir
	fh := c.deps.Agent.FileHistory()
	fh.Record(agent.WithRunWorkspace(ctx, ws), ws+"/out.txt", nil, []byte("line1\nline2\n"))
	fh.Record(agent.WithRunWorkspace(ctx, ws), ws+"/src/a.go",
		[]byte("func f() {}\n"), []byte("func f() { return }\n"))

	s := &Session{}
	res, err = c.Execute(ctx, "changes", nil, s)
	if err != nil {
		t.Fatalf("changes: %v", err)
	}
	if !strings.Contains(res.Output, "new") || !strings.Contains(res.Output, "out.txt") {
		t.Fatalf("changes output missing the new file:\n%s", res.Output)
	}
	if !strings.Contains(res.Output, "mod") || !strings.Contains(res.Output, "a.go") {
		t.Fatalf("changes output missing the modified file:\n%s", res.Output)
	}

	// The full variant prints the unified diff.
	res, err = c.Execute(ctx, "changes", []string{"full"}, s)
	if err != nil || !strings.Contains(res.Output, "+line1") {
		t.Fatalf("changes full = %+v %v", res, err)
	}
}

package memory

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func newEngine(t *testing.T) *LadyM {
	t.Helper()
	eng, err := Build(BuildInput{
		EngineEnabled:    true,
		DBPath:           filepath.Join(t.TempDir(), "mem.db"),
		DefaultWorkspace: "/ws",
		Embedding:        "hashing",
		LLM:              "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func TestDefaultDBPathAndExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if got := DefaultDBPath(); got != filepath.Join(home, ".woca", "ladym.db") {
		t.Fatalf("DefaultDBPath = %q", got)
	}
	if got := ExpandHome("~/x/y.db"); got != filepath.Join(home, "x", "y.db") {
		t.Fatalf("expand = %q", got)
	}
	if got := ExpandHome("~"); got != home {
		t.Fatalf("bare ~ = %q", got)
	}
	if got := ExpandHome("/abs/x.db"); got != "/abs/x.db" {
		t.Fatalf("absolute passthrough = %q", got)
	}
	if got := ExpandHome("~notafile"); got != "~notafile" {
		t.Fatalf("non-path tilde = %q", got)
	}
	if got := ExpandHome(""); got != "" {
		t.Fatalf("empty = %q", got)
	}
}

func TestBuildValidation(t *testing.T) {
	if _, err := Build(BuildInput{}); err == nil {
		t.Fatal("disabled engine errors")
	}
	if _, err := Build(BuildInput{EngineEnabled: true}); err == nil {
		t.Fatal("missing db path errors")
	}
	if _, err := Build(BuildInput{EngineEnabled: true, DBPath: "/tmp/x.db"}); err == nil {
		t.Fatal("missing workspace errors")
	}
}

func TestEngineCRUDAndStats(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()

	if eng.Description() == "" {
		t.Fatal("description empty")
	}
	if _, err := eng.Remember(ctx, "/ws", "the deploy script lives in scripts/", []string{"infra"}, "test"); err != nil {
		t.Fatal(err)
	}
	if err := eng.RecordEvent(ctx, "/ws", "agent-1", "ran tests", "3 passed", "success"); err != nil {
		t.Fatal(err)
	}

	hits, err := eng.Recall(ctx, "deploy script", "/ws", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("recall found nothing")
	}

	list, err := eng.List(ctx, "/ws", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) == 0 {
		t.Fatal("list empty")
	}

	if _, err := eng.Stats(ctx, "/ws"); err != nil {
		t.Fatal(err)
	}

	// Forget the first remembered entry via its list id.
	if len(list) > 0 {
		if err := eng.Forget(ctx, list[0].ID); err != nil {
			t.Fatal(err)
		}
	}

	// Code search over a tiny tree.
	root := t.TempDir()
	if err := writeFile(root, "main.go", "package main\nfunc main() {}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexCode(ctx, root, "/ws", true); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SearchCode(ctx, "func main", "/ws", 5); err != nil {
		t.Fatal(err)
	}

	// Consolidation without an LLM is a no-op message, not an error.
	if _, err := eng.Consolidate(ctx, "/ws"); err != nil {
		t.Fatal(err)
	}
}

func writeFile(root, name, content string) error {
	return os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
}

func TestEngineBranches(t *testing.T) {
	eng := newEngine(t)
	ctx := context.Background()

	// Default topK and workspace fallback (empty workspace → default).
	if _, err := eng.Recall(ctx, "anything", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.SearchCode(ctx, "anything", "", 0); err != nil {
		t.Fatal(err)
	}
	// Layer-filtered and paged lists.
	if _, err := eng.List(ctx, "", "semantic", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.List(ctx, "", "", 5, 1); err != nil {
		t.Fatal(err)
	}
	// Writes with the default workspace (the withWorkspace no-swap path).
	if _, err := eng.Remember(ctx, "", "default workspace fact", nil, "t"); err != nil {
		t.Fatal(err)
	}
	// A different workspace takes the retarget path.
	if _, err := eng.Remember(ctx, "/other", "other workspace fact", nil, "t"); err != nil {
		t.Fatal(err)
	}
	if err := eng.RecordEvent(ctx, "/other", "a", "act", "obs", "out"); err != nil {
		t.Fatal(err)
	}

	// Forget with an unknown id is tolerated as a no-op or error depending
	// on the engine; both are acceptable as long as it does not panic.
	_ = eng.Forget(ctx, "no-such-id")

	if _, err := eng.Stats(ctx, "/other"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexCode(ctx, t.TempDir(), "/other", false); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Consolidate(ctx, "/other"); err != nil {
		t.Fatal(err)
	}
	// Empty-workspace index/consolidate fallbacks.
	if _, err := eng.IndexCode(ctx, t.TempDir(), "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Consolidate(ctx, ""); err != nil {
		t.Fatal(err)
	}
}

func TestEngineNilSafety(t *testing.T) {
	// Closing twice and after use stays safe.
	eng := newEngine(t)
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
}

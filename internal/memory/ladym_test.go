package memory

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testInput(t *testing.T) BuildInput {
	t.Helper()
	return BuildInput{
		EngineEnabled:    true,
		DBPath:           filepath.Join(t.TempDir(), "mem.db"),
		DefaultWorkspace: "/ws/test",
		Embedding:        "hashing",
		LLM:              "none",
	}
}

func TestBuildDisabled(t *testing.T) {
	in := testInput(t)
	in.EngineEnabled = false
	if _, err := Build(in); err == nil {
		t.Fatal("Build should fail when disabled")
	}
}

func TestRememberRecallListStatsForget(t *testing.T) {
	eng, err := Build(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()

	id, err := eng.Remember(ctx, "/ws/test", "auth uses JWT with 24h expiry", []string{"auth"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("Remember returned empty id")
	}
	hits, err := eng.Recall(ctx, "how does auth work", "/ws/test", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("Recall found nothing after Remember")
	}
	if hits[0].Layer == "" || hits[0].Type == "" {
		t.Fatalf("hit missing layer/type: %+v", hits[0])
	}
	other, err := eng.Recall(ctx, "how does auth work", "/ws/other", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Fatalf("other workspace should be empty, got %d", len(other))
	}
	items, err := eng.List(ctx, "/ws/test", "", 10, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("List = %v, %v", items, err)
	}
	st, err := eng.Stats(ctx, "/ws/test")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 || st.Layers["L2_semantic"] != 1 {
		t.Fatalf("Stats = %+v", st)
	}
	if err := eng.RecordEvent(ctx, "/ws/test", "woca", "run", "test", "success"); err != nil {
		t.Fatal(err)
	}
	if err := eng.Forget(ctx, id); err != nil {
		t.Fatal(err)
	}
	st2, _ := eng.Stats(ctx, "/ws/test")
	if st2.Total != 1 { // the episode remains
		t.Fatalf("after Forget: %+v", st2)
	}
}

func TestResolveEmbeddingInherit(t *testing.T) {
	p, m, u, k := resolveEmbedding(BuildInput{
		Embedding: "inherit", OpenAIKey: "sk-1", OpenAIBaseURL: "https://api.example.com",
	})
	if p != "openai" || m != "text-embedding-3-small" || u != "https://api.example.com" || k != "sk-1" {
		t.Fatalf("inherit/openai = %q %q %q %q", p, m, u, k)
	}
	p, _, _, _ = resolveEmbedding(BuildInput{Embedding: "inherit"})
	if p != "hashing" {
		t.Fatalf("inherit/none = %q, want hashing", p)
	}
	_, _, _, k = resolveEmbedding(BuildInput{
		Embedding: "openai", OpenAIKey: "sk-old", EmbeddingAPIKey: "sk-new",
	})
	if k != "sk-new" {
		t.Fatalf("explicit key should win, got %q", k)
	}
}

func TestDescriptionMentionsDB(t *testing.T) {
	in := testInput(t)
	eng, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if !strings.Contains(eng.Description(), in.DBPath) {
		t.Fatalf("Description = %q", eng.Description())
	}
}

func TestExpandHome(t *testing.T) {
	if got := ExpandHome("~/x/y.db"); !strings.HasSuffix(got, "x/y.db") || strings.Contains(got, "~") {
		t.Fatalf("ExpandHome(~/x/y.db) = %q", got)
	}
	if got := ExpandHome("/abs/path.db"); got != "/abs/path.db" {
		t.Fatalf("absolute passthrough broken: %q", got)
	}
}

func TestBuildRejectsEmptyDefaultWorkspace(t *testing.T) {
	in := testInput(t)
	in.DefaultWorkspace = ""
	if _, err := Build(in); err == nil {
		t.Fatal("Build should reject an empty default workspace")
	}
}

func TestWorkspaceWriteIsolation(t *testing.T) {
	in := testInput(t)
	in.DefaultWorkspace = "/ws/A"
	eng, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()

	// Writes into a non-default workspace must land in that workspace, not
	// silently in the engine's default.
	id, err := eng.Remember(ctx, "/ws/B", "auth uses JWT with 24h expiry", []string{"auth"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("Remember returned empty id")
	}
	if err := eng.RecordEvent(ctx, "/ws/B", "woca", "run", "ran the suite", "success"); err != nil {
		t.Fatal(err)
	}

	stB, err := eng.Stats(ctx, "/ws/B")
	if err != nil {
		t.Fatal(err)
	}
	if stB.Total != 2 || stB.Layers["L1_episodic"] != 1 || stB.Layers["L2_semantic"] != 1 {
		t.Fatalf("Stats(/ws/B) = %+v, want 1 episodic + 1 semantic", stB)
	}
	stA, err := eng.Stats(ctx, "/ws/A")
	if err != nil {
		t.Fatal(err)
	}
	if stA.Total != 0 {
		t.Fatalf("Stats(/ws/A) = %+v, want empty", stA)
	}

	hitsB, err := eng.Recall(ctx, "how does auth work", "/ws/B", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsB) == 0 {
		t.Fatal("Recall in /ws/B found nothing")
	}
	hitsA, err := eng.Recall(ctx, "how does auth work", "/ws/A", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hitsA) != 0 {
		t.Fatalf("Recall in /ws/A leaked %d hits from /ws/B", len(hitsA))
	}

	// A write with no explicit workspace must land in the default workspace
	// even after the engine was retargeted to /ws/B and back.
	if _, err := eng.Remember(ctx, "", "default workspace write", nil, "test"); err != nil {
		t.Fatal(err)
	}
	stA2, err := eng.Stats(ctx, "/ws/A")
	if err != nil {
		t.Fatal(err)
	}
	if stA2.Total != 1 || stA2.Layers["L2_semantic"] != 1 {
		t.Fatalf("Stats(/ws/A) after default write = %+v", stA2)
	}
}

func TestRememberGateDropReturnsEmptyID(t *testing.T) {
	eng, err := Build(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()

	// RecordEvent persists the episodic tuple as
	// "agent=.. | action=.. | observation=.. | outcome=..". Remembering
	// identical content within the attention gate's dedup window (1h) is
	// dropped by the gate and must surface as an empty id — never a phantom
	// id for a memory that was not persisted.
	if err := eng.RecordEvent(ctx, "/ws/test", "woca", "run", "auth uses JWT with 24h expiry", "success"); err != nil {
		t.Fatal(err)
	}
	st, err := eng.Stats(ctx, "/ws/test")
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 1 {
		t.Fatalf("setup Stats = %+v", st)
	}

	dup := "agent=woca | action=run | observation=auth uses JWT with 24h expiry | outcome=success"
	id, err := eng.Remember(ctx, "/ws/test", dup, nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if id != "" {
		t.Fatalf("gate-dropped Remember returned phantom id %q", id)
	}

	st2, err := eng.Stats(ctx, "/ws/test")
	if err != nil {
		t.Fatal(err)
	}
	if st2.Total != 1 || st2.Layers["L2_semantic"] != 0 {
		t.Fatalf("after gate drop Stats = %+v, want the episode only", st2)
	}
}

func TestConcurrentWorkspaces(t *testing.T) {
	in := testInput(t)
	in.DefaultWorkspace = "/ws/A"
	eng, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	ctx := context.Background()

	const perWS = 8
	var wg sync.WaitGroup
	for i := 0; i < perWS; i++ {
		wg.Add(3)
		go func(i int) { // writer into workspace A
			defer wg.Done()
			id, err := eng.Remember(ctx, "/ws/A", fmt.Sprintf("fact number %d about auth flows", i), nil, "race")
			if err != nil || id == "" {
				t.Errorf("A write %d: id=%q err=%v", i, id, err)
			}
		}(i)
		go func(i int) { // writer into workspace B
			defer wg.Done()
			id, err := eng.Remember(ctx, "/ws/B", fmt.Sprintf("resolver number %d for graphql queries", i), nil, "race")
			if err != nil || id == "" {
				t.Errorf("B write %d: id=%q err=%v", i, id, err)
			}
		}(i)
		go func() { // concurrent readers on both workspaces
			defer wg.Done()
			for q := 0; q < 3; q++ {
				if _, err := eng.Recall(ctx, "auth flows", "/ws/A", 5); err != nil {
					t.Errorf("recall A: %v", err)
				}
				if _, err := eng.Recall(ctx, "graphql", "/ws/B", 5); err != nil {
					t.Errorf("recall B: %v", err)
				}
			}
		}()
	}
	wg.Wait()

	stA, err := eng.Stats(ctx, "/ws/A")
	if err != nil {
		t.Fatal(err)
	}
	stB, err := eng.Stats(ctx, "/ws/B")
	if err != nil {
		t.Fatal(err)
	}
	if stA.Total != perWS || stA.Layers["L2_semantic"] != perWS {
		t.Fatalf("Stats(/ws/A) = %+v, want %d semantic facts", stA, perWS)
	}
	if stB.Total != perWS || stB.Layers["L2_semantic"] != perWS {
		t.Fatalf("Stats(/ws/B) = %+v, want %d semantic facts", stB, perWS)
	}
}

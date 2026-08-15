package memory

import (
	"context"
	"path/filepath"
	"strings"
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

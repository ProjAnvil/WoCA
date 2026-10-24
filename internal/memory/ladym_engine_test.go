package memory

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite" // database/sql driver name "sqlite"

	"github.com/projanvil/langchain-golang/core/language"
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
	ctx := t.Context()

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
	// inherit + official OpenAI base (or no base = official default) + key → openai
	p, m, u, k := resolveEmbedding(BuildInput{
		Embedding: "inherit", OpenAIKey: "sk-1", OpenAIBaseURL: "https://api.openai.com/v1",
	})
	if p != "openai" || m != "text-embedding-3-small" || u != "https://api.openai.com/v1" || k != "sk-1" {
		t.Fatalf("inherit/official = %q %q %q %q", p, m, u, k)
	}
	p, _, _, k = resolveEmbedding(BuildInput{Embedding: "inherit", OpenAIKey: "sk-1"})
	if p != "openai" || k != "sk-1" {
		t.Fatalf("inherit/default-base = %q %q, want openai sk-1", p, k)
	}
	// inherit + chat-only OpenAI-compatible endpoint (deepseek et al.) → hashing
	// (their /embeddings 404s and would break engine startup)
	p, _, _, _ = resolveEmbedding(BuildInput{
		Embedding: "inherit", OpenAIKey: "sk-1", OpenAIBaseURL: "https://api.deepseek.com",
	})
	if p != "hashing" {
		t.Fatalf("inherit/chat-only = %q, want hashing", p)
	}
	// explicit embedding key overrides the endpoint check (user knows best)
	p, _, _, k = resolveEmbedding(BuildInput{
		Embedding: "inherit", OpenAIBaseURL: "https://api.deepseek.com", EmbeddingAPIKey: "sk-embed",
	})
	if p != "openai" || k != "sk-embed" {
		t.Fatalf("inherit/explicit-key = %q %q, want openai sk-embed", p, k)
	}
	// inherit with nothing usable → hashing
	p, _, _, _ = resolveEmbedding(BuildInput{Embedding: "inherit"})
	if p != "hashing" {
		t.Fatalf("inherit/none = %q, want hashing", p)
	}
	// explicit openai keeps the key-precedence rule
	_, _, _, k = resolveEmbedding(BuildInput{
		Embedding: "openai", OpenAIKey: "sk-old", EmbeddingAPIKey: "sk-new",
	})
	if k != "sk-new" {
		t.Fatalf("explicit key should win, got %q", k)
	}
}

// TestBuildInheritChatOnlyEndpointFallsBackToHashing reproduces the
// deepseek-style startup failure: inherit + a chat-completions-only base
// used to probe the missing /embeddings endpoint and kill the whole engine.
func TestBuildInheritChatOnlyEndpointFallsBackToHashing(t *testing.T) {
	eng, err := Build(BuildInput{
		EngineEnabled:    true,
		DBPath:           filepath.Join(t.TempDir(), "mem.db"),
		DefaultWorkspace: "/ws",
		Embedding:        "inherit",
		LLM:              "none",
		OpenAIKey:        "sk-deepseek",
		OpenAIBaseURL:    "https://api.deepseek.com",
	})
	if err != nil {
		t.Fatalf("Build should fall back to hashing, got: %v", err)
	}
	defer eng.Close()
	if _, err := eng.Remember(t.Context(), "/ws", "auth uses JWT", nil, "t"); err != nil {
		t.Fatal(err)
	}
	hits, err := eng.Recall(t.Context(), "auth", "/ws", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("hashing engine should work offline: %v %v", hits, err)
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
	ctx := t.Context()

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
	ctx := t.Context()

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
	ctx := t.Context()

	const perWS = 8
	var wg sync.WaitGroup
	for i := range perWS {
		wg.Go(func() { // writer into workspace A
			id, err := eng.Remember(ctx, "/ws/A", fmt.Sprintf("fact number %d about auth flows", i), nil, "race")
			if err != nil || id == "" {
				t.Errorf("A write %d: id=%q err=%v", i, id, err)
			}
		})
		wg.Go(func() { // writer into workspace B
			id, err := eng.Remember(ctx, "/ws/B", fmt.Sprintf("resolver number %d for graphql queries", i), nil, "race")
			if err != nil || id == "" {
				t.Errorf("B write %d: id=%q err=%v", i, id, err)
			}
		})
		wg.Go(func() { // episodic writers race the semantic writers
			if err := eng.RecordEvent(ctx, "/ws/A", "woca", "run", fmt.Sprintf("episode %d for auth work", i), "success"); err != nil {
				t.Errorf("A event %d: %v", i, err)
			}
		})
		wg.Go(func() { // concurrent readers on both workspaces
			for range 3 {
				if _, err := eng.Recall(ctx, "auth flows", "/ws/A", 5); err != nil {
					t.Errorf("recall A: %v", err)
				}
				if _, err := eng.Recall(ctx, "graphql", "/ws/B", 5); err != nil {
					t.Errorf("recall B: %v", err)
				}
				if _, err := eng.List(ctx, "/ws/B", "", 5, 0); err != nil {
					t.Errorf("list B: %v", err)
				}
			}
		})
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
	if stA.Total != 2*perWS || stA.Layers["L2_semantic"] != perWS || stA.Layers["L1_episodic"] != perWS {
		t.Fatalf("Stats(/ws/A) = %+v, want %d semantic + %d episodic", stA, perWS, perWS)
	}
	if stB.Total != perWS || stB.Layers["L2_semantic"] != perWS {
		t.Fatalf("Stats(/ws/B) = %+v, want %d semantic facts", stB, perWS)
	}
}

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
	ctx := t.Context()

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
	if _, err := eng.Consolidate(ctx, "/ws", 0); err != nil {
		t.Fatal(err)
	}
}

func writeFile(root, name, content string) error {
	return os.WriteFile(filepath.Join(root, name), []byte(content), 0o644)
}

func TestEngineBranches(t *testing.T) {
	eng := newEngine(t)
	ctx := t.Context()

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
	if _, err := eng.Consolidate(ctx, "/other", 0); err != nil {
		t.Fatal(err)
	}
	// Empty-workspace index/consolidate fallbacks.
	if _, err := eng.IndexCode(ctx, t.TempDir(), "", true); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Consolidate(ctx, "", 0); err != nil {
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

func TestDefaultDBPathWithoutHome(t *testing.T) {
	t.Setenv("HOME", "")
	if got := DefaultDBPath(); got != "" {
		t.Fatalf("DefaultDBPath without HOME = %q, want empty", got)
	}
}

func TestOfficialOpenAIRejectsMalformedURL(t *testing.T) {
	if officialOpenAI("http://exam\x7fle.com") {
		t.Fatal("malformed URL should not parse as official OpenAI")
	}
}

func TestBuildSetsEmbedKeyEnv(t *testing.T) {
	t.Setenv(embedKeyEnv, "stale-key")
	in := testInput(t)
	in.EmbeddingAPIKey = "fresh-key"
	eng, err := Build(in)
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	if got := os.Getenv(embedKeyEnv); got != "fresh-key" {
		t.Fatalf("%s = %q, want fresh-key", embedKeyEnv, got)
	}
	// A later keyless Build must not inherit the stale value.
	keyless := testInput(t)
	eng2, err := Build(keyless)
	if err != nil {
		t.Fatal(err)
	}
	defer eng2.Close()
	if got := os.Getenv(embedKeyEnv); got != "" {
		t.Fatalf("%s = %q after keyless Build, want unset", embedKeyEnv, got)
	}
}

func TestBuildInheritChatModel(t *testing.T) {
	in := testInput(t)
	in.LLM = "inherit"
	in.ChatModel = stubChatModel{}
	eng, err := Build(in)
	if err != nil {
		t.Fatalf("Build with inherit chat model: %v", err)
	}
	defer eng.Close()
	if _, err := eng.Remember(t.Context(), "/ws/test", "a fact stored via the inherited-model engine", nil, "t"); err != nil {
		t.Fatal(err)
	}
}

func TestBuildEngineOpenFailure(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "blocker")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	in := testInput(t)
	in.DBPath = filepath.Join(blocker, "mem.db")
	if _, err := Build(in); err == nil {
		t.Fatal("Build should fail when the engine cannot open its database")
	}
}

// TestEngineErrorsAfterClose: once the underlying store is closed every
// surface returns a wrapped error instead of panicking.
func TestEngineErrorsAfterClose(t *testing.T) {
	eng, err := Build(testInput(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	if _, err := eng.Recall(ctx, "q", "/ws/test", 5); err == nil {
		t.Error("Recall after Close should fail")
	}
	if _, err := eng.SearchCode(ctx, "q", "/ws/test", 5); err == nil {
		t.Error("SearchCode after Close should fail")
	}
	if _, err := eng.Remember(ctx, "/ws/test", "late fact", nil, "t"); err == nil {
		t.Error("Remember after Close should fail")
	}
	if err := eng.RecordEvent(ctx, "/ws/test", "a", "b", "c", "d"); err == nil {
		t.Error("RecordEvent after Close should fail")
	}
	if _, err := eng.List(ctx, "/ws/test", "", 10, 0); err == nil {
		t.Error("List after Close should fail")
	}
	if err := eng.Forget(ctx, "some-id"); err == nil {
		t.Error("Forget after Close should fail")
	}
	if _, err := eng.Stats(ctx, "/ws/test"); err == nil {
		t.Error("Stats after Close should fail")
	}
	root := t.TempDir()
	if err := writeFile(root, "main.go", "package main\nfunc main() {}\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.IndexCode(ctx, root, "/ws/test", true); err == nil {
		t.Error("IndexCode after Close should fail")
	}
	if _, err := eng.Consolidate(ctx, "/ws/test", 0); err == nil {
		t.Error("Consolidate after Close should fail")
	}
	_ = eng.Close() // second Close stays safe
}

// stubChatModel satisfies language.ChatModel for Build(LLM=inherit); the
// adapter only stores the model, so no method is ever invoked.
type stubChatModel struct {
	language.ChatModel
}

// Closing the engine surfaces errors on every Engine path instead of
// panicking (deep error-injection sweep over the embedded adapter).
func TestLadyMEngineClosedSurfacesErrors(t *testing.T) {
	dir := t.TempDir()
	eng, err := Build(BuildInput{
		EngineEnabled: true, DBPath: filepath.Join(dir, "l.db"),
		DefaultWorkspace: "/w", Embedding: "hashing", LLM: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: no error after close", what)
		}
	}
	_, err = eng.Recall(ctx, "q", "/w", 3)
	check("recall", err)
	_, err = eng.SearchCode(ctx, "q", "/w", 3)
	check("searchcode", err)
	_, err = eng.Remember(ctx, "/w", "x", nil, "s")
	check("remember", err)
	check("recordevent", eng.RecordEvent(ctx, "/w", "a", "b", "c", "d"))
	_, err = eng.List(ctx, "/w", "", 5, 0)
	check("list", err)
	check("forget", eng.Forget(ctx, "id"))
	_, err = eng.Stats(ctx, "/w")
	check("stats", err)
	// IndexCode walks the source root (filesystem-driven); a closed engine
	// must not panic even where it may not error.
	_, _ = eng.IndexCode(ctx, dir, "/w", false)
	_, err = eng.Consolidate(ctx, "/w", 0)
	check("consolidate", err)
}

// The retain pipeline's load-bearing path: auto-retain writes only L1
// episodes (the Q/A material rides the observation); the composed System 2
// cycle is what promotes that material into an L2 fact, and a repeated
// cycle must not duplicate it.
func TestConsolidatePromotesEpisodeMaterialIntoFacts(t *testing.T) {
	eng := newEngine(t)
	ctx := t.Context()

	if err := eng.RecordEvent(ctx, "/ws", "woca", "run",
		"Q: which framework does this project use?\nA: fiber, pinned in go.mod", "success"); err != nil {
		t.Fatal(err)
	}
	report, err := eng.Consolidate(ctx, "/ws", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report, "facts_promoted=1") {
		t.Fatalf("report = %q, want one promotion", report)
	}
	facts, err := eng.List(ctx, "/ws", "L2_semantic", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 || !strings.Contains(facts[0].Content, "which framework does this project use?") {
		t.Fatalf("facts = %+v", facts)
	}

	// A second cycle no-ops on the already-promoted episode: the
	// consolidated_at mark skips it in selection (facts_promoted=0), so the
	// fact layer stays at one entry without re-classifying anything.
	report2, err := eng.Consolidate(ctx, "/ws", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(report2, "facts_promoted=0") {
		t.Fatalf("second report = %q, want no promotions", report2)
	}
	facts, err = eng.List(ctx, "/ws", "L2_semantic", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1 {
		t.Fatalf("repeated cycle duplicated facts: %+v", facts)
	}
}

func TestConsolidateSweepsDanglingEdges(t *testing.T) {
	eng := newEngine(t)
	ctx := t.Context()

	id1, err := eng.Remember(ctx, "", "fact one about auth flows", nil, "test")
	if err != nil || id1 == "" {
		t.Fatalf("Remember one: id=%q err=%v", id1, err)
	}
	id2, err := eng.Remember(ctx, "", "fact two about auth flows", nil, "test")
	if err != nil || id2 == "" {
		t.Fatalf("Remember two: id=%q err=%v", id2, err)
	}

	// Insert edges behind the engine's back: one valid, one pointing at a
	// memory that does not exist (the legacy pre-FK shape the sweep fixes).
	db, err := sql.Open("sqlite", eng.path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO edges (id, src_id, relation, dst_id, weight, valid_from)
		 VALUES ('e-ok', ?, 'relates_to', ?, 1.0, 0)`, id1, id2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`INSERT INTO edges (id, src_id, relation, dst_id, weight, valid_from)
		 VALUES ('e-dangling', ?, 'relates_to', 'memory-that-never-existed', 1.0, 0)`, id1); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Consolidate(ctx, "", 0); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE id = 'e-dangling'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("Consolidate left the dangling edge behind")
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE id = 'e-ok'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatal("Consolidate removed the valid edge")
	}
}

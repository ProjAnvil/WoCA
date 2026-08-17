package settings

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedOverridesAndLocks(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// User preference first.
	if err := st.Save(Settings{Model: "user-model"}); err != nil {
		t.Fatal(err)
	}
	st.SetManaged(Settings{Provider: "openai", Model: "managed-model"})

	// Reads: managed wins.
	got := st.Load()
	if got.Model != "managed-model" || got.Provider != "openai" {
		t.Fatalf("load = %+v", got)
	}

	// Writes to locked fields are rejected…
	if err := st.Save(Settings{Model: "user-model-2"}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("model write should be locked, got %v", err)
	}
	if err := st.Save(Settings{Provider: "anthropic"}); err == nil {
		t.Fatal("provider write should be locked")
	}
	// …but untouched fields stay writable.
	if err := st.Save(Settings{CheckpointDir: "/tmp/cks"}); err != nil {
		t.Fatalf("unmanaged field write: %v", err)
	}

	// Clearing the overlay restores write access.
	st.SetManaged(Settings{})
	if err := st.Save(Settings{Model: "user-model-3"}); err != nil {
		t.Fatalf("write after clearing overlay: %v", err)
	}
}

func TestManagedMemoryOverlay(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// User preference first.
	if err := st.Save(Settings{Model: "user-model", LadyMTopK: 3}); err != nil {
		t.Fatal(err)
	}
	st.SetManaged(Settings{
		MemoryEngine:   MemoryEngineNone,
		LadyMTopK:      12,
		LadyMEmbedding: LadyMEmbeddingHashing,
	})

	// Reads: managed wins.
	got := st.Load()
	if got.MemoryEngine != MemoryEngineNone || got.LadyMTopK != 12 || got.LadyMEmbedding != LadyMEmbeddingHashing {
		t.Fatalf("load = %+v", got)
	}

	// Writes to locked memory keys are rejected…
	if err := st.Save(Settings{MemoryEngine: MemoryEngineLadyM}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("memory_engine write should be locked, got %v", err)
	}
	if err := st.Save(Settings{LadyMTopK: 8}); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("ladym_topk write should be locked, got %v", err)
	}
	// …but unlocked fields stay writable.
	if err := st.Save(Settings{Model: "user-model-2"}); err != nil {
		t.Fatalf("unmanaged field write: %v", err)
	}
}

func TestLoadManagedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed.json")
	os.WriteFile(path, []byte(`{"provider":"openai","max_tokens":4096}`), 0o644)
	m, err := LoadManaged(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Provider != "openai" || m.MaxTokens != 4096 {
		t.Fatalf("managed = %+v", m)
	}

	os.WriteFile(path, []byte("{not json"), 0o644)
	if _, err := LoadManaged(path); err == nil {
		t.Fatal("invalid JSON should fail")
	}
	if _, err := LoadManaged(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("missing file should fail")
	}
}

func TestManagedOverlayAllFieldsAndLocks(t *testing.T) {
	st := newCryptoStore(t)
	base := Settings{
		Provider: "openai", Model: "m1", MaxTokens: 111, Temperature: 0.4,
		AnthropicBaseURL: "http://a", OpenAIBaseURL: "http://o", GLMBaseURL: "http://g", DeepSeekBaseURL: "http://d",
		CheckpointBackend: CheckpointPostgres, MemoryBackend: MemoryPostgres, CheckpointDir: "/ck",
		OutputStyle: "concise", MemoryEngine: MemoryEngineNone, LadyMDBPath: "/lady.db",
		LadyMEmbedding: LadyMEmbeddingOpenAI, LadyMEmbeddingModel: "emb-1", LadyMEmbeddingBaseURL: "http://emb",
		LadyMLLM: LadyMLLMNone, LadyMTopK: 9, LadyMSystem2Interval: 90,
	}
	st.SetManaged(base)
	got := st.Load()
	if got.Provider != "openai" || got.Model != "m1" || got.MaxTokens != 111 || got.Temperature != 0.4 {
		t.Fatalf("overlay scalars = %+v", got)
	}
	if got.CheckpointBackend != CheckpointPostgres || got.MemoryBackend != MemoryPostgres || got.CheckpointDir != "/ck" {
		t.Fatalf("overlay backends = %+v", got)
	}
	if got.OutputStyle != "concise" || got.MemoryEngine != MemoryEngineNone || got.LadyMDBPath != "/lady.db" {
		t.Fatalf("overlay misc = %+v", got)
	}
	if got.LadyMEmbedding != LadyMEmbeddingOpenAI || got.LadyMEmbeddingModel != "emb-1" || got.LadyMEmbeddingBaseURL != "http://emb" {
		t.Fatalf("overlay embedding = %+v", got)
	}
	if got.LadyMLLM != LadyMLLMNone || got.LadyMTopK != 9 || got.LadyMSystem2Interval != 90 {
		t.Fatalf("overlay ladym = %+v", got)
	}

	// Writes that would change any managed field are rejected.
	changed := got
	changed.Provider = "glm"
	if err := st.Save(changed); err == nil {
		t.Fatal("provider should be locked")
	}
	changed = got
	changed.Model = "other"
	if err := st.Save(changed); err == nil {
		t.Fatal("model should be locked")
	}
	changed = got
	changed.MaxTokens = 222
	if err := st.Save(changed); err == nil {
		t.Fatal("max-tokens should be locked")
	}
	changed = got
	changed.Temperature = 1
	if err := st.Save(changed); err == nil {
		t.Fatal("temperature should be locked")
	}
	for _, mut := range []func(*Settings){
		func(s *Settings) { s.AnthropicBaseURL = "x" },
		func(s *Settings) { s.OpenAIBaseURL = "x" },
		func(s *Settings) { s.CheckpointBackend = CheckpointFile },
		func(s *Settings) { s.MemoryBackend = MemorySQLite },
		func(s *Settings) { s.CheckpointDir = "x" },
		func(s *Settings) { s.OutputStyle = "x" },
		func(s *Settings) { s.MemoryEngine = MemoryEngineLadyM },
		func(s *Settings) { s.LadyMDBPath = "x" },
		func(s *Settings) { s.LadyMEmbedding = LadyMEmbeddingHashing },
		func(s *Settings) { s.LadyMEmbeddingModel = "x" },
		func(s *Settings) { s.LadyMEmbeddingBaseURL = "x" },
		func(s *Settings) { s.LadyMLLM = LadyMLLMInherit },
		func(s *Settings) { s.LadyMTopK = 5 },
		func(s *Settings) { s.LadyMSystem2Interval = 100 },
	} {
		changed := got
		mut(&changed)
		if err := st.Save(changed); err == nil {
			t.Errorf("field should be locked after mutation %+v", changed)
		}
	}
	// Saving the managed values themselves is fine, and clearing the overlay
	// restores the database values.
	if err := st.Save(got); err != nil {
		t.Fatalf("saving managed values = %v", err)
	}
	st.SetManaged(Settings{}) // empty overlay clears it
	// The managed values were persisted above, so they now come from the DB.
	if got := st.Load(); got.Model != "m1" || got.Provider != "openai" {
		t.Fatalf("after clear = %+v", got)
	}
}

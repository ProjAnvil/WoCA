package settings

import "testing"

func TestLadyMDefaults(t *testing.T) {
	got, err := Default().Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryEngine != MemoryEngineLadyM {
		t.Fatalf("memory engine default = %q, want ladym", got.MemoryEngine)
	}
	if got.LadyMEmbedding != LadyMEmbeddingInherit {
		t.Fatalf("embedding default = %q, want inherit", got.LadyMEmbedding)
	}
	if got.LadyMLLM != LadyMLLMInherit {
		t.Fatalf("llm default = %q, want inherit", got.LadyMLLM)
	}
	if !got.LadyMAutoRetain {
		t.Fatal("autoretain default should be true")
	}
	if got.LadyMTopK != 6 {
		t.Fatalf("topk default = %d, want 6", got.LadyMTopK)
	}
	if got.LadyMSystem2Interval != 300 {
		t.Fatalf("system2 interval default = %d, want 300", got.LadyMSystem2Interval)
	}
}

func TestApplyLadyMKeys(t *testing.T) {
	cfg := Default()
	cases := []struct {
		key, value string
		wantErr    bool
	}{
		{KeyMemoryEngine, "none", false},
		{KeyMemoryEngine, "ladym", false},
		{KeyMemoryEngine, "bogus", true},
		{KeyLadyMEmbedding, "hashing", false},
		{KeyLadyMEmbedding, "inherit", false},
		{KeyLadyMEmbedding, "nope", true},
		{KeyLadyMLLM, "none", false},
		{KeyLadyMLLM, "inherit", false},
		{KeyLadyMLLM, "weird", true},
		{KeyLadyMTopK, "20", false},
		{KeyLadyMTopK, "21", true},
		{KeyLadyMTopK, "0", true},
		{KeyLadyMSystem2Interval, "60", false},
		{KeyLadyMSystem2Interval, "59", true},
		{KeyLadyMAutoRetain, "false", false},
		{KeyLadyMDBPath, "/tmp/x.db", false},
		{KeyLadyMEmbeddingModel, "text-embedding-3-small", false},
		{KeyLadyMEmbeddingBaseURL, "https://x", false},
		{KeyLadyMEmbeddingAPIKey, "sk-x", true}, // secret: Apply must refuse
	}
	for _, c := range cases {
		err := Apply(&cfg, c.key, c.value)
		if c.wantErr && err == nil {
			t.Fatalf("Apply(%s=%s) should fail", c.key, c.value)
		}
		if !c.wantErr && err != nil {
			t.Fatalf("Apply(%s=%s): %v", c.key, c.value, err)
		}
	}
}

func TestLadyMPairsRoundTrip(t *testing.T) {
	base := Default()
	pairs := map[string]string{
		"memory_engine":            "none",
		"ladym_db_path":            "/tmp/mem.db",
		"ladym_embedding":          "hashing",
		"ladym_embedding_model":    "m1",
		"ladym_embedding_base_url": "https://e",
		"ladym_llm":                "none",
		"ladym_topk":               "12",
		"ladym_system2":            "true",
		"ladym_system2_interval":   "600",
	}
	applyPairs(&base, pairs)
	if base.MemoryEngine != "none" || base.LadyMDBPath != "/tmp/mem.db" || base.LadyMTopK != 12 ||
		!base.LadyMSystem2 || base.LadyMSystem2Interval != 600 {
		t.Fatalf("applyPairs mismatch: %+v", base)
	}
}

func TestLadyMSaveRoundTrip(t *testing.T) {
	st := openTest(t)
	cfg := Settings{
		MemoryEngine:          MemoryEngineNone,
		LadyMDBPath:           "/tmp/ladym.db",
		LadyMEmbedding:        LadyMEmbeddingHashing,
		LadyMEmbeddingModel:   "text-embedding-3-small",
		LadyMEmbeddingBaseURL: "https://embed.example",
		LadyMLLM:              LadyMLLMNone,
		LadyMAutoRetain:       false,
		LadyMTopK:             15,
		LadyMSystem2:          true,
		LadyMSystem2Interval:  900,
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	got := st.Load()
	if got.MemoryEngine != MemoryEngineNone || got.LadyMDBPath != "/tmp/ladym.db" ||
		got.LadyMEmbedding != LadyMEmbeddingHashing || got.LadyMEmbeddingModel != "text-embedding-3-small" ||
		got.LadyMEmbeddingBaseURL != "https://embed.example" || got.LadyMLLM != LadyMLLMNone {
		t.Fatalf("save round trip mismatch: %+v", got)
	}
	if got.LadyMTopK != 15 || got.LadyMSystem2Interval != 900 || !got.LadyMSystem2 {
		t.Fatalf("save round trip mismatch (nums/bool): %+v", got)
	}
	if got.LadyMAutoRetain {
		t.Fatal("ladym_autoretain=false must survive Save/Load")
	}
}

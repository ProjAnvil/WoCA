package memory

import (
	"path/filepath"
	"testing"
)

// The CJK dictionary capability: the embedded engine reports the tokenizer
// dictionary state and downloads it on explicit user action only. A missing
// dictionary is the status quo (per-character tokens), so both tests read
// real storage state against a fresh directory — no network.

func TestDictStatusUninstalled(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "dict")
	eng, err := Build(BuildInput{
		EngineEnabled:    true,
		DBPath:           filepath.Join(t.TempDir(), "mem.db"),
		DefaultWorkspace: "/ws",
		Embedding:        "hashing",
		LLM:              "none",
		DictDir:          dir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()

	var mgr DictManager = eng
	st := mgr.CJKDictStatus()
	if st.Available {
		t.Fatalf("fresh dict dir reported available: %+v", st)
	}
	if st.Source != "none" {
		t.Fatalf("source = %q, want none", st.Source)
	}
	if st.Dir != dir {
		t.Fatalf("dir = %q, want %q (Build must route DictDir)", st.Dir, dir)
	}
	if st.Variant != "" {
		t.Fatalf("variant = %q, want empty", st.Variant)
	}
}

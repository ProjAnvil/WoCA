package settings

import (
	"slices"
	"strings"
	"testing"
)

// subagents.max-concurrent caps the concurrently active foreground subagent
// children per session (graph-native HITL D12): int in [1,64], default 8
// when unset, delivered to EE bridges through the managed overlay (the
// config-pack key "subagents.max-concurrent").

func TestSubagentsMaxConcurrentDefaults(t *testing.T) {
	if DefaultSubagentsMaxConcurrent != 8 {
		t.Fatalf("DefaultSubagentsMaxConcurrent = %d, want 8", DefaultSubagentsMaxConcurrent)
	}
	got := Default()
	if got.SubagentsMaxConcurrent != 8 {
		t.Fatalf("default = %d, want 8", got.SubagentsMaxConcurrent)
	}
	// Normalize fills the default when unset and repairs a stored zero.
	norm, err := Settings{}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if norm.SubagentsMaxConcurrent != DefaultSubagentsMaxConcurrent {
		t.Fatalf("zero normalizes to %d, want default %d", norm.SubagentsMaxConcurrent, DefaultSubagentsMaxConcurrent)
	}
}

func TestSubagentsMaxConcurrentRegistered(t *testing.T) {
	if KeySubagentsMaxConcurrent != "subagents.max-concurrent" {
		t.Fatalf("key spelling = %q", KeySubagentsMaxConcurrent)
	}
	if !slices.Contains(KeyNames(), KeySubagentsMaxConcurrent) {
		t.Fatal("KeyNames missing subagents.max-concurrent")
	}
	if h := KeyHelp(KeySubagentsMaxConcurrent); h == "" || h == "unknown key" {
		t.Fatalf("KeyHelp = %q", h)
	}
	out := Describe(Default(), func(string) bool { return false }, "db")
	if !strings.Contains(out, "subagents.max-concurrent: 8") {
		t.Fatalf("Describe missing the cap line:\n%s", out)
	}
}

func TestApplySubagentsMaxConcurrent(t *testing.T) {
	cfg := Default()
	if err := Apply(&cfg, KeySubagentsMaxConcurrent, "3"); err != nil {
		t.Fatal(err)
	}
	if cfg.SubagentsMaxConcurrent != 3 {
		t.Fatalf("Apply = %d, want 3", cfg.SubagentsMaxConcurrent)
	}
	for _, bad := range []string{"0", "-1", "abc", "65", ""} {
		if err := Apply(&cfg, KeySubagentsMaxConcurrent, bad); err == nil {
			t.Fatalf("Apply(%q) should reject", bad)
		}
	}
	if v := KeyValue(cfg, KeySubagentsMaxConcurrent); v != "3" {
		t.Fatalf("KeyValue = %q", v)
	}
}

func TestSubagentsMaxConcurrentRoundTrip(t *testing.T) {
	st := openTest(t)
	cfg := st.Load()
	if err := Apply(&cfg, KeySubagentsMaxConcurrent, "3"); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := st.Load().SubagentsMaxConcurrent; got != 3 {
		t.Fatalf("round trip = %d, want 3", got)
	}
	// A cleared (absent) row falls back to the default.
	if err := st.Clear("subagents_max_concurrent"); err != nil {
		t.Fatal(err)
	}
	if got := st.Load().SubagentsMaxConcurrent; got != DefaultSubagentsMaxConcurrent {
		t.Fatalf("absent row loads as %d, want default %d", got, DefaultSubagentsMaxConcurrent)
	}
}

func TestSubagentsMaxConcurrentManagedOverlay(t *testing.T) {
	st := openTest(t)
	st.SetManaged(Settings{SubagentsMaxConcurrent: 2})
	if got := st.Load().SubagentsMaxConcurrent; got != 2 {
		t.Fatalf("overlay load = %d, want 2", got)
	}
	if fields := st.ManagedFields(); !slices.Contains(fields, KeySubagentsMaxConcurrent) {
		t.Fatalf("managed fields missing the cap: %v", fields)
	}
	// A conflicting write is locked; the managed value itself passes.
	if err := st.Save(Settings{SubagentsMaxConcurrent: 5}); err == nil {
		t.Fatal("conflicting cap write accepted")
	}
	if err := st.Save(Settings{SubagentsMaxConcurrent: 2}); err != nil {
		t.Fatalf("same-value write rejected: %v", err)
	}
}

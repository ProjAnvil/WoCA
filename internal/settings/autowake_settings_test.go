package settings

import (
	"slices"
	"strings"
	"testing"
)

// auto-wake / auto-wake-limit drive the idle wake driver (the server wake
// loop and the REPL): auto-wake gates automatic runs on queued
// notifications, auto-wake-limit caps the wake chain (a user-initiated run
// resets it). Stored semantics: an absent or zero row loads as
// DefaultAutoWakeLimit; a negative stored value loads as 0 = unlimited.

func TestAutoWakeDefaults(t *testing.T) {
	got := Default()
	if !got.AutoWake {
		t.Fatal("auto-wake default should be true")
	}
	if got.AutoWakeLimit != 20 {
		t.Fatalf("auto-wake-limit default = %d, want 20", got.AutoWakeLimit)
	}
	if DefaultAutoWakeLimit != 20 {
		t.Fatalf("DefaultAutoWakeLimit = %d, want 20", DefaultAutoWakeLimit)
	}
}

func TestAutoWakeKeyNames(t *testing.T) {
	names := KeyNames()
	if !slices.Contains(names, KeyAutoWake) || !slices.Contains(names, KeyAutoWakeLimit) {
		t.Fatalf("KeyNames missing auto-wake keys: %v", names)
	}
	if KeyAutoWake != "auto-wake" || KeyAutoWakeLimit != "auto-wake-limit" {
		t.Fatalf("key spellings drifted: %q %q", KeyAutoWake, KeyAutoWakeLimit)
	}
}

func TestApplyAutoWakeKeys(t *testing.T) {
	cfg := Default()
	cases := []struct {
		key, value string
		wantErr    bool
	}{
		{KeyAutoWake, "true", false},
		{KeyAutoWake, "false", false},
		{KeyAutoWake, "maybe", true},
		{KeyAutoWakeLimit, "5", false},
		{KeyAutoWakeLimit, "0", false},
		{KeyAutoWakeLimit, "-1", false}, // negative = the stored unlimited form
		{KeyAutoWakeLimit, "few", true},
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
	if cfg.AutoWake {
		t.Fatal("auto-wake=false must stick through Apply")
	}
	if cfg.AutoWakeLimit != -1 {
		t.Fatalf("auto-wake-limit = %d, want -1 (last valid case)", cfg.AutoWakeLimit)
	}
	if v := KeyValue(cfg, KeyAutoWake); v != "false" {
		t.Fatalf("KeyValue(auto-wake) = %q", v)
	}
	if v := KeyValue(cfg, KeyAutoWakeLimit); v != "-1" {
		t.Fatalf("KeyValue(auto-wake-limit) = %q", v)
	}
}

func TestAutoWakeNormalize(t *testing.T) {
	norm, err := Settings{}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if norm.AutoWakeLimit != DefaultAutoWakeLimit {
		t.Fatalf("zero limit normalizes to %d, want default %d", norm.AutoWakeLimit, DefaultAutoWakeLimit)
	}
	norm, err = Settings{AutoWakeLimit: -3}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if norm.AutoWakeLimit != 0 {
		t.Fatalf("negative limit normalizes to %d, want 0 (unlimited)", norm.AutoWakeLimit)
	}
	norm, err = Settings{AutoWakeLimit: 7}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if norm.AutoWakeLimit != 7 {
		t.Fatalf("positive limit must pass through, got %d", norm.AutoWakeLimit)
	}
}

func TestAutoWakeSaveRoundTrip(t *testing.T) {
	st := openTest(t)
	cfg := Settings{AutoWake: false, AutoWakeLimit: 3}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	got := st.Load()
	if got.AutoWake {
		t.Fatal("auto-wake=false must survive Save/Load")
	}
	if got.AutoWakeLimit != 3 {
		t.Fatalf("auto-wake-limit = %d, want 3", got.AutoWakeLimit)
	}

	// A stored negative value loads as 0 = unlimited.
	if err := st.Save(Settings{AutoWake: true, AutoWakeLimit: -1}); err != nil {
		t.Fatal(err)
	}
	got = st.Load()
	if got.AutoWakeLimit != 0 {
		t.Fatalf("stored -1 loads as %d, want 0 (unlimited)", got.AutoWakeLimit)
	}
	if !got.AutoWake {
		t.Fatal("auto-wake=true must survive Save/Load")
	}

	// A cleared (absent) row falls back to the default.
	if err := st.Clear("auto_wake_limit"); err != nil {
		t.Fatal(err)
	}
	if got := st.Load(); got.AutoWakeLimit != DefaultAutoWakeLimit {
		t.Fatalf("absent limit loads as %d, want default %d", got.AutoWakeLimit, DefaultAutoWakeLimit)
	}

	// Describe renders both keys.
	out := Describe(Default(), func(string) bool { return false }, "test.db")
	if !strings.Contains(out, "auto-wake: true") || !strings.Contains(out, "auto-wake-limit: 20") {
		t.Fatalf("Describe missing auto-wake lines:\n%s", out)
	}
}

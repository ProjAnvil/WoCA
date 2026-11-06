package settings

import (
	"slices"
	"strings"
	"testing"
)

// disabled-plugins is the name-list setting (the builtin-skills pattern)
// recording which installed plugins are skipped at load; plugins.
// LoadFiltered consumes it. Empty (or absent) means all installed plugins
// are enabled.
func TestDisabledPluginsSetting(t *testing.T) {
	st := openTest(t)

	// Default: nothing disabled.
	if got := st.Load().DisabledPlugins; got != nil {
		t.Fatalf("default DisabledPlugins = %v; want nil", got)
	}

	// Apply parses a JSON name list and rejects anything else.
	cfg := st.Load()
	if err := Apply(&cfg, KeyDisabledPlugins, `["toolkit","acme"]`); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.DisabledPlugins, []string{"toolkit", "acme"}) {
		t.Fatalf("Apply = %v", cfg.DisabledPlugins)
	}
	if err := Apply(&cfg, KeyDisabledPlugins, "toolkit"); err == nil {
		t.Fatal("Apply should reject a non-JSON-array value")
	}

	// Save/Load round-trips; KeyValue re-formats as a JSON array.
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := st.Load().DisabledPlugins; !slices.Equal(got, []string{"toolkit", "acme"}) {
		t.Fatalf("round trip = %v", got)
	}
	if v := KeyValue(cfg, KeyDisabledPlugins); v != `["toolkit","acme"]` {
		t.Fatalf("KeyValue = %q", v)
	}

	// Clearing the list persists (name lists save as at least "[]").
	if err := Apply(&cfg, KeyDisabledPlugins, `[]`); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(cfg); err != nil {
		t.Fatal(err)
	}
	if got := st.Load().DisabledPlugins; len(got) != 0 {
		t.Fatalf("cleared list = %v; want empty", got)
	}

	// Registered at every site a name-list key must be.
	if KeyDisabledPlugins != "disabled-plugins" {
		t.Fatalf("key spelling = %q", KeyDisabledPlugins)
	}
	if !slices.Contains(KeyNames(), KeyDisabledPlugins) {
		t.Fatal("KeyNames missing disabled-plugins")
	}
	if h := KeyHelp(KeyDisabledPlugins); h == "" || h == "unknown key" {
		t.Fatalf("KeyHelp = %q", h)
	}
	if !strings.Contains(Describe(cfg, func(string) bool { return false }, "db"), "disabled-plugins:") {
		t.Fatal("Describe missing disabled-plugins")
	}
}

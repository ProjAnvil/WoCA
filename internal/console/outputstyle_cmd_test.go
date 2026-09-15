package console

import (
	"strings"
	"testing"
)

// TestOutputStyleSaveKeepsLadyMBools guards against partial-Save clobbering:
// /output-style must use Load-modify-Save so persisting the style does not
// reset explicitly-written bools (ladym_autoretain, ladym_system2) to false.
func TestOutputStyleSaveKeepsLadyMBools(t *testing.T) {
	c, _, _ := newObsConsole(t)
	sess := &Session{}

	// Seed the store with the ladyM bools persisted as true (Save writes
	// bools explicitly, so the rows exist in the settings table).
	cfg := c.deps.Settings.Load()
	cfg.LadyMAutoRetain = true
	cfg.LadyMSystem2 = true
	if err := c.deps.Settings.Save(cfg); err != nil {
		t.Fatal(err)
	}

	res, err := c.Execute(t.Context(), "output-style", []string{"explanatory"}, sess)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "explanatory") {
		t.Fatalf("output = %q", res.Output)
	}

	got := c.deps.Settings.Load()
	if got.OutputStyle != "explanatory" {
		t.Fatalf("output style = %q, want explanatory", got.OutputStyle)
	}
	if !got.LadyMAutoRetain {
		t.Fatal("partial Save clobbered ladym_autoretain")
	}
	if !got.LadyMSystem2 {
		t.Fatal("partial Save clobbered ladym_system2")
	}
}

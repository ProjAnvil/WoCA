// External test package: it may import console (which transitively imports
// the server, which imports slashcmds) without an import cycle.
package slashcmds_test

import (
	"testing"

	"github.com/projanvil/woca/internal/console"
	"github.com/projanvil/woca/internal/slashcmds"
)

// TestConsoleParity ensures the TUI console and the web autocomplete see the
// exact same builtin command table (the console delegates to this package).
func TestConsoleParity(t *testing.T) {
	consoleCmds := console.Commands()
	builtin := slashcmds.Builtin()
	if len(consoleCmds) != len(builtin) {
		t.Fatalf("console has %d commands, slashcmds has %d", len(consoleCmds), len(builtin))
	}
	for i := range builtin {
		if consoleCmds[i].Name != builtin[i].Name || consoleCmds[i].Description != builtin[i].Description {
			t.Fatalf("command %d mismatch: console %+v vs slashcmds %+v", i, consoleCmds[i], builtin[i])
		}
	}
}

func TestSortedAndUnique(t *testing.T) {
	seen := map[string]bool{}
	last := ""
	for _, c := range slashcmds.Builtin() {
		if seen[c.Name] {
			t.Fatalf("duplicate command %q", c.Name)
		}
		seen[c.Name] = true
		if last != "" && c.Name < last {
			t.Fatalf("commands not sorted: %q before %q", last, c.Name)
		}
		last = c.Name
		if c.Description == "" {
			t.Fatalf("command %q has empty description", c.Name)
		}
	}
}

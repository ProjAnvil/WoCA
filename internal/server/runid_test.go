package server

import (
	"strings"
	"testing"
)

// Run ids must be unique across processes: the shared run ledger keys rows
// by run_id, and per-process counters collide with rows left by previous
// instances (an enterprise restart used to fail its first run on the stale
// primary key).
func TestRunIDsAreProcessUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := newRunID()
		if !strings.HasPrefix(id, "run-") {
			t.Fatalf("id %q lost the run- prefix", id)
		}
		if seen[id] {
			t.Fatalf("duplicate run id %q", id)
		}
		seen[id] = true
	}
}

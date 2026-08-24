package merge

import "testing"

func TestEntriesLevelWins(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "a", Value: "user", Level: LevelUser, Source: "~/.woca"},
		{Name: "a", Value: "ws", Level: LevelWorkspace, Source: "work/.woca"},
		{Name: "a", Value: "flag", Level: LevelFlag, Source: "flag"},
	})
	if len(out) != 1 || out[0].Value != "flag" {
		t.Fatalf("flag layer must win, got %+v", out)
	}
	if len(conflicts) != 2 {
		t.Fatalf("expected 2 conflicts, got %+v", conflicts)
	}
	for _, c := range conflicts {
		if c.Name != "a" || c.Winner == c.Loser {
			t.Fatalf("bad conflict record: %+v", c)
		}
	}
}

func TestEntriesOrderBreaksTies(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "a", Value: "far", Level: LevelWorkspace, Order: 4, Source: "root"},
		{Name: "a", Value: "near", Level: LevelWorkspace, Order: 0, Source: "workdir"},
	})
	if len(out) != 1 || out[0].Value != "near" {
		t.Fatalf("smaller Order must win, got %+v", out)
	}
	if len(conflicts) != 1 || conflicts[0].Winner != "workdir" || conflicts[0].Loser != "root" {
		t.Fatalf("bad conflict: %+v", conflicts)
	}
}

func TestEntriesSourceBreaksFinalTie(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "a", Value: "b-val", Level: LevelUser, Order: 1, Source: "b-entry"},
		{Name: "a", Value: "a-val", Level: LevelUser, Order: 1, Source: "a-entry"},
	})
	if len(out) != 1 || out[0].Value != "a-val" {
		t.Fatalf("lexicographically smaller Source must win, got %+v", out)
	}
	if len(conflicts) != 1 || conflicts[0].Winner != "a-entry" || conflicts[0].Loser != "b-entry" {
		t.Fatalf("bad conflict: %+v", conflicts)
	}
}

func TestEntriesDeterministicOutput(t *testing.T) {
	out, _ := Entries([]Entry[int]{
		{Name: "b", Value: 1, Level: LevelUser, Order: 0, Source: "u"},
		{Name: "a", Value: 2, Level: LevelWorkspace, Order: 3, Source: "w"},
		{Name: "c", Value: 3, Level: LevelWorkspace, Order: 1, Source: "w"},
	})
	var got []string
	for _, e := range out {
		got = append(got, e.Name)
	}
	// Higher Level first; within a Level, lower Order first.
	if len(got) != 3 || got[0] != "c" || got[1] != "a" || got[2] != "b" {
		t.Fatalf("unexpected order: %v", got)
	}
}

func TestEntriesSkipsEmptyNames(t *testing.T) {
	out, conflicts := Entries([]Entry[string]{
		{Name: "", Value: "x", Level: LevelUser, Source: "s"},
		{Name: "a", Value: "y", Level: LevelUser, Source: "s"},
	})
	if len(out) != 1 || out[0].Name != "a" {
		t.Fatalf("empty names must be dropped: %+v", out)
	}
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
}

func TestLevelString(t *testing.T) {
	for _, tc := range []struct {
		l    Level
		want string
	}{{LevelUser, "user"}, {LevelWorkspace, "workspace"}, {LevelFlag, "flag"}} {
		if got := tc.l.String(); got != tc.want {
			t.Fatalf("Level(%d).String() = %q, want %q", tc.l, got, tc.want)
		}
	}
}

// TestManagedLayerLoses: the managed (operator-delivered) level is the
// lowest — a user-level same-name entry wins.
func TestManagedLayerLoses(t *testing.T) {
	es := []Entry[string]{
		{Name: "fetch", Value: "managed", Level: LevelManaged, Source: "managed"},
		{Name: "fetch", Value: "user", Level: LevelUser, Source: "user"},
		{Name: "extra", Value: "managed-only", Level: LevelManaged, Source: "managed"},
	}
	merged, conflicts := Entries(es)
	if len(merged) != 2 || merged[0].Value != "user" {
		t.Fatalf("merged = %+v", merged)
	}
	if len(conflicts) != 1 || conflicts[0].Winner != "user" {
		t.Fatalf("conflicts = %+v", conflicts)
	}
	if LevelManaged >= LevelUser {
		t.Fatal("LevelManaged must sort below LevelUser")
	}
}

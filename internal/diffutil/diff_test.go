package diffutil

import (
	"strings"
	"testing"
)

func script(t *testing.T, a, b string) []Op {
	t.Helper()
	return Diff(SplitLines(a), SplitLines(b))
}

func TestDiffIdentical(t *testing.T) {
	ops := script(t, "a\nb\nc\n", "a\nb\nc\n")
	if len(ops) != 3 || ops[0].Kind != '=' {
		t.Fatalf("want 3 context ops, got %v", ops)
	}
	if s := Unified("f", []byte("a\nb\n"), []byte("a\nb\n"), 3); s != "" {
		t.Fatalf("identical input must render empty diff, got %q", s)
	}
}

func TestDiffInsertDeleteModify(t *testing.T) {
	// middle line modified + one appended
	ops := script(t, "one\ntwo\nthree\n", "one\nTWO\nthree\nfour\n")
	var got []string
	for _, op := range ops {
		got = append(got, string(op.Kind)+op.Line)
	}
	want := "=one,-two,+TWO,=three,+four"
	if strings.Join(got, ",") != want {
		t.Fatalf("ops = %v, want %s", got, want)
	}
	added, removed := Counts(ops)
	if added != 2 || removed != 1 {
		t.Fatalf("counts = %d/%d, want 2/1", added, removed)
	}
}

func TestDiffEmptySides(t *testing.T) {
	// created file: everything added.
	ops := script(t, "", "x\ny\n")
	if len(ops) != 2 || ops[0].Kind != '+' || ops[1].Kind != '+' {
		t.Fatalf("created-file ops = %v", ops)
	}
	// deleted file: everything removed.
	ops = script(t, "x\ny\n", "")
	if len(ops) != 2 || ops[0].Kind != '-' || ops[1].Kind != '-' {
		t.Fatalf("deleted-file ops = %v", ops)
	}
}

func TestUnifiedHunkHeadersAndContext(t *testing.T) {
	before := strings.Repeat("ctx\n", 10) + "old\n" + strings.Repeat("ctx\n", 10)
	after := strings.Repeat("ctx\n", 10) + "new\n" + strings.Repeat("ctx\n", 10)
	diff := Unified("dir/f.txt", []byte(before), []byte(after), 3)
	if !strings.Contains(diff, "--- a/dir/f.txt") || !strings.Contains(diff, "+++ b/dir/f.txt") {
		t.Fatalf("missing file headers: %q", diff)
	}
	// One hunk with 3+3 context lines around the single change.
	if got := strings.Count(diff, "@@"); got != 2 { // "@@" appears twice per header
		t.Fatalf("hunk headers = %d, want 1: %q", got/2, diff)
	}
	if !strings.Contains(diff, "-old") || !strings.Contains(diff, "+new") {
		t.Fatalf("missing change lines: %q", diff)
	}
	ctxLines := strings.Count(diff, " ctx")
	if ctxLines != 6 {
		t.Fatalf("context lines = %d, want 6: %q", ctxLines, diff)
	}
}

func TestUnifiedSplitsDistantChanges(t *testing.T) {
	before := "a\n" + strings.Repeat("fill\n", 20) + "b\n"
	after := "A\n" + strings.Repeat("fill\n", 20) + "B\n"
	diff := Unified("f", []byte(before), []byte(after), 3)
	if n := strings.Count(diff, "@@ -"); n != 2 {
		t.Fatalf("hunks = %d, want 2: %q", n, diff)
	}
}

func TestDiffLargeInputsFallback(t *testing.T) {
	// Two completely different 3000-line files exceed maxDepth: the
	// replace-all fallback keeps the result bounded and correct.
	a := make([]string, 3000)
	b := make([]string, 3000)
	for i := range a {
		a[i] = "x"
		b[i] = "y"
	}
	ops := Diff(a, b)
	added, removed := Counts(ops)
	if added != 3000 || removed != 3000 {
		t.Fatalf("fallback counts = %d/%d", added, removed)
	}
}

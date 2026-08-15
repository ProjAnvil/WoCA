package repl

import (
	"io"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Action
	}{
		{"hello world", Action{Kind: KindPrompt, Text: "hello world"}},
		{"  spaced  ", Action{Kind: KindPrompt, Text: "spaced"}},
		{"", Action{Kind: KindPrompt}},
		{"!ls -la", Action{Kind: KindShell, Text: "ls -la"}},
		{"!  git status  ", Action{Kind: KindShell, Text: "git status"}},
		{"/help me", Action{Kind: KindCommand, Text: "help", Args: []string{"me"}}},
		{"/exit", Action{Kind: KindQuit, Text: "exit"}},
		{"/quit", Action{Kind: KindQuit, Text: "quit"}},
	}
	for _, c := range cases {
		got := Parse(c.in)
		if got.Kind != c.want.Kind || got.Text != c.want.Text {
			t.Errorf("Parse(%q) = %+v, want %+v", c.in, got, c.want)
		}
		if len(got.Args) != len(c.want.Args) {
			t.Errorf("Parse(%q) args = %v, want %v", c.in, got.Args, c.want.Args)
		} else {
			for i := range got.Args {
				if got.Args[i] != c.want.Args[i] {
					t.Errorf("Parse(%q) args = %v, want %v", c.in, got.Args, c.want.Args)
				}
			}
		}
	}
}

func TestHistoryNavigation(t *testing.T) {
	h := &History{}
	h.Add("a")
	h.Add("b")

	got, ok := h.Prev()
	if !ok || got != "b" {
		t.Fatalf("Prev = %q %v", got, ok)
	}
	got, _ = h.Prev()
	if got != "a" {
		t.Fatalf("Prev = %q", got)
	}
	if _, ok := h.Prev(); ok {
		t.Fatal("Prev past oldest should be !ok")
	}
	got, _ = h.Next()
	if got != "b" {
		t.Fatalf("Next = %q", got)
	}
	got, ok = h.Next()
	if !ok || got != "" {
		t.Fatalf("Next past newest = %q %v", got, ok)
	}
}

func TestHistoryDedupConsecutive(t *testing.T) {
	h := &History{}
	h.Add("a")
	h.Add("a")
	if got, _ := h.Prev(); got != "a" {
		t.Fatalf("dedup failed, Prev = %q", got)
	}
	if _, ok := h.Prev(); ok {
		t.Fatal("expected single entry after dedup")
	}
}

func FuzzParse(f *testing.F) {
	f.Add("hello")
	f.Add("!ls -la")
	f.Add("/help me")
	f.Add("")
	f.Fuzz(func(t *testing.T, line string) {
		_ = Parse(line) // must not panic
	})
}

func TestEditorReadsAllPipedLines(t *testing.T) {
	// Regression: a per-call bufio.Reader used to swallow every buffered line
	// after the first, so piped multi-line input only ever executed line one.
	in := strings.NewReader("/one\n/two\n/three\n")
	e := NewEditor(in, io.Discard, nil)
	var got []string
	for {
		line, err := e.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, line)
	}
	want := []string{"/one", "/two", "/three"}
	if len(got) != len(want) {
		t.Fatalf("lines read = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

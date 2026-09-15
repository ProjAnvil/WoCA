package repl

import (
	"errors"
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
		if errors.Is(err, io.EOF) {
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

func TestParseEdgeCases(t *testing.T) {
	for _, c := range []struct {
		line string
		kind Kind
		text string
	}{
		{"", KindPrompt, ""},
		{"   ", KindPrompt, ""},
		{"!ls -l", KindShell, "ls -l"},
		{"/", KindPrompt, ""},
		{"//odd", KindCommand, "/odd"},
		{"/model  gpt-4o   extra", KindCommand, "model"},
		{"/exit", KindQuit, "exit"},
		{"/q", KindQuit, "q"},
		{"hello", KindPrompt, "hello"},
	} {
		a := Parse(c.line)
		if a.Kind != c.kind || a.Text != c.text {
			t.Errorf("Parse(%q) = %+v, want kind=%v text=%q", c.line, a, c.kind, c.text)
		}
	}
	// Command args split.
	if a := Parse("/model one two"); len(a.Args) != 2 || a.Args[0] != "one" {
		t.Errorf("args = %v", a.Args)
	}
}

func TestHistoryNavigationEdges(t *testing.T) {
	h := &History{}
	if _, ok := h.Prev(); ok {
		t.Error("empty history should not navigate")
	}
	if _, ok := h.Next(); ok {
		t.Error("empty history should not navigate forward")
	}
	h.Add("one")
	h.Add("two")
	h.Add("two") // consecutive duplicate dropped
	if len(h.lines) != 2 {
		t.Fatalf("lines = %v", h.lines)
	}
	// Navigate up twice, then down once.
	first, ok := h.Prev()
	if !ok || first != "two" {
		t.Fatalf("prev = %q %v", first, ok)
	}
	second, ok := h.Prev()
	if !ok || second != "one" {
		t.Fatalf("prev2 = %q %v", second, ok)
	}
	next, ok := h.Next()
	if !ok || next != "two" {
		t.Fatalf("next = %q %v", next, ok)
	}
	// Past the newest, Next signals a fresh line with an empty value.
	if v, ok := h.Next(); !ok || v != "" {
		t.Errorf("boundary next = %q %v", v, ok)
	}
	if _, ok := h.Next(); ok {
		t.Error("past the boundary next should not resolve")
	}
}

func TestHandleEscapeDirect(t *testing.T) {
	newRead := func(bytes ...byte) func() (byte, error) {
		i := 0
		return func() (byte, error) {
			if i >= len(bytes) {
				return 0, errors.New("eof-ish")
			}
			b := bytes[i]
			i++
			return b, nil
		}
	}
	// Up arrow with history.
	h := &History{}
	h.Add("saved")
	buf := []rune("typed")
	drawn := 0
	if err := handleEscape(newRead('[', 'A'), &buf, h, func() { drawn++ }); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "saved" || drawn != 1 {
		t.Fatalf("up: buf=%q drawn=%d", buf, drawn)
	}
	// Down arrow after an up restores the fresh (empty) line.
	if err := handleEscape(newRead('[', 'B'), &buf, h, func() {}); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "" {
		t.Fatalf("down after up: buf=%q", buf)
	}
	// Down with the selection parked at the fresh end leaves the buffer.
	buf = []rune("x")
	if err := handleEscape(newRead('[', 'B'), &buf, h, func() {}); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "x" {
		t.Fatalf("down at end: buf=%q", buf)
	}
	// A non-[ second byte is ignored.
	if err := handleEscape(newRead('O', 'A'), &buf, h, func() {}); err != nil {
		t.Fatal(err)
	}
	// No history: sequence consumed silently.
	if err := handleEscape(newRead('[', 'A'), &buf, nil, func() {}); err != nil {
		t.Fatal(err)
	}
	// Read errors propagate.
	if err := handleEscape(func() (byte, error) { return 0, errors.New("x") }, &buf, h, func() {}); err == nil {
		t.Fatal("expected read error")
	}
	if err := handleEscape(newRead('['), &buf, h, func() {}); err == nil {
		t.Fatal("expected second read error")
	}
}

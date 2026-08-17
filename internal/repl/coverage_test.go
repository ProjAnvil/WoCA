package repl

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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

func TestEditorPlainReadLines(t *testing.T) {
	var out bytes.Buffer
	e := NewEditor(strings.NewReader("first\nsecond\n"), &out, nil)
	line, err := e.ReadLine()
	if err != nil || line != "first" {
		t.Fatalf("first = %q %v", line, err)
	}
	line, err = e.ReadLine()
	if err != nil || line != "second" {
		t.Fatalf("second = %q %v", line, err)
	}
	if !strings.HasPrefix(out.String(), "> ") {
		t.Fatalf("prompt = %q", out.String())
	}
	// EOF terminates cleanly.
	if _, err := e.ReadLine(); err == nil {
		t.Fatal("expected EOF")
	}
}

// TestEditorTerminalViaPty re-executes this test binary under `script` so
// the editor's terminal (raw-mode) path runs against a real pty.
func TestEditorTerminalViaPty(t *testing.T) {
	if os.Getenv("WOCA_REPL_PTY_INNER") == "1" {
		innerTerminalEditor(t)
		return
	}
	bin, err := os.Executable()
	if err != nil {
		t.Skip("no executable path")
	}
	cmd := exec.Command("script", "-q", "/dev/null", bin, "-test.run", "TestEditorTerminalViaPty", "-test.v")
	cmd.Env = append(os.Environ(), "WOCA_REPL_PTY_INNER=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Skipf("script unavailable: %v", err)
	}
	// Keystrokes: type "abc", backspace the c, pull history up, submit.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = stdin.Write([]byte("abc"))
		_, _ = stdin.Write([]byte{0x7f}) // backspace
		_, _ = stdin.Write([]byte{0x1b, '[', 'A'})
		_, _ = stdin.Write([]byte("\r"))
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("inner run failed: %v\noutput:\n%s", err, out.String())
		}
	case <-time.After(15 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("inner run timed out")
	}
}

// innerTerminalEditor runs inside the pty: stdin is a real terminal.
func innerTerminalEditor(t *testing.T) {
	h := &History{}
	h.Add("history-line")
	e := NewEditor(os.Stdin, os.Stdout, h)
	line, err := e.ReadLine()
	if err != nil {
		t.Fatalf("ReadLine: %v", err)
	}
	// "abc" minus the backspaced c, replaced by the history entry.
	if line != "history-line" {
		t.Fatalf("line = %q", line)
	}
	os.Exit(0)
}

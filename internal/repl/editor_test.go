package repl

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

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
	if _, err = e.ReadLine(); err == nil {
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
	// Keystrokes: type "abc", backspace the c, pull history up, submit; then
	// exercise Ctrl-C, Ctrl-D EOF, and a closed stdin inside the pty.
	go func() {
		time.Sleep(150 * time.Millisecond)
		_, _ = stdin.Write([]byte("abc"))
		_, _ = stdin.Write([]byte{0x7f}) // backspace
		_, _ = stdin.Write([]byte{0x1b, '[', 'A'})
		_, _ = stdin.Write([]byte("\r"))
		time.Sleep(150 * time.Millisecond)
		_, _ = stdin.Write([]byte{0x03}) // Ctrl-C → interrupted
		time.Sleep(150 * time.Millisecond)
		_, _ = stdin.Write([]byte("xy"))
		_, _ = stdin.Write([]byte{0x04}) // Ctrl-D → EOF with buffer
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

	// Ctrl-C aborts the line with an interrupt error.
	if _, err = e.ReadLine(); err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("ctrl-c = %v", err)
	}
	// Ctrl-D returns EOF with whatever was buffered.
	line, err = e.ReadLine()
	if line != "xy" || err == nil {
		t.Fatalf("ctrl-d = %q %v", line, err)
	}
	os.Exit(0)
}

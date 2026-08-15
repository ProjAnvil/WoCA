// Package repl editor: interactive line reading with terminal raw-mode
// editing (backspace, history navigation) and a persistent buffered fallback
// for non-terminal readers (pipes), so multi-line piped input is not
// swallowed by a per-call buffer.
package repl

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"golang.org/x/term"
)

// Editor reads input lines. On a terminal it enables raw-mode editing with a
// "> " prompt; on any other reader it falls back to buffered plain reads —
// using ONE persistent bufio.Reader so previously buffered lines survive
// across calls.
type Editor struct {
	stdin  io.Reader
	stdout io.Writer
	hist   *History
	buf    *bufio.Reader
}

// NewEditor creates an Editor. hist may be nil (history navigation disabled).
// Passing a *bufio.Reader reuses it, so callers that also read from the same
// buffered reader (e.g. interrupt prompts) share one buffer instead of racing
// ahead of each other.
func NewEditor(stdin io.Reader, stdout io.Writer, hist *History) *Editor {
	e := &Editor{stdin: stdin, stdout: stdout, hist: hist}
	if br, ok := stdin.(*bufio.Reader); ok {
		e.buf = br
	}
	return e
}

// ReadLine reads one line. It returns an io.EOF error at end of input.
func (e *Editor) ReadLine() (string, error) {
	if fd, ok := e.stdin.(interface{ Fd() uintptr }); ok && term.IsTerminal(int(fd.Fd())) {
		return e.readTerminal(fd)
	}
	return e.readPlain()
}

func (e *Editor) readTerminal(fd interface{ Fd() uintptr }) (string, error) {
	oldState, err := term.MakeRaw(int(fd.Fd()))
	if err != nil {
		return e.readPlain()
	}
	defer term.Restore(int(fd.Fd()), oldState)

	var buf []rune
	read := func() (byte, error) {
		var b [1]byte
		_, err := e.stdin.Read(b[:])
		return b[0], err
	}
	redraw := func() {
		fmt.Fprint(e.stdout, "\r\x1b[K> ")
		fmt.Fprint(e.stdout, string(buf))
	}
	redraw()

	for {
		ch, err := read()
		if err != nil {
			fmt.Fprintln(e.stdout)
			return string(buf), err
		}
		switch ch {
		case '\r', '\n':
			fmt.Fprintln(e.stdout)
			line := strings.TrimSpace(string(buf))
			if line != "" && e.hist != nil {
				e.hist.Add(line)
			}
			return line, nil
		case 0x03: // Ctrl-C
			fmt.Fprintln(e.stdout)
			return "", fmt.Errorf("interrupted")
		case 0x04: // Ctrl-D (EOF)
			fmt.Fprintln(e.stdout)
			return string(buf), io.EOF
		case 0x7f, 0x08: // backspace
			if len(buf) > 0 {
				buf = buf[:len(buf)-1]
				redraw()
			}
		case 0x1b: // escape sequence (arrows)
			if err := handleEscape(read, &buf, e.hist, redraw); err != nil {
				fmt.Fprintln(e.stdout)
				return "", err
			}
		default:
			if ch >= 0x20 { // printable
				buf = append(buf, rune(ch))
				fmt.Fprint(e.stdout, string(ch))
			}
		}
	}
}

// handleEscape consumes the remainder of an escape sequence and handles
// up/down history navigation.
func handleEscape(read func() (byte, error), buf *[]rune, hist *History, redraw func()) error {
	second, err := read()
	if err != nil {
		return err
	}
	if second != '[' {
		return nil
	}
	third, err := read()
	if err != nil {
		return err
	}
	if hist == nil {
		return nil
	}
	switch third {
	case 'A': // up
		if prev, ok := hist.Prev(); ok {
			*buf = []rune(prev)
			redraw()
		}
	case 'B': // down
		if next, ok := hist.Next(); ok {
			*buf = []rune(next)
			redraw()
		}
	}
	return nil
}

func (e *Editor) readPlain() (string, error) {
	fmt.Fprint(e.stdout, "> ")
	if e.buf == nil {
		e.buf = bufio.NewReader(e.stdin)
	}
	line, err := e.buf.ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) != "" && e.hist != nil {
		e.hist.Add(line)
	}
	return line, err
}

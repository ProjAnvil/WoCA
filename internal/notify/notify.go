// Package notify delivers terminal notifications: a BEL attention bell plus
// an iTerm2/ConEmu-style OSC 9 toast. Enabled unless WOCA_NOTIFY=0.
package notify

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// Enabled reports whether notifications are on (WOCA_NOTIFY=0 disables).
func Enabled() bool {
	return os.Getenv("WOCA_NOTIFY") != "0"
}

// Bell writes the terminal attention bell.
func Bell(w io.Writer) {
	fmt.Fprint(w, "\a")
}

// OSC9 writes an iTerm2/ConEmu desktop notification escape sequence.
func OSC9(w io.Writer, title string) {
	title = strings.ReplaceAll(title, "\x1b", "")
	title = strings.ReplaceAll(title, "\x07", "")
	fmt.Fprintf(w, "\x1b]9;%s\x07", title)
}

// Attention rings the bell and posts a toast when enabled (used to signal
// permission requests and turn completion).
func Attention(w io.Writer, message string) {
	if !Enabled() {
		return
	}
	Bell(w)
	OSC9(w, message)
}

// Package repl provides the input-parsing and line-editing pieces for the woca
// interactive REPL: slash-command / `!command` / prompt classification, and a
// command history.
package repl

import "strings"

// Kind classifies a parsed input line.
type Kind int

const (
	// KindPrompt is a normal prompt to send to the agent.
	KindPrompt Kind = iota
	// KindShell is a `!command` line to run in the shell; its output is fed
	// back to the agent.
	KindShell
	// KindCommand is a slash command (/name args...).
	KindCommand
	// KindQuit terminates the REPL.
	KindQuit
)

// Action is the result of parsing a line.
type Action struct {
	Kind Kind
	// Text is the prompt text, the shell command, or the slash command name.
	Text string
	// Args are the slash-command arguments.
	Args []string
}

// Parse classifies a trimmed input line.
func Parse(line string) Action {
	line = strings.TrimSpace(line)
	if line == "" {
		return Action{Kind: KindPrompt}
	}
	switch {
	case strings.HasPrefix(line, "!"):
		return Action{Kind: KindShell, Text: strings.TrimSpace(strings.TrimPrefix(line, "!"))}
	case strings.HasPrefix(line, "/"):
		parts := strings.Fields(strings.TrimPrefix(line, "/"))
		if len(parts) == 0 {
			return Action{Kind: KindPrompt}
		}
		name := parts[0]
		if name == "exit" || name == "quit" || name == "q" {
			return Action{Kind: KindQuit, Text: name}
		}
		return Action{Kind: KindCommand, Text: name, Args: parts[1:]}
	default:
		return Action{Kind: KindPrompt, Text: line}
	}
}

// History is a bounded command history with up/down navigation.
type History struct {
	lines []string
	idx   int // 0..len(lines); len(lines) means "no selection (fresh line)"
}

// Add records a non-empty line, deduplicating consecutive repeats, and resets
// navigation to the fresh position.
func (h *History) Add(line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	if len(h.lines) == 0 || h.lines[len(h.lines)-1] != line {
		h.lines = append(h.lines, line)
		if len(h.lines) > 100 {
			h.lines = h.lines[len(h.lines)-100:]
		}
	}
	h.idx = len(h.lines)
}

// Prev returns the previous (older) entry.
func (h *History) Prev() (string, bool) {
	if h.idx <= 0 {
		return "", false
	}
	h.idx--
	return h.lines[h.idx], true
}

// Next returns the next (newer) entry; past the newest it returns ("", true)
// to signal a fresh line.
func (h *History) Next() (string, bool) {
	if h.idx >= len(h.lines) {
		return "", false
	}
	h.idx++
	if h.idx == len(h.lines) {
		return "", true
	}
	return h.lines[h.idx], true
}

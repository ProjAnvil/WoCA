package console

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/session"
)

// rewindCmd implements /rewind, a conversation-level rewind: with no args it
// lists rewind points (one per user turn); with a turn number it truncates
// the conversation to just before that turn. File snapshots are not restored
// — only the transcript rewinds.
func (c *Console) rewind(args []string, s *Session) (Result, error) {
	idx := userTurns(s.History)
	if len(args) == 0 {
		if len(idx) == 0 {
			return Result{Output: "nothing to rewind (no user turns yet)\n"}, nil
		}
		var b strings.Builder
		b.WriteString("rewind points (use /rewind <n> to go back to before turn n):\n")
		for n, i := range idx {
			fmt.Fprintf(&b, "  %d. %s\n", n+1, preview(s.History[i].Content, 72))
		}
		return Result{Output: b.String()}, nil
	}
	n, err := strconv.Atoi(args[0])
	if err != nil || n < 1 || n > len(idx) {
		return Result{}, fmt.Errorf("turn must be 1..%d (see /rewind for the list)", len(idx))
	}
	cut := idx[n-1]
	removed := len(s.History) - cut
	s.History = s.History[:cut]
	res := Result{Output: fmt.Sprintf("rewound to before turn %d (%d messages removed)\n", n, removed)}
	if len(s.History) == 0 {
		res.Clear = true
	}
	return res, nil
}

// userTurns returns the indexes of genuine user messages (synthetic tagged
// attachments — queued input, background notifications — do not count).
func userTurns(history []messages.Message) []int {
	var idx []int
	for i, m := range history {
		if m.Role != messages.RoleHuman {
			continue
		}
		if strings.HasPrefix(m.Content, "<") {
			continue // tagged attachment, not a typed turn
		}
		idx = append(idx, i)
	}
	return idx
}

// preview renders a single-line preview of a message.
func preview(content string, max int) string {
	line := strings.TrimSpace(content)
	if idx := strings.IndexByte(line, '\n'); idx >= 0 {
		line = line[:idx]
	}
	runes := []rune(line)
	if len(runes) > max {
		return strings.TrimSpace(string(runes[:max])) + "…"
	}
	if line == "" {
		return "(empty)"
	}
	return line
}

// branchCmd implements /branch: it snapshots the current conversation to a
// durable session file under ~/.woca/sessions that can be resumed later with
// --resume, while the current conversation continues unchanged (a fork rather
// than a switch).
func (c *Console) branch(args []string, s *Session) (Result, error) {
	if len(s.History) == 0 {
		return Result{}, fmt.Errorf("nothing to branch (the conversation is empty)")
	}
	name := "branch"
	if len(args) > 0 {
		name = sanitizeSessionName(args[0])
		if name == "" {
			name = "branch"
		}
	}
	dir := c.deps.SessionDir
	if dir == "" {
		dir = filepath.Join(session.DefaultDir(), "sessions")
	}
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.json", name, time.Now().Format("20060102-150405")))
	st := session.State{Messages: s.History}
	if c.deps.Agent != nil {
		st.PlanMode = c.deps.Agent.PlanMode()
		for _, t := range c.deps.Agent.Todos() {
			st.Todos = append(st.Todos, session.Todo{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm})
		}
		for _, t := range c.deps.Agent.Tasks() {
			st.Tasks = append(st.Tasks, session.Task{ID: t.ID, Subject: t.Subject, Description: t.Description, Status: string(t.Status)})
		}
	}
	if err := session.SaveState(path, st); err != nil {
		return Result{}, err
	}
	return Result{Output: fmt.Sprintf("branch saved: %s\nresume it later with: woca --resume %s\n(the current conversation continues unchanged)\n", path, path)}, nil
}

package console

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/session"
)

// exportCmd implements /export: it writes the conversation to a file —
// markdown transcript by default, or the resumable session JSON when the path
// ends in .json.
func (c *Console) exportCmd(args []string, s *Session) (Result, error) {
	if len(s.History) == 0 {
		return Result{}, fmt.Errorf("nothing to export (the conversation is empty)")
	}
	path := "woca-session-" + time.Now().Format("20060102-150405") + ".md"
	if len(args) > 0 {
		path = args[0]
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(c.deps.Cfg.WorkDir, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return Result{}, err
	}

	if strings.EqualFold(filepath.Ext(path), ".json") {
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
		return Result{Output: fmt.Sprintf("exported %d messages (resumable JSON) to %s\n", len(s.History), path)}, nil
	}

	if err := os.WriteFile(path, []byte(renderTranscript(s.History)), 0o644); err != nil {
		return Result{}, err
	}
	return Result{Output: fmt.Sprintf("exported %d messages (markdown) to %s\n", len(s.History), path)}, nil
}

// renderTranscript formats a conversation as readable markdown.
func renderTranscript(history []messages.Message) string {
	var b strings.Builder
	b.WriteString("# woca session transcript\n\n")
	for _, m := range history {
		switch m.Role {
		case messages.RoleHuman:
			fmt.Fprintf(&b, "## User\n\n%s\n\n", strings.TrimSpace(m.Content))
		case messages.RoleAI:
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					fmt.Fprintf(&b, "**Assistant tool call:** `%s(%s)`\n\n", tc.Name, preview(fmt.Sprint(tc.Args), 120))
				}
			}
			if txt := strings.TrimSpace(m.Content); txt != "" {
				fmt.Fprintf(&b, "## Assistant\n\n%s\n\n", txt)
			}
		case messages.RoleTool:
			fmt.Fprintf(&b, "> tool result: %s\n\n", strings.TrimSpace(preview(m.Content, 400)))
		case messages.RoleSystem:
			// The system prompt is harness plumbing; skip it.
		}
	}
	return b.String()
}

// sanitizeSessionName maps arbitrary input onto a safe file-name fragment
// (letters, digits, dash, underscore; everything else collapses to dashes).
func sanitizeSessionName(raw string) string {
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		default:
			return '-'
		}
	}, raw)
	return strings.Trim(name, "-")
}

// renameCmd implements /rename: it saves the conversation as a named session
// snapshot (~/.woca/sessions/<name>.json, replacing any earlier snapshot with
// the same name) so it can be resumed by name instead of timestamp.
func (c *Console) renameCmd(args []string, s *Session) (Result, error) {
	if len(args) == 0 {
		return Result{}, fmt.Errorf("usage: /rename <name> (names the session snapshot for later --resume)")
	}
	if len(s.History) == 0 {
		return Result{}, fmt.Errorf("nothing to name yet (the conversation is empty)")
	}
	name := sanitizeSessionName(args[0])
	if name == "" {
		return Result{}, fmt.Errorf("invalid session name %q", args[0])
	}
	dir := c.deps.SessionDir
	if dir == "" {
		dir = filepath.Join(session.DefaultDir(), "sessions")
	}
	path := filepath.Join(dir, name+".json")
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
	return Result{Output: fmt.Sprintf("session named %q\nresume later with: woca --resume %s\n(subsequent /rename or /branch calls refresh the snapshot)\n", name, path)}, nil
}

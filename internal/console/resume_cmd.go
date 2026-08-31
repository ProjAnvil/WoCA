package console

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/session"
	"github.com/projanvil/woca/internal/tools"
)

// /resume: list and load resumable session snapshots, so switching history
// does not require restarting woca with --resume. The snapshot directory and
// message format are the ones --resume already reads (~/.woca/sessions/
// sessions/<name>.json plus the last-session.jsonl tail), so the two entry
// points stay interchangeable.

// SessionEntry is one resumable session snapshot.
type SessionEntry struct {
	Name    string // display name (file base without extension)
	Path    string // absolute path
	ModTime time.Time
	Size    int64
}

// sessionsDir resolves the snapshot directory (deps override for tests).
func (c *Console) sessionsDir() string {
	if c.deps.SessionDir != "" {
		return c.deps.SessionDir
	}
	return filepath.Join(session.DefaultDir(), "sessions")
}

// ListSessions enumerates resumable snapshots: the named sessions directory
// plus the last-session tail, newest first.
func (c *Console) ListSessions() ([]SessionEntry, error) {
	var out []SessionEntry
	if files, err := os.ReadDir(c.sessionsDir()); err == nil {
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".json") {
				continue
			}
			info, err := f.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(c.sessionsDir(), f.Name())
			out = append(out, SessionEntry{
				Name:    strings.TrimSuffix(f.Name(), ".json"),
				Path:    path,
				ModTime: info.ModTime(),
				Size:    info.Size(),
			})
		}
	}
	if last := filepath.Join(session.DefaultDir(), "last-session.jsonl"); c.deps.SessionDir == "" {
		if info, err := os.Stat(last); err == nil {
			out = append(out, SessionEntry{
				Name:    "last-session",
				Path:    last,
				ModTime: info.ModTime(),
				Size:    info.Size(),
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.After(out[j].ModTime) })
	return out, nil
}

// resumeCmd implements /resume: no args lists snapshots, /resume <name>
// loads one into the live conversation (history, plan mode, todos, tasks).
func (c *Console) resumeCmd(args []string, s *Session) (Result, error) {
	if len(args) == 0 {
		entries, err := c.ListSessions()
		if err != nil {
			return Result{}, err
		}
		if len(entries) == 0 {
			return Result{Output: "no saved sessions (name one with /rename <name>)\n"}, nil
		}
		var b strings.Builder
		b.WriteString("saved sessions (newest first):\n")
		for _, e := range entries {
			fmt.Fprintf(&b, "  %-24s %s\n", e.Name, e.ModTime.Format("2006-01-02 15:04"))
		}
		b.WriteString("resume with: /resume <name>\n")
		return Result{Output: b.String()}, nil
	}
	name := sanitizeSessionName(args[0])
	if name == "" {
		return Result{}, fmt.Errorf("invalid session name %q", args[0])
	}
	path := filepath.Join(c.sessionsDir(), name+".json")
	if _, err := os.Stat(path); err != nil {
		// The last-session tail lives outside the snapshot directory.
		if name == "last-session" {
			path = filepath.Join(session.DefaultDir(), "last-session.jsonl")
		}
		if _, err := os.Stat(path); err != nil {
			return Result{}, fmt.Errorf("no session named %q (try /resume to list)", args[0])
		}
	}
	st, err := session.LoadState(path)
	if err != nil {
		return Result{}, fmt.Errorf("resume %s: %w", name, err)
	}
	if len(st.Messages) == 0 {
		return Result{}, fmt.Errorf("session %q is empty", name)
	}
	s.History = st.Messages
	if c.deps.Agent != nil {
		c.deps.Agent.SetPlanMode(st.PlanMode)
		c.deps.Agent.SetTodos(todosFromSession(st.Todos))
		c.deps.Agent.SetTasks(tasksFromSession(st.Tasks))
	}
	return Result{
		Output: fmt.Sprintf("resumed %q (%d messages) — /clear to reset\n", name, len(st.Messages)),
		Clear:  true,
	}, nil
}

// todosFromSession maps persisted todos back to agent todos (the same
// mapping the cmd/woca --resume path applies).
func todosFromSession(todos []session.Todo) []tools.Todo {
	out := make([]tools.Todo, 0, len(todos))
	for _, t := range todos {
		out = append(out, tools.Todo{Content: t.Content, Status: t.Status, ActiveForm: t.ActiveForm})
	}
	return out
}

// tasksFromSession maps persisted tasks back to agent task records.
func tasksFromSession(tasks []session.Task) []agent.TaskRecord {
	out := make([]agent.TaskRecord, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, agent.TaskRecord{ID: t.ID, Subject: t.Subject, Description: t.Description, Status: agent.TaskRecordStatus(t.Status)})
	}
	return out
}

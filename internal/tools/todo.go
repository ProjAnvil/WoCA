package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Todo is a single todo-list entry (mirrors Claude Code's TodoWrite shape).
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed | cancelled
	ActiveForm string `json:"active_form,omitempty"`
}

// TodoStore is a session-scoped, concurrency-safe todo list shared across all
// TodoWrite invocations.
type TodoStore struct {
	mu    sync.Mutex
	todos []Todo
}

// NewTodoStore creates an empty todo store.
func NewTodoStore() *TodoStore { return &TodoStore{} }

// Set replaces the whole list.
func (s *TodoStore) Set(todos []Todo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.todos = append([]Todo(nil), todos...)
}

// Merge updates entries with matching content and appends new ones.
func (s *TodoStore) Merge(todos []Todo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	byContent := map[string]int{}
	for i, t := range s.todos {
		byContent[t.Content] = i
	}
	for _, t := range todos {
		if idx, ok := byContent[t.Content]; ok {
			s.todos[idx] = t
		} else {
			byContent[t.Content] = len(s.todos)
			s.todos = append(s.todos, t)
		}
	}
}

// Get returns a copy of the current list.
func (s *TodoStore) Get() []Todo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Todo(nil), s.todos...)
}

func formatTodos(todos []Todo) string {
	if len(todos) == 0 {
		return "(no todos)"
	}
	var b strings.Builder
	for _, t := range todos {
		mark := " "
		switch t.Status {
		case "in_progress":
			mark = ">"
		case "completed":
			mark = "x"
		case "cancelled":
			mark = "-"
		}
		text := t.Content
		if t.ActiveForm != "" {
			text = t.ActiveForm
		}
		fmt.Fprintf(&b, "[%s] %s\n", mark, text)
	}
	return b.String()
}

const todoWriteDescription = `Create or update the todo list. Each item has content and a status (pending, in_progress, completed, or cancelled). By default the list is replaced; set merge to update/append. Use this to plan multi-step work.`

type todoWriteArgs struct {
	Todos []Todo `json:"todos"`
	Merge bool   `json:"merge,omitempty"`
}

func todoWriteTool(store *TodoStore) coretools.Tool {
	t, err := coretools.FromFunc("TodoWrite", todoWriteDescription,
		func(ctx context.Context, a todoWriteArgs) (coretools.Result, error) {
			if a.Merge {
				store.Merge(a.Todos)
			} else {
				store.Set(a.Todos)
			}
			return coretools.Result{Content: formatTodos(store.Get())}, nil
		})
	if err != nil {
		panic(err)
	}
	return t
}

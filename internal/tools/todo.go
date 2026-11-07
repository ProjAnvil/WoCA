package tools

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"
)

// Todo is a single todo-list entry for the TodoWrite tool.
type Todo struct {
	Content    string `json:"content"`
	Status     string `json:"status"` // pending | in_progress | completed | cancelled
	ActiveForm string `json:"active_form,omitempty"`
}

// TodoStore is a concurrency-safe todo store with one list per run-scope
// session id. TodoWrite resolves the session from its context and only ever
// reads and writes that bucket, so the VisibleTo rule holds by construction
// (owner == viewer on every tool call) and a scoped session sees exactly its
// own list. The "" bucket carries today's single-session semantics for
// TUI/REPL/CLI callers, which have no run scope. Each Registry owns one
// store; subagents get their own registry and therefore their own store, so
// their todos never surface in another session's panel.
type TodoStore struct {
	mu       sync.Mutex
	lists    map[string]*todoList // keyed by run-scope session; "" = TUI/REPL
	onChange func(session string)
}

// todoList is one session's list plus its last-write time.
type todoList struct {
	items     []Todo
	updatedAt time.Time
}

// NewTodoStore creates an empty todo store.
func NewTodoStore() *TodoStore {
	return &TodoStore{lists: map[string]*todoList{}}
}

// Set replaces the session's whole list. Setting an empty list clears it
// (the session keeps ok=true from Get: "cleared" stays distinct from
// "never written").
func (s *TodoStore) Set(session string, todos []Todo) {
	s.mu.Lock()
	l := s.listFor(session)
	l.items = append([]Todo(nil), todos...)
	l.updatedAt = time.Now()
	fn := s.onChange
	s.mu.Unlock()
	s.notify(fn, session)
}

// Merge updates the session's entries with matching content and appends new
// ones; it never touches another session's list.
func (s *TodoStore) Merge(session string, todos []Todo) {
	s.mu.Lock()
	l := s.listFor(session)
	byContent := map[string]int{}
	for i, t := range l.items {
		byContent[t.Content] = i
	}
	for _, t := range todos {
		if idx, ok := byContent[t.Content]; ok {
			l.items[idx] = t
		} else {
			byContent[t.Content] = len(l.items)
			l.items = append(l.items, t)
		}
	}
	l.updatedAt = time.Now()
	fn := s.onChange
	s.mu.Unlock()
	s.notify(fn, session)
}

// Get returns a copy of the session's list and its last-update time.
// ok=false means this session has no list yet, distinguishing "never
// written" from "cleared to empty".
func (s *TodoStore) Get(session string) (todos []Todo, updatedAt time.Time, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	l, ok := s.lists[session]
	if !ok {
		return nil, time.Time{}, false
	}
	return append([]Todo(nil), l.items...), l.updatedAt, true
}

// Drop forgets the session's list (session deletion); it is a no-op for
// sessions that never wrote one.
func (s *TodoStore) Drop(session string) {
	s.mu.Lock()
	_, existed := s.lists[session]
	delete(s.lists, session)
	fn := s.onChange
	s.mu.Unlock()
	if existed {
		s.notify(fn, session)
	}
}

// SetOnChange installs the change callback the activity seam subscribes to.
// It fires synchronously after every Set, Merge, and Drop that changed the
// store, with the owner session id; it is called outside the store lock and
// must not block.
func (s *TodoStore) SetOnChange(fn func(session string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onChange = fn
}

// listFor returns the session's list, creating it on first write. Callers
// must hold s.mu.
func (s *TodoStore) listFor(session string) *todoList {
	l := s.lists[session]
	if l == nil {
		l = &todoList{}
		s.lists[session] = l
	}
	return l
}

// notify invokes the change callback outside the store lock.
func (s *TodoStore) notify(fn func(session string), session string) {
	if fn != nil {
		fn(session)
	}
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
	t := MustTool(coretools.FromFunc("TodoWrite", todoWriteDescription,
		func(ctx context.Context, a todoWriteArgs) (coretools.Result, error) {
			// Todos are per-session state: the write lands in — and the
			// echo reads — only the calling run's session bucket, so the
			// merge echo can never leak another session's items. Outside a
			// run scope (TUI/REPL) the "" bucket keeps today's behavior.
			session := sessionFromCtx(ctx)
			if a.Merge {
				store.Merge(session, a.Todos)
			} else {
				store.Set(session, a.Todos)
			}
			todos, _, _ := store.Get(session)
			return coretools.Result{Content: formatTodos(todos)}, nil
		}))
	return t
}

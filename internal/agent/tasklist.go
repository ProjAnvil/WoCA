package agent

import (
	"fmt"
	"sort"
	"sync"
)

// TaskRecordStatus is the lifecycle state of a listed task.
type TaskRecordStatus string

const (
	TaskRecordPending    TaskRecordStatus = "pending"
	TaskRecordInProgress TaskRecordStatus = "in_progress"
	TaskRecordCompleted  TaskRecordStatus = "completed"
	TaskRecordCancelled  TaskRecordStatus = "cancelled"
)

// TaskRecord is one entry in the task list (Claude Code's TaskCreate/Get/
// Update/List family).
type TaskRecord struct {
	ID          string           `json:"id"`
	Subject     string           `json:"subject"`
	Description string           `json:"description"`
	ActiveForm  string           `json:"active_form,omitempty"`
	Status      TaskRecordStatus `json:"status"`
}

// TaskList is a session-scoped, concurrency-safe task list.
type TaskList struct {
	mu    sync.Mutex
	tasks map[string]TaskRecord
	next  int
}

func newTaskList() *TaskList {
	return &TaskList{tasks: map[string]TaskRecord{}}
}

// Create adds a task and returns it with its ID and default pending status.
func (l *TaskList) Create(subject, description, activeForm string) TaskRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	t := TaskRecord{
		ID:          fmt.Sprintf("task-%d", l.next),
		Subject:     subject,
		Description: description,
		ActiveForm:  activeForm,
		Status:      TaskRecordPending,
	}
	l.tasks[t.ID] = t
	return t
}

// Get returns a task by ID.
func (l *TaskList) Get(id string) (TaskRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tasks[id]
	return t, ok
}

// Update mutates a task. Zero-value fields are left unchanged (status must be
// explicitly set).
func (l *TaskList) Update(id string, status TaskRecordStatus, subject, description string) (TaskRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tasks[id]
	if !ok {
		return TaskRecord{}, false
	}
	if status != "" {
		t.Status = status
	}
	if subject != "" {
		t.Subject = subject
	}
	if description != "" {
		t.Description = description
	}
	l.tasks[id] = t
	return t, true
}

// List returns all tasks sorted by ID.
func (l *TaskList) List() []TaskRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]TaskRecord, 0, len(l.tasks))
	for _, t := range l.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Restore replaces the list with a persisted snapshot, recomputing the next
// counter from the numeric task-ID suffixes.
func (l *TaskList) Restore(tasks []TaskRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tasks = map[string]TaskRecord{}
	l.next = 0
	for _, t := range tasks {
		l.tasks[t.ID] = t
		var n int
		if _, err := fmt.Sscanf(t.ID, "task-%d", &n); err == nil && n > l.next {
			l.next = n
		}
	}
}

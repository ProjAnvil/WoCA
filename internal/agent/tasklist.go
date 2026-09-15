package agent

import (
	"cmp"
	"fmt"
	"slices"
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

// TaskRecord is one entry in the task list.
type TaskRecord struct {
	ID          string           `json:"id"`
	Subject     string           `json:"subject"`
	Description string           `json:"description"`
	ActiveForm  string           `json:"active_form,omitempty"`
	Status      TaskRecordStatus `json:"status"`
	// Owner records who claimed an in_progress task (teammate name or
	// "lead"); empty means unassigned/claimable.
	Owner string `json:"owner,omitempty"`
	// DependsOn lists task IDs that must be completed before this task can
	// be claimed (agent-team coordination).
	DependsOn []string `json:"depends_on,omitempty"`
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
	return l.CreateDeps(subject, description, activeForm, nil)
}

// CreateDeps adds a task with dependency IDs (agent-team coordination).
func (l *TaskList) CreateDeps(subject, description, activeForm string, dependsOn []string) TaskRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.next++
	t := TaskRecord{
		ID:          fmt.Sprintf("task-%d", l.next),
		Subject:     subject,
		Description: description,
		ActiveForm:  activeForm,
		Status:      TaskRecordPending,
		DependsOn:   dependsOn,
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
	slices.SortFunc(out, func(a, b TaskRecord) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// Claim atomically takes ownership of a pending, unowned task on behalf of
// owner (sets status in_progress). It fails when the task is missing, not
// pending, already owned, or has incomplete dependencies. Dependencies are
// checked against the list's own locked state — never via a caller callback,
// which would re-enter the mutex and deadlock.
func (l *TaskList) Claim(id, owner string) (TaskRecord, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tasks[id]
	if !ok || t.Status != TaskRecordPending || t.Owner != "" {
		return TaskRecord{}, false
	}
	for _, dep := range t.DependsOn {
		d, ok := l.tasks[dep]
		if !ok || d.Status != TaskRecordCompleted {
			return TaskRecord{}, false
		}
	}
	t.Status = TaskRecordInProgress
	t.Owner = owner
	l.tasks[id] = t
	return t, true
}

// ReleaseForOwner returns a claimed task to the pending pool, but only when
// it is still owned by owner and still in progress. The owner and status are
// re-checked under the lock so a task the lead completed between the
// caller's snapshot and this call is never reverted to pending.
func (l *TaskList) ReleaseForOwner(id, owner string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	t, ok := l.tasks[id]
	if !ok || t.Owner != owner || t.Status != TaskRecordInProgress {
		return false
	}
	t.Status = TaskRecordPending
	t.Owner = ""
	l.tasks[id] = t
	return true
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

package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/skills"
)

// TaskStatus is the lifecycle state of a background task.
type TaskStatus string

const (
	TaskRunning   TaskStatus = "running"
	TaskCompleted TaskStatus = "completed"
	TaskStopped   TaskStatus = "stopped"
)

// backgroundTask is a Task (subagent) running in the background.
type backgroundTask struct {
	ID          string
	Subject     string
	Description string

	mu     sync.Mutex
	status TaskStatus
	output []string
	final  string
	cancel context.CancelFunc
}

func (t *backgroundTask) appendOutput(line string) {
	if line == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, part := range strings.Split(line, "\n") {
		if part != "" {
			t.output = append(t.output, part)
		}
	}
	if len(t.output) > 200 {
		t.output = t.output[len(t.output)-200:]
	}
}

func (t *backgroundTask) snapshot() (TaskStatus, string, string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.status, strings.Join(t.output, "\n"), t.final
}

func (t *backgroundTask) setFinal(status TaskStatus, final string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.status = status
	t.final = final
}

// TaskManager holds all background tasks for a session.
type TaskManager struct {
	mu    sync.Mutex
	tasks map[string]*backgroundTask
	next  int
}

func newTaskManager() *TaskManager {
	return &TaskManager{tasks: map[string]*backgroundTask{}}
}

func (m *TaskManager) start(subject, description string) *backgroundTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next++
	t := &backgroundTask{
		ID:          fmt.Sprintf("task-%d", m.next),
		Subject:     subject,
		Description: description,
		status:      TaskRunning,
	}
	m.tasks[t.ID] = t
	return t
}

func (m *TaskManager) get(id string) (*backgroundTask, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.tasks[id]
	return t, ok
}

func (m *TaskManager) list() []*backgroundTask {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*backgroundTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

const taskDescription = `Launch a subagent to handle a self-contained piece of work and return a concise report. Set run_in_background to run it asynchronously (poll with TaskOutput, stop with TaskStop). Set fork=true to copy the current conversation into the subagent first — it starts with your full context instead of a blank slate, which fits "continue this thread in parallel" tasks.`

type taskArgs struct {
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	SubagentType    string `json:"subagent_type,omitempty"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
	Fork            bool   `json:"fork,omitempty"`
}

const taskOutputDescription = `Fetch the current output and status of a background task launched with the Task tool.`

type taskOutputArgs struct {
	TaskID string `json:"task_id"`
}

const taskStopDescription = `Stop a running background task launched with the Task tool.`

type taskStopArgs struct {
	TaskID string `json:"task_id"`
}

// taskTools builds the Task / TaskOutput / TaskStop tools, wired to the agent's
// subagent runner and task manager.
func (a *Agent) taskTools() []coretools.Tool {
	task, err := coretools.FromFunc("Task", taskDescription, a.runTask)
	if err != nil {
		panic(err)
	}
	taskOutput, err := coretools.FromFunc("TaskOutput", taskOutputDescription, a.taskOutput)
	if err != nil {
		panic(err)
	}
	taskStop, err := coretools.FromFunc("TaskStop", taskStopDescription, a.taskStop)
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{task, taskOutput, taskStop}
}

func (a *Agent) runTask(ctx context.Context, args taskArgs) (coretools.Result, error) {
	if args.Description == "" && args.Prompt == "" {
		return coretools.Result{}, fmt.Errorf("Task requires a description or prompt")
	}
	prompt := args.Prompt
	if prompt == "" {
		prompt = args.Description
	}

	var def *agents.Definition
	if args.SubagentType != "" {
		d, ok := agents.Find(a.agentDefs, args.SubagentType)
		if !ok {
			return coretools.Result{}, fmt.Errorf("unknown subagent_type %q", args.SubagentType)
		}
		def = &d
	}

	history := []messages.Message{messages.Human(prompt)}
	if args.Fork {
		// forkSubagent: the subagent inherits the parent's live transcript
		// (minus the system prompt) and continues with the task at hand.
		base := a.transcriptSnapshot()
		for len(base) > 0 && base[0].Role == messages.RoleSystem {
			base = base[1:]
		}
		history = append(base, messages.Human(forkTaskPrompt(prompt)))
	}

	if args.RunInBackground {
		t := a.tasks.start(args.Description, prompt)
		runCtx, cancel := context.WithCancel(ctx)
		t.cancel = cancel
		go func() {
			report, err := a.runSubagent(runCtx, history, func(text string) { t.appendOutput(text) }, def)
			if err != nil {
				report = "Error: " + err.Error()
			}
			t.setFinal(TaskCompleted, report)
		}()
		return coretools.Result{Content: fmt.Sprintf("Started background task %s: %s", t.ID, args.Description)}, nil
	}

	report, err := a.runSubagent(ctx, history, nil, def)
	if err != nil {
		return coretools.Result{}, err
	}
	return coretools.Result{Content: report}, nil
}

// forkTaskPrompt wraps the fork directive so the subagent knows it inherited
// the conversation above.
func forkTaskPrompt(prompt string) string {
	return "<task-fork>\nYou are a fork of the conversation above and inherit its full context. Complete the following task yourself:\n" + prompt + "\n</task-fork>"
}

func (a *Agent) taskOutput(ctx context.Context, args taskOutputArgs) (coretools.Result, error) {
	t, ok := a.tasks.get(args.TaskID)
	if !ok {
		return coretools.Result{}, fmt.Errorf("no task with id %q", args.TaskID)
	}
	status, output, final := t.snapshot()
	switch status {
	case TaskRunning:
		if output == "" {
			return coretools.Result{Content: fmt.Sprintf("Task %s is still running (no output yet).", t.ID)}, nil
		}
		return coretools.Result{Content: fmt.Sprintf("Task %s is still running. Output so far:\n%s", t.ID, output)}, nil
	case TaskStopped:
		return coretools.Result{Content: fmt.Sprintf("Task %s was stopped.", t.ID)}, nil
	default:
		return coretools.Result{Content: final}, nil
	}
}

func (a *Agent) taskStop(ctx context.Context, args taskStopArgs) (coretools.Result, error) {
	t, ok := a.tasks.get(args.TaskID)
	if !ok {
		return coretools.Result{}, fmt.Errorf("no task with id %q", args.TaskID)
	}
	if t.cancel != nil {
		t.cancel()
	}
	t.setFinal(TaskStopped, "")
	return coretools.Result{Content: fmt.Sprintf("Stopped task %s.", t.ID)}, nil
}

// runSubagent runs a subagent to completion from the given history. If onText
// is non-nil it streams model text into the callback (used for background
// task output). Returns the subagent's final assistant text.
func (a *Agent) runSubagent(ctx context.Context, history []messages.Message, onText func(string), def *agents.Definition) (string, error) {
	sub := a.newSubagent(def)
	if onText == nil {
		msgs, err := sub.Invoke(ctx, history)
		if err != nil {
			return "", err
		}
		return lastAIText(msgs), nil
	}

	stream, err := sub.Graph().Stream(ctx, sub.NewState(history))
	if err != nil {
		return "", err
	}
	defer stream.Close()
	final := ""
	for {
		step, ok, err := stream.Next(ctx)
		if err != nil {
			return "", err
		}
		if !ok {
			break
		}
		if step.Done {
			for _, m := range MessagesFrom(step.Values) {
				if m.Role == messages.RoleAI && m.Content != "" {
					final = m.Content
				}
			}
			break
		}
		if step.Err != nil {
			return "", step.Err
		}
		for _, m := range MessagesFrom(step.Update) {
			if m.Role == messages.RoleAI {
				if text := strings.TrimSpace(m.Content); text != "" {
					onText(text)
				}
				for _, tc := range m.ToolCalls {
					onText(fmt.Sprintf("  %s(%v)", tc.Name, tc.Args))
				}
			}
		}
	}
	return final, nil
}

func lastAIText(msgs []messages.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleAI && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// skillTool exposes loaded skills via the Skill tool.
func (a *Agent) skillTool() []coretools.Tool {
	if len(a.skills) == 0 {
		return nil
	}
	desc := "Execute a skill by name, expanding it into instructions to follow. Available skills:\n" + skillList(a.skills)
	t, err := coretools.FromFunc("Skill", desc,
		func(ctx context.Context, args skillArgs) (coretools.Result, error) {
			s, ok := skills.Find(a.skills, args.Skill)
			if !ok {
				return coretools.Result{}, fmt.Errorf("unknown skill %q", args.Skill)
			}
			return coretools.Result{Content: skills.Expand(s, args.Args)}, nil
		})
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{t}
}

type skillArgs struct {
	Skill string         `json:"skill"`
	Args  map[string]any `json:"args,omitempty"`
}

func skillList(list []skills.Skill) string {
	var b strings.Builder
	for _, s := range list {
		fmt.Fprintf(&b, "- %s: %s\n", s.Name, s.Description)
	}
	return b.String()
}

// taskListTools exposes TaskCreate/Get/Update/List over the task list.
func (a *Agent) taskListTools() []coretools.Tool {
	create, err := coretools.FromFunc("TaskCreate", "Create a task in the task list.", a.taskCreate)
	if err != nil {
		panic(err)
	}
	get, err := coretools.FromFunc("TaskGet", "Get a task by id.", a.taskGet)
	if err != nil {
		panic(err)
	}
	update, err := coretools.FromFunc("TaskUpdate", "Update a task's status, subject, or description.", a.taskUpdate)
	if err != nil {
		panic(err)
	}
	list, err := coretools.FromFunc("TaskList", "List all tasks in the task list.", a.runTaskList)
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{create, get, update, list}
}

type taskCreateArgs struct {
	Subject     string `json:"subject"`
	Description string `json:"description"`
	ActiveForm  string `json:"active_form,omitempty"`
}

func (a *Agent) taskCreate(ctx context.Context, args taskCreateArgs) (coretools.Result, error) {
	rec := a.taskList.Create(args.Subject, args.Description, args.ActiveForm)
	return coretools.Result{Content: fmt.Sprintf("Created task %s: %s", rec.ID, rec.Subject)}, nil
}

type taskGetArgs struct {
	TaskID string `json:"task_id"`
}

func (a *Agent) taskGet(ctx context.Context, args taskGetArgs) (coretools.Result, error) {
	rec, ok := a.taskList.Get(args.TaskID)
	if !ok {
		return coretools.Result{}, fmt.Errorf("no task with id %q", args.TaskID)
	}
	return coretools.Result{Content: formatTaskRecord(rec)}, nil
}

type taskUpdateArgs struct {
	TaskID      string `json:"task_id"`
	Status      string `json:"status,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Description string `json:"description,omitempty"`
}

func (a *Agent) taskUpdate(ctx context.Context, args taskUpdateArgs) (coretools.Result, error) {
	rec, ok := a.taskList.Update(args.TaskID, TaskRecordStatus(args.Status), args.Subject, args.Description)
	if !ok {
		return coretools.Result{}, fmt.Errorf("no task with id %q", args.TaskID)
	}
	return coretools.Result{Content: formatTaskRecord(rec)}, nil
}

type taskListArgs struct{}

func (a *Agent) runTaskList(ctx context.Context, args taskListArgs) (coretools.Result, error) {
	recs := a.taskList.List()
	if len(recs) == 0 {
		return coretools.Result{Content: "(no tasks)"}, nil
	}
	var b strings.Builder
	for _, rec := range recs {
		b.WriteString(formatTaskRecord(rec))
		b.WriteString("\n")
	}
	return coretools.Result{Content: b.String()}, nil
}

func formatTaskRecord(t TaskRecord) string {
	return fmt.Sprintf("[%s] %s: %s", t.ID, t.Status, t.Subject)
}

// sendMessageTool exposes SendMessage over the message bus (team coordination
// plumbing). It is a no-op error when no bus is configured.
func (a *Agent) sendMessageTool() []coretools.Tool {
	if a.bus == nil {
		return nil
	}
	t, err := coretools.FromFunc("SendMessage", "Send a message to another agent/recipient on the shared message bus.", a.sendMessage)
	if err != nil {
		panic(err)
	}
	return []coretools.Tool{t}
}

type sendMessageArgs struct {
	Recipient string `json:"recipient"`
	Content   string `json:"content"`
}

func (a *Agent) sendMessage(ctx context.Context, args sendMessageArgs) (coretools.Result, error) {
	if args.Recipient == "" {
		return coretools.Result{}, fmt.Errorf("recipient is required")
	}
	a.bus.Send("main", args.Recipient, args.Content)
	return coretools.Result{Content: fmt.Sprintf("Sent message to %s.", args.Recipient)}, nil
}

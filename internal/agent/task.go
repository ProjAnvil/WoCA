package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
)

// toolCallIDKey carries the executing tool call's ID through ctx so the Task
// tool can derive a stable subagent thread across interrupt re-executions.
type toolCallIDKey struct{}

func withToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, id)
}

func toolCallIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(toolCallIDKey{}).(string)
	return v
}

// deriveSubagentThread mints the subagent's checkpoint thread: stable across
// the parent toolsNode's interrupt re-executions (same replayed tool call),
// unique per call.
func deriveSubagentThread(parentThread, callID, prompt string) string {
	sum := sha256.Sum256([]byte(prompt))
	digest := hex.EncodeToString(sum[:8])
	if callID == "" {
		return parentThread + "/sub-" + digest
	}
	return parentThread + "/sub-" + callID + "-" + digest
}

// subagentStarted reports whether the derived thread already has a
// checkpoint on the shared saver — i.e. this handler execution is a
// re-entry after a parent-graph resume, not a fresh spawn.
func (a *Agent) subagentStarted(ctx context.Context, sub *Agent, thread string) bool {
	snap, err := sub.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: thread})
	if err != nil {
		return false
	}
	return len(snap.Values) > 0 || len(snap.Interrupts) > 0
}

// subagentPendingPermission decodes the permission request the child is
// paused on, so a re-entering relay can rebuild an accurate payload.
func subagentPendingPermission(ctx context.Context, sub *Agent, thread string) (PermissionRequest, bool) {
	snap, err := sub.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: thread})
	if err != nil || len(snap.Interrupts) == 0 {
		return PermissionRequest{}, false
	}
	req, ok := decodeInterruptPayload(snap.Interrupts[0].Value).(PermissionRequest)
	return req, ok
}

// detachStreamCtx hides the fork's stream emitter from a nested graph run.
// The emitter travels via an unexported context key, so a foreground
// subagent invoked with the tool goroutine's ctx would otherwise emit its
// chunks — including its RAW pause interrupts — into the parent run's
// stream, ahead of the wrapped SubagentInterrupt the relay raises; the
// harness surfaces the first interrupt chunk it sees, so the leak would
// defeat the relay. The key is matched by type identity via reflection (the
// module is pinned; the relay tests fail loudly if this stops matching).
type detachStreamCtx struct{ context.Context }

func (d detachStreamCtx) Value(key any) any {
	if t := reflect.TypeOf(key); t != nil &&
		t.Name() == "streamEmitterKey" &&
		t.PkgPath() == "github.com/projanvil/langchain-golang/langgraph/graph" {
		return nil
	}
	return d.Context.Value(key)
}

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

// setFinalIfRunning records a terminal state only while the task is still
// running: a completion arriving after TaskStop must not flip the stopped
// status back to completed.
func (t *backgroundTask) setFinalIfRunning(status TaskStatus, final string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.status != TaskRunning {
		return
	}
	t.status = status
	t.final = final
}

// setCancel stores the cancellation handle under the mutex; taskStop reads it
// from another goroutine.
func (t *backgroundTask) setCancel(cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel = cancel
}

// stop cancels the task and marks it stopped in one critical section, so a
// concurrent completion cannot interleave.
func (t *backgroundTask) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cancel != nil {
		t.cancel()
	}
	if t.status != TaskRunning {
		return
	}
	t.status = TaskStopped
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

// maxBackgroundTasks caps concurrently running background tasks.
const maxBackgroundTasks = 16

func (m *TaskManager) start(subject, description string) (*backgroundTask, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	running := 0
	for _, t := range m.tasks {
		if st, _, _ := t.snapshot(); st == TaskRunning {
			running++
		}
	}
	if running >= maxBackgroundTasks {
		return nil, fmt.Errorf("已达后台 task 上限（%d 个运行中）：请先等待其完成或用 TaskStop 停止部分 task", maxBackgroundTasks)
	}
	m.next++
	t := &backgroundTask{
		ID:          fmt.Sprintf("task-%d", m.next),
		Subject:     subject,
		Description: description,
		status:      TaskRunning,
	}
	m.tasks[t.ID] = t
	return t, nil
}

// RunningCount reports running background tasks (for /api/stats).
func (m *TaskManager) RunningCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, t := range m.tasks {
		if st, _, _ := t.snapshot(); st == TaskRunning {
			n++
		}
	}
	return n
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
	task := tools.MustTool(coretools.FromFunc("Task", taskDescription, a.runTask))
	taskOutput := tools.MustTool(coretools.FromFunc("TaskOutput", taskOutputDescription, a.taskOutput))
	taskStop := tools.MustTool(coretools.FromFunc("TaskStop", taskStopDescription, a.taskStop))
	return []coretools.Tool{task, taskOutput, taskStop}
}

func (a *Agent) runTask(ctx context.Context, args taskArgs) (coretools.Result, error) {
	if args.Description == "" && args.Prompt == "" {
		return coretools.Result{}, fmt.Errorf("task requires a description or prompt")
	}
	prompt := args.Prompt
	if prompt == "" {
		prompt = args.Description
	}

	var def *agents.Definition
	if args.SubagentType != "" {
		d, ok := agents.Find(a.agentsFor(ctx), args.SubagentType)
		if !ok {
			return coretools.Result{}, fmt.Errorf("unknown subagent_type %q", args.SubagentType)
		}
		def = &d
	}

	history := []messages.Message{messages.Human(prompt)}
	if args.Fork {
		// Subagent fork: the subagent inherits the parent's live transcript
		// (minus the system prompt) and continues with the task at hand.
		base := a.transcriptSnapshot()
		for len(base) > 0 && base[0].Role == messages.RoleSystem {
			base = base[1:]
		}
		history = append(base, messages.Human(forkTaskPrompt(prompt)))
	}

	if args.RunInBackground {
		t, err := a.tasks.start(args.Description, prompt)
		if err != nil {
			return coretools.Result{}, err
		}
		runCtx, cancel := context.WithCancel(ctx)
		t.setCancel(cancel)
		go func() {
			report, err := a.runSubagent(runCtx, history, func(text string) { t.appendOutput(text) }, def)
			if err != nil {
				report = "Error: " + err.Error()
			}
			t.setFinalIfRunning(TaskCompleted, report)
			// L1 episode for the completed subagent task. runCtx may already
			// be cancelled by TaskStop, but its values (run workspace) are
			// still readable; the write itself is detached.
			a.recordTaskEvent(runCtx, subagentName(def, args), args.Description, report, err)
		}()
		return coretools.Result{Content: fmt.Sprintf("Started background task %s: %s", t.ID, args.Description)}, nil
	}

	report, err := a.runForegroundTask(ctx, history, def, subagentName(def, args), prompt)
	if err != nil {
		return coretools.Result{}, err
	}
	a.recordTaskEvent(ctx, subagentName(def, args), args.Description, report, nil)
	return coretools.Result{Content: report}, nil
}

// subagentName resolves the display name for task events.
func subagentName(def *agents.Definition, args taskArgs) string {
	if def != nil && def.Name != "" {
		return def.Name
	}
	if args.SubagentType != "" {
		return args.SubagentType
	}
	return "general"
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
	t.stop()
	return coretools.Result{Content: fmt.Sprintf("Stopped task %s.", t.ID)}, nil
}

// runSubagent runs a subagent to completion from the given history. If onText
// is non-nil it streams model text into the callback (used for background
// task output). Returns the subagent's final assistant text.
func (a *Agent) runSubagent(ctx context.Context, history []messages.Message, onText func(string), def *agents.Definition) (string, error) {
	sub, err := a.newSubagent(ctx, def)
	if err != nil {
		return "", err
	}
	if onText == nil {
		msgs, err := sub.Invoke(ctx, history, NewThreadID())
		if err != nil {
			return "", err
		}
		return lastAIText(msgs), nil
	}

	// Streaming subagent run (background Task output): consume node
	// updates for complete AI messages and the final values snapshot.
	final := ""
	for chunk, err := range sub.Graph().Stream(ctx, sub.NewState(ctx, history), lggraph.StreamOptions{
		Options: lggraph.Options{ThreadID: NewThreadID()},
		Modes:   []lggraph.StreamMode{lggraph.StreamUpdates, lggraph.StreamValues},
	}) {
		if err != nil {
			return "", err
		}
		switch chunk.Mode {
		case lggraph.StreamUpdates:
			updates, _ := chunk.Payload.(map[string]any)
			for _, v := range updates {
				nodeUpdate, _ := v.(map[string]any)
				for _, m := range MessagesFrom(nodeUpdate) {
					if m.Role != messages.RoleAI {
						continue
					}
					if text := strings.TrimSpace(m.Content); text != "" {
						onText(text)
					}
					for _, tc := range m.ToolCalls {
						onText(fmt.Sprintf("  %s(%v)", tc.Name, tc.Args))
					}
				}
			}
		case lggraph.StreamValues:
			final = lastAIText(MessagesFrom(valuesState(chunk.Payload)))
		}
	}
	return final, nil
}

// relaySeq recovers how many resume values earlier re-executions of this
// relay consumed, read back from the SubagentInterrupt pending on the parent
// checkpoint. The fork's resume queue is positional — the persisted consumed
// prefix followed by this resume's fresh answer — and the prefix length is
// exactly the Seq the last surfaced SubagentInterrupt recorded. Missing or
// undecodable state degrades to 0 (correct for the first re-entry).
func (a *Agent) relaySeq(ctx context.Context, thread string) int {
	snap, err := a.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: a.LastThreadID()})
	if err != nil {
		return 0
	}
	for _, intr := range snap.Interrupts {
		if si, ok := decodeInterruptPayload(intr.Value).(SubagentInterrupt); ok && si.ThreadID == thread {
			return si.Seq
		}
	}
	return 0
}

// runForegroundTask runs a foreground subagent with interrupt relay: a
// permission pause inside the subagent re-interrupts the PARENT graph as a
// SubagentInterrupt, so the user approves it on the main session; on resume
// this handler re-executes, finds the child's checkpoint by its derived
// thread, and resumes it with the answer.
func (a *Agent) runForegroundTask(ctx context.Context, history []messages.Message, def *agents.Definition, name, prompt string) (string, error) {
	thread := deriveSubagentThread(a.LastThreadID(), toolCallIDFrom(ctx), prompt)
	sub, err := a.newSubagent(ctx, def)
	if err != nil {
		return "", err
	}
	// The child runs detached from the parent stream's emitter so its chunks
	// (above all its raw pause interrupts) never reach the harness; the relay
	// surfaces pauses itself, wrapped as SubagentInterrupt.
	subCtx := detachStreamCtx{ctx}
	started := a.subagentStarted(ctx, sub, thread)
	pending := SubagentInterrupt{Subagent: name, ToolCallID: toolCallIDFrom(ctx), ThreadID: thread}
	seq := 0
	if started {
		// Re-entry after a parent resume: rebuild the payload from the
		// child's paused checkpoint and fast-forward the positional resume
		// queue past the answers earlier re-executions already consumed, so
		// the loop's first Interrupt call reads THIS resume's answer.
		if req, ok := subagentPendingPermission(ctx, sub, thread); ok {
			pending.Inner = req
		}
		seq = a.relaySeq(ctx, thread)
		pending.Seq = seq
		lggraph.ReplayInterruptConsumption(ctx, seq)
	}
	for {
		var msgs []messages.Message
		if !started {
			msgs, err = sub.Invoke(subCtx, history, thread)
			started = true
		} else {
			// Collect the queued answer to the pause that restarted us.
			answer := decodeResumeValue(decodeInterruptPayload(lggraph.Interrupt(ctx, encodeInterruptPayload(pending))))
			seq++
			if !parseAnswer(answer) {
				_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
				return fmt.Sprintf("Subagent %q paused for a permission (%s) the user denied; the task was aborted.", name, pending.Inner.ToolName), nil
			}
			msgs, err = sub.Resume(subCtx, thread, "y")
		}
		var gi *lgtypes.GraphInterrupt
		if !errors.As(err, &gi) {
			if err == nil {
				_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
				return lastAIText(msgs), nil
			}
			return "", err
		}
		req, ok := decodeInterruptPayload(gi.Interrupt.Value).(PermissionRequest)
		if !ok {
			// Non-permission pauses (AskUser etc.) stay fatal for subagents.
			return "", err
		}
		pending.Inner = req
		pending.Seq = seq
		// Pause the parent graph. On the pause pass this call never returns
		// (panics GraphInterrupt); the re-execution restarts this function
		// and takes the started==true branch above. The return value is
		// deliberately unused: with one queued answer per resume, this call
		// is always the one that pauses.
		_ = lggraph.Interrupt(ctx, encodeInterruptPayload(pending))
	}
}

func valuesState(payload any) map[string]any {
	state, _ := payload.(map[string]any)
	return state
}

// NewThreadID mints a fresh checkpoint thread identifier.
func NewThreadID() string {
	return "run-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func lastAIText(msgs []messages.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == messages.RoleAI && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// skillTool exposes loaded skills via the Skill tool. The available-skill
// list is workspace-scoped (see skillsFor), so it lives in the system prompt
// (rebuilt per run) instead of this static description.
func (a *Agent) skillTool() []coretools.Tool {
	if len(a.skills) == 0 && a.skillsForFn == nil {
		return nil
	}
	desc := "Execute a skill by name, expanding it into instructions to follow. The system prompt lists the skills available in the current workspace."
	t := tools.MustTool(coretools.FromFunc("Skill", desc,
		func(ctx context.Context, args skillArgs) (coretools.Result, error) {
			s, ok := skills.Find(a.skillsFor(ctx), args.Skill)
			if !ok {
				return coretools.Result{}, fmt.Errorf("unknown skill %q", args.Skill)
			}
			return coretools.Result{Content: skills.Expand(s, args.Args)}, nil
		}))
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
	create := tools.MustTool(coretools.FromFunc("TaskCreate", "Create a task in the task list.", a.taskCreate))
	get := tools.MustTool(coretools.FromFunc("TaskGet", "Get a task by id.", a.taskGet))
	update := tools.MustTool(coretools.FromFunc("TaskUpdate", "Update a task's status, subject, or description.", a.taskUpdate))
	list := tools.MustTool(coretools.FromFunc("TaskList", "List all tasks in the task list.", a.runTaskList))
	return []coretools.Tool{create, get, update, list}
}

type taskCreateArgs struct {
	Subject     string   `json:"subject"`
	Description string   `json:"description"`
	ActiveForm  string   `json:"active_form,omitempty"`
	DependsOn   []string `json:"depends_on,omitempty"`
}

func (a *Agent) taskCreate(ctx context.Context, args taskCreateArgs) (coretools.Result, error) {
	list := a.currentTaskList()
	rec := list.CreateDeps(args.Subject, args.Description, args.ActiveForm, args.DependsOn)
	// New work must wake idle teammates: the spawn prompt promises "you
	// will be woken when there is work", and without the poke an all-idle
	// team would sleep until the idle timeout while the task sits pending.
	if team := a.currentTeam(); team != nil {
		for _, name := range team.memberNames() {
			team.poke(name)
		}
	}
	return coretools.Result{Content: fmt.Sprintf("Created task %s: %s", rec.ID, rec.Subject)}, nil
}

type taskGetArgs struct {
	TaskID string `json:"task_id"`
}

func (a *Agent) taskGet(ctx context.Context, args taskGetArgs) (coretools.Result, error) {
	rec, ok := a.currentTaskList().Get(args.TaskID)
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
	list := a.currentTaskList()
	if existing, ok := list.Get(args.TaskID); ok && a.currentTeam() != nil {
		// Team scope: ownership is enforced. Claiming happens by moving an
		// unowned pending task to in_progress; a task owned by someone else
		// is only editable by the lead.
		if existing.Owner != "" && existing.Owner != a.agentName && a.agentName != LeadName {
			return coretools.Result{}, fmt.Errorf("task %s is owned by %s", args.TaskID, existing.Owner)
		}
		if TaskRecordStatus(args.Status) == TaskRecordInProgress && existing.Owner == "" && existing.Status == TaskRecordPending {
			claimed, ok := list.Claim(args.TaskID, a.agentName)
			if !ok {
				return coretools.Result{}, fmt.Errorf("task %s cannot be claimed (dependencies incomplete or already taken)", args.TaskID)
			}
			return coretools.Result{Content: formatTaskRecord(claimed)}, nil
		}
	}
	rec, ok := list.Update(args.TaskID, TaskRecordStatus(args.Status), args.Subject, args.Description)
	if !ok {
		return coretools.Result{}, fmt.Errorf("no task with id %q", args.TaskID)
	}
	return coretools.Result{Content: formatTaskRecord(rec)}, nil
}

type taskListArgs struct{}

func (a *Agent) runTaskList(ctx context.Context, args taskListArgs) (coretools.Result, error) {
	recs := a.currentTaskList().List()
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
	if t.Owner != "" {
		return fmt.Sprintf("[%s] %s (%s): %s", t.ID, t.Status, t.Owner, t.Subject)
	}
	return fmt.Sprintf("[%s] %s: %s", t.ID, t.Status, t.Subject)
}

// sendMessageTool exposes SendMessage over the message bus (team coordination
// plumbing). It is a no-op error when no bus is configured.
func (a *Agent) sendMessageTool() []coretools.Tool {
	if a.bus == nil {
		return nil
	}
	t := tools.MustTool(coretools.FromFunc("SendMessage", "Send a message to another agent/recipient. In a team, recipients are teammate names or \"lead\"; messages are pushed (an idle teammate wakes to process them).", a.sendMessage))
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
	if team := a.currentTeam(); team != nil {
		team.Send(a.agentName, args.Recipient, args.Content)
		return coretools.Result{Content: fmt.Sprintf("Sent message to %s.", args.Recipient)}, nil
	}
	a.bus.Send("main", args.Recipient, args.Content)
	return coretools.Result{Content: fmt.Sprintf("Sent message to %s.", args.Recipient)}, nil
}

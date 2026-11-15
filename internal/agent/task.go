package agent

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/projanvil/langchain-golang/core/messages"
	coretools "github.com/projanvil/langchain-golang/core/tools"
	lgcheckpoint "github.com/projanvil/langchain-golang/langgraph/checkpoint"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	lgtypes "github.com/projanvil/langchain-golang/langgraph/types"

	"github.com/projanvil/woca/internal/agents"
	"github.com/projanvil/woca/internal/skills"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/tracing"
)

// toolCallIDKey carries the executing tool call's identity through ctx so the
// Task tool can derive a stable subagent thread across interrupt
// re-executions: the call ID plus the PARENT run's thread (injected by
// toolsNode from the run's ExecutionInfo). The parent thread must travel on
// ctx rather than via Agent.LastThreadID: a shared agent serves concurrent
// sessions, and another session's turn would overwrite the global mid-relay.
type toolCallIDKey struct{}

type toolCallCtx struct {
	id     string
	thread string
}

func withToolCallID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, toolCallCtx{id: id})
}

// withToolCallThread is withToolCallID plus the parent run's thread.
func withToolCallThread(ctx context.Context, id, thread string) context.Context {
	return context.WithValue(ctx, toolCallIDKey{}, toolCallCtx{id: id, thread: thread})
}

func toolCallIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(toolCallIDKey{}).(toolCallCtx)
	return v.id
}

// parentThreadFrom returns the parent run's thread carried on the tool-call
// ctx ("" when the caller was not dispatched from toolsNode).
func parentThreadFrom(ctx context.Context) string {
	v, _ := ctx.Value(toolCallIDKey{}).(toolCallCtx)
	return v.thread
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
// paused on, so a re-entering relay can rebuild an accurate payload. Since
// Task 5 the child pauses on an ApprovalRequest (its own approval superstep);
// legacy PermissionRequest child pauses (pre-upgrade checkpoints) still
// decode, and the new shape also returns the whole envelope so the relay can
// answer every child ask.
func subagentPendingPermission(ctx context.Context, sub *Agent, thread string) (req PermissionRequest, approval ApprovalRequest, ok bool) {
	snap, err := sub.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: thread})
	if err != nil || len(snap.Interrupts) == 0 {
		return PermissionRequest{}, ApprovalRequest{}, false
	}
	switch v := decodeInterruptPayload(snap.Interrupts[0].Value).(type) {
	case ApprovalRequest:
		return permissionFromAsks(v), v, true
	case PermissionRequest:
		return v, ApprovalRequest{}, true
	}
	return PermissionRequest{}, ApprovalRequest{}, false
}

// subagentPendingApproval reports the child's paused ApprovalRequest
// envelope, if its pause is the new shape.
func subagentPendingApproval(ctx context.Context, sub *Agent, thread string) (ApprovalRequest, bool) {
	_, approval, ok := subagentPendingPermission(ctx, sub, thread)
	return approval, ok && len(approval.Asks) > 0
}

// permissionFromAsks projects an ApprovalRequest onto the legacy
// PermissionRequest the relay's wire (SubagentInterrupt.Inner) carries: the
// first ask's card. The relay is single-answer ("y"/"n" applies to every
// child ask); Task 7's β topology replaces it with per-NS routing. It is
// only valid for PLAIN permission envelopes — see nonPermissionAsk.
func permissionFromAsks(req ApprovalRequest) PermissionRequest {
	if len(req.Asks) == 0 {
		return PermissionRequest{}
	}
	ask := req.Asks[0]
	return PermissionRequest{
		ToolName:   ask.ToolName,
		ToolArgs:   ask.ToolArgs,
		Reason:     ask.Reason,
		IsReadOnly: tools.IsReadOnly(ask.ToolName, ask.ToolArgs),
	}
}

// nonPermissionAsk returns the first ask of the envelope that is NOT a plain
// permission ask (AskUserQuestion and ExitPlanMode asks carry questions/plan
// text the relay has no way to surface), or nil. Such asks must fail closed:
// projecting them onto a permission card would let a parent "y" approve an
// unseen plan or hand AskUser an empty answer — the pre-Task-5 relay made
// them fatal for the child, and this keeps that stance for the relay's
// remaining lifetime.
func nonPermissionAsk(req ApprovalRequest) *Ask {
	for i := range req.Asks {
		if req.Asks[i].ToolName == "AskUserQuestion" || req.Asks[i].ToolName == "ExitPlanMode" {
			return &req.Asks[i]
		}
	}
	return nil
}

// relayNoAnswerError is the fail-closed error for child asks the relay
// cannot carry to the user (mirrors the background arm's "no one to answer"
// semantics).
func relayNoAnswerError(subagent, toolName string) error {
	return fmt.Errorf("subagent %q paused (%s) while running in the foreground relay with no one to answer", subagent, toolName)
}

// approvalAnswerAll builds the child resume value applying one outcome to
// every ask of the child's paused envelope.
func approvalAnswerAll(req ApprovalRequest, outcome string) ApprovalAnswer {
	decisions := make(map[string]AskDecision, len(req.Asks))
	for _, ask := range req.Asks {
		decisions[ask.CallID] = AskDecision{Outcome: outcome}
	}
	return ApprovalAnswer{Decisions: decisions}
}

// graphCarrierKey reports whether key is one of the named unexported
// context-carrier types in the pinned langgraph-golang fork's graph package,
// matched by type identity via reflection (the module is pinned; the tests
// fail loudly if this stops matching).
func graphCarrierKey(key any, names ...string) bool {
	t := reflect.TypeOf(key)
	if t == nil || t.PkgPath() != "github.com/projanvil/langchain-golang/langgraph/graph" {
		return false
	}
	for _, name := range names {
		if t.Name() == name {
			return true
		}
	}
	return false
}

// detachStreamCtx hides the fork's stream emitter from a nested graph run.
// The emitter travels via an unexported context key, so a foreground
// subagent invoked with the tool goroutine's ctx would otherwise emit its
// chunks — including its RAW pause interrupts — into the parent run's
// stream, ahead of the wrapped SubagentInterrupt the relay raises; the
// harness surfaces the first interrupt chunk it sees, so the leak would
// defeat the relay.
type detachStreamCtx struct{ context.Context }

func (d detachStreamCtx) Value(key any) any {
	if graphCarrierKey(key, "streamEmitterKey") {
		return nil
	}
	return d.Context.Value(key)
}

// detachRunMetaCtx hides the fork's execution-meta carrier from a background
// subagent run. The run loop publishes its checkpoint position under
// executionMetaKey as a *checkpoint.Config it keeps mutating in place
// between supersteps (persist's `*currentCfg = cfg`), and a nested run's
// staticNext reads it through the CALLER's ctx — for a background child on
// its own goroutine that read races the parent's writes. The foreground
// relay does not need the strip: its child runs synchronously inside the
// parent's tools node, where the parent run loop cannot persist.
type detachRunMetaCtx struct{ context.Context }

func (d detachRunMetaCtx) Value(key any) any {
	if graphCarrierKey(key, "executionMetaKey") {
		return nil
	}
	return d.Context.Value(key)
}

// TaskStatus, its four constants, ErrTaskNotFound and BackgroundTaskInfo
// moved to internal/agent/activity with the seam (F1) — the activity
// package owns the snapshot vocabulary its interfaces carry — and are
// re-exported here under their historical names; see activity_delegate.go.

// backgroundTask is a Task (subagent) running in the background.
type backgroundTask struct {
	ID          string
	Subject     string
	Description string
	// subagent is the display name of the subagent definition running the
	// task (subagentName).
	subagent string

	// session is the starting run's session id (sessionFromCtx); "" outside a
	// run scope. The completion notification is delivered to it.
	session string
	// startedAt / endedAt bracket the task: set at start, endedAt on the
	// terminal transition only.
	startedAt time.Time
	// done is closed exactly once, when the task first leaves the running
	// state (completion or TaskStop), releasing TaskOutput waiters. Immutable
	// after start, like the exported fields above.
	done chan struct{}
	// exited is closed by the run goroutine when it returns, so StopAll can
	// settle stopped tasks before shutdown continues. Nil on bare tasks
	// (unit tests without a goroutine).
	exited chan struct{}
	// onChange, when set at start, fires with the owner session when the
	// task leaves the running state (terminal transition). Immutable after
	// start; nil on bare tasks (unit tests), like the nil cancel in stop.
	onChange func()

	mu        sync.Mutex
	status    TaskStatus
	output    []string
	final     string
	cancel    context.CancelFunc
	endedAt   time.Time
	stoppedBy tools.StopOrigin
}

func (t *backgroundTask) appendOutput(line string) {
	if line == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for part := range strings.SplitSeq(line, "\n") {
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
	wasRunning := t.status == TaskRunning
	t.status = status
	t.final = final
	if wasRunning {
		t.endedAt = time.Now()
		t.signalDone()
	}
	t.mu.Unlock()
	t.fireChange(wasRunning)
}

// setFinalIfRunning records a terminal state only while the task is still
// running: a completion arriving after TaskStop must not flip the stopped
// status back to completed. It reports whether this call performed the
// terminal transition.
func (t *backgroundTask) setFinalIfRunning(status TaskStatus, final string) bool {
	t.mu.Lock()
	moved := t.finishIfRunningLocked(status, final)
	t.mu.Unlock()
	t.fireChange(moved)
	return moved
}

// finishIfRunningLocked moves a running task to its terminal state; the
// caller holds mu.
func (t *backgroundTask) finishIfRunningLocked(status TaskStatus, final string) bool {
	if t.status != TaskRunning {
		return false
	}
	t.status = status
	t.final = final
	t.endedAt = time.Now()
	t.signalDone()
	return true
}

// signalDone closes the done channel, releasing TaskOutput waiters. The
// caller holds mu and has just moved the task off running, so per task the
// close runs exactly once — whichever terminal transition wins the race, the
// losers see a non-running status and never reach here. A bare task without a
// channel (unit tests) is tolerated, like the nil cancel in stop.
func (t *backgroundTask) signalDone() {
	if t.done != nil {
		close(t.done)
	}
}

// setCancel stores the cancellation handle under the mutex; taskStop reads it
// from another goroutine.
func (t *backgroundTask) setCancel(cancel context.CancelFunc) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel = cancel
}

// stop cancels the task and marks it stopped in one critical section, so a
// concurrent completion cannot interleave. It records who stopped the task
// and reports whether this call performed the terminal transition (false
// when the task had already finished on its own).
func (t *backgroundTask) stop(by tools.StopOrigin) bool {
	t.mu.Lock()
	if t.cancel != nil {
		t.cancel()
	}
	if t.status == TaskRunning {
		t.stoppedBy = by
	}
	moved := t.finishIfRunningLocked(TaskStopped, "")
	t.mu.Unlock()
	t.fireChange(moved)
	return moved
}

// fireChange invokes the change callback after a terminal transition, with
// no lock held (the callback may read the manager and this task).
func (t *backgroundTask) fireChange(moved bool) {
	if moved && t.onChange != nil {
		t.onChange()
	}
}

// info snapshots the task's lifecycle facts.
func (t *backgroundTask) info() BackgroundTaskInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.infoLocked()
}

// infoLocked is info for callers already holding mu.
func (t *backgroundTask) infoLocked() BackgroundTaskInfo {
	return BackgroundTaskInfo{
		ID:          t.ID,
		Session:     t.session,
		Description: t.Description,
		Subagent:    t.subagent,
		Status:      t.status,
		StoppedBy:   t.stoppedBy,
		StartedAt:   t.startedAt,
		EndedAt:     t.endedAt,
	}
}

// completion renders the completion body handed to the notify callback and
// reports who stopped the task ("" unless it was stopped), mirroring
// backgroundShell.completion() (internal/tools/background.go): id, final
// status, subject, and the report tail bounded by tools.NotificationOutputCap
// so a chatty report cannot flood the context. The first line is
// stop-attributed (PM §4.6): a user stop says who did it, everything else
// keeps the honest status line. The body carries no wrapper tags — the queue
// tag the agent wraps around this text is the single
// <background-task-completed> wrapper the model sees. The subject line (the
// model-facing description) names the task; the full prompt deliberately
// stays out. A stopped task carries no report — stop leaves no final report
// behind.
func (t *backgroundTask) completion() (text string, stoppedBy tools.StopOrigin) {
	t.mu.Lock()
	status, final, stoppedBy := t.status, t.final, t.stoppedBy
	t.mu.Unlock()
	report := final
	if len(report) > tools.NotificationOutputCap {
		report = "…\n" + report[len(report)-tools.NotificationOutputCap:]
	}
	var b strings.Builder
	if status == TaskStopped && stoppedBy == tools.StopByUser {
		fmt.Fprintf(&b, "Background task %s was stopped by the user: %s\n", t.ID, t.Subject)
	} else {
		fmt.Fprintf(&b, "Background task %s finished (%s): %s\n", t.ID, status, t.Subject)
	}
	if report != "" {
		fmt.Fprintf(&b, "Report (tail):\n%s\n", strings.TrimRight(report, "\n"))
	}
	return b.String(), stoppedBy
}

// TaskManager holds all background tasks for a session.
type TaskManager struct {
	mu       sync.Mutex
	tasks    map[string]*backgroundTask
	next     int
	notify   func(session, text string, stoppedBy tools.StopOrigin)
	onChange func(session string)
}

func newTaskManager() *TaskManager {
	return &TaskManager{tasks: map[string]*backgroundTask{}}
}

// maxBackgroundTasks caps concurrently running background tasks.
const maxBackgroundTasks = 16

func (m *TaskManager) start(ctx context.Context, subject, description, subagent string) (*backgroundTask, error) {
	m.mu.Lock()
	running := 0
	for _, t := range m.tasks {
		if st, _, _ := t.snapshot(); st == TaskRunning {
			running++
		}
	}
	if running >= maxBackgroundTasks {
		m.mu.Unlock()
		return nil, fmt.Errorf("background task limit reached (%d running): wait or stop some with TaskStop", maxBackgroundTasks)
	}
	m.next++
	onChange := m.onChange
	t := &backgroundTask{
		ID:          fmt.Sprintf("task-%d", m.next),
		Subject:     subject,
		Description: description,
		subagent:    subagent,
		session:     sessionFromCtx(ctx),
		startedAt:   time.Now(),
		status:      TaskRunning,
		done:        make(chan struct{}),
		exited:      make(chan struct{}),
	}
	if onChange != nil {
		session := t.session
		t.onChange = func() { onChange(session) }
	}
	m.tasks[t.ID] = t
	m.mu.Unlock()
	if onChange != nil {
		onChange(t.session)
	}
	return t, nil
}

// SetNotify installs the completion callback. It fires once per finished
// task as fn(session, text, stoppedBy), where session is the starting run's
// session id ("" outside a run scope), text is the rendered completion
// notification and stoppedBy names who stopped the task ("" unless it was
// stopped — the quiet/attribution signal, D1). It is called in the task's
// run goroutine; it must not block.
func (m *TaskManager) SetNotify(fn func(session, text string, stoppedBy tools.StopOrigin)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.notify = fn
}

// SetOnChange installs the change callback the activity seam subscribes to.
// It fires synchronously on task start and on the terminal transition
// (completion, failure or stop), with the owner session id; it must not
// block. Tasks started before the call keep the callback that was installed
// when they started.
func (m *TaskManager) SetOnChange(fn func(session string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onChange = fn
}

// notifyDone delivers t's completion notification through the installed
// callback (a no-op without one). Called from the task's run goroutine once
// the task has reached its terminal state — completed or stopped alike, so a
// stopped task is never silently dropped — with the stop attribution so the
// queueing side can keep user stops Quiet (D1). It also prunes the session's
// finished records (F4): this is the one hook every goroutine-driven
// terminal transition passes through.
func (m *TaskManager) notifyDone(t *backgroundTask) {
	m.mu.Lock()
	fn := m.notify
	m.mu.Unlock()
	if fn != nil {
		text, stoppedBy := t.completion()
		fn(t.session, text, stoppedBy)
	}
	m.pruneFinished(t.session)
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

// lookup returns the task with id when session may access it. A task owned
// by another scoped session answers exactly like an unknown id
// (tracing.VisibleTo), so callers cannot probe for foreign tasks' existence.
func (m *TaskManager) lookup(session, id string) (*backgroundTask, error) {
	m.mu.Lock()
	t := m.tasks[id]
	m.mu.Unlock()
	if t == nil || !tracing.VisibleTo(t.session, session) {
		return nil, ErrTaskNotFound
	}
	return t, nil
}

// visible returns the tasks a run in session can see, in numeric id order
// (task-2 before task-10): everything for the unscoped session "" (the
// TUI/REPL single-session case), otherwise the unscoped tasks plus the
// session's own. Finished records are pruned per session (F4), so the live
// map — not the historical id counter — is walked; ids are minted from a
// monotonic counter and never reused.
func (m *TaskManager) visible(session string) []*backgroundTask {
	m.mu.Lock()
	out := make([]*backgroundTask, 0, len(m.tasks))
	for _, t := range m.tasks {
		if tracing.VisibleTo(t.session, session) {
			out = append(out, t)
		}
	}
	m.mu.Unlock()
	slices.SortFunc(out, func(a, b *backgroundTask) int { return cmpTaskID(a.ID, b.ID) })
	return out
}

// cmpTaskID orders task ids numerically (task-2 before task-10), falling
// back to lexical order for anything off the counter's scheme.
func cmpTaskID(a, b string) int {
	na, ea := strconv.Atoi(strings.TrimPrefix(a, "task-"))
	nb, eb := strconv.Atoi(strings.TrimPrefix(b, "task-"))
	if ea == nil && eb == nil {
		return cmp.Compare(na, nb)
	}
	return strings.Compare(a, b)
}

// pruneFinished caps the session's finished task records at
// tools.MaxFinishedRecordsPerSession, dropping the OLDEST finished records
// first (F4); running records are always kept. Called from the terminal
// transitions' manager hooks — notifyDone (the run goroutine's post-terminal
// path), StopFor and StopAll — so records stay bounded without new starts.
func (m *TaskManager) pruneFinished(session string) {
	type fin struct {
		id    string
		ended time.Time
		num   int
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var fins []fin
	for id, t := range m.tasks {
		if t.session != session {
			continue
		}
		info := t.info() // m.mu → t.mu, the managers' established order
		if info.Status == TaskRunning {
			continue // running records are always kept
		}
		num, err := strconv.Atoi(strings.TrimPrefix(id, "task-"))
		if err != nil {
			continue
		}
		fins = append(fins, fin{id: id, ended: info.EndedAt, num: num})
	}
	if len(fins) <= tools.MaxFinishedRecordsPerSession {
		return
	}
	slices.SortFunc(fins, func(a, b fin) int {
		if c := a.ended.Compare(b.ended); c != 0 {
			return c
		}
		return cmp.Compare(a.num, b.num)
	})
	for _, f := range fins[:len(fins)-tools.MaxFinishedRecordsPerSession] {
		delete(m.tasks, f.id)
	}
}

// DropSession removes every record the session owns, finished or running
// (F4): the activity seam's Release calls it after stopping the session's
// running tasks — their goroutines finish invisibly, late completions are
// already silenced for a released session. It returns how many records
// were removed. (Exported to satisfy activity.Tasks; the type itself stays
// package-internal.)
func (m *TaskManager) DropSession(session string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, t := range m.tasks {
		if t.session == session {
			delete(m.tasks, id)
			n++
		}
	}
	return n
}

// ListFor returns snapshots of the background tasks a run in session can
// see, in numeric id order.
func (m *TaskManager) ListFor(session string) []BackgroundTaskInfo {
	tasks := m.visible(session)
	out := make([]BackgroundTaskInfo, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.info())
	}
	return out
}

// InfoFor returns a snapshot of one background task, when session may see
// it. Unknown and foreign ids both return ErrTaskNotFound.
func (m *TaskManager) InfoFor(session, id string) (BackgroundTaskInfo, error) {
	t, err := m.lookup(session, id)
	if err != nil {
		return BackgroundTaskInfo{}, err
	}
	return t.info(), nil
}

// TailFor returns at most the last maxBytes bytes of the task's buffered
// output plus the truncated flag and the full byte count, when session may
// see the task. The cut point is backed off to a UTF-8 rune boundary so a
// multibyte character is never split; maxBytes <= 0 returns the whole
// buffer. Mirrors tools.BackgroundShells.TailFor.
func (m *TaskManager) TailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, info BackgroundTaskInfo, err error) {
	t, err := m.lookup(session, id)
	if err != nil {
		return "", false, 0, BackgroundTaskInfo{}, err
	}
	t.mu.Lock()
	out := strings.Join(t.output, "\n")
	info = t.infoLocked()
	t.mu.Unlock()
	total = len(out)
	if maxBytes <= 0 || total <= maxBytes {
		return out, false, total, info, nil
	}
	tail = out[total-maxBytes:]
	// A leading continuation byte means the first character straddles the
	// cut; drop its partial bytes so the tail starts on a rune boundary.
	for len(tail) > 0 && !utf8.RuneStart(tail[0]) {
		tail = tail[1:]
	}
	return tail, true, total, info, nil
}

// StopFor stops a running task on behalf of by (model or user), recording
// the stop attribution, when session may see it. Stopping an
// already-finished task changes nothing: it reports alreadyFinished=true
// with the unchanged snapshot. A stop is a terminal transition, so the
// session's finished records are pruned here too (F4).
func (m *TaskManager) StopFor(session, id string, by tools.StopOrigin) (info BackgroundTaskInfo, alreadyFinished bool, err error) {
	t, err := m.lookup(session, id)
	if err != nil {
		return BackgroundTaskInfo{}, false, err
	}
	moved := t.stop(by)
	m.pruneFinished(t.session)
	if !moved {
		return t.info(), true, nil
	}
	return t.info(), false, nil
}

// taskStopSettleBound is how long StopAll waits, after stopping, for the
// stopped subagents' goroutines to return. A cancelled subagent ends on its
// ctx (no signal escalation is involved, unlike the shells' KillAll), so the
// bound only covers a model round-trip or tool teardown already in flight.
// Bounded so shutdown — and tests — can never hang on an unresponsive run.
const taskStopSettleBound = 5 * time.Second

// StopAll stops every running task on behalf of by — shutdown cleanup
// (Agent.Close), where session scoping no longer matters because the whole
// manager is going away. Finished tasks keep their recorded state (each
// session's finished records are pruned back under their cap, F4, since an
// SDK host keeps living past Close). Like BackgroundShells.KillAll it then
// waits up to taskStopSettleBound for the stopped tasks' run goroutines to
// settle: background subagents are detached from every context (D3), so
// without this a host that keeps living past Close — the SDK — would carry
// orphaned runs still spending model calls.
func (m *TaskManager) StopAll(by tools.StopOrigin) {
	m.mu.Lock()
	running := make([]*backgroundTask, 0, len(m.tasks))
	sessions := map[string]bool{}
	for _, t := range m.tasks {
		sessions[t.session] = true
		if st, _, _ := t.snapshot(); st == TaskRunning {
			running = append(running, t)
		}
	}
	m.mu.Unlock()
	for _, t := range running {
		t.stop(by)
	}
	for session := range sessions {
		m.pruneFinished(session)
	}
	// Wait out the settle window: once a task's exited channel closes, its
	// goroutine — and any work it still had in flight — is done.
	deadline := time.Now().Add(taskStopSettleBound)
	for _, t := range running {
		if t.exited == nil {
			continue // bare task without a goroutine (unit tests)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-t.exited:
			timer.Stop()
		case <-timer.C:
			// Settle bound elapsed; stay bounded rather than block shutdown.
			return
		}
	}
}

const taskDescription = `Launch a subagent to handle a self-contained piece of work and return a concise report. Set run_in_background to run it asynchronously (poll with TaskOutput, stop with TaskStop). Set fork=true to copy the current conversation into the subagent first — it starts with your full context instead of a blank slate, which fits "continue this thread in parallel" tasks.`

type taskArgs struct {
	Description     string `json:"description"`
	Prompt          string `json:"prompt"`
	SubagentType    string `json:"subagent_type,omitempty"`
	RunInBackground bool   `json:"run_in_background,omitempty"`
	Fork            bool   `json:"fork,omitempty"`
}

const taskOutputDescription = `Fetch the current output and status of a background task launched with the Task tool. Set wait_ms to block inside this call until the task finishes or the wait elapses (max 600000ms), instead of polling repeatedly.`

type taskOutputArgs struct {
	TaskID string `json:"task_id"`
	WaitMS int    `json:"wait_ms,omitempty"`
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
	name := subagentName(def, args)
	// SubagentStart fires once the task is validated and named, ahead of both
	// the foreground and background runs. Its feedback is informational.
	_, _ = a.runSubagentStart(ctx, name)

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
		t, err := a.tasks.start(ctx, args.Description, prompt, name)
		if err != nil {
			return coretools.Result{}, err
		}
		// D3: the background subagent detaches from the tool call's
		// cancellation — a run stop or approval timeout no longer kills it —
		// while keeping the ctx values it was launched with (run workspace,
		// run executor, run scope). The cancel handle belongs to TaskStop /
		// the user stop alone, exactly like a background shell.
		runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		t.setCancel(cancel)
		go func() {
			// exited releases StopAll's settle once this goroutine — and the
			// subagent run it drives — has fully returned.
			defer close(t.exited)
			report, err := a.runSubagent(runCtx, history, func(text string) { t.appendOutput(text) }, def)
			// Honest status per the shared vocabulary: a subagent error is a
			// failed task, not a completed one with an "Error:" body.
			status := TaskCompleted
			if err != nil {
				status = TaskFailed
				report = "Error: " + err.Error()
			}
			// SubagentStop fires once the subagent run is over (report or
			// error alike — both leave a final report; completed, failed or
			// stopped alike). It runs on a detached copy of runCtx —
			// TaskStop's cancellation is exactly what ended the subagent
			// run, and the command runner fails on a cancelled context, so a
			// stopped task would otherwise skip the hook (same reasoning as
			// recordTaskEvent below). It runs BEFORE the final state lands
			// so the exit-2 feedback reaches every report consumer:
			// TaskOutput, the completion notification and the L1 episode.
			report = a.withSubagentStopFeedback(context.WithoutCancel(runCtx), name, report)
			t.setFinalIfRunning(status, report)
			// Completion notification for the starting run's session. It
			// fires for stopped tasks too — completion() then renders the
			// stopped status line, attributed when the user stopped it — so
			// TaskStop never silently drops a task.
			a.tasks.notifyDone(t)
			// L1 episode for the completed subagent task. runCtx may already
			// be cancelled by TaskStop, but its values (run workspace) are
			// still readable; the write itself is detached.
			a.recordTaskEvent(runCtx, name, args.Description, report, err)
		}()
		return coretools.Result{Content: fmt.Sprintf("Started background task %s: %s", t.ID, args.Description)}, nil
	}

	report, err := a.runForegroundTask(ctx, history, def, name, prompt)
	if err != nil {
		return coretools.Result{}, err
	}
	report = a.withSubagentStopFeedback(ctx, name, report)
	a.recordTaskEvent(ctx, name, args.Description, report, nil)
	return coretools.Result{Content: report}, nil
}

// withSubagentStopFeedback fires the SubagentStop hooks for a finished
// subagent and appends exit-2 feedback to its report inside the documented
// wrapper. There is no re-run loop (a divergence from Claude Code's
// continue-the-subagent semantics): the blocked flag is informational and
// only the feedback travels, into the report.
func (a *Agent) withSubagentStopFeedback(ctx context.Context, name, report string) string {
	feedback, blocked := a.runSubagentStop(ctx, name)
	if blocked && feedback != "" {
		return report + "\n<subagent-stop-feedback>\n" + feedback + "\n</subagent-stop-feedback>"
	}
	return report
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
	session := sessionFromCtx(ctx)
	t, err := a.tasks.lookup(session, args.TaskID)
	if err != nil {
		// The listing carries only the tasks this session can see, and the
		// message names no task id at all — a foreign id must stay
		// indistinguishable from a nonexistent one.
		return coretools.Result{}, fmt.Errorf("no background task (started tasks: %s)", formatTaskIDs(a.tasks.ListFor(session)))
	}
	if args.WaitMS > 0 {
		// Block until the task reaches a terminal state, the (clamped) wait
		// elapses, or the surrounding tool call is cancelled. NewTimer over
		// time.After so an early return stops the timer instead of letting it
		// fire for the full wait (up to MaxWaitMS) after the caller moved on.
		wait := time.Duration(min(args.WaitMS, tools.MaxWaitMS)) * time.Millisecond
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-t.done:
		case <-timer.C:
		case <-ctx.Done():
		}
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
	session := sessionFromCtx(ctx)
	info, alreadyFinished, err := a.tasks.StopFor(session, args.TaskID, tools.StopByModel)
	if err != nil {
		return coretools.Result{}, fmt.Errorf("no background task (started tasks: %s)", formatTaskIDs(a.tasks.ListFor(session)))
	}
	if alreadyFinished {
		return coretools.Result{Content: fmt.Sprintf("Task %s already finished (%s).", info.ID, info.Status)}, nil
	}
	return coretools.Result{Content: fmt.Sprintf("Stopped task %s.", info.ID)}, nil
}

// formatTaskIDs renders a task id listing for not-found errors, mirroring
// formatShellIDs: only ids the caller can already see ever appear.
func formatTaskIDs(list []BackgroundTaskInfo) string {
	if len(list) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(list))
	for _, info := range list {
		ids = append(ids, info.ID)
	}
	return strings.Join(ids, ", ")
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
	// The child runs on its own goroutine, so it must not see the parent
	// run's execution-meta carrier (see detachRunMetaCtx).
	ctx = detachRunMetaCtx{ctx}
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
			if intrs, ok := updates[lgcheckpoint.ReservedInterrupt].([]lgtypes.Interrupt); ok && len(intrs) > 0 {
				// A background subagent has no one to answer a pause: fail
				// the task closed instead of completing with a silent empty
				// report. (The foreground relay lives in runForegroundTask.)
				if req, ok := decodeInterruptPayload(intrs[0].Value).(PermissionRequest); ok && req.ToolName != "" {
					return "", fmt.Errorf("subagent paused for a permission (%s) it cannot ask while running in the background", req.ToolName)
				}
				return "", fmt.Errorf("subagent paused (%v) while running in the background with no one to answer", decodeInterruptPayload(intrs[0].Value))
			}
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

// parentThreadFor resolves the parent run's thread for relay addressing: the
// tool-call ctx carrier first (injected by toolsNode from the run's
// ExecutionInfo, race-free under concurrent sessions); the agent-global
// LastThreadID is only a fallback for non-graph-driven invocations where the
// carrier was never installed.
func (a *Agent) parentThreadFor(ctx context.Context) string {
	if t := parentThreadFrom(ctx); t != "" {
		return t
	}
	return a.LastThreadID()
}

// relaySeq recovers how many resume values earlier re-executions of this
// relay consumed, read back from the SubagentInterrupt pending on the parent
// checkpoint. The fork's resume queue is positional — the persisted consumed
// prefix followed by this resume's fresh answer — and the prefix length is
// exactly the Seq the last surfaced SubagentInterrupt recorded. Missing or
// undecodable state degrades to 0 (correct for the first re-entry).
func (a *Agent) relaySeq(ctx context.Context, thread string) int {
	snap, err := a.Graph().GetState(ctx, lgcheckpoint.Config{ThreadID: a.parentThreadFor(ctx)})
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
//
// Durability note: the child's own checkpoint thread persists on the shared
// saver, so a cross-process parent resume re-enters here and continues the
// paused child — but the toolsNode batch memo is process-local (see
// Agent.batchMemo), so sibling tool calls in the same batch re-execute once
// after a process restart.
func (a *Agent) runForegroundTask(ctx context.Context, history []messages.Message, def *agents.Definition, name, prompt string) (string, error) {
	thread := deriveSubagentThread(a.parentThreadFor(ctx), toolCallIDFrom(ctx), prompt)
	sub, err := a.newSubagent(ctx, def)
	if err != nil {
		return "", err
	}
	// The child runs detached from the parent stream's emitter so its chunks
	// (above all its raw pause interrupts) never reach the harness; the relay
	// surfaces pauses itself, wrapped as SubagentInterrupt. The ctx is also
	// re-based onto the PARENT'S RUN ctx, not this per-superstep tool-call
	// ctx: the child captures whatever it is handed as its own run ctx
	// (beginRunCtx via Invoke/Resume), and its speculative siblings must
	// follow the run's lifetime — survive the relay's pause (which suspends
	// the parent graph and tears down its stream, cancelling the derived ctx
	// tree) and die only with the parent run (spec decision F1).
	subCtx := detachStreamCtx{a.runCtxFor(baseRunID(ctx), ctx)}
	started := a.subagentStarted(ctx, sub, thread)
	pending := SubagentInterrupt{Subagent: name, ToolCallID: toolCallIDFrom(ctx), ThreadID: thread}
	seq := 0
	if started {
		// Re-entry after a parent resume: rebuild the payload from the
		// child's paused checkpoint and fast-forward the positional resume
		// queue past the answers earlier re-executions already consumed, so
		// the loop's first Interrupt call reads THIS resume's answer. A
		// parked envelope carrying a non-permission ask (only possible from
		// a pre-fix checkpoint) fails closed here too.
		if approval, isApproval := subagentPendingApproval(ctx, sub, thread); isApproval {
			if bad := nonPermissionAsk(approval); bad != nil {
				return "", relayNoAnswerError(name, bad.ToolName)
			}
		}
		if req, _, ok := subagentPendingPermission(ctx, sub, thread); ok {
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
			// The child's own pause is an ApprovalRequest since Task 5 (its
			// approval superstep): the parent's single y/n applies to every
			// child ask. Legacy child checkpoints pause on a bare
			// PermissionRequest and keep the scalar "y" resume.
			resumeValue := any("y")
			if approval, isApproval := subagentPendingApproval(ctx, sub, thread); isApproval {
				resumeValue = approvalAnswerAll(approval, OutcomeAllow)
			}
			msgs, err = sub.Resume(subCtx, thread, resumeValue)
		}
		gi, ok := errors.AsType[*lgtypes.GraphInterrupt](err)
		if !ok {
			if err == nil {
				_ = a.saver.DeleteThread(context.WithoutCancel(ctx), thread)
				return lastAIText(msgs), nil
			}
			return "", err
		}
		switch v := decodeInterruptPayload(gi.Interrupt.Value).(type) {
		case ApprovalRequest:
			// The child's approval superstep paused. Plain permission asks
			// relay as the first ask's card (the legacy wire); anything the
			// relay cannot carry (AskUser questions, plan approval) fails
			// closed — the child aborts with an error instead of surfacing
			// a blanket-approvable card.
			if bad := nonPermissionAsk(v); bad != nil {
				return "", relayNoAnswerError(name, bad.ToolName)
			}
			pending.Inner = permissionFromAsks(v)
		case PermissionRequest:
			// Legacy child checkpoint pause (pre-upgrade child thread).
			pending.Inner = v
		default:
			// Non-permission pauses (AskUser etc.) stay fatal for subagents.
			return "", err
		}
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
	for _, msg := range slices.Backward(msgs) {
		if msg.Role == messages.RoleAI && msg.Content != "" {
			return msg.Content
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

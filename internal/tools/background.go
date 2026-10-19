package tools

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
)

// BackgroundShells manages Bash commands started with run_in_background:
// the command runs detached while the turn continues, its combined output
// accumulates in memory, and completion fires a notification that the agent
// injects after the next tool round (the queued-attachment mechanism). Output
// is polled with BashOutput and the shell terminated with KillShell.

// BackgroundShellStatus is a snapshot of one background shell.
type BackgroundShellStatus struct {
	ID       string
	Command  string
	Running  bool
	ExitCode int // -1 while running or after a signal kill without a status
}

// notifyBackgroundDone, when set, is called once when a background shell
// finishes, with the rendered completion notification. The agent wires this to
// its mid-turn input queue.
type BackgroundShells struct {
	mu     sync.Mutex
	next   int
	shells map[string]*backgroundShell
	onDone func(notification string)
	// execFor resolves the execution backend per start, so a run executor
	// injected into the invoking context (a per-run sandbox) serves the
	// background shell too.
	execFor func(context.Context) executor.Executor
}

type backgroundShell struct {
	id      string
	command string
	proc    executor.Process
	buf     strings.Builder
	mu      sync.Mutex
	done    chan struct{}
	exited  bool
	exit    int
}

// NewBackgroundShells builds an empty shell manager running on the local
// machine.
func NewBackgroundShells() *BackgroundShells {
	return NewBackgroundShellsWithExecutor(nil)
}

// NewBackgroundShellsWithExecutor is NewBackgroundShells with an explicit
// fixed execution backend; a nil executor falls back to the local machine.
// The backend does not resolve run executors from the context — use
// NewBackgroundShellsWithExecFor for that.
func NewBackgroundShellsWithExecutor(x executor.Executor) *BackgroundShells {
	if x == nil {
		x = executor.NewLocal()
	}
	return NewBackgroundShellsWithExecFor(func(context.Context) executor.Executor { return x })
}

// NewBackgroundShellsWithExecFor resolves the execution backend per start
// from the invoking context; a nil resolver falls back to the local
// machine.
func NewBackgroundShellsWithExecFor(execFor func(context.Context) executor.Executor) *BackgroundShells {
	if execFor == nil {
		execFor = func(context.Context) executor.Executor { return executor.NewLocal() }
	}
	return &BackgroundShells{shells: map[string]*backgroundShell{}, execFor: execFor}
}

// SetNotify installs the completion callback (called in the shell's run
// goroutine; it must not block).
func (b *BackgroundShells) SetNotify(fn func(notification string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onDone = fn
}

// Start launches a command in the background and returns its shell ID. The
// working directory resolution matches the foreground Bash tool. ctx is the
// invoking tool's context: its values (run executor resolution) carry into
// the process start, while its cancellation deliberately does not —
// background shells outlive their turn, so the process starts under
// context.WithoutCancel(ctx).
func (b *BackgroundShells) Start(ctx context.Context, command, workdir, baseWorkdir string) (string, error) {
	wd := workdir
	if wd == "" {
		wd = baseWorkdir
	}
	if !filepath.IsAbs(wd) {
		wd = filepath.Join(baseWorkdir, wd)
	}

	b.mu.Lock()
	b.next++
	id := strconv.Itoa(b.next)
	sh := &backgroundShell{
		id:      id,
		command: command,
		done:    make(chan struct{}),
		exit:    -1,
	}
	b.shells[id] = sh
	onDone := b.onDone
	execFor := b.execFor
	b.mu.Unlock()

	// Background shells deliberately outlive the calling turn's context
	// (that is their point), but keep its values — WithoutCancel preserves
	// them, so a per-run sandbox executor resolved from ctx still serves
	// the shell while the turn's cancellation cannot kill it.
	x := execFor(ctx)
	proc, err := x.Start(context.WithoutCancel(ctx),
		executor.ExecRequest{Name: "bash", Args: []string{"-c", command}, Dir: wd},
		executor.StartOptions{Stdout: lineWriter{sh}, Stderr: lineWriter{sh}})
	if err != nil {
		b.mu.Lock()
		delete(b.shells, id)
		b.mu.Unlock()
		return "", err
	}
	sh.proc = proc
	go func() {
		code := 0
		if err := proc.Wait(); err != nil {
			code = -1
			if ee, ok := errors.AsType[*executor.ExitError](err); ok {
				code = ee.Code
			}
		}
		sh.mu.Lock()
		sh.exited = true
		sh.exit = code
		sh.mu.Unlock()
		close(sh.done)
		if onDone != nil {
			onDone(sh.notification())
		}
	}()
	return id, nil
}

// lineWriter appends to a background shell's buffer under its lock.
type lineWriter struct{ sh *backgroundShell }

func (w lineWriter) Write(p []byte) (int, error) {
	w.sh.mu.Lock()
	defer w.sh.mu.Unlock()
	return w.sh.buf.Write(p)
}

// snapshot renders the shell's current state and buffered output (never races
// with the writer goroutine).
func (sh *backgroundShell) snapshot() BackgroundShellStatus {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	select {
	case <-sh.done:
		return BackgroundShellStatus{ID: sh.id, Command: sh.command, Running: false, ExitCode: sh.exit}
	default:
		return BackgroundShellStatus{ID: sh.id, Command: sh.command, Running: true, ExitCode: -1}
	}
}

// Status returns a snapshot of one shell.
func (b *BackgroundShells) Status(id string) (BackgroundShellStatus, bool) {
	b.mu.Lock()
	sh := b.shells[id]
	b.mu.Unlock()
	if sh == nil {
		return BackgroundShellStatus{}, false
	}
	return sh.snapshot(), true
}

// List returns snapshots of every shell this manager started, oldest first.
func (b *BackgroundShells) List() []BackgroundShellStatus {
	b.mu.Lock()
	shells := make([]*backgroundShell, 0, len(b.shells))
	for i := 1; i <= b.next; i++ {
		id := strconv.Itoa(i)
		if sh, ok := b.shells[id]; ok {
			shells = append(shells, sh)
		}
	}
	b.mu.Unlock()
	out := make([]BackgroundShellStatus, 0, len(shells))
	for _, sh := range shells {
		out = append(out, sh.snapshot())
	}
	return out
}

// Output returns the accumulated output and status of one shell.
func (b *BackgroundShells) Output(id string) (output string, status BackgroundShellStatus, ok bool) {
	st, ok := b.Status(id)
	if !ok {
		return "", BackgroundShellStatus{}, false
	}
	b.mu.Lock()
	sh := b.shells[id]
	b.mu.Unlock()
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.buf.String(), st, true
}

// Kill terminates a running shell. Killing an already-finished shell is a
// no-op. Returns the (possibly new) status.
func (b *BackgroundShells) Kill(id string) (BackgroundShellStatus, error) {
	b.mu.Lock()
	sh := b.shells[id]
	b.mu.Unlock()
	if sh == nil {
		return BackgroundShellStatus{}, fmt.Errorf("no background shell %q", id)
	}
	sh.mu.Lock()
	running := !sh.exited
	proc := sh.proc
	sh.mu.Unlock()
	if running && proc != nil {
		_ = proc.Kill()
	}
	st, _ := b.Status(id)
	return st, nil
}

// notificationOutputCap bounds the output tail included in a completion
// notification so a chatty command cannot flood the context.
const notificationOutputCap = 2000

// notification renders the completion message injected after the next tool
// round: exit status plus a bounded output tail.
func (sh *backgroundShell) notification() string {
	sh.mu.Lock()
	st := BackgroundShellStatus{ID: sh.id, Command: sh.command, ExitCode: sh.exit}
	out := sh.buf.String()
	sh.mu.Unlock()
	var b strings.Builder
	b.WriteString("<background-task-completed>\n")
	fmt.Fprintf(&b, "Background shell %s finished (exit code %d): %s\n", st.ID, st.ExitCode, st.Command)
	if out != "" {
		if len(out) > notificationOutputCap {
			out = "…\n" + out[len(out)-notificationOutputCap:]
		}
		fmt.Fprintf(&b, "Output (tail):\n%s\n", strings.TrimRight(out, "\n"))
	}
	b.WriteString("</background-task-completed>")
	return b.String()
}

// bashOutputTool polls a background shell's output.
func bashOutputTool(bg *BackgroundShells) coretools.Tool {
	t := MustTool(coretools.FromFunc("BashOutput",
		"Get the current output and status of a background shell started with Bash run_in_background. Returns the accumulated stdout/stderr, whether it is still running, and its exit code once finished. Poll repeatedly for long-running commands.",
		func(ctx context.Context, a struct {
			ShellID string `json:"shell_id"`
		}) (coretools.Result, error) {
			out, st, ok := bg.Output(strings.TrimSpace(a.ShellID))
			if !ok {
				return coretools.Result{Content: fmt.Sprintf("Error: no background shell %q (started shells: %s)", a.ShellID, formatShellIDs(bg.List()))}, nil
			}
			var b strings.Builder
			state := "running"
			if !st.Running {
				state = fmt.Sprintf("done (exit code %d)", st.ExitCode)
			}
			fmt.Fprintf(&b, "shell %s: %s\ncommand: %s\n", st.ID, state, st.Command)
			if out == "" {
				b.WriteString("(no output yet)\n")
			} else {
				b.WriteString(out)
				if !strings.HasSuffix(out, "\n") {
					b.WriteString("\n")
				}
			}
			return coretools.Result{Content: b.String()}, nil
		}))
	return t
}

// killShellTool terminates a background shell.
func killShellTool(bg *BackgroundShells) coretools.Tool {
	t := MustTool(coretools.FromFunc("KillShell",
		"Terminate a background shell started with Bash run_in_background. Use when a background command is no longer needed or is misbehaving.",
		func(ctx context.Context, a struct {
			ShellID string `json:"shell_id"`
		}) (coretools.Result, error) {
			st, err := bg.Kill(strings.TrimSpace(a.ShellID))
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			if st.Running {
				return coretools.Result{Content: fmt.Sprintf("kill signal sent to shell %s (%s)", st.ID, st.Command)}, nil
			}
			return coretools.Result{Content: fmt.Sprintf("shell %s already finished (exit code %d)", st.ID, st.ExitCode)}, nil
		}))
	return t
}

func formatShellIDs(list []BackgroundShellStatus) string {
	if len(list) == 0 {
		return "none"
	}
	ids := make([]string, 0, len(list))
	for _, st := range list {
		ids = append(ids, st.ID)
	}
	return strings.Join(ids, ", ")
}

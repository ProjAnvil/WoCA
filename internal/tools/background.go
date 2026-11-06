package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/tracing"
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
// finishes, with the starting run's session id and the rendered completion
// notification. The agent wires this to its session-scoped mid-turn queue.
type BackgroundShells struct {
	mu     sync.Mutex
	next   int
	shells map[string]*backgroundShell
	onDone func(session, notification string)
	// execFor resolves the execution backend per start, so a run executor
	// injected into the invoking context (a per-run sandbox) serves the
	// background shell too.
	execFor func(context.Context) executor.Executor
}

type backgroundShell struct {
	id      string
	command string
	// session is the starting run's session id (tracing.RunFrom); "" when
	// the shell started outside any run scope.
	session string
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

// SetNotify installs the completion callback. It fires once per finished
// shell as fn(session, notification), where session is the starting run's
// session id ("" outside a run scope). It is called in the shell's run
// goroutine; it must not block.
func (b *BackgroundShells) SetNotify(fn func(session, notification string)) {
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
		session: sessionFromCtx(ctx),
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
			onDone(sh.session, sh.notification())
		}
	}()
	return id, nil
}

// sessionFromCtx returns the session id of the run scope in ctx, if any;
// shells started outside a run scope carry "" (unscoped delivery).
func sessionFromCtx(ctx context.Context) string {
	if sc, ok := tracing.RunFrom(ctx); ok {
		return sc.SessionID
	}
	return ""
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

// Wait blocks until the shell finishes, d elapses, or ctx is canceled, then
// returns its snapshot: final status on completion, running status on timeout
// or cancellation. ok=false reports an unknown id (without blocking).
func (b *BackgroundShells) Wait(ctx context.Context, id string, d time.Duration) (BackgroundShellStatus, bool) {
	b.mu.Lock()
	sh := b.shells[id]
	b.mu.Unlock()
	if sh == nil {
		return BackgroundShellStatus{}, false
	}
	// NewTimer over time.After so an early return stops the timer instead of
	// letting it fire for the full wait (up to MaxWaitMS) after the caller
	// moved on.
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-sh.done:
	case <-timer.C:
	case <-ctx.Done():
	}
	return sh.snapshot(), true
}

// WriteStdin writes input to a running shell's standard input and, when
// closeStdin is set, closes the pipe to signal EOF — the standard way to
// finish a stdin consumer such as cat. Writing to an unknown or already
// finished shell is an error.
func (b *BackgroundShells) WriteStdin(id, input string, closeStdin bool) error {
	b.mu.Lock()
	sh := b.shells[id]
	b.mu.Unlock()
	if sh == nil {
		return fmt.Errorf("no background shell %q", id)
	}
	sh.mu.Lock()
	exited := sh.exited
	exit := sh.exit
	var stdin io.WriteCloser
	if !exited && sh.proc != nil {
		stdin = sh.proc.Stdin()
	}
	sh.mu.Unlock()
	if exited {
		return fmt.Errorf("background shell %s already finished (exit code %d)", sh.id, exit)
	}
	if stdin == nil {
		return fmt.Errorf("background shell %s has no stdin yet", sh.id)
	}
	if input != "" {
		if _, err := io.WriteString(stdin, input); err != nil {
			return fmt.Errorf("write stdin: %w", err)
		}
	}
	if closeStdin {
		if err := stdin.Close(); err != nil {
			return fmt.Errorf("close stdin: %w", err)
		}
	}
	return nil
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

// NotificationOutputCap bounds the output tail included in a completion
// notification so a chatty command cannot flood the context.
const NotificationOutputCap = 2000

// MaxWaitMS is the largest BashOutput wait_ms (10 minutes); longer requests
// are clamped so a typo cannot pin a tool round for hours.
const MaxWaitMS = 600000

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
		if len(out) > NotificationOutputCap {
			out = "…\n" + out[len(out)-NotificationOutputCap:]
		}
		fmt.Fprintf(&b, "Output (tail):\n%s\n", strings.TrimRight(out, "\n"))
	}
	b.WriteString("</background-task-completed>")
	return b.String()
}

// bashOutputTool polls a background shell's output, optionally blocking
// until it finishes.
func bashOutputTool(bg *BackgroundShells) coretools.Tool {
	t := MustTool(coretools.FromFunc("BashOutput",
		"Get the current output and status of a background shell started with Bash run_in_background. Returns the accumulated stdout/stderr, whether it is still running, and its exit code once finished. Set wait_ms to block inside this call until the shell finishes or the wait elapses (max 600000ms), instead of polling repeatedly.",
		func(ctx context.Context, a struct {
			ShellID string `json:"shell_id"`
			WaitMS  int    `json:"wait_ms,omitempty"`
		}) (coretools.Result, error) {
			if a.WaitMS > 0 {
				wait := time.Duration(min(a.WaitMS, MaxWaitMS)) * time.Millisecond
				// Unknown ids return immediately from Wait; Output renders the
				// error below.
				_, _ = bg.Wait(ctx, strings.TrimSpace(a.ShellID), wait)
			}
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

// bashInputTool writes to a background shell's standard input.
func bashInputTool(bg *BackgroundShells) coretools.Tool {
	t := MustTool(coretools.FromFunc("BashInput",
		"Write to the standard input of a background shell started with Bash run_in_background — answer prompts, feed data to filters. Set close_stdin=true to signal EOF once you are done writing; stdin consumers (cat, read loops) usually finish only then.",
		func(ctx context.Context, a struct {
			ShellID    string `json:"shell_id"`
			Input      string `json:"input"`
			CloseStdin bool   `json:"close_stdin"`
		}) (coretools.Result, error) {
			id := strings.TrimSpace(a.ShellID)
			if err := bg.WriteStdin(id, a.Input, a.CloseStdin); err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			action := fmt.Sprintf("wrote %d bytes to shell %s stdin", len(a.Input), id)
			if a.CloseStdin {
				action += " and closed it (EOF)"
			}
			return coretools.Result{Content: action}, nil
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

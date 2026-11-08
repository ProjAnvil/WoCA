package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/tracing"
)

// BackgroundShells manages Bash commands started with run_in_background:
// the command runs detached while the turn continues, its combined output
// accumulates in memory, and completion fires a notification that the agent
// injects after the next tool round (the queued-attachment mechanism). Output
// is polled with BashOutput and the shell terminated with KillShell.
//
// Every access is session-scoped (tracing.VisibleTo): the tools derive the
// caller's session from the ctx run scope, HTTP callers pass it explicitly,
// and a shell owned by another session answers exactly like an unknown id —
// no existence leak.

// StopOrigin records who terminated a background shell.
type StopOrigin string

const (
	StopByModel StopOrigin = "model"
	StopByUser  StopOrigin = "user"
)

// ErrShellNotFound covers both unknown ids and shells owned by another
// session; the two cases are deliberately indistinguishable.
var ErrShellNotFound = errors.New("no such background shell")

// BackgroundShellStatus is a snapshot of one background shell.
type BackgroundShellStatus struct {
	ID          string
	Session     string
	Command     string
	Running     bool
	ExitCode    int        // -1 while running or after a signal kill (unchanged tool semantics)
	Killed      bool       // terminated through KillFor, any origin
	StoppedBy   StopOrigin // "" unless Killed
	StartedAt   time.Time
	EndedAt     time.Time // zero while running
	OutputBytes int
}

// ShellCompletion is one finished background shell handed to the notify
// callback: the starting run's session id, the rendered notification text
// and who killed the shell ("" when it exited on its own). StoppedBy is the
// quiet/attribution signal — the agent queues user stops as Quiet
// notifications that never wake the session (D1).
type ShellCompletion struct {
	Session   string
	Text      string
	StoppedBy StopOrigin
}

// notifyBackgroundDone, when set, is called once when a background shell
// finishes, with the completion facts. The agent wires this to its
// session-scoped mid-turn queue.
type BackgroundShells struct {
	mu       sync.Mutex
	next     int
	shells   map[string]*backgroundShell
	onDone   func(ShellCompletion)
	onChange func(session string)
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
	// buf is the kept tail of the combined output, at most
	// MaxBufferedOutputBytes; dropped counts the bytes the cap discarded
	// from the front (keep-tail, F3). Both live under mu.
	buf       []byte
	dropped   int
	mu        sync.Mutex
	done      chan struct{}
	exited    bool
	exit      int
	started   time.Time
	ended     time.Time
	killed    bool
	stoppedBy StopOrigin
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
// shell as fn(completion), carrying the starting run's session id (""
// outside a run scope), the rendered notification text and the stop
// attribution. It is called in the shell's run goroutine; it must not block.
func (b *BackgroundShells) SetNotify(fn func(ShellCompletion)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onDone = fn
}

// SetOnChange installs the change callback the activity seam subscribes to.
// It fires synchronously on shell start and on shell exit, with the owner
// session id; it must not block.
func (b *BackgroundShells) SetOnChange(fn func(session string)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.onChange = fn
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
		started: time.Now(),
	}
	onDone := b.onDone
	onChange := b.onChange
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
		// The id was never published, so there is nothing to unregister.
		return "", err
	}
	// proc is stored under sh.mu — KillFor and WriteStdinFor read it there —
	// and the shell joins the map only now that its process exists, so a
	// kill racing the start can never mark a never-signalled process as
	// killed: before the process is visible, it is not killable either.
	sh.mu.Lock()
	sh.proc = proc
	sh.mu.Unlock()
	b.mu.Lock()
	b.shells[id] = sh
	b.mu.Unlock()
	if onChange != nil {
		onChange(sh.session)
	}
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
		sh.ended = time.Now()
		sh.mu.Unlock()
		close(sh.done)
		// Prune this session's finished records while the shell is still
		// unpublished-as-changed: the onChange refetch below then observes
		// the already-pruned list (F4).
		b.pruneFinished(sh.session)
		if onChange != nil {
			onChange(sh.session)
		}
		if onDone != nil {
			text, stoppedBy := sh.completion()
			onDone(ShellCompletion{Session: sh.session, Text: text, StoppedBy: stoppedBy})
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

// lookup returns the shell with id when session may access it. A shell
// owned by another scoped session answers exactly like an unknown id, so
// callers cannot probe for foreign shells' existence.
func (b *BackgroundShells) lookup(session, id string) (*backgroundShell, error) {
	b.mu.Lock()
	sh := b.shells[id]
	b.mu.Unlock()
	if sh == nil || !tracing.VisibleTo(sh.session, session) {
		return nil, ErrShellNotFound
	}
	return sh, nil
}

// lineWriter appends to a background shell's buffer under its lock.
type lineWriter struct{ sh *backgroundShell }

func (w lineWriter) Write(p []byte) (int, error) {
	w.sh.mu.Lock()
	defer w.sh.mu.Unlock()
	w.sh.appendLocked(p)
	return len(p), nil
}

// appendLocked appends output bytes and enforces the keep-tail cap (F3):
// once the buffer passes MaxBufferedOutputBytes the overflow is dropped from
// the front and counted, and the cut is backed off to a UTF-8 rune boundary
// so a multibyte character is never split. Reslicing keeps this amortized
// O(1) per byte — the underlying array is only recopied when append grows
// it, roughly once per cap's worth of throughput.
func (sh *backgroundShell) appendLocked(p []byte) {
	sh.buf = append(sh.buf, p...)
	if overflow := len(sh.buf) - MaxBufferedOutputBytes; overflow > 0 {
		sh.buf = sh.buf[overflow:]
		sh.dropped += overflow
		for len(sh.buf) > 0 && !utf8.RuneStart(sh.buf[0]) {
			sh.buf = sh.buf[1:]
			sh.dropped++
		}
	}
}

// truncationMarker renders the loud dropped-bytes prefix (F3): one line,
// prepended once per read — not per poll batch — naming exactly how many
// older bytes the cap discarded.
func truncationMarker(dropped int) string {
	return fmt.Sprintf("[%d older bytes truncated]\n", dropped)
}

// outputLocked renders the shell's output for readers: the kept tail plus,
// when the cap dropped bytes, the truncation marker in front. The caller
// holds sh.mu.
func (sh *backgroundShell) outputLocked() string {
	if sh.dropped == 0 {
		return string(sh.buf)
	}
	return truncationMarker(sh.dropped) + string(sh.buf)
}

// snapshot renders the shell's current state (never races with the writer
// goroutine).
func (sh *backgroundShell) snapshot() BackgroundShellStatus {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.snapshotLocked()
}

// snapshotLocked is snapshot for callers already holding sh.mu.
func (sh *backgroundShell) snapshotLocked() BackgroundShellStatus {
	st := BackgroundShellStatus{
		ID:        sh.id,
		Session:   sh.session,
		Command:   sh.command,
		Running:   true,
		ExitCode:  -1,
		Killed:    sh.killed,
		StoppedBy: sh.stoppedBy,
		StartedAt: sh.started,
		// The honest total produced so far, dropped prefix included — the
		// kept buffer alone would under-report a capped shell (F3).
		OutputBytes: sh.dropped + len(sh.buf),
	}
	select {
	case <-sh.done:
		st.Running = false
		st.ExitCode = sh.exit
		st.EndedAt = sh.ended
	default:
	}
	return st
}

// ListFor returns snapshots of the shells a run in session can see, in
// numeric id (creation) order: everything for the unscoped session "" (the
// TUI/REPL single-session case), otherwise the unscoped shells plus the
// session's own. Finished records are pruned per session (F4), so the live
// map — not the historical id counter — is walked; ids are monotonic and
// never reused.
func (b *BackgroundShells) ListFor(session string) []BackgroundShellStatus {
	b.mu.Lock()
	shells := make([]*backgroundShell, 0, len(b.shells))
	for _, sh := range b.shells {
		if tracing.VisibleTo(sh.session, session) {
			shells = append(shells, sh)
		}
	}
	b.mu.Unlock()
	slices.SortFunc(shells, func(a, b *backgroundShell) int { return cmpNumericID(a.id, b.id) })
	out := make([]BackgroundShellStatus, 0, len(shells))
	for _, sh := range shells {
		out = append(out, sh.snapshot())
	}
	return out
}

// cmpNumericID orders numeric id strings by value ("2" before "10"), falling
// back to lexical order for anything non-numeric.
func cmpNumericID(a, b string) int {
	na, ea := strconv.Atoi(a)
	nb, eb := strconv.Atoi(b)
	if ea == nil && eb == nil {
		return cmp.Compare(na, nb)
	}
	return strings.Compare(a, b)
}

// pruneFinished caps the session's finished shell records at
// MaxFinishedRecordsPerSession, dropping the OLDEST finished records first
// (F4); running records are always kept. Called from a shell's exit
// goroutine after it settles, before its change callback fires.
func (b *BackgroundShells) pruneFinished(session string) {
	type fin struct {
		id    string
		num   int
		ended time.Time
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var fins []fin
	for id, sh := range b.shells {
		if sh.session != session {
			continue
		}
		// done is closed exactly at the terminal transition, so this needs
		// no per-shell lock; ended was written before the close.
		select {
		case <-sh.done:
		default:
			continue // running records are always kept
		}
		num, err := strconv.Atoi(id)
		if err != nil {
			continue // only counter-minted numeric ids are prunable
		}
		fins = append(fins, fin{id: id, num: num, ended: sh.ended})
	}
	if len(fins) <= MaxFinishedRecordsPerSession {
		return
	}
	slices.SortFunc(fins, func(a, b fin) int {
		if c := a.ended.Compare(b.ended); c != 0 {
			return c
		}
		return cmp.Compare(a.num, b.num)
	})
	for _, f := range fins[:len(fins)-MaxFinishedRecordsPerSession] {
		delete(b.shells, f.id)
	}
}

// DropSession removes every record the session owns, finished or running
// (F4): ReleaseSession calls it after killing the session's running shells —
// their exit goroutines finish invisibly, late completions are already
// silenced for a released session. The unscoped "" bucket is never dropped
// this way (it belongs to the local single-session surface). It returns how
// many records were removed.
func (b *BackgroundShells) DropSession(session string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for id, sh := range b.shells {
		if sh.session == session {
			delete(b.shells, id)
			n++
		}
	}
	return n
}

// StatusFor returns a snapshot of one shell, when session may see it.
func (b *BackgroundShells) StatusFor(session, id string) (BackgroundShellStatus, error) {
	sh, err := b.lookup(session, id)
	if err != nil {
		return BackgroundShellStatus{}, err
	}
	return sh.snapshot(), nil
}

// OutputFor returns the shell's buffered output — the kept tail plus a
// "[N older bytes truncated]" marker when the cap dropped bytes (F3) — and
// its status, when session may see it.
func (b *BackgroundShells) OutputFor(session, id string) (output string, status BackgroundShellStatus, err error) {
	sh, err := b.lookup(session, id)
	if err != nil {
		return "", BackgroundShellStatus{}, err
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.outputLocked(), sh.snapshotLocked(), nil
}

// TailFor returns at most the last maxBytes bytes of output plus the
// truncated flag and the full byte count, when session may see the shell.
// The cut point is backed off to a UTF-8 rune boundary so a multibyte
// character is never split; maxBytes <= 0 returns the whole buffer. When the
// buffer cap itself dropped bytes (F3), the marker is prepended and the
// truncated flag set regardless of maxBytes — the returned text is honestly
// flagged as not the complete output — and total keeps reporting the full
// logical byte count, dropped prefix included.
func (b *BackgroundShells) TailFor(session, id string, maxBytes int) (tail string, truncated bool, total int, st BackgroundShellStatus, err error) {
	sh, err := b.lookup(session, id)
	if err != nil {
		return "", false, 0, BackgroundShellStatus{}, err
	}
	sh.mu.Lock()
	tailBytes := sh.buf
	if maxBytes > 0 && len(tailBytes) > maxBytes {
		tailBytes = tailBytes[len(tailBytes)-maxBytes:]
		// A leading continuation byte means the first character straddles
		// the cut; drop its partial bytes so the tail starts on a rune
		// boundary.
		for len(tailBytes) > 0 && !utf8.RuneStart(tailBytes[0]) {
			tailBytes = tailBytes[1:]
		}
	}
	dropped := sh.dropped
	total = dropped + len(sh.buf)
	st = sh.snapshotLocked()
	sh.mu.Unlock()
	tail = string(tailBytes)
	if dropped > 0 {
		tail = truncationMarker(dropped) + tail
	}
	truncated = dropped > 0 || (maxBytes > 0 && total > maxBytes)
	return tail, truncated, total, st, nil
}

// WaitFor blocks until the shell finishes, d elapses, or ctx is canceled,
// then returns its snapshot: final status on completion, running status on
// timeout or cancellation. Unknown or foreign ids return ErrShellNotFound
// without blocking.
func (b *BackgroundShells) WaitFor(ctx context.Context, session, id string, d time.Duration) (BackgroundShellStatus, error) {
	sh, err := b.lookup(session, id)
	if err != nil {
		return BackgroundShellStatus{}, err
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
	return sh.snapshot(), nil
}

// WriteStdinFor writes input to a running shell's standard input and, when
// closeStdin is set, closes the pipe to signal EOF — the standard way to
// finish a stdin consumer such as cat. Writing to an unknown, foreign, or
// already finished shell is an error.
func (b *BackgroundShells) WriteStdinFor(session, id, input string, closeStdin bool) error {
	sh, err := b.lookup(session, id)
	if err != nil {
		return err
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

// KillFor terminates a running shell on behalf of by, recording the stop
// facts. Killing an already-finished shell changes nothing: it reports
// alreadyFinished=true with the unchanged status.
func (b *BackgroundShells) KillFor(session, id string, by StopOrigin) (st BackgroundShellStatus, alreadyFinished bool, err error) {
	sh, err := b.lookup(session, id)
	if err != nil {
		return BackgroundShellStatus{}, false, err
	}
	if b.killShell(sh, by) {
		return sh.snapshot(), true, nil
	}
	return sh.snapshot(), false, nil
}

// killSettleBound is how long KillAll waits, after signalling, for killed
// shells to leave the running state: the local executor escalates a group
// SIGTERM to SIGKILL after executor.KillGrace on an in-process timer, and
// the settle margin covers the teardown and pipe close that follow. Bounded
// so shutdown — and tests — can never hang on an unkillable process.
const killSettleBound = executor.KillGrace + time.Second

// KillAll terminates every running shell in the manager on behalf of by —
// shutdown cleanup (Agent.Close), where session scoping no longer matters
// because the whole manager is going away. Finished shells keep their
// recorded state. It also backs the Ctrl+C story: the local executor runs
// each shell in its own process group, so terminal signals reach none of
// the children and explicit termination is the only path that stops them.
//
// After signalling, KillAll waits up to killSettleBound for the killed
// shells to settle: the SIGKILL escalation runs on a timer inside this
// process, so a KillAll that returned earlier would lose it as soon as the
// caller exits (graceful shutdown exits right after Agent.Close) and a
// TERM-ignoring tree would survive as an orphan. It returns early once
// every killed shell has settled.
func (b *BackgroundShells) KillAll(by StopOrigin) {
	b.mu.Lock()
	shells := make([]*backgroundShell, 0, len(b.shells))
	for _, sh := range b.shells {
		shells = append(shells, sh)
	}
	b.mu.Unlock()
	var killed []*backgroundShell
	for _, sh := range shells {
		if !b.killShell(sh, by) {
			killed = append(killed, sh)
		}
	}
	// Wait out the escalation window: once a shell's done channel closes,
	// its process — and, for the local executor, its whole group, SIGKILL
	// included — is gone, so no in-process timer for it is pending anymore.
	deadline := time.Now().Add(killSettleBound)
	for _, sh := range killed {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		timer := time.NewTimer(remaining)
		select {
		case <-sh.done:
			timer.Stop()
		case <-timer.C:
			// Settle bound elapsed; stay bounded rather than block shutdown.
			return
		}
	}
}

// killShell records the stop facts on a running shell and signals its
// process. It reports whether the shell had already exited on its own, in
// which case nothing is changed.
func (b *BackgroundShells) killShell(sh *backgroundShell, by StopOrigin) (alreadyFinished bool) {
	sh.mu.Lock()
	if sh.exited {
		sh.mu.Unlock()
		return true
	}
	// Record the stop facts while holding sh.mu and before proc.Kill(), so
	// the exit goroutine (which takes this lock to record the exit) can
	// never publish an exited shell without its stop attribution. Kill
	// itself runs after the release: a remote executor's Kill does I/O and
	// must not block the shell's output writers on this lock.
	sh.killed = true
	sh.stoppedBy = by
	proc := sh.proc
	sh.mu.Unlock()
	if proc != nil {
		_ = proc.Kill()
	}
	return false
}

// NotificationOutputCap bounds the output tail included in a completion
// notification so a chatty command cannot flood the context.
const NotificationOutputCap = 2000

// MaxBufferedOutputBytes caps the combined output one background shell
// buffers in memory (keep-tail, spec F3): once the cap is reached the buffer
// keeps the newest bytes and counts the dropped prefix, which reads surface
// as a loud "[N older bytes truncated]" marker — never silent loss.
const MaxBufferedOutputBytes = 1 << 20

// MaxFinishedRecordsPerSession caps how many finished records each session
// keeps per manager (spec F4) — the newest finished background shells here
// and the newest finished tasks in agent.TaskManager; running records are
// always kept, and the unscoped ("") bucket follows the same rule.
const MaxFinishedRecordsPerSession = 20

// MaxWaitMS is the largest BashOutput wait_ms (10 minutes); longer requests
// are clamped so a typo cannot pin a tool round for hours.
const MaxWaitMS = 600000

// completion renders the completion body handed to the notify callback and
// reports who killed the shell ("" for a natural exit). The first line is
// stop-attributed (PM §4.6): a user kill says who did it, a model kill names
// the tool, a natural exit keeps the exit-code line. The body carries no
// wrapper tags — the queue tag the agent wraps around this text is the
// single <background-task-completed> wrapper the model sees.
func (sh *backgroundShell) completion() (text string, stoppedBy StopOrigin) {
	sh.mu.Lock()
	st := BackgroundShellStatus{ID: sh.id, Command: sh.command, ExitCode: sh.exit, StoppedBy: sh.stoppedBy}
	out := string(sh.buf)
	sh.mu.Unlock()
	var b strings.Builder
	switch st.StoppedBy {
	case StopByUser:
		fmt.Fprintf(&b, "Background shell %s was killed by the user: %s\n", st.ID, st.Command)
	case StopByModel:
		fmt.Fprintf(&b, "Background shell %s was killed (KillShell): %s\n", st.ID, st.Command)
	default:
		fmt.Fprintf(&b, "Background shell %s finished (exit code %d): %s\n", st.ID, st.ExitCode, st.Command)
	}
	if out != "" {
		if len(out) > NotificationOutputCap {
			out = "…\n" + out[len(out)-NotificationOutputCap:]
		}
		fmt.Fprintf(&b, "Output (tail):\n%s\n", strings.TrimRight(out, "\n"))
	}
	return b.String(), st.StoppedBy
}

// bashOutputTool polls a background shell's output, optionally blocking
// until it finishes.
func bashOutputTool(bg *BackgroundShells) coretools.Tool {
	t := MustTool(coretools.FromFunc("BashOutput",
		"Get the current output and status of a background shell started with Bash run_in_background. Returns the buffered stdout/stderr (at most the newest 1 MiB is kept; when older output was dropped, the text starts with a \"[N older bytes truncated]\" marker naming the dropped byte count), whether it is still running, and its exit code once finished. Set wait_ms to block inside this call until the shell finishes or the wait elapses (max 600000ms), instead of polling repeatedly.",
		func(ctx context.Context, a struct {
			ShellID string `json:"shell_id"`
			WaitMS  int    `json:"wait_ms,omitempty"`
		}) (coretools.Result, error) {
			id := strings.TrimSpace(a.ShellID)
			session := sessionFromCtx(ctx)
			if a.WaitMS > 0 {
				wait := time.Duration(min(a.WaitMS, MaxWaitMS)) * time.Millisecond
				// Unknown and foreign ids return immediately from Wait;
				// OutputFor renders the error below.
				_, _ = bg.WaitFor(ctx, session, id, wait)
			}
			out, st, err := bg.OutputFor(session, id)
			if err != nil {
				// The listing carries only the shells this session can see,
				// and the message names no shell id at all — a foreign id
				// must stay indistinguishable from a nonexistent one.
				return coretools.Result{Content: fmt.Sprintf("Error: no background shell (started shells: %s)", formatShellIDs(bg.ListFor(session)))}, nil
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
			st, alreadyFinished, err := bg.KillFor(sessionFromCtx(ctx), strings.TrimSpace(a.ShellID), StopByModel)
			if err != nil {
				return coretools.Result{Content: "Error: " + err.Error()}, nil
			}
			if alreadyFinished {
				return coretools.Result{Content: fmt.Sprintf("shell %s already finished (exit code %d)", st.ID, st.ExitCode)}, nil
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
			if err := bg.WriteStdinFor(sessionFromCtx(ctx), id, a.Input, a.CloseStdin); err != nil {
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

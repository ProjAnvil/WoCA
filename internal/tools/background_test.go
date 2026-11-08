package tools

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/runexec"
	"github.com/projanvil/woca/internal/tracing"
)

func TestBackgroundShellLifecycle(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	bg := reg.Background()

	bash, ok := reg.Get("Bash")
	if !ok {
		t.Fatal("Bash tool missing")
	}
	res, err := bash.Invoke(t.Context(), map[string]any{
		"command":           "echo started; sleep 1; echo done",
		"run_in_background": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "[Background] started shell 1") {
		t.Fatalf("background start = %q", res.Content)
	}

	// Polling before completion reports running state and interim output.
	var out string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var err error
		out, _, err = bg.OutputFor("", "1")
		if err != nil {
			t.Fatal("shell 1 vanished")
		}
		if strings.Contains(out, "started") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "started") {
		t.Fatalf("interim output missing \"started\": %q", out)
	}
	if _, st, _ := bg.OutputFor("", "1"); !st.Running {
		t.Fatalf("shell finished too early: %+v", st)
	}

	// KillShell terminates the running shell.
	if _, _, err := bg.KillFor("", "1", StopByModel); err != nil {
		t.Fatal(err)
	}
	killDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(killDeadline) {
		if _, st, _ := bg.OutputFor("", "1"); !st.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, st, _ := bg.OutputFor("", "1"); st.Running {
		t.Fatal("shell still running after kill")
	}
}

func TestBackgroundShellCompletionNotification(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	notifications := make(chan string, 4)
	bg.SetNotify(func(c ShellCompletion) {
		if c.Session != "" {
			t.Errorf("session = %q, want empty for an unscoped ctx", c.Session)
		}
		notifications <- c.Text
	})

	id, err := bg.Start(t.Context(), "echo hello-bg", "", reg.dir.Base())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-notifications:
		// R4: the producer emits the body only — the queue tag is the single
		// wrapper — so the raw callback text carries no wrapper lines.
		if strings.Contains(text, "<background-task-completed>") ||
			strings.Contains(text, "</background-task-completed>") {
			t.Fatalf("producer text must carry no wrapper: %q", text)
		}
		if !strings.Contains(text, "hello-bg") || !strings.Contains(text, "hello") {
			t.Fatalf("notification = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
	}
	if _, st, _ := bg.OutputFor("", id); st.Running || st.ExitCode != 0 {
		t.Fatalf("final status = %+v", st)
	}
}

// The completion callback receives the session id of the run that started
// the shell, so notifications route to that session's mid-turn queue.
func TestBackgroundShellNotificationCarriesSession(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	type sessionNote struct{ session, text string }
	got := make(chan sessionNote, 2)
	bg.SetNotify(func(c ShellCompletion) { got <- sessionNote{c.Session, c.Text} })

	ctx := tracing.WithRun(t.Context(), "run-1", "sess-1")
	if _, err := bg.Start(ctx, "echo scoped", "", "."); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n.session != "sess-1" {
			t.Fatalf("session = %q, want %q", n.session, "sess-1")
		}
		if strings.Contains(n.text, "<background-task-completed>") || !strings.Contains(n.text, "scoped") {
			t.Fatalf("notification = %q, want wrapper-free body naming the command", n.text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
	}

	// A ctx without a run scope degrades to the unscoped session "".
	if _, err := bg.Start(t.Context(), "true", "", "."); err != nil {
		t.Fatal(err)
	}
	select {
	case n := <-got:
		if n.session != "" {
			t.Fatalf("session = %q, want empty for an unscoped ctx", n.session)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification for unscoped shell")
	}
}

func TestBackgroundShellNotificationOutputCap(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	done := make(chan string, 1)
	bg.SetNotify(func(c ShellCompletion) { done <- c.Text })
	if _, err := bg.Start(t.Context(), "head -c 9000 /dev/zero | tr '\\0' 'x'", "", "."); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-done:
		if len(text) > NotificationOutputCap+500 {
			t.Fatalf("notification too long: %d", len(text))
		}
		if !strings.Contains(text, "…") {
			t.Fatalf("expected truncated tail marker: %q", text[:80])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no completion notification")
	}
}

// Wait blocks until the shell finishes, the duration elapses, or ctx is
// canceled; every branch returns promptly (bounded waits, no hang).
func TestBackgroundShellWait(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	// Wait until completion reports the final status, no earlier than the
	// command's own runtime.
	id, err := bg.Start(t.Context(), "sleep 0.3 && echo done", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	st, err := bg.WaitFor(t.Context(), "", id, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running || st.ExitCode != 0 {
		t.Fatalf("waited status = %+v", st)
	}
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("Wait returned before the command finished: %s", elapsed)
	}

	// A short wait on a long shell reports running when the deadline passes.
	id2, err := bg.Start(t.Context(), "sleep 5", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _, _ = bg.KillFor("", id2, StopByModel) })
	start = time.Now()
	st, err = bg.WaitFor(t.Context(), "", id2, 200*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatalf("status = %+v, want running", st)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Wait blocked %s past its deadline", elapsed)
	}

	// A canceled ctx unblocks Wait immediately with the running status.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	st, err = bg.WaitFor(cctx, "", id2, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running {
		t.Fatalf("canceled status = %+v, want running", st)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Wait ignored ctx cancellation: %s", elapsed)
	}

	// Unknown IDs report not-found instead of blocking.
	if _, err := bg.WaitFor(t.Context(), "", "99", time.Second); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("Wait on unknown id err = %v, want ErrShellNotFound", err)
	}
}

// BashInput feeds a background shell's stdin; cat echoes the input while
// running and finishes once close_stdin signals EOF. Unknown or finished
// shells error instead of hanging.
func TestBashInputToolFeedsStdin(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()
	input, ok := reg.Get("BashInput")
	if !ok {
		t.Fatal("BashInput tool missing")
	}
	poll, _ := reg.Get("BashOutput")

	id, err := bg.Start(t.Context(), "cat", "", ".")
	if err != nil {
		t.Fatal(err)
	}

	res, err := input.Invoke(t.Context(), map[string]any{"shell_id": id, "input": "hi\n"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "Error") {
		t.Fatalf("BashInput = %q", res.Content)
	}

	// The written bytes reach the process: cat echoes them while running.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		res, _ = poll.Invoke(t.Context(), map[string]any{"shell_id": id})
		if strings.Contains(res.Content, "hi") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(res.Content, "hi") {
		t.Fatalf("BashOutput never showed the stdin echo: %q", res.Content)
	}
	if !strings.Contains(res.Content, "running") {
		t.Fatalf("cat finished before close_stdin: %q", res.Content)
	}

	// Closing stdin signals EOF; cat exits 0 with the echoed input.
	res, err = input.Invoke(t.Context(), map[string]any{"shell_id": id, "close_stdin": true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Content, "Error") {
		t.Fatalf("BashInput close = %q", res.Content)
	}
	st, err := bg.WaitFor(t.Context(), "", id, 5*time.Second)
	if err != nil || st.Running || st.ExitCode != 0 {
		t.Fatalf("cat after close_stdin = %+v err=%v", st, err)
	}
	if out, _, _ := bg.OutputFor("", id); !strings.Contains(out, "hi") {
		t.Fatalf("final output missing stdin echo: %q", out)
	}

	// Unknown IDs error instead of hanging.
	res, _ = input.Invoke(t.Context(), map[string]any{"shell_id": "99", "input": "x"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("BashInput unknown id = %q", res.Content)
	}
	// Finished shells reject input.
	res, _ = input.Invoke(t.Context(), map[string]any{"shell_id": id, "input": "x"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("BashInput finished shell = %q", res.Content)
	}
}

func TestBashOutputAndKillShellTools(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	poll, ok := reg.Get("BashOutput")
	if !ok {
		t.Fatal("BashOutput tool missing")
	}
	kill, ok := reg.Get("KillShell")
	if !ok {
		t.Fatal("KillShell tool missing")
	}

	if _, err := bg.Start(t.Context(), "sleep 5", "", "."); err != nil {
		t.Fatal(err)
	}
	res, err := poll.Invoke(t.Context(), map[string]any{"shell_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "running") {
		t.Fatalf("BashOutput = %q", res.Content)
	}

	res, err = kill.Invoke(t.Context(), map[string]any{"shell_id": "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "shell 1") {
		t.Fatalf("KillShell = %q", res.Content)
	}

	// Unknown IDs surface a helpful error, and a finished shell reports done.
	res, _ = poll.Invoke(t.Context(), map[string]any{"shell_id": "99"})
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("BashOutput unknown id = %q", res.Content)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if res, _ = poll.Invoke(t.Context(), map[string]any{"shell_id": "1"}); strings.Contains(res.Content, "done") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("shell never reported done: %q", res.Content)
}

func TestBackgroundShellIDsIncrement(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()
	for i, cmd := range []string{"true", "false"} {
		id, err := bg.Start(t.Context(), cmd, "", ".")
		if err != nil {
			t.Fatal(err)
		}
		if want := strconv.Itoa(i + 1); id != want {
			t.Fatalf("id = %q, want %q", id, want)
		}
	}
	// Both exit; statuses reflect exit codes.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		_, s1, _ := bg.OutputFor("", "1")
		_, s2, _ := bg.OutputFor("", "2")
		if !s1.Running && !s2.Running {
			if s1.ExitCode != 0 || s2.ExitCode != 1 {
				t.Fatalf("exit codes = %+v %+v", s1, s2)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("shells did not finish")
}

// Background shells capture the invoking context (design §10.1 C-3): the
// process starts under context.WithoutCancel(ctx) so it outlives the turn,
// while the ctx values — including the run executor — survive. A sandboxed
// run therefore starts its background shells in the sandbox, not on the
// host behind the sandbox's back.
func TestBackgroundShellCapturesRunExecutor(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir)
	fake := newFakeStartExecutor()
	parent, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	ctx := runexec.WithRunExecutor(parent, fake)

	bg := reg.Background()
	id, err := bg.Start(ctx, "long-running", "", dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _, _ = bg.KillFor("", id, StopByModel) })

	<-fake.started
	if len(fake.reqs) != 1 || fake.reqs[0].Name != "bash" {
		t.Fatalf("start requests = %+v", fake.reqs)
	}
	// The values survived WithoutCancel: the started process's ctx still
	// resolves the run executor.
	if _, ok := runexec.RunExecutorFrom(fake.startCtx); !ok {
		t.Fatal("run executor missing from the background start ctx")
	}
	// Canceling the parent (turn end) must not kill the background shell.
	cancel()
	if err := fake.startCtx.Err(); err != nil {
		t.Fatalf("background ctx canceled by parent: %v", err)
	}
}

// N1: the background tools must be session-scoped. A run in session B must
// not read, write to, or kill a shell started by session A, and an unknown-id
// error must list only the shells the calling session can see (not every
// session's ids).
func TestBackgroundShellToolsAreSessionScoped(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()
	poll, ok := reg.Get("BashOutput")
	if !ok {
		t.Fatal("BashOutput tool missing")
	}
	kill, ok := reg.Get("KillShell")
	if !ok {
		t.Fatal("KillShell tool missing")
	}
	input, ok := reg.Get("BashInput")
	if !ok {
		t.Fatal("BashInput tool missing")
	}

	ctxA := tracing.WithRun(t.Context(), "run-A", "sess-A")
	aID, err := bg.Start(ctxA, "echo leaky-secret; sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _, _ = bg.KillFor("", aID, StopByModel) })
	// A second A-owned shell the caller never names: its id must not leak
	// through the unknown-id error listing either.
	if _, err := bg.Start(ctxA, "sleep 30", "", "."); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _, _ = bg.KillFor("", "2", StopByModel) })

	ctxB := tracing.WithRun(t.Context(), "run-B", "sess-B")

	// BashOutput on A's shell must fail without echoing A's output or ids.
	res, err := poll.Invoke(ctxB, map[string]any{"shell_id": aID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("session B read session A's shell: %q", res.Content)
	}
	if strings.Contains(res.Content, "leaky-secret") {
		t.Fatalf("session B saw session A's output: %q", res.Content)
	}
	if strings.Contains(res.Content, aID) {
		t.Fatalf("error text leaks session A's shell id %q: %q", aID, res.Content)
	}

	// KillShell on A's shell must fail and leave it running.
	res, err = kill.Invoke(ctxB, map[string]any{"shell_id": aID})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("session B killed session A's shell: %q", res.Content)
	}
	if strings.Contains(res.Content, aID) {
		t.Fatalf("error text leaks session A's shell id %q: %q", aID, res.Content)
	}
	if _, st, _ := bg.OutputFor("sess-A", aID); !st.Running {
		t.Fatal("session A's shell died from session B's kill")
	}

	// BashInput on A's shell must fail too.
	res, err = input.Invoke(ctxB, map[string]any{"shell_id": aID, "input": "x"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("session B wrote to session A's shell stdin: %q", res.Content)
	}
	if strings.Contains(res.Content, aID) {
		t.Fatalf("error text leaks session A's shell id %q: %q", aID, res.Content)
	}

	// The unknown-id error lists only the calling session's visible shells —
	// B sees none of A's ids.
	res, err = poll.Invoke(ctxB, map[string]any{"shell_id": "99"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Content, "Error") {
		t.Fatalf("unknown id = %q", res.Content)
	}
	if strings.Contains(res.Content, aID) || strings.Contains(res.Content, "2") {
		t.Fatalf("unknown-id error leaks session A's shell ids: %q", res.Content)
	}
}

// The PM §4.1 ownership table at the manager level: owner A / viewer A
// visible; owner A / viewer B not-found; owner "" / viewer B visible;
// viewer "" sees all. Every scoped accessor enforces the same rule.
func TestBackgroundShellVisibilityTable(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	ctxA := tracing.WithRun(t.Context(), "run-A", "sess-A")
	aID, err := bg.Start(ctxA, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	ctxB := tracing.WithRun(t.Context(), "run-B", "sess-B")
	bID, err := bg.Start(ctxB, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	uID, err := bg.Start(t.Context(), "sleep 30", "", ".") // unscoped owner ""
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{aID, bID, uID} {
		t.Cleanup(func() { _, _, _ = bg.KillFor("", id, StopByModel) })
	}

	// owner A / viewer A sees it.
	if _, err := bg.StatusFor("sess-A", aID); err != nil {
		t.Fatalf("owner A / viewer A: %v", err)
	}
	// owner A / viewer B gets not-found, identical to an unknown id.
	if _, err := bg.StatusFor("sess-B", aID); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("owner A / viewer B err = %v, want ErrShellNotFound", err)
	}
	// owner "" / viewer B sees it (unscoped items are everyone's).
	if _, err := bg.StatusFor("sess-B", uID); err != nil {
		t.Fatalf("owner unscoped / viewer B: %v", err)
	}
	// viewer "" sees everything.
	if all := bg.ListFor(""); len(all) != 3 {
		t.Fatalf("unscoped viewer sees %d shells, want 3: %+v", len(all), all)
	}
	// viewer B sees exactly its own shell plus the unscoped one.
	got := map[string]bool{}
	for _, st := range bg.ListFor("sess-B") {
		got[st.ID] = true
	}
	if len(got) != 2 || !got[bID] || !got[uID] || got[aID] {
		t.Fatalf("viewer B list = %v, want exactly {%s, %s}", got, bID, uID)
	}

	// The same rule holds for every scoped accessor.
	if _, _, err := bg.OutputFor("sess-B", aID); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("OutputFor foreign err = %v", err)
	}
	if _, err := bg.WaitFor(t.Context(), "sess-B", aID, time.Second); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("WaitFor foreign err = %v", err)
	}
	if _, _, _, _, err := bg.TailFor("sess-B", aID, 16); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("TailFor foreign err = %v", err)
	}
	if err := bg.WriteStdinFor("sess-B", aID, "x", false); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("WriteStdinFor foreign err = %v", err)
	}
	if _, _, err := bg.KillFor("sess-B", aID, StopByUser); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("KillFor foreign err = %v", err)
	}
	// None of that touched A's shell.
	if st, err := bg.StatusFor("sess-A", aID); err != nil || !st.Running {
		t.Fatalf("A's shell affected by B's foreign access: %+v err=%v", st, err)
	}
}

// Lifecycle facts: StartedAt/EndedAt/ExitCode on natural exits, Killed/
// StoppedBy after KillFor, and alreadyFinished with an unchanged status for
// a shell that already ended.
func TestBackgroundShellLifecycleFacts(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()
	ctx := tracing.WithRun(t.Context(), "run-A", "sess-A")

	// Natural success: exit 0, both timestamps set, no stop facts.
	id, err := bg.Start(ctx, "true", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	st, err := bg.WaitFor(t.Context(), "sess-A", id, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if st.Running || st.ExitCode != 0 || st.Killed || st.StoppedBy != "" {
		t.Fatalf("natural exit status = %+v", st)
	}
	if st.Session != "sess-A" || st.Command != "true" {
		t.Fatalf("identity fields = %+v", st)
	}
	if st.StartedAt.IsZero() || st.EndedAt.IsZero() || st.EndedAt.Before(st.StartedAt) {
		t.Fatalf("timestamps = %+v", st)
	}

	// Natural failure keeps the concrete exit code.
	id, err = bg.Start(ctx, "false", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if st, err = bg.WaitFor(t.Context(), "sess-A", id, 5*time.Second); err != nil || st.ExitCode != 1 {
		t.Fatalf("false status = %+v err=%v", st, err)
	}

	// A running shell: started recorded, no end yet, running exit code -1.
	id, err = bg.Start(ctx, "sleep 30", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _, _ = bg.KillFor("", id, StopByModel) })
	st, err = bg.StatusFor("sess-A", id)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Running || st.ExitCode != -1 || st.StartedAt.IsZero() || !st.EndedAt.IsZero() {
		t.Fatalf("running status = %+v", st)
	}

	// KillFor records who stopped it; the shell dies with exit code -1.
	st, alreadyFinished, err := bg.KillFor("sess-A", id, StopByUser)
	if err != nil || alreadyFinished {
		t.Fatalf("KillFor running shell = %+v already=%v err=%v", st, alreadyFinished, err)
	}
	if st, err = bg.WaitFor(t.Context(), "sess-A", id, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if st.Running || !st.Killed || st.StoppedBy != StopByUser || st.ExitCode != -1 || st.EndedAt.IsZero() {
		t.Fatalf("killed status = %+v", st)
	}

	// Killing the finished shell again changes nothing.
	again, alreadyFinished, err := bg.KillFor("sess-A", id, StopByModel)
	if err != nil {
		t.Fatal(err)
	}
	if !alreadyFinished {
		t.Fatal("KillFor on finished shell: alreadyFinished=false")
	}
	if again.Killed != st.Killed || again.StoppedBy != st.StoppedBy || again.ExitCode != st.ExitCode ||
		!again.EndedAt.Equal(st.EndedAt) || again.Running {
		t.Fatalf("finished shell status changed by second kill: %+v vs %+v", again, st)
	}
}

// TailFor returns a byte-bounded output tail: the truncated flag and full
// byte count, with the cut point backed off to a UTF-8 rune boundary so
// multibyte characters are never split.
func TestBackgroundShellTailFor(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()
	ctx := tracing.WithRun(t.Context(), "run-A", "sess-A")

	// 6 ASCII bytes + 4 three-byte Han runes = 18 bytes.
	const text = "abcdef你好世界"
	id, err := bg.Start(ctx, "printf '"+text+"'", "", ".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bg.WaitFor(t.Context(), "sess-A", id, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// maxBytes <= 0 returns the whole buffer, never truncated.
	tail, truncated, total, st, err := bg.TailFor("sess-A", id, 0)
	if err != nil || tail != text || truncated || total != len(text) {
		t.Fatalf("full tail = %q truncated=%v total=%d err=%v", tail, truncated, total, err)
	}
	if st.OutputBytes != len(text) {
		t.Fatalf("OutputBytes = %d, want %d", st.OutputBytes, len(text))
	}

	// Output within the bound is returned whole.
	if tail, truncated, _, _, err = bg.TailFor("sess-A", id, len(text)); err != nil || tail != text || truncated {
		t.Fatalf("exact-bound tail = %q truncated=%v err=%v", tail, truncated, err)
	}

	// A bound aligned with a rune boundary keeps the full last runes.
	if tail, truncated, total, _, err = bg.TailFor("sess-A", id, 6); err != nil || !truncated || total != 18 || tail != "世界" {
		t.Fatalf("rune-aligned tail = %q truncated=%v total=%d err=%v", tail, truncated, total, err)
	}

	// A bound slicing through a rune backs off to the rune boundary.
	tail, truncated, total, _, err = bg.TailFor("sess-A", id, 8)
	if err != nil || !truncated || total != 18 || tail != "世界" {
		t.Fatalf("mid-rune tail = %q truncated=%v total=%d err=%v", tail, truncated, total, err)
	}
	if !utf8.ValidString(tail) || !strings.HasSuffix(text, tail) {
		t.Fatalf("tail not a rune-boundary suffix: %q", tail)
	}

	// A bound too small for any whole rune yields an empty tail, still
	// flagged truncated.
	if tail, truncated, _, _, err = bg.TailFor("sess-A", id, 1); err != nil || !truncated || tail != "" {
		t.Fatalf("1-byte tail = %q truncated=%v err=%v", tail, truncated, err)
	}

	// Foreign sessions get not-found like everywhere else.
	if _, _, _, _, err := bg.TailFor("sess-B", id, 16); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("TailFor foreign err = %v", err)
	}
}

// SetOnChange fires once on start and once on exit, each carrying the owner
// session, and nothing else.
func TestBackgroundShellOnChange(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	events := make(chan string, 4)
	bg.SetOnChange(func(session string) { events <- session })

	ctx := tracing.WithRun(t.Context(), "run-A", "sess-A")
	if _, err := bg.Start(ctx, "true", "", "."); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"sess-A", "sess-A"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("change event session = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no change event (wanted %q)", want)
		}
	}
	select {
	case got := <-events:
		t.Fatalf("unexpected extra change event %q", got)
	case <-time.After(300 * time.Millisecond):
	}

	// An unscoped start fires with "".
	if _, err := bg.Start(t.Context(), "true", "", "."); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"", ""} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("unscoped change event session = %q, want %q", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("no unscoped change event (wanted %q)", want)
		}
	}
}

// fakeStartExecutor records the Start call and hands out a controllable
// process.
type fakeStartExecutor struct {
	startCtx  context.Context
	reqs      []executor.ExecRequest
	started   chan struct{}
	startOnce sync.Once
}

func newFakeStartExecutor() *fakeStartExecutor {
	return &fakeStartExecutor{started: make(chan struct{})}
}

func (f *fakeStartExecutor) Start(ctx context.Context, req executor.ExecRequest, _ executor.StartOptions) (executor.Process, error) {
	f.startCtx = ctx
	f.reqs = append(f.reqs, req)
	f.startOnce.Do(func() { close(f.started) })
	return &fakeProcess{done: make(chan struct{})}, nil
}

func (f *fakeStartExecutor) Exec(context.Context, executor.ExecRequest) (executor.ExecResult, error) {
	return executor.ExecResult{}, nil
}
func (f *fakeStartExecutor) ReadFile(context.Context, string) ([]byte, error) { return nil, nil }
func (f *fakeStartExecutor) WriteFile(context.Context, string, []byte, fs.FileMode) error {
	return nil
}
func (f *fakeStartExecutor) MkdirAll(context.Context, string, fs.FileMode) error { return nil }
func (f *fakeStartExecutor) Stat(context.Context, string) (fs.FileInfo, error) {
	return nil, fs.ErrNotExist
}
func (f *fakeStartExecutor) Glob(context.Context, string) ([]string, error) { return nil, nil }
func (f *fakeStartExecutor) Grep(context.Context, string, *regexp.Regexp) ([]executor.GrepMatch, error) {
	return nil, nil
}

// fakeProcess blocks in Wait until killed.
type fakeProcess struct {
	done    chan struct{}
	killOne sync.Once
}

func (p *fakeProcess) Stdin() io.WriteCloser { return nopWriteCloser{} }
func (p *fakeProcess) Stdout() io.Reader     { return nil }
func (p *fakeProcess) Wait() error           { <-p.done; return nil }
func (p *fakeProcess) Kill() error {
	p.killOne.Do(func() { close(p.done) })
	return nil
}

// TestBackgroundShellKilledNotificationAttribution pins the PM §4.6 first
// lines and the stop attribution on the notify callback: a user kill says who
// did it, a model kill names the tool, and a natural completion keeps the
// exit-code line. The output tail still rides along whenever the shell
// produced output.
func TestBackgroundShellKilledNotificationAttribution(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	type note struct {
		text string
		by   StopOrigin
	}
	got := make(chan note, 4)
	bg.SetNotify(func(c ShellCompletion) { got <- note{text: c.Text, by: c.StoppedBy} })

	firstLine := func(body string) string {
		line, _, _ := strings.Cut(body, "\n")
		return line
	}
	start := func(t *testing.T, command string) string {
		t.Helper()
		id, err := bg.Start(t.Context(), command, "", reg.dir.Base())
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("user kill", func(t *testing.T) {
		id := start(t, "echo user-killed-output && sleep 30")
		// Wait for the echo to land in the buffer before killing, so the
		// output tail is guaranteed to be part of the notification.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if out, _, err := bg.OutputFor("", id); err == nil && strings.Contains(out, "user-killed-output") {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, already, err := bg.KillFor("", id, StopByUser); err != nil || already {
			t.Fatalf("KillFor(user) = err %v already %v", err, already)
		}
		select {
		case n := <-got:
			if n.by != StopByUser {
				t.Fatalf("StoppedBy = %q, want %q", n.by, StopByUser)
			}
			want := "Background shell " + id + " was killed by the user: echo user-killed-output && sleep 30"
			if line := firstLine(n.text); line != want {
				t.Fatalf("first line = %q, want %q", line, want)
			}
			if !strings.Contains(n.text, "Output (tail):\nuser-killed-output") {
				t.Fatalf("notification missing output tail: %q", n.text)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no kill notification")
		}
	})

	t.Run("model kill", func(t *testing.T) {
		id := start(t, "sleep 30")
		if _, already, err := bg.KillFor("", id, StopByModel); err != nil || already {
			t.Fatalf("KillFor(model) = err %v already %v", err, already)
		}
		select {
		case n := <-got:
			if n.by != StopByModel {
				t.Fatalf("StoppedBy = %q, want %q", n.by, StopByModel)
			}
			want := "Background shell " + id + " was killed (KillShell): sleep 30"
			if line := firstLine(n.text); line != want {
				t.Fatalf("first line = %q, want %q", line, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no kill notification")
		}
	})

	t.Run("natural completion", func(t *testing.T) {
		id := start(t, "true")
		select {
		case n := <-got:
			if n.by != "" {
				t.Fatalf("StoppedBy = %q, want empty for a natural exit", n.by)
			}
			want := "Background shell " + id + " finished (exit code 0): true"
			if line := firstLine(n.text); line != want {
				t.Fatalf("first line = %q, want %q", line, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("no completion notification")
		}
	})
}

// waitShellDone blocks (bounded) until the shell leaves the running state.
func waitShellDone(t *testing.T, bg *BackgroundShells, session, id string) {
	t.Helper()
	if _, err := bg.WaitFor(t.Context(), session, id, 10*time.Second); err != nil {
		t.Fatal(err)
	}
	st, err := bg.StatusFor(session, id)
	if err != nil || st.Running {
		t.Fatalf("shell %s did not finish: err=%v status=%+v", id, err, st)
	}
}

// F3: the combined output a background shell buffers is capped at
// MaxBufferedOutputBytes (keep-tail). The dropped prefix is counted, the
// counter is surfaced loudly — one "[N older bytes truncated]" marker per
// read, never per poll batch — and the honest total byte count survives.
func TestBackgroundShellOutputCapKeepsTailWithMarker(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	// 200000 numbered lines of 7 bytes each: a fast generator producing
	// 1 400 000 bytes, comfortably past the 1 MiB cap, no sleeping.
	const lines = 200000
	const lineLen = 7 // "%06d\n"
	cmd := fmt.Sprintf("awk 'BEGIN{for(i=1;i<=%d;i++)printf \"%%06d\\n\",i}'", lines)
	id, err := bg.Start(t.Context(), cmd, "", ".")
	if err != nil {
		t.Fatal(err)
	}
	waitShellDone(t, bg, "", id)

	total := lines * lineLen
	dropped := total - MaxBufferedOutputBytes
	marker := fmt.Sprintf("[%d older bytes truncated]\n", dropped)

	out, st, err := bg.OutputFor("", id)
	if err != nil {
		t.Fatal(err)
	}
	if st.OutputBytes != total {
		t.Fatalf("OutputBytes = %d, want the honest total %d", st.OutputBytes, total)
	}
	if !strings.HasPrefix(out, marker) {
		t.Fatalf("output missing dropped-bytes marker, prefix = %q", out[:min(80, len(out))])
	}
	if strings.Count(out, "older bytes truncated") != 1 {
		t.Fatalf("marker must appear exactly once per read, got %d", strings.Count(out, "older bytes truncated"))
	}
	body := out[len(marker):]
	if len(body) != MaxBufferedOutputBytes {
		t.Fatalf("kept tail = %d bytes, want exactly the cap %d", len(body), MaxBufferedOutputBytes)
	}
	if !strings.HasSuffix(body, fmt.Sprintf("%06d\n", lines)) {
		t.Fatalf("tail does not end with the newest line: %q", body[len(body)-40:])
	}
	if strings.Contains(body, "000001\n") {
		t.Fatal("the oldest output line survived the cap")
	}

	// TailFor carries the same facts: the marker plus a bounded tail, the
	// truncated flag set, and the full logical byte count.
	tail, truncated, tailTotal, _, err := bg.TailFor("", id, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || tailTotal != total {
		t.Fatalf("tail truncated=%v total=%d, want true/%d", truncated, tailTotal, total)
	}
	if !strings.HasPrefix(tail, marker) {
		t.Fatalf("tail missing dropped-bytes marker: %q", tail[:min(80, len(tail))])
	}
	if len(tail) != len(marker)+100 {
		t.Fatalf("tail length = %d, want marker + 100 bytes", len(tail))
	}
	if !strings.HasSuffix(tail, fmt.Sprintf("%06d\n", lines)) {
		t.Fatalf("tail does not end with the newest line: %q", tail[len(tail)-40:])
	}
}

// F4: finished shell records are capped per session — the newest
// MaxFinishedRecordsPerSession finished records stay, running records are
// always kept, and the unscoped ("") bucket follows the same rule.
func TestBackgroundShellFinishedRecordsCappedPerSession(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()
	t.Cleanup(func() { bg.KillAll(StopByUser) })
	sess := tracing.WithRun(t.Context(), "run-cap", "sess-cap")

	// A running shell is retained no matter how much finishes after it.
	if _, err := bg.Start(sess, "sleep 60", "", "."); err != nil {
		t.Fatal(err)
	}
	// Sequential quick shells: the finish order equals the id order, so the
	// newest finished set is exactly the highest ids.
	for range MaxFinishedRecordsPerSession + 6 {
		id, err := bg.Start(sess, "true", "", ".")
		if err != nil {
			t.Fatal(err)
		}
		waitShellDone(t, bg, "sess-cap", id)
	}

	list := bg.ListFor("sess-cap")
	if len(list) != MaxFinishedRecordsPerSession+1 {
		t.Fatalf("sess-cap keeps %d records, want running + %d newest finished", len(list), MaxFinishedRecordsPerSession)
	}
	if list[0].ID != "1" || !list[0].Running {
		t.Fatalf("the running shell must be kept, got %+v", list[0])
	}
	if list[1].ID != "8" || list[len(list)-1].ID != "27" {
		t.Fatalf("kept finished set = %s…%s, want 8…27", list[1].ID, list[len(list)-1].ID)
	}
	for _, id := range []string{"2", "3", "4", "5", "6", "7"} {
		if _, err := bg.StatusFor("sess-cap", id); !errors.Is(err, ErrShellNotFound) {
			t.Fatalf("pruned shell %s still resolvable: %v", id, err)
		}
	}

	// The unscoped ("") bucket follows the same rule; other sessions are
	// untouched by it.
	for range MaxFinishedRecordsPerSession + 2 {
		id, err := bg.Start(t.Context(), "true", "", ".")
		if err != nil {
			t.Fatal(err)
		}
		waitShellDone(t, bg, "", id)
	}
	if _, err := bg.StatusFor("", "28"); !errors.Is(err, ErrShellNotFound) {
		t.Fatalf("pruned unscoped shell 28 still resolvable: %v", err)
	}
	// The scoped viewer also sees unscoped shells, so count only its own.
	own := 0
	for _, st := range bg.ListFor("sess-cap") {
		if st.Session == "sess-cap" {
			own++
		}
	}
	if own != MaxFinishedRecordsPerSession+1 {
		t.Fatalf("unscoped churn pruned another session's records: %d own records survive, want %d", own, MaxFinishedRecordsPerSession+1)
	}
}

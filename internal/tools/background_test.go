package tools

import (
	"context"
	"io"
	"io/fs"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/runexec"
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
		var ok bool
		out, _, ok = bg.Output("1")
		if !ok {
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
	if _, st, _ := bg.Output("1"); !st.Running {
		t.Fatalf("shell finished too early: %+v", st)
	}

	// KillShell terminates the running shell.
	if _, err := bg.Kill("1"); err != nil {
		t.Fatal(err)
	}
	killDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(killDeadline) {
		if _, st, _ := bg.Output("1"); !st.Running {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, st, _ := bg.Output("1"); st.Running {
		t.Fatal("shell still running after kill")
	}
}

func TestBackgroundShellCompletionNotification(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	notifications := make(chan string, 4)
	bg.SetNotify(func(text string) { notifications <- text })

	id, err := bg.Start(t.Context(), "echo hello-bg", "", reg.dir.Base())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-notifications:
		if !strings.Contains(text, "<background-task-completed>") ||
			!strings.Contains(text, "hello-bg") || !strings.Contains(text, "hello") {
			t.Fatalf("notification = %q", text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no completion notification")
	}
	if _, st, _ := bg.Output(id); st.Running || st.ExitCode != 0 {
		t.Fatalf("final status = %+v", st)
	}
}

func TestBackgroundShellNotificationOutputCap(t *testing.T) {
	reg := NewRegistry(t.TempDir())
	bg := reg.Background()

	done := make(chan string, 1)
	bg.SetNotify(func(text string) { done <- text })
	if _, err := bg.Start(t.Context(), "head -c 9000 /dev/zero | tr '\\0' 'x'", "", "."); err != nil {
		t.Fatal(err)
	}
	select {
	case text := <-done:
		if len(text) > notificationOutputCap+500 {
			t.Fatalf("notification too long: %d", len(text))
		}
		if !strings.Contains(text, "…") {
			t.Fatalf("expected truncated tail marker: %q", text[:80])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no completion notification")
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
		_, s1, _ := bg.Output("1")
		_, s2, _ := bg.Output("2")
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
	t.Cleanup(func() { _, _ = bg.Kill(id) })

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

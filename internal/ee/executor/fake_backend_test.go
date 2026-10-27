package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// --- helpers ---------------------------------------------------------------

// newBackend returns a FakeBackend whose lifetime is bound to the test.
func newBackend(t *testing.T) *FakeBackend {
	t.Helper()
	be, err := NewFakeBackend()
	if err != nil {
		t.Fatalf("NewFakeBackend: %v", err)
	}
	t.Cleanup(func() { _ = be.Close() })
	return be
}

func mustCreate(t *testing.T, be Backend, spec CreateSpec) Container {
	t.Helper()
	c, err := be.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create(%+v): %v", spec, err)
	}
	return c
}

func mustStart(t *testing.T, c Container, cmd ...string) Process {
	t.Helper()
	p, err := c.Start(context.Background(), ExecSpec{Cmd: cmd})
	if err != nil {
		t.Fatalf("Start(%q): %v", cmd, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func mustExec(t *testing.T, c Container, req ExecSpec) ExecResult {
	t.Helper()
	res, err := c.Exec(context.Background(), req)
	if err != nil {
		t.Fatalf("Exec(%q): %v", req.Cmd, err)
	}
	return res
}

// waitFor polls cond every 10ms until it holds or the deadline expires.
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", desc)
}

// streamText concatenates the data of all events on one stream.
func streamText(events []OutputEvent, stream string) string {
	var b strings.Builder
	for _, e := range events {
		if e.Stream == stream {
			b.Write(e.Data)
		}
	}
	return b.String()
}

// buildTar packs files (plus parent dir entries) into a plain tar stream.
func buildTar(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for name := range files {
		for d := path.Dir(name); d != "." && d != "/"; d = path.Dir(d) {
			dirs[d] = true
		}
	}
	for _, d := range slices.Sorted(maps.Keys(dirs)) {
		if err := tw.WriteHeader(&tar.Header{Name: d + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatalf("tar dir header %s: %v", d, err)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(files)) {
		data := []byte(files[name])
		hdr := &tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("tar header %s: %v", name, err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatalf("tar write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

// readTarEntries parses a tar stream into cleaned-name -> file content.
func readTarEntries(t *testing.T, r io.Reader) map[string]string {
	t.Helper()
	entries := map[string]string{}
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatalf("tar next: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("tar read %s: %v", hdr.Name, err)
		}
		entries[path.Clean(hdr.Name)] = string(data)
	}
}

// --- Backend: Create / List -------------------------------------------------

func TestFakeBackend_Create(t *testing.T) {
	be := newBackend(t)

	if _, err := be.Create(context.Background(), CreateSpec{}); err == nil {
		t.Fatal("Create with empty Image: want error")
	}

	c := mustCreate(t, be, CreateSpec{
		Image:   "woca-sandbox:latest",
		WorkDir: "/workspace",
		Labels:  map[string]string{"plane": "ee", "run": "r1"},
	})
	if c.ID() == "" {
		t.Fatal("Create returned an empty container ID")
	}

	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Running {
		t.Error("fresh container reports Running; want false until a process starts")
	}
	if st.ExitCode != 0 {
		t.Errorf("fresh container ExitCode = %d; want 0", st.ExitCode)
	}
	if got := st.Labels["plane"]; got != "ee" {
		t.Errorf("State.Labels[plane] = %q; want %q", got, "ee")
	}
}

func TestFakeBackend_List_LabelFilter(t *testing.T) {
	be := newBackend(t)
	specs := []CreateSpec{
		{Image: "img", Labels: map[string]string{"plane": "test", "tier": "a"}},
		{Image: "img", Labels: map[string]string{"plane": "test", "tier": "b"}},
		{Image: "img", Labels: map[string]string{"plane": "other"}},
	}
	ids := map[string]bool{}
	for _, spec := range specs {
		c := mustCreate(t, be, spec)
		ids[c.ID()] = true
	}

	tests := []struct {
		name   string
		filter map[string]string
		want   int
	}{
		{"no filter lists all", nil, 3},
		{"single pair", map[string]string{"plane": "test"}, 2},
		{"multiple pairs intersect", map[string]string{"plane": "test", "tier": "a"}, 1},
		{"no match", map[string]string{"plane": "nope"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := be.List(context.Background(), tt.filter)
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(got) != tt.want {
				t.Fatalf("List(%v) returned %d containers; want %d", tt.filter, len(got), tt.want)
			}
			for _, c := range got {
				if !ids[c.ID()] {
					t.Errorf("List returned unknown container %s", c.ID())
				}
			}
		})
	}
}

// --- Container: Exec ---------------------------------------------------------

func TestFakeContainer_Exec(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img", WorkDir: "/workspace"})
	mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", "mkdir -p in && echo alpha > in/a.txt"}, Dir: "/"})

	sh := func(script string) []string { return []string{"sh", "-c", script} }
	tests := []struct {
		name       string
		req        ExecSpec
		wantStdout string
		wantStderr string
		wantExit   int
		wantErr    bool
	}{
		{
			name:       "stdout",
			req:        ExecSpec{Cmd: sh("echo hi")},
			wantStdout: "hi\n",
			wantExit:   0,
		},
		{
			name:     "exit code passthrough",
			req:      ExecSpec{Cmd: sh("exit 3")},
			wantExit: 3,
		},
		{
			name:       "stderr",
			req:        ExecSpec{Cmd: sh("echo boom >&2")},
			wantStderr: "boom\n",
			wantExit:   0,
		},
		{
			name:       "stdin",
			req:        ExecSpec{Cmd: []string{"cat"}, Stdin: []byte("piped\n")},
			wantStdout: "piped\n",
			wantExit:   0,
		},
		{
			name:       "env overlay keeps PATH",
			req:        ExecSpec{Cmd: sh(`printf %s "$FAKE_EXEC_PROBE"`), Env: []string{"FAKE_EXEC_PROBE=zz"}},
			wantStdout: "zz",
			wantExit:   0,
		},
		{
			name:       "dir absolute container path",
			req:        ExecSpec{Cmd: sh("cat a.txt"), Dir: "/in"},
			wantStdout: "alpha\n",
			wantExit:   0,
		},
		{
			name:       "dir relative container path",
			req:        ExecSpec{Cmd: sh("cat a.txt"), Dir: "in"},
			wantStdout: "alpha\n",
			wantExit:   0,
		},
		{
			name:     "missing binary",
			req:      ExecSpec{Cmd: []string{"woca-definitely-missing-binary"}},
			wantExit: -1,
			wantErr:  true,
		},
		{
			name:     "empty cmd",
			req:      ExecSpec{},
			wantExit: -1,
			wantErr:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := c.Exec(context.Background(), tt.req)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Exec(%q) error = nil; want error", tt.req.Cmd)
				}
			} else if err != nil {
				t.Fatalf("Exec(%q): %v", tt.req.Cmd, err)
			}
			if res.ExitCode != tt.wantExit {
				t.Errorf("ExitCode = %d; want %d", res.ExitCode, tt.wantExit)
			}
			if got := string(res.Stdout); got != tt.wantStdout {
				t.Errorf("Stdout = %q; want %q", got, tt.wantStdout)
			}
			if got := string(res.Stderr); got != tt.wantStderr {
				t.Errorf("Stderr = %q; want %q", got, tt.wantStderr)
			}
		})
	}
}

func TestFakeContainer_Exec_DefaultDirIsWorkDir(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img", WorkDir: "/workspace"})

	mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", "echo mark > m.txt"}})
	res := mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", "cat m.txt"}, Dir: "/workspace"})
	if got := string(res.Stdout); got != "mark\n" {
		t.Errorf("file written with empty Dir not found under WorkDir: stdout = %q", got)
	}
}

func TestFakeContainer_Exec_Timeout(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	res, err := c.Exec(context.Background(), ExecSpec{
		Cmd:     []string{"sh", "-c", "sleep 5"},
		Timeout: 50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("Exec past its timeout: want error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v; want context.DeadlineExceeded", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("timed-out ExitCode = %d; want non-zero", res.ExitCode)
	}
}

// --- Container: Start / Process ----------------------------------------------

func TestFakeProcess_Streams(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	p := mustStart(t, c, "sh", "-c", "echo out; echo err >&2")
	waitFor(t, "process to finish", func() bool {
		_, _, done, _ := p.Snapshot(0)
		return done
	})

	events, next, done, exit := p.Snapshot(0)
	if !done {
		t.Fatal("Snapshot done = false after wait")
	}
	if exit != 0 {
		t.Errorf("exit = %d; want 0", exit)
	}
	if want := len("out\n") + len("err\n"); next != want {
		t.Errorf("next = %d; want %d", next, want)
	}
	if got := streamText(events, "stdout"); got != "out\n" {
		t.Errorf("stdout stream = %q; want %q", got, "out\n")
	}
	if got := streamText(events, "stderr"); got != "err\n" {
		t.Errorf("stderr stream = %q; want %q", got, "err\n")
	}
}

func TestFakeProcess_SnapshotOffsets(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	p := mustStart(t, c, "sh", "-c", "while :; do echo tick; sleep 0.05; done")

	// First read: at least three ticks, next offset equals consumed bytes.
	var first []OutputEvent
	var next int
	waitFor(t, "three ticks", func() bool {
		first, next, _, _ = p.Snapshot(0)
		return next >= 3*len("tick\n")
	})
	if got := streamText(first, "stdout"); strings.Count(got, "tick\n") < 3 {
		t.Fatalf("first batch = %q; want at least three ticks", got)
	}

	// Incremental read: nothing at or before the last offset repeats. New
	// ticks only arrive every 50ms, so poll until one shows up.
	var second []OutputEvent
	var next2 int
	waitFor(t, "new ticks past the first offset", func() bool {
		second, next2, _, _ = p.Snapshot(next)
		return len(second) > 0
	})
	for _, e := range second {
		if e.Start < next {
			t.Fatalf("event with Start %d re-delivered below offset %d", e.Start, next)
		}
	}
	if next2 < next {
		t.Fatalf("next2 = %d < previous next = %d", next2, next)
	}
	joined := streamText(first, "stdout") + streamText(second, "stdout")
	for line := range strings.SplitSeq(joined, "\n") {
		if line != "" && line != "tick" {
			t.Fatalf("unexpected line %q in joined stream", line)
		}
	}

	// Kill makes the process done; the tail is still readable from next2.
	if err := p.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitFor(t, "process to die", func() bool {
		_, _, done, _ := p.Snapshot(0)
		return done
	})
	_, _, done, exit := p.Snapshot(next2)
	if !done {
		t.Fatal("done = false after Kill")
	}
	if exit == 0 {
		t.Errorf("killed exit = %d; want non-zero", exit)
	}
	// Kill on an already-dead process is nil-tolerant.
	if err := p.Kill(context.Background()); err != nil {
		t.Errorf("Kill after exit: %v; want nil", err)
	}
}

func TestFakeProcess_StdinRoundTrip(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	p := mustStart(t, c, "cat")
	if err := p.WriteStdin([]byte("hi\n")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	if err := p.WriteStdin([]byte("there\n")); err != nil {
		t.Fatalf("WriteStdin: %v", err)
	}
	if err := p.CloseStdin(); err != nil {
		t.Fatalf("CloseStdin: %v", err)
	}
	if err := p.CloseStdin(); err != nil {
		t.Errorf("second CloseStdin: %v; want nil (idempotent)", err)
	}

	waitFor(t, "cat to finish", func() bool {
		_, _, done, _ := p.Snapshot(0)
		return done
	})
	events, _, done, exit := p.Snapshot(0)
	if !done || exit != 0 {
		t.Fatalf("done = %v exit = %d; want done with exit 0", done, exit)
	}
	if got := streamText(events, "stdout"); got != "hi\nthere\n" {
		t.Errorf("stdout = %q; want %q", got, "hi\nthere\n")
	}
	if err := p.WriteStdin([]byte("late\n")); err == nil {
		t.Error("WriteStdin after CloseStdin: want error")
	}
}

func TestFakeProcess_WriteStdinBlockedByFullPipe(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	// sleep never reads stdin, so a write larger than the pipe buffer
	// (~64KB) parks inside the write while holding no process state.
	p, err := c.Start(context.Background(), ExecSpec{Cmd: []string{"sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- p.WriteStdin(bytes.Repeat([]byte("x"), 1<<20))
	}()
	time.Sleep(150 * time.Millisecond) // let the writer reach the blocked state

	closeDone := make(chan error, 1)
	go func() { closeDone <- p.Close() }()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Errorf("Close during blocked stdin write: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close deadlocked behind a blocked stdin write (lock held across pipe write)")
	}

	// Closing the pipe unblocks the parked write; it must report an error,
	// not hang.
	select {
	case err := <-writeDone:
		if err == nil {
			t.Error("blocked WriteStdin after Close: want error, got nil")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteStdin never returned after Close")
	}

	// Kill still answers while the process is being torn down.
	if err := p.Kill(context.Background()); err != nil {
		t.Errorf("Kill after Close: %v", err)
	}
	waitFor(t, "process to die", func() bool {
		_, _, done, _ := p.Snapshot(0)
		return done
	})
}

func TestFakeProcess_CloseKillsRunningProcess(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	p, err := c.Start(context.Background(), ExecSpec{Cmd: []string{"sh", "-c", "sleep 30"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitFor(t, "process to die after Close", func() bool {
		_, _, done, _ := p.Snapshot(0)
		return done
	})
	if err := p.Close(); err != nil {
		t.Errorf("second Close: %v; want nil (idempotent)", err)
	}
}

// --- Container: State --------------------------------------------------------

func TestFakeContainer_State_RunningTracksSessions(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	st, err := c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.Running {
		t.Fatal("fresh container Running = true; want false")
	}

	p := mustStart(t, c, "sh", "-c", "sleep 30")
	waitFor(t, "container to report running", func() bool {
		st, _ := c.State(context.Background())
		return st.Running
	})

	if err := p.Kill(context.Background()); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waitFor(t, "container to stop running", func() bool {
		st, _ := c.State(context.Background())
		return !st.Running
	})
	st, err = c.State(context.Background())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if st.ExitCode == 0 {
		t.Errorf("ExitCode after killed session = %d; want non-zero", st.ExitCode)
	}
}

// --- Container: CopyIn / CopyOut ----------------------------------------------

func TestFakeContainer_CopyRoundtrip(t *testing.T) {
	files := map[string]string{"a.txt": "alpha", "sub/b.txt": "beta"}
	tarball := buildTar(t, files)

	for _, format := range []struct {
		name  string
		input func() io.Reader
	}{
		{"plain tar", func() io.Reader { return bytes.NewReader(tarball) }},
		{"gzip tar", func() io.Reader {
			var gz bytes.Buffer
			zw := gzip.NewWriter(&gz)
			if _, err := zw.Write(tarball); err != nil {
				t.Fatalf("gzip: %v", err)
			}
			if err := zw.Close(); err != nil {
				t.Fatalf("gzip close: %v", err)
			}
			return bytes.NewReader(gz.Bytes())
		}},
	} {
		t.Run(format.name, func(t *testing.T) {
			be := newBackend(t)
			c := mustCreate(t, be, CreateSpec{Image: "img"})

			if err := c.CopyIn(context.Background(), "/data", format.input()); err != nil {
				t.Fatalf("CopyIn: %v", err)
			}
			// Copied files are visible to exec sessions in that directory.
			res := mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", "cat a.txt sub/b.txt"}, Dir: "/data"})
			if got := string(res.Stdout); got != "alphabeta" {
				t.Errorf("exec in /data stdout = %q; want %q", got, "alphabeta")
			}

			// CopyOut mirrors Docker archive naming: entries live under the
			// base name of the requested path.
			rc, err := c.CopyOut(context.Background(), "/data")
			if err != nil {
				t.Fatalf("CopyOut: %v", err)
			}
			defer rc.Close()
			entries := readTarEntries(t, rc)
			want := map[string]string{
				"data/a.txt":     "alpha",
				"data/sub/b.txt": "beta",
			}
			if !maps.Equal(entries, want) {
				t.Errorf("CopyOut entries = %v; want %v", entries, want)
			}
		})
	}
}

func TestFakeContainer_CopyOut_MissingPath(t *testing.T) {
	be := newBackend(t)
	c := mustCreate(t, be, CreateSpec{Image: "img"})

	if _, err := c.CopyOut(context.Background(), "/nope"); err == nil {
		t.Fatal("CopyOut of missing path: want error")
	}
}

// --- Container: Remove --------------------------------------------------------

func TestFakeContainer_Remove(t *testing.T) {
	t.Run("force kills live sessions", func(t *testing.T) {
		be := newBackend(t)
		c := mustCreate(t, be, CreateSpec{Image: "img", Labels: map[string]string{"plane": "ee"}})
		p := mustStart(t, c, "sh", "-c", "sleep 30")

		if err := c.Remove(context.Background(), false); err == nil {
			t.Fatal("Remove(force=false) with a live session: want error")
		}
		if err := c.Remove(context.Background(), true); err != nil {
			t.Fatalf("Remove(force=true): %v", err)
		}
		waitFor(t, "session to die after forced remove", func() bool {
			_, _, done, _ := p.Snapshot(0)
			return done
		})

		if _, err := c.State(context.Background()); !errors.Is(err, ErrRemoved) {
			t.Errorf("State after Remove: err = %v; want ErrRemoved", err)
		}
		if _, err := c.Exec(context.Background(), ExecSpec{Cmd: []string{"sh", "-c", "true"}}); !errors.Is(err, ErrRemoved) {
			t.Errorf("Exec after Remove: err = %v; want ErrRemoved", err)
		}
		if err := c.Remove(context.Background(), true); !errors.Is(err, ErrRemoved) {
			t.Errorf("second Remove: err = %v; want ErrRemoved", err)
		}
		listed, err := be.List(context.Background(), map[string]string{"plane": "ee"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(listed) != 0 {
			t.Errorf("List after Remove returned %d containers; want 0", len(listed))
		}
	})

	t.Run("idle container removes without force", func(t *testing.T) {
		be := newBackend(t)
		c := mustCreate(t, be, CreateSpec{Image: "img"})
		if err := c.Remove(context.Background(), false); err != nil {
			t.Fatalf("Remove(force=false) on idle container: %v", err)
		}
	})
}

// --- Container: workspace bind ("mount") ---------------------------------------
//
// The fake's stand-in for the bind mount (spec §5.3): a container created
// with WorkspaceDir resolves /workspace onto that host directory, so
// exec's writes, CopyIn unpacks, and CopyOut reads all land on the real
// files in place — no staging, no copy back.

func TestFakeContainer_WorkspaceBindWritesInPlace(t *testing.T) {
	be := newBackend(t)
	store, _ := newTestStore(t)
	ws := mustWorkspaceDir(t, store, "ws-mount")

	c := mustCreate(t, be, CreateSpec{
		Image:        "img",
		WorkDir:      "/workspace",
		WorkspaceDir: ws,
	})

	// An exec with the default working directory writes straight onto the
	// host-side workspace directory.
	res := mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", "echo mounted > product.txt"}})
	if res.ExitCode != 0 {
		t.Fatalf("exec exit %d, stderr %q", res.ExitCode, res.Stderr)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "product.txt")); err != nil || string(b) != "mounted\n" {
		t.Fatalf("workspace file after exec = (%q, %v), want %q", b, err, "mounted\n")
	}

	// CopyIn into /workspace unpacks onto the same directory.
	if err := c.CopyIn(context.Background(), "/workspace", bytes.NewReader(buildTar(t, map[string]string{"imported/d.txt": "delta"}))); err != nil {
		t.Fatalf("CopyIn: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "imported", "d.txt")); err != nil || string(b) != "delta" {
		t.Fatalf("imported file = (%q, %v)", b, err)
	}

	// CopyOut of /workspace reads the same directory back.
	rc, err := c.CopyOut(context.Background(), "/workspace")
	if err != nil {
		t.Fatalf("CopyOut: %v", err)
	}
	defer rc.Close()
	entries := readTarEntries(t, rc)
	if got := entries["workspace/product.txt"]; got != "mounted\n" {
		t.Errorf("CopyOut workspace/product.txt = %q, want %q", got, "mounted\n")
	}

	// A pre-existing host file is visible from the container (the mount
	// is the directory, not a copy of it). Fake-container commands see
	// host paths, so the probe cats the host path of /workspace/pre.txt.
	if err := os.WriteFile(filepath.Join(ws, "pre.txt"), []byte("already-there"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = mustExec(t, c, ExecSpec{Cmd: []string{"cat", filepath.Join(ws, "pre.txt")}, Dir: "/workspace"})
	if got := string(res.Stdout); got != "already-there" {
		t.Errorf("pre-existing host file seen as %q, want %q", got, "already-there")
	}

	// Removing the container never deletes the workspace directory: it
	// belongs to the store, not the container.
	if err := c.Remove(context.Background(), true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(ws, "product.txt")); err != nil || string(b) != "mounted\n" {
		t.Fatalf("workspace content after container removal = (%q, %v)", b, err)
	}
}

// A missing or non-directory WorkspaceDir fails Create explicitly — the
// mount cannot silently degrade into an unmounted /workspace.
func TestFakeBackend_CreateWorkspaceDirMustExist(t *testing.T) {
	be := newBackend(t)
	missing := filepath.Join(t.TempDir(), "nope")
	if _, err := be.Create(context.Background(), CreateSpec{Image: "img", WorkDir: "/workspace", WorkspaceDir: missing}); err == nil {
		t.Fatal("Create with missing WorkspaceDir succeeded, want error")
	}

	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := be.Create(context.Background(), CreateSpec{Image: "img", WorkDir: "/workspace", WorkspaceDir: file}); err == nil {
		t.Fatal("Create with a file WorkspaceDir succeeded, want error")
	}
}

// Paths outside /workspace keep resolving onto the container's own root —
// the mount covers exactly the workspace directory, nothing wider.
func TestFakeContainer_WorkspaceBindScoped(t *testing.T) {
	be := newBackend(t)
	store, _ := newTestStore(t)
	ws := mustWorkspaceDir(t, store, "ws-scope")

	c := mustCreate(t, be, CreateSpec{Image: "img", WorkDir: "/workspace", WorkspaceDir: ws})
	mustExec(t, c, ExecSpec{Cmd: []string{"sh", "-c", "mkdir -p out && echo outside > out/note.txt"}, Dir: "/"})

	if entries, _ := os.ReadDir(ws); len(entries) != 0 {
		t.Fatalf("write outside /workspace leaked into the workspace: %v", entries)
	}
	res := mustExec(t, c, ExecSpec{Cmd: []string{"cat", "out/note.txt"}, Dir: "/"})
	if got := string(res.Stdout); got != "outside\n" {
		t.Errorf("/out/note.txt = %q, want %q", got, "outside\n")
	}
}

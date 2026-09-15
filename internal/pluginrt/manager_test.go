package pluginrt

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

func TestManagerStatusesRunStates(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())

	// A failed run keeps its detail and log tail visible.
	m.runs["pdf"] = &installRun{
		state:  StateError,
		detail: "download uv: no route",
		log:    []string{"step one", "step two"},
		done:   make(chan struct{}),
	}
	// A completed run for browser: ready state derives its detail.
	m.runs["browser"] = &installRun{state: StateReady, done: make(chan struct{})}
	if err := os.MkdirAll(filepath.Dir(m.br.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.br.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	byName := map[string]Status{}
	for _, st := range m.Statuses([]string{"pdf"}) {
		byName[st.Name] = st
	}
	pdf := byName["pdf"]
	if pdf.State != StateError || pdf.Detail != "download uv: no route" {
		t.Fatalf("pdf status = %+v", pdf)
	}
	if len(pdf.Log) != 2 || pdf.Log[1] != "step two" {
		t.Fatalf("pdf log = %v", pdf.Log)
	}
	if pdf.Enabled != true || byName["browser"].Enabled {
		t.Fatal("enabled flags wrong")
	}
	browser := byName["browser"]
	if browser.State != StateReady || browser.Detail != "pinchtab "+pinchtabVer {
		t.Fatalf("browser status = %+v", browser)
	}

	// An in-flight run shows the installing state.
	m2 := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	m2.runs["pdf"] = &installRun{state: StateInstalling, done: make(chan struct{})}
	for _, st := range m2.Statuses(nil) {
		if st.Name == "pdf" && st.State != StateInstalling {
			t.Fatalf("installing state = %q", st.State)
		}
	}

	// A ready python env without any run also reports ready, with the
	// versioned detail from the ENV_OK stamp.
	m3 := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	stampPythonEnv(t, m3.py, "pdf", nil)
	for _, st := range m3.Statuses(nil) {
		if st.Name == "pdf" {
			if st.State != StateReady {
				t.Fatalf("pdf state = %q, want ready", st.State)
			}
			if !strings.HasPrefix(st.Detail, "uv "+uvVersion+" · CPython "+pythonPin+" · lock ") {
				t.Fatalf("pdf detail = %q", st.Detail)
			}
			break
		}
	}
}

func TestManagerReadyAndDetailDispatch(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())

	if ok, detail := m.ready(mustBuiltin(t, "pdf")); ok || detail == "" {
		t.Fatalf("python not ready: %v %q", ok, detail)
	}
	if ok, detail := m.ready(mustBuiltin(t, "browser")); ok || detail == "" {
		t.Fatalf("pinchtab not ready: %v %q", ok, detail)
	}
	if err := os.MkdirAll(filepath.Dir(m.br.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.br.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, detail := m.ready(mustBuiltin(t, "browser")); !ok || detail != "" {
		t.Fatalf("pinchtab ready: %v %q", ok, detail)
	}
	// Unknown runtimes are vacuously ready.
	if ok, _ := m.ready(Builtin{Name: "odd", Runtime: "carrier-pigeon"}); !ok {
		t.Fatal("unknown runtime must be ready")
	}

	// readyDetail reads the python stamp, degrading on missing/junk files.
	if got := m.readyDetail(mustBuiltin(t, "pdf")); got != "" {
		t.Fatalf("detail without stamp = %q", got)
	}
	stampPythonEnv(t, m.py, "pdf", nil)
	if got := m.readyDetail(mustBuiltin(t, "pdf")); !strings.Contains(got, "lock ") || len(got[strings.Index(got, "lock "):]) != len("lock ")+7 {
		t.Fatalf("detail = %q, want 7-char lock prefix", got)
	}
	if err := os.WriteFile(filepath.Join(m.py.envDir("pdf"), "ENV_OK"), []byte("<<junk>>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.readyDetail(mustBuiltin(t, "pdf")); got != "" {
		t.Fatalf("junk stamp detail = %q", got)
	}
	if got := m.readyDetail(Builtin{Name: "odd"}); got != "" {
		t.Fatalf("unknown runtime detail = %q", got)
	}

	// The install dispatcher rejects plugins without an installable runtime.
	if err := m.install(t.Context(), Builtin{Name: "odd"}, nil); err == nil ||
		!strings.Contains(err.Error(), "no installable runtime") {
		t.Fatalf("install dispatch: %v", err)
	}
}

// waitState polls Statuses until the plugin reaches want (failing fast on an
// unexpected error state).
func waitState(t *testing.T, m *Manager, name string, want InstallState) Status {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		for _, st := range m.Statuses(nil) {
			if st.Name != name {
				continue
			}
			if st.State == want {
				return st
			}
			if st.State == StateError && want != StateError {
				t.Fatalf("plugin %s errored: %s", name, st.Detail)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("plugin %s never reached %s", name, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestManagerInstallBrowserAlreadyPresent(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m.Close()
	if err := os.MkdirAll(filepath.Dir(m.br.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.br.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.InstallWait(t.Context(), "browser"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if st := waitState(t, m, "browser", StateReady); st.Detail != "pinchtab "+pinchtabVer {
		t.Fatalf("browser detail = %q", st.Detail)
	}
}

func TestManagerInstallBrowserDownloadFails(t *testing.T) {
	// No binary on disk: the pinned GitHub download is refused by the dead
	// proxy from TestMain, so the background run lands in the error state
	// without touching the network.
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m.Close()
	if err := m.InstallWait(t.Context(), "browser"); err == nil ||
		!strings.Contains(err.Error(), "download pinchtab") {
		t.Fatalf("err = %v, want download pinchtab", err)
	}
	st := waitState(t, m, "browser", StateError)
	if st.Detail == "" || len(st.Log) == 0 {
		t.Fatalf("error status lost detail/log: %+v", st)
	}
}

func TestManagerInstallPDFWithFakeUV(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m.Close()
	writeFakeUV(t, m.py)
	if err := m.InstallWait(t.Context(), "pdf"); err != nil {
		t.Fatalf("install: %v", err)
	}
	waitState(t, m, "pdf", StateReady)
	if !m.py.Ready("pdf") {
		t.Fatal("pdf env not ready after manager install")
	}
}

func TestManagerInstallFinishesButNotReady(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m.Close()
	writeFakeUV(t, m.py)
	t.Setenv("FAKE_UV_MAKE_PYTHON", "0")
	err := m.InstallWait(t.Context(), "pdf")
	if err == nil || !strings.Contains(err.Error(), "install finished but runtime still not ready") {
		t.Fatalf("err = %v", err)
	}
	waitState(t, m, "pdf", StateError)
}

func TestManagerInstallWaitContextCanceled(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m.Close()
	writeFakeUV(t, m.py)
	t.Setenv("FAKE_UV_SLEEP", "1")
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if err := m.InstallWait(ctx, "pdf"); err == nil {
		t.Fatal("canceled InstallWait must fail")
	}
}

func TestManagerReinstall(t *testing.T) {
	ctx := t.Context()

	if err := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop()).Reinstall(ctx, "ghost"); err == nil {
		t.Fatal("unknown plugin reinstall must fail")
	}

	// Browser: the binary is removed, the re-download fails via the dead
	// proxy, and the run lands in the error state.
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m.Close()
	if err := os.MkdirAll(filepath.Dir(m.br.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.br.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.Reinstall(ctx, "browser"); err != nil {
		t.Fatalf("reinstall kick-off: %v", err)
	}
	waitState(t, m, "browser", StateError)

	// A non-empty directory at the binary slot: the removal itself fails.
	m2 := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	if err := os.MkdirAll(filepath.Join(m2.br.binaryPath(), "junk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m2.Reinstall(ctx, "browser"); err == nil {
		t.Fatal("reinstall with unremovable binary must fail")
	}

	// Python: the env dir is wiped and re-provisioned by the fake uv.
	m3 := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	defer m3.Close()
	writeFakeUV(t, m3.py)
	if err := m3.Reinstall(ctx, "pdf"); err != nil {
		t.Fatalf("reinstall kick-off: %v", err)
	}
	waitState(t, m3, "pdf", StateReady)
}

func TestManagerClose(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	m.Close() // no runtimes started: must be a safe no-op
	if _, err := m.br.Client(t.Context()); err == nil {
		t.Fatal("runtime must be stopped after Close")
	}
}

func TestManagerInstallWaitUnknownPlugin(t *testing.T) {
	m := NewManager(t.TempDir(), t.TempDir(), zerolog.Nop())
	if err := m.InstallWait(t.Context(), "ghost"); err == nil ||
		!strings.Contains(err.Error(), "unknown plugin") {
		t.Fatalf("err = %v", err)
	}
}

func TestManagerReinstallRemoveAllFails(t *testing.T) {
	home := t.TempDir()
	m := NewManager(home, t.TempDir(), zerolog.Nop())
	if err := os.MkdirAll(filepath.Join(home, "pdf", "venv"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(home, 0o700) })
	if err := m.Reinstall(t.Context(), "pdf"); err == nil {
		t.Fatal("reinstall with unremovable env must fail")
	}
}

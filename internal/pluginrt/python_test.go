package pluginrt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"
)

// fakeUvScript impersonates the managed uv binary. Behaviour knobs arrive via
// the environment (FAKE_UV_*), which PythonRuntime.env() forwards because it
// extends os.Environ().
const fakeUvScript = `#!/bin/sh
if [ "$1" = "--version" ]; then
	echo "uv ${FAKE_UV_VERSION:-0.12.5} [fake]"
	exit 0
fi
case "$1" in
python)
	if [ -n "$FAKE_UV_SLEEP" ]; then sleep "$FAKE_UV_SLEEP"; fi
	if [ -n "$FAKE_UV_PYTHON_RC" ]; then echo "python install failed" >&2; exit "$FAKE_UV_PYTHON_RC"; fi
	echo "Installed CPython 3.12.14"
	exit 0
	;;
venv)
	if [ -n "$FAKE_UV_VENV_RC" ]; then echo "venv failed" >&2; exit "$FAKE_UV_VENV_RC"; fi
	mkdir -p "$2/bin"
	if [ "${FAKE_UV_MAKE_PYTHON:-1}" = "1" ]; then
		printf '%s\n' '#!/bin/sh' 'echo "{\"ok\":true,\"stub\":\"venv\"}"' > "$2/bin/python"
		chmod +x "$2/bin/python"
	fi
	exit 0
	;;
pip)
	if [ -n "$FAKE_UV_SYNC_RC" ]; then echo "pip sync failed" >&2; exit "$FAKE_UV_SYNC_RC"; fi
	echo "Audited 12 packages"
	exit 0
	;;
esac
echo "fake uv: unexpected args: $*" >&2
exit 64
`

// fakeVenvPython impersonates the plugin venv's interpreter; its stdout plays
// pdf.py's JSON protocol.
const fakeVenvPython = `#!/bin/sh
if [ -n "$FAKE_PYTHON_ARGS_FILE" ]; then printf '%s\n' "$@" >> "$FAKE_PYTHON_ARGS_FILE"; fi
case "${FAKE_PYTHON_MODE:-ok}" in
ok)      echo '{"ok":true,"pages":3,"text":"hello pdf","selected":2,"truncated":true}' ;;
meta)    echo '{"ok":true,"pages":9,"encrypted":true,"metadata":{"Title":"Doc","Author":"Me"}}' ;;
write)   echo '{"ok":true,"output":"merged.pdf","pages":5,"files":["a.pdf","b.pdf"]}' ;;
errjson) echo '{"ok":false,"error":"boom said pdf.py"}' ;;
invalid) echo 'this is not json' ;;
fail)    echo "pdf.py: fatal" >&2; exit 4 ;;
silent)  exit 5 ;;
big)     head -c 1500000 /dev/zero | tr '\0' 'x' ;;
officeread)  printf '%s\n' '{"ok":true,"file":"notes.docx","format":"docx","truncated":true,"markdown":"# Hello\n\n- alpha"}' ;;
officedocx)   printf '%s\n' '{"ok":true,"output":"out/report.docx","blocks":3,"bytes":4321}' ;;
officexlsx)   printf '%s\n' '{"ok":true,"output":"data.xlsx","sheets":2,"rows":9,"bytes":4321}' ;;
officepptx)   printf '%s\n' '{"ok":true,"output":"deck.pptx","slides":4,"bytes":4321}' ;;
esac
exit 0
`

// writeFakeUV installs the fake uv under the shared runtimes bin dir.
func writeFakeUV(t *testing.T, py *PythonRuntime) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(py.uvBin()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py.uvBin(), []byte(fakeUvScript), 0o755); err != nil {
		t.Fatal(err)
	}
}

// stampPythonEnv provisions a Ready-looking environment for plugin: a fake
// venv interpreter, the ENV_OK stamp and materialized scripts. mutate tweaks
// the stamp to simulate drift.
func stampPythonEnv(t *testing.T, py *PythonRuntime, plugin string, mutate func(*envStamp)) {
	t.Helper()
	b, ok := ByName(plugin)
	if !ok {
		t.Fatalf("plugin %s missing", plugin)
	}
	lock, err := pluginLockBytes(b)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(lock)
	st := envStamp{UV: uvVersion, Python: pythonPin, LockSHA: hex.EncodeToString(sum[:])}
	if mutate != nil {
		mutate(&st)
	}
	if err := os.MkdirAll(filepath.Dir(py.venvPython(plugin)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(py.venvPython(plugin), []byte(fakeVenvPython), 0o755); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(st)
	if err := os.WriteFile(filepath.Join(py.envDir(plugin), "ENV_OK"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := py.materializePlugin(b); err != nil {
		t.Fatal(err)
	}
}

func TestPythonReadyVariants(t *testing.T) {
	newPy := func() *PythonRuntime {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		stampPythonEnv(t, py, "pdf", nil)
		return py
	}
	if !newPy().Ready("pdf") {
		t.Fatal("stamped env must be ready")
	}

	py := newPy()
	stampPythonEnv(t, py, "pdf", func(st *envStamp) { st.UV = "0.0.0" })
	if py.Ready("pdf") {
		t.Fatal("uv drift must invalidate")
	}
	stampPythonEnv(t, py, "pdf", func(st *envStamp) { st.Python = "3.11" })
	if py.Ready("pdf") {
		t.Fatal("python drift must invalidate")
	}
	stampPythonEnv(t, py, "pdf", func(st *envStamp) { st.LockSHA = strings.Repeat("0", 64) })
	if py.Ready("pdf") {
		t.Fatal("lock drift must invalidate")
	}

	// Corrupt stamp JSON.
	py2 := newPy()
	if err := os.WriteFile(filepath.Join(py2.envDir("pdf"), "ENV_OK"), []byte("<<junk>>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if py2.Ready("pdf") {
		t.Fatal("junk stamp must not be ready")
	}
	// Missing stamp.
	py3 := newPy()
	if err := os.Remove(filepath.Join(py3.envDir("pdf"), "ENV_OK")); err != nil {
		t.Fatal(err)
	}
	if py3.Ready("pdf") {
		t.Fatal("missing stamp must not be ready")
	}
	// Interpreter is a directory.
	py4 := newPy()
	if err := os.Remove(py4.venvPython("pdf")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(py4.venvPython("pdf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if py4.Ready("pdf") {
		t.Fatal("dir at venv python must not be ready")
	}
	// Unknown plugin name with an otherwise valid layout.
	py5 := newPy()
	if err := os.Rename(py5.envDir("pdf"), filepath.Join(py5.home, "ghost")); err != nil {
		t.Fatal(err)
	}
	if py5.Ready("ghost") {
		t.Fatal("unknown plugin must not be ready")
	}
}

func TestPythonRunProtocol(t *testing.T) {
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	stampPythonEnv(t, py, "pdf", nil)
	ctx := context.Background()

	// Unknown plugin and undeclared script are rejected before anything runs.
	if _, _, err := py.Run(ctx, "ghost", "scripts/pdf.py", nil, ".", time.Second); err == nil ||
		!strings.Contains(err.Error(), "unknown python plugin") {
		t.Fatalf("unknown plugin: %v", err)
	}
	if _, _, err := py.Run(ctx, "pdf", "scripts/evil.py", nil, ".", time.Second); err == nil ||
		!strings.Contains(err.Error(), "not declared") {
		t.Fatalf("undeclared script: %v", err)
	}

	// Success: stdout relays the JSON protocol, timeout 0 gets the default.
	argsFile := filepath.Join(t.TempDir(), "args")
	t.Setenv("FAKE_PYTHON_ARGS_FILE", argsFile)
	stdout, stderr, err := py.Run(ctx, "pdf", "scripts/pdf.py", []string{"text", "doc.pdf"}, ".", 0)
	if err != nil || stderr != "" || !strings.Contains(stdout, "\"ok\":true") {
		t.Fatalf("run: stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}
	rawArgs, _ := os.ReadFile(argsFile)
	lines := strings.Split(strings.TrimSpace(string(rawArgs)), "\n")
	if len(lines) != 3 || !strings.HasSuffix(lines[0], "scripts/pdf.py") ||
		lines[1] != "text" || lines[2] != "doc.pdf" {
		t.Fatalf("interpreter args = %q", lines)
	}

	// Script failure relays stderr and the exit error.
	t.Setenv("FAKE_PYTHON_MODE", "fail")
	stdout, stderr, err = py.Run(ctx, "pdf", "scripts/pdf.py", nil, ".", time.Second)
	if err == nil || !strings.Contains(stderr, "pdf.py: fatal") {
		t.Fatalf("failing script: stdout=%q stderr=%q err=%v", stdout, stderr, err)
	}

	// Runaway output is capped at the 1 MiB write limit (the truncation
	// marker itself depends on pipe chunking and is asserted directly in
	// TestLimitWriterCapsOutput).
	t.Setenv("FAKE_PYTHON_MODE", "big")
	stdout, _, err = py.Run(ctx, "pdf", "scripts/pdf.py", nil, ".", time.Second)
	if err != nil {
		t.Fatalf("big output run: %v", err)
	}
	if len(stdout) > writeLimit+len("…(truncated)") {
		t.Fatalf("big output len=%d exceeds the cap", len(stdout))
	}

	// A ready env whose scripts dir cannot be written fails materialization.
	t.Setenv("FAKE_PYTHON_MODE", "ok")
	py2 := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	stampPythonEnv(t, py2, "pdf", nil)
	if err := os.RemoveAll(filepath.Join(py2.envDir("pdf"), "scripts")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(py2.envDir("pdf"), "scripts"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := py2.Run(ctx, "pdf", "scripts/pdf.py", nil, ".", time.Second); err == nil {
		t.Fatal("unmaterializable scripts must fail Run")
	}
}

func TestPythonMaterializeErrors(t *testing.T) {
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	// No embedded dir: the script read fails.
	if err := py.materializePlugin(Builtin{Name: "orphan", Scripts: []string{"x.py"}}); err == nil ||
		!strings.Contains(err.Error(), "read orphan/x.py") {
		t.Fatalf("orphan plugin: %v", err)
	}
	// Destination occupied by a directory: the write fails.
	b, ok := ByName("pdf")
	if !ok {
		t.Fatal("pdf plugin missing")
	}
	dest := filepath.Join(py.envDir("pdf"), "scripts", "scripts", "pdf.py")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := py.materializePlugin(b); err == nil {
		t.Fatal("dir at dest must fail")
	}
}

func TestPluginLockBytesErrors(t *testing.T) {
	if _, err := pluginLockBytes(Builtin{Name: "nolock"}); err == nil ||
		!strings.Contains(err.Error(), "declares no requirements lock") {
		t.Fatalf("no lock: %v", err)
	}
	b, ok := ByName("pdf")
	if !ok {
		t.Fatal("pdf plugin missing")
	}
	lock, err := pluginLockBytes(b)
	if err != nil || len(lock) == 0 {
		t.Fatalf("pdf lock: %d bytes, %v", len(lock), err)
	}
	if _, err := readPluginFile(Builtin{Name: "nodir"}, "x"); err == nil {
		t.Fatal("plugin without dir must fail reads")
	}
}

func TestPythonRuntimeHelpers(t *testing.T) {
	runtimes, home := t.TempDir(), t.TempDir()
	py := NewPythonRuntime(runtimes, home, zerolog.Nop())
	if got := py.scriptPath("pdf", "scripts/pdf.py"); got != filepath.Join(home, "pdf", "scripts", "scripts", "pdf.py") {
		t.Fatalf("scriptPath = %q", got)
	}
	if got := py.envDir("pdf"); got != filepath.Join(home, "pdf") {
		t.Fatalf("envDir = %q", got)
	}
	env := py.env()
	joined := "\n" + strings.Join(env, "\n") + "\n"
	for _, want := range []string{
		"UV_PYTHON_INSTALL_DIR=" + filepath.Join(runtimes, "pythons"),
		"UV_CACHE_DIR=" + filepath.Join(runtimes, "cache"),
		"PYTHONIOENCODING=utf-8",
		"PYTHONDONTWRITEBYTECODE=1",
	} {
		if !strings.Contains(joined, "\n"+want+"\n") {
			t.Fatalf("env missing %s", want)
		}
	}
}

func TestPythonInstallWithFakeUV(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		writeFakeUV(t, py)
		var lines []string
		if err := py.Install(context.Background(), mustBuiltin(t, "pdf"), func(line string) {
			lines = append(lines, line)
		}); err != nil {
			t.Fatalf("install: %v", err)
		}
		if !py.Ready("pdf") {
			t.Fatal("env not ready after install")
		}
		if len(lines) == 0 || !strings.Contains(strings.Join(lines, "\n"), "uv 0.12.5 already present") {
			t.Fatalf("progress = %v", lines)
		}
		// Re-run skips venv creation (interpreter already there).
		lines = nil
		if err := py.Install(context.Background(), mustBuiltin(t, "pdf"), func(line string) {
			lines = append(lines, line)
		}); err != nil {
			t.Fatalf("reinstall: %v", err)
		}
		if strings.Contains(strings.Join(lines, "\n"), "creating venv") {
			t.Fatalf("venv recreated: %v", lines)
		}
	})

	t.Run("runtimes-unwritable", func(t *testing.T) {
		dir := t.TempDir()
		blocker := filepath.Join(dir, "blocker")
		if err := os.WriteFile(blocker, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		py := NewPythonRuntime(filepath.Join(blocker, "rt"), t.TempDir(), zerolog.Nop())
		if err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil); err == nil {
			t.Fatal("install under a file must fail")
		}
	})

	t.Run("python-install-fails", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		writeFakeUV(t, py)
		t.Setenv("FAKE_UV_PYTHON_RC", "7")
		err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil)
		if err == nil || !strings.Contains(err.Error(), "uv python install") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("venv-fails", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		writeFakeUV(t, py)
		t.Setenv("FAKE_UV_VENV_RC", "6")
		err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil)
		if err == nil || !strings.Contains(err.Error(), "uv venv") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("pip-sync-fails", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		writeFakeUV(t, py)
		t.Setenv("FAKE_UV_SYNC_RC", "5")
		err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil)
		if err == nil || !strings.Contains(err.Error(), "uv pip sync") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("stale-uv-triggers-download", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		writeFakeUV(t, py)
		t.Setenv("FAKE_UV_VERSION", "9.9.9")
		err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil)
		if err == nil || !strings.Contains(err.Error(), "download uv") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("uv-bin-is-a-dir", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		if err := os.MkdirAll(py.uvBin(), 0o755); err != nil {
			t.Fatal(err)
		}
		err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil)
		if err == nil || !strings.Contains(err.Error(), "download uv") {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("install-without-venv-python", func(t *testing.T) {
		py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
		writeFakeUV(t, py)
		t.Setenv("FAKE_UV_MAKE_PYTHON", "0")
		if err := py.Install(context.Background(), mustBuiltin(t, "pdf"), nil); err != nil {
			t.Fatalf("install: %v", err)
		}
		if py.Ready("pdf") {
			t.Fatal("env without interpreter must not be ready")
		}
	})
}

func mustBuiltin(t *testing.T, name string) Builtin {
	t.Helper()
	b, ok := ByName(name)
	if !ok {
		t.Fatalf("plugin %s missing", name)
	}
	return b
}

func TestTailLines(t *testing.T) {
	if got := tailLines("a\nb\nc\nd\n", 2); got != "c\nd" {
		t.Fatalf("tailLines = %q", got)
	}
	if got := tailLines("a\nb", 5); got != "a\nb" {
		t.Fatalf("tailLines short = %q", got)
	}
	if got := tailLines("", 3); got != "" {
		t.Fatalf("tailLines empty = %q", got)
	}
}

func TestLimitWriterCapsOutput(t *testing.T) {
	var w limitWriter
	if n, err := w.Write([]byte("hello")); err != nil || n != 5 {
		t.Fatalf("small write: %d %v", n, err)
	}
	// A single oversized write is clipped with the marker.
	w = limitWriter{}
	big := strings.Repeat("x", writeLimit+100)
	if n, err := w.Write([]byte(big)); err != nil || n != len(big) {
		t.Fatalf("big write: %d %v", n, err)
	}
	out := w.String()
	if len(out) != writeLimit+len("…(truncated)") {
		t.Fatalf("capped len = %d", len(out))
	}
	// Writes past the cap are swallowed.
	if n, err := w.Write([]byte("more")); err != nil || n != 4 {
		t.Fatalf("post-cap write: %d %v", n, err)
	}
	if w.String() != out {
		t.Fatal("post-cap write must not change the buffer")
	}
}

func TestPythonNotInstalledSentinel(t *testing.T) {
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	_, _, err := py.Run(context.Background(), "pdf", "scripts/pdf.py", nil, ".", time.Second)
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

func TestPythonInstallLateFailures(t *testing.T) {
	ctx := context.Background()

	// A plugin without an embedded lock fails after the shared steps.
	py := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	writeFakeUV(t, py)
	err := py.Install(ctx, Builtin{Name: "orphan", Kind: "python"}, nil)
	if err == nil || !strings.Contains(err.Error(), "declares no requirements lock") {
		t.Fatalf("orphan install: %v", err)
	}

	// The plugin env dir is occupied by a file.
	home := t.TempDir()
	py2 := NewPythonRuntime(t.TempDir(), home, zerolog.Nop())
	writeFakeUV(t, py2)
	if err := os.WriteFile(filepath.Join(home, "pdf"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := py2.Install(ctx, mustBuiltin(t, "pdf"), nil); err == nil {
		t.Fatal("env dir blocked by a file must fail")
	}

	// The lock destination is occupied by a directory.
	home2 := t.TempDir()
	py3 := NewPythonRuntime(t.TempDir(), home2, zerolog.Nop())
	writeFakeUV(t, py3)
	if err := os.MkdirAll(filepath.Join(home2, "pdf", "requirements.lock"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := py3.Install(ctx, mustBuiltin(t, "pdf"), nil); err == nil {
		t.Fatal("lock path blocked by a dir must fail")
	}

	// The scripts dir is occupied by a file.
	home3 := t.TempDir()
	py4 := NewPythonRuntime(t.TempDir(), home3, zerolog.Nop())
	writeFakeUV(t, py4)
	if err := os.MkdirAll(filepath.Join(home3, "pdf"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home3, "pdf", "scripts"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := py4.Install(ctx, mustBuiltin(t, "pdf"), nil); err == nil ||
		!strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("scripts blocked: %v", err)
	}

	// The ENV_OK destination is occupied by a directory.
	home4 := t.TempDir()
	py5 := NewPythonRuntime(t.TempDir(), home4, zerolog.Nop())
	writeFakeUV(t, py5)
	if err := os.MkdirAll(filepath.Join(home4, "pdf", "ENV_OK"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := py5.Install(ctx, mustBuiltin(t, "pdf"), nil); err == nil {
		t.Fatal("ENV_OK blocked by a dir must fail")
	}

	// An unsupported platform fails before any download when uv is missing.
	pinPlatform(t, "plan9", "mips")
	py6 := NewPythonRuntime(t.TempDir(), t.TempDir(), zerolog.Nop())
	err = py6.Install(ctx, mustBuiltin(t, "pdf"), nil)
	if err == nil || !strings.Contains(err.Error(), "unsupported platform plan9-mips for uv") {
		t.Fatalf("plan9 install: %v", err)
	}
}

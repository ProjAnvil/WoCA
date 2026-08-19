package pluginrt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/executor"
)

// pythonPin is the managed-CPython major.minor the venvs are built from. uv's
// frozen per-release metadata resolves it to an exact patch (e.g. 3.12.14),
// so pinning uv pins the interpreter build too.
const pythonPin = "3.12"

// ErrNotInstalled is returned by Run when a plugin's Python environment has
// not been provisioned (or its lock changed and it needs a reinstall).
var ErrNotInstalled = errors.New("python plugin environment is not installed — open Settings → 插件 and click 安装 (install)")

// PythonRuntime follows the pipx / `uv tool` layout: heavy machinery (the uv
// binary, managed CPython interpreters, the hard-link cache) is shared under
// runtimesDir, while every plugin gets its own isolated environment dir
// (<pluginHome>/<plugin>/) holding a venv with only that plugin's locked
// wheels, its materialized embedded scripts, and its ENV_OK stamp. Deleting
// the plugin's directory cleanly uninstalls it; two plugins can never fight
// over dependency versions.
type PythonRuntime struct {
	runtimes string // shared: <home>/runtimes/python (bin/, pythons/, cache/)
	home     string // plugin home root: <home>/<plugin>/{venv,scripts,…}
	log      zerolog.Logger
	exec     executor.Executor
}

// NewPythonRuntime builds a runtime with runtimesDir for the shared uv
// machinery and pluginHome for the per-plugin environments, running commands
// on the local machine.
func NewPythonRuntime(runtimesDir, pluginHome string, log zerolog.Logger) *PythonRuntime {
	return NewPythonRuntimeWithExecutor(runtimesDir, pluginHome, log, nil)
}

// NewPythonRuntimeWithExecutor is NewPythonRuntime with an explicit execution
// backend for uv and the plugin scripts; a nil executor falls back to the
// local machine.
func NewPythonRuntimeWithExecutor(runtimesDir, pluginHome string, log zerolog.Logger, x executor.Executor) *PythonRuntime {
	if x == nil {
		x = executor.NewLocal()
	}
	return &PythonRuntime{runtimes: runtimesDir, home: pluginHome, log: log, exec: x}
}

func (p *PythonRuntime) uvBin() string {
	if goos() == "windows" {
		return filepath.Join(p.runtimes, "bin", "uv.exe")
	}
	return filepath.Join(p.runtimes, "bin", "uv")
}

// envDir is the plugin's isolated environment directory.
func (p *PythonRuntime) envDir(plugin string) string { return filepath.Join(p.home, plugin) }

func (p *PythonRuntime) venvPython(plugin string) string {
	if goos() == "windows" {
		return filepath.Join(p.envDir(plugin), "venv", "Scripts", "python.exe")
	}
	return filepath.Join(p.envDir(plugin), "venv", "bin", "python")
}

// env returns the child-process environment with uv state contained under
// the shared runtimes dir.
func (p *PythonRuntime) env() []string {
	return append(os.Environ(),
		"UV_PYTHON_INSTALL_DIR="+filepath.Join(p.runtimes, "pythons"),
		"UV_CACHE_DIR="+filepath.Join(p.runtimes, "cache"),
		"PYTHONIOENCODING=utf-8",
		"PYTHONDONTWRITEBYTECODE=1",
	)
}

// pluginLockBytes reads one plugin's embedded requirements.lock.
func pluginLockBytes(b Builtin) ([]byte, error) {
	if b.Requirements == "" {
		return nil, fmt.Errorf("plugin %s declares no requirements lock", b.Name)
	}
	return readPluginFile(b, b.Requirements)
}

// envStamp is the per-plugin ENV_OK file: when its recorded lock digest
// matches the embedded lock, the plugin's environment is ready.
type envStamp struct {
	UV      string `json:"uv"`
	Python  string `json:"python"`
	LockSHA string `json:"lock_sha256"`
}

// Ready reports whether the plugin's venv exists and matches its embedded
// dependency lock (a woca upgrade that changes the lock forces a reinstall).
func (p *PythonRuntime) Ready(plugin string) bool {
	if fi, err := os.Stat(p.venvPython(plugin)); err != nil || fi.IsDir() {
		return false
	}
	raw, err := os.ReadFile(filepath.Join(p.envDir(plugin), "ENV_OK"))
	if err != nil {
		return false
	}
	var stamp envStamp
	if json.Unmarshal(raw, &stamp) != nil || stamp.UV != uvVersion || stamp.Python != pythonPin {
		return false
	}
	b, ok := ByName(plugin)
	if !ok || b.Kind != "python" {
		return false
	}
	lock, err := pluginLockBytes(b)
	if err != nil {
		return false
	}
	sum := sha256.Sum256(lock)
	return stamp.LockSHA == hex.EncodeToString(sum[:])
}

// Install provisions the shared uv machinery (idempotent across plugins)
// and the plugin's own environment. Safe to re-run.
// progress receives human-readable milestone lines for the settings UI.
func (p *PythonRuntime) Install(ctx context.Context, b Builtin, progress func(string)) error {
	step := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}
	for _, dir := range []string{
		filepath.Join(p.runtimes, "bin"),
		filepath.Join(p.runtimes, "pythons"),
		filepath.Join(p.runtimes, "cache"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// 1. Shared uv binary (pinned release archive, checksum-verified).
	needUV := true
	if fi, err := os.Stat(p.uvBin()); err == nil && !fi.IsDir() {
		res, err := p.exec.Exec(ctx, executor.ExecRequest{Name: p.uvBin(), Args: []string{"--version"}})
		if err == nil && strings.Contains(string(res.Stdout), uvVersion) {
			needUV = false
		}
	}
	if needUV {
		asset, err := uvAssetNames()
		if err != nil {
			return err
		}
		sum, err := platformChecksum(uvChecksums, "uv")
		if err != nil {
			return err
		}
		step("downloading uv %s (%s)…", uvVersion, asset)
		archive := filepath.Join(p.runtimes, "bin", asset)
		if err := fetchVerified(ctx, uvReleaseBase+"/"+asset, sum, archive); err != nil {
			return fmt.Errorf("download uv: %w", err)
		}
		step("verifying sha256… ok")
		if err := extractArchiveMembers(archive, filepath.Join(p.runtimes, "bin"), map[string]bool{"uv": true, "uvx": true}); err != nil {
			return fmt.Errorf("extract uv: %w", err)
		}
		_ = os.Remove(archive)
		step("uv installed (shared runtimes)")
	} else {
		step("uv %s already present", uvVersion)
	}

	// 2. Shared managed CPython (--no-bin keeps ~/.local untouched).
	step("ensuring managed CPython %s…", pythonPin)
	if out, err := p.runUV(ctx, "python", "install", pythonPin, "--no-bin", "--no-registry"); err != nil {
		return fmt.Errorf("uv python install: %w\n%s", err, tailLines(out, 5))
	}

	// 3. The plugin's own venv (recreate only when missing).
	if _, err := os.Stat(p.venvPython(b.Name)); err != nil {
		step("creating venv for %s…", b.Name)
		if out, err := p.runUV(ctx, "venv", filepath.Join(p.envDir(b.Name), "venv"), "--python", pythonPin); err != nil {
			return fmt.Errorf("uv venv: %w\n%s", err, tailLines(out, 5))
		}
	}

	// 4. Materialize the plugin's lock + scripts, then install wheels only.
	lock, err := pluginLockBytes(b)
	if err != nil {
		return err
	}
	lockPath := filepath.Join(p.envDir(b.Name), "requirements.lock")
	if err := os.MkdirAll(p.envDir(b.Name), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(lockPath, lock, 0o644); err != nil {
		return err
	}
	step("installing %s locked wheels (--only-binary)…", b.Name)
	// pip sync (not install): the lock is the exact desired state, so a
	// reinstall after a lock change also removes stale packages. Note sync
	// takes the requirements file positionally (no -r).
	if out, err := p.runUV(ctx, "pip", "sync", "--python", p.venvPython(b.Name), lockPath, "--only-binary=:all:"); err != nil {
		return fmt.Errorf("uv pip sync: %w\n%s", err, tailLines(out, 8))
	}
	if err := p.materializePlugin(b); err != nil {
		return err
	}
	sum := sha256.Sum256(lock)
	stamp, _ := json.Marshal(envStamp{UV: uvVersion, Python: pythonPin, LockSHA: hex.EncodeToString(sum[:])})
	if err := os.WriteFile(filepath.Join(p.envDir(b.Name), "ENV_OK"), stamp, 0o644); err != nil {
		return err
	}
	step("%s environment ready", b.Name)
	return nil
}

// runUV executes the managed uv with the contained environment. A non-zero
// exit is returned as an error, matching the previous cmd.Run semantics uv
// callers rely on.
func (p *PythonRuntime) runUV(ctx context.Context, args ...string) (string, error) {
	res, err := p.exec.Exec(ctx, executor.ExecRequest{Name: p.uvBin(), Args: args, Env: p.env()})
	// Combined output preserves the previous single-buffer behavior (uv
	// interleaves progress into both streams).
	out := string(res.Stdout) + string(res.Stderr)
	if err != nil {
		return out, err
	}
	if res.ExitCode != 0 {
		return out, &executor.ExitError{Code: res.ExitCode}
	}
	return out, nil
}

// materializePlugin writes the plugin's embedded scripts under
// <envDir>/scripts/, rewriting a file only when its content differs (so an
// upgraded woca transparently upgrades scripts, and unchanged files keep
// mtimes).
func (p *PythonRuntime) materializePlugin(b Builtin) error {
	for _, script := range b.Scripts {
		raw, err := readPluginFile(b, script)
		if err != nil {
			return fmt.Errorf("read %s/%s: %w", b.Name, script, err)
		}
		dest := filepath.Join(p.envDir(b.Name), "scripts", filepath.FromSlash(script))
		if existing, err := os.ReadFile(dest); err == nil && bytes.Equal(existing, raw) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dest, raw, 0o644); err != nil {
			return fmt.Errorf("materialize %s/%s: %w", b.Name, script, err)
		}
	}
	return nil
}

// scriptPath returns the on-disk path of one materialized plugin script.
func (p *PythonRuntime) scriptPath(plugin, script string) string {
	return filepath.Join(p.envDir(plugin), "scripts", filepath.FromSlash(script))
}

// Run executes an embedded plugin script with the plugin's venv interpreter —
// never `uv run`, and never a script path that did not come from the embed
// FS. stdout carries the script's JSON protocol; stderr is diagnostics.
func (p *PythonRuntime) Run(ctx context.Context, plugin, script string, args []string, dir string, timeout time.Duration) (stdout, stderr string, err error) {
	b, ok := ByName(plugin)
	if !ok || b.Kind != "python" {
		return "", "", fmt.Errorf("unknown python plugin %q", plugin)
	}
	allowed := false
	for _, s := range b.Scripts {
		if s == script {
			allowed = true
			break
		}
	}
	if !allowed {
		return "", "", fmt.Errorf("script %q is not declared by plugin %q", script, plugin)
	}
	if !p.Ready(plugin) {
		return "", "", ErrNotInstalled
	}
	if err := p.materializePlugin(b); err != nil {
		return "", "", err
	}
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var outBuf, errBuf limitWriter
	// cctx bounds the script run: canceling it kills the process (Start
	// ties the child to the context like Exec does).
	proc, err := p.exec.Start(cctx,
		executor.ExecRequest{
			Name: p.venvPython(plugin),
			Args: append([]string{p.scriptPath(plugin, script)}, args...),
			Env:  p.env(),
			Dir:  dir,
		},
		executor.StartOptions{Stdout: &outBuf, Stderr: &errBuf})
	if err != nil {
		return "", "", err
	}
	runErr := proc.Wait()
	return outBuf.String(), errBuf.String(), runErr
}

// limitWriter caps captured process output (1 MiB) so a runaway script
// cannot balloon agent memory.
type limitWriter struct {
	buf bytes.Buffer
}

const writeLimit = 1 << 20

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.buf.Len() >= writeLimit {
		return len(p), nil // swallow beyond the cap
	}
	room := writeLimit - w.buf.Len()
	if len(p) > room {
		w.buf.Write(p[:room])
		w.buf.WriteString("…(truncated)")
		return len(p), nil
	}
	return w.buf.Write(p)
}

func (w *limitWriter) String() string { return w.buf.String() }

// tailLines returns the last n lines of s (for error surfaces).
func tailLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

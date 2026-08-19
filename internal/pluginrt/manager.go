package pluginrt

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/rs/zerolog"

	coretools "github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/platform/executor"
)

// InstallState is a plugin runtime's provisioning state.
type InstallState string

const (
	StateNotInstalled InstallState = "not_installed"
	StateInstalling   InstallState = "installing"
	StateReady        InstallState = "ready"
	StateError        InstallState = "error"
)

// Status is one plugin's row in GET /api/plugins.
type Status struct {
	Name        string       `json:"name"`
	Kind        string       `json:"kind"`
	Version     string       `json:"version,omitempty"`
	Description string       `json:"description,omitempty"`
	Tools       []string     `json:"tools"`
	Runtime     string       `json:"runtime,omitempty"`
	Enabled     bool         `json:"enabled"`
	State       InstallState `json:"state"`
	Detail      string       `json:"detail,omitempty"`
	Log         []string     `json:"log,omitempty"`
}

// installRun tracks one background install.
type installRun struct {
	state  InstallState
	detail string
	log    []string
	done   chan struct{}
}

const installLogCap = 200

// Manager owns the built-in plugin runtimes and per-plugin environments
// under one home (the plugin root, ~/.woca/plugins):
//
//	<home>/runtimes/python/{bin,pythons,cache}  shared uv machinery
//	<home>/runtimes/pinchtab/…                  browser runtime
//	<home>/<plugin>/{venv,scripts,…}            per-plugin python envs
//
// The same root hosts marketplace skills/agents plugins (internal/plugins);
// its loader skips manifest-less directories such as these.
type Manager struct {
	home    string
	workdir string
	log     zerolog.Logger

	mu   sync.Mutex
	runs map[string]*installRun

	py *PythonRuntime
	br *BrowserRuntime
}

// NewManager builds a Manager (home is typically ~/.woca/plugins; workdir is
// the agent's working directory for script execution) running all plugin
// processes on the local machine.
func NewManager(home, workdir string, log zerolog.Logger) *Manager {
	return NewManagerWithExecutor(home, workdir, log, nil)
}

// NewManagerWithExecutor is NewManager with an explicit execution backend for
// every plugin process (pinchtab server, uv, venv scripts); a nil executor
// falls back to the local machine.
func NewManagerWithExecutor(home, workdir string, log zerolog.Logger, x executor.Executor) *Manager {
	return &Manager{
		home:    home,
		workdir: workdir,
		log:     log,
		runs:    map[string]*installRun{},
		py:      NewPythonRuntimeWithExecutor(filepath.Join(home, "runtimes", "python"), home, log, x),
		br:      NewBrowserRuntimeWithExecutor(filepath.Join(home, "runtimes", "pinchtab"), log, x),
	}
}

// ready reports whether the plugin's runtime is provisioned.
func (m *Manager) ready(b Builtin) (bool, string) {
	switch b.Runtime {
	case "python":
		if m.py.Ready(b.Name) {
			return true, ""
		}
		return false, "Python 环境未安装（需要下载 uv 与 CPython 并安装依赖 wheel）"
	case "pinchtab":
		if m.br.Installed() {
			return true, ""
		}
		return false, "pinchtab 未安装（需要下载固定版本二进制）"
	default:
		return true, ""
	}
}

// install provisions the plugin's runtime synchronously.
func (m *Manager) install(ctx context.Context, b Builtin, progress func(string)) error {
	switch b.Runtime {
	case "python":
		return m.py.Install(ctx, b, progress)
	case "pinchtab":
		return m.br.Install(ctx, progress)
	default:
		return fmt.Errorf("plugin %q has no installable runtime", b.Name)
	}
}

// Statuses snapshots every builtin with enabled flags and runtime state.
func (m *Manager) Statuses(enabled []string) []Status {
	enabledSet := map[string]bool{}
	for _, name := range enabled {
		enabledSet[name] = true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Status{}
	for _, b := range Builtins() {
		st := Status{
			Name: b.Name, Kind: b.Kind, Version: b.Version, Description: b.Description,
			Tools: b.Tools, Runtime: b.Runtime, Enabled: enabledSet[b.Name],
			State: StateNotInstalled,
		}
		if st.Tools == nil {
			st.Tools = []string{}
		}
		if run, ok := m.runs[b.Name]; ok {
			// The latest run's outcome (including its log tail) stays
			// visible until the next install overwrites it.
			st.State = run.state
			st.Detail = run.detail
			if run.state == StateReady {
				st.Detail = m.readyDetail(b)
			}
			st.Log = append([]string(nil), run.log...)
		} else if ok, detail := m.ready(b); ok {
			st.State = StateReady
			st.Detail = m.readyDetail(b)
		} else {
			st.Detail = detail
		}
		out = append(out, st)
	}
	return out
}

// readyDetail describes the installed runtime versions for the ready state.
func (m *Manager) readyDetail(b Builtin) string {
	switch b.Runtime {
	case "python":
		raw, err := os.ReadFile(filepath.Join(m.py.envDir(b.Name), "ENV_OK"))
		if err != nil {
			return ""
		}
		var stamp struct {
			UV      string `json:"uv"`
			Python  string `json:"python"`
			LockSHA string `json:"lock_sha256"`
		}
		if json.Unmarshal(raw, &stamp) != nil {
			return ""
		}
		short := stamp.LockSHA
		if len(short) > 7 {
			short = short[:7]
		}
		return fmt.Sprintf("uv %s · CPython %s · lock %s", stamp.UV, stamp.Python, short)
	case "pinchtab":
		return "pinchtab " + pinchtabVer
	default:
		return ""
	}
}

// Install kicks off a background install for one plugin. It returns an error
// when the plugin is unknown or an install is already in flight; progress is
// visible through Statuses.
func (m *Manager) Install(ctx context.Context, name string) error {
	b, ok := ByName(name)
	if !ok {
		return fmt.Errorf("unknown plugin %q", name)
	}
	if b.Runtime == "" {
		return fmt.Errorf("plugin %q has nothing to install", name)
	}
	m.mu.Lock()
	if run, ok := m.runs[name]; ok && run.state == StateInstalling {
		m.mu.Unlock()
		return fmt.Errorf("plugin %q is already installing", name)
	}
	run := &installRun{state: StateInstalling, done: make(chan struct{})}
	m.runs[name] = run
	m.mu.Unlock()

	// Installs outlive request contexts (they are downloads); cap them at
	// 15 minutes instead.
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Minute)
	progress := func(line string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(run.log) >= installLogCap {
			run.log = append(run.log[:0], run.log[len(run.log)-installLogCap/2:]...)
		}
		run.log = append(run.log, line)
		m.log.Info().Str("plugin", name).Msg(line)
	}
	go func() {
		defer cancel()
		defer close(run.done)
		err := m.install(ictx, b, progress)
		m.mu.Lock()
		defer m.mu.Unlock()
		if err != nil {
			run.state = StateError
			run.detail = err.Error()
			m.log.Error().Str("plugin", name).Err(err).Msg("plugin install failed")
			return
		}
		ready, detail := m.ready(b)
		if !ready {
			run.state = StateError
			run.detail = "install finished but runtime still not ready: " + detail
			return
		}
		run.state = StateReady
		run.detail = ""
	}()
	return nil
}

// InstallWait is the synchronous variant used by tests.
func (m *Manager) InstallWait(ctx context.Context, name string) error {
	if err := m.Install(ctx, name); err != nil {
		return err
	}
	m.mu.Lock()
	run := m.runs[name]
	m.mu.Unlock()
	if run == nil {
		return nil
	}
	select {
	case <-run.done:
		if run.state == StateError {
			return fmt.Errorf("%s", run.detail)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Reinstall upgrades or repairs a plugin's runtime: the python env (or the
// pinchtab binary) is deleted and re-provisioned from the currently pinned
// downloads — shared runtimes (uv, CPython, cache) are kept.
func (m *Manager) Reinstall(ctx context.Context, name string) error {
	b, ok := ByName(name)
	if !ok {
		return fmt.Errorf("unknown plugin %q", name)
	}
	switch b.Runtime {
	case "python":
		if err := os.RemoveAll(m.py.envDir(name)); err != nil {
			return err
		}
	case "pinchtab":
		if err := os.Remove(m.br.binaryPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
	default:
		return fmt.Errorf("plugin %q has nothing to reinstall", name)
	}
	return m.Install(ctx, name)
}

// Tools builds the agent tools contributed by the enabled plugins. Unknown
// names are skipped with a log line (stale settings rows must not break the
// harness).
func (m *Manager) Tools(enabled []string) []coretools.Tool {
	var out []coretools.Tool
	for _, name := range enabled {
		b, ok := ByName(name)
		if !ok {
			m.log.Warn().Str("plugin", name).Msg("unknown plugin in builtin-plugins setting; skipped")
			continue
		}
		switch b.Runtime {
		case "pinchtab":
			out = append(out, browserTools(m.br)...)
		case "python":
			out = append(out, pythonTools(b, m.py, m.workdir)...)
		default:
			m.log.Warn().Str("plugin", name).Str("runtime", b.Runtime).Msg("plugin runtime has no tool implementation")
		}
	}
	return out
}

// Close releases runtime resources (kills a spawned pinchtab server).
func (m *Manager) Close() {
	if m.br != nil {
		m.br.Stop()
	}
}

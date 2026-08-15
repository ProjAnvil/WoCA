// Package webd manages the woca web GUI as a background daemon: start spawns
// a detached `woca serve` process (logging to ~/.woca/web.log), stop and
// restart manage it via the pidfile (~/.woca/web.json, recording pid + addr),
// and status reports whether it is alive. Process spawning is injectable for
// tests.
package webd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/projanvil/woca/internal/webgui"
)

// Record is the pidfile payload.
type Record struct {
	PID     int    `json:"pid"`
	Addr    string `json:"addr"`
	Started string `json:"started,omitempty"`
}

// Manager starts and stops the background web GUI.
type Manager struct {
	// DataDir holds web.json and web.log (default ~/.woca).
	DataDir string
	// Spawn launches the detached server process; argv is the full command
	// (default: the current executable with the given args).
	Spawn func(argv []string, logFile string) (pid int, err error)
	// Kill terminates a pid (default SIGTERM + grace).
	Kill func(pid int) error
	// Alive checks a pid (default Signal 0).
	Alive func(pid int) bool
}

// New builds a Manager rooted at the woca data directory.
func New() *Manager {
	home, err := os.UserHomeDir()
	dir := ""
	if err == nil {
		dir = filepathJoin(home, ".woca")
	} else {
		dir = os.TempDir()
	}
	return &Manager{
		DataDir: dir,
		Spawn:   defaultSpawn,
		Kill:    defaultKill,
		Alive:   defaultAlive,
	}
}

func filepathJoin(parts ...string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += string(os.PathSeparator) + p
	}
	return out
}

func (m *Manager) recordPath() string { return filepathJoin(m.DataDir, "web.json") }

// LogPath returns the daemon's log file location (diagnostics/messages).
func (m *Manager) LogPath() string { return m.logPath() }
func (m *Manager) logPath() string { return filepathJoin(m.DataDir, "web.log") }

// Status reports the running record (nil when not running).
func (m *Manager) Status() *Record {
	rec, err := m.read()
	if err != nil {
		return nil
	}
	if !m.Alive(rec.PID) {
		return nil
	}
	recCopy := rec
	return &recCopy
}

func (m *Manager) read() (Record, error) {
	data, err := os.ReadFile(m.recordPath())
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func (m *Manager) write(rec Record) error {
	if err := os.MkdirAll(m.DataDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return os.WriteFile(m.recordPath(), data, 0o644)
}

// Start launches the web GUI in the background. It returns the new record or
// the existing one when already running.
func (m *Manager) Start(addr string) (Record, bool, error) {
	if rec := m.Status(); rec != nil {
		return *rec, false, nil
	}
	if addr == "" {
		addr = webgui.DefaultAddr
	}
	if err := os.MkdirAll(m.DataDir, 0o755); err != nil {
		return Record{}, false, err
	}
	argv := []string{os.Args[0], "serve", "--addr", addr, "--no-browser"}
	pid, err := m.Spawn(argv, m.logPath())
	if err != nil {
		return Record{}, false, err
	}
	rec := Record{PID: pid, Addr: addr, Started: time.Now().Format(time.RFC3339)}
	if err := m.write(rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// Stop terminates the background web GUI (no error when not running).
func (m *Manager) Stop() (stopped bool, err error) {
	rec, rerr := m.read()
	if rerr != nil {
		return false, nil
	}
	if m.Alive(rec.PID) {
		if err := m.Kill(rec.PID); err != nil {
			return false, fmt.Errorf("webd: stop pid %d: %w", rec.PID, err)
		}
		stopped = true
	}
	_ = os.Remove(m.recordPath())
	return stopped, nil
}

// Restart stops (if running) and starts again.
func (m *Manager) Restart(addr string) (Record, error) {
	if _, err := m.Stop(); err != nil {
		return Record{}, err
	}
	rec, _, err := m.Start(addr)
	return rec, err
}

// defaultSpawn launches argv detached in its own process group, appending
// output to logFile.
func defaultSpawn(argv []string, logFile string) (int, error) {
	f, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.Stdin = nil
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func defaultKill(pid int) error {
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		return err
	}
	// Bounded grace: wait for exit, escalating to SIGKILL after 5s.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !defaultAlive(pid) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return syscall.Kill(pid, syscall.SIGKILL)
}

func defaultAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

// ParseAddr normalizes a port-only value to a loopback address.
func ParseAddr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return webgui.DefaultAddr
	}
	if _, err := strconv.Atoi(v); err == nil {
		return "127.0.0.1:" + v
	}
	return v
}

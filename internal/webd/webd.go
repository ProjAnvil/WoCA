// Package webd manages the woca web GUI as a background daemon: start spawns
// a detached `woca serve` process (logging to ~/.woca/web.log), stop and
// restart manage it via the pidfile (~/.woca/web.json, recording pid + addr),
// and status reports whether it is alive. Process spawning is injectable for
// tests.
package webd

import (
	"encoding/json"
	"fmt"
	"net/http"
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
	// WaitReady blocks until the spawned daemon answers /api/health (or has
	// exited, which is an error — the "bind: address already in use" child
	// dies within milliseconds). New wires the default; nil skips the check
	// (tests with fake daemons).
	WaitReady func(addr string, alive func() bool) error
	// ReadyTimeout bounds WaitReady's polling (default 5s).
	ReadyTimeout time.Duration
	// PortOwner reports the pid listening on addr (default lsof). ok=false
	// when nothing listens or the tool is unavailable. Used to sweep orphan
	// daemons left behind by a stale pidfile.
	PortOwner func(addr string) (pid int, ok bool)
	// CommName reports a process's executable name (default ps), used to
	// validate that a port holder is really a woca process before killing.
	CommName func(pid int) string
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
	m := &Manager{
		DataDir:      dir,
		Spawn:        defaultSpawn,
		Kill:         defaultKill,
		Alive:        defaultAlive,
		PortOwner:    defaultPortOwner,
		CommName:     defaultCommName,
		ReadyTimeout: 5 * time.Second,
	}
	m.WaitReady = func(addr string, alive func() bool) error {
		return m.defaultWaitReady(addr, alive)
	}
	return m
}

func filepathJoin(parts ...string) string {
	var out strings.Builder
	out.WriteString(parts[0])
	for _, p := range parts[1:] {
		out.WriteString(string(os.PathSeparator) + p)
	}
	return out.String()
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
//
// Two guards keep restart honest: a stale pidfile whose port is still held
// by an orphan woca daemon gets swept first (foreign holders are reported,
// never killed), and the spawned daemon must answer /api/health before
// success is reported — a child that dies on "address already in use"
// fails loudly instead of leaving a dead pidfile behind.
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
	if err := m.sweepOrphan(addr); err != nil {
		return Record{}, false, err
	}
	argv := []string{os.Args[0], "serve", "--addr", addr, "--no-browser"}
	pid, err := m.Spawn(argv, m.logPath())
	if err != nil {
		return Record{}, false, err
	}
	if m.WaitReady != nil {
		if err := m.WaitReady(addr, func() bool { return m.Alive(pid) }); err != nil {
			// Clean up so the failure leaves no misleading pidfile.
			if m.Alive(pid) {
				_ = m.Kill(pid)
			}
			_ = os.Remove(m.recordPath())
			return Record{}, false, err
		}
	}
	rec := Record{PID: pid, Addr: addr, Started: time.Now().Format(time.RFC3339)}
	if err := m.write(rec); err != nil {
		return Record{}, false, err
	}
	return rec, true, nil
}

// sweepOrphan retires a leftover woca daemon holding the port despite a
// stale (or missing) pidfile — the state that made `web restart` silently
// reconnect to the old binary. A holder that is not a woca process is
// reported as an error instead of being killed.
func (m *Manager) sweepOrphan(addr string) error {
	if m.PortOwner == nil {
		return nil
	}
	pid, ok := m.PortOwner(addr)
	if !ok || pid == 0 {
		return nil
	}
	if m.Status() != nil && m.readPID() == pid {
		return nil // the recorded daemon; Stop handles it
	}
	name := "?"
	if m.CommName != nil {
		name = m.CommName(pid)
	}
	if !strings.Contains(strings.ToLower(name), "woca") {
		return fmt.Errorf("webd: %s is held by pid %d (%s), not a woca daemon; free the port or pass --addr", addr, pid, name)
	}
	if err := m.Kill(pid); err != nil {
		return fmt.Errorf("webd: retire orphan daemon pid %d: %w", pid, err)
	}
	return nil
}

// readPID returns the recorded pid (0 when unreadable).
func (m *Manager) readPID() int {
	rec, err := m.read()
	if err != nil {
		return 0
	}
	return rec.PID
}

// defaultWaitReady polls the daemon's health endpoint until it answers,
// failing fast when the process exits (bind failures die within
// milliseconds) and bounding the wait at ReadyTimeout.
func (m *Manager) defaultWaitReady(addr string, alive func() bool) error {
	deadline := time.Now().Add(m.ReadyTimeout)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	url := "http://" + addr + "/api/health"
	for {
		if !alive() {
			return fmt.Errorf("webd: daemon exited before serving (see %s; commonly the port is already in use)", m.logPath())
		}
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("webd: daemon not ready on %s within %s (see %s)", addr, m.ReadyTimeout, m.logPath())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// defaultPortOwner resolves the pid listening on addr via lsof (absent tool
// or free port → ok=false).
func defaultPortOwner(addr string) (int, bool) {
	_, port, ok := strings.Cut(addr, ":")
	if !ok {
		return 0, false
	}
	out, err := exec.Command("lsof", "-t", "-iTCP:"+port, "-sTCP:LISTEN").Output()
	if err != nil || len(out) == 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
	if err != nil || pid == 0 {
		return 0, false
	}
	return pid, true
}

// defaultCommName reports a process's executable name via ps.
func defaultCommName(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
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
	defer func() { _ = f.Close() }()
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

// Package bridgeproc manages the embedded woca-bridge subprocess (product
// design §5.1 native 项): install-on-first-run, spawn on a random loopback
// port, discover the bound URL through the WOCA_READY_FILE handshake, and
// keep it alive for the app's lifetime. The loopback bind keeps the
// zero-login posture — no key, no login page.
package bridgeproc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Manager owns the embedded bridge process. Command builds the process
// for a ready-file path — production points at the extracted
// woca-bridge binary; tests inject a fake.
type Manager struct {
	// Command builds the serve process. The returned Cmd's Env must carry
	// WOCA_READY_FILE=<readyFile> (the default command does).
	Command func(readyFile string) *exec.Cmd
	// OnExit observes the process ending (crash reporting).
	OnExit func(err error)

	mu        sync.Mutex
	cmd       *exec.Cmd
	baseURL   string
	done      chan struct{}
	extraArgs atomic.Value // []string, read inside Command while Start holds mu
}

// ErrNotRunning is returned by BaseURL when no bridge is up.
var ErrNotRunning = errors.New("bridgeproc: bridge not running")

// New builds a manager running the given woca-bridge binary. The extra
// args ride every spawn (the enrollment flow swaps them via RestartWith —
// e.g. "--auth apikey" once the bridge joins a private cloud).
func New(binary string, extraArgs ...string) *Manager {
	m := &Manager{}
	m.extraArgs.Store(append([]string(nil), extraArgs...))
	m.Command = func(readyFile string) *exec.Cmd {
		args := append([]string{"serve", "--addr", "127.0.0.1:0", "--no-browser"}, m.currentArgs()...)
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "WOCA_READY_FILE="+readyFile, "WOCA_NO_BROWSER=1")
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		return cmd
	}
	return m
}

// currentArgs snapshots the extra args (atomic: Command runs while Start
// holds the process mutex, so this must not take it).
func (m *Manager) currentArgs() []string {
	args, _ := m.extraArgs.Load().([]string)
	return append([]string(nil), args...)
}

// RestartWith stops the bridge and spawns a fresh process with the given
// extra args, returning the new base URL once healthy. The port is
// randomized per spawn — callers must re-publish the URL they receive.
func (m *Manager) RestartWith(ctx context.Context, extraArgs ...string) (string, error) {
	m.extraArgs.Store(append([]string(nil), extraArgs...))
	if err := m.Stop(); err != nil {
		return "", fmt.Errorf("bridgeproc: restart: %w", err)
	}
	return m.Start(ctx)
}

// Start spawns the bridge and returns its base URL once healthy.
func (m *Manager) Start(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.cmd != nil && m.cmd.Process != nil {
		url := m.baseURL
		m.mu.Unlock()
		if url != "" {
			return url, nil
		}
		return "", fmt.Errorf("bridgeproc: bridge already starting")
	}
	readyFile, err := os.CreateTemp("", "woca-bridge-ready-*")
	if err != nil {
		return "", err
	}
	_ = readyFile.Close()
	_ = os.Remove(readyFile.Name())
	cmd := m.Command(readyFile.Name())
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("bridgeproc: start: %w", err)
	}
	m.cmd = cmd
	m.done = make(chan struct{})
	m.mu.Unlock()

	go func() {
		err := cmd.Wait()
		m.mu.Lock()
		m.cmd = nil
		m.baseURL = ""
		m.mu.Unlock()
		close(m.done)
		if m.OnExit != nil {
			m.OnExit(err)
		}
	}()

	baseURL, err := waitReady(ctx, readyFile.Name(), 30*time.Second)
	if err != nil {
		_ = m.Stop()
		return "", err
	}
	if err := waitHealthy(ctx, baseURL, 20*time.Second); err != nil {
		_ = m.Stop()
		return "", err
	}
	m.mu.Lock()
	m.baseURL = baseURL
	m.mu.Unlock()
	return baseURL, nil
}

// BaseURL returns the running bridge's URL.
func (m *Manager) BaseURL() (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.baseURL == "" {
		return "", ErrNotRunning
	}
	return m.baseURL, nil
}

// Running reports whether the bridge process is up.
func (m *Manager) Running() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cmd != nil
}

// Stop terminates the bridge (graceful, then kill).
func (m *Manager) Stop() error {
	m.mu.Lock()
	cmd := m.cmd
	done := m.done
	m.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = cmd.Process.Signal(os.Interrupt)
	select {
	case <-done:
		return nil
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		return nil
	}
}

// waitReady polls the ready file for the bound URL.
func waitReady(ctx context.Context, path string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		if data, err := os.ReadFile(path); err == nil && len(data) > 0 {
			return string(data), nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("bridgeproc: bridge did not become ready within %s", timeout)
}

// waitHealthy polls /api/health until the server answers.
func waitHealthy(ctx context.Context, baseURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		resp, err := client.Get(baseURL + "/api/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("bridgeproc: health check failed within %s", timeout)
}

// Install extracts the bundled bridge binary to the native bin directory
// when missing or version-drifted (打包期绑定版本；启动校验自动覆盖更新).
func Install(source, version string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".woca", "native", "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stamp := filepath.Join(dir, "woca-bridge.version")
	target := filepath.Join(dir, "woca-bridge")
	if cur, _ := os.ReadFile(stamp); string(cur) == version {
		if _, err := os.Stat(target); err == nil {
			return target, nil
		}
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("bridgeproc: bundled binary: %w", err)
	}
	if err := writeFileAtomic(target, data, 0o755); err != nil {
		return "", err
	}
	if err := writeFileAtomic(stamp, []byte(version), 0o644); err != nil {
		return "", err
	}
	return target, nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

package pluginrt

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/platform/executor"
)

// ErrBrowserNotInstalled is returned when the pinchtab binary is missing.
var ErrBrowserNotInstalled = errors.New("browser plugin runtime is not installed — open Settings → 插件 and click 安装 (install)")

// ErrBrowserStopped is returned when the runtime was stopped and a client is
// requested afterwards (the runtime does not resurrect after Stop).
var ErrBrowserStopped = errors.New("browser runtime is stopped")

// BrowserRuntime downloads and hosts the pinchtab server (CDP-driven Chrome)
// under <root>: the versioned binary, an isolated config.json (random bearer
// token + chosen localhost port, persisted so a surviving server instance is
// reused across woca restarts) and the server log.
type BrowserRuntime struct {
	root string
	log  zerolog.Logger
	exec executor.Executor

	mu      sync.Mutex
	proc    executor.Process // nil when we reused an already-running server
	client  *pinchClient
	stopped bool // Stop was called; a spawn finishing after it is discarded
	// startMu serializes the slow start path (health probe + spawn) so
	// concurrent tool calls block each other here instead of on mu, which
	// must stay quick for fast-path reads and Stop.
	startMu sync.Mutex
}

// NewBrowserRuntime builds a runtime rooted at dir (~/.woca/plugins/runtimes/pinchtab)
// running the server on the local machine.
func NewBrowserRuntime(dir string, log zerolog.Logger) *BrowserRuntime {
	return NewBrowserRuntimeWithExecutor(dir, log, nil)
}

// NewBrowserRuntimeWithExecutor is NewBrowserRuntime with an explicit
// execution backend for the server process; a nil executor falls back to the
// local machine.
func NewBrowserRuntimeWithExecutor(dir string, log zerolog.Logger, x executor.Executor) *BrowserRuntime {
	if x == nil {
		x = executor.NewLocal()
	}
	return &BrowserRuntime{root: dir, log: log, exec: x}
}

func (b *BrowserRuntime) binaryPath() string {
	name := "pinchtab"
	if goos() == "windows" {
		name += ".exe"
	}
	return filepath.Join(b.root, pinchtabVer, name)
}

// Installed reports whether the pinned pinchtab binary is on disk.
func (b *BrowserRuntime) Installed() bool {
	fi, err := os.Stat(b.binaryPath())
	return err == nil && !fi.IsDir()
}

// Install downloads the pinned pinchtab release binary (checksum-verified).
func (b *BrowserRuntime) Install(ctx context.Context, progress func(string)) error {
	step := func(format string, args ...any) {
		if progress != nil {
			progress(fmt.Sprintf(format, args...))
		}
	}
	if b.Installed() {
		step("pinchtab %s already present", pinchtabVer)
		return nil
	}
	asset, err := pinchtabAssetName()
	if err != nil {
		return err
	}
	sum, err := platformChecksum(pinchtabChecksums, "pinchtab")
	if err != nil {
		return err
	}
	step("downloading pinchtab %s (%s ~28MB)…", pinchtabVer, asset)
	if err := fetchVerified(ctx, ptReleaseBase+"/"+asset, sum, b.binaryPath()); err != nil {
		return fmt.Errorf("download pinchtab: %w", err)
	}
	step("verifying sha256… ok")
	return nil
}

// pinchConfig is the subset of pinchtab's config woca manages. The isolated
// file (pointed at via PINCHTAB_CONFIG) keeps woca's server separate from any
// user-managed ~/.pinchtab setup.
type pinchConfig struct {
	Server struct {
		Port  string `json:"port"`
		Bind  string `json:"bind"`
		Token string `json:"token"`
	} `json:"server"`
	InstanceDefaults struct {
		Mode string `json:"mode"`
	} `json:"instanceDefaults"`
}

func (b *BrowserRuntime) configPath() string { return filepath.Join(b.root, "config.json") }
func (b *BrowserRuntime) serverLogPath() string {
	return filepath.Join(b.root, "server.log")
}

// loadConfig reads (creating or completing on first use) the isolated
// pinchtab config: random 32-byte token, first free port from 9867.
func (b *BrowserRuntime) loadConfig() (pinchConfig, string, error) {
	var cfg pinchConfig
	raw, err := os.ReadFile(b.configPath())
	if err == nil {
		_ = json.Unmarshal(raw, &cfg)
	}
	changed := false
	if cfg.Server.Token == "" {
		buf := make([]byte, 32)
		if _, err := rand.Read(buf); err != nil {
			return cfg, "", err
		}
		cfg.Server.Token = hex.EncodeToString(buf)
		changed = true
	}
	if cfg.Server.Port == "" {
		cfg.Server.Port = pickPort()
		changed = true
	}
	if cfg.Server.Bind == "" {
		cfg.Server.Bind = "127.0.0.1"
		changed = true
	}
	if cfg.InstanceDefaults.Mode == "" {
		cfg.InstanceDefaults.Mode = "headless"
		changed = true
	}
	if changed {
		if err := os.MkdirAll(b.root, 0o755); err != nil {
			return cfg, "", err
		}
		out, err := json.MarshalIndent(cfg, "", "  ")
		if err != nil {
			return cfg, "", err
		}
		if err := os.WriteFile(b.configPath(), append(out, '\n'), 0o600); err != nil {
			return cfg, "", err
		}
	}
	return cfg, "http://" + net.JoinHostPort(cfg.Server.Bind, cfg.Server.Port), nil
}

// pickPort prefers pinchtab's default 9867, scanning a small range before
// falling back to an ephemeral port.
func pickPort() string {
	for port := 9867; port <= 9877; port++ {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			_ = ln.Close()
			return fmt.Sprintf("%d", port)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "9867"
	}
	defer func() { _ = ln.Close() }()
	return fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)
}

// Client returns an API client, lazily starting (or reusing) the server.
// The fast path (client already set) only touches mu briefly; the health
// probe and the up-to-20s spawn run outside mu under startMu, so parallel
// tool calls and Stop are never blocked behind a cold start.
func (b *BrowserRuntime) Client(ctx context.Context) (*pinchClient, error) {
	b.mu.Lock()
	if b.client != nil && !b.stopped {
		c := b.client
		b.mu.Unlock()
		return c, nil
	}
	b.mu.Unlock()

	b.startMu.Lock()
	defer b.startMu.Unlock()
	// Another caller may have finished the start while we waited.
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return nil, ErrBrowserStopped
	}
	if b.client != nil {
		c := b.client
		b.mu.Unlock()
		return c, nil
	}
	b.mu.Unlock()

	if !b.Installed() {
		return nil, ErrBrowserNotInstalled
	}
	cfg, base, err := b.loadConfig()
	if err != nil {
		return nil, err
	}
	c := &pinchClient{base: base, token: cfg.Server.Token}
	if err := c.health(ctx); err == nil {
		// A previous woca run left the server alive — adopt it.
		b.log.Info().Str("base", base).Msg("pinchtab server reused")
		b.setClient(c, nil)
		return c, nil
	}
	proc, err := b.spawn(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if !b.setClient(c, proc) {
		// Stop() raced the spawn: discard the just-started process.
		_ = proc.Kill()
		return nil, fmt.Errorf("browser runtime was stopped during startup")
	}
	return c, nil
}

// setClient commits the client (and the spawned process) unless Stop already
// ran; it reports whether the commit happened.
func (b *BrowserRuntime) setClient(c *pinchClient, proc executor.Process) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return false
	}
	b.client = c
	b.proc = proc
	return true
}

// call runs fn against a healthy client. When the request fails with a
// transport error (the cached server died after it was started), the cached
// client is dropped and the runtime restarts the server once before
// retrying the whole sequence.
func (b *BrowserRuntime) call(ctx context.Context, fn func(*pinchClient) error) error {
	c, err := b.Client(ctx)
	if err != nil {
		return err
	}
	err = fn(c)
	if err == nil || ctx.Err() != nil || !isTransportError(err) {
		return err
	}
	b.mu.Lock()
	b.client = nil
	b.proc = nil // dead by definition
	b.mu.Unlock()
	c, err = b.Client(ctx)
	if err != nil {
		return err
	}
	return fn(c)
}

// isTransportError reports whether err is a connection-level failure rather
// than a pinchtab API response.
func isTransportError(err error) bool {
	var ue *url.Error
	return errors.As(err, &ue)
}

// spawn starts `pinchtab server` with the isolated config and waits for
// /health. It runs without b.mu held and returns the spawned process for the
// caller to commit.
func (b *BrowserRuntime) spawn(ctx context.Context, cfg pinchConfig) (executor.Process, error) {
	_ = os.MkdirAll(filepath.Dir(b.serverLogPath()), 0o755)
	logFH, err := os.OpenFile(b.serverLogPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	// The server outlives the request that started it (it is adopted by
	// later runs when it survives), so it starts under the background
	// context; ctx only bounds the health-poll below.
	proc, err := b.exec.Start(context.Background(),
		executor.ExecRequest{
			Name: b.binaryPath(),
			Args: []string{"server"},
			Env:  append(os.Environ(), "PINCHTAB_CONFIG="+b.configPath()),
		},
		executor.StartOptions{Stdout: logFH, Stderr: logFH})
	if err != nil {
		_ = logFH.Close()
		return nil, fmt.Errorf("start pinchtab: %w", err)
	}
	base := "http://" + net.JoinHostPort(cfg.Server.Bind, cfg.Server.Port)
	c := &pinchClient{base: base, token: cfg.Server.Token}

	// Reap the child when it exits so we do not leak zombies; exitErr is
	// read after the health-poll failure path.
	exitErr := make(chan error, 1)
	go func() { exitErr <- proc.Wait(); _ = logFH.Close() }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			_ = proc.Kill()
			return nil, ctx.Err()
		case err := <-exitErr:
			return nil, fmt.Errorf("pinchtab exited during startup: %w\n%s", err, b.logTail())
		default:
		}
		if err := c.health(ctx); err == nil {
			b.log.Info().Str("base", base).Msg("pinchtab server started")
			return proc, nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	_ = proc.Kill()
	return nil, fmt.Errorf("pinchtab did not become healthy within 20s\n%s", b.logTail())
}

// logTail returns the tail of the server log for error surfaces.
func (b *BrowserRuntime) logTail() string {
	raw, err := os.ReadFile(b.serverLogPath())
	if err != nil {
		return "(no server log)"
	}
	return "pinchtab log tail:\n" + tailLines(string(raw), 8)
}

// Stop kills the server if this process spawned it; an adopted instance is
// deliberately left running for reuse. A spawn still in flight discards its
// process when it finishes.
func (b *BrowserRuntime) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopped = true
	if b.proc != nil {
		_ = b.proc.Kill()
		b.proc = nil
	}
	b.client = nil
}

// pinchClient is the authenticated HTTP client for pinchtab's API.
type pinchClient struct {
	base  string
	token string
}

func (c *pinchClient) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 90 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("pinchtab %s %s: HTTP %d: %s", method, path, resp.StatusCode, pinchError(raw))
	}
	if out == nil {
		return nil
	}
	if w, ok := out.(*string); ok {
		*w = string(raw)
		return nil
	}
	return json.Unmarshal(raw, out)
}

// pinchError extracts pinchtab's {"code":…,"error":…} error payload.
func pinchError(raw []byte) string {
	var payload struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &payload) == nil && payload.Error != "" {
		if payload.Code != "" {
			return payload.Code + ": " + payload.Error
		}
		return payload.Error
	}
	return strings.TrimSpace(string(raw))
}

func (c *pinchClient) health(ctx context.Context) error {
	hctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	var payload struct {
		Status string `json:"status"`
	}
	return c.do(hctx, http.MethodGet, "/health", nil, &payload)
}

// navigateResult mirrors POST /navigate's success payload.
type navigateResult struct {
	TabID string `json:"tabId"`
	Title string `json:"title"`
	URL   string `json:"url"`
	// error payloads share the wire shape; a non-empty Error fails the call.
	Code  string `json:"code"`
	Error string `json:"error"`
}

func (c *pinchClient) navigate(ctx context.Context, rawURL string) (navigateResult, error) {
	var res navigateResult
	err := c.do(ctx, http.MethodPost, "/navigate", map[string]string{"url": rawURL}, &res)
	if err != nil {
		return res, err
	}
	if res.Error != "" {
		if res.Code != "" {
			return res, fmt.Errorf("navigate %s: %s", res.Code, res.Error)
		}
		return res, fmt.Errorf("navigate: %s", res.Error)
	}
	return res, nil
}

// textResult mirrors GET /text's payload.
type textResult struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Text      string `json:"text"`
	TextLen   int    `json:"textLength"`
	Truncated bool   `json:"truncated"`
}

func (c *pinchClient) text(ctx context.Context, maxChars int, raw bool) (textResult, error) {
	var res textResult
	q := url.Values{}
	if maxChars > 0 {
		q.Set("maxChars", fmt.Sprintf("%d", maxChars))
	}
	if raw {
		q.Set("mode", "raw")
	}
	err := c.do(ctx, http.MethodGet, "/text?"+q.Encode(), nil, &res)
	return res, err
}

// snapshot returns the compact interactive-element listing of the current tab.
func (c *pinchClient) snapshot(ctx context.Context) (string, error) {
	var out string
	q := url.Values{"filter": {"interactive"}, "format": {"compact"}}
	err := c.do(ctx, http.MethodGet, "/snapshot?"+q.Encode(), nil, &out)
	return out, err
}

func (c *pinchClient) action(ctx context.Context, body map[string]string) error {
	return c.do(ctx, http.MethodPost, "/action", body, nil)
}

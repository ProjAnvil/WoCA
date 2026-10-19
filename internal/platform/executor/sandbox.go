// sandbox.go is the contract-v2 sandbox Executor (CONTRACT.md, design §4.1):
// commands run in a remote sandbox behind a generic HTTP API instead of the
// host. The concrete provider is any HTTP service implementing the 15
// endpoints of the contract; E2B, Daytona, and friends fit with thin
// adapters layered on the provider registry.
package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrSandboxNotImplemented reports a surface the sandbox executor does not
// yet provide (the callers fall back or surface the error to the agent).
var ErrSandboxNotImplemented = errors.New("sandbox: operation not available in this sandbox executor revision")

// SandboxConfig configures the sandbox executor.
type SandboxConfig struct {
	// Endpoint is the sandbox service base URL (scheme://host:port).
	Endpoint string
	// APIKey authenticates to the sandbox service.
	APIKey string
	// Timeout bounds each sandbox HTTP call (default 120s — sandbox
	// provisioning can be slow).
	Timeout time.Duration
}

// Sandbox is an Executor backed by a remote sandbox service.
type Sandbox struct {
	client   *http.Client
	endpoint string
	apiKey   string
	sbxID    string
}

// compile-time interface check.
var _ Executor = (*Sandbox)(nil)

// NewSandbox creates a sandbox instance and provisions it.
func NewSandbox(cfg SandboxConfig) (*Sandbox, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("sandbox: endpoint is required")
	}
	if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("sandbox: invalid endpoint %q", cfg.Endpoint)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 120 * time.Second
	}
	s := &Sandbox{
		client:   &http.Client{Timeout: cfg.Timeout},
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:   cfg.APIKey,
	}
	if err := s.create(context.Background()); err != nil {
		return nil, fmt.Errorf("sandbox: create: %w", err)
	}
	return s, nil
}

func (s *Sandbox) create(ctx context.Context) error {
	var out struct {
		SandboxID string `json:"sandbox_id"`
	}
	if err := s.do(ctx, http.MethodPost, "/sandbox/create", map[string]any{
		"cwd": "/workspace",
	}, &out); err != nil {
		return err
	}
	if out.SandboxID == "" {
		return errors.New("sandbox: no sandbox id in create response")
	}
	s.sbxID = out.SandboxID
	return nil
}

// Close destroys the sandbox.
func (s *Sandbox) Close() error {
	if s.sbxID == "" {
		return nil
	}
	return s.do(context.Background(), http.MethodDelete, "/sandbox/"+s.sbxID, nil, nil)
}

func (s *Sandbox) do(ctx context.Context, method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Woca-Sandbox-Contract", "2")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("sandbox: %s %s: http %d", method, path, resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Exec runs a command in the sandbox per contract v2: argv array, cwd,
// env object, base64 stdin; stdout/stderr come back base64-encoded.
func (s *Sandbox) Exec(ctx context.Context, req ExecRequest) (ExecResult, error) {
	var out struct {
		ExitCode int    `json:"exit_code"`
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
	}
	if err := s.do(ctx, http.MethodPost, "/sandbox/"+s.sbxID+"/exec", map[string]any{
		"argv":       append([]string{req.Name}, req.Args...),
		"cwd":        req.Dir,
		"env":        envSliceToMap(req.Env),
		"stdin":      base64.StdEncoding.EncodeToString(req.Stdin),
		"timeout_ms": timeoutMillis(ctx),
	}, &out); err != nil {
		return ExecResult{}, err
	}
	stdout, err := base64.StdEncoding.DecodeString(out.Stdout)
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec stdout not base64: %w", err)
	}
	stderr, err := base64.StdEncoding.DecodeString(out.Stderr)
	if err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: exec stderr not base64: %w", err)
	}
	return ExecResult{ExitCode: out.ExitCode, Stdout: stdout, Stderr: stderr}, nil
}

// envSliceToMap converts the Env slice form ("K=V", "K=") into the contract
// v2 env object. It always returns a non-nil map so the wire body carries
// an env object ({} when nil/empty = inherit the sandbox defaults).
func envSliceToMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

// timeoutMillis derives the exec timeout_ms field from the context
// deadline; 0 means the backend default.
func timeoutMillis(ctx context.Context) int64 {
	if dl, ok := ctx.Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 {
			return ms
		}
	}
	return 0
}

// ReadFile reads a file from the sandbox (base64-wrapped content).
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	var out struct {
		Content string `json:"content"`
	}
	if err := s.do(ctx, http.MethodGet, "/sandbox/"+s.sbxID+"/read?path="+url.QueryEscape(path), nil, &out); err != nil {
		return nil, err
	}
	return base64.StdEncoding.DecodeString(out.Content)
}

// WriteFile writes data to the sandbox (base64 content + Unix mode bits).
func (s *Sandbox) WriteFile(ctx context.Context, path string, data []byte, perm fs.FileMode) error {
	return s.do(ctx, http.MethodPost, "/sandbox/"+s.sbxID+"/write", map[string]any{
		"path":    path,
		"content": base64.StdEncoding.EncodeToString(data),
		"mode":    int(perm),
	}, nil)
}

// MkdirAll is not yet available on sandboxes.
func (s *Sandbox) MkdirAll(context.Context, string, fs.FileMode) error {
	return ErrSandboxNotImplemented
}

// Stat is not yet available on sandboxes.
func (s *Sandbox) Stat(context.Context, string) (fs.FileInfo, error) {
	return nil, ErrSandboxNotImplemented
}

// Glob matches a pattern in the sandbox.
func (s *Sandbox) Glob(ctx context.Context, pattern string) ([]string, error) {
	var out struct {
		Matches []string `json:"matches"`
	}
	if err := s.do(ctx, http.MethodPost, "/sandbox/"+s.sbxID+"/glob", map[string]any{
		"pattern": pattern,
	}, &out); err != nil {
		return nil, err
	}
	return out.Matches, nil
}

// Grep is not yet available on sandboxes.
func (s *Sandbox) Grep(context.Context, string, *regexp.Regexp) ([]GrepMatch, error) {
	return nil, ErrSandboxNotImplemented
}

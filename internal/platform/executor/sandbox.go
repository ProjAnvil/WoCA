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
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// ErrSandboxNotImplemented reports a surface a sandbox provider does not
// provide (kept for provider adapters that surface explicit "not yet
// available" errors; the contract-v2 client implements the full Executor).
var ErrSandboxNotImplemented = errors.New("sandbox: operation not available in this sandbox executor revision")

// ErrSandboxUnsupported is returned by host-bound surfaces during sandbox
// runs (design §10.1 C-2): operations with no sandbox-side equivalent
// (file-history undo, worktree switching, MCP stdio, LSP).
var ErrSandboxUnsupported = errors.New("sandbox: operation not supported during sandboxed runs")

// IsSandbox reports whether x executes in a remote sandbox. Wrapper types
// that know their inner executor (Translate, future providers) delegate
// via the IsSandbox() bool method; the type check is the fallback.
func IsSandbox(x Executor) bool {
	if x == nil {
		return false
	}
	if s, ok := x.(interface{ IsSandbox() bool }); ok {
		return s.IsSandbox()
	}
	_, ok := x.(*Sandbox)
	return ok
}

// IsSandbox marks the sandbox executor for executor.IsSandbox.
func (s *Sandbox) IsSandbox() bool { return true }

// SandboxConfig configures the sandbox executor.
type SandboxConfig struct {
	// Endpoint is the sandbox service base URL (scheme://host:port).
	Endpoint string
	// APIKey authenticates to the sandbox service.
	APIKey string
	// Timeout bounds each sandbox HTTP call (default 120s — sandbox
	// provisioning can be slow).
	Timeout time.Duration
	// Workspace is the workspace ID this sandbox is routed under (EE
	// sandbox plane, design §4): sent as the create body's workspace field
	// and as the X-Woca-Workspace header on every request so the routing
	// proxy can route statelessly. Empty = not workspace-routed; the field
	// and header are omitted.
	Workspace string
}

// Sandbox is an Executor backed by a remote sandbox service.
type Sandbox struct {
	client    *http.Client
	endpoint  string
	apiKey    string
	workspace string
	sbxID     string
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
		client:    &http.Client{Timeout: cfg.Timeout},
		endpoint:  strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:    cfg.APIKey,
		workspace: cfg.Workspace,
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
	// workspace is the routing/workspace identity (CONTRACT.md §3.1); cwd
	// keeps its container-working-directory meaning. Both defaults hold
	// when the sandbox is not workspace-routed.
	body := map[string]any{"cwd": "/workspace"}
	if s.workspace != "" {
		body["workspace"] = s.workspace
	}
	if err := s.do(ctx, http.MethodPost, "/sandbox/create", body, &out); err != nil {
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

// withHeaders stamps the per-request headers shared by every sandbox call
// (do and doRaw): the contract version, optional bearer auth, and the
// optional X-Woca-Workspace routing header (CONTRACT.md §1) — the routing
// proxy routes on that header alone, so it must ride every request, not
// just create.
func (s *Sandbox) withHeaders(req *http.Request) {
	req.Header.Set("X-Woca-Sandbox-Contract", "2")
	if s.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+s.apiKey)
	}
	if s.workspace != "" {
		req.Header.Set("X-Woca-Workspace", s.workspace)
	}
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
	s.withHeaders(req)
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

// doRaw performs a non-JSON sandbox request (raw body in or out) with the
// same contract headers as do. On success the response body is left open
// for the caller; status >= 300 reads at most 4 KiB of the error body and
// returns it as an error.
func (s *Sandbox) doRaw(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, s.endpoint+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	s.withHeaders(req)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		snippet := make([]byte, 4096)
		n, _ := io.ReadFull(resp.Body, snippet)
		if n == 0 {
			return nil, fmt.Errorf("sandbox: %s %s: http %d", method, path, resp.StatusCode)
		}
		var errOut struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(snippet[:n], &errOut) == nil && errOut.Error != "" {
			return nil, fmt.Errorf("sandbox: %s %s: http %d: %s", method, path, resp.StatusCode, errOut.Error)
		}
		return nil, fmt.Errorf("sandbox: %s %s: http %d: %s", method, path, resp.StatusCode, snippet[:n])
	}
	return resp, nil
}

// PutArchive uploads a gzipped tar of the host workspace into the sandbox
// (PUT /sandbox/{id}/archive, CONTRACT.md §3.13). The tar conventions are
// the caller's responsibility (§4): entries relative to the workspace
// root, permission bits and symlinks preserved, .git excluded.
func (s *Sandbox) PutArchive(ctx context.Context, r io.Reader) error {
	resp, err := s.doRaw(ctx, http.MethodPut, "/sandbox/"+s.sbxID+"/archive", "application/gzip", r)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// GetArchive downloads the sandbox workspace as a gzipped tar stream
// (GET /sandbox/{id}/archive, CONTRACT.md §3.14). The caller must Close
// the returned reader.
func (s *Sandbox) GetArchive(ctx context.Context) (io.ReadCloser, error) {
	resp, err := s.doRaw(ctx, http.MethodGet, "/sandbox/"+s.sbxID+"/archive", "", nil)
	if err != nil {
		return nil, err
	}
	if ct := resp.Header.Get("Content-Type"); ct != "" && ct != "application/gzip" && ct != "application/octet-stream" {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("sandbox: archive content-type %q, want application/gzip", ct)
	}
	return resp.Body, nil
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

// MkdirAll creates path and all missing parents in the sandbox
// (POST .../mkdir); permission bits are backend-defined per the contract.
func (s *Sandbox) MkdirAll(ctx context.Context, path string, _ fs.FileMode) error {
	return s.do(ctx, http.MethodPost, "/sandbox/"+s.sbxID+"/mkdir", map[string]any{
		"path": path,
	}, nil)
}

// Stat returns file information for path in the sandbox (GET .../stat).
func (s *Sandbox) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	var out struct {
		Mode    int64 `json:"mode"`
		Size    int64 `json:"size"`
		MtimeMs int64 `json:"mtime_ms"`
		IsDir   bool  `json:"is_dir"`
	}
	if err := s.do(ctx, http.MethodGet, "/sandbox/"+s.sbxID+"/stat?path="+url.QueryEscape(path), nil, &out); err != nil {
		return nil, err
	}
	return &sandboxFileInfo{
		name:    path,
		mode:    out.Mode,
		size:    out.Size,
		mtimeMs: out.MtimeMs,
		isDir:   out.IsDir,
	}, nil
}

// sandboxFileInfo adapts a contract v2 stat response to fs.FileInfo.
type sandboxFileInfo struct {
	name    string // full path; Name() derives the base element
	mode    int64  // Unix permission bits (decimal on the wire)
	size    int64
	mtimeMs int64
	isDir   bool
}

var _ fs.FileInfo = (*sandboxFileInfo)(nil)

func (fi *sandboxFileInfo) Name() string {
	if i := strings.LastIndexByte(fi.name, '/'); i >= 0 {
		return fi.name[i+1:]
	}
	return fi.name
}

func (fi *sandboxFileInfo) Size() int64        { return fi.size }
func (fi *sandboxFileInfo) ModTime() time.Time { return time.UnixMilli(fi.mtimeMs) }
func (fi *sandboxFileInfo) IsDir() bool        { return fi.isDir }
func (fi *sandboxFileInfo) Sys() any           { return nil }

func (fi *sandboxFileInfo) Mode() fs.FileMode {
	m := fs.FileMode(fi.mode).Perm()
	if fi.isDir {
		m |= fs.ModeDir
	}
	return m
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

// Grep walks root in the sandbox (POST .../grep) and returns lines matching
// pattern, mirroring the contract's hidden-directory skip semantics.
func (s *Sandbox) Grep(ctx context.Context, root string, pattern *regexp.Regexp) ([]GrepMatch, error) {
	var out struct {
		Matches []struct {
			Path string `json:"path"`
			Line int    `json:"line"`
			Text string `json:"text"`
		} `json:"matches"`
	}
	if err := s.do(ctx, http.MethodPost, "/sandbox/"+s.sbxID+"/grep", map[string]any{
		"root":        root,
		"pattern":     pattern.String(),
		"max_matches": 200,
	}, &out); err != nil {
		return nil, err
	}
	matches := make([]GrepMatch, 0, len(out.Matches))
	for _, m := range out.Matches {
		text, err := base64.StdEncoding.DecodeString(m.Text)
		if err != nil {
			return nil, fmt.Errorf("sandbox: grep text not base64: %w", err)
		}
		matches = append(matches, GrepMatch{Path: m.Path, Line: m.Line, Text: string(text)})
	}
	return matches, nil
}

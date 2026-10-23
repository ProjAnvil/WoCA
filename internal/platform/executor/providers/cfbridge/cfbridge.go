// Package cfbridge adapts the Cloudflare sandbox bridge HTTP API to the
// executor.Executor interface. Unlike the woca contract v2 client
// (executor.Sandbox), the bridge speaks its own protocol: exec responses
// are server-sent events, the file surface moves raw bytes, and the
// archive endpoints exchange uncompressed tars (adapted to gzip here so
// callers can treat both providers alike).
//
// Provider limitations (bridge API shape, not omissions):
//   - exec/start take no env and no stdin; ExecRequest.Env and .Stdin
//     are ignored, and Process.Stdin discards writes (the bridge has no
//     process-input endpoint).
//   - WriteFile perm is ignored (the raw file endpoint has no mode field).
//   - Glob/Grep/Stat/MkdirAll are synthesized through exec running
//     find/grep/stat/mkdir, so the sandbox image must ship coreutils.
package cfbridge

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/woca/internal/platform/executor"
)

// maxHydrateBytes is the bridge-side cap on hydrate uploads (raw tar).
const maxHydrateBytes = 32 << 20

// defaultTimeout bounds bridge calls when the caller's context carries
// no deadline.
const defaultTimeout = 120 * time.Second

// Config configures the bridge client.
type Config struct {
	// Endpoint is the bridge base URL (scheme://host[:port]).
	Endpoint string
	// APIKey is sent as the Bearer token when non-empty.
	APIKey string
	// Timeout bounds each non-streaming call when the caller's context
	// has no deadline; <= 0 means defaultTimeout.
	Timeout time.Duration
}

// Bridge is an executor.Executor backed by one Cloudflare sandbox bridge
// sandbox. New provisions the sandbox; Close destroys it.
type Bridge struct {
	client   *http.Client // no global timeout: SSE streams live here
	endpoint string
	apiKey   string
	timeout  time.Duration
	sbxID    string
}

// compile-time interface checks.
var (
	_ executor.Executor = (*Bridge)(nil)
	_ io.Closer         = (*Bridge)(nil)
)

// New provisions a sandbox via POST /v1/sandbox and returns a Bridge
// driving it.
func New(cfg Config) (*Bridge, error) {
	if cfg.Endpoint == "" {
		return nil, errors.New("cfbridge: endpoint is required")
	}
	if u, err := url.Parse(cfg.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("cfbridge: invalid endpoint %q", cfg.Endpoint)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = defaultTimeout
	}
	b := &Bridge{
		client:   &http.Client{},
		endpoint: strings.TrimRight(cfg.Endpoint, "/"),
		apiKey:   cfg.APIKey,
		timeout:  cfg.Timeout,
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	var out struct {
		ID string `json:"id"`
	}
	if err := b.doJSON(ctx, http.MethodPost, "/v1/sandbox", map[string]any{}, &out); err != nil {
		return nil, fmt.Errorf("cfbridge: create: %w", err)
	}
	if out.ID == "" {
		return nil, errors.New("cfbridge: no sandbox id in create response")
	}
	b.sbxID = out.ID
	return b, nil
}

// Close destroys the sandbox (DELETE /v1/sandbox/{id}); it is idempotent.
func (b *Bridge) Close() error {
	if b.sbxID == "" {
		return nil
	}
	id := b.sbxID
	b.sbxID = ""
	ctx, cancel := context.WithTimeout(context.Background(), b.timeout)
	defer cancel()
	return b.doJSON(ctx, http.MethodDelete, "/v1/sandbox/"+id, nil, nil)
}

// Running reports whether the sandbox is up (GET /v1/sandbox/{id}/running).
func (b *Bridge) Running(ctx context.Context) (bool, error) {
	var out struct {
		Running bool `json:"running"`
	}
	if err := b.doJSON(ctx, http.MethodGet, b.sbxPath("/running"), nil, &out); err != nil {
		return false, err
	}
	return out.Running, nil
}

// --- HTTP plumbing -----------------------------------------------------------

// withDeadline adds the configured timeout to ctx when it carries no
// deadline of its own.
func (b *Bridge) withDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, b.timeout)
}

// do performs one bridge request and returns the open response on 2xx.
func (b *Bridge) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, b.endpoint+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if b.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+b.apiKey)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer func() { _ = resp.Body.Close() }()
		return nil, fmt.Errorf("cfbridge: %s %s: http %d: %s",
			method, path, resp.StatusCode, errorSnippet(resp.Body))
	}
	return resp, nil
}

// doJSON marshals in as the request body and decodes a JSON response.
func (b *Bridge) doJSON(ctx context.Context, method, path string, in, out any) error {
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	var rdr io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	resp, err := b.do(ctx, method, path, "application/json", rdr)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// errorSnippet reads at most 4 KiB of an error body, preferring the
// {"error": "..."} field when present.
func errorSnippet(r io.Reader) string {
	snippet := make([]byte, 4096)
	n, _ := io.ReadFull(r, snippet)
	if n == 0 {
		return ""
	}
	var errOut struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(snippet[:n], &errOut) == nil && errOut.Error != "" {
		return errOut.Error
	}
	return strings.TrimSpace(string(snippet[:n]))
}

func (b *Bridge) sbxPath(suffix string) string {
	return "/v1/sandbox/" + b.sbxID + suffix
}

// filePath builds the file-endpoint path with each path segment
// URL-escaped individually.
func (b *Bridge) filePath(path string) string {
	segs := strings.Split(strings.TrimPrefix(path, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return b.sbxPath("/file/") + strings.Join(segs, "/")
}

// timeoutMillis derives the exec timeout_ms field from the context
// deadline; 0 means the bridge default.
func timeoutMillis(ctx context.Context) int64 {
	if dl, ok := ctx.Deadline(); ok {
		if ms := time.Until(dl).Milliseconds(); ms > 0 {
			return ms
		}
	}
	return 0
}

// --- Exec --------------------------------------------------------------------

// Exec runs a command to completion via POST /v1/sandbox/{id}/exec and
// aggregates the SSE response: stdout/stderr events append base64-decoded
// bytes, exit sets the exit code, and an error event is recorded as
// stderr with exit code -1.
func (b *Bridge) Exec(ctx context.Context, req executor.ExecRequest) (executor.ExecResult, error) {
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	body := map[string]any{
		"argv":       append([]string{req.Name}, req.Args...),
		"cwd":        req.Dir,
		"timeout_ms": timeoutMillis(ctx),
	}
	resp, err := b.do(ctx, http.MethodPost, b.sbxPath("/exec"), "application/json", jsonReader(body))
	if err != nil {
		return executor.ExecResult{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := requireEventStream(resp); err != nil {
		return executor.ExecResult{}, err
	}

	var res executor.ExecResult
	res.ExitCode = -1
	terminated := false
	var parseErr error
	err = scanSSE(resp.Body, func(name, data string) bool {
		switch name {
		case "stdout", "stderr":
			chunk, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				parseErr = fmt.Errorf("cfbridge: exec %s event not base64: %w", name, err)
				return false
			}
			if name == "stdout" {
				res.Stdout = append(res.Stdout, chunk...)
			} else {
				res.Stderr = append(res.Stderr, chunk...)
			}
		case "exit":
			var e struct {
				ExitCode int `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				parseErr = fmt.Errorf("cfbridge: exec exit event: %w", err)
				return false
			}
			res.ExitCode = e.ExitCode
			terminated = true
		case "error":
			var e struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				parseErr = fmt.Errorf("cfbridge: exec error event: %w", err)
				return false
			}
			msg := e.Error
			if e.Code != "" && msg == "" {
				msg = e.Code
			}
			res.Stderr = append(res.Stderr, []byte(msg)...)
			res.ExitCode = -1
			terminated = true
		}
		return true
	})
	if err != nil {
		return executor.ExecResult{}, fmt.Errorf("cfbridge: exec stream: %w", err)
	}
	if parseErr != nil {
		return executor.ExecResult{}, parseErr
	}
	if !terminated {
		return executor.ExecResult{}, errors.New("cfbridge: exec stream ended without exit event")
	}
	return res, nil
}

// runChecked runs a command via Exec and turns a non-zero exit into an
// error carrying stderr.
func (b *Bridge) runChecked(ctx context.Context, name string, args ...string) error {
	res, err := b.Exec(ctx, executor.ExecRequest{Name: name, Args: args})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("cfbridge: %s: exit %d: %s", name, res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}

func jsonReader(v any) io.Reader {
	raw, err := json.Marshal(v)
	if err != nil {
		return bytes.NewReader(nil) // unreachable for map[string]any inputs
	}
	return bytes.NewReader(raw)
}

func requireEventStream(resp *http.Response) error {
	ct := resp.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/event-stream") {
		return fmt.Errorf("cfbridge: exec: unexpected content-type %q, want text/event-stream", ct)
	}
	return nil
}

// --- Start / Process ---------------------------------------------------------

// Start launches a long-running command on the same exec endpoint,
// holding the SSE connection open: the stream is the process lifetime.
// Aborting the connection (Kill, or canceling ctx) terminates the
// process bridge-side.
func (b *Bridge) Start(ctx context.Context, req executor.ExecRequest, opts executor.StartOptions) (executor.Process, error) {
	body := map[string]any{
		"argv": append([]string{req.Name}, req.Args...),
		"cwd":  req.Dir,
	}
	// No deadline: the stream outlives the request context. ctx cancel
	// still kills the process via the watcher goroutine below.
	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	resp, err := b.do(streamCtx, http.MethodPost, b.sbxPath("/exec"), "application/json", jsonReader(body))
	if err != nil {
		cancel()
		return nil, err
	}
	if err := requireEventStream(resp); err != nil {
		cancel()
		_ = resp.Body.Close()
		return nil, err
	}

	var stdoutR io.Reader
	var stdoutW io.WriteCloser
	if opts.Stdout == nil {
		pr, pw := io.Pipe()
		stdoutR, stdoutW = pr, pw
	}
	p := &bridgeProcess{
		cancel:  cancel,
		body:    resp.Body,
		stdoutR: stdoutR,
		stdoutW: stdoutW,
		opts:    opts,
		exitCh:  make(chan error, 1),
		stopCh:  make(chan struct{}),
	}
	go p.consume()
	if ctx.Done() != nil {
		go func() {
			select {
			case <-ctx.Done():
				_ = p.Kill()
			case <-p.stopCh:
			}
		}()
	}
	return p, nil
}

// bridgeProcess is a Process backed by one held-open exec SSE stream.
type bridgeProcess struct {
	cancel    context.CancelFunc
	body      io.ReadCloser
	stdoutR   io.Reader // nil when StartOptions.Stdout redirected output
	stdoutW   io.WriteCloser
	opts      executor.StartOptions
	exitCh    chan error
	stopCh    chan struct{}
	closeOnce sync.Once
}

var _ executor.Process = (*bridgeProcess)(nil)

// Stdin returns a sink that accepts and discards writes: the bridge has
// no process-input endpoint, so stdin is simply not available.
func (p *bridgeProcess) Stdin() io.WriteCloser { return discardWriteCloser{} }

func (p *bridgeProcess) Stdout() io.Reader { return p.stdoutR }

func (p *bridgeProcess) Wait() error { return <-p.exitCh }

// Kill aborts the SSE stream, which terminates the process bridge-side.
// The wait result of a killed process is nil (the caller asked for
// termination), mirroring executor.Sandbox's Kill semantics.
func (p *bridgeProcess) Kill() error {
	p.finish(nil)
	p.cancel()
	return nil
}

// finish reports the process outcome exactly once.
func (p *bridgeProcess) finish(err error) {
	p.closeOnce.Do(func() {
		if p.stdoutW != nil {
			_ = p.stdoutW.Close()
		}
		p.exitCh <- err
		close(p.stopCh)
	})
}

// consume parses the SSE stream, routing stdout events to the pipe (or
// StartOptions.Stdout) and stderr events to StartOptions.Stderr.
func (p *bridgeProcess) consume() {
	defer func() { _ = p.body.Close() }()
	_ = scanSSE(p.body, func(name, data string) bool {
		switch name {
		case "stdout", "stderr":
			chunk, err := base64.StdEncoding.DecodeString(data)
			if err != nil {
				p.finish(fmt.Errorf("cfbridge: stream %s event not base64: %w", name, err))
				return false
			}
			if name == "stdout" {
				if p.opts.Stdout != nil {
					_, _ = p.opts.Stdout.Write(chunk)
				} else if p.stdoutW != nil {
					if _, werr := p.stdoutW.Write(chunk); werr != nil {
						p.finish(werr)
						return false
					}
				}
			} else if p.opts.Stderr != nil {
				_, _ = p.opts.Stderr.Write(chunk)
			}
		case "exit":
			var e struct {
				ExitCode int `json:"exit_code"`
			}
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				p.finish(fmt.Errorf("cfbridge: stream exit event: %w", err))
				return false
			}
			if e.ExitCode == 0 {
				p.finish(nil)
			} else {
				p.finish(&executor.ExitError{Code: e.ExitCode})
			}
			return false
		case "error":
			var e struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal([]byte(data), &e)
			msg := e.Error
			if msg == "" {
				msg = e.Code
			}
			p.finish(errors.New("cfbridge: " + msg))
			return false
		}
		return true
	})
	select {
	case <-p.stopCh: // killed or already finished — nothing to report
	default:
		p.finish(errors.New("cfbridge: exec stream ended without exit event"))
	}
}

// discardWriteCloser accepts and drops all writes.
type discardWriteCloser struct{}

func (discardWriteCloser) Write(b []byte) (int, error) { return len(b), nil }
func (discardWriteCloser) Close() error                { return nil }

// --- file surface -------------------------------------------------------------

// ReadFile fetches the raw bytes at path (GET .../file/<path>).
func (b *Bridge) ReadFile(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	resp, err := b.do(ctx, http.MethodGet, b.filePath(path), "", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(resp.Body)
}

// WriteFile uploads raw bytes (PUT .../file/<path>). perm is ignored:
// the bridge file endpoint has no mode field and applies its default.
func (b *Bridge) WriteFile(ctx context.Context, path string, data []byte, _ fs.FileMode) error {
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	resp, err := b.do(ctx, http.MethodPut, b.filePath(path), "application/octet-stream", bytes.NewReader(data))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// MkdirAll creates path and all missing parents via `mkdir -p`; perm is
// ignored (mkdir applies the umask default).
func (b *Bridge) MkdirAll(ctx context.Context, path string, _ fs.FileMode) error {
	return b.runChecked(ctx, "mkdir", "-p", path)
}

// Stat runs `stat -c "%a %s %Y %F" <path>` and parses the
// "<octal-mode> <size> <mtime-seconds> <type>" line.
func (b *Bridge) Stat(ctx context.Context, path string) (fs.FileInfo, error) {
	res, err := b.Exec(ctx, executor.ExecRequest{Name: "stat", Args: []string{"-c", "%a %s %Y %F", path}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("cfbridge: stat %s: %s", path, strings.TrimSpace(string(res.Stderr)))
	}
	// Format: "<octal-mode> <size> <mtime-seconds> <type>"; the type
	// itself may contain spaces ("regular file", "symbolic link").
	fields := strings.SplitN(strings.TrimSpace(string(res.Stdout)), " ", 4)
	if len(fields) != 4 {
		return nil, fmt.Errorf("cfbridge: stat %s: unexpected output %q", path, strings.TrimSpace(string(res.Stdout)))
	}
	mode, err := strconv.ParseUint(fields[0], 8, 32)
	if err != nil {
		return nil, fmt.Errorf("cfbridge: stat %s: bad mode %q: %w", path, fields[0], err)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cfbridge: stat %s: bad size %q: %w", path, fields[1], err)
	}
	mtime, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil {
		return nil, fmt.Errorf("cfbridge: stat %s: bad mtime %q: %w", path, fields[2], err)
	}
	fi := &bridgeFileInfo{
		name:  path,
		mode:  fs.FileMode(mode).Perm(),
		size:  size,
		mtime: time.Unix(mtime, 0),
		isDir: fields[3] == "directory",
	}
	if fields[3] == "symbolic link" {
		fi.mode |= fs.ModeSymlink
	}
	return fi, nil
}

// bridgeFileInfo adapts a parsed stat line to fs.FileInfo.
type bridgeFileInfo struct {
	name  string // full path; Name() derives the base element
	mode  fs.FileMode
	size  int64
	mtime time.Time
	isDir bool
}

var _ fs.FileInfo = (*bridgeFileInfo)(nil)

func (fi *bridgeFileInfo) Name() string {
	if i := strings.LastIndexByte(fi.name, '/'); i >= 0 {
		return fi.name[i+1:]
	}
	return fi.name
}
func (fi *bridgeFileInfo) Size() int64        { return fi.size }
func (fi *bridgeFileInfo) ModTime() time.Time { return fi.mtime }
func (fi *bridgeFileInfo) IsDir() bool        { return fi.isDir }
func (fi *bridgeFileInfo) Sys() any           { return nil }
func (fi *bridgeFileInfo) Mode() fs.FileMode {
	m := fi.mode
	if fi.isDir {
		m |= fs.ModeDir
	}
	return m
}

// Glob runs `find <root> -path <pattern>` (find's * crosses separators)
// and filters the results with filepath.Match to restore Go glob
// semantics. root is the meta-free directory prefix of the pattern.
// Unlike filepath.Glob, a missing root surfaces as an error (find's
// exit status) rather than an empty match.
func (b *Bridge) Glob(ctx context.Context, pattern string) ([]string, error) {
	root := globRoot(pattern)
	res, err := b.Exec(ctx, executor.ExecRequest{Name: "find", Args: []string{root, "-path", pattern}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("cfbridge: find %s: %s", root, strings.TrimSpace(string(res.Stderr)))
	}
	var matches []string
	for line := range strings.SplitSeq(string(res.Stdout), "\n") {
		if line == "" {
			continue
		}
		if ok, _ := filepath.Match(pattern, line); ok || line == pattern {
			matches = append(matches, line)
		}
	}
	return matches, nil
}

// globRoot returns the deepest meta-free directory prefix of pattern.
func globRoot(pattern string) string {
	i := strings.IndexAny(pattern, "*?[")
	if i < 0 { // exact path: search its directory
		if j := strings.LastIndexByte(pattern, '/'); j > 0 {
			return pattern[:j]
		}
		return "/"
	}
	if j := strings.LastIndexByte(pattern[:i], '/'); j > 0 {
		return pattern[:j]
	}
	return "/"
}

// Grep runs GNU grep with the pinned shape
//
//	grep -r -n -E -I --null --exclude-dir=.* <pattern> <root>
//
// (-I skips binary files so "Binary file matches" lines cannot corrupt
// the parse; --exclude-dir=.* skips hidden directories while still
// searching a hidden root itself, matching the Executor contract; the
// RE2 pattern source is ERE-compatible for the common subset). Output
// records are "path\0line:text" (--null, NUL after the path) with a
// "path:line:text" fallback; grep exit 1 (no matches) is not an error.
// Results are capped at 200 per the contract's max_matches.
func (b *Bridge) Grep(ctx context.Context, root string, pattern *regexp.Regexp) ([]executor.GrepMatch, error) {
	res, err := b.Exec(ctx, executor.ExecRequest{Name: "grep", Args: []string{
		"-r", "-n", "-E", "-I", "--null", "--exclude-dir=.*", pattern.String(), root,
	}})
	if err != nil {
		return nil, err
	}
	if res.ExitCode > 1 {
		return nil, fmt.Errorf("cfbridge: grep %s: %s", root, strings.TrimSpace(string(res.Stderr)))
	}
	matches := make([]executor.GrepMatch, 0, 16)
	for rec := range strings.SplitSeq(string(res.Stdout), "\n") {
		if rec == "" || len(matches) == 200 {
			continue
		}
		path, rest := rec, ""
		if p, r, ok := strings.Cut(rec, "\x00"); ok {
			path, rest = p, r
		} else if parts := strings.SplitN(rec, ":", 3); len(parts) == 3 {
			path, rest = parts[0], parts[1]+":"+parts[2]
		} else {
			continue
		}
		lineStr, text, _ := strings.Cut(rest, ":")
		line, err := strconv.Atoi(lineStr)
		if err != nil {
			continue
		}
		matches = append(matches, executor.GrepMatch{Path: path, Line: line, Text: text})
	}
	return matches, nil
}

// --- archives (persist / hydrate) --------------------------------------------

// PutArchive uploads a gzipped tar of the host workspace into the
// sandbox. The bridge's hydrate endpoint takes an uncompressed tar, so
// the gzip stream is decompressed client-side; raw tars larger than
// 32 MiB are rejected without a request.
func (b *Bridge) PutArchive(ctx context.Context, r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("cfbridge: hydrate: not a gzip stream: %w", err)
	}
	defer func() { _ = gz.Close() }()
	var buf bytes.Buffer
	n, err := io.Copy(&buf, io.LimitReader(gz, maxHydrateBytes+1))
	if err != nil {
		return fmt.Errorf("cfbridge: hydrate: decompress: %w", err)
	}
	if n > maxHydrateBytes {
		return fmt.Errorf("cfbridge: hydrate: archive exceeds %d bytes", maxHydrateBytes)
	}
	ctx, cancel := b.withDeadline(ctx)
	defer cancel()
	resp, err := b.do(ctx, http.MethodPost, b.sbxPath("/hydrate"), "application/x-tar", &buf)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// GetArchive downloads the sandbox workspace as a gzipped tar stream,
// compressing the bridge's raw tar persist response on the fly. The
// caller's context governs the whole download.
func (b *Bridge) GetArchive(ctx context.Context) (io.ReadCloser, error) {
	resp, err := b.do(ctx, http.MethodPost, b.sbxPath("/persist?excludes=.git"), "", nil)
	if err != nil {
		return nil, err
	}
	pr, pw := io.Pipe()
	go func() {
		zw := gzip.NewWriter(pw)
		_, copyErr := io.Copy(zw, resp.Body)
		closeErr := zw.Close()
		_ = resp.Body.Close()
		switch {
		case copyErr != nil:
			_ = pw.CloseWithError(copyErr)
		case closeErr != nil:
			_ = pw.CloseWithError(closeErr)
		default:
			_ = pw.Close()
		}
	}()
	return pr, nil
}

// --- SSE parsing ---------------------------------------------------------------

// scanSSE reads a server-sent-events stream, splitting on blank-line
// (\n\n) block boundaries and parsing event:/data: lines (comment lines
// and unknown fields are skipped, a trailing \r is tolerated, multiple
// data lines join with \n, and an unterminated final block is still
// dispatched). fn returning false stops the scan.
func scanSSE(r io.Reader, fn func(name, data string) bool) error {
	var pending []byte
	chunk := make([]byte, 8192)
	for {
		n, err := r.Read(chunk)
		pending = append(pending, chunk[:n]...)
		for {
			i := bytes.Index(pending, []byte("\n\n"))
			if i < 0 {
				break
			}
			name, data, ok := parseSSEBlock(pending[:i])
			pending = pending[i+2:]
			if ok && !fn(name, data) {
				return nil
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && len(bytes.TrimSpace(pending)) > 0 {
				name, data, ok := parseSSEBlock(pending)
				if ok {
					_ = fn(name, data)
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// parseSSEBlock parses one event block into its event name and joined
// data lines; ok is false for blocks with neither field (comments).
func parseSSEBlock(block []byte) (name string, data string, ok bool) {
	var dataLines []string
	for line := range strings.SplitSeq(string(block), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if v, isEvent := strings.CutPrefix(line, "event:"); isEvent {
			name = strings.TrimSpace(v)
		} else if v, isData := strings.CutPrefix(line, "data:"); isData {
			dataLines = append(dataLines, strings.TrimPrefix(v, " "))
		}
	}
	if name == "" && len(dataLines) == 0 {
		return "", "", false
	}
	return name, strings.Join(dataLines, "\n"), true
}

// Package fakesandbox is an in-memory implementation of the sandbox HTTP
// contract v2 (internal/platform/executor/CONTRACT.md) for tests: every
// endpoint is served from RAM maps, exec/start really spawn the requested
// command via os/exec inside this process, and archives are tar.gz
// round-trips over the same maps. The file surface and the process surface
// do not share state — a spawned command's file writes are not visible to
// the read/write endpoints — which is fine for the contract-level tests
// it backs.
package fakesandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// workspaceRoot is the sandbox-side directory archives unpack into and the
// path prefix the file surface is addressed by.
const workspaceRoot = "/workspace"

// Server serves the full contract v2 endpoint set from memory.
type Server struct {
	mu        sync.Mutex
	nextID    int64
	sandboxes map[string]*fakeSandbox
	root      string // fallback cwd for spawned commands
	syncs     int    // workspace sync calls served (for test assertions)
}

// New returns a Server with an empty sandbox table.
func New() *Server {
	root, err := os.MkdirTemp("", "fakesandbox-*")
	if err != nil {
		root = os.TempDir()
	}
	return &Server{sandboxes: map[string]*fakeSandbox{}, root: root}
}

// Handler returns the contract v2 routes mounted on a fresh mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandbox/create", s.handleCreate)
	mux.HandleFunc("POST /sandbox/{id}/exec", s.withSandbox(s.handleExec))
	mux.HandleFunc("POST /sandbox/{id}/start", s.withSandbox(s.handleStart))
	mux.HandleFunc("GET /sandbox/{id}/sessions/{sid}/output", s.withSandboxSession(s.handleOutput))
	mux.HandleFunc("POST /sandbox/{id}/sessions/{sid}/input", s.withSandboxSession(s.handleInput))
	mux.HandleFunc("POST /sandbox/{id}/sessions/{sid}/kill", s.withSandboxSession(s.handleKill))
	mux.HandleFunc("GET /sandbox/{id}/read", s.withSandbox(s.handleRead))
	mux.HandleFunc("POST /sandbox/{id}/write", s.withSandbox(s.handleWrite))
	mux.HandleFunc("GET /sandbox/{id}/stat", s.withSandbox(s.handleStat))
	mux.HandleFunc("POST /sandbox/{id}/mkdir", s.withSandbox(s.handleMkdir))
	mux.HandleFunc("POST /sandbox/{id}/glob", s.withSandbox(s.handleGlob))
	mux.HandleFunc("POST /sandbox/{id}/grep", s.withSandbox(s.handleGrep))
	mux.HandleFunc("PUT /sandbox/{id}/archive", s.withSandbox(s.handlePutArchive))
	mux.HandleFunc("GET /sandbox/{id}/archive", s.withSandbox(s.handleGetArchive))
	mux.HandleFunc("POST /sandbox/{id}/workspace/sync", s.withSandbox(s.handleWorkspaceSync))
	mux.HandleFunc("DELETE /sandbox/{id}", s.handleDestroy) // idempotent 2xx per CONTRACT.md §3.15
	return mux
}

// SyncCalls reports how many workspace syncs were served across all
// sandboxes, for contract-test assertions.
func (s *Server) SyncCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.syncs
}

// Addr starts the fake on a random port, bound to t's lifetime, and
// returns its base URL.
func Addr(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(New().Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// --- fake sandbox state -------------------------------------------------

type fakeFile struct {
	data     []byte
	mode     int64
	mtimeMs  int64
	typeflag byte // tar.TypeReg or tar.TypeSymlink; symlink data is the target
}

type fakeSandbox struct {
	mu       sync.Mutex
	files    map[string]fakeFile
	dirs     map[string]int64 // path -> mode bits
	sessions map[string]*fakeSession
	nextSes  int64
}

func newFakeSandbox() *fakeSandbox {
	return &fakeSandbox{
		files:    map[string]fakeFile{},
		dirs:     map[string]int64{},
		sessions: map[string]*fakeSession{},
	}
}

type fakeEvent struct {
	stream string
	data   []byte
	start  int // cumulative byte offset before this event
}

type fakeSession struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	events   []fakeEvent
	total    int
	done     bool
	exitCode int
}

// --- plumbing ------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) get(id string) *fakeSandbox {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sandboxes[id]
}

type sandboxHandler func(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox)

func (s *Server) withSandbox(h sandboxHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sbx := s.get(r.PathValue("id"))
		if sbx == nil {
			writeErr(w, http.StatusNotFound, "unknown sandbox "+r.PathValue("id"))
			return
		}
		h(w, r, sbx)
	}
}

type sessionHandler func(w http.ResponseWriter, r *http.Request, ses *fakeSession)

func (s *Server) withSandboxSession(h sessionHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sbx := s.get(r.PathValue("id"))
		if sbx == nil {
			writeErr(w, http.StatusNotFound, "unknown sandbox "+r.PathValue("id"))
			return
		}
		sbx.mu.Lock()
		ses := sbx.sessions[r.PathValue("sid")]
		sbx.mu.Unlock()
		if ses == nil {
			writeErr(w, http.StatusNotFound, "unknown session "+r.PathValue("sid"))
			return
		}
		h(w, r, ses)
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return false
	}
	return true
}

// --- lifecycle -----------------------------------------------------------

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cwd string `json:"cwd"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // body is advisory only
	s.mu.Lock()
	s.nextID++
	id := "sbx_" + strconv.FormatInt(s.nextID, 10)
	s.sandboxes[id] = newFakeSandbox()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"sandbox_id": id})
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	sbx := s.sandboxes[r.PathValue("id")]
	delete(s.sandboxes, r.PathValue("id"))
	s.mu.Unlock()
	if sbx != nil {
		sbx.mu.Lock()
		sessions := make([]*fakeSession, 0, len(sbx.sessions))
		for _, ses := range sbx.sessions {
			sessions = append(sessions, ses)
		}
		sbx.mu.Unlock()
		for _, ses := range sessions {
			_ = ses.kill()
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- process surface -----------------------------------------------------

// buildCommand assembles the os/exec.Cmd shared by exec and start: real
// argv (no shell), cwd honored only when it exists on this machine (the
// fake's root otherwise), env overlayed on the server environment. The
// returned cancel releases the timeout timer and must be called once the
// command finished (nil when no timeout was requested).
func (s *Server) buildCommand(argv []string, cwd string, env map[string]string, timeoutMs int64) (*exec.Cmd, context.CancelFunc) {
	if len(argv) == 0 {
		return nil, nil
	}
	var cmd *exec.Cmd
	cancel := context.CancelFunc(func() {})
	if timeoutMs > 0 {
		ctx, c := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
		cmd = exec.CommandContext(ctx, argv[0], argv[1:]...)
		cancel = c
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
	}
	if cwd != "" {
		if info, err := os.Stat(cwd); err == nil && info.IsDir() {
			cmd.Dir = cwd
		} else {
			cmd.Dir = s.root
		}
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(env)...)
	}
	return cmd, cancel
}

func envSlice(env map[string]string) []string {
	keys := slices.Sorted(maps.Keys(env))
	out := make([]string, 0, len(env))
	for _, k := range keys {
		out = append(out, k+"="+env[k])
	}
	return out
}

func (s *Server) handleExec(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	var body struct {
		Argv      []string          `json:"argv"`
		Cwd       string            `json:"cwd"`
		Env       map[string]string `json:"env"`
		Stdin     string            `json:"stdin"`
		TimeoutMs int64             `json:"timeout_ms"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cmd, cancel := s.buildCommand(body.Argv, body.Cwd, body.Env, body.TimeoutMs)
	if cmd == nil {
		writeErr(w, http.StatusBadRequest, "argv must not be empty")
		return
	}
	defer cancel()
	stdin, _ := base64.StdEncoding.DecodeString(body.Stdin)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	exitCode := 0
	if err := cmd.Run(); err != nil {
		exitCode = -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exit_code": exitCode,
		"stdout":    base64.StdEncoding.EncodeToString(stdout.Bytes()),
		"stderr":    base64.StdEncoding.EncodeToString(stderr.Bytes()),
	})
}

func (s *Server) handleStart(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	var body struct {
		Argv []string          `json:"argv"`
		Cwd  string            `json:"cwd"`
		Env  map[string]string `json:"env"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	cmd, cancel := s.buildCommand(body.Argv, body.Cwd, body.Env, 0)
	if cmd == nil {
		writeErr(w, http.StatusBadRequest, "argv must not be empty")
		return
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusInternalServerError, "start: "+err.Error())
		return
	}
	ses := &fakeSession{cmd: cmd, stdin: stdin}
	go copyEvents(ses, "stdout", stdout)
	go copyEvents(ses, "stderr", stderr)
	go func() {
		_ = cmd.Wait()
		cancel()
		ses.mu.Lock()
		ses.stdin = nil
		ses.done = true
		ses.exitCode = -1
		if cmd.ProcessState != nil {
			ses.exitCode = cmd.ProcessState.ExitCode()
		}
		ses.mu.Unlock()
	}()
	sbx.mu.Lock()
	sbx.nextSes++
	sid := "ses_" + strconv.FormatInt(sbx.nextSes, 10)
	sbx.sessions[sid] = ses
	sbx.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"session_id": sid})
}

func copyEvents(ses *fakeSession, stream string, rdr io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := rdr.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			ses.mu.Lock()
			ses.events = append(ses.events, fakeEvent{stream: stream, data: chunk, start: ses.total})
			ses.total += len(chunk)
			ses.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) handleOutput(w http.ResponseWriter, r *http.Request, ses *fakeSession) {
	offset := 0
	if v := r.URL.Query().Get("offset"); v != "" {
		offset, _ = strconv.Atoi(v)
	}
	type event struct {
		Stream string `json:"stream"`
		Data   string `json:"data"`
	}
	ses.mu.Lock()
	events := make([]event, 0, len(ses.events))
	for _, e := range ses.events {
		if e.start >= offset {
			events = append(events, event{Stream: e.stream, Data: base64.StdEncoding.EncodeToString(e.data)})
		}
	}
	out := struct {
		Events     []event `json:"events"`
		Done       bool    `json:"done"`
		ExitCode   int     `json:"exit_code"`
		NextOffset int     `json:"next_offset"`
	}{Events: events, Done: ses.done, ExitCode: ses.exitCode, NextOffset: ses.total}
	ses.mu.Unlock()
	if out.Events == nil {
		out.Events = []event{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleInput(w http.ResponseWriter, r *http.Request, ses *fakeSession) {
	var body struct {
		Data string `json:"data"`
		EOF  bool   `json:"eof"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.EOF {
		ses.mu.Lock()
		stdin := ses.stdin
		ses.mu.Unlock()
		if stdin != nil {
			_ = stdin.Close()
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Data)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "input data not base64")
		return
	}
	ses.mu.Lock()
	stdin := ses.stdin
	ses.mu.Unlock()
	if stdin != nil {
		_, _ = stdin.Write(data)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleKill(w http.ResponseWriter, _ *http.Request, ses *fakeSession) {
	_ = ses.kill()
	w.WriteHeader(http.StatusNoContent)
}

func (ses *fakeSession) kill() error {
	ses.mu.Lock()
	cmd := ses.cmd
	ses.mu.Unlock()
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

// --- file surface --------------------------------------------------------

func (s *Server) handleRead(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	p := r.URL.Query().Get("path")
	sbx.mu.Lock()
	f, ok := sbx.files[p]
	sbx.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "no such file "+p)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"content": base64.StdEncoding.EncodeToString(f.data)})
}

func (s *Server) handleWrite(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	var body struct {
		Path    string `json:"path"`
		Content string `json:"content"`
		Mode    int64  `json:"mode"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Path == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "content not base64")
		return
	}
	sbx.mu.Lock()
	sbx.files[body.Path] = fakeFile{data: data, mode: body.Mode, mtimeMs: time.Now().UnixMilli(), typeflag: tar.TypeReg}
	sbx.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	p := r.URL.Query().Get("path")
	sbx.mu.Lock()
	f, isFile := sbx.files[p]
	dirMode, isDir := sbx.dirs[p]
	sbx.mu.Unlock()
	switch {
	case isFile:
		writeJSON(w, http.StatusOK, map[string]any{
			"mode": f.mode, "size": len(f.data), "mtime_ms": f.mtimeMs, "is_dir": false,
		})
	case isDir:
		writeJSON(w, http.StatusOK, map[string]any{
			"mode": dirMode, "size": 0, "mtime_ms": 0, "is_dir": true,
		})
	default:
		writeErr(w, http.StatusNotFound, "no such path "+p)
	}
}

func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	var body struct {
		Path string `json:"path"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if body.Path == "" {
		writeErr(w, http.StatusBadRequest, "path is required")
		return
	}
	sbx.mu.Lock()
	markDirs(sbx.dirs, body.Path, 0o755)
	sbx.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// markDirs records path and every ancestor as a directory with the given
// mode (MkdirAll semantics; ancestors keep any mode already recorded).
func markDirs(dirs map[string]int64, p string, mode int64) {
	for {
		if _, ok := dirs[p]; !ok {
			dirs[p] = mode
		}
		parent := path.Dir(p)
		if parent == p || parent == "." || parent == "/" {
			return
		}
		p = parent
	}
}

func (s *Server) handleGlob(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	var body struct {
		Pattern string `json:"pattern"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	sbx.mu.Lock()
	matches := make([]string, 0)
	for _, key := range append(sbx.fileKeys(), sbx.dirKeys()...) {
		if ok, _ := path.Match(body.Pattern, key); ok {
			matches = append(matches, key)
		}
	}
	sbx.mu.Unlock()
	slices.Sort(matches)
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

func (sbx *fakeSandbox) fileKeys() []string {
	return slices.Sorted(maps.Keys(sbx.files))
}

func (sbx *fakeSandbox) dirKeys() []string {
	return slices.Sorted(maps.Keys(sbx.dirs))
}

func (s *Server) handleGrep(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	var body struct {
		Root       string `json:"root"`
		Pattern    string `json:"pattern"`
		MaxMatches int    `json:"max_matches"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	re, err := regexp.Compile(body.Pattern)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad pattern: "+err.Error())
		return
	}
	if body.MaxMatches <= 0 {
		body.MaxMatches = 200
	}
	type match struct {
		Path string `json:"path"`
		Line int    `json:"line"`
		Text string `json:"text"`
	}
	matches := make([]match, 0)
	sbx.mu.Lock()
	for _, key := range sbx.fileKeys() {
		f := sbx.files[key]
		if !strings.HasPrefix(key, body.Root+"/") {
			continue
		}
		if hiddenUnderRoot(key, body.Root) || f.typeflag == tar.TypeSymlink {
			continue
		}
		for i, text := range strings.Split(string(f.data), "\n") {
			if len(matches) >= body.MaxMatches {
				break
			}
			if re.MatchString(text) {
				matches = append(matches, match{Path: key, Line: i + 1, Text: base64.StdEncoding.EncodeToString([]byte(text))})
			}
		}
		if len(matches) >= body.MaxMatches {
			break
		}
	}
	sbx.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"matches": matches})
}

// hiddenUnderRoot reports whether any path segment below root starts with
// "." (the contract's hidden-directory skip; root itself is exempt).
func hiddenUnderRoot(p, root string) bool {
	rel := strings.TrimPrefix(p, root)
	rel = strings.TrimPrefix(rel, "/")
	for seg := range strings.SplitSeq(rel, "/") {
		if strings.HasPrefix(seg, ".") {
			return true
		}
	}
	return false
}

// --- workspace sync ------------------------------------------------------

// handleWorkspaceSync serves POST /sandbox/{id}/workspace/sync (CONTRACT.md
// §3.16): the real executor streams the container /workspace back to its
// host-side workspace directory; the fake has no host-side directory, so it
// records the call and answers 200 — enough to exercise the client shape.
func (s *Server) handleWorkspaceSync(w http.ResponseWriter, _ *http.Request, _ *fakeSandbox) {
	s.mu.Lock()
	s.syncs++
	s.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

// --- archive surface -----------------------------------------------------

func (s *Server) handlePutArchive(w http.ResponseWriter, r *http.Request, sbx *fakeSandbox) {
	zr, err := gzip.NewReader(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "archive is not gzip: "+err.Error())
		return
	}
	tr := tar.NewReader(zr)
	files := map[string]fakeFile{}
	dirs := map[string]int64{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, http.StatusBadRequest, "bad tar: "+err.Error())
			return
		}
		name := path.Clean(hdr.Name)
		if name == "." || name == ".." || strings.HasPrefix(name, "../") {
			continue
		}
		key := workspaceRoot + "/" + name
		switch hdr.Typeflag {
		case tar.TypeDir:
			markDirs(dirs, key, hdr.Mode)
		case tar.TypeSymlink:
			files[key] = fakeFile{data: []byte(hdr.Linkname), mode: hdr.Mode, mtimeMs: hdr.ModTime.UnixMilli(), typeflag: tar.TypeSymlink}
		default:
			data, err := io.ReadAll(tr)
			if err != nil {
				writeErr(w, http.StatusBadRequest, "bad tar entry: "+err.Error())
				return
			}
			files[key] = fakeFile{data: data, mode: hdr.Mode, mtimeMs: hdr.ModTime.UnixMilli(), typeflag: tar.TypeReg}
			markDirs(dirs, path.Dir(key), 0o755)
		}
	}
	sbx.mu.Lock()
	sbx.files = files
	sbx.dirs = dirs
	sbx.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetArchive(w http.ResponseWriter, _ *http.Request, sbx *fakeSandbox) {
	sbx.mu.Lock()
	files := sbx.files
	dirs := sbx.dirs
	sbx.mu.Unlock()

	w.Header().Set("Content-Type", "application/gzip")
	zw := gzip.NewWriter(w)
	tw := tar.NewWriter(zw)

	// Synthesize every parent directory of every entry so unpackers that
	// need them (and round-trip comparisons) see a complete tree.
	entryDirs := map[string]int64{}
	maps.Copy(entryDirs, dirs)
	for key := range files {
		for p := path.Dir(key); ; p = path.Dir(p) {
			if p == workspaceRoot || p == "/" || p == "." {
				break
			}
			if _, ok := entryDirs[p]; !ok {
				entryDirs[p] = 0o755
			}
		}
	}
	dirNames := make([]string, 0, len(entryDirs))
	for key := range entryDirs {
		if strings.HasPrefix(key, workspaceRoot+"/") {
			dirNames = append(dirNames, key)
		}
	}
	slices.Sort(dirNames)
	seen := map[string]bool{}
	for _, key := range dirNames {
		if seen[key] {
			continue
		}
		seen[key] = true
		name := strings.TrimPrefix(key, workspaceRoot+"/")
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: entryDirs[key], Typeflag: tar.TypeDir, ModTime: time.UnixMilli(0)})
	}
	for _, key := range slices.Sorted(maps.Keys(files)) {
		f := files[key]
		name := strings.TrimPrefix(key, workspaceRoot+"/")
		hdr := &tar.Header{
			Name:    name,
			Mode:    f.mode,
			ModTime: time.UnixMilli(f.mtimeMs),
		}
		if f.typeflag == tar.TypeSymlink {
			hdr.Typeflag = tar.TypeSymlink
			hdr.Linkname = string(f.data)
		} else {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(f.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(f.data); err != nil {
				return
			}
		}
	}
	_ = tw.Close()
	_ = zw.Close()
}

package executor

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"
)

// miniSandbox is a hand-rolled contract-v2 backend for the session
// endpoints only (create/start/output/input/kill): it really spawns the
// requested command with os/exec and serves its output via offset polling.
type miniSandbox struct {
	mu       sync.Mutex
	nextSes  int
	sessions map[string]*miniSession
}

type miniEvent struct {
	stream string
	data   []byte
	end    int // cumulative byte offset after this event
}

type miniSession struct {
	mu       sync.Mutex
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	events   []miniEvent
	total    int
	done     bool
	exitCode int
}

func newMiniSandbox(t *testing.T) *httptest.Server {
	t.Helper()
	m := &miniSandbox{sessions: map[string]*miniSession{}}
	mux := http.NewServeMux()
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("POST /sandbox/create", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, map[string]string{"sandbox_id": "sbx_mini"})
	})
	mux.HandleFunc("POST /sandbox/sbx_mini/start", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Argv []string          `json:"argv"`
			Env  map[string]string `json:"env"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.nextSes++
		sid := "ses_" + itoa(m.nextSes)
		m.mu.Unlock()
		ses, err := startMini(body.Argv, body.Env)
		if err != nil {
			http.Error(w, `{"error":"start failed"}`, 500)
			return
		}
		m.mu.Lock()
		m.sessions[sid] = ses
		m.mu.Unlock()
		writeJSON(w, map[string]string{"session_id": sid})
	})
	mux.HandleFunc("GET /sandbox/sbx_mini/sessions/{sid}/output", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		ses := m.sessions[r.PathValue("sid")]
		m.mu.Unlock()
		if ses == nil {
			http.Error(w, `{"error":"no session"}`, 404)
			return
		}
		offset := 0
		if v := r.URL.Query().Get("offset"); v != "" {
			offset, _ = strconv.Atoi(v)
		}
		type event struct {
			Stream string `json:"stream"`
			Data   string `json:"data"`
		}
		ses.mu.Lock()
		var events []event
		for _, e := range ses.events {
			if e.end-len(e.data) >= offset {
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
		writeJSON(w, out)
	})
	mux.HandleFunc("POST /sandbox/sbx_mini/sessions/{sid}/input", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		ses := m.sessions[r.PathValue("sid")]
		m.mu.Unlock()
		if ses == nil {
			http.Error(w, `{"error":"no session"}`, 404)
			return
		}
		var body struct {
			Data string `json:"data"`
			EOF  bool   `json:"eof"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.EOF {
			ses.closeStdin()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		data, _ := base64.StdEncoding.DecodeString(body.Data)
		_ = ses.writeStdin(data)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /sandbox/sbx_mini/sessions/{sid}/kill", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		ses := m.sessions[r.PathValue("sid")]
		m.mu.Unlock()
		if ses != nil {
			_ = ses.kill()
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /sandbox/sbx_mini", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *miniSession) writeStdin(data []byte) error {
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin == nil {
		return nil
	}
	_, err := stdin.Write(data)
	return err
}

func (s *miniSession) closeStdin() {
	s.mu.Lock()
	stdin := s.stdin
	s.mu.Unlock()
	if stdin != nil {
		_ = stdin.Close()
	}
}

func (s *miniSession) kill() error {
	s.mu.Lock()
	cmd := s.cmd
	s.mu.Unlock()
	if cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func (s *miniSession) addEvent(stream string, data []byte) {
	s.mu.Lock()
	s.total += len(data)
	s.events = append(s.events, miniEvent{stream: stream, data: data, end: s.total})
	s.mu.Unlock()
}

func startMini(argv []string, env map[string]string) (*miniSession, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), envSlice(env)...)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	ses := &miniSession{cmd: cmd, stdin: stdin}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		_, _ = io.Copy(writerFunc(func(p []byte) (int, error) {
			ses.addEvent("stdout", append([]byte(nil), p...))
			return len(p), nil
		}), stdout)
	}()
	go func() {
		_, _ = io.Copy(writerFunc(func(p []byte) (int, error) {
			ses.addEvent("stderr", append([]byte(nil), p...))
			return len(p), nil
		}), stderr)
	}()
	go func() {
		err := cmd.Wait()
		ses.mu.Lock()
		ses.stdin = nil
		ses.done = true
		ses.exitCode = -1
		if cmd.ProcessState != nil {
			ses.exitCode = cmd.ProcessState.ExitCode()
		}
		ses.mu.Unlock()
		_ = err
	}()
	return ses, nil
}

type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func envSlice(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func itoa(n int) string { return strconv.Itoa(n) }

func newMiniExecutor(t *testing.T) *Sandbox {
	t.Helper()
	srv := newMiniSandbox(t)
	sb, err := NewSandbox(SandboxConfig{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func TestSandboxStartCatRoundTrip(t *testing.T) {
	sb := newMiniExecutor(t)
	p, err := sb.Start(t.Context(), ExecRequest{Name: "cat"}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Stdin().Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := p.Stdin().Close(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hi" {
		t.Fatalf("stdout = %q, want %q", got, "hi")
	}
	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestSandboxStartExitError(t *testing.T) {
	sb := newMiniExecutor(t)
	p, err := sb.Start(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "exit 3"}}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	err = p.Wait()
	if exitErr, ok := errors.AsType[*ExitError](err); !ok || exitErr.Code != 3 {
		t.Fatalf("Wait err = %v, want *ExitError{3}", err)
	}
}

func TestSandboxStartKill(t *testing.T) {
	sb := newMiniExecutor(t)
	p, err := sb.Start(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "sleep 60"}}, StartOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()
	select {
	case <-waited:
		// Kill-triggered exit; error value is implementation detail.
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
}

func TestSandboxStartRedirectedStdout(t *testing.T) {
	sb := newMiniExecutor(t)
	var buf safeBuffer
	p, err := sb.Start(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "echo redirected"}}, StartOptions{Stdout: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if p.Stdout() != nil {
		t.Fatal("Stdout() must be nil when StartOptions.Stdout redirected")
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for buf.Len() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := buf.String(); got != "redirected\n" {
		t.Fatalf("redirected stdout = %q", got)
	}
}

type safeBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *safeBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.buf)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

func TestSandboxStartStderrSink(t *testing.T) {
	sb := newMiniExecutor(t)
	var errBuf safeBuffer
	p, err := sb.Start(t.Context(), ExecRequest{Name: "sh", Args: []string{"-c", "echo errline >&2; echo outline"}}, StartOptions{Stderr: &errBuf})
	if err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(p.Stdout())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	if string(out) != "outline\n" {
		t.Fatalf("stdout = %q", out)
	}
	if errBuf.String() != "errline\n" {
		t.Fatalf("stderr = %q", errBuf.String())
	}
}

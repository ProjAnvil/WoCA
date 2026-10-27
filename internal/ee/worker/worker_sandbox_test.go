package worker

// Per-run sandbox execution (design §4.5, spec §5.3): when the materialized
// config carries an enabled backend, the worker provisions a sandbox and
// executes the run through the run-executor injection. There is no transfer
// step anywhere — the sandbox bind-mounts its workspace directory and works
// on it in place, so the wire must show zero archive uploads, zero archive
// downloads, and zero sync calls for every backend type. Failures fail the
// run explicitly — no silent fallback to local execution.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// seedSandboxBackend creates the materialized sandbox_backends table and
// one enabled row pointing at endpoint.
func seedSandboxBackend(t *testing.T, db *wocadb.DB, backendType, endpoint string) {
	t.Helper()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS sandbox_backends (
		id TEXT PRIMARY KEY, name TEXT NOT NULL UNIQUE,
		type TEXT NOT NULL DEFAULT 'contract-http',
		endpoint TEXT NOT NULL, timeout_ms INTEGER NOT NULL DEFAULT 120000,
		enabled INTEGER NOT NULL DEFAULT 0,
		last_probe_at INTEGER, last_probe_ok INTEGER, last_probe_error TEXT,
		options TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO sandbox_backends
		(id, name, type, endpoint, timeout_ms, enabled, created_at, updated_at)
		VALUES ('sb1', 'fake', ?, ?, 120000, 1, ?, ?)`,
		backendType, endpoint, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// newSandboxWorker builds a worker whose DB carries an enabled backend at
// endpoint and whose agent replays the scripted model responses.
func newSandboxWorker(t *testing.T, backendType, endpoint string, replies ...messages.Message) (*Worker, *runstate.Ledger) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedSandboxBackend(t, db, backendType, endpoint)
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model: language.NewFakeChatModel(
			language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
			language.WithResponses(replies...),
		),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{
		Ledger:    ledger,
		Agent:     a,
		Logger:    zerolog.Nop(),
		ID:        "worker-sbx",
		DB:        db,
		SecretFor: func(id string) string { return "secret-for-" + id },
	})
	return w, ledger
}

func enqueueWorkspaceRun(t *testing.T, ledger *runstate.Ledger, runID, prompt, workspace string) {
	t.Helper()
	snap, err := runstate.EncodeSnapshot(runstate.RunSnapshot{
		Prompt: prompt, SessionID: "s-" + runID, Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Enqueue(t.Context(), runstate.Run{
		RunID: runID, SessionID: "s-" + runID, ThreadID: "th-" + runID,
		ConfigSnapshot: snap,
	}); err != nil {
		t.Fatal(err)
	}
}

// enqueueWorkspaceIDRun enqueues a run carrying an execution-side sandbox
// workspace identity (Task 14's runstate channel) — the EE routing path.
func enqueueWorkspaceIDRun(t *testing.T, ledger *runstate.Ledger, runID, prompt, workspace, workspaceID string) {
	t.Helper()
	snap, err := runstate.EncodeSnapshot(runstate.RunSnapshot{
		Prompt: prompt, SessionID: "s-" + runID, Workspace: workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Enqueue(t.Context(), runstate.Run{
		RunID: runID, SessionID: "s-" + runID, ThreadID: "th-" + runID,
		ConfigSnapshot: snap, WorkspaceID: workspaceID,
	}); err != nil {
		t.Fatal(err)
	}
}

func runStatus(t *testing.T, ledger *runstate.Ledger, runID string) (string, []runstate.Event) {
	t.Helper()
	row, ok, err := ledger.Row(t.Context(), runID)
	if err != nil || !ok {
		t.Fatalf("row = ok=%v err=%v", ok, err)
	}
	events, err := ledger.Replay(t.Context(), runID, 0, 1000)
	if err != nil {
		t.Fatal(err)
	}
	return row.Status, events
}

// recordingSandbox delegates to the fakesandbox handler while recording
// the wire traffic the zero-transfer assertions need: create bodies,
// archive uploads/downloads, workspace syncs, and destroys.
type recordingSandbox struct {
	inner http.Handler

	mu       sync.Mutex
	creates  []string
	putArch  int
	getArch  int
	syncs    int
	destroys int
}

func (s *recordingSandbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sandbox/create":
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(b)))
		s.mu.Lock()
		s.creates = append(s.creates, string(b))
		s.mu.Unlock()
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/archive"):
		s.mu.Lock()
		s.putArch++
		s.mu.Unlock()
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/archive"):
		s.mu.Lock()
		s.getArch++
		s.mu.Unlock()
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/workspace/sync"):
		s.mu.Lock()
		s.syncs++
		s.mu.Unlock()
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/sandbox/"):
		s.mu.Lock()
		s.destroys++
		s.mu.Unlock()
	}
	s.inner.ServeHTTP(w, r)
}

func (s *recordingSandbox) stats() (creates []string, putArch, getArch, syncs, destroys int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.creates...), s.putArch, s.getArch, s.syncs, s.destroys
}

// assertZeroTransfer fails the test when any transfer call was recorded.
func assertZeroTransfer(t *testing.T, putArch, getArch, syncs int) {
	t.Helper()
	if putArch != 0 {
		t.Errorf("archive uploads = %d, want 0 (woca does not transfer workspaces)", putArch)
	}
	if getArch != 0 {
		t.Errorf("archive downloads = %d, want 0 (woca does not transfer workspaces)", getArch)
	}
	if syncs != 0 {
		t.Errorf("workspace syncs = %d, want 0 (the sync surface is gone)", syncs)
	}
}

// A run over an enabled backend executes in the sandbox and moves no
// files: no archive round trip, no sync — the ruling (spec §5.3) applies
// to every mode.
func TestWorkerSandboxRunMovesNoFiles(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "in.txt"), []byte("host-side"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, ledger := newSandboxWorker(t, "contract-http", srv.URL,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "Write", Args: map[string]any{"file_path": "out.txt", "content": "sandbox-side"}},
		}},
		messages.AI("done"),
	)
	enqueueWorkspaceRun(t, ledger, "r1", "write the file", ws)

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, events := runStatus(t, ledger, "r1")
	if status != "done" {
		for _, ev := range events {
			t.Logf("event %s: %s", ev.Kind, ev.Payload)
		}
		t.Fatalf("status = %q, want done", status)
	}
	creates, putArch, getArch, syncs, destroys := rec.stats()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	assertZeroTransfer(t, putArch, getArch, syncs)
	if destroys != 1 {
		t.Fatalf("destroy calls = %d, want 1 (teardown destroys the sandbox)", destroys)
	}
	// The host workspace is never the transfer target anymore: whatever
	// the run wrote stayed in the sandbox.
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("host workspace was written outside the sandbox mount (err=%v)", err)
	}
}

// EE workspace mode: a run carrying a workspace identity routes the
// sandbox create under it (the workspace field rides the create body via
// providers.Config) and still moves no files — zero archives, zero syncs,
// teardown destroys.
func TestWorkerSandboxWorkspaceRunZeroTransfer(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "in.txt"), []byte("host-side"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, ledger := newSandboxWorker(t, "contract-http", srv.URL,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "Write", Args: map[string]any{"file_path": "out.txt", "content": "sandbox-side"}},
		}},
		messages.AI("done"),
	)
	enqueueWorkspaceIDRun(t, ledger, "r2", "write the file", ws, "ws_ee1")

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r2")
	if status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
	creates, putArch, getArch, syncs, destroys := rec.stats()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	var createBody map[string]any
	if err := json.Unmarshal([]byte(creates[0]), &createBody); err != nil {
		t.Fatalf("create body %q: %v", creates[0], err)
	}
	if got, _ := createBody["workspace"].(string); got != "ws_ee1" {
		t.Fatalf("create body workspace = %q, want ws_ee1 (body %s)", got, creates[0])
	}
	assertZeroTransfer(t, putArch, getArch, syncs)
	if destroys != 1 {
		t.Fatalf("destroy calls = %d, want 1 (teardown destroys the sandbox)", destroys)
	}
}

// A broken backend fails the run — the worker never falls back to local
// execution.
func TestWorkerSandboxBrokenBackendFailsRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	w, ledger := newSandboxWorker(t, "contract-http", srv.URL, messages.AI("done"))
	enqueueWorkspaceRun(t, ledger, "r3", "unreachable backend", ws)

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r3")
	if status != "error" {
		t.Fatalf("status = %q, want error (broken backend must fail the run)", status)
	}
	// The prompt never executed locally: no side effects, and the ledger
	// shows the failure instead of a quiet local run.
	if entries, _ := os.ReadDir(ws); len(entries) != 0 {
		t.Fatalf("host workspace mutated: %v", entries)
	}
}

// No enabled backend (table present, no rows) keeps the pre-sandbox
// behavior: the run executes locally and finishes done.
func TestWorkerNoBackendRunsLocally(t *testing.T) {
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedSandboxBackend(t, db, "contract-http", "http://unused.example") // creates the table
	if _, err := db.Exec(`DELETE FROM sandbox_backends`); err != nil {
		t.Fatal(err)
	}
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("local reply"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{Ledger: ledger, Agent: a, Logger: zerolog.Nop(), ID: "w-local", DB: db})
	enqueueWorkspaceRun(t, ledger, "r4", "plain run", t.TempDir())

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r4")
	if status != "done" {
		t.Fatalf("status = %q, want done (no backend = local execution)", status)
	}
}

// provisionRunSandbox threads the run's workspace identity through
// providers.Config into the contract-http create body (Task 15 wiring)
// and does nothing else on the wire: no archives, no sync, one destroy
// on teardown.
func TestProvisionRunSandboxPassesWorkspaceToCreate(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	w, _ := newSandboxWorker(t, "contract-http", srv.URL)
	ctx := context.Background()
	teardown, err := w.provisionRunSandbox(&ctx, "ws_direct", runstate.RunSnapshot{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("provisionRunSandbox: %v", err)
	}
	if teardown == nil {
		t.Fatal("provision must return a teardown")
	}
	creates, putArch, getArch, syncs, destroys := rec.stats()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	var createBody map[string]any
	if err := json.Unmarshal([]byte(creates[0]), &createBody); err != nil {
		t.Fatalf("create body %q: %v", creates[0], err)
	}
	if got, _ := createBody["workspace"].(string); got != "ws_direct" {
		t.Fatalf("create body workspace = %q, want ws_direct", got)
	}
	assertZeroTransfer(t, putArch, getArch, syncs)
	if destroys != 0 {
		t.Fatalf("destroy calls before teardown = %d, want 0", destroys)
	}
	teardown()
	if _, _, _, _, destroys := rec.stats(); destroys != 1 {
		t.Fatalf("destroy calls after teardown = %d, want 1", destroys)
	}
}

// fakeBridge is the minimal Cloudflare-bridge wire fake: it provisions
// sandboxes, counts every hydrate/persist (archive) call, and destroys.
type fakeBridge struct {
	mu       sync.Mutex
	creates  int
	hydrates int
	persists int
	destroys int
}

func (f *fakeBridge) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/sandbox":
			f.creates++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sbx_cf_1"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/hydrate"):
			f.hydrates++
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/persist"):
			f.persists++
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sandbox/"):
			f.destroys++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	})
}

func (f *fakeBridge) stats() (creates, hydrates, persists, destroys int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creates, f.hydrates, f.persists, f.destroys
}

// The archive-only backend (cfbridge — the one that used to fall back to
// client-side archives) goes through the same mount-only provision: zero
// hydrate (upload), zero persist (download), one destroy on teardown. The
// former "falling back to client-side archives" path is gone for good.
func TestProvisionRunSandboxCfbridgeZeroTransfer(t *testing.T) {
	fb := &fakeBridge{}
	srv := httptest.NewServer(fb.handler())
	t.Cleanup(srv.Close)

	w, _ := newSandboxWorker(t, "cfbridge", srv.URL)
	ctx := context.Background()
	teardown, err := w.provisionRunSandbox(&ctx, "ws_cf", runstate.RunSnapshot{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("provisionRunSandbox (cfbridge): %v", err)
	}
	if teardown == nil {
		t.Fatal("provision must return a teardown")
	}
	creates, hydrates, persists, destroys := fb.stats()
	if creates != 1 {
		t.Fatalf("bridge creates = %d, want 1", creates)
	}
	if hydrates != 0 {
		t.Fatalf("bridge hydrate (upload) calls = %d, want 0 (no client-side archive fallback)", hydrates)
	}
	if persists != 0 {
		t.Fatalf("bridge persist (download) calls = %d, want 0 (no client-side archive fallback)", persists)
	}
	if destroys != 0 {
		t.Fatalf("bridge destroys before teardown = %d, want 0", destroys)
	}
	teardown()
	if _, _, _, destroys := fb.stats(); destroys != 1 {
		t.Fatalf("bridge destroys after teardown = %d, want 1", destroys)
	}
}

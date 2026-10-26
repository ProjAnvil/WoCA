package worker

// Per-run sandbox execution (design §4.5): when the materialized config
// carries an enabled backend, the worker provisions a sandbox, uploads the
// run workspace, executes the run through the run-executor injection, and
// downloads the products back. Every step fails the run explicitly — no
// silent fallback to local execution.

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/projanvil/woca/internal/platform/executor"
	"github.com/projanvil/woca/internal/platform/executor/fakesandbox"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// seedSandboxBackend creates the materialized sandbox_backends table and
// one enabled row pointing at endpoint.
func seedSandboxBackend(t *testing.T, db *wocadb.DB, endpoint string) {
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
		VALUES ('sb1', 'fake', 'contract-http', ?, 120000, 1, ?, ?)`,
		endpoint, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

// newSandboxWorker builds a worker whose DB carries an enabled backend at
// endpoint and whose agent replays the scripted model responses.
func newSandboxWorker(t *testing.T, endpoint string, maxBytes int64, replies ...messages.Message) (*Worker, *runstate.Ledger) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	seedSandboxBackend(t, db, endpoint)
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
		Ledger:          ledger,
		Agent:           a,
		Logger:          zerolog.Nop(),
		ID:              "worker-sbx",
		DB:              db,
		MaxArchiveBytes: maxBytes,
		SecretFor:       func(id string) string { return "secret-for-" + id },
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

// recordingSandbox delegates to the fakesandbox handler while recording
// the wire traffic the workspace-mode assertions need: create bodies,
// client-side archive uploads (PUT .../archive), and workspace syncs
// (POST .../workspace/sync). syncFails turns the sync endpoint into a 500.
type recordingSandbox struct {
	inner     http.Handler
	syncFails bool

	mu      sync.Mutex
	creates []string
	putArch int
	syncs   int
}

func (s *recordingSandbox) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/sandbox/create":
		b, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(b))
		s.mu.Lock()
		s.creates = append(s.creates, string(b))
		s.mu.Unlock()
	case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/archive"):
		s.mu.Lock()
		s.putArch++
		s.mu.Unlock()
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/workspace/sync"):
		s.mu.Lock()
		s.syncs++
		fail := s.syncFails
		s.mu.Unlock()
		if fail {
			http.Error(w, "sync failed", http.StatusInternalServerError)
			return
		}
	}
	s.inner.ServeHTTP(w, r)
}

func (s *recordingSandbox) stats() (creates []string, putArch, syncs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.creates...), s.putArch, s.syncs
}

// A run over an enabled backend writes its products in the sandbox and
// the download returns them to the host workspace.
func TestWorkerSandboxRoundTrip(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "in.txt"), []byte("host-side"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, ledger := newSandboxWorker(t, fakesandbox.Addr(t), 8<<20,
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
	data, err := os.ReadFile(filepath.Join(ws, "out.txt"))
	if err != nil {
		t.Fatalf("product not downloaded back to the host workspace: %v", err)
	}
	if string(data) != "sandbox-side" {
		t.Fatalf("out.txt = %q", data)
	}
	// The host's .git exclusion keeps the host tree intact (nothing was
	// clobbered); the pre-existing file survives the download.
	if _, err := os.Stat(filepath.Join(ws, "in.txt")); err != nil {
		t.Fatalf("pre-existing host file lost: %v", err)
	}
}

// An upload past the archive cap fails the run instead of silently
// executing locally.
func TestWorkerSandboxUploadOverflowFailsRun(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "big.bin"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	w, ledger := newSandboxWorker(t, fakesandbox.Addr(t), 4<<10, messages.AI("done"))
	enqueueWorkspaceRun(t, ledger, "r2", "big workspace", ws)

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, events := runStatus(t, ledger, "r2")
	if status != "error" {
		t.Fatalf("status = %q, want error (upload overflow must fail the run)", status)
	}
	var sawSandboxErr bool
	for _, ev := range events {
		if strings.Contains(ev.Payload, "sandbox") {
			sawSandboxErr = true
		}
	}
	if !sawSandboxErr {
		t.Fatalf("no sandbox error event: %+v", events)
	}
}

// A download past the archive cap fails the run (the products cannot be
// recovered; the run must not report success).
func TestWorkerSandboxDownloadOverflowFailsRun(t *testing.T) {
	ws := t.TempDir()
	w, ledger := newSandboxWorker(t, fakesandbox.Addr(t), 4<<10,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "Write", Args: map[string]any{
				"file_path": "huge.txt", "content": strings.Repeat("x", 64<<10),
			}},
		}},
		messages.AI("done"),
	)
	enqueueWorkspaceRun(t, ledger, "r3", "write a huge file", ws)

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r3")
	if status != "error" {
		t.Fatalf("status = %q, want error (download overflow must fail the run)", status)
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
	w, ledger := newSandboxWorker(t, srv.URL, 8<<20, messages.AI("done"))
	enqueueWorkspaceRun(t, ledger, "r4", "unreachable backend", ws)

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r4")
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
	seedSandboxBackend(t, db, "http://unused.example") // creates the table
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
	enqueueWorkspaceRun(t, ledger, "r5", "plain run", t.TempDir())

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r5")
	if status != "done" {
		t.Fatalf("status = %q, want done (no backend = local execution)", status)
	}
}

var _ = fmt.Sprintf

// EE workspace mode: a run carrying a workspace identity routes the
// sandbox create under it (the workspace field rides the create body via
// providers.Config), never uploads client-side (the executor seeds the
// workspace during create, spec §4), and confirms the products with
// exactly one workspace sync.
func TestWorkerSandboxWorkspaceRunSyncsInsteadOfUploading(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "in.txt"), []byte("host-side"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, ledger := newSandboxWorker(t, srv.URL, 8<<20,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "Write", Args: map[string]any{"file_path": "out.txt", "content": "sandbox-side"}},
		}},
		messages.AI("done"),
	)
	enqueueWorkspaceIDRun(t, ledger, "r6", "write the file", ws, "ws_ee1")

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r6")
	if status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
	creates, putArch, syncs := rec.stats()
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
	if putArch != 0 {
		t.Fatalf("client-side archive uploads = %d, want 0 (workspace mode uploads inside create)", putArch)
	}
	if syncs != 1 {
		t.Fatalf("workspace syncs = %d, want exactly 1", syncs)
	}
}

// A failing workspace sync fails the run: the products were never
// confirmed durably executor-side, so "done" would lie. The error must
// name the sync step.
func TestWorkerSandboxSyncFailureFailsRun(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler(), syncFails: true}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	w, ledger := newSandboxWorker(t, srv.URL, 8<<20,
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{ID: "1", Name: "Write", Args: map[string]any{"file_path": "out.txt", "content": "x"}},
		}},
		messages.AI("done"),
	)
	enqueueWorkspaceIDRun(t, ledger, "r7", "write the file", ws, "ws_ee2")

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, events := runStatus(t, ledger, "r7")
	if status != "error" {
		t.Fatalf("status = %q, want error (failed sync must fail the run)", status)
	}
	var sawSyncErr bool
	for _, ev := range events {
		if ev.Kind == "error" && strings.Contains(ev.Payload, "sync") {
			sawSyncErr = true
		}
	}
	if !sawSyncErr {
		t.Fatalf("no sync error event: %+v", events)
	}
}

// provisionRunSandbox threads the run's workspace identity through
// providers.Config into the contract-http create body (Task 15 wiring).
func TestProvisionRunSandboxPassesWorkspaceToCreate(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	w, _ := newSandboxWorker(t, srv.URL, 8<<10)
	ctx := context.Background()
	dl, teardown, err := w.provisionRunSandbox(&ctx, "ws_direct", runstate.RunSnapshot{Workspace: t.TempDir()})
	if err != nil {
		t.Fatalf("provisionRunSandbox: %v", err)
	}
	if dl == nil || teardown == nil {
		t.Fatal("workspace-mode provision must return download and teardown")
	}
	defer teardown()
	if err := dl(true); err != nil {
		t.Fatalf("download(true) = %v, want nil (fake sync succeeds)", err)
	}
	creates, putArch, syncs := rec.stats()
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
	if putArch != 0 {
		t.Fatalf("client-side archive uploads = %d, want 0", putArch)
	}
	if syncs != 1 {
		t.Fatalf("workspace syncs = %d, want 1", syncs)
	}
}

// A run without a workspace identity (personal edition / transition) keeps
// the pre-EE client-side archive round trip: the create body carries no
// workspace field, the workspace is uploaded once, and no sync happens.
func TestWorkerSandboxLocalPathStillArchives(t *testing.T) {
	rec := &recordingSandbox{inner: fakesandbox.New().Handler()}
	srv := httptest.NewServer(rec)
	t.Cleanup(srv.Close)

	ws := t.TempDir()
	w, ledger := newSandboxWorker(t, srv.URL, 8<<20, messages.AI("done"))
	enqueueWorkspaceRun(t, ledger, "r8", "plain run", ws)

	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, _ := runStatus(t, ledger, "r8")
	if status != "done" {
		t.Fatalf("status = %q, want done", status)
	}
	creates, putArch, syncs := rec.stats()
	if len(creates) != 1 {
		t.Fatalf("create calls = %d, want 1", len(creates))
	}
	var createBody map[string]any
	if err := json.Unmarshal([]byte(creates[0]), &createBody); err != nil {
		t.Fatalf("create body %q: %v", creates[0], err)
	}
	if _, ok := createBody["workspace"]; ok {
		t.Fatalf("create body %s carries workspace, want none on the local path", creates[0])
	}
	if putArch != 1 {
		t.Fatalf("client-side archive uploads = %d, want 1 (local path unchanged)", putArch)
	}
	if syncs != 0 {
		t.Fatalf("workspace syncs = %d, want 0 on the local path", syncs)
	}
}

// Terminal-review hardening: the download must not reintroduce .git
// (host-owned, excluded on upload) or write symlinks whose targets escape
// the workspace — a hostile backend could otherwise plant git hooks on the
// host workspace via run products.
func TestDownloadSkipsGitAndEscapingSymlinks(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "normal.txt", Mode: 0o644, Size: 3}))
	_, err := tw.Write([]byte("ok\n"))
	must(err)
	must(tw.WriteHeader(&tar.Header{Name: ".git/hooks/pre-commit", Mode: 0o755, Size: 5}))
	_, err = tw.Write([]byte("evil\n"))
	must(err)
	must(tw.WriteHeader(&tar.Header{Name: ".git", Typeflag: tar.TypeDir, Mode: 0o755}))
	must(tw.WriteHeader(&tar.Header{Name: "lnk-abs", Typeflag: tar.TypeSymlink, Linkname: "/etc/passwd"}))
	must(tw.WriteHeader(&tar.Header{Name: "lnk-esc", Typeflag: tar.TypeSymlink, Linkname: "../../escape"}))
	must(tw.Close())
	must(gz.Close())

	srv := httptest.NewServer(fakesandbox.New().Handler())
	t.Cleanup(srv.Close)
	sbx, err := executor.NewSandbox(executor.SandboxConfig{Endpoint: srv.URL, Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sbx.Close() })
	if err := sbx.PutArchive(t.Context(), bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatal(err)
	}

	ws := t.TempDir()
	if err := downloadWorkspace(t.Context(), sbx, ws, 8<<20); err != nil {
		t.Fatal(err)
	}

	if b, err := os.ReadFile(filepath.Join(ws, "normal.txt")); err != nil || string(b) != "ok\n" {
		t.Fatalf("normal.txt = %q err=%v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(ws, ".git")); !os.IsNotExist(err) {
		t.Fatalf(".git leaked into host workspace (err=%v)", err)
	}
	for _, l := range []string{"lnk-abs", "lnk-esc"} {
		if _, err := os.Lstat(filepath.Join(ws, l)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked into host workspace", l)
		}
	}
}

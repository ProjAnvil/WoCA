package worker

// Dispatch end-to-end across both roles: a run created on the server
// (gateway posture, DispatchToQueue) lands in the shared queue, a worker
// (separate process role, same database) claims and executes it, and a
// client attached to the server follows the whole stream over ledger SSE.
// This is the Phase 3 wiring acceptance shape; the server-side queue
// semantics live in internal/server's dispatch tests.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/server"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"
)

func e2eLedger(t *testing.T) *runstate.Ledger {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	l, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func e2eAgent(t *testing.T, reply string) *agent.Agent {
	t.Helper()
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI(reply))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// sseEvents reads the SSE stream and returns the event names up to and
// including the terminal frame.
func sseEvents(t *testing.T, url string) []string {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE status = %d", resp.StatusCode)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		rest, ok := strings.CutPrefix(line, "event:")
		if !ok {
			continue
		}
		ev := strings.TrimSpace(rest)
		events = append(events, ev)
		if ev == "done" {
			break
		}
	}
	return events
}

func TestDispatchServerWorkerEndToEnd(t *testing.T) {
	ledger := e2eLedger(t)
	s := server.New(server.Config{
		Agent: e2eAgent(t, "x"), Static: fstest.MapFS{}, Logger: zerolog.Nop(),
		SessionDir: t.TempDir(), WorkDir: t.TempDir(),
		RunLedger: ledger, DispatchToQueue: true,
	})
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)

	body, _ := json.Marshal(map[string]any{"session_id": "s-e2e", "prompt": "run on the worker"})
	resp, err := http.Post(srv.URL+"/api/runs", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		RunID string `json:"run_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if out.RunID == "" {
		t.Fatal("no run id")
	}

	bus := eventbus.NewLocalWithRetention(16)
	t.Cleanup(func() { _ = bus.Close() })
	w := New(Config{
		Ledger: ledger, Agent: e2eAgent(t, "worker executed this"),
		Bus: bus, Logger: zerolog.Nop(), ID: "w-e2e",
	})
	go func() { _ = w.Step(t.Context()) }()

	events := sseEvents(t, srv.URL+"/api/runs/"+out.RunID+"/events")
	var delta, done bool
	for _, ev := range events {
		switch ev {
		case "delta":
			delta = true
		case "done":
			done = true
		}
	}
	if !delta || !done {
		t.Fatalf("events = delta:%v done:%v (%v)", delta, done, events)
	}
	row, _, _ := ledger.Row(t.Context(), out.RunID)
	if row.Status != "done" || row.Owner != "w-e2e" {
		t.Fatalf("terminal row = %+v", row)
	}
}

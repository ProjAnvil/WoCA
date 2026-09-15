package worker

// Dispatch-path tests: the snapshot carries the execution environment
// (workspace, session transcript, attachments) and the session state rows
// track the run's lifetime from claim to finish.

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/projanvil/woca/internal/tools"
	"github.com/projanvil/woca/internal/wocadb"

	"github.com/projanvil/langchain-golang/core/language"
)

// memSessions is an in-memory worker.Sessions.
type memSessions struct {
	saved  map[string][]messages.Message
	loaded map[string][]messages.Message
}

func (m *memSessions) Load(id string) []messages.Message { return m.loaded[id] }
func (m *memSessions) Save(id string, msgs []messages.Message, _ string) error {
	m.saved[id] = msgs
	return nil
}

// memStates records the mark/clear calls in order.
type memStates struct {
	marks  []string // "running:runID" entries
	clears []string
}

func (m *memStates) MarkRunning(sessionID, runID string) { m.marks = append(m.marks, runID) }
func (m *memStates) Clear(sessionID, runID string)       { m.clears = append(m.clears, runID) }

func newDispatchFixture(t *testing.T, sessions *memSessions, states *memStates) (*Worker, *runstate.Ledger) {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ledger, err := runstate.New(db)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(agent.Config{
		Model:          language.NewFakeChatModel(language.WithResponses(messages.AI("dispatch reply"))),
		Registry:       tools.NewRegistry(t.TempDir()),
		SystemPrompt:   "test",
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	w := New(Config{
		Ledger: ledger, Agent: a, Bus: eventbus.NewLocalWithRetention(16),
		Logger: zerolog.Nop(), ID: "w-dispatch", Sessions: sessions, States: states,
	})
	return w, ledger
}

// A dispatched run continues its session's history and persists the final
// transcript back through the Sessions seam.
func TestWorkerDispatchContinuesAndSavesSession(t *testing.T) {
	sessions := &memSessions{
		loaded: map[string][]messages.Message{
			"s1": {{Role: messages.RoleHuman, Content: "earlier"}, {Role: messages.RoleAI, Content: "answer"}},
		},
		saved: map[string][]messages.Message{},
	}
	states := &memStates{}
	w, ledger := newDispatchFixture(t, sessions, states)
	snap, _ := runstate.EncodeSnapshot(runstate.RunSnapshot{
		Prompt: "next question", SessionID: "s1", Workspace: t.TempDir(), ThreadID: "th-1",
	})
	if err := ledger.Enqueue(t.Context(), runstate.Run{
		RunID: "r1", SessionID: "s1", ThreadID: "th-1", ConfigSnapshot: snap,
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	saved := sessions.saved["s1"]
	if len(saved) == 0 {
		t.Fatal("final transcript not saved")
	}
	last := saved[len(saved)-1]
	if last.Role != messages.RoleAI || last.Content != "dispatch reply" {
		t.Fatalf("last saved = %+v", last)
	}
	if len(states.marks) != 1 || states.marks[0] != "r1" || len(states.clears) != 1 {
		t.Fatalf("states = marks %v clears %v", states.marks, states.clears)
	}
}

// A run dispatched with attachments rebuilds the multimodal prompt message
// from the snapshot.
func TestWorkerDispatchRebuildsAttachments(t *testing.T) {
	snap := runstate.RunSnapshot{
		Prompt: "look", SessionID: "s", ThreadID: "th",
		Attachments: []runstate.Attachment{{Name: "a.png", MimeType: "image/png", Data: "aGk="}},
	}
	blob, err := runstate.EncodeSnapshot(snap)
	if err != nil {
		t.Fatal(err)
	}
	// Round-trip through JSON so the block types match what a real dispatch
	// produces.
	parsed, err := runstate.ParseSnapshot(blob)
	if err != nil {
		t.Fatal(err)
	}
	msg := humanMessage(parsed)
	if msg.Role != messages.RoleHuman || len(msg.ContentBlocks) != 2 {
		b, _ := json.Marshal(msg)
		t.Fatalf("message = %s", b)
	}
}

// An unreadable snapshot fails the run with an error event instead of
// executing or crashing the worker.
func TestWorkerUnreadableSnapshotFailsRun(t *testing.T) {
	states := &memStates{}
	w, ledger := newDispatchFixture(t, &memSessions{}, states)
	if err := ledger.Enqueue(t.Context(), runstate.Run{
		RunID: "r-bad", SessionID: "s-bad", ThreadID: "th-bad", ConfigSnapshot: "{not json",
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	row, _, _ := ledger.Row(t.Context(), "r-bad")
	if row.Status != "error" {
		t.Fatalf("status = %q want error", row.Status)
	}
	if len(states.marks) != 0 {
		t.Fatalf("no state marks expected on a bad snapshot, got %v", states.marks)
	}
}

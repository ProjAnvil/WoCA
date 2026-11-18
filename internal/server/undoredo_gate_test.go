package server

// T7 admission-gate bypass: /undo and /redo command runs used to execute and
// full-overwrite-save the session transcript OUTSIDE the session admission
// gate. When a gated model run was in flight on the same session, the two
// full-overwrite saves raced last-writer-wins and silently dropped one side's
// messages. The regression tests here drive the command run through the same
// HTTP surface a client uses (POST /api/runs with prompt "/undo") while a
// holder run is mid-flight, and require BOTH turns to persist.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/rs/zerolog"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// newGatedUndoServer builds an in-process server whose scripted model first
// writes a file (recording file history) then replies, with each model call
// delayed so the holder run stays in flight across a mid-flight /undo POST.
func newGatedUndoServer(t *testing.T, dir string, perCallDelay time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	model := language.NewFakeChatModel(
		language.WithCapabilities(language.ChatModelCapabilities{ToolCalling: true}),
		language.WithResponses(
			messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
				{ID: "gate-write-1", Name: "Write", Args: map[string]any{
					"file_path": filepath.Join(dir, "out.txt"),
					"content":   "from agent",
				}},
			}},
			messages.Message{Role: messages.RoleAI, Content: "wrote the file"},
		),
		language.WithRateLimiter(delayLimiter{d: perCallDelay}),
	)
	a, err := agent.New(agent.Config{
		Model:          model,
		Registry:       tools.NewRegistry(dir),
		SystemPrompt:   "test",
		WorkDir:        dir,
		PermissionMode: config.ModeBypass,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := New(Config{Agent: a, Static: fstest.MapFS{}, Logger: zerolog.Nop(), SessionDir: t.TempDir(), WorkDir: dir})
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

// postCommandRun POSTs a harness command ("/undo", "/redo") as a run prompt
// and returns the created run id, queue position and HTTP status. A non-200
// body (e.g. the queue-full 409 plain text) is not JSON: runID stays empty.
func postCommandRun(t *testing.T, base, sessionID, prompt string) (runID string, pos int, status int) {
	t.Helper()
	body := fmt.Sprintf(`{"prompt":%q,"session_id":%q}`, prompt, sessionID)
	resp, err := http.Post(base+"/api/runs", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if status != http.StatusOK {
		return "", -1, status
	}
	var created struct {
		RunID         string `json:"run_id"`
		QueuePosition int    `json:"queue_position"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		t.Fatalf("decode run response (status %d): %v", resp.StatusCode, err)
	}
	return created.RunID, created.QueuePosition, status
}

// Core regression (T7): a gated run in flight on session S, /undo arrives
// mid-flight. The command run must wait for the holder (Option A: it is a
// session TURN — the file revert and the transcript append both order against
// the holder), and after both settle the transcript contains BOTH the
// holder's reply AND the /undo turn — no last-writer-wins message loss. The
// holder's Write must also have happened before the undo executes, so the
// revert really reverts it ("Undid", file gone) instead of racing it.
func TestUndoCommandRunWaitsForInFlightRun(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	saved := trackSaves(s)
	const sid = "s-undo-gate"

	// Holder: a gated model run, still in flight (two delayed model calls)
	// when the /undo POST lands below.
	holderRun, _, pos := postRun(t, ts.URL, sid, "write it")
	if pos != 0 {
		t.Fatalf("holder position = %d, want 0 (runs immediately)", pos)
	}

	// /undo mid-flight: it must queue behind the holder, not execute ungated.
	undoRun, undoPos, status := postCommandRun(t, ts.URL, sid, "/undo")
	if status != http.StatusOK {
		t.Fatalf("/undo status = %d, want 200", status)
	}
	if undoPos != 1 {
		t.Fatalf("/undo queue position = %d, want 1 (queued behind the holder)", undoPos)
	}

	out := driveCommandRun(t, ts.URL, undoRun)
	if !strings.Contains(out, "Undid") {
		t.Fatalf("/undo output = %q, want the holder's write reverted (Undid...)", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); !os.IsNotExist(err) {
		t.Fatalf("out.txt still present after /undo: %v", err)
	}
	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 2) // holder transcript + command turn

	// Both turns persisted, in order, nothing lost.
	msgs := sessionMessages(t, ts.URL, sid)
	var holderReply, undoHuman, undoReply int = -1, -1, -1
	for i, m := range msgs {
		if m.Role == messages.RoleAI && m.Content == "wrote the file" && holderReply < 0 {
			holderReply = i
		}
		if m.Role == messages.RoleHuman && m.Content == "/undo" && undoHuman < 0 {
			undoHuman = i
		}
		if m.Role == messages.RoleAI && strings.HasPrefix(m.Content, "Undid") && undoReply < 0 {
			undoReply = i
		}
	}
	if holderReply < 0 || undoHuman < 0 || undoReply < 0 {
		t.Fatalf("transcript lost a turn (holderReply=%d undoHuman=%d undoReply=%d): %+v",
			holderReply, undoHuman, undoReply, msgs)
	}
	if !(holderReply < undoHuman && undoHuman < undoReply) {
		t.Fatalf("turns out of order: holderReply=%d undoHuman=%d undoReply=%d", holderReply, undoHuman, undoReply)
	}
}

// Finalizer interplay: a /undo run queued behind the holder and cancelled
// before promotion persists its derived history (command + stop notice)
// through the holder's save→release window — the T1 finalizer queue — exactly
// like a queued-cancelled model run. Both the holder's reply and the
// cancelled command turn survive.
func TestQueuedUndoCancelledPersistsViaFinalizer(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	saved := trackSaves(s)
	const sid = "s-undo-cancel"

	holderRun, _, pos := postRun(t, ts.URL, sid, "write it")
	if pos != 0 {
		t.Fatalf("holder position = %d, want 0", pos)
	}
	undoRun, undoPos, status := postCommandRun(t, ts.URL, sid, "/undo")
	if status != http.StatusOK || undoPos != 1 {
		t.Fatalf("/undo status=%d pos=%d, want 200 pos=1 (queued)", status, undoPos)
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+undoRun+"/cancel", ""); err != nil {
		t.Fatal(err)
	}
	waitRunDoneInProc(t, s, undoRun)
	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 2) // holder transcript + the drained queued-cancel save

	msgs := sessionMessages(t, ts.URL, sid)
	hasHolder, hasCommand := false, false
	for _, m := range msgs {
		if m.Role == messages.RoleAI && m.Content == "wrote the file" {
			hasHolder = true
		}
		if m.Role == messages.RoleHuman && m.Content == "/undo" {
			hasCommand = true
		}
	}
	if !hasHolder || !hasCommand {
		t.Fatalf("transcript lost a turn (holder=%v command=%v): %+v", hasHolder, hasCommand, msgs)
	}
	if got := lastStopNotice(msgs); got != wantStopNotice(undoRun, StopReasonUser) {
		t.Fatalf("stop notice = %q, want %q", got, wantStopNotice(undoRun, StopReasonUser))
	}
}

// Gate cap + FIFO drain: a busy session's command runs consume queue slots
// like any run — beyond maxQueuedPerSession the POST fails 409 — and the
// queued command turns all persist in FIFO order after the holder, no loss.
func TestUndoQueueCapAndFIFOOrder(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	// 1 holder + 8 command-run saves: more than trackSaves' buffer of 8
	// (the hook blocks inside sessions.save when the channel fills).
	saved := make(chan string, 16)
	s.sessions.testAfterSave = func(id string) { saved <- id }
	const sid = "s-undo-cap"

	holderRun, _, _ := postRun(t, ts.URL, sid, "write it")
	runIDs := make([]string, 0, maxQueuedPerSession)
	for i := 0; i < maxQueuedPerSession; i++ {
		runID, pos, status := postCommandRun(t, ts.URL, sid, "/undo")
		if status != http.StatusOK {
			t.Fatalf("/undo %d status = %d, want 200", i+1, status)
		}
		if pos != i+1 {
			t.Fatalf("/undo %d queue position = %d, want %d", i+1, pos, i+1)
		}
		runIDs = append(runIDs, runID)
	}
	// The queue is full: one more command run is rejected like any run.
	if _, pos, status := postCommandRun(t, ts.URL, sid, "/undo"); status != http.StatusConflict || pos != -1 {
		t.Fatalf("overflow /undo status=%d pos=%d, want 409 pos=-1", status, pos)
	}

	for _, id := range runIDs {
		driveCommandRun(t, ts.URL, id)
	}
	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 1+maxQueuedPerSession)

	msgs := sessionMessages(t, ts.URL, sid)
	holderReply, undoTurns := -1, 0
	for i, m := range msgs {
		if m.Role == messages.RoleAI && m.Content == "wrote the file" {
			holderReply = i
		}
		if m.Role == messages.RoleHuman && m.Content == "/undo" {
			undoTurns++
			if holderReply < 0 {
				t.Fatalf("/undo turn at %d precedes the holder reply at %d (FIFO violated)", i, holderReply)
			}
		}
	}
	if undoTurns != maxQueuedPerSession {
		t.Fatalf("persisted %d /undo turns, want %d (message loss)", undoTurns, maxQueuedPerSession)
	}
}

// /btw stays a deliberate no-persist side run (T7 scope guard): it must not
// become gated persistence by the back of this fix — its exchange never
// enters the transcript and never fires a session save.
func TestBtwStillNeverPersists(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 0)
	saved := trackSaves(s)
	const sid = "s-btw"

	if err := postJSON(t, ts.URL+"/api/sessions/"+sid+"/btw", `{"prompt":"side question"}`); err != nil {
		t.Fatal(err)
	}
	// The btw run completes without any transcript write.
	time.Sleep(200 * time.Millisecond)
	select {
	case id := <-saved:
		t.Fatalf("btw persisted a save for %s; the side run must stay no-persist", id)
	default:
	}
	before := len(sessionMessages(t, ts.URL, sid))
	if before != 0 {
		t.Fatalf("btw session has %d messages, want 0", before)
	}
}

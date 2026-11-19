package server

// F1 admission-gate bypass: handleTruncateSession used to snapshot the
// session transcript and full-overwrite save it immediately — outside the
// session admission gate. When a gated run was in flight on the same
// session, the two load+save pairs raced last-writer-wins and silently lost
// messages in BOTH directions: the truncate was undone by the holder's
// later completion save (built from the holder's start-time history), or
// the holder's new turn was wiped by the truncate. The regression tests
// here drive POST /api/sessions/{id}/truncate through the HTTP surface
// while a holder run is mid-flight and require the settled transcript to be
// exactly "holder's final output MINUS the last turn" (drop_last_turn
// semantics) — never a mixed loss.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/platform/blobstore"
	"github.com/projanvil/woca/internal/session"
)

// postTruncate POSTs the truncate endpoint and decodes {"kept": n}. It
// BLOCKS until the server answers — under the gated implementation that is
// after the session's in-flight runs settle, so callers that need the
// request in flight must run it on their own goroutine.
func postTruncate(t *testing.T, base, sid string, body string) (kept int, status int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base+"/api/sessions/"+sid+"/truncate", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	status = resp.StatusCode
	if status != http.StatusOK {
		return -1, status
	}
	var out struct {
		Kept int `json:"kept"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode truncate response (status %d): %v", resp.StatusCode, err)
	}
	return out.Kept, status
}

// Core regression (F1): a gated run in flight on session S, a
// drop_last_turn truncate arrives mid-flight. The truncate must order
// against the holder (it is a full-overwrite session writer): after both
// settle, the transcript is the holder's final output MINUS its last turn
// — the pre-seeded turns survive, the holder's prompt+reply are dropped —
// and `kept` is exact at response time. Pre-fix, the truncate snapshotted
// the pre-holder history ([one, two] → keep=0), and the holder's later
// completion save resurrected its whole turn on top: kept=0 plus a final
// transcript of 4 messages, both asserted against here.
func TestTruncateWaitsForInFlightRun(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	const sid = "s-trunc-gate"
	// Pre-seed one settled turn so "minus the last turn" leaves something.
	if err := s.sessions.save(t.Context(), sid, []messages.Message{
		messages.Human("one"), messages.AI("two"),
	}, dir); err != nil {
		t.Fatal(err)
	}
	saved := trackSaves(s) // AFTER the seed: only holder + truncate saves land here

	// Holder: a gated model run, still in flight (two delayed model calls)
	// when the truncate POST lands below.
	holderRun, _, pos := postRun(t, ts.URL, sid, "write it")
	if pos != 0 {
		t.Fatalf("holder position = %d, want 0 (runs immediately)", pos)
	}

	type truncResult struct {
		kept   int
		status int
	}
	truncCh := make(chan truncResult, 1)
	go func() {
		kept, status := postTruncate(t, ts.URL, sid, `{"drop_last_turn":true}`)
		truncCh <- truncResult{kept, status}
	}()

	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 2) // holder transcript + the truncate save
	res := <-truncCh
	if res.status != http.StatusOK {
		t.Fatalf("truncate status = %d, want 200", res.status)
	}
	// Exact `kept` at response time: the settled history is
	// [one, two, "write it", "wrote the file"]; dropping the last turn keeps 2.
	if res.kept != 2 {
		t.Fatalf("kept = %d, want 2 (snapshot taken after the holder settled)", res.kept)
	}

	// Settled transcript: the pre-seeded turn survives, the holder's turn is
	// dropped — never a mixed loss.
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 2 || msgs[0].Content != "one" || msgs[1].Content != "two" {
		t.Fatalf("truncated transcript = %v, want [one two]", msgs)
	}
}

// Idle-session fast path: an idle session grants the gate on the spot, so
// drop_last_turn stays an immediate exact answer (the pre-existing
// behavior, now inside the gate window).
func TestTruncateDropLastTurnIdleSession(t *testing.T) {
	s, _ := newChangesServer(t)
	const sid = "s-trunc-idle"
	if err := s.sessions.save(t.Context(), sid, []messages.Message{
		messages.Human("q1"), messages.AI("a1"), messages.Human("q2"), messages.AI("a2"),
	}, s.workdir); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	kept, status := postTruncate(t, ts.URL, sid, `{"drop_last_turn":true}`)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if kept != 2 {
		t.Fatalf("kept = %d, want 2", kept)
	}
	msgs := s.sessions.load(t.Context(), sid)
	if len(msgs) != 2 || msgs[0].Content != "q1" || msgs[1].Content != "a1" {
		t.Fatalf("transcript = %v, want [q1 a1]", msgs)
	}
}

// Runaway-client guard: the truncate queues for the gate like any waiter,
// so a session at maxQueuedPerSession refuses it with the same 409 the run
// surface returns — and the refusal consumes no queue slot (the queued runs
// all still complete afterwards).
func TestTruncateQueueFullRefused(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	saved := make(chan string, 16)
	s.sessions.testAfterSave = func(id string) { saved <- id }
	const sid = "s-trunc-cap"

	holderRun, _, _ := postRun(t, ts.URL, sid, "write it")
	runIDs := make([]string, 0, maxQueuedPerSession)
	for i := 0; i < maxQueuedPerSession; i++ {
		runID, pos, status := postCommandRun(t, ts.URL, sid, "/undo")
		if status != http.StatusOK {
			t.Fatalf("/undo %d status = %d", i+1, status)
		}
		if pos != i+1 {
			t.Fatalf("/undo %d position = %d, want %d", i+1, pos, i+1)
		}
		runIDs = append(runIDs, runID)
	}
	// Queue full: the truncate is refused like any further run.
	if kept, status := postTruncate(t, ts.URL, sid, `{"keep":1}`); status != http.StatusConflict || kept != -1 {
		t.Fatalf("truncate on full queue = kept/%d status/%d, want -1/409", kept, status)
	}
	// No slot was consumed and no write happened: the holder plus every
	// queued command turn still settles in FIFO order.
	for _, id := range runIDs {
		driveCommandRun(t, ts.URL, id)
	}
	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 1+maxQueuedPerSession)
	msgs := sessionMessages(t, ts.URL, sid)
	undoTurns := 0
	for _, m := range msgs {
		if m.Role == messages.RoleHuman && m.Content == "/undo" {
			undoTurns++
		}
	}
	if undoTurns != maxQueuedPerSession {
		t.Fatalf("persisted %d /undo turns, want %d", undoTurns, maxQueuedPerSession)
	}
}

// Abandoned waiter: a truncate queued behind a holder whose client goes
// away before promotion must answer 503, leave the gate untouched — no
// write, no leaked hold — so the session's next run still executes
// normally. The 503 is asserted through a recorder because a client that
// canceled its request context can no longer read the response it caused.
func TestTruncateAbandonedWhileQueuedReleasesNothing(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	saved := trackSaves(s)
	const sid = "s-trunc-abandon"

	holderRun, _, pos := postRun(t, ts.URL, sid, "write it")
	if pos != 0 {
		t.Fatalf("holder position = %d, want 0", pos)
	}

	// Truncate queued behind the holder, then its client disconnects.
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/truncate",
		strings.NewReader(`{"keep":1}`))
	req.SetPathValue("id", sid)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleTruncateSession(rec, req)
	}()
	// Give the truncate time to join the queue, then abandon it.
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("abandoned truncate handler never returned")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("abandoned truncate status = %d, want 503", rec.Code)
	}

	// The gate must be intact: the holder settles, and a fresh run on the
	// same session is granted (position 0) and completes.
	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 1) // holder only — the abandoned truncate wrote nothing
	nextRun, _, nextPos := postRun(t, ts.URL, sid, "next question")
	if nextPos != 0 {
		t.Fatalf("next run position = %d, want 0 (gate must be free)", nextPos)
	}
	waitRunDone(t, ts.URL, nextRun)
	awaitSave(t, saved, 1) // the next run's transcript
	msgs := sessionMessages(t, ts.URL, sid)
	last := msgs[len(msgs)-1]
	if msgs[0].Content != "write it" || last.Role != messages.RoleAI || last.Content == "" {
		t.Fatalf("transcript after abandoned truncate = %s", summarize(msgs))
	}
}

// Finalizer interplay: a queued run cancelled before promotion persists its
// derived turn through the holder's save→release window (the T1 finalizer
// queue). The truncate queued behind all of that must snapshot the FULLY
// settled history — holder reply plus the cancelled turn — and drop the
// last turn from it, instead of racing past the deferred save. The settled
// transcript keeps the holder's turn and drops the cancelled run's turn.
func TestTruncateDrainsPendingFinalizerBeforeSnapshot(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	const sid = "s-trunc-finalizer"
	if err := s.sessions.save(t.Context(), sid, []messages.Message{
		messages.Human("one"), messages.AI("two"),
	}, dir); err != nil {
		t.Fatal(err)
	}
	saved := trackSaves(s) // AFTER the seed: holder + finalizer + truncate

	holderRun, _, _ := postRun(t, ts.URL, sid, "write it")
	queuedRun, _, qPos := postRun(t, ts.URL, sid, "queued and doomed")
	if qPos != 1 {
		t.Fatalf("queued run position = %d, want 1", qPos)
	}
	if err := postJSON(t, ts.URL+"/api/runs/"+queuedRun+"/cancel", ""); err != nil {
		t.Fatal(err)
	}

	// The truncate joins the queue behind the holder (the cancelled queued
	// run is being removed from it in parallel).
	type truncResult struct {
		kept   int
		status int
	}
	truncCh := make(chan truncResult, 1)
	go func() {
		kept, status := postTruncate(t, ts.URL, sid, `{"drop_last_turn":true}`)
		truncCh <- truncResult{kept, status}
	}()

	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 3) // holder + drained queued-cancel save + truncate
	res := <-truncCh
	if res.status != http.StatusOK {
		t.Fatalf("truncate status = %d, want 200", res.status)
	}
	// Settled history: [one(h), two(a), "write it"(h), toolcall(a),
	// tool result(t), "wrote the file"(a), "queued and doomed"(h),
	// stop notice(h)]. drop_last_turn anchors on the LAST human message —
	// the stop notice — so keep=7. The load-bearing ordering fact: the
	// snapshot saw the finalizer's writes (kept>0 and the cancelled run's
	// prompt survives); pre-fix the ungated truncate snapshotted the
	// pre-holder seed and returned kept=0.
	if res.kept != 7 {
		t.Fatalf("kept = %d, want 7 (snapshot after the finalizer drained)", res.kept)
	}
	msgs := sessionMessages(t, ts.URL, sid)
	if len(msgs) != 7 {
		t.Fatalf("truncated transcript = %d msgs, want 7: %s", len(msgs), summarize(msgs))
	}
	wantContent := []string{"one", "two", "write it", "", "", "wrote the file", "queued and doomed"}
	for i, want := range wantContent {
		if msgs[i].Content != want && !(i == 4 && msgs[i].Role == messages.RoleTool) {
			t.Fatalf("msg %d = %s, want %q (full: %s)", i, summarize([]messages.Message{msgs[i]}), want, summarize(msgs))
		}
	}
	if got := lastStopNotice(msgs); got != "" {
		t.Fatalf("the dropped anchor (stop notice) survived the truncate: %q", got)
	}
}

// disconnectAwareStore wraps a blobstore.Store with the context semantics of
// the interface's remote implementations (S3/NATS-style stores honor ctx;
// the in-tree Local deliberately ignores it): once the context is canceled,
// Put fails with the ctx error. The second Put for key is additionally held
// on a test-owned gate so the test can schedule the client disconnect
// exactly between the gate promotion and the truncate's transcript write.
type disconnectAwareStore struct {
	inner blobstore.Store
	key    string
	puts   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func (d *disconnectAwareStore) Put(ctx context.Context, key string, data []byte) error {
	if key == d.key && d.puts.Add(1) == 2 {
		close(d.started)
		<-d.release
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return d.inner.Put(ctx, key, data)
}

func (d *disconnectAwareStore) Get(ctx context.Context, key string) ([]byte, error) {
	return d.inner.Get(ctx, key)
}

func (d *disconnectAwareStore) Delete(ctx context.Context, key string) error {
	return d.inner.Delete(ctx, key)
}

func (d *disconnectAwareStore) List(ctx context.Context, prefix string) ([]string, error) {
	return d.inner.List(ctx, prefix)
}

// Disconnect AFTER promotion: the waiter survives (unlike the 503 case, the
// client is gone only once the gate already handed it the session), so the
// in-window load+save must run detached from the connection — the same
// persistence posture as completeRun (WithoutCancel: keep the ctx values,
// drop the cancellation). Pre-fix the in-window save used r.Context():
// sessionStore.save published the truncated transcript to memory (mem write
// comes first), then the blob Put failed on any ctx-honoring store — mem and
// disk forked (memory truncated, disk still the full holder history) and the
// handler answered 500 into a dead connection. The regression pins the
// disconnect between promotion and the truncate's Put via the store wrapper
// and requires: 200 (the write completed), disk equal to memory (no fork),
// and the gate released (the next run is admitted immediately).
func TestTruncateSaveDetachedFromDisconnectAfterPromotion(t *testing.T) {
	dir := t.TempDir()
	s, ts := newGatedUndoServer(t, dir, 400*time.Millisecond)
	const sid = "s-trunc-disconnect"
	if err := s.sessions.save(t.Context(), sid, []messages.Message{
		messages.Human("one"), messages.AI("two"),
	}, dir); err != nil {
		t.Fatal(err)
	}
	saved := trackSaves(s) // AFTER the seed: holder + truncate saves
	// Wrap the store: Put #2 for this session's key is the truncate's save
	// (Put #1 is the holder's completion save) — hold it for the disconnect.
	store := &disconnectAwareStore{
		inner:   s.sessions.blobs,
		key:     sid + ".jsonl",
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	s.sessions.blobs = store

	holderRun, _, pos := postRun(t, ts.URL, sid, "write it")
	if pos != 0 {
		t.Fatalf("holder position = %d, want 0 (runs immediately)", pos)
	}

	// Truncate queued behind the holder with a client that will disconnect
	// AFTER promotion (once its save starts, the gate is already its own).
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+sid+"/truncate",
		strings.NewReader(`{"drop_last_turn":true}`))
	req.SetPathValue("id", sid)
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.handleTruncateSession(rec, req)
	}()

	// Holder settles → the truncate is promoted → its save starts. Disconnect
	// the client right then, and let the held Put proceed with whatever ctx
	// the handler chose for the in-window save.
	waitRunDone(t, ts.URL, holderRun)
	awaitSave(t, saved, 1) // holder transcript only so far
	select {
	case <-store.started:
	case <-time.After(5 * time.Second):
		t.Fatal("truncate save never started (promotion or save lost)")
	}
	cancel()
	close(store.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("truncate handler never returned")
	}

	// The write completed despite the dead connection.
	if rec.Code != http.StatusOK {
		t.Fatalf("truncate after disconnect = %d, want 200 (in-window save must be detached)", rec.Code)
	}
	awaitSave(t, saved, 1) // the truncate save

	// No mem/disk fork: memory and disk both hold the truncated transcript.
	memMsgs := s.sessions.load(t.Context(), sid)
	diskMsgs, err := session.LoadBlob(t.Context(), s.sessions.blobs, sid+".jsonl")
	if err != nil {
		t.Fatalf("reload transcript from disk: %v", err)
	}
	if len(memMsgs) != 2 || memMsgs[0].Content != "one" || memMsgs[1].Content != "two" {
		t.Fatalf("memory transcript = %s, want [one two] (truncated)", summarize(memMsgs))
	}
	if len(diskMsgs) != len(memMsgs) || diskMsgs[0].Content != memMsgs[0].Content || diskMsgs[1].Content != memMsgs[1].Content {
		t.Fatalf("disk transcript = %s, want it to match memory %s (no fork)", summarize(diskMsgs), summarize(memMsgs))
	}

	// The gate was released: the session's next run is granted immediately.
	nextRun, _, nextPos := postRun(t, ts.URL, sid, "next question")
	if nextPos != 0 {
		t.Fatalf("next run position = %d, want 0 (gate must be free)", nextPos)
	}
	waitRunDone(t, ts.URL, nextRun)
}

// summarize renders a compact role:content view for failure messages.
func summarize(msgs []messages.Message) string {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s:%q", m.Role, truncateRunes(m.Content, 30))
	}
	return b.String()
}

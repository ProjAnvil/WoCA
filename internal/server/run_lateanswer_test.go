package server

// Late answers after terminal completion (F3 item 2): once a run emitted its
// terminal event, no park can ever consume an answer. List-form answers used
// to buffer into the resume window and emit interrupt_resolved into the
// replay history — resurrecting a card the terminal event already closed and
// growing history the done frame terminates. They must be a no-op.

import (
	"testing"
	"time"
)

// finishForTest stamps the run terminal like every production path does.
func finishForTest(r *Run, cancelled bool) {
	if cancelled {
		r.cancelRun(StopReasonUser)
		r.finish(EventDone, map[string]any{"cancelled": true, "reason": r.stopCause()})
		return
	}
	r.finish(EventDone, map[string]any{"cancelled": false})
}

// doneSeq returns the terminal event's seq (0 when the run is not done).
func doneSeq(t *testing.T, r *Run) int64 {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.done {
		return 0
	}
	for i := len(r.history) - 1; i >= 0; i-- {
		if r.history[i].Type == EventDone || r.history[i].Type == EventError {
			return r.history[i].Seq
		}
	}
	return 0
}

// countResolvedAfter counts interrupt_resolved events past seq.
func countResolvedAfter(r *Run, seq int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, ev := range r.history {
		if ev.Type == EventInterruptResolved && ev.Seq > seq {
			n++
		}
	}
	return n
}

func bufferedCount(r *Run) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.buffered)
}

// historyLen snapshots the event count (the live-run control test measures
// exactly one resolution event per accepted buffered answer).
func historyLen(r *Run) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.history)
}

// A late list-form answer after a clean completion must not buffer and must
// not emit interrupt_resolved.
func TestLateListAnswerAfterCleanDoneIsDropped(t *testing.T) {
	r, cancel, got := parkCardsForTest(t, multiCardPause())
	defer cancel()

	// Answer the park fully: the run resumes (pending nil, resume window).
	if err := r.answer([]byte(`{"answers":[
		{"key":"approval:se1","value":{"approve":true}},
		{"key":"subagent:researcher-ad33:se2","value":{"approve":false}}
	]}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupt never resumed")
	}

	finishForTest(r, false)
	seq := doneSeq(t, r)
	if seq == 0 {
		t.Fatal("run not terminal")
	}

	err := r.answer([]byte(`{"answers":[{"key":"approval:se1","value":{"approve":true}}]}`))
	if err == nil {
		t.Fatal("late answer after completion must fail, not return ok")
	}
	if n := bufferedCount(r); n != 0 {
		t.Fatalf("late answer buffered %d entries; the run is over", n)
	}
	if n := countResolvedAfter(r, seq); n != 0 {
		t.Fatalf("late answer emitted %d interrupt_resolved events after done", n)
	}
}

// The cancellation shape: cancelRun sweeps the park, the terminal done lands,
// and both the list form and the legacy form are refused without touching
// history.
func TestLateAnswerAfterCancelledRunIsDropped(t *testing.T) {
	r, cancel, _ := parkCardsForTest(t, multiCardPause())
	defer cancel()

	finishForTest(r, true)
	seq := doneSeq(t, r)
	if seq == 0 {
		t.Fatal("run not terminal")
	}

	for name, body := range map[string]string{
		"list form":   `{"answers":[{"key":"approval:se1","value":{"approve":true}}]}`,
		"legacy form": `{"approve":true}`,
	} {
		if err := r.answer([]byte(body)); err == nil {
			t.Fatalf("%s after cancellation must fail, not return ok", name)
		}
	}
	if n := bufferedCount(r); n != 0 {
		t.Fatalf("late answer buffered %d entries after cancellation", n)
	}
	if n := countResolvedAfter(r, seq); n != 0 {
		t.Fatalf("late answer emitted %d interrupt_resolved events after done", n)
	}
}

// Control: the same list-form answer in the live resume window (run not
// done) still buffers and emits resolution — the gate must not over-reach.
func TestListAnswerStillBuffersWhileRunLive(t *testing.T) {
	r, cancel, _ := parkCardsForTest(t, multiCardPause())
	defer cancel()

	// Answer the park fully to open the resume window, then re-park by
	// parking again is unnecessary: the window itself (pending nil, run not
	// done) is the buffering shape.
	if err := r.answer([]byte(`{"answers":[
		{"key":"approval:se1","value":{"approve":true}},
		{"key":"subagent:researcher-ad33:se2","value":{"approve":false}}
	]}`)); err != nil {
		t.Fatal(err)
	}
	before := historyLen(r)
	if err := r.answer([]byte(`{"answers":[{"key":"approval:se1","value":{"approve":true}}]}`)); err != nil {
		t.Fatal(err)
	}
	if n := bufferedCount(r); n != 1 {
		t.Fatalf("live-run buffering = %d entries, want 1", n)
	}
	if got := countResolvedAfter(r, 0); got == 0 {
		t.Fatal("live-run buffered answer must still emit interrupt_resolved")
	}
	if after := historyLen(r); after != before+1 {
		t.Fatalf("history grew by %d events, want exactly the one resolution", after-before)
	}
}

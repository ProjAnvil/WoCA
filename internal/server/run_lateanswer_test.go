package server

// Late answers after terminal completion (F3 item 2): once a run emitted its
// terminal event, no park can ever consume an answer. List-form answers used
// to buffer into the resume window and emit interrupt_resolved into the
// replay history — resurrecting a card the terminal event already closed and
// growing history the done frame terminates. They must be a no-op.

import (
	"context"
	"sync"
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

// TestAnswerFinishInterleaveKeepsTerminalLast races answer() against
// finish() across both answer branches (buffering and parked) and requires
// the invariant the atomic re-check buys: interrupt_resolved never orders
// past the terminal event.
//
// The interleave is NOT deterministically forceable — the window sits
// between answer's entry gate and its second critical section, with only
// pure-CPU decoding in between and no production seam to pause on — so this
// is a bounded stress guard, not a step-by-step reproduction: it can never
// fail on correct code (the resolution append shares the critical section
// with the done re-check), and it catches a missing guard with high
// probability across thousands of cheap iterations (verified by mutation:
// dropping the in-section re-check fails it within one -count=1 run). The
// runs are built directly (pending set by hand) so each iteration costs
// microseconds instead of a parked-goroutine poll.
func TestAnswerFinishInterleaveKeepsTerminalLast(t *testing.T) {
	const fullAnswer = `{"answers":[
		{"key":"approval:se1","value":{"approve":true}},
		{"key":"subagent:researcher-ad33:se2","value":{"approve":false}}
	]}`
	const oneEntry = `{"answers":[{"key":"approval:se1","value":{"approve":true}}]}`

	// buffering: pending nil (resume-window shape). parked: a hand-set
	// two-card pause with the same cards multiCardPause classifies.
	newRacer := func(parked bool) *Run {
		ctx, cancel := context.WithCancel(t.Context())
		r := newRun("race", nil, ctx, cancel)
		if parked {
			r.mu.Lock()
			r.pending = &interruptState{
				cards: []parkedCard{
					{id: "ia1", ns: "approval:se1", kind: "permission", payload: map[string]any{}},
					{id: "ia2", ns: "subagent:researcher-ad33:se2", kind: "permission", payload: map[string]any{}},
				},
				reply: make(chan any, 1),
			}
			r.mu.Unlock()
		}
		return r
	}

	race := func(answerBody string, parked bool) {
		r := newRacer(parked)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_ = r.answer([]byte(answerBody)) // nil or "run already finished"
		}()
		go func() {
			defer wg.Done()
			<-start
			r.finish(EventDone, map[string]any{"cancelled": false})
		}()
		close(start)
		wg.Wait()
		r.cancel()

		seq := doneSeq(t, r)
		if seq == 0 {
			t.Fatal("run never finished")
		}
		if n := countResolvedAfter(r, seq); n != 0 {
			t.Fatalf("interrupt_resolved emitted %d times past the terminal event", n)
		}
	}

	const iterations = 2000
	for i := 0; i < iterations; i++ {
		race(oneEntry, false) // buffering branch (the F3 late-answer path)
		race(fullAnswer, true)
	}
}

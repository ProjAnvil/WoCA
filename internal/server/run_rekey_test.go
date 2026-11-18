package server

// T5 Part A: buffered answers across re-parks. An answer posted in the
// resume window (after a partial answer, before the interrupted sibling
// re-parks) buffers keyed by the card's THEN-current key (its interrupt
// NS). If the re-parked approval surfaces under a regenerated NS, the
// buffered decision must RE-KEY onto it via the approval request's own
// identity (its asks' call ids — stable across the approval node's
// re-execution) instead of being silently dropped: a buffered decision is
// never orphaned while its approval is still live, and never immortal once
// its approval is gone.

import (
	"testing"
	"time"

	"github.com/projanvil/woca/internal/agent"
)

// rekeyAsks builds one permission ask.
func rekeyAsks(callID string) []agent.Ask {
	return []agent.Ask{{
		CallID:         callID,
		ToolName:       "Bash",
		ToolArgs:       map[string]any{"command": "npm install left-pad"},
		Reason:         "Run command: npm install left-pad",
		Origin:         agent.OriginMain,
		SessionRuleKey: "bash:npm install left-pad*",
	}}
}

// TestBufferedAnswerReKeysOntoReParkedCard: park two cards, answer ONE
// (partial resume), buffer the sibling's deny keyed by its ORIGINAL NS
// while the run is between parks, then re-park the sibling under a
// REGENERATED NS carrying the SAME approval ask — the buffered deny must
// apply to the re-parked card (this is the orphan the e2e "two parallel
// child approvals" deny step raced into).
func TestBufferedAnswerReKeysOntoReParkedCard(t *testing.T) {
	r, cancel, _ := parkCardsForTest(t, []agent.PauseCard{
		{NS: "child-a:t1/approval:s1", ID: "ia", Value: agent.ApprovalRequest{Asks: rekeyAsks("ca1")}},
		{NS: "child-b:t2/approval:s2", ID: "ib", Value: agent.ApprovalRequest{Asks: rekeyAsks("cb1")}},
	})
	defer cancel()

	// Partial answer: card A only — the pause resumes, card B re-parks next.
	if err := r.answer([]byte(`{"answers":[{"key":"child-a:t1/approval:s1","value":{"approve":true}}]}`)); err != nil {
		t.Fatal(err)
	}

	// The resume window: pending is gone; the sibling's deny arrives keyed
	// by its ORIGINAL NS (the card the user clicked) and must buffer.
	deny := []byte(`{"answers":[{"key":"child-b:t2/approval:s2","value":{"decisions":{"cb1":{"outcome":"deny"}}}}]}`)
	if err := r.answer(deny); err != nil {
		t.Fatalf("answer in the resume window must buffer, got: %v", err)
	}

	// The re-park: the sibling's approval re-fires under a REGENERATED NS
	// (per-invocation fork identity) but carries the SAME approval ask.
	done := make(chan any, 1)
	errCh := make(chan error, 1)
	go func() {
		ans, err := r.handleInterrupt([]agent.PauseCard{{
			NS:    "child-b:t2/approval:s3",
			Value: agent.ApprovalRequest{Asks: rekeyAsks("cb1")},
		}})
		if err != nil {
			errCh <- err
			return
		}
		done <- ans
	}()
	select {
	case ans := <-done:
		got, ok := ans.(agent.ApprovalAnswer)
		if !ok || got.Decisions["cb1"].Outcome != agent.OutcomeDeny {
			t.Fatalf("buffered deny did not re-key onto the re-parked card: %#v", ans)
		}
	case err := <-errCh:
		t.Fatalf("handleInterrupt errored: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("handleInterrupt blocked: the buffered decision was orphaned on the NS change")
	}
}

// TestBufferedAnswerForGoneApprovalIsDropped: the re-keying must not make
// buffered decisions immortal — a buffered entry whose approval ask never
// re-parks (the sibling's ask was collapsed away by a session grant, or the
// child finished) dies at the next park even when that park carries OTHER
// asks, and a park carrying a DIFFERENT ask under the same shape must not
// consume it either.
func TestBufferedAnswerForGoneApprovalIsDropped(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}, ctx: t.Context()}
	// Buffer a deny for ask cb1 (no pending park).
	if err := r.answer([]byte(`{"answers":[{"key":"child-b:t2/approval:s2","value":{"decisions":{"cb1":{"outcome":"deny"}}}}]}`)); err != nil {
		t.Fatalf("buffer: %v", err)
	}
	// Park an unrelated approval (a different ask entirely): the stale
	// buffered entry must not be consumed by identity or key.
	parked := make(chan struct{})
	r.onPark = func() { close(parked) }
	go func() {
		_, _ = r.handleInterrupt([]agent.PauseCard{{
			NS:    "child-c:t3/approval:s9",
			Value: agent.ApprovalRequest{Asks: rekeyAsks("cc9")},
		}})
	}()
	<-parked
	select {
	case ans := <-r.pending.reply:
		t.Fatalf("stale buffered entry leaked onto a different approval: %#v", ans)
	default:
	}
	// The buffered list must already be empty HERE (the application pass
	// finishes before the park's onPark fires): the entry died with the
	// park that could not address it. Read before cancelRun, which clears
	// the buffer unconditionally.
	r.mu.Lock()
	left := len(r.buffered)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("buffered entries survived a park that could not address them: %d", left)
	}
	r.cancelRun(StopReasonUser)
}

// TestBufferedIdentityStampedEntryForGoneApprovalIsDropped: the same
// no-immortality guarantee for entries buffered through the REAL resume
// window — these carry a stamped approval identity (answer() resolved the
// key against the last park's cards), so the drop must terminate them on an
// IDENTITY miss too, not only on a key miss. Without this rung, an
// identity-carrying entry whose approval never re-parks (the sibling's ask
// was collapsed away by a session grant, or the child finished) could ride
// the buffer forever, matching any later park that happens to reuse the ask.
func TestBufferedIdentityStampedEntryForGoneApprovalIsDropped(t *testing.T) {
	r, cancel, _ := parkCardsForTest(t, []agent.PauseCard{
		{NS: "child-a:t1/approval:s1", ID: "ia", Value: agent.ApprovalRequest{Asks: rekeyAsks("ca1")}},
		{NS: "child-b:t2/approval:s2", ID: "ib", Value: agent.ApprovalRequest{Asks: rekeyAsks("cb1")}},
	})
	defer cancel()

	// Partial answer: card A only — the pause resumes, card B is expected to
	// re-park (and lastCards now names this park's cards).
	if err := r.answer([]byte(`{"answers":[{"key":"child-a:t1/approval:s1","value":{"approve":true}}]}`)); err != nil {
		t.Fatal(err)
	}

	// The sibling's deny arrives in the resume window and buffers keyed by
	// its ORIGINAL NS — stamped with ask cb1's identity via lastCards.
	deny := []byte(`{"answers":[{"key":"child-b:t2/approval:s2","value":{"decisions":{"cb1":{"outcome":"deny"}}}}]}`)
	if err := r.answer(deny); err != nil {
		t.Fatalf("answer in the resume window must buffer, got: %v", err)
	}
	r.mu.Lock()
	stamped := append([]cardAnswerEntry(nil), r.buffered...)
	r.mu.Unlock()
	if len(stamped) != 1 || stamped[0].identity == "" {
		t.Fatalf("buffered entry must carry the approval identity for this rung, got %+v", stamped)
	}

	// The approval never re-parks: the next park is an UNRELATED request
	// (neither the old key nor cb1's identity). The identity-stamped entry
	// must DIE here — an identity miss terminates it, exactly like a key
	// miss — instead of riding the buffer to a later park.
	go func() {
		_, _ = r.handleInterrupt([]agent.PauseCard{{
			NS:    "child-c:t3/approval:s9",
			Value: agent.ApprovalRequest{Asks: rekeyAsks("cc9")},
		}})
	}()
	// waitPending observes r.pending under the run mutex: handleInterrupt
	// sets pending and runs the buffered-application pass in ONE critical
	// section, so observing the park means the pass already finished and
	// the buffer is final. (r.onPark cannot be used as the barrier here —
	// the first park's goroutine may still be reading it.)
	waitPending(t, r)
	select {
	case ans := <-r.pending.reply:
		t.Fatalf("identity-stamped entry leaked onto an unrelated approval: %#v", ans)
	default:
	}
	// Read the buffer BEFORE cancelRun (which clears it unconditionally).
	r.mu.Lock()
	left := len(r.buffered)
	r.mu.Unlock()
	if left != 0 {
		t.Fatalf("identity-stamped entry survived the park that could not address it: %d", left)
	}
	r.cancelRun(StopReasonUser)
}

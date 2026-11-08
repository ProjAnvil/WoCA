package queue

import (
	"strings"
	"sync"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/tracing"
)

// The buffer-mechanics tests in this file moved from internal/agent
// (queue_test.go and the addTagged atomicity pin in activity_test.go) when
// the queue was extracted (queue-extract spec): the semantics are the
// buffer's own, so the pins live with the code. Identifier names adapted to
// the package's own (Buffer.Add/AddTagged/Drain/Count for the former
// queue.add/addTagged/drain/count); the cases and expectations are
// unchanged. The agent-side wiring tests (run-start prepend, toolsNode
// attach, observer, released interplay) stayed in internal/agent.

// TestAddDrainCount moved from internal/agent/queue_test.go
// (TestQueueDrainSemantics): Add drops whitespace-only text, Count mirrors
// the buffered total, Drain returns items in arrival order and clears.
func TestAddDrainCount(t *testing.T) {
	b := &Buffer{}
	b.Add("first")
	b.Add("  ") // whitespace-only: ignored
	b.Add("second")
	if got := b.Count(""); got != 2 {
		t.Fatalf("count = %d, want 2", got)
	}
	items := b.Drain("")
	if len(items) != 2 || items[0].Text != "first" || items[1].Text != "second" {
		t.Fatalf("drain = %+v", items)
	}
	if got := b.Count(""); got != 0 {
		t.Fatalf("buffer not cleared: %d", got)
	}
	if again := b.Drain(""); len(again) != 0 {
		t.Fatalf("second drain = %+v", again)
	}
}

// TestAddConcurrent moved from internal/agent/queue_test.go
// (TestQueueInputConcurrent): concurrent Add is safe and loses nothing.
func TestAddConcurrent(t *testing.T) {
	b := &Buffer{}
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			b.Add(strings.Repeat("x", i+1))
		})
	}
	wg.Wait()
	if got := b.Count(""); got != 32 {
		t.Fatalf("count = %d, want 32", got)
	}
}

// TestSessionDrainSemantics moved from internal/agent/queue_test.go
// (TestQueueSessionDrainSemantics): draining for session "a" returns the
// unscoped items plus "a"'s own and leaves other sessions' items buffered;
// the count for a session mirrors what a run in that session would drain.
func TestSessionDrainSemantics(t *testing.T) {
	b := &Buffer{}
	b.Add("unscoped")                                                        // Session ""
	b.AddTagged(nil, QueuedInput{Text: "for-a", Tag: "tag-a", Session: "a"}) // Session "a"
	b.AddTagged(nil, QueuedInput{Text: "for-b", Tag: "tag-b", Session: "b"}) // Session "b"

	if got := b.Count(""); got != 3 {
		t.Fatalf("unscoped count = %d, want 3 (counts every session)", got)
	}
	if got := b.Count("a"); got != 2 {
		t.Fatalf("count for a = %d, want 2 (unscoped + a)", got)
	}
	if got := b.Count("b"); got != 2 {
		t.Fatalf("count for b = %d, want 2 (unscoped + b)", got)
	}

	items := b.Drain("a")
	if len(items) != 2 ||
		items[0].Text != "unscoped" || items[0].Session != "" ||
		items[1].Text != "for-a" || items[1].Session != "a" {
		t.Fatalf("drain for a = %+v", items)
	}
	if got := b.Count("a"); got != 0 {
		t.Fatalf("after drain for a: count for a = %d, want 0", got)
	}
	if got := b.Count(""); got != 1 {
		t.Fatalf("after drain for a: total = %d, want 1 (b still buffered)", got)
	}

	// The unscoped drain takes everything that is left, across sessions.
	rest := b.Drain("")
	if len(rest) != 1 || rest[0].Text != "for-b" || rest[0].Session != "b" {
		t.Fatalf("unscoped drain = %+v", rest)
	}
}

// TestDrainUnscopedReturnsEverything moved from internal/agent/queue_test.go
// (TestQueueDrainUnscopedReturnsEverything): drain("") — an unscoped run
// such as the TUI — takes every buffered item regardless of session.
func TestDrainUnscopedReturnsEverything(t *testing.T) {
	b := &Buffer{}
	b.Add("unscoped")
	b.AddTagged(nil, QueuedInput{Text: "for-a", Tag: "tag-a", Session: "a"})
	b.AddTagged(nil, QueuedInput{Text: "for-b", Tag: "tag-b", Session: "b"})

	items := b.Drain("")
	if len(items) != 3 {
		t.Fatalf("drain(\"\") = %+v, want all 3 items", items)
	}
	if got := b.Count(""); got != 0 {
		t.Fatalf("buffer not cleared: %d", got)
	}
}

// TestAddTaggedRejectsReleasedUnderLock moved from internal/agent/
// activity_test.go (TestQueueAddTaggedRejectsReleasedUnderLock). It pins the
// primitive's atomicity contract (round-1 review fix): the released decision
// is made INSIDE the buffer's critical section, so both serial orderings
// around a mark+drop leave nothing behind — an item appended before the drop
// is removed by it, and an item arriving after the drop is refused outright.
// The genuinely concurrent interleaving stays in internal/agent
// (TestReleasedDropAtomicWithQueueing), which races the agent's wiring.
func TestAddTaggedRejectsReleasedUnderLock(t *testing.T) {
	b := &Buffer{}
	marked := false
	reject := func(string) bool { return marked }

	// Ordering 1: append lands before the mark; DropScope then removes it.
	if !b.AddTagged(reject, QueuedInput{Text: "a", Tag: "t", Session: "s"}) {
		t.Fatal("pre-mark item refused")
	}
	marked = true
	if dropped := b.DropScope("s"); dropped != 1 {
		t.Fatalf("DropScope removed %d, want 1", dropped)
	}
	if n := b.Count("s"); n != 0 {
		t.Fatalf("%d items survived DropScope", n)
	}

	// Ordering 2: append attempts after the mark are refused under the lock.
	if b.AddTagged(reject, QueuedInput{Text: "b", Tag: "t", Session: "s"}) {
		t.Fatal("item queued for a released session")
	}
	if n := b.Count("s"); n != 0 {
		t.Fatalf("released session holds %d items", n)
	}
	// Blank text is still dropped, predicate or not.
	if b.AddTagged(reject, QueuedInput{Text: "  ", Tag: "t", Session: "s"}) {
		t.Fatal("blank text queued")
	}
	// Unscoped items never consult the predicate.
	if !b.AddTagged(reject, QueuedInput{Text: "u", Tag: "t"}) {
		t.Fatal("unscoped item refused")
	}
}

// TestWakeableCountExcludesQuiet pins the D1 half of the count surface:
// WakeableCount counts only the non-Quiet items a run in session would
// drain, while Count still sees Quiet items (they are delivered, they just
// never trigger a run).
func TestWakeableCountExcludesQuiet(t *testing.T) {
	b := &Buffer{}
	b.AddTagged(nil, QueuedInput{Text: "loud", Tag: "t", Session: "a"})
	b.AddTagged(nil, QueuedInput{Text: "hush", Tag: "t", Session: "a", Quiet: true})
	b.Add("typed") // unscoped, never Quiet
	if got := b.WakeableCount("a"); got != 2 {
		t.Fatalf("wakeable for a = %d, want 2 (loud + typed, Quiet excluded)", got)
	}
	if got := b.Count("a"); got != 3 {
		t.Fatalf("count for a = %d, want 3 (Quiet still delivered)", got)
	}
	if got := b.WakeableCount("b"); got != 1 {
		t.Fatalf("wakeable for b = %d, want 1 (unscoped only)", got)
	}
}

// TestTaggedCountCountsTaggedOnly pins the pending-notification count the
// activity seam surfaces report: tagged items only, Quiet included, session
// visibility applied.
func TestTaggedCountCountsTaggedOnly(t *testing.T) {
	b := &Buffer{}
	b.AddTagged(nil, QueuedInput{Text: "notif", Tag: "bg", Session: "a"})
	b.AddTagged(nil, QueuedInput{Text: "quiet notif", Tag: "bg", Session: "a", Quiet: true})
	b.AddTagged(nil, QueuedInput{Text: "steer", Session: "a"}) // untagged steer
	b.Add("typed")
	if got := b.TaggedCount("a"); got != 2 {
		t.Fatalf("tagged for a = %d, want 2 (Quiet included, untagged excluded)", got)
	}
	if got := b.TaggedCount("b"); got != 0 {
		t.Fatalf("tagged for b = %d, want 0", got)
	}
}

// TestFormatAndCoalesce pins the rendering byte-for-byte: the default
// queued-user-input wrapper with trimmed text, custom tags passed through,
// and Coalesce joining each item's block with a blank line (empty input
// yields an empty Human message).
func TestFormatAndCoalesce(t *testing.T) {
	got := Format(QueuedInput{Text: "  hello \n"})
	if got.Role != messages.RoleHuman || got.Content != "<queued-user-input>\nhello\n</queued-user-input>" {
		t.Fatalf("Format default tag = %+v", got)
	}
	got = Format(QueuedInput{Text: "done", Tag: "background-task-completed"})
	if got.Content != "<background-task-completed>\ndone\n</background-task-completed>" {
		t.Fatalf("Format custom tag = %+v", got)
	}

	coalesced := Coalesce([]QueuedInput{
		{Text: "one", Session: "a"},
		{Text: "two", Tag: "bg", Session: "a"},
	})
	want := "<queued-user-input>\none\n</queued-user-input>\n\n<bg>\ntwo\n</bg>"
	if coalesced.Role != messages.RoleHuman || coalesced.Content != want {
		t.Fatalf("Coalesce = %q, want %q", coalesced.Content, want)
	}
	if empty := Coalesce(nil); empty.Content != "" || empty.Role != messages.RoleHuman {
		t.Fatalf("Coalesce(nil) = %+v, want empty human message", empty)
	}
}

// TestSessionFrom pins the ctx read: a run-scoped ctx yields its session id,
// a bare ctx yields "" (unscoped delivery — the TUI/REPL case).
func TestSessionFrom(t *testing.T) {
	if got := SessionFrom(t.Context()); got != "" {
		t.Fatalf("SessionFrom(bare ctx) = %q, want \"\"", got)
	}
	ctx := tracing.WithRun(t.Context(), "run-1", "sess-1")
	if got := SessionFrom(ctx); got != "sess-1" {
		t.Fatalf("SessionFrom(run ctx) = %q, want sess-1", got)
	}
}

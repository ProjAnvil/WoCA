package server

import (
	"context"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/tools"
	"testing"
	"time"
)

func TestEmitAssignsMonotonicSeq(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	r.emit(Event{Type: "delta", Data: map[string]any{"text": "a"}})
	r.emit(Event{Type: "delta", Data: map[string]any{"text": "b"}})
	r.emit(Event{Type: "done", Data: map[string]any{}})
	if len(r.history) != 3 || r.history[0].Seq != 1 || r.history[1].Seq != 2 || r.history[2].Seq != 3 {
		t.Fatalf("seqs = %+v, want 1,2,3", r.history)
	}
}

func TestSubscribeSinceReplaysTail(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	for i := range 4 {
		r.emit(Event{Type: "delta", Data: map[string]any{"i": i}})
	}
	sub, gap := r.subscribeSince(2)
	if gap {
		t.Fatal("unexpected gap")
	}
	defer r.unsubscribe(sub)
	first := <-sub.ch
	if first.Seq != 3 {
		t.Fatalf("first replayed seq = %d, want 3", first.Seq)
	}
}

func TestSubscribeSinceGap(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	r.history = append(r.history, Event{Type: "delta", Seq: 41}, Event{Type: "delta", Seq: 42})
	r.seq = 42
	_, gap := r.subscribeSince(10) // 11..40 are no longer visible
	if !gap {
		t.Fatal("want gap=true when lastSeq falls outside retained history")
	}
}

func TestEmitDropCounter(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	full := &subscriber{ch: make(chan Event)} // unbuffered → always full
	r.subs[full] = struct{}{}
	r.emit(Event{Type: "delta", Data: map[string]any{}})
	if n := r.droppedCount(); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
}

func TestParkTimesOut(t *testing.T) {
	old := parkedRunTTL
	parkedRunTTL = 50 * time.Millisecond
	defer func() { parkedRunTTL = old }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &Run{id: "test", ctx: ctx, cancel: cancel, subs: map[*subscriber]struct{}{}}
	done := make(chan error, 1)
	go func() {
		cards := []parkedCard{{id: "p1", kind: "permission", payload: map[string]any{"tool_name": "Write"}}}
		reply := make(chan any, 1)
		r.mu.Lock()
		r.pending = &interruptState{cards: cards, reply: reply}
		r.mu.Unlock()
		_, err := r.waitParked(cards, reply) // no answer until timeout
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("park returned nil error after TTL, want ctx error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("park did not return after TTL")
	}
	if ctx.Err() == nil {
		t.Fatal("park TTL should cancel the run ctx")
	}
	var sawTimeout bool
	for _, ev := range r.history {
		if ev.Type == "timeout" {
			sawTimeout = true
		}
	}
	if !sawTimeout {
		t.Fatal("want timeout event in history")
	}
}

// TestSessionGrantCollapsesSameKeyPendingCards: granting one card's ask for
// the session auto-allows every OTHER pending card whose permission asks
// share the rule key (their ask decisions predate the grant and cannot
// re-evaluate it); ask/plan cards and different keys stay pending.
func TestSessionGrantCollapsesSameKeyPendingCards(t *testing.T) {
	r := &Run{}
	bashAsk := func(id, key string) agent.Ask {
		return agent.Ask{CallID: id, ToolName: "Bash", SessionRuleKey: key}
	}
	p := &interruptState{
		cards: []parkedCard{
			{ns: "ns-a", kind: "approvals", asks: []agent.Ask{bashAsk("c1", "bash:npm *")}},
			{ns: "ns-b", kind: "approvals", asks: []agent.Ask{bashAsk("c2", "bash:npm *")}},
			{ns: "ns-c", kind: "approvals", asks: []agent.Ask{bashAsk("c3", "bash:go *")}},
			{ns: "ns-d", kind: "ask", asks: []agent.Ask{{CallID: "c4", ToolName: "AskUserQuestion", Questions: []tools.Question{{}}}}},
		},
		reply: make(chan any, 1),
	}
	r.mu.Lock()
	r.pending = p
	r.mu.Unlock()
	body := []byte(`{"answers":[{"key":"ns-a","value":{"decisions":{"c1":{"outcome":"allow","grant":"session"}}}}]}`)
	if err := r.answer(body); err != nil {
		t.Fatal(err)
	}
	resume := <-p.reply
	m, ok := resume.(map[string]any)
	if !ok {
		t.Fatalf("resume = %#v, want the NS map", resume)
	}
	if len(m) != 2 {
		t.Fatalf("answered keys = %v, want ns-a + the collapsed ns-b only", m)
	}
	if _, ok := m["ns-b"]; !ok {
		t.Fatal("same-key sibling card not collapsed by the session grant")
	}
	ans, ok := m["ns-b"].(agent.ApprovalAnswer)
	if !ok || ans.Decisions["c2"].Outcome != agent.OutcomeAllow {
		t.Fatalf("collapsed card answer = %#v, want allow-once", m["ns-b"])
	}
	if _, ok := m["ns-c"]; ok {
		t.Fatal("different rule key must stay pending")
	}
	if _, ok := m["ns-d"]; ok {
		t.Fatal("ask card must stay pending")
	}
}

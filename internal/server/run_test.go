package server

import (
	"testing"
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
	for i := 0; i < 4; i++ {
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
	_, gap := r.subscribeSince(10) // 11..40 已不可见
	if !gap {
		t.Fatal("want gap=true when lastSeq falls outside retained history")
	}
}

func TestEmitDropCounter(t *testing.T) {
	r := &Run{id: "test", subs: map[*subscriber]struct{}{}}
	full := &subscriber{ch: make(chan Event)} // 无缓冲 → 永远满
	r.subs[full] = struct{}{}
	r.emit(Event{Type: "delta", Data: map[string]any{}})
	if n := r.droppedCount(); n != 1 {
		t.Fatalf("dropped = %d, want 1", n)
	}
}

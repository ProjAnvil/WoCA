package server

import (
	"context"
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

func TestParkTimesOut(t *testing.T) {
	old := parkedRunTTL
	parkedRunTTL = 50 * time.Millisecond
	defer func() { parkedRunTTL = old }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &Run{id: "test", ctx: ctx, cancel: cancel, subs: map[*subscriber]struct{}{}}
	done := make(chan error, 1)
	go func() {
		_, err := r.park("permission", map[string]any{"tool_name": "Write"}) // 无 answer，直到超时
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

package eventbus

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// publishN publishes n delta events and returns the assigned seqs.
func publishN(t *testing.T, b Bus, topic string, n int) []int64 {
	t.Helper()
	seqs := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		seq, err := b.Publish(context.Background(), topic, Event{Type: "delta", Data: fmt.Sprintf("e%d", i)})
		if err != nil {
			t.Fatalf("Publish(%q) error: %v", topic, err)
		}
		seqs = append(seqs, seq)
	}
	return seqs
}

// recv returns the next event or fails the test after a timeout.
func recv(t *testing.T, sub *Subscription) Event {
	t.Helper()
	select {
	case ev, ok := <-sub.C():
		if !ok {
			t.Fatal("subscription channel closed unexpectedly")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
		return Event{}
	}
}

// expectNoEvent fails the test if an event arrives within a short window.
func expectNoEvent(t *testing.T, sub *Subscription) {
	t.Helper()
	select {
	case ev := <-sub.C():
		t.Fatalf("unexpected event: %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPublishAssignsMonotonicSeq(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()

	// Publish must ignore the caller-supplied Seq.
	seqs := make([]int64, 0, 3)
	for i := 0; i < 3; i++ {
		seq, err := bus.Publish(context.Background(), "run-1", Event{Type: "delta", Seq: 99})
		if err != nil {
			t.Fatalf("Publish error: %v", err)
		}
		seqs = append(seqs, seq)
	}
	for i, want := range []int64{1, 2, 3} {
		if seqs[i] != want {
			t.Fatalf("seqs = %v, want [1 2 3]", seqs)
		}
	}

	sub, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer sub.Unsubscribe()
	for want := int64(1); want <= 3; want++ {
		if ev := recv(t, sub); ev.Seq != want {
			t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
		}
	}
}

func TestTopicsAreIsolated(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()

	seqsA := publishN(t, bus, "a", 2)
	seqsB := publishN(t, bus, "b", 3)
	if seqsA[0] != 1 || seqsA[1] != 2 {
		t.Fatalf("topic a seqs = %v, want [1 2]", seqsA)
	}
	if seqsB[0] != 1 || seqsB[1] != 2 || seqsB[2] != 3 {
		t.Fatalf("topic b seqs = %v, want [1 2 3]", seqsB)
	}

	subA, err := bus.Subscribe(context.Background(), "a", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer subA.Unsubscribe()
	for i := 0; i < 2; i++ {
		ev := recv(t, subA)
		if ev.Data != fmt.Sprintf("e%d", i) {
			t.Fatalf("topic a event %d data = %v, want e%d", i, ev.Data, i)
		}
	}
	expectNoEvent(t, subA) // topic b events must not leak into a
}

func TestSubscribeReplaysHistory(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()
	publishN(t, bus, "run-1", 4)

	tests := []struct {
		name     string
		sinceSeq int64
		wantSeqs []int64
	}{
		{"zero replays full history", 0, []int64{1, 2, 3, 4}},
		{"mid value replays tail", 2, []int64{3, 4}},
		{"at head replays nothing", 4, nil},
		{"beyond head replays nothing", 10, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, err := bus.Subscribe(context.Background(), "run-1", tt.sinceSeq)
			if err != nil {
				t.Fatalf("Subscribe error: %v", err)
			}
			defer sub.Unsubscribe()
			for _, want := range tt.wantSeqs {
				if ev := recv(t, sub); ev.Seq != want {
					t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
				}
			}
			expectNoEvent(t, sub)
		})
	}
}

func TestSubscribeReceivesLiveEvents(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()

	sub, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer sub.Unsubscribe()

	seqs := publishN(t, bus, "run-1", 2)
	for i, want := range seqs {
		ev := recv(t, sub)
		if ev.Seq != want || ev.Type != "delta" {
			t.Fatalf("live event %d = %+v, want seq %d type delta", i, ev, want)
		}
	}
}

func TestSubscribeLiveOnlySkipsHistory(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()
	publishN(t, bus, "run-1", 3)

	// sinceSeq < 0 subscribes live-only: no history is replayed.
	sub, err := bus.Subscribe(context.Background(), "run-1", -1)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer sub.Unsubscribe()
	expectNoEvent(t, sub)
}

func TestSubscribeLiveOnlyReceivesNewEvents(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()
	publishN(t, bus, "run-1", 2)

	sub, err := bus.Subscribe(context.Background(), "run-1", -1)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer sub.Unsubscribe()

	seqs := publishN(t, bus, "run-1", 2)
	for i, want := range seqs {
		if ev := recv(t, sub); ev.Seq != want {
			t.Fatalf("live event %d seq = %d, want %d", i, ev.Seq, want)
		}
	}
	expectNoEvent(t, sub)
}

func TestRetentionTruncatesHistory(t *testing.T) {
	bus := NewLocalWithRetention(3)
	defer bus.Close()
	publishN(t, bus, "run-1", 5)

	// Only the newest 3 events survive the retention cap.
	sub, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer sub.Unsubscribe()
	for _, want := range []int64{3, 4, 5} {
		if ev := recv(t, sub); ev.Seq != want {
			t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
		}
	}
	expectNoEvent(t, sub)
}

func TestRetentionBoundary(t *testing.T) {
	tests := []struct {
		name     string
		publish  int
		wantSeqs []int64
	}{
		{"exactly N retained in full", 3, []int64{1, 2, 3}},
		{"N+1 drops the oldest", 4, []int64{2, 3, 4}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := NewLocalWithRetention(3)
			defer bus.Close()
			publishN(t, bus, "run-1", tt.publish)

			sub, err := bus.Subscribe(context.Background(), "run-1", 0)
			if err != nil {
				t.Fatalf("Subscribe error: %v", err)
			}
			defer sub.Unsubscribe()
			for _, want := range tt.wantSeqs {
				if ev := recv(t, sub); ev.Seq != want {
					t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
				}
			}
			expectNoEvent(t, sub)
		})
	}
}

func TestRetentionUnboundedByDefault(t *testing.T) {
	// NewLocal and a non-positive retention cap keep full history.
	for name, bus := range map[string]*Local{
		"NewLocal":       NewLocal(),
		"zero retention": NewLocalWithRetention(0),
		"negative cap":   NewLocalWithRetention(-5),
	} {
		t.Run(name, func(t *testing.T) {
			defer bus.Close()
			publishN(t, bus, "run-1", 10)

			sub, err := bus.Subscribe(context.Background(), "run-1", 0)
			if err != nil {
				t.Fatalf("Subscribe error: %v", err)
			}
			defer sub.Unsubscribe()
			for want := int64(1); want <= 10; want++ {
				if ev := recv(t, sub); ev.Seq != want {
					t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
				}
			}
		})
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()

	publishN(t, bus, "run-1", 1)
	sub, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	recv(t, sub) // drain replay

	sub.Unsubscribe()
	if _, ok := <-sub.C(); ok {
		t.Fatal("channel should be closed after Unsubscribe")
	}

	publishN(t, bus, "run-1", 3) // must not panic or deliver
	if _, ok := <-sub.C(); ok {
		t.Fatal("channel should stay closed after further publishes")
	}

	sub.Unsubscribe() // idempotent, must not panic
}

func TestSlowSubscriberDropsWithoutBlockingPublish(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()

	// Empty history at subscribe time: buffer is exactly defaultBuffer.
	sub, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer sub.Unsubscribe()

	const total = defaultBuffer + 36
	done := make(chan struct{})
	go func() {
		defer close(done)
		publishN(t, bus, "run-1", total)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}

	// Only the first defaultBuffer events fit; the rest are dropped. The
	// client is expected to recover the gap via a sinceSeq replay.
	for want := int64(1); want <= defaultBuffer; want++ {
		if ev := recv(t, sub); ev.Seq != want {
			t.Fatalf("delivered seq = %d, want %d", ev.Seq, want)
		}
	}
	expectNoEvent(t, sub)

	// A fresh subscriber can replay everything, including the dropped tail.
	replay, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer replay.Unsubscribe()
	for want := int64(1); want <= total; want++ {
		if ev := recv(t, replay); ev.Seq != want {
			t.Fatalf("replayed seq = %d, want %d", ev.Seq, want)
		}
	}
}

func TestClose(t *testing.T) {
	bus := NewLocal()
	publishN(t, bus, "run-1", 1)
	sub, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	recv(t, sub) // drain replay

	if err := bus.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if _, ok := <-sub.C(); ok {
		t.Fatal("active subscription channel should be closed on Close")
	}

	if _, err := bus.Publish(context.Background(), "run-1", Event{Type: "delta"}); !errors.Is(err, ErrClosed) {
		t.Fatalf("Publish after Close = %v, want ErrClosed", err)
	}
	if _, err := bus.Subscribe(context.Background(), "run-1", 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("Subscribe after Close = %v, want ErrClosed", err)
	}
	if err := bus.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}

	sub.Unsubscribe() // after Close: no-op, must not panic
}

func TestCanceledContext(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := bus.Publish(ctx, "run-1", Event{Type: "delta"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish with canceled ctx = %v, want context.Canceled", err)
	}
	if _, err := bus.Subscribe(ctx, "run-1", 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe with canceled ctx = %v, want context.Canceled", err)
	}
}

func TestConcurrentPublishSubscribe(t *testing.T) {
	bus := NewLocal()
	defer bus.Close()

	const (
		publishers       = 8
		eventsPerPub     = 50
		subscriberChurns = 16
	)

	// One long-lived reader keeps draining so the race detector sees real
	// fanout traffic.
	reader, err := bus.Subscribe(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatalf("Subscribe error: %v", err)
	}
	defer reader.Unsubscribe()
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case _, ok := <-reader.C():
				if !ok {
					return
				}
			case <-stop:
				return
			}
		}
	}()

	seqCh := make(chan int64, publishers*eventsPerPub)
	var wg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			for i := 0; i < eventsPerPub; i++ {
				seq, err := bus.Publish(context.Background(), "run-1", Event{Type: "delta", Data: p})
				if err != nil {
					t.Errorf("Publish error: %v", err)
					return
				}
				seqCh <- seq
			}
		}(p)
	}
	// Concurrent subscribe/unsubscribe churn.
	for s := 0; s < subscriberChurns; s++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sub, err := bus.Subscribe(context.Background(), "run-1", 0)
			if err != nil {
				t.Errorf("Subscribe error: %v", err)
				return
			}
			sub.Unsubscribe()
		}()
	}
	wg.Wait()
	close(stop)
	readerWG.Wait()

	seqs := make([]int64, 0, publishers*eventsPerPub)
	for i := 0; i < publishers*eventsPerPub; i++ {
		seqs = append(seqs, <-seqCh)
	}
	slices.Sort(seqs)
	for i, seq := range seqs {
		if seq != int64(i)+1 {
			t.Fatalf("seqs not a 1..%d permutation: got %d at index %d", publishers*eventsPerPub, seq, i)
		}
	}
}

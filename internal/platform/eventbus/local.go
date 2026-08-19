package eventbus

import (
	"context"
	"sync"
)

// defaultBuffer is the base capacity of a subscription channel; the buffer
// additionally grows by the replayed history length at subscribe time.
const defaultBuffer = 64

// topic holds the retained history and live subscribers of one topic.
type topic struct {
	seq     int64
	history []Event
	subs    map[*Subscription]struct{}
}

// Subscription is a single consumer's view of a topic. The channel carries
// replayed history followed by live events and is closed by Unsubscribe or
// when the bus is closed.
type Subscription struct {
	bus   *Local
	topic string
	ch    chan Event
}

// C returns the receive-only event channel.
func (s *Subscription) C() <-chan Event { return s.ch }

// Unsubscribe detaches the subscription and closes its channel. It is
// idempotent and safe to call after the bus is closed.
func (s *Subscription) Unsubscribe() {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	if t, ok := s.bus.topics[s.topic]; ok {
		if _, live := t.subs[s]; live {
			delete(t.subs, s)
			close(s.ch)
		}
	}
}

// Local is the in-process Bus implementation: per-topic history is retained
// in memory until Close, matching the current single-node server behavior.
// Retention is optionally bounded per topic (see NewLocalWithRetention).
type Local struct {
	mu          sync.Mutex
	closed      bool
	topics      map[string]*topic
	maxPerTopic int // <= 0 means unbounded history
}

// NewLocal returns a ready-to-use in-process bus with unbounded per-topic
// history retention.
func NewLocal() *Local {
	return &Local{topics: map[string]*topic{}}
}

// NewLocalWithRetention returns an in-process bus that retains at most
// maxPerTopic history events per topic; maxPerTopic <= 0 means unbounded
// (same as NewLocal).
func NewLocalWithRetention(maxPerTopic int) *Local {
	return &Local{topics: map[string]*topic{}, maxPerTopic: maxPerTopic}
}

var _ Bus = (*Local)(nil)

// topicLocked returns the named topic, creating it on first use.
func (l *Local) topicLocked(name string) *topic {
	t, ok := l.topics[name]
	if !ok {
		t = &topic{subs: map[*Subscription]struct{}{}}
		l.topics[name] = t
	}
	return t
}

// Publish implements Bus. The fan-out send is non-blocking: a subscriber
// whose buffer is full misses the event and must resubscribe with sinceSeq.
func (l *Local) Publish(ctx context.Context, topicName string, ev Event) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return 0, ErrClosed
	}
	t := l.topicLocked(topicName)
	t.seq++
	ev.Seq = t.seq
	t.history = append(t.history, ev)
	if l.maxPerTopic > 0 && len(t.history) > l.maxPerTopic {
		// Bounded retention: drop the oldest events beyond the cap.
		t.history = t.history[len(t.history)-l.maxPerTopic:]
	}
	for sub := range t.subs {
		select {
		case sub.ch <- ev:
		default:
		}
	}
	return t.seq, nil
}

// Subscribe implements Bus. History with seq > sinceSeq is queued into the
// subscription buffer before it goes live, so no event is lost or
// duplicated between replay and fan-out.
func (l *Local) Subscribe(ctx context.Context, topicName string, sinceSeq int64) (*Subscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, ErrClosed
	}
	t := l.topicLocked(topicName)
	if sinceSeq < 0 {
		// Live-only: resolve to the current head under the lock so no event
		// published between now and going live is missed.
		sinceSeq = t.seq
	}
	sub := &Subscription{bus: l, topic: topicName, ch: make(chan Event, len(t.history)+defaultBuffer)}
	for _, ev := range t.history {
		if ev.Seq > sinceSeq {
			sub.ch <- ev
		}
	}
	t.subs[sub] = struct{}{}
	return sub, nil
}

// Close implements Bus. It is idempotent.
func (l *Local) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	for _, t := range l.topics {
		for sub := range t.subs {
			close(sub.ch)
		}
		t.subs = map[*Subscription]struct{}{}
	}
	return nil
}

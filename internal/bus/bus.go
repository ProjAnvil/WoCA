// Package bus provides a minimal in-process message bus for the SendMessage
// tool (team/coordinator-style coordination plumbing).
package bus

import "sync"

// Msg is one addressed message.
type Msg struct {
	From    string
	To      string
	Content string
}

// Bus is a concurrency-safe message store.
type Bus struct {
	mu   sync.Mutex
	msgs []Msg
}

// New creates an empty bus.
func New() *Bus { return &Bus{} }

// Send delivers a message.
func (b *Bus) Send(from, to, content string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.msgs = append(b.msgs, Msg{From: from, To: to, Content: content})
}

// Drain returns and removes all messages addressed to recipient.
func (b *Bus) Drain(recipient string) []Msg {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []Msg
	var rest []Msg
	for _, m := range b.msgs {
		if m.To == recipient || m.To == "*" {
			out = append(out, m)
		} else {
			rest = append(rest, m)
		}
	}
	b.msgs = rest
	return out
}

// All returns a copy of all pending messages.
func (b *Bus) All() []Msg {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Msg(nil), b.msgs...)
}

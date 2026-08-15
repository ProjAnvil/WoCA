package bus

import "testing"

func TestSendAndDrain(t *testing.T) {
	b := New()
	b.Send("alice", "bob", "hello")
	b.Send("alice", "carol", "hi")
	b.Send("bob", "bob", "self")

	got := b.Drain("bob")
	if len(got) != 2 {
		t.Fatalf("drain bob = %+v", got)
	}
	if got[0].Content != "hello" || got[1].Content != "self" {
		t.Fatalf("drain contents = %+v", got)
	}
	// Remaining message is for carol.
	if rest := b.All(); len(rest) != 1 || rest[0].To != "carol" {
		t.Fatalf("remaining = %+v", rest)
	}
	// Wildcard recipient.
	b.Send("x", "*", "broadcast")
	if got := b.Drain("anyone"); len(got) != 1 || got[0].Content != "broadcast" {
		t.Fatalf("wildcard drain = %+v", got)
	}
}

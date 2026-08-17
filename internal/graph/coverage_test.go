package graph

import (
	"context"
	"testing"
)

func TestMemoryCheckpointerRoundTrip(t *testing.T) {
	m := NewMemoryCheckpointer()
	cp := checkpoint{node: "n1", values: State{"k": "v"}}
	if err := m.Put("key1", cp); err != nil {
		t.Fatal(err)
	}
	got, ok := m.Get("key1")
	if !ok || got.node != "n1" || got.values["k"] != "v" {
		t.Fatalf("get = %+v %v", got, ok)
	}
	if _, ok := m.Get("missing"); ok {
		t.Fatal("missing key should miss")
	}
}

// newInterruptingGraph builds a graph whose single node interrupts once.
func newInterruptingGraph(t *testing.T) *CompiledGraph {
	t.Helper()
	g := NewStateGraph()
	g.AddNode("ask", func(ctx context.Context, s State) (any, error) {
		answer, err := Interrupt(ctx, "proceed?")
		if err != nil {
			return nil, err
		}
		return State{"answer": answer}, nil
	})
	g.SetEntryPoint("ask")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	return cg
}

func TestStreamResumeInterrupt(t *testing.T) {
	cg := newInterruptingGraph(t)
	// Stream to the interrupt.
	stream, err := cg.Stream(context.Background(), State{})
	if err != nil {
		t.Fatal(err)
	}
	var ie *InterruptError
	for {
		step, more, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		if candidate, ok := AsInterrupt(step.Err); ok {
			ie = candidate
		}
	}
	if ie == nil {
		t.Fatal("expected an interrupt step")
	}

	// Resume through the streaming path.
	resumed, err := cg.StreamResume(context.Background(), ie, "yes")
	if err != nil {
		t.Fatal(err)
	}
	var answer any
	for {
		step, more, err := resumed.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		if v, ok := step.Update["answer"]; ok {
			answer = v
		}
	}
	if answer != "yes" {
		t.Fatalf("answer = %v", answer)
	}
	if _, err := cg.StreamResume(context.Background(), nil, "x"); err == nil {
		t.Fatal("nil interrupt should error")
	}
}

func TestResumeWithoutCheckpoint(t *testing.T) {
	cg := newInterruptingGraph(t)
	ie := &InterruptError{Value: "x"}
	if _, err := cg.Resume(context.Background(), ie, "y"); err == nil {
		t.Fatal("resume without checkpoint should error")
	}
}

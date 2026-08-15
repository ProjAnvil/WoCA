package graph

import (
	"context"
	"testing"
)

func TestAppendReducer(t *testing.T) {
	got := Append([]int{1, 2}, []int{3})
	want := []int{1, 2, 3}
	if len(got.([]int)) != 3 || got.([]int)[2] != 3 {
		t.Fatalf("Append = %v, want %v", got, want)
	}
	if Append(nil, []int{5}).([]int)[0] != 5 {
		t.Fatalf("Append(nil, ...) should return update")
	}
}

func TestLinearGraph(t *testing.T) {
	g := NewStateGraph()
	g.AddNode("a", func(ctx context.Context, s State) (any, error) {
		return State{"x": 1}, nil
	})
	g.AddNode("b", func(ctx context.Context, s State) (any, error) {
		return State{"x": s["x"].(int) + 10}, nil
	})
	g.SetEntryPoint("a")
	g.AddEdge("a", "b")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cg.Invoke(context.Background(), State{})
	if err != nil {
		t.Fatal(err)
	}
	if out["x"].(int) != 11 {
		t.Fatalf("x = %v, want 11", out["x"])
	}
}

func TestConditionalLoop(t *testing.T) {
	g := NewStateGraph()
	g.AddNode("inc", func(ctx context.Context, s State) (any, error) {
		c := 0
		if v, ok := s["count"]; ok {
			c = v.(int)
		}
		return State{"count": c + 1}, nil
	})
	g.AddConditionalEdges("inc", func(ctx context.Context, s State) (string, error) {
		if s["count"].(int) >= 3 {
			return END, nil
		}
		return "inc", nil
	})
	g.SetEntryPoint("inc")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cg.Invoke(context.Background(), State{"count": 0})
	if err != nil {
		t.Fatal(err)
	}
	if got := out["count"].(int); got != 3 {
		t.Fatalf("count = %d, want 3", got)
	}
}

func TestCommandGoto(t *testing.T) {
	g := NewStateGraph()
	g.AddNode("a", func(ctx context.Context, s State) (any, error) {
		return &Command{Update: State{"x": 1}, Goto: "c"}, nil
	})
	g.AddNode("b", func(ctx context.Context, s State) (any, error) {
		return State{"x": 2}, nil
	})
	g.AddNode("c", func(ctx context.Context, s State) (any, error) {
		return State{"x": s["x"].(int) + 100}, nil
	})
	g.SetEntryPoint("a")
	g.AddEdge("a", "b") // overridden by Command.Goto
	g.AddEdge("b", "c")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cg.Invoke(context.Background(), State{})
	if err != nil {
		t.Fatal(err)
	}
	if out["x"].(int) != 101 {
		t.Fatalf("x = %v, want 101 (Command.Goto must bypass edge a->b)", out["x"])
	}
}

func TestInterruptResume(t *testing.T) {
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

	_, err = cg.Invoke(context.Background(), State{})
	ie, ok := AsInterrupt(err)
	if !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	if ie.Value != "proceed?" {
		t.Fatalf("interrupt value = %v", ie.Value)
	}

	out, err := cg.Resume(context.Background(), ie, "yes")
	if err != nil {
		t.Fatal(err)
	}
	if out["answer"] != "yes" {
		t.Fatalf("answer = %v, want yes", out["answer"])
	}
}

func TestRecursionLimit(t *testing.T) {
	g := NewStateGraph()
	g.AddNode("loop", func(ctx context.Context, s State) (any, error) {
		return nil, nil
	})
	g.AddConditionalEdges("loop", func(ctx context.Context, s State) (string, error) {
		return "loop", nil
	})
	g.SetEntryPoint("loop")
	cg, err := g.Compile(WithRecursionLimit(5))
	if err != nil {
		t.Fatal(err)
	}
	_, err = cg.Invoke(context.Background(), State{})
	if err == nil {
		t.Fatal("expected recursion limit error")
	}
}

func TestStream(t *testing.T) {
	g := NewStateGraph()
	g.AddNode("a", func(ctx context.Context, s State) (any, error) {
		return State{"x": 1}, nil
	})
	g.SetEntryPoint("a")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := cg.Stream(context.Background(), State{})
	if err != nil {
		t.Fatal(err)
	}
	var sawNode, sawDone bool
	for {
		step, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if step.Node == "a" {
			sawNode = true
			if step.Values["x"] != 1 {
				t.Fatalf("stream values = %v", step.Values)
			}
		}
		if step.Done {
			sawDone = true
			if step.Values["x"] != 1 {
				t.Fatalf("terminal values = %v", step.Values)
			}
		}
	}
	if !sawNode || !sawDone {
		t.Fatalf("stream missing events: node=%v done=%v", sawNode, sawDone)
	}
}

func TestStreamingNodeEmitsDeltas(t *testing.T) {
	g := NewStateGraph()
	g.AddStreamingNode("model", func(ctx context.Context, state State, emit func(any)) (any, error) {
		emit("hello ")
		emit("world")
		return State{"text": "hello world"}, nil
	})
	g.SetEntryPoint("model")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	stream, err := cg.Stream(context.Background(), State{})
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	var sawDone bool
	for {
		step, ok, err := stream.Next(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		if step.Done {
			sawDone = true
			if step.Values["text"] != "hello world" {
				t.Fatalf("terminal values = %v", step.Values)
			}
			continue
		}
		if step.Delta != nil {
			deltas = append(deltas, step.Delta.(string))
		}
	}
	if !sawDone {
		t.Fatal("expected terminal step")
	}
	if len(deltas) != 2 || deltas[0] != "hello " || deltas[1] != "world" {
		t.Fatalf("deltas = %v", deltas)
	}
}

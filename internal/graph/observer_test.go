package graph

import (
	"context"
	"errors"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
)

type recordingObserver struct {
	events []NodeEvent
}

func (o *recordingObserver) ObserveNode(ev NodeEvent) { o.events = append(o.events, ev) }

func TestObserverSeesDispatchLifecycle(t *testing.T) {
	obs := &recordingObserver{}
	g := NewStateGraph()
	g.AddReducer("messages", MessagesReducerForTest())
	g.AddStreamingNode("model", func(_ context.Context, _ State, emit func(any)) (any, error) {
		emit("delta")
		return State{"messages": []messages.Message{messages.AI("hi")}}, nil
	})
	g.AddConditionalEdges("model", func(_ context.Context, _ State) (string, error) {
		return END, nil
	})
	g.SetEntryPoint("model")
	cg, err := g.Compile(WithRecursionLimit(5), WithObserver("test", obs))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cg.Invoke(context.Background(), State{"messages": []messages.Message{messages.Human("q")}}); err != nil {
		t.Fatal(err)
	}

	if len(obs.events) != 2 {
		t.Fatalf("events = %d (%+v), want 2 (start + end)", len(obs.events), obs.events)
	}
	start, end := obs.events[0], obs.events[1]
	if start.Graph != "test" || start.Node != "model" || start.Phase != PhaseStart {
		t.Fatalf("start event = %+v", start)
	}
	if len(start.Values["messages"].([]messages.Message)) != 1 {
		t.Fatalf("start values not carried: %+v", start.Values)
	}
	if end.Phase != PhaseEnd || end.DurationMS < 0 {
		t.Fatalf("end event = %+v", end)
	}
	if st, ok := end.Update.(State); !ok || len(st["messages"].([]messages.Message)) != 1 {
		t.Fatalf("end update not carried: %+v", end.Update)
	}
}

func TestObserverSeesInterruptAndError(t *testing.T) {
	obs := &recordingObserver{}
	g := NewStateGraph()
	g.AddNode("ask", func(ctx context.Context, _ State) (any, error) {
		_, err := Interrupt(ctx, "approve?")
		return nil, err
	})
	g.AddNode("boom", func(context.Context, State) (any, error) {
		return nil, errors.New("kaboom")
	})
	g.SetEntryPoint("ask")
	cg, _ := g.Compile(WithObserver("t", obs))

	_, err := cg.Invoke(context.Background(), nil)
	if _, ok := AsInterrupt(err); !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	last := obs.events[len(obs.events)-1]
	if last.Phase != PhaseInterrupt || last.Err == "" {
		t.Fatalf("interrupt event = %+v", last)
	}

	// A plain error surfaces as the error phase.
	obs2 := &recordingObserver{}
	g2 := NewStateGraph()
	g2.AddNode("boom", func(context.Context, State) (any, error) {
		return nil, errors.New("kaboom")
	})
	g2.SetEntryPoint("boom")
	cg2, _ := g2.Compile(WithObserver("t", obs2))
	_, err = cg2.Invoke(context.Background(), nil)
	if err == nil || err.Error() != "kaboom" {
		t.Fatalf("err = %v", err)
	}
	if last := obs2.events[len(obs2.events)-1]; last.Phase != PhaseError {
		t.Fatalf("error event = %+v", last)
	}
}

// MessagesReducerForTest reuses the agent's reducer semantics without an
// import cycle (the reducer itself lives in internal/agent).
func MessagesReducerForTest() Reducer {
	return func(existing, update any) any {
		var base []messages.Message
		if existing != nil {
			base, _ = existing.([]messages.Message)
		}
		add, _ := update.([]messages.Message)
		return append(append([]messages.Message(nil), base...), add...)
	}
}

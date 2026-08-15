package graph

import (
	"context"
	"testing"
)

func TestFileCheckpointerRoundTrip(t *testing.T) {
	f := NewFileCheckpointer(t.TempDir())
	cp := checkpoint{
		node:   "ask",
		values: State{"name": "alice", "count": 42},
	}
	if err := f.Put("checkpoint-1", cp); err != nil {
		t.Fatal(err)
	}

	got, ok := f.Get("checkpoint-1")
	if !ok {
		t.Fatal("Get returned ok=false for a stored key")
	}
	if got.node != "ask" {
		t.Fatalf("node = %q, want %q", got.node, "ask")
	}
	if got.values["name"] != "alice" {
		t.Fatalf("values[name] = %v, want alice", got.values["name"])
	}
	// JSON numbers decode to float64.
	if n, ok := got.values["count"].(float64); !ok || n != 42 {
		t.Fatalf("values[count] = %v (%T), want float64(42)", got.values["count"], got.values["count"])
	}
}

func TestFileCheckpointerMissingKey(t *testing.T) {
	f := NewFileCheckpointer(t.TempDir())
	if _, ok := f.Get("missing"); ok {
		t.Fatal("Get returned ok=true for a missing key")
	}
}

func TestFileCheckpointerSanitizesKey(t *testing.T) {
	f := NewFileCheckpointer(t.TempDir())
	cp := checkpoint{node: "n", values: State{"v": "kept"}}
	key := "dir/sub:key?" // characters that are unsafe in a file name
	if err := f.Put(key, cp); err != nil {
		t.Fatal(err)
	}
	got, ok := f.Get(key)
	if !ok {
		t.Fatal("Get returned ok=false for a sanitized key")
	}
	if got.node != "n" || got.values["v"] != "kept" {
		t.Fatalf("got %+v, want node=n values[v]=kept", got)
	}
}

func TestResumeCheckpoint(t *testing.T) {
	g := NewStateGraph()
	g.AddNode("ask", func(ctx context.Context, s State) (any, error) {
		answer, err := Interrupt(ctx, "proceed?")
		if err != nil {
			return nil, err
		}
		return State{"answer": answer, "seen": s["seen"]}, nil
	})
	g.SetEntryPoint("ask")

	cg, err := g.Compile(WithCheckpointer(NewFileCheckpointer(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cg.Invoke(context.Background(), State{"seen": "before"})
	ie, ok := AsInterrupt(err)
	if !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	if ie.Key == "" {
		t.Fatal("expected a checkpoint key on the interrupt")
	}

	out, err := cg.ResumeCheckpoint(context.Background(), ie.Key, "yes")
	if err != nil {
		t.Fatal(err)
	}
	if out["answer"] != "yes" {
		t.Fatalf("answer = %v, want yes", out["answer"])
	}
	if out["seen"] != "before" {
		t.Fatalf("seen = %v, want before", out["seen"])
	}
}

func TestResumeCheckpointErrors(t *testing.T) {
	// No checkpointer configured.
	g := NewStateGraph()
	g.AddNode("ask", func(ctx context.Context, s State) (any, error) {
		_, err := Interrupt(ctx, "proceed?")
		return nil, err
	})
	g.SetEntryPoint("ask")
	cg, err := g.Compile()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cg.ResumeCheckpoint(context.Background(), "k", "v"); err == nil {
		t.Fatal("expected error when no checkpointer is configured")
	}

	// Missing checkpoint key.
	cg2, err := NewStateGraph().
		AddNode("ask", func(ctx context.Context, s State) (any, error) {
			_, err := Interrupt(ctx, "proceed?")
			return nil, err
		}).
		SetEntryPoint("ask").
		Compile(WithCheckpointer(NewFileCheckpointer(t.TempDir())))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cg2.ResumeCheckpoint(context.Background(), "missing", "v"); err == nil {
		t.Fatal("expected error for a missing checkpoint key")
	}
}

package graph

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/wocadb"
)

func newSQLCheckpointer(t *testing.T) *SQLCheckpointer {
	t.Helper()
	db, err := wocadb.Open("sqlite:" + filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return NewSQLCheckpointer(db.DB, string(db.Dialect))
}

func TestSQLCheckpointerRoundTrip(t *testing.T) {
	s := newSQLCheckpointer(t)
	cp := checkpoint{
		node:   "tools",
		values: State{"messages": []messages.Message{messages.Human("hi"), messages.AI("hello")}},
	}
	if err := s.Put("cp-1", cp); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Get("cp-1")
	if !ok {
		t.Fatal("Get returned ok=false")
	}
	if got.node != "tools" {
		t.Fatalf("node = %q", got.node)
	}
	// The messages channel must come back typed, not as []any.
	msgs, ok := got.values["messages"].([]messages.Message)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages not restored: %T", got.values["messages"])
	}
	if msgs[0].Content != "hi" || msgs[1].Content != "hello" {
		t.Fatalf("messages content mismatch: %+v", msgs)
	}
}

func TestSQLCheckpointerMissing(t *testing.T) {
	s := newSQLCheckpointer(t)
	if _, ok := s.Get("missing"); ok {
		t.Fatal("Get returned ok=true for a missing key")
	}
}

func TestSQLCheckpointerOverwrite(t *testing.T) {
	s := newSQLCheckpointer(t)
	_ = s.Put("k", checkpoint{node: "a", values: State{"v": "1"}})
	if err := s.Put("k", checkpoint{node: "b", values: State{"v": "2"}}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Get("k")
	if got.node != "b" || got.values["v"] != "2" {
		t.Fatalf("overwrite failed: %+v", got)
	}
}

func TestResumeCheckpointSQL(t *testing.T) {
	s := newSQLCheckpointer(t)
	g := NewStateGraph()
	g.AddNode("ask", func(ctx context.Context, st State) (any, error) {
		answer, err := Interrupt(ctx, "proceed?")
		if err != nil {
			return nil, err
		}
		return State{"answer": answer, "seen": st["seen"]}, nil
	})
	g.SetEntryPoint("ask")
	cg, err := g.Compile(WithCheckpointer(s))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cg.Invoke(context.Background(), State{"seen": "before"})
	ie, ok := AsInterrupt(err)
	if !ok {
		t.Fatalf("expected interrupt, got %v", err)
	}
	out, err := cg.ResumeCheckpoint(context.Background(), ie.Key, "yes")
	if err != nil {
		t.Fatal(err)
	}
	if out["answer"] != "yes" || out["seen"] != "before" {
		t.Fatalf("resume state = %v", out)
	}
}

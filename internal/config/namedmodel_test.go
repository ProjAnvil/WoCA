package config

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/tools"
)

// bindStub is a chat model whose BindTools returns a fresh instance the way
// the partner adapters do (the bound copy carries no name of its own).
type bindStub struct {
	language.ChatModel
	binds int
}

func (b *bindStub) BindTools(_ []tools.Tool) (language.ChatModel, error) {
	b.binds++
	return &bindStub{}, nil
}

// TestNamedModelKeepsNameThroughBindTools: the agent calls the BOUND copy
// (agent.go binds tools before every run), and the name must survive that
// hop — chat-span attributes (gen_ai.request.model) and the per-workspace
// prompt caches key on it.
func TestNamedModelKeepsNameThroughBindTools(t *testing.T) {
	stub := &bindStub{}
	m := withName(stub, "claude-sonnet-4-5")

	bound, err := m.BindTools(nil)
	if err != nil {
		t.Fatal(err)
	}
	n, ok := bound.(interface{ Name() string })
	if !ok {
		t.Fatal("bound model lost Name()")
	}
	if n.Name() != "claude-sonnet-4-5" {
		t.Fatalf("bound model name = %q", n.Name())
	}
	// The inner adapter really got the bind call exactly once.
	if stub.binds != 1 {
		t.Fatalf("inner BindTools calls = %d", stub.binds)
	}
}

// TestWithNamePassesThroughAlreadyNamed: a model that already reports a
// stable name must not be double-wrapped.
func TestWithNamePassesThroughAlreadyNamed(t *testing.T) {
	stub := &namedStub{}
	if m := withName(stub, "other"); m != language.ChatModel(stub) {
		t.Fatal("withName rewrapped an already-named model")
	}
}

type namedStub struct {
	language.ChatModel
}

func (namedStub) Name() string { return "own-name" }

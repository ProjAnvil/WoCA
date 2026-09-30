package model

import (
	"testing"

	"github.com/projanvil/langchain-golang/core/language"
)

// nameStub reports a configured name; the partner adapters do not.
type nameStub struct {
	language.ChatModel
}

func (nameStub) Name() string { return "claude-sonnet-4-5" }

// TestRouterReportsPrimaryName: displays and chat-span attributes probe
// Name() on the model they hold — the Router must not hide the primary's
// configured id (the fallback swaps underneath, the identity stays).
func TestRouterReportsPrimaryName(t *testing.T) {
	r, err := New(Config{Primary: nameStub{}})
	if err != nil {
		t.Fatal(err)
	}
	n, ok := language.ChatModel(r).(interface{ Name() string })
	if !ok {
		t.Fatal("Router does not implement Name()")
	}
	if n.Name() != "claude-sonnet-4-5" {
		t.Fatalf("Router name = %q", n.Name())
	}
}

// TestRouterNameFallbackUnnamed: an unnamed primary degrades to the
// adapter's type name, matching Agent.CurrentModelName's fallback.
func TestRouterNameFallbackUnnamed(t *testing.T) {
	r, err := New(Config{Primary: unnamedStub{}})
	if err != nil {
		t.Fatal(err)
	}
	n := r.Name()
	if n == "" {
		t.Fatal("Router.Name() returned empty for an unnamed primary")
	}
}

type unnamedStub struct {
	language.ChatModel
}

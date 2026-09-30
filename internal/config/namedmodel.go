package config

import (
	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/tools"
)

// namedModel wraps a built chat model so it reports its configured model id
// through Name(). The agent uses the name to key per-workspace system-prompt
// caches and to name the model in the prompt, so swapping models (same
// provider, different id) must produce a different name. Embedding the
// interface passes every capability through; no call site type-asserts the
// concrete partner adapters.
type namedModel struct {
	language.ChatModel
	name string
}

func (m namedModel) Name() string { return m.name }

// Unwrap returns the wrapped chat model; see model.Router.Unwrap for why
// capability interfaces need unwrapping.
func (m namedModel) Unwrap() language.ChatModel { return m.ChatModel }

// BindTools re-wraps the bound copy: the partner adapters return a fresh
// model from BindTools, and the agent runs that bound instance — without
// this the configured name (prompt caches, chat-span attributes) is lost on
// exactly the model the spans are recorded from.
func (m namedModel) BindTools(bound []tools.Tool) (language.ChatModel, error) {
	b, err := m.ChatModel.BindTools(bound)
	if err != nil {
		return nil, err
	}
	return namedModel{ChatModel: b, name: m.name}, nil
}

// withName wraps m when it does not already report a stable name.
func withName(m language.ChatModel, name string) language.ChatModel {
	if _, ok := m.(interface{ Name() string }); ok {
		return m
	}
	return namedModel{ChatModel: m, name: name}
}

package config

import "github.com/projanvil/langchain-golang/core/language"

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

// withName wraps m when it does not already report a stable name.
func withName(m language.ChatModel, name string) language.ChatModel {
	if _, ok := m.(interface{ Name() string }); ok {
		return m
	}
	return namedModel{ChatModel: m, name: name}
}

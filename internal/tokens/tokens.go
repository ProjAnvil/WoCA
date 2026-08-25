// Package tokens provides token-count estimators for message lists.
// EstimateWith prefers the model's own counter (langchain v0.6: OpenAI
// models approximate tiktoken; others fall back to the default BPE-ish
// approximation); Estimate/EstimateText keep the word + punctuation + CJK
// heuristic for call sites without a model at hand.
package tokens

import (
	"encoding/json"
	"unicode"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
)

// EstimateWith approximates the token count of a message list using the
// model's own counter when it exposes one (language.GetNumTokensFromMessages
// — approximate tiktoken on OpenAI, the default approximation elsewhere) and
// the heuristic otherwise.
func EstimateWith(model any, msgs []messages.Message) int {
	if model == nil {
		return Estimate(msgs)
	}
	if n, err := language.GetNumTokensFromMessages(model, msgs); err == nil {
		return n
	}
	return Estimate(msgs)
}

// EstimateTextWith is EstimateWith for one string.
func EstimateTextWith(model any, s string) int {
	if model == nil {
		return EstimateText(s)
	}
	return language.GetNumTokens(model, s)
}

// EstimateText approximates the token count of a string. Words count as one
// token each, standalone punctuation/operators count as one, and each CJK
// character counts as one (mirroring how BPE tokenizers handle CJK).
func EstimateText(s string) int {
	total := 0
	inWord := false
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			inWord = false
		case unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r):
			total++ // each CJK char ~ one token
			inWord = false
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			if !inWord {
				total++
				inWord = true
			}
		default:
			total++ // punctuation / operator
			inWord = false
		}
	}
	return total
}

// Estimate approximates the token count of a message list (content plus
// tool-call name and serialized arguments).
func Estimate(msgs []messages.Message) int {
	total := 0
	for _, m := range msgs {
		total += EstimateText(m.Content)
		for _, tc := range m.ToolCalls {
			total += EstimateText(tc.Name)
			if raw, err := json.Marshal(tc.Args); err == nil {
				total += EstimateText(string(raw))
			}
		}
	}
	return total
}

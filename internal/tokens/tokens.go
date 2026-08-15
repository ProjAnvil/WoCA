// Package tokens provides a token-count estimator for message lists. It is a
// heuristic (word + punctuation + CJK based) that is a better approximation
// than the naive characters/4 rule, without requiring an external BPE
// tokenizer.
package tokens

import (
	"encoding/json"
	"unicode"

	"github.com/projanvil/langchain-golang/core/messages"
)

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

// Package tokens provides token-count estimators for message lists.
//
// EstimateWith prefers the model's own counters (langchain v0.6: OpenAI
// models count with real tiktoken BPE, including the per-message overhead
// and OpenAI's image token formula), unwrapping woca's Router/namedModel
// wrappers to reach them. Counters backed by provider APIs — the Anthropic
// partner's GetNumTokensFromMessages calls POST /messages/count_tokens —
// are deliberately skipped: estimate paths run on every compaction check
// and context breakdown and must stay offline.
//
// The model-less estimators mirror the structure of langchain's
// CountTokensApproximately (core/messages/trim.go): per-message role/name
// overhead, tool calls and tool-call IDs, a fixed image penalty, float
// accumulation with a single final ceil, and calibration against
// provider-reported usage (usage-metadata scaling, clamped to [1.0, 1.25]).
// The tokenizer base is woca's CJK-aware heuristic rather than the
// framework's chars/4: Chinese text tokenizes at roughly one token per
// character, which chars/4 underestimates several-fold and the 1.25 scaling
// clamp cannot repair.
package tokens

import (
	"encoding/json"
	"math"
	"unicode"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
)

// imageTokenPenalty mirrors CountTokensApproximately's tokensPerImage: a
// fixed per-image charge in place of sizing the image.
const imageTokenPenalty = 85

// chatModelUnwrapper is implemented by woca's model wrappers (model.Router,
// config's namedModel) to expose the wrapped chat model.
type chatModelUnwrapper interface{ Unwrap() language.ChatModel }

// unwrapChatModel strips woca's model wrappers down to the partner chat
// model, so optional capability interfaces (token counters) that the
// ChatModel interface does not declare become reachable.
func unwrapChatModel(model any) any {
	for model != nil {
		u, ok := model.(chatModelUnwrapper)
		if !ok {
			break
		}
		next := u.Unwrap()
		if next == nil {
			break
		}
		model = next
	}
	return model
}

// EstimateWith approximates the token count of a message list using the
// model's own counter when it has a local one (language.TokenCounter +
// language.MessageTokenCounter — real tiktoken on OpenAI models). A model
// whose only counter is provider-API-backed (Anthropic) has no TokenCounter
// and falls back to Estimate, keeping the estimate offline.
func EstimateWith(model any, msgs []messages.Message) int {
	if m := unwrapChatModel(model); m != nil {
		if _, local := m.(language.TokenCounter); local {
			if counter, ok := m.(language.MessageTokenCounter); ok {
				if n, err := counter.GetNumTokensFromMessages(msgs); err == nil {
					return n
				}
			}
		}
	}
	return Estimate(msgs)
}

// EstimateTextWith is EstimateWith for one string: the model's local
// tokenizer (real tiktoken on OpenAI models) when it has one, the heuristic
// otherwise.
func EstimateTextWith(model any, s string) int {
	if m := unwrapChatModel(model); m != nil {
		if counter, ok := m.(language.TokenCounter); ok {
			return len(counter.GetTokenIDs(s))
		}
	}
	return EstimateText(s)
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

// Estimate approximates the token count of a message list with the
// CountTokensApproximately structure (see the package comment): role and
// name overhead per message, tool-call names and serialized arguments,
// tool-call IDs, a fixed image penalty, and usage-metadata scaling against
// the most recent provider-reported total.
func Estimate(msgs []messages.Message) int {
	total := 0.0
	// Usage-metadata scaling state, mirroring CountTokensApproximately: the
	// first AI message's model_provider is adopted (even when absent), any
	// later divergence disables scaling, and the most recent AI message
	// reporting usage calibrates the whole list.
	var (
		aiProvider      string
		hasAIProvider   bool
		invalidProvider bool
		lastAITotal     int
		hasLastAITotal  bool
		approxAtLastAI  float64
	)
	for _, m := range msgs {
		n := EstimateText(wireRole(m.Role) + ": " + m.Content)
		if m.Name != "" {
			n += EstimateText(m.Name)
		}
		for _, block := range m.ContentBlocks {
			switch typed := block.(type) {
			case messages.TextBlock:
				n += EstimateText(typed.Text)
			case messages.ImageBlock:
				n += imageTokenPenalty
			}
		}
		if len(m.ContentBlocks) == 0 {
			// Tool calls ride the content blocks in Anthropic wire format;
			// only the plain-string format needs them counted separately.
			for _, tc := range m.ToolCalls {
				n += EstimateText(tc.Name)
				if raw, err := json.Marshal(tc.Args); err == nil {
					n += EstimateText(string(raw))
				}
			}
		}
		if m.Role == messages.RoleTool && m.ToolCallID != "" {
			n += EstimateText(m.ToolCallID)
		}
		total += float64(n)

		if m.Role == messages.RoleAI {
			provider, _ := m.ResponseMetadata["model_provider"].(string)
			if !hasAIProvider {
				aiProvider, hasAIProvider = provider, true
			} else if provider != aiProvider {
				invalidProvider = true
			}
			if m.UsageMetadata.TotalTokens > 0 {
				lastAITotal = m.UsageMetadata.TotalTokens
				hasLastAITotal = true
				approxAtLastAI = total
			}
		}
	}
	if len(msgs) > 1 && !invalidProvider && aiProvider != "" &&
		hasLastAITotal && approxAtLastAI > 0 {
		factor := float64(lastAITotal) / approxAtLastAI
		total *= math.Min(1.25, math.Max(1.0, factor))
	}
	return int(math.Ceil(total))
}

// wireRole maps a message role to its OpenAI wire spelling, whose length
// feeds the per-message overhead (mirroring approxOpenAIRole).
func wireRole(role messages.Role) string {
	switch role {
	case messages.RoleHuman:
		return "user"
	case messages.RoleAI:
		return "assistant"
	default:
		return string(role)
	}
}

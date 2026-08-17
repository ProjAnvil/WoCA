package agent

import (
	"strings"

	"github.com/projanvil/langchain-golang/core/messages"
)

// TextDelta extracts the incremental text from a streaming chunk. Providers
// deliver text either as chunk.Content or as text/text_delta content blocks.
func TextDelta(chunk messages.Message) string {
	if chunk.Content != "" {
		return chunk.Content
	}
	var b strings.Builder
	for _, block := range chunk.ContentBlocks {
		switch v := block.(type) {
		case messages.TextBlock:
			b.WriteString(v.Text)
		case messages.PlainTextBlock:
			b.WriteString(v.Text)
		}
	}
	return b.String()
}

// ReasoningDeltaText extracts the incremental chain-of-thought text from a
// streaming chunk (Anthropic thinking deltas, DeepSeek/GLM
// reasoning_content), leaving the answer text to TextDelta.
func ReasoningDeltaText(chunk messages.Message) string {
	var b strings.Builder
	for _, block := range chunk.ContentBlocks {
		if rb, ok := block.(messages.ReasoningBlock); ok {
			b.WriteString(rb.Reasoning)
		}
	}
	return b.String()
}

// MergeStreamChunks accumulates a streaming chunk into full. The first chunk
// seeds role/ID/metadata; later chunks append text and merge tool calls by ID.
func MergeStreamChunks(full *messages.Message, chunk messages.Message) {
	if full.Role == "" {
		*full = chunk
		return
	}
	full.Content += TextDelta(chunk)
	full.ContentBlocks = append(full.ContentBlocks, chunk.ContentBlocks...)
	for _, tc := range chunk.ToolCalls {
		mergeToolCall(&full.ToolCalls, tc)
	}
	if chunk.UsageMetadata.TotalTokens > 0 {
		full.UsageMetadata = chunk.UsageMetadata
	}
	if chunk.ResponseMetadata != nil {
		full.ResponseMetadata = chunk.ResponseMetadata
	}
}

// mergeToolCall merges a tool call into calls, matching by ID (or appending).
func mergeToolCall(calls *[]messages.ToolCall, tc messages.ToolCall) {
	if tc.ID != "" {
		for i := range *calls {
			if (*calls)[i].ID == tc.ID {
				if (*calls)[i].Name == "" {
					(*calls)[i].Name = tc.Name
				}
				if (*calls)[i].Args == nil {
					(*calls)[i].Args = map[string]any{}
				}
				for k, v := range tc.Args {
					(*calls)[i].Args[k] = v
				}
				return
			}
		}
	}
	*calls = append(*calls, tc)
}

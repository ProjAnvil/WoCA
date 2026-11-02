package agent

import (
	"maps"
	"slices"
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
// tool-call-delta protocol fragments (ToolCallChunkBlock, fork v0.12+ openai
// chat-completions) are dropped: the terminal chunk carries each call fully
// assembled in ToolCalls, and stray fragments would pollute the persisted
// history, the token estimate, and cross-provider replay.
func MergeStreamChunks(full *messages.Message, chunk messages.Message) {
	chunk.ContentBlocks = dropToolCallFragments(chunk.ContentBlocks)
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

// dropToolCallFragments returns blocks without tool-call stream fragments:
// openai tool_call_chunk slices (fork v0.12+ tool-call-delta protocol) and
// anthropic NonStandard tool_use partials (content_block_start and
// input_json_delta chunks; the complete call arrives in ToolCalls at
// content_block_stop). Both are redundant in the merged message, and the
// anthropic partials would replay as duplicate tool_use blocks on the wire.
// The slice is returned unchanged (no copy) when nothing is dropped.
func dropToolCallFragments(blocks []messages.ContentBlock) []messages.ContentBlock {
	out := blocks
	dropped := false
	for i, block := range blocks {
		if isToolCallFragment(block) {
			if !dropped {
				out = slices.Clone(blocks[:i])
				dropped = true
			}
			continue
		}
		if dropped {
			out = append(out, block)
		}
	}
	return out
}

func isToolCallFragment(block messages.ContentBlock) bool {
	switch v := block.(type) {
	case messages.ToolCallChunkBlock:
		return true
	case messages.NonStandardContentBlock:
		// tool_use partials only: other NonStandard blocks (e.g. woca's own
		// restart markers) must survive the merge.
		return v.Type == "tool_use"
	}
	return false
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
				maps.Copy((*calls)[i].Args, tc.Args)
				return
			}
		}
	}
	*calls = append(*calls, tc)
}

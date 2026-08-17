package agent

// ReasoningDelta is a streamed chain-of-thought fragment: Anthropic extended
// thinking deltas, DeepSeek/GLM reasoning_content. It rides graph Step.Delta
// alongside the plain-string text deltas; consumers that only handle strings
// (the TUI) ignore it, and the web run renderer forwards it as the SSE
// "reasoning" event.
type ReasoningDelta struct {
	Text string
}

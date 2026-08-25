package tokens

import (
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/modelconfig"
	"github.com/projanvil/langchain-golang/partners/anthropic"
	"github.com/projanvil/langchain-golang/partners/openai"
	"github.com/projanvil/woca/internal/model"
)

func TestEstimateText(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello world", 2},
		{"hello world this is a test", 6},
		{"x := foo(bar)", 7}, // x : = foo ( bar ) = 7
		{"if err != nil { return }", 8},
		{"你好世界", 4}, // 4 CJK chars
	}
	for _, c := range cases {
		if got := EstimateText(c.in); got != c.want {
			t.Errorf("EstimateText(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestEstimateMessages(t *testing.T) {
	msgs := []messages.Message{
		messages.Human("hello world"),
		messages.Message{Role: messages.RoleAI, ToolCalls: []messages.ToolCall{
			{Name: "Bash", Args: map[string]any{"command": "ls -la"}},
		}},
	}
	if got := Estimate(msgs); got <= 0 {
		t.Fatalf("Estimate = %d", got)
	}
}

func TestEstimateMonotonic(t *testing.T) {
	short := EstimateText("abc")
	long := EstimateText("abc def ghi jkl mno")
	if long <= short {
		t.Fatalf("longer text should estimate more tokens: %d vs %d", short, long)
	}
}

func FuzzEstimateText(f *testing.F) {
	f.Add("hello world")
	f.Add("if err != nil { return }")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		if n := EstimateText(s); n < 0 {
			t.Fatalf("negative estimate %d for %q", n, s)
		}
	})
}

// TestEstimateWithRealTiktokenThroughWrappers proves the model-aware path
// reaches the OpenAI partner's local tiktoken counter through woca's
// Router/namedModel wrappers (interface embedding does not promote
// capability interfaces, so this is what Unwrap exists for).
func TestEstimateWithRealTiktokenThroughWrappers(t *testing.T) {
	om := openai.NewChatModel(modelconfig.WithModel("gpt-4o"))
	router, err := model.New(model.Config{Primary: om})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	msgs := []messages.Message{
		messages.Human("hello world, this is a tiktoken parity check 你好"),
	}
	want, err := om.GetNumTokensFromMessages(msgs)
	if err != nil {
		t.Fatalf("openai counter: %v", err)
	}
	if got := EstimateWith(router, msgs); got != want {
		t.Fatalf("EstimateWith through Router = %d, want openai counter result %d", got, want)
	}
}

// TestEstimateTextWithMatchesTiktoken asserts the text path uses the model's
// real tokenizer rather than the heuristic on text where the two differ.
func TestEstimateTextWithMatchesTiktoken(t *testing.T) {
	om := openai.NewChatModel(modelconfig.WithModel("gpt-4o"))
	router, err := model.New(model.Config{Primary: om})
	if err != nil {
		t.Fatalf("model.New: %v", err)
	}
	text := "allocator synchronization primitives"
	if got, want := EstimateTextWith(router, text), len(om.GetTokenIDs(text)); got != want {
		t.Fatalf("EstimateTextWith = %d, want tiktoken %d", got, want)
	}
}

// TestEstimateWithAnthropicStaysOffline guards the estimate seam against the
// Anthropic partner's count_tokens API (v0.6.1+ implements
// GetNumTokensFromMessages as a network call): estimates must never leave
// the process, so the anthropic model falls back to the local estimator.
func TestEstimateWithAnthropicStaysOffline(t *testing.T) {
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	am := anthropic.NewChatModel(
		modelconfig.WithModel("claude-3-5-sonnet"),
		modelconfig.WithAPIKey("test-key"),
		modelconfig.WithBaseURL(srv.URL),
	)
	msgs := []messages.Message{messages.Human("hello world")}
	if got, want := EstimateWith(am, msgs), Estimate(msgs); got != want {
		t.Fatalf("EstimateWith(anthropic) = %d, want local estimate %d", got, want)
	}
	if n := requests.Load(); n != 0 {
		t.Fatalf("estimate path made %d HTTP requests, want 0", n)
	}
}

// TestEstimateWithNonGptOpenAIModelFallsBack: the OpenAI message counter
// refuses non-gpt-family model names, so OpenAI-compatible gateways running
// other models fall back to the local estimator.
func TestEstimateWithNonGptOpenAIModelFallsBack(t *testing.T) {
	om := openai.NewChatModel(modelconfig.WithModel("glm-4.7"))
	msgs := []messages.Message{messages.Human("hello world")}
	if got, want := EstimateWith(om, msgs), Estimate(msgs); got != want {
		t.Fatalf("EstimateWith(non-gpt openai) = %d, want fallback estimate %d", got, want)
	}
}

func scalingHistory(withMeta bool, total int) []messages.Message {
	human := messages.Human("a decently long user message about allocator synchronization primitives")
	ai := messages.AI("a reasonably sized assistant reply touching the same topic")
	if withMeta {
		ai.ResponseMetadata = map[string]any{"model_provider": "openai"}
		ai.UsageMetadata = messages.UsageMetadata{TotalTokens: total}
	}
	return []messages.Message{human, ai}
}

// TestEstimateUsageScalingClampedHigh: a provider-reported total far above
// the approximation engages the scaling clamp at 1.25.
func TestEstimateUsageScalingClampedHigh(t *testing.T) {
	raw := Estimate(scalingHistory(false, 0))
	got := Estimate(scalingHistory(true, raw*10))
	if want := int(math.Ceil(float64(raw) * 1.25)); got != want {
		t.Fatalf("Estimate with huge reported total = %d, want clamped %d (raw %d)", got, want, raw)
	}
}

// TestEstimateUsageScalingPartialFactor: a reported total only slightly
// above the approximation scales the estimate up to it.
func TestEstimateUsageScalingPartialFactor(t *testing.T) {
	raw := Estimate(scalingHistory(false, 0))
	got := Estimate(scalingHistory(true, raw+5))
	if want := int(math.Ceil(float64(raw + 5))); got != want {
		t.Fatalf("Estimate with partial factor = %d, want %d (raw %d)", got, want, raw)
	}
}

// withoutUsage strips usage metadata so the same list estimates unscaled.
func withoutUsage(msgs []messages.Message) []messages.Message {
	out := append([]messages.Message(nil), msgs...)
	for i := range out {
		out[i].UsageMetadata = messages.UsageMetadata{}
	}
	return out
}

// TestEstimateUsageScalingDisabled: scaling needs a consistent non-empty
// model_provider across AI messages and more than one message.
func TestEstimateUsageScalingDisabled(t *testing.T) {
	raw := Estimate(scalingHistory(false, 0))

	mixed := scalingHistory(true, raw*10)
	third := messages.AI("later turn from another provider")
	third.ResponseMetadata = map[string]any{"model_provider": "anthropic"}
	mixed = append(mixed, third)
	if got, want := Estimate(mixed), Estimate(withoutUsage(mixed)); got != want {
		t.Fatalf("mixed providers should disable scaling: got %d want %d", got, want)
	}

	noProvider := scalingHistory(true, raw*10)
	noProvider[1].ResponseMetadata = nil
	if got := Estimate(noProvider); got != raw {
		t.Fatalf("missing model_provider should disable scaling: got %d want %d", got, raw)
	}

	single := scalingHistory(true, raw*10)[:1]
	if got, want := Estimate(single), Estimate(withoutUsage(single)); got != want {
		t.Fatalf("single message should not scale: got %d want %d", got, want)
	}
}

// TestEstimateImagePenalty: each image block contributes the fixed 85-token
// penalty (mirroring CountTokensApproximately's tokensPerImage).
func TestEstimateImagePenalty(t *testing.T) {
	plain := messages.Human("look at this")
	withImg := messages.Human("look at this")
	withImg.ContentBlocks = []messages.ContentBlock{messages.ImageBlock{}}
	delta := Estimate([]messages.Message{withImg}) - Estimate([]messages.Message{plain})
	if delta != imageTokenPenalty {
		t.Fatalf("image penalty delta = %d, want %d", delta, imageTokenPenalty)
	}
}

// TestEstimateWithNilAndUnknownModels keeps the no-model call sites safe.
func TestEstimateWithNilAndUnknownModels(t *testing.T) {
	msgs := []messages.Message{messages.Human("hello")}
	if got, want := EstimateWith(nil, msgs), Estimate(msgs); got != want {
		t.Fatalf("EstimateWith(nil) = %d, want %d", got, want)
	}
	if got, want := EstimateWith(struct{}{}, msgs), Estimate(msgs); got != want {
		t.Fatalf("EstimateWith(unknown) = %d, want %d", got, want)
	}
	if got, want := EstimateTextWith(nil, "hello world"), EstimateText("hello world"); got != want {
		t.Fatalf("EstimateTextWith(nil) = %d, want %d", got, want)
	}
}

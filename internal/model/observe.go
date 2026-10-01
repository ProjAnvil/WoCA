// Observability decorator for chat models: records one "chat" span per LLM
// call (invoke and stream) with the GenAI semantic-convention attributes, so
// the run/session tags carried by the context reach the OTel sink (and with
// it Langfuse/Jaeger). Semantics are untouched — every call passes straight
// through to the wrapped model.

package model

import (
	"context"
	"fmt"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"

	"github.com/projanvil/woca/internal/tracing"
)

// Observe wraps a chat model with gen_ai tracing spans. Agent.SetModel
// applies it to every model entering an agent (initial build, settings
// swaps, fallback recovery), which is the single seam every deployment
// form shares. Idempotent: a model that already reports spans is returned
// as-is, so restore paths that re-set a previously-installed model keep
// identity (and spans are never recorded twice).
func Observe(m language.ChatModel) language.ChatModel {
	if m == nil {
		return nil
	}
	if _, ok := m.(*ObservedModel); ok {
		return m
	}
	return &ObservedModel{ChatModel: m}
}

// ObservedModel decorates a chat model with "chat" spans. The embedded
// interface passes every capability through; Name/Unwrap are forwarded
// explicitly because they sit outside the interface (token counting walks
// Unwrap chains; the agent keys prompt caches on Name).
type ObservedModel struct {
	language.ChatModel
}

func (o *ObservedModel) Name() string {
	if name := modelName(o.ChatModel); name != "" {
		return name
	}
	// Mirror Agent.CurrentModelName's unnamed fallback so displays keep
	// showing the concrete adapter, not this decorator.
	return fmt.Sprintf("%T", o.ChatModel)
}

func (o *ObservedModel) Unwrap() language.ChatModel { return o.ChatModel }

func (o *ObservedModel) BindTools(bound []tools.Tool) (language.ChatModel, error) {
	m, err := o.ChatModel.BindTools(bound)
	if err != nil {
		return nil, err
	}
	return &ObservedModel{ChatModel: m}, nil
}

func (o *ObservedModel) Invoke(
	ctx context.Context,
	input []messages.Message,
	opts ...runnables.Option,
) (messages.Message, error) {
	sp := chatSpan(ctx, o)
	resp, err := o.ChatModel.Invoke(ctx, input, opts...)
	setUsage(sp, resp)
	sp.End(ctx, err)
	return resp, err
}

func (o *ObservedModel) Stream(
	ctx context.Context,
	input []messages.Message,
	opts ...runnables.Option,
) (runnables.Stream[messages.Message], error) {
	sp := chatSpan(ctx, o)
	st, err := o.ChatModel.Stream(ctx, input, opts...)
	if err != nil {
		sp.End(ctx, err)
		return nil, err
	}
	return &observedStream{Stream: st, sp: sp, ctx: ctx}, nil
}

// InvokeStructured implements language.StructuredCaller the same way the
// Router does: the wrapped model's native JSON-schema path when it has one
// (observed), a plain observed Invoke otherwise — the caller's
// prompt-guided JSON handling stays authoritative on the degrade path.
func (o *ObservedModel) InvokeStructured(ctx context.Context, input []messages.Message, sch schema.Schema) (messages.Message, error) {
	if sc, ok := o.ChatModel.(language.StructuredCaller); ok {
		sp := chatSpan(ctx, o)
		resp, err := sc.InvokeStructured(ctx, input, sch)
		setUsage(sp, resp)
		sp.End(ctx, err)
		return resp, err
	}
	return o.Invoke(ctx, input)
}

// chatSpan starts the per-call "chat" span with the attributes every
// backend maps (Langfuse: operation, request model, usage, error status).
func chatSpan(ctx context.Context, o *ObservedModel) *tracing.Builder {
	sp := tracing.Start(ctx, "chat", tracing.KindLLM).
		Set(tracing.AttrOperationName, "chat")
	if name := modelName(o.ChatModel); name != "" {
		sp = sp.Set(tracing.AttrRequestModel, name)
	}
	return sp
}

func modelName(m language.ChatModel) string {
	if n, ok := m.(interface{ Name() string }); ok {
		return n.Name()
	}
	return ""
}

// setUsage copies the response's token accounting onto the span. Usage
// arrives on the final AI message; zero values are omitted.
func setUsage(sp *tracing.Builder, resp messages.Message) {
	u := resp.UsageMetadata
	if u.InputTokens > 0 {
		sp = sp.Set(tracing.AttrUsageInputTokens, u.InputTokens)
	}
	if u.OutputTokens > 0 {
		sp.Set(tracing.AttrUsageOutputTokens, u.OutputTokens)
	}
}

// observedStream ends the span when the stream completes and carries the
// last non-zero usage seen on AI chunks (providers report totals on the
// final chunk; partials are skipped).
type observedStream struct {
	runnables.Stream[messages.Message]
	sp     *tracing.Builder
	ctx    context.Context
	done   bool
	usage  messages.UsageMetadata
	hasUse bool
}

func (s *observedStream) Next(ctx context.Context) (messages.Message, bool, error) {
	msg, ok, err := s.Stream.Next(ctx)
	if ok && msg.Role == messages.RoleAI && msg.UsageMetadata != (messages.UsageMetadata{}) {
		s.usage, s.hasUse = msg.UsageMetadata, true
	}
	if ok && err == nil {
		return msg, ok, err
	}
	s.finish(err)
	return msg, ok, err
}

func (s *observedStream) Close() error {
	err := s.Stream.Close()
	s.finish(err)
	return err
}

func (s *observedStream) finish(err error) {
	if s.done {
		return
	}
	s.done = true
	if s.hasUse {
		if s.usage.InputTokens > 0 {
			s.sp = s.sp.Set(tracing.AttrUsageInputTokens, s.usage.InputTokens)
		}
		if s.usage.OutputTokens > 0 {
			s.sp = s.sp.Set(tracing.AttrUsageOutputTokens, s.usage.OutputTokens)
		}
	}
	s.sp.End(s.ctx, err)
}

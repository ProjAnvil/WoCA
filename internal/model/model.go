// Package model provides a chat-model Router with retry and fallback behavior,
// mirroring Claude Code's model-fallback on overload/transient errors.
package model

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/language"
	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/langchain-golang/core/runnables"
	"github.com/projanvil/langchain-golang/core/schema"
	"github.com/projanvil/langchain-golang/core/tools"
)

// Router wraps a primary language.ChatModel with retry and optional fallback.
// It implements language.ChatModel.
type Router struct {
	primary    language.ChatModel
	fallback   language.ChatModel
	maxRetries int
	retryable  func(error) bool
	onFallback func(error)

	mu           sync.Mutex
	usedFallback bool
}

// Config configures a Router.
type Config struct {
	Primary    language.ChatModel
	Fallback   language.ChatModel
	MaxRetries int
	Retryable  func(error) bool
	OnFallback func(error)
}

// New builds a Router. MaxRetries defaults to 2 when <= 0.
func New(cfg Config) (*Router, error) {
	if cfg.Primary == nil {
		return nil, fmt.Errorf("model: primary is required")
	}
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 2
	}
	return &Router{
		primary:    cfg.Primary,
		fallback:   cfg.Fallback,
		maxRetries: cfg.MaxRetries,
		retryable:  cfg.Retryable,
		onFallback: cfg.OnFallback,
	}, nil
}

// IsRetryable reports whether an error looks transient (overload/rate/5xx/
// timeout/connection).
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	for _, k := range []string{"overload", "rate", "429", "500", "502", "503", "504", "timeout", "connection", "temporarily"} {
		if strings.Contains(s, k) {
			return true
		}
	}
	return false
}

func (r *Router) retryableFn() func(error) bool {
	if r.retryable != nil {
		return r.retryable
	}
	return func(error) bool { return true }
}

// Invoke retries transient errors, then falls back to the fallback model.
func (r *Router) Invoke(ctx context.Context, msgs []messages.Message, opts ...runnables.Option) (messages.Message, error) {
	resp, err := r.primary.Invoke(ctx, msgs, opts...)
	if err == nil {
		return resp, nil
	}
	retryable := r.retryableFn()
	for attempt := 0; attempt < r.maxRetries; attempt++ {
		if !retryable(err) {
			break
		}
		resp, err = r.primary.Invoke(ctx, msgs, opts...)
		if err == nil {
			return resp, nil
		}
	}
	if r.fallback != nil {
		r.mu.Lock()
		first := !r.usedFallback
		r.usedFallback = true
		r.mu.Unlock()
		if first && r.onFallback != nil {
			r.onFallback(err)
		}
		return r.fallback.Invoke(ctx, msgs, opts...)
	}
	return messages.Message{}, err
}

// Batch invokes each input through Invoke (retaining retry/fallback).
func (r *Router) Batch(ctx context.Context, inputs [][]messages.Message, opts ...runnables.Option) ([]messages.Message, error) {
	out := make([]messages.Message, len(inputs))
	var firstErr error
	for i, in := range inputs {
		resp, err := r.Invoke(ctx, in, opts...)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		out[i] = resp
	}
	return out, firstErr
}

// Stream retries transient errors and falls back to the fallback model,
// mirroring the Invoke path and Claude Code's streaming fallback
// (withRetry.ts + query.ts's FallbackTriggeredError handling): a failed
// attempt is retried on the primary model and, once retries are exhausted,
// retried on the fallback model. When a restart happens after chunks were
// already delivered, the consumer first receives a restart marker chunk so
// it can discard the partial response — the tombstone equivalent for
// Claude Code's orphaned assistant messages.
func (r *Router) Stream(ctx context.Context, input []messages.Message, opts ...runnables.Option) (runnables.Stream[messages.Message], error) {
	return &retryStream{r: r, ctx: ctx, input: input, opts: opts}, nil
}

// RestartBlockType marks a synthetic chunk that signals the stream restarted
// (retry or model fallback) and any previously delivered partial output must
// be discarded by the consumer.
const RestartBlockType = "woca_stream_restart"

// RestartChunk builds the synthetic marker message emitted before the
// retried/fallback attempt's output.
func RestartChunk(reason string) messages.Message {
	m := messages.AI("")
	m.ContentBlocks = []messages.ContentBlock{messages.NonStandardContentBlock{
		Type:  RestartBlockType,
		Value: map[string]any{"reason": reason},
	}}
	return m
}

// IsRestartChunk reports whether chunk is a restart marker emitted by the
// Router's stream retry/fallback logic.
func IsRestartChunk(chunk messages.Message) bool {
	for _, block := range chunk.ContentBlocks {
		if ns, ok := block.(messages.NonStandardContentBlock); ok && ns.Type == RestartBlockType {
			return true
		}
	}
	return false
}

// RestartReason extracts the human-readable reason from a restart marker.
func RestartReason(chunk messages.Message) string {
	for _, block := range chunk.ContentBlocks {
		if ns, ok := block.(messages.NonStandardContentBlock); ok && ns.Type == RestartBlockType {
			if reason, ok := ns.Value["reason"].(string); ok {
				return reason
			}
		}
	}
	return ""
}

// retryStream wraps one model with the Router's retry/fallback policy. The
// first transient error retries the primary (with backoff); after maxRetries
// the stream switches to the fallback model (if any) and retries there. A
// restart after partial output delivers a RestartChunk first.
type retryStream struct {
	r     *Router
	ctx   context.Context
	input []messages.Message
	opts  []runnables.Option

	cur       runnables.Stream[messages.Message]
	onFB      bool // current attempts are on the fallback model
	attempts  int  // attempts used on the current model
	delivered bool
	pending   *messages.Message
	done      bool
}

// retryBaseDelay is the backoff base between stream retries (exponential,
// capped); a variable so tests can shorten it.
var retryBaseDelay = 500 * time.Millisecond

// nextDelay is the backoff before retry number attempts (1-based), capped.
func nextDelay(attempts int) time.Duration {
	d := retryBaseDelay << (attempts - 1)
	if d > 8*time.Second {
		d = 8 * time.Second
	}
	return d
}

// start opens a stream on the current model (primary, then fallback).
func (s *retryStream) start() error {
	m := s.r.primary
	if s.onFB {
		m = s.r.fallback
	}
	stream, err := m.Stream(s.ctx, s.input, s.opts...)
	if err != nil {
		return err
	}
	s.cur = stream
	return nil
}

// switchModel moves attempts to the fallback model, notifying once.
func (s *retryStream) switchModel(lastErr error) bool {
	if s.r.fallback == nil || s.onFB {
		return false
	}
	s.onFB = true
	s.attempts = 0
	s.r.mu.Lock()
	first := !s.r.usedFallback
	s.r.usedFallback = true
	s.r.mu.Unlock()
	if first && s.r.onFallback != nil {
		s.r.onFallback(lastErr)
	}
	return true
}

// Next returns the next chunk, transparently retrying transient errors and
// switching to the fallback model once primary retries are exhausted.
func (s *retryStream) Next(ctx context.Context) (messages.Message, bool, error) {
	for {
		if s.done {
			return messages.Message{}, false, nil
		}
		if s.pending != nil {
			chunk := *s.pending
			s.pending = nil
			return chunk, true, nil
		}
		if s.cur == nil {
			if err := s.start(); err != nil {
				if s.scheduleRetry(err) {
					continue
				}
				return messages.Message{}, false, err
			}
		}
		chunk, ok, err := s.cur.Next(ctx)
		if err == nil {
			if !ok {
				s.done = true
				return messages.Message{}, false, nil
			}
			s.delivered = true
			return chunk, true, nil
		}
		if s.scheduleRetry(err) {
			continue
		}
		return messages.Message{}, false, err
	}
}

// scheduleRetry handles a stream error: for transient errors it schedules a
// retry on the current model (with backoff) or switches to the fallback
// model once retries are exhausted, queueing a restart marker when partial
// output was already delivered. It returns false when the error is fatal
// and must propagate to the consumer.
func (s *retryStream) scheduleRetry(err error) bool {
	if s.ctx.Err() != nil {
		return false // consumer cancelled: no retry
	}
	// A "context canceled" error with a live consumer ctx is NOT a user
	// cancellation: it is the connection layer dying under a request — most
	// commonly a gateway (DeepSeek and similar OpenAI-compatible endpoints)
	// closing a keep-alive connection right after a previous stream ended,
	// resetting the next in-flight request that reused it. Treat it as a
	// transient transport error and retry; only a cancelled consumer ctx is
	// fatal.
	if !s.r.retryableFn()(err) {
		return false
	}
	if s.attempts >= s.r.maxRetries {
		if !s.switchModel(err) {
			return false // no fallback left
		}
	} else {
		s.attempts++
		if d := nextDelay(s.attempts); d > 0 {
			select {
			case <-time.After(d):
			case <-s.ctx.Done():
				return false
			}
		}
	}
	if s.cur != nil {
		_ = s.cur.Close()
		s.cur = nil
	}
	if s.delivered {
		reason := fmt.Sprintf("retrying after error: %v", err)
		if s.onFB {
			reason = fmt.Sprintf("switched to fallback model after error: %v", err)
		}
		marker := RestartChunk(reason)
		s.pending = &marker
	}
	return true
}

// Close closes the underlying stream when one is open.
func (s *retryStream) Close() error {
	if s.cur != nil {
		return s.cur.Close()
	}
	return nil
}

// InputSchema delegates to primary.
func (r *Router) InputSchema() schema.Schema { return r.primary.InputSchema() }

// OutputSchema delegates to primary.
func (r *Router) OutputSchema() schema.Schema { return r.primary.OutputSchema() }

// BindTools binds tools on primary and fallback, returning a new Router.
func (r *Router) BindTools(bound []tools.Tool) (language.ChatModel, error) {
	primary, err := r.primary.BindTools(bound)
	if err != nil {
		return nil, err
	}
	next := &Router{
		primary:      primary,
		maxRetries:   r.maxRetries,
		retryable:    r.retryable,
		onFallback:   r.onFallback,
		usedFallback: r.usedFallback,
	}
	if r.fallback != nil {
		fb, err := r.fallback.BindTools(bound)
		if err != nil {
			return nil, err
		}
		next.fallback = fb
	}
	return next, nil
}

// Capabilities delegates to primary.
func (r *Router) Capabilities() language.ChatModelCapabilities { return r.primary.Capabilities() }

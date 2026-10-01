package sdk

import (
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"
	"github.com/rs/zerolog"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/runner"
	"github.com/projanvil/woca/internal/tracing"
)

// Sentinel errors returned by Ask.
var (
	// ErrSessionClosed: the session was closed; further asks are rejected.
	ErrSessionClosed = errors.New("sdk: session closed")
	// ErrAskInProgress: one turn is still streaming; wait for its result
	// event (or Close) before asking again.
	ErrAskInProgress = errors.New("sdk: previous ask still in progress")
)

// Session is one conversational agent session: the agent is assembled once
// (NewSession) and reused across turns (Ask), with the transcript carried
// between turns. Ask returns one event stream per turn, ending with exactly
// one EventResult. Close cancels any in-flight turn and releases the agent,
// plugin manager, and build-time teardown hooks.
type Session struct {
	opts      Options
	ap        *agentbuild.App
	sessionID string
	observers []EventPlugin

	mu         sync.Mutex
	history    []messages.Message // next turn's input (system stripped)
	busy       bool
	closed     bool
	turnCancel context.CancelFunc
	turnDone   chan struct{}

	// oneShot marks the Query form: the session ends with its single turn
	// (session-end hooks run after the result event, not at Close).
	oneShot bool

	events    <-chan Event // one-shot view, set by Query
	endOnce   sync.Once    // session-end hooks run exactly once
	closeOnce func()
}

// NewSession assembles the agent (model, tools, hooks, skills, MCP,
// permissions — the same assembly the CLI runs) and starts a session. Pair
// with Ask for multi-turn conversations; Query wraps NewSession plus a
// single Ask.
func NewSession(ctx context.Context, opts ...Option) (*Session, error) {
	o := Options{}
	for _, fn := range opts {
		fn(&o)
	}

	log, err := logFor(o)
	if err != nil {
		return nil, err
	}

	// Install telemetry sinks before the build: spans like load_skills are
	// recorded while BuildAgent runs.
	o.installTelemetry()

	// A non-empty prompt marker keeps build-time decisions on the headless
	// posture (tool-use summaries are interactive-UI only).
	ap, err := agentbuild.BuildAgent(ctx, o.buildOptions("sdk-session"), log)
	if err != nil {
		return nil, err
	}

	s := &Session{
		opts:      o,
		ap:        ap,
		sessionID: newID(),
		observers: eventObservers(o.Plugins),
	}
	s.closeOnce = sync.OnceFunc(func() {
		s.mu.Lock()
		s.closed = true
		cancel, done := s.turnCancel, s.turnDone
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if done != nil {
			<-done
		}
		s.endSession()
		s.ap.PluginMgr.Close()
		s.ap.Agent.Close()
		for _, td := range s.ap.Teardown {
			td()
		}
	})
	if fb, err := ap.Agent.RunSessionStart(ctx); err == nil && fb != "" {
		ap.Log.Info().Str("feedback", fb).Msg("session-start hook feedback")
	}
	return s, nil
}

// Query starts one headless agent run and returns its session. The caller
// must drain Events (or call Close) to avoid leaking the run goroutine.
// Query is NewSession plus a single Ask; Events streams that one turn.
func Query(ctx context.Context, prompt string, opts ...Option) (*Session, error) {
	s, err := NewSession(ctx, opts...)
	if err != nil {
		return nil, err
	}
	s.oneShot = true
	evs, err := s.Ask(ctx, prompt)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	s.events = evs
	return s, nil
}

// Events returns the single turn's event stream (the Query form). Sessions
// created with NewSession stream per-turn channels from Ask instead.
func (s *Session) Events() <-chan Event { return s.events }

// Ask runs one conversational turn and returns its event stream, which ends
// with exactly one EventResult and then closes. The turn's transcript feeds
// the next Ask. Only one turn may stream at a time; a second Ask while one
// is in flight returns ErrAskInProgress.
func (s *Session) Ask(ctx context.Context, prompt string) (<-chan Event, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, ErrSessionClosed
	}
	if s.busy {
		s.mu.Unlock()
		return nil, ErrAskInProgress
	}
	s.busy = true
	s.mu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	events := make(chan Event, 64)
	done := make(chan struct{})
	s.mu.Lock()
	s.turnCancel, s.turnDone = cancel, done
	s.mu.Unlock()
	go s.turn(runCtx, cancel, done, events, prompt)
	return events, nil
}

// Close cancels any in-flight turn, runs the session-end hooks (unless the
// one-shot form already did), and releases all resources. Safe to call more
// than once.
func (s *Session) Close() error {
	if s.closeOnce != nil {
		s.closeOnce()
	}
	return nil
}

// turn drives the runner for one ask, converting stream chunks into events.
// It mirrors the CLI's one-shot path (cmd/woca/main.go run, the opts.Prompt
// branch): submit hooks prepend feedback, turns count tool messages + 1,
// the result event mirrors sdkout.Final (with per-turn usage, not the
// session cumulative totals).
func (s *Session) turn(ctx context.Context, cancel context.CancelFunc, done chan struct{}, events chan Event, prompt string) {
	defer close(done)
	defer close(events)
	defer func() {
		cancel()
		s.mu.Lock()
		s.busy = false
		s.turnCancel, s.turnDone = nil, nil
		s.mu.Unlock()
	}()

	a := s.ap.Agent
	runID := newID()
	// Tag every span the run records (chat/invoke_tool/...) with the
	// session/run identity, so an installed TelemetrySink — e.g. the
	// Langfuse exporter — can group spans per run.
	ctx = tracing.WithRun(ctx, runID, s.sessionID)
	dispatchRunEvent(ctx, s.observers, RunEvent{Kind: RunEventStart, SessionID: s.sessionID, RunID: runID, Prompt: prompt, At: time.Now()})

	render := func(chunk lggraph.StreamChunk) {
		if chunk.Mode != lggraph.StreamUpdates {
			return
		}
		updates, _ := chunk.Payload.(map[string]any)
		for _, nodeUpdate := range updates {
			state, _ := nodeUpdate.(map[string]any)
			for _, m := range agent.MessagesFrom(state) {
				switch m.Role {
				case messages.RoleAI:
					if m.Content == "" && len(m.ToolCalls) == 0 {
						continue
					}
					ev := Event{Type: EventAssistant, Message: messageFrom(m)}
					s.observeMessage(ctx, ev.Message, runID)
					send(ctx, events, ev)
				case messages.RoleTool:
					ev := Event{Type: EventUser, Message: messageFrom(m)}
					s.observeMessage(ctx, ev.Message, runID)
					send(ctx, events, ev)
				}
			}
		}
	}

	r := runner.New(a, s.opts.interruptHandler(), render)

	if fb, err := a.RunUserPromptSubmit(ctx, prompt); err == nil && fb != "" {
		prompt = fb + "\n\n" + prompt
	}

	s.mu.Lock()
	hist := make([]messages.Message, 0, len(s.history)+1)
	hist = append(hist, s.history...)
	hist = append(hist, messages.Human(prompt))
	s.mu.Unlock()

	before := a.Usage()
	result, runErr := r.Run(ctx, hist)
	delta := usageDelta(before, a.Usage())

	// The next turn's input history is this transcript minus the leading
	// system block (runner.Run takes history without the system prompt) —
	// the same strip the turn count uses.
	s.mu.Lock()
	stripped := stripSystem(result)
	s.history = stripped
	s.mu.Unlock()

	// Turn count mirrors the CLI's stripSystem(result) + tool-message count:
	// tool messages plus one.
	turns := 1
	for _, m := range stripped {
		if m.Role == messages.RoleTool {
			turns++
		}
	}
	// The result envelope is built from the run error alone, before the
	// session-end hook runs — matching the CLI ordering (cmd/woca/main.go
	// run, the opts.Prompt branch), where sdkout.Final sees the run error and
	// RunSessionEnd only affects the process exit code afterwards.
	final := resultEvent(result, delta, s.ap.Cfg.Model, turns, runErr)
	sendFinal(events, final)
	dispatchRunEvent(context.WithoutCancel(ctx), s.observers, RunEvent{
		Kind: RunEventEnd, SessionID: s.sessionID, RunID: runID, Result: final.Result, At: time.Now(),
	})
	// The one-shot form ends its session with the turn, exactly like the
	// CLI's -p path; NewSession sessions end at Close.
	if s.oneShot {
		s.endSession()
	}
}

// observeMessage dispatches one transcript-message RunEvent to the
// configured plugins (no-op without observers).
func (s *Session) observeMessage(ctx context.Context, msg *Message, runID string) {
	if len(s.observers) == 0 {
		return
	}
	dispatchRunEvent(ctx, s.observers, RunEvent{
		Kind: RunEventMessage, SessionID: s.sessionID, RunID: runID, Message: msg, At: time.Now(),
	})
}

// endSession runs the session-end hooks exactly once (after the single turn
// for the Query form, at Close otherwise).
func (s *Session) endSession() {
	s.endOnce.Do(func() {
		reason := "sdk-session"
		if s.oneShot {
			reason = "oneshot"
		}
		if err := s.ap.Agent.RunSessionEnd(context.WithoutCancel(context.Background()), reason); err != nil {
			s.ap.Log.Error().Err(err).Msg("session-end hook failed")
		}
	})
}

// logFor builds the embedded agent's logger: silent by default (an embedded
// agent must not spam the host process), overridable per option.
func logFor(o Options) (zerolog.Logger, error) {
	w := o.LogOutput
	if w == nil {
		w = io.Discard
	}
	level := o.LogLevel
	if level == "" {
		level = "error"
	}
	lvl, err := logging.ParseLevel(level)
	if err != nil {
		return zerolog.Logger{}, err
	}
	return logging.NewConsole(w, lvl), nil
}

// usageDelta subtracts two cumulative snapshots into per-turn totals.
func usageDelta(before, after agent.UsageTotals) agent.UsageTotals {
	return agent.UsageTotals{
		Requests:         after.Requests - before.Requests,
		InputTokens:      after.InputTokens - before.InputTokens,
		OutputTokens:     after.OutputTokens - before.OutputTokens,
		TotalTokens:      after.TotalTokens - before.TotalTokens,
		CacheReadTokens:  after.CacheReadTokens - before.CacheReadTokens,
		CacheWriteTokens: after.CacheWriteTokens - before.CacheWriteTokens,
	}
}

// stripSystem drops the leading system messages from a transcript.
func stripSystem(ms []messages.Message) []messages.Message {
	for len(ms) > 0 && ms[0].Role == messages.RoleSystem {
		ms = ms[1:]
	}
	return ms
}

// send delivers an event unless the turn was cancelled.
func send(ctx context.Context, events chan Event, ev Event) {
	select {
	case events <- ev:
	case <-ctx.Done():
	}
}

// sendFinal delivers the terminal result event even after cancellation,
// dropping it only when the events buffer is already full.
func sendFinal(events chan Event, ev Event) {
	select {
	case events <- ev:
	default:
	}
}

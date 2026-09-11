package sdk

import (
	"context"
	"io"
	"sync"

	"github.com/projanvil/langchain-golang/core/messages"
	lggraph "github.com/projanvil/langchain-golang/langgraph/graph"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/agentbuild"
	"github.com/projanvil/woca/internal/logging"
	"github.com/projanvil/woca/internal/runner"
)

// Session is one headless run. Events streams until the result event, then
// closes. Close cancels the run (if still going) and releases the agent,
// plugin manager, and build-time teardown hooks.
type Session struct {
	events chan Event

	cancel context.CancelFunc
	done   chan struct{}

	ap        *agentbuild.App
	closeOnce sync.Once
}

// Query starts one headless agent run and returns its session. The caller
// must drain Events (or call Close) to avoid leaking the run goroutine.
func Query(ctx context.Context, prompt string, opts ...Option) (*Session, error) {
	o := Options{}
	for _, fn := range opts {
		fn(&o)
	}

	level, err := logging.ParseLevel("error")
	if err != nil {
		return nil, err
	}
	log := logging.NewConsole(io.Discard, level)

	ap, err := agentbuild.BuildAgent(ctx, o.buildOptions(prompt), log)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithCancel(ctx)
	s := &Session{
		events: make(chan Event, 64),
		cancel: cancel,
		done:   make(chan struct{}),
		ap:     ap,
	}
	go s.run(runCtx, o, prompt)
	return s, nil
}

// Events returns the run's event stream (closed after the result event).
func (s *Session) Events() <-chan Event { return s.events }

// Close cancels the run and releases all resources. Safe to call twice.
func (s *Session) Close() error {
	s.cancel()
	<-s.done
	s.closeOnce.Do(func() {
		s.ap.PluginMgr.Close()
		s.ap.Agent.Close()
		for _, td := range s.ap.Teardown {
			td()
		}
	})
	return nil
}

// run drives the runner, converting stream chunks into events. It mirrors
// the CLI's one-shot path (cmd/woca/main.go run, the opts.Prompt branch):
// submit hooks prepend feedback, turns count tool messages + 1, the result
// event mirrors sdkout.Final.
func (s *Session) run(ctx context.Context, o Options, prompt string) {
	defer close(s.done)
	defer close(s.events)

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
					s.send(ctx, Event{Type: EventAssistant, Message: messageFrom(m)})
				case messages.RoleTool:
					s.send(ctx, Event{Type: EventUser, Message: messageFrom(m)})
				}
			}
		}
	}

	a := s.ap.Agent
	r := runner.New(a, o.interruptHandler(), render)

	if fb, err := a.RunSessionStart(ctx); err == nil && fb != "" {
		s.ap.Log.Info().Str("feedback", fb).Msg("session-start hook feedback")
	}
	if fb, err := a.RunUserPromptSubmit(ctx, prompt); err == nil && fb != "" {
		prompt = fb + "\n\n" + prompt
	}

	history := []messages.Message{messages.Human(prompt)}
	result, runErr := r.Run(ctx, history)
	if endErr := a.RunSessionEnd(context.WithoutCancel(ctx), "oneshot"); endErr != nil && runErr == nil {
		runErr = endErr
	}

	// Turn count mirrors the CLI's stripSystem(result) + tool-message count:
	// drop leading system messages, then count RoleTool and add one.
	counted := result
	for len(counted) > 0 && counted[0].Role == messages.RoleSystem {
		counted = counted[1:]
	}
	turns := 1
	for _, m := range counted {
		if m.Role == messages.RoleTool {
			turns++
		}
	}
	s.send(ctx, resultEvent(result, a.Usage(), s.ap.Cfg.Model, turns, runErr))
}

// send delivers an event unless the run was cancelled.
func (s *Session) send(ctx context.Context, ev Event) {
	select {
	case s.events <- ev:
	case <-ctx.Done():
	}
}

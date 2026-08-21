// Package worker is the Phase 3 execution role (design §5.3/§13): it
// claims runs from the runstate ledger's queue, executes them through the
// agent, mirrors events into run_events, and finishes the ledger row —
// all state in the shared database, so gateway instances and workers
// scale independently.
package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"
	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/platform/eventbus"
	"github.com/projanvil/woca/internal/runstate"
	"github.com/rs/zerolog"
)

// leaseDuration is how long a claimed run's lease lasts before another
// worker may reap it; the heartbeat extends it every interval.
const (
	leaseDuration  = 90 * time.Second
	heartbeatEvery = leaseDuration / 3
)

// Metrics are optional run-lifecycle hooks, wired by the entry point (the
// server edition feeds them from internal/obs; nil fields are no-ops). They
// stay plain funcs so this package carries no observability dependency.
type Metrics struct {
	OnClaim  func()                           // one queued run claimed
	OnFinish func(status string, dur float64) // run reached a terminal state, dur in seconds
}

func (m *Metrics) claim() {
	if m != nil && m.OnClaim != nil {
		m.OnClaim()
	}
}

func (m *Metrics) finish(status string, dur time.Duration) {
	if m != nil && m.OnFinish != nil {
		m.OnFinish(status, dur.Seconds())
	}
}

// Config wires the worker.
type Config struct {
	Ledger  *runstate.Ledger
	Agent   *agent.Agent
	Bus     eventbus.Bus // event fanout (SSE tail reads from run_events)
	Logger  zerolog.Logger
	ID      string   // worker identity for lease ownership
	Metrics *Metrics // optional run-lifecycle hooks (server edition wires obs)
}

// Worker claims and executes runs from the queue.
type Worker struct {
	ledger  *runstate.Ledger
	agent   *agent.Agent
	bus     eventbus.Bus
	log     zerolog.Logger
	id      string
	metrics *Metrics
}

// New returns a Worker over cfg.
func New(cfg Config) *Worker {
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	return &Worker{ledger: cfg.Ledger, agent: cfg.Agent, bus: cfg.Bus, log: cfg.Logger, id: cfg.ID, metrics: cfg.Metrics}
}

// Step claims one queued run and executes it to completion (or returns nil
// when the queue is empty). Events mirror into run_events under the SSE id
// sequence; the lease is heartbeated during execution and released at
// finish.
func (w *Worker) Step(ctx context.Context) error {
	run, err := w.ledger.Claim(ctx, w.id, leaseDuration)
	if err != nil {
		return fmt.Errorf("worker: claim: %w", err)
	}
	if run == nil {
		return nil // queue empty
	}
	w.metrics.claim()
	return w.execute(ctx, run)
}

// execute runs one claimed run to its terminal state.
func (w *Worker) execute(ctx context.Context, run *runstate.Run) error {
	w.log.Info().Str("run", run.RunID).Str("session", run.SessionID).Msg("worker claimed run")
	started := time.Now()

	// Heartbeat the lease while executing; stop when done.
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go w.heartbeat(hbCtx, run.RunID)

	// The prompt rides the run's event stream (the gateway enqueued it as
	// the first event); for the v1 worker it is the config snapshot's
	// prompt field. An empty prompt skips the model turn (the run may have
	// been claimed just for state cleanup).
	prompt := extractPrompt(run.ConfigSnapshot)
	if prompt != "" {
		w.emitAndMirror(ctx, run.RunID, "run_started", map[string]any{})
		history := []messages.Message{messages.Human(prompt)}
		final, err := w.agent.Invoke(ctx, history, run.ThreadID)
		if err != nil {
			w.emitAndMirror(ctx, run.RunID, "error", map[string]any{"message": err.Error()})
			return w.ledger.Finish(ctx, run.RunID, w.id, "error")
		}
		if len(final) > 0 {
			last := final[len(final)-1]
			w.emitAndMirror(ctx, run.RunID, "delta", map[string]any{"text": last.Content})
		}
	}
	w.emitAndMirror(ctx, run.RunID, "done", map[string]any{"cancelled": false})
	w.metrics.finish("done", time.Since(started))
	return w.ledger.Finish(ctx, run.RunID, w.id, "done")
}

// emitAndMirror appends one event to run_events (the SSE id sequence).
func (w *Worker) emitAndMirror(ctx context.Context, runID, kind string, data map[string]any) {
	payload, err := json.Marshal(data)
	if err != nil {
		payload = []byte(`{}`)
	}
	if _, err := w.ledger.Append(ctx, runID, runstate.Event{Kind: kind, Payload: string(payload)}); err != nil {
		w.log.Warn().Str("run", runID).Str("kind", kind).Err(err).Msg("event mirror failed")
	}
}

// heartbeat extends the claimed run's lease until the context stops.
func (w *Worker) heartbeat(ctx context.Context, runID string) {
	ticker := time.NewTicker(heartbeatEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.ledger.Heartbeat(ctx, runID, w.id, leaseDuration); err != nil {
				w.log.Warn().Str("run", runID).Err(err).Msg("lease heartbeat failed")
				return
			}
		}
	}
}

// Run drives Step in a loop until the context is cancelled (the daemon
// mode workers deploy with).
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			if err := w.Step(ctx); err != nil {
				w.log.Error().Err(err).Msg("worker step failed")
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
			}
		}
	}
}

// extractPrompt pulls the prompt from a config snapshot JSON blob.
func extractPrompt(snapshot string) string {
	if snapshot == "" {
		return ""
	}
	var cfg struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal([]byte(snapshot), &cfg); err != nil {
		return ""
	}
	return cfg.Prompt
}

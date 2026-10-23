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
	"github.com/projanvil/woca/internal/settings"
	"github.com/projanvil/woca/internal/tracing"
	"github.com/projanvil/woca/internal/wocadb"
	"github.com/rs/zerolog"
)

// Sessions is the transcript persistence the worker shares with the
// gateways: load the history a dispatched run continues, save the final
// transcript back. Implemented over the same session store layout the
// server uses (JSONL transcripts behind the blobstore seam), so a shared
// session volume makes gateways and workers one conversation.
type Sessions interface {
	Load(id string) []messages.Message
	Save(id string, msgs []messages.Message, workspace string) error
}

// SessionStates is the durable per-session busy signal (the session_state
// table behind the server's listings). The worker marks running on claim
// and lifts the row on finish — ClearIfRun semantics keep a promoted run's
// row safe.
type SessionStates interface {
	MarkRunning(sessionID, runID string)
	Clear(sessionID, runID string)
}

// leaseDuration is how long a claimed run's lease lasts before another
// worker may reap it; the heartbeat extends it every interval.
const (
	leaseDuration  = 90 * time.Second
	heartbeatEvery = leaseDuration / 3
)

// Metrics are optional run-lifecycle hooks, wired by the entry point (the
// enterprise server role feeds them from internal/obs; nil fields are no-ops). They
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
	Metrics *Metrics // optional run-lifecycle hooks (the server role wires obs)
	// Sessions, when set, persists dispatched-run transcripts so a run
	// continues its session's history across processes. Nil disables
	// persistence (the run executes with prompt-only history).
	Sessions Sessions
	// States, when set, marks sessions busy while their dispatched runs
	// execute here, so gateway listings reflect the truth. Nil disables.
	States SessionStates
	// Idle blocks between claim attempts on an empty queue. The msgplane
	// waiter wakes instantly on a NATS dispatch notification (with a poll
	// safety net); nil sleeps one poll interval.
	Idle IdleWaiter
	// DB is the shared settings/runstate database the materialized
	// sandbox_backends table lives in (the nacos pack apply creates it).
	// Nil keeps the pre-sandbox behavior: every run executes locally.
	DB *wocadb.DB
	// SecretFor resolves a sandbox backend's API key by id (the worker
	// counterpart of the config plane's sandbox-api-key:<id> secrets).
	// Nil yields no key.
	SecretFor func(backendID string) string
	// MaxArchiveBytes caps the workspace tar.gz uploaded to / downloaded
	// from a sandbox, in both directions (settings key
	// sandbox-archive-max-bytes; 0 = the settings default of 512 MiB).
	MaxArchiveBytes int64
}

// IdleWaiter blocks a worker between empty claim attempts.
type IdleWaiter interface {
	Wait(ctx context.Context)
}

const idlePollEvery = 200 * time.Millisecond

// Worker claims and executes runs from the queue.
type Worker struct {
	ledger          *runstate.Ledger
	agent           *agent.Agent
	bus             eventbus.Bus
	log             zerolog.Logger
	id              string
	metrics         *Metrics
	sessions        Sessions
	states          SessionStates
	idle            IdleWaiter
	db              *wocadb.DB
	secretFor       func(backendID string) string
	maxArchiveBytes int64
}

// New returns a Worker over cfg.
func New(cfg Config) *Worker {
	if cfg.ID == "" {
		cfg.ID = fmt.Sprintf("worker-%d", time.Now().UnixNano())
	}
	if cfg.MaxArchiveBytes <= 0 {
		cfg.MaxArchiveBytes = settings.DefaultSandboxArchiveMaxBytes
	}
	return &Worker{
		ledger: cfg.Ledger, agent: cfg.Agent, bus: cfg.Bus, log: cfg.Logger,
		id: cfg.ID, metrics: cfg.Metrics, sessions: cfg.Sessions, states: cfg.States,
		idle:            cfg.Idle,
		db:              cfg.DB,
		secretFor:       cfg.SecretFor,
		maxArchiveBytes: cfg.MaxArchiveBytes,
	}
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

	snap, err := runstate.ParseSnapshot(run.ConfigSnapshot)
	if err != nil {
		w.emitAndMirror(ctx, run.RunID, "error", map[string]any{"message": "unreadable run snapshot: " + err.Error()})
		return w.ledger.Finish(ctx, run.RunID, w.id, "error")
	}
	if w.states != nil {
		w.states.MarkRunning(run.SessionID, run.RunID)
	}
	if snap.Prompt != "" {
		w.emitAndMirror(ctx, run.RunID, "run_started", map[string]any{})
		// Restore the run's execution environment: the session workspace the
		// gateway recorded (per-run workspace scoping drives the system
		// prompt environment and every workspace-aware tool), then the
		// session transcript the run continues.
		if snap.Workspace != "" {
			ctx = agent.WithRunWorkspace(ctx, snap.Workspace)
		}
		// Per-run sandbox (design §4.5): when the materialized config
		// carries an enabled backend, provision one, upload the workspace,
		// execute through the run executor, and download the products back.
		// Any failure — including a sandbox-table read error — fails the
		// run explicitly; the worker never silently falls back to local
		// execution.
		sbxDownload, teardown, err := w.provisionRunSandbox(&ctx, snap)
		if teardown != nil {
			defer teardown()
		}
		if err != nil {
			w.emitAndMirror(ctx, run.RunID, "error", map[string]any{"message": err.Error()})
			w.metrics.finish("error", time.Since(started))
			if w.states != nil {
				w.states.Clear(run.SessionID, run.RunID)
			}
			return w.ledger.Finish(ctx, run.RunID, w.id, "error")
		}
		if sbxDownload != nil {
			defer func() { _ = sbxDownload(false) }() // recovery logs its own errors
		}
		// Tag the execution ctx with the run/session identity so every
		// telemetry span the agent records (chat, tools) carries it — in the
		// queue-dispatch form this process is where runs execute, and spans
		// without these tags cannot be grouped by session downstream
		// (Langfuse session.id, per-run filtering).
		ctx = tracing.WithRun(ctx, run.RunID, run.SessionID)
		history := []messages.Message{humanMessage(snap)}
		if w.sessions != nil && snap.SessionID != "" {
			if prev := w.sessions.Load(snap.SessionID); len(prev) > 0 {
				history = append(append([]messages.Message(nil), prev...), humanMessage(snap))
			}
		}
		final, err := w.agent.Invoke(ctx, history, run.ThreadID)
		if err != nil {
			w.emitAndMirror(ctx, run.RunID, "error", map[string]any{"message": err.Error()})
			w.metrics.finish("error", time.Since(started))
			if w.states != nil {
				w.states.Clear(run.SessionID, run.RunID)
			}
			return w.ledger.Finish(ctx, run.RunID, w.id, "error")
		}
		// Recover the products from the sandbox before reporting success:
		// a download past the archive cap (or a broken download) fails the
		// run — the products never reached the host workspace, so "done"
		// would lie.
		if sbxDownload != nil {
			if err := sbxDownload(true); err != nil {
				w.emitAndMirror(ctx, run.RunID, "error", map[string]any{"message": "sandbox download: " + err.Error()})
				w.metrics.finish("error", time.Since(started))
				if w.states != nil {
					w.states.Clear(run.SessionID, run.RunID)
				}
				return w.ledger.Finish(ctx, run.RunID, w.id, "error")
			}
		}
		if len(final) > 0 {
			last := final[len(final)-1]
			w.emitAndMirror(ctx, run.RunID, "delta", map[string]any{"text": last.Content})
			if w.sessions != nil && snap.SessionID != "" {
				if err := w.sessions.Save(snap.SessionID, stripSystem(final), snap.Workspace); err != nil {
					w.log.Warn().Str("session", snap.SessionID).Err(err).Msg("transcript save failed")
				}
			}
		}
	}
	w.emitAndMirror(ctx, run.RunID, "done", map[string]any{"cancelled": false})
	w.metrics.finish("done", time.Since(started))
	if w.states != nil {
		w.states.Clear(run.SessionID, run.RunID)
	}
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
// mode workers deploy with). Between empty claims the idle waiter parks the
// loop (poll cadence, or an instant NATS wake-up when the message plane is
// pg+nats) instead of spinning on the database.
func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			claimed, err := w.stepAndReport(ctx)
			if err != nil {
				w.log.Error().Err(err).Msg("worker step failed")
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(time.Second):
				}
				continue
			}
			if !claimed {
				if w.idle != nil {
					w.idle.Wait(ctx)
				} else {
					select {
					case <-ctx.Done():
						return nil
					case <-time.After(idlePollEvery):
					}
				}
			}
		}
	}
}

// stepAndReport runs one Step and reports whether a run was claimed.
func (w *Worker) stepAndReport(ctx context.Context) (bool, error) {
	run, err := w.ledger.Claim(ctx, w.id, leaseDuration)
	if err != nil {
		return false, fmt.Errorf("worker: claim: %w", err)
	}
	if run == nil {
		return false, nil
	}
	w.metrics.claim()
	return true, w.execute(ctx, run)
}

// humanMessage rebuilds the prompt message from a dispatched snapshot,
// upgrading to multimodal content blocks when image attachments rode along
// (mirroring the gateway's request decoding).
func humanMessage(snap runstate.RunSnapshot) messages.Message {
	if len(snap.Attachments) == 0 {
		return messages.Human(snap.Prompt)
	}
	blocks := []messages.ContentBlock{messages.TextBlock{Text: snap.Prompt}}
	for _, a := range snap.Attachments {
		blocks = append(blocks, messages.ImageBlock{MimeType: a.MimeType, Base64: a.Data})
	}
	return messages.Message{Role: messages.RoleHuman, Content: snap.Prompt, ContentBlocks: blocks}
}

// stripSystem removes system messages from a transcript before persisting
// (the session store keeps the conversation, not the assembled prompt).
func stripSystem(msgs []messages.Message) []messages.Message {
	out := make([]messages.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role != messages.RoleSystem {
			out = append(out, m)
		}
	}
	return out
}

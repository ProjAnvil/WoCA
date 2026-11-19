package server

import (
	"context"

	"github.com/projanvil/langchain-golang/core/messages"
)

// runCompletion captures everything a run's completion path persists with.
// It folds the four former cancel/save seams into one shape: the executing
// run's onDone (main runs and goal runs) and the queued-run cancel branch
// differed only in (a) human non-nil vs nil (goal rounds carry no human
// turn), (b) which history the derivation bases on — the history loaded at
// run start (executing runs) vs a reload at completion time (queued cancels:
// base nil) — and (c) whether the run holds the admission gate (executing
// runs release it; a queued-cancel run never acquired it and must not free
// another run's gate).
type runCompletion struct {
	run       *Run
	sid       string
	workspace string
	// human is the round's prompt message; nil for goal rounds.
	human *messages.Message
	// base is the history the run started from (the derivation base for an
	// executing run's cancel). Nil means reload now — the queued-cancel
	// path, which must rebase on whatever the current holder just
	// persisted.
	base []messages.Message
	// final is the runner's final transcript; nil on cancel/error.
	final []messages.Message
	// partial is the executing run's partial transcript at cancel (spec
	// 2026-11-19-cancel-partial-output §D3): what already streamed, read
	// from the run's accumulator by the onDone closure. Nil for queued
	// cancels (never executed) and ignored on clean finishes.
	partial []messages.Message
	// heldGate reports whether the run held the session admission gate (it
	// executed). Only a holder releases the gate and re-checks wake.
	heldGate bool
}

// completeRun is the single persist-then-release point for session runs.
// The order is the correctness core:
//
//  1. persist — the clean transcript on completion, or the derived
//     cancelled history (prompt + partial + synthesized results + stop
//     notice) on cancel; both through the same save posture (WithoutCancel —
//     the run ctx is already dead but its values must survive — and
//     stripSystem). Errored runs persist nothing.
//  2. drain the session's queued-cancel finalizers (cancel-semantics gap
//     1): each deferred save re-loads the history just persisted here and
//     appends its own prompt + stop notice, so it lands inside this
//     holder's window instead of being clobbered by the full-overwrite
//     save above.
//  3. release the admission gate, so the next promoted run is guaranteed
//     to see this run's writes — including the drained finalizers' (a
//     queued run is promoted only after the holder's transcript, and every
//     other write, has been persisted).
//  4. re-check wake (lost-wakeup R4): onFinish — and with it
//     wakeAfterRun — fires BEFORE this release, so a notification queued
//     in between saw admission.busy and returned without waking. Re-check
//     now that the gate is open; the ranClean gate keeps the
//     cancelled/errored rule (never restart over what the user just
//     stopped).
func (s *Server) completeRun(c runCompletion) {
	ctx := context.WithoutCancel(c.run.ctx)
	if c.final != nil {
		// Completion persistence must survive run cancellation: keep the
		// ctx values, drop the cancellation.
		_ = s.sessions.save(ctx, c.sid, stripSystem(c.final), c.workspace)
		s.logObsRunEnd(c.sid, c.run.id, c.workspace, c.final)
	} else if reason, cancelled := c.run.cancelledReason(); cancelled {
		// Cancelled round: persist the derived history (prompt + partial +
		// synthesized results + stop notice) so a reload keeps what the user
		// typed, what already streamed, and why the round ended. ranClean
		// stays false and the wake re-check below keeps its cancelled-run
		// rule.
		history := c.base
		if history == nil {
			history = s.sessions.load(ctx, c.sid)
		}
		synth := synthesizeAborted(c.partial, reason, c.run.runElapsed())
		derived := derivedCancelledHistory(history, c.human, c.partial, synth, c.run.id, reason)
		_ = s.sessions.save(ctx, c.sid, stripSystem(derived), c.workspace)
	}
	// Deferred queued-cancel saves join this holder's serialized window:
	// drain AFTER the holder's own save, BEFORE the release. (A
	// self-executed finalizer drains too — later ones may be waiting behind
	// it in the FIFO.)
	s.drainCancelFinalizers(c.sid)
	if c.heldGate {
		s.admission.release(c.sid)
		if c.run.ranClean() {
			s.maybeWake(c.sid)
		}
	}
}

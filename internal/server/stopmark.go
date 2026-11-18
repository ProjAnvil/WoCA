package server

// Stop-notice persistence for cancelled runs: a cancelled round used to
// persist nothing, losing the user's prompt on reload. The helpers here
// derive the transcript a cancelled run persists — the loaded history, the
// round's human prompt when it had one, and a machine notice tag following
// the existing notification convention (Human messages whose content starts
// with a registered tag prefix, like <scheduled-task ...>).

import (
	"slices"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/machinetag"
)

// Reasons a run stopped — a closed, wire-stable cause set (the strings
// appear in persisted notice tags and the done event's reason field, so
// they must stay readable by the web parser). The cause is stamped exactly
// once at the cancel entry point (Run.cancelRun, the single write point);
// consumers (the persisted tag, the done payload) only read it.
const (
	// StopReasonUser marks a run stopped by the user (cancel / stop button).
	StopReasonUser = "user_stop"
	// StopReasonParkedTTL marks a run cancelled by the parked-interrupt TTL
	// (an unanswered approval or ask timed out).
	StopReasonParkedTTL = "parked_ttl"
	// StopReasonServerStop marks a run aborted by the server lifecycle — a
	// queued run whose admission wait was aborted by shutdown
	// (admission.stop). Distinct from user_stop so a shutdown does not
	// persist "the user stopped this".
	StopReasonServerStop = "server_stop"
	// StopReasonLeaseLost marks a run cancelled because its run-ledger
	// lease was reaped or adopted by another instance (EE posture; stamped
	// by trackRunLease's heartbeat goroutine — no producer in the personal
	// edition, which configures no ledger).
	StopReasonLeaseLost = "lease_lost"
)

// stopNotice builds the persisted stop notice: a Human message whose content
// is the registered machine tag `<run-stopped run="..." reason="..."/>`. The
// wire format lives in the machinetag leaf package (one source shared with
// the agent/tui consumers); the tag stays model-visible in history (same
// class as <scheduled-task>); the consumers that must exclude it match on
// machinetag.RunStopped.
func stopNotice(runID, reason string) messages.Message {
	return messages.Human(machinetag.StopNotice(runID, reason))
}

// derivedCancelledHistory builds the transcript a cancelled run persists.
// history is cloned first: sessionStore.load returns a slice shared with the
// in-memory session state, so appending to it directly would pollute every
// later load (and the next run's prompt ride-along would overwrite the
// derived messages). human is nil for goal runs, whose rounds carry no human
// turn — the derivation then appends the notice tag alone.
func derivedCancelledHistory(history []messages.Message, human *messages.Message, runID, reason string) []messages.Message {
	derived := slices.Clone(history)
	if human != nil {
		derived = append(derived, *human)
	}
	return append(derived, stopNotice(runID, reason))
}

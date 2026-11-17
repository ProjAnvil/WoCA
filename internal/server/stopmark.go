package server

// Stop-notice persistence for cancelled runs: a cancelled round used to
// persist nothing, losing the user's prompt on reload. The helpers here
// derive the transcript a cancelled run persists — the loaded history, the
// round's human prompt when it had one, and a machine notice tag following
// the existing notification convention (Human messages whose content starts
// with a registered tag prefix, like <scheduled-task ...>).

import (
	"fmt"
	"slices"

	"github.com/projanvil/langchain-golang/core/messages"
)

// Reasons a run stopped, carried by the notice tag's reason attribute so the
// frontend can distinguish the copy (a user stop from an approval timeout).
const (
	// StopReasonUser marks a run stopped by the user (cancel / stop button).
	StopReasonUser = "user_stop"
	// StopReasonParkedTTL marks a run cancelled by the parked-interrupt TTL
	// (an unanswered approval or ask timed out).
	StopReasonParkedTTL = "parked_ttl"
)

// stopNotice builds the persisted stop notice: a Human message whose content
// is the registered machine tag `<run-stopped run="..." reason="..."/>`. The
// tag stays model-visible in history (same class as <scheduled-task>); the
// consumers that must exclude it match on the "<run-stopped" prefix.
func stopNotice(runID, reason string) messages.Message {
	return messages.Human(fmt.Sprintf(`<run-stopped run="%s" reason="%s"/>`, runID, reason))
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

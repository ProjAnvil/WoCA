package agent

import (
	"fmt"
	"sync"
)

// MaxTurns: the number of model rounds within one turn is capped; reaching
// the cap ends the turn with a <max-turns-reached> marker message. The check
// runs in the tools node — one completed tool round counts as one turn, and
// appending the (non-tool-carrying) marker makes the conditional route end
// the turn deterministically, without a wasted model call.

// turnCounter tracks model rounds within the current turn.
type turnCounter struct {
	mu    sync.Mutex
	count int
}

// ResetTurnCounter zeroes the per-turn round counter (one runner Run or
// agent Invoke = one turn).
func (a *Agent) ResetTurnCounter() {
	a.turns.mu.Lock()
	a.turns.count = 0
	a.turns.mu.Unlock()
}

// TurnsDone reports the model rounds completed in the current turn.
func (a *Agent) TurnsDone() int {
	a.turns.mu.Lock()
	defer a.turns.mu.Unlock()
	return a.turns.count
}

// maxTurnsMarker builds the end-of-turn marker message text.
func maxTurnsMarker(rounds int) string {
	return fmt.Sprintf("<max-turns-reached>\nStopped after %d model round(s) (max turns limit).\n</max-turns-reached>", rounds)
}

package tui

import (
	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/woca/internal/agent"
)

// Wake driver (background loop closure): a notification queued while the
// session is idle — a background shell or task completion, a cron firing —
// would otherwise sit in the agent's mid-turn queue until some
// user-initiated run drained it. The observer New installs hands each queued
// item to the Update loop as a notificationQueued signal, and maybeWake
// starts an ordinary run whose prompt is the coalesced tagged message the
// queue already produces. The agent queue is the source of truth: the signal
// only prompts a look, so a dropped one is recovered by the next signal or
// by the clean-finish re-check in Update's runDone branch.

// notificationQueued reports one notification the agent queued while no run
// was draining the queue. item is the trigger; maybeWake drains the whole
// queue itself.
type notificationQueued struct{ item agent.QueuedInput }

// notifyBufferSize bounds the signal channel between the agent's producer
// goroutines and the Update loop: deep enough that bursts of completions
// rarely drop a signal, small enough to be trivially cheap.
const notifyBufferSize = 64

// armNotifications returns a command that parks until the agent queues the
// next notification and yields it as a notificationQueued message; Update
// re-arms after each signal.
func (m *Model) armNotifications() tea.Cmd {
	ch := m.notifyCh
	return func() tea.Msg {
		q, ok := <-ch
		if !ok {
			return nil
		}
		return notificationQueued{item: q}
	}
}

// maybeWake starts a run for the notifications that piled up while no run
// could deliver them. A run in flight owns the queue (its tool rounds drain
// it in-turn) and an empty queue is nothing to wake on; otherwise the
// drained items become one coalesced wake turn.
func (m *Model) maybeWake() tea.Cmd {
	if m.running || m.agent == nil {
		return nil
	}
	items := m.agent.DrainQueuedInputsFor("")
	if len(items) == 0 {
		return nil
	}
	m.write(dimStyle.Render("⏵ background notification") + "\n")
	m.history = append(m.history, agent.CoalesceQueuedInputs(items))
	m.running = true
	return m.runWithHistory(m.history)
}

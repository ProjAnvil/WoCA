package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/projanvil/woca/internal/agent"
	"github.com/projanvil/woca/internal/config"
)

// Wake driver tests (see wake.go): the observer installed by New hands
// queued notifications to the Update loop, which wakes an idle session into
// a run over the coalesced queue. All tests drive Update directly and stay
// bounded — no tea.Program is run.

// TestWakeDrivesIdleNotificationIntoRun covers the full path: QueueNotification
// signals notifyCh, the signal wakes the idle session into a tagged Human
// turn plus a run, a second signal while running is a no-op, and a clean
// runDone with an empty queue returns the session to idle.
func TestWakeDrivesIdleNotificationIntoRun(t *testing.T) {
	a := newFakeAgent(t, messages.AI("noted"), messages.AI("noted again"))
	m := New(a, nil, config.Config{})

	// New installs the observer: the queued item also lands on notifyCh.
	a.QueueNotification("background-task-completed", "shell 1 done")
	var q agent.QueuedInput
	select {
	case q = <-m.notifyCh:
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not deliver the queued notification to notifyCh")
	}
	if q.Tag != "background-task-completed" || q.Text != "shell 1 done" {
		t.Fatalf("signaled item = %+v", q)
	}

	// The signal wakes the idle session: the queue is drained into one
	// tagged Human turn and a run starts.
	mm, _ := m.Update(notificationQueued{item: q})
	m = mm.(*Model)
	if !m.running {
		t.Fatal("a queued notification must wake an idle session")
	}
	last := m.history[len(m.history)-1]
	if last.Role != messages.RoleHuman ||
		!strings.Contains(last.Content, "background-task-completed") ||
		!strings.Contains(last.Content, "shell 1 done") {
		t.Fatalf("wake turn = %+v, want the tagged notification", last)
	}
	m.flush()
	if !strings.Contains(strings.Join(m.lines, "\n"), "⏵ background notification") {
		t.Fatalf("transcript missing the wake banner:\n%s", strings.Join(m.lines, "\n"))
	}

	// A signal while the wake run is in flight changes nothing: the running
	// turn's tool rounds deliver queued items in-turn.
	before := len(m.history)
	mm, _ = m.Update(notificationQueued{})
	m = mm.(*Model)
	if !m.running || len(m.history) != before {
		t.Fatalf("signal while running: running=%v history=%d (want %d)",
			m.running, len(m.history), before)
	}

	// A clean finish with an empty queue returns the session to idle.
	mm, _ = m.Update(runDone{result: m.history})
	m = mm.(*Model)
	if m.running {
		t.Fatal("runDone must clear running")
	}
}

// TestWakeAfterRunDoneRecoversLateNotifications covers the runDone re-check:
// a notification that lands mid-run after its last tool round stays queued,
// and the clean finish wakes the session instead of waiting for a user turn.
func TestWakeAfterRunDoneRecoversLateNotifications(t *testing.T) {
	a := newFakeAgent(t, messages.AI("one"), messages.AI("two"))
	m := New(a, nil, config.Config{})

	m.running = true // a user turn is in flight
	a.QueueNotification("background-task-completed", "shell 1 done")
	select {
	case q := <-m.notifyCh:
		mm, _ := m.Update(notificationQueued{item: q})
		m = mm.(*Model)
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not deliver the queued notification to notifyCh")
	}
	if !m.running {
		t.Fatal("a signal while running must not disturb the run")
	}

	// The clean finish drains the leftover queue into a fresh wake run.
	mm, _ := m.Update(runDone{result: m.history})
	m = mm.(*Model)
	if !m.running {
		t.Fatal("runDone must wake the session on leftover notifications")
	}
	last := m.history[len(m.history)-1]
	if last.Role != messages.RoleHuman ||
		!strings.Contains(last.Content, "background-task-completed") ||
		!strings.Contains(last.Content, "shell 1 done") {
		t.Fatalf("wake turn = %+v, want the tagged notification", last)
	}
}

// TestWakeSkippedAfterInterruptedRun pins the safety rail: the user stopped
// the agent, so a queued notification never restarts it — the item stays
// queued for the next user turn.
func TestWakeSkippedAfterInterruptedRun(t *testing.T) {
	a := newFakeAgent(t, messages.AI("ok"))
	m := New(a, nil, config.Config{})
	a.QueueNotification("background-task-completed", "shell 1 done")
	<-m.notifyCh // already buffered by the observer above

	m.running = true
	m.interrupting = true
	mm, _ := m.Update(runDone{err: context.Canceled})
	m = mm.(*Model)
	if m.running {
		t.Fatal("an interrupted run must leave the session idle")
	}
	if got := a.QueuedInputCountFor(""); got != 1 {
		t.Fatalf("queued items after interrupt = %d, want 1 (kept for the next user run)", got)
	}
	if len(m.history) != 0 {
		t.Fatalf("interrupted run appended history: %+v", m.history)
	}
}

// TestWakeArmNotificationsYieldsQueuedSignal drives the armNotifications
// command directly, the way the runtime would: it parks on notifyCh and
// yields the next queued notification as a notificationQueued message.
func TestWakeArmNotificationsYieldsQueuedSignal(t *testing.T) {
	a := newFakeAgent(t)
	m := New(a, nil, config.Config{})
	cmd := m.armNotifications()
	if cmd == nil {
		t.Fatal("armNotifications must return a command")
	}
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	a.QueueNotification("background-task-completed", "shell 2 done")
	select {
	case msg := <-done:
		nq, ok := msg.(notificationQueued)
		if !ok {
			t.Fatalf("armNotifications yielded %T, want notificationQueued", msg)
		}
		if nq.item.Tag != "background-task-completed" || nq.item.Text != "shell 2 done" {
			t.Fatalf("yielded item = %+v", nq.item)
		}
	case <-time.After(2 * time.Second):
		// Unblock the parked read so no goroutine outlives the test.
		a.QueueNotification("test-unblock", "x")
		t.Fatal("armNotifications did not yield the queued notification")
	}
}

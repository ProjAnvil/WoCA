// Package machinetag defines the machine-tag vocabulary of the harness: the
// tag prefixes of harness-injected notification messages (messages that look
// like Human turns but are machine-generated) and the builder for the
// persisted stop-notice tag. The vocabulary is shared across the server
// (which produces stop notices), the agent (whose retain pass skips notice
// tags when extracting a run's request) and the TUI (whose prompt fold skips
// them), so it lives in a stdlib-only leaf under internal/.
//
// The web keeps a hand-maintained parser twin of this vocabulary in
// web/src/lib/transcript.ts (RUN_STOPPED_TAG and NOTICE_TAGS); a CI drift
// check may compare the constants later, mirroring the SSE event-name
// contract check.
package machinetag

import "strings"

// RunStopped is the prefix of the persisted stop-notice tag: a cancelled run
// persists a Human message whose content starts with this prefix, carrying
// the run id and the stop cause as attributes. Consumers that must exclude
// the notice match on the prefix.
const RunStopped = "<run-stopped"

// Producer tag vocabulary: the other prefixes under which the harness
// injects machine-generated Human messages. They are defined here for one
// source of truth; membership in the excluded NoticePrefixes family is a
// separate decision (see below).
const (
	// ScheduledTask wraps cron-fired prompts. NOT part of NoticePrefixes: a
	// scheduled prompt is the run's actual request, so consumers extracting
	// "what did the user ask" must return it, not skip it.
	ScheduledTask = "<scheduled-task"
	// QueuedUserInput wraps text typed while a run was in flight.
	QueuedUserInput = "<queued-user-input"
	// StopHook wraps stop-hook feedback injected between continuations.
	StopHook = "<stop-hook>"
	// TaskFork wraps the fork prompt of a background task subagent.
	TaskFork = "<task-fork>"
	// RelevantMemory wraps memory entries prefetched for a run.
	RelevantMemory = "<relevant-memory>"
)

// Prefixes is the notice family the Go consumers exclude when they look for
// a run's real request (agent retain's runQuestion skip) — the exact five
// prefixes those consumers match on today. StopNotice tags are members via
// RunStopped. Editing this table is a behavior change for every consumer;
// tests pin it.
var Prefixes = []string{
	RelevantMemory,
	QueuedUserInput,
	StopHook,
	RunStopped,
	TaskFork,
}

// StopNotice builds the persisted stop-notice tag for a cancelled run:
// `<run-stopped run="{runID}" reason="{reason}"/>`. The reason is a
// wire-stable stop cause string (user_stop, parked_ttl, server_stop,
// lease_lost — the server owns the constants); the attribute values are not
// escaped, so callers must pass already-validated ids and causes.
func StopNotice(runID, reason string) string {
	return `<run-stopped run="` + runID + `" reason="` + reason + `"/>`
}

// IsStopNotice reports whether content is a stop-notice tag (leading
// whitespace tolerated — consumers match on trimmed content).
func IsStopNotice(content string) bool {
	return strings.HasPrefix(strings.TrimSpace(content), RunStopped)
}

// IsNotice reports whether content starts with any prefix of the excluded
// notice family (leading whitespace tolerated).
func IsNotice(content string) bool {
	c := strings.TrimSpace(content)
	if c == "" {
		return false
	}
	for _, p := range Prefixes {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

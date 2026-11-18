package machinetag

import (
	"strings"
	"testing"
)

// The four wire-stable stop causes (server stopmark constants; mirrored here
// as plain strings so the leaf package stays stdlib-only).
var stopCauses = []string{"user_stop", "parked_ttl", "server_stop", "lease_lost"}

// StopNotice builds the exact persisted tag for every stop cause: the wire
// format is `<run-stopped run="..." reason="..."/>` and the reason attribute
// round-trips verbatim (the web parser, transcript.ts, reads it after reload).
func TestStopNoticeBuildsExactTag(t *testing.T) {
	for _, cause := range stopCauses {
		want := `<run-stopped run="run-9" reason="` + cause + `"/>`
		if got := StopNotice("run-9", cause); got != want {
			t.Fatalf("StopNotice(%q) = %q, want %q", cause, got, want)
		}
	}
}

// Round-trip: a notice built by StopNotice is recognized by IsStopNotice and
// by the family check IsNotice, and carries the cause in its reason
// attribute — for all four causes.
func TestStopNoticeRoundTripAllCauses(t *testing.T) {
	for _, cause := range stopCauses {
		tag := StopNotice("run-rt", cause)
		if !IsStopNotice(tag) {
			t.Fatalf("IsStopNotice(%q) = false, want true", tag)
		}
		if !IsNotice(tag) {
			t.Fatalf("IsNotice(%q) = false, want true (stop notices are family members)", tag)
		}
		if !strings.Contains(tag, `reason="`+cause+`"`) {
			t.Fatalf("notice %q lost its cause attribute", tag)
		}
	}
}

// IsStopNotice matches the tag prefix with surrounding whitespace tolerated
// (the TUI matches on TrimSpace'd content) and rejects everything else.
func TestIsStopNotice(t *testing.T) {
	cases := []struct {
		content string
		want    bool
	}{
		{StopNotice("r", "user_stop"), true},
		{"  \n" + StopNotice("r", "server_stop"), true},
		{`<run-stopped run="x"`, true}, // prefix match, unclosed form
		{"plain user prompt", false},
		{"", false},
		{"<stop-hook>\nfeedback\n</stop-hook>", false},
		{"<run-stop", false}, // not the registered prefix
	}
	for _, c := range cases {
		if got := IsStopNotice(c.content); got != c.want {
			t.Fatalf("IsStopNotice(%q) = %v, want %v", c.content, got, c.want)
		}
	}
}

// IsNotice covers exactly the harness-injected notice family the Go
// consumers exclude (retain.go's runQuestion skip, the TUI's prompt fold):
// relevant-memory, queued-user-input, stop-hook, run-stopped, task-fork.
// A scheduled-task block is deliberately NOT a member: cron-delivered
// prompts are the run's actual request (runQuestion must return them).
func TestIsNoticeFamily(t *testing.T) {
	members := []string{
		"<relevant-memory>\nfact\n</relevant-memory>",
		"<queued-user-input>\ntyped while busy\n</queued-user-input>",
		"<stop-hook>\nfeedback\n</stop-hook>",
		StopNotice("r", "parked_ttl"),
		"<task-fork>\nforked task\n</task-fork>",
	}
	for _, m := range members {
		if !IsNotice(m) {
			t.Fatalf("IsNotice(%q) = false, want true", m)
		}
		if !IsNotice(" " + m) {
			t.Fatalf("IsNotice with leading whitespace = false for %q", m)
		}
	}
	nonMembers := []string{
		"what did you do?",
		"<scheduled-task>\ncron prompt\n</scheduled-task>",
		"<run-stopp",
		"",
	}
	for _, m := range nonMembers {
		if IsNotice(m) {
			t.Fatalf("IsNotice(%q) = true, want false", m)
		}
	}
}

// Prefixes pins the family table: changing its membership changes
// runQuestion/prompt-fold behavior, so it must be an explicit edit.
func TestPrefixesPinTheFamily(t *testing.T) {
	want := []string{
		"<relevant-memory>",
		"<queued-user-input",
		"<stop-hook>",
		"<run-stopped",
		"<task-fork>",
	}
	if len(Prefixes) != len(want) {
		t.Fatalf("Prefixes = %v, want exactly %v", Prefixes, want)
	}
	for i, p := range want {
		if Prefixes[i] != p {
			t.Fatalf("Prefixes[%d] = %q, want %q", i, Prefixes[i], p)
		}
	}
}

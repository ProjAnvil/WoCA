package server

// toolabort_test.go pins the synthesized terminal tool results for calls
// aborted by a run stop (spec 2026-11-19-cancel-partial-output §D2): every
// dangling call gets a Tool message whose content names the abort reason,
// the elapsed time (one decimal, measured from run start) and the
// verify-before-retry guidance — and the content must never start with a
// machine-notice tag prefix (it is Tool-role content, not a Human notice).

import (
	"fmt"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/machinetag"
)

func TestSynthToolAbortContentVerbatim(t *testing.T) {
	cases := []struct {
		reason string
		elapsed time.Duration
		want    string
	}{
		{StopReasonUser, 12500 * time.Millisecond,
			"Tool call aborted by user after 12.5s — outcome unknown. Verify the effect before retrying."},
		{StopReasonParkedTTL, 2 * time.Second,
			"Tool call aborted: approval timed out — outcome unknown. Verify the effect before retrying."},
		{StopReasonServerStop, time.Second,
			"Tool call aborted by server shutdown — outcome unknown. Verify the effect before retrying."},
		{StopReasonLeaseLost, time.Duration(0),
			"Tool call aborted: run lease lost to another instance — outcome unknown. Verify the effect before retrying."},
	}
	for _, tc := range cases {
		if got := synthToolAbortContent(tc.reason, tc.elapsed); got != tc.want {
			t.Fatalf("synthToolAbortContent(%q, %v) =\n  %q\nwant\n  %q", tc.reason, tc.elapsed, got, tc.want)
		}
	}
	// One decimal, sub-second precision kept (0.1s granularity).
	if got := synthToolAbortContent(StopReasonUser, 240*time.Millisecond); got != fmt.Sprintf("Tool call aborted by user after %.1fs — outcome unknown. Verify the effect before retrying.", 0.24) {
		t.Fatalf("sub-second elapsed formatting wrong: %q", got)
	}
	// Unknown reasons degrade to the generic user phrasing (defensive; the
	// closed set is the only producer).
	if got, want := synthToolAbortContent("", time.Second), "Tool call aborted by user after 1.0s — outcome unknown. Verify the effect before retrying."; got != want {
		t.Fatalf("empty reason = %q, want user fallback", got)
	}
}

// The synthesized content is Tool-role output rendered verbatim by tool
// cards — it must never be mistakable for a machine-notice tag (the Prefixes
// family matches Human notices; an accidental prefix would pull Tool content
// into notice consumers).
func TestSynthToolAbortContentNotNoticePrefixed(t *testing.T) {
	for _, reason := range []string{StopReasonUser, StopReasonParkedTTL, StopReasonServerStop, StopReasonLeaseLost} {
		content := synthToolAbortContent(reason, time.Second)
		if machinetag.IsNotice(content) {
			t.Fatalf("synth content for %q reads as a notice tag: %q", reason, content)
		}
		for _, p := range machinetag.Prefixes {
			if len(content) >= len(p) && content[:len(p)] == p {
				t.Fatalf("synth content for %q starts with prefix %q", reason, p)
			}
		}
	}
}

func TestDanglingToolCalls(t *testing.T) {
	ai := messages.AI("")
	ai.ToolCalls = []messages.ToolCall{
		{ID: "c1", Name: "Read"}, {ID: "c2", Name: "Bash"}, {ID: "c3", Name: "Grep"},
	}
	partial := []messages.Message{
		messages.Human("go"),
		ai,
		messages.Tool("c1", "result 1"),
	}
	got := danglingToolCalls(partial)
	if len(got) != 2 || got[0].ID != "c2" || got[1].ID != "c3" {
		t.Fatalf("danglingToolCalls = %+v, want [c2 c3]", got)
	}
	// A second AI round: its answered call survives, its dangling one is
	// collected in stream order after the earlier round's.
	ai2 := messages.AI("")
	ai2.ToolCalls = []messages.ToolCall{{ID: "c4", Name: "Read"}, {ID: "c5", Name: "Bash"}}
	partial2 := append(partial, ai2, messages.Tool("c4", "result 4"))
	got2 := danglingToolCalls(partial2)
	if len(got2) != 3 || got2[0].ID != "c2" || got2[1].ID != "c3" || got2[2].ID != "c5" {
		t.Fatalf("danglingToolCalls multi-round = %+v, want [c2 c3 c5]", got2)
	}
	// Everything answered → nothing dangles.
	if got := danglingToolCalls([]messages.Message{ai, messages.Tool("c1", "r"), messages.Tool("c2", "r"), messages.Tool("c3", "r")}); len(got) != 0 {
		t.Fatalf("fully answered = %+v, want none", got)
	}
}

func TestSynthesizeAborted(t *testing.T) {
	ai := messages.AI("")
	ai.ToolCalls = []messages.ToolCall{{ID: "a", Name: "Read"}, {ID: "b", Name: "Bash"}}
	partial := []messages.Message{ai, messages.Tool("a", "real result")}
	synth := synthesizeAborted(partial, StopReasonUser, 3*time.Second)
	if len(synth) != 1 {
		t.Fatalf("synthesized %d messages, want 1 (only the dangling call)", len(synth))
	}
	m := synth[0]
	if m.Role != messages.RoleTool || m.ToolCallID != "b" {
		t.Fatalf("synth message = %+v, want Tool role for call b", m)
	}
	want := "Tool call aborted by user after 3.0s — outcome unknown. Verify the effect before retrying."
	if m.Content != want {
		t.Fatalf("synth content = %q, want %q", m.Content, want)
	}
	// Empty partial and fully-answered partial synthesize nothing.
	if got := synthesizeAborted(nil, StopReasonUser, time.Second); len(got) != 0 {
		t.Fatalf("nil partial synthesized %+v", got)
	}
}

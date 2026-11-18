package agent

// T5 Part A: the identity of an approval across a re-park. When a pause
// carrying several child approvals is answered PARTIALLY, the unanswered
// child's approval node re-executes and re-fires its interrupt — the
// server's buffered-answer path must still be able to address that re-parked
// approval. These tests pin WHAT is stable across the re-park (the
// ApprovalRequest's own identity — its asks' call ids, which persist in the
// checkpointed message state) versus what the fork stamps per invocation
// (the interrupt NS), which is why the server re-keys buffered answers by
// the ask identity (see internal/server/run_rekey_test.go).

import (
	"fmt"
	"testing"
	"time"

	"github.com/projanvil/langchain-golang/core/messages"

	"github.com/projanvil/woca/internal/config"
	"github.com/projanvil/woca/internal/tools"
)

// pauseAsks extracts the ask call ids of an approval pause value.
func pauseAsks(t *testing.T, value any) []string {
	t.Helper()
	req, ok := DecodeInterruptValue(value).(ApprovalRequest)
	if !ok {
		t.Fatalf("pause payload = %#v, want ApprovalRequest", DecodeInterruptValue(value))
	}
	ids := make([]string, 0, len(req.Asks))
	for _, ask := range req.Asks {
		ids = append(ids, ask.CallID)
	}
	return ids
}

// interruptLike is the subset of lgtypes.Interrupt the pause helpers read.
type interruptLike struct {
	NS    string
	ID    string
	Value any
}

// twoChildApprovalPause runs the two-parallel-children scenario to its first
// pause and returns both interrupts.
func twoChildApprovalPause(t *testing.T, thread string) (a *Agent, intrs []interruptLike) {
	t.Helper()
	workdir := t.TempDir()
	agent, err := New(Config{
		Model:          &n1ParentModel{},
		SubagentModel:  &promptFileModel{},
		Registry:       tools.NewRegistry(workdir),
		SystemPrompt:   "t",
		PermissionMode: config.ModeDefault,
		WorkDir:        workdir,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := pumpStream(t, agent.Stream(t.Context(), []messages.Message{messages.Human("go")}, thread))
	if len(raw) != 2 {
		t.Fatalf("pauses = %d, want 2 (both children's Write asks in one superstep)", len(raw))
	}
	for _, in := range raw {
		intrs = append(intrs, interruptLike{NS: in.NS, ID: in.ID, Value: in.Value})
	}
	return agent, intrs
}

// TestReParkApprovalIdentity: answer only ONE child of a two-child pause; the
// unanswered child re-parks. The re-parked approval's ASK CALL IDS (the
// approval request's own identity) must be identical to the original pause's
// — that is the stable identity the server re-keys buffered answers by.
func TestReParkApprovalIdentity(t *testing.T) {
	const thread = "thread-t5-repark-identity"
	a, intrs := twoChildApprovalPause(t, thread)
	allow, pending := intrs[0], intrs[1]

	// Partial answer: allow the first child only; the second re-parks.
	resume := map[string]any{
		allow.NS: ApprovalAnswer{Decisions: map[string]AskDecision{
			pauseAsks(t, allow.Value)[0]: {Outcome: OutcomeAllow},
		}},
	}
	raw, _ := pumpStream(t, a.ResumeStream(t.Context(), thread, resume))
	if len(raw) != 1 {
		t.Fatalf("re-park pauses = %d, want 1 (the unanswered child)", len(raw))
	}
	reParked := raw[0]

	// The stable identity: same ask call ids (the checkpointed tool call the
	// approval decision addresses).
	want := pauseAsks(t, pending.Value)
	got := pauseAsks(t, reParked.Value)
	if len(got) != len(want) {
		t.Fatalf("re-parked ask ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("re-parked ask ids = %v, want %v", got, want)
		}
	}

	// The re-parked approval must still be answerable to completion: deny it
	// through its CURRENT interrupt address (the fork's resume contract).
	finalIntrs, final := pumpStream(t, a.ResumeStream(t.Context(), thread, ApprovalAnswer{Decisions: map[string]AskDecision{
		got[0]: {Outcome: OutcomeDeny},
	}}))
	if len(finalIntrs) != 0 {
		t.Fatalf("run re-paused after the deny: %#v", DecodeInterruptValue(finalIntrs[0].Value))
	}
	if !toolContentContains(final, "sub done") {
		t.Fatalf("transcript missing the children's reports: %v", final)
	}
}

// TestPartialFlatAnswerReachesAnsweredChild: the server's flat {approve}
// answer builds a partial resume map whose value is the plain bool (not an
// ApprovalAnswer). The ANSWERED child must consume it and proceed — only the
// unanswered sibling may re-park.
func TestPartialFlatAnswerReachesAnsweredChild(t *testing.T) {
	thread := fmt.Sprintf("thread-t5-flat-%d", time.Now().UnixNano())
	a, intrs := twoChildApprovalPause(t, thread)
	answered, pending := intrs[0], intrs[1]

	// Exactly what the server's /answer builds for {approve:true} on the
	// first card of a two-card park: the single-entry map keyed by NS with
	// the flat bool value.
	resume := map[string]any{answered.NS: true}
	raw, _ := pumpStream(t, a.ResumeStream(t.Context(), thread, resume))
	if len(raw) == 0 {
		return // completed: the answered child proceeded, sibling collapsed
	}
	for _, in := range raw {
		if in.NS == pending.NS {
			continue // the unanswered sibling legitimately re-parks
		}
		t.Fatalf("ANSWERED child re-parked: its answer was lost (%q became %q)", answered.NS, in.NS)
	}
}
